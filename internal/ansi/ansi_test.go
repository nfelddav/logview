package ansi

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestPlainPassthrough(t *testing.T) {
	l := Parse([]byte("hello world"))
	if l.Text != "hello world" || len(l.Spans) != 0 {
		t.Fatalf("got %+v", l)
	}
}

func TestSGRSpans(t *testing.T) {
	l := Parse([]byte("a\x1b[31mred\x1b[0m b \x1b[1;38;5;208mX\x1b[m"))
	if l.Text != "ared b X" {
		t.Fatalf("text %q", l.Text)
	}
	if len(l.Spans) != 2 {
		t.Fatalf("spans %+v", l.Spans)
	}
	s := l.Spans[0]
	if s.Start != 1 || s.End != 4 || s.Style != tcell.StyleDefault.Foreground(tcell.PaletteColor(1)) {
		t.Errorf("span0 %+v", s)
	}
	s = l.Spans[1]
	want := tcell.StyleDefault.Bold(true).Foreground(tcell.PaletteColor(208))
	if s.Start != 7 || s.End != 8 || s.Style != want {
		t.Errorf("span1 %+v", s)
	}
}

func TestTrueColorAndBright(t *testing.T) {
	l := Parse([]byte("\x1b[38;2;1;2;3;101mz"))
	want := tcell.StyleDefault.Foreground(tcell.NewRGBColor(1, 2, 3)).Background(tcell.PaletteColor(9))
	if len(l.Spans) != 1 || l.Spans[0].Style != want {
		t.Fatalf("%+v", l.Spans)
	}
}

func TestDangerousSequencesStripped(t *testing.T) {
	cases := map[string]string{
		"a\x1b[2Jb":               "ab", // clear screen
		"a\x1b[10;5Hb":            "ab", // cursor move
		"a\x1b]0;evil title\x07b": "ab", // OSC title, BEL
		"a\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\b": "alinkb", // hyperlink, ST
		"a\x1bPq#0;2;0;0;0\x1b\\b":                   "ab",     // DCS (sixel)
		"a\x1b(Bb":                                   "ab",     // charset
		"a\x1b7\x1b8b":                               "ab",     // save/restore
		"a\rb\x08c\x00d\x7f":                         "abcd",
		"a\x1b[?1049h":                               "a", // alt screen
		"unterminated\x1b[12":                        "unterminated",
		"unterminated osc\x1b]0;foo":                 "unterminated osc",
	}
	for in, want := range cases {
		if got := Strip([]byte(in)); got != want {
			t.Errorf("Strip(%q) = %q, want %q", in, got, want)
		}
		if got := Parse([]byte(in)).Text; got != want {
			t.Errorf("Parse(%q).Text = %q, want %q", in, got, want)
		}
	}
}

func TestTabs(t *testing.T) {
	if got := Strip([]byte("ab\tc\t\td")); got != "ab      c               d" {
		t.Fatalf("%q", got)
	}
}

func TestUTF8Kept(t *testing.T) {
	if got := Strip([]byte("héllo\t世界")); got != "héllo   世界" {
		t.Fatalf("%q", got)
	}
}

func BenchmarkStripPlain(b *testing.B) {
	line := []byte("2026-10-25T12:00:00.123Z INFO server: request handled path=/api/v1/items status=200 dur=12ms")
	for i := 0; i < b.N; i++ {
		Strip(line)
	}
}
