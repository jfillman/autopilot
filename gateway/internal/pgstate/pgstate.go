// Package pgstate is the gateway's own database (GATEWAY_DSN): the same tables, columns and advisory lock as the
// Python gateway (engine/pg.py; schema.sql is generated from it), so either implementation can read the other's rows.
// This slice reads and writes sessions, the outbox and the pinned definitions; the inbox, results, artifacts, events,
// holds and trigger ledger tables are created (the schema is whole) but only the Python gateway uses them yet.
package pgstate

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

//go:embed schema.sql
var schema string

// LockKey is "apg0": one gateway writer per database.
const LockKey = 0x61706730

// Recent is how long closed or expired sessions are kept in memory for the run tree's lookups.
const Recent = 24 * time.Hour

// ErrWriterLocked is returned when another gateway holds the writer lock.
var ErrWriterLocked = errors.New("another gateway is writing to this database")

// State is an open gateway database: a pool, and the writer lock held on a connection of its own.
type State struct {
	Pool *pgxpool.Pool
	lock *pgx.Conn
}

// Open connects, takes the writer lock (failing if another gateway holds it) and creates the schema if missing.
func Open(ctx context.Context, dsn string, maxConns int32) (*State, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_DSN: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	lock, err := pgx.Connect(ctx, dsn)
	if err != nil {
		pool.Close()
		return nil, err
	}
	var got bool
	if err := lock.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(LockKey)).Scan(&got); err != nil {
		pool.Close()
		_ = lock.Close(ctx)
		return nil, err
	}
	if !got {
		pool.Close()
		_ = lock.Close(ctx)
		return nil, ErrWriterLocked
	}
	s := &State{Pool: pool, lock: lock}
	if _, err := pool.Exec(ctx, schema); err != nil {
		s.Close(ctx)
		return nil, fmt.Errorf("schema: %w", err)
	}
	return s, nil
}

// Close releases the lock and the pool.
func (s *State) Close(ctx context.Context) {
	if s.lock != nil {
		_ = s.lock.Close(ctx)
	}
	s.Pool.Close()
}

// Sessions is session.Persistence on gateway_sessions. Concurrent Saves are grouped into one transaction (a group
// commit), so 200 sessions each saving after every call cost a handful of round trips, not 200 commits; every Save
// still returns only once its rows are committed.
type Sessions struct {
	st  *State
	now func() time.Time
	in  chan *saveReq
}

type saveReq struct {
	recs []session.Record
	done chan error
}

// NewSessions starts the group committer; it stops when ctx ends.
func NewSessions(ctx context.Context, st *State) *Sessions {
	s := &Sessions{st: st, now: time.Now, in: make(chan *saveReq, 1024)}
	go s.run(ctx)
	return s
}

// Load reads live and recently ended sessions.
func (s *Sessions) Load(ctx context.Context) ([]session.Record, error) {
	rows, err := s.st.Pool.Query(ctx, "SELECT record FROM gateway_sessions WHERE NOT closed OR expires_at > $1", s.now().Add(-Recent))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Record
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var r session.Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("a stored session does not parse: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Save writes the records durably.
func (s *Sessions) Save(ctx context.Context, recs []session.Record) error {
	req := &saveReq{recs: recs, done: make(chan error, 1)}
	select {
	case s.in <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

const upsert = "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2, $3, $4) " +
	"ON CONFLICT (id) DO UPDATE SET record = EXCLUDED.record, expires_at = EXCLUDED.expires_at, " +
	"closed = EXCLUDED.closed, updated_at = now()"

func (s *Sessions) run(ctx context.Context) {
	for {
		var first *saveReq
		select {
		case first = <-s.in:
		case <-ctx.Done():
			return
		}
		group := []*saveReq{first}
	drain:
		for len(group) < 256 {
			select {
			case r := <-s.in:
				group = append(group, r)
			default:
				break drain
			}
		}
		err := s.commit(ctx, group)
		for _, r := range group {
			r.done <- err
		}
	}
}

func (s *Sessions) commit(ctx context.Context, group []*saveReq) error {
	// The latest record of a session in the group wins (requests arrive in order).
	latest := map[string]session.Record{}
	var order []string
	for _, g := range group {
		for _, r := range g.recs {
			if _, seen := latest[r.ID]; !seen {
				order = append(order, r.ID)
			}
			latest[r.ID] = r
		}
	}
	b := &pgx.Batch{}
	for _, id := range order {
		r := latest[id]
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		exp, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if err != nil {
			return err
		}
		b.Queue(upsert, r.ID, raw, exp, r.Closed)
	}
	tx, err := s.st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	br := tx.SendBatch(ctx, b)
	for range order {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			_ = tx.Rollback(ctx)
			return err
		}
	}
	if err := br.Close(); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// Outbox is core.Outbox on gateway_outbox.
type Outbox struct{ st *State }

// NewOutbox returns the outbox on st.
func NewOutbox(st *State) *Outbox { return &Outbox{st: st} }

var _ core.Outbox = (*Outbox)(nil)

// Add queues an entry.
func (o *Outbox) Add(ctx context.Context, e core.Entry, reason, errText string) (int64, error) {
	body, _ := json.Marshal(e.Body)
	state := "pending"
	if reason == "refused" {
		state = "refused"
	}
	var kind, decision *string
	if e.Kind != "" {
		kind = &e.Kind
	}
	if e.DecisionID != "" {
		decision = &e.DecisionID
	}
	var errp *string
	if errText != "" {
		errp = &errText
	}
	var id int64
	err := o.st.Pool.QueryRow(ctx, "INSERT INTO gateway_outbox (op, kind, decision_id, body, reason, state, error) "+
		"VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id", e.Op, kind, decision, body, reason, state, errp).Scan(&id)
	return id, err
}

// Pending lists pending entries oldest first.
func (o *Outbox) Pending(ctx context.Context, limit int) ([]core.OutboxRow, error) {
	rows, err := o.st.Pool.Query(ctx, "SELECT id, op, kind, decision_id, body FROM gateway_outbox WHERE state = 'pending' ORDER BY id LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.OutboxRow
	for rows.Next() {
		var id int64
		var op string
		var kind, decision *string
		var body []byte
		if err := rows.Scan(&id, &op, &kind, &decision, &body); err != nil {
			return nil, err
		}
		e := core.Entry{Op: op}
		if kind != nil {
			e.Kind = *kind
		}
		if decision != nil {
			e.DecisionID = *decision
		}
		if err := json.Unmarshal(body, &e.Body); err != nil {
			return nil, err
		}
		out = append(out, core.OutboxRow{ID: id, Entry: e})
	}
	return out, rows.Err()
}

// HasPending reports whether anything waits.
func (o *Outbox) HasPending(ctx context.Context) (bool, error) {
	var b bool
	err := o.st.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM gateway_outbox WHERE state = 'pending')").Scan(&b)
	return b, err
}

// Done marks an entry posted.
func (o *Outbox) Done(ctx context.Context, id int64, seq *int64) error {
	_, err := o.st.Pool.Exec(ctx, "UPDATE gateway_outbox SET state = 'done', seq = $1, posted_at = now() WHERE id = $2", seq, id)
	return err
}

// Refused marks an entry the core refused.
func (o *Outbox) Refused(ctx context.Context, id int64, errText string) error {
	_, err := o.st.Pool.Exec(ctx, "UPDATE gateway_outbox SET state = 'refused', error = $1 WHERE id = $2", errText, id)
	return err
}

// Definitions reads the cluster-pinned definition set the Python gateway's loader keeps (gateway_definitions).
func (s *State) Definitions(ctx context.Context) ([]json.RawMessage, error) {
	rows, err := s.Pool.Query(ctx, "SELECT record FROM gateway_definitions ORDER BY state, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
