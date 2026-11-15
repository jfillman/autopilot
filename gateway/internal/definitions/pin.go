package definitions

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// RunIdentityNamespace is the namespace a pinned agent's workload identity lives in (engine/definitions.py).
const RunIdentityNamespace = "autopilot-runs"

// Source is where a definition came from. Kind is "bundled" (the image's agents/ directory) or "cluster" (an Agent XR's pin).
// A field that is nil is absent (Python's None), which is not the same as an empty string: the published request carries an
// empty repo but not an absent one.
type Source struct {
	Kind      string
	Repo      *string
	Path      *string
	Commit    *string
	Namespace *string
	XR        *string
	LoadedAt  *string
}

// S is a pointer to a copy of s.
func S(s string) *string { return &s }

func strOr(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}

// Pin is one line: repo@short path, or `bundled`.
func (s Source) Pin() string {
	if s.Kind != "cluster" {
		return "bundled"
	}
	c := strOr(s.Commit, "")
	if len(c) > 7 {
		c = c[:7]
	}
	show := func(p *string) string {
		if p == nil {
			return "None"
		}
		return *p
	}
	return fmt.Sprintf("%s@%s %s", show(s.Repo), c, show(s.Path))
}

// View is Source.view(): the fields that are set, and the pin.
func (s Source) View() map[string]any {
	out := map[string]any{"kind": s.Kind}
	for k, v := range map[string]*string{"repo": s.Repo, "path": s.Path, "commit": s.Commit, "namespace": s.Namespace, "xr": s.XR, "loaded_at": s.LoadedAt} {
		if v != nil {
			out[k] = *v
		}
	}
	out["pin"] = s.Pin()
	return out
}

// SourceFromView is Source.from_view: a stored row's source.
func SourceFromView(v map[string]any) Source {
	str := func(k string) *string {
		if s, ok := v[k].(string); ok {
			return &s
		}
		return nil
	}
	kind, _ := v["kind"].(string)
	if kind == "" {
		kind = "cluster"
	}
	return Source{Kind: kind, Repo: str("repo"), Path: str("path"), Commit: str("commit"), Namespace: str("namespace"), XR: str("xr"), LoadedAt: str("loaded_at")}
}

func (s Source) key() string {
	f := func(p *string) string {
		if p == nil {
			return "\x00"
		}
		return "\x01" + *p
	}
	return strings.Join([]string{s.Kind, f(s.Repo), f(s.Path), f(s.Commit), f(s.Namespace), f(s.XR), f(s.LoadedAt)}, "\x02")
}

// Loaded is a pin that became a definition: the definition, its source, the XR's owner and the document it was read from.
type Loaded struct {
	Def    *Definition
	Source Source
	Owner  string
	Doc    map[string]any
}

// Name is the agent's name.
func (l *Loaded) Name() string { return l.Def.Name }

// Rejected is a pin that did not become a definition, and why.
type Rejected struct {
	Name   string
	Source Source
	Reason string
}

func (r Rejected) key() string { return r.Name + "\x03" + r.Source.key() + "\x03" + r.Reason }

// FileReason is why a pinned file's text is not a definition document, in the words the Python loader uses (agent_xrs._decide):
// "<path> is not valid YAML: ...", "<path> is not a YAML mapping (an agent definition)"; or, for a file that reads but is not
// written strictly (value.go), "<path> is not acceptable: ...". A nil document and an empty reason is not possible.
func ReadPinnedFile(path, text string) (*OMap, string) {
	doc, err := ParseYAML([]byte(text))
	var se *StrictError
	var ye *YAMLSyntaxError
	switch {
	case err == nil:
		return doc, ""
	case errors.Is(err, ErrNotAMapping):
		return nil, path + " is not a YAML mapping (an agent definition)"
	case errors.As(err, &ye):
		first := ye.First
		if r := []rune(first); len(r) > 160 {
			first = string(r[:160])
		}
		return nil, path + " is not valid YAML: " + first
	case errors.As(err, &se):
		return nil, path + " is not acceptable: " + se.Msg
	}
	return nil, path + " is not valid YAML: " + err.Error()
}

// PinPolicy is what the Go gateway asks of a pinned definition beyond the schema and the Python rules: a definition arrives
// through an app's own repository, so it is held to what the platform allows an agent of an app team to have. Bundled
// definitions are the platform's own and are not held to it. The zero value asks only for lower-case, owner-literal repository
// patterns and non-blanket app patterns.
type PinPolicy struct {
	// Tools, when set, is the registry a pin's tools must be in (a name the gateway has no tool for grants nothing today and
	// would grant whatever a later release gives that name).
	Tools *tools.Registry
	// MaxTier is the highest tier ceiling a pin may name: T0, T1 or T2 ("" is T2, which is the schema's own highest).
	MaxTier string
}

var tierRank = map[string]int{"T0": 0, "T1": 1, "T2": 2}

var onlyWildcards = regexp.MustCompile(`^\*+$`)

// Check is nil for a definition a pin may be, else the reason it may not (the text starts "policy: ").
func (p PinPolicy) Check(d *Definition) error {
	max := p.MaxTier
	if max == "" {
		max = "T2"
	}
	if tierRank[d.Limits.TierCeiling] > tierRank[max] {
		return fmt.Errorf("policy: profile/tierCeiling: %s is above %s, the highest ceiling a pinned agent may have", d.Limits.TierCeiling, max)
	}
	if p.Tools != nil {
		for _, t := range toolList(d) {
			if _, ok := p.Tools.Get(t); !ok {
				return fmt.Errorf("policy: profile/tools: %s is not a tool this gateway has", pyrepr.Str(t))
			}
		}
	}
	for _, pat := range d.ReposAllow {
		owner, repo, _ := strings.Cut(pat, "/")
		switch {
		case pat != strings.ToLower(pat):
			return fmt.Errorf("policy: profile/repos/allow: %s: write the pattern in lower case (repository names match without regard to case)", pyrepr.Str(pat))
		case strings.Contains(owner, "*"):
			return fmt.Errorf("policy: profile/repos/allow: %s: the owner must be written out, not matched with a wildcard", pyrepr.Str(pat))
		case onlyWildcards.MatchString(repo):
			return fmt.Errorf("policy: profile/repos/allow: %s: the repository part may not be only wildcards (name the repositories, or a prefix such as \"gate-*\")", pyrepr.Str(pat))
		}
	}
	prof, _ := d.Doc["profile"].(map[string]any)
	for _, section := range []string{"glidepath", "airframe"} {
		sec, _ := prof[section].(map[string]any)
		apps, _ := sec["apps"].([]any)
		for _, a := range apps {
			if s, _ := a.(string); onlyWildcards.MatchString(s) {
				return fmt.Errorf("policy: profile/%s/apps: %s: an app pattern that is only wildcards names every app", section, pyrepr.Str(s))
			}
		}
	}
	return nil
}

func toolList(d *Definition) []string {
	prof, _ := d.Doc["profile"].(map[string]any)
	ts, _ := prof["tools"].([]any)
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// FromPin decides one pin (engine/definitions.py from_pin): src.XR is the XR's name, owner its spec.owner, reserved the bundled
// definitions' names. A bad document is a Rejected with the reason the team needs, never a panic or an error.
func FromPin(doc *OMap, src Source, owner string, reserved map[string]*Definition, baseline *tools.Registry, policy PinPolicy) (*Loaded, *Rejected) {
	name := strOr(src.XR, "")
	if name == "" {
		name = "?"
	}
	rej := func(format string, a ...any) (*Loaded, *Rejected) {
		return nil, &Rejected{Name: name, Source: src, Reason: fmt.Sprintf(format, a...)}
	}
	d, err := ParseDoc(doc, baseline)
	if err != nil {
		return rej("schema: %s", err.Error())
	}
	if d.Name != name {
		return rej("the file's name is %s; a pin named %s must pin a definition named %s", pyrepr.Str(d.Name), pyrepr.Str(name), pyrepr.Str(name))
	}
	if d.Owner == nil || *d.Owner != owner {
		shown := "None"
		if d.Owner != nil {
			shown = pyrepr.Str(*d.Owner)
		}
		return rej("the file's owner is %s; the pin's owner is %s: they must agree", shown, pyrepr.Str(owner))
	}
	if d.IdentityType == "workload" && (d.ServiceAccount == nil || *d.ServiceAccount != RunIdentityNamespace+"/"+d.Name) {
		shown := "None"
		if d.ServiceAccount != nil {
			shown = pyrepr.Str(*d.ServiceAccount)
		}
		return rej("a pinned agent's workload identity is %s/%s, not %s", RunIdentityNamespace, d.Name, shown)
	}
	if _, bundled := reserved[name]; bundled {
		return rej("%s is a bundled platform definition; a pin cannot replace it", pyrepr.Str(name))
	}
	if err := policy.Check(d); err != nil {
		return rej("%s", err.Error())
	}
	return &Loaded{Def: d, Source: src, Owner: *d.Owner, Doc: d.Doc}, nil
}
