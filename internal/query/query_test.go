package query

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nfelddav/logview/internal/logsrc"
)

func openStatic(t *testing.T, content string) logsrc.Seq {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.log")
	os.WriteFile(path, []byte(content), 0o644)
	s := logsrc.OpenFile(path, logsrc.Options{NoTail: true, SpoolDir: t.TempDir()})
	t.Cleanup(s.Close)
	deadline := time.Now().Add(5 * time.Second)
	for !s.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("source not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return s
}

func visibleLines(t *testing.T, seq logsrc.Seq, cfg *Config) []string {
	t.Helper()
	f := NewFilter(seq, cfg)
	defer f.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for !f.Done() {
		if time.Now().After(deadline) {
			t.Fatal("filter not done")
		}
		time.Sleep(time.Millisecond)
	}
	var out []string
	for g := 0; g < seq.NumSegs(); g++ {
		for i := f.Next(g, 0); i >= 0; i = f.Next(g, i+1) {
			b, _, _ := seq.Line(g, i)
			out = append(out, string(b))
		}
	}
	if int64(len(out)) != f.Count() {
		t.Fatalf("Count %d != %d visible", f.Count(), len(out))
	}
	return out
}

func mustRe(t *testing.T, p string) *regexp.Regexp {
	re, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

const javaLog = `2026-10-25 10:00:00 INFO starting
2026-10-25 10:00:01 ERROR request failed
java.lang.IllegalStateException: boom
	at com.example.Foo.bar(Foo.java:10)
	at com.example.Main.main(Main.java:5)
2026-10-25 10:00:02 INFO user=alice ok
2026-10-25 10:00:03 WARN slow request user=bob
`

func TestRecordsAutoKeepStackTraces(t *testing.T) {
	seq := openStatic(t, javaLog)
	got := visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "error")}})
	want := []string{
		"2026-10-25 10:00:01 ERROR request failed",
		"java.lang.IllegalStateException: boom",
		"\tat com.example.Foo.bar(Foo.java:10)",
		"\tat com.example.Main.main(Main.java:5)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
	// Matching a continuation line selects the whole record too.
	got = visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "Main.java")}})
	if len(got) != 4 || got[0] != want[0] {
		t.Fatalf("continuation match: %q", got)
	}
	// Record mode off: only the matching line.
	got = visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "Main.java")}, Record: RecordMode{Kind: RecOff}})
	if len(got) != 1 {
		t.Fatalf("record off: %q", got)
	}
}

func TestExcludeHidesWholeRecord(t *testing.T) {
	seq := openStatic(t, javaLog)
	got := visibleLines(t, seq, &Config{Excludes: []*regexp.Regexp{mustRe(t, "IllegalState")}})
	for _, l := range got {
		if strings.Contains(l, "ERROR") || strings.Contains(l, "\tat") {
			t.Fatalf("excluded record leaked: %q", got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %q", got)
	}
}

func TestIncludesAreANDed(t *testing.T) {
	seq := openStatic(t, javaLog)
	got := visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "user="), mustRe(t, "warn")}})
	if len(got) != 1 || !strings.Contains(got[0], "bob") {
		t.Fatalf("%q", got)
	}
}

func TestRecordsWithoutTimestampsUseIndentation(t *testing.T) {
	seq := openStatic(t, "Traceback (most recent call last):\n  File \"x.py\", line 1\nValueError: bad\nnext thing\n  detail of next\n")
	got := visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "x.py")}})
	if len(got) != 2 || got[0] != "Traceback (most recent call last):" {
		t.Fatalf("%q", got)
	}
}

func TestRecordRegex(t *testing.T) {
	seq := openStatic(t, "BEGIN a\nfoo\nBEGIN b\nbar\nbaz\n")
	rm, _ := ParseRecordMode("^BEGIN")
	got := visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "baz")}, Record: rm})
	if strings.Join(got, ",") != "BEGIN b,bar,baz" {
		t.Fatalf("%q", got)
	}
}

func TestTimeRange(t *testing.T) {
	seq := openStatic(t, javaLog)
	since := time.Date(2026, 10, 25, 10, 0, 1, 0, time.Local).UnixNano()
	until := time.Date(2026, 10, 25, 10, 0, 2, 0, time.Local).UnixNano()
	got := visibleLines(t, seq, &Config{Since: since, Until: until})
	if len(got) != 5 || !strings.Contains(got[0], "ERROR") || !strings.Contains(got[4], "alice") {
		t.Fatalf("%q", got)
	}
}

func TestRewriteAffectsFiltering(t *testing.T) {
	seq := openStatic(t, javaLog)
	rw, err := ParseSub(`/user=(\w+)/who:\1/`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Includes: []*regexp.Regexp{mustRe(t, "who:alice")}, Rewrites: []Rewrite{rw}}
	if got := visibleLines(t, seq, cfg); len(got) != 1 {
		t.Fatalf("%q", got)
	}
	if got := cfg.Text([]byte("x user=bob y")); got != "x who:bob y" {
		t.Fatalf("Text = %q", got)
	}
}

func TestFilterSeesThroughANSI(t *testing.T) {
	seq := openStatic(t, "\x1b[31mERR\x1b[0mOR one\nplain two\n")
	got := visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "^error")}, Record: RecordMode{Kind: RecOff}})
	if len(got) != 1 {
		t.Fatalf("%q", got)
	}
}

func TestFilterLargeParallel(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		if i%7 == 0 {
			b.WriteString("2026-10-25 10:00:00 match line\n")
		} else {
			b.WriteString("2026-10-25 10:00:00 other line\n")
		}
	}
	seq := openStatic(t, b.String())
	got := visibleLines(t, seq, &Config{Includes: []*regexp.Regexp{mustRe(t, "match")}})
	if len(got) != (50000+6)/7 {
		t.Fatalf("got %d", len(got))
	}
}

func TestSmartCase(t *testing.T) {
	if !mustRe(t, "error").MatchString("ERROR") {
		t.Error("lower-case pattern should ignore case")
	}
	if mustRe(t, "Error").MatchString("ERROR") {
		t.Error("mixed-case pattern should respect case")
	}
	if !mustRe(t, `\S+error`).MatchString("xERROR") {
		t.Error(`\S must not count as upper case`)
	}
}

func TestParseSub(t *testing.T) {
	for in, want := range map[string]string{
		`/a(\d)/b\1/`:         "xb1y",
		`s/a(\d)/b$1/`:        "xb1y",
		`|a\d|[gone]|`:        "x[gone]y",
		`#a(?P<n>\d)#<${n}>#`: "x<1>y",
	} {
		rw, err := ParseSub(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := ApplyRewrites([]Rewrite{rw}, "xa1y"); got != want {
			t.Errorf("%s: %q want %q", in, got, want)
		}
	}
	if _, err := ParseSub("/only"); err == nil {
		t.Error("expected error")
	}
}

func TestBitset(t *testing.T) {
	var b Bitset
	for _, i := range []int64{3, 64, 65, 200, 1000} {
		b.Set(i)
	}
	b.Set(64)
	if b.Count() != 5 || b.NextSet(4, 2000) != 64 || b.NextSet(66, 2000) != 200 || b.NextSet(201, 1000) != -1 {
		t.Fatal("NextSet")
	}
	if b.PrevSet(199) != 65 || b.PrevSet(2) != -1 || b.PrevSet(5000) != 1000 || b.Rank(65) != 2 || b.Rank(1001) != 5 {
		t.Fatal("PrevSet/Rank")
	}
	b.Clear(64)
	if b.Test(64) || b.Count() != 4 {
		t.Fatal("Clear")
	}
}

func TestColumns(t *testing.T) {
	c, err := ParseColumns(`^(?P<ts>\S+ \S+) (?P<level>\w+) (.*)$`)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := c.Extract("2026-10-25 10:00:00 INFO hello world")
	if !ok || strings.Join(c.Names, ",") != "ts,level,$3" || v[1] != "INFO" || v[2] != "hello world" {
		t.Fatalf("%v %q %v", c.Names, v, ok)
	}
	j, _ := ParseColumns(".level .user.id .missing .tags")
	v, ok = j.Extract(`prefix {"level":"warn","user":{"id":42},"tags":["a","b"]}`)
	if !ok || strings.Join(v, "|") != `warn|42||["a","b"]` {
		t.Fatalf("%q %v", v, ok)
	}
}

func TestSearch(t *testing.T) {
	seq := openStatic(t, "a\nfoo 1\nb\nfoo 2\nc\n")
	var cancel atomic.Bool
	re := mustRe(t, "foo")
	cfg := &Config{}
	p, wrapped, ok := Search(seq, cfg, re, Pos{0, 1}, true, nil, &cancel, nil)
	if !ok || p.I != 3 || wrapped {
		t.Fatalf("forward: %v %v %v", p, wrapped, ok)
	}
	p, wrapped, ok = Search(seq, cfg, re, Pos{0, 3}, true, nil, &cancel, nil)
	if !ok || p.I != 1 || !wrapped {
		t.Fatalf("wrap: %v %v %v", p, wrapped, ok)
	}
	p, wrapped, ok = Search(seq, cfg, re, Pos{0, 3}, false, nil, &cancel, nil)
	if !ok || p.I != 1 || wrapped {
		t.Fatalf("backward: %v %v %v", p, wrapped, ok)
	}
	p, _, ok = Search(seq, cfg, re, Pos{0, 3}, true, func(g int, i int64) bool { return i != 1 }, &cancel, nil)
	if !ok || p.I != 3 {
		t.Fatalf("visible-only: %v %v", p, ok)
	}
	if _, _, ok = Search(seq, cfg, mustRe(t, "nothing"), Pos{0, 0}, true, nil, &cancel, nil); ok {
		t.Fatal("found nothing")
	}
}
