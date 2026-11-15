package readapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// scratch makes a database of its own on the server AUTOPILOT_TEST_PG names, with the writer's schema and indexes.
func scratch(t *testing.T) (dsn string, st *pgstate.State) {
	t.Helper()
	base := os.Getenv("AUTOPILOT_TEST_PG")
	if base == "" {
		t.Skip("no scratch Postgres (set AUTOPILOT_TEST_PG)")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("apitest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	t.Cleanup(func() {
		if a, err := pgx.Connect(ctx, base); err == nil {
			_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close(ctx)
		}
	})
	st, err = pgstate.Open(ctx, u.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(context.Background()) })
	return u.String(), st
}

type fixture struct {
	Now  string `json:"now"`
	Data struct {
		Sessions      []map[string]any `json:"sessions"`
		Results       []map[string]any `json:"results"`
		HumanRequests []map[string]any `json:"human_requests"`
		Events        []map[string]any `json:"events"`
		Holds         []map[string]any `json:"holds"`
		DefRows       []map[string]any `json:"definition_rows"`
	} `json:"data"`
	Directory map[string][]string                  `json:"directory"`
	Ownership map[string][]string                  `json:"ownership"`
	Tokens    map[string]struct{ ID, Kind string } `json:"tokens"`
	Callers   map[string]struct {
		Token      *string `json:"token"`
		OnBehalfOf *string `json:"on_behalf_of"`
	} `json:"callers"`
	Config struct {
		Delegates, Auditors, Notifiers []string
		GatewayURL                     string   `json:"gateway_url"`
		ClearanceAPIURL                string   `json:"clearance_api_url"`
		CustomImages                   []string `json:"custom_images"`
	} `json:"config"`
	Cases  []fcase `json:"cases"`
	GoOnly []struct {
		Name   string         `json:"name"`
		Caller string         `json:"caller"`
		Path   string         `json:"path"`
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
		Why    string         `json:"why"`
	} `json:"go_only"`
}

type answer struct {
	Status      int            `json:"status"`
	ContentType *string        `json:"content_type"`
	Allow       *string        `json:"allow"`
	Location    *string        `json:"location"`
	Body        map[string]any `json:"body"`
}

type fcase struct {
	Name   string  `json:"name"`
	Caller string  `json:"caller"`
	Method string  `json:"method"`
	Path   string  `json:"path"`
	Down   bool    `json:"down"`
	Python answer  `json:"python"`
	Go     *answer `json:"go"`
	Why    string  `json:"why"`
}

func loadFixture(t *testing.T) *fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/readapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return &f
}

func (f *fixture) now(t *testing.T) time.Time {
	t.Helper()
	n, err := dispatch.ParsePyIso(f.Now)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// seed writes the fixture's rows as the writer would.
func (f *fixture) seed(t *testing.T, st *pgstate.State) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	ts := func(v any) time.Time {
		tm, err := dispatch.ParsePyIso(v.(string))
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, r := range f.Data.Sessions {
		exec("INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2::jsonb, $3, $4)", r["id"], js(r), ts(r["expires_at"]), r["closed"])
	}
	for _, r := range f.Data.Results {
		exec("INSERT INTO gateway_results (session, task_id, record, expires_at) VALUES ($1, $2, $3::jsonb, $4)", r["session"], r["task_id"], js(r["record"]), ts(r["expires_at"]))
	}
	for _, r := range f.Data.HumanRequests {
		exec("INSERT INTO gateway_human_requests (id, session, status, expires_at, record) VALUES ($1, $2, $3, $4, $5::jsonb)", r["id"], r["session"], r["status"], ts(r["expires_at"]), js(r["record"]))
	}
	for _, r := range f.Data.Events {
		exec("INSERT INTO gateway_events (id, received_at, source, app, routing_key, record) VALUES ($1, $2, $3, $4, $5, $6::jsonb)", r["id"], ts(r["received_at"]), r["source"], r["app"], r["routing_key"], js(r["record"]))
	}
	for _, r := range f.Data.Holds {
		exec("INSERT INTO gateway_holds (agent, until, record) VALUES ($1, $2, $3::jsonb)", r["agent"], ts(r["until"]), js(r["record"]))
	}
	for _, r := range f.Data.DefRows {
		exec("INSERT INTO gateway_definitions (name, state, record) VALUES ($1, $2, $3::jsonb)", r["name"], r["state"], js(r))
	}
}

type fakeAuth struct {
	tokens map[string]struct{ ID, Kind string }
}

func (a fakeAuth) Authenticate(_ context.Context, token string) (*dispatch.Principal, error) {
	if p, ok := a.tokens[strings.TrimPrefix(token, "tok-")]; ok && strings.HasPrefix(token, "tok-") {
		return &dispatch.Principal{ID: p.ID, Kind: p.Kind, TierCeiling: 1}, nil
	}
	return nil, &core.AuthError{Msg: "token rejected"}
}

type mapDir map[string][]string

func (d mapDir) Groups(_ context.Context, p string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, g := range d[p] {
		out[g] = true
	}
	return out, nil
}

type mapOwn map[string][]string

func (d mapOwn) Apps(_ context.Context, p string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, g := range d[p] {
		out[strings.ToLower(g)] = true
	}
	return out, nil
}

// defsFrom is the live set: the bundled agents (the repository's agents/) and the pinned rows.
func defsFrom(t *testing.T, rows []map[string]any) *definitions.Set {
	t.Helper()
	reg := tools.Default()
	bundled, err := definitions.LoadDir("../../../agents", reg)
	if err != nil {
		t.Fatal(err)
	}
	set := definitions.NewSet(bundled)
	var rr []definitions.Row
	for _, r := range rows {
		rr = append(rr, r)
	}
	loaded, rejected := definitions.FromRecords(rr, set.Bundled(), reg, definitions.PinPolicy{Tools: reg})
	set.ReplaceCluster(loaded, rejected)
	return set
}

func (f *fixture) api(t *testing.T, store Store) *API {
	t.Helper()
	set := defsFrom(t, f.Data.DefRows)
	rts, err := LoadRuntimes("../../../runtime/catalog.yaml")
	if err != nil || len(rts) == 0 {
		t.Fatalf("runtimes: %v %d", err, len(rts))
	}
	now := f.now(t)
	a, err := New(&API{Access: &Access{Auth: fakeAuth{f.Tokens}, Delegates: f.Config.Delegates, Auditors: f.Config.Auditors, Notifiers: f.Config.Notifiers,
		Directory: mapDir(f.Directory), Ownership: mapOwn(f.Ownership), Defs: set}, Store: store, Defs: set, Runtimes: rts,
		CustomImages: f.Config.CustomImages, GatewayURL: f.Config.GatewayURL, ClearanceURL: f.Config.ClearanceAPIURL, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
