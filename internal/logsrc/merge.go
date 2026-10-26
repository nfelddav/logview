package logsrc

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nfelddav/logview/internal/ts"
)

// Merged interleaves several sources by timestamp into one segment.
// Lines without a timestamp inherit the previous line's, so multi-line
// records stay together. The order is append-only: a line that arrives
// late with an older timestamp is placed at the end, not re-sorted.
type Merged struct {
	srcs []*Source

	mu    sync.RWMutex
	runs  []mrun
	total int64

	ver  atomic.Uint64
	idle atomic.Bool
	done chan struct{}
	wg   sync.WaitGroup
}

// mrun is a stretch of consecutive lines from one generation.
type mrun struct {
	start int64 // merged index of the first line
	src   int32
	gen   int32
	line  int64
	n     int64
}

type mcursor struct {
	gen    int
	it     *GenIter
	head   bool
	ts     int64
	line   int64
	lastTs int64
}

func NewMerged(srcs []*Source) *Merged {
	for i, s := range srcs {
		s.Index = i
	}
	m := &Merged{srcs: srcs, done: make(chan struct{})}
	m.wg.Add(1)
	go m.run()
	return m
}

func (m *Merged) Close() {
	close(m.done)
	m.wg.Wait()
	for _, s := range m.srcs {
		s.Close()
	}
}

func (m *Merged) fill(k int, c *mcursor) {
	src := m.srcs[k]
	for {
		if c.it == nil {
			g := src.gen(c.gen)
			if g == nil {
				return
			}
			c.it = g.Iter(0)
		}
		line, idx, lost, ok := c.it.Next()
		if !ok {
			g := src.gen(c.gen)
			if !g.Final() || src.gen(c.gen+1) == nil {
				return
			}
			// Final now: one more attempt picks up a just-completed partial line.
			if line, idx, lost, ok = c.it.Next(); !ok {
				c.gen++
				c.it = nil
				continue
			}
		}
		if lost {
			continue
		}
		t, _, has := ts.Parse(line)
		if has {
			c.lastTs = t
		} else {
			t = c.lastTs
		}
		c.ts, c.line, c.head = t, idx, true
		return
	}
}

func (m *Merged) run() {
	defer m.wg.Done()
	curs := make([]mcursor, len(m.srcs))
	last := -1
	var pend []mrun
	var pendN int64
	flush := func() {
		if len(pend) == 0 {
			return
		}
		m.mu.Lock()
		for _, r := range pend {
			if n := len(m.runs); n > 0 {
				l := &m.runs[n-1]
				if l.src == r.src && l.gen == r.gen && l.line+l.n == r.line {
					l.n += r.n
					continue
				}
			}
			r.start = m.total
			m.runs = append(m.runs, r)
			m.total += r.n
		}
		m.mu.Unlock()
		pend, pendN = pend[:0], 0
		m.ver.Add(1)
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		blocked := false
		for {
			best := -1
			blocked = false
			for k := range curs {
				c := &curs[k]
				if !c.head {
					m.fill(k, c)
				}
				if !c.head {
					if !m.srcs[k].Ready() {
						blocked = true
					}
					continue
				}
				if best < 0 || c.ts < curs[best].ts || (c.ts == curs[best].ts && k == last) {
					best = k
				}
			}
			if best < 0 || blocked {
				break
			}
			c := &curs[best]
			if n := len(pend); n > 0 && pend[n-1].src == int32(best) && pend[n-1].gen == int32(c.gen) && pend[n-1].line+pend[n-1].n == c.line {
				pend[n-1].n++
			} else {
				pend = append(pend, mrun{src: int32(best), gen: int32(c.gen), line: c.line, n: 1})
			}
			pendN++
			c.head = false
			last = best
			if pendN >= 8192 {
				flush()
				select {
				case <-m.done:
					return
				default:
				}
			}
		}
		flush()
		ready := !blocked
		for _, s := range m.srcs {
			ready = ready && s.Ready()
		}
		m.idle.Store(ready)
		select {
		case <-m.done:
			return
		case <-tick.C:
		}
	}
}

func (m *Merged) NumSegs() int { return 1 }

func (m *Merged) Info(seg int) SegInfo {
	return SegInfo{Label: "merged: " + joinNames(m.srcs)}
}

func (m *Merged) DisplayLen(seg int) int64 {
	if seg != 0 {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.total
}

func (m *Merged) ScanLen(seg int) int64 { return m.DisplayLen(seg) }

func (m *Merged) findRun(i int64) (mrun, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if i < 0 || i >= m.total {
		return mrun{}, false
	}
	k := sort.Search(len(m.runs), func(k int) bool { return m.runs[k].start > i }) - 1
	return m.runs[k], true
}

func (m *Merged) Line(seg int, i int64) ([]byte, Ref, error) {
	r, ok := m.findRun(i)
	if seg != 0 || !ok {
		return nil, Ref{}, ErrRange
	}
	line := r.line + (i - r.start)
	b, err := m.srcs[r.src].gen(int(r.gen)).Line(line)
	return b, Ref{Src: int(r.src), Gen: int(r.gen), Line: line}, err
}

type mergedIter struct {
	m     *Merged
	i     int64
	run   mrun
	have  bool
	iters map[[2]int32]*GenIter
}

func (m *Merged) Iter(seg int, from int64) Iter {
	if seg != 0 {
		return emptyIter{}
	}
	return &mergedIter{m: m, i: from, iters: map[[2]int32]*GenIter{}}
}

func (it *mergedIter) Next() ([]byte, Ref, bool, bool) {
	if !it.have || it.i < it.run.start || it.i >= it.run.start+it.run.n {
		r, ok := it.m.findRun(it.i)
		if !ok {
			return nil, Ref{}, false, false
		}
		it.run, it.have = r, true
	}
	r := it.run
	line := r.line + (it.i - r.start)
	key := [2]int32{r.src, r.gen}
	gi := it.iters[key]
	if gi == nil || gi.next != line {
		gi = it.m.srcs[r.src].gen(int(r.gen)).Iter(line)
		it.iters[key] = gi
	}
	b, _, lost, ok := gi.Next()
	if !ok {
		return nil, Ref{}, false, false
	}
	it.i++
	return b, Ref{Src: int(r.src), Gen: int(r.gen), Line: line}, lost, true
}

func (m *Merged) Version() uint64 { return m.ver.Load() }

func (m *Merged) Ready() bool { return m.idle.Load() }

func (m *Merged) Progress() (done, total int64) {
	for _, s := range m.srcs {
		d, t := s.Progress()
		done += d
		total += t
	}
	return
}

func (m *Merged) Sources() []*Source { return m.srcs }

func (m *Merged) Title() string {
	t := "merged: " + joinNames(m.srcs)
	for _, s := range m.srcs {
		if st := s.Status(); st != "" {
			t += " (" + s.Name + ": " + st + ")"
		}
	}
	return t
}
