// Command logview is a terminal log viewer: it follows log files through
// rotation and truncation, keeps earlier generations around while it runs,
// and filters, searches, rewrites, highlights and pretty-prints them.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"

	"github.com/nfelddav/logview/internal/logsrc"
	"github.com/nfelddav/logview/internal/query"
	"github.com/nfelddav/logview/internal/state"
	"github.com/nfelddav/logview/internal/ts"
	"github.com/nfelddav/logview/internal/ui"
)

var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

const usageText = `usage: logview [flags] PATH...
       command | logview [flags] [-]

Follows each PATH (which need not exist yet) like tail -F. Rotated copies
next to it (PATH.1, PATH.2.gz, ...) are loaded too. Several PATHs are merged
by timestamp. Contents seen before a truncation or rotation are kept until
logview exits. Press H inside for keys and commands.

flags:
`

func main() { os.Exit(run(os.Args[1:])) }

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "ALL" || s == "-1" {
		return -1, nil
	}
	mult := int64(1)
	for suf, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(s, suf) || strings.HasSuffix(s, suf+"B") || strings.HasSuffix(s, suf+"IB") {
			s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B"), suf)
			mult = m
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(n * float64(mult)), nil
}

func expandPaths(args []string) ([]string, error) {
	var out []string
	for _, p := range args {
		if p == "-" {
			out = append(out, p)
			continue
		}
		if _, err := os.Stat(p); err != nil && strings.ContainsAny(p, "*?[") {
			m, gerr := filepath.Glob(p)
			if gerr != nil {
				return nil, gerr
			}
			if len(m) == 0 {
				return nil, fmt.Errorf("%s: no matching files", p)
			}
			out = append(out, m...)
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return nil, fmt.Errorf("%s is a directory", p)
		}
		out = append(out, p)
	}
	return out, nil
}

func run(args []string) int {
	fs := flag.NewFlagSet("logview", flag.ContinueOnError)
	var includes, excludes, hls, subs multiFlag
	fs.Var(&includes, "f", "only show records matching `REGEX` (repeatable; all must match)")
	fs.Var(&includes, "filter", "same as -f")
	fs.Var(&excludes, "x", "hide records matching `REGEX` (repeatable)")
	fs.Var(&excludes, "exclude", "same as -x")
	fs.Var(&hls, "hl", "highlight `REGEX[=colour]` (repeatable)")
	fs.Var(&subs, "sub", "rewrite lines with `/REGEX/REPLACEMENT/` (repeatable)")
	cols := fs.String("cols", "", "table view: a regex with groups, or JSON fields like '.level .msg'")
	record := fs.String("record", "auto", "multi-line records: auto, off, or a REGEX matching the first line")
	since := fs.String("since", "", "only records at or after `TIME` (2026-10-25 12:00, 12:00, -15m, ...)")
	until := fs.String("until", "", "only records at or before `TIME`")
	noRotated := fs.Bool("no-rotated", false, "do not load rotated copies (PATH.1, PATH.2.gz, ...)")
	preserve := fs.String("preserve", "256M", "how much of each file's existing content to copy aside so it survives truncation (`SIZE`, or 'all')")
	noSpool := fs.Bool("no-spool", false, "do not copy anything aside (truncated content is lost)")
	spoolDir := fs.String("spool-dir", os.TempDir(), "where to keep copies (deleted on exit)")
	poll := fs.Duration("poll", time.Second, "fallback polling interval when file events are unavailable")
	noFollow := fs.Bool("no-follow", false, "start at the top instead of following the end")
	noWrap := fs.Bool("nowrap", false, "do not wrap long lines")
	noNums := fs.Bool("no-line-numbers", false, "hide line numbers")
	noDefHL := fs.Bool("no-default-hl", false, "do not colour log levels")
	mouse := fs.Bool("mouse", false, "enable mouse wheel (disables terminal text selection)")
	dump := fs.Bool("dump", false, "print the matching lines (rewritten, without colours) and exit")
	statePath := fs.String("state", state.DefaultPath(), "remember display settings and each log's rules in `FILE`")
	noState := fs.Bool("no-state", false, "neither load nor save remembered settings")
	showVersion := fs.Bool("version", false, "print version")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageText)
		fs.PrintDefaults()
	}

	var paths []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		paths = append(paths, rest[0])
		args = rest[1:]
	}
	if *showVersion {
		fmt.Println("logview", version)
		return 0
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "logview: "+format+"\n", a...)
		return 2
	}

	if len(paths) == 0 {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fs.Usage()
			return 2
		}
		paths = []string{"-"}
	}
	paths, err := expandPaths(paths)
	if err != nil {
		return fail("%v", err)
	}

	var cfg query.Config
	for _, p := range includes {
		re, err := query.Compile(p)
		if err != nil {
			return fail("-f %s: %v", p, err)
		}
		cfg.Includes = append(cfg.Includes, re)
	}
	if len(cfg.Includes) > query.MaxIncludes {
		return fail("at most %d -f filters", query.MaxIncludes)
	}
	for _, p := range excludes {
		re, err := query.Compile(p)
		if err != nil {
			return fail("-x %s: %v", p, err)
		}
		cfg.Excludes = append(cfg.Excludes, re)
	}
	for _, s := range subs {
		rw, err := query.ParseSub(s)
		if err != nil {
			return fail("-sub %s: %v", s, err)
		}
		cfg.Rewrites = append(cfg.Rewrites, rw)
	}
	if cfg.Record, err = query.ParseRecordMode(*record); err != nil {
		return fail("-record: %v", err)
	}
	for _, tv := range []struct {
		s   string
		dst *int64
	}{{*since, &cfg.Since}, {*until, &cfg.Until}} {
		if tv.s == "" {
			continue
		}
		t, err := ts.ParseUser(tv.s, time.Now())
		if err != nil {
			return fail("%v", err)
		}
		*tv.dst = t.UnixNano()
	}
	var highlights []query.Highlight
	for i, h := range hls {
		hl, err := query.ParseHighlight(h, i)
		if err != nil {
			return fail("-hl %s: %v", h, err)
		}
		highlights = append(highlights, hl)
	}
	var columns *query.Columns
	if *cols != "" {
		if columns, err = query.ParseColumns(*cols); err != nil {
			return fail("-cols: %v", err)
		}
	}
	pres, err := parseSize(*preserve)
	if err != nil {
		return fail("-preserve: %v", err)
	}

	opts := logsrc.Options{
		Preserve: pres, NoSpool: *noSpool, SpoolDir: *spoolDir, Poll: *poll,
		Rotated: !*noRotated, NoTail: *dump,
	}
	var srcs []*logsrc.Source
	stdinUsed := false
	for _, p := range paths {
		if p == "-" {
			if stdinUsed {
				return fail("stdin given twice")
			}
			stdinUsed = true
			srcs = append(srcs, logsrc.OpenReader("<stdin>", os.Stdin, opts))
			continue
		}
		srcs = append(srcs, logsrc.OpenFile(p, opts))
	}
	var seq logsrc.Seq
	if len(srcs) == 1 {
		seq = srcs[0]
		defer srcs[0].Close()
	} else {
		m := logsrc.NewMerged(srcs)
		seq = m
		defer m.Close()
	}

	if *dump { // scripts get exactly what the flags say: no remembered state
		return dumpLines(seq, &cfg)
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	lineNums, wrap, levelColours := !*noNums, !*noWrap, !*noDefHL
	stPath, stateMsg := *statePath, ""
	if *noState {
		stPath = ""
	}
	key := logKey(paths)
	if stPath != "" {
		st, err := state.Load(stPath)
		if err != nil {
			stateMsg = fmt.Sprintf("settings neither loaded nor saved: %v", err)
			stPath = ""
		} else {
			if !set["no-line-numbers"] {
				lineNums = st.Display.LineNumbers
			}
			if !set["nowrap"] {
				wrap = st.Display.Wrap
			}
			if !set["no-default-hl"] {
				levelColours = st.Display.LevelColours
			}
			ruleFlags := false
			for _, n := range []string{"f", "filter", "x", "exclude", "sub", "hl", "cols", "record"} {
				ruleFlags = ruleFlags || set[n]
			}
			if r := st.Logs[key]; r != nil && key != "" && !ruleFlags {
				var warns []string
				cfg, highlights, columns, warns = restoreRules(r, cfg)
				if len(warns) > 0 {
					stateMsg = "some remembered rules were dropped: " + strings.Join(warns, "; ")
				}
			}
		}
	}
	persist := func(s ui.Settings) error {
		if stPath == "" {
			return nil
		}
		return state.Update(stPath, func(f *state.File) {
			f.Display = state.Display{LineNumbers: s.LineNumbers, Wrap: s.Wrap, LevelColours: s.LevelColours}
			if key != "" {
				f.Logs[key] = &state.Rules{Filters: s.Filters, Excludes: s.Excludes, Subs: s.Subs,
					Highlights: s.Highlights, Cols: s.Cols, Record: s.Record, LastUsed: time.Now()}
			}
		})
	}

	scr, err := newScreen()
	if err != nil {
		return fail("terminal: %v", err)
	}
	if err := scr.Init(); err != nil {
		return fail("terminal: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			scr.Fini()
			panic(r)
		}
		scr.Fini()
	}()
	if *mouse {
		scr.EnableMouse()
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		scr.PostEvent(tcell.NewEventInterrupt(ui.QuitSignal))
	}()
	app := ui.New(scr, seq, ui.Options{
		Config: cfg, Highlights: highlights, Columns: columns,
		DefaultHL: levelColours, Follow: !*noFollow, Wrap: wrap, LineNumbers: lineNums, Mouse: *mouse,
		Message: stateMsg, OnChange: persist,
	})
	// Save once at startup: records rules given as flags and marks the log as recently used.
	if err := persist(app.Settings()); err != nil {
		app.ShowError("saving settings: " + err.Error())
	}
	app.Run()
	return 0
}

// logKey identifies a set of log files in the state file; "" (nothing
// remembered) when reading stdin, whose contents vary from run to run.
func logKey(paths []string) string {
	var abs []string
	for _, p := range paths {
		if p == "-" {
			return ""
		}
		a, err := filepath.Abs(p)
		if err != nil {
			return ""
		}
		abs = append(abs, a)
	}
	sort.Strings(abs)
	return strings.Join(abs, " + ")
}

// restoreRules rebuilds remembered rules. Regexes were saved as compiled,
// so they are recompiled verbatim (no smart-case prefixing again).
func restoreRules(r *state.Rules, cfg query.Config) (query.Config, []query.Highlight, *query.Columns, []string) {
	var warns []string
	compile := func(list []string) []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, p := range list {
			if re, err := regexp.Compile(p); err == nil {
				out = append(out, re)
			} else {
				warns = append(warns, p)
			}
		}
		return out
	}
	cfg.Includes, cfg.Excludes, cfg.Rewrites = compile(r.Filters), compile(r.Excludes), nil
	for _, s := range r.Subs {
		if rw, err := query.ParseSub(s); err == nil {
			cfg.Rewrites = append(cfg.Rewrites, rw)
		} else {
			warns = append(warns, s)
		}
	}
	if m, err := query.ParseRecordMode(r.Record); err == nil {
		cfg.Record = m
	} else {
		warns = append(warns, r.Record)
	}
	var hls []query.Highlight
	for _, s := range r.Highlights {
		if h, err := query.ParseHighlight(s, len(hls)); err == nil {
			hls = append(hls, h)
		} else {
			warns = append(warns, s)
		}
	}
	var cols *query.Columns
	if r.Cols != "" {
		var err error
		if cols, err = query.ParseColumns(r.Cols); err != nil {
			warns = append(warns, r.Cols)
		}
	}
	return cfg, hls, cols, warns
}

// newScreen is replaced by tests with a simulated terminal.
var newScreen = terminalScreen

// terminalScreen falls back to a common terminal type when $TERM is unknown to
// both the built-in terminfo database and the system's infocmp.
func terminalScreen() (tcell.Screen, error) {
	scr, err := tcell.NewScreen()
	if err == nil {
		return scr, nil
	}
	if os.Getenv("TERM") != "xterm-256color" {
		os.Setenv("TERM", "xterm-256color")
		if scr, err2 := tcell.NewScreen(); err2 == nil {
			return scr, nil
		}
	}
	return nil, err
}

func dumpLines(seq logsrc.Seq, cfg *query.Config) int {
	var filter *query.Filter
	if cfg.Filtering() {
		filter = query.NewFilter(seq, cfg)
		defer filter.Stop()
	}
	for !seq.Ready() || (filter != nil && !filter.Done()) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, s := range seq.Sources() {
		if st := s.Status(); st != "" {
			fmt.Fprintf(os.Stderr, "logview: %s: %s\n", s.Path, st)
		}
	}
	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	_, err := ui.WriteLines(w, seq, cfg, filter)
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "logview: %v\n", err)
		return 1
	}
	return 0
}
