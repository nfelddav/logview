package query

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nfelddav/logview/internal/logsrc"
	"github.com/nfelddav/logview/internal/ts"
)

const (
	batchLines     = 2048
	maxRecordLines = 10000
)

// Filter evaluates a Config over a Seq in the background, keeping one
// bitset of visible lines per segment. Line matching runs on all cores;
// record grouping runs sequentially afterwards.
//
// Lines below a segment's frontier have been decided. A record that is
// still growing can change its mind later (a continuation line arrives
// that matches), so bits may be set — or, for excludes, cleared — behind
// the frontier.
type Filter struct {
	seq     logsrc.Seq
	cfg     *Config
	allMask uint64
	needTs  bool
	workers int

	mu   sync.RWMutex
	segs []*segState

	ver  atomic.Uint64
	stop chan struct{}
	wg   sync.WaitGroup
}

type segState struct {
	bits     Bitset
	frontier int64
	next     int64
	it       logsrc.Iter
	recs     map[int]*recState
	dropped  bool
}

type recState struct {
	started bool
	lines   []int64
	inc     uint64
	excl    bool
	ts      int64
	startTs bool
	vis     bool
	lastTs  int64
}

type lineEval struct {
	idx      int64
	stream   int
	lost     bool
	inc      uint64
	excl     bool
	ts       int64
	tsLead   bool
	leadWS   bool
	recStart bool
}

type batch struct {
	lines logsrc.Batch
	first int64
	evals []lineEval
}

var batchPool = sync.Pool{New: func() any { return &batch{} }}

func NewFilter(seq logsrc.Seq, cfg *Config) *Filter {
	f := &Filter{
		seq: seq, cfg: cfg,
		allMask: uint64(1)<<len(cfg.Includes) - 1,
		needTs:  cfg.Since != 0 || cfg.Until != 0 || cfg.Record.Kind == RecAuto,
		workers: max(1, runtime.GOMAXPROCS(0)),
		stop:    make(chan struct{}),
	}
	if len(cfg.Includes) >= 64 {
		f.allMask = ^uint64(0)
	}
	f.wg.Add(1)
	go f.run()
	return f
}

func (f *Filter) Stop() {
	close(f.stop)
	f.wg.Wait()
}

func (f *Filter) stopped() bool {
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

func (f *Filter) seg(g int) *segState {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.segs) <= g {
		k := len(f.segs)
		f.segs = append(f.segs, &segState{it: f.seq.Iter(k, 0), recs: map[int]*recState{}})
	}
	return f.segs[g]
}

func (f *Filter) run() {
	defer f.wg.Done()
	for !f.stopped() {
		progressed := false
		// Newest segments first: that is where people look.
		for g := f.seq.NumSegs() - 1; g >= 0 && !f.stopped(); g-- {
			st := f.seg(g)
			if st.next < f.seq.ScanLen(g) {
				f.scanChunk(st)
				progressed = true
				break
			}
		}
		if !progressed {
			select {
			case <-f.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// scanChunk reads batches while earlier batches are already being
// evaluated, then applies the results in order.
func (f *Filter) scanChunk(st *segState) {
	var batches []*batch
	var wg sync.WaitGroup
	sem := make(chan struct{}, f.workers)
	for len(batches) < f.workers*4 {
		b := batchPool.Get().(*batch)
		b.lines.Reset()
		b.first = st.next
		n := logsrc.ReadBatch(st.it, &b.lines, batchLines)
		if n == 0 {
			batchPool.Put(b)
			break
		}
		st.next += int64(n)
		batches = append(batches, b)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			f.evalBatch(b)
		}()
		if n < batchLines {
			break
		}
	}
	wg.Wait()
	f.mu.Lock()
	for _, b := range batches {
		for i := range b.evals {
			if !st.dropped {
				f.apply(st, &b.evals[i])
			}
		}
	}
	st.frontier = st.next
	f.mu.Unlock()
	for _, b := range batches {
		batchPool.Put(b)
	}
	f.ver.Add(1)
}

// DropSeg forgets every match in segment g (its lines were forgotten).
func (f *Filter) DropSeg(g int) {
	st := f.seg(g)
	f.mu.Lock()
	st.bits, st.recs, st.dropped = Bitset{}, map[int]*recState{}, true
	f.mu.Unlock()
	f.ver.Add(1)
}

func (f *Filter) evalBatch(b *batch) {
	n := b.lines.Len()
	b.evals = append(b.evals[:0], make([]lineEval, n)...)
	for i := range b.evals {
		e := &b.evals[i]
		e.idx, e.stream, e.lost = b.first+int64(i), b.lines.Refs[i].Src, b.lines.Lost[i]
		if !e.lost {
			f.eval(f.cfg.TextBytes(b.lines.Line(i)), e)
		}
	}
}

func (f *Filter) eval(text []byte, e *lineEval) {
	c := f.cfg
	for k, re := range c.Includes {
		if k < 64 && re.Match(text) {
			e.inc |= 1 << k
		}
	}
	for _, re := range c.Excludes {
		if re.Match(text) {
			e.excl = true
			break
		}
	}
	e.leadWS = len(text) > 0 && (text[0] == ' ' || text[0] == '\t')
	if f.needTs {
		if t, pos, ok := ts.Parse(text); ok {
			e.ts = t
			e.tsLead = pos <= 4 && !bytes.ContainsAny(text[:pos], " \t")
		}
	}
	if c.Record.Kind == RecRegex {
		e.recStart = c.Record.Re.Match(text)
	}
}

func (f *Filter) inTime(t int64) bool {
	c := f.cfg
	if c.Since == 0 && c.Until == 0 {
		return true
	}
	return t != 0 && (c.Since == 0 || t >= c.Since) && (c.Until == 0 || t <= c.Until)
}

func (f *Filter) apply(st *segState, e *lineEval) {
	rs := st.recs[e.stream]
	if rs == nil {
		rs = &recState{}
		st.recs[e.stream] = rs
	}
	if e.lost {
		rs.started = false
		return
	}
	start := true
	switch f.cfg.Record.Kind {
	case RecRegex:
		start = e.recStart
	case RecAuto:
		start = e.tsLead || (!e.leadWS && !rs.startTs)
	}
	if !rs.started || start || len(rs.lines) >= maxRecordLines {
		rs.started, rs.lines, rs.inc, rs.excl, rs.vis = true, rs.lines[:0], 0, false, false
		rs.startTs = e.tsLead
		rs.ts = e.ts
		if rs.ts == 0 {
			rs.ts = rs.lastTs
		}
	}
	if e.ts != 0 {
		rs.lastTs = e.ts
	}
	rs.lines = append(rs.lines, e.idx)
	rs.inc |= e.inc
	rs.excl = rs.excl || e.excl
	vis := rs.inc&f.allMask == f.allMask && !rs.excl && f.inTime(rs.ts)
	switch {
	case vis && !rs.vis:
		for _, l := range rs.lines {
			st.bits.Set(l)
		}
	case vis:
		st.bits.Set(e.idx)
	case rs.vis:
		for _, l := range rs.lines[:len(rs.lines)-1] {
			st.bits.Clear(l)
		}
	}
	rs.vis = vis
}

func (f *Filter) Version() uint64 { return f.ver.Load() }

func (f *Filter) Frontier(g int) int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if g < len(f.segs) {
		return f.segs[g].frontier
	}
	return 0
}

func (f *Filter) Visible(g int, i int64) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return g < len(f.segs) && i < f.segs[g].frontier && f.segs[g].bits.Test(i)
}

// Next returns the first visible line >= from in segment g, or -1.
func (f *Filter) Next(g int, from int64) int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if g >= len(f.segs) {
		return -1
	}
	st := f.segs[g]
	return st.bits.NextSet(from, st.frontier)
}

// Prev returns the last visible line <= from in segment g, or -1.
func (f *Filter) Prev(g int, from int64) int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if g >= len(f.segs) {
		return -1
	}
	st := f.segs[g]
	if from >= st.frontier {
		from = st.frontier - 1
	}
	return st.bits.PrevSet(from)
}

// Count returns the number of visible lines.
func (f *Filter) Count() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var n int64
	for _, st := range f.segs {
		n += st.bits.Count()
	}
	return n
}

// Rank returns the number of visible lines before (g, i).
func (f *Filter) Rank(g int, i int64) int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var n int64
	for k := 0; k < g && k < len(f.segs); k++ {
		n += f.segs[k].bits.Count()
	}
	if g < len(f.segs) {
		n += f.segs[g].bits.Rank(i)
	}
	return n
}

// Progress returns lines scanned and lines available.
func (f *Filter) Progress() (done, total int64) {
	for g := 0; g < f.seq.NumSegs(); g++ {
		total += f.seq.ScanLen(g)
		done += f.Frontier(g)
	}
	return
}

// Done reports whether everything currently available has been scanned.
func (f *Filter) Done() bool {
	d, t := f.Progress()
	return d >= t && f.seq.Ready()
}
