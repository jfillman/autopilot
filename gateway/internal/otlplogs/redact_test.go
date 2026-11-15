package otlplogs

import (
	"strings"
	"testing"
)

// Each shape a credential comes in, on a line of its own: the secret part is gone, and what is around it that is not secret stays.
var shapes = []struct {
	name, line string
	secret     string   // must not survive
	keep       []string // must survive
}{
	{"json password", `{"user":"app","password":"hunter2hunter2"}`, "hunter2hunter2", []string{`"user":"app"`}},
	{"json secret, short", `{"secret": "abc"}`, "abc", nil},
	{"json client_secret", `{"client_id":"id1","client_secret":"zzzyyyxxx"}`, "zzzyyyxxx", []string{`"client_id":"id1"`}},
	{"json token", `{"token":"t0k3n-value"}`, "t0k3n-value", nil},
	{"json api_key", `{"api_key":"k-123"}`, "k-123", nil},
	{"json api-key header name", `{"X-Api-Key":"abc123"}`, "abc123", nil},
	{"json access_key", `{"access_key":"ak1234"}`, "ak1234", nil},
	{"client_secret colon quoted with spaces", `client_secret: "one two three"`, "one two three", nil},
	{"client_secret equals", `client_secret=abcdef`, "abcdef", nil},
	{"password with a semicolon and ampersand", `password=p@ss;w0rd&more`, "w0rd", nil},
	{"single quoted", `token: 'abc def'`, "abc def", nil},
	{"db_password", `db_password=correct-horse`, "correct-horse", nil},
	{"URL userinfo with a password", `connecting to postgres://app:pw1234@db.example:5432/gateway`, "pw1234", []string{"db.example:5432/gateway", "postgres://"}},
	{"URL userinfo, user only", `fetching https://deploy-token-xyz@github.example/o/r`, "deploy-token-xyz", []string{"github.example/o/r"}},
	{"AWS access key id", `key AKIAIOSFODNN7EXAMPLE used`, "AKIAIOSFODNN7EXAMPLE", []string{"used"}},
	{"AWS temporary key id", `ASIAIOSFODNN7EXAMPLE`, "ASIAIOSFODNN7EXAMPLE", nil},
	{"AWS secret key", `secret wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY end`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", []string{"end"}},
	{"AWS secret key alone", `wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`, "bPxRfiCYEXAMPLEKEY", nil},
	{"bearer with a colon", `bearer: abcdef123456`, "abcdef123456", nil},
	{"Bearer scheme", `Authorization: Bearer abc123def456ghi`, "abc123def456ghi", nil},
	{"Token scheme", `sent Token abcdef123456 to it`, "abcdef123456", []string{"to it"}},
	{"Basic scheme", `Authorization: Basic dXNlcjpwYXNzd29yZA==`, "dXNlcjpwYXNzd29yZA", nil},
	{"cookie", `Cookie: session=abc123; theme=dark`, "abc123", nil},
	{"set-cookie", `Set-Cookie: sid=abc123; HttpOnly; Secure`, "abc123", nil},
	{"GitLab token", `glpat-abcdefghijklmnopqrst`, "abcdefghijklmnopqrst", nil},
	{"Slack token", `xoxb-1234567890-abcdefghij`, "1234567890", nil},
	{"Slack token, other kind", `xoxp-1234567890-abcdefghij`, "abcdefghij", nil},
	{"Stripe live key", `key sk_live_abcdefghij1234`, "abcdefghij1234", nil},
	{"Stripe restricted test key", `rk_test_abcdefghij1234`, "abcdefghij1234", nil},
	{"provider key", `sk-ant-api03-abcdefghijklmnopqrstu`, "abcdefghijklmnopqrstu", nil},
	{"GitHub token", `ghp_0123456789abcdefghijklmnopqrstuvwxyz`, "0123456789abcdefghijkl", nil},
	{"JWT", `eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl`, "eyJzdWIiOiJ4In0", nil},
	{"PEM block, short last line", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\nshortlast=\n-----END RSA PRIVATE KEY-----", "shortlast", nil},
	{"PEM block in a line of text", "key follows -----BEGIN PRIVATE KEY-----\nMIIE\nab\n-----END PRIVATE KEY----- and more", "MIIE", []string{"key follows", "and more"}},
	{"PEM block, not closed", "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\nxy", "MHcCAQEE", nil},
	{"a 45-character key-like run", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhI", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", nil},
}

func TestEveryShapeOfCredentialIsRemoved(t *testing.T) {
	if len(shapes) < 30 {
		t.Fatal("the table is the point")
	}
	for _, c := range shapes {
		got := Redact(c.line)
		if strings.Contains(got, c.secret) {
			t.Errorf("%s: %q survives in %q", c.name, c.secret, got)
		}
		if !strings.Contains(got, Redacted) {
			t.Errorf("%s: nothing was redacted: %q", c.name, got)
		}
		for _, k := range c.keep {
			if !strings.Contains(got, k) {
				t.Errorf("%s: %q was removed with the secret: %q", c.name, k, got)
			}
		}
		if again := Redact(got); again != got {
			t.Errorf("%s: not stable: %q -> %q", c.name, got, again)
		}
	}
}

// What is not a credential is left alone.
func TestOrdinaryLinesAreNotRedacted(t *testing.T) {
	for _, s := range []string{"agent=triage-agent session=s-0123 tool=repo.file.get", "tower/backend acted for user:alice", "ns/name denied: R012 over budget",
		"the 39-character run is not a key AbCdEfGhIjKlMnOpQrStUvWxYz0123456789Ab", "listening on :8080", "definitions published pins=3 rejected=1 sha=3fa2",
		"GET /v1/sessions 200 12ms", "postgres://db.example:5432/gateway"} {
		if got := Redact(s); got != s {
			t.Errorf("%q became %q", s, got)
		}
	}
}

// A literal the process knows is removed wherever it is, with no label and no shape.
func TestALiteralSecretIsRemovedAnywhere(t *testing.T) {
	got := Redact("the value was plainword-xyz here, plainword-xyz again", "plainword-xyz")
	if strings.Contains(got, "plainword") {
		t.Fatal(got)
	}
}

// What a rule would take and is not a credential stays: a count under a plural label, and an image's digest. The same shapes in the
// singular, or a run that is not a whole digest, are still removed (no secret shape was loosened to keep these).
func TestCountsAndImageDigestsSurvive(t *testing.T) {
	digest := "b5c9832f0d5d092e616a98e365862eb1e03a15dffe2d545f97c9adfed7f1d965"
	for _, s := range []string{"secrets: 3 loaded", "tokens: 12 issued", "credentials=2 rotated", `"secrets": 4`, "agent runtime pulled ghcr.io/jfillman/autopilot-agent-runtime@sha256:" + digest + " ok",
		"image sha256:" + digest, "sha512:" + digest + digest} {
		if got := Redact(s); got != s {
			t.Errorf("%q became %q", s, got)
		}
	}
	for _, c := range []struct{ line, gone string }{
		{"secret: 3", "3"}, {"password=123", "123"}, {"token=5", "5"}, {"secrets: abc", "abc"}, {"tokens: 12abc", "12abc"}, {`"secrets": "hunter2hunter2"`, "hunter2hunter2"},
		{"sha256:" + digest[:40], digest[:40]}, {"sha256:" + digest + "ab", digest[:50]}, {"sha512:" + digest, digest[:50]}, {"abc:" + digest, digest[:50]},
		{"AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhI", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
	} {
		if got := Redact(c.line); strings.Contains(got, c.gone) || !strings.Contains(got, Redacted) {
			t.Errorf("%q survives as %q", c.line, got)
		}
	}
}
