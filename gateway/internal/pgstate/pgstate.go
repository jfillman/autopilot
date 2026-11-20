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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// goSchema is what only the Go gateway adds to the shared schema: three columns on the trigger ledger that count the failed attempts of a
// claimed row (see triggers.Row.Failures). They have defaults, so the Python gateway's own statements are unaffected.
const goSchema = `ALTER TABLE gateway_trigger_launches ADD COLUMN IF NOT EXISTS failures integer NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS first_failed_at timestamptz, ADD COLUMN IF NOT EXISTS last_failed_at timestamptz`

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

// Open connects, takes the writer lock (failing if another gateway holds it) and creates the schema if missing. It creates no index (Migrate does).
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
	if _, err := pool.Exec(ctx, goSchema); err != nil {
		s.Close(ctx)
		return nil, fmt.Errorf("schema: %w", err)
	}
	// The read API's indexes are not created here: a plain CREATE INDEX blocks the writes of a table for as long as it builds. They are
	// made by `autopilot-gateway migrate` (Migrate), which the deployment runs before the gateway starts; serve checks them (dbindex).
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
	// The shared advisory lock is held from before the id is taken until the insert commits, so Quiesce (which takes it exclusively) waits for
	// an add under way and holds later ones off: nothing can commit an entry with an id below one Quiesce has seen. Adds do not wait for each
	// other. The lock is the transaction's, released by commit or rollback on every path.
	tx, err := o.st.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1)", outboxLockKey); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, "INSERT INTO gateway_outbox (op, kind, decision_id, body, reason, state, error) "+
		"VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id", e.Op, kind, decision, body, reason, state, errp).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// OutboxLockKey is the advisory lock that orders adds against Quiesce (the digits spell the table).
const OutboxLockKey int64 = 0x676174657761795f

const outboxLockKey = OutboxLockKey

// querier is what a pool and a transaction share for the outbox's reads.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Quiesce runs fn with every Add held off: it takes the add lock exclusively in a transaction of its own and hands fn a reader on that
// transaction (so fn needs no second connection, which a pool all of whose connections wait on the lock could not give it). The lock
// is released when the transaction ends, by the rollback below on every path, including fn's error and the context's end.
//
// It only TRIES for the lock (pg_try_advisory_xact_lock, core.QuiesceTries times, core.QuiesceWait apart) and gives up with core.BusyError.
// A blocking request would queue, and Postgres queues every later shared request (every Add) behind a waiting exclusive one: an Add
// would then wait for the score, and an outcome that cannot be queued within its 5 s timeout is lost. A try never queues, so an Add is
// never delayed by a Quiesce, whether or not it gets the lock. It deadlocks with nothing: an add holds only the shared lock and its own
// insert and waits for nothing Quiesce holds; Drain's updates take no advisory lock.
//
// The argument that nothing can commit below the greatest id read here also needs the id sequence to hand out ids in order, which a
// sequence with CACHE above 1 does not (each connection keeps its own block): it is checked, and a score is refused when it is not.
func (o *Outbox) Quiesce(ctx context.Context, fn func(core.OutboxReader) error) error {
	// READ COMMITTED whatever the database's default is: under REPEATABLE READ the snapshot would be fixed by the first statement, before the lock
	// is taken, and the reads under the lock would not see what an add committed meanwhile.
	tx, err := o.st.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var cache int64
	if err := tx.QueryRow(ctx, "SELECT seqcache FROM pg_sequence WHERE seqrelid = pg_get_serial_sequence('gateway_outbox', 'id')::regclass").Scan(&cache); err != nil {
		return fmt.Errorf("the outbox's id sequence could not be checked: %w", err)
	}
	if cache != 1 {
		return fmt.Errorf("the outbox's id sequence has CACHE %d: its ids are not handed out in order, which the Preflight outbox guard needs (ALTER SEQUENCE gateway_outbox_id_seq CACHE 1)", cache)
	}
	for i := 0; ; i++ {
		var got bool
		if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", outboxLockKey).Scan(&got); err != nil {
			return err
		}
		if got {
			break
		}
		if i+1 >= core.QuiesceTries {
			return core.BusyError()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(core.QuiesceWait):
		}
	}
	return fn(txReader{tx})
}

type txReader struct{ q querier }

func (r txReader) Entries(ctx context.Context, q core.OutboxQuery) ([]core.OutboxRow, error) {
	return entries(ctx, r.q, q)
}

func (r txReader) HighWater(ctx context.Context) (int64, error) { return highWater(ctx, r.q) }

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

// Entries lists entries by a query (core.OutboxQuery), oldest first.
func (o *Outbox) Entries(ctx context.Context, q core.OutboxQuery) ([]core.OutboxRow, error) {
	return entries(ctx, o.st.Pool, q)
}

// withPaths is core.carriesPaths in SQL: an outcome whose effect is not an object, or whose effect names paths (not null, not empty).
const withPaths = "(body->'effect' IS NOT NULL AND jsonb_typeof(body->'effect') <> 'null' AND (jsonb_typeof(body->'effect') <> 'object' OR " +
	"(body #> '{effect,paths}' IS NOT NULL AND jsonb_typeof(body #> '{effect,paths}') <> 'null' AND body #> '{effect,paths}' <> '[]'::jsonb)))"

// entries builds its query from what is asked, with a literal predicate for each (never `$n IS NULL OR ...`, which a generic plan could
// not use an index for): one state is named as a literal so the planner can match the partial indexes on state (gateway_outbox_pending, and
// gateway_outbox_refused, which dbindex.Indexes carries); a time lower bound is a plain comparison on created_at.
func entries(ctx context.Context, db querier, q core.OutboxQuery) ([]core.OutboxRow, error) {
	conds, args := []string{"id > $1"}, []any{q.AfterID}
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	for _, st := range q.States {
		if st != "pending" && st != "done" && st != "refused" {
			return nil, fmt.Errorf("outbox state %q", st)
		}
	}
	switch len(q.States) {
	case 0:
	case 1:
		conds = append(conds, "state = '"+q.States[0]+"'") // a closed set, checked above
	default:
		conds = append(conds, "state = ANY("+arg(q.States)+")")
	}
	if !q.Since.IsZero() {
		conds = append(conds, "created_at >= "+arg(q.Since))
	}
	if q.OutcomesOnly {
		conds = append(conds, "op = 'outcome'")
	}
	if q.WithPaths {
		conds = append(conds, withPaths)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.Query(ctx, "SELECT id, op, kind, decision_id, body, state, created_at FROM gateway_outbox WHERE "+strings.Join(conds, " AND ")+
		" ORDER BY id LIMIT "+arg(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.OutboxRow
	for rows.Next() {
		var id int64
		var op, state string
		var kind, decision *string
		var body []byte
		var at time.Time
		if err := rows.Scan(&id, &op, &kind, &decision, &body, &state, &at); err != nil {
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
		out = append(out, core.OutboxRow{ID: id, Entry: e, State: state, At: at})
	}
	return out, rows.Err()
}

// HighWater is the greatest id ever added (0: none).
func (o *Outbox) HighWater(ctx context.Context) (int64, error) { return highWater(ctx, o.st.Pool) }

func highWater(ctx context.Context, db querier) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, "SELECT COALESCE(MAX(id), 0) FROM gateway_outbox").Scan(&id)
	return id, err
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

// ReplaceDefinitions replaces the cluster-pinned definition set (gateway_definitions) with rows, whole and in one transaction: a
// reader sees the old set or the new one, never a mix, and a failure leaves the old one. A row is {name, state, source, doc | reason}.
// The table's key is (name, state), so of two rejections of one name (two pins of one agent name) the first is kept: the Python
// gateway's insert of both fails on the key.
func (s *State) ReplaceDefinitions(ctx context.Context, rows []map[string]any) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "DELETE FROM gateway_definitions"); err != nil {
		return err
	}
	for _, r := range rows {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		name, _ := r["name"].(string)
		state, _ := r["state"].(string)
		if _, err := tx.Exec(ctx, "INSERT INTO gateway_definitions (name, state, record) VALUES ($1, $2, $3) ON CONFLICT (name, state) DO NOTHING",
			name, state, raw); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
