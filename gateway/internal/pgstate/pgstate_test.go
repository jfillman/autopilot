package pgstate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/core"
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
