package session_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// The limit rules, held equal to the Python gateway's (engine/limits.py narrow, SessionStore.open): the cases are
// generated from the Python code by tools/gen_gateway_registry.py. A child is never wider than the parent has left, never
// deeper than MaxDepth, never past the parent's deadline; a request can only narrow.
func TestCutChildEqualsPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Parents map[string]session.Record `json:"parents"`
		Base    client.Limits             `json:"base"`
		Now     time.Time                 `json:"now"`
		Cases   []struct {
			Parent  string          `json:"parent"`
			Request json.RawMessage `json:"request"`
			Result  struct {
				Limits    *client.Limits `json:"limits"`
				ExpiresAt time.Time      `json:"expires_at"`
				Depth     int            `json:"depth"`
				Widening  []string       `json:"widening"`
				Error     string         `json:"error"`
				Bad       bool           `json:"bad_request"`
			} `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) < 200 {
		t.Fatalf("only %d cases: the fixture is not the one the generator writes", len(fx.Cases))
	}
	seen := map[string]int{}
	for _, c := range fx.Cases {
		parent, err := session.FromRecord(fx.Parents[c.Parent])
		if err != nil {
			t.Fatal(err)
		}
		req, perr := session.ParseLimitRequest(c.Request)
		if c.Result.Bad {
			if perr == nil {
				t.Errorf("%s %s: a bad request was accepted", c.Parent, c.Request)
			}
			seen["bad"]++
			continue
		}
		if perr != nil {
			t.Errorf("%s %s: %v", c.Parent, c.Request, perr)
			continue
		}
		parent.Lock()
		got, err := session.CutChild(parent, fx.Base, req, fx.Now)
		parent.Unlock()
		switch {
		case c.Result.Widening != nil:
			var w *session.WideningError
			if !errors.As(err, &w) || !reflect.DeepEqual(w.Fields, c.Result.Widening) {
				t.Errorf("%s %s: want widening %v, got %v", c.Parent, c.Request, c.Result.Widening, err)
			}
			seen["widening"]++
		case c.Result.Error != "":
			var e *session.Error
			if !errors.As(err, &e) || e.Code != c.Result.Error {
				t.Errorf("%s %s: want error %q, got %v", c.Parent, c.Request, c.Result.Error, err)
			}
			seen["error"]++
		default:
			if err != nil {
				t.Errorf("%s %s: %v", c.Parent, c.Request, err)
				continue
			}
			if !reflect.DeepEqual(got.Limits, *c.Result.Limits) || !got.ExpiresAt.Equal(c.Result.ExpiresAt) || got.Depth != c.Result.Depth {
				t.Errorf("%s %s:\n go     %+v %v %d\n python %+v %v %d", c.Parent, c.Request, got.Limits, got.ExpiresAt, got.Depth,
					*c.Result.Limits, c.Result.ExpiresAt, c.Result.Depth)
			}
			seen["ok"]++
		}
	}
	for _, k := range []string{"ok", "widening", "error", "bad"} {
		if seen[k] == 0 {
			t.Errorf("no case compares %q: a comparison of nothing proves nothing", k)
		}
	}
	t.Logf("compared %v", seen)
}

// A grant the core cut is only as wide as the parent allows: Within names every field above what a child of this
// definition could have.
func TestWithinNamesEveryWidenedField(t *testing.T) {
	raw, _ := os.ReadFile("testdata/limits.json")
	var fx struct {
		Parents map[string]session.Record `json:"parents"`
		Base    client.Limits             `json:"base"`
		Now     time.Time                 `json:"now"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	parent, _ := session.FromRecord(fx.Parents["spent"])
	parent.Lock()
	defer parent.Unlock()
	max := session.ChildBase(parent, fx.Base, fx.Now)
	if over := session.Within(parent, fx.Base, max, fx.Now); len(over) != 0 {
		t.Fatalf("the widest child is within itself: %v", over)
	}
	wide := max
	wide.TierCeiling, wide.TTLMinutes, wide.ToolCalls, wide.GithubCalls = "T3", max.TTLMinutes+1, max.ToolCalls+1, max.GithubCalls+1
	wide.OpenPRs, wide.ModelTokens, wide.Children = max.OpenPRs+1, max.ModelTokens+1, max.Children+1
	wide.Tools, wide.Models = append(append([]string{}, max.Tools...), "repo.pr.open"), append(append([]string{}, max.Models...), "gpt-9")
	wide.Network, wide.Compute = "allowlist", "gpu"
	over := session.Within(parent, fx.Base, wide, fx.Now)
	if len(over) != 11 {
		t.Fatalf("every widened field is named: %v", over)
	}
	neg := max
	neg.ToolCalls = -1
	if over := session.Within(parent, fx.Base, neg, fx.Now); len(over) != 1 {
		t.Fatalf("a negative limit is not narrower, it is nonsense: %v", over)
	}
}
