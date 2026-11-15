package backends

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The Kubernetes read adapter behind argo.app.get (airframe_api.KubeReader, narrowed to what S5a needs): GET only, against
// a hard allowlist of paths, as the gateway's own identity. The bearer token is read from its file on every call (a
// projected service-account token the kubelet rotates; nothing keeps it, nothing logs it, no error carries it), the server
// is verified against the cluster CA read from its file (re-read when it changes), a redirect is never followed (it would
// carry the token to wherever it points), and the answer is read to at most maxKubeBody bytes. RBAC is the outer limit; this
// is the inner one, so a mistake in either alone still reads nothing else.
//
// Python's reader allows Airframe XR kinds and Rollouts too, for the airframe.* tools (S5c). Go's allows Application
// objects only, in one namespace each, as a get by name or a list: the one resource kind argo.app.get reads.

const (
	// InClusterAPI is the API server as a pod reaches it; the service account's token and CA are where the kubelet puts them.
	InClusterAPI = "https://kubernetes.default.svc"
	saDir        = "/var/run/secrets/kubernetes.io/serviceaccount"
	maxKubeBody  = 16 << 20
	maxPages     = 20
)

var (
	nsPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	segPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	// allowedPath is the whole allowlist: Applications of one namespace, listed or read by name; and (A-9 S4, the Agent XR loader)
	// the Agent XRs of one namespace, listed (never read by name, never across namespaces).
	allowedPath = regexp.MustCompile(`^(?:/apis/argoproj\.io/v1alpha1/namespaces/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/applications(/[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?)?` +
		`|/apis/catalog\.hangar\.io/v1alpha1/namespaces/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/agents)$`)
	allowedQuery = map[string]bool{"labelSelector": true, "limit": true, "continue": true}
)

// AirframeError is the API server failing or being unreachable. Never shown to the agent (dispatch says "backend error").
// Its text is Python's: "API server unreachable: URLError", "API server 500".
type AirframeError struct{ Msg string }

func (e *AirframeError) Error() string    { return e.Msg }
func (e *AirframeError) Kind() string     { return "AirframeError" }
func (e *AirframeError) Evidence() string { return dispatch.ClipEvidence(e.Msg) }

// KubeReader is GET-only, allowlisted reads of one API server.
type KubeReader struct {
	Server    string
	TokenFile string
	CAFile    string
	Timeout   time.Duration
	// AllowHTTP lets Server be plain http (a test); the in-cluster reader is https.
	AllowHTTP bool

	lim    limiter
	mu     sync.Mutex
	hc     *http.Client
	caKey  string
	hcOnce sync.Once
}

// NewKubeReader is the in-cluster reader: the pod's token and CA, a 30 s timeout.
func NewKubeReader() *KubeReader {
	return &KubeReader{Server: InClusterAPI, TokenFile: saDir + "/token", CAFile: saDir + "/ca.crt", Timeout: 30 * time.Second}
}

func (k *KubeReader) limiter() limiter {
	k.hcOnce.Do(func() { k.lim = newLimiter(8) })
	return k.lim
}

// httpClient is the client for the CA file as it is now. A client is built once per CA file content: its pool is its own.
func (k *KubeReader) httpClient() (*http.Client, error) {
	key := ""
	var pool *x509.CertPool
	var pem []byte
	if k.CAFile != "" {
		st, err := os.Stat(k.CAFile)
		if err != nil {
			return nil, fileError(err)
		}
		key = fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.hc != nil && key == k.caKey {
		return k.hc, nil
	}
	if k.CAFile != "" {
		var err error
		if pem, err = os.ReadFile(k.CAFile); err != nil {
			return nil, fileError(err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, &dispatch.KindError{Kind: "SSLError", Err: errors.New("the CA file holds no certificate")}
		}
	}
	if k.hc != nil {
		k.hc.CloseIdleConnections()
	}
	k.hc = client(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, 5*time.Second, 8)
	k.caKey = key
	return k.hc, nil
}

func pathAllowed(p string) bool { return allowedPath.MatchString(p) }

func refusedPath(p string) error {
	return &core.Denied{Rule: "AF-K8S-PATH", Message: "Clearance does not read " + pyStrRepr(p),
		Hint: "The airframe.* tools read Airframe XRs and Argo CD Applications and Rollouts only; Secrets, ConfigMaps, pods, exec, port-forward and proxy are never reachable."}
}

// segEscape is urllib.parse.quote(s, safe=""): what a path segment becomes. Every segment here has already matched a DNS
// pattern, so it is returned unchanged; the escape is the second guard.
func segEscape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// ApplicationsPath is the path of an Application (name != "") or of a namespace's Applications.
func ApplicationsPath(namespace, name string) string {
	p := "/apis/argoproj.io/v1alpha1/namespaces/" + segEscape(namespace) + "/applications"
	if name != "" {
		p += "/" + segEscape(name)
	}
	return p
}

type kv struct{ k, v string }

// Get is one GET: 200 is the object, 404 is (nil, false), 401 and 403 a denial, anything else a hidden failure.
func (k *KubeReader) Get(ctx context.Context, path string, query []kv) (any, bool, error) {
	var parts []string
	for _, q := range query {
		if !allowedQuery[q.k] {
			return nil, false, refusedPath(path + "?" + q.k)
		}
		parts = append(parts, url.QueryEscape(q.k)+"="+url.QueryEscape(q.v))
	}
	if !pathAllowed(path) {
		return nil, false, refusedPath(path)
	}
	full := strings.TrimRight(k.Server, "/") + path
	if len(parts) > 0 {
		full += "?" + strings.Join(parts, "&")
	}
	tok, err := readToken(k.TokenFile)
	if err != nil {
		return nil, false, err
	}
	if strings.ContainsAny(tok, "\r\n") {
		return nil, false, headerError()
	}
	hc, err := k.httpClient()
	if err != nil {
		return nil, false, err
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	status, body, over, err := get(ctx, hc, k.limiter(), timeout, full,
		http.Header{"Authorization": {"Bearer " + tok}, "Accept": {"application/json"}}, maxKubeBody)
	if err != nil {
		return nil, false, &AirframeError{Msg: "API server unreachable: " + reason(err)}
	}
	switch {
	case status == 200:
		if over {
			return nil, false, valueError(errors.New("the answer is larger than the gateway reads"))
		}
		v, err := decodeJSON(body)
		if err != nil {
			return nil, false, valueError(err)
		}
		return v, true, nil
	case status == 404:
		return nil, false, nil
	case status == 401 || status == 403:
		return nil, false, &core.Denied{Rule: "AF-K8S-403", Message: fmt.Sprintf("the API server refused Clearance's identity (%d)", status),
			Hint: "Clearance's ServiceAccount may not read this; ask a human to check its RBAC."}
	}
	return nil, false, &AirframeError{Msg: fmt.Sprintf("API server %d", status)}
}

// Validate refuses a reader that would send its token in the clear: an API server URL must be https unless the reader was told
// otherwise (a test).
func (k *KubeReader) Validate() error {
	return requireHTTPS("the Kubernetes API URL", strings.TrimRight(k.Server, "/"), k.AllowHTTP)
}

// AgentsPath is the path of a namespace's Agent XRs.
func AgentsPath(namespace string) string {
	return "/apis/catalog.hangar.io/v1alpha1/namespaces/" + segEscape(namespace) + "/agents"
}

// ListAgents is the Agent XRs of one namespace (the definition loader's read): a list, GET only, through the same allowlist and
// the same token-per-call rules as every read here. Unlike List, a 404 is an error: "the kind is not served" (a CRD that is gone,
// a wrong path) is not "no definitions", and the loader must not read it as the instruction to unload every pin.
func (k *KubeReader) ListAgents(ctx context.Context, namespace string) ([]map[string]any, error) {
	if !nsPattern.MatchString(namespace) {
		return nil, refusedPath(AgentsPath(namespace))
	}
	return k.list(ctx, AgentsPath(namespace), "", true)
}

// List is every item of a list, following continue (bounded). A list that is not there (404) is an empty one.
func (k *KubeReader) List(ctx context.Context, path, labelSelector string) ([]map[string]any, error) {
	return k.list(ctx, path, labelSelector, false)
}

func (k *KubeReader) list(ctx context.Context, path, labelSelector string, strict bool) ([]map[string]any, error) {
	var items []map[string]any
	cont := ""
	for i := 0; i < maxPages; i++ {
		q := []kv{}
		if labelSelector != "" {
			q = append(q, kv{"labelSelector", labelSelector})
		}
		q = append(q, kv{"limit", "500"})
		if cont != "" {
			q = append(q, kv{"continue", cont})
		}
		page, found, err := k.Get(ctx, path, q)
		if err != nil {
			return nil, err
		}
		if !found {
			if strict {
				return nil, &AirframeError{Msg: "the list is not served (404)"}
			}
			return items, nil
		}
		pm := obj(page)
		if its, ok := pm["items"].([]any); ok {
			for _, it := range its {
				m, ok := it.(map[string]any)
				if !ok {
					return nil, &dispatch.KindError{Kind: "AttributeError", Err: errors.New("an item is not an object")}
				}
				items = append(items, m)
			}
		}
		cont, _ = obj(pm["metadata"])["continue"].(string)
		if cont == "" {
			return items, nil
		}
	}
	return nil, &AirframeError{Msg: fmt.Sprintf("more than %d pages", maxPages)}
}
