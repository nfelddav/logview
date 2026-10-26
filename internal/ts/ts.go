// Package ts recognises the timestamp formats commonly found in log lines.
//
// Recognised (anywhere in the first 48 bytes):
//
//	2026-10-25T12:34:56.789Z, 2026-10-25 12:34:56,789 +02:00, 2026/10/25 12:34:56
//	25/Oct/2026:12:34:56 +0000          (Apache / nginx)
//	Oct 25 12:34:56, Oct  5 12:34:56     (syslog; year inferred)
//
// and, for lines starting with '{', the JSON fields time, timestamp, ts and
// @timestamp holding either a string in one of the above formats or a number
// of seconds / milliseconds / microseconds / nanoseconds since the epoch.
// Timestamps without a zone are interpreted in the local zone.
package ts

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"time"
)

const scanLimit = 48

// Parse returns the first timestamp near the start of b as unix nanoseconds,
// together with the byte offset at which it starts.
func Parse(b []byte) (nanos int64, pos int, ok bool) {
	if len(b) > 0 && b[0] == '{' {
		if t, ok := parseJSON(b); ok {
			return t, 0, true
		}
	}
	lim := len(b)
	if lim > scanLimit {
		lim = scanLimit
	}
	for i := 0; i < lim; i++ {
		c := b[i]
		switch {
		case isDigit(c):
			if i > 0 && isDigit(b[i-1]) {
				continue
			}
			if t, ok := parseISO(b[i:]); ok {
				return t, i, true
			}
			if t, ok := parseApache(b[i:]); ok {
				return t, i, true
			}
		case c >= 'A' && c <= 'Z':
			if i > 0 && isLetter(b[i-1]) {
				continue
			}
			if t, ok := parseSyslog(b[i:]); ok {
				return t, i, true
			}
		}
	}
	return 0, 0, false
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return (c|0x20) >= 'a' && (c|0x20) <= 'z' }

func num(b []byte) int {
	n := 0
	for _, c := range b {
		if !isDigit(c) {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func month(b []byte) time.Month {
	if len(b) < 3 {
		return 0
	}
	switch string([]byte{b[0] | 0x20, b[1] | 0x20, b[2] | 0x20}) {
	case "jan":
		return time.January
	case "feb":
		return time.February
	case "mar":
		return time.March
	case "apr":
		return time.April
	case "may":
		return time.May
	case "jun":
		return time.June
	case "jul":
		return time.July
	case "aug":
		return time.August
	case "sep":
		return time.September
	case "oct":
		return time.October
	case "nov":
		return time.November
	case "dec":
		return time.December
	}
	return 0
}

func valid(mo time.Month, d, h, mi, s int) bool {
	return mo >= 1 && mo <= 12 && d >= 1 && d <= 31 && h >= 0 && h < 24 && mi >= 0 && mi < 60 && s >= 0 && s <= 60
}

// fraction parses ".123" / ",123456" at b[0]; returns nanoseconds and bytes consumed.
func fraction(b []byte) (int, int) {
	if len(b) < 2 || (b[0] != '.' && b[0] != ',') || !isDigit(b[1]) {
		return 0, 0
	}
	ns, j, scale := 0, 1, 100000000
	for j < len(b) && isDigit(b[j]) {
		if scale > 0 {
			ns += int(b[j]-'0') * scale
			scale /= 10
		}
		j++
	}
	return ns, j
}

// zone parses Z, +HH, +HHMM or +HH:MM at b[0].
func zone(b []byte) (*time.Location, bool) {
	if len(b) == 0 {
		return nil, false
	}
	if b[0] == 'Z' {
		return time.UTC, true
	}
	if b[0] != '+' && b[0] != '-' || len(b) < 3 {
		return nil, false
	}
	h := num(b[1:3])
	if h < 0 || h > 14 {
		return nil, false
	}
	m := 0
	rest := b[3:]
	if len(rest) > 0 && rest[0] == ':' {
		rest = rest[1:]
	}
	if len(rest) >= 2 && num(rest[:2]) >= 0 {
		m = num(rest[:2])
	}
	off := h*3600 + m*60
	if b[0] == '-' {
		off = -off
	}
	return time.FixedZone("", off), true
}

func parseISO(b []byte) (int64, bool) {
	if len(b) < 16 {
		return 0, false
	}
	sep := b[4]
	if (sep != '-' && sep != '/') || b[7] != sep || (b[10] != 'T' && b[10] != ' ') || b[13] != ':' {
		return 0, false
	}
	y, mo, d, h, mi := num(b[0:4]), num(b[5:7]), num(b[8:10]), num(b[11:13]), num(b[14:16])
	if y < 1970 || mo < 0 || d < 0 || h < 0 || mi < 0 {
		return 0, false
	}
	s, ns, i := 0, 0, 16
	if len(b) >= 19 && b[16] == ':' {
		if s = num(b[17:19]); s < 0 {
			return 0, false
		}
		i = 19
		f, n := fraction(b[i:])
		ns, i = f, i+n
	}
	if !valid(time.Month(mo), d, h, mi, s) {
		return 0, false
	}
	loc := time.Local
	if i < len(b) {
		if l, ok := zone(b[i:]); ok {
			loc = l
		} else if b[i] == ' ' && i+1 < len(b) && (b[i+1] == '+' || b[i+1] == '-' || b[i+1] == 'Z') {
			if l, ok := zone(b[i+1:]); ok && (b[i+1] == 'Z' || len(b) >= i+4 && isDigit(b[i+2]) && isDigit(b[i+3])) {
				loc = l
			}
		}
	}
	return time.Date(y, time.Month(mo), d, h, mi, s, ns, loc).UnixNano(), true
}

func parseApache(b []byte) (int64, bool) {
	if len(b) < 20 || b[2] != '/' || b[6] != '/' || b[11] != ':' || b[14] != ':' || b[17] != ':' {
		return 0, false
	}
	d, mo, y := num(b[0:2]), month(b[3:6]), num(b[7:11])
	h, mi, s := num(b[12:14]), num(b[15:17]), num(b[18:20])
	if y < 1970 || !valid(mo, d, h, mi, s) {
		return 0, false
	}
	loc := time.Local
	if len(b) > 21 && b[20] == ' ' {
		if l, ok := zone(b[21:]); ok {
			loc = l
		}
	}
	return time.Date(y, mo, d, h, mi, s, 0, loc).UnixNano(), true
}

func parseSyslog(b []byte) (int64, bool) {
	if len(b) < 15 || b[3] != ' ' {
		return 0, false
	}
	mo := month(b[0:3])
	if mo == 0 {
		return 0, false
	}
	var d int
	if b[4] == ' ' {
		d = num(b[5:6])
	} else {
		d = num(b[4:6])
	}
	if b[6] != ' ' || b[9] != ':' || b[12] != ':' {
		return 0, false
	}
	h, mi, s := num(b[7:9]), num(b[10:12]), num(b[13:15])
	if !valid(mo, d, h, mi, s) {
		return 0, false
	}
	ns, _ := fraction(b[15:])
	now := time.Now()
	t := time.Date(now.Year(), mo, d, h, mi, s, ns, time.Local)
	if t.After(now.Add(48 * time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t.UnixNano(), true
}

var jsonKeys = [][]byte{[]byte(`"time"`), []byte(`"timestamp"`), []byte(`"ts"`), []byte(`"@timestamp"`)}

func parseJSON(b []byte) (int64, bool) {
	for _, k := range jsonKeys {
		idx := bytes.Index(b, k)
		if idx < 0 {
			continue
		}
		j := idx + len(k)
		for j < len(b) && b[j] == ' ' {
			j++
		}
		if j >= len(b) || b[j] != ':' {
			continue
		}
		j++
		for j < len(b) && b[j] == ' ' {
			j++
		}
		if j >= len(b) {
			continue
		}
		if b[j] == '"' {
			end := bytes.IndexByte(b[j+1:], '"')
			if end < 0 {
				continue
			}
			v := b[j+1 : j+1+end]
			if t, ok := parseISO(v); ok {
				return t, true
			}
			if t, ok := parseApache(v); ok {
				return t, true
			}
			if t, ok := parseSyslog(v); ok {
				return t, true
			}
			continue
		}
		e := j
		for e < len(b) && (isDigit(b[e]) || b[e] == '.' || b[e] == 'e' || b[e] == 'E' || b[e] == '+') {
			e++
		}
		if n, err := strconv.ParseInt(string(b[j:e]), 10, 64); err == nil {
			if n < 1e8 {
				continue
			}
			switch {
			case n > 1e17:
				return n, true
			case n > 1e14:
				return n * 1e3, true
			case n > 1e11:
				return n * 1e6, true
			default:
				return n * 1e9, true
			}
		}
		v, err := strconv.ParseFloat(string(b[j:e]), 64)
		if err != nil || v < 1e8 {
			continue
		}
		switch {
		case v > 1e17:
			return int64(v), true
		case v > 1e14:
			return int64(v * 1e3), true
		case v > 1e11:
			return int64(v * 1e6), true
		default:
			return int64(v * 1e9), true
		}
	}
	return 0, false
}

// ParseUser parses a time typed by the user. Accepted forms:
//
//	now, -15m, -2h, -1d            relative to now
//	2026-10-25                     midnight, local time
//	12:30, 12:30:15                on the date of ref
//	any format Parse recognises
func ParseUser(s string, ref time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	now := time.Now()
	switch {
	case s == "":
		return time.Time{}, errors.New("empty time")
	case s == "now":
		return now, nil
	case s[0] == '-':
		rest := s[1:]
		if strings.HasSuffix(rest, "d") {
			n, err := strconv.ParseFloat(strings.TrimSuffix(rest, "d"), 64)
			if err != nil {
				return time.Time{}, err
			}
			return now.Add(-time.Duration(n * float64(24*time.Hour))), nil
		}
		d, err := time.ParseDuration(rest)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if ref.IsZero() {
				ref = now
			}
			ref = ref.In(time.Local)
			return time.Date(ref.Year(), ref.Month(), ref.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local), nil
		}
	}
	if n, pos, ok := Parse([]byte(s)); ok && pos == 0 {
		return time.Unix(0, n), nil
	}
	return time.Time{}, errors.New("unrecognised time: " + s)
}
