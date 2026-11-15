package otlplogs

import (
	"regexp"
	"strings"
)

// Redacted replaces what must not leave the process.
const Redacted = "[redacted]"

// sensitiveKey are field names that hold a credential, a prompt or a request body: their value is never sent, whatever it is.
var sensitiveKey = regexp.MustCompile(`(?i)(token|secret|passw|credential|authorization|bearer|cookie|apikey|api[_-]?key|private|signature|` +
	`^body$|\.body$|request[_-]?body|payload|prompt|^input$|\.input$|instructions|content)`)

// SensitiveKey says whether a field of this name is redacted.
func SensitiveKey(k string) bool { return sensitiveKey.MatchString(k) }

// A rule removes one shape from a line. They run in this order: whole blocks and connection strings first (their pieces would not be
// recognised after), then labelled values, then values that are credentials whatever they are labelled.
type rule struct {
	re   *regexp.Regexp
	with string
	// keep, when set, says a match is left alone (s is the whole line, [start, end) the match): the few shapes that are not credentials
	// though a rule would take them.
	keep func(s string, start, end int) bool
}

var rules = []rule{
	// A private key's whole block, the short last line of its body included; one that is not closed runs to the end of the line's text.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted, nil},
	// A URL's userinfo: postgres://app:pw@host, https://user@host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`), "${1}" + Redacted + "@", nil},
	// A cookie header: its whole value (it holds several pairs).
	{regexp.MustCompile(`(?i)\b(?:set-)?cookie\b["']?\s*[:=][^\r\n]*`), Redacted, nil},
	// An authorization scheme and its value, with or without a colon: Bearer x, bearer: x, Basic x, Token x.
	{regexp.MustCompile(`(?i)\b(?:bearer|basic|token)\b[\s:=]+[A-Za-z0-9._~+/=-]{8,}`), Redacted, nil},
	// A labelled value, in text or JSON, quoted or not: password=x, "client_secret": "x y", X-Api-Key: x. The label may be a part of a
	// longer name (client_secret, db_password, github_token); a quoted value is read to its closing quote, an unquoted one to a space, a
	// comma or a brace.
	{regexp.MustCompile(`(?i)(["']?[A-Za-z0-9_.-]*(?:token|secret|passw(?:or)?d|pwd|api[_-]?key|access[_-]?key|authorization|credential|private[_-]?key|signature)[A-Za-z0-9_.-]*["']?\s*[:=]\s*)(?:"(?:[^"\\]|\\.)*"[^\s,}]*|'[^']*'[^\s,}]*|[^\s,}]+)`), "${1}" + Redacted, keepCounts},
	// Credentials by their own shape.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), Redacted, nil},                  // a JWT
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), Redacted, nil},              // GitHub
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`), Redacted, nil},                                                 // GitLab
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), Redacted, nil},                                             // Slack
	{regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{10,}`), Redacted, nil},                              // Stripe
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), Redacted, nil},                                                    // a provider key
	{regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`), Redacted, nil},                // an AWS access key id
	{regexp.MustCompile(`(^|[^A-Za-z0-9/+=])[A-Za-z0-9/+=]{40}($|[^A-Za-z0-9/+=])`), "${1}" + Redacted + "${2}", nil}, // the paired AWS secret key
	{regexp.MustCompile(`\b[A-Za-z0-9+/_-]{40,}={0,2}`), Redacted, keepDigest},                                        // a long key-like run
}

// Redact removes the literal secrets (a token the process read from a file) and everything shaped like a credential from a line.
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	for _, r := range rules {
		if r.keep == nil {
			s = r.re.ReplaceAllString(s, r.with)
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range r.re.FindAllStringSubmatchIndex(s, -1) {
			if r.keep(s, m[0], m[1]) {
				continue
			}
			b.WriteString(s[last:m[0]])
			b.WriteString(string(r.re.ExpandString(nil, r.with, s, m)))
			last = m[1]
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	return s
}

var plural = regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(?:tokens|secrets|credentials|passwords)["']?\s*[:=]\s*[0-9]+$`)

// keepCounts leaves "secrets: 3 loaded" alone: a plural label with a whole number after it is a count of them, where a secret is in the
// singular (`secret: 3`, `password=123` are still removed).
func keepCounts(s string, start, end int) bool { return plural.MatchString(s[start:end]) }

// keepDigest leaves an image digest, sha256:<64 hex digits> (or sha512 and 128): a hash that names an image is not a credential and the
// logs need it. Any other long run, and a run of another length after `sha256:`, is still removed.
func keepDigest(s string, start, end int) bool {
	run := s[start:end]
	for _, d := range []struct {
		prefix string
		n      int
	}{{"sha256:", 64}, {"sha512:", 128}} {
		if len(run) == d.n && strings.HasSuffix(s[:start], d.prefix) && isHex(run) {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
