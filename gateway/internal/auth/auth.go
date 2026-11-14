// Package auth turns a bearer token into a Principal: the workload authenticator (a Kubernetes service-account token,
// checked by the API server's TokenReview), the people's OIDC verifier (oidc.go), the foreign-cluster workload verifier
// and the GitHub Actions verifier, the router that picks one by the token's unverified issuer, a short cache over any
// of them, and a static one for tests. They are engine/auth.py's, with the same behaviour for the same token. Every
// failure is *core.AuthError with no detail about the token.
package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

const (
	saDir    = "/var/run/secrets/kubernetes.io/serviceaccount"
	saPrefix = "system:serviceaccount:"
)

func refuse(msg string) error { return &core.AuthError{Msg: msg} }

// Workload verifies a service-account token with TokenReview. The gateway's identity needs `create` on tokenreviews
// (the system:auth-delegator ClusterRole). The token and CA files are re-read on every call: the kubelet rotates the
// projected token.
type Workload struct {
	// Audience is the primary audience; Audiences the whole set accepted (R-10b: the run pods' `autopilot`, and for a
	// cluster that still runs an older runtime image the old `clearance` one). TokenReview is asked for all of them and
	// answers with the ones the token carries; a token is accepted when it carries any (Python: WorkloadAuthenticator).
	Audience    string
	Audiences   []string
	API         string
	TokenFile   string
	CAFile      string
	TierCeiling int
	HTTP        *http.Client // tests; nil builds one from CAFile
	mu          sync.Mutex
}

// NewWorkload returns the in-cluster authenticator for an audience.
func NewWorkload(audience string, more ...string) *Workload {
	return &Workload{Audience: audience, Audiences: dedupe(append([]string{audience}, more...)), API: "https://kubernetes.default.svc", TokenFile: saDir + "/token", CAFile: saDir + "/ca.crt", TierCeiling: 1}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, a := range in {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// audiences is the set accepted: Audiences, or the one Audience of a Workload built as a literal.
func (w *Workload) audiences() []string {
	if len(w.Audiences) > 0 {
		return w.Audiences
	}
	return []string{w.Audience}
}

func (w *Workload) client() (*http.Client, error) {
	if w.HTTP != nil {
		return w.HTTP, nil
	}
	ca, err := os.ReadFile(w.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, MaxIdleConnsPerHost: 16}}, nil
}

// Authenticate implements dispatch.Authenticator.
func (w *Workload) Authenticate(ctx context.Context, token string) (*dispatch.Principal, error) {
	own, err := os.ReadFile(w.TokenFile)
	if err != nil {
		return nil, refuse("token review failed")
	}
	hc, err := w.client()
	if err != nil {
		return nil, refuse("token review failed")
	}
	body, _ := json.Marshal(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
		"spec": map[string]any{"token": token, "audiences": w.audiences()}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(w.API, "/")+"/apis/authentication.k8s.io/v1/tokenreviews", bytes.NewReader(body))
	if err != nil {
		return nil, refuse("token review failed")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(own)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, refuse("token review failed") // the API server unreachable is a refusal, never a pass
	}
	defer resp.Body.Close()
	var review struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(resp.Body).Decode(&review) != nil {
		return nil, refuse("token review failed")
	}
	st := review.Status
	aud := false
	for _, a := range st.Audiences {
		for _, want := range w.audiences() {
			aud = aud || a == want
		}
	}
	if !st.Authenticated || !aud {
		return nil, refuse("token not authenticated for this audience")
	}
	name := st.User.Username
	if !strings.HasPrefix(name, saPrefix) || strings.Count(name, ":") != 3 {
		return nil, refuse("not a service-account token")
	}
	parts := strings.Split(strings.TrimPrefix(name, saPrefix), ":")
	return &dispatch.Principal{ID: parts[0] + "/" + parts[1], Kind: "workload", TierCeiling: w.TierCeiling}, nil
}

// Static maps tokens to principals (tests, and the conformance control interface's one caller).
type Static map[string]*dispatch.Principal

// Authenticate implements dispatch.Authenticator.
func (s Static) Authenticate(_ context.Context, token string) (*dispatch.Principal, error) {
	if p, ok := s[token]; ok {
		return p, nil
	}
	return nil, refuse("token not accepted")
}

// Cache remembers a verified token for a short time (never past its expiry, never a failure), so a session's calls cost
// one TokenReview a minute rather than one each.
type Cache struct {
	Inner dispatch.Authenticator
	TTL   time.Duration
	Now   func() time.Time
	Max   int // entries kept; default 4096
	mu    sync.Mutex
	m     map[string]cached
}

type cached struct {
	p       *dispatch.Principal
	expires time.Time
}

// Authenticate implements dispatch.Authenticator.
func (c *Cache) Authenticate(ctx context.Context, token string) (*dispatch.Principal, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	c.mu.Lock()
	if e, ok := c.m[key]; ok && now().Before(e.expires) {
		c.mu.Unlock()
		return e.p, nil
	}
	c.mu.Unlock()
	p, err := c.Inner.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	until := now().Add(c.TTL)
	if exp := JWTExpiry(token); !exp.IsZero() && exp.Before(until) {
		until = exp
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]cached{}
	}
	max := c.Max
	if max <= 0 {
		max = 4096
	}
	if _, exists := c.m[key]; !exists && len(c.m) >= max { // as Python: the quarter that expires soonest goes, not everything
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(c.m))
		for k, e := range c.m {
			all = append(all, kv{k, e.expires})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		n := max / 4
		if n < 1 {
			n = 1
		}
		for _, e := range all[:n] {
			delete(c.m, e.k)
		}
	}
	c.m[key] = cached{p, until}
	c.mu.Unlock()
	return p, nil
}

// JWTExpiry reads a token's exp claim WITHOUT verifying it (only to bound a cache or to tell the MCP middleware when
// the token ends; the authenticator has already verified it). Zero when there is none.
func JWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(c.Exp), 0)
}
