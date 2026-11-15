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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The Kubernetes read adapter behind argo.app.get (airframe_api.KubeReader, narrowed to what S5a needs): GET only, against
// a hard allowlist of paths, as the gateway's own identity. The bearer token is read from its file on every call (a
// projected service-account token the kubelet rotates; nothing keeps it, nothing logs it, no error carries it), the server
// is verified against the cluster CA read from its file (re-read when it changes), a redirect is never followed (it would
// carry the token to wherever it points), and the answer is read to at most maxKubeBody bytes. RBAC is the outer limit; this
// is the inner one, so a mistake in either alone still reads nothing else.
//
// The airframe.* tools (S5c) read Airframe XRs and Rollouts through it too. Python's reader takes any single-word plural of the
// group and lists across namespaces; Go's takes only the contract's XR plurals and only inside one namespace.

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
	// The whole allowlist (pathAllowed): Applications and Rollouts of one namespace, listed or read by name; the Airframe XR kinds of
	// one namespace (only the plurals of the vendored contract's xrds, listed or read by name); and (A-9 S4, the Agent XR loader)
	// the Agent XRs of one namespace, listed (never read by name, never across namespaces). No path without a namespace.
	argoPath   = regexp.MustCompile(`^/apis/argoproj\.io/v1alpha1/namespaces/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/(?:applications|rollouts)(/[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?)?$`)
	xrKindPath = regexp.MustCompile(`^/apis/catalog\.hangar\.io/v1alpha1/namespaces/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/([a-z0-9]{1,63})(/[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?)?$`)
	// nsPath is one Namespace by name (never a list, never a subresource): the platform labels its namespaces with the app they belong to.
	nsPath       = regexp.MustCompile(`^/api/v1/namespaces/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
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

// xrPlurals are the plurals of the catalog.hangar.io kinds the vendored contract lists: the only XR kinds that are read.
var xrPlurals = func() map[string]bool {
	m := map[string]bool{}
	xrds, _ := airframe.Contract().M["xrds"].(*definitions.OMap)
	if xrds != nil {
		for _, k := range xrds.Keys {
			x, _ := xrds.M[k].(*definitions.OMap)
			if x != nil && x.M["group"] == "catalog.hangar.io" {
				if pl, ok := x.M["plural"].(string); ok {
					m[pl] = true
				}
			}
		}
	}
	return m
}()

// AllowedKubeGETs is the list of requests the Kubernetes reader can make (every one a GET, with at most the query parameters
// labelSelector, limit and continue), for the documentation and its tests.
var AllowedKubeGETs = []string{
	"GET /apis/argoproj.io/v1alpha1/namespaces/{ns}/applications[/{name}]",
	"GET /apis/argoproj.io/v1alpha1/namespaces/{ns}/rollouts[/{name}]",
	"GET /apis/catalog.hangar.io/v1alpha1/namespaces/{ns}/{plural}[/{name}] (plural: one of the contract's XR kinds except agents)",
	"GET /apis/catalog.hangar.io/v1alpha1/namespaces/{ns}/agents (a list only)",
	"GET /api/v1/namespaces/{ns} (one Namespace, for its hangar.io/app label)",
}

func pathAllowed(p string) bool {
	if argoPath.MatchString(p) || nsPath.MatchString(p) {
		return true
	}
	m := xrKindPath.FindStringSubmatch(p)
	if m == nil || !xrPlurals[m[2]] {
		return false
	}
	return m[2] != "agents" || m[3] == ""
}

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
		// Python's message names the query keys too ("...?labelSelector&limit"), sorted.
		keys := make([]string, 0, len(query))
		for _, q := range query {
			keys = append(keys, q.k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			path += "?" + strings.Join(keys, "&")
		}
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

// XRPath is the path of an Airframe XR (name != "") or of a namespace's XRs of one kind.
func XRPath(plural, namespace, name string) string {
	p := "/apis/catalog.hangar.io/v1alpha1/namespaces/" + segEscape(namespace) + "/" + segEscape(plural)
	if name != "" {
		p += "/" + segEscape(name)
	}
	return p
}

// RolloutsPath is the path of a Rollout (name != "") or of a namespace's Rollouts.
func RolloutsPath(namespace, name string) string {
	p := "/apis/argoproj.io/v1alpha1/namespaces/" + segEscape(namespace) + "/rollouts"
	if name != "" {
		p += "/" + segEscape(name)
	}
	return p
}

// GetObject is Get with no query: the object at path, or (nil, false, nil) for a 404.
func (k *KubeReader) GetObject(ctx context.Context, path string) (any, bool, error) {
	return k.Get(ctx, path, nil)
}

// StateReader is a KubeReader as the airframe state tools read it (airframe.Reader): Get by path, List by path and selector.
type StateReader struct{ *KubeReader }

// Get is the object at path, or found false for a 404.
func (r StateReader) Get(ctx context.Context, path string) (any, bool, error) {
	return r.KubeReader.GetObject(ctx, path)
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
