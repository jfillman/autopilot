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
	// OutboxBusyRule is a score that could not take the outbox's add lock (an add was under way for the whole of a few short tries); repeat it.
	OutboxBusyRule = "CORE-OUTBOX-BUSY"
	// OutcomeLostRule is a score refused because an outcome that carries paths could be neither posted nor queued while the run could have been
	// making it: it is in neither Clearance nor the outbox, and nothing durable says so.
	OutcomeLostRule = "CORE-OUTCOME-LOST"
)

// LostStore keeps the lost-outcome marker durably (Postgres: pgstate.LostMarker), so a restart does not forget it.
type LostStore interface {
	// Mark records a loss at the time (never moves an earlier one past a later one).
	Mark(ctx context.Context, at time.Time) error
	// Load reads the time of the last loss (zero: none).
	Load(ctx context.Context) (time.Time, error)
	// Clear removes the marker.
	Clear(ctx context.Context) error
}

// UseLostStore makes s the durable home of the lost-outcome marker and reads it back (call it once, at start, before the core serves): a
// marker that an earlier process left is in force again. A store that cannot be read is an error: starting without it would forget a loss.
func (c *Core) UseLostStore(ctx context.Context, s LostStore) error {
	at, err := s.Load(ctx)
	if err != nil {
		return fmt.Errorf("the lost-outcome marker: %w", err)
	}
	c.lostMu.Lock()
	c.lostStore = s
	if at.After(c.lostAt) {
		c.lostAt = at
	}
	c.lostMu.Unlock()
	return nil
}

// ClearLost forgets the lost-outcome marker, in memory and in its store: what a person does once the lost write has been looked at. A store
// that cannot be written leaves the marker where it is, and says so.
func (c *Core) ClearLost(ctx context.Context) error {
	c.lostMu.Lock()
	defer c.lostMu.Unlock()
	if c.lostStore != nil {
		if err := c.lostStore.Clear(ctx); err != nil {
			return err
		}
	}
	c.lostAt = time.Time{}
	return nil
}

// noteLost records that an outcome with paths was lost (not posted, and not queued: the outbox could not be written either). It is held in
// memory first, so it is in force at once, and then written to the lost-outcome store (a bounded context of its own, not the caller's). The
// failure that loses an outcome is usually the outbox's store being unavailable, which may be the marker's store too: that write can fail,
// and is logged; the marker then holds until a restart, as before the store existed. When it succeeds, a restart keeps it. Until cleared (or until
// every run that could have made the outcome is older than it), a score of any run that began before it is refused.
func (c *Core) noteLost(at time.Time) {
	c.lostMu.Lock()
	if at.After(c.lostAt) {
		c.lostAt = at
	}
	s := c.lostStore
	c.lostMu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), outboxAddTimeout)
	defer cancel()
	if err := s.Mark(ctx, at); err != nil {
		c.Log.Error("the lost-outcome marker could not be stored: a restart forgets it", "err", err.Error())
	}
}

// lostSince says whether an outcome with paths was lost at or after t (zero t: ever).
func (c *Core) lostSince(t time.Time) bool {
	c.lostMu.Lock()
	defer c.lostMu.Unlock()
	return !c.lostAt.IsZero() && !c.lostAt.Before(t)
}

// OutboxBlocked is a score that cannot be trusted because of what the outbox holds. Rule is OutboxPendingRule or OutcomeRefusedRule.
type OutboxBlocked struct{ Rule, Msg, Hint string }

func (e *OutboxBlocked) Error() string { return e.Msg }

// CarriesPaths says whether an outbox entry is an outcome whose effect names paths (or has an effect this code cannot read, which is not
// known to be free of them).
func CarriesPaths(e Entry) bool {
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
// outbox's entries that have already been delivered. The guard (BeginEffectCheck before the first read, Finish after the last, one pair for the
// whole score) therefore refuses a score when a write that carries paths is, at any point of it, in neither place:
//
//   - before the read: an entry still pending (not in Clearance yet), or refused (the core will never take it), blocks. A refused entry
//     blocks only scores of runs that started at or after it was queued (an outcome cannot predate its session), so one old refusal does not
//     hold back every later run; a person still has to look at it (the hint says so). A pending entry blocks every score: it names its
//     decision, not its session. The check names only such entries in its queries, so no number of old or irrelevant ones counts for anything.
//   - across the read: the outbox's greatest id is taken before anything else, and Finish blocks on any outcome with paths added after it,
//     in any state: one queued after the first check, and one queued and then delivered between the read and Finish (in Clearance only
//     after the read, so in neither place for it), are both caught. An entry at or below that id that was not pending or refused at the
//     first check was delivered before the read.
//   - the greatest id is read while the outbox is quiesced (Outbox.Quiesce): no add is under way and none can start, so no entry can be
//     committed with an id at or below the one taken. The hold lasts only for those reads, not for the score: an add outside it waits
//     milliseconds, never the read of Clearance, which matters because a call queues its outcome under a 5 s timeout and a dropped one is lost.
type EffectGuard struct {
	c  *Core
	hw int64
	// lostCut is the cut-off BeginEffectCheck used for the lost-outcome check, which Finish repeats after the last read.
	lostCut time.Time
}

// BeginEffectCheck starts a score's check. since is when the earliest of the scored sessions was created (zero: not known, so every
// refused entry counts). The outbox is quiesced (no entry can be added, and an add under way has committed) while the greatest id is taken
// and the pending and refused entries are read, so no entry can be committed at or below that id afterwards. The queries name only the
// entries that matter (an outcome with paths, pending, or refused since the run could have begun), so entries that do not qualify, however
// many, never count towards anything and never block: the scan stops at the first qualifying entry.
func (c *Core) BeginEffectCheck(ctx context.Context, since time.Time) (*EffectGuard, error) {
	cut := since.Add(-time.Minute)
	if c.lostSince(cut) {
		return nil, &OutboxBlocked{Rule: OutcomeLostRule,
			Msg:  "a write's outcome could be neither delivered nor queued (the outbox could not be written), so it is in neither Clearance nor the outbox and the paths the run touched are not all known",
			Hint: "A person must look at the gateway's logs (outbox errors) and the run's writes; score a new run once the outbox is healthy. The marker is kept in the gateway's database; it stays until cleared."}
	}
	g := &EffectGuard{c: c, lostCut: cut}
	err := c.Outbox.Quiesce(ctx, func(r OutboxReader) error {
		var err error
		if g.hw, err = r.HighWater(ctx); err != nil {
			return err
		}
		pending, err := r.Entries(ctx, OutboxQuery{States: []string{"pending"}, OutcomesOnly: true, WithPaths: true, Limit: 1})
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return &OutboxBlocked{Rule: OutboxPendingRule,
				Msg:  "a write of a run is still queued to reach Clearance, so the paths the run touched are not all known yet",
				Hint: "The gateway delivers it when Clearance is reachable. Score again then."}
		}
		q := OutboxQuery{States: []string{"refused"}, OutcomesOnly: true, WithPaths: true, Limit: 1}
		if !since.IsZero() {
			q.Since = since.Add(-time.Minute) // the database's clock and the gateway's are two clocks
		}
		refused, err := r.Entries(ctx, q)
		if err != nil {
			return err
		}
		if len(refused) > 0 {
			return &OutboxBlocked{Rule: OutcomeRefusedRule,
				Msg:  fmt.Sprintf("the core refused the outcome of a write (outbox entry %d), so its paths are not in Clearance's records and the paths the run touched are not all known", refused[0].ID),
				Hint: "A person must look at it: the gateway's outbox entry (state refused, its error column says why) is a write Clearance never recorded. It blocks scores of runs started after it was queued; fix the cause and score a new run."}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// Finish is the check after the read: no outcome with paths may have been added to the outbox since BeginEffectCheck, whatever became of it.
func (g *EffectGuard) Finish(ctx context.Context) error {
	rows, err := g.c.Outbox.Entries(ctx, OutboxQuery{AfterID: g.hw, OutcomesOnly: true, WithPaths: true, Limit: 1})
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return &OutboxBlocked{Rule: OutboxPendingRule, Msg: "a write of a run was queued to reach Clearance while its paths were being read, so they are not all known",
			Hint: "Score again: the write is in Clearance's records once it has been delivered."}
	}
	// An outcome lost during the read (after BeginEffectCheck's check) is in neither place either. Checked last, after the outbox query, so
	// one lost while that query ran is seen too.
	if g.c.lostSince(g.lostCut) {
		return &OutboxBlocked{Rule: OutcomeLostRule,
			Msg:  "a write's outcome could be neither delivered nor queued while the paths were being read, so the paths the run touched are not all known",
			Hint: "A person must look at the gateway's logs (outbox errors) and the run's writes; score a new run once the outbox is healthy. The marker is kept in the gateway's database; it stays until cleared."}
	}
	return nil
}
