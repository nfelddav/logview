package logsrc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sibling is a rotated copy of a log found next to it on disk.
type Sibling struct {
	Path        string
	Compression string // "", "gz" or "bz2"
	suffix      string
	mod         time.Time
}

// FindSiblings returns rotated versions of path, oldest first. It recognises
// the usual logrotate / newsyslog naming schemes:
//
//	app.log.1  app.log.2.gz  app.log-20261024  app.log-20261024.bz2
//	app-20261024.log  app.2026-10-24.log.gz
func FindSiblings(path string) []Sibling {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	pats := []*regexp.Regexp{
		regexp.MustCompile(`^` + regexp.QuoteMeta(base) + `[.\-_](\d[\d\-_T]*)(\.gz|\.bz2)?$`),
	}
	if ext := filepath.Ext(base); ext != "" && ext != base {
		stem := strings.TrimSuffix(base, ext)
		pats = append(pats, regexp.MustCompile(`^`+regexp.QuoteMeta(stem)+`[.\-_](\d[\d\-_T]*)`+regexp.QuoteMeta(ext)+`(\.gz|\.bz2)?$`))
	}
	live, liveErr := os.Stat(path)
	var out []Sibling
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		for _, re := range pats {
			m := re.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			p := filepath.Join(dir, e.Name())
			fi, err := os.Stat(p)
			if err != nil || (liveErr == nil && os.SameFile(fi, live)) {
				break
			}
			out = append(out, Sibling{Path: p, Compression: strings.TrimPrefix(m[2], "."), suffix: m[1], mod: fi.ModTime()})
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.mod.Equal(b.mod) {
			return a.mod.Before(b.mod)
		}
		an, aerr := strconv.Atoi(a.suffix)
		bn, berr := strconv.Atoi(b.suffix)
		if aerr == nil && berr == nil && an < 1000 && bn < 1000 { // .1 is newer than .2
			return an > bn
		}
		return a.suffix < b.suffix
	})
	return out
}
