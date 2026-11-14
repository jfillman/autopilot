package pgstate

// The people's side of the gateway database (engine/pg.py PgInbox, PgHolds, PgEvents, PgTriggers): the same tables, columns and
// documents the Python gateway writes, so either implementation reads the other's rows. Each write is its own transaction (the
// Python gateway's join the operation's; the Go gateway saves its sessions separately, see docs/admin/go-gateway.md).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/triggers"
)

// Inbox is dispatch.InboxStore on gateway_human_requests.
type Inbox struct{ st *State }

// NewInbox returns the inbox on st.
func NewInbox(st *State) *Inbox { return &Inbox{st: st} }

var _ dispatch.InboxStore = (*Inbox)(nil)

func loadRequest(raw []byte) (*dispatch.HumanRequest, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return dispatch.HumanRequestFromRecord(m)
}

func (i *Inbox) put(ctx context.Context, r *dispatch.HumanRequest) error {
	raw, err := json.Marshal(r.Record())
	if err != nil {
		return err
	}
	_, err = i.st.Pool.Exec(ctx, "INSERT INTO gateway_human_requests (id, session, status, expires_at, record) VALUES ($1, $2, $3, $4, $5) "+
		"ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, record = EXCLUDED.record, updated_at = now()",
		r.ID, r.Session, r.Status, r.ExpiresAt, raw)
	return err
}

// Add stores a request.
func (i *Inbox) Add(ctx context.Context, r *dispatch.HumanRequest) error { return i.put(ctx, r) }

// Save stores a request.
func (i *Inbox) Save(ctx context.Context, r *dispatch.HumanRequest) error { return i.put(ctx, r) }

// Get is the request, or nil.
func (i *Inbox) Get(ctx context.Context, id string) (*dispatch.HumanRequest, error) {
	var raw []byte
	err := i.st.Pool.QueryRow(ctx, "SELECT record FROM gateway_human_requests WHERE id = $1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return loadRequest(raw)
}

// MaxPendingListed bounds what one listing reads: no person has more questions waiting than this.
const MaxPendingListed = 5000

// Pending lists the pending, unexpired requests oldest first.
func (i *Inbox) Pending(ctx context.Context, now time.Time) ([]*dispatch.HumanRequest, error) {
	rows, err := i.st.Pool.Query(ctx, "SELECT record FROM gateway_human_requests WHERE status = 'pending' AND expires_at > $1 "+
		"ORDER BY record->>'created_at', id LIMIT $2", now, MaxPendingListed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dispatch.HumanRequest
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := loadRequest(raw)
		if err != nil {
			continue // a row that does not parse is not shown (and does not stop the others)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingFor is the pending requests a person may answer: the filter is in the query (on the owner and group fields Python writes),
// so other people's pending requests cannot hide a person's own beyond the limit, which is per person.
func (i *Inbox) PendingFor(ctx context.Context, now time.Time, person string, groups []string) ([]*dispatch.HumanRequest, error) {
	if groups == nil {
		groups = []string{}
	}
	rows, err := i.st.Pool.Query(ctx, "SELECT record FROM gateway_human_requests WHERE status = 'pending' AND expires_at > $1 "+
		"AND (record->>'owner' = $2 OR record->>'group' = ANY($3)) ORDER BY record->>'created_at', id LIMIT $4", now, person, groups, dispatch.MaxListed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dispatch.HumanRequest
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if r, err := loadRequest(raw); err == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// CountPending is how many pending, unexpired requests a session has.
func (i *Inbox) CountPending(ctx context.Context, sessionID string, now time.Time) (int, error) {
	var n int
	err := i.st.Pool.QueryRow(ctx, "SELECT count(*) FROM gateway_human_requests WHERE session = $1 AND status = 'pending' AND expires_at > $2",
		sessionID, now).Scan(&n)
	return n, err
}

// Prune deletes the requests that expired before the cutoff.
func (i *Inbox) Prune(ctx context.Context, before time.Time) (int, error) {
	tag, err := i.st.Pool.Exec(ctx, "DELETE FROM gateway_human_requests WHERE expires_at < $1", before)
	return int(tag.RowsAffected()), err
}

// Holds is dispatch.HoldStore on gateway_holds.
type Holds struct{ st *State }

// NewHolds returns the hold store on st.
func NewHolds(st *State) *Holds { return &Holds{st: st} }

var _ dispatch.HoldStore = (*Holds)(nil)

// Get is the agent's hold, or nil.
func (h *Holds) Get(ctx context.Context, agent string) (*dispatch.Hold, error) {
	var raw []byte
	err := h.st.Pool.QueryRow(ctx, "SELECT record FROM gateway_holds WHERE agent = $1", agent).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return loadHold(raw)
}

func loadHold(raw []byte) (*dispatch.Hold, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return dispatch.HoldFromRecord(m)
}

// Put sets the agent's hold.
func (h *Holds) Put(ctx context.Context, hold *dispatch.Hold) error {
	raw, err := json.Marshal(hold.View())
	if err != nil {
		return err
	}
	_, err = h.st.Pool.Exec(ctx, "INSERT INTO gateway_holds (agent, until, record) VALUES ($1, $2, $3) "+
		"ON CONFLICT (agent) DO UPDATE SET until = EXCLUDED.until, record = EXCLUDED.record, updated_at = now()", hold.Agent, hold.Until, raw)
	return err
}

// Delete removes the agent's hold.
func (h *Holds) Delete(ctx context.Context, agent string) error {
	_, err := h.st.Pool.Exec(ctx, "DELETE FROM gateway_holds WHERE agent = $1", agent)
	return err
}

// All lists every hold.
func (h *Holds) All(ctx context.Context) ([]*dispatch.Hold, error) {
	rows, err := h.st.Pool.Query(ctx, "SELECT record FROM gateway_holds ORDER BY agent")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dispatch.Hold
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if hold, err := loadHold(raw); err == nil {
			out = append(out, hold)
		}
	}
	return out, rows.Err()
}

// Events is dispatch.EventsStore on gateway_events: one row per event the trigger bridge handled, kept dispatch.EventRetention. Every
// PruneEvery writes, rows past retention are deleted.
type Events struct {
	st     *State
	writes int
}

// PruneEvery is how many writes pass between prunes, and MaxEventRows how many rows of history are kept at most.
const (
	PruneEvery   = 500
	MaxEventRows = 200_000
)

// ClampEventTime is a time within [now - retention, now + 5 minutes].
func ClampEventTime(t, now time.Time) time.Time {
	if lo := now.Add(-dispatch.EventRetention); t.Before(lo) {
		return lo
	}
	if hi := now.Add(5 * time.Minute); t.After(hi) {
		return hi
	}
	return t
}

// NewEvents returns the event history on st.
func NewEvents(st *State) *Events { return &Events{st: st} }

var _ dispatch.EventsStore = (*Events)(nil)

// Record stores an event's history, replacing the row of the same id.
func (e *Events) Record(ctx context.Context, ev *dispatch.EventRecord) error {
	raw, err := json.Marshal(ev.View())
	if err != nil {
		return err
	}
	// received_at is the retention key and the order the size cap evicts in; the upstream's value is not trusted for it: a time far in
	// the future would never age out and would push real history out. The stored event keeps the time it carried.
	now := time.Now().UTC()
	key := ClampEventTime(ev.ReceivedAt, now)
	_, err = e.st.Pool.Exec(ctx, "INSERT INTO gateway_events (id, received_at, source, app, routing_key, record) VALUES ($1, $2, $3, $4, $5, $6) "+
		"ON CONFLICT (id) DO UPDATE SET record = EXCLUDED.record, written_at = now()", ev.ID, key, ev.Source, ev.App, ev.RoutingKey, raw)
	if err != nil {
		return err
	}
	if e.writes++; e.writes%PruneEvery == 0 {
		_, _ = e.st.Pool.Exec(ctx, "DELETE FROM gateway_events WHERE received_at < $1", time.Now().UTC().Add(-dispatch.EventRetention))
		// and the table is bounded by size as well as age (Python bounds it by age alone): the newest MaxEventRows stay
		_, _ = e.st.Pool.Exec(ctx, "DELETE FROM gateway_events WHERE id IN (SELECT id FROM gateway_events ORDER BY received_at DESC, id OFFSET $1)", MaxEventRows)
	}
	return nil
}

// Triggers is triggers.Ledger on gateway_trigger_launches and gateway_event_cursors. Each method is its own committed statement, and
// the claim is one INSERT .. ON CONFLICT, so the primary key decides who launches.
type Triggers struct{ st *State }

// NewTriggers returns the trigger ledger on st.
func NewTriggers(st *State) *Triggers { return &Triggers{st: st} }

var _ triggers.Ledger = (*Triggers)(nil)

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Claim is Ledger.Claim.
func (t *Triggers) Claim(ctx context.Context, agent, key, via, eventID string, now, staleBefore time.Time) (bool, error) {
	var one int
	err := t.st.Pool.QueryRow(ctx, "INSERT INTO gateway_trigger_launches (agent, key, via, event_id, state, claimed_at) "+
		"VALUES ($1, $2, $3, $4, 'claimed', $5) ON CONFLICT (agent, key) DO UPDATE SET via = EXCLUDED.via, event_id = EXCLUDED.event_id, "+
		"claimed_at = EXCLUDED.claimed_at, run = NULL, updated_at = now() WHERE gateway_trigger_launches.state = 'claimed' "+
		"AND gateway_trigger_launches.claimed_at <= $6 RETURNING 1", agent, key, via, nullable(eventID), now, staleBefore).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Opened is Ledger.Opened: the session opened for a claimed key, written before its launch.
func (t *Triggers) Opened(ctx context.Context, agent, key, session string) error {
	tag, err := t.st.Pool.Exec(ctx, "UPDATE gateway_trigger_launches SET session = $1, updated_at = now() WHERE agent = $2 AND key = $3 AND state = 'claimed'",
		session, agent, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return triggers.ErrNotClaimed // not claimed any more: nothing was written, and nothing may launch
	}
	return nil
}

// Decide is Ledger.Decide.
func (t *Triggers) Decide(ctx context.Context, agent, key, state, session, run string) error {
	_, err := t.st.Pool.Exec(ctx, "UPDATE gateway_trigger_launches SET state = $1, session = $2, run = $3, updated_at = now() WHERE agent = $4 AND key = $5",
		state, nullable(session), nullable(run), agent, key)
	return err
}

// Get is Ledger.Get.
func (t *Triggers) Get(ctx context.Context, agent, key string) (*triggers.Row, error) {
	r := &triggers.Row{}
	err := t.st.Pool.QueryRow(ctx, "SELECT agent, key, via, event_id, state, session, run, claimed_at FROM gateway_trigger_launches WHERE agent = $1 AND key = $2",
		agent, key).Scan(&r.Agent, &r.Key, &r.Via, &r.EventID, &r.State, &r.Session, &r.Run, &r.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Cursor is Ledger.Cursor.
func (t *Triggers) Cursor(ctx context.Context, source string, now time.Time) (triggers.Cursor, error) {
	if _, err := t.st.Pool.Exec(ctx, "INSERT INTO gateway_event_cursors (source, floor_at) VALUES ($1, $2) ON CONFLICT (source) DO NOTHING", source, now); err != nil {
		return triggers.Cursor{}, err
	}
	c := triggers.Cursor{Source: source}
	err := t.st.Pool.QueryRow(ctx, "SELECT last_id, floor_at FROM gateway_event_cursors WHERE source = $1", source).Scan(&c.LastID, &c.Floor)
	return c, err
}

// Advance is Ledger.Advance: ids order by receive time as plain strings, compared bytewise (COLLATE "C"), never by the locale.
func (t *Triggers) Advance(ctx context.Context, source, lastID string) error {
	_, err := t.st.Pool.Exec(ctx, `UPDATE gateway_event_cursors SET last_id = $1, updated_at = now() WHERE source = $2 `+
		`AND (last_id IS NULL OR last_id COLLATE "C" < $1 COLLATE "C")`, lastID, source)
	return err
}

// Prune is Ledger.Prune.
func (t *Triggers) Prune(ctx context.Context, now time.Time) (int, error) {
	tag, err := t.st.Pool.Exec(ctx, "DELETE FROM gateway_trigger_launches WHERE claimed_at < $1", now.Add(-triggers.Retention))
	return int(tag.RowsAffected()), err
}
