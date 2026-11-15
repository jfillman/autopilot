package airframe_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// oracle is the closed list of reads written out on its own (not the allowlist's own regexes).
var oracle = regexp.MustCompile(`^(?:/api/v1/namespaces/[a-z0-9][a-z0-9-]*|/apis/(catalog\.hangar\.io/v1alpha1/namespaces/[a-z0-9][a-z0-9-]*/[a-z]+(/[a-z0-9][a-z0-9.-]*)?|argoproj\.io/v1alpha1/namespaces/[a-z0-9][a-z0-9-]*/(applications|rollouts)(/[a-z0-9][a-z0-9.-]*)?))$`)

var attacks = []string{"", "a", "shop", "A", "Shop", "a/b", "../x", "..", ".", "a%2fb", "a%2Fb", "x%00", "a\x00b", "a\nb", "a\r\nb", "é", "日本", "a b", "a?b=1", "a#b",
	"a\\b", "a;b", "x,hangar.io/env=y", "x=y", strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 300), "-", "-a", "a-", "a.b", "a_b", "*", "{name}", "${x}", "'; drop", "\u202e", "a\ufeffb"}

type recorder struct {
	mu   sync.Mutex
	reqs []string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Method+" "+req.URL.RequestURI())
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.Count(req.URL.Path, "/") == 6 {
		_, _ = w.Write([]byte(`{"items":[]}`))
		return
	}
	w.WriteHeader(404)
}

func checkRequests(t *testing.T, reqs []string) {
	t.Helper()
	for _, r := range reqs {
		method, target, _ := strings.Cut(r, " ")
		u, err := url.Parse(target)
		if method != "GET" || err != nil || !oracle.MatchString(u.EscapedPath()) || u.EscapedPath() != u.Path {
			t.Errorf("a request outside the closed list: %s", r)
			continue
		}
		for k := range u.Query() {
			if k != "labelSelector" && k != "limit" && k != "continue" {
				t.Errorf("a query parameter outside the list: %s", r)
			}
		}
	}
}

func TestStateNeverReachesAPathOutsideTheClosedList(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	s := &airframe.State{Kube: newReader(t, srv), Cluster: "c"}
	ctx := context.Background()
	refused := 0
	for _, a := range attacks {
		for _, e := range attacks {
			e := e
			for _, call := range []func() (any, error){
				func() (any, error) { return s.Status(ctx, a, &e) },
				func() (any, error) { return s.Describe(ctx, a, e) },
				func() (any, error) { return s.Verify(ctx, a, "PythonApplication", e, nil) },
				func() (any, error) { return s.Verify(ctx, "shop", a, "x", &e) },
			} {
				if _, err := call(); err != nil {
					refused++
				}
			}
		}
	}
	checkRequests(t, rec.reqs)
	if refused == 0 || len(rec.reqs) == 0 {
		t.Fatalf("the corpus proved nothing: %d refused, %d requests", refused, len(rec.reqs))
	}
	for _, r := range rec.reqs {
		if strings.Contains(r, "/secrets/") || strings.Contains(r, "/secrets?") || strings.Contains(r, "/configmaps") || (strings.Contains(r, "/api/") && !strings.Contains(r, " /api/v1/namespaces/")) {
			t.Fatalf("%s", r)
		}
	}
}

// hostile answers: every shape an XR, an Application or a Rollout may have, none of which may panic or make the answer unbounded.
func hostileBodies() []string {
	deep := strings.Repeat(`{"a":`, 150) + `1` + strings.Repeat(`}`, 150)
	long := strings.Repeat("x", 200000)
	return []string{
		`{"items":[{"kind":"PythonApplication","metadata":["x"],"status":"s","spec":[1]}]}`,
		`{"items":[{"kind":5,"metadata":{"name":7,"generation":"3","namespace":[]},"status":{"conditions":"nope"}}]}`,
		`{"items":[{"kind":"Redis","metadata":{"name":"r","generation":1e999},"status":{"conditions":[{"type":{"a":1},"status":[],"reason":7,"message":"` + long + `","observedGeneration":"2"},3,null,"x"]}}]}`,
		`{"items":[{"kind":"PythonApplication","metadata":{"name":"` + long + `"},"spec":` + deep + `,"status":{"conditions":[{"type":"Synced","status":"True","observedGeneration":true},{"type":"Ready","status":"True","observedGeneration":null}]}}]}`,
		`{"items":[{"kind":"Application","metadata":{"labels":"x"},"spec":{"sources":"s","source":[1]},"status":{"sync":[],"resources":"r","health":5,"operationState":7}}]}`,
		`{"items":[{"kind":"Application","metadata":{"name":"a","namespace":"b","labels":{"hangar.io/ephemeral-env":"true"}},"spec":{"sources":[1,null,{"repoURL":5}]},"status":{"resources":[1,{"kind":"Rollout","group":"argoproj.io","namespace":5,"name":[]},{"kind":"Rollout","group":"argoproj.io","namespace":"app-shop-dev","name":"` + long + `"}],"sync":{"revisions":"abc"}}}]}`,
		`{"kind":"Rollout","metadata":{"generation":"x"},"spec":{"template":{"spec":{"containers":[1,{"image":{}}]}},"strategy":[]},"status":{"observedGeneration":{"a":1}}}`,
		`[1,2,3]`, `"string"`, `null`, `5`, `{"items":null}`, `{"items":"x"}`, `{"items":[1,2]}`, `{"items":[[]]}`, `{"metadata":{"continue":5},"items":[]}`, ``, `{`,
		`{"items":[{"kind":"PythonApplication","metadata":{"name":"x","namespace":"app-shop-cicd","generation":2},"status":{"conditions":[{"type":"Synced","status":"True","observedGeneration":1.5},{"type":"Ready","status":false}]},"spec":{"crossplane":"x"}}]}`,
		`{"items":[{"kind":"PythonApplication","metadata":{"name":"x"},"spec":{"crossplane":{"resourceRefs":"abc"}}}]}`,
	}
}

func TestStateReadsHostileAnswersWithoutPanicOrUnboundedOutput(t *testing.T) {
	for i, body := range hostileBodies() {
		body := body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		s := &airframe.State{Kube: newReader(t, srv), Cluster: "c"}
		env := "dev"
		for name, call := range map[string]func() (any, error){
			"status":   func() (any, error) { return s.Status(context.Background(), "shop", &env) },
			"describe": func() (any, error) { return s.Describe(context.Background(), "shop", "dev") },
			"verify":   func() (any, error) { return s.Verify(context.Background(), "shop", "PythonApplication", "x", &env) },
			"verify2":  func() (any, error) { return s.Verify(context.Background(), "shop", "Redis", "x", nil) },
		} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("body %d, %s: panic %v", i, name, r)
					}
				}()
				out, err := call()
				if err == nil {
					b, merr := json.Marshal(out)
					if merr != nil {
						t.Errorf("body %d, %s: result does not marshal: %v", i, name, merr)
					}
					if len(b) > 12<<20 {
						t.Errorf("body %d, %s: %d bytes of answer", i, name, len(b))
					}
				}
			}()
		}
		srv.Close()
	}
}

type bigReader struct{ n int }

func (b bigReader) Get(ctx context.Context, path string) (any, bool, error) {
	if path == "/api/v1/namespaces/app-shop-cicd" {
		return map[string]any{"metadata": map[string]any{"labels": map[string]any{"hangar.io/app": "shop"}}}, true, nil
	}
	return nil, false, nil
}
func (b bigReader) List(ctx context.Context, path, sel string) ([]map[string]any, error) {
	if !strings.HasSuffix(path, "/pythonapplications") || !strings.Contains(path, "app-shop-cicd") {
		return nil, nil
	}
	out := make([]map[string]any, b.n)
	for i := range out {
		out[i] = map[string]any{"kind": "PythonApplication", "metadata": map[string]any{"name": fmt.Sprintf("a%05d", i)}}
	}
	return out, nil
}

func TestAListOfAThousandItemsIsTheMostOneKindAnswers(t *testing.T) {
	s := &airframe.State{Kube: bigReader{5000}}
	out, err := s.Status(context.Background(), "shop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out.(map[string]any)["xrs"].([]any)); n != 1000 {
		t.Fatalf("%d XRs", n)
	}
}

func TestEveryKnownFailureMatchesItsOwnExampleUnderGoRegexps(t *testing.T) {
	c := airframe.Contract()
	n := 0
	for _, g := range []string{"components", "targets", "kinds"} {
		grp := c.M[g].(*definitions.OMap)
		for _, k := range grp.Keys {
			sc := grp.M[k].(*definitions.OMap)
			list, _ := sc.M["knownFailures"].([]any)
			for _, kf := range list {
				o := kf.(*definitions.OMap)
				re, err := regexp.Compile(o.M["match"].(string))
				if err != nil {
					t.Fatalf("%s: %v", o.M["id"], err)
				}
				if !re.MatchString(o.M["example"].(string)) {
					t.Errorf("%s does not match its own example", o.M["id"])
				}
				n++
			}
		}
	}
	if n < 5 {
		t.Fatalf("%d known failures", n)
	}
}
