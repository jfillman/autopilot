package core

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryOutbox is the outbox in memory: an ephemeral gateway, the conformance control interface and tests.
type MemoryOutbox struct {
	mu   sync.Mutex
	next int64
	rows map[int64]*memRow
	// Now stamps an entry when it is added (nil: time.Now).
	Now func() time.Time
	// ins is held shared by an Add and exclusively by Quiesce.
	ins sync.RWMutex
}

type memRow struct {
	At     time.Time
	Entry  Entry
	Reason string
	State  string
	Err    string
	Seq    *int64
}

// NewMemoryOutbox returns an empty outbox.
func NewMemoryOutbox() *MemoryOutbox { return &MemoryOutbox{rows: map[int64]*memRow{}} }

func (m *MemoryOutbox) Add(_ context.Context, e Entry, reason, errText string) (int64, error) {
	m.ins.RLock()
	defer m.ins.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	st := "pending"
	if reason == "refused" {
		st = "refused"
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	m.rows[m.next] = &memRow{At: now(), Entry: e, Reason: reason, State: st, Err: errText}
	return m.next, nil
}

func (m *MemoryOutbox) Pending(_ context.Context, limit int) ([]OutboxRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for id, r := range m.rows {
		if r.State == "pending" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]OutboxRow, len(ids))
	for i, id := range ids {
		out[i] = OutboxRow{ID: id, Entry: m.rows[id].Entry}
	}
	return out, nil
}

func (m *MemoryOutbox) HasPending(ctx context.Context) (bool, error) {
	rows, err := m.Pending(ctx, 1)
	return len(rows) > 0, err
}

func (m *MemoryOutbox) Done(_ context.Context, id int64, seq *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil {
		r.State, r.Seq = "done", seq
	}
	return nil
}

func (m *MemoryOutbox) Refused(_ context.Context, id int64, errText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil {
		r.State, r.Err = "refused", errText
	}
	return nil
}

// Len counts rows in a state (tests).
func (m *MemoryOutbox) Len(state string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.State == state {
			n++
		}
	}
	return n
}

func (m *MemoryOutbox) Entries(_ context.Context, q OutboxQuery) ([]OutboxRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for id, r := range m.rows {
		if id <= q.AfterID || (q.OutcomesOnly && r.Entry.Op != "outcome") || (q.WithPaths && !CarriesPaths(r.Entry)) || (!q.Since.IsZero() && r.At.Before(q.Since)) {
			continue
		}
		if len(q.States) > 0 {
			ok := false
			for _, s := range q.States {
				ok = ok || s == r.State
			}
			if !ok {
				continue
			}
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if q.Limit > 0 && len(ids) > q.Limit {
		ids = ids[:q.Limit]
	}
	out := make([]OutboxRow, len(ids))
	for i, id := range ids {
		r := m.rows[id]
		out[i] = OutboxRow{ID: id, Entry: r.Entry, State: r.State, At: r.At}
	}
	return out, nil
}

func (m *MemoryOutbox) HighWater(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next, nil
}

// QuiesceTries and QuiesceWait are how Quiesce takes the add lock: a few tries, a short wait apart, never a wait in the lock's queue. A
// waiting exclusive request makes every later shared one (every Add) queue behind it, and an outcome that cannot be queued within its 5 s
// timeout is lost, so Quiesce only ever TRIES for the lock, and gives up (OutboxBusyRule) rather than make an Add wait.
const (
	QuiesceTries = 5
	QuiesceWait  = 30 * time.Millisecond
)

// BusyError is what Quiesce returns when the lock could not be taken.
func BusyError() error {
	return &OutboxBlocked{Rule: OutboxBusyRule, Msg: "the gateway's outbox was busy taking a write, so it is not known that every write of the run reached Clearance",
		Hint: "Score again in a moment."}
}

// Quiesce holds off every Add while fn runs (it tries for the lock QuiesceTries times and returns BusyError when it never gets it).
func (m *MemoryOutbox) Quiesce(ctx context.Context, fn func(OutboxReader) error) error {
	for i := 0; ; i++ {
		if m.ins.TryLock() {
			break
		}
		if i+1 >= QuiesceTries {
			return BusyError()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(QuiesceWait):
		}
	}
	defer m.ins.Unlock()
	return fn(m)
}
