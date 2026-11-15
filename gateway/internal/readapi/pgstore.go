package readapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// StatementTimeoutMS bounds every statement on the server (read_api.STATEMENT_TIMEOUT_MS).
const StatementTimeoutMS = 5000

// PgStore reads the gateway's own tables (`gateway_*`) through a pool of read-only connections. It holds no writer lock, creates no
// table and no index, and has no statement that is not a SELECT: every connection is opened read-only (default_transaction_read_only)
// and bounded by a statement timeout, whatever the role allows. The tables' names are constants, never caller input; every value is a
// parameter.
//
// The index that serves each query (the migrations are the writer's, pgstate.indexes, run when `serve` starts; nothing here may create
// one):
//
//	Sessions, unscoped (auditor)     gateway_sessions_created   ((record->>'created_at') DESC, id DESC)
//	Sessions, a person's scope       gateway_sessions_principal / gateway_sessions_agent, with created_at, a bitmap OR
//	Sessions, state=live | expired   gateway_sessions_live      (closed, expires_at)
//	Sessions, a task or an id        gateway_sessions_task, the primary key
//	Task                             gateway_sessions_task
//	Result                           the primary key (session)
//	HumanPending                     gateway_human_requests_pending (status, expires_at)
//	Events, unscoped                 gateway_events_received (received_at DESC)
//	Events, scoped by app            gateway_events_app_id (app, received_at DESC, id DESC), a branch per app
//	Events, scoped by agent          gateway_events_outcomes, a GIN index on the outcomes (record->'outcomes' @> ...)
//	Holds, Definitions               small tables (one row per agent; one per pinned definition)
type PgStore struct{ Pool *pgxpool.Pool }

// OpenPg opens the pool. It does not connect (the first read does), takes no lock and runs no statement but the SELECTs below.
func OpenPg(ctx context.Context, dsn string, conns int32) (*PgStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_API_DSN: %w", err)
	}
	cfg.MaxConns, cfg.MinConns = conns, 0
	cfg.MaxConnLifetime, cfg.MaxConnIdleTime = 30*time.Minute, 5*time.Minute
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["statement_timeout"] = fmt.Sprint(StatementTimeoutMS)
	rp["default_transaction_read_only"] = "on"
	rp["idle_in_transaction_session_timeout"] = "10000"
	rp["application_name"] = "autopilot-api"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &PgStore{Pool: pool}, nil
}

// Close closes the pool.
func (s *PgStore) Close() { s.Pool.Close() }

func decode(raw []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("a stored row does not parse: %w", err)
	}
	return r, nil
}

func (s *PgStore) records(ctx context.Context, sql string, args ...any) ([]Record, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := decode(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// args numbers parameters: add returns the placeholder of a value.
type args struct{ v []any }

func (a *args) add(x any) string { a.v = append(a.v, x); return fmt.Sprintf("$%d", len(a.v)) }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sessionScope is the part of the WHERE a viewer's reach is: everything for an auditor, else their own sessions and those of their
// teams' agents (a notifier has neither: a person of "" matches nothing).
func sessionScope(a *args, v *Viewer, alias string) string {
	if v.All() {
		return "TRUE"
	}
	return fmt.Sprintf("(%s.record->>'principal' = %s OR %s.record->>'agent' = ANY(%s))", alias, a.add(v.Person), alias, a.add(sortedKeys(v.Agents)))
}

// SessionView is _session_view: the record and its state (closed, expired or live) at now.
func SessionView(r Record, now time.Time) (Record, error) {
	out := make(Record, len(r)+1)
	for k, v := range r {
		out[k] = v
	}
	state := "live"
	if c, _ := r["closed"].(bool); c {
		state = "closed"
	} else {
		raw, _ := r["expires_at"].(string)
		exp, err := dispatch.ParsePyIso(raw)
		if err != nil {
			return nil, fmt.Errorf("a stored session has no expiry")
		}
		if !exp.After(now) {
			state = "expired"
		}
	}
	out["state"] = state
	return out, nil
}

func views(rs []Record, now time.Time) ([]Record, error) {
	out := make([]Record, 0, len(rs))
	for _, r := range rs {
		v, err := SessionView(r, now)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// maxAgentBranches is how many of a person's teams' agents get a branch of their own in the list of sessions (see sessionsSQL).
const maxAgentBranches = 32

// sessionsSQL is the list of sessions a viewer may see, newest first. An auditor's is one ordered index scan. A person's is the UNION of
// ordered scans, one for their own sessions (principal) and one per agent of their teams (each capped at the page's size by its own LIMIT),
// merged and cut to the page: each branch reads from the index of its own column in created_at order and stops after a page, so the
// cost is a few pages however long the history is, and a person with nothing to see reads nothing (an OR across the two columns would
// scan the history newest-first for rows that are not there). More than maxAgentBranches agents fall back to one branch for them all.
func sessionsSQL(f SessionFilter, v *Viewer, limit int, after string, now time.Time) (string, []any) {
	a := &args{}
	var common []string
	for _, c := range []struct{ val, col string }{{f.Agent, "agent"}, {f.Principal, "principal"}, {f.Task, "task_id"}} {
		if c.val != "" {
			common = append(common, fmt.Sprintf("s.record->>'%s' = %s", c.col, a.add(c.val)))
		}
	}
	switch f.State {
	case "live":
		common = append(common, "NOT s.closed AND s.expires_at > "+a.add(now))
	case "closed":
		common = append(common, "s.closed")
	case "expired":
		common = append(common, "NOT s.closed AND s.expires_at <= "+a.add(now))
	}
	if after != "" {
		common = append(common, "s.record->>'created_at' < "+a.add(after))
	}
	n := a.add(limit)
	branch := func(scope string) string {
		where := append([]string{scope}, common...)
		return "(SELECT s.record AS record, s.record->>'created_at' AS created, s.id AS id FROM gateway_sessions s WHERE " + strings.Join(where, " AND ") +
			" ORDER BY s.record->>'created_at' DESC, s.id DESC LIMIT " + n + ")"
	}
	if v.All() {
		return "SELECT record FROM " + branch("TRUE") + " u ORDER BY created DESC, id DESC", a.v
	}
	branches := []string{branch("s.record->>'principal' = " + a.add(v.Person))}
	agents := sortedKeys(v.Agents)
	switch {
	case len(agents) == 0:
	case len(agents) <= maxAgentBranches:
		for _, ag := range agents {
			branches = append(branches, branch("s.record->>'agent' = "+a.add(ag)))
		}
	default:
		branches = append(branches, branch("s.record->>'agent' = ANY("+a.add(agents)+")"))
	}
	return "SELECT record FROM (" + strings.Join(branches, " UNION ") + ") u ORDER BY created DESC, id DESC LIMIT " + n, a.v
}

func (s *PgStore) Sessions(ctx context.Context, f SessionFilter, v *Viewer, limit int, after string, now time.Time) ([]Record, error) {
	sql, a := sessionsSQL(f, v, limit, after, now)
	rows, err := s.records(ctx, sql, a...)
	if err != nil {
		return nil, err
	}
	return views(rows, now)
}

func (s *PgStore) Session(ctx context.Context, id string, now time.Time) (Record, error) {
	rows, err := s.records(ctx, "SELECT record FROM gateway_sessions WHERE id = $1", id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return SessionView(rows[0], now)
}

func (s *PgStore) Result(ctx context.Context, sessionID string, now time.Time) (Record, error) {
	rows, err := s.records(ctx, "SELECT record FROM gateway_results WHERE session = $1 AND expires_at > $2", sessionID, now)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func taskSQL(taskID string, v *Viewer) (string, []any) {
	a := &args{}
	sql := "SELECT s.record FROM gateway_sessions s WHERE s.record->>'task_id' = " + a.add(taskID) + " AND " + sessionScope(a, v, "s") +
		" ORDER BY s.record->>'created_at', s.id LIMIT " + a.add(maxTaskSessions)
	return sql, a.v
}

func (s *PgStore) Task(ctx context.Context, taskID string, v *Viewer, now time.Time) ([]Record, error) {
	sql, a := taskSQL(taskID, v)
	rows, err := s.records(ctx, sql, a...)
	if err != nil {
		return nil, err
	}
	return views(rows, now)
}

func (s *PgStore) HumanPending(ctx context.Context, now time.Time, since string, v *Viewer) ([]Record, error) {
	sql, a := humanSQL(now, since, v)
	return s.records(ctx, sql, a...)
}

func humanSQL(now time.Time, since string, v *Viewer) (string, []any) {
	a := &args{}
	where := []string{"status = 'pending'", "expires_at > " + a.add(now)}
	if since != "" {
		where = append(where, "record->>'created_at' > "+a.add(since))
	}
	if v.Role == RolePerson {
		where = append(where, fmt.Sprintf("(record->>'owner' = %s OR record->>'group' = ANY(%s))", a.add(v.Person), a.add(sortedKeys(v.Groups))))
	}
	return "SELECT record FROM gateway_human_requests WHERE " + strings.Join(where, " AND ") + " ORDER BY record->>'created_at', id LIMIT " + a.add(maxPending), a.v
}

// outcomeOf is the containment test that says an event's outcomes name an agent (served by the GIN index on record->'outcomes').
func outcomeOf(a *args, agent string) string {
	doc, _ := json.Marshal([]map[string]string{{"agent": agent}})
	return "e.record->'outcomes' @> " + a.add(string(doc)) + "::jsonb"
}

func (s *PgStore) Events(ctx context.Context, f EventFilter, v *Viewer, limit int, after *time.Time) ([]Record, error) {
	sql, a := eventsSQL(f, v, limit, after)
	return s.records(ctx, sql, a...)
}

// eventsSQL is the trigger bridge's history a viewer may see, newest first. As the list of sessions, a person's is the UNION of ordered
// branches: one per app they own (gateway_events_app_id: app, received_at DESC, id DESC, so each stops after a page) and one for their
// teams' agents (the GIN index on the outcomes finds those events; it cannot order them, so that branch sorts only the agents' own
// events, which the table's retention (30 days, 200,000 rows) bounds). More than maxAgentBranches apps fall back to one branch for them all.
func eventsSQL(f EventFilter, v *Viewer, limit int, after *time.Time) (string, []any) {
	a := &args{}
	var common []string
	for _, c := range []struct{ val, col string }{{f.App, "e.app"}, {f.Source, "e.source"}, {f.Key, "e.routing_key"}, {f.Subject, "e.record->'subject'->>'id'"}} {
		if c.val != "" {
			common = append(common, c.col+" = "+a.add(c.val))
		}
	}
	if f.Agent != "" {
		common = append(common, outcomeOf(a, f.Agent))
	}
	if f.Since != nil {
		common = append(common, "e.received_at >= "+a.add(*f.Since))
	}
	if f.Until != nil {
		common = append(common, "e.received_at < "+a.add(*f.Until))
	}
	if after != nil {
		common = append(common, "e.received_at < "+a.add(*after))
	}
	n := a.add(limit)
	branch := func(scope string) string {
		where := common
		if scope != "" {
			where = append([]string{scope}, common...)
		}
		sql := "(SELECT e.record AS record, e.received_at AS received, e.id AS id FROM gateway_events e"
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		return sql + " ORDER BY e.received_at DESC, e.id DESC LIMIT " + n + ")"
	}
	if v.All() {
		return "SELECT record FROM " + branch("") + " u ORDER BY received DESC, id DESC", a.v
	}
	var branches []string
	apps := sortedKeys(v.Apps)
	switch {
	case len(apps) == 0:
	case len(apps) <= maxAgentBranches:
		for _, app := range apps {
			branches = append(branches, branch("e.app = "+a.add(app)))
		}
	default:
		branches = append(branches, branch("e.app = ANY("+a.add(apps)+")"))
	}
	if agents := sortedKeys(v.Agents); len(agents) > 0 {
		var any []string
		for _, ag := range agents {
			any = append(any, outcomeOf(a, ag))
		}
		branches = append(branches, branch("("+strings.Join(any, " OR ")+")"))
	}
	if len(branches) == 0 {
		return "SELECT record FROM " + branch("FALSE") + " u ORDER BY received DESC, id DESC", a.v
	}
	return "SELECT record FROM (" + strings.Join(branches, " UNION ") + ") u ORDER BY received DESC, id DESC LIMIT " + n, a.v
}

func (s *PgStore) Holds(ctx context.Context, now time.Time) (map[string]Record, error) {
	rows, err := s.records(ctx, "SELECT record FROM gateway_holds WHERE until > $1 ORDER BY agent", now)
	if err != nil {
		return nil, err
	}
	out := map[string]Record{}
	for _, r := range rows {
		if agent, ok := r["agent"].(string); ok {
			out[agent] = r
		}
	}
	return out, nil
}

func (s *PgStore) Definitions(ctx context.Context) ([]definitions.Row, error) {
	rows, err := s.Pool.Query(ctx, "SELECT record FROM gateway_definitions ORDER BY state, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []definitions.Row
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var r definitions.Row
		if json.Unmarshal(raw, &r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func (s *PgStore) Ping(ctx context.Context) error {
	var one int
	err := s.Pool.QueryRow(ctx, "SELECT 1 FROM gateway_sessions LIMIT 1").Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
