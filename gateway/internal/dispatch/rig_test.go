package dispatch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// memPersist is the sessions' database: it loads what it holds and keeps every save, so a test can read what was durable
// at any point and start a second gateway from it (a restart).
type memPersist struct {
	mu    sync.Mutex
	rows  map[string]session.Record
	saves int
	// failAfter, when > 0, makes the save after that many fail (a crash: the process would exit).
	failAfter int
}

func newPersist(recs ...session.Record) *memPersist {
	p := &memPersist{rows: map[string]session.Record{}}
	for _, r := range recs {
		p.rows[r.ID] = r
	}
	return p
}

func (p *memPersist) Load(context.Context) ([]session.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []session.Record
	for _, r := range p.rows {
		out = append(out, r)
	}
	return out, nil
}

func (p *memPersist) Save(_ context.Context, recs []session.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saves++
	if p.failAfter > 0 && p.saves > p.failAfter {
		return fmt.Errorf("the database went away")
	}
	for _, r := range recs {
		raw, _ := json.Marshal(r)
		var c session.Record
		_ = json.Unmarshal(raw, &c)
		p.rows[r.ID] = c
	}
	return nil
}

func (p *memPersist) row(id string) (session.Record, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.rows[id]
	return r, ok
}

// runRig is a gateway with a run backend over a fake AgentRun store, a fake core, and the bundled agent definitions.
type runRig struct {
	t           *testing.T
	fc          *fakecore.Core
	g           *dispatch.Gateway
	store       *session.Store
	api         *runs.FakeStore
	results     *dispatch.MemoryResults
	persist     *memPersist
	tokens      auth.Static
	persistErrs []error
}

func loadDefs(t testing.TB) map[string]*definitions.Definition {
	t.Helper()
	d, err := definitions.LoadDir("../../../agents", tools.Default())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type rigOpt func(*runRig)

func withPersist(p *memPersist) rigOpt { return func(r *runRig) { r.persist = p } }

func newRunRig(t *testing.T, opts ...rigOpt) *runRig {
	t.Helper()
	r := &runRig{t: t, fc: fakecore.New(), api: runs.NewFakeStore(), results: &dispatch.MemoryResults{Rows: map[string]map[string]any{}}, persist: newPersist(),
		tokens: auth.Static{}}
	t.Cleanup(r.fc.Close)
	for _, o := range opts {
		o(r)
	}
	var err error
	r.store, err = session.NewStore(context.Background(), r.persist)
	if err != nil {
		t.Fatal(err)
	}
	c := core.New(r.fc.Client(2*time.Second), core.NewMemoryOutbox(), false)
	g, err := dispatch.New(loadDefs(t), r.store, c, r.tokens, tools.Default(), map[string]dispatch.Handler{
		"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
			return map[string]any{"n": 1}, nil
		},
		"argo.app.get": func(context.Context, *session.Session, map[string]any) (any, error) {
			return map[string]any{"app": "x"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.Now = func() time.Time { return t0 }
	g.OnPersistError = func(err error) { r.persistErrs = append(r.persistErrs, err) }
	g.SetRuns(&runs.Backend{Store: r.api})
	g.Results = r.results
	r.g = g
	return r
}

// who registers a caller and returns its token.
func (r *runRig) who(id, kind string) string {
	tok := "tok:" + kind + ":" + id
	r.tokens[tok] = &dispatch.Principal{ID: id, Kind: kind, TierCeiling: 1}
	return tok
}

func (r *runRig) person(id string) string { return r.who(id, "user") }
func (r *runRig) pod(run string) string   { return r.who("agent-"+run+"/run", "workload") }

// sess puts a session in the store (as a row the database held), with a run when run is not "".
func sessionRow(id, principal, agent, run string, mod func(*session.Record)) session.Record {
	def := loadDefsOnce[agent]
	l := client.Limits{TierCeiling: "T2", TTLMinutes: 60, ToolCalls: 100, GithubCalls: 10, OpenPRs: 2, ModelTokens: 100000, Children: 4,
		Tools: []string{"argo.app.get", "catalog.read", "run.close", "run.spawn"}, Models: []string{"claude-sonnet-5"}, Network: "clearance+model", Compute: "small"}
	if def != nil {
		l = def.Limits
	}
	r := session.Record{ID: id, Principal: principal, Agent: agent, Limits: l, CreatedAt: t0.Format(session.IsoFormat),
		ExpiresAt: t0.Add(time.Hour).Format(session.IsoFormat), TaskID: "t-" + id[2:], Used: map[string]int64{}, Reserved: map[string]int64{},
		Children: []string{}, Usage: session.NoUsage(), Signals: map[string]any{}}
	if run != "" {
		r.Run = &run
	}
	if mod != nil {
		mod(&r)
	}
	return r
}

var loadDefsOnce = func() map[string]*definitions.Definition {
	d, err := definitions.LoadDir("../../../agents", tools.Default())
	if err != nil {
		panic(err)
	}
	return d
}()

// records is what the fake core was sent as records, as {kind, ...fields}, sorted so two runs compare.
func (r *runRig) records() []map[string]any {
	out := []map[string]any{}
	for _, raw := range r.fc.RecordBodies() {
		var rr struct {
			Kind   string         `json:"kind"`
			Record map[string]any `json:"record"`
		}
		_ = json.Unmarshal(raw, &rr)
		m := map[string]any{"kind": rr.Kind}
		for k, v := range rr.Record {
			m[k] = v
		}
		out = append(out, m)
	}
	return sorted(out)
}

func sorted(a []map[string]any) []map[string]any {
	sort.SliceStable(a, func(i, j int) bool {
		ki, kj := fmt.Sprint(a[i]["kind"], a[i]["session"], a[i]["result"]), fmt.Sprint(a[j]["kind"], a[j]["session"], a[j]["result"])
		return ki < kj
	})
	return a
}

// norm is a record or a row as generic JSON, with what the Go side omits when empty dropped from both sides: nulls, and a
// run's cost counters that are zero (the client's Cost type omits them; the evidence record is the same).
func norm(t testing.TB, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return dropEmpty(out, "")
}

func dropEmpty(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e == nil {
				delete(x, k)
				continue
			}
			if key == "cost" && k != "charged_tokens" && e == float64(0) {
				delete(x, k)
				continue
			}
			x[k] = dropEmpty(e, k)
		}
	case []any:
		for i := range x {
			x[i] = dropEmpty(x[i], key)
		}
	}
	return v
}
