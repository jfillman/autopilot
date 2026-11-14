package session_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// What a child may have at most, held equal to the Python gateway's cut (SessionStore.open, with no request): the child's
// definition limits cut to what the parent has left, field by field. The cases are generated from the Python code. Python's
// request narrowing (limits.narrow) is the core's job here and is not in Go: the gateway only checks the grant against this.
func TestChildBaseEqualsPythonsCutOfAChild(t *testing.T) {
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
			} `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range fx.Cases {
		if c.Result.Limits == nil || (string(c.Request) != "null" && string(c.Request) != "{}") {
			continue
		}
		parent, err := session.FromRecord(fx.Parents[c.Parent])
		if err != nil {
			t.Fatal(err)
		}
		parent.Lock()
		got := session.ChildBase(parent, fx.Base, fx.Now)
		parent.Unlock()
		if !reflect.DeepEqual(got, *c.Result.Limits) {
			t.Errorf("%s:\n go     %+v\n python %+v", c.Parent, got, *c.Result.Limits)
		}
		n++
	}
	if n < 5 {
		t.Fatalf("only %d cases compared", n)
	}
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
