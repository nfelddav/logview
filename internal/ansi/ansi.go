// Package ansi turns raw log bytes into printable text plus style spans.
//
// SGR sequences (colours, bold, ...) become spans. Every other escape or
// control sequence is dropped: a log file must never be able to move the
// cursor, retitle the terminal, or otherwise talk to the terminal directly.
package ansi

import (
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

const tabWidth = 8

// Span styles Text[Start:End].
type Span struct {
	Start, End int
	Style      tcell.Style
}

// Line is a sanitised line: Text contains no control characters.
type Line struct {
	Text  string
	Spans []Span
}

// needsParse reports whether b contains anything Parse would alter.
func needsParse(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// Strip returns the sanitised text of b without computing styles.
func Strip(b []byte) string {
	if !needsParse(b) {
		return string(b)
	}
	return parse(b, false).Text
}

// StripBytes is Strip for callers that only read the result: it returns b
// itself, without copying, when there is nothing to remove.
func StripBytes(b []byte) []byte {
	if !needsParse(b) {
		return b
	}
	return []byte(parse(b, false).Text)
}

// Parse returns the sanitised text of b and the styles its SGR sequences describe.
func Parse(b []byte) Line {
	if !needsParse(b) {
		return Line{Text: string(b)}
	}
	return parse(b, true)
}

func parse(b []byte, styles bool) Line {
	out := make([]byte, 0, len(b))
	var spans []Span
	cur := tcell.StyleDefault
	spanStart := 0
	col := 0
	setStyle := func(st tcell.Style) {
		if st == cur {
			return
		}
		if cur != tcell.StyleDefault && len(out) > spanStart {
			spans = append(spans, Span{spanStart, len(out), cur})
		}
		cur = st
		spanStart = len(out)
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(b) {
				i++
				continue
			}
			switch b[i+1] {
			case '[': // CSI: params, intermediates, final byte
				j := i + 2
				for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
					j++
				}
				pEnd := j
				for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
					j++
				}
				if j >= len(b) {
					i = len(b)
					continue
				}
				if b[j] < 0x40 || b[j] > 0x7e { // malformed; drop the introducer only
					i = j
					continue
				}
				if b[j] == 'm' && pEnd == j && styles {
					setStyle(applySGR(cur, b[i+2:pEnd]))
				}
				i = j + 1
			case ']', 'P', 'X', '^', '_': // string sequences end at BEL or ST
				j := i + 2
				for j < len(b) {
					if b[j] == 0x07 {
						j++
						break
					}
					if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j
			default: // two-byte escapes, optionally with intermediates
				j := i + 1
				for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
					j++
				}
				if j < len(b) {
					j++
				}
				i = j
			}
		case c == '\t':
			n := tabWidth - col%tabWidth
			for k := 0; k < n; k++ {
				out = append(out, ' ')
			}
			col += n
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c < 0x80:
			out = append(out, c)
			col++
			i++
		default:
			_, sz := utf8.DecodeRune(b[i:])
			out = append(out, b[i:i+sz]...)
			col++
			i += sz
		}
	}
	if cur != tcell.StyleDefault && len(out) > spanStart {
		spans = append(spans, Span{spanStart, len(out), cur})
	}
	return Line{Text: string(out), Spans: spans}
}

func applySGR(st tcell.Style, p []byte) tcell.Style {
	var buf [32]int
	n, cur := 0, 0
	for _, ch := range p {
		switch {
		case ch >= '0' && ch <= '9':
			if cur < 1<<20 {
				cur = cur*10 + int(ch-'0')
			}
		case ch == ';' || ch == ':':
			if n < len(buf) {
				buf[n] = cur
				n++
			}
			cur = 0
		default: // private-mode markers etc.: not an SGR we understand
			return st
		}
	}
	if n < len(buf) {
		buf[n] = cur
		n++
	}
	ps := buf[:n]
	for k := 0; k < len(ps); k++ {
		v := ps[k]
		switch {
		case v == 0:
			st = tcell.StyleDefault
		case v == 1:
			st = st.Bold(true)
		case v == 2:
			st = st.Dim(true)
		case v == 3:
			st = st.Italic(true)
		case v == 4 || v == 21:
			st = st.Underline(true)
		case v == 5 || v == 6:
			st = st.Blink(true)
		case v == 7:
			st = st.Reverse(true)
		case v == 9:
			st = st.StrikeThrough(true)
		case v == 22:
			st = st.Bold(false).Dim(false)
		case v == 23:
			st = st.Italic(false)
		case v == 24:
			st = st.Underline(false)
		case v == 25:
			st = st.Blink(false)
		case v == 27:
			st = st.Reverse(false)
		case v == 29:
			st = st.StrikeThrough(false)
		case v >= 30 && v <= 37:
			st = st.Foreground(tcell.PaletteColor(v - 30))
		case v == 39:
			st = st.Foreground(tcell.ColorDefault)
		case v >= 40 && v <= 47:
			st = st.Background(tcell.PaletteColor(v - 40))
		case v == 49:
			st = st.Background(tcell.ColorDefault)
		case v >= 90 && v <= 97:
			st = st.Foreground(tcell.PaletteColor(v - 90 + 8))
		case v >= 100 && v <= 107:
			st = st.Background(tcell.PaletteColor(v - 100 + 8))
		case v == 38 || v == 48:
			c, adv, ok := extColor(ps[k+1:])
			k += adv
			if ok {
				if v == 38 {
					st = st.Foreground(c)
				} else {
					st = st.Background(c)
				}
			}
		}
	}
	return st
}

func extColor(ps []int) (tcell.Color, int, bool) {
	if len(ps) >= 2 && ps[0] == 5 {
		return tcell.PaletteColor(ps[1] & 0xff), 2, true
	}
	if len(ps) >= 4 && ps[0] == 2 {
		return tcell.NewRGBColor(int32(ps[1]&0xff), int32(ps[2]&0xff), int32(ps[3]&0xff)), 4, true
	}
	return tcell.ColorDefault, len(ps), false
}
