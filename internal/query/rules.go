// Package query holds everything that interprets line text: filters,
// multi-line records, rewrites, highlights, columns and search.
package query

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unsafe"

	"github.com/gdamore/tcell/v2"

	"github.com/nfelddav/logview/internal/ansi"
)

// Compile compiles a user pattern with smart case: a pattern without
// upper-case letters (escapes like \S do not count) matches case-insensitively.
func Compile(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, errors.New("empty pattern")
	}
	if !hasUpper(pattern) {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

func hasUpper(p string) bool {
	escaped := false
	for _, r := range p {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

type Rewrite struct {
	Re   *regexp.Regexp
	Repl string
	Src  string
}

var backref = regexp.MustCompile(`\\(\d)`)

// ParseSub parses a sed-style substitution: /regex/replacement/ with any
// delimiter. The replacement may use $1, ${name} or \1.
func ParseSub(s string) (Rewrite, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "s") && len(s) > 1 && !isWordByte(s[1]) {
		s = s[1:]
	}
	if len(s) < 3 {
		return Rewrite{}, errors.New("usage: /regex/replacement/")
	}
	d := s[0]
	var parts []string
	var cur strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == d {
			cur.WriteByte(d)
			i++
			continue
		}
		if c == d {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 || len(parts) < 2 {
		parts = append(parts, cur.String())
	}
	if len(parts) < 2 {
		return Rewrite{}, errors.New("usage: /regex/replacement/")
	}
	re, err := Compile(parts[0])
	if err != nil {
		return Rewrite{}, err
	}
	repl := backref.ReplaceAllString(parts[1], "$${$1}")
	return Rewrite{Re: re, Repl: repl, Src: s}, nil
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ApplyRewrites runs each rewrite over s in order.
func ApplyRewrites(rw []Rewrite, s string) string {
	for _, r := range rw {
		s = r.Re.ReplaceAllString(s, r.Repl)
	}
	return s
}

type RecordKind uint8

const (
	RecAuto  RecordKind = iota // a line starting with a timestamp (or, in logs without them, a non-indented line) starts a record
	RecOff                     // every line is its own record
	RecRegex                   // lines matching Re start a record
)

type RecordMode struct {
	Kind RecordKind
	Re   *regexp.Regexp
}

func ParseRecordMode(s string) (RecordMode, error) {
	switch strings.TrimSpace(s) {
	case "", "auto":
		return RecordMode{Kind: RecAuto}, nil
	case "off", "line", "none":
		return RecordMode{Kind: RecOff}, nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return RecordMode{}, err
	}
	return RecordMode{Kind: RecRegex, Re: re}, nil
}

func (m RecordMode) String() string {
	switch m.Kind {
	case RecOff:
		return "off"
	case RecRegex:
		return m.Re.String()
	}
	return "auto"
}

// Config is an immutable filter configuration. Includes are ANDed (each
// must match some line of a record); any exclude match hides the record.
type Config struct {
	Includes []*regexp.Regexp
	Excludes []*regexp.Regexp
	Since    int64 // unix nanoseconds, 0 = unbounded
	Until    int64
	Record   RecordMode
	Rewrites []Rewrite
}

const MaxIncludes = 64

func (c *Config) Filtering() bool {
	return len(c.Includes) > 0 || len(c.Excludes) > 0 || c.Since != 0 || c.Until != 0
}

// Text is the line as filters, search and highlights see it: escape
// sequences stripped, rewrites applied.
func (c *Config) Text(raw []byte) string {
	s := ansi.Strip(raw)
	if len(c.Rewrites) > 0 {
		s = ApplyRewrites(c.Rewrites, s)
	}
	return s
}

// TextBytes is Text without copying when the line needs no changes. The
// result may alias raw.
func (c *Config) TextBytes(raw []byte) []byte {
	b := ansi.StripBytes(raw)
	if len(c.Rewrites) > 0 {
		s := ApplyRewrites(c.Rewrites, unsafeString(b))
		return stringBytes(s)
	}
	return b
}

func stringBytes(s string) []byte { return unsafe.Slice(unsafe.StringData(s), len(s)) }

func unsafeString(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

var namedColors = map[string]tcell.Color{
	"black": tcell.ColorBlack, "red": tcell.ColorMaroon, "green": tcell.ColorGreen,
	"yellow": tcell.ColorOlive, "blue": tcell.ColorNavy, "magenta": tcell.ColorPurple,
	"cyan": tcell.ColorTeal, "white": tcell.ColorSilver, "gray": tcell.ColorGray, "grey": tcell.ColorGray,
	"brightred": tcell.ColorRed, "brightgreen": tcell.ColorLime, "brightyellow": tcell.ColorYellow,
	"brightblue": tcell.ColorBlue, "brightmagenta": tcell.ColorFuchsia, "brightcyan": tcell.ColorAqua,
	"orange": tcell.PaletteColor(208), "pink": tcell.PaletteColor(213), "purple": tcell.PaletteColor(135),
}

// ParseColor accepts a name, a palette index 0-255 or #rrggbb.
func ParseColor(s string) (tcell.Color, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if c, ok := namedColors[s]; ok {
		return c, nil
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n < 256 {
		return tcell.PaletteColor(n), nil
	}
	if strings.HasPrefix(s, "#") && len(s) == 7 {
		if v, err := strconv.ParseUint(s[1:], 16, 32); err == nil {
			return tcell.NewHexColor(int32(v)), nil
		}
	}
	return tcell.ColorDefault, fmt.Errorf("unknown colour %q", s)
}

type Highlight struct {
	Re    *regexp.Regexp
	Style tcell.Style
	Src   string
}

var hlRotation = []tcell.Color{
	tcell.PaletteColor(208), tcell.PaletteColor(45), tcell.PaletteColor(170),
	tcell.PaletteColor(118), tcell.PaletteColor(220), tcell.PaletteColor(33),
}

// ParseHighlight parses "regex [colour]"; without a colour one is picked
// from a rotation based on n.
func ParseHighlight(s string, n int) (Highlight, error) {
	s = strings.TrimSpace(s)
	pat, col := s, ""
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		if _, err := ParseColor(s[i+1:]); err == nil {
			pat, col = strings.TrimSpace(s[:i]), s[i+1:]
		}
	}
	if i := strings.LastIndexByte(s, '='); col == "" && i > 0 {
		if _, err := ParseColor(s[i+1:]); err == nil {
			pat, col = s[:i], s[i+1:]
		}
	}
	re, err := Compile(pat)
	if err != nil {
		return Highlight{}, err
	}
	c := hlRotation[n%len(hlRotation)]
	if col != "" {
		c, _ = ParseColor(col)
	}
	return Highlight{Re: re, Style: tcell.StyleDefault.Foreground(c).Bold(true), Src: s}, nil
}

// DefaultHighlights colour common log levels.
func DefaultHighlights() []Highlight {
	mk := func(p string, st tcell.Style) Highlight {
		return Highlight{Re: regexp.MustCompile(p), Style: st, Src: p}
	}
	return []Highlight{
		mk(`\b(?:FATAL|PANIC|CRIT(?:ICAL)?|ERROR|ERR|EMERG|ALERT|fatal|panic|error)\b`, tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true)),
		mk(`\b(?:WARN(?:ING)?|warn(?:ing)?)\b`, tcell.StyleDefault.Foreground(tcell.ColorYellow).Bold(true)),
		mk(`\b(?:INFO|NOTICE|info)\b`, tcell.StyleDefault.Foreground(tcell.ColorGreen)),
		mk(`\b(?:DEBUG|TRACE|debug|trace)\b`, tcell.StyleDefault.Foreground(tcell.ColorGray)),
	}
}
