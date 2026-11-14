package pgstate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

type artifactOp struct {
	Op      string `json:"op"`
	Task    string `json:"task"`
	Name    any    `json:"name"`
	Content any    `json:"content"`
	At      int64  `json:"at"`
	Micro   int64  `json:"micro"`
	Result  any    `json:"result"`
	Denied  *struct{ Rule, Message, Hint string }
	Go      *struct {
		Result any `json:"result"`
		Denied *struct{ Rule, Message, Hint string }
	} `json:"go"`
}

func expand(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if parts, ok := x["$str"].([]any); ok && len(x) == 1 {
			n, _ := parts[2].(json.Number).Int64()
			return parts[0].(string) + strings.Repeat(parts[1].(string), int(n)) + parts[3].(string)
		}
		for k, e := range x {
			x[k] = expand(e)
		}
	case []any:
		for i, e := range x {
			x[i] = expand(e)
		}
	}
	return v
}

func norm(t *testing.T, v any) any {
	t.Helper()
	b, _ := json.Marshal(v)
	var out any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The artifacts the Python gateway's PgArtifacts keeps and the Go store keeps, over the same sequence (the fixture the
// generator writes by driving the Python class): the same answers, the same refusals, in the real database.
func TestArtifactsEqualPythonInPostgres(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	raw, err := os.ReadFile("../backends/testdata/artifacts.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Start string       `json:"start"`
		Ops   []artifactOp `json:"ops"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.RFC3339, f.Start)
	var clock time.Time
	h, err := backends.NewHandlers(backends.Config{Artifacts: pgstate.NewArtifacts(st), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	for i, op := range f.Ops {
		if op.Task == "" {
			continue
		}
		clock = start.Add(time.Duration(op.At)*time.Second + time.Duration(op.Micro)*time.Microsecond)
		s := &session.Session{ID: "s-" + op.Task, TaskID: op.Task}
		var got any
		var gerr error
		if op.Op == "put" {
			got, gerr = h["artifact.put"](ctx, s, map[string]any{"name": op.Name, "content": expand(op.Content)})
		} else {
			var c any
			c, gerr = h["artifact.get"](ctx, s, map[string]any{"name": op.Name})
			got = map[string]any{"content": c}
		}
		denied, result := op.Denied, op.Result
		if op.Go != nil {
			denied, result = op.Go.Denied, op.Go.Result
		}
		if denied != nil {
			var den *core.Denied
			if !errors.As(gerr, &den) || den.Rule != denied.Rule || den.Message != denied.Message || den.Hint != denied.Hint {
				t.Fatalf("op %d (%s %v in %s): want %+v, got %v / %v", i, op.Op, op.Name, op.Task, denied, got, gerr)
			}
			continue
		}
		if gerr != nil {
			t.Fatalf("op %d (%s %v in %s): %v", i, op.Op, op.Name, op.Task, gerr)
		}
		if !reflect.DeepEqual(norm(t, expand(result)), norm(t, got)) {
			t.Fatalf("op %d (%s %v in %s): want %v got %v", i, op.Op, op.Name, op.Task, expand(result), got)
		}
	}
}

// Two puts that race for the last place in a task's quota cannot both win; the count never passes the limit.
func TestConcurrentArtifactPutsRespectTheQuotaInPostgres(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	a := pgstate.NewArtifacts(st)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "n" + strings.Repeat("x", i)
			if _, err := a.Put(ctx, "t-race", name, "x", now); err == nil {
				mu.Lock()
				stored++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var count int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM gateway_artifacts WHERE task_id = 't-race'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != backends.MaxArtifactCount || stored != backends.MaxArtifactCount {
		t.Fatalf("%d rows, %d stored: the limit is %d", count, stored, backends.MaxArtifactCount)
	}
}

// Puts of different tasks no longer touch each other's rows: with 20,000 expired rows across 200 tasks and 32 tasks putting at
// once, every put succeeds, each deletes only its own task's expired rows, the others are left for Purge, Purge deletes them in
// bounded batches while puts run, and an expired row is never read.
func TestPutsAcrossTasksAndThePurgeNeverBlockEachOther(t *testing.T) {
	dsn := scratch(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	a := pgstate.NewArtifacts(st)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_artifacts (task_id, name, content, bytes, expires_at) "+
		"SELECT 't-old-' || (i % 200), 'n' || i, 'x', 1, $1 FROM generate_series(0, 19999) i", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := a.Get(ctx, "t-old-0", "n0", now); ok {
		t.Fatalf("an expired row was read: %q", got)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	start := time.Now()
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := a.Put(ctx, fmt.Sprintf("t-old-%d", i), fmt.Sprintf("new%d", j), "y", now); err != nil {
					mu.Lock()
					failed++
					t.Log(err)
					mu.Unlock()
				}
			}
		}()
	}
	var purged int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < 3; k++ {
			n, err := a.Purge(ctx, now, 500, 5)
			if err != nil {
				mu.Lock()
				failed++
				t.Log(err)
				mu.Unlock()
			}
			mu.Lock()
			purged += n
			mu.Unlock()
		}
	}()
	wg.Wait()
	if failed != 0 {
		t.Fatalf("%d operations failed", failed)
	}
	t.Logf("32 tasks x 5 puts and 3 purges beside 20,000 expired rows: %s", time.Since(start))
	// the batches are bounded: one purge of 2 batches of 100 deletes at most 200 rows
	before := count(t, st)
	n, err := a.Purge(ctx, now, 100, 2)
	if err != nil || n != 200 || before-count(t, st) != 200 {
		t.Fatalf("a bounded purge: %d %v", n, err)
	}
	for {
		n, err := a.Purge(ctx, now, 1000, 50)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	// only live rows are left: the 160 puts (each task's new0..new4), and none expired
	var expired, live int64
	_ = st.Pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE expires_at <= $1), count(*) FILTER (WHERE expires_at > $1) FROM gateway_artifacts", now).Scan(&expired, &live)
	if expired != 0 || live != 160 {
		t.Fatalf("expired %d live %d", expired, live)
	}
	// a Python-written row (the same table) is read, and a replaced name is Go's
	if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_artifacts (task_id, name, content, bytes, expires_at) VALUES ('t-py', 'plan', 'from python', 11, $1)", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := a.Get(ctx, "t-py", "plan", now); err != nil || !ok || got != "from python" {
		t.Fatalf("%q %v %v", got, ok, err)
	}
	if _, err := a.Put(ctx, "t-py", "plan", "from go", now); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := a.Get(ctx, "t-py", "plan", now); got != "from go" {
		t.Fatal(got)
	}
}

func count(t *testing.T, st *pgstate.State) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM gateway_artifacts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
