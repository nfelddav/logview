package query

import (
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/nfelddav/logview/internal/logsrc"
)

type Pos struct {
	G int
	I int64
}

func (p Pos) Less(q Pos) bool { return p.G < q.G || p.G == q.G && p.I < q.I }

type span struct {
	g          int
	start, end int64
}

// Search looks for the next line after (or before) from whose text matches
// re, wrapping around once. visible filters candidate lines (nil: all).
// It returns the match and whether the search wrapped. cancel aborts it.
func Search(seq logsrc.Seq, cfg *Config, re *regexp.Regexp, from Pos, forward bool,
	visible func(g int, i int64) bool, cancel *atomic.Bool, progress func(scanned int64)) (Pos, bool, bool) {

	n := seq.NumSegs()
	if from.G < 0 || from.G >= n {
		return from, false, false
	}
	lens := make([]int64, n)
	for g := range lens {
		lens[g] = seq.ScanLen(g)
	}
	var order []span
	if forward {
		order = append(order, span{from.G, from.I + 1, lens[from.G]})
		for g := from.G + 1; g < n; g++ {
			order = append(order, span{g, 0, lens[g]})
		}
		for g := 0; g < from.G; g++ {
			order = append(order, span{g, 0, lens[g]})
		}
		order = append(order, span{from.G, 0, min(from.I+1, lens[from.G])})
	} else {
		order = append(order, span{from.G, 0, min(from.I, lens[from.G])})
		for g := from.G - 1; g >= 0; g-- {
			order = append(order, span{g, 0, lens[g]})
		}
		for g := n - 1; g > from.G; g-- {
			order = append(order, span{g, 0, lens[g]})
		}
		order = append(order, span{from.G, from.I, lens[from.G]})
	}
	s := &searcher{seq: seq, cfg: cfg, re: re, visible: visible, cancel: cancel, progress: progress}
	for k, sp := range order {
		wrapped := k > 0 && (forward && sp.g < from.G || !forward && sp.g > from.G || k == len(order)-1)
		if sp.start >= sp.end {
			continue
		}
		var i int64
		if forward {
			i = s.forward(sp)
		} else {
			i = s.backward(sp)
		}
		if i >= 0 {
			return Pos{sp.g, i}, wrapped, true
		}
		if cancel.Load() {
			break
		}
	}
	return from, false, false
}

type searcher struct {
	seq      logsrc.Seq
	cfg      *Config
	re       *regexp.Regexp
	visible  func(g int, i int64) bool
	cancel   *atomic.Bool
	progress func(int64)
	scanned  atomic.Int64
}

// firstMatch returns the index within b of the first matching line, or -1.
func (s *searcher) firstMatch(g int, first int64, b *logsrc.Batch, fromEnd bool) int {
	n := b.Len()
	defer func() {
		if v := s.scanned.Add(int64(n)); s.progress != nil {
			s.progress(v)
		}
	}()
	for k := 0; k < n; k++ {
		i := k
		if fromEnd {
			i = n - 1 - k
		}
		if b.Lost[i] || (s.visible != nil && !s.visible(g, first+int64(i))) {
			continue
		}
		if s.re.Match(s.cfg.TextBytes(b.Line(i))) {
			return i
		}
	}
	return -1
}

// forward evaluates batches in parallel and returns the earliest hit.
func (s *searcher) forward(sp span) int64 {
	workers := max(1, runtime.GOMAXPROCS(0))
	it := s.seq.Iter(sp.g, sp.start)
	next := sp.start
	for next < sp.end && !s.cancel.Load() {
		type job struct {
			b   *batch
			hit int
		}
		var jobs []*job
		var wg sync.WaitGroup
		for len(jobs) < workers*4 && next < sp.end {
			b := batchPool.Get().(*batch)
			b.lines.Reset()
			b.first = next
			n := logsrc.ReadBatch(it, &b.lines, int(min(batchLines, sp.end-next)))
			if n == 0 {
				batchPool.Put(b)
				break
			}
			next += int64(n)
			j := &job{b: b}
			jobs = append(jobs, j)
			wg.Add(1)
			go func() {
				defer wg.Done()
				j.hit = s.firstMatch(sp.g, b.first, &b.lines, false)
			}()
		}
		wg.Wait()
		if len(jobs) == 0 {
			break
		}
		found := int64(-1)
		for _, j := range jobs {
			if found < 0 && j.hit >= 0 {
				found = j.b.first + int64(j.hit)
			}
			batchPool.Put(j.b)
		}
		if found >= 0 {
			return found
		}
	}
	return -1
}

// backward reads fixed-size chunks from the end and scans each from its end.
func (s *searcher) backward(sp span) int64 {
	const chunk = 8192
	var b logsrc.Batch
	for hi := sp.end; hi > sp.start && !s.cancel.Load(); hi -= chunk {
		lo := max(sp.start, hi-chunk)
		b.Reset()
		logsrc.ReadBatch(s.seq.Iter(sp.g, lo), &b, int(hi-lo))
		if i := s.firstMatch(sp.g, lo, &b, true); i >= 0 {
			return lo + int64(i)
		}
	}
	return -1
}
