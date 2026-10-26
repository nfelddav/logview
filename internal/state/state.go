// Package state remembers settings between runs in a small JSON dotfile:
// display preferences globally, and filters / transforms per log.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	version = 1
	maxLogs = 200 // least recently used entries beyond this are dropped
)

type Display struct {
	LineNumbers  bool `json:"line_numbers"`
	Wrap         bool `json:"wrap"`
	LevelColours bool `json:"level_colours"`
}

// Rules are the per-log settings. Regexes are stored in Go syntax exactly
// as compiled, so they reload identically.
type Rules struct {
	Filters    []string  `json:"filters,omitempty"`
	Excludes   []string  `json:"excludes,omitempty"`
	Subs       []string  `json:"subs,omitempty"`
	Highlights []string  `json:"highlights,omitempty"`
	Cols       string    `json:"cols,omitempty"`
	Record     string    `json:"record,omitempty"`
	LastUsed   time.Time `json:"last_used"`
}

func (r *Rules) Empty() bool {
	return len(r.Filters)+len(r.Excludes)+len(r.Subs)+len(r.Highlights) == 0 &&
		r.Cols == "" && (r.Record == "" || r.Record == "auto")
}

type File struct {
	Version int               `json:"version"`
	Display Display           `json:"display"`
	Logs    map[string]*Rules `json:"logs,omitempty"`
}

func Default() *File {
	return &File{Version: version, Display: Display{LineNumbers: true, Wrap: true, LevelColours: true}, Logs: map[string]*Rules{}}
}

// DefaultPath is $LOGVIEW_STATE, else ~/.logview.json ("" if there is no home).
func DefaultPath() string {
	if p := os.Getenv("LOGVIEW_STATE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".logview.json")
}

// Load reads the state file. A missing file gives the defaults; an
// unreadable or corrupt one is an error (and should not be overwritten).
func Load(path string) (*File, error) {
	f := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Logs == nil {
		f.Logs = map[string]*Rules{}
	}
	return f, nil
}

// Update re-reads the file, applies fn and writes it back atomically, so
// concurrent viewers of different logs do not lose each other's entries.
func Update(path string, fn func(*File)) error {
	f, err := Load(path)
	if err != nil {
		return err
	}
	fn(f)
	f.Version = version
	prune(f)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".logview-state-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

func prune(f *File) {
	for k, r := range f.Logs {
		if r == nil || r.Empty() {
			delete(f.Logs, k)
		}
	}
	if len(f.Logs) <= maxLogs {
		return
	}
	keys := make([]string, 0, len(f.Logs))
	for k := range f.Logs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return f.Logs[keys[i]].LastUsed.After(f.Logs[keys[j]].LastUsed) })
	for _, k := range keys[maxLogs:] {
		delete(f.Logs, k)
	}
}
