package definitions_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

func norm(t *testing.T, v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// Every bundled agent definition parses to the scope and agent the Python gateway sends the core for it
// (testdata/agents.json is generated from the Python parser: tools/gen_gateway_registry.py).
func TestBundledDefinitionsParseAsPythonParsesThem(t *testing.T) {
	raw, err := os.ReadFile("testdata/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string          `json:"name"`
		Doc    json.RawMessage `json:"doc"`
		Scope  any             `json:"scope"`
		Agent  any             `json:"agent"`
		RunsIn string          `json:"runs_in"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("only %d definitions", len(cases))
	}
	reg := tools.Default()
	for _, c := range cases {
		d, err := definitions.Parse(c.Doc, reg)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := norm(t, d.Scope()); !reflect.DeepEqual(got, norm(t, c.Scope)) {
			t.Errorf("%s: scope\n got %v\nwant %v", c.Name, got, c.Scope)
		}
		if got := norm(t, d.AgentDef()); !reflect.DeepEqual(got, norm(t, c.Agent)) {
			t.Errorf("%s: agent\n got %v\nwant %v", c.Name, got, c.Agent)
		}
		if d.RunsIn() != c.RunsIn {
			t.Errorf("%s: runs_in %s != %s", c.Name, d.RunsIn(), c.RunsIn)
		}
	}
}

func TestLoadDirReadsTheBundledAgents(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "agents")
	defs, err := definitions.LoadDir(dir, tools.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := defs["coding-agent"]; !ok || len(defs) < 8 {
		t.Fatalf("%d definitions", len(defs))
	}
}

func TestAMissingFieldIsRefusedNotDefaulted(t *testing.T) {
	_, err := definitions.Parse([]byte(`{"name":"x","kind":"task","identity":{"type":"delegated"},"description":"d","profile":{"tierCeiling":"T1","tools":[]}}`), tools.Default())
	if err == nil {
		t.Fatal("limits are required")
	}
}
