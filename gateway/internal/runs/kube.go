package runs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// InClusterAPI is the API server as a pod reaches it.
	InClusterAPI = "https://kubernetes.default.svc"
	maxBody      = 1 << 20
)

// Collection is the one path every request is under.
var Collection = "/apis/" + APIVersion + "/namespaces/" + Namespace + "/agentruns"

// Allowed is the whole allowlist: POST to the collection, DELETE or GET of one named AgentRun in it (kube_runs.allowed).
func Allowed(method, path string) bool {
	switch method {
	case http.MethodPost:
		return path == Collection
	case http.MethodDelete, http.MethodGet:
		return strings.HasPrefix(path, Collection+"/") && dnsLabel.MatchString(strings.TrimPrefix(path, Collection+"/"))
	}
	return false
}

// Kube is AgentRuns over the Kubernetes API (kube_runs.KubeAgentRuns). It has an HTTP client and connection pool of its
// own, so a slow API server cannot starve another backend, and every request carries the call's context and its own
// timeout. The bearer token is read from its file on every call (the kubelet rotates the projected token; nothing keeps
// it and nothing logs it); the CA is re-read when its file changes.
type Kube struct {
	Server    string
	TokenFile string
	CAFile    string
	Timeout   time.Duration

	mu     sync.Mutex
	client *http.Client
	caKey  string
}

// NewKube is the in-cluster backend: the pod's service account token and CA, a 30 s timeout.
func NewKube() *Kube {
	return &Kube{Server: InClusterAPI, TokenFile: saDir + "/token", CAFile: saDir + "/ca.crt", Timeout: 30 * time.Second}
}

func (k *Kube) http() (*http.Client, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := ""
	var pool *x509.CertPool
	if k.CAFile != "" {
		st, err := os.Stat(k.CAFile)
		if err != nil {
			return nil, err
		}
		key = fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
	}
	if k.client != nil && key == k.caKey {
		return k.client, nil
	}
	if k.CAFile != "" {
		ca, err := os.ReadFile(k.CAFile)
		if err != nil {
			return nil, err
		}
		pool = x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
	}
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	// A redirect would carry the bearer token to wherever it points: never followed.
	k.client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	k.caKey = key
	return k.client, nil
}

func (k *Kube) send(ctx context.Context, method, path string, body map[string]any) (int, []byte, error) {
	if !Allowed(method, path) {
		return 0, nil, &Refused{fmt.Sprintf("Clearance does not send %s %s", method, safe(path))}
	}
	tok, err := os.ReadFile(k.TokenFile)
	if err != nil {
		return 0, nil, &Error{"API server unreachable: no service account token"}
	}
	hc, err := k.http()
	if err != nil {
		return 0, nil, &Error{"API server unreachable: no CA"}
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	t := k.Timeout
	if t <= 0 {
		t = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.Server, "/")+path, rd)
	if err != nil {
		return 0, nil, &Error{"API server unreachable: bad request"}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, &Error{"API server unreachable: " + reason(err)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, b, nil
}

// reason is the class of a transport failure, in the names the Python gateway's record carries (urllib's URLError, and
// TimeoutError for a timeout). It never carries the address or the transport's own message.
func reason(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &ne) && ne.Timeout()) {
		return "TimeoutError"
	}
	return "URLError"
}

// Create posts an AgentRun. It refuses anything but an AgentRun with a DNS-label name in autopilot-runs before any
// request exists. 409 is an error: a run is new.
func (k *Kube) Create(ctx context.Context, manifest map[string]any) error {
	meta, _ := manifest["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if manifest["apiVersion"] != APIVersion || manifest["kind"] != "AgentRun" || meta["namespace"] != Namespace || !dnsLabel.MatchString(name) {
		return &Refused{"only an AgentRun with a DNS-label name in autopilot-runs may be created"}
	}
	status, _, err := k.send(ctx, http.MethodPost, Collection, manifest)
	if err != nil {
		return err
	}
	switch status {
	case 200, 201, 202:
		return nil
	case 409:
		return &Error{fmt.Sprintf("AgentRun %s already exists", name)}
	}
	return &Error{fmt.Sprintf("API server %d creating AgentRun %s", status, name)}
}

// Delete removes an AgentRun. 404 is success: already gone is what a delete wants.
func (k *Kube) Delete(ctx context.Context, name string) error {
	if !dnsLabel.MatchString(name) {
		return &Refused{"an AgentRun name is a DNS label"}
	}
	status, _, err := k.send(ctx, http.MethodDelete, Collection+"/"+name, nil)
	if err != nil {
		return err
	}
	switch status {
	case 200, 202, 404:
		return nil
	}
	return &Error{fmt.Sprintf("API server %d deleting AgentRun %s", status, name)}
}

// Outcome is how a run's workload ended: the AgentRun's status.outcome (Succeeded, Failed, ...), "Gone" when it no
// longer exists, "" while it is still running.
func (k *Kube) Outcome(ctx context.Context, name string) (string, error) {
	if !dnsLabel.MatchString(name) {
		return "", &Refused{"an AgentRun name is a DNS label"}
	}
	status, body, err := k.send(ctx, http.MethodGet, Collection+"/"+name, nil)
	if err != nil {
		return "", err
	}
	if status == 404 {
		return "Gone", nil
	}
	if status != 200 {
		return "", &Error{fmt.Sprintf("API server %d reading AgentRun %s", status, name)}
	}
	var doc struct {
		Status struct {
			Outcome any `json:"outcome"`
		} `json:"status"`
	}
	_ = json.Unmarshal(body, &doc)
	if s, ok := doc.Status.Outcome.(string); ok {
		return s, nil
	}
	return "", nil
}
