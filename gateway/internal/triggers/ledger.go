// Package triggers is what starts agents without a person: interval slots (the scheduler), Glidepath events (the event bridge)
// and the reconciliation sweep that recovers what the bridge missed. It is the Go side of scheduler.py, event_bridge.py,
// reconcile.py, triggers.py and engine/trigger_ledger.py.
//
// Triggers fire once (A-5). Every firing has a key. One row per (agent, key) is claimed in the gateway database before anything
// is opened, and the row's state is the decision made for it. A second claim of the same (agent, key) loses, whoever makes it:
// the stream after a restart, the recent-events catch-up, the reconciliation sweep or the scheduler. The one exception is a
// claim left `claimed` (the gateway died between the claim and the decision): it may be claimed again once it is older than Stale
// or than the claiming process itself (one writer holds the database, so a claim from before this process started is an
// orphan), and the session half (dispatch.Gateway.OpenTriggered) answers a still-live run of the same task as a dedupe.
//
// Nothing a trigger source sends picks the principal, the agent, the budget or the tools: the agent is the one whose definition's
// trigger matched, and what the run may do is the core's cut of that definition.
package triggers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Ledger limits and the closed sets of the rows' columns (trigger_ledger.py).
const (
	Stale     = 5 * time.Minute     // a claim with no decision after this may be claimed again
	Retention = 30 * 24 * time.Hour // rows are kept this long
)

// States are the decisions a row can hold. `claimed` is the claim itself.
var States = []string{"claimed", "launched", "deduped", "braked", "held", "refused", "launch-failed", "missed"}

// Vias are the paths a trigger arrives by.
var Vias = []string{"stream", "recent", "reconcile", "schedule", "rabbitmq"}

func oneOf(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// Row is one ledger row.
type Row struct {
	Agent, Key, Via string
	EventID         *string
	State           string
	Session, Run    *string
	ClaimedAt       time.Time
}

// Cursor is the newest event id the bridge has handled for a source, and the floor: when the ledger first ran for that source.
// The reconciliation sweep and the missed-slot check never reach back before it, so turning the ledger on does not launch a day of
// old failures.
type Cursor struct {
	Source string
	LastID *string
	Floor  time.Time
}

// Ledger is the trigger ledger and the cursors (trigger_ledger.TriggerStore). Each call is its own committed transaction.
type Ledger interface {
	// Claim takes (agent, key): true when it is this caller's (no row, or a `claimed` row claimed at or before staleBefore). The
	// primary key decides who launches.
	Claim(ctx context.Context, agent, key, via, eventID string, now, staleBefore time.Time) (bool, error)
	// Decide writes the decision of a claimed row.
	Decide(ctx context.Context, agent, key, state, session, run string) error
	// Get is the row, or nil.
	Get(ctx context.Context, agent, key string) (*Row, error)
	// Cursor is the source's cursor, creating it (floor now) when there is none.
	Cursor(ctx context.Context, source string, now time.Time) (Cursor, error)
	// Advance moves the cursor forward, never backward (ids order by receive time as plain strings, compared bytewise).
	Advance(ctx context.Context, source, lastID string) error
	// Prune deletes the rows older than Retention.
	Prune(ctx context.Context, now time.Time) (int, error)
}

// StaleBefore is when a claim stops being live: those from before this process started, and any older than Stale.
func StaleBefore(started, now time.Time) time.Time {
	if t := now.Add(-Stale); t.After(started) {
		return t
	}
	return started
}

// SlotKey is the ledger key of an interval slot.
func SlotKey(every string, slot int64) string { return fmt.Sprintf("slot:%s:%d", every, slot) }

// MaxKeyLen bounds a ledger key.
const MaxKeyLen = 512

// TriggerKey is the ledger key of a glidepath-api event envelope: `<app>.pipelinerun.<predicate>/<pipelinerun name>` for a
// pipelinerun event (the routing key and the CDEvent's subject id, both documented in glidepath docs/admin/api.md: a finished run
// read back from the run records gives the same key, so the sweep and the stream agree without knowing how the event ids are
// formed), else `event:<id>`; "" without an id. A key over MaxKeyLen is "" too: the event is dropped.
func TriggerKey(env map[string]any) string {
	rk, _ := env["routingKey"].(string)
	app, _ := env["app"].(string)
	eid, _ := env["id"].(string)
	var sid string
	if ev, ok := env["event"].(map[string]any); ok {
		if sub, ok := ev["subject"].(map[string]any); ok {
			sid, _ = sub["id"].(string)
		}
	}
	var key string
	switch {
	case rk != "" && app != "" && strings.HasPrefix(rk, app+".pipelinerun.") && sid != "":
		key = rk + "/" + sid
	case eid != "":
		key = "event:" + eid
	}
	if len(key) > MaxKeyLen {
		return ""
	}
	return key
}

// MemoryLedger is a Ledger in memory (tests).
type MemoryLedger struct {
	mu      sync.Mutex
	rows    map[[2]string]*Row
	cursors map[string]*Cursor
	// FailClaim, FailDecide and FailCursor make the next call of that kind fail once (a database that went away).
	FailClaim, FailDecide, FailAdvance error
}

func (m *MemoryLedger) init() {
	if m.rows == nil {
		m.rows, m.cursors = map[[2]string]*Row{}, map[string]*Cursor{}
	}
}

func sp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Claim is Ledger.Claim.
func (m *MemoryLedger) Claim(_ context.Context, agent, key, via, eventID string, now, staleBefore time.Time) (bool, error) {
	if !oneOf(Vias, via) {
		return false, fmt.Errorf("trigger via %q", via)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if m.FailClaim != nil {
		err := m.FailClaim
		m.FailClaim = nil
		return false, err
	}
	if r := m.rows[[2]string{agent, key}]; r != nil && !(r.State == "claimed" && !r.ClaimedAt.After(staleBefore)) {
		return false, nil
	}
	m.rows[[2]string{agent, key}] = &Row{Agent: agent, Key: key, Via: via, EventID: sp(eventID), State: "claimed", ClaimedAt: now}
	return true, nil
}

// Decide is Ledger.Decide.
func (m *MemoryLedger) Decide(_ context.Context, agent, key, state, session, run string) error {
	if !oneOf(States, state) {
		return fmt.Errorf("trigger state %q", state)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if m.FailDecide != nil {
		err := m.FailDecide
		m.FailDecide = nil
		return err
	}
	if r := m.rows[[2]string{agent, key}]; r != nil {
		r.State, r.Session, r.Run = state, sp(session), sp(run)
	}
	return nil
}

// Get is Ledger.Get.
func (m *MemoryLedger) Get(_ context.Context, agent, key string) (*Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if r := m.rows[[2]string{agent, key}]; r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}

// Cursor is Ledger.Cursor.
func (m *MemoryLedger) Cursor(_ context.Context, source string, now time.Time) (Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	c := m.cursors[source]
	if c == nil {
		c = &Cursor{Source: source, Floor: now}
		m.cursors[source] = c
	}
	return *c, nil
}

// Advance is Ledger.Advance.
func (m *MemoryLedger) Advance(_ context.Context, source, lastID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if m.FailAdvance != nil {
		err := m.FailAdvance
		m.FailAdvance = nil
		return err
	}
	c := m.cursors[source]
	if c == nil {
		return fmt.Errorf("no cursor for %q", source)
	}
	if c.LastID == nil || lastID > *c.LastID {
		c.LastID = &lastID
	}
	return nil
}

// Prune is Ledger.Prune.
func (m *MemoryLedger) Prune(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	n := 0
	for k, r := range m.rows {
		if now.Sub(r.ClaimedAt) > Retention {
			delete(m.rows, k)
			n++
		}
	}
	return n, nil
}

// Rows is a copy of every row, for a test.
func (m *MemoryLedger) Rows() []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for _, r := range m.rows {
		out = append(out, *r)
	}
	return out
}
