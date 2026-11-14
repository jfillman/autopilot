package runs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

func generic(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The manifest is the Python gateway's (manifests.agentrun_manifest), for every bundled in-cluster agent, a child with an
// input and a definition with a sandbox, a framework and sidecars. The cases are generated from the Python code.
func TestManifestEqualsPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/manifests.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Label    string          `json:"label"`
		Doc      json.RawMessage `json:"doc"`
		Session  session.Record  `json:"session"`
		Input    map[string]any  `json:"input"`
		Manifest map[string]any  `json:"manifest"`
		RunName  string          `json:"run_name"`
		Error    *string         `json:"error"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("%d cases", len(cases))
	}
	reg := tools.Default()
	errs, children := 0, 0
	for _, c := range cases {
		d, err := definitions.Parse(c.Doc, reg)
		if err != nil {
			t.Fatalf("%s: %v", c.Label, err)
		}
		s, err := session.FromRecord(c.Session)
		if err != nil {
			t.Fatal(err)
		}
		m, merr := runs.Manifest(s, d, c.Input)
		if c.Error != nil {
			errs++
			if merr == nil {
				t.Errorf("%s: a definition with no image made a manifest", c.Label)
			}
			continue
		}
		if merr != nil {
			t.Errorf("%s: %v", c.Label, merr)
			continue
		}
		if !reflect.DeepEqual(generic(t, m), generic(t, c.Manifest)) {
			t.Errorf("%s differs:\n go     %v\n python %v", c.Label, generic(t, m), generic(t, c.Manifest))
		}
		if runs.RunName(s.ID) != c.RunName {
			t.Errorf("%s: run name %q, python %q", c.Label, runs.RunName(s.ID), c.RunName)
		}
		if s.ParentID != nil {
			children++
		}
	}
	if errs == 0 || children == 0 {
		t.Fatal("the fixture has no error case or no child")
	}
}

func agentSession(t *testing.T, mod func(*session.Record)) (*session.Session, *definitions.Definition) {
	t.Helper()
	d, err := definitions.LoadDir("../../../agents", tools.Default())
	if err != nil {
		t.Fatal(err)
	}
	def := d["researcher"]
	r := session.Record{ID: "s-0000a1", Principal: "user:alice", Agent: "researcher", Limits: def.Limits, CreatedAt: "2026-09-26T12:00:00+00:00",
		ExpiresAt: "2026-09-26T12:30:00+00:00", TaskID: "t-0000a1", Used: map[string]int64{}, Reserved: map[string]int64{}}
	if mod != nil {
		mod(&r)
	}
	s, err := session.FromRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	return s, def
}

// Nothing a caller supplies can reach a manifest's namespace, service account, image or volumes: the object has the fixed
// namespace and no service account or volumes at all, and its image is the definition's. What a caller can influence (the
// task id, the input) is checked or carried as data under spec.input.
func TestManifestRefusesWhatIsNotAValidName(t *testing.T) {
	s, d := agentSession(t, nil)
	m, err := runs.Manifest(s, d, map[string]any{"prompt": "hi", "namespace": "kube-system", "serviceAccountName": "admin", "image": "evil"})
	if err != nil {
		t.Fatal(err)
	}
	meta := m["metadata"].(map[string]any)
	spec := m["spec"].(map[string]any)
	if meta["namespace"] != "autopilot-runs" || spec["image"] != *d.Image {
		t.Fatalf("%v %v", meta["namespace"], spec["image"])
	}
	for _, k := range []string{"serviceAccountName", "volumes", "namespace", "hostNetwork"} {
		if _, ok := spec[k]; ok {
			t.Errorf("spec carries %s", k)
		}
	}
	if in := spec["input"].(map[string]any); in["namespace"] != "kube-system" {
		t.Fatalf("the input is carried as data: %v", in)
	}
	for name, mod := range map[string]func(*session.Record){
		"a task id that is not a label value": func(r *session.Record) { r.TaskID = "t-1\nx: y" },
		"an empty task id":                    func(r *session.Record) { r.TaskID = "" },
		"a long task id":                      func(r *session.Record) { r.TaskID = "t-" + strings.Repeat("a", 70) },
		"a task id with a slash":              func(r *session.Record) { r.TaskID = "a/b" },
		"a session id that is not hex":        func(r *session.Record) { r.ID = "s-NOTHEX" },
		"a session id too short":              func(r *session.Record) { r.ID = "s-ab" },
		"a session id with a path":            func(r *session.Record) { r.ID = "s-../x" },
		"a session id that is long":           func(r *session.Record) { r.ID = "s-" + strings.Repeat("a", 40) },
	} {
		s2, _ := agentSession(t, mod)
		if _, err := runs.Manifest(s2, d, nil); err == nil {
			t.Errorf("%s made a manifest", name)
		} else {
			var rf *runs.Refused
			if !errors.As(err, &rf) {
				t.Errorf("%s: %T", name, err)
			}
		}
	}
}

func TestPrincipalIsOnlyEverARunsOwnIdentity(t *testing.T) {
	for run, want := range map[string]string{"r-0000a1": "agent-r-0000a1/run", "r-" + strings.Repeat("ab", 16): "agent-r-" + strings.Repeat("ab", 16) + "/run",
		"": "", "r-12": "", "r-AB12CD": "", "x": "", "r-0000a1/../x": "", "r-0000a1\n": "", "r-" + strings.Repeat("a", 33): ""} {
		if got := runs.Principal(run); got != want {
			t.Errorf("Principal(%q) = %q, want %q", run, got, want)
		}
	}
}

// ---- the Kubernetes API ----------------------------------------------------------------------------------------------

type apiRig struct {
	srv  *httptest.Server
	kube *runs.Kube
	reqs chan *http.Request
	body chan []byte
	tok  string
	// status answers each request
	status atomic.Int32
	reply  atomic.Value
}

func newAPI(t *testing.T) *apiRig {
	t.Helper()
	r := &apiRig{reqs: make(chan *http.Request, 64), body: make(chan []byte, 64)}
	r.status.Store(201)
	r.reply.Store("{}")
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.reqs <- req
		r.body <- b
		w.WriteHeader(int(r.status.Load()))
		_, _ = w.Write([]byte(r.reply.Load().(string)))
	}))
	t.Cleanup(r.srv.Close)
	r.tok = filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(r.tok, []byte("tok-1\n"), 0o600)
	r.kube = &runs.Kube{Server: r.srv.URL + "/", TokenFile: r.tok, Timeout: 2 * time.Second}
	return r
}

func (r *apiRig) sent() int { return len(r.reqs) }

var manifest = func() map[string]any {
	return map[string]any{"apiVersion": runs.APIVersion, "kind": "AgentRun",
		"metadata": map[string]any{"name": "r-00002a", "namespace": "autopilot-runs"}, "spec": map[string]any{"agent": "coding-agent"}}
}

func TestKubeCreatePostsTheManifestToTheRunNamespaceWithTheCurrentToken(t *testing.T) {
	r := newAPI(t)
	if err := r.kube.Create(context.Background(), manifest()); err != nil {
		t.Fatal(err)
	}
	req := <-r.reqs
	if req.Method != "POST" || req.URL.Path != "/apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns" || req.Header.Get("Authorization") != "Bearer tok-1" {
		t.Fatalf("%s %s %q", req.Method, req.URL.Path, req.Header.Get("Authorization"))
	}
	var got map[string]any
	_ = json.Unmarshal(<-r.body, &got)
	if !reflect.DeepEqual(got, generic(t, manifest())) {
		t.Fatalf("%v", got)
	}
	// the kubelet rotates the projected token: it is read again on every call, never kept
	_ = os.WriteFile(r.tok, []byte("tok-2"), 0o600)
	r.status.Store(200)
	if err := r.kube.Delete(context.Background(), "r-00002a"); err != nil {
		t.Fatal(err)
	}
	if got := (<-r.reqs).Header.Get("Authorization"); got != "Bearer tok-2" {
		t.Fatalf("%q", got)
	}
}

func TestKubeStatuses(t *testing.T) {
	r := newAPI(t)
	ctx := context.Background()
	r.status.Store(409)
	err := r.kube.Create(ctx, manifest())
	var ke *runs.Error
	if !errors.As(err, &ke) || !strings.Contains(err.Error(), "already exists") || ke.Kind() != "KubeRunError" {
		t.Fatalf("%v", err)
	}
	for _, s := range []int32{401, 403, 422, 500} {
		r.status.Store(s)
		if err := r.kube.Create(ctx, manifest()); err == nil {
			t.Errorf("create %d is an error", s)
		}
	}
	for _, s := range []int32{200, 202, 404} {
		r.status.Store(s)
		if err := r.kube.Delete(ctx, "r-00002a"); err != nil {
			t.Errorf("delete %d (404 is already gone): %v", s, err)
		}
	}
	for _, s := range []int32{403, 409, 500} {
		r.status.Store(s)
		if err := r.kube.Delete(ctx, "r-00002a"); err == nil {
			t.Errorf("delete %d is an error", s)
		}
	}
	for status, reply := range map[int32]string{200: `{"status":{"phase":"Running"}}`} {
		r.status.Store(status)
		r.reply.Store(reply)
		if out, err := r.kube.Outcome(ctx, "r-00002a"); err != nil || out != "" {
			t.Errorf("running: %q %v", out, err)
		}
	}
	r.reply.Store(`{"status":{"outcome":"Succeeded"}}`)
	if out, _ := r.kube.Outcome(ctx, "r-00002a"); out != "Succeeded" {
		t.Errorf("%q", out)
	}
	r.status.Store(404)
	if out, _ := r.kube.Outcome(ctx, "r-00002a"); out != "Gone" {
		t.Errorf("%q", out)
	}
	r.status.Store(403)
	if _, err := r.kube.Outcome(ctx, "r-00002a"); err == nil {
		t.Error("a read the API server refused is an error")
	}
}

// A manifest or a name outside what this adapter may send is refused before a request exists.
func TestKubeRefusesBeforeAnyRequest(t *testing.T) {
	r := newAPI(t)
	ctx := context.Background()
	for name, mod := range map[string]func(m map[string]any){
		"a Pod":              func(m map[string]any) { m["kind"] = "Pod" },
		"core v1":            func(m map[string]any) { m["apiVersion"] = "v1" },
		"kube-system":        func(m map[string]any) { m["metadata"].(map[string]any)["namespace"] = "kube-system" },
		"a path as the name": func(m map[string]any) { m["metadata"].(map[string]any)["name"] = "../secrets" },
		"an upper-case name": func(m map[string]any) { m["metadata"].(map[string]any)["name"] = "R-UPPER" },
		"no name":            func(m map[string]any) { delete(m["metadata"].(map[string]any), "name") },
		"a long name":        func(m map[string]any) { m["metadata"].(map[string]any)["name"] = strings.Repeat("x", 64) },
	} {
		m := manifest()
		mod(m)
		err := r.kube.Create(ctx, m)
		var rf *runs.Refused
		if !errors.As(err, &rf) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", "../../namespaces/x", "a/b", "r-1/status", strings.Repeat("x", 64), "R1", "r-1?x=y", "r-1#"} {
		var rf *runs.Refused
		if err := r.kube.Delete(ctx, name); !errors.As(err, &rf) {
			t.Errorf("delete %q: %v", name, err)
		}
		if _, err := r.kube.Outcome(ctx, name); !errors.As(err, &rf) {
			t.Errorf("outcome %q: %v", name, err)
		}
	}
	if r.sent() != 0 {
		t.Fatalf("%d requests reached the API server", r.sent())
	}
}

func TestKubeAllowlistIsExactlyThreeRequests(t *testing.T) {
	c := runs.Collection
	for _, ok := range [][2]string{{"POST", c}, {"DELETE", c + "/r-1"}, {"GET", c + "/r-1"}} {
		if !runs.Allowed(ok[0], ok[1]) {
			t.Errorf("%v", ok)
		}
	}
	for _, no := range [][2]string{{"GET", c}, {"GET", c + "/r-1/status"}, {"PATCH", c + "/r-1"}, {"PUT", c + "/r-1"}, {"DELETE", c},
		{"POST", c + "/r-1"}, {"DELETE", c + "/r-1/status"}, {"POST", "/api/v1/namespaces/autopilot-runs/secrets"},
		{"DELETE", "/apis/catalog.hangar.io/v1alpha1/namespaces/other/agentruns/r-1"}, {"GET", c + "/"}} {
		if runs.Allowed(no[0], no[1]) {
			t.Errorf("%v is allowed", no)
		}
	}
}

// A failing API server or an unreachable one is an error whose text carries a status or a transport class, never the
// token, the address or the response body.
func TestKubeErrorsCarryNoSecrets(t *testing.T) {
	r := newAPI(t)
	r.status.Store(500)
	r.reply.Store(`{"message":"tok-1 is bad","token":"tok-1"}`)
	err := r.kube.Create(context.Background(), manifest())
	if err == nil || strings.Contains(err.Error(), "tok-1") || strings.Contains(err.Error(), "bad") {
		t.Fatalf("%v", err)
	}
	r.srv.Close()
	err = r.kube.Create(context.Background(), manifest())
	if err == nil || strings.Contains(err.Error(), "tok-1") || strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("%v", err)
	}
	var ev interface{ Evidence() string }
	if !errors.As(err, &ev) || ev.Evidence() == "" {
		t.Fatal("no evidence")
	}
	if dispatch.ErrorResult(err) != "error:KubeRunError: "+err.Error() {
		t.Fatalf("%q", dispatch.ErrorResult(err))
	}
	if dispatch.BackendError(err) != "backend error" {
		t.Fatalf("the agent is told nothing of the API server: %q", dispatch.BackendError(err))
	}
	// no token file: the same, without sending anything
	r2 := &runs.Kube{Server: "http://127.0.0.1:1", TokenFile: filepath.Join(t.TempDir(), "none")}
	if err := r2.Delete(context.Background(), "r-00002a"); err == nil || strings.Contains(err.Error(), "none") {
		t.Fatalf("%v", err)
	}
}

// A redirect is never followed: it would carry the bearer token to wherever it points.
func TestKubeDoesNotFollowRedirects(t *testing.T) {
	var hit atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("tok"), 0o600)
	k := &runs.Kube{Server: srv.URL, TokenFile: tok, Timeout: time.Second}
	if err := k.Create(context.Background(), manifest()); err == nil || hit.Load() {
		t.Fatalf("followed: %v", hit.Load())
	}
}

func TestKubeTimeoutIsAnErrorNotAHang(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body) // the server notices a client that went away once the body is read
		<-req.Context().Done()
	}))
	defer slow.Close()
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("tok"), 0o600)
	k := &runs.Kube{Server: slow.URL, TokenFile: tok, Timeout: 100 * time.Millisecond}
	start := time.Now()
	err := k.Create(context.Background(), manifest())
	if err == nil || time.Since(start) > 2*time.Second || !strings.Contains(err.Error(), "TimeoutError") {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestBackendCreatesDeletesAndSaysHowARunEnded(t *testing.T) {
	st := runs.NewFakeStore()
	b := &runs.Backend{Store: st}
	s, d := agentSession(t, nil)
	name, err := b.Create(context.Background(), s, d, map[string]any{"prompt": "hi"})
	if err != nil || name != "r-0000a1" || st.Manifests["r-0000a1"] == nil || b.Name(s) != "r-0000a1" {
		t.Fatalf("%q %v", name, err)
	}
	if got, _ := b.Ended(context.Background(), s); got != "" {
		t.Fatalf("still running: %q", got)
	}
	st.Outcomes["r-0000a1"] = "Succeeded"
	if got, _ := b.Ended(context.Background(), s); got != "Succeeded" {
		t.Fatalf("%q", got)
	}
	if err := b.Delete(context.Background(), s); err != nil || len(st.Manifests) != 0 {
		t.Fatal(err)
	}
	if b.Principal("r-0000a1") != "agent-r-0000a1/run" {
		t.Fatal(b.Principal("r-0000a1"))
	}
	// the backend sends nothing the manifest builder refused
	bad, _ := agentSession(t, func(r *session.Record) { r.TaskID = "no good" })
	if _, err := b.Create(context.Background(), bad, d, nil); err == nil || len(st.Manifests) != 0 {
		t.Fatal("a manifest with an invalid label value reached the store")
	}
}
