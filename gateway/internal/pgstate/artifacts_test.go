package pgstate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
