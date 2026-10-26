package logsrc

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Options struct {
	// Preserve is how many bytes of a file's pre-existing content are copied
	// into the spool when it is opened (negative: all of it). Everything
	// appended afterwards is always spooled unless NoSpool is set.
	Preserve int64
	NoSpool  bool
	SpoolDir string
	Poll     time.Duration // fallback polling interval
	Grace    time.Duration // how long a rotated-away file is still drained
	Rotated  bool          // also load rotated siblings (app.log.1, app.log.2.gz, ...)
	NoTail   bool          // read what is there and stop
}

func (o Options) withDefaults() Options {
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Grace <= 0 {
		o.Grace = 3 * time.Second
	}
	return o
}

// Source is one log: its generations, followed by a tailer goroutine.
type Source struct {
	Name  string
	Path  string
	Index int // position among all sources; set by the caller

	opts Options

	mu   sync.RWMutex
	gens []*Gen

	ver             atomic.Uint64
	tailIdle        atomic.Bool
	pendingSiblings atomic.Int32
	status          atomic.Value // string

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// OpenFile starts following path. The file need not exist yet.
func OpenFile(path string, opts Options) *Source {
	opts = opts.withDefaults()
	s := &Source{Name: filepath.Base(path), Path: path, opts: opts, done: make(chan struct{})}
	s.status.Store("")
	if opts.Rotated {
		for _, sib := range FindSiblings(path) {
			s.addSibling(sib)
		}
	}
	s.wg.Add(1)
	go s.tail()
	return s
}

// OpenReader reads a stream (stdin) to EOF, keeping everything in the spool.
func OpenReader(name string, r io.Reader, opts Options) *Source {
	opts = opts.withDefaults()
	s := &Source{Name: name, opts: opts, done: make(chan struct{})}
	s.status.Store("")
	g := newGen(KindStream, name, nil, newSpoolOrMem(opts.SpoolDir), 0, &s.ver)
	s.addGen(g)
	go func() {
		buf := make([]byte, 256<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				s.tailIdle.Store(false)
				g.append(buf[:n])
			}
			if err != nil {
				g.finish()
				if err != io.EOF {
					s.setStatus("read error: " + err.Error())
				}
				s.tailIdle.Store(true)
				return
			}
			// A pipe has no "end of what is there now"; treat a short read as
			// caught up, except when asked to read everything (NoTail).
			s.tailIdle.Store(n < len(buf) && !opts.NoTail)
		}
	}()
	return s
}

func (s *Source) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, g := range s.gens {
			g.close()
		}
	})
}

func (s *Source) stopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Source) setStatus(msg string) { s.status.Store(msg); s.ver.Add(1) }

// Status describes a problem with the source, or "".
func (s *Source) Status() string { return s.status.Load().(string) }

// Ready reports whether everything currently on disk has been indexed.
func (s *Source) Ready() bool { return s.tailIdle.Load() && s.pendingSiblings.Load() == 0 }

func (s *Source) addGen(g *Gen) {
	s.mu.Lock()
	s.gens = append(s.gens, g)
	s.mu.Unlock()
	s.ver.Add(1)
}

func (s *Source) gen(i int) *Gen {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i < 0 || i >= len(s.gens) {
		return nil
	}
	return s.gens[i]
}

func (s *Source) addSibling(sib Sibling) {
	var g *Gen
	if sib.Compression == "" {
		f, err := openShared(sib.Path)
		if err != nil {
			return
		}
		g = newGen(KindSibling, filepath.Base(sib.Path), f, nil, 0, &s.ver)
	} else {
		g = newGen(KindSibling, filepath.Base(sib.Path), nil, newSpoolOrMem(s.opts.SpoolDir), 0, &s.ver)
	}
	s.addGen(g)
	s.pendingSiblings.Add(1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.pendingSiblings.Add(-1)
		defer g.finish()
		if err := s.indexSibling(g, sib); err != nil {
			g.setNote(err.Error())
		}
	}()
}

// siblingSem bounds concurrent sibling indexing (decompression is CPU bound).
var siblingSem = make(chan struct{}, 4)

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (s *Source) indexSibling(g *Gen, sib Sibling) error {
	siblingSem <- struct{}{}
	defer func() { <-siblingSem }()
	var f *os.File
	if sib.Compression == "" {
		f = g.file
	} else {
		var err error
		if f, err = openShared(sib.Path); err != nil {
			return err
		}
		defer f.Close()
	}
	if fi, err := f.Stat(); err == nil {
		g.progTotal.Store(fi.Size())
	}
	var r io.Reader = io.NewSectionReader(f, 0, 1<<62)
	if sib.Compression != "" {
		cr := countingReader{r, &g.progDone}
		switch sib.Compression {
		case "gz":
			zr, err := gzip.NewReader(cr)
			if err != nil {
				return err
			}
			r = zr
		case "bz2":
			r = bzip2.NewReader(cr)
		}
	}
	buf := make([]byte, 1<<20)
	for !s.stopped() {
		n, err := r.Read(buf)
		if n > 0 {
			g.append(buf[:n])
			if sib.Compression == "" {
				g.progDone.Add(int64(n))
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type tailState struct {
	f          *os.File
	g          *Gen
	off        int64
	fp         []byte // the last bytes read, to detect truncate-and-rewrite
	lastGrowth time.Time
}

const fingerprintLen = 64

func (s *Source) startLive(f *os.File, label string) *tailState {
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	var spool store
	threshold := int64(0)
	note := ""
	if !s.opts.NoSpool {
		if s.opts.Preserve >= 0 {
			threshold = size - s.opts.Preserve
		}
		var err error
		if spool, err = newSpool(s.opts.SpoolDir); err != nil {
			spool, note = nil, "no spool (contents lost on truncation): "+err.Error()
		}
	}
	g := newGen(KindLive, label, f, spool, threshold, &s.ver)
	g.note = note
	g.progTotal.Store(size)
	s.addGen(g)
	return &tailState{f: f, g: g, lastGrowth: time.Now()}
}

// readAvail appends everything currently readable. Each read starts at the
// fingerprint (the last bytes already read), so the same pread that fetches
// new data also proves the file was not truncated and rewritten underneath
// us: bytes from a new generation can never leak into the old one.
func (s *Source) readAvail(st *tailState, buf []byte) (grew, truncated bool) {
	for !s.stopped() {
		k := len(st.fp)
		n, err := st.f.ReadAt(buf, st.off-int64(k))
		if n < k || !bytes.Equal(buf[:k], st.fp) {
			if n < k && err != nil && err != io.EOF {
				break // transient I/O error; retry on the next tick
			}
			truncated = true
			break
		}
		if data := buf[k:n]; len(data) > 0 {
			st.g.append(data)
			st.off += int64(len(data))
			st.g.progDone.Store(st.off)
			from := n - fingerprintLen
			if from < 0 {
				from = 0
			}
			st.fp = append(st.fp[:0], buf[from:n]...)
			grew = true
		}
		if n < len(buf) || err != nil {
			break
		}
	}
	if grew {
		st.lastGrowth = time.Now()
	}
	return grew, truncated
}

func stamp() string { return time.Now().Format("15:04:05") }

func (s *Source) restartAfterTruncation(cur *tailState) *tailState {
	cur.g.freezeTruncated("truncated at " + stamp())
	return s.startLive(cur.f, fmt.Sprintf("%s (after truncation at %s)", s.Name, stamp()))
}

// checkReplaced detects rotation (a different file now at the path). It
// returns the state to keep following, or nil when the path must be reopened.
func (s *Source) checkReplaced(cur *tailState, draining *[]*tailState) *tailState {
	fi, err := cur.f.Stat()
	pfi, perr := os.Stat(s.Path)
	if perr == nil && err == nil && !os.SameFile(fi, pfi) {
		cur.g.setRotated("rotated away at " + stamp())
		cur.lastGrowth = time.Now()
		*draining = append(*draining, cur)
		return nil
	}
	return cur
}

func (s *Source) tail() {
	defer s.wg.Done()
	var events <-chan fsnotify.Event
	var errs <-chan error
	if !s.opts.NoTail {
		if w, err := fsnotify.NewWatcher(); err == nil {
			defer w.Close()
			if w.Add(filepath.Dir(s.Path)) == nil {
				events, errs = w.Events, w.Errors
			}
		}
	}
	tick := time.NewTicker(s.opts.Poll)
	defer tick.Stop()
	buf := make([]byte, 1<<20)
	var cur *tailState
	var draining []*tailState
	opened := false
	for {
		if cur == nil {
			f, err := openShared(s.Path)
			if err == nil {
				label := s.Name
				if opened {
					label = fmt.Sprintf("%s (new file after rotation at %s)", s.Name, stamp())
				}
				cur, opened = s.startLive(f, label), true
				s.setStatus("")
			} else if !opened {
				s.setStatus("waiting for file: " + err.Error())
			}
		}
		kept := draining[:0]
		for _, d := range draining {
			if _, trunc := s.readAvail(d, buf); trunc {
				d.g.freezeTruncated("truncated after rotation at " + stamp())
				continue
			}
			if time.Since(d.lastGrowth) > s.opts.Grace {
				d.g.finish()
			} else {
				kept = append(kept, d)
			}
		}
		draining = kept
		again := false
		if cur != nil {
			s.tailIdle.Store(false)
			_, trunc := s.readAvail(cur, buf)
			if s.opts.NoTail {
				cur.g.finish()
				s.tailIdle.Store(true)
				return
			}
			if trunc {
				cur, again = s.restartAfterTruncation(cur), true
			} else if cur = s.checkReplaced(cur, &draining); cur == nil {
				again = true
			}
		}
		if s.stopped() {
			return
		}
		s.tailIdle.Store(!again)
		if s.opts.NoTail && cur == nil && !again {
			s.tailIdle.Store(true)
			return
		}
		if again {
			continue
		}
		select {
		case <-s.done:
			return
		case <-events:
		case <-errs:
		case <-tick.C:
		}
	}
}

// Progress reports bytes indexed versus bytes known to exist at startup.
func (s *Source) Progress() (done, total int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.gens {
		d, t := g.progDone.Load(), g.progTotal.Load()
		if d > t {
			d = t
		}
		done += d
		total += t
	}
	return
}
