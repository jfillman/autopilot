// Package tools is the gateway's tool registry: the names, tiers, arguments and costs of every tool a Hangar gateway
// registers (core tools, then the Hangar pack), as the Python gateway registers them. registry.json is generated from
// the Python registry (tools/gen_gateway_registry.py) and a Python test fails when the two differ, so the names and
// shapes stay one thing while both implementations exist.
package tools

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

// Tier is a tool's tier: T0 reads, T1 reversible writes, T2 propose-only, T3 never exposed.
type Tier int

const (
	T0 Tier = iota
	T1
	T2
	T3
)

func (t Tier) String() string { return fmt.Sprintf("T%d", int(t)) }

// Spec is one tool (autopilot.engine.tiers.ToolSpec).
type Spec struct {
	Name         string   `json:"name"`
	Tier         Tier     `json:"tier"`
	Description  string   `json:"description"`
	RequiredArgs []string `json:"required_args"`
	GithubCost   int64    `json:"github_cost"`
	Target       string   `json:"target"`
	ProposeOnly  bool     `json:"propose_only"`
	WritesFiles  bool     `json:"writes_files"`
	ReadsRepo    bool     `json:"reads_repo"`
	Receipt      []string `json:"receipt"`
}

// Registry is the tools by name, in registration order.
type Registry struct {
	byName map[string]*Spec
	order  []string
	// Tripwires are the T3 names registered only so an attempt is loud.
	Tripwires []string
	// BaselineDenyPaths and BaselineDenyFields are denied for every agent, whatever its definition says.
	BaselineDenyPaths  []string
	BaselineDenyFields []string
}

//go:embed registry.json
var embedded []byte

type file struct {
	Tools              []Spec   `json:"tools"`
	Tripwires          []string `json:"tripwires"`
	BaselineDenyPaths  []string `json:"baseline_deny_paths"`
	BaselineDenyFields []string `json:"baseline_deny_fields"`
}

// Default is the registry this build carries.
func Default() *Registry {
	r, err := Parse(embedded)
	if err != nil {
		panic("tools: embedded registry: " + err.Error())
	}
	return r
}

// Parse reads a registry document. A name registered twice, or a tripwire registered as a tool, is an error.
func Parse(raw []byte) (*Registry, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	r := &Registry{byName: map[string]*Spec{}, Tripwires: f.Tripwires, BaselineDenyPaths: f.BaselineDenyPaths,
		BaselineDenyFields: f.BaselineDenyFields}
	trip := map[string]bool{}
	for _, t := range f.Tripwires {
		trip[t] = true
	}
	for i := range f.Tools {
		t := f.Tools[i]
		if _, dup := r.byName[t.Name]; dup {
			return nil, fmt.Errorf("tool %q is registered twice", t.Name)
		}
		if trip[t.Name] {
			return nil, fmt.Errorf("tool %q is a T3 tripwire and can never be registered", t.Name)
		}
		r.byName[t.Name] = &t
		r.order = append(r.order, t.Name)
	}
	return r, nil
}

// Get returns a tool's spec.
func (r *Registry) Get(name string) (*Spec, bool) { s, ok := r.byName[name]; return s, ok }

// All lists the tools in registration order.
func (r *Registry) All() []*Spec {
	out := make([]*Spec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Names lists the tool names sorted.
func (r *Registry) Names() []string {
	n := append([]string(nil), r.order...)
	sort.Strings(n)
	return n
}

// Len is the number of tools.
func (r *Registry) Len() int { return len(r.order) }
