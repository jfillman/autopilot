package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// review serves TokenReview: it records the audiences asked for and answers with the ones the token carries.
func review(t *testing.T, carries []string, asked *[]string) *Workload {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Spec struct {
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*asked = body.Spec.Audiences
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"authenticated": true, "audiences": carries,
			"user": map[string]any{"username": "system:serviceaccount:agents:run-1"}}})
	}))
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("own"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWorkload("autopilot", "clearance", "autopilot")
	w.API, w.TokenFile, w.HTTP = srv.URL, tok, srv.Client()
	return w
}

// R-10b: the run pods' token carries `autopilot` (and a pre-R-10b runtime image's the old `clearance`); TokenReview is
// asked for the whole set and a token is accepted when it carries any of them (Python: WorkloadAuthenticator).
func TestAWorkloadTokenIsAcceptedForAnyAudienceOfTheSet(t *testing.T) {
	var asked []string
	w := review(t, []string{"autopilot"}, &asked)
	if !slices.Equal(w.Audiences, []string{"autopilot", "clearance"}) {
		t.Fatalf("the set is deduplicated, the primary first: %v", w.Audiences)
	}
	p, err := w.Authenticate(context.Background(), "tok")
	if err != nil || p.ID != "agents/run-1" || !slices.Equal(asked, []string{"autopilot", "clearance"}) {
		t.Fatalf("%+v %v asked %v", p, err, asked)
	}
	for _, carries := range [][]string{{"clearance"}, {"x", "autopilot"}} {
		if _, err := review(t, carries, &asked).Authenticate(context.Background(), "tok"); err != nil {
			t.Fatalf("a token carrying %v: %v", carries, err)
		}
	}
	for _, carries := range [][]string{{}, {"model-proxy"}} {
		if _, err := review(t, carries, &asked).Authenticate(context.Background(), "tok"); err == nil {
			t.Fatalf("a token carrying %v was accepted", carries)
		}
	}
}

func TestASingleAudienceWorkloadStillAsksForOne(t *testing.T) {
	w := NewWorkload("model-proxy")
	if !slices.Equal(w.audiences(), []string{"model-proxy"}) {
		t.Fatal(w.audiences())
	}
	lit := &Workload{Audience: "x"}
	if !slices.Equal(lit.audiences(), []string{"x"}) {
		t.Fatal(lit.audiences())
	}
}
