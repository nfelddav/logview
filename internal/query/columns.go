package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nfelddav/logview/internal/jsonfmt"
)

// Columns extracts fields from lines for a table view, either with a regex
// whose (named) groups are the columns, or with JSON paths like .level .user.id
type Columns struct {
	Spec  string
	Names []string
	re    *regexp.Regexp
	paths [][]string
}

func ParseColumns(spec string) (*Columns, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("empty column spec")
	}
	c := &Columns{Spec: spec}
	if strings.HasPrefix(spec, ".") {
		for _, f := range strings.FieldsFunc(spec, func(r rune) bool { return r == ' ' || r == ',' }) {
			p := strings.Split(strings.TrimPrefix(f, "."), ".")
			c.paths = append(c.paths, p)
			c.Names = append(c.Names, strings.Join(p, "."))
		}
		return c, nil
	}
	re, err := regexp.Compile(spec)
	if err != nil {
		return nil, err
	}
	if re.NumSubexp() == 0 {
		return nil, errors.New("column regex needs capture groups, e.g. (?P<level>\\w+)")
	}
	c.re = re
	for i, n := range re.SubexpNames()[1:] {
		if n == "" {
			n = fmt.Sprintf("$%d", i+1)
		}
		c.Names = append(c.Names, n)
	}
	return c, nil
}

// Extract returns the column values for text, or false if it does not match.
func (c *Columns) Extract(text string) ([]string, bool) {
	if c.re != nil {
		m := c.re.FindStringSubmatch(text)
		if m == nil {
			return nil, false
		}
		return m[1:], true
	}
	at := jsonfmt.Find(text)
	if at < 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(text[at:]))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	out := make([]string, len(c.paths))
	for i, p := range c.paths {
		out[i] = formatValue(lookup(v, p))
	}
	return out, true
}

func lookup(v any, path []string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = m[k]; !ok {
			return nil
		}
	}
	return v
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return fmt.Sprint(x)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSpace(b.String())
}
