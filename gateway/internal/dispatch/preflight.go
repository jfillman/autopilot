package dispatch

// Preflight candidate runs (A-6): a PR's candidate definition runs as a task under a clamped envelope, so its behaviour is
// scored before it is merged. The parts that live in the gateway's dispatch: the sandboxed definition (Gateway._candidate),
// the run a person starts (Gateway.run_agent, from the person's own session on) and the bookkeeping of candidates. What is
// not here: the person-facing routes (who may run what, holds), the cases and the scoring, which are a later slice's.
//
// A candidate document comes from a pull request, so it is untrusted input to the definition loader. This gateway does not
// validate definitions against the JSON schema yet (a later slice), so Candidate refuses to build one until
// CandidateParse, the validated parse, is supplied.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// PreflightCap is the envelope a candidate run is clamped to; a budget in the request can only lower it. The candidate
// runs as the person, so it can do no more than they may.
var PreflightCap = map[string]int64{"ttl_minutes": 10, "tool_calls": 60, "model_tokens": 150_000, "children": 3}

// PreflightTier is the highest tier a candidate may use.
const PreflightTier = "T1"

// PreflightKeep is how many candidate runs are remembered for scoring.
const PreflightKeep = 500

type candidateRun struct {
	session, agent, run, fingerprint string
}

// ErrNotCarried is a boundary this slice stops at: the work belongs to a later slice (docs/admin/go-gateway.md).
var ErrNotCarried = errors.New("not carried by the Go gateway yet")

// ClampCandidate is the sandboxed definition a validated candidate runs as: tier at most T1 and only the tools at or under
// it that open no PR and spend no GitHub calls, no GitHub calls or PRs, time, calls, tokens and children capped (PreflightCap;
// budget can only lower them), a task kind with a delegated identity and a manual trigger. It is named
// pf-<agent>-<fingerprint> so it never shadows the live definition (Python: the end of Gateway._candidate).
func (g *Gateway) ClampCandidate(cand *definitions.Definition, agent string, budget map[string]any, fp string) (*definitions.Definition, error) {
	want := func(key string, own int64) int64 {
		w := PreflightCap[key]
		if n, ok := budget[key]; ok {
			if v, isInt := intOf(n); isInt {
				if _, isBool := n.(bool); !isBool && v > 0 {
					w = v
				}
			}
		}
		if own <= 0 {
			return 0
		}
		return max64(1, min64(min64(w, PreflightCap[key]), own))
	}
	lim := cand.Limits
	var keep []string
	for _, t := range lim.Tools {
		if spec, ok := g.Tools.Get(t); ok && int(spec.Tier) <= 1 && !spec.ProposeOnly && spec.GithubCost == 0 {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		keep = []string{}
	}
	out := *cand
	out.Limits.TierCeiling = lowerTier(lim.TierCeiling, PreflightTier)
	out.Limits.Tools = keep
	out.Limits.GithubCalls, out.Limits.OpenPRs = 0, 0
	out.Limits.TTLMinutes = want("ttl_minutes", lim.TTLMinutes)
	out.Limits.ToolCalls = want("tool_calls", lim.ToolCalls)
	out.Limits.ModelTokens = want("model_tokens", lim.ModelTokens)
	out.Limits.Children = want("children", lim.Children)
	name := "pf-" + firstChars(agent, 28) + "-" + firstChars(fp, 8)
	if g.Defs[name] != nil {
		return nil, &ControlError{"R001", name + " is a live definition", "", 409}
	}
	out.Name, out.Kind, out.IdentityType, out.ServiceAccount = name, "task", "delegated", nil
	return &out, nil
}

func lowerTier(a, b string) string {
	if a > b { // "T0" < "T1" < "T2" < "T3"
		return b
	}
	return a
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Candidate is the sandboxed definition a PR's candidate document runs as, and its fingerprint (Gateway._candidate). The
// document is parsed and validated by CandidateParse like any pin, must name `agent`, run in the cluster and pin its
// runtime image by digest (and be on the deployment's image list when it has one); then it is clamped (ClampCandidate).
func (g *Gateway) Candidate(agent string, document any, budget any) (*definitions.Definition, string, error) {
	if g.CandidateParse == nil || g.Fingerprint == nil {
		return nil, "", fmt.Errorf("%w: candidate definitions are validated against the definition schema, which is not carried yet", ErrNotCarried)
	}
	if _, ok := document.(map[string]any); !ok {
		return nil, "", &ControlError{"R008", "document is an agent definition (an object)", "", 400}
	}
	cand, err := g.CandidateParse(document)
	if err != nil {
		return nil, "", &ControlError{"R008", "the candidate definition would not load: " + err.Error(), "", 400}
	}
	if cand.Name != agent {
		return nil, "", &ControlError{"R008", fmt.Sprintf("the document names %q, not %q", cand.Name, agent), "", 400}
	}
	if cand.RunsIn() != "cluster" {
		return nil, "", &ControlError{"R001", agent + " runs in a person's MCP client; Preflight runs in-cluster agents", "", 409}
	}
	if err := g.checkCandidateImage(*cand.Image); err != nil {
		return nil, "", err
	}
	b, _ := budget.(map[string]any)
	fp := g.Fingerprint(cand)
	d, err := g.ClampCandidate(cand, agent, b, fp)
	return d, fp, err
}

// candidateImage is a runtime image a candidate may name: a name, then "@sha256:" and exactly 64 lowercase hex digits at the very
// end. The digest is what makes the image the one that was reviewed; a tag, an upper-case digest, text after the digest and
// a second "@" are not that.
var candidateImage = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// allZeroDigest is the placeholder digest the loader refuses (profile.PLACEHOLDER_DIGEST): it names no image.
var allZeroDigest = "@sha256:" + strings.Repeat("0", 64)

// checkCandidateImage refuses a candidate's runtime image unless it is pinned by a full digest and is on this deployment's
// allow-list. Two differences from Python's _candidate, both deliberate (a candidate document is untrusted input behind a
// route): an empty allow-list refuses every candidate (Python allows any registry), and a glob matches the whole image: the
// bare-prefix fallback Python adds (image.startswith(glob.rstrip("*"))) is gone, so "ghcr.io/team" never admits
// "ghcr.io/team-evil/x" or "ghcr.io/teamx/x", and a glob with no wildcard is an exact image.
func (g *Gateway) checkCandidateImage(image string) error {
	if !candidateImage.MatchString(image) || strings.HasSuffix(image, allZeroDigest) {
		return &ControlError{"R008", "the candidate's runtime image must be pinned by digest (name@sha256: and 64 lowercase hex digits)", "", 400}
	}
	for _, glob := range g.RuntimeImages {
		if glob != "" && fnmatch(glob, image) {
			return nil
		}
	}
	if len(g.RuntimeImages) == 0 {
		return &ControlError{"R008", "this deployment lists no runtime images, so no candidate may run", "", 400}
	}
	return &ControlError{"R008", "the candidate's runtime image is not on this deployment's list", "", 400}
}

// RunAgentAs starts an in-cluster run of a task agent for a person who has already been checked (who may run what, and
// holds, are the person controls' rule, a later slice): a session opened as the person, then Launch with their input as the
// run's task. The session is the person's, so the run acts with their delegated authority inside the definition's envelope.
// Recorded as session.open (via the delegate) and run.launch. fp is the candidate's fingerprint, or "" for a live
// definition, which is d. (Python: Gateway.run_agent from the open on.)
func (g *Gateway) RunAgentAs(ctx context.Context, person, via, agent string, d *definitions.Definition, fp string, runInput *string) (map[string]any, error) {
	if fp != "" {
		g.cmu.Lock()
		if g.candidates == nil {
			g.candidates = map[string]*definitions.Definition{}
		}
		g.candidates[d.Name] = d
		g.cmu.Unlock()
	}
	if d.Kind != "task" || d.IdentityType != "delegated" {
		return nil, &ControlError{"R001", fmt.Sprintf("%s is a %s agent; only task agents are run by a person", agent, d.Kind),
			"Event and scheduled agents start from their triggers.", 409}
	}
	if d.RunsIn() != "cluster" {
		return nil, &ControlError{"R001", agent + " runs in a person's MCP client, not in the cluster",
			"Open a session from your MCP client (session_open), or give the definition a runtime image.", 409}
	}
	text := ""
	if runInput != nil {
		if len([]rune(*runInput)) > MaxRunInput {
			return nil, &ControlError{"R008", fmt.Sprintf("input is a string of at most %d characters", MaxRunInput), "", 400}
		}
		text = *runInput
	}
	instructions := ""
	if d.Instructions != nil {
		instructions = *d.Instructions
	}
	if strings.TrimSpace(text) == "" && instructions == "" {
		return nil, &ControlError{"R008", agent + " has no instructions of its own: say what it should do", "", 400}
	}
	now := g.Now()
	newID := g.Store.NewID()
	req := g.OpenRequest(&Principal{ID: person, Kind: "user", TierCeiling: 1}, d.Name, nil, "", newID, now)
	req.Action.Properties.Run, req.Action.Properties.Via = true, via
	v, err := g.Core.Evaluate(ctx, req)
	var un *core.Unreachable
	if errors.As(err, &un) {
		v, err = core.OutageVerdict(un, nil), nil
	}
	if err != nil {
		g.Store.GiveBack(newID)
		return nil, err
	}
	if !v.Allow || v.Grant == nil {
		g.Store.GiveBack(newID)
		if v.Rule == core.OutageRule || v.Rule == core.RefusedRule {
			return nil, &ControlError{v.Rule, v.Message, v.Hint, 503}
		}
		rule := v.Rule
		if rule == "" {
			rule = "R008"
		}
		return nil, &ControlError{rule, v.Message, v.Hint, 409}
	}
	if v.Grant.Session != newID || g.Store.Get(v.Grant.Session) != nil {
		g.Store.GiveBack(newID)
		return nil, &ControlError{core.RefusedRule, "Clearance's grant was refused", "", 503}
	}
	s := g.Store.Adopt(v.Grant, now, nil, 0)
	g.flush(ctx, s)
	prompt := strings.TrimSpace(text)
	if prompt == "" {
		prompt = "Carry out your instructions."
	}
	task := map[string]any{"prompt": prompt, "started_by": person}
	if instructions != "" {
		task["instructions"] = instructions
	}
	out, lerr := g.Launch(ctx, s.ID, task)
	if lerr != nil {
		return nil, lerr
	}
	if !out.OK {
		rule, _ := out.Decision["rule"].(string)
		if rule == "" {
			rule = "R001"
		}
		msg := "the run was not launched"
		if out.Error != nil {
			msg = *out.Error
		}
		return nil, &ControlError{rule, msg, "", 409}
	}
	s.Lock()
	taskID := s.TaskID
	s.Unlock()
	view := map[string]any{"agent": agent, "session": s.ID, "task_id": taskID, "run": out.Data.(map[string]any)["run"], "by": person}
	if via != "" {
		view["via"] = via
	} else {
		view["via"] = nil
	}
	if fp != "" {
		g.rememberCandidate(candidateRun{session: s.ID, agent: agent, run: d.Name, fingerprint: fp})
		view["preflight"], view["definition"], view["fingerprint"], view["limits"] = true, d.Name, fp, d.Limits
	}
	return view, nil
}

func (g *Gateway) rememberCandidate(c candidateRun) {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	g.candidateRuns = append(g.candidateRuns, c)
	if n := len(g.candidateRuns); n > PreflightKeep {
		g.candidateRuns = g.candidateRuns[n-PreflightKeep:]
	}
	kept := map[string]bool{}
	for _, r := range g.candidateRuns {
		kept[r.run] = true
	}
	for name := range g.candidates {
		if !kept[name] {
			delete(g.candidates, name)
		}
	}
}

// CandidateRun is the Preflight run a session is (the agent it is a candidate of, the candidate's run name and its
// fingerprint), for the scoring route.
func (g *Gateway) CandidateRun(sessionID string) (agent, run, fingerprint string, ok bool) {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	for _, c := range g.candidateRuns {
		if c.session == sessionID {
			return c.agent, c.run, c.fingerprint, true
		}
	}
	return "", "", "", false
}

// fnmatch is fnmatch.fnmatchcase for the two wildcards a runtime image glob uses: * (any run of characters, / included)
// and ? (one). A pattern with a bracket never matches here, which fails closed.
func fnmatch(pattern, s string) bool {
	if strings.Contains(pattern, "[") {
		return false
	}
	p, t := []rune(pattern), []rune(s)
	var match func(i, j int) bool
	match = func(i, j int) bool {
		for i < len(p) {
			switch p[i] {
			case '*':
				for k := j; k <= len(t); k++ {
					if match(i+1, k) {
						return true
					}
				}
				return false
			case '?':
				if j >= len(t) {
					return false
				}
			default:
				if j >= len(t) || t[j] != p[i] {
					return false
				}
			}
			i++
			j++
		}
		return j == len(t)
	}
	return match(0, 0)
}
