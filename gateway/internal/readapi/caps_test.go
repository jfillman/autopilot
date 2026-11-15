package readapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func capsAPI(t *testing.T) (*API, func(sql string, args ...any)) {
	t.Helper()
	dsn, st := scratch(t)
	store, err := OpenPg(context.Background(), dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	exec := func(sql string, args ...any) {
		if _, err := st.Pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return apiWithAccess(t, store), exec
}

func apiWithAccess(t *testing.T, store Store) *API {
	t.Helper()
	a, err := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: store, Defs: defsFrom(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func insertTask(exec func(string, ...any), n int) {
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i)*time.Second).Format("2006-01-02T15:04:05") + "+00:00"
		rec, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("s-%05d", i), "principal": "user:alice", "agent": "triage-agent", "task_id": "big", "created_at": ts,
			"expires_at": ts, "closed": true, "used": map[string]int{"tool_calls": 1}, "usage": map[string]int{"model_calls": 1}})
		exec("INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2::jsonb, $3, true)", fmt.Sprintf("s-%05d", i), string(rec), base)
	}
}

// A run tree longer than the API shows says so, and says nothing about its totals (the sum of part of a tree is not the tree's); one
// exactly at the cap is whole.
func TestATaskTreeLongerThanTheCapIsMarkedTruncatedWithoutTotals(t *testing.T) {
	api, exec := capsAPI(t)
	insertTask(exec, maxTaskSessions+1)
	rec := get(api, "/v1/tasks/big", "admin")
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["truncated"] != true || len(body["sessions"].([]any)) != maxTaskSessions || body["used"] != nil || body["cost"] != nil {
		t.Fatalf("%d truncated=%v sessions=%d used=%v", rec.Code, body["truncated"], len(body["sessions"].([]any)), body["used"])
	}
	exec("DELETE FROM gateway_sessions WHERE id = 's-01000'")
	rec = get(api, "/v1/tasks/big", "admin")
	body = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, has := body["truncated"]; has || len(body["sessions"].([]any)) != maxTaskSessions || body["used"].(map[string]any)["tool_calls"] != float64(maxTaskSessions) {
		t.Fatalf("a tree at the cap is whole: %v", body["truncated"])
	}
}

// The inbox keeps the NEWEST pending requests when there are more than it shows, says so, and still lists them oldest first.
func TestTheInboxKeepsTheNewestAndSaysWhenItCutsOff(t *testing.T) {
	api, exec := capsAPI(t)
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for i := 0; i < maxPending+1; i++ {
		ts := base.Add(time.Duration(i)*time.Second).Format("2006-01-02T15:04:05") + "+00:00"
		rec, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("h-%05d", i), "owner": "user:alice", "created_at": ts})
		exec("INSERT INTO gateway_human_requests (id, session, status, expires_at, record) VALUES ($1, 's', 'pending', now() + interval '1 day', $2::jsonb)", fmt.Sprintf("h-%05d", i), string(rec))
	}
	rec := get(api, "/v1/human/requests", "admin")
	var body struct {
		Items     []map[string]any `json:"items"`
		Truncated bool             `json:"truncated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || !body.Truncated || len(body.Items) != maxPending || body.Items[0]["id"] != "h-00001" || body.Items[maxPending-1]["id"] != fmt.Sprintf("h-%05d", maxPending) {
		t.Fatalf("%d truncated=%v n=%d first=%v last=%v", rec.Code, body.Truncated, len(body.Items), body.Items[0]["id"], body.Items[len(body.Items)-1]["id"])
	}
	exec("DELETE FROM gateway_human_requests WHERE id = 'h-00000'")
	rec = get(api, "/v1/human/requests", "admin")
	body.Truncated, body.Items = false, nil
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Truncated || len(body.Items) != maxPending || body.Items[0]["id"] != "h-00001" {
		t.Fatalf("an inbox at the cap is whole: %v %d", body.Truncated, len(body.Items))
	}
}
