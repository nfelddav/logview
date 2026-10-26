# logview

A terminal log viewer that follows files through rotation and truncation,
keeps the earlier contents around while it runs, and filters, searches,
rewrites, highlights and pretty-prints them. One static binary, no runtime
dependencies.

```
logview /var/log/app.log                 # follow, like tail -F
logview /var/log/nginx/*.log             # several files, merged by timestamp
kubectl logs -f pod | logview            # stdin
logview -f error -x healthcheck app.log  # start with filters
logview --dump -f 'user=bob' --since -1h app.log > bob.txt   # no UI, for scripts
```

Press `H` inside for all keys and commands.

## Installing

Download the binary for your machine from the
[latest release](https://github.com/nfelddav/logview/releases/latest)
(`uname -m` says which: `x86_64` → amd64, `aarch64` → arm64,
`armv7l` → armhf, `riscv64` → riscv64), then:

```
chmod +x logview-linux-amd64
sudo mv logview-linux-amd64 /usr/local/bin/logview
```

`SHA256SUMS` in the release lets you check the download
(`sha256sum --check --ignore-missing SHA256SUMS`).

With Go installed: `go install github.com/nfelddav/logview@latest`.

## Building and deploying

```
scripts/build.sh            # -> dist/logview-linux-{amd64,arm64,armhf,riscv64}
scp dist/logview-linux-arm64 host:/usr/local/bin/logview
ssh host chmod +x /usr/local/bin/logview
```

The binaries are statically linked (`CGO_ENABLED=0`, checked by
`scripts/checkelf`), so they run on any Linux kernel from the last decade:
no libc, terminfo or other files needed. Unknown `$TERM` values fall back to
`xterm-256color`. Building needs only Go ≥ 1.24.

## What it does

**Following.** Each path is followed by name. File events (inotify) trigger
reads, with polling as a fallback (`--poll`). The path need not exist yet.

**Generations.** When the file is renamed away (logrotate's default), the
old file is drained for a few seconds and kept, and the new file starts a
new generation. When the file is truncated in place (`copytruncate`, or
`> file`), the old generation is kept from a private copy (the *spool*, in
`--spool-dir`, deleted on exit). Everything read after opening is spooled,
plus the last `--preserve` bytes (default 256M, `all` for everything) of
what was already there. Lines that were not preserved collapse into one
"‹N lines not preserved›" row. Generations are separated by a labelled
rule; `[` and `]` jump between them.

To drop an earlier generation you no longer need, press `X` on it (it asks
first) or use `:forget` (this one), `:forget N`, or `:forget old` (all
earlier ones). Its lines disappear and its file handle and spool space are
released; files on disk are never touched. The generation currently being
written cannot be forgotten. Not available when viewing several files merged.

Every read re-verifies the last 64 bytes already read in the same `pread`,
so bytes from a rewritten file can never be mixed into the old generation.
With `copytruncate`, data written between logview's last read and the
truncation is unavoidably lost (it is also missing from logrotate's copy
if the writer is fast); the harness measures this at ~0.1–0.3% for a writer
producing 70k lines/s rotated every 60 ms, and zero for rename rotation.

**Remembered settings.** Line numbers, wrapping and level colours are
remembered globally, and each log's filters, excludes, substitutions,
highlights, columns and record mode are remembered per log (by absolute
path, or the set of paths when merging), in `~/.logview.json`
(`--state FILE` or `$LOGVIEW_STATE` to move it, `--no-state` to ignore it).
Changes are saved as you make them. Rule flags on the command line (`-f`,
`-x`, `-sub`, `-hl`, `-cols`, `-record`) replace the remembered rules for
that log. Time filters are not remembered, and neither is anything for
stdin. `--dump` never reads or writes the file, so scripts get exactly what
their flags say. A corrupt state file is reported and left untouched.

**Rotated copies.** `app.log.1`, `app.log.2.gz`, `app.log-20261024`,
`app-20261024.log.bz2`, ... next to the file are loaded as older
generations (oldest first; disable with `--no-rotated`).

**Multi-line records.** Filters work on records, not lines: a stack trace
stays with the line that logged it. In `auto` mode a line starting with a
timestamp starts a record; in logs without timestamps, indented lines
continue the previous one. `--record off|auto|REGEX` or `r` to toggle.

**Regexes** (RE2 syntax; all-lower-case patterns ignore case):

| | key | command | flag |
|---|---|---|---|
| filter (ANDed) | `f` | `:filter RE` | `-f RE` |
| hide | `x` | `:exclude RE` | `-x RE` |
| search | `/` `?` `n` `N` | | |
| highlight | `h` | `:hl RE [colour]` | `-hl RE=colour` |
| rewrite | `s` | `:sub /RE/REPL/` | `-sub /RE/REPL/` |
| columns | `c` | `:cols (?P<lvl>\w+) (.*)` or `:cols .level .msg` | `-cols` |

Rewrites apply before filtering and searching, so you filter what you see.
`u` removes the last rule, `C` clears all, `:w FILE` saves the visible lines.

**Time.** Timestamps are recognised in ISO 8601 / RFC 3339, Go, Python,
syslog, Apache/nginx formats and JSON `time`/`ts`/`timestamp` fields.
`t` / `:time 12:30` jumps; `:since` / `:until` (or `--since`/`--until`)
filter. Times may be `2026-10-25 12:00`, `12:00` (date of the current
line), `-15m`, `-2h`, `-1d`.

**ANSI and JSON.** Colour escape sequences are rendered; every other escape
or control sequence is removed, so a log cannot control your terminal. A
JSON object or array at the end of a line is syntax-coloured; `Enter`
pretty-prints the current line's, `J` all of them.

**Scale.** The index stores one offset per 128 lines (~0.25 bytes per
line), filters keep one bit per line, and filtering runs on all cores. On a
16-core machine with the file in page cache: indexing ~3 GB/s, filtering
~1.8 GB/s, search ~2.5 GB/s (see `TestScale`).

## Testing

```
go test ./...                                   # unit, UI (simulated terminal), CLI end-to-end, rotation chaos
go test ./internal/logsrc -run Chaos -chaos 60s # longer rotation chaos run
LOGVIEW_SCALE_GB=20 go test ./internal/query -run Scale -v -timeout 1h
```
