package readapi

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func FuzzParseISO(f *testing.F) {
	for _, s := range []string{"2026-11-14", "2026-11-14T10:00:00Z", "2026-11-14T10:00:00.123456+00:00", "2026-11-14 10:00", "2026-02-30", "", "x", "2026-11-14T10:00:00+05:30"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tm, naive, err := ParseISO(s)
		if err != nil {
			return
		}
		if len(s) > 64 {
			t.Fatalf("a time of %d bytes", len(s))
		}
		out := PyIsoformat(tm, naive)
		back, naive2, err := ParseISO(out)
		if err != nil || !back.Equal(tm.UTC().Truncate(1000)) && !back.Equal(tm.UTC()) || naive2 != naive {
			t.Fatalf("%q -> %q does not read back: %v %v", s, out, back, err)
		}
	})
}

func FuzzCursor(f *testing.F) {
	for _, s := range []string{"2026-11-14T10:00:00.123456+00:00", "2026-11-14T10:00:00", "'; DROP TABLE x;--", "", "2026-11-14T10:00:00Z"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !validCursor(s) {
			return
		}
		// What reaches the database as text is made of the characters of a time and nothing else.
		if strings.Trim(s, "0123456789T:.+-Z") != "" || len(s) > 40 {
			t.Fatalf("a cursor with other characters passed: %q", s)
		}
		if _, _, err := ParseISO(s); err != nil {
			t.Fatalf("%q is a cursor and not a time", s)
		}
	})
}

func FuzzLimit(f *testing.F) {
	for _, s := range []string{"", "5", "-1", "0", "999999999999999999999", " 7 ", "+3", "1_0", "x", "٣"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, ok := limitOf(query{"limit": s})
		if ok && (n < 1 || n > MaxLimit) {
			t.Fatalf("%q -> %d", s, n)
		}
	})
}

func FuzzFilter(f *testing.F) {
	for _, s := range []string{"user:alice", "a\x00b", "é", "‮", strings.Repeat("a", 300), ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if okFilter(s) && (len(s) > maxFilter || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n\t")) {
			t.Fatalf("%q", s)
		}
	})
}

// Whatever a caller sends, the API answers with a status it knows, JSON where it is not a route miss, and never a panic.
func FuzzRequest(f *testing.F) {
	for _, s := range []string{"/v1/sessions?limit=3", "/v1/sessions/s-1/result", "/v1/events?since=2026-11-14", "/v1/%00", "/v1/tasks/..%2f..", "/\xff"} {
		f.Add(s, "alice", "user:bob")
	}
	api, _ := New(&API{Access: accessWith(mapDir{"user:alice": {"group:default/a"}}, mapOwn{}), Store: &captureStore{}, Defs: definitions.NewSet(nil)})
	f.Fuzz(func(t *testing.T, target, token, obo string) {
		u, err := url.ParseRequestURI(target)
		if err != nil || !strings.HasPrefix(target, "/") {
			return
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.URL = u
		req.Header.Set("Authorization", "Bearer "+strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return -1
			}
			return r
		}, token))
		if obo != "" && !strings.ContainsAny(obo, "\r\n\x00") {
			req.Header.Set("X-Hangar-On-Behalf-Of", obo)
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		switch rec.Code {
		case 200, 400, 401, 403, 404, 405, 414, 503:
		default:
			t.Fatalf("%q: status %d", target, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%q: cache %q", target, rec.Header().Get("Cache-Control"))
		}
	})
}
