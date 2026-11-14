package session

import (
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
