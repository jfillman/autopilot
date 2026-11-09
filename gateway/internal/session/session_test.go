package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// A row the Python gateway wrote (Session.to_record): the Go gateway must read it, and write it back the same.
const pythonRow = `{"id":"s-1a6aaf485880","principal":"user:alice","agent":"coding-agent",
"limits":{"children":2,"compute":"small","github_calls":300,"model_tokens":500000,"models":["claude-sonnet-5"],"network":"clearance+model","open_prs":3,"tier_ceiling":"T1","tool_calls":200,"tools":["catalog.read","run.spawn"],"ttl_minutes":30},
"created_at":"2026-09-26T12:00:00+00:00","expires_at":"2026-09-26T12:30:00+00:00","parent_id":null,"depth":0,"task_id":"t-1a6aaf485880",
"used":{"children":0,"github_calls":4,"model_tokens":10,"open_prs":1,"tool_calls":3},"reserved":{"children":0,"github_calls":0,"model_tokens":0,"open_prs":0,"tool_calls":0},
"children":["s-c1"],"tripped":true,"trip_reason":"5 denied calls","denials":5,"closed":false,"run":"agent-x","input":{"prompt":"hi"},"outcome":null,
"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"charged_tokens":3,"model_calls":1}}`

func TestAPythonRowRoundTrips(t *testing.T) {
	var r session.Record
	if err := json.Unmarshal([]byte(pythonRow), &r); err != nil {
		t.Fatal(err)
	}
	s, err := session.FromRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if s.Used.GithubCalls != 4 || !s.Tripped || *s.TripReason != "5 denied calls" || s.Run == nil || s.Input["prompt"] != "hi" {
		t.Fatalf("%+v", s)
	}
	var want, got map[string]any
	_ = json.Unmarshal([]byte(pythonRow), &want)
	back, _ := json.Marshal(s.ToRecord())
	_ = json.Unmarshal(back, &got)
	// Python writes microseconds only when there are some; the instants are what must agree.
	for _, k := range []string{"created_at", "expires_at"} {
		a, _ := time.Parse(time.RFC3339Nano, want[k].(string))
		b, _ := time.Parse(time.RFC3339Nano, got[k].(string))
		if !a.Equal(b) {
			t.Fatalf("%s: %v != %v", k, a, b)
		}
		delete(want, k)
		delete(got, k)
	}
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(got)
	if string(wj) != string(gj) {
		t.Fatalf("\n got %s\nwant %s", gj, wj)
	}
}

type mem struct{ rows map[string]session.Record }

func (m *mem) Load(context.Context) ([]session.Record, error) {
	var out []session.Record
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *mem) Save(_ context.Context, recs []session.Record) error {
	for _, r := range recs {
		m.rows[r.ID] = r
	}
	return nil
}

func grant(id string, parent *string, depth int, limits client.Limits) *client.Grant {
	return &client.Grant{Session: id, TaskID: "t-" + id, Principal: "user:alice", Agent: "a", Parent: parent, Depth: depth, Limits: limits,
		ExpiresAt: time.Now().Add(time.Hour)}
}

func TestFlushSavesOnlyWhatChangedAndRestartReloadsIt(t *testing.T) {
	p := &mem{rows: map[string]session.Record{}}
	st, _ := session.NewStore(context.Background(), p)
	s := st.Adopt(grant("s-1", nil, 0, client.Limits{ToolCalls: 10, Tools: []string{"a"}}), time.Now(), nil, 0)
	if err := st.Flush(context.Background(), s); err != nil || len(p.rows) != 1 {
		t.Fatalf("%v %d", err, len(p.rows))
	}
	delete(p.rows, "s-1")
	if err := st.Flush(context.Background(), s); err != nil || len(p.rows) != 0 {
		t.Fatal("an unchanged session was saved again")
	}
	s.Lock()
	s.Used.ToolCalls = 4
	s.Unlock()
	_ = st.Flush(context.Background(), s)
	st2, err := session.NewStore(context.Background(), p)
	if err != nil || st2.Get("s-1") == nil || st2.Get("s-1").Used.ToolCalls != 4 {
		t.Fatalf("%v", err)
	}
}

func TestChargeAppliesTheCoresSpendAndTripsTheTree(t *testing.T) {
	st, _ := session.NewStore(context.Background(), nil)
	lim := client.Limits{ToolCalls: 10, GithubCalls: 10, OpenPRs: 2, ModelTokens: 100, Children: 2, Tools: []string{}}
	root := st.Adopt(grant("s-r", nil, 0, lim), time.Now(), nil, 0)
	rid := "s-r"
	st.Adopt(grant("s-c", &rid, 1, lim), time.Now(), &client.Budgets{ToolCalls: 5}, 1)
	root.Lock()
	tree := st.Charge(root, client.Spend{ToolCalls: 1, GithubCalls: 2, Denial: true, Trip: "x", TripTree: "boom"})
	root.Unlock()
	if root.Used.ToolCalls != 1 || root.Used.GithubCalls != 2 || root.Denials != 1 || !root.Tripped || tree != "boom" {
		t.Fatalf("%+v", root)
	}
	st.TripTree("s-r", tree)
	if c := st.Get("s-c"); !c.Tripped || *c.TripReason != "parent tripped: boom" {
		t.Fatalf("%+v", c)
	}
	if root.Reserved.ToolCalls != 5 || root.Used.Children != 1 || root.Remaining("tool_calls") != 10-1-5 {
		t.Fatalf("reserve %+v used %+v", root.Reserved, root.Used)
	}
	st.Close("s-c")
	if root.Reserved.ToolCalls != 5-10 { // the child's whole limit is released, as the Python store does
		t.Fatalf("%+v", root.Reserved)
	}
}

func TestIdsAreHandedBackAndReused(t *testing.T) {
	st, _ := session.NewStore(context.Background(), nil)
	a := st.NewID()
	st.GiveBack(a)
	if st.NewID() != a {
		t.Fatal("a given-back id was not reused")
	}
	st.Adopt(grant(a, nil, 0, client.Limits{Tools: []string{}}), time.Now(), nil, 0)
	st.GiveBack(a)
	if st.NewID() == a {
		t.Fatal("an id in use was handed out")
	}
}

func TestSweepClosesTheExpired(t *testing.T) {
	st, _ := session.NewStore(context.Background(), nil)
	g := grant("s-old", nil, 0, client.Limits{Tools: []string{}})
	g.ExpiresAt = time.Now().Add(-time.Minute)
	st.Adopt(g, time.Now().Add(-time.Hour), nil, 0)
	st.Adopt(grant("s-new", nil, 0, client.Limits{Tools: []string{}}), time.Now(), nil, 0)
	if gone := st.Sweep(time.Now()); len(gone) != 1 || gone[0] != "s-old" || !st.Get("s-old").Closed || st.Get("s-new").Closed {
		t.Fatalf("%v", gone)
	}
}
