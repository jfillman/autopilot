package readapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The read API's connections are read-only and bounded by a statement timeout whatever the role allows, and they coexist with the
// writer: its advisory lock is held (the scratch database's writer is open) while the store reads.
func TestTheStoreIsReadOnlyBoundedAndTakesNoLock(t *testing.T) {
	dsn, st := scratch(t) // st is the writer: it holds the gateway's advisory lock
	store, err := OpenPg(context.Background(), dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	var ro, timeout, app string
	if err := store.Pool.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&ro); err != nil || ro != "on" {
		t.Fatalf("read only: %q %v", ro, err)
	}
	if err := store.Pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil || timeout != "5s" {
		t.Fatalf("statement_timeout: %q %v", timeout, err)
	}
	_ = store.Pool.QueryRow(ctx, "SHOW application_name").Scan(&app)
	if app != "autopilot-api" {
		t.Fatalf("application_name %q", app)
	}
	for _, w := range []string{"INSERT INTO gateway_holds (agent, until, record) VALUES ('x', now(), '{}')", "DELETE FROM gateway_sessions", "CREATE TABLE zz (a int)",
		"CREATE INDEX zz ON gateway_sessions (id)", "UPDATE gateway_sessions SET closed = true"} {
		if _, err := store.Pool.Exec(ctx, w); err == nil || !strings.Contains(err.Error(), "read-only transaction") {
			t.Errorf("%q was not refused as a write: %v", w, err)
		}
	}
	// It takes no advisory lock: while the writer holds the gateway's, the store reads, and holds none of its own.
	if err := store.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Pool.Query(ctx, "SELECT a.application_name FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE l.locktype = 'advisory'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	holders := 0
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		holders++
		if name == "autopilot-api" {
			t.Fatal("the read API holds an advisory lock")
		}
	}
	if holders == 0 {
		t.Fatal("the writer's own lock was not seen: the probe proves nothing")
	}
}

// Each query is served by an index: with sequential scans switched off, the planner has to name the index it reads, and the one it names
// is the one the comment on PgStore says serves that query. (A table of a few thousand rows, analysed.)
func TestEveryQueryIsServedByAnIndex(t *testing.T) {
	dsn, st := scratch(t)
	ctx := context.Background()
	for i := 0; i < 4000; i++ {
		created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		rec := map[string]any{"id": fmt.Sprintf("s-%05d", i), "principal": fmt.Sprintf("user:p%d", i%40), "agent": fmt.Sprintf("agent-%d", i%20), "task_id": fmt.Sprintf("t-%d", i/3),
			"created_at": created.Format("2006-01-02T15:04:05") + "+00:00", "expires_at": created.Add(time.Hour).Format("2006-01-02T15:04:05") + "+00:00", "closed": i%50 != 0}
		b, _ := json.Marshal(rec)
		if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2::jsonb, $3, $4)", rec["id"], string(b), created.Add(time.Hour), rec["closed"]); err != nil {
			t.Fatal(err)
		}
		evApp, evAgent := fmt.Sprintf("app-%d", i%30), fmt.Sprintf("agent-%d", i%20)
		if i%400 == 7 {
			evApp = "rare-app"
		}
		if i%500 == 11 {
			evAgent = "rare-agent"
		}
		ev := map[string]any{"id": fmt.Sprintf("e-%05d", i), "received_at": created.Format("2006-01-02T15:04:05") + "+00:00", "outcomes": []map[string]string{{"agent": evAgent, "result": "launched"}}, "subject": map[string]string{"id": "x"}}
		eb, _ := json.Marshal(ev)
		if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_events (id, received_at, source, app, routing_key, record) VALUES ($1, $2, 'glidepath', $3, 'k', $4::jsonb)", ev["id"], created, evApp, string(eb)); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			hr := map[string]any{"id": fmt.Sprintf("h-%d", i), "owner": fmt.Sprintf("user:p%d", i%40), "group": "group:default/g", "created_at": rec["created_at"]}
			hb, _ := json.Marshal(hr)
			if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_human_requests (id, session, status, expires_at, record) VALUES ($1, $2, $3, $4, $5::jsonb)", hr["id"], rec["id"], map[bool]string{true: "pending", false: "answered"}[i%20 == 0], time.Now().Add(time.Hour), string(hb)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tbl := range []string{"gateway_sessions", "gateway_events", "gateway_human_requests"} {
		if _, err := st.Pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatal(err)
		}
	}
	_ = dsn
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	person := &Viewer{Role: RolePerson, Person: "user:p3", Agents: map[string]bool{"agent-4": true, "agent-9": true}, Apps: map[string]bool{"app-3": true}, Groups: map[string]bool{"group:default/g": true}}
	auditor := &Viewer{Role: RoleAuditor}
	after := "2026-11-02T00:00:00+00:00"
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	type q struct {
		name string
		sql  string
		args []any
		want []string // every one must appear in the plan
	}
	sql := func(s string, a []any) (string, []any) { return s, a }
	var qs []q
	add := func(name string, s string, a []any, want ...string) { qs = append(qs, q{name, s, a, want}) }
	s1, a1 := sessionsSQL(SessionFilter{}, auditor, 50, "", now)
	add("sessions, an auditor", s1, a1, "gateway_sessions_created")
	s1, a1 = sessionsSQL(SessionFilter{}, auditor, 50, after, now)
	add("sessions, an auditor, a page after a cursor", s1, a1, "gateway_sessions_created")
	s1, a1 = sessionsSQL(SessionFilter{}, person, 50, "", now)
	add("sessions, a person", s1, a1, "gateway_sessions_principal", "gateway_sessions_agent")
	s1, a1 = sessionsSQL(SessionFilter{Principal: "user:p3"}, auditor, 50, "", now)
	add("sessions, one principal", s1, a1, "gateway_sessions_principal")
	s1, a1 = sessionsSQL(SessionFilter{Agent: "agent-4"}, auditor, 50, "", now)
	add("sessions, one agent", s1, a1, "gateway_sessions_agent")
	s1, a1 = sessionsSQL(SessionFilter{Task: "t-5"}, auditor, 50, "", now)
	add("sessions, one task", s1, a1, "gateway_sessions_task")
	s1, a1 = sessionsSQL(SessionFilter{State: "live"}, auditor, 50, "", now)
	add("sessions, live", s1, a1, "gateway_sessions_live")
	s1, a1 = taskSQL("t-5", person)
	add("a task's run tree", s1, a1, "gateway_sessions_task")
	s1, a1 = taskSQL("t-5", auditor)
	add("a task's run tree, an auditor", s1, a1, "gateway_sessions_task")
	add("a session by id", "SELECT record FROM gateway_sessions WHERE id = $1", []any{"s-00005"}, "gateway_sessions_pkey")
	add("a result", "SELECT record FROM gateway_results WHERE session = $1 AND expires_at > $2", []any{"s-00005", now}, "gateway_results_pkey")
	s1, a1 = humanSQL(time.Now(), "", person)
	add("the inbox, a person", s1, a1, "gateway_human_requests_pending")
	s1, a1 = humanSQL(time.Now(), "", auditor)
	add("the inbox, a notifier", s1, a1, "gateway_human_requests_pending")
	s1, a1 = eventsSQL(EventFilter{}, auditor, 50, nil)
	add("events, an auditor", s1, a1, "gateway_events_received_id|gateway_events_received")
	s1, a1 = eventsSQL(EventFilter{}, auditor, 50, &at)
	add("events, an auditor, after a cursor", s1, a1, "gateway_events_received_id|gateway_events_received")
	s1, a1 = eventsSQL(EventFilter{App: "rare-app"}, auditor, 50, nil)
	add("events, one app", s1, a1, "gateway_events_app_id")
	s1, a1 = eventsSQL(EventFilter{Agent: "rare-agent"}, auditor, 50, nil)
	add("events, one agent", s1, a1, "gateway_events_outcomes")
	rare := &Viewer{Role: RolePerson, Person: "user:p3", Agents: map[string]bool{"rare-agent": true}, Apps: map[string]bool{"rare-app": true}}
	s1, a1 = eventsSQL(EventFilter{}, rare, 50, nil)
	add("events, a person's apps and teams' agents", s1, a1, "gateway_events_app_id", "gateway_events_outcomes")
	_ = sql
	for _, c := range qs {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
		var plan string
		if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+c.sql, c.args...).Scan(&plan); err != nil {
			t.Errorf("%s: %v", c.name, err)
			conn.Close(ctx)
			continue
		}
		conn.Close(ctx)
		if strings.Contains(plan, `"Seq Scan"`) {
			t.Errorf("%s: a sequential scan\n%s", c.name, plan)
		}
		for _, ix := range c.want {
			found := false
			for _, alt := range strings.Split(ix, "|") {
				found = found || strings.Contains(plan, alt)
			}
			if !found {
				t.Errorf("%s: the plan does not read %s\n%s", c.name, ix, plan)
			}
		}
	}
}
