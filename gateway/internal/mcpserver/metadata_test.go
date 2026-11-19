package mcpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The 401 names a metadata URL; the same handler must serve it (the I-40 fault: it advertised a URL it 404'd).
func TestTheAdvertised401MetadataURLIsServed(t *testing.T) {
	srv, _ := setup(t)
	resp, err := http.Post(srv.URL+"/mcp", "application/json", nil)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("%v %v", resp, err)
	}
	hdr := resp.Header.Get("WWW-Authenticate")
	const prefix = `Bearer resource_metadata="http://gw.test`
	if !strings.HasPrefix(hdr, prefix) || !strings.HasSuffix(hdr, `"`) {
		t.Fatalf("header %q", hdr)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(hdr, prefix), `"`)
	if path != "/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("advertised path %q is not the RFC 9728 path", path)
	}
	got := getJSON(t, srv.URL+path)
	if got["resource"] != "http://gw.test/mcp" {
		t.Fatalf("resource %v", got["resource"])
	}
	if as, _ := got["authorization_servers"].([]any); len(as) != 1 || as[0] != "http://dex.test" {
		t.Fatalf("authorization_servers %v", got["authorization_servers"])
	}
}

func TestAllThreeMetadataPathsAnswer(t *testing.T) {
	srv, _ := setup(t)
	for _, p := range []string{
		"/.well-known/oauth-protected-resource/mcp", // RFC 9728, derived from the resource URL's path
		"/.well-known/oauth-protected-resource",     // root
		"/mcp/.well-known/oauth-protected-resource", // what earlier builds advertised
	} {
		got := getJSON(t, srv.URL+p)
		if got["resource"] != "http://gw.test/mcp" {
			t.Errorf("%s: resource %v", p, got["resource"])
		}
		if as, _ := got["authorization_servers"].([]any); len(as) != 1 || as[0] != "http://dex.test" {
			t.Errorf("%s: authorization_servers %v", p, got["authorization_servers"])
		}
	}
}

func TestThe401BodyExplainsAndLeaksNothing(t *testing.T) {
	srv, _ := setup(t)
	var bodies []string
	for _, tok := range []string{"", "bad"} {
		resp, err := (&http.Client{Transport: bearer{tok}}).Post(srv.URL+"/mcp", "application/json", nil)
		if err != nil || resp.StatusCode != 401 {
			t.Fatalf("%q: %v %v", tok, resp, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type %q", ct)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("body %q: %v", b, err)
		}
		if m["error"] != "a bearer token is required" || !strings.Contains(m["hint"], "dynamic client registration") ||
			!strings.Contains(m["hint"], "Signing in as a person") {
			t.Errorf("body %v", m)
		}
		bodies = append(bodies, string(b))
	}
	if bodies[0] != bodies[1] {
		t.Error("a missing and a rejected token must get the same body")
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET %s: %v %v", url, resp, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}
