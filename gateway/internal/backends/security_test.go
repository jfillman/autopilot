package backends

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The attacks on the three outbound backends: an agent that names the URL, a redirect that names another host, an upstream
// that is slow or endless, a credential that must not leave. Each is a test here, so a change that lets one through fails.

func writeFile(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func isDenied(err error, rule string) bool {
	var d *core.Denied
	return errors.As(err, &d) && d.Rule == rule
}

// -- app.api.get: the URL an agent can make --------------------------------------------------------------------------

var appPathAttacks = []string{
	"http://evil.test/", "https://evil.test/x", "//evil.test/x", "///evil.test", "/\\evil.test", "\\\\evil.test", "@evil.test/",
	"evil.test/x", "api/x", "", "?x=1", "#frag", "/a#frag", "/a?x#frag", "/a b", "/a\tb", "/a\nb", "/a\rb", "/a\r\nX-Evil: 1", "/a\x00b",
	"/a/../b", "/..", "/../../etc/passwd", "/.", "/a/./b", "/a/..", "/a/../../b", "/a/%2e%2e/b", "/a/%2E%2e/b", "/a/.%2e/b", "/a/%2e./b",
	"/a%2fb", "/a%2Fb", "/a%5cb", "/a%5Cb", "/a%252e", "/a%25", "/a%00", "/a%0d%0aX-Evil: 1", "/a%0D%0A", "/a%0a", "/a%1f", "/a%7f", "/a%",
	"/a%z", "/a%zz", "/a%2", "/a/..;/b", "/a/.;x/b", "/a/..;x=y", "/a//b", "/a?x=//evil.test", "/a?x=http://evil.test", "/" + strings.Repeat("a", 600),
	"/é", "/a|b", "/a<b", "/a>b", "/a\"b", "/a{b}", "/a^b", "/a`b", "/a[b]",
}

func TestAppAPIPathAttacksAreRefusedBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, base := range []string{srv.URL, srv.URL + "/v2", srv.URL + "/a/b"} {
		apis, err := NewAppAPIs(map[string]string{"svc": base}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range appPathAttacks {
			_, err := apis.Get(context.Background(), "svc", p)
			if !isDenied(err, "READ-API-PATH") {
				t.Errorf("base %s path %q: not refused: %v", base, p, err)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests reached the service for refused paths", hits.Load())
	}
}

func TestAppAPIOnlyTheListedServicesAndOnlyGET(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method+" "+r.Host+" "+r.Header.Get("Authorization")+r.Header.Get("Cookie"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	apis, _ := NewAppAPIs(map[string]string{"flight-api": srv.URL}, time.Second)
	for _, svc := range []any{"other", "", "Flight-API", "flight-api ", "flight-api\n", "../flight-api", "http://evil.test", "flight-api@evil.test", nil, 1} {
		if _, err := apis.Get(context.Background(), svc, "/x"); !isDenied(err, "READ-API-SERVICE") {
			t.Errorf("service %v: %v", svc, err)
		}
	}
	if _, err := apis.Get(context.Background(), "flight-api", "/x"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || !strings.HasPrefix(methods[0], "GET "+strings.TrimPrefix(srv.URL, "http://")+" ") || strings.TrimSpace(strings.SplitN(methods[0], " ", 3)[2]) != "" {
		t.Fatalf("what the service saw: %q (no credential, no cookie, only GET)", methods)
	}
}

func TestAppAPIAnAcceptedPathStaysOnTheConfiguredHostAndBase(t *testing.T) {
	for _, base := range []string{"http://svc.app.svc:8080", "https://svc.app.svc/v2", "http://10.0.0.1/a/b/c"} {
		bu, _ := url.Parse(base)
		for _, p := range []string{"/x", "/a/b", "/a?x=1", "/a@evil.test", "/@evil.test", "/@evil.test:80/x", "/a:80", "/~x", "/a;b", "/a%20b", "/" + strings.Repeat("a", 100), "/a?next=a/b?c"} {
			full, err := BuildAppURL(base, p)
			if err != nil {
				t.Errorf("%s %q: %v", base, p, err)
				continue
			}
			u, err := url.Parse(full)
			if err != nil || u.Host != bu.Host || u.Scheme != bu.Scheme || u.User != nil || u.Fragment != "" || !strings.HasPrefix(u.EscapedPath(), bu.EscapedPath()+"/") {
				t.Errorf("%s %q became %s", base, p, full)
			}
		}
	}
}

func FuzzBuildAppURL(f *testing.F) {
	for _, p := range append(append([]string{}, appPathAttacks...), "/x", "/a?b=c", "/a/b/c", "/~", "/a@b") {
		f.Add(p)
	}
	bases := []string{"http://svc.ns.svc:8080", "https://svc.ns.svc/v2", "http://svc.ns.svc/a/b"}
	f.Fuzz(func(t *testing.T, p string) {
		for _, base := range bases {
			full, err := BuildAppURL(base, p)
			if err != nil {
				if !isDenied(err, "READ-API-PATH") {
					t.Fatalf("%q: an error that is not the refusal: %v", p, err)
				}
				continue
			}
			bu, _ := url.Parse(base)
			u, perr := url.Parse(full)
			if perr != nil || u.Scheme != bu.Scheme || u.Host != bu.Host || u.User != nil || u.Fragment != "" || u.Opaque != "" {
				t.Fatalf("%q on %s became %q: another scheme, host or userinfo", p, base, full)
			}
			ep := u.EscapedPath()
			if !strings.HasPrefix(ep, bu.EscapedPath()+"/") {
				t.Fatalf("%q on %s became %q: left the base path", p, base, full)
			}
			if strings.ContainsAny(full, " \t\r\n\x00\\") || strings.ContainsAny(full, "#") {
				t.Fatalf("%q became %q: a character a request line cannot hold", p, full)
			}
			for _, seg := range strings.Split(ep, "/") {
				if name, _, _ := strings.Cut(seg, ";"); name == ".." || name == "." {
					t.Fatalf("%q became %q: a dot segment", p, full)
				}
				if l := strings.ToLower(seg); strings.Contains(l, "%2e") || strings.Contains(l, "%2f") || strings.Contains(l, "%5c") {
					t.Fatalf("%q became %q: an encoded dot, slash or backslash", p, full)
				}
			}
			if !strings.HasPrefix(full, base+"/") {
				t.Fatalf("%q became %q: not the base and the path", p, full)
			}
		}
	})
}

// -- the catalog's and the Kubernetes reader's URLs -------------------------------------------------------------------

func FuzzCatalogQuery(f *testing.F) {
	for _, q := range []string{"gate", "kind=component", "a&b=c", "x#y", "a b+c%d", "../../x", "http://evil.test", "\r\nHost: evil", "é", "=", "a=b&filter=c", "%00"} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		const base = "http://backstage.test:7007"
		full := base + catalogPath + "?" + catalogQuery(q)
		u, err := url.Parse(full)
		if err != nil || u.Host != "backstage.test:7007" || u.Path != catalogPath || u.Fragment != "" || u.User != nil {
			t.Fatalf("%q became %q", q, full)
		}
		if strings.ContainsAny(u.RawQuery, " \r\n#") {
			t.Fatalf("%q: a raw special character in the query: %q", q, u.RawQuery)
		}
		vals, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		for k := range vals {
			switch k {
			case "limit", "fields", "filter", "fullTextFilterTerm", "fullTextFilterFields":
			default:
				t.Fatalf("%q added the parameter %q", q, k)
			}
		}
		want := pyStrip(q)
		if strings.Contains(q, "=") {
			if got := vals["filter"]; len(got) != 1 || got[0] != want || vals.Has("fullTextFilterTerm") {
				t.Fatalf("%q: filter %q", q, got)
			}
		} else if got := vals["fullTextFilterTerm"]; len(got) != 1 || got[0] != want || vals.Has("filter") {
			t.Fatalf("%q: term %q", q, got)
		}
	})
}

func FuzzApplicationsPath(f *testing.F) {
	for _, s := range []string{"argocd", "flight-api-dev", "a.b", "../x", "a/b", "a%2fb", "", "A", "a\n"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, ns, name string) {
		if !nsPattern.MatchString(ns) || !segPattern.MatchString(name) {
			return // what the callers check before a path is built
		}
		p := ApplicationsPath(ns, name)
		if !pathAllowed(p) || strings.Count(p, "/") != 7 || strings.ContainsAny(p, "?#%\\ ") {
			t.Fatalf("%q %q became %q", ns, name, p)
		}
		u, err := url.Parse("https://kubernetes.default.svc" + p)
		if err != nil || u.Host != "kubernetes.default.svc" || u.Path != p {
			t.Fatalf("%q %q became %q", ns, name, p)
		}
	})
}

// -- redirects: never followed, and a credential never goes to another host -------------------------------------------

func TestARedirectIsNeverFollowedAndNoCredentialGoesToItsTarget(t *testing.T) {
	var other atomic.Int32
	var otherAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		otherAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"items":[],"stolen":true}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	}))
	defer redir.Close()
	dir := t.TempDir()
	tok := writeFile(t, dir, "tok", "secret-token\n")

	cat, _ := NewCatalog(redir.URL, tok, time.Second)
	_, err := cat.Read(context.Background(), "x")
	var re *ReadError
	if !errors.As(err, &re) || re.Msg != "catalog 302" {
		t.Fatalf("catalog: %v", err)
	}

	apis, _ := NewAppAPIs(map[string]string{"svc": redir.URL}, time.Second)
	got, err := apis.Get(context.Background(), "svc", "/x")
	if err != nil || got.(map[string]any)["status"] != 302 {
		t.Fatalf("app api: %v %v", got, err)
	}

	tls := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer tls.Close()
	kube := &KubeReader{Server: tls.URL, TokenFile: tok, CAFile: writeCA(t, tls), Timeout: time.Second}
	_, _, err = kube.Get(context.Background(), ApplicationsPath("argocd", "x"), nil)
	var ae *AirframeError
	if !errors.As(err, &ae) || ae.Msg != "API server 307" {
		t.Fatalf("kube: %v", err)
	}
	if other.Load() != 0 {
		t.Fatalf("a redirect target was requested %d times (it saw Authorization %q)", other.Load(), otherAuth.Load())
	}
}

// -- credentials: re-read on every call, never in an error, only to the configured host -------------------------------

func TestTheTokenIsReReadOnEveryCallAndNeverInAnError(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.HasSuffix(r.Header.Get("Authorization"), "two") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("body mentions Bearer token-two and secret-token-two"))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	tok := writeFile(t, dir, "tok", "token-one\n")
	cat, _ := NewCatalog(srv.URL, tok, time.Second)
	if _, err := cat.Read(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "tok", "token-two")
	_, err := cat.Read(context.Background(), "x")
	if err == nil {
		t.Fatal("want the 500")
	}
	if got := fmt.Sprint(seen); got != "[Bearer token-one Bearer token-two]" {
		t.Fatalf("tokens sent: %s", got)
	}
	for _, s := range []string{err.Error(), dispatch.ErrorResult(err), dispatch.BackendError(err)} {
		if strings.Contains(s, "token-two") || strings.Contains(s, "secret") || strings.Contains(s, "127.0.0.1") {
			t.Fatalf("a credential, a body or an address in %q", s)
		}
	}
	// a token that cannot be sent as a header is not sent
	writeFile(t, dir, "tok", "a\nb")
	if _, err := cat.Read(context.Background(), "x"); dispatch.ErrorKind(err) != "ValueError" {
		t.Fatalf("%v", err)
	}
	// no token file: the file error's class, no path
	if _, err := (&Catalog{base: srv.URL, TokenFile: filepath.Join(dir, "nope"), Timeout: time.Second, hc: client(nil, time.Second, 1), lim: newLimiter(1)}).Read(context.Background(), "x"); dispatch.ErrorKind(err) != "FileNotFoundError" || strings.Contains(dispatch.ErrorResult(err), dir) {
		t.Fatalf("%v", err)
	}
}

// -- size and time -----------------------------------------------------------------------------------------------------

func endless(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	chunk := []byte(strings.Repeat("x", 64<<10))
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
}

func TestAnEndlessAnswerIsCutAtItsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(endless))
	defer srv.Close()
	start := time.Now()
	apis, _ := NewAppAPIs(map[string]string{"svc": srv.URL}, 5*time.Second)
	got, err := apis.Get(context.Background(), "svc", "/x")
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["truncated"] != true || len(m["text"].(string)) != MaxAPIBody {
		t.Fatalf("truncated=%v len=%d", m["truncated"], len(fmt.Sprint(m["text"])))
	}
	dir := t.TempDir()
	tok := writeFile(t, dir, "tok", "t")
	cat, _ := NewCatalog(srv.URL, tok, 5*time.Second)
	if _, err := cat.Read(context.Background(), "x"); dispatch.ErrorKind(err) != "JSONDecodeError" {
		t.Fatalf("catalog: %v", err)
	}
	// a 400 with an endless body answers with its first 500 characters
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); endless(w, r) }))
	defer bad.Close()
	cat, _ = NewCatalog(bad.URL, tok, 5*time.Second)
	out, err := cat.Read(context.Background(), "kind=")
	if err != nil || len(out.(map[string]any)["message"].(string)) != MaxMessage+3 {
		t.Fatalf("%v %v", out, err)
	}
	tls := tlsServer(http.HandlerFunc(endless))
	defer tls.Close()
	kube := &KubeReader{Server: tls.URL, TokenFile: tok, CAFile: writeCA(t, tls), Timeout: 10 * time.Second}
	if _, _, err := kube.Get(context.Background(), ApplicationsPath("argocd", "x"), nil); dispatch.ErrorKind(err) != "JSONDecodeError" {
		t.Fatalf("kube: %v", err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("the caps took %s", time.Since(start))
	}
}

func TestADeepAnswerIsRefusedBeforeItIsDecoded(t *testing.T) {
	deep := strings.Repeat("[", 200000) + strings.Repeat("]", 200000)
	if _, err := decodeJSON([]byte(deep)); err == nil {
		t.Fatal("a 200000-deep document decoded")
	}
	if _, err := decodeJSON([]byte(strings.Repeat("[", 200) + strings.Repeat("]", 200))); err != nil {
		t.Fatalf("a 200-deep document: %v", err)
	}
	// brackets in strings do not count
	if _, err := decodeJSON([]byte(`{"a":"` + strings.Repeat("[", 1000) + `\"[[["}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestASlowUpstreamTimesOutEverywhere(t *testing.T) {
	block := make(chan struct{})
	slowHeaders := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	slowBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(`{"items":[`))
		w.(http.Flusher).Flush()
		<-block
	}))
	slowTLS := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer func() { close(block); slowHeaders.Close(); slowBody.Close(); slowTLS.Close() }()
	dir := t.TempDir()
	tok := writeFile(t, dir, "tok", "t")
	for _, u := range []string{slowHeaders.URL, slowBody.URL} {
		start := time.Now()
		cat, _ := NewCatalog(u, tok, 150*time.Millisecond)
		_, err := cat.Read(context.Background(), "x")
		var re *ReadError
		if !errors.As(err, &re) || re.Msg != "catalog unreachable: TimeoutError" || time.Since(start) > 2*time.Second {
			t.Fatalf("catalog %s: %v after %s", u, err, time.Since(start))
		}
		apis, _ := NewAppAPIs(map[string]string{"svc": u}, 150*time.Millisecond)
		_, err = apis.Get(context.Background(), "svc", "/x")
		if !errors.As(err, &re) || re.Msg != "svc API unreachable: TimeoutError" {
			t.Fatalf("app api %s: %v", u, err)
		}
	}
	kube := &KubeReader{Server: slowTLS.URL, TokenFile: tok, CAFile: writeCA(t, slowTLS), Timeout: 150 * time.Millisecond}
	_, _, err := kube.Get(context.Background(), ApplicationsPath("argocd", "x"), nil)
	var ae *AirframeError
	if !errors.As(err, &ae) || ae.Msg != "API server unreachable: TimeoutError" {
		t.Fatalf("kube: %v", err)
	}
	// the call's own deadline is the shorter one
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cat, _ := NewCatalog(slowHeaders.URL, tok, 30*time.Second)
	start := time.Now()
	if _, err := cat.Read(ctx, "x"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

func TestASlowServiceHoldsAtMostItsShareOfTheGatewaysCalls(t *testing.T) {
	var in, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := in.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		in.Add(-1)
	}))
	defer srv.Close()
	apis, _ := NewAppAPIs(map[string]string{"svc": srv.URL, "other": srv.URL}, 400*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = apis.Get(context.Background(), "svc", "/x")
		}()
	}
	time.Sleep(200 * time.Millisecond)
	if p := peak.Load(); p > 8 {
		t.Fatalf("%d requests in flight to one service", p)
	}
	wg.Wait()
	close(release)
}

// -- the Kubernetes reader: allowlist, TLS ------------------------------------------------------------------------------

func TestKubeReaderReadsApplicationsOnly(t *testing.T) {
	var hits atomic.Int32
	srv := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	dir := t.TempDir()
	k := &KubeReader{Server: srv.URL, TokenFile: writeFile(t, dir, "tok", "t"), CAFile: writeCA(t, srv), Timeout: time.Second}
	for _, p := range []string{
		"/api/v1/namespaces/argocd/secrets", "/api/v1/namespaces/argocd/secrets/x", "/api/v1/namespaces/argocd/configmaps/x",
		"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/x/status", "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/x/logs",
		"/apis/argoproj.io/v1alpha1/namespaces/argocd/applicationsets/x", "/apis/argoproj.io/v1alpha1/namespaces/argocd/rollouts/x",
		"/apis/argoproj.io/v1alpha1/applications", "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/",
		"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/../secrets", "/apis/argoproj.io/v1alpha1/namespaces/../applications/x",
		"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/a%2fb", "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/a?x=1",
		"/apis/argoproj.io/v1alpha1/namespaces/Argo CD/applications/x", "/apis/catalog.hangar.io/v1alpha1/namespaces/a/xr/x", "/apis/rbac.authorization.k8s.io/v1/clusterroles",
		"/api/v1/namespaces/argocd/pods/x/exec", "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/x/", "//apis/argoproj.io/v1alpha1/namespaces/argocd/applications/x",
		"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/" + strings.Repeat("a", 254),
	} {
		if _, _, err := k.Get(context.Background(), p, nil); !isDenied(err, "AF-K8S-PATH") {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, q := range []string{"watch", "fieldSelector", "resourceVersion", "limit=1&watch", "labelSelector=x&watch"} {
		if _, _, err := k.Get(context.Background(), ApplicationsPath("argocd", ""), []kv{{q, "true"}}); !isDenied(err, "AF-K8S-PATH") {
			t.Errorf("query %q: %v", q, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests for refused paths", hits.Load())
	}
	if _, _, err := k.Get(context.Background(), ApplicationsPath("argocd", "ok"), nil); err != nil && !isDenied(err, "AF-K8S-403") {
		var ae *AirframeError
		if !errors.As(err, &ae) {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("%d", hits.Load())
	}
}

func TestKubeReaderVerifiesTheServerAgainstTheClusterCA(t *testing.T) {
	srv := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	other := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()
	dir := t.TempDir()
	tok := writeFile(t, dir, "tok", "t")
	ca := writeFile(t, dir, "ca.crt", mustRead(t, writeCA(t, other))) // the wrong CA
	k := &KubeReader{Server: srv.URL, TokenFile: tok, CAFile: ca, Timeout: time.Second}
	_, _, err := k.Get(context.Background(), ApplicationsPath("argocd", "x"), nil)
	var ae *AirframeError
	if !errors.As(err, &ae) || ae.Msg != "API server unreachable: URLError" {
		t.Fatalf("a server the CA does not vouch for was trusted: %v", err)
	}
	// the CA file is re-read: with the right CA the same reader connects
	writeFile(t, dir, "ca.crt", mustRead(t, writeCA(t, srv))+" ")
	if _, found, err := k.Get(context.Background(), ApplicationsPath("argocd", "x"), nil); err != nil || !found {
		t.Fatalf("after the CA was replaced: %v %v", found, err)
	}
	// a CA file with no certificate in it
	writeFile(t, dir, "ca.crt", "not a certificate")
	if _, _, err := k.Get(context.Background(), ApplicationsPath("argocd", "x"), nil); dispatch.ErrorKind(err) != "SSLError" {
		t.Fatalf("%v", err)
	}
	// no CA configured: the system roots, which do not vouch for a test server
	k2 := &KubeReader{Server: srv.URL, TokenFile: tok, Timeout: time.Second}
	if _, _, err := k2.Get(context.Background(), ApplicationsPath("argocd", "x"), nil); !errors.As(err, &ae) {
		t.Fatalf("%v", err)
	}
	// and the client never allows TLS below 1.2 or skips verification
	c, err := k.httpClient()
	_ = c
	_ = err
	var cfg *tls.Config
	if tr, ok := k.hc.Transport.(*http.Transport); ok {
		cfg = tr.TLSClientConfig
	}
	if cfg == nil || cfg.InsecureSkipVerify || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("%+v", cfg)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestKubeAnswersCarryNoSecretsFromTheApplication(t *testing.T) {
	srv := tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"metadata":{"name":"x","namespace":"argocd","annotations":{"a":"ANNOTATION-SECRET"},"managedFields":[{"v":"MANAGED-SECRET"}]},
"spec":{"project":"p","source":{"repoURL":"r","path":"p","targetRevision":"main","helm":{"valuesObject":{"password":"HELM-SECRET"},"parameters":[{"value":"PARAM-SECRET"}]},
"plugin":{"env":[{"name":"T","value":"PLUGIN-SECRET"}]},"directory":{"jsonnet":{"extVars":[{"value":"JSONNET-SECRET"}]}}},
"destination":{"server":"s","name":"n","namespace":"ns","extra":"DEST-SECRET"},"info":[{"value":"INFO-SECRET"}]},
"status":{"sync":{"status":"Synced","revision":"r","comparedTo":{"source":{"helm":"CMP-SECRET"}}},"health":{"status":"Healthy"},
"operationState":{"phase":"Failed","message":"m","operation":{"sync":{"source":{"helm":"OP-SECRET"}}},"syncResult":{"x":"SYNC-SECRET"}},
"summary":{"images":["IMAGE-SECRET"]},"history":[{"source":{"helm":"HIST-SECRET"}}],"resources":[{"kind":"K","name":"n","status":"OutOfSync","extra":"RES-SECRET"}]}}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	k := &KubeReader{Server: srv.URL, TokenFile: writeFile(t, dir, "tok", "t"), CAFile: writeCA(t, srv), Timeout: time.Second}
	out, err := (&Argo{Kube: k, Namespaces: []string{"argocd"}}).Get(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if s := short(out); strings.Contains(s, "SECRET") || strings.Contains(s, "valuesObject") {
		t.Fatalf("a field Python does not return: %s", s)
	}
}

// -- configuration -----------------------------------------------------------------------------------------------------

func TestConfigurationIsCheckedBeforeAnythingRuns(t *testing.T) {
	for _, u := range []string{"", "ftp://h", "file:///x", "http://", "http:///x", "http://u:p@h", "http://h?x=1", "http://h?", "http://h#f", "http://h/ x", "h:80", "http://h\\x", "//h"} {
		if _, err := NewCatalog(u, "t", 0); err == nil {
			t.Errorf("BACKSTAGE_URL %q was accepted", u)
		}
		if _, err := NewAppAPIs(map[string]string{"svc": u}, 0); err == nil {
			t.Errorf("an app API base %q was accepted", u)
		}
	}
	for _, u := range []string{"http://backstage.backstage.svc:7007", "https://tower.example/", "http://h/prefix/", "http://[::1]:7007"} {
		if _, err := NewCatalog(u, "t", 0); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for k := range map[string]string{"": "", "A": "", "a_b": "", "-a": "", "a-": "", strings.Repeat("a", 64): "", "a.b": "", "a b": ""} {
		if _, err := NewAppAPIs(map[string]string{k: "http://h"}, 0); err == nil {
			t.Errorf("service name %q was accepted", k)
		}
	}
	for _, raw := range []string{"[]", "x", `{"a":1}`, `{"a":null}`, `"s"`, `{"a":["b"]}`} {
		if m, err := ParseAppAPIServices(raw); err == nil {
			t.Errorf("APP_API_SERVICES %s was accepted: %v", raw, m)
		}
	}
	if m, err := ParseAppAPIServices(" \n"); err != nil || m != nil {
		t.Errorf("%v %v", m, err)
	}
	if m, err := ParseAppAPIServices(`{"a":"http://h"}`); err != nil || m["a"] != "http://h" {
		t.Errorf("%v %v", m, err)
	}
	for _, raw := range []string{"A", "a/b", "a b", "../x", strings.Repeat("a", 64), "a_b", "a,../x"} {
		if _, err := ParseNamespaces(raw); err == nil {
			t.Errorf("ARGO_APP_NAMESPACES %q was accepted", raw)
		}
	}
	if ns, err := ParseNamespaces(" argocd , argocd-apps ,,"); err != nil || strings.Join(ns, "|") != "argocd|argocd-apps" {
		t.Errorf("%v %v", ns, err)
	}
	if _, err := NewHandlers(Config{ArgoNamespaces: []string{"../x"}}); err == nil {
		t.Error("a namespace that is not a DNS label reached the handlers")
	}
	if h, err := NewHandlers(Config{}); err != nil || len(h) != 0 {
		t.Errorf("an empty configuration has handlers: %v %v", h, err)
	}
}

func TestOnlyTheConfiguredBackendsHaveHandlers(t *testing.T) {
	h, err := NewHandlers(Config{BackstageURL: "http://b", AppAPIServices: map[string]string{"s": "http://s"}, ArgoNamespaces: []string{"argocd"}, Artifacts: &MemoryArtifacts{}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for k := range h {
		got[k] = true
	}
	want := map[string]bool{"catalog.read": true, "app.api.get": true, "argo.app.get": true, "artifact.put": true, "artifact.get": true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%v", got)
	}
}

// tlsServer is an HTTPS test server with a certificate of its own (httptest.NewTLSServer gives every server the same one, so
// a test that needs a server the CA does not vouch for cannot use it).
func tlsServer(h http.Handler) *httptest.Server {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	return srv
}
