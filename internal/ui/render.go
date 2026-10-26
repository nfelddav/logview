package ui

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"

	"github.com/nfelddav/logview/internal/ansi"
	"github.com/nfelddav/logview/internal/jsonfmt"
	"github.com/nfelddav/logview/internal/logsrc"
	"github.com/nfelddav/logview/internal/query"
)

const maxRenderLen = 64 << 10

var (
	styleSep    = tcell.StyleDefault.Foreground(tcell.ColorTeal).Bold(true)
	styleGutter = tcell.StyleDefault.Foreground(tcell.ColorGray)
	styleCursor = tcell.StyleDefault.Reverse(true).Bold(true)
	styleStatus = tcell.StyleDefault.Reverse(true)
	styleInfo   = tcell.StyleDefault.Foreground(tcell.ColorTeal)
	styleErr    = tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true)
	styleLost   = tcell.StyleDefault.Dim(true).Italic(true)
	styleMatch  = tcell.StyleDefault.Background(tcell.ColorYellow).Foreground(tcell.ColorBlack)
	styleColSep = tcell.StyleDefault.Foreground(tcell.ColorGray)
	styleHeader = tcell.StyleDefault.Bold(true).Underline(true)

	jsonStyles = map[jsonfmt.Kind]tcell.Style{
		jsonfmt.Key:    tcell.StyleDefault.Foreground(tcell.PaletteColor(6)),
		jsonfmt.String: tcell.StyleDefault.Foreground(tcell.PaletteColor(2)),
		jsonfmt.Number: tcell.StyleDefault.Foreground(tcell.PaletteColor(5)),
		jsonfmt.Bool:   tcell.StyleDefault.Foreground(tcell.PaletteColor(3)),
		jsonfmt.Null:   tcell.StyleDefault.Foreground(tcell.PaletteColor(8)),
	}
	srcColors = []tcell.Color{tcell.PaletteColor(6), tcell.PaletteColor(5), tcell.PaletteColor(3),
		tcell.PaletteColor(2), tcell.PaletteColor(4), tcell.PaletteColor(1)}
)

// lrow is a logical row: text plus one style per byte.
type lrow struct {
	buf []byte
	st  []tcell.Style
}

func (r *lrow) text() string { return string(r.buf) }

func (r *lrow) add(s string, st tcell.Style) {
	r.buf = append(r.buf, s...)
	for range len(s) {
		r.st = append(r.st, st)
	}
}

func (r *lrow) addSpans(text string, spans []ansi.Span) {
	base := len(r.st)
	r.add(text, tcell.StyleDefault)
	for _, sp := range spans {
		for i := sp.Start; i < sp.End && i < len(text); i++ {
			r.st[base+i] = sp.Style
		}
	}
}

// overlay applies style to every match of the highlight regexes; a style
// replaces colours it sets and keeps the rest.
func (r *lrow) overlay(hls []query.Highlight) {
	for _, h := range hls {
		for _, m := range h.Re.FindAllIndex(r.buf, -1) {
			for i := m[0]; i < m[1]; i++ {
				r.st[i] = merge(r.st[i], h.Style)
			}
		}
	}
}

func merge(base, over tcell.Style) tcell.Style {
	fg, bg, attr := over.Decompose()
	if fg != tcell.ColorDefault {
		base = base.Foreground(fg)
	}
	if bg != tcell.ColorDefault {
		base = base.Background(bg)
	}
	return base.Attributes(attr | attrsOf(base))
}

func attrsOf(s tcell.Style) tcell.AttrMask {
	_, _, a := s.Decompose()
	return a
}

type built struct {
	rows []lrow
	ref  logsrc.Ref
}

type cacheKey struct {
	p        Pos
	expanded bool
}

func (a *App) expanded(p Pos) bool { return a.jsonAll != a.jsonToggled[p] }

// build turns line p into logical rows: ANSI styles, rewrites, columns or
// JSON formatting, then highlights.
func (a *App) build(p Pos) built {
	key := cacheKey{p, a.expanded(p)}
	if b, ok := a.cache[key]; ok {
		return b
	}
	raw, ref, err := a.seq.Line(p.G, p.I)
	var b built
	b.ref = ref
	switch {
	case errors.Is(err, logsrc.ErrLost):
		info := a.seq.Info(p.G)
		var r lrow
		msg := fmt.Sprintf("‹%d lines not preserved", info.Lost)
		if info.Note != "" {
			msg += ": " + info.Note
		}
		r.add(msg+"›", styleLost)
		b.rows = []lrow{r}
	case err != nil:
		var r lrow
		r.add("‹"+err.Error()+"›", styleLost)
		b.rows = []lrow{r}
	default:
		if len(raw) > maxRenderLen {
			raw = raw[:maxRenderLen]
		}
		b.rows = a.format(raw, key.expanded)
	}
	if p.I < a.seq.ScanLen(p.G) { // a trailing partial line may still change
		if len(a.cache) > 4096 {
			clear(a.cache)
		}
		a.cache[key] = b
	}
	return b
}

func (a *App) format(raw []byte, expanded bool) []lrow {
	pl := ansi.Parse(raw)
	text, spans := pl.Text, pl.Spans
	if len(a.cfg.Rewrites) > 0 {
		if nt := query.ApplyRewrites(a.cfg.Rewrites, text); nt != text {
			text, spans = nt, nil
		}
	}
	if a.cols != nil {
		if vals, ok := a.cols.Extract(text); ok {
			return []lrow{a.columnRow(vals)}
		}
	}
	var rows []lrow
	if at := jsonfmt.Find(text); at >= 0 {
		var first lrow
		var pre []ansi.Span
		for _, sp := range spans {
			if sp.Start < at {
				pre = append(pre, ansi.Span{Start: sp.Start, End: min(sp.End, at), Style: sp.Style})
			}
		}
		first.addSpans(text[:at], pre)
		if expanded {
			pretty := jsonfmt.Pretty(text[at:], 2)
			for k, tr := range pretty {
				r := first
				if k > 0 {
					r = lrow{}
				}
				for _, t := range tr {
					r.add(t.Text, jsonStyles[t.Kind])
				}
				rows = append(rows, r)
			}
		} else {
			for _, t := range jsonfmt.Compact(text[at:]) {
				first.add(t.Text, jsonStyles[t.Kind])
			}
			rows = []lrow{first}
		}
	} else {
		var r lrow
		r.addSpans(text, spans)
		rows = []lrow{r}
	}
	for i := range rows {
		a.highlight(&rows[i])
	}
	return rows
}

const maxColWidth = 40

func (a *App) columnRow(vals []string) lrow {
	if len(a.colW) < len(vals) {
		a.colW = append(a.colW, make([]int, len(vals)-len(a.colW))...)
	}
	var r lrow
	for i, v := range vals {
		if i > 0 {
			r.add(" │ ", styleColSep)
		}
		w := uniseg.StringWidth(v)
		if i < len(vals)-1 {
			if w > a.colW[i] && a.colW[i] < maxColWidth {
				a.colW[i] = min(w, maxColWidth)
				a.colsChanged = true
			}
			if w > a.colW[i] {
				v = truncateWidth(v, a.colW[i]-1) + "…"
				w = a.colW[i]
			}
			r.add(v+strings.Repeat(" ", a.colW[i]-w), tcell.StyleDefault)
		} else {
			r.add(v, tcell.StyleDefault)
		}
	}
	a.highlight(&r)
	return r
}

func (a *App) highlight(r *lrow) {
	if a.defaultHL {
		r.overlay(a.defaults)
	}
	r.overlay(a.hls)
	if a.searchRe != nil {
		r.overlay([]query.Highlight{{Re: a.searchRe, Style: styleMatch}})
	}
}

func truncateWidth(s string, w int) string {
	n := 0
	for i, r := range s {
		rw := runeWidth(r)
		if n+rw > w {
			return s[:i]
		}
		n += rw
	}
	return s
}

type cell struct {
	r  rune
	w  int
	st tcell.Style
}

func runeWidth(r rune) int {
	if r < 0x300 {
		return 1
	}
	return uniseg.StringWidth(string(r))
}

func toCells(r lrow) []cell {
	cells := make([]cell, 0, len(r.buf))
	for i, ch := range r.text() {
		if ch == utf8.RuneError {
			cells = append(cells, cell{'�', 1, r.st[i]})
			continue
		}
		if w := runeWidth(ch); w > 0 {
			cells = append(cells, cell{ch, w, r.st[i]})
		}
	}
	return cells
}

func wrapCells(cells []cell, width int) [][]cell {
	if width <= 0 || len(cells) == 0 {
		return [][]cell{cells}
	}
	var out [][]cell
	start, col := 0, 0
	for i, c := range cells {
		if col+c.w > width && i > start {
			out = append(out, cells[start:i])
			start, col = i, 0
		}
		col += c.w
	}
	return append(out, cells[start:])
}

const (
	rowFirst = iota
	rowCont
	rowSep
)

type srow struct {
	kind  int
	cells []cell
	ref   logsrc.Ref
}

// screenRows returns the rows line p occupies: maybe a generation
// separator, then the (wrapped) rows of the line. Cached per frame.
func (a *App) screenRows(p Pos) []srow {
	if rs, ok := a.frameRows[p]; ok {
		return rs
	}
	var out []srow
	if p.I == 0 && a.seq.NumSegs() > 1 {
		out = append(out, srow{kind: rowSep, cells: a.sepCells(p.G)})
	}
	b := a.build(p)
	cw := a.contentW
	for k, lr := range b.rows {
		cells := toCells(lr)
		parts := [][]cell{cells}
		if a.wrap {
			parts = wrapCells(cells, cw)
		}
		for j, part := range parts {
			kind := rowCont
			if k == 0 && j == 0 {
				kind = rowFirst
			}
			out = append(out, srow{kind: kind, cells: part, ref: b.ref})
		}
	}
	a.frameRows[p] = out
	return out
}

func (a *App) sepCells(g int) []cell {
	info := a.seq.Info(g)
	label := info.Label
	if info.Note != "" {
		label += " · " + info.Note
	}
	var r lrow
	r.add("━━━ "+label+" ", styleSep)
	cells := toCells(r)
	for w := len(cells); w < a.width; w++ {
		cells = append(cells, cell{'━', 1, styleSep})
	}
	return cells
}
