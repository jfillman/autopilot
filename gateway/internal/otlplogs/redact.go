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

// secretShapes are values shaped like a credential, wherever they appear in a line: an Authorization header's value, a JWT, a GitHub
// token, a provider API key, a PEM block, and a long base64/hex run (a key, not a word).
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(authorization|x-glidepath-authorization|api[_-]?key|token|secret|password)\s*[:=]\s*\S+`),
	regexp.MustCompile(`\b[A-Za-z0-9+/_-]{40,}={0,2}`),
}

// Redact removes the literal secrets (a token the process read from a file) and everything shaped like a credential from a line.
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	for _, re := range secretShapes {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}
