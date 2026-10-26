package ui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
)

type helpEntry struct{ keys, desc string }

type helpSection struct {
	title   string
	entries []helpEntry
	notes   []string
}

var helpSections = []helpSection{
	{"Moving", []helpEntry{
		{"j k ↑ ↓", "move one line"},
		{"Space PgDn", "page down"},
		{"b PgUp", "page up"},
		{"Ctrl-D Ctrl-U", "half page down / up"},
		{"g Home", "first line"},
		{"G End", "last line, and follow"},
		{"F", "toggle following new lines"},
		{"[", "start of this generation, then the previous one"},
		{"]", "next generation"},
		{"← → 0", "scroll sideways (when not wrapping)"},
	}, nil},
	{"Searching", []helpEntry{
		{"/ ?", "search forward / backward"},
		{"n N", "next / previous match"},
		{"Esc", "cancel search, clear match highlight"},
		{"t", "jump to a time"},
	}, nil},
	{"Rules", []helpEntry{
		{"f", "add a filter (show only matching records; all filters must match)"},
		{"x", "add an exclude (hide matching records)"},
		{"h", "highlight a regex"},
		{"s", "substitute: s/regex/replacement/"},
		{"c", "columns"},
		{"r", "toggle multi-line records"},
		{"u", "undo the last rule"},
		{"C", "clear all rules"},
	}, nil},
	{"Display", []helpEntry{
		{"Enter", "pretty-print the JSON on this line"},
		{"J", "pretty-print all JSON"},
		{"w", "wrap long lines"},
		{"#", "line numbers"},
		{"T", "source names (merged files)"},
		{"D", "log level colours"},
	}, nil},
	{"Generations", []helpEntry{
		{"X", "forget the generation under the cursor (asks first)"},
	}, nil},
	{"Other", []helpEntry{
		{":", "command line (commands below)"},
		{"H F1", "this help"},
		{"q Ctrl-C", "quit"},
	}, nil},
	{"Commands", []helpEntry{
		{"filter RE", "same as f"},
		{"exclude RE", "same as x"},
		{"hl RE [colour]", "highlight; colour is a name, 0-255 or #rrggbb"},
		{"sub /RE/REPL/", "substitute; REPL may use $1, ${name} or \\1"},
		{"cols SPEC | off", "columns: regex groups, or JSON fields like .level .msg"},
		{"since T | off", "hide records before time T"},
		{"until T | off", "hide records after time T"},
		{"time T", "jump to time T"},
		{"record auto|off|RE", "how multi-line records start"},
		{"clear [WHAT]", "all, filter, hl, sub, cols or time"},
		{"undo", "same as u"},
		{"forget [N|old]", "forget this generation, generation N, or all earlier ones"},
		{"goto N", "line N of this generation"},
		{"w FILE", "save the visible lines to FILE"},
		{"json  wrap  nums", "toggle JSON / wrapping / line numbers"},
		{"defaulthl on|off", "log level colours"},
		{"follow", "toggle following"},
		{"q", "quit"},
	}, nil},
	{"Notes", nil, []string{
		"Regexes use RE2 syntax; all-lower-case patterns ignore case.",
		"Times T: 2026-10-25 12:00, 12:00 (on the current line's date), -15m, -2h, -1d, now.",
		"Line numbers, wrapping, level colours and each log's rules are remembered",
		"in ~/.logview.json (see --state).",
	}},
}

type helpLine struct {
	text string
	bold bool
}

func helpLines() []helpLine {
	keyW := 0
	for _, s := range helpSections {
		for _, e := range s.entries {
			keyW = max(keyW, len([]rune(e.keys)))
		}
	}
	lines := []helpLine{{"logview", true}}
	for _, s := range helpSections {
		lines = append(lines, helpLine{}, helpLine{s.title, true})
		for _, e := range s.entries {
			pad := strings.Repeat(" ", keyW-len([]rune(e.keys)))
			lines = append(lines, helpLine{text: "  " + e.keys + pad + "   " + e.desc})
		}
		for _, n := range s.notes {
			lines = append(lines, helpLine{text: "  " + n})
		}
	}
	return lines
}

func (a *App) openHelp() {
	a.help, a.helpScroll = true, 0
}

// helpPage is the number of help lines that fit above the status line.
func (a *App) helpPage() int {
	_, H := a.scr.Size()
	return max(H-1, 1)
}

func (a *App) helpKey(ev *tcell.EventKey) {
	page := a.helpPage()
	maxScroll := max(len(helpLines())-page, 0)
	switch {
	case ev.Key() == tcell.KeyDown || ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
		a.helpScroll++
	case ev.Key() == tcell.KeyUp || ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
		a.helpScroll--
	case ev.Key() == tcell.KeyPgDn || ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
		a.helpScroll += page
	case ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyRune && ev.Rune() == 'b':
		a.helpScroll -= page
	case ev.Key() == tcell.KeyHome || ev.Key() == tcell.KeyRune && ev.Rune() == 'g':
		a.helpScroll = 0
	case ev.Key() == tcell.KeyEnd || ev.Key() == tcell.KeyRune && ev.Rune() == 'G':
		a.helpScroll = maxScroll
	default:
		a.help = false
	}
	a.helpScroll = min(max(a.helpScroll, 0), maxScroll)
}

// drawHelp shows the help as a full-screen page over the log.
func (a *App) drawHelp() {
	a.scr.Clear()
	a.scr.HideCursor()
	W, H := a.scr.Size()
	lines := helpLines()
	page := a.helpPage()
	a.helpScroll = min(max(a.helpScroll, 0), max(len(lines)-page, 0))
	for i := 0; i < page && a.helpScroll+i < len(lines); i++ {
		l := lines[a.helpScroll+i]
		st := tcell.StyleDefault
		if l.bold {
			st = st.Bold(true)
		}
		a.putStr(1, i, W, l.text, st)
	}
	for x := 0; x < W; x++ {
		a.scr.SetContent(x, H-1, ' ', nil, styleStatus)
	}
	footer := " help · any key returns to the log"
	if len(lines) > page {
		footer = fmt.Sprintf(" help · lines %d-%d of %d · j/k PgUp/PgDn g/G scroll · any other key returns to the log",
			a.helpScroll+1, min(a.helpScroll+page, len(lines)), len(lines))
	}
	a.putStr(0, H-1, W, footer, styleStatus)
}
