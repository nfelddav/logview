package logsrc

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

const (
	// blockLines is the index granularity: one offset is stored per block,
	// so a 500M-line file costs ~30MB of index.
	blockLines = 128
	// MaxLineLen caps how much of a single line is ever read.
	MaxLineLen = 1 << 20

	iterChunk       = 256 << 10
	blockCacheLimit = 1024
)

var (
	ErrLost  = errors.New("line not preserved")
	ErrRange = errors.New("line out of range")
)

type GenKind uint8

const (
	KindLive      GenKind = iota // the file currently at the path
	KindRotated                  // a file that was renamed away while we followed it
	KindTruncated                // contents before a truncation; only the spooled part survives
	KindSibling                  // a rotated file found on disk at startup
	KindStream                   // stdin
)

// Gen is one generation of a log: a contiguous byte stream that only grows.
// When the file at the path is truncated or replaced, the old Gen is frozen
// and a new one starts, so line numbers inside a Gen never change.
//
// Bytes come from the original file while it is trustworthy, otherwise from
// the spool, which holds a copy of everything from spoolBase onwards.
type Gen struct {
	ver *atomic.Uint64

	mu    sync.RWMutex
	label string
	note  string
	kind  GenKind
	file  *os.File // nil once the file no longer holds this generation's bytes
	spool store

	spoolThreshold int64 // the first line starting at or after this offset begins the spool
	spoolBase      int64 // offset of the first spooled byte; -1 until spooling has begun
	spoolLine      int64 // index of the line starting at spoolBase

	cps    []int64 // cps[k] = offset of line k*blockLines
	nlines int64   // complete (newline-terminated) lines
	endOff int64   // offset just past the last complete line
	size   int64   // bytes appended so far
	final  bool    // no more bytes will arrive; a trailing partial line counts

	forgotten bool // dropped at the user's request; holds no lines

	progDone, progTotal atomic.Int64

	cacheMu    sync.Mutex
	cache      map[int64][]int64
	cacheOrder []int64
}

func newGen(kind GenKind, label string, file *os.File, spool store, threshold int64, ver *atomic.Uint64) *Gen {
	g := &Gen{
		ver: ver, kind: kind, label: label, file: file, spool: spool,
		spoolThreshold: threshold, spoolBase: -1,
		cps:   []int64{0},
		cache: map[int64][]int64{},
	}
	if spool != nil && threshold <= 0 {
		g.spoolBase = 0
	}
	return g
}

// append adds bytes to the generation. Only one goroutine appends to a Gen.
func (g *Gen) append(data []byte) {
	g.mu.Lock()
	base := g.size
	spoolFrom := -1
	if g.spool != nil && g.spoolBase >= 0 {
		spoolFrom = 0
	}
	for rel := 0; ; {
		k := bytes.IndexByte(data[rel:], '\n')
		if k < 0 {
			break
		}
		p := rel + k
		g.nlines++
		g.endOff = base + int64(p) + 1
		if g.nlines%blockLines == 0 {
			g.cps = append(g.cps, g.endOff)
		}
		if g.spool != nil && g.spoolBase < 0 && g.endOff >= g.spoolThreshold {
			g.spoolBase, g.spoolLine = g.endOff, g.nlines
			spoolFrom = p + 1
		}
		rel = p + 1
	}
	if spoolFrom >= 0 && spoolFrom < len(data) {
		if err := g.spool.Append(data[spoolFrom:]); err != nil {
			g.spool.Close()
			g.spool, g.spoolBase = nil, -1
			g.note = "spool write failed: " + err.Error()
		}
	}
	g.size += int64(len(data))
	g.mu.Unlock()
	g.ver.Add(1)
}

func (g *Gen) finish() {
	g.mu.Lock()
	g.final = true
	g.mu.Unlock()
	g.ver.Add(1)
}

// freezeTruncated marks the original file as no longer holding our bytes.
func (g *Gen) freezeTruncated(note string) {
	g.mu.Lock()
	g.file, g.final, g.kind, g.note = nil, true, KindTruncated, note
	g.mu.Unlock()
	g.ver.Add(1)
}

func (g *Gen) setRotated(note string) {
	g.mu.Lock()
	g.kind, g.note = KindRotated, note
	g.mu.Unlock()
	g.ver.Add(1)
}

func (g *Gen) setNote(note string) {
	g.mu.Lock()
	g.note = note
	g.mu.Unlock()
	g.ver.Add(1)
}

func (g *Gen) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.file != nil {
		g.file.Close()
	}
	if g.spool != nil {
		g.spool.Close()
	}
}

// forget empties a finished generation and releases its file handle and
// spool. The Gen stays in place (as zero lines) so that the numbering of
// the other generations does not change.
func (g *Gen) forget() {
	g.mu.Lock()
	if g.file != nil {
		g.file.Close()
	}
	if g.spool != nil {
		g.spool.Close()
	}
	g.file, g.spool, g.spoolBase = nil, nil, -1
	g.cps, g.nlines, g.endOff, g.size = nil, 0, 0, 0
	g.forgotten = true
	g.mu.Unlock()
	g.cacheMu.Lock()
	clear(g.cache)
	g.cacheOrder = nil
	g.cacheMu.Unlock()
	g.progDone.Store(0)
	g.progTotal.Store(0)
	g.ver.Add(1)
}

func (g *Gen) displayLenLocked() int64 {
	if g.size > g.endOff {
		return g.nlines + 1
	}
	return g.nlines
}

func (g *Gen) scanLenLocked() int64 {
	if g.final && g.size > g.endOff {
		return g.nlines + 1
	}
	return g.nlines
}

func (g *Gen) lostLocked() int64 {
	if g.file != nil {
		return 0
	}
	if g.spool != nil && g.spoolBase >= 0 {
		return g.spoolLine
	}
	return g.displayLenLocked()
}

// DisplayLen counts lines including a trailing partial line still being written.
func (g *Gen) DisplayLen() int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.displayLenLocked()
}

// ScanLen counts lines that are complete and will not change.
func (g *Gen) ScanLen() int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.scanLenLocked()
}

func (g *Gen) Final() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.final
}

func (g *Gen) info() SegInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return SegInfo{Label: g.label, Note: g.note, Kind: g.kind, Lost: g.lostLocked(), Final: g.final, Bytes: g.size, Forgotten: g.forgotten}
}

func (g *Gen) readAtLocked(p []byte, off int64) (int, error) {
	if g.file != nil {
		return g.file.ReadAt(p, off)
	}
	if g.spool != nil && g.spoolBase >= 0 && off >= g.spoolBase {
		return g.spool.ReadAt(p, off-g.spoolBase)
	}
	return 0, ErrLost
}

// seekLocked returns the nearest known line start at or before line i.
func (g *Gen) seekLocked(i int64) (line, off int64) {
	c := i / blockLines
	line, off = c*blockLines, g.cps[c]
	if g.file == nil && g.spoolBase >= 0 && off < g.spoolBase {
		line, off = g.spoolLine, g.spoolBase
	}
	return
}

var blockBufPool = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

// blockOffsetsLocked returns the start offsets of the complete lines in
// block c plus one trailing end offset. Unreadable entries are -1.
func (g *Gen) blockOffsetsLocked(c int64) ([]int64, error) {
	first := c * blockLines
	last := first + blockLines
	complete := last <= g.nlines
	if !complete {
		last = g.nlines
	}
	if complete {
		g.cacheMu.Lock()
		offs, ok := g.cache[c]
		g.cacheMu.Unlock()
		if ok {
			return offs, nil
		}
	}
	offs := make([]int64, last-first+1)
	line, off := g.seekLocked(first)
	if line < first {
		line, off = first, g.cps[c]
	}
	for k := first; k < line && k <= last; k++ {
		offs[k-first] = -1
	}
	if line > last {
		return offs, nil
	}
	offs[line-first] = off
	bp := blockBufPool.Get().(*[]byte)
	defer blockBufPool.Put(bp)
	buf := *bp
	for readPos := off; line < last; {
		n, err := g.readAtLocked(buf, readPos)
		if n == 0 {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		chunk := buf[:n]
		for rel := 0; line < last; {
			k := bytes.IndexByte(chunk[rel:], '\n')
			if k < 0 {
				break
			}
			rel += k + 1
			line++
			offs[line-first] = readPos + int64(rel)
		}
		readPos += int64(n)
	}
	if complete {
		g.cacheMu.Lock()
		if len(g.cacheOrder) >= blockCacheLimit {
			delete(g.cache, g.cacheOrder[0])
			g.cacheOrder = g.cacheOrder[1:]
		}
		g.cache[c] = offs
		g.cacheOrder = append(g.cacheOrder, c)
		g.cacheMu.Unlock()
	}
	return offs, nil
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// Line returns a copy of line i without its terminator.
func (g *Gen) Line(i int64) ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if i < 0 || i >= g.displayLenLocked() {
		return nil, ErrRange
	}
	if i < g.lostLocked() {
		return nil, ErrLost
	}
	var start, end int64
	if i == g.nlines {
		start, end = g.endOff, g.size
	} else {
		offs, err := g.blockOffsetsLocked(i / blockLines)
		if err != nil {
			return nil, err
		}
		k := i % blockLines
		if offs[k] < 0 {
			return nil, ErrLost
		}
		start, end = offs[k], offs[k+1]-1
	}
	if end-start > MaxLineLen {
		end = start + MaxLineLen
	}
	b := make([]byte, end-start)
	n, err := g.readAtLocked(b, start)
	if n < len(b) {
		if n == 0 && err != nil && err != io.EOF {
			return nil, err
		}
		b = b[:n]
	}
	return trimCR(b), nil
}

// GenIter reads lines sequentially, much faster than repeated Line calls.
type GenIter struct {
	g          *Gen
	next       int64
	off        int64
	positioned bool
	buf        []byte // window of bytes starting at bufOff
	bufOff     int64
	long       []byte
}

func (g *Gen) Iter(from int64) *GenIter {
	return &GenIter{g: g, next: from, buf: make([]byte, 0, 2*iterChunk)}
}

// Next returns the next line. The slice is only valid until the next call.
// ok is false when no further complete line is available yet; calling Next
// again later resumes. lost reports a line whose bytes were not preserved.
func (it *GenIter) Next() (line []byte, idx int64, lost bool, ok bool) {
	it.g.mu.RLock()
	defer it.g.mu.RUnlock()
	return it.nextLocked(it.g.scanLenLocked())
}

// nextBatch appends up to max lines to b under a single lock.
func (it *GenIter) nextBatch(b *Batch, src int, max int) int {
	g := it.g
	g.mu.RLock()
	defer g.mu.RUnlock()
	limit := g.scanLenLocked()
	n := 0
	for ; n < max; n++ {
		line, idx, lost, ok := it.nextLocked(limit)
		if !ok {
			break
		}
		b.add(line, Ref{Src: src, Gen: -1, Line: idx}, lost)
	}
	return n
}

func (it *GenIter) nextLocked(limit int64) (line []byte, idx int64, lost bool, ok bool) {
	g := it.g
	idx = it.next
	if idx >= limit {
		return nil, idx, false, false
	}
	it.next++
	if idx < g.lostLocked() {
		it.positioned = false
		return nil, idx, true, true
	}
	if !it.positioned {
		l, off := g.seekLocked(idx)
		it.off = off
		for ; l < idx; l++ {
			if _, err := it.readLineLocked(false); err != nil {
				return nil, idx, true, true
			}
		}
		it.positioned = true
	}
	b, err := it.readLineLocked(idx == g.nlines)
	if err != nil {
		it.positioned = false
		return nil, idx, true, true
	}
	return trimCR(b), idx, false, true
}

func (it *GenIter) readLineLocked(partial bool) ([]byte, error) {
	g := it.g
	if partial {
		n := g.size - it.off
		if n > MaxLineLen {
			n = MaxLineLen
		}
		b := make([]byte, n)
		m, err := g.readAtLocked(b, it.off)
		if m == 0 && n > 0 {
			return nil, err
		}
		it.off = g.size
		return b[:m], nil
	}
	longMode := false
	it.long = it.long[:0]
	for {
		rel := it.off - it.bufOff
		if rel < 0 || rel > int64(len(it.buf)) {
			it.buf, it.bufOff, rel = it.buf[:0], it.off, 0
		}
		if k := bytes.IndexByte(it.buf[rel:], '\n'); k >= 0 {
			line := it.buf[rel : rel+int64(k)]
			it.off += int64(k) + 1
			if longMode {
				return appendCapped(it.long, line), nil
			}
			return line, nil
		}
		avail := it.buf[rel:]
		if len(avail) >= iterChunk {
			it.long = appendCapped(it.long, avail)
			longMode = true
			it.off += int64(len(avail))
			it.buf, it.bufOff = it.buf[:0], it.off
		} else {
			n := copy(it.buf[:cap(it.buf)], avail)
			it.buf, it.bufOff = it.buf[:n], it.off
		}
		n, err := g.readAtLocked(it.buf[len(it.buf):cap(it.buf)], it.bufOff+int64(len(it.buf)))
		if n == 0 {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		it.buf = it.buf[:len(it.buf)+n]
	}
}

func appendCapped(dst, src []byte) []byte {
	if room := MaxLineLen - len(dst); room < len(src) {
		if room <= 0 {
			return dst
		}
		src = src[:room]
	}
	return append(dst, src...)
}
