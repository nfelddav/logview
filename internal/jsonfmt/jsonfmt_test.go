package jsonfmt

import (
	"strings"
	"testing"
)

func TestFind(t *testing.T) {
	cases := map[string]int{
		`{"a":1}`:                             0,
		`2026-10-25 INFO {"a":1,"b":[1,2]}  `: 16,
		`[INFO] {"a":"x"}`:                    7,
		`[1,2,3]`:                             0,
		`no json here`:                        -1,
		`{"a":1} trailing`:                    -1,
		`{broken`:                             -1,
		`msg {"a": "}"} `:                     4,
		`{}`:                                  0,
	}
	for in, want := range cases {
		if got := Find(in); got != want {
			t.Errorf("Find(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestCompactPreservesText(t *testing.T) {
	in := `{"a": 1, "b":[true,null,"x\"y"],"c":{}}`
	toks := Compact(in)
	if RowText(toks) != in {
		t.Fatalf("roundtrip %q", RowText(toks))
	}
	kinds := map[string]Kind{}
	for _, tk := range toks {
		kinds[tk.Text] = tk.Kind
	}
	if kinds[`"a"`] != Key || kinds[`"x\"y"`] != String || kinds["true"] != Bool || kinds["null"] != Null || kinds["1"] != Number {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestPretty(t *testing.T) {
	rows := Pretty(`{"a":1,"b":[1,{"c":"d"}],"e":{},"f":[]}`, 2)
	var lines []string
	for _, r := range rows {
		lines = append(lines, RowText(r))
	}
	want := `{
  "a": 1,
  "b": [
    1,
    {
      "c": "d"
    }
  ],
  "e": {},
  "f": []
}`
	if got := strings.Join(lines, "\n"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrettyStringsWithPunct(t *testing.T) {
	rows := Pretty(`{"k":"a,b{c}:d"}`, 2)
	if len(rows) != 3 || RowText(rows[1]) != `  "k": "a,b{c}:d"` {
		t.Fatalf("%q", rows)
	}
}
