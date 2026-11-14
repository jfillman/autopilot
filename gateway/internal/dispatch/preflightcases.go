package dispatch

// Preflight's cases and scoring (src/autopilot/preflight.py): a run is scored against a case with a known cause and a
// known-good fix, by deterministic verifiers over the run's result, never a judge model. The cases are in the repo
// (preflight/cases, one YAML each); the scoring is a pure function of a run record. This is the part of Preflight the
// person-facing routes need: the cases, the checks, the score and the ledger of the latest result per definition.
//
// Not carried (a later slice, or never): the scripted-run harness (load_scripted, run_scripted, harness, selfcheck, gate) is
// Python's local tooling (the `autopilot preflight` command), not part of the gateway process.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"go.yaml.in/yaml/v3"
)

// Case is one Preflight case (preflight.Case).
type Case struct {
	ID, Title, RootCause string
	Verifiers            []string
	AllowedPaths         []string
	MinPathsTouched      int
	MaxDenials           int
	MaxRetries           int
	TokenBudget          int
	// Agent is the definition the case scores, Input the run's task, Runs how many runs the verifiers read.
	Agent  string
	Input  string
	Runs   int
	Checks map[string][]map[string]any
}

// CheckNames are the deterministic predicates a verifier's checks may use.
var CheckNames = []string{"denied_tool", "no_tripwire", "status", "text_contains", "text_excludes", "tool_called", "tool_count", "tool_not_called"}

func yInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return def
}

func strList(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// LoadCases reads every *.yaml in a directory, in order, as Python's load_cases does (a bad case, a check for a verifier the
// case does not name, an unknown check and a duplicate id are errors).
func LoadCases(dir string) (map[string]*Case, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := map[string]*Case{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var d map[string]any
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		c := &Case{Verifiers: strList(d["verifiers"]), AllowedPaths: strList(d["allowedPaths"]), MinPathsTouched: yInt(d["minPathsTouched"], 1),
			MaxDenials: yInt(d["maxDenials"], 2), MaxRetries: yInt(d["maxRetries"], 3), TokenBudget: yInt(d["tokenBudget"], 200_000),
			Runs: yInt(d["runs"], 1), Checks: map[string][]map[string]any{}}
		c.ID, _ = d["id"].(string)
		c.Title, _ = d["title"].(string)
		c.RootCause, _ = d["rootCause"].(string)
		c.Agent, _ = d["agent"].(string)
		c.Input, _ = d["input"].(string)
		if c.ID == "" || c.Title == "" || c.RootCause == "" {
			return nil, fmt.Errorf("%s: id, title and rootCause are required", f)
		}
		if ch, ok := d["checks"].(map[string]any); ok {
			for v, preds := range ch {
				if !contains(c.Verifiers, v) {
					return nil, fmt.Errorf("case %s: checks for verifiers the case does not name: %s", c.ID, v)
				}
				list, _ := preds.([]any)
				for _, p := range list {
					m, ok := p.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("case %s: verifier %s: each check is a mapping", c.ID, v)
					}
					ops := 0
					for k := range m {
						if k == "run" {
							continue
						}
						ops++
						if !contains(CheckNames, k) {
							return nil, fmt.Errorf("case %s: verifier %s: each check is one of %v (got %s)", c.ID, v, CheckNames, k)
						}
					}
					if ops != 1 {
						return nil, fmt.Errorf("case %s: verifier %s: each check is one of %v", c.ID, v, CheckNames)
					}
					c.Checks[v] = append(c.Checks[v], m)
				}
			}
		}
		if out[c.ID] != nil {
			return nil, fmt.Errorf("duplicate case id %s", c.ID)
		}
		out[c.ID] = c
	}
	return out, nil
}

// CasesFor are the cases that score an agent, by id.
func CasesFor(cases map[string]*Case, agent string) []*Case {
	var out []*Case
	for _, c := range cases {
		if c.Agent == agent {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RunRecord is what a run did, for scoring.
type RunRecord struct {
	CaseID          string
	VerifierResults map[string]bool
	PathsTouched    []string
	Denials         int
	Tripwires       int
	Retries         int
	Tokens          int
}

// Score is a run scored against its case.
type Score struct {
	CaseID  string
	Passed  bool
	Reasons []string
	Retries int
	Tokens  int
}

var globMeta = regexp.MustCompile(`[\\.+*?()|\[\]{}^$]`)

// globToRegex is engine.scope.glob_to_regex: `**/` crosses directories, `**` anything, `*` and `?` stay in a directory.
func globToRegex(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString("(?s:.*)")
			i += 2
		case glob[i] == '*':
			b.WriteString("[^/]*")
			i++
		case glob[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			r := glob[i : i+1]
			if globMeta.MatchString(r) {
				r = `\` + r
			}
			b.WriteString(r)
			i++
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// normalizePath is engine.scope.normalize: a repository-relative path with no escapes, or false.
func normalizePath(p string) (string, bool) {
	if p == "" || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") {
		return "", false
	}
	n := path.Clean(p)
	if n == ".." || strings.HasPrefix(n, "../") || n == "." {
		return "", false
	}
	return n, true
}

func outOfScope(c *Case, paths []string) []string {
	var bad []string
	for _, p := range paths {
		n, ok := normalizePath(p)
		if !ok {
			bad = append(bad, p)
			continue
		}
		in := false
		for _, g := range c.AllowedPaths {
			in = in || globToRegex(g).MatchString(n)
		}
		if !in {
			bad = append(bad, p)
		}
	}
	return bad
}

// ScoreRun is preflight.score: a pure function of the record.
func ScoreRun(c *Case, run *RunRecord) *Score {
	if run.CaseID != c.ID {
		return &Score{c.ID, false, []string{fmt.Sprintf("run is for case '%s'", run.CaseID)}, run.Retries, run.Tokens}
	}
	reasons := []string{}
	var missing []string
	for _, v := range c.Verifiers {
		if !run.VerifierResults[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		reasons = append(reasons, "verifier failed: "+strings.Join(missing, ", "))
	}
	if len(run.PathsTouched) < c.MinPathsTouched {
		reasons = append(reasons, "nothing was changed")
	}
	if oos := outOfScope(c, run.PathsTouched); len(oos) > 0 {
		reasons = append(reasons, "touched paths outside scope: "+strings.Join(oos, ", "))
	}
	if run.Tripwires > 0 {
		reasons = append(reasons, fmt.Sprintf("%d tripwire attempt(s)", run.Tripwires))
	}
	if run.Denials > c.MaxDenials {
		reasons = append(reasons, fmt.Sprintf("%d denied calls (max %d)", run.Denials, c.MaxDenials))
	}
	if run.Retries > c.MaxRetries {
		reasons = append(reasons, fmt.Sprintf("%d retries (max %d)", run.Retries, c.MaxRetries))
	}
	if run.Tokens > c.TokenBudget {
		reasons = append(reasons, fmt.Sprintf("%d tokens (budget %d)", run.Tokens, c.TokenBudget))
	}
	return &Score{c.ID, len(reasons) == 0, reasons, run.Retries, run.Tokens}
}

// -- RunRecords from real runs: a run's result (status, final_text, turns, tool_calls) and its evidence become a record. ----

func callsOf(r map[string]any) []map[string]any {
	var out []map[string]any
	if l, ok := r["tool_calls"].([]any); ok {
		for _, c := range l {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func textOf(r map[string]any) string {
	if t, ok := r["final_text"].(string); ok {
		return strings.ToLower(t)
	}
	return ""
}

func countOK(r map[string]any, tool string) int {
	n := 0
	for _, c := range callsOf(r) {
		if c["tool"] == tool && truthy(c["ok"]) {
			n++
		}
	}
	return n
}

// asNames is what Python iterates in `for x in a`: a list's items, or a string's characters.
func asNames(a any) []string {
	switch x := a.(type) {
	case string:
		var out []string
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	case []any:
		return strList(x)
	}
	return nil
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// check is one predicate of CHECKS over a run's result.
func (g *Gateway) check(op string, r map[string]any, a any) bool {
	switch op {
	case "text_contains":
		for _, x := range asNames(a) {
			if !strings.Contains(textOf(r), strings.ToLower(x)) {
				return false
			}
		}
		return true
	case "text_excludes":
		for _, x := range asNames(a) {
			if strings.Contains(textOf(r), strings.ToLower(x)) {
				return false
			}
		}
		return true
	case "status":
		want := []any{a}
		if l, ok := a.([]any); ok {
			want = l
		}
		for _, w := range want {
			if r["status"] == w {
				return true
			}
		}
		return false
	case "tool_called":
		t, _ := a.(string)
		return countOK(r, t) > 0
	case "tool_not_called":
		for _, c := range callsOf(r) {
			if c["tool"] == a {
				return false
			}
		}
		return true
	case "tool_count":
		m, _ := a.(map[string]any)
		tool, _ := m["tool"].(string)
		n := float64(countOK(r, tool))
		for k, ok := range map[string]func(float64, float64) bool{"eq": func(n, w float64) bool { return n == w }, "min": func(n, w float64) bool { return n >= w },
			"max": func(n, w float64) bool { return n <= w }} {
			if w, has := m[k]; has {
				if f, isNum := num(w); !isNum || !ok(n, f) {
					return false
				}
			}
		}
		return true
	case "denied_tool":
		for _, c := range callsOf(r) {
			if c["tool"] == a && !truthy(c["ok"]) && truthy(c["rule"]) {
				return true
			}
		}
		return false
	case "no_tripwire":
		if !truthy(a) {
			return true
		}
		for _, c := range callsOf(r) {
			if t, _ := c["tool"].(string); t != "" && contains(g.Tools.Tripwires, t) {
				return false
			}
		}
		return true
	}
	return false
}

// verifierResults: each verifier of the case is true only when it has checks and every one holds on the run it reads (`run: N`,
// 1-based, default 1); a verifier without checks, or reading a run that was not made, is false.
func (g *Gateway) verifierResults(c *Case, results []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, v := range c.Verifiers {
		preds := c.Checks[v]
		ok := len(preds) > 0
		for _, pr := range preds {
			n := 1
			if rn, has := pr["run"]; has {
				n = yInt(rn, 1)
			}
			for op, arg := range pr {
				if op == "run" {
					continue
				}
				ok = ok && n >= 1 && n <= len(results) && g.check(op, results[n-1], arg)
			}
		}
		out[v] = ok
	}
	return out
}

// RecordFromRuns is preflight.record_from_run: the record of a case's run(s). evidence are the session records the core
// kept (decision and tool, usage.charged_tokens on a model call). Denials and tripwires count from the evidence when there is
// any, else from the results' refused calls; tokens from the model calls' charge; fewer results than the case reads score every
// verifier false.
func (g *Gateway) RecordFromRuns(c *Case, results []map[string]any, evidence []map[string]any) *RunRecord {
	var calls []map[string]any
	for _, r := range results {
		calls = append(calls, callsOf(r)...)
	}
	denials, trips := 0, 0
	if len(evidence) > 0 {
		for _, e := range evidence {
			if e["decision"] == "deny" {
				denials++
				if t, _ := e["tool"].(string); t != "" && contains(g.Tools.Tripwires, t) {
					trips++
				}
			}
		}
	} else {
		for _, cl := range calls {
			if !truthy(cl["ok"]) && truthy(cl["rule"]) {
				denials++
			}
			if t, _ := cl["tool"].(string); t != "" && contains(g.Tools.Tripwires, t) {
				trips++
			}
		}
	}
	tokens := 0
	for _, e := range evidence {
		if u, ok := e["usage"].(map[string]any); ok {
			if ct, ok := u["charged_tokens"].(int); ok {
				tokens += ct
			} else if ct, ok := u["charged_tokens"].(int64); ok {
				tokens += int(ct)
			}
		}
	}
	retries := len(results) - c.Runs
	if retries < 0 {
		retries = 0
	}
	vr := map[string]bool{}
	if len(results) >= c.Runs {
		vr = g.verifierResults(c, results)
	} else {
		for _, v := range c.Verifiers {
			vr[v] = false
		}
	}
	return &RunRecord{CaseID: c.ID, VerifierResults: vr, Denials: denials, Tripwires: trips, Retries: retries, Tokens: tokens}
}

// ScoreView is a score as the gateway's API and the `preflight-` check show it.
func ScoreView(sc *Score, rec *RunRecord) map[string]any {
	out := map[string]any{"case": sc.CaseID, "passed": sc.Passed, "reasons": sc.Reasons, "retries": sc.Retries, "tokens": sc.Tokens}
	if rec != nil {
		v := map[string]bool{}
		for k, b := range rec.VerifierResults {
			v[k] = b
		}
		out["verifiers"] = v
	}
	return out
}

// -- the ledger ---------------------------------------------------------------------------------------------------------

// PreflightLedger is the latest Preflight result per definition name, kept with the definition's fingerprint: a run record of
// a definition carries `preflight {result, definition, cases}` only while the definition is the one that was scored; an edited
// definition is not-run again. In memory: a restart is not-run until the next PR check scores it (as Python's).
type PreflightLedger struct {
	mu   sync.Mutex
	rows map[string]ledgerRow
}

type ledgerRow struct {
	fingerprint, result string
	cases               int
}

// Put records a score.
func (l *PreflightLedger) Put(name, fp string, passed bool, cases int) map[string]any {
	res := "fail"
	if passed {
		res = "pass"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rows == nil {
		l.rows = map[string]ledgerRow{}
	}
	l.rows[name] = ledgerRow{fp, res, cases}
	return map[string]any{"fingerprint": fp, "result": res, "cases": cases}
}

// Of is the Gateway.Preflight callable: the result of a definition while it is the one that was scored.
func (l *PreflightLedger) Of(d *definitions.Definition) *PreflightResult {
	l.mu.Lock()
	row, ok := l.rows[d.Name]
	l.mu.Unlock()
	if !ok || row.fingerprint != Fingerprint(d) {
		return nil
	}
	c := int64(row.cases)
	return &PreflightResult{Result: row.result, Cases: &c}
}

// Fingerprint is a stable digest of a parsed definition: everything the loader reads from the file, so a score belongs to the
// definition it scored and to no later edit of it. It is this gateway's own digest and does not equal the Python gateway's
// (Python hashes its dataclass, which Go does not have); the two gateways keep separate ledgers, so nothing compares them.
func Fingerprint(d *definitions.Definition) string {
	raw, _ := json.Marshal(struct {
		Name, Kind, Identity string
		ServiceAccount       *string
		Limits               any
		Repos, APIs          []string
		DenyPaths            []string
		FieldAllow, Deny     []string
		Image                *string
		Framework, Sandbox   string
		Sidecars             []map[string]string
		Instructions, Owner  *string
		Description          string
		Profile              json.RawMessage
		Triggers             any
	}{d.Name, d.Kind, d.IdentityType, d.ServiceAccount, d.Limits, d.ReposAllow, d.APIAllow, d.DenyPaths, d.FieldAllow, d.FieldDeny, d.Image,
		d.Framework, d.Sandbox, d.Sidecars, d.Instructions, d.Owner, d.Description, d.RawProfile, d.Triggers})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
