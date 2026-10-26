package ts

import (
	"testing"
	"time"
)

func TestParseFormats(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi, s, ns int) int64 {
		return time.Date(y, mo, d, h, mi, s, ns, time.UTC).UnixNano()
	}
	local := func(y int, mo time.Month, d, h, mi, s, ns int) int64 {
		return time.Date(y, mo, d, h, mi, s, ns, time.Local).UnixNano()
	}
	cases := []struct {
		in   string
		want int64
		pos  int
	}{
		{"2026-10-25T12:34:56.789Z INFO x", utc(2026, 10, 25, 12, 34, 56, 789000000), 0},
		{"2026-10-25T12:34:56+02:00 x", utc(2026, 10, 25, 10, 34, 56, 0), 0},
		{"2026-10-25 12:34:56,5 +0100 x", utc(2026, 10, 25, 11, 34, 56, 500000000), 0},
		{"[2026-10-25 12:34:56] x", local(2026, 10, 25, 12, 34, 56, 0), 1},
		{"2026/10/25 12:34:56 go log", local(2026, 10, 25, 12, 34, 56, 0), 0},
		{"2026-10-25 12:34 - msg", local(2026, 10, 25, 12, 34, 0, 0), 0},
		{`1.2.3.4 - - [25/Oct/2026:12:34:56 +0000] "GET /"`, utc(2026, 10, 25, 12, 34, 56, 0), 13},
		{`{"level":"info","time":"2026-10-25T12:34:56Z","msg":"hi"}`, utc(2026, 10, 25, 12, 34, 56, 0), 0},
		{`{"ts":1792931696.5,"msg":"hi"}`, time.Unix(1792931696, 500000000).UnixNano(), 0},
		{`{"ts":1792931696123,"msg":"hi"}`, time.Unix(1792931696, 123000000).UnixNano(), 0},
	}
	for _, c := range cases {
		got, pos, ok := Parse([]byte(c.in))
		if !ok || got != c.want || pos != c.pos {
			t.Errorf("Parse(%q) = %v,%d,%v want %v,%d", c.in, time.Unix(0, got), pos, ok, time.Unix(0, c.want), c.pos)
		}
	}
}

func TestSyslogYearInference(t *testing.T) {
	now := time.Now()
	line := now.Add(-time.Hour).Format("Jan _2 15:04:05") + " host sshd[1]: hi"
	got, pos, ok := Parse([]byte(line))
	if !ok || pos != 0 || got > now.UnixNano() || got < now.Add(-2*time.Hour).UnixNano() {
		t.Fatalf("Parse(%q) = %v %v", line, time.Unix(0, got), ok)
	}
}

func TestNoFalsePositives(t *testing.T) {
	for _, in := range []string{
		"hello world",
		"error code 2026-99-99T99:99 nope",
		"version 1.2.3 build 20261025",
		"Octopus 25 12:34:56",
		"",
	} {
		if _, _, ok := Parse([]byte(in)); ok {
			t.Errorf("Parse(%q) unexpectedly ok", in)
		}
	}
}

func TestParseUser(t *testing.T) {
	ref := time.Date(2026, 10, 25, 8, 0, 0, 0, time.Local)
	got, err := ParseUser("12:30", ref)
	if err != nil || !got.Equal(time.Date(2026, 10, 25, 12, 30, 0, 0, time.Local)) {
		t.Errorf("12:30 -> %v %v", got, err)
	}
	got, err = ParseUser("2026-10-24", ref)
	if err != nil || !got.Equal(time.Date(2026, 10, 24, 0, 0, 0, 0, time.Local)) {
		t.Errorf("date -> %v %v", got, err)
	}
	got, err = ParseUser("-90m", ref)
	if err != nil || time.Since(got) < 89*time.Minute || time.Since(got) > 91*time.Minute {
		t.Errorf("-90m -> %v %v", got, err)
	}
	got, err = ParseUser("2026-10-25T01:02:03Z", ref)
	if err != nil || !got.Equal(time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)) {
		t.Errorf("iso -> %v %v", got, err)
	}
	if _, err := ParseUser("yesterday-ish", ref); err == nil {
		t.Error("expected error")
	}
}

func BenchmarkParseISO(b *testing.B) {
	line := []byte("2026-10-25T12:34:56.789Z INFO server: request handled")
	for i := 0; i < b.N; i++ {
		Parse(line)
	}
}
