package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// ---- a rig for the people's side ---------------------------------------------------------------------------------------

// gatedStore is the AgentRun store with a hook inside Create: a test stops a spawn or a launch mid-way with channels.
type gatedStore struct {
	*runs.FakeStore
	onCreate func(name string)
}

func (g *gatedStore) Create(ctx context.Context, m map[string]any) error {
	if g.onCreate != nil {
		g.onCreate(m["metadata"].(map[string]any)["name"].(string))
	}
	return g.FakeStore.Create(ctx, m)
}

type fakeDir map[string][]string

func (d fakeDir) Groups(_ context.Context, person string) map[string]bool {
	out := map[string]bool{}
	for _, g := range d[person] {
		out[g] = true
	}
	return out
}

// peopleRig is the tree rig (user:alice's orchestrator s-0000a1 with a run) with an inbox, holds, a directory, delegates and admins.
type peopleRig struct {
	*runRig
	oc    *ownerCore
	inbox *dispatch.MemoryInbox
	holds *dispatch.MemoryHolds
	dir   fakeDir
	mu    sync.Mutex
	evals []*client.EvaluationRequest
	// openGrant, when set, changes the grant a session.open gets (a hostile core).
	openGrant func(req *client.EvaluationRequest, g *client.Grant)
	// beforeOpen, when set, runs inside the core's evaluation of a session.open (a test's gate).
	beforeOpen func(req *client.EvaluationRequest)
	opens      atomic.Int64
}

const (
	delegateID = "hangar-prod/tower/backend" // Tower's backend: another cluster's service account
	adminID    = "user:root"
)

func newPeopleRig(t *testing.T, opts ...rigOpt) *peopleRig {
	t.Helper()
	r, oc := treeRig(t, nil, opts...)
	p := &peopleRig{runRig: r, oc: oc, inbox: &dispatch.MemoryInbox{}, holds: &dispatch.MemoryHolds{}, dir: fakeDir{}}
	r.fc.Decide = p.decide
	r.g.SetInbox(p.inbox, p.dir, []string{"hangar-prod/tower/*"})
	r.g.Admins = []string{adminID}
	r.g.Holds = p.holds
	r.g.Fingerprint = dispatch.Fingerprint
	return p
}

func (p *peopleRig) decide(req *client.EvaluationRequest) client.EvaluationResult {
	if req.Action.Name != "session.open" {
		return p.oc.decide(req)
	}
	p.mu.Lock()
	p.evals = append(p.evals, req)
	p.mu.Unlock()
	p.opens.Add(1)
	if p.beforeOpen != nil {
		p.beforeOpen(req)
	}
	id := fmt.Sprintf("d-open-%d", p.opens.Load())
	ref := &client.RecordRef{Seq: p.opens.Load(), Hash: "h"}
	if h := req.Context.Hold; h != nil {
		return client.EvaluationResult{DecisionID: &id, Record: ref, Context: client.DecisionContext{Rule: "R024",
			Message: fmt.Sprintf("triggers held by %s until %s: %s", h.By, h.Until.Format(time.RFC3339), h.Reason)}}
	}
	d := req.Context.Agent
	if d == nil {
		return client.EvaluationResult{DecisionID: &id, Record: ref, Context: client.DecisionContext{Rule: "R003", Message: "unknown agent"}}
	}
	var g *client.Grant
	if l := req.Context.Live; l != nil {
		g = &client.Grant{Session: l.ID, TaskID: l.TaskID, Principal: l.Principal, Agent: l.Agent, Limits: l.Limits, ExpiresAt: l.ExpiresAt, Deduped: true}
	} else {
		principal := req.Subject.ID
		if sa := d.ServiceAccount; sa != nil && strings.HasPrefix(principal, "trigger:") {
			principal = *sa
		}
		task := req.Action.Properties.TaskID
		if task == "" {
			task = "t-" + req.Context.NewSessionID[2:]
		}
		g = &client.Grant{Session: req.Context.NewSessionID, TaskID: task, Principal: principal, Agent: d.Name, Limits: d.Limits,
			ExpiresAt: req.Context.Now.Add(time.Duration(d.Limits.TTLMinutes) * time.Minute)}
	}
	if p.openGrant != nil {
		p.openGrant(req, g)
	}
	return client.EvaluationResult{Decision: true, DecisionID: &id, Record: ref, Context: client.DecisionContext{Message: "allowed", Grant: g}}
}

func (p *peopleRig) lastOpen() *client.EvaluationRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evals[len(p.evals)-1]
}

func (p *peopleRig) recordsOf(kind string) []map[string]any {
	var out []map[string]any
	for _, rec := range p.records() {
		if rec["kind"] == kind {
			out = append(out, rec)
		}
	}
	return out
}

func ctlRule(err error) string {
	var ce *dispatch.ControlError
	var ie *dispatch.InboxError
	switch {
	case errors.As(err, &ce):
		return ce.Rule
	case errors.As(err, &ie):
		return ie.Rule
	case err == nil:
		return ""
	}
	return "error:" + err.Error()
}

func ctlStatus(err error) int {
	var ce *dispatch.ControlError
	var ie *dispatch.InboxError
	switch {
	case errors.As(err, &ce):
		return ce.Status
	case errors.As(err, &ie):
		return ie.Status
	}
	return 0
}

var ctx = context.Background()

// ---- who may do what ----------------------------------------------------------------------------------------------------

// The person controls are for people. For each way a caller might try (another person, a run, a service account with no pod, a
// foreign-cluster principal, a delegate that names nobody, a delegate that names someone who may not, an unlisted workload that
// names an admin, a person whose id looks like a run's), and for each control, the answer is the Python gateway's refusal or
// stricter. Each call that is refused writes no record and changes nothing.
func TestPersonControlsAreForPeopleOnly(t *testing.T) {
	type caller struct {
		name, tok, onBehalf string
	}
	build := func(p *peopleRig) []caller {
		p.dir["user:grace"] = []string{"group:default/team-platform"}
		return []caller{
			{"the session's person", p.person("user:alice"), ""},
			{"another person", p.person("user:bob"), ""},
			{"a platform admin", p.person(adminID), ""},
			{"a member of the agent's owner group", p.person("user:grace"), ""},
			{"the run itself, bound to its pod", p.pod("r-0000a1"), ""},
			{"the run, naming the admin", p.pod("r-0000a1"), adminID},
			{"the run, naming its own person", p.pod("r-0000a1"), "user:alice"},
			{"a service account bound to no pod", p.who("agent-r-0000a1/run", "workload"), ""},
			{"another cluster's workload that is not a delegate", p.who("hangar-prod/other/thing", "workload"), adminID},
			{"a delegate naming nobody", p.who(delegateID, "workload"), ""},
			{"a delegate naming the session's person", p.who(delegateID, "workload"), "user:alice"},
			{"a delegate naming another person", p.who(delegateID, "workload"), "user:bob"},
			{"a delegate naming an admin", p.who(delegateID, "workload"), adminID},
			{"a delegate naming a group member", p.who(delegateID, "workload"), "user:grace"},
			{"a delegate naming something that is not a person", p.who(delegateID, "workload"), "agent-r-0000a1/run"},
			{"a delegate naming an empty person", p.who(delegateID, "workload"), "user:"},
			{"a person whose id looks like a run's", p.who("agent-r-0000a1/run", "user"), ""},
			{"a person naming someone on behalf (not a delegate)", p.person("user:alice"), adminID},
			{"nobody (a token the authenticator refuses)", "no-such-token", ""},
		}
	}
	type control struct {
		name string
		do   func(p *peopleRig, c caller) error
		// who is allowed: the callers (by name) for which the control must work
		allowed map[string]bool
	}
	ownSession := map[string]bool{"the session's person": true, "a platform admin": true, "a member of the agent's owner group": true,
		"a delegate naming the session's person": true, "a delegate naming an admin": true, "a delegate naming a group member": true}
	agentOnly := map[string]bool{"a platform admin": true, "a member of the agent's owner group": true, "a delegate naming an admin": true,
		"a delegate naming a group member": true}
	controls := []control{
		{"freeze", func(p *peopleRig, c caller) error {
			_, err := p.g.FreezeSession(ctx, c.tok, "s-0000a1", "it opens bad PRs", c.onBehalf)
			return err
		}, ownSession},
		{"end", func(p *peopleRig, c caller) error {
			_, err := p.g.EndSession(ctx, c.tok, "s-0000a1", "done here", c.onBehalf)
			return err
		}, ownSession},
		{"hold", func(p *peopleRig, c caller) error {
			_, err := p.g.HoldTriggers(ctx, c.tok, "triage-agent", "storm", 2, c.onBehalf)
			return err
		}, agentOnly},
		{"release", func(p *peopleRig, c caller) error {
			_ = p.holds.Put(ctx, &dispatch.Hold{Agent: "triage-agent", By: "user:x", Reason: "r", Since: t0, Until: t0.Add(time.Hour)})
			_, err := p.g.ReleaseTriggers(ctx, c.tok, "triage-agent", c.onBehalf)
			return err
		}, agentOnly},
		{"run_agent", func(p *peopleRig, c caller) error {
			_, err := p.g.RunAgent(ctx, c.tok, "researcher", "hello", c.onBehalf, nil, nil)
			return err
		}, agentOnly},
		{"answer", func(p *peopleRig, c caller) error {
			// (an admin is not an answerer: only the session's owner and the agent's owner group are)
			// a request of s-0000a1 (alice's session; its agent is owned by the group)
			if r, _ := p.inbox.Get(ctx, "h-000000000001"); r == nil {
				_ = p.inbox.Add(ctx, &dispatch.HumanRequest{ID: "h-000000000001", TaskID: "t-0000a1", Session: "s-0000a1", Agent: "orchestrator", Owner: "user:alice",
					Group: ptr("group:default/team-platform"), Question: "q?", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), Status: "pending"})
			}
			_, err := p.g.HumanAnswer(ctx, c.tok, "h-000000000001", "yes", c.onBehalf)
			return err
		}, map[string]bool{"the session's person": true, "a member of the agent's owner group": true,
			"a delegate naming the session's person": true, "a delegate naming a group member": true}},
	}
	for _, ctl := range controls {
		// one fresh rig per caller: a control that worked changes the state the next attempt sees
		p0 := newPeopleRig(t)
		for _, c := range build(p0) {
			p := newPeopleRig(t)
			build(p)
			c := caller{c.name, tokenIn(p, p0, c), c.onBehalf}
			before := len(p.records())
			err := ctl.do(p, c)
			if ctl.allowed[c.name] {
				if err != nil {
					t.Errorf("%s / %s: refused: %v", ctl.name, c.name, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s / %s: allowed, and it must not be", ctl.name, c.name)
				continue
			}
			if ctlStatus(err) == 0 {
				var ae *core.AuthError
				if !errors.As(err, &ae) {
					t.Errorf("%s / %s: a refusal with no status: %v", ctl.name, c.name, err)
				}
			}
			if got := len(p.records()); got != before {
				t.Errorf("%s / %s: a refused call wrote %d record(s)", ctl.name, c.name, got-before)
			}
			// nothing changed
			s := p.store.Get("s-0000a1")
			s.Lock()
			changed := s.Closed || s.Tripped
			s.Unlock()
			if changed {
				t.Errorf("%s / %s: refused, and the session changed", ctl.name, c.name)
			}
			if h, _ := p.holds.Get(ctx, "triage-agent"); ctl.name == "hold" && h != nil {
				t.Errorf("%s / %s: refused, and a hold was saved", ctl.name, c.name)
			}
			if ctl.name == "run_agent" && p.opens.Load() != 0 {
				t.Errorf("%s / %s: refused, and the core was asked to open a session", ctl.name, c.name)
			}
			if ctl.name == "answer" {
				if r, _ := p.inbox.Get(ctx, "h-000000000001"); r != nil && r.Status != "pending" {
					t.Errorf("%s / %s: refused, and the request was answered", ctl.name, c.name)
				}
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// tokenIn maps a caller built on one rig to the same caller on another (tokens are registered per rig; they are names).
func tokenIn(to, from *peopleRig, c struct{ name, tok, onBehalf string }) string {
	for k, v := range from.tokens {
		if k == c.tok {
			to.tokens[k] = v
		}
	}
	return c.tok
}

// The rule, field by field, for the cases the matrix cannot say in one line.
func TestWhoIsAPerson(t *testing.T) {
	p := newPeopleRig(t)
	cases := []struct {
		name, tok, on string
		person, via   string
		rule          string
	}{
		{"a person", p.person("user:alice"), "", "user:alice", "", ""},
		{"a delegate names a person", p.who(delegateID, "workload"), "user:alice", "user:alice", delegateID, ""},
		{"a workload is not a person", p.who("ns/sa", "workload"), "", "", "", "HR-PERSON"},
		{"a run is not a person", p.pod("r-0000a1"), "", "", "", "HR-PERSON"},
		{"a workload that is not a delegate may not name one", p.who("ns/sa", "workload"), "user:alice", "", "", "HR-DELEGATE"},
		{"a person that is not a delegate may not name one", p.person("user:bob"), "user:alice", "", "", "HR-DELEGATE"},
		{"a delegate must name user:...", p.who(delegateID, "workload"), "alice", "", "", "HR-PERSON"},
		{"a delegate must name a name", p.who(delegateID, "workload"), "user:", "", "", "HR-PERSON"},
		{"the name is bounded", p.who(delegateID, "workload"), "user:" + strings.Repeat("a", 200), "", "", "HR-PERSON"},
		{"the name has no newline", p.who(delegateID, "workload"), "user:a\nb", "", "", "HR-PERSON"},
	}
	for _, c := range cases {
		person, via, err := p.g.Person(ctx, c.tok, c.on)
		if ctlRule(err) != c.rule || person != c.person || via != c.via {
			t.Errorf("%s: %q %q %v", c.name, person, via, err)
		}
	}
	if _, _, err := p.g.Person(ctx, "unknown", ""); err == nil {
		t.Error("a token nobody vouches for is a person")
	} else if ctlStatus(err) != 0 {
		t.Errorf("an unauthenticated caller is told no more than 'not authenticated': %v", err)
	}
}

// Python answers 404 for an unknown session before it looks at the token (so an unauthenticated caller learns which sessions
// exist), and "not held" for a release before it looks at who asks. Go authenticates first, and tells a person who may not manage
// the agent nothing about its holds.
func TestAnUnauthenticatedCallerLearnsNothing(t *testing.T) {
	p := newPeopleRig(t)
	for name, do := range map[string]func() error{
		"freeze of a session that exists":  func() error { _, err := p.g.FreezeSession(ctx, "bad", "s-0000a1", "r", ""); return err },
		"freeze of one that does not":      func() error { _, err := p.g.FreezeSession(ctx, "bad", "s-nope", "r", ""); return err },
		"end of one that does not":         func() error { _, err := p.g.EndSession(ctx, "bad", "s-nope", "r", ""); return err },
		"hold of an agent that does not":   func() error { _, err := p.g.HoldTriggers(ctx, "bad", "nope", "r", 1, ""); return err },
		"release of an agent not held":     func() error { _, err := p.g.ReleaseTriggers(ctx, "bad", "triage-agent", ""); return err },
		"run of an agent that does not":    func() error { _, err := p.g.RunAgent(ctx, "bad", "nope", "x", "", nil, nil); return err },
		"answer of a request that is none": func() error { _, err := p.g.HumanAnswer(ctx, "bad", "h-000000000001", "x", ""); return err },
	} {
		var ae *core.AuthError
		if err := do(); !errors.As(err, &ae) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a person who may not manage an agent cannot tell whether it is held, or whether it exists
	_ = p.holds.Put(ctx, &dispatch.Hold{Agent: "triage-agent", By: "user:x", Reason: "r", Since: t0, Until: t0.Add(time.Hour)})
	bob := p.person("user:bob")
	_, e1 := p.g.ReleaseTriggers(ctx, bob, "triage-agent", "")
	_, e2 := p.g.ReleaseTriggers(ctx, bob, "nope", "")
	if ctlRule(e1) != "CT-FORBIDDEN" || ctlRule(e2) != "CT-FORBIDDEN" {
		t.Errorf("%v %v", e1, e2)
	}
}

// ---- freeze and end ------------------------------------------------------------------------------------------------------

func TestFreezeTripsTheSessionAndItsChildrenOnceAndRecordsIt(t *testing.T) {
	p := newPeopleRig(t)
	child := sessionRow("s-0000c1", "user:alice", "researcher", "r-0000c1", func(r *session.Record) { id := "s-0000a1"; r.ParentID = &id; r.Depth = 1 })
	parent, _ := p.persist.row("s-0000a1")
	parent.Children = []string{"s-0000c1"}
	_ = p.persist.Save(ctx, []session.Record{parent, child})
	st, _ := session.NewStore(ctx, p.persist)
	p.g.Store, p.store = st, st
	alice := p.person("user:alice")
	if _, err := p.g.FreezeSession(ctx, alice, "s-0000a1", "  ", ""); ctlRule(err) != "CT-REASON" {
		t.Fatalf("%v", err)
	}
	if _, err := p.g.FreezeSession(ctx, alice, "s-nope", "x", ""); ctlRule(err) != "CT-UNKNOWN" || ctlStatus(err) != 404 {
		t.Fatalf("%v", err)
	}
	out, err := p.g.FreezeSession(ctx, p.who(delegateID, "workload"), "s-0000a1", "it opens bad PRs", "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	if out["tripped"] != true || out["trip_reason"] != "frozen by user:alice: it opens bad PRs" || out["agent"] != "orchestrator" {
		t.Fatalf("%v", out)
	}
	if s := p.store.Get("s-0000c1"); !s.Tripped || *s.TripReason != "parent tripped: frozen by user:alice: it opens bad PRs" {
		t.Fatalf("the child: %+v", s.TripReason)
	}
	recs := p.recordsOf("session.freeze")
	if len(recs) != 1 || recs[0]["principal"] != "user:alice" || recs[0]["result"] != "frozen: it opens bad PRs via "+delegateID || recs[0]["target"] != "s-0000a1" || recs[0]["session"] != "s-0000a1" {
		t.Fatalf("%v", recs)
	}
	// saved: a restart sees it frozen
	if row, _ := p.persist.row("s-0000a1"); !row.Tripped {
		t.Fatal("the freeze was not saved")
	}
	if _, err := p.g.FreezeSession(ctx, alice, "s-0000a1", "again", ""); ctlRule(err) != "CT-TRIPPED" || ctlStatus(err) != 409 {
		t.Fatalf("%v", err)
	}
	// a frozen session's further calls are the core's to refuse by its state; the gateway sends the tripped state
	p.call(p.pod("r-0000a1"), "s-0000a1", "argo.app.get", map[string]any{"app": "x"})
	if !p.oc.last().Context.Session.Tripped {
		t.Fatal("the core was not told the session is frozen")
	}
}

func TestEndClosesTheSessionAndItsChildrenOnceAndGivesTheBudgetBack(t *testing.T) {
	p := newPeopleRig(t)
	parent, _ := p.persist.row("s-0000a1")
	parent.Children = []string{"s-0000c1"}
	parent.Reserved = map[string]int64{"tool_calls": 10, "github_calls": 0, "open_prs": 0, "model_tokens": 0, "children": 0}
	child := sessionRow("s-0000c1", "user:alice", "researcher", "r-0000c1", func(r *session.Record) {
		id := "s-0000a1"
		r.ParentID, r.Depth = &id, 1
		r.Limits.ToolCalls = 10
		r.Used = map[string]int64{"tool_calls": 4}
	})
	_ = p.persist.Save(ctx, []session.Record{parent, child})
	st, _ := session.NewStore(ctx, p.persist)
	p.g.Store, p.store = st, st
	out, err := p.g.EndSession(ctx, p.person(adminID), "s-0000c1", "enough", "")
	if err != nil {
		t.Fatal(err)
	}
	if out["closed"] != true || out["session"] != "s-0000c1" {
		t.Fatalf("%v", out)
	}
	if row, _ := p.persist.row("s-0000c1"); !row.Closed {
		t.Fatal("not saved closed")
	}
	if row, _ := p.persist.row("s-0000a1"); row.Reserved["tool_calls"] != 0 || row.Used["tool_calls"] != 4 {
		t.Fatalf("the parent did not get the budget back, saved with it: %+v %+v", row.Reserved, row.Used)
	}
	if _, err := p.g.EndSession(ctx, p.person(adminID), "s-0000c1", "again", ""); ctlRule(err) != "CT-CLOSED" || ctlStatus(err) != 409 {
		t.Fatalf("%v", err)
	}
	if recs := p.recordsOf("session.end"); len(recs) != 1 || recs[0]["result"] != "ended: enough" {
		t.Fatalf("%v", recs)
	}
	// the sweep deletes the run of a closed session (Python: the same, with why "expired")
	p.g.Expire(ctx)
	if p.api.Manifests[runName("s-0000c1")] != nil || len(p.api.Deleted) == 0 {
		t.Fatal("the run of an ended session was not deleted by the sweep")
	}
	// a frozen session can be ended; a closed one cannot be frozen
	if _, err := p.g.FreezeSession(ctx, p.person(adminID), "s-0000c1", "x", ""); ctlRule(err) != "CT-CLOSED" {
		t.Fatalf("%v", err)
	}
}

// A core that is down does not undo a control: the record waits in the outbox, the answer still carries no audit_seq.
func TestAControlStandsWhenTheCoreIsDown(t *testing.T) {
	p := newPeopleRig(t)
	p.fc.Down.Store(true)
	out, err := p.g.FreezeSession(ctx, p.person(adminID), "s-0000a1", "x", "")
	if err != nil || out["tripped"] != true || out["audit_seq"] != nil {
		t.Fatalf("%v %v", out, err)
	}
	if _, err := p.g.EndSession(ctx, p.person(adminID), "s-0000a1", "x", ""); err != nil {
		t.Fatal(err)
	}
}

// Two controls racing for one session: exactly one wins, the other is told why, and one record is written. Run with -race.
func TestRacingFreezesAndEndsGiveOneOutcomeEach(t *testing.T) {
	for _, op := range []string{"freeze", "end"} {
		p := newPeopleRig(t)
		root := p.person(adminID)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var ok, refused atomic.Int64
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var err error
				if op == "freeze" {
					_, err = p.g.FreezeSession(ctx, root, "s-0000a1", "race", "")
				} else {
					_, err = p.g.EndSession(ctx, root, "s-0000a1", "race", "")
				}
				switch {
				case err == nil:
					ok.Add(1)
				case ctlRule(err) == "CT-TRIPPED" || ctlRule(err) == "CT-CLOSED":
					refused.Add(1)
				default:
					t.Errorf("%s: %v", op, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		kind := map[string]string{"freeze": "session.freeze", "end": "session.end"}[op]
		if ok.Load() != 1 || refused.Load() != 7 || len(p.recordsOf(kind)) != 1 {
			t.Errorf("%s: %d won, %d refused, %d records", op, ok.Load(), refused.Load(), len(p.recordsOf(kind)))
		}
	}
}

// An End that arrives while a spawn is creating its child's run (gated inside the run backend): the child is closed by the End, the
// spawn then finds it closed and deletes the run, nothing is left live, and the parent got its reservation back once.
func TestEndDuringASpawnLeavesNoLiveRun(t *testing.T) {
	p := newPeopleRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p.g.SetRuns(&runs.Backend{Store: &gatedStore{FakeStore: p.api, onCreate: func(name string) {
		if name != "r-0000a1" {
			close(entered)
			<-release
		}
	}}})
	done := make(chan *dispatch.Result, 1)
	go func() {
		done <- p.call(p.pod("r-0000a1"), "s-0000a1", "run.spawn", map[string]any{"agent": "researcher", "limits": map[string]any{}, "input": map[string]any{"prompt": "go"}})
	}()
	<-entered
	var child string
	for _, s := range p.store.All() {
		if s.ParentID != nil {
			child = s.ID
		}
	}
	if child == "" {
		t.Fatal("no child exists while its run is being made")
	}
	if _, err := p.g.EndSession(ctx, p.person(adminID), child, "stop", ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	res := <-done
	if res.OK {
		t.Fatalf("a spawn whose child was ended succeeded: %v", res)
	}
	p.g.Expire(ctx)
	if len(p.api.Manifests) != 0 {
		t.Fatalf("a run is still live: %v", p.api.Manifests)
	}
	parent := p.store.Get("s-0000a1")
	parent.Lock()
	defer parent.Unlock()
	if parent.Reserved.ToolCalls != 0 {
		t.Fatalf("the reservation did not come back: %+v", parent.Reserved)
	}
}

// ---- holds ---------------------------------------------------------------------------------------------------------------

func TestHoldAndRelease(t *testing.T) {
	p := newPeopleRig(t)
	root := p.person(adminID)
	if _, err := p.g.HoldTriggers(ctx, root, "nope", "x", nil, ""); ctlRule(err) != "CT-UNKNOWN" {
		t.Fatalf("%v", err)
	}
	for hours, rule := range map[any]string{json.Number("0"): "CT-HOURS", json.Number("337"): "CT-HOURS", "x": "CT-HOURS", 2.5: "", "": "", nil: "", true: "", "7": "", json.Number("1e400"): "CT-HOURS"} {
		_, err := p.g.HoldTriggers(ctx, root, "triage-agent", "storm", hours, "")
		if ctlRule(err) != rule {
			t.Errorf("hours %v: %v", hours, err)
		}
	}
	if _, err := p.g.HoldTriggers(ctx, root, "triage-agent", strings.Repeat("x", 501), 1, ""); ctlRule(err) != "CT-REASON" {
		t.Fatal(err)
	}
	out, err := p.g.HoldTriggers(ctx, p.who(delegateID, "workload"), "triage-agent", "storm", 3, adminID)
	if err != nil {
		t.Fatal(err)
	}
	if out["by"] != adminID || out["via"] != delegateID || out["until"] != "2026-09-26T15:00:00+00:00" || out["since"] != "2026-09-26T12:00:00+00:00" {
		t.Fatalf("%v", out)
	}
	var last map[string]any
	for _, rec := range p.recordsOf("trigger.hold") {
		if strings.Contains(rec["result"].(string), "15:00:00Z") {
			last = rec
		}
	}
	if last["target"] != "triage-agent" || last["session"] != nil || last["result"] != "held until 2026-09-26T15:00:00Z: storm via "+delegateID {
		t.Fatalf("%v", last)
	}
	if h, _ := p.g.HoldFor(ctx, "triage-agent"); h == nil || h.By != adminID {
		t.Fatal("not held")
	}
	// held: a trigger's open tells the core, which refuses (R024)
	res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
	if err != nil || res.OK || res.Decision["rule"] != "R024" || p.lastOpen().Context.Hold == nil || p.lastOpen().Context.Hold.By != adminID {
		t.Fatalf("%+v %v", res, err)
	}
	// and a person's run of a held agent is R024 before anything is opened
	opens := p.opens.Load()
	if _, err := p.g.RunAgent(ctx, root, "researcher", "x", "", nil, nil); err != nil {
		t.Fatalf("researcher is not held: %v", err)
	}
	_ = p.holds.Put(ctx, &dispatch.Hold{Agent: "researcher", By: adminID, Reason: "no", Since: t0, Until: t0.Add(time.Hour)})
	if _, err := p.g.RunAgent(ctx, root, "researcher", "x", "", nil, nil); ctlRule(err) != "R024" || p.opens.Load() != opens+1 {
		t.Fatalf("%v", err)
	}
	// release
	if _, err := p.g.ReleaseTriggers(ctx, p.person("user:bob"), "triage-agent", ""); ctlRule(err) != "CT-FORBIDDEN" {
		t.Fatal(err)
	}
	if _, err := p.g.ReleaseTriggers(ctx, root, "triage-agent", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.g.ReleaseTriggers(ctx, root, "triage-agent", ""); ctlRule(err) != "CT-NOHOLD" || ctlStatus(err) != 404 {
		t.Fatalf("%v", err)
	}
	if rec := p.recordsOf("trigger.release"); len(rec) != 1 || !strings.HasPrefix(rec[0]["result"].(string), "released (held by user:root since 2026-09-26T12:00:00Z: storm") {
		t.Fatalf("%v", rec)
	}
	// an expired hold holds nothing, and the sweep drops it
	_ = p.holds.Put(ctx, &dispatch.Hold{Agent: "triage-agent", By: adminID, Reason: "old", Since: t0.Add(-2 * time.Hour), Until: t0.Add(-time.Hour)})
	if h, _ := p.g.HoldFor(ctx, "triage-agent"); h != nil {
		t.Fatal("an expired hold holds")
	}
	if p.g.PruneHolds(ctx) != 1 {
		t.Fatal("the expired hold was not dropped")
	}
	// a deployment that keeps no holds
	p.g.Holds = nil
	if _, err := p.g.HoldTriggers(ctx, root, "triage-agent", "x", 1, ""); ctlRule(err) != "CT-OFF" {
		t.Fatal(err)
	}
}

// A hold and a trigger's open are ordered: when the hold call returns, no open that read "no hold" is still in flight, and every
// later open sees the hold. The open is held inside the core's evaluation by a channel, not a sleep.
func TestAHoldWaitsForAnOpenInFlightAndLaterOpensSeeIt(t *testing.T) {
	p := newPeopleRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p.beforeOpen = func(*client.EvaluationRequest) {
		if p.opens.Load() == 1 {
			close(entered)
			<-release
		}
	}
	opened := make(chan *dispatch.Result, 1)
	go func() {
		res, _ := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
		opened <- res
	}()
	<-entered
	held := make(chan error, 1)
	go func() {
		_, err := p.g.HoldTriggers(ctx, p.person(adminID), "triage-agent", "storm", 1, "")
		held <- err
	}()
	select {
	case err := <-held:
		t.Fatalf("the hold returned while an open that read no hold was still in flight: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if res := <-opened; !res.OK {
		t.Fatalf("%+v", res)
	}
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	res, _ := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e2", "t-bbbbbbbbbbbb")
	if res.OK || res.Decision["rule"] != "R024" {
		t.Fatalf("%+v", res)
	}
}

// ---- open_triggered ------------------------------------------------------------------------------------------------------

func TestOpenTriggeredOpensDedupesAndChecksTheGrant(t *testing.T) {
	p := newPeopleRig(t)
	res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	req := p.lastOpen()
	if req.Subject.ID != "trigger:glidepath:e1" || req.Subject.Properties.Kind != "workload" || req.Subject.Properties.TierCeiling != 1 ||
		req.Action.Properties.Trigger != "glidepath:e1" || req.Action.Properties.TaskID != "t-aaaaaaaaaaaa" || req.Context.Agent.Name != "triage-agent" ||
		req.Context.Hold != nil || req.Context.Live != nil {
		t.Fatalf("%+v", req)
	}
	data := res.Data.(map[string]any)
	sid := data["session"].(string)
	s := p.store.Get(sid)
	if s == nil || s.Agent != "triage-agent" || s.TaskID != "t-aaaaaaaaaaaa" || data["deduped"] != false {
		t.Fatalf("%v %+v", data, s)
	}
	if row, ok := p.persist.row(sid); !ok || row.Principal != "autopilot-runs/triage-agent" {
		t.Fatal("the session was not saved, or its principal is not the definition's own identity")
	}
	// the same task again: the core is shown the live session and answers a dedupe; no second session
	n := len(p.store.All())
	res, err = p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
	if err != nil || !res.OK || res.Data.(map[string]any)["deduped"] != true || res.Data.(map[string]any)["session"] != sid {
		t.Fatalf("%+v %v", res, err)
	}
	if p.lastOpen().Context.Live == nil || p.lastOpen().Context.Live.ID != sid || len(p.store.All()) != n {
		t.Fatal("not shown the live session, or a session was made")
	}
	// a session that ended is not live: a new one
	p.store.Close(sid)
	if res, _ = p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa"); res.Data.(map[string]any)["deduped"] != false {
		t.Fatalf("%+v", res)
	}
	// a trigger name is bounded
	if _, err := p.g.OpenTriggered(ctx, "triage-agent", strings.Repeat("x", 301), "t-cccccccccccc"); err == nil {
		t.Fatal("a 301 character trigger name was taken")
	}
}

// What the core grants a trigger is checked: whatever the trigger's source sent, the agent is the one asked for, the principal is
// the definition's own identity, the limits are no wider than the definition's, the session id is the one the gateway named.
func TestOpenTriggeredRefusesWhatTheCoreShouldNotHaveGranted(t *testing.T) {
	d := loadDefsOnce["triage-agent"].Limits
	cases := map[string]func(g *client.Grant){
		"another agent":               func(g *client.Grant) { g.Agent = "coding-agent" },
		"another principal":           func(g *client.Grant) { g.Principal = "user:root" },
		"a parent":                    func(g *client.Grant) { p := "s-0000a1"; g.Parent = &p },
		"a depth":                     func(g *client.Grant) { g.Depth = 1 },
		"another session id":          func(g *client.Grant) { g.Session = "s-0000a1" },
		"a tool the definition lacks": func(g *client.Grant) { g.Limits.Tools = append(append([]string(nil), d.Tools...), "repo.pr.merge") },
		"more calls":                  func(g *client.Grant) { g.Limits.ToolCalls = d.ToolCalls + 1 },
		"more tokens":                 func(g *client.Grant) { g.Limits.ModelTokens = d.ModelTokens * 2 },
		"a higher tier":               func(g *client.Grant) { g.Limits.TierCeiling = "T3" },
		"a longer ttl": func(g *client.Grant) {
			g.Limits.TTLMinutes = d.TTLMinutes + 60
			g.ExpiresAt = g.ExpiresAt.Add(time.Hour)
		},
		"a deadline past the ttl": func(g *client.Grant) { g.ExpiresAt = g.ExpiresAt.Add(time.Hour) },
		"a bigger network":        func(g *client.Grant) { g.Limits.Network = "allowlist" },
		"another task":            func(g *client.Grant) { g.TaskID = "t-ffffffffffff" },
	}
	for name, mod := range cases {
		p := newPeopleRig(t)
		p.openGrant = func(_ *client.EvaluationRequest, g *client.Grant) { mod(g) }
		before := len(p.store.All())
		res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
		if err != nil || res.OK || res.Decision["rule"] != core.RefusedRule {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		if len(p.store.All()) != before {
			t.Errorf("%s: a session was made", name)
		}
	}
	// a dedupe the gateway did not offer: the core names a session this gateway does not hold, or one that is not the live one
	p := newPeopleRig(t)
	p.openGrant = func(_ *client.EvaluationRequest, g *client.Grant) { g.Deduped = true }
	if res, _ := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa"); res.OK {
		t.Errorf("a dedupe with nothing live was accepted: %+v", res)
	}
	// an agent that is not a live definition has nothing to check a grant against
	p = newPeopleRig(t)
	if res, _ := p.g.OpenTriggered(ctx, "no-such-agent", "glidepath:e1", "t-aaaaaaaaaaaa"); res.OK {
		t.Errorf("%+v", res)
	}
}

func TestOpenTriggeredWhenTheCoreIsDown(t *testing.T) {
	p := newPeopleRig(t)
	p.fc.Down.Store(true)
	res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
	if err != nil || res.OK || res.Decision["rule"] != core.OutageRule {
		t.Fatalf("%+v %v", res, err)
	}
	if len(p.store.All()) != 1 {
		t.Fatal("a session was made with no decision")
	}
}

// ---- run_agent -----------------------------------------------------------------------------------------------------------

func TestRunAgentByThePersonRouteChecksWhoAndWhat(t *testing.T) {
	p := newPeopleRig(t)
	root := p.person(adminID)
	view, err := p.g.RunAgent(ctx, p.who(delegateID, "workload"), "researcher", "what do you know?", adminID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := p.lastOpen()
	if req.Subject.ID != adminID || !req.Action.Properties.Run || req.Action.Properties.Via != delegateID || view["by"] != adminID || view["via"] != delegateID {
		t.Fatalf("%+v %v", req, view)
	}
	if recs := p.recordsOf("run.launch"); len(recs) != 1 {
		t.Fatalf("%v", recs)
	}
	for _, c := range []struct {
		name  string
		agent string
		input any
		rule  string
	}{
		{"an agent that does not exist", "nope", "x", "R003"},
		{"an event agent", "triage-agent", "x", "R001"},
		{"a scheduled agent", "nightly-reviewer", "x", "R001"},
		{"an input that is not a string", "researcher", 5, "R008"},
		{"an input that is too long", "researcher", strings.Repeat("x", dispatch.MaxRunInput+1), "R008"},
	} {
		if _, err := p.g.RunAgent(ctx, root, c.agent, c.input, "", nil, nil); ctlRule(err) != c.rule {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// a person that may not manage the agent is refused before the agent's kind is said
	if _, err := p.g.RunAgent(ctx, p.person("user:bob"), "triage-agent", "x", "", nil, nil); ctlRule(err) != "CT-FORBIDDEN" {
		t.Fatal(err)
	}
	// a run that cannot be launched leaves no live session behind
	n := len(p.store.All())
	p.api.Err = errors.New("API server 500")
	if _, err := p.g.RunAgent(ctx, root, "researcher", "x", "", nil, nil); err == nil {
		t.Fatal("a failed launch was reported as a run")
	}
	live := 0
	for _, s := range p.store.All() {
		s.Lock()
		if !s.Closed {
			live++
		}
		s.Unlock()
	}
	if live != n || len(p.store.All()) != n+1 {
		t.Fatalf("a session that was never launched is still live: %d live, %d sessions, was %d", live, len(p.store.All()), n)
	}
}

// A person's run is checked against the definition like a trigger's: a grant for another principal or a wider one is refused.
func TestRunAgentRefusesAGrantForSomeoneElseOrWider(t *testing.T) {
	for name, mod := range map[string]func(g *client.Grant){
		"another principal": func(g *client.Grant) { g.Principal = "user:bob" },
		"wider limits":      func(g *client.Grant) { g.Limits.ToolCalls += 100 },
		"another agent":     func(g *client.Grant) { g.Agent = "coding-agent" },
	} {
		p := newPeopleRig(t)
		p.openGrant = func(_ *client.EvaluationRequest, g *client.Grant) { mod(g) }
		n := len(p.store.All())
		if _, err := p.g.RunAgent(ctx, p.person(adminID), "researcher", "x", "", nil, nil); ctlRule(err) != core.RefusedRule {
			t.Errorf("%s: %v", name, err)
		}
		if len(p.store.All()) != n {
			t.Errorf("%s: a session was made", name)
		}
	}
}

// A candidate document is refused (501) after the person has been checked, because the definition schema (S4) is not carried; with a
// validated parse supplied (as S4 will) it runs clamped, and a refused request leaves no candidate behind.
func TestACandidateIsRefusedUntilTheSchemaIsCarriedThenRunsClamped(t *testing.T) {
	p := newPeopleRig(t)
	doc := map[string]any{"name": "researcher"}
	if _, err := p.g.RunAgent(ctx, p.person("user:bob"), "researcher", "x", "", doc, nil); ctlRule(err) != "CT-FORBIDDEN" {
		t.Fatalf("a person who may not manage the agent is told so first: %v", err)
	}
	_, err := p.g.RunAgent(ctx, p.person(adminID), "researcher", "x", "", doc, nil)
	if ctlRule(err) != "CT-NOTCARRIED" || ctlStatus(err) != 501 || p.opens.Load() != 0 {
		t.Fatalf("%v", err)
	}
	p.g.RuntimeImages = []string{"ghcr.io/jfillman/*"}
	p.g.CandidateParse = func(doc any) (*definitions.Definition, error) {
		d := *p.g.Defs["researcher"]
		return &d, nil
	}
	view, err := p.g.RunAgent(ctx, p.person(adminID), "researcher", "x", "", doc, map[string]any{"tool_calls": json.Number("5")})
	if err != nil {
		t.Fatal(err)
	}
	if view["preflight"] != true || !strings.HasPrefix(view["definition"].(string), "pf-researcher-") {
		t.Fatalf("%v", view)
	}
	lim := p.lastOpen().Context.Agent.Limits
	if lim.ToolCalls != 5 || lim.GithubCalls != 0 || lim.TierCeiling > "T1" {
		t.Fatalf("not clamped: %+v", lim)
	}
	// a candidate that is refused leaves nothing in the table: the open is refused by a hold on nothing, so make the launch fail
	p.api.Err = errors.New("API server 500")
	doc2 := map[string]any{"name": "researcher", "other": "doc"}
	p.g.Fingerprint = func(*definitions.Definition) string { return "99999999aaaaaaaa" }
	if _, err := p.g.RunAgent(ctx, p.person(adminID), "researcher", "x", "", doc2, nil); err == nil {
		t.Fatal("a failed launch was reported as a run")
	}
	if p.g.Def("pf-researcher-99999999") != nil {
		t.Fatal("a candidate whose run was refused stays in the table")
	}
}

// ---- the inbox -----------------------------------------------------------------------------------------------------------

func inboxSession(p *peopleRig) (string, string) {
	// researcher's definition grants human.request; give the session a task, a run and the tool
	row := sessionRow("s-0000b1", "user:alice", "researcher", "r-0000b1", func(r *session.Record) {
		r.Limits.Tools = append(r.Limits.Tools, "human.request", "human.request.get")
	})
	_ = p.persist.Save(ctx, []session.Record{row})
	st, _ := session.NewStore(ctx, p.persist)
	p.g.Store, p.store = st, st
	return "s-0000b1", p.pod("r-0000b1")
}

func TestHumanRequestAndAnswer(t *testing.T) {
	p := newPeopleRig(t)
	p.dir["user:grace"] = []string{"group:default/team-platform"}
	sid, pod := inboxSession(p)
	res := p.call(pod, sid, "human.request", map[string]any{"question": "  Which environment?  ", "options": []any{"dev", " prod "}})
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	d := res.Data.(map[string]any)
	id := d["request"].(string)
	if d["status"] != "pending" || d["expires_at"] != "2026-09-26T13:00:00+00:00" || !strings.HasPrefix(id, "h-") {
		t.Fatalf("%v", d)
	}
	// the run reads it back, and it is nobody else's
	got := p.call(pod, sid, "human.request.get", map[string]any{"request": id})
	if !got.OK || got.Data.(map[string]any)["question"] != "Which environment?" || got.Data.(map[string]any)["status"] != "pending" {
		t.Fatalf("%+v", got)
	}
	if res := p.call(pod, sid, "human.request.get", map[string]any{"request": "h-000000000000"}); res.OK || res.Decision["rule"] != "HR-UNKNOWN" {
		t.Fatalf("%+v", res)
	}
	// who sees it
	bob, alice, grace := p.person("user:bob"), p.person("user:alice"), p.person("user:grace")
	for who, want := range map[string]int{bob: 0, alice: 1, grace: 1} {
		list, err := p.g.HumanRequests(ctx, who, "")
		if err != nil || len(list) != want {
			t.Errorf("%s: %v %v", who, list, err)
		}
	}
	if _, err := p.g.HumanRequests(ctx, pod, ""); ctlRule(err) != "HR-PERSON" {
		t.Fatalf("a run lists the inbox: %v", err)
	}
	// answers: the run cannot, another person cannot, an answer outside the options cannot
	for name, c := range map[string]struct {
		tok, on string
		ans     any
		rule    string
	}{
		"the run itself":            {pod, "", "dev", "HR-PERSON"},
		"the run naming its person": {pod, "user:alice", "dev", "HR-DELEGATE"},
		"another person":            {bob, "", "dev", "HR-UNKNOWN"},
		"not one of the options":    {alice, "", "staging", "HR-ANSWER"},
		"no answer":                 {alice, "", nil, "HR-ANSWER"},
		"too long":                  {alice, "", strings.Repeat("x", dispatch.MaxAnswer+1), "HR-ANSWER"},
	} {
		if _, err := p.g.HumanAnswer(ctx, c.tok, id, c.ans, c.on); ctlRule(err) != c.rule {
			t.Errorf("%s: %v", name, err)
		}
		if len(p.recordsOf("human.answer")) != 0 {
			t.Fatalf("%s: a refused answer wrote a record", name)
		}
	}
	out, err := p.g.HumanAnswer(ctx, p.who(delegateID, "workload"), id, " prod ", "user:grace")
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "answered" || out["answer"] != "prod" || out["answered_by"] != "user:grace" || out["answered_via"] != delegateID {
		t.Fatalf("%v", out)
	}
	recs := p.recordsOf("human.answer")
	if len(recs) != 1 || recs[0]["principal"] != "user:grace" || recs[0]["result"] != "answered via "+delegateID || recs[0]["target"] != id ||
		recs[0]["args_sha256"] != core.ArgsDigest(map[string]any{"answer": "prod"}) {
		t.Fatalf("%v", recs)
	}
	if _, err := p.g.HumanAnswer(ctx, alice, id, "dev", ""); ctlRule(err) != "HR-ANSWERED" || ctlStatus(err) != 409 {
		t.Fatalf("a second answer: %v", err)
	}
	if got := p.call(pod, sid, "human.request.get", map[string]any{"request": id}); got.Data.(map[string]any)["answer"] != "prod" {
		t.Fatalf("%+v", got)
	}
}

func TestHumanRequestLimitsAndExpiry(t *testing.T) {
	p := newPeopleRig(t)
	sid, pod := inboxSession(p)
	bad := map[string]map[string]any{
		"no question":    {},
		"a long one":     {"question": strings.Repeat("q", dispatch.MaxQuestion+1)},
		"11 options":     {"question": "q", "options": []any{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}},
		"a duplicate":    {"question": "q", "options": []any{"a", "a"}},
		"a long option":  {"question": "q", "options": []any{strings.Repeat("o", dispatch.MaxOption+1)}},
		"a blank option": {"question": "q", "options": []any{" "}},
		"not a list":     {"question": "q", "options": "a"},
	}
	for name, args := range bad {
		if res := p.call(pod, sid, "human.request", args); res.OK || (res.Decision["rule"] != "HR-QUESTION" && res.Decision["rule"] != "HR-OPTIONS") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	for i := 0; i < dispatch.MaxPending; i++ {
		if res := p.call(pod, sid, "human.request", map[string]any{"question": fmt.Sprint("q", i)}); !res.OK {
			t.Fatalf("%d: %+v", i, res)
		}
	}
	if res := p.call(pod, sid, "human.request", map[string]any{"question": "one more"}); res.OK || res.Decision["rule"] != "HR-FLOOD" {
		t.Fatalf("%+v", res)
	}
	// expired with its session: not listed, cannot be answered, and the sweep's housekeeping drops it after the retention
	later := t0.Add(2 * time.Hour)
	p.g.Now = func() time.Time { return later }
	alice := p.person("user:alice")
	if list, _ := p.g.HumanRequests(ctx, alice, ""); len(list) != 0 {
		t.Fatalf("%d expired requests are listed", len(list))
	}
	pending, _ := p.inbox.Pending(ctx, t0)
	if _, err := p.g.HumanAnswer(ctx, alice, pending[0].ID, "x", ""); ctlRule(err) != "HR-EXPIRED" || ctlStatus(err) != 410 {
		t.Fatalf("%v", err)
	}
	p.g.Now = func() time.Time { return t0.Add(dispatch.InboxRetention + 3*time.Hour) }
	p.g.ExpireAt(ctx, p.g.Now())
	if left, _ := p.inbox.Get(ctx, pending[0].ID); left != nil {
		t.Fatal("an old request was not pruned")
	}
}

// Two people answering one request at once: one answer, one HR-ANSWERED, one record. The first is held inside the core's record
// call by a channel.
func TestRacingAnswersGiveOneAnswerAndOneRecord(t *testing.T) {
	p := newPeopleRig(t)
	p.dir["user:grace"] = []string{"group:default/team-platform"}
	_ = p.inbox.Add(ctx, &dispatch.HumanRequest{ID: "h-0000000000aa", TaskID: "t-0000a1", Session: "s-0000a1", Agent: "orchestrator", Owner: "user:alice",
		Group: ptr("group:default/team-platform"), Question: "q?", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), Status: "pending"})
	start := make(chan struct{})
	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for _, who := range []string{"user:alice", "user:grace", "user:alice", "user:grace", "user:alice", "user:grace"} {
		tok := p.person(who)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := p.g.HumanAnswer(ctx, tok, "h-0000000000aa", "yes", "")
			switch {
			case err == nil:
				ok.Add(1)
			case ctlRule(err) == "HR-ANSWERED":
				refused.Add(1)
			default:
				t.Errorf("%v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || refused.Load() != 5 || len(p.recordsOf("human.answer")) != 1 {
		t.Fatalf("%d answered, %d refused, %d records", ok.Load(), refused.Load(), len(p.recordsOf("human.answer")))
	}
}

// The answer stands only once its record does: with the core down the person is told to retry (503) and the request is still pending.
func TestAnAnswerIsNotGivenWhenItsRecordCannotBeWritten(t *testing.T) {
	p := newPeopleRig(t)
	_ = p.inbox.Add(ctx, &dispatch.HumanRequest{ID: "h-0000000000ab", TaskID: "t-0000a1", Session: "s-0000a1", Agent: "orchestrator", Owner: "user:alice",
		Question: "q?", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), Status: "pending"})
	p.fc.Down.Store(true)
	_, err := p.g.HumanAnswer(ctx, p.person("user:alice"), "h-0000000000ab", "yes", "")
	if ctlStatus(err) != 503 || ctlRule(err) != core.OutageRule {
		t.Fatalf("%v", err)
	}
	if r, _ := p.inbox.Get(ctx, "h-0000000000ab"); r.Status != "pending" || r.Answer != nil {
		t.Fatalf("%+v", r)
	}
	p.fc.Down.Store(false)
	if _, err := p.g.HumanAnswer(ctx, p.person("user:alice"), "h-0000000000ab", "yes", ""); err != nil {
		t.Fatal(err)
	}
}

// A gateway with no inbox registers no handler: the tool has no backend, as in a Python gateway with none.
func TestNoInboxNoHandler(t *testing.T) {
	p := newPeopleRig(t)
	p.g.Inbox = nil
	if _, err := p.g.HumanAnswer(ctx, p.person("user:alice"), "h-0000000000ab", "x", ""); ctlRule(err) != "HR-OFF" || ctlStatus(err) != 404 {
		t.Fatal(err)
	}
	if list, err := p.g.HumanRequests(ctx, p.person("user:alice"), ""); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
}

// ---- the rate of the repeatable controls ---------------------------------------------------------------------------------

func TestHoldsAndRunsAreRateLimitedPerPerson(t *testing.T) {
	p := newPeopleRig(t)
	root := p.person(adminID)
	for i := 0; i < dispatch.ControlRate; i++ {
		if _, err := p.g.HoldTriggers(ctx, root, "triage-agent", "storm", 1, ""); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	before := len(p.records())
	if _, err := p.g.HoldTriggers(ctx, root, "triage-agent", "storm", 1, ""); ctlRule(err) != "CT-RATE" || ctlStatus(err) != 429 {
		t.Fatalf("%v", err)
	}
	if len(p.records()) != before {
		t.Fatal("a limited call wrote a record")
	}
	// another person is not affected, and the limit is the same person's runs apart from holds
	if _, err := p.g.HoldTriggers(ctx, p.who(delegateID, "workload"), "triage-agent", "storm", 1, "user:grace"); ctlRule(err) != "CT-FORBIDDEN" {
		t.Fatal(err)
	}
	if _, err := p.g.RunAgent(ctx, root, "researcher", "x", "", nil, nil); err != nil {
		t.Fatalf("runs have a limit of their own: %v", err)
	}
	p.g.Now = func() time.Time { return t0.Add(61 * time.Second) }
	if _, err := p.g.HoldTriggers(ctx, root, "triage-agent", "storm", 1, ""); err != nil {
		t.Fatalf("the window did not pass: %v", err)
	}
}

// ---- Preflight's routes --------------------------------------------------------------------------------------------------

func TestPreflightCasesAndScore(t *testing.T) {
	p := newPeopleRig(t)
	p.g.PreflightCases = map[string]*dispatch.Case{"c1": {ID: "c1", Title: "t", RootCause: "r", Verifiers: []string{"v1", "v2"}, Agent: "researcher", Input: "do it", Runs: 1,
		MaxDenials: 0, MaxRetries: 1, TokenBudget: 1000, MinPathsTouched: 0,
		Checks: map[string][]map[string]any{"v1": {{"text_contains": []any{"hello"}}}, "v2": {{"tool_count": map[string]any{"tool": "catalog.read", "eq": 1}}}}}}
	p.g.PreflightLedger = &dispatch.PreflightLedger{}
	p.g.Preflight = p.g.PreflightLedger.Of
	if _, err := p.g.PreflightCaseList(ctx, "bad", "researcher"); err == nil {
		t.Fatal("an unauthenticated caller listed cases")
	}
	list, err := p.g.PreflightCaseList(ctx, p.pod("r-0000a1"), "researcher")
	if err != nil || len(list["cases"].([]map[string]any)) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := p.g.PreflightCaseList(ctx, p.person("user:bob"), 5); ctlRule(err) != "R008" {
		t.Fatal(err)
	}
	// a run of a candidate, scored
	cand := *p.g.Defs["researcher"]
	cand.Name = "pf-researcher-abcdef01"
	live := p.g.Defs["researcher"]
	view, err := p.g.RunAgentAs(ctx, adminID, "", "researcher", &cand, dispatch.Fingerprint(live), ptr("go"))
	if err != nil {
		t.Fatal(err)
	}
	sid := view["session"].(string)
	body := map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "c1", "sessions": []any{sid}}}}
	// only a person who manages the agent scores
	for _, tok := range []string{p.person("user:bob"), p.pod("r-0000a1")} {
		if _, err := p.g.PreflightScore(ctx, tok, body, ""); err == nil {
			t.Fatal("scored by someone who may not")
		}
	}
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["finished"] != false {
		t.Fatalf("%v %v", out, err)
	}
	// the run finishes with its answer
	pod := p.pod(view["run"].(string))
	exp := core.Iso(t0.Add(time.Hour))
	p.results.Rows[sid] = map[string]any{"expires_at": exp, "status": "done", "final_text": "Hello there", "tool_calls": []any{map[string]any{"tool": "catalog.read", "ok": true}}}
	_ = pod
	out, err = p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["finished"] != true || out["passed"] != true {
		t.Fatalf("%v %v", out, err)
	}
	if rec := out["record"].(map[string]any); rec["result"] != "pass" || rec["cases"] != 1 {
		t.Fatalf("%v", rec)
	}
	// the ledger carries the result for the definition that was scored, and only while it is unchanged
	if r := p.g.PreflightLedger.Of(live); r == nil || r.Result != "pass" || *r.Cases != 1 {
		t.Fatalf("%+v", r)
	}
	edited := *live
	edited.Description += " (edited)"
	if p.g.PreflightLedger.Of(&edited) != nil {
		t.Fatal("an edited definition carries the result of the one that was scored")
	}
	// refusals
	for name, b := range map[string]any{
		"no body":          nil,
		"another agent":    map[string]any{"agent": "orchestrator", "cases": body["cases"]},
		"an unknown case":  map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "zz", "sessions": []any{sid}}}},
		"not a run":        map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "c1", "sessions": []any{"s-0000a1"}}}},
		"no sessions":      map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "c1", "sessions": []any{}}}},
		"too many":         map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "c1", "sessions": make([]any, dispatch.MaxScoreSessions+1)}}},
		"sessions not ids": map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "c1", "sessions": []any{5}}}},
	} {
		if _, err := p.g.PreflightScore(ctx, p.person(adminID), b, ""); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// a failing run
	p.results.Rows[sid] = map[string]any{"expires_at": exp, "status": "done", "final_text": "nothing", "tool_calls": []any{}}
	out, _ = p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if out["passed"] != false || p.g.PreflightLedger.Of(live).Result != "fail" {
		t.Fatalf("%v", out)
	}
}

var _ = time.Second
