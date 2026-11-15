package readapi

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxLimit and DefaultLimit are the list bounds (read_api.MAX_LIMIT, DEFAULT_LIMIT).
	MaxLimit     = 200
	DefaultLimit = 50
	// MaxQuery is the longest query string the API reads (Go only).
	MaxQuery = 4096
	// maxFilter is the longest filter value; a longer one cannot be an id, a key or a name.
	maxFilter = 256
	// maxPending is the most pending human requests one read returns (Go only: Python returns every one).
	maxPending = 1000
	// maxTaskSessions is the most sessions one task's tree reads (Go only).
	maxTaskSessions = 1000
)

// query is a request's query string as Starlette reads it: the LAST value of a repeated name wins, and a name with no value is empty.
type query map[string]string

func parseQuery(raw string) (query, bool) {
	q := query{}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		// a malformed escape: the pairs that do parse are still read, as Starlette (parse_qsl) does
		_ = err
	}
	for k, vs := range vals {
		if len(vs) > 0 {
			q[k] = vs[len(vs)-1]
		}
	}
	return q, true
}

func (q query) get(k string) string { return q[k] }

var intRe = regexp.MustCompile(`^\s*[+-]?[0-9]+\s*$`)

// limitOf is read_api._limit: absent or empty is the default; a number is clamped to 1..MaxLimit; anything else is an error.
func limitOf(q query) (int, bool) {
	s := q.get("limit")
	if s == "" {
		return DefaultLimit, true
	}
	if !intRe.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil { // out of range: as large (or as small) a number as Python's int would hold
		if strings.HasPrefix(strings.TrimSpace(s), "-") {
			return 1, true
		}
		return MaxLimit, true
	}
	if n > MaxLimit {
		n = MaxLimit
	}
	if n < 1 {
		n = 1
	}
	return int(n), true
}

// okFilter says whether a filter value, an id or a name could be one that was stored: valid UTF-8, no control or format character, at most
// maxFilter bytes. A value outside it matches nothing (the empty answer Python gives) and is never sent to the database.
func okFilter(s string) bool {
	if len(s) > maxFilter || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}
