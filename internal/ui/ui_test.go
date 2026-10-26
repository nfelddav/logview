package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/nfelddav/logview/internal/logsrc"
)

type harness struct {
	t    *testing.T
	a    *App
	scr  tcell.SimulationScreen
	path string
}

func srcOpts(t *testing.T) logsrc.Options {
	return logsrc.Options{Preserve: -1, SpoolDir: t.TempDir(), Poll: 20 * time.Millisecond, Grace: 100 * time.Millisecond}
}

func waitReady(t *testing.T, q logsrc.Seq) {
	deadline := time.Now().Add(5 * time.Second)
	for !q.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("source not ready")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newHarness(t *testing.T, content string, o Options, w, h int) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(path, []byte(content), 0o644)
	so := srcOpts(t)
	src := logsrc.OpenFile(path, so)
	t.Cleanup(src.Close)
	waitReady(t, src)
	return newHarnessSeq(t, src, path, o, w, h)
}

func newHarnessSeq(t *testing.T, seq logsrc.Seq, path string, o Options, w, h int) *harness {
	scr := tcell.NewSimulationScreen("")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(w, h)
	a := New(scr, seq, o)
	t.Cleanup(func() {
		if a.filter != nil {
			a.filter.Stop()
		}
		scr.Fini()
	})
	hs := &harness{t: t, a: a, scr: scr, path: path}
	hs.draw()
	return hs
}

func (h *harness) draw() {
	h.a.Pending()
	h.a.Draw()
}

func (h *harness) rows() []string {
	cells, w, ht := h.scr.GetContents()
	out := make([]string, ht)
	for y := 0; y < ht; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
			} else {
				b.WriteString(string(c.Runes))
			}
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

func (h *harness) cellAt(x, y int) tcell.SimCell {
	cells, w, _ := h.scr.GetContents()
	return cells[y*w+x]
}

func (h *harness) screen() string { return strings.Join(h.rows(), "\n") }

// until redraws until cond holds for the screen.
func (h *harness) until(what string, cond func(screen string) bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.draw()
		s := h.screen()
		if cond(s) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; screen:\n%s", what, s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) contains(sub string) {
	h.until(fmt.Sprintf("%q", sub), func(s string) bool { return strings.Contains(s, sub) })
}

func (h *harness) lacks(sub string) {
	h.t.Helper()
	h.draw()
	if s := h.screen(); strings.Contains(s, sub) {
		h.t.Fatalf("screen unexpectedly contains %q:\n%s", sub, s)
	}
}

func (h *harness) keys(s string) {
	for _, r := range s {
		switch r {
		case '\n':
			h.a.Handle(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
		case '\x1b':
			h.a.Handle(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
		default:
			h.a.Handle(tcell.NewEventKey(tcell.KeyRune, r, 0))
		}
	}
	h.draw()
}

func (h *harness) key(k tcell.Key) {
	h.a.Handle(tcell.NewEventKey(k, 0, 0))
	h.draw()
}

// cursorRow returns the screen row marked as the cursor line.
func (h *harness) cursorRow() string {
	for _, r := range h.rows() {
		if strings.HasPrefix(r, ">") {
			return r
		}
	}
	return ""
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "2026-10-25 10:00:%02d INFO line %d\n", i%60, i)
	}
	return b.String()
}

func defaults() Options { return Options{Follow: true, Wrap: true, LineNumbers: true, DefaultHL: true} }

func TestFollowShowsNewLines(t *testing.T) {
	h := newHarness(t, numbered(30), defaults(), 80, 10)
	h.contains("line 30")
	h.lacks("line 1\n")
	if !strings.Contains(h.cursorRow(), "line 30") {
		t.Fatalf("cursor not on last line:\n%s", h.screen())
	}
	f, _ := os.OpenFile(h.path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("2026-10-25 10:01:00 WARN brand new line\n")
	f.Close()
	h.contains("brand new line")
	if !strings.Contains(h.cursorRow(), "brand new line") || !strings.Contains(h.screen(), "FOLLOW") {
		t.Fatalf("follow did not track:\n%s", h.screen())
	}
	// Moving up stops following.
	h.keys("k")
	if strings.Contains(h.screen(), "FOLLOW") {
		t.Fatal("still following after moving up")
	}
}

func TestNavigation(t *testing.T) {
	o := defaults()
	o.Follow = false
	h := newHarness(t, numbered(100), o, 80, 10)
	h.contains("line 1\n")
	h.keys("jjj")
	if !strings.Contains(h.cursorRow(), "line 4") {
		t.Fatalf("after jjj:\n%s", h.screen())
	}
	h.keys("G")
	h.contains("line 100")
	h.keys("g")
	h.contains("line 1\n")
	h.key(tcell.KeyPgDn)
	if !strings.Contains(h.screen(), "line 10") || strings.Contains(h.screen(), "line 1\n") {
		t.Fatalf("after PgDn:\n%s", h.screen())
	}
	h.keys(":goto 50\n")
	if !strings.Contains(h.cursorRow(), "line 50") {
		t.Fatalf("goto:\n%s", h.screen())
	}
}

func TestFilterAndUndo(t *testing.T) {
	content := numbered(50) + "2026-10-25 10:02:00 ERROR disk full\n2026-10-25 10:02:01 ERROR disk on fire\n"
	h := newHarness(t, content, defaults(), 80, 10)
	h.keys("ferror\n")
	h.contains("2 matching")
	s := h.screen()
	if !strings.Contains(s, "disk full") || !strings.Contains(s, "disk on fire") || strings.Contains(s, "INFO line") {
		t.Fatalf("filter:\n%s", s)
	}
	if !strings.Contains(s, "+error") {
		t.Fatalf("info line missing:\n%s", s)
	}
	h.keys("xfire\n")
	h.contains("1 matching")
	h.lacks("on fire")
	h.keys("u")
	h.contains("2 matching")
	h.keys("u")
	h.contains("INFO line 50")
}

func TestSearch(t *testing.T) {
	o := defaults()
	o.Follow = false
	content := numbered(200)
	h := newHarness(t, content, o, 80, 10)
	h.keys("/line 150\n")
	h.until("search result", func(string) bool { return strings.Contains(h.cursorRow(), "line 150") })
	// The match is highlighted.
	rows := h.rows()
	for y, r := range rows {
		if strings.HasPrefix(r, ">") {
			x := strings.Index(r, "line 150")
			if _, bg, _ := h.cellAt(x, y).Style.Decompose(); bg != tcell.ColorYellow {
				t.Fatalf("match not highlighted (bg %v)", bg)
			}
		}
	}
	h.keys("/line 1\\d\\d$\n")
	h.until("next", func(string) bool { return strings.Contains(h.cursorRow(), "line 151") })
	h.keys("n")
	h.until("n", func(string) bool { return strings.Contains(h.cursorRow(), "line 152") })
	h.keys("N")
	h.until("N", func(string) bool { return strings.Contains(h.cursorRow(), "line 151") })
	h.keys("/nonexistent\n")
	h.contains("pattern not found")
}

func TestJSONPrettyPrint(t *testing.T) {
	content := `2026-10-25 10:00:00 INFO {"user":{"id":42,"name":"ann"},"ok":true}` + "\n"
	h := newHarness(t, content, defaults(), 80, 12)
	h.contains(`{"user":{"id":42,"name":"ann"},"ok":true}`)
	h.key(tcell.KeyEnter)
	h.contains(`"name": "ann"`)
	s := h.screen()
	if !strings.Contains(s, `"id": 42,`) || !strings.Contains(s, `"ok": true`) {
		t.Fatalf("pretty:\n%s", s)
	}
	h.keys("J") // global toggle resets per-line toggles
	h.contains(`"name": "ann"`)
	h.keys("J")
	h.contains(`{"user":{"id":42`)
}

func TestANSIColoursRendered(t *testing.T) {
	h := newHarness(t, "plain \x1b[31mred text\x1b[0m done\x1b]0;title\x07\n", defaults(), 80, 6)
	h.contains("plain red text done")
	for y, r := range h.rows() {
		if x := strings.Index(r, "red text"); x >= 0 {
			if fg, _, _ := h.cellAt(x, y).Style.Decompose(); fg != tcell.PaletteColor(1) {
				t.Fatalf("fg = %v", fg)
			}
			return
		}
	}
	t.Fatal("row not found")
}

func TestTruncationKeepsOldGeneration(t *testing.T) {
	h := newHarness(t, numbered(5), defaults(), 100, 16)
	h.contains("line 5")
	os.Truncate(h.path, 0)
	os.WriteFile(h.path, []byte("2026-10-25 11:00:00 INFO after truncate\n"), 0o644)
	h.contains("after truncate")
	h.contains("after truncation at")
	if !strings.Contains(h.screen(), "line 5") {
		t.Fatalf("old generation gone:\n%s", h.screen())
	}
	// The cursor is on the first line of the newest generation, so [ goes
	// to the previous generation, and ] comes back.
	h.keys("[")
	if !strings.Contains(h.cursorRow(), "line 1") {
		t.Fatalf("[ should go to the previous generation:\n%s", h.screen())
	}
	h.keys("jj[")
	if !strings.Contains(h.cursorRow(), "line 1") {
		t.Fatalf("[ mid-generation should go to its start:\n%s", h.screen())
	}
	h.keys("]")
	if !strings.Contains(h.cursorRow(), "after truncate") {
		t.Fatalf("] should go to the next generation:\n%s", h.screen())
	}
}

func TestLostLinesCollapse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(path, []byte(numbered(100)), 0o644)
	so := srcOpts(t)
	so.NoSpool = true
	src := logsrc.OpenFile(path, so)
	t.Cleanup(src.Close)
	waitReady(t, src)
	h := newHarnessSeq(t, src, path, defaults(), 100, 10)
	os.WriteFile(path, []byte("2026-10-25 11:00:00 INFO fresh\n"), 0o644)
	h.contains("fresh")
	h.contains("100 lines not preserved")
	h.keys("g")
	if !strings.Contains(h.cursorRow(), "not preserved") {
		t.Fatalf("collapsed row:\n%s", h.screen())
	}
	h.keys("j")
	if !strings.Contains(h.cursorRow(), "fresh") {
		t.Fatalf("j should skip the lost run:\n%s", h.screen())
	}
}

func TestColumnsAndRewrite(t *testing.T) {
	content := `{"level":"info","msg":"hello","token":"abc123"}` + "\n" + `{"level":"error","msg":"oh no","token":"zzz"}` + "\n"
	h := newHarness(t, content, defaults(), 80, 8)
	h.keys(":cols .level .msg\n")
	h.contains("level │ msg")
	h.contains("error │ oh no")
	h.keys(":cols off\n")
	h.keys(`:sub /"token":"\w+"/"token":"***"/` + "\n")
	h.contains(`"token":"***"`)
	h.lacks("abc123")
}

func TestTimeJump(t *testing.T) {
	o := defaults()
	o.Follow = false
	h := newHarness(t, numbered(50), o, 80, 8)
	h.keys(":time 10:00:33\n")
	if !strings.Contains(h.cursorRow(), "line 33") {
		t.Fatalf("time jump:\n%s", h.screen())
	}
	h.keys(":since 10:00:40\n")
	h.contains("11 matching")
}

func TestHelpOverlay(t *testing.T) {
	h := newHarness(t, numbered(3), defaults(), 110, 30)
	h.keys("H")
	h.contains("move one line")
	// Every key/command description starts in the same column.
	lines := helpLines()
	col := -1
	for _, s := range helpSections {
		for _, e := range s.entries {
			found := false
			for _, l := range lines {
				if strings.HasPrefix(l.text, "  "+e.keys+" ") && strings.HasSuffix(l.text, e.desc) {
					c := len([]rune(l.text)) - len([]rune(e.desc))
					if col < 0 {
						col = c
					}
					if c != col {
						t.Fatalf("misaligned help line %q: description at column %d, want %d", l.text, c, col)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("entry %q missing", e.keys)
			}
		}
	}
	// It scrolls when the screen is too short, and other keys close it.
	if !strings.Contains(h.screen(), "lines 1-") {
		t.Fatalf("no scroll footer:\n%s", h.screen())
	}
	h.key(tcell.KeyPgDn)
	h.contains("Display")
	h.keys("G")
	h.contains("~/.logview.json")
	h.keys("x")
	h.lacks("move one line")
	if h.a.prompt != nil {
		t.Fatal("key closing help must not also act")
	}
}

func TestMergedSourceTags(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "api.log"), filepath.Join(dir, "db.log")
	os.WriteFile(a, []byte("2026-10-25 10:00:00 INFO api start\n2026-10-25 10:00:02 INFO api ready\n"), 0o644)
	os.WriteFile(b, []byte("2026-10-25 10:00:01 INFO db start\n"), 0o644)
	so := srcOpts(t)
	m := logsrc.NewMerged([]*logsrc.Source{logsrc.OpenFile(a, so), logsrc.OpenFile(b, so)})
	t.Cleanup(m.Close)
	waitReady(t, m)
	h := newHarnessSeq(t, m, a, defaults(), 80, 8)
	h.contains("api ready")
	rows := h.rows()
	var order []string
	for _, r := range rows {
		if strings.Contains(r, " start") || strings.Contains(r, " ready") {
			order = append(order, r)
		}
	}
	if len(order) != 3 || !strings.Contains(order[1], "db.log") || !strings.Contains(order[1], "db start") || !strings.Contains(order[0], "api.log") {
		t.Fatalf("merged view:\n%s", h.screen())
	}
}

func TestExportVisible(t *testing.T) {
	content := "keep 1\ndrop 2\nkeep \x1b[32m3\x1b[0m\n"
	h := newHarness(t, content, defaults(), 80, 8)
	h.keys(":record off\n")
	h.keys("fkeep\n")
	h.contains("2 matching")
	out := filepath.Join(t.TempDir(), "out.txt")
	h.keys(":w " + out + "\n")
	h.contains("wrote 2 lines")
	got, _ := os.ReadFile(out)
	if string(got) != "keep 1\nkeep 3\n" {
		t.Fatalf("export = %q", got)
	}
}

func TestNoWrapHorizontalScroll(t *testing.T) {
	long := "start " + strings.Repeat("x", 100) + " END"
	h := newHarness(t, long+"\n", defaults(), 40, 6)
	h.contains("END") // wrapped by default
	h.keys("w")
	h.lacks("END")
	for i := 0; i < 10; i++ {
		h.key(tcell.KeyRight)
	}
	h.contains("END")
}

func TestPromptEditing(t *testing.T) {
	h := newHarness(t, numbered(3), defaults(), 80, 6)
	h.keys(":filtr")
	h.key(tcell.KeyLeft)
	h.keys("e")
	h.key(tcell.KeyEnd)
	h.keys(" line 2")
	if string(h.a.prompt.buf) != "filter line 2" {
		t.Fatalf("buf %q", string(h.a.prompt.buf))
	}
	h.key(tcell.KeyEnter)
	h.contains("1 matching")
	h.keys(":")
	h.key(tcell.KeyUp)
	if string(h.a.prompt.buf) != "filter line 2" {
		t.Fatalf("history %q", string(h.a.prompt.buf))
	}
	h.key(tcell.KeyEscape)
	if h.a.prompt != nil {
		t.Fatal("escape should close prompt")
	}
	h.keys(":bogus\n")
	h.contains("unknown command")
}

func TestSettingsChangesAreReported(t *testing.T) {
	o := defaults()
	var got []Settings
	o.OnChange = func(s Settings) error { got = append(got, s); return nil }
	h := newHarness(t, numbered(20), o, 80, 10)
	h.keys("kkj")
	if len(got) != 0 {
		t.Fatalf("navigation reported a change: %+v", got)
	}
	h.keys("#")
	if len(got) != 1 || got[0].LineNumbers {
		t.Fatalf("line numbers: %+v", got)
	}
	h.keys("ferror\n")
	h.keys(`:sub /line (\d+)/L$1/` + "\n")
	h.keys(":hl INFO red\n")
	h.keys(`:cols ^(?P<d>\S+)` + "\n")
	last := got[len(got)-1]
	if strings.Join(last.Filters, ",") != "(?i)error" || strings.Join(last.Subs, ",") != `/line (\d+)/L$1/` ||
		strings.Join(last.Highlights, ",") != "INFO red" || last.Cols != `^(?P<d>\S+)` {
		t.Fatalf("rules: %+v", last)
	}
	h.keys("C")
	last = got[len(got)-1]
	if len(last.Filters)+len(last.Subs)+len(last.Highlights) != 0 || last.Cols != "" {
		t.Fatalf("after clear: %+v", last)
	}
}

func TestSettingsSaveErrorShown(t *testing.T) {
	o := defaults()
	o.OnChange = func(Settings) error { return fmt.Errorf("disk full") }
	h := newHarness(t, numbered(3), o, 80, 6)
	h.keys("w")
	h.contains("saving settings: disk full")
}

func truncatedHarness(t *testing.T) *harness {
	h := newHarness(t, numbered(5), defaults(), 100, 16)
	h.contains("line 5")
	os.Truncate(h.path, 0)
	os.WriteFile(h.path, []byte("2026-10-25 11:00:00 INFO after truncate\n"), 0o644)
	h.contains("after truncation at")
	return h
}

func TestForgetGeneration(t *testing.T) {
	h := truncatedHarness(t)
	h.keys("X") // the cursor is on the live file
	h.contains("still being written")
	h.keys("[")
	h.keys("X")
	h.contains("Forget app.log (5 lines)? Files on disk are not touched. (y/n)")
	h.keys("n")
	h.contains("cancelled")
	h.contains("line 5")
	h.keys("Xy")
	h.contains("forgot app.log")
	h.lacks("line 5")
	h.lacks("truncated at")
	if !strings.Contains(h.cursorRow(), "after truncate") {
		t.Fatalf("cursor should move to the remaining generation:\n%s", h.screen())
	}
}

func TestForgetCommandWithFilter(t *testing.T) {
	h := truncatedHarness(t)
	h.keys("finfo\n")
	h.contains("6 matching")
	h.keys(":forget old\n")
	h.contains("forgot 1 generations")
	h.contains("1 matching")
	h.lacks("line 3")
	h.keys(":forget 2\n")
	h.contains("still being written")
	h.keys(":forget 1\n")
	h.contains("no such generation")
}
