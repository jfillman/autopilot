package backends

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Agent XR list (A-9 S4, the definition loader's read): read-only, one namespace per list, nothing else reachable, the token read on
// every call, a 404 an error and not an empty list, an https server.

func TestListAgentsIsAReadOfOneNamespacesAgentXRsAndNothingElse(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var auth []string
	srv := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/gone/") {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
			return
		}
		if r.URL.Query().Get("continue") == "" {
			_, _ = io.WriteString(w, `{"metadata":{"continue":"c2"},"items":[{"metadata":{"name":"a","namespace":"ns1"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{},"items":[{"metadata":{"name":"b","namespace":"ns1"}}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	tok := writeFile(t, dir, "tok", "first-token")
	k := &KubeReader{Server: srv.URL, TokenFile: tok, CAFile: writeCA(t, srv), Timeout: time.Second}
	items, err := k.ListAgents(context.Background(), "ns1")
	if err != nil || len(items) != 2 {
		t.Fatalf("%d items, %v", len(items), err)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET /apis/catalog.hangar.io/v1alpha1/namespaces/ns1/agents?") {
			t.Errorf("a request that is not a list of the namespace's Agent XRs: %s", s)
		}
	}
	// the token is read on every call: a rotated one is the one sent
	writeFile(t, dir, "tok", "second-token")
	if _, err := k.ListAgents(context.Background(), "ns1"); err != nil {
		t.Fatal(err)
	}
	if last := auth[len(auth)-1]; last != "Bearer second-token" {
		t.Fatalf("the token was not read again: %s", last)
	}
	// a list that is not served is an error: "no such kind" is not "no definitions"
	var ae *AirframeError
	if _, err := k.ListAgents(context.Background(), "gone"); !errors.As(err, &ae) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a 404 must be an error: %v", err)
	}
	// while the generic list still reads a missing one as empty (the Argo reads rely on it)
	if got, err := k.List(context.Background(), AgentsPath("gone"), ""); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestOnlyTheListOfOneNamespacesAgentsIsReachable(t *testing.T) {
	for _, p := range []string{
		"/apis/catalog.hangar.io/v1alpha1/agents",                       // cluster-wide
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/agents/x",        // by name
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/agents/",         //
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/agents/x/status", //
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/xr/x",            // another kind
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/secrets",
		"/apis/catalog.hangar.io/v1alpha1/namespaces/../agents",
		"/apis/catalog.hangar.io/v1alpha1/namespaces/A/agents",
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/agents?watch=true",
		"/apis/catalog.hangar.io/v1/namespaces/a/agents",
		"/api/v1/namespaces/a/secrets",
	} {
		if pathAllowed(p) {
			t.Errorf("%s is reachable", p)
		}
	}
	if !pathAllowed(AgentsPath("app-gate-cicd")) {
		t.Fatal("the list of a namespace's Agent XRs must be")
	}
	k := &KubeReader{Server: "https://k.invalid", TokenFile: "/nonexistent"}
	for _, ns := range []string{"", "A", "a/b", "../x", "a b", strings.Repeat("a", 64), "*"} {
		if _, err := k.ListAgents(context.Background(), ns); err == nil || !strings.Contains(err.Error(), "does not read") {
			t.Errorf("namespace %q was not refused before a request: %v", ns, err)
		}
	}
	// verbs: the reader has no method that is not a GET (the allowlist test in security_test.go covers the transport)
	if err := (&KubeReader{Server: "http://k.invalid"}).Validate(); err == nil || !strings.Contains(err.Error(), "must be an https URL") {
		t.Fatalf("a plain http API server must be refused: %v", err)
	}
	if err := (&KubeReader{Server: "http://k.invalid", AllowHTTP: true}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := NewKubeReader().Validate(); err != nil {
		t.Fatal(err)
	}
}
