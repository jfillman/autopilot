package dispatch_test

import (
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/triggers"
)

// Real goroutines with a barrier at the interesting point (run with -race): the interleavings the crash harness, which is one
// goroutine, cannot see.

// Two deliveries of one key at once (the stream and the sweep, here two bridges on one gateway and one ledger): the first is held inside
// the run's create; the second takes the claim over and runs into the same session. Exactly one run, and both deliveries agree.
func TestTwoDeliveriesOfOneKeyAtOnceMakeOneRun(t *testing.T) {
	p := factsRig(t)
	ledger := &triggers.MemoryLedger{}
	now := t0
	mk := func() *triggers.Bridge {
		b, err := triggers.NewBridge(p.g, ledger, triggers.BridgeConfig{BaseURL: "http://glidepath.invalid", AllowHTTP: true, TokenFile: "/dev/null", Now: func() time.Time { return now }}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	inCreate, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.g.SetRuns(&runs.Backend{Store: &gatedStore{FakeStore: p.api, onCreate: func(string) {
		once.Do(func() { close(inCreate); <-proceed })
	}}})
	env := harnessEvent()
	env["id"] = "e1"
	type out struct {
		got []triggers.Outcome
		err error
	}
	res := make(chan out, 2)
	go func() { g, err := mk().Handle(ctx, env, "stream"); res <- out{g, err} }()
	<-inCreate
	go func() { g, err := mk().Handle(ctx, env, "reconcile"); res <- out{g, err} }()
	time.Sleep(150 * time.Millisecond) // the second delivery is at the session, waiting for the first
	close(proceed)
	var launched int
	for i := 0; i < 2; i++ {
		o := <-res
		if o.err != nil || len(o.got) != 1 {
			t.Fatalf("%+v %v", o.got, o.err)
		}
		if o.got[0].Result == "launched" {
			launched++
		} else if o.got[0].Result != "deduped" {
			t.Fatalf("%+v", o.got)
		}
	}
	if launched == 0 {
		t.Fatal("neither delivery launched")
	}
	if len(p.api.Manifests) != 1 {
		t.Fatalf("%d runs were made by two deliveries of one key", len(p.api.Manifests))
	}
	row, _ := ledger.Get(ctx, "triage-agent", harnessKey)
	if row == nil || row.State != "launched" || row.Run == nil {
		t.Fatalf("%+v", row)
	}
	var live int
	for _, s := range p.store.All() {
		s.Lock()
		if s.Agent == "triage-agent" && !s.Closed {
			live++
		}
		s.Unlock()
	}
	if live != 1 {
		t.Fatalf("%d live sessions", live)
	}
}

// A person's end racing the launch's own save: the launch is inside the write-ahead save (the record that names the run is being
// written) when the end closes the session. The end's write waits for that save and then writes the closed record (saves of a session are
// ordered, so the stale one cannot land after it); the launch sees the close right before it would create, and makes no run.
func TestAnEndRacingTheLaunchesSaveWinsAndNoRunSurvives(t *testing.T) {
	p := factsRig(t)
	sid := openTrig(t, p)
	entered, release := make(chan struct{}), make(chan struct{})
	var saves int
	var mu sync.Mutex
	p.persist.onSave = func() {
		mu.Lock()
		saves++
		n := saves
		mu.Unlock()
		if n == 1 { // the write-ahead save of the run's name
			close(entered)
			<-release
		}
	}
	launched := make(chan *dispatchResult, 1)
	go func() {
		out, _ := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"})
		launched <- &dispatchResult{out.OK}
	}()
	<-entered
	ended := make(chan error, 1)
	go func() { _, err := p.g.EndSession(ctx, p.person(adminID), sid, "stop", ""); ended <- err }()
	// the end has closed the session in memory; its own write is waiting for the launch's save
	for i := 0; i < 400; i++ {
		s := p.store.Get(sid)
		s.Lock()
		c := s.Closed
		s.Unlock()
		if c {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	if r := <-launched; r.ok {
		t.Fatal("the launch succeeded after the end")
	}
	if len(p.api.Manifests) != 0 {
		t.Fatalf("a run survived the end: %v", p.api.Manifests)
	}
	row, _ := p.persist.row(sid)
	if !row.Closed || row.CloseReason == nil || row.CloseReason.Control != session.CloseEnd {
		t.Fatalf("the person's end did not win the write: %+v %+v", row.Closed, row.CloseReason)
	}
}

type dispatchResult struct{ ok bool }
