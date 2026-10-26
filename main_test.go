package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/nfelddav/logview/internal/query"
	"github.com/nfelddav/logview/internal/state"
)

// The test binary doubles as the logview binary: re-executed with
// LOGVIEW_RUN_MAIN=1 it runs the real command line.
func TestMain(m *testing.M) {
	if os.Getenv("LOGVIEW_RUN_MAIN") == "1" {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func logview(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "LOGVIEW_RUN_MAIN=1")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("logview %v timed out", args)
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func write(t *testing.T, dir, name, content string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const appLog = `2026-10-25 10:00:00 INFO starting user=alice
2026-10-25 10:00:01 ERROR request failed user=bob
java.lang.IllegalStateException: boom
	at com.example.Foo.bar(Foo.java:10)
2026-10-25 10:00:02 INFO done user=alice
`

func TestDumpFilterKeepsRecords(t *testing.T) {
	p := write(t, t.TempDir(), "app.log", appLog)
	out, errs, code := logview(t, "", "--dump", "-f", "error", p)
	want := "2026-10-25 10:00:01 ERROR request failed user=bob\njava.lang.IllegalStateException: boom\n        at com.example.Foo.bar(Foo.java:10)\n"
	if code != 0 || out != want {
		t.Fatalf("code %d, stderr %q, out:\n%s", code, errs, out)
	}
	out, _, _ = logview(t, "", "--dump", "--record", "off", "-f", "boom", p)
	if out != "java.lang.IllegalStateException: boom\n" {
		t.Fatalf("record off: %q", out)
	}
}

func TestFlagsAfterPathAndRewrite(t *testing.T) {
	p := write(t, t.TempDir(), "app.log", appLog)
	out, _, code := logview(t, "", p, "--dump", "-f", "alice", "-sub", `/user=(\w+)/who=\1/`, "-x", "starting")
	if code != 0 || out != "2026-10-25 10:00:02 INFO done who=alice\n" {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestDumpStdin(t *testing.T) {
	out, _, code := logview(t, "a 1\nb 2\n\x1b[31ma 3\x1b[0m\n", "--dump", "--record", "off", "-f", "^a")
	if code != 0 || out != "a 1\na 3\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	out, _, _ = logview(t, "x\ny\n", "--dump", "-")
	if out != "x\ny\n" {
		t.Fatalf("explicit - : %q", out)
	}
}

func TestDumpMergedAndGlob(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.log", "2026-10-25T10:00:00Z a1\n2026-10-25T10:00:02Z a2\n")
	write(t, dir, "b.log", "2026-10-25T10:00:01Z b1\n2026-10-25T10:00:03Z b2\n")
	out, errs, code := logview(t, "", "--dump", filepath.Join(dir, "*.log"))
	want := "2026-10-25T10:00:00Z a1\n2026-10-25T10:00:01Z b1\n2026-10-25T10:00:02Z a2\n2026-10-25T10:00:03Z b2\n"
	if code != 0 || out != want {
		t.Fatalf("code %d stderr %q out:\n%s", code, errs, out)
	}
}

func TestDumpRotatedSiblings(t *testing.T) {
	dir := t.TempDir()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("oldest\n"))
	zw.Close()
	write(t, dir, "app.log.2.gz", gz.String())
	write(t, dir, "app.log.1", "older\n")
	p := write(t, dir, "app.log", "current\n")
	now := time.Now()
	os.Chtimes(filepath.Join(dir, "app.log.2.gz"), now, now.Add(-2*time.Hour))
	os.Chtimes(filepath.Join(dir, "app.log.1"), now, now.Add(-time.Hour))
	out, _, _ := logview(t, "", "--dump", p)
	if out != "oldest\nolder\ncurrent\n" {
		t.Fatalf("with siblings: %q", out)
	}
	out, _, _ = logview(t, "", "--dump", "--no-rotated", p)
	if out != "current\n" {
		t.Fatalf("--no-rotated: %q", out)
	}
}

func TestDumpTimeRange(t *testing.T) {
	p := write(t, t.TempDir(), "app.log", appLog)
	out, _, code := logview(t, "", "--dump", "--since", "2026-10-25 10:00:01", "--until", "2026-10-25 10:00:01", p)
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.HasPrefix(out, "2026-10-25 10:00:01 ERROR") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "app.log", appLog)
	cases := []struct {
		args []string
		msg  string
	}{
		{[]string{"--dump", "-f", "(unclosed", p}, "-f (unclosed"},
		{[]string{"--dump", dir}, "is a directory"},
		{[]string{"--dump", filepath.Join(dir, "*.nope")}, "no matching files"},
		{[]string{"--dump", "--preserve", "lots", p}, "-preserve"},
		{[]string{"--dump", "--since", "whenever", p}, "unrecognised time"},
		{[]string{"--dump", "--cols", "nogroups", p}, "capture groups"},
		{[]string{"--bogus-flag", p}, "flag provided but not defined"},
	}
	for _, c := range cases {
		_, errs, code := logview(t, "", c.args...)
		if code != 2 || !strings.Contains(errs, c.msg) {
			t.Errorf("%v: code %d stderr %q (want %q)", c.args, code, errs, c.msg)
		}
	}
	out, _, code := logview(t, "", "--version")
	if code != 0 || !strings.HasPrefix(out, "logview ") {
		t.Errorf("--version: %d %q", code, out)
	}
}

func TestDumpMissingFileReportsStatus(t *testing.T) {
	out, errs, code := logview(t, "", "--dump", filepath.Join(t.TempDir(), "missing.log"))
	if code != 0 || out != "" || !strings.Contains(errs, "waiting for file") {
		t.Fatalf("code %d out %q stderr %q", code, out, errs)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"256M": 256 << 20, "1g": 1 << 30, "512KiB": 512 << 10, "100": 100, "all": -1, "1.5G": 3 << 29} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
}

func TestRestoreRulesRoundTrip(t *testing.T) {
	inc, _ := query.Compile("error")
	rw, _ := query.ParseSub(`/user=(\w+)/who=\1/`)
	hl, _ := query.ParseHighlight("bob red", 0)
	r := &state.Rules{
		Filters: []string{inc.String()}, Excludes: []string{"health"},
		Subs: []string{rw.Src}, Highlights: []string{hl.Src}, Cols: ".level .msg", Record: "off",
	}
	cfg, hls, cols, warns := restoreRules(r, query.Config{})
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if cfg.Includes[0].String() != inc.String() || !cfg.Includes[0].MatchString("ERROR") {
		t.Errorf("include %q", cfg.Includes[0])
	}
	if cfg.Excludes[0].String() != "health" || cfg.Rewrites[0].Src != rw.Src || cfg.Record.Kind != query.RecOff {
		t.Errorf("cfg %+v", cfg)
	}
	if query.ApplyRewrites(cfg.Rewrites, "user=al") != "who=al" {
		t.Error("rewrite")
	}
	if len(hls) != 1 || hls[0].Style != hl.Style || cols == nil || cols.Spec != ".level .msg" {
		t.Errorf("hl %+v cols %+v", hls, cols)
	}
	_, _, _, warns = restoreRules(&state.Rules{Filters: []string{"(bad"}, Subs: []string{"nope"}}, query.Config{})
	if len(warns) != 2 {
		t.Errorf("bad rules should be reported: %v", warns)
	}
}

func TestLogKey(t *testing.T) {
	a := logKey([]string{"b.log", "a.log"})
	b := logKey([]string{"a.log", "b.log"})
	if a == "" || a != b || !filepath.IsAbs(strings.Split(a, " + ")[0]) {
		t.Errorf("%q %q", a, b)
	}
	if logKey([]string{"a.log", "-"}) != "" {
		t.Error("stdin must not be remembered")
	}
}

// session runs the interactive viewer on a simulated terminal, waits for
// each of want to appear, types keys in between, then quits.
func session(t *testing.T, args []string, steps ...string) {
	t.Helper()
	sim := tcell.NewSimulationScreen("")
	newScreen = func() (tcell.Screen, error) { return sim, nil }
	defer func() { newScreen = terminalScreen }()
	done := make(chan int, 1)
	go func() { done <- run(args) }()
	screen := func() string {
		cells, w, _ := sim.GetContents()
		var b strings.Builder
		for i, c := range cells {
			if i > 0 && i%w == 0 {
				b.WriteByte('\n')
			}
			if len(c.Runes) > 0 {
				b.WriteString(string(c.Runes))
			} else {
				b.WriteByte(' ')
			}
		}
		return b.String()
	}
	waitFor := func(s string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(screen(), s) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; screen:\n%s", s, screen())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Steps alternate: text to wait for, keys to type ("" types nothing).
	for i := 0; i < len(steps); i += 2 {
		waitFor(steps[i])
		if i+1 < len(steps) {
			for _, r := range steps[i+1] {
				if r == '\n' {
					sim.InjectKey(tcell.KeyEnter, 0, 0)
				} else {
					sim.InjectKey(tcell.KeyRune, r, 0)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
	sim.InjectKey(tcell.KeyRune, 'q', 0)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("viewer did not quit")
	}
}

func TestSettingsRememberedAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, ".logview.json")
	p := write(t, dir, "app.log", appLog)

	// Run 1: add a filter and hide line numbers.
	session(t, []string{"--state", st, p}, "starting", "ferror\n#", "+error", "")
	f, err := state.Load(st)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Logs[logKey([]string{p})]
	if f.Display.LineNumbers || r == nil || strings.Join(r.Filters, ",") != "(?i)error" {
		t.Fatalf("saved: %+v %+v", f.Display, r)
	}

	// Run 2: both are restored.
	session(t, []string{"--state", st, p}, "+error", "", "3 matching", "")

	// Run 3: rules given as flags replace the remembered ones (and are remembered).
	session(t, []string{"--state", st, "-f", "alice", p}, "+alice", "")
	if f, _ = state.Load(st); strings.Join(f.Logs[logKey([]string{p})].Filters, ",") != "(?i)alice" {
		t.Fatalf("flags not remembered: %+v", f.Logs)
	}

	// --no-state ignores the file entirely; a different log has its own rules.
	session(t, []string{"--state", st, "--no-state", p}, "bob", "")
	other := write(t, dir, "other.log", appLog)
	session(t, []string{"--state", st, other}, "bob", "")
}

func TestCorruptStateFileIsReportedNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	st := write(t, dir, ".logview.json", "{oops")
	p := write(t, dir, "app.log", appLog)
	session(t, []string{"--state", st, p}, "settings neither loaded nor saved", "w")
	if b, _ := os.ReadFile(st); string(b) != "{oops" {
		t.Fatalf("state file overwritten: %q", b)
	}
}
