package session_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func lim(mod func(*client.Limits)) client.Limits {
	l := client.Limits{TierCeiling: "T2", TTLMinutes: 60, ToolCalls: 100, GithubCalls: 10, OpenPRs: 2, ModelTokens: 10000, Children: 50,
		Tools: []string{"catalog.read", "run.spawn"}, Models: []string{"m"}, Network: "clearance+model", Compute: "medium"}
	if mod != nil {
		mod(&l)
	}
	return l
}

func newParent(t *testing.T) (*session.Store, *session.Session) {
	st, _ := session.NewStore(context.Background(), nil)
	p := st.Adopt(&client.Grant{Session: "s-p", TaskID: "t-p", Principal: "user:alice", Agent: "orch", Limits: lim(nil),
		ExpiresAt: t0.Add(time.Hour)}, t0, nil, 0)
	// another live session of the same principal and task: a grant must not be able to attach a child to it
	st.Adopt(&client.Grant{Session: "s-other", TaskID: "t-p", Principal: "user:alice", Agent: "orch", Limits: lim(nil),
		ExpiresAt: t0.Add(time.Hour)}, t0, nil, 0)
	return st, p
}

func grantFor(id string, l client.Limits) (*client.Grant, client.Spend) {
	parent := "s-p"
	return &client.Grant{Session: id, TaskID: "t-p", Principal: "user:alice", Agent: "kid", Parent: &parent, Depth: 1, Limits: l,
			ExpiresAt: t0.Add(time.Duration(l.TTLMinutes) * time.Minute)},
		client.Spend{ToolCalls: 1, Children: 1, Reserve: &client.Budgets{ToolCalls: l.ToolCalls, GithubCalls: l.GithubCalls,
			OpenPRs: l.OpenPRs, ModelTokens: l.ModelTokens}}
}

func TestAdoptChildReservesAndRefusesWhatTheParentCannotGive(t *testing.T) {
	kid := lim(func(l *client.Limits) {
		l.ToolCalls, l.GithubCalls, l.OpenPRs, l.ModelTokens, l.Children, l.TTLMinutes = 10, 0, 0, 500, 0, 20
	})
	def := lim(nil)
	cases := []struct {
		name string
		mod  func(g *client.Grant, sp *client.Spend)
		want string // "" = accepted
	}{
		{"accepted", func(*client.Grant, *client.Spend) {}, ""},
		{"wider tool_calls", func(g *client.Grant, sp *client.Spend) { g.Limits.ToolCalls = 101; sp.Reserve.ToolCalls = 101 }, "tool_calls"},
		{"wider model_tokens", func(g *client.Grant, sp *client.Spend) { g.Limits.ModelTokens = 10001; sp.Reserve.ModelTokens = 10001 }, "model_tokens"},
		{"higher tier than the parent", func(g *client.Grant, _ *client.Spend) { g.Limits.TierCeiling = "T3" }, "tier_ceiling"},
		{"a tool the parent lacks", func(g *client.Grant, _ *client.Spend) { g.Limits.Tools = []string{"repo.pr.open"} }, "tools:repo.pr.open"},
		{"a model the parent lacks", func(g *client.Grant, _ *client.Spend) { g.Limits.Models = []string{"other"} }, "models:other"},
		{"more network", func(g *client.Grant, _ *client.Spend) { g.Limits.Network = "allowlist" }, "network"},
		{"more compute", func(g *client.Grant, _ *client.Spend) { g.Limits.Compute = "gpu" }, "compute"},
		{"longer ttl than the parent has", func(g *client.Grant, _ *client.Spend) { g.Limits.TTLMinutes = 61 }, "ttl_minutes"},
		{"outlives the parent", func(g *client.Grant, _ *client.Spend) { g.ExpiresAt = t0.Add(2 * time.Hour) }, "outlives"},
		{"outlives its own ttl", func(g *client.Grant, _ *client.Spend) { g.ExpiresAt = t0.Add(59 * time.Minute) }, "outlives"},
		{"another principal", func(g *client.Grant, _ *client.Spend) { g.Principal = "user:mallory" }, "another principal"},
		{"another task", func(g *client.Grant, _ *client.Spend) { g.TaskID = "t-other" }, "another principal or task"},
		{"another agent", func(g *client.Grant, _ *client.Spend) { g.Agent = "other" }, "another agent"},
		{"the parent's own id", func(g *client.Grant, _ *client.Spend) { g.Session = "s-p" }, "not the one the gateway named"},
		{"an id the gateway did not name", func(g *client.Grant, _ *client.Spend) { g.Session = "s-x" }, "not the one the gateway named"},
		{"a parent that is not the caller", func(g *client.Grant, _ *client.Spend) { p := "s-other"; g.Parent = &p }, "not the session that asked"},
		{"too deep", func(g *client.Grant, _ *client.Spend) { g.Depth = 4 }, "depth"},
		{"depth skipped", func(g *client.Grant, _ *client.Spend) { g.Depth = 2 }, "depth"},
		{"no parent", func(g *client.Grant, _ *client.Spend) { g.Parent = nil }, "no parent"},
		{"an unknown parent", func(g *client.Grant, _ *client.Spend) { p := "s-nope"; g.Parent = &p }, "not the session that asked"},
		{"reserves less than the child may spend", func(_ *client.Grant, sp *client.Spend) { sp.Reserve.ToolCalls = 1 }, "reserves other"},
		{"reserves more than the child may spend", func(_ *client.Grant, sp *client.Spend) { sp.Reserve.ModelTokens = 9999 }, "reserves other"},
		{"reserves a children budget", func(_ *client.Grant, sp *client.Spend) { sp.Reserve.Children = 1 }, "does not count"},
		{"counts no child", func(_ *client.Grant, sp *client.Spend) { sp.Children = 0 }, "does not count"},
		{"counts two children", func(_ *client.Grant, sp *client.Spend) { sp.Children = 2 }, "does not count"},
		{"reserves nothing", func(_ *client.Grant, sp *client.Spend) { sp.Reserve = nil }, "does not count"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, p := newParent(t)
			g, sp := grantFor("s-kid", kid)
			c.mod(g, &sp)
			child, _, err := st.AdoptChild(g, sp, "s-p", "s-kid", "kid", def, t0)
			if c.want == "" {
				if err != nil || child == nil || p.Reserved.ToolCalls != 10 || p.Reserved.ModelTokens != 500 || p.Used.Children != 1 || len(p.Children) != 1 {
					t.Fatalf("%v %+v", err, p)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q, got %v", c.want, err)
			}
			if st.Get("s-kid") != nil || p.Reserved != (client.Budgets{}) || p.Used.Children != 0 || len(p.Children) != 0 {
				t.Fatalf("a refused grant changed something: %+v", p)
			}
		})
	}
}

func TestAdoptChildRefusesAnExistingSessionIDAndAParentThatCannotHaveOne(t *testing.T) {
	kid := lim(func(l *client.Limits) {
		l.ToolCalls, l.GithubCalls, l.OpenPRs, l.ModelTokens, l.Children, l.TTLMinutes = 1, 0, 0, 1, 0, 5
	})
	st, p := newParent(t)
	g, sp := grantFor("s-kid", kid)
	if _, _, err := st.AdoptChild(g, sp, "s-p", "s-kid", "kid", lim(nil), t0); err != nil {
		t.Fatal(err)
	}
	// the same id again would replace a live session
	if _, _, err := st.AdoptChild(g, sp, "s-p", "s-kid", "kid", lim(nil), t0); err == nil || !strings.Contains(err.Error(), "already a session") {
		t.Fatalf("%v", err)
	}
	for name, mod := range map[string]func(){
		"tripped": func() { p.Lock(); p.Trip("x"); p.Unlock() },
		"closed":  func() { p.Lock(); p.Closed = true; p.Unlock() },
		"expired": func() { p.Lock(); p.ExpiresAt = t0; p.Unlock() },
	} {
		st2, p2 := newParent(t)
		p = p2 // the case's change is made to this parent
		mod()
		g2, sp2 := grantFor("s-k2", kid)
		if _, _, err := st2.AdoptChild(g2, sp2, "s-p", "s-k2", "kid", lim(nil), t0); err == nil {
			t.Errorf("%s parent got a child", name)
		}
	}
	st3, p3 := newParent(t)
	p3.Used.Children = 50
	g3, sp3 := grantFor("s-k3", kid)
	if _, _, err := st3.AdoptChild(g3, sp3, "s-p", "s-k3", "kid", lim(nil), t0); err == nil || !strings.Contains(err.Error(), "children budget") {
		t.Fatalf("%v", err)
	}
}

// Concurrent spawns cannot overspend a parent: the check and the reservation are one step under the parent's lock, and
// the parent is being charged from elsewhere (the model proxy's usage) at the same time. Run with -race.
func TestAdoptChildStormCannotOverspendTheParent(t *testing.T) {
	st, p := newParent(t)
	var ok, refused atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() { // model usage charges the parent's model tokens (clamped to what is left) while spawns race
		// bounded: an unbounded charger can use up every model token before a spawn gets in, and the test then sees no
		// child made (a flake of the test, not of the store)
		for charged := 0; charged < 500; {
			select {
			case <-stop:
				return
			default:
				p.Lock()
				if room := p.Remaining("model_tokens"); room > 0 {
					p.Used.ModelTokens++
					charged++
				}
				p.Unlock()
			}
		}
	}()
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := st.NewID()
			kid := lim(func(l *client.Limits) {
				l.ToolCalls, l.GithubCalls, l.OpenPRs, l.ModelTokens, l.Children, l.TTLMinutes = 10, 0, 0, 1000, 0, 20
			})
			g, sp := grantFor(id, kid)
			if _, _, err := st.AdoptChild(g, sp, "s-p", id, "kid", lim(nil), t0); err != nil {
				refused.Add(1)
				st.GiveBack(id)
				return
			}
			ok.Add(1)
		}()
	}
	wg.Wait()
	close(stop)
	p.Lock()
	defer p.Unlock()
	if p.Remaining("tool_calls") < 0 || p.Remaining("model_tokens") < 0 || p.Remaining("children") < 0 {
		t.Fatalf("the parent was overspent: %+v used %+v reserved", p.Used, p.Reserved)
	}
	if p.Reserved.ToolCalls != ok.Load()*10 || int(p.Used.Children) != int(ok.Load()) || len(p.Children) != int(ok.Load()) {
		t.Fatalf("the reservation does not match the children made: %d made, %+v %+v", ok.Load(), p.Used, p.Reserved)
	}
	if ok.Load() == 0 || ok.Load() > 10 || refused.Load() == 0 {
		t.Fatalf("%d made, %d refused: 100 tool calls reserve 10 each, at most 10 fit", ok.Load(), refused.Load())
	}
}

func TestCloseTouchedReturnsTheParentItGaveBudgetBackTo(t *testing.T) {
	st, p := newParent(t)
	kid := lim(func(l *client.Limits) {
		l.ToolCalls, l.GithubCalls, l.OpenPRs, l.ModelTokens, l.Children, l.TTLMinutes = 10, 0, 0, 100, 0, 5
	})
	g, sp := grantFor("s-kid", kid)
	child, _, err := st.AdoptChild(g, sp, "s-p", "s-kid", "kid", lim(nil), t0)
	if err != nil {
		t.Fatal(err)
	}
	child.Used.ToolCalls = 4
	touched := st.CloseTouched("s-kid")
	var ids []string
	for _, s := range touched {
		ids = append(ids, s.ID)
	}
	if len(ids) != 2 || !child.Closed || p.Reserved.ToolCalls != 0 || p.Used.ToolCalls != 4 {
		t.Fatalf("%v %+v %+v", ids, p.Reserved, p.Used)
	}
	if again := st.CloseTouched("s-kid"); len(again) != 0 {
		t.Fatalf("closing twice changes nothing: %v", again)
	}
}
