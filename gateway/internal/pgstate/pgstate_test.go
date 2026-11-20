package pgstate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dbindex"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// scratch makes a database of its own on the server AUTOPILOT_TEST_PG names (as tests/test_gateway_pg.py does).
func scratch(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AUTOPILOT_TEST_PG")
	if dsn == "" {
		t.Skip("no scratch Postgres (set AUTOPILOT_TEST_PG)")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("gwtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		a, err := pgx.Connect(ctx, dsn)
		if err == nil {
			_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close(ctx)
		}
	})
	return u.String()
}

func TestOneWriterPerDatabase(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	a, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgstate.Open(ctx, dsn, 4); err != pgstate.ErrWriterLocked {
		t.Fatalf("a second gateway must stop: %v", err)
	}
	a.Close(ctx)
	b, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("the lock is released with the connection: %v", err)
	}
	b.Close(ctx)
}

// A row the Python gateway wrote loads here; one written here is a row the Python gateway's from_record reads.
func TestSessionsShareTheTableWithThePythonGateway(t *testing.T) {
	dsn := scratch(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	py := `{"id":"s-py","principal":"user:alice","agent":"coding-agent","limits":{"children":2,"compute":"small","github_calls":300,"model_tokens":500000,"models":["m"],"network":"clearance","open_prs":3,"tier_ceiling":"T1","tool_calls":200,"tools":["catalog.read"],"ttl_minutes":30},"created_at":"2099-01-01T12:00:00+00:00","expires_at":"2099-01-01T12:30:00+00:00","parent_id":null,"depth":0,"task_id":"t-py","used":{"children":0,"github_calls":0,"model_tokens":0,"open_prs":0,"tool_calls":2},"reserved":{"children":0,"github_calls":0,"model_tokens":0,"open_prs":0,"tool_calls":0},"children":[],"tripped":false,"trip_reason":null,"denials":0,"closed":false,"run":null,"input":null,"outcome":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"charged_tokens":0,"model_calls":0}}`
	if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ('s-py', $1, '2099-01-01T12:30:00Z', false)", py); err != nil {
		t.Fatal(err)
	}
	p := pgstate.NewSessions(ctx, st)
	store, err := session.NewStore(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s := store.Get("s-py")
	if s == nil || s.Used.ToolCalls != 2 {
		t.Fatalf("%+v", s)
	}
	s.Lock()
	s.Used.ToolCalls = 5
	s.Unlock()
	if err := store.Flush(ctx, s); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	var closed bool
	if err := st.Pool.QueryRow(ctx, "SELECT record, closed FROM gateway_sessions WHERE id = 's-py'").Scan(&raw, &closed); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	_ = json.Unmarshal(raw, &rec)
	if rec["used"].(map[string]any)["tool_calls"] != float64(5) || closed {
		t.Fatalf("%s", raw)
	}
	for _, k := range []string{"id", "principal", "agent", "limits", "created_at", "expires_at", "parent_id", "depth", "task_id", "used", "reserved",
		"children", "tripped", "trip_reason", "denials", "closed", "run", "input", "outcome", "usage"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("the record lacks %q, which the Python gateway reads", k)
		}
	}
}

func TestConcurrentSavesAreGroupedAndEveryOneIsDurable(t *testing.T) {
	dsn := scratch(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := pgstate.Open(ctx, dsn, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	p := pgstate.NewSessions(ctx, st)
	store, _ := session.NewStore(ctx, p)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("s-%03d", i)
			_ = id
			s := store.Adopt(&client.Grant{Session: id, TaskID: "t", Principal: "u", Agent: "a", Limits: client.Limits{Tools: []string{}, Models: []string{}},
				ExpiresAt: time.Now().Add(time.Hour)}, time.Now(), nil, 0)
			if err := store.Flush(ctx, s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	_ = st.Pool.QueryRow(ctx, "SELECT count(*) FROM gateway_sessions").Scan(&n)
	if n != 200 {
		rows, _ := st.Pool.Query(ctx, "SELECT id FROM gateway_sessions ORDER BY id")
		var ids []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		t.Fatalf("%d rows: %v", n, ids[len(ids)-3:])
	}
}

// The queries the Preflight score's outbox guard (core.EffectGuard) makes: entries by id, state, time and op, and the greatest id.
func TestTheOutboxCanBeQueriedForTheEffectGuard(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	ob := pgstate.NewOutbox(st)
	if hw, err := ob.HighWater(ctx); err != nil || hw != 0 {
		t.Fatalf("%d %v", hw, err)
	}
	eff := map[string]any{"effect": map[string]any{"repo": "o/r", "pr": 7, "paths": []any{"a.yaml"}}}
	a, _ := ob.Add(ctx, core.Entry{Op: "outcome", DecisionID: "d-1", Body: eff}, "core-unavailable", "")
	r, _ := ob.Add(ctx, core.Entry{Op: "record", Kind: "tool.call", Body: map[string]any{}}, "outage", "")
	c, _ := ob.Add(ctx, core.Entry{Op: "outcome", DecisionID: "d-3", Body: eff}, "refused", "the core refused")
	_ = ob.Done(ctx, a, nil)
	if hw, _ := ob.HighWater(ctx); hw != c {
		t.Fatalf("high water %d, want %d", hw, c)
	}
	all, err := ob.Entries(ctx, core.OutboxQuery{OutcomesOnly: true})
	if err != nil || len(all) != 2 || all[0].ID != a || all[0].State != "done" || all[1].ID != c || all[1].State != "refused" || all[1].At.IsZero() {
		t.Fatalf("%+v %v", all, err)
	}
	if all[1].Entry.Body["effect"] == nil {
		t.Fatal("the body is not read back")
	}
	refused, _ := ob.Entries(ctx, core.OutboxQuery{States: []string{"refused", "pending"}})
	if len(refused) != 2 || refused[0].ID != r || refused[1].ID != c { // the record is pending, the outcome refused
		t.Fatalf("%+v", refused)
	}
	after, _ := ob.Entries(ctx, core.OutboxQuery{AfterID: a})
	if len(after) != 2 || after[0].ID != r {
		t.Fatalf("%+v", after)
	}
	if none, _ := ob.Entries(ctx, core.OutboxQuery{Since: time.Now().Add(time.Hour)}); len(none) != 0 {
		t.Fatalf("%+v", none)
	}
	if one, _ := ob.Entries(ctx, core.OutboxQuery{Limit: 1}); len(one) != 1 {
		t.Fatalf("%+v", one)
	}
}

func TestTheOutboxIsOrderedAndSurvivesARestart(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	ob := pgstate.NewOutbox(st)
	a, _ := ob.Add(ctx, core.Entry{Op: "outcome", DecisionID: "d-1", Body: map[string]any{"result": "ok"}}, "core-unavailable", "")
	b, _ := ob.Add(ctx, core.Entry{Op: "record", Kind: "tool.call", Body: map[string]any{"coverage": "coverage-unavailable"}}, "outage", "")
	_, _ = ob.Add(ctx, core.Entry{Op: "outcome", DecisionID: "d-3", Body: map[string]any{"result": "x"}}, "refused", "the core refused")
	st.Close(ctx)

	st, err = pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	ob = pgstate.NewOutbox(st)
	rows, _ := ob.Pending(ctx, 10)
	if len(rows) != 2 || rows[0].ID != a || rows[1].ID != b || rows[0].Entry.DecisionID != "d-1" || rows[1].Entry.Kind != "tool.call" {
		t.Fatalf("%+v", rows)
	}
	seq := int64(9)
	_ = ob.Done(ctx, a, &seq)
	_ = ob.Refused(ctx, b, "no")
	if has, _ := ob.HasPending(ctx); has {
		t.Fatal("nothing should wait")
	}
}

// A finished run's answer is kept by session until it expires, the first one wins, and what has expired is dropped by the
// next put (engine/pg.py PgResults).
func TestResultsAreKeptByTheSessionUntilTheyExpire(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	r := pgstate.NewResults(st)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	rec := func(id, text string, days int) map[string]any {
		return map[string]any{"session": id, "task_id": "t-" + id, "final_text": text, "status": "done", "warnings": []any{},
			"expires_at": now.Add(time.Duration(days) * 24 * time.Hour).Format("2006-01-02T15:04:05Z")}
	}
	if err := r.Put(ctx, rec("s-1", "first", 30), now); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(ctx, rec("s-1", "second", 30), now); err != nil { // a finish retried writes once
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "s-1", now)
	if err != nil || got["final_text"] != "first" || got["status"] != "done" {
		t.Fatalf("%v %v", got, err)
	}
	if got, _ := r.Get(ctx, "s-nope", now); got != nil {
		t.Fatalf("%v", got)
	}
	if got, _ := r.Get(ctx, "s-1", now.Add(31*24*time.Hour)); got != nil {
		t.Fatal("an expired answer is not read")
	}
	if err := r.Put(ctx, rec("s-2", "later", 30), now.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM gateway_results WHERE session = 's-1'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a put drops what has expired: %d %v", n, err)
	}
	if err := r.Put(ctx, map[string]any{"session": "", "expires_at": "x"}, now); err == nil {
		t.Fatal("a record with no session or expiry is refused")
	}
}

// The pinned set is replaced whole in one transaction and read back as it was written: a smaller set leaves nothing of the larger
// one, a failed replace leaves the old set, and two rejections of one name do not fail the replace (the Python gateway's would).
func TestDefinitionsAreReplacedWhole(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	rows := []map[string]any{
		{"name": "a", "state": "loaded", "source": map[string]any{"kind": "cluster"}, "doc": map[string]any{"name": "a"}},
		{"name": "b", "state": "rejected", "source": map[string]any{"kind": "cluster"}, "reason": "one"},
		{"name": "b", "state": "rejected", "source": map[string]any{"kind": "cluster"}, "reason": "two"},
	}
	if err := st.ReplaceDefinitions(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got, err := st.Definitions(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("%d rows, %v: want the loaded one and the first rejection", len(got), err)
	}
	if err := st.ReplaceDefinitions(ctx, rows[:1]); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.Definitions(ctx); len(got) != 1 {
		t.Fatalf("a smaller set left %d rows", len(got))
	}
	// a row that cannot be stored rolls the whole replace back
	bad := []map[string]any{{"name": "z", "state": "loaded", "doc": map[string]any{"x": make(chan int)}}}
	if err := st.ReplaceDefinitions(ctx, bad); err == nil {
		t.Fatal("an unencodable row must fail the replace")
	}
	if got, _ = st.Definitions(ctx); len(got) != 1 {
		t.Fatalf("a failed replace changed the set: %d rows", len(got))
	}
	if err := st.ReplaceDefinitions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.Definitions(ctx); len(got) != 0 {
		t.Fatalf("an empty set left %d rows", len(got))
	}
}

// serve creates no index (a plain CREATE INDEX blocks writes while it builds): `migrate` does, and a second run changes nothing.
func TestOpenCreatesNoIndexAndMigrateCreatesTheReadAPIsIndexes(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	states, err := st.CheckIndexes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 6 {
		t.Fatalf("%d indexes are named, want 6", len(states))
	}
	for _, s := range states {
		if s.Present {
			t.Errorf("serve's start created %s", s.Name)
		}
	}
	if m := dbindex.Problems(states); !strings.Contains(m, "autopilot-gateway migrate") || !strings.Contains(m, "gateway_sessions_created (missing)") {
		t.Errorf("a missing index must say what to run: %q", m)
	}
	st.Close(ctx)

	var logged []string
	created, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{Log: func(m string) { logged = append(logged, m) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 6 {
		t.Fatalf("migrate created %v", created)
	}
	st, err = pgstate.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	states, _ = st.CheckIndexes(ctx)
	for _, s := range states {
		if !s.Valid {
			t.Errorf("index %s is not in place and valid after migrate", s.Name)
		}
	}
	if m := dbindex.Problems(states); m != "" {
		t.Errorf("nothing to say when every index is there: %q", m)
	}
	created, err = pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}) // again: idempotent
	if err != nil || len(created) != 0 {
		t.Fatalf("a second migrate: created %v, %v", created, err)
	}
}

// Migrate runs beside a writer: the Python gateway (or this one) holds the writer lock and writes while the indexes are built, and a
// write is not made to wait for a build (CONCURRENTLY).
func TestMigrateRunsBesideAWriterAndDoesNotBlockItsWrites(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	for i := 0; i < 2000; i++ {
		if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2, now(), false)",
			fmt.Sprintf("s%05d", i), fmt.Sprintf(`{"created_at":"2026-11-14T10:%02d:00Z","principal":"user:a","agent":"x","task_id":"t%d"}`, i%60, i)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}); done <- err }()
	for i := 2000; i < 2100; i++ { // writes while it runs: each one returns (a plain CREATE INDEX would hold them for the build)
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := st.Pool.Exec(wctx, "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, '{}', now(), false)", fmt.Sprintf("s%05d", i))
		cancel()
		if err != nil {
			t.Fatalf("a write beside migrate: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// An interrupted CONCURRENTLY build leaves an INVALID index that serves no query: serve says so, and migrate drops it and builds it again.
func TestMigrateRepairsAnInvalidIndex(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	if _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := pgstate.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	if _, err := st.Pool.Exec(ctx, "UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'gateway_sessions_task'::regclass"); err != nil {
		t.Skipf("the scratch role cannot edit the catalog to plant an invalid index: %v", err)
	}
	states, _ := st.CheckIndexes(ctx)
	if m := dbindex.Problems(states); !strings.Contains(m, "gateway_sessions_task (invalid") {
		t.Fatalf("an invalid index must be named: %q", m)
	}
	created, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{})
	if err != nil || len(created) != 1 || created[0] != "gateway_sessions_task" {
		t.Fatalf("migrate repaired %v, %v", created, err)
	}
	if states, _ = st.CheckIndexes(ctx); dbindex.Problems(states) != "" {
		t.Fatalf("still a problem: %v", dbindex.Problems(states))
	}
}

// Two migrates at once: the second says so and changes nothing; and a lock held open past the lock timeout fails the migrate fast (it does
// not queue behind the lock for ever).
func TestMigrateTakesOneAtATimeAndGivesUpOnALockHeldOpen(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	hold, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close(ctx)
	if _, err := hold.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(pgstate.MigrateLockKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}); !errors.Is(err, pgstate.ErrMigrateRunning) {
		t.Fatalf("a second migrate: %v", err)
	}
	if _, err := hold.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(pgstate.MigrateLockKey)); err != nil {
		t.Fatal(err)
	}
	// A transaction that holds the table open (an open transaction that read it) makes CREATE INDEX CONCURRENTLY wait; with a short
	// lock timeout the migrate fails with a lock error instead of waiting behind it.
	tx, err := hold.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE gateway_sessions IN SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{LockTimeout: 300 * time.Millisecond})
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("a held lock must fail the migrate with a lock timeout, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("migrate waited %s behind a lock", time.Since(start))
	}
	if _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}); err != nil { // and a re-run completes it
		t.Fatal(err)
	}
}

// A valid index with the right name and the wrong definition is not accepted: migrate builds the right one beside it, drops the old, renames.
func TestMigrateRebuildsAnIndexWithTheWrongDefinition(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	if _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if w, err := pgstate.WrongDefinitions(ctx, c); err != nil || len(w) != 0 {
		t.Fatalf("a fresh migrate has wrong definitions: %v %v", w, err)
	}
	// the right name, valid, but on another column and in the locale's collation
	for _, q := range []string{"DROP INDEX gateway_sessions_agent", "CREATE INDEX gateway_sessions_agent ON gateway_sessions ((record->>'agent'))"} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if w, _ := pgstate.WrongDefinitions(ctx, c); !w["gateway_sessions_agent"] || len(w) != 1 {
		t.Fatalf("the wrong definition was not seen: %v", w)
	}
	created, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{})
	if err != nil || len(created) != 1 || created[0] != "gateway_sessions_agent" {
		t.Fatalf("migrate rebuilt %v, %v", created, err)
	}
	if w, _ := pgstate.WrongDefinitions(ctx, c); len(w) != 0 {
		t.Fatalf("still wrong: %v", w)
	}
	var n int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname LIKE 'gateway_sessions_agent%'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d indexes named like it (the temporary one must be gone): %v", n, err)
	}
}

// A routine re-run takes no table lock: with a reader holding ACCESS SHARE on the ledger table (what an ADD COLUMN IF NOT EXISTS would queue
// behind), and a short lock timeout, a second migrate still succeeds, because it runs no ALTER and builds nothing.
func TestAMigrateReRunTakesNoTableLock(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	if _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	hold, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close(ctx)
	tx, err := hold.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, tb := range []string{"gateway_trigger_launches", "gateway_sessions", "gateway_events"} {
		if _, err := tx.Exec(ctx, "LOCK TABLE "+tb+" IN ACCESS SHARE MODE"); err != nil {
			t.Fatal(err)
		}
	}
	var logged []string
	if _, err := pgstate.Migrate(ctx, dsn, pgstate.MigrateOptions{LockTimeout: 300 * time.Millisecond, Log: func(m string) { logged = append(logged, m) }}); err != nil {
		t.Fatalf("a re-run queued behind a reader: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "|"), "schema: present (no statement run)") {
		t.Fatalf("the schema step ran: %v", logged)
	}
}
