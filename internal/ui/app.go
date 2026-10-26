// Package ui is the interactive viewer.
package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/nfelddav/logview/internal/logsrc"
	"github.com/nfelddav/logview/internal/query"
)

type Options struct {
	Config      query.Config
	Highlights  []query.Highlight
	Columns     *query.Columns
	DefaultHL   bool
	Follow      bool
	Wrap        bool
	LineNumbers bool
	Mouse       bool

	// Message is shown in the status line at startup (e.g. a settings problem).
	Message string
	// OnChange is called after an input event changed the rememberable settings.
	OnChange func(Settings) error
}

type ruleKind int

const (
	ruleInclude ruleKind = iota
	ruleExclude
	ruleHighlight
	ruleRewrite
)

type App struct {
	scr   tcell.Screen
	seq   logsrc.Seq
	multi bool

	cfg      *query.Config
	filter   *query.Filter
	hls      []query.Highlight
	defaults []query.Highlight
	cols     *query.Columns
	colW     []int
	undo     []ruleKind

	defaultHL, wrap, lineNums, showSrc, follow, jsonAll, help bool

	cursor, top Pos
	topSkip     int
	anchor      *Pos
	hscroll     int
	jsonToggled map[Pos]bool

	searchRe      *regexp.Regexp
	searchFwd     bool
	searchCancel  *atomic.Bool
	searchRunning bool

	prompt     *prompt
	confirm    *confirm
	history    map[string][]string
	msg        string
	msgErr     bool
	helpScroll int

	onChange     func(Settings) error
	lastSettings Settings

	// layout of the current frame
	width, viewH, contentW, srcW, numW int
	frameRows                          map[Pos][]srow
	cache                              map[cacheKey]built
	colsChanged                        bool
	lostSig                            int64
	lastVer                            uint64

	async chan func()
	quit  bool
}

func New(scr tcell.Screen, seq logsrc.Seq, o Options) *App {
	cfg := o.Config
	a := &App{
		scr: scr, seq: seq, multi: len(seq.Sources()) > 1,
		cfg: &cfg, hls: o.Highlights, defaults: query.DefaultHighlights(), cols: o.Columns,
		defaultHL: o.DefaultHL, wrap: o.Wrap, lineNums: o.LineNumbers, showSrc: true, follow: o.Follow,
		jsonToggled: map[Pos]bool{},
		history:     map[string][]string{},
		cache:       map[cacheKey]built{},
		frameRows:   map[Pos][]srow{},
		async:       make(chan func(), 64),
		searchFwd:   true,
		onChange:    o.OnChange,
	}
	if o.Message != "" {
		a.setErr("%s", o.Message)
	}
	// Rules given at startup (flags or remembered) can be undone too.
	for _, n := range []struct {
		k ruleKind
		n int
	}{{ruleInclude, len(cfg.Includes)}, {ruleExclude, len(cfg.Excludes)}, {ruleRewrite, len(cfg.Rewrites)}, {ruleHighlight, len(a.hls)}} {
		for range n.n {
			a.undo = append(a.undo, n.k)
		}
	}
	a.restartFilter()
	if !a.follow {
		a.cursor = Pos{G: 0, I: 0}
		a.anchor = &Pos{G: 0, I: 0}
	}
	a.lastSettings = a.Settings()
	return a
}

// Run is the event loop.
func (a *App) Run() {
	events := make(chan tcell.Event, 64)
	quit := make(chan struct{})
	go a.scr.ChannelEvents(events, quit)
	defer close(quit)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	a.Draw()
	for !a.quit {
		select {
		case ev := <-events:
			a.Handle(ev)
		case fn := <-a.async:
			fn()
		case <-tick.C:
			if !a.changed() {
				continue
			}
		}
		a.Draw()
	}
	if a.filter != nil {
		a.filter.Stop()
	}
}

// Pending runs results posted by background work (used by tests).
func (a *App) Pending() {
	for {
		select {
		case fn := <-a.async:
			fn()
		default:
			return
		}
	}
}

func (a *App) Quit() bool { return a.quit }

func (a *App) version() uint64 {
	v := a.seq.Version()
	if a.filter != nil {
		v += a.filter.Version() << 32
	}
	return v
}

func (a *App) changed() bool {
	v := a.version()
	if v == a.lastVer {
		return false
	}
	a.lastVer = v
	return true
}

func (a *App) invalidate() {
	clear(a.cache)
	a.colW = nil
}

func (a *App) restartFilter() {
	if a.filter != nil {
		a.filter.Stop()
		a.filter = nil
	}
	if a.cfg.Filtering() {
		a.filter = query.NewFilter(a.seq, a.cfg)
	}
	if a.visible(a.cursor) || a.anchor == nil {
		c := a.cursor
		a.anchor = &c
	}
	a.invalidate()
}

func (a *App) setMsg(format string, args ...any) {
	a.msg, a.msgErr = fmt.Sprintf(format, args...), false
}

func (a *App) setErr(format string, args ...any) {
	a.msg, a.msgErr = fmt.Sprintf(format, args...), true
}

func digits(n int64) int {
	return len(strconv.FormatInt(max(n, 1), 10))
}

// Draw renders a frame.
func (a *App) Draw() {
	a.scr.Clear()
	W, H := a.scr.Size()
	a.width = W
	clear(a.frameRows)
	a.lastVer = a.version()

	var lostSig int64
	var maxLen int64
	for g := 0; g < a.seq.NumSegs(); g++ {
		lostSig += a.seq.Info(g).Lost + 1
		maxLen = max(maxLen, a.seq.DisplayLen(g))
	}
	if lostSig != a.lostSig {
		a.lostSig = lostSig
		a.invalidate()
	}
	a.srcW, a.numW = 0, 0
	if a.multi && a.showSrc {
		for _, s := range a.seq.Sources() {
			a.srcW = max(a.srcW, min(len(s.Name), 14)+1)
		}
	}
	if a.lineNums {
		a.numW = max(digits(maxLen), 4) + 1
	}
	gutter := 1 + a.srcW + a.numW
	a.contentW = max(W-gutter, 1)

	info := a.infoLine()
	contentTop, bottom := 0, H-1
	if a.cols != nil {
		contentTop = 1
	}
	if info != "" {
		bottom--
	}
	a.viewH = max(bottom-contentTop, 1)

	a.ensureCursor()
	a.ensureVisible()

	y := contentTop
	p, skip := a.top, a.topSkip
	for ok := a.visible(p); ok && y < bottom; p, ok = a.nextVis(p) {
		rows := a.screenRows(p)
		for k := skip; k < len(rows) && y < bottom; k++ {
			a.drawRow(y, p, rows[k], gutter)
			y++
		}
		skip = 0
	}
	if a.colsChanged { // column widths grew while drawing: redraw with the final widths
		a.colsChanged = false
		clear(a.cache)
		a.Draw()
		return
	}
	if y == contentTop {
		a.drawEmpty(contentTop)
	}
	if a.cols != nil {
		a.drawHeader(gutter)
	}
	if info != "" {
		a.putStr(0, H-2, W, info, styleInfo)
	}
	if a.prompt != nil {
		a.drawPrompt(H - 1)
	} else {
		a.scr.HideCursor()
		a.drawStatus(H - 1)
	}
	if a.help {
		a.drawHelp()
	}
	a.scr.Show()
}

func (a *App) putStr(x, y, maxX int, s string, st tcell.Style) int {
	for _, r := range s {
		w := runeWidth(r)
		if x+w > maxX {
			break
		}
		a.scr.SetContent(x, y, r, nil, st)
		x += w
	}
	return x
}

func (a *App) drawRow(y int, p Pos, row srow, gutter int) {
	W := a.width
	if row.kind == rowSep {
		x := 0
		for _, c := range row.cells {
			if x+c.w > W {
				break
			}
			a.scr.SetContent(x, y, c.r, nil, c.st)
			x += c.w
		}
		return
	}
	isCursor := p == a.cursor
	if row.kind == rowFirst {
		if isCursor {
			a.scr.SetContent(0, y, '>', nil, styleCursor)
		}
		x := 1
		if a.srcW > 0 {
			srcs := a.seq.Sources()
			name := srcs[row.ref.Src].Name
			if len(name) > a.srcW-1 {
				name = name[:a.srcW-1]
			}
			a.putStr(x, y, x+a.srcW, name, tcell.StyleDefault.Foreground(srcColors[row.ref.Src%len(srcColors)]))
			x += a.srcW
		}
		if a.numW > 0 {
			n := strconv.FormatInt(row.ref.Line+1, 10)
			st := styleGutter
			if isCursor {
				st = styleCursor
			}
			a.putStr(x+a.numW-1-len(n), y, x+a.numW, n, st)
		}
	}
	x := gutter
	cells := row.cells
	if !a.wrap {
		skip := a.hscroll
		for len(cells) > 0 && skip > 0 {
			skip -= cells[0].w
			cells = cells[1:]
		}
	}
	for _, c := range cells {
		if x+c.w > W {
			break
		}
		a.scr.SetContent(x, y, c.r, nil, c.st)
		x += c.w
	}
}

func (a *App) drawHeader(gutter int) {
	var r lrow
	for i, n := range a.cols.Names {
		if i > 0 {
			r.add(" │ ", styleColSep)
		}
		w := len(n)
		if i < len(a.colW) && i < len(a.cols.Names)-1 && a.colW[i] > w {
			n += strings.Repeat(" ", a.colW[i]-w)
		}
		r.add(n, styleHeader)
	}
	x := gutter
	for _, c := range toCells(r) {
		if x+c.w > a.width {
			break
		}
		a.scr.SetContent(x, 0, c.r, nil, c.st)
		x += c.w
	}
}

func (a *App) drawEmpty(y int) {
	msg := "(no lines yet)"
	if a.filter != nil {
		d, t := a.filter.Progress()
		if d < t || !a.seq.Ready() {
			msg = fmt.Sprintf("(no matching lines yet; scanned %s of %s lines)", human(d), human(t))
		} else {
			msg = "(no matching lines)"
		}
	}
	a.putStr(2, y, a.width, msg, styleLost)
}

func human(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fG", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e4:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func (a *App) infoLine() string {
	var parts []string
	for _, re := range a.cfg.Includes {
		parts = append(parts, "+"+re.String())
	}
	for _, re := range a.cfg.Excludes {
		parts = append(parts, "-"+re.String())
	}
	if a.cfg.Since != 0 {
		parts = append(parts, "since "+time.Unix(0, a.cfg.Since).Format("2006-01-02 15:04:05"))
	}
	if a.cfg.Until != 0 {
		parts = append(parts, "until "+time.Unix(0, a.cfg.Until).Format("2006-01-02 15:04:05"))
	}
	for _, rw := range a.cfg.Rewrites {
		parts = append(parts, "sub "+rw.Src)
	}
	for _, h := range a.hls {
		parts = append(parts, "hl "+h.Src)
	}
	if a.cols != nil {
		parts = append(parts, "cols "+a.cols.Spec)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.ReplaceAll(strings.Join(parts, "  "), "(?i)", "")
}

func (a *App) statusLeft() string {
	var b strings.Builder
	b.WriteString(" " + a.seq.Title())
	n := a.seq.NumSegs()
	if a.visible(a.cursor) {
		if n > 1 {
			fmt.Fprintf(&b, " │ gen %d/%d", a.cursor.G+1, n)
		}
		fmt.Fprintf(&b, " │ line %d/%d", a.cursor.I+1, a.seq.DisplayLen(a.cursor.G))
	}
	if a.filter != nil {
		d, t := a.filter.Progress()
		fmt.Fprintf(&b, " │ %s matching", human(a.filter.Count()))
		if d < t {
			fmt.Fprintf(&b, " (scanning %d%%)", d*100/max(t, 1))
		}
	}
	if !a.seq.Ready() {
		if d, t := a.seq.Progress(); t > 0 && d < t {
			fmt.Fprintf(&b, " │ indexing %d%%", d*100/t)
		} else {
			b.WriteString(" │ indexing")
		}
	}
	if a.searchRunning {
		b.WriteString(" │ searching… (Esc cancels)")
	}
	var flags []string
	if a.follow {
		flags = append(flags, "FOLLOW")
	}
	if a.cfg.Record.Kind != query.RecOff && a.filter != nil {
		flags = append(flags, "rec:"+a.cfg.Record.String())
	}
	if a.jsonAll {
		flags = append(flags, "json")
	}
	if !a.wrap {
		flags = append(flags, "nowrap")
		if a.hscroll > 0 {
			flags = append(flags, fmt.Sprintf("→%d", a.hscroll))
		}
	}
	if len(flags) > 0 {
		b.WriteString(" │ " + strings.Join(flags, " "))
	}
	return b.String()
}

func (a *App) drawStatus(y int) {
	W := a.width
	for x := 0; x < W; x++ {
		a.scr.SetContent(x, y, ' ', nil, styleStatus)
	}
	if a.confirm != nil {
		a.putStr(0, y, W, " "+a.confirm.question+" (y/n)", styleStatus.Bold(true))
		return
	}
	left := a.statusLeft()
	right := a.msg
	if right == "" {
		right = "H: help "
	}
	rw := len([]rune(right)) + 1
	a.putStr(0, y, max(W-rw, 0), left, styleStatus)
	st := styleStatus
	if a.msgErr {
		st = styleStatus.Foreground(tcell.ColorRed)
	}
	a.putStr(max(W-rw, 0), y, W, right, st)
}

// QuitSignal, posted as an EventInterrupt's data, makes Run return.
var QuitSignal = new(int)

// Handle processes one input event.
func (a *App) Handle(ev tcell.Event) {
	switch ev := ev.(type) {
	case *tcell.EventInterrupt:
		if ev.Data() == QuitSignal {
			a.quit = true
		}
	case *tcell.EventResize:
		a.scr.Sync()
	case *tcell.EventKey:
		switch {
		case a.help:
			a.helpKey(ev)
		case a.confirm != nil:
			c := a.confirm
			a.confirm = nil
			if ev.Key() == tcell.KeyRune && (ev.Rune() == 'y' || ev.Rune() == 'Y') {
				c.yes()
			} else {
				a.setMsg("cancelled")
			}
		case a.prompt != nil:
			a.promptKey(ev)
		default:
			a.msg = ""
			a.key(ev)
		}
		a.notifyChange()
	case *tcell.EventMouse:
		switch ev.Buttons() {
		case tcell.WheelUp:
			a.follow = false
			a.scrollRows(-3)
		case tcell.WheelDown:
			a.scrollRows(3)
		}
	}
}

func (a *App) userMoved() {
	a.anchor = nil
}

func (a *App) key(ev *tcell.EventKey) {
	page := max(a.viewH-1, 1)
	switch ev.Key() {
	case tcell.KeyCtrlC:
		a.quit = true
	case tcell.KeyEscape:
		if a.searchCancel != nil {
			a.searchCancel.Store(true)
		}
		a.searchRe = nil
		a.invalidate()
	case tcell.KeyDown, tcell.KeyCtrlN:
		a.userMoved()
		a.moveCursor(1)
	case tcell.KeyUp, tcell.KeyCtrlP:
		a.userMoved()
		a.follow = false
		a.moveCursor(-1)
	case tcell.KeyPgDn, tcell.KeyCtrlF:
		a.userMoved()
		a.scrollRows(page)
	case tcell.KeyPgUp, tcell.KeyCtrlB:
		a.userMoved()
		a.follow = false
		a.scrollRows(-page)
	case tcell.KeyCtrlD:
		a.userMoved()
		a.scrollRows(page / 2)
	case tcell.KeyCtrlU:
		a.userMoved()
		a.follow = false
		a.scrollRows(-page / 2)
	case tcell.KeyHome:
		a.goTop()
	case tcell.KeyEnd:
		a.goBottom()
	case tcell.KeyLeft:
		if !a.wrap {
			a.hscroll = max(a.hscroll-8, 0)
		}
	case tcell.KeyRight:
		if !a.wrap {
			a.hscroll += 8
		}
	case tcell.KeyEnter:
		a.jsonToggled[a.cursor] = !a.jsonToggled[a.cursor]
		a.anchor = nil
	case tcell.KeyF1:
		a.openHelp()
	case tcell.KeyCtrlL:
		a.invalidate()
		a.scr.Sync()
	case tcell.KeyRune:
		a.runeKey(ev.Rune())
	}
}

func (a *App) goTop() {
	a.userMoved()
	a.follow = false
	if p, ok := a.first(); ok {
		a.cursor, a.top, a.topSkip = p, p, 0
	}
}

func (a *App) goBottom() {
	a.userMoved()
	a.follow = true
}

func (a *App) runeKey(r rune) {
	page := max(a.viewH-1, 1)
	switch r {
	case 'q':
		a.quit = true
	case 'j':
		a.userMoved()
		a.moveCursor(1)
	case 'k':
		a.userMoved()
		a.follow = false
		a.moveCursor(-1)
	case ' ':
		a.userMoved()
		a.scrollRows(page)
	case 'b':
		a.userMoved()
		a.follow = false
		a.scrollRows(-page)
	case 'g':
		a.goTop()
	case 'G':
		a.goBottom()
	case 'F':
		a.follow = !a.follow
		a.userMoved()
	case '/', '?':
		a.searchFwd = r == '/'
		a.openPrompt(string(r), "search", "")
	case 'n', 'N':
		a.searchNext(r == 'n')
	case ':':
		a.openPrompt(":", "cmd", "")
	case 'f':
		a.openPrompt(":", "cmd", "filter ")
	case 'x':
		a.openPrompt(":", "cmd", "exclude ")
	case 'h':
		a.openPrompt(":", "cmd", "hl ")
	case 's':
		a.openPrompt(":", "cmd", "sub /")
	case 'c':
		a.openPrompt(":", "cmd", "cols ")
	case 't':
		a.openPrompt(":", "cmd", "time ")
	case 'r':
		a.copyConfig()
		if a.cfg.Record.Kind == query.RecOff {
			a.cfg.Record = query.RecordMode{Kind: query.RecAuto}
		} else {
			a.cfg.Record = query.RecordMode{Kind: query.RecOff}
		}
		a.setMsg("records: %s", a.cfg.Record)
		a.restartFilter()
	case 'u':
		a.undoRule()
	case 'C':
		a.runCommand("clear all")
	case 'J':
		a.jsonAll = !a.jsonAll
		clear(a.jsonToggled)
	case 'w':
		a.wrap = !a.wrap
		a.hscroll = 0
	case '#':
		a.lineNums = !a.lineNums
	case 'T':
		a.showSrc = !a.showSrc
	case 'X':
		a.askForget()
	case 'D':
		a.defaultHL = !a.defaultHL
		a.invalidate()
	case '[':
		a.userMoved()
		a.follow = false
		a.jumpSegment(-1)
	case ']':
		a.userMoved()
		a.jumpSegment(1)
	case '0':
		a.hscroll = 0
	case 'H':
		a.openHelp()
	}
}
