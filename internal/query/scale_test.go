package query

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nfelddav/logview/internal/logsrc"
)

// TestScale exercises a multi-gigabyte file. It only runs when
// LOGVIEW_SCALE_GB is set, e.g. LOGVIEW_SCALE_GB=20 go test -run Scale -v -timeout 1h
// LOGVIEW_SCALE_DIR picks where the file is written (default: a temp dir).
func TestScale(t *testing.T) {
	gb, _ := strconv.ParseFloat(os.Getenv("LOGVIEW_SCALE_GB"), 64)
	if gb <= 0 {
		t.Skip("set LOGVIEW_SCALE_GB to run")
	}
	dir := os.Getenv("LOGVIEW_SCALE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, "scale.log")
	defer os.Remove(path)
	target := int64(gb * (1 << 30))

	start := time.Now()
	lines, errors := generate(t, path, target)
	genDur := time.Since(start)
	t.Logf("generated %.1f GiB, %d lines, %d ERROR lines in %v (%.0f MiB/s)",
		float64(target)/(1<<30), lines, errors, genDur.Round(time.Second), float64(target)/(1<<20)/genDur.Seconds())

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start = time.Now()
	src := logsrc.OpenFile(path, logsrc.Options{Preserve: 256 << 20, SpoolDir: dir})
	defer src.Close()
	for !src.Ready() {
		time.Sleep(50 * time.Millisecond)
	}
	idxDur := time.Since(start)
	if got := src.ScanLen(0); got != lines {
		t.Fatalf("indexed %d lines, want %d", got, lines)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("indexed in %v (%.0f MiB/s); heap in use %.1f MiB for %d lines (%.2f bytes/line)",
		idxDur.Round(time.Millisecond), float64(target)/(1<<20)/idxDur.Seconds(),
		float64(after.HeapInuse)/(1<<20), lines, float64(after.HeapInuse)/float64(lines))

	rng := rand.New(rand.NewSource(1))
	const probes = 20000
	start = time.Now()
	for i := 0; i < probes; i++ {
		n := rng.Int63n(lines)
		b, _, err := src.Line(0, n)
		if err != nil || len(b) == 0 {
			t.Fatalf("Line(%d): %v", n, err)
		}
	}
	t.Logf("random access: %v per line (cold blocks)", (time.Since(start) / probes).Round(time.Microsecond))

	re := regexp.MustCompile(`ERROR`)
	start = time.Now()
	f := NewFilter(src, &Config{Includes: []*regexp.Regexp{re}, Record: RecordMode{Kind: RecOff}})
	for !f.Done() {
		time.Sleep(50 * time.Millisecond)
	}
	fDur := time.Since(start)
	if f.Count() != errors {
		t.Fatalf("filter matched %d, want %d", f.Count(), errors)
	}
	t.Logf("filter (regex, %d cores) over all lines: %v (%.0f MiB/s, %.1fM lines/s), %d matches",
		runtime.GOMAXPROCS(0), fDur.Round(time.Millisecond), float64(target)/(1<<20)/fDur.Seconds(),
		float64(lines)/1e6/fDur.Seconds(), f.Count())
	f.Stop()

	f = NewFilter(src, &Config{Includes: []*regexp.Regexp{re}})
	start = time.Now()
	for !f.Done() {
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("filter with auto multi-line records (timestamp parsing): %v", time.Since(start).Round(time.Millisecond))
	f.Stop()

	var cancel atomic.Bool
	needle := regexp.MustCompile(`needle-at-the-end`)
	start = time.Now()
	p, _, ok := Search(src, &Config{}, needle, Pos{0, 0}, true, nil, &cancel, nil)
	if !ok || p.I != lines-1 {
		t.Fatalf("search: %v %v", p, ok)
	}
	t.Logf("search from top to the last line: %v", time.Since(start).Round(time.Millisecond))
}

func generate(t *testing.T, path string, target int64) (lines, errors int64) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 4<<20)
	rng := rand.New(rand.NewSource(42))
	base := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	words := []string{"request", "handled", "user", "cache", "miss", "db", "query", "took", "retry", "upstream"}
	var written int64
	for written < target-200 {
		ts := base.Add(time.Duration(lines) * time.Millisecond).Format("2006-01-02T15:04:05.000Z")
		var s string
		switch {
		case lines%1000 == 0:
			s = fmt.Sprintf("%s ERROR worker-%d: %s failed after %dms\n", ts, rng.Intn(64), words[rng.Intn(10)], rng.Intn(5000))
			errors++
		case lines%7 == 0:
			s = fmt.Sprintf(`{"time":"%s","level":"info","msg":"%s %s","user":{"id":%d},"dur_ms":%d}`+"\n",
				ts, words[rng.Intn(10)], words[rng.Intn(10)], rng.Intn(100000), rng.Intn(900))
		case lines%5 == 0:
			s = fmt.Sprintf("%s \x1b[32mINFO\x1b[0m http: GET /api/v1/items/%d status=200 bytes=%d\n", ts, rng.Intn(1e6), rng.Intn(1e5))
		default:
			s = fmt.Sprintf("%s DEBUG %s %s %s id=%08x\n", ts, words[rng.Intn(10)], words[rng.Intn(10)], words[rng.Intn(10)], rng.Uint32())
		}
		w.WriteString(s)
		written += int64(len(s))
		lines++
	}
	s := "2026-10-26T00:00:00.000Z INFO needle-at-the-end\n"
	w.WriteString(s)
	lines++
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return lines, errors
}
