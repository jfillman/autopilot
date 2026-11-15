package readapi

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// isoRe is the part of ISO 8601 Python's datetime.fromisoformat reads that a person writes: a date, and optionally a time (hours and
// minutes, then seconds, then up to nine fraction digits) and an offset (Z, +hh:mm, +hhmm).
var isoRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:[.,](\d{1,9}))?)?(Z|[+-]\d{2}:?\d{2})?)?$`)

var errNotISO = errors.New("not an ISO 8601 time")

// ParseISO reads a time as the read API's filters do. naive says it carried no offset (Python's datetime is then naive; the database reads
// it in its own time zone, which is UTC for the gateway's, and so does this).
func ParseISO(s string) (t time.Time, naive bool, err error) {
	if len(s) > 64 {
		return t, false, errNotISO
	}
	m := isoRe.FindStringSubmatch(s)
	if m == nil {
		return t, false, errNotISO
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	nanos := 0
	if m[7] != "" {
		f := (m[7] + "000000000")[:9]
		nanos, _ = strconv.Atoi(f)
	}
	loc := time.UTC
	naive = m[8] == ""
	if m[8] != "" && m[8] != "Z" {
		z := strings.ReplaceAll(m[8], ":", "")
		h, _ := strconv.Atoi(z[1:3])
		mm, _ := strconv.Atoi(z[3:5])
		if h > 23 || mm > 59 {
			return t, false, errNotISO
		}
		off := h*3600 + mm*60
		if z[0] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	}
	t = time.Date(n(1), time.Month(n(2)), n(3), n(4), n(5), n(6), nanos, loc)
	// time.Date normalises 2026-02-30; a date that is not one is refused, as Python refuses it.
	if t.Year() != n(1) || int(t.Month()) != n(2) || t.Day() != n(3) || n(4) > 23 || n(5) > 59 || n(6) > 59 {
		return time.Time{}, false, errNotISO
	}
	// Python's datetime holds years 1 to 9999, in UTC too (an offset must not carry the time out of them).
	if y := t.UTC().Year(); n(1) < 1 || y < 1 || y > 9999 {
		return time.Time{}, false, errNotISO
	}
	return t, naive, nil
}

// PyIsoformat is datetime.isoformat() for what ParseISO read: no suffix for a naive time, +00:00 for UTC (a time with another offset is
// written in UTC: the stored times it is compared with are).
func PyIsoformat(t time.Time, naive bool) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += "." + leftPad(strconv.Itoa(us), 6)
	}
	if naive {
		return s
	}
	return s + "+00:00"
}

func leftPad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// cursorRe is a cursor the API itself wrote: the created_at of the last session of a page, as Python writes a datetime.
var cursorRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?(\+00:00|Z)?$`)

// validCursor is whether a session cursor is a time of the form the API writes (it is then compared as the text it is).
func validCursor(s string) bool {
	if len(s) > 40 || !cursorRe.MatchString(s) {
		return false
	}
	_, _, err := ParseISO(s)
	return err == nil
}
