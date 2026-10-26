package logsrc

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOpts(t *testing.T) Options {
	return Options{Preserve: -1, SpoolDir: t.TempDir(), Poll: 20 * time.Millisecond, Grace: 200 * time.Millisecond}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func lines(prefix string, from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "%s %06d\n", prefix, i)
	}
	return b.String()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// collect reads every scannable line of every segment.
func collect(q Seq) (out [][]string) {
	for g := 0; g < q.NumSegs(); g++ {
		var seg []string
		it := q.Iter(g, 0)
		for {
			b, _, lost, ok := it.Next()
			if !ok {
				break
			}
			if lost {
				seg = append(seg, "<lost>")
			} else {
				seg = append(seg, string(b))
			}
		}
		out = append(out, seg)
	}
	return out
}

func totalScan(q Seq) (n int64) {
	for g := 0; g < q.NumSegs(); g++ {
		n += q.ScanLen(g)
	}
	return
}

func TestIndexAndRandomAccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	var want []string
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		l := fmt.Sprintf("line %d %s", i, strings.Repeat("x", i%300))
		if i%1000 == 7 {
			l = "crlf " + l + "\r" // CRLF endings are stripped
			want = append(want, strings.TrimSuffix(l, "\r"))
		} else {
			want = append(want, l)
		}
		b.WriteString(l + "\n")
	}
	long := strings.Repeat("L", iterChunk*3) // longer than the iterator window
	huge := strings.Repeat("H", MaxLineLen+5000)
	b.WriteString(long + "\n" + huge + "\n" + "tail without newline")
	want = append(want, long, huge[:MaxLineLen], "tail without newline")
	os.WriteFile(path, []byte(b.String()), 0o644)

	o := testOpts(t)
	o.NoTail = true
	s := OpenFile(path, o)
	t.Cleanup(s.Close)
	waitUntil(t, "ready", s.Ready)

	if got := s.ScanLen(0); got != int64(len(want)) {
		t.Fatalf("ScanLen = %d, want %d", got, len(want))
	}
	for _, i := range []int64{0, 1, 127, 128, 129, 1007, 2999, 4999, 5000, 5001, 5002} {
		got, _, err := s.Line(0, i)
		if err != nil || string(got) != want[i] {
			t.Errorf("Line(%d) = %.40q, %v; want %.40q", i, got, err, want[i])
		}
	}
	segs := collect(s)
	if len(segs) != 1 || len(segs[0]) != len(want) {
		t.Fatalf("collect: %d segs", len(segs))
	}
	for i := range want {
		if segs[0][i] != want[i] {
			t.Fatalf("iter line %d = %.40q want %.40q", i, segs[0][i], want[i])
		}
	}
	// An iterator started mid-block must agree with random access.
	it := s.Iter(0, 300)
	for i := 300; i < 600; i++ {
		b, ref, _, _ := it.Next()
		if string(b) != want[i] || ref.Line != int64(i) {
			t.Fatalf("mid iter %d: %q", i, b)
		}
	}
}

func TestFollowAppendAndPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, lines("a", 0, 10))
	s := OpenFile(path, testOpts(t))
	t.Cleanup(s.Close)
	waitUntil(t, "10 lines", func() bool { return s.ScanLen(0) == 10 })

	appendFile(t, path, lines("a", 10, 300))
	waitUntil(t, "300 lines", func() bool { return s.ScanLen(0) == 300 })

	appendFile(t, path, "partial")
	waitUntil(t, "partial shown", func() bool { return s.DisplayLen(0) == 301 })
	if s.ScanLen(0) != 300 {
		t.Fatal("partial line must not be scannable yet")
	}
	if b, _, _ := s.Line(0, 300); string(b) != "partial" {
		t.Fatalf("partial = %q", b)
	}
	appendFile(t, path, " done\nnext\n")
	waitUntil(t, "302 lines", func() bool { return s.ScanLen(0) == 302 })
	if b, _, _ := s.Line(0, 300); string(b) != "partial done" {
		t.Fatalf("completed = %q", b)
	}
}

func TestWaitsForMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "later.log")
	s := OpenFile(path, testOpts(t))
	t.Cleanup(s.Close)
	waitUntil(t, "status", func() bool { return strings.HasPrefix(s.Status(), "waiting") })
	appendFile(t, path, lines("x", 0, 5))
	waitUntil(t, "lines", func() bool { return s.NumSegs() == 1 && s.ScanLen(0) == 5 })
	if s.Status() != "" {
		t.Fatalf("status %q", s.Status())
	}
}

func TestRenameRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	appendFile(t, path, lines("old", 0, 100))
	s := OpenFile(path, testOpts(t))
	t.Cleanup(s.Close)
	waitUntil(t, "100", func() bool { return s.ScanLen(0) == 100 })

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path+".1", lines("old", 100, 110)) // a writer that has not reopened yet
	appendFile(t, path, lines("new", 0, 50))
	waitUntil(t, "rotation", func() bool {
		return s.NumSegs() == 2 && s.ScanLen(1) == 50 && s.ScanLen(0) == 110 && s.Info(0).Final
	})
	segs := collect(s)
	if segs[0][109] != "old 000109" || segs[1][0] != "new 000000" {
		t.Fatalf("segs: %q / %q", segs[0][109], segs[1][0])
	}
	if s.Info(0).Kind != KindRotated || s.Info(1).Kind != KindLive {
		t.Fatalf("kinds %v %v", s.Info(0).Kind, s.Info(1).Kind)
	}
}

func copyTruncate(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path+".1", data, 0o644)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTruncatePreservesEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, path, lines("old", 0, 1000))
	s := OpenFile(path, testOpts(t))
	t.Cleanup(s.Close)
	waitUntil(t, "1000", func() bool { return s.ScanLen(0) == 1000 })
	copyTruncate(t, path)
	appendFile(t, path, lines("new", 0, 20))
	waitUntil(t, "new gen", func() bool { return s.NumSegs() == 2 && s.ScanLen(1) == 20 })

	info := s.Info(0)
	if info.Kind != KindTruncated || info.Lost != 0 || !info.Final {
		t.Fatalf("old gen info %+v", info)
	}
	segs := collect(s)
	for i, l := range segs[0] {
		if l != fmt.Sprintf("old %06d", i) {
			t.Fatalf("old line %d = %q", i, l)
		}
	}
	if b, _, err := s.Line(0, 999); err != nil || string(b) != "old 000999" {
		t.Fatalf("random access into spool: %q %v", b, err)
	}
}

func TestPreserveLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, path, lines("old", 0, 1000)) // 11 bytes per line
	o := testOpts(t)
	o.Preserve = 1100 // the last ~100 lines
	s := OpenFile(path, o)
	t.Cleanup(s.Close)
	waitUntil(t, "1000", func() bool { return s.ScanLen(0) == 1000 })
	appendFile(t, path, lines("old", 1000, 1010)) // appended data is always spooled
	waitUntil(t, "1010", func() bool { return s.ScanLen(0) == 1010 })
	copyTruncate(t, path)
	appendFile(t, path, lines("new", 0, 1))
	waitUntil(t, "new gen", func() bool { return s.NumSegs() == 2 && s.ScanLen(1) == 1 })

	info := s.Info(0)
	if info.Lost != 900 {
		t.Fatalf("lost = %d, want 900", info.Lost)
	}
	if _, _, err := s.Line(0, 5); !errors.Is(err, ErrLost) {
		t.Fatalf("expected ErrLost, got %v", err)
	}
	for _, i := range []int64{900, 950, 1009} {
		if b, _, err := s.Line(0, i); err != nil || string(b) != fmt.Sprintf("old %06d", i) {
			t.Fatalf("Line(%d) = %q %v", i, b, err)
		}
	}
	segs := collect(s)
	if segs[0][899] != "<lost>" || segs[0][900] != "old 000900" {
		t.Fatalf("iter boundary: %q %q", segs[0][899], segs[0][900])
	}
}

// A truncate followed by a rewrite that is already longer than what we had
// read must still be detected (size alone would not show it).
func TestTruncateAndRewriteDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, path, lines("old", 0, 10))
	s := &Source{Name: "app.log", Path: path, opts: testOpts(t).withDefaults(), done: make(chan struct{})}
	s.status.Store("")
	f, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	st := s.startLive(f, "app.log")
	buf := make([]byte, 1<<20)
	if grew, trunc := s.readAvail(st, buf); !grew || trunc {
		t.Fatalf("initial read grew=%v trunc=%v", grew, trunc)
	}
	os.WriteFile(path, []byte(lines("new", 0, 30)), 0o644) // truncates and rewrites
	if _, trunc := s.readAvail(st, buf); !trunc {
		t.Fatal("rewrite not detected")
	}
	if got := st.g.ScanLen(); got != 10 {
		t.Fatalf("old generation grew to %d lines", got)
	}
	st2 := s.restartAfterTruncation(st)
	s.readAvail(st2, buf)
	if st2.g.ScanLen() != 30 || s.Info(0).Kind != KindTruncated {
		t.Fatalf("new gen %d lines, old kind %v", st2.g.ScanLen(), s.Info(0).Kind)
	}
	f.Close()
	st.g.close()
	st2.g.close()
}

func TestSiblingsLoadedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, lines("three", 0, 3000))
	zw.Close()
	os.WriteFile(path+".3.gz", gz.Bytes(), 0o644)
	os.WriteFile(path+".2", []byte(lines("two", 0, 20)), 0o644)
	os.WriteFile(filepath.Join(dir, "app-20261024.log"), []byte(lines("one", 0, 10)), 0o644)
	os.WriteFile(path, []byte(lines("live", 0, 5)), 0o644)
	os.WriteFile(filepath.Join(dir, "app.logger"), []byte("unrelated\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "other.log.1"), []byte("unrelated\n"), 0o644)
	now := time.Now()
	os.Chtimes(path+".3.gz", now, now.Add(-3*time.Hour))
	os.Chtimes(path+".2", now, now.Add(-2*time.Hour))
	os.Chtimes(filepath.Join(dir, "app-20261024.log"), now, now.Add(-1*time.Hour))

	o := testOpts(t)
	o.Rotated = true
	s := OpenFile(path, o)
	t.Cleanup(s.Close)
	waitUntil(t, "ready", s.Ready)
	if s.NumSegs() != 4 {
		t.Fatalf("segs = %d", s.NumSegs())
	}
	wantLabels := []string{"app.log.3.gz", "app.log.2", "app-20261024.log", "app.log"}
	wantLens := []int64{3000, 20, 10, 5}
	for i := range wantLabels {
		if info := s.Info(i); info.Label != wantLabels[i] || s.ScanLen(i) != wantLens[i] {
			t.Errorf("seg %d: %q %d lines", i, info.Label, s.ScanLen(i))
		}
	}
	if b, _, _ := s.Line(0, 2999); string(b) != "three 002999" {
		t.Fatalf("gz random access %q", b)
	}
}

func TestStdinSource(t *testing.T) {
	pr, pw := io.Pipe()
	s := OpenReader("<stdin>", pr, testOpts(t))
	t.Cleanup(s.Close)
	go func() {
		io.WriteString(pw, lines("in", 0, 500))
		io.WriteString(pw, "last")
		pw.Close()
	}()
	waitUntil(t, "eof", func() bool { return s.Info(0).Final && s.ScanLen(0) == 501 })
	if b, _, _ := s.Line(0, 500); string(b) != "last" {
		t.Fatalf("%q", b)
	}
}

func TestMergeByTimestamp(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	os.WriteFile(a, []byte(
		"2026-10-25T10:00:00Z a1\n"+
			"2026-10-25T10:00:02Z a2\n"+
			"  continuation of a2\n"+
			"2026-10-25T10:00:04Z a3\n"), 0o644)
	os.WriteFile(b, []byte(
		"2026-10-25T10:00:01Z b1\n"+
			"2026-10-25T10:00:02Z b2\n"+
			"2026-10-25T10:00:05Z b3\n"), 0o644)
	o := testOpts(t)
	o.NoTail = true
	m := NewMerged([]*Source{OpenFile(a, o), OpenFile(b, o)})
	t.Cleanup(m.Close)
	waitUntil(t, "merged", func() bool { return m.Ready() && m.DisplayLen(0) == 7 })
	got := collect(m)[0]
	// Ties go to the source emitted last, which keeps a2's continuation
	// (same inherited timestamp as b2) attached to a2.
	want := []string{
		"2026-10-25T10:00:00Z a1",
		"2026-10-25T10:00:01Z b1",
		"2026-10-25T10:00:02Z b2",
		"2026-10-25T10:00:02Z a2",
		"  continuation of a2",
		"2026-10-25T10:00:04Z a3",
		"2026-10-25T10:00:05Z b3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("merged:\n%s", strings.Join(got, "\n"))
	}
	if l, ref, _ := m.Line(0, 2); string(l) != want[2] || ref.Src != 1 || ref.Line != 1 {
		t.Fatalf("Line(4) = %q %+v", l, ref)
	}
}

func TestForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, path, lines("old", 0, 100))
	s := OpenFile(path, testOpts(t))
	t.Cleanup(s.Close)
	waitUntil(t, "100", func() bool { return s.ScanLen(0) == 100 })
	if err := s.Forget(0); !errors.Is(err, ErrNotFinal) {
		t.Fatalf("forgetting the live file: %v", err)
	}
	copyTruncate(t, path)
	appendFile(t, path, lines("new", 0, 3))
	waitUntil(t, "new gen", func() bool { return s.NumSegs() == 2 && s.ScanLen(1) == 3 })
	if err := s.Forget(0); err != nil {
		t.Fatal(err)
	}
	if s.DisplayLen(0) != 0 || !s.Info(0).Forgotten || s.Info(0).Lost != 0 {
		t.Fatalf("after forget: len %d info %+v", s.DisplayLen(0), s.Info(0))
	}
	if _, _, err := s.Line(0, 0); !errors.Is(err, ErrRange) {
		t.Fatalf("Line on forgotten gen: %v", err)
	}
	if _, _, _, ok := s.Iter(0, 0).Next(); ok {
		t.Fatal("iterator yielded a forgotten line")
	}
	if b, _, _ := s.Line(1, 2); string(b) != "new 000002" {
		t.Fatalf("other generation affected: %q", b)
	}
	if err := s.Forget(5); !errors.Is(err, ErrRange) {
		t.Fatalf("out of range: %v", err)
	}
}
