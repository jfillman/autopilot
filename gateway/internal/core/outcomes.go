package core

// A session's effect paths, read back from Clearance (A-10 W-16). Preflight scores a run on the paths its writes touched
// (min paths touched, the out-of-scope check). The gateway keeps no state of its own for them: the core already holds, for every
// allowed write, an outcome record carrying the effect (`effect.paths`, set from the call's files by dispatch.effect), so the
// score reads them from there, at scoring time, from clearance-api (GET /v1/evidence?session=&outcomes=only, Clearance's
// contract operation listEvidence). The core itself serves no reads.
//
// The read is made with the caller's own credentials: the bearer token the person (or the delegate naming one) presented to the
// gateway, and for a delegate the on-behalf-of header, so clearance-api applies its own per-person scoping (a person sees their
// runs, an agent's owner group its runs). Nothing is read with a wider identity than the one that asked for the score.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// OutcomePageSize is the page asked for: clearance-api's MaxLimit, so a run's few outcomes are one request.
	OutcomePageSize = 200
	// OutcomeMaxPages bounds the pages read for one session (10 000 outcome records): more than that is "unknown", never a partial list.
	OutcomeMaxPages = 50
	// OutcomeTimeout is one request's timeout.
	OutcomeTimeout = 10 * time.Second
	maxOutcomeBody = 8 << 20
	// OnBehalfOfHeader is the header a delegate names a person in (Hangar's: clearance-api's CLEARANCE_ON_BEHALF_OF_HEADER).
	OnBehalfOfHeader = "X-Hangar-On-Behalf-Of"
)

// OutcomeReader reads a session's outcome records from clearance-api.
type OutcomeReader struct {
	BaseURL string       // CLEARANCE_API_URL
	HTTP    *http.Client // nil: a client with OutcomeTimeout that follows no redirect
	Header  string       // the on-behalf-of header; empty is OnBehalfOfHeader
}

// PathsUnknown is the paths a session touched that could not be read: the score must not treat them as none.
// Refused is true when clearance-api answered and refused (a 4xx, a body that is not an evidence page), false when it could not
// be reached or failed (the outage side); Rule says which, as the core client's own errors do.
//
// NotVisible is true when clearance-api answered but shows the caller none of the session's records at all: it scopes evidence per
// viewer and drops what the caller may not see without saying so, so an empty list there is "nothing visible", not "nothing written".
type PathsUnknown struct {
	Msg        string
	Refused    bool
	NotVisible bool
}

func (e *PathsUnknown) Error() string { return e.Msg }

// NotVisibleRule is the rule for a session none of whose records the caller may read (clearance-api's per-viewer scope).
const NotVisibleRule = "CORE-NOT-VISIBLE"

// Rule is OutageRule, RefusedRule or NotVisibleRule.
func (e *PathsUnknown) Rule() string {
	if e.NotVisible {
		return NotVisibleRule
	}
	if e.Refused {
		return RefusedRule
	}
	return OutageRule
}

func unknown(refused bool, f string, a ...any) error {
	return &PathsUnknown{Msg: fmt.Sprintf(f, a...), Refused: refused}
}

func (r *OutcomeReader) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: OutcomeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// EffectPaths is the distinct paths the effects of a session's outcome records name, sorted. No effects is an empty list and a nil
// error; anything that leaves the list uncertain (no API configured, unreachable, refused, an unreadable page, more pages than
// OutcomeMaxPages, a cursor that does not advance) is a *PathsUnknown.
func (r *OutcomeReader) EffectPaths(ctx context.Context, session, token, onBehalfOf string) ([]string, error) {
	if r == nil || r.BaseURL == "" {
		return nil, unknown(true, "no Clearance API is configured (CLEARANCE_API_URL), so the paths the run touched cannot be read")
	}
	if session == "" {
		return nil, unknown(true, "no session to read the outcome records of")
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") || strings.ContainsAny(onBehalfOf, "\r\n") {
		return nil, unknown(true, "no usable credentials to read the outcome records with")
	}
	hdr := r.Header
	if hdr == "" {
		hdr = OnBehalfOfHeader
	}
	// Visible first: clearance-api drops the records the caller may not see and still answers 200, so a session whose outcome records
	// are hidden would read as "no effects". The session's own non-outcome records (its session.open at least) exist for every
	// session the gateway opened; if none is visible, nothing about the session is.
	probe, err := r.fetch(ctx, url.Values{"session": {session}, "outcomes": {"exclude"}, "limit": {"1"}}, token, onBehalfOf, hdr)
	if err != nil {
		return nil, err
	}
	var seenAny struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(probe, &seenAny) != nil || seenAny.Items == nil {
		return nil, unknown(true, "the Clearance API's answer is not an evidence page")
	}
	if len(seenAny.Items) == 0 {
		return nil, &PathsUnknown{Msg: "none of the session's records are visible to the caller, so the paths its writes touched cannot be known (clearance-api shows a person the runs they started)", NotVisible: true}
	}
	seen := map[string]bool{}
	after := ""
	for page := 0; ; page++ {
		if page >= OutcomeMaxPages {
			return nil, unknown(false, "the session has more than %d outcome records: its paths are not known", OutcomePageSize*OutcomeMaxPages)
		}
		q := url.Values{"session": {session}, "outcomes": {"only"}, "limit": {strconv.Itoa(OutcomePageSize)}}
		if after != "" {
			q.Set("after", after)
		}
		raw, err := r.fetch(ctx, q, token, onBehalfOf, hdr)
		if err != nil {
			return nil, err
		}
		next, err := collectPaths(raw, session, seen)
		if err != nil {
			return nil, unknown(true, "%v", err)
		}
		if next == "" {
			break
		}
		// clearance-api lists newest first (ORDER BY seq DESC, seq < after): the cursor is the last, smallest seq of a full page and
		// must go down on every page. One that does not would loop for ever.
		if after != "" {
			a, _ := strconv.ParseInt(after, 10, 64)
			if n, _ := strconv.ParseInt(next, 10, 64); n >= a {
				return nil, unknown(true, "the Clearance API's cursor did not decrease")
			}
		}
		after = next
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// fetch is one GET /v1/evidence and its items (as the raw page for the outcome read; the probe only needs to know if there are any).
func (r *OutcomeReader) fetch(ctx context.Context, q url.Values, token, onBehalfOf, hdr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.BaseURL, "/")+"/v1/evidence?"+q.Encode(), nil)
	if err != nil {
		return nil, unknown(true, "the Clearance API address is not usable: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if onBehalfOf != "" {
		req.Header.Set(hdr, onBehalfOf)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, unknown(false, "reading the outcome records ran out of time or was cancelled: %v", ctx.Err())
		}
		return nil, unknown(false, "the Clearance API could not be reached")
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxOutcomeBody+1))
	resp.Body.Close()
	switch {
	case rerr != nil:
		return nil, unknown(false, "the Clearance API's answer was cut off")
	case resp.StatusCode >= 500:
		return nil, unknown(false, "the Clearance API answered %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, unknown(true, "the Clearance API refused the read: %d", resp.StatusCode)
	case len(raw) > maxOutcomeBody:
		return nil, unknown(true, "the Clearance API's answer is over %d bytes", maxOutcomeBody)
	}
	return raw, nil
}

// collectPaths adds a page's effect paths to seen and returns the cursor of the next page ("" for the last). A page is
// {"items": [record, ...], "next": seq|null} (clearance-api's EvidencePage); a record's effect is {repo, pr, commit, branch,
// paths: [...]} (clearance client.Effect). A record of another session is not the session's.
func collectPaths(raw []byte, session string, seen map[string]bool) (string, error) {
	var page struct {
		Items []struct {
			Session string `json:"session"`
			Effect  *struct {
				Paths []json.RawMessage `json:"paths"`
			} `json:"effect"`
		} `json:"items"`
		Next json.RawMessage `json:"next"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.Items == nil {
		return "", errors.New("the Clearance API's answer is not an evidence page")
	}
	for _, it := range page.Items {
		if it.Session != "" && it.Session != session {
			continue
		}
		if it.Effect == nil {
			continue
		}
		for _, p := range it.Effect.Paths {
			var s string
			if err := json.Unmarshal(p, &s); err != nil || s == "" {
				return "", errors.New("an outcome record has a path that is not a string")
			}
			seen[s] = true
		}
	}
	n := strings.TrimSpace(string(page.Next))
	if n == "" || n == "null" {
		return "", nil
	}
	if _, err := strconv.ParseInt(n, 10, 64); err != nil {
		return "", errors.New("the Clearance API's cursor is not a seq")
	}
	return n, nil
}

// OutboxPendingRule is the rule for a score refused while a write of a run (an outcome that carries an effect with paths) is queued in the
// gateway's outbox or reached Clearance while the score was being made, and OutcomeRefusedRule for one the core refused, which never will.
const (
	OutboxPendingRule  = "CORE-OUTBOX-PENDING"
	OutcomeRefusedRule = "CORE-OUTCOME-REFUSED"
)

// OutboxBlocked is a score that cannot be trusted because of what the outbox holds. Rule is OutboxPendingRule or OutcomeRefusedRule.
type OutboxBlocked struct{ Rule, Msg, Hint string }

func (e *OutboxBlocked) Error() string { return e.Msg }

// pendingScan is how many outbox entries one check looks at; more than that cannot be told and blocks.
const pendingScan = 1000

// carriesPaths says whether an outbox entry is an outcome whose effect names paths (or has an effect this code cannot read, which is not
// known to be free of them).
func carriesPaths(e Entry) bool {
	if e.Op != "outcome" {
		return false
	}
	eff, has := e.Body["effect"]
	if !has || eff == nil {
		return false
	}
	m, ok := eff.(map[string]any)
	if !ok {
		return true
	}
	switch ps := m["paths"].(type) {
	case nil:
		return false
	case []any:
		return len(ps) > 0
	case []string:
		return len(ps) > 0
	}
	return true
}

// EffectGuard closes the gap between the gateway's outbox and Clearance's records for one score. A write of a run reaches Clearance either
// straight from the call or, when the core was unreachable, later from the outbox; a read of Clearance sees only the first kind and the
// outbox's entries that have already been delivered. The guard (BeginEffectCheck before the read, Finish after it) therefore refuses a score
// when a write that carries paths is, at any point of it, in neither place:
//
//   - before the read: an entry still pending (not in Clearance yet), or refused (the core will never take it), blocks. A refused entry
//     blocks only scores of runs that started at or after it was queued (an outcome cannot predate its session), so one old refusal does not
//     hold back every later run; a person still has to look at it (the hint says so). A pending entry blocks every score: it names its
//     decision, not its session.
//   - across the read: the outbox's greatest id is taken before anything else, and Finish blocks on any outcome with paths added after it,
//     in any state: one queued after the first check, and one queued and then delivered between the read and Finish (in Clearance only
//     after the read, so in neither place for it), are both caught. An entry at or below that id that was not pending or refused at the
//     first check was delivered before the read.
type EffectGuard struct {
	c  *Core
	hw int64
}

// BeginEffectCheck starts a score's check. since is when the earliest of the scored sessions was created (zero: not known, so every
// refused entry counts).
func (c *Core) BeginEffectCheck(ctx context.Context, since time.Time) (*EffectGuard, error) {
	hw, err := c.Outbox.HighWater(ctx)
	if err != nil {
		return nil, err
	}
	if !since.IsZero() {
		since = since.Add(-time.Minute) // the database's clock and the gateway's are two clocks
	}
	rows, err := c.Outbox.Entries(ctx, OutboxQuery{States: []string{"pending", "refused"}, OutcomesOnly: true, Limit: pendingScan})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if !carriesPaths(r.Entry) {
			continue
		}
		if r.State == "refused" && !since.IsZero() && r.At.Before(since) {
			continue
		}
		if r.State == "refused" {
			return nil, &OutboxBlocked{Rule: OutcomeRefusedRule,
				Msg:  fmt.Sprintf("the core refused the outcome of a write (outbox entry %d), so its paths are not in Clearance's records and the paths the run touched are not all known", r.ID),
				Hint: "A person must look at it: the gateway's outbox entry (state refused, its error column says why) is a write Clearance never recorded. It blocks scores of runs started after it was queued; fix the cause and score a new run."}
		}
		return nil, &OutboxBlocked{Rule: OutboxPendingRule,
			Msg:  "a write of a run is still queued to reach Clearance, so the paths the run touched are not all known yet",
			Hint: "The gateway delivers it when Clearance is reachable. Score again then."}
	}
	if len(rows) >= pendingScan {
		return nil, &OutboxBlocked{Rule: OutboxPendingRule, Msg: "too many entries wait in the outbox to tell whether a write of a run is among them", Hint: "Score again once the outbox has drained."}
	}
	return &EffectGuard{c: c, hw: hw}, nil
}

// Finish is the check after the read: no outcome with paths may have been added to the outbox since BeginEffectCheck, whatever became of it.
func (g *EffectGuard) Finish(ctx context.Context) error {
	rows, err := g.c.Outbox.Entries(ctx, OutboxQuery{AfterID: g.hw, OutcomesOnly: true, Limit: pendingScan})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if carriesPaths(r.Entry) {
			return &OutboxBlocked{Rule: OutboxPendingRule, Msg: "a write of a run was queued to reach Clearance while its paths were being read, so they are not all known",
				Hint: "Score again: the write is in Clearance's records once it has been delivered."}
		}
	}
	if len(rows) >= pendingScan {
		return &OutboxBlocked{Rule: OutboxPendingRule, Msg: "too many entries were queued while the paths were being read", Hint: "Score again once the outbox has drained."}
	}
	return nil
}
