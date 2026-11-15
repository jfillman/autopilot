package readapi

import (
	"encoding/json"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// DefinitionView is _definition_view: a definition as the API shows it, with the hold on its triggers and where it came from.
func DefinitionView(d *definitions.Definition, full bool, hold Record, src *definitions.Source) Record {
	l := d.Limits
	repos := d.ReposAllow
	if repos == nil {
		repos = []string{}
	}
	tools, models := l.Tools, l.Models
	if tools == nil {
		tools = []string{}
	}
	if models == nil {
		models = []string{}
	}
	triggers := any([]any{map[string]any{"type": "manual"}})
	if t, ok := d.Doc["triggers"]; ok {
		triggers = t
	}
	scopes := map[string]any{}
	if len(d.RawProfile) > 0 {
		var p map[string]any
		dec := json.NewDecoder(jsonReader(d.RawProfile))
		dec.UseNumber()
		if dec.Decode(&p) == nil {
			for _, k := range []string{"glidepath", "airframe"} {
				if v, ok := p[k]; ok {
					scopes[k] = v
				}
			}
		}
	}
	var source any
	if src != nil {
		source = src.View()
	}
	var h any
	if hold != nil {
		h = hold
	}
	out := Record{"name": d.Name, "description": nilIfEmpty(d.Description), "kind": d.Kind, "runsIn": d.RunsIn(), "owner": strPtr(d.Owner),
		"identity":    map[string]any{"type": d.IdentityType, "serviceAccount": strPtr(d.ServiceAccount)},
		"tierCeiling": l.TierCeiling, "tools": tools, "models": models,
		"limits": map[string]any{"ttl_minutes": l.TTLMinutes, "tool_calls": l.ToolCalls, "github_calls": l.GithubCalls, "open_prs": l.OpenPRs,
			"model_tokens": l.ModelTokens, "children": l.Children},
		"network": l.Network, "compute": l.Compute, "repos": map[string]any{"allow": repos}, "triggers": triggers,
		"runtime": map[string]any{"framework": d.Framework, "sandbox": d.Sandbox, "image": strPtr(d.Image)},
		"scopes":  scopes, "hold": h, "source": source}
	if full {
		out["instructions"] = strPtr(d.Instructions)
	}
	return out
}
