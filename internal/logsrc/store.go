package logsrc

import (
	"io"
	"os"
	"runtime"
	"sync"
)

// store is append-only byte storage with random reads: the spool.
type store interface {
	io.ReaderAt
	Append(p []byte) error
	Close() error
}

type fileStore struct {
	f             *os.File
	n             int64 // only touched by the single appending goroutine
	path          string
	removeOnClose bool
}

func (s *fileStore) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }

func (s *fileStore) Append(p []byte) error {
	n, err := s.f.WriteAt(p, s.n)
	s.n += int64(n)
	return err
}

func (s *fileStore) Close() error {
	err := s.f.Close()
	if s.removeOnClose {
		os.Remove(s.path)
	}
	return err
}

type memStore struct {
	mu sync.RWMutex
	b  []byte
}

func (s *memStore) ReadAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if off >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *memStore) Append(p []byte) error {
	s.mu.Lock()
	s.b = append(s.b, p...)
	s.mu.Unlock()
	return nil
}

func (s *memStore) Close() error { return nil }

// newSpool creates an anonymous temporary spool file. On Unix the file is
// unlinked immediately, so it disappears even if the viewer is killed.
func newSpool(dir string) (store, error) {
	f, err := os.CreateTemp(dir, "logview-spool-*")
	if err != nil {
		return nil, err
	}
	s := &fileStore{f: f, path: f.Name()}
	if runtime.GOOS == "windows" {
		s.removeOnClose = true
	} else {
		os.Remove(f.Name())
	}
	return s, nil
}

// newSpoolOrMem is for data that exists nowhere else (stdin, decompressed
// files): fall back to memory if no spool file can be created.
func newSpoolOrMem(dir string) store {
	if s, err := newSpool(dir); err == nil {
		return s
	}
	return &memStore{}
}
