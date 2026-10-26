package ui

import (
	"bufio"

	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"regexp"

	"github.com/gdamore/tcell/v2"

	"github.com/nfelddav/logview/internal/logsrc"
	"github.com/nfelddav/logview/internal/query"
	"github.com/nfelddav/logview/internal/ts"
)

type prompt struct {
	label string
	kind  string // "search" or "cmd"
	buf   []rune
	pos   int
	hist  int
}

func (a *App) openPrompt(label, kind, initial string) {
	a.prompt = &prompt{label: label, kind: kind, buf: []rune(initial), pos: len([]rune(initial)), hist: len(a.history[kind])}
}

func (a *App) drawPrompt(y int) {
	p := a.prompt
	x := a.putStr(0, y, a.width, p.label, tcell.StyleDefault.Bold(true))
	start := x
	text := string(p.buf)
	a.putStr(x, y, a.width, text, tcell.StyleDefault)
	cx := start
	for _, r := range p.buf[:p.pos] {
		cx += runeWidth(r)
	}
	a.scr.ShowCursor(min(cx, a.width-1), y)
}

func (a *App) promptKey(ev *tcell.EventKey) {
	p := a.prompt
	hist := a.history[p.kind]
	switch ev.Key() {
	case tcell.KeyEscape, tcell.KeyCtrlC:
		a.prompt = nil
		if p.kind == "search" {
			a.searchRe = nil
			a.invalidate()
		}
		return
	case tcell.KeyEnter:
		a.prompt = nil
		text := strings.TrimSpace(string(p.buf))
		if text != "" && (len(hist) == 0 || hist[len(hist)-1] != text) {
			a.history[p.kind] = append(hist, text)
		}
		if p.kind == "search" {
			a.startSearch(text)
		} else {
			a.runCommand(text)
		}
		return
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if p.pos > 0 {
			p.buf = append(p.buf[:p.pos-1], p.buf[p.pos:]...)
			p.pos--
		} else if len(p.buf) == 0 {
			a.prompt = nil
			return
		}
	case tcell.KeyDelete:
		if p.pos < len(p.buf) {
			p.buf = append(p.buf[:p.pos], p.buf[p.pos+1:]...)
		}
	case tcell.KeyLeft:
		p.pos = max(p.pos-1, 0)
	case tcell.KeyRight:
		p.pos = min(p.pos+1, len(p.buf))
	case tcell.KeyHome, tcell.KeyCtrlA:
		p.pos = 0
	case tcell.KeyEnd, tcell.KeyCtrlE:
		p.pos = len(p.buf)
	case tcell.KeyCtrlU:
		p.buf, p.pos = p.buf[p.pos:], 0
	case tcell.KeyCtrlW:
		i := p.pos
		for i > 0 && p.buf[i-1] == ' ' {
			i--
		}
		for i > 0 && p.buf[i-1] != ' ' {
			i--
		}
		p.buf, p.pos = append(p.buf[:i], p.buf[p.pos:]...), i
	case tcell.KeyUp:
		if p.hist > 0 {
			p.hist--
			p.buf = []rune(hist[p.hist])
			p.pos = len(p.buf)
		}
	case tcell.KeyDown:
		if p.hist < len(hist)-1 {
			p.hist++
			p.buf = []rune(hist[p.hist])
		} else {
			p.hist, p.buf = len(hist), nil
		}
		p.pos = len(p.buf)
	case tcell.KeyRune:
		p.buf = append(p.buf[:p.pos], append([]rune{ev.Rune()}, p.buf[p.pos:]...)...)
		p.pos++
	}
	if p.kind == "search" { // highlight as you type
		if re, err := query.Compile(string(p.buf)); err == nil {
			a.searchRe = re
		} else {
			a.searchRe = nil
		}
		a.invalidate()
	}
}

func (a *App) startSearch(pattern string) {
	if pattern == "" {
		if a.searchRe == nil {
			return
		}
	} else {
		re, err := query.Compile(pattern)
		if err != nil {
			a.setErr("bad pattern: %v", err)
			return
		}
		a.searchRe = re
	}
	a.invalidate()
	a.searchNext(true)
}

// searchNext searches in the remembered direction (same=true) or the opposite one.
func (a *App) searchNext(same bool) {
	if a.searchRe == nil {
		a.setErr("no search pattern")
		return
	}
	if a.searchCancel != nil {
		a.searchCancel.Store(true)
	}
	forward := a.searchFwd == same
	cancel := &atomic.Bool{}
	a.searchCancel, a.searchRunning = cancel, true
	seq, cfg, re, from := a.seq, a.cfg, a.searchRe, a.cursor
	var vis func(int, int64) bool
	if a.filter != nil {
		vis = a.filter.Visible
	}
	go func() {
		p, wrapped, ok := query.Search(seq, cfg, re, from, forward, vis, cancel, nil)
		a.async <- func() {
			if a.searchCancel != cancel {
				return
			}
			a.searchRunning, a.searchCancel = false, nil
			switch {
			case cancel.Load():
				a.setMsg("search cancelled")
			case !ok:
				a.setErr("pattern not found: %s", strings.TrimPrefix(re.String(), "(?i)"))
			default:
				a.follow = false
				a.userMoved()
				a.cursor = p
				if wrapped {
					a.setMsg("search wrapped")
				}
			}
		}
	}()
}

func (a *App) addRule(kind ruleKind) {
	a.undo = append(a.undo, kind)
}

func (a *App) undoRule() {
	if len(a.undo) == 0 {
		a.setMsg("nothing to undo")
		return
	}
	k := a.undo[len(a.undo)-1]
	a.undo = a.undo[:len(a.undo)-1]
	a.copyConfig()
	switch k {
	case ruleInclude:
		a.cfg.Includes = a.cfg.Includes[:len(a.cfg.Includes)-1]
		a.restartFilter()
	case ruleExclude:
		a.cfg.Excludes = a.cfg.Excludes[:len(a.cfg.Excludes)-1]
		a.restartFilter()
	case ruleRewrite:
		a.cfg.Rewrites = a.cfg.Rewrites[:len(a.cfg.Rewrites)-1]
		a.restartFilter()
	case ruleHighlight:
		a.hls = a.hls[:len(a.hls)-1]
		a.invalidate()
	}
	a.setMsg("removed last rule")
}

// copyConfig gives the running filter an immutable snapshot.
func (a *App) copyConfig() {
	c := *a.cfg
	c.Includes = append([]*regexp.Regexp(nil), c.Includes...)
	c.Excludes = append([]*regexp.Regexp(nil), c.Excludes...)
	c.Rewrites = append([]query.Rewrite(nil), c.Rewrites...)
	a.cfg = &c
}

func (a *App) refTime() time.Time {
	if raw, _, err := a.seq.Line(a.cursor.G, a.cursor.I); err == nil {
		if t, _, ok := ts.Parse([]byte(a.cfg.Text(raw))); ok {
			return time.Unix(0, t)
		}
	}
	return time.Now()
}

func (a *App) runCommand(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "q", "quit", "exit":
		a.quit = true
	case "filter", "f", "include":
		re, err := query.Compile(arg)
		if err != nil {
			a.setErr("filter: %v", err)
			return
		}
		if len(a.cfg.Includes) >= query.MaxIncludes {
			a.setErr("too many filters")
			return
		}
		a.copyConfig()
		a.cfg.Includes = append(a.cfg.Includes, re)
		a.addRule(ruleInclude)
		a.restartFilter()
	case "exclude", "x", "hide":
		re, err := query.Compile(arg)
		if err != nil {
			a.setErr("exclude: %v", err)
			return
		}
		a.copyConfig()
		a.cfg.Excludes = append(a.cfg.Excludes, re)
		a.addRule(ruleExclude)
		a.restartFilter()
	case "hl", "highlight":
		h, err := query.ParseHighlight(arg, len(a.hls))
		if err != nil {
			a.setErr("hl: %v", err)
			return
		}
		a.hls = append(a.hls, h)
		a.addRule(ruleHighlight)
		a.invalidate()
	case "sub", "s":
		rw, err := query.ParseSub(arg)
		if err != nil {
			a.setErr("sub: %v", err)
			return
		}
		a.copyConfig()
		a.cfg.Rewrites = append(a.cfg.Rewrites, rw)
		a.addRule(ruleRewrite)
		a.restartFilter()
	case "cols", "columns":
		if arg == "" || arg == "off" {
			a.cols = nil
		} else {
			c, err := query.ParseColumns(arg)
			if err != nil {
				a.setErr("cols: %v", err)
				return
			}
			a.cols = c
		}
		a.invalidate()
	case "since", "after", "until", "before":
		var v int64
		if arg != "" && arg != "off" {
			t, err := ts.ParseUser(arg, a.refTime())
			if err != nil {
				a.setErr("%s: %v", name, err)
				return
			}
			v = t.UnixNano()
		}
		a.copyConfig()
		if name == "since" || name == "after" {
			a.cfg.Since = v
		} else {
			a.cfg.Until = v
		}
		a.restartFilter()
	case "time", "t", "jump":
		t, err := ts.ParseUser(arg, a.refTime())
		if err != nil {
			a.setErr("time: %v", err)
			return
		}
		a.jumpTime(t.UnixNano())
	case "record", "records", "rec":
		m, err := query.ParseRecordMode(arg)
		if err != nil {
			a.setErr("record: %v", err)
			return
		}
		a.copyConfig()
		a.cfg.Record = m
		a.restartFilter()
		a.setMsg("records: %s", m)
	case "clear":
		a.copyConfig()
		switch arg {
		case "", "all":
			a.cfg.Includes, a.cfg.Excludes, a.cfg.Rewrites = nil, nil, nil
			a.cfg.Since, a.cfg.Until = 0, 0
			a.hls, a.cols, a.undo = nil, nil, nil
		case "filter", "filters":
			a.cfg.Includes, a.cfg.Excludes = nil, nil
		case "hl", "highlights":
			a.hls = nil
		case "sub", "subs":
			a.cfg.Rewrites = nil
		case "cols":
			a.cols = nil
		case "time":
			a.cfg.Since, a.cfg.Until = 0, 0
		default:
			a.setErr("clear what? all|filter|hl|sub|cols|time")
			return
		}
		a.pruneUndo()
		a.restartFilter()
	case "undo":
		a.undoRule()
	case "goto", "line":
		n, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || n < 1 {
			a.setErr("goto: need a line number")
			return
		}
		a.follow = false
		a.userMoved()
		g := a.cursor.G
		if g < 0 || g >= a.seq.NumSegs() {
			g = 0
		}
		target := Pos{G: g, I: min(n-1, max(a.seq.DisplayLen(g)-1, 0))}
		if p, ok := a.atOrAfter(target); ok {
			a.cursor = p
		} else if p, ok := a.atOrBefore(target); ok {
			a.cursor = p
		}
	case "w", "write", "save":
		a.export(arg)
	case "json":
		a.jsonAll = !a.jsonAll
		clear(a.jsonToggled)
	case "wrap":
		a.wrap = !a.wrap
	case "nums", "numbers":
		a.lineNums = !a.lineNums
	case "defaulthl":
		a.defaultHL = arg != "off"
		a.invalidate()
	case "follow":
		a.follow = !a.follow
	case "forget":
		a.forgetCommand(arg)
	case "help":
		a.openHelp()
	default:
		a.setErr("unknown command %q (H for help)", name)
	}
}

func (a *App) pruneUndo() {
	counts := map[ruleKind]int{ruleInclude: len(a.cfg.Includes), ruleExclude: len(a.cfg.Excludes),
		ruleRewrite: len(a.cfg.Rewrites), ruleHighlight: len(a.hls)}
	kept := a.undo[:0]
	seen := map[ruleKind]int{}
	for _, k := range a.undo {
		if seen[k] < counts[k] {
			seen[k]++
			kept = append(kept, k)
		}
	}
	a.undo = kept
}

// jumpTime moves to the first line at or after t, assuming lines are
// roughly in time order (binary search across all segments).
func (a *App) jumpTime(t int64) {
	n := a.seq.NumSegs()
	starts := make([]int64, n+1)
	for g := 0; g < n; g++ {
		starts[g+1] = starts[g] + a.seq.DisplayLen(g)
	}
	posOf := func(k int64) Pos {
		g := 0
		for g+1 < n && starts[g+1] <= k {
			g++
		}
		return Pos{G: g, I: k - starts[g]}
	}
	tsAt := func(k int64) (int64, bool) {
		p := posOf(k)
		for i := int64(0); i < 200 && p.I+i < a.seq.DisplayLen(p.G); i++ {
			raw, _, err := a.seq.Line(p.G, p.I+i)
			if err != nil {
				continue
			}
			if v, _, ok := ts.Parse([]byte(a.cfg.Text(raw))); ok {
				return v, true
			}
		}
		return 0, false
	}
	lo, hi := int64(0), starts[n]
	for lo < hi {
		mid := lo + (hi-lo)/2
		if v, ok := tsAt(mid); !ok || v < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= starts[n] {
		a.setErr("no line at or after %s", time.Unix(0, t).Format("2006-01-02 15:04:05"))
		return
	}
	a.follow = false
	a.userMoved()
	if p, ok := a.atOrAfter(posOf(lo)); ok {
		a.cursor = p
	}
	a.setMsg("jumped to %s", time.Unix(0, t).Format("2006-01-02 15:04:05"))
}

// export writes the visible lines (as displayed text, without colours).
func (a *App) export(path string) {
	if path == "" {
		a.setErr("usage: w FILE")
		return
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	seq, cfg, filter := a.seq, a.cfg, a.filter
	a.setMsg("writing %s…", path)
	go func() {
		n, err := writeVisible(path, seq, cfg, filter)
		a.async <- func() {
			if err != nil {
				a.setErr("write: %v", err)
			} else {
				a.setMsg("wrote %d lines to %s", n, path)
			}
		}
	}()
}

func writeVisible(path string, seq logsrc.Seq, cfg *query.Config, filter *query.Filter) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	n, err := WriteLines(w, seq, cfg, filter)
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// WriteLines writes every visible line's text to w.
func WriteLines(w *bufio.Writer, seq logsrc.Seq, cfg *query.Config, filter *query.Filter) (int64, error) {
	var n int64
	for g := 0; g < seq.NumSegs(); g++ {
		it := seq.Iter(g, 0)
		for i := int64(0); ; i++ {
			raw, _, lost, ok := it.Next()
			if !ok {
				break
			}
			if lost || (filter != nil && !filter.Visible(g, i)) {
				continue
			}
			w.WriteString(cfg.Text(raw))
			if err := w.WriteByte('\n'); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
