package core

import (
	"context"
	"sort"
	"sync"
)

// MemoryOutbox is the outbox in memory: an ephemeral gateway, the conformance control interface and tests.
type MemoryOutbox struct {
	mu   sync.Mutex
	next int64
	rows map[int64]*memRow
}

type memRow struct {
	Entry  Entry
	Reason string
	State  string
	Err    string
	Seq    *int64
}

// NewMemoryOutbox returns an empty outbox.
func NewMemoryOutbox() *MemoryOutbox { return &MemoryOutbox{rows: map[int64]*memRow{}} }

func (m *MemoryOutbox) Add(_ context.Context, e Entry, reason, errText string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	st := "pending"
	if reason == "refused" {
		st = "refused"
	}
	m.rows[m.next] = &memRow{Entry: e, Reason: reason, State: st, Err: errText}
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
