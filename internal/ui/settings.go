package ui

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/nfelddav/logview/internal/logsrc"
)

// Settings are what is remembered between runs. Regexes are in the form
// they were compiled, so they reload identically.
type Settings struct {
	LineNumbers, Wrap, LevelColours bool

	Filters, Excludes, Subs, Highlights []string
	Cols, Record                        string
}

func (a *App) Settings() Settings {
	s := Settings{LineNumbers: a.lineNums, Wrap: a.wrap, LevelColours: a.defaultHL, Record: a.cfg.Record.String()}
	for _, re := range a.cfg.Includes {
		s.Filters = append(s.Filters, re.String())
	}
	for _, re := range a.cfg.Excludes {
		s.Excludes = append(s.Excludes, re.String())
	}
	for _, rw := range a.cfg.Rewrites {
		s.Subs = append(s.Subs, rw.Src)
	}
	for _, h := range a.hls {
		s.Highlights = append(s.Highlights, h.Src)
	}
	if a.cols != nil {
		s.Cols = a.cols.Spec
	}
	return s
}

func (s Settings) equal(o Settings) bool {
	return s.LineNumbers == o.LineNumbers && s.Wrap == o.Wrap && s.LevelColours == o.LevelColours &&
		slices.Equal(s.Filters, o.Filters) && slices.Equal(s.Excludes, o.Excludes) &&
		slices.Equal(s.Subs, o.Subs) && slices.Equal(s.Highlights, o.Highlights) &&
		s.Cols == o.Cols && s.Record == o.Record
}

// ShowError puts msg in the status line.
func (a *App) ShowError(msg string) { a.setErr("%s", msg) }

func (a *App) notifyChange() {
	if a.onChange == nil {
		return
	}
	if s := a.Settings(); !s.equal(a.lastSettings) {
		a.lastSettings = s
		if err := a.onChange(s); err != nil {
			a.setErr("saving settings: %v", err)
		}
	}
}

type confirm struct {
	question string
	yes      func()
}

// forgettable checks that generation g can be forgotten, reporting why not.
func (a *App) forgettable(g int) (logsrc.Forgetter, bool) {
	fg, ok := a.seq.(logsrc.Forgetter)
	if !ok {
		a.setErr("forgetting generations is not available when viewing several files")
		return nil, false
	}
	info := a.seq.Info(g)
	switch {
	case g < 0 || g >= a.seq.NumSegs() || info.Forgotten:
		a.setErr("no such generation")
		return nil, false
	case !info.Final:
		a.setErr("%s is still being written; only earlier generations can be forgotten", info.Label)
		return nil, false
	}
	return fg, true
}

func (a *App) askForget() {
	if !a.visible(a.cursor) {
		a.setErr("nothing to forget here")
		return
	}
	g := a.cursor.G
	if _, ok := a.forgettable(g); !ok {
		return
	}
	info := a.seq.Info(g)
	a.confirm = &confirm{
		question: fmt.Sprintf("Forget %s (%s lines)? Files on disk are not touched.", info.Label, human(a.seq.DisplayLen(g))),
		yes:      func() { a.forget(g) },
	}
}

func (a *App) forget(g int) bool {
	fg, ok := a.forgettable(g)
	if !ok {
		return false
	}
	label := a.seq.Info(g).Label
	if err := fg.Forget(g); err != nil {
		a.setErr("forget: %v", err)
		return false
	}
	if a.filter != nil {
		a.filter.DropSeg(g)
	}
	a.invalidate()
	c := a.cursor
	a.anchor = &c
	a.setMsg("forgot %s", label)
	return true
}

// forgetCommand handles ":forget", ":forget N" and ":forget old".
func (a *App) forgetCommand(arg string) {
	switch arg {
	case "":
		if !a.visible(a.cursor) {
			a.setErr("nothing to forget here")
			return
		}
		a.forget(a.cursor.G)
	case "old", "all":
		n := 0
		for g := 0; g < a.seq.NumSegs(); g++ {
			if info := a.seq.Info(g); info.Final && !info.Forgotten {
				if !a.forget(g) {
					return
				}
				n++
			}
		}
		if n == 0 {
			a.setMsg("no earlier generations to forget")
		} else {
			a.setMsg("forgot %d generations", n)
		}
	default:
		n, err := strconv.Atoi(arg)
		if err != nil {
			a.setErr("usage: forget [N|old]")
			return
		}
		a.forget(n - 1)
	}
}
