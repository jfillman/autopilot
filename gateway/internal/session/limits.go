package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jfillman/clearance/client"
)

// MaxDepth is how deep a run tree may go (Python: session.MAX_DEPTH).
const MaxDepth = 3

// The orders Python's IntEnums give. Narrowing means going down.
var (
	tiers = map[string]int{"T0": 0, "T1": 1, "T2": 2, "T3": 3}
	// keyed by the label ("clearance+model") and by Network.parse's form ("clearance_model")
	networks = map[string]int{"none": 0, "clearance": 1, "clearance+model": 2, "clearance_model": 2, "allowlist": 3}
	computes = map[string]int{"small": 0, "medium": 1, "large": 2, "gpu": 3}
)

// Rank orders. ok is false for a name the gateway does not know, which is never "narrower than" anything.
func tierRank(s string) (int, bool)    { n, ok := tiers[s]; return n, ok }
func networkRank(s string) (int, bool) { n, ok := networks[s]; return n, ok }
func computeRank(s string) (int, bool) { n, ok := computes[s]; return n, ok }

// Error is a refusal to cut a child session (Python: SessionError, with its code).
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg }

// WideningError lists every field a request tried to push above what it inherited (Python: limits.WideningError).
type WideningError struct{ Fields []string }

func (e *WideningError) Error() string {
	return "requested limits widen: " + strings.Join(e.Fields, ", ")
}

// LimitRequest is what a session or run asks for: every field optional, missing means inherit (Python: LimitRequest).
type LimitRequest struct {
	TierCeiling *string
	TTLMinutes  *int64
	ToolCalls   *int64
	GithubCalls *int64
	OpenPRs     *int64
	ModelTokens *int64
	Children    *int64
	Tools       *[]string
	Models      *[]string
	Network     *string
	Compute     *string
}

// ParseLimitRequest reads a request the way LimitRequest.from_dict does: an unknown field is an error, as is a tier,
// network or compute that is not one of the names.
func ParseLimitRequest(raw json.RawMessage) (LimitRequest, error) {
	var r LimitRequest
	if len(raw) == 0 || string(raw) == "null" {
		return r, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return r, err
	}
	var unknown []string
	for k, v := range m {
		var err error
		switch k {
		case "tier_ceiling":
			r.TierCeiling, err = optString(v)
			if err == nil && r.TierCeiling != nil {
				if _, ok := tierRank(*r.TierCeiling); !ok {
					err = fmt.Errorf("unknown tier %q", *r.TierCeiling)
				}
			}
		case "ttl_minutes":
			r.TTLMinutes, err = optInt(v)
		case "tool_calls":
			r.ToolCalls, err = optInt(v)
		case "github_calls":
			r.GithubCalls, err = optInt(v)
		case "open_prs":
			r.OpenPRs, err = optInt(v)
		case "model_tokens":
			r.ModelTokens, err = optInt(v)
		case "children":
			r.Children, err = optInt(v)
		case "tools":
			r.Tools, err = optList(v)
		case "models":
			r.Models, err = optList(v)
		case "network":
			r.Network, err = optString(v)
			if err == nil && r.Network != nil {
				n := canonicalNetwork(*r.Network)
				if _, ok := networkRank(n); !ok {
					err = fmt.Errorf("unknown network mode %q", *r.Network)
				}
				r.Network = &n
			}
		case "compute":
			r.Compute, err = optString(v)
			if err == nil && r.Compute != nil {
				c := strings.ToLower(*r.Compute)
				if _, ok := computeRank(c); !ok {
					err = fmt.Errorf("unknown compute class %q", *r.Compute)
				}
				r.Compute = &c
			}
		default:
			unknown = append(unknown, k)
		}
		if err != nil {
			return r, fmt.Errorf("%s: %w", k, err)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return r, fmt.Errorf("unknown limit fields: %v", unknown)
	}
	return r, nil
}

// canonicalNetwork is Network.parse then Network.label: "CLEARANCE-MODEL" and "clearance_model" are "clearance+model".
func canonicalNetwork(s string) string {
	return strings.ToLower(strings.NewReplacer("+", "_", "-", "_").Replace(s)) // "clearance_model"
}

func optString(v json.RawMessage) (*string, error) {
	if string(v) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, err
	}
	if s == "" {
		return nil, nil // Python: `if d.get(...)` treats "" as missing
	}
	return &s, nil
}

func optInt(v json.RawMessage) (*int64, error) {
	if string(v) == "null" {
		return nil, nil
	}
	var n int64
	if err := json.Unmarshal(v, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func optList(v json.RawMessage) (*[]string, error) {
	if string(v) == "null" {
		return nil, nil
	}
	var l []string
	if err := json.Unmarshal(v, &l); err != nil {
		return nil, err
	}
	if l == nil {
		l = []string{}
	}
	return &l, nil
}

func setOf(a []string) map[string]bool {
	m := make(map[string]bool, len(a))
	for _, x := range a {
		m[x] = true
	}
	return m
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intersect(a, b []string) []string {
	bs := setOf(b)
	m := map[string]bool{}
	for _, x := range a {
		if bs[x] {
			m[x] = true
		}
	}
	return sortedSet(m)
}

func minInt(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Narrow applies a request to inherited limits. It can only narrow: every field that tried to go above what was
// inherited is named in the WideningError and nothing is applied (Python: limits.narrow).
func Narrow(parent client.Limits, req LimitRequest) (client.Limits, error) {
	out := parent
	var widened []string
	capInt := func(name string, want *int64, inherited int64, set func(int64)) {
		if want == nil {
			return
		}
		if *want > inherited {
			widened = append(widened, name)
		} else {
			set(*want)
		}
	}
	if req.TierCeiling != nil {
		want, _ := tierRank(*req.TierCeiling)
		have, ok := tierRank(parent.TierCeiling)
		if !ok || want > have {
			widened = append(widened, "tier_ceiling")
		} else {
			out.TierCeiling = *req.TierCeiling
		}
	}
	capInt("ttl_minutes", req.TTLMinutes, parent.TTLMinutes, func(n int64) { out.TTLMinutes = n })
	capInt("tool_calls", req.ToolCalls, parent.ToolCalls, func(n int64) { out.ToolCalls = n })
	capInt("github_calls", req.GithubCalls, parent.GithubCalls, func(n int64) { out.GithubCalls = n })
	capInt("open_prs", req.OpenPRs, parent.OpenPRs, func(n int64) { out.OpenPRs = n })
	capInt("model_tokens", req.ModelTokens, parent.ModelTokens, func(n int64) { out.ModelTokens = n })
	capInt("children", req.Children, parent.Children, func(n int64) { out.Children = n })
	if req.Tools != nil {
		if extra := minus(*req.Tools, parent.Tools); len(extra) > 0 {
			widened = append(widened, "tools:"+strings.Join(extra, ","))
		} else {
			out.Tools = sortedSet(setOf(*req.Tools))
		}
	}
	if req.Models != nil {
		if extra := minus(*req.Models, parent.Models); len(extra) > 0 {
			widened = append(widened, "models:"+strings.Join(extra, ","))
		} else {
			out.Models = sortedSet(setOf(*req.Models))
		}
	}
	if req.Network != nil {
		want, _ := networkRank(*req.Network)
		have, ok := networkRank(parent.Network)
		if !ok || want > have {
			widened = append(widened, "network")
		} else {
			out.Network = labelOf(*req.Network)
		}
	}
	if req.Compute != nil {
		want, _ := computeRank(*req.Compute)
		have, ok := computeRank(parent.Compute)
		if !ok || want > have {
			widened = append(widened, "compute")
		} else {
			out.Compute = *req.Compute
		}
	}
	if len(widened) > 0 {
		return parent, &WideningError{Fields: widened}
	}
	return out, nil
}

// labelOf is Network.label for a canonical parse name ("clearance_model" -> "clearance+model").
func labelOf(s string) string { return strings.ReplaceAll(s, "_", "+") }

func minus(a, b []string) []string {
	bs := setOf(b)
	m := map[string]bool{}
	for _, x := range a {
		if !bs[x] {
			m[x] = true
		}
	}
	return sortedSet(m)
}

// Child is a cut child session: its limits, deadline and depth.
type Child struct {
	Limits    client.Limits
	ExpiresAt time.Time
	Depth     int
}

// MinutesLeft is how many whole minutes remain before the deadline (Python: Session.minutes_left). Caller holds the lock.
func (s *Session) MinutesLeft(now time.Time) int64 {
	d := int64(s.ExpiresAt.Sub(now).Seconds()) / 60
	if s.ExpiresAt.Sub(now) < 0 || d < 0 {
		return 0
	}
	return d
}

// ChildBase is what a child may have at most: its definition's limits (base) cut to what the parent has left, field by
// field (Python: SessionStore.open before the request is applied). The child starts from what the parent has left, not
// what it started with. Caller holds the parent's lock.
func ChildBase(parent *Session, base client.Limits, now time.Time) client.Limits {
	out := base
	if a, ok := tierRank(base.TierCeiling); ok {
		if b, ok := tierRank(parent.Limits.TierCeiling); ok && b < a {
			out.TierCeiling = parent.Limits.TierCeiling
		}
	}
	out.TTLMinutes = minInt(base.TTLMinutes, parent.MinutesLeft(now))
	out.ToolCalls = minInt(base.ToolCalls, parent.Remaining("tool_calls"))
	out.GithubCalls = minInt(base.GithubCalls, parent.Remaining("github_calls"))
	out.OpenPRs = minInt(base.OpenPRs, parent.Remaining("open_prs"))
	out.ModelTokens = minInt(base.ModelTokens, parent.Remaining("model_tokens"))
	out.Children = minInt(base.Children, parent.Remaining("children"))
	out.Tools = intersect(base.Tools, parent.Limits.Tools)
	out.Models = intersect(base.Models, parent.Limits.Models)
	if a, ok := networkRank(base.Network); ok {
		if b, ok := networkRank(parent.Limits.Network); ok && b < a {
			out.Network = parent.Limits.Network
		}
	}
	if a, ok := computeRank(base.Compute); ok {
		if b, ok := computeRank(parent.Limits.Compute); ok && b < a {
			out.Compute = parent.Limits.Compute
		}
	}
	return out
}

// CutChild is what the Python SessionStore.open decides for a child: refuse when the parent cannot have one, else the
// limits the definition's limits and the request leave (never wider than the parent has left), the deadline (never
// past the parent's) and the depth. It changes nothing. Caller holds the parent's lock.
func CutChild(parent *Session, base client.Limits, req LimitRequest, now time.Time) (*Child, error) {
	if parent.Expired(now) || parent.Tripped {
		return nil, &Error{"parent-unavailable", "parent is expired or tripped"}
	}
	if parent.Depth+1 > MaxDepth {
		return nil, &Error{"depth", fmt.Sprintf("run tree deeper than %d", MaxDepth)}
	}
	if !parent.CanSpend("children", 1) {
		return nil, &Error{"budget", "children budget exhausted"}
	}
	limits, err := Narrow(ChildBase(parent, base, now), req)
	if err != nil {
		return nil, err
	}
	if limits.TTLMinutes < 1 {
		return nil, &Error{"ttl", "no time left to run"}
	}
	exp := now.Add(time.Duration(limits.TTLMinutes) * time.Minute)
	if parent.ExpiresAt.Before(exp) {
		exp = parent.ExpiresAt
	}
	return &Child{Limits: limits, ExpiresAt: exp.UTC(), Depth: parent.Depth + 1}, nil
}

// Within reports the fields of a grant's limits that are above what the parent could give a child of this definition
// (empty when the grant is no wider). The core cuts the grant; this is the gateway's own, independent check, so a core
// that widened a child (a bug, a bad bundle) cannot widen what the run tree may spend. Caller holds the parent's lock.
func Within(parent *Session, def client.Limits, grant client.Limits, now time.Time) []string {
	max := ChildBase(parent, def, now)
	var over []string
	if a, ok := tierRank(grant.TierCeiling); !ok {
		over = append(over, "tier_ceiling")
	} else if b, _ := tierRank(max.TierCeiling); a > b {
		over = append(over, "tier_ceiling")
	}
	for _, f := range []struct {
		name      string
		got, want int64
	}{{"ttl_minutes", grant.TTLMinutes, max.TTLMinutes}, {"tool_calls", grant.ToolCalls, max.ToolCalls},
		{"github_calls", grant.GithubCalls, max.GithubCalls}, {"open_prs", grant.OpenPRs, max.OpenPRs},
		{"model_tokens", grant.ModelTokens, max.ModelTokens}, {"children", grant.Children, max.Children}} {
		if f.got > f.want || f.got < 0 {
			over = append(over, f.name)
		}
	}
	if extra := minus(grant.Tools, max.Tools); len(extra) > 0 {
		over = append(over, "tools:"+strings.Join(extra, ","))
	}
	if extra := minus(grant.Models, max.Models); len(extra) > 0 {
		over = append(over, "models:"+strings.Join(extra, ","))
	}
	if a, ok := networkRank(grant.Network); !ok {
		over = append(over, "network")
	} else if b, _ := networkRank(max.Network); a > b {
		over = append(over, "network")
	}
	if a, ok := computeRank(grant.Compute); !ok {
		over = append(over, "compute")
	} else if b, _ := computeRank(max.Compute); a > b {
		over = append(over, "compute")
	}
	return over
}
