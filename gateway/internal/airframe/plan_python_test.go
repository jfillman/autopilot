package airframe

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// The planner, capabilities and explain are held equal to the Python code: tools/gen_gateway_airframe_plan.py drives the Python
// tools over a corpus and records what each answered. A case carries "go" where Go deliberately differs, and "why".

type planCase struct {
	Name     string          `json:"name"`
	Tool     string          `json:"tool"`
	Spec     json.RawMessage `json:"spec"`
	BaseCicd json.RawMessage `json:"baseCicd"`
	Registry struct {
		Dev    string   `json:"dev"`
		Upper  []string `json:"upper"`
		Tenant string   `json:"tenants"`
		Owner  string   `json:"owner"`
	} `json:"registry"`
	Args   map[string]json.RawMessage `json:"args"`
	Expect json.RawMessage            `json:"expect"`
	Raises string                     `json:"raises"`
	Go     json.RawMessage            `json:"go"`
	Why    string                     `json:"why"`
}

func decodeNum(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	return v, err
}

// roundTrip is v as JSON read back (numbers by value), the form the two sides are compared in.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(definitions.Plain(v))
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func loadPlanCases(t *testing.T) []planCase {
	t.Helper()
	b, err := os.ReadFile("testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []planCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Cases
}

func argOf(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	v, err := decodeNum(raw)
	if err != nil {
		t.Fatal(err)
	}
	a, err := FromArgs(v)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPlanToolsEqualPython(t *testing.T) {
	cases := loadPlanCases(t)
	if len(cases) < 250 {
		t.Fatalf("the corpus has %d cases", len(cases))
	}
	n := map[string]int{}
	for _, c := range cases {
		t.Run(c.Tool+"/"+c.Name, func(t *testing.T) {
			if (len(c.Go) > 0) != (c.Why != "") {
				t.Fatal("a case carries go with a why, or neither")
			}
			want := c.Expect
			if len(c.Go) > 0 {
				want = c.Go
			}
			var got any
			var raised string
			switch c.Tool {
			case "plan":
				reg := Registry{DevCluster: c.Registry.Dev, UpperClusters: c.Registry.Upper, TenantsRepo: c.Registry.Tenant, Owner: c.Registry.Owner}
				var base any
				if len(c.BaseCicd) > 0 && string(c.BaseCicd) != "null" {
					base = argOf(t, c.BaseCicd)
				}
				got = PlanAnswer(argOf(t, c.Spec), base, reg)
			case "capabilities":
				var kind, section string
				_ = json.Unmarshal(c.Args["kind"], &kind)
				_ = json.Unmarshal(c.Args["section"], &section)
				v, err := Capabilities(kind, section)
				var pe *PyError
				if errors.As(err, &pe) {
					raised = pe.Class
				} else if err != nil {
					t.Fatal(err)
				}
				got = v
			case "explain":
				v, err := Explain(argOf(t, c.Args["rule"]))
				var pe *PyError
				if errors.As(err, &pe) {
					raised = pe.Class
				} else if err != nil {
					t.Fatal(err)
				}
				got = v
			default:
				t.Fatal("unknown tool " + c.Tool)
			}
			if c.Raises != "" {
				if raised != c.Raises {
					t.Fatalf("raised %q, Python %q", raised, c.Raises)
				}
				return
			}
			if raised != "" {
				t.Fatalf("raised %s; Python answered", raised)
			}
			var w any
			if err := json.Unmarshal(want, &w); err != nil {
				t.Fatal(err)
			}
			g := roundTrip(t, got)
			if !reflect.DeepEqual(g, w) {
				gj, _ := json.MarshalIndent(g, "", " ")
				wj, _ := json.MarshalIndent(w, "", " ")
				t.Fatalf("differs from Python\n got: %.3000s\nwant: %.3000s", gj, wj)
			}
		})
		n[c.Tool]++
		if len(c.Go) > 0 {
			n["go"]++
		}
	}
	t.Logf("cases: %v", n)
}
