// Package dbindex names the seven indexes only the Go gateway needs (six serve the read API's queries, one the Preflight outbox guard) and checks that they
// exist and are valid. It writes nothing: the process that creates them is `autopilot-gateway migrate` (pgstate.Migrate), and the
// processes that need them (`serve`, `api`) only ask. The read API imports this package, never pgstate.
package dbindex

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Index is one index: its name, its table, and what follows `ON <table>` in its definition.
type Index struct{ Name, Table, Def string }

// Indexes are what the read API's queries (readapi.PgStore) need and the shared schema does not have: expression indexes on the session
// record's fields, so no list of sessions or run tree scans the history, and a GIN index on an event's outcomes, so an agent's events are
// found without reading every event. The text is ordered and compared in the C collation (a locale's ignores punctuation and misorders
// an ISO time with a fraction against one without), and every index ends in the id, so a cursor of (time, id) is a row comparison the
// index answers. Go only (Python's schema is the shared one).
var Indexes = []Index{
	{"gateway_sessions_created", "gateway_sessions", `(((record->>'created_at') COLLATE "C") DESC, (id COLLATE "C") DESC)`},
	{"gateway_sessions_principal", "gateway_sessions", `((record->>'principal'), ((record->>'created_at') COLLATE "C") DESC, (id COLLATE "C") DESC)`},
	{"gateway_sessions_agent", "gateway_sessions", `((record->>'agent'), ((record->>'created_at') COLLATE "C") DESC, (id COLLATE "C") DESC)`},
	{"gateway_sessions_task", "gateway_sessions", `((record->>'task_id'), ((record->>'created_at') COLLATE "C"), (id COLLATE "C"))`},
	{"gateway_events_app_id", "gateway_events", `(app, received_at DESC, (id COLLATE "C") DESC)`},
	{"gateway_events_outcomes", "gateway_events", `USING gin ((record->'outcomes') jsonb_path_ops)`},
	// The Preflight outbox guard's check for a refused outcome (core.EffectGuard) reads only refused rows since a time; done rows are never
	// deleted, so without this it would scan them all while it holds the outbox's add lock. The pending rows have a partial index in the schema.
	{"gateway_outbox_refused", "gateway_outbox", `(created_at, id) WHERE state = 'refused'`},
}

// Querier is the one read the check needs (a pgx pool or connection).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// Rows is the part of a result the check reads.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// PgxQuerier is what a pgx pool or connection offers; Pgx adapts it.
type PgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type pgxAdapter struct{ q PgxQuerier }

func (a pgxAdapter) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return a.q.Query(ctx, sql, args...)
}

// Pgx is a pool or connection as a Querier.
func Pgx(q PgxQuerier) Querier { return pgxAdapter{q} }

// State is each index's state: present and valid, present and invalid (a CREATE INDEX CONCURRENTLY that failed leaves one: it is
// maintained on every write and used by no query), or absent.
type State struct {
	Index
	Present, Valid bool
}

// Check reads the state of every index in the current schema from the system catalogs (readable by any role).
func Check(ctx context.Context, q Querier) ([]State, error) {
	rows, err := q.Query(ctx, `SELECT c.relname, i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`)
	if err != nil {
		return nil, fmt.Errorf("the indexes could not be checked: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		var valid bool
		if err := rows.Scan(&name, &valid); err != nil {
			return nil, err
		}
		have[name] = valid
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]State, 0, len(Indexes))
	for _, ix := range Indexes {
		v, ok := have[ix.Name]
		out = append(out, State{Index: ix, Present: ok, Valid: ok && v})
	}
	return out, nil
}

// Problems says each index that is absent or invalid ("" when all seven are in place), and what to run.
func Problems(states []State) string {
	var bad []string
	for _, s := range states {
		switch {
		case !s.Present:
			bad = append(bad, s.Name+" (missing)")
		case !s.Valid:
			bad = append(bad, s.Name+" (invalid: an interrupted build)")
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return strings.Join(bad, ", ") + ": the read API's lists and the Preflight outbox guard scan their tables without them; run `autopilot-gateway migrate` (docs/admin/go-gateway-release.md)"
}
