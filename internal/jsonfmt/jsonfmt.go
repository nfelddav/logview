// Package jsonfmt finds JSON documents at the end of log lines and formats
// them as coloured token rows, either compact (one row) or pretty-printed.
package jsonfmt

import (
	"encoding/json"
	"strings"
)

type Kind uint8

const (
	Punct Kind = iota
	Key
	String
	Number
	Bool
	Null
	Space
)

type Token struct {
	Text string
	Kind Kind
}

// Find returns the byte index at which a JSON object or array begins that
// runs to the end of s (ignoring trailing whitespace), or -1.
// Only the first few '{' / '[' candidates are tried to bound the cost.
func Find(s string) int {
	tries := 0
	for i := 0; i < len(s) && tries < 4; i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		tries++
		rest := strings.TrimRight(s[i:], " \t")
		if len(rest) < 2 {
			return -1
		}
		last := rest[len(rest)-1]
		if (s[i] == '{' && last != '}') || (s[i] == '[' && last != ']') {
			continue
		}
		if json.Valid([]byte(rest)) {
			return i
		}
	}
	return -1
}

// tokenize splits valid JSON into tokens, keeping whitespace runs as Space.
func tokenize(s string) []Token {
	var toks []Token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			toks = append(toks, Token{s[i:j], Space})
			i = j
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(s) {
				j++
			}
			toks = append(toks, Token{s[i:j], String})
			i = j
		case strings.IndexByte("{}[]:,", c) >= 0:
			toks = append(toks, Token{s[i : i+1], Punct})
			i++
		default:
			j := i
			for j < len(s) && strings.IndexByte("{}[]:,\" \t\r\n", s[j]) < 0 {
				j++
			}
			if j == i {
				j++
			}
			w := s[i:j]
			k := Number
			switch w {
			case "true", "false":
				k = Bool
			case "null":
				k = Null
			}
			toks = append(toks, Token{w, k})
			i = j
		}
	}
	// A string followed (after optional space) by ':' is an object key.
	for i := range toks {
		if toks[i].Kind != String {
			continue
		}
		j := i + 1
		for j < len(toks) && toks[j].Kind == Space {
			j++
		}
		if j < len(toks) && toks[j].Text == ":" {
			toks[i].Kind = Key
		}
	}
	return toks
}

// Compact returns s as a single row of tokens, preserving its original text.
func Compact(s string) []Token {
	return tokenize(s)
}

// Pretty returns s pretty-printed as rows of tokens with the given indent.
// Empty objects and arrays stay on one line.
func Pretty(s string, indent int) [][]Token {
	all := tokenize(s)
	toks := all[:0:0]
	for _, t := range all {
		if t.Kind != Space {
			toks = append(toks, t)
		}
	}
	pad := func(depth int) []Token {
		if depth <= 0 {
			return nil
		}
		return []Token{{strings.Repeat(" ", depth*indent), Space}}
	}
	var rows [][]Token
	var cur []Token
	depth := 0
	newline := func() {
		rows = append(rows, cur)
		cur = pad(depth)
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.Text {
		case "{", "[":
			if t.Kind != Punct {
				break
			}
			if i+1 < len(toks) && toks[i+1].Kind == Punct && (toks[i+1].Text == "}" || toks[i+1].Text == "]") {
				cur = append(cur, Token{t.Text + toks[i+1].Text, Punct})
				i++
				continue
			}
			cur = append(cur, t)
			depth++
			newline()
			continue
		case "}", "]":
			if t.Kind != Punct {
				break
			}
			depth--
			newline()
			cur = append(cur, t)
			continue
		case ",":
			if t.Kind != Punct {
				break
			}
			cur = append(cur, t)
			newline()
			continue
		case ":":
			if t.Kind != Punct {
				break
			}
			cur = append(cur, Token{": ", Punct})
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	return rows
}

// RowText concatenates the text of a token row.
func RowText(row []Token) string {
	var b strings.Builder
	for _, t := range row {
		b.WriteString(t.Text)
	}
	return b.String()
}
