// Package definitions parses agent definitions (autopilot.engine.profile.parse) into what the gateway reads of them:
// the limits a session is cut from, and the scope every call is decided against. It reads the cluster-pinned set the
// Python gateway's loader keeps in gateway_definitions (or a directory of YAML converted to JSON by the caller). It
// does not validate against the JSON schema: the loader that wrote the set did, and the core decides every pin again.
package definitions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
	"go.yaml.in/yaml/v3"
)

// Definition is one agent definition.
type Definition struct {
	Name           string
	Kind           string
	IdentityType   string
	ServiceAccount *string
	Limits         client.Limits
	ReposAllow     []string
	APIAllow       []string
	DenyPaths      []string
	FieldAllow     []string
	FieldDeny      []string
	Image          *string
	Framework      string
	Sandbox        string
	Sidecars       []map[string]string
	Instructions   *string
	Owner          *string
	Description    string
	RawProfile     json.RawMessage
}

// RunsIn is where the agent's loop runs: "cluster" when the definition names a runtime image, else "client".
func (d *Definition) RunsIn() string {
	if d.Image != nil && *d.Image != "" {
		return "cluster"
	}
	return "client"
}

type doc struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Owner       *string `json:"owner"`
	Description string  `json:"description"`
	Identity    struct {
		Type           string  `json:"type"`
		ServiceAccount *string `json:"serviceAccount"`
	} `json:"identity"`
	Profile json.RawMessage `json:"profile"`
	Runtime struct {
		Image        *string             `json:"image"`
		Framework    string              `json:"framework"`
		Sandbox      string              `json:"sandbox"`
		Sidecars     []map[string]string `json:"sidecars"`
		Instructions *string             `json:"instructions"`
	} `json:"runtime"`
}

type profile struct {
	TierCeiling string `json:"tierCeiling"`
	Limits      struct {
		TTLMinutes  *int64 `json:"ttlMinutes"`
		ToolCalls   *int64 `json:"toolCalls"`
		GithubCalls *int64 `json:"githubCalls"`
		OpenPRs     *int64 `json:"openPrs"`
		ModelTokens *int64 `json:"modelTokens"`
		Children    *int64 `json:"children"`
	} `json:"limits"`
	Tools   []string `json:"tools"`
	Models  []string `json:"models"`
	Network string   `json:"network"`
	Compute string   `json:"compute"`
	Repos   struct {
		Allow      []string `json:"allow"`
		DenyPaths  []string `json:"denyPaths"`
		FieldDeny  []string `json:"fieldDeny"`
		FieldAllow []string `json:"fieldAllow"`
	} `json:"repos"`
	APIs struct {
		Allow []string `json:"allow"`
	} `json:"apis"`
}

var networks = map[string]string{"NONE": "none", "CLEARANCE": "clearance", "CLEARANCE_MODEL": "clearance+model", "ALLOWLIST": "allowlist"}
var computes = map[string]bool{"small": true, "medium": true, "large": true, "gpu": true}

// Parse reads one definition document. baseline is the registry that carries the baseline deny lists.
func Parse(raw []byte, baseline *tools.Registry) (*Definition, error) {
	var d doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.Name == "" || d.Kind == "" || d.Identity.Type == "" || d.Description == "" || len(d.Profile) == 0 {
		return nil, fmt.Errorf("%s: name, kind, identity.type, description and profile are required", d.Name)
	}
	var p profile
	if err := json.Unmarshal(d.Profile, &p); err != nil {
		return nil, fmt.Errorf("%s: profile: %w", d.Name, err)
	}
	l := p.Limits
	if l.TTLMinutes == nil || l.ToolCalls == nil || l.GithubCalls == nil || l.OpenPRs == nil || l.ModelTokens == nil || l.Children == nil {
		return nil, fmt.Errorf("%s: profile/limits: every limit is required", d.Name)
	}
	if len(p.TierCeiling) != 2 || p.TierCeiling[0] != 'T' || p.TierCeiling[1] < '0' || p.TierCeiling[1] > '3' {
		return nil, fmt.Errorf("%s: profile/tierCeiling: %q is not T0 to T3", d.Name, p.TierCeiling)
	}
	net := p.Network
	if net == "" {
		net = "clearance"
	}
	label, ok := networks[strings.NewReplacer("+", "_", "-", "_").Replace(strings.ToUpper(net))]
	if !ok {
		return nil, fmt.Errorf("%s: unknown network mode %q", d.Name, net)
	}
	compute := strings.ToLower(p.Compute)
	if compute == "" {
		compute = "small"
	}
	if !computes[compute] {
		return nil, fmt.Errorf("%s: unknown compute class %q", d.Name, p.Compute)
	}
	toolset := map[string]bool{}
	for _, t := range p.Tools {
		toolset[t] = true
	}
	// human.request grants human.request.get with it: an agent that may ask may read its own answers.
	if toolset["human.request"] {
		toolset["human.request.get"] = true
	}
	framework, sandbox := d.Runtime.Framework, d.Runtime.Sandbox
	if framework == "" {
		framework = "generic"
	}
	if sandbox == "" {
		sandbox = "standard"
	}
	def := &Definition{
		Name: d.Name, Kind: d.Kind, IdentityType: d.Identity.Type, ServiceAccount: d.Identity.ServiceAccount,
		Limits: client.Limits{TierCeiling: p.TierCeiling, TTLMinutes: *l.TTLMinutes, ToolCalls: *l.ToolCalls,
			GithubCalls: *l.GithubCalls, OpenPRs: *l.OpenPRs, ModelTokens: *l.ModelTokens, Children: *l.Children,
			Tools: sortedKeys(toolset), Models: sortedList(p.Models), Network: label, Compute: compute},
		ReposAllow: nonNil(p.Repos.Allow), APIAllow: nonNil(p.APIs.Allow), FieldAllow: nonNil(p.Repos.FieldAllow),
		DenyPaths: dedupe(baseline.BaselineDenyPaths, p.Repos.DenyPaths), FieldDeny: dedupe(baseline.BaselineDenyFields, p.Repos.FieldDeny),
		Image: d.Runtime.Image, Framework: framework, Sandbox: sandbox, Sidecars: d.Runtime.Sidecars,
		Instructions: d.Runtime.Instructions, Owner: d.Owner, Description: d.Description, RawProfile: d.Profile,
	}
	return def, nil
}

// FromRecords reads the loaded rows of gateway_definitions ({name, state: "loaded", source, doc}); a row that does not
// parse, and a rejected one, is skipped and named in the second result.
func FromRecords(records []json.RawMessage, baseline *tools.Registry) (map[string]*Definition, []string) {
	out := map[string]*Definition{}
	var skipped []string
	for _, raw := range records {
		var r struct {
			Name  string          `json:"name"`
			State string          `json:"state"`
			Doc   json.RawMessage `json:"doc"`
		}
		if json.Unmarshal(raw, &r) != nil || r.State != "loaded" {
			skipped = append(skipped, r.Name)
			continue
		}
		d, err := Parse(r.Doc, baseline)
		if err != nil {
			skipped = append(skipped, r.Name+": "+err.Error())
			continue
		}
		out[d.Name] = d
	}
	return out, skipped
}

// Scope is the scope the evaluation carries for a session of this definition.
func (d *Definition) Scope() *client.Scope {
	s := &client.Scope{AgentKind: d.Kind, DenyPaths: nonNil(d.DenyPaths), ReposAllow: nonNil(d.ReposAllow), APIAllow: nonNil(d.APIAllow),
		FieldDeny: nonNil(d.FieldDeny), FieldAllow: nonNil(d.FieldAllow)}
	if len(d.RawProfile) > 0 && string(d.RawProfile) != "{}" {
		s.Profile = d.RawProfile
	}
	return s
}

// AgentDef is the part a session.open or session.spawn reads.
func (d *Definition) AgentDef() *client.AgentDef {
	return &client.AgentDef{Name: d.Name, Kind: d.Kind, IdentityType: d.IdentityType, ServiceAccount: d.ServiceAccount, Limits: d.Limits}
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedList(a []string) []string {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	return sortedKeys(set)
}

func dedupe(base, extra []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range [][]string{base, extra} {
		for _, x := range l {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	return out
}

// LoadDir reads a directory of agent definitions (YAML, as bundled in agents/), the file name matching the agent name.
func LoadDir(dir string, baseline *tools.Registry) (map[string]*Definition, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := map[string]*Definition{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var generic any
		if err := yaml.Unmarshal(raw, &generic); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		js, err := json.Marshal(generic)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		d, err := Parse(js, baseline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if _, dup := out[d.Name]; dup {
			return nil, fmt.Errorf("duplicate agent name %q (%s)", d.Name, filepath.Base(f))
		}
		if strings.TrimSuffix(filepath.Base(f), ".yaml") != d.Name {
			return nil, fmt.Errorf("%s: file name must match agent name %q", filepath.Base(f), d.Name)
		}
		out[d.Name] = d
	}
	return out, nil
}
