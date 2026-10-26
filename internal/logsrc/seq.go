package logsrc

import (
	"errors"
	"strings"
)

// Ref identifies where a line came from.
type Ref struct {
	Src  int
	Gen  int
	Line int64
}

// SegInfo describes one segment of a Seq.
type SegInfo struct {
	Label string
	Note  string
	Kind  GenKind
	Lost  int64 // lines [0, Lost) were not preserved
	Final bool
	Bytes int64

	Forgotten bool // dropped by Forget; has no lines
}

// Forgetter is implemented by Seqs whose segments can be dropped.
type Forgetter interface {
	Forget(seg int) error
}

var ErrNotFinal = errors.New("still being written; only earlier generations can be forgotten")

// Forget drops a finished generation, releasing its file handle and spool.
// Files on disk are not touched.
func (s *Source) Forget(seg int) error {
	g := s.gen(seg)
	if g == nil {
		return ErrRange
	}
	if !g.Final() {
		return ErrNotFinal
	}
	g.forget()
	return nil
}

// Seq is what the viewer and filters work on: an ordered list of segments,
// each an append-only sequence of lines. A single source exposes its
// generations as segments; a merge of several sources is one segment.
type Seq interface {
	NumSegs() int
	Info(seg int) SegInfo
	DisplayLen(seg int) int64 // includes a trailing partial line
	ScanLen(seg int) int64    // complete lines only
	Line(seg int, i int64) ([]byte, Ref, error)
	Iter(seg int, from int64) Iter
	Version() uint64
	Ready() bool
	Progress() (done, total int64)
	Sources() []*Source
	Title() string
}

// Iter reads a segment sequentially; see GenIter.Next.
type Iter interface {
	Next() (line []byte, ref Ref, lost bool, ok bool)
}

// Batch holds copies of consecutive lines read from an Iter.
type Batch struct {
	Arena []byte
	Ends  []int
	Refs  []Ref
	Lost  []bool
}

func (b *Batch) Reset() {
	b.Arena, b.Ends, b.Refs, b.Lost = b.Arena[:0], b.Ends[:0], b.Refs[:0], b.Lost[:0]
}

func (b *Batch) Len() int { return len(b.Ends) }

func (b *Batch) Line(i int) []byte {
	start := 0
	if i > 0 {
		start = b.Ends[i-1]
	}
	return b.Arena[start:b.Ends[i]]
}

func (b *Batch) add(line []byte, ref Ref, lost bool) {
	b.Arena = append(b.Arena, line...)
	b.Ends = append(b.Ends, len(b.Arena))
	b.Refs = append(b.Refs, ref)
	b.Lost = append(b.Lost, lost)
}

type batcher interface {
	nextBatch(b *Batch, max int) int
}

// ReadBatch appends up to max lines from it to b and returns how many.
func ReadBatch(it Iter, b *Batch, max int) int {
	if bi, ok := it.(batcher); ok {
		return bi.nextBatch(b, max)
	}
	n := 0
	for ; n < max; n++ {
		line, ref, lost, ok := it.Next()
		if !ok {
			break
		}
		b.add(line, ref, lost)
	}
	return n
}

func (s *Source) NumSegs() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.gens)
}

func (s *Source) Info(seg int) SegInfo {
	if g := s.gen(seg); g != nil {
		return g.info()
	}
	return SegInfo{}
}

func (s *Source) DisplayLen(seg int) int64 {
	if g := s.gen(seg); g != nil {
		return g.DisplayLen()
	}
	return 0
}

func (s *Source) ScanLen(seg int) int64 {
	if g := s.gen(seg); g != nil {
		return g.ScanLen()
	}
	return 0
}

func (s *Source) Line(seg int, i int64) ([]byte, Ref, error) {
	ref := Ref{Src: s.Index, Gen: seg, Line: i}
	g := s.gen(seg)
	if g == nil {
		return nil, ref, ErrRange
	}
	b, err := g.Line(i)
	return b, ref, err
}

type srcIter struct {
	src, seg int
	it       *GenIter
}

func (it *srcIter) Next() ([]byte, Ref, bool, bool) {
	b, idx, lost, ok := it.it.Next()
	return b, Ref{Src: it.src, Gen: it.seg, Line: idx}, lost, ok
}

func (it *srcIter) nextBatch(b *Batch, max int) int {
	start := len(b.Refs)
	n := it.it.nextBatch(b, it.src, max)
	for i := start; i < len(b.Refs); i++ {
		b.Refs[i].Gen = it.seg
	}
	return n
}

type emptyIter struct{}

func (emptyIter) Next() ([]byte, Ref, bool, bool) { return nil, Ref{}, false, false }

func (s *Source) Iter(seg int, from int64) Iter {
	g := s.gen(seg)
	if g == nil {
		return emptyIter{}
	}
	return &srcIter{src: s.Index, seg: seg, it: g.Iter(from)}
}

func (s *Source) Version() uint64    { return s.ver.Load() }
func (s *Source) Sources() []*Source { return []*Source{s} }

func (s *Source) Title() string {
	if st := s.Status(); st != "" {
		return s.Name + " (" + st + ")"
	}
	return s.Name
}

func joinNames(srcs []*Source) string {
	names := make([]string, len(srcs))
	for i, s := range srcs {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}
