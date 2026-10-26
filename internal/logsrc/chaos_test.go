package logsrc

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var chaosDuration = flag.Duration("chaos", 3*time.Second, "how long each chaos rotation scenario runs")

func payload(seq int) string {
	return strings.Repeat(string(rune('a'+seq%26)), seq%113)
}

func chaosLine(seq int) string { return fmt.Sprintf("seq %08d %s\n", seq, payload(seq)) }

type chaosStats struct {
	written, seen, missing, lost, rotations int
}

// runChaos has one goroutine writing numbered lines as fast as it can while
// another rotates the file, then checks what the Source recovered: every
// line intact, strictly in order, no duplicates; and counts what is missing.
func runChaos(t *testing.T, mode string, dur time.Duration) chaosStats {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	o := testOpts(t)
	o.Poll = 50 * time.Millisecond
	o.Grace = 300 * time.Millisecond
	src := OpenFile(path, o)
	t.Cleanup(src.Close)

	var reopen atomic.Bool
	var written atomic.Int64
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	rng := rand.New(rand.NewSource(1))

	go func() {
		defer close(writerDone)
		f, err := openAppend(path)
		if err != nil {
			t.Error(err)
			return
		}
		seq, lateLines := 0, -1
		for {
			select {
			case <-stop:
				f.Close()
				written.Store(int64(seq))
				return
			default:
			}
			if reopen.Load() && lateLines < 0 {
				lateLines = 1 + rand.Intn(20) // keep writing to the old file for a while, like a logger awaiting SIGHUP
			}
			burst := 1 + rand.Intn(50)
			var b strings.Builder
			for i := 0; i < burst; i++ {
				b.WriteString(chaosLine(seq))
				seq++
			}
			if _, err := f.WriteString(b.String()); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			if lateLines >= 0 {
				if lateLines -= burst; lateLines < 0 {
					f.Close()
					if f, err = openAppend(path); err != nil {
						t.Error(err)
						return
					}
					reopen.Store(false)
				}
			}
			if rand.Intn(4) == 0 {
				time.Sleep(time.Duration(rand.Intn(1500)) * time.Microsecond)
			}
		}
	}()

	rotations := 0
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(20+rng.Intn(80)) * time.Millisecond)
		m := mode
		if mode == "mixed" {
			m = []string{"rename", "copytruncate"}[rng.Intn(2)]
		}
		switch m {
		case "rename":
			if reopen.Load() {
				continue // previous rotation not yet picked up by the writer
			}
			if err := renameRetry(path, fmt.Sprintf("%s.%d", path, rotations)); err != nil {
				t.Fatalf("rename: %v", err)
			}
			reopen.Store(true)
		case "copytruncate":
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(fmt.Sprintf("%s.%d", path, rotations), data, 0o644)
			if err := os.Truncate(path, 0); err != nil {
				t.Fatalf("truncate: %v", err)
			}
		}
		rotations++
	}
	close(stop)
	<-writerDone

	// Let the tailer drain and the grace period for rotated files expire.
	last, stableSince := int64(-1), time.Now()
	waitUntil(t, "source to settle", func() bool {
		n := totalScan(src)
		if n != last {
			last, stableSince = n, time.Now()
		}
		return src.Ready() && time.Since(stableSince) > 3*o.Grace
	})

	st := chaosStats{written: int(written.Load()), rotations: rotations}
	prev := -1
	seen := make([]bool, st.written)
	for g, seg := range collect(src) {
		for i, l := range seg {
			if l == "<lost>" {
				st.lost++
				continue
			}
			parts := strings.SplitN(l, " ", 3)
			if len(parts) != 3 || parts[0] != "seq" {
				t.Fatalf("seg %d line %d malformed: %.80q", g, i, l)
			}
			seq, err := strconv.Atoi(parts[1])
			if err != nil || seq >= st.written || parts[2] != payload(seq) {
				t.Fatalf("seg %d line %d corrupt: %.80q", g, i, l)
			}
			if seq <= prev {
				t.Fatalf("seg %d line %d: seq %d after %d (duplicate or out of order)", g, i, seq, prev)
			}
			prev = seq
			seen[seq] = true
			st.seen++
		}
	}
	st.missing = st.written - st.seen
	t.Logf("%s: %d lines written, %d rotations, %d generations, %d recovered, %d missing (%.3f%%), %d not preserved",
		mode, st.written, st.rotations, src.NumSegs(), st.seen, st.missing, 100*float64(st.missing)/float64(max(1, st.written)), st.lost)
	return st
}

func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}

func TestChaosRenameRotation(t *testing.T) {
	st := runChaos(t, "rename", *chaosDuration)
	if st.missing != 0 || st.lost != 0 {
		t.Fatalf("rename rotation must lose nothing: %+v", st)
	}
	if st.rotations < 5 {
		t.Fatalf("too few rotations to be meaningful: %d", st.rotations)
	}
}

// copytruncate is inherently racy: bytes written between our last read and
// the truncation are gone. We require that loss to stay small and that no
// line is ever corrupted, duplicated or reordered (checked in runChaos).
func TestChaosCopyTruncate(t *testing.T) {
	st := runChaos(t, "copytruncate", *chaosDuration)
	if st.lost != 0 || float64(st.missing) > 0.05*float64(st.written) {
		t.Fatalf("too much loss: %+v", st)
	}
}

func TestChaosMixed(t *testing.T) {
	st := runChaos(t, "mixed", *chaosDuration)
	if st.lost != 0 || float64(st.missing) > 0.05*float64(st.written) {
		t.Fatalf("too much loss: %+v", st)
	}
}
