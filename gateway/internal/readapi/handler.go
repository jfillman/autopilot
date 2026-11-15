// Package readapi is `autopilot-gateway api`: the gateway's read API (src/autopilot/read_api.py), a separate, read-only process that serves
// the runtime's reads (agents with the hold on their triggers, sessions, a run's result, the run tree, the human inbox, the trigger
// bridge's history and the runtime catalog) from the gateway's database through a read-only role. It takes no writer lock, creates
// nothing and writes nothing (store.go has only SELECTs, and the connections are read-only).
//
// Who sees what is decided in two places only: Access.Viewer (who the caller is: one of auditor, person, delegate-for-a-person,
// notifier, or nobody) and the route table (embedded contract.json, generated from Python's ROUTES: which roles may call which route).
// What a person sees is then scoped by the viewer INSIDE each query (their own sessions and those of their teams' agents; their own and
// their groups' inbox items; the events of the apps they own and of their teams' agents), so a row that is not theirs is never read, and
// a session that is not theirs is a 404, exactly as one that does not exist.
package readapi

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

//go:embed contract.json
var contractJSON []byte

// operation is one route of the contract.
type operation struct {
	ID     string   `json:"operationId"`
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Roles  []string `json:"roles"`
	segs   []string
}

func (o *operation) allows(role string) bool {
	for _, r := range o.Roles {
		if r == role || (role == RoleAuditor && r == RolePerson) {
			return true
		}
	}
	return false
}

// API is the read API.
type API struct {
	Access       *Access
	Store        Store
	Defs         *definitions.Set
	Runtimes     []Runtime
	CustomImages []string
	GatewayURL   string
	ClearanceURL string
	Clock        func() time.Time
	// Timeout bounds one request (default 10 s); MaxInFlight bounds the requests being served at once (default 64): more get a 503.
	Timeout     time.Duration
	MaxInFlight int
	// ProbeEvery is how long a /readyz answer stands (default 5 s): the probe reaches the database once per that, however many ask.
	ProbeEvery time.Duration

	ops      []*operation
	contract map[string]any
	sem      chan struct{}
	probeMu  sync.Mutex
	probeAt  time.Time
	probeOK  bool
	fns      map[string]func(*request) (any, error)
}

type request struct {
	ctx    context.Context
	r      *http.Request
	q      query
	v      *Viewer
	params map[string]string
	now    time.Time
}

// New checks that every route of the contract has a handler and the other way round.
func New(a *API) (*API, error) {
	var doc struct {
		Ops []*operation `json:"operations"`
	}
	if err := json.Unmarshal(contractJSON, &doc); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(contractJSON, &a.contract); err != nil {
		return nil, err
	}
	a.ops = doc.Ops
	for _, o := range a.ops {
		o.segs = strings.Split(strings.TrimPrefix(o.Path, "/"), "/")
	}
	// Literal paths before parameterised ones.
	sort.SliceStable(a.ops, func(i, j int) bool {
		return !strings.Contains(a.ops[i].Path, "{") && strings.Contains(a.ops[j].Path, "{")
	})
	if a.Clock == nil {
		a.Clock = func() time.Time { return time.Now().UTC() }
	}
	if a.Timeout <= 0 {
		a.Timeout = 10 * time.Second
	}
	if a.MaxInFlight <= 0 {
		a.MaxInFlight = 64
	}
	a.sem = make(chan struct{}, a.MaxInFlight)
	a.fns = map[string]func(*request) (any, error){"listAgents": a.listAgents, "getAgent": a.getAgent, "listSessions": a.listSessions,
		"getSession": a.getSession, "getSessionResult": a.getSessionResult, "getTask": a.getTask, "listHumanRequests": a.human,
		"listEvents": a.listEvents, "listRuntimes": a.listRuntimes}
	for _, o := range a.ops {
		if _, ok := a.fns[o.ID]; !ok && o.ID != "getContract" {
			return nil, &Error{500, "the contract has a route with no handler: " + o.ID}
		}
	}
	return a, nil
}

// Operations are the contract's operation ids.
func (a *API) Operations() []string {
	var ids []string
	for _, o := range a.ops {
		ids = append(ids, o.ID)
	}
	sort.Strings(ids)
	return ids
}

func (a *API) match(path string) (*operation, map[string]string) {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, o := range a.ops {
		if len(o.segs) != len(segs) {
			continue
		}
		params := map[string]string{}
		ok := true
		for i, s := range o.segs {
			switch {
			case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
				if segs[i] == "" {
					ok = false
				}
				params[s[1:len(s)-1]] = segs[i]
			case s != segs[i]:
				ok = false
			}
		}
		if ok {
			return o, params
		}
	}
	return nil, nil
}

func writeJSON(w http.ResponseWriter, code int, v any, hdr map[string]string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		code = http.StatusServiceUnavailable
		b.Reset()
		b.WriteString(`{"error":"read failed"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.WriteHeader(code)
	_, _ = w.Write(bytes.TrimRight(b.Bytes(), "\n"))
}

func plain(w http.ResponseWriter, code int, text string, hdr map[string]string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.WriteHeader(code)
	_, _ = w.Write([]byte(text))
}

// ServeHTTP is the whole API.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("the read API panicked", "path", r.URL.Path, "panic", p)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "read failed"}, nil)
		}
	}()
	switch r.URL.Path {
	case "/healthz":
		a.simple(w, r, func() (int, any) { return 200, map[string]bool{"ok": true} })
		return
	case "/readyz":
		a.simple(w, r, func() (int, any) {
			if a.ready() {
				return 200, map[string]bool{"ok": true}
			}
			return http.StatusServiceUnavailable, map[string]bool{"ok": false}
		})
		return
	}
	op, params := a.match(r.URL.Path)
	if op == nil {
		plain(w, http.StatusNotFound, "Not Found", nil)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		plain(w, http.StatusMethodNotAllowed, "Method Not Allowed", map[string]string{"Allow": "GET, HEAD"})
		return
	}
	if op.ID == "getContract" { // the one route that needs no caller: it says what the API is
		doc := map[string]any{}
		for k, v := range a.contract {
			doc[k] = v
		}
		if a.GatewayURL != "" {
			doc["gateway"] = a.GatewayURL
		}
		if a.ClearanceURL != "" {
			doc["clearance"] = a.ClearanceURL
		}
		writeJSON(w, 200, doc, nil)
		return
	}
	if len(r.URL.RawQuery) > MaxQuery {
		writeJSON(w, http.StatusRequestURITooLong, map[string]string{"error": "the query string is too long"}, nil)
		return
	}
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many requests are being served"}, map[string]string{"Retry-After": "1"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.Timeout)
	defer cancel()
	v, err := a.viewer(ctx, r, op)
	if err != nil {
		a.fail(w, op.ID, err, nil)
		return
	}
	q, _ := parseQuery(r.URL.RawQuery)
	req := &request{ctx: ctx, r: r, q: q, v: v, params: params, now: a.Clock()}
	var hdr map[string]string
	if v.OwnersUnknown {
		hdr = map[string]string{"X-Autopilot-Owners": "unknown"}
	}
	out, err := a.fns[op.ID](req)
	if err != nil {
		a.fail(w, op.ID, err, hdr)
		return
	}
	writeJSON(w, 200, out, hdr)
}

// ready is the readiness probe: the database answers a ping, remembered for ProbeEvery. One caller probes at a time (the lock is held
// while it does), the others take its answer; a probe is bounded at 3 s on a context of its own, so a caller that hangs up does not leave
// the answer unmade, and a flood of /readyz (it needs no token) is one query per interval and never a use of the pool per request.
func (a *API) ready() bool {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	every := a.ProbeEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	if !a.probeAt.IsZero() && time.Since(a.probeAt) < every {
		return a.probeOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.Store.Ping(ctx)
	if err != nil {
		slog.Warn("readiness: the database did not answer", "err", err.Error())
	}
	a.probeAt, a.probeOK = time.Now(), err == nil
	return a.probeOK
}

func (a *API) simple(w http.ResponseWriter, r *http.Request, f func() (int, any)) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		plain(w, http.StatusMethodNotAllowed, "Method Not Allowed", map[string]string{"Allow": "GET, HEAD"})
		return
	}
	code, v := f()
	writeJSON(w, code, v, nil)
}

// maxToken is the longest bearer token read (a JWT of a few KB is the largest there is).
const maxToken = 16 << 10

func (a *API) viewer(ctx context.Context, r *http.Request, op *operation) (*Viewer, error) {
	token := ""
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		token = strings.TrimSpace(h[7:])
	}
	if len(token) > maxToken {
		return nil, &Error{401, "token not accepted"}
	}
	vals := r.Header.Values("X-Hangar-On-Behalf-Of")
	if len(vals) == 1 && vals[0] == "" {
		vals = nil
	}
	v, err := a.Access.Viewer(ctx, token, vals)
	if err != nil {
		return nil, err
	}
	if role := v.RouteRole(); !op.allows(role) {
		return nil, &Error{403, "a " + role + " may not call " + op.ID}
	}
	return v, nil
}

// fail answers an error: a refusal as it is, anything else as a failed read that names nothing (the cause is logged).
func (a *API) fail(w http.ResponseWriter, op string, err error, hdr map[string]string) {
	if e, ok := err.(*Error); ok {
		writeJSON(w, e.Status, map[string]string{"error": e.Msg}, hdr)
		return
	}
	slog.Warn("a read failed", "op", op, "err", err.Error())
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "read failed"}, hdr)
}

func bad(msg string) error { return &Error{400, msg} }

// page is a list and the cursor of its next page: when it is full, the time and the id of its last row, "<time>|<id>" (a time alone loses
// the rows that share it). A cursor of Python's, a plain time, is still read.
func page(items []Record, limit int, key string) Record {
	if items == nil {
		items = []Record{}
	}
	var next any
	if len(items) == limit && len(items) > 0 {
		last := items[len(items)-1]
		id, _ := last["id"].(string)
		if t, ok := last[key].(string); ok && id != "" {
			next = t + "|" + id
		} else {
			next = last[key]
		}
	}
	return Record{"items": items, "next": next}
}

func (a *API) listAgents(q *request) (any, error) {
	full := q.v.All() && q.q.get("full") == "1"
	holds, err := a.Store.Holds(q.ctx, q.now)
	if err != nil {
		return nil, err
	}
	snap := a.Defs.Snapshot()
	names := make([]string, 0, len(snap))
	for n := range snap {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]Record, 0, len(names))
	for _, n := range names {
		items = append(items, DefinitionView(snap[n], full, holds[n], a.Defs.Source(n)))
	}
	_, rejected, _ := a.Defs.ClusterSet()
	rej := make([]Record, 0, len(rejected))
	for _, r := range rejected {
		rej = append(rej, Record{"name": r.Name, "source": r.Source.View(), "reason": r.Reason})
	}
	return Record{"items": items, "rejected": rej}, nil
}

func (a *API) getAgent(q *request) (any, error) {
	name := q.params["name"]
	d := a.Defs.Snapshot()[name]
	if d == nil {
		return nil, &Error{404, "no such agent"}
	}
	holds, err := a.Store.Holds(q.ctx, q.now)
	if err != nil {
		return nil, err
	}
	return DefinitionView(d, q.v.All() && q.q.get("full") == "1", holds[name], a.Defs.Source(name)), nil
}

func (a *API) listSessions(q *request) (any, error) {
	limit, ok := limitOf(q.q)
	if !ok {
		return nil, bad("limit must be a number")
	}
	f := SessionFilter{Agent: q.q.get("agent"), Principal: q.q.get("principal"), Task: q.q.get("task"), State: q.q.get("state")}
	if f.State != "" && f.State != "live" && f.State != "closed" && f.State != "expired" {
		return nil, bad("state is live, closed or expired")
	}
	after := q.q.get("after")
	if after != "" && !validCursor(after) {
		return nil, bad("after is a cursor from a previous page")
	}
	if !okFilter(f.Agent) || !okFilter(f.Principal) || !okFilter(f.Task) {
		return page(nil, limit, "created_at"), nil // a value that cannot have been stored matches nothing
	}
	items, err := a.Store.Sessions(q.ctx, f, q.v, limit, after, q.now)
	if err != nil {
		return nil, err
	}
	return page(items, limit, "created_at"), nil
}

func visible(s Record, v *Viewer) bool {
	if v.All() {
		return true
	}
	p, _ := s["principal"].(string)
	ag, _ := s["agent"].(string)
	return (v.Person != "" && p == v.Person) || v.Agents[ag]
}

func (a *API) session(q *request) (Record, error) {
	id := q.params["id"]
	if !okFilter(id) {
		return nil, &Error{404, "no such session"}
	}
	s, err := a.Store.Session(q.ctx, id, q.now)
	if err != nil {
		return nil, err
	}
	if s == nil || !visible(s, q.v) {
		return nil, &Error{404, "no such session"}
	}
	return s, nil
}

func (a *API) getSession(q *request) (any, error) { return a.session(q) }

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	}
	return true
}

func (a *API) getSessionResult(q *request) (any, error) {
	s, err := a.session(q)
	if err != nil {
		return nil, err
	}
	id, _ := s["id"].(string)
	r, err := a.Store.Result(q.ctx, id, q.now)
	if err != nil {
		return nil, err
	}
	if r == nil {
		if truthy(s["outcome"]) {
			return nil, &Error{404, "this run kept no result, or it expired"}
		}
		return nil, &Error{404, "the run has not finished"}
	}
	return r, nil
}

func isInt(n json.Number) (int64, bool) {
	if strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

func (a *API) getTask(q *request) (any, error) {
	id := q.params["task_id"]
	if !okFilter(id) {
		return nil, &Error{404, "no such task"}
	}
	sessions, err := a.Store.Task(q.ctx, id, q.v, q.now)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, &Error{404, "no such task"}
	}
	if len(sessions) > maxTaskSessions { // more than a tree is shown: the oldest are, and the totals of part of a tree are not the tree's
		return Record{"task_id": id, "sessions": sessions[:maxTaskSessions], "used": nil, "cost": nil, "truncated": true}, nil
	}
	used, cost := map[string]int64{}, map[string]int64{}
	for _, s := range sessions {
		if m, ok := s["used"].(map[string]any); ok {
			for k, n := range m {
				if num, ok := n.(json.Number); ok {
					if i, ok := isInt(num); ok {
						used[k] += i
					} else if f, err := num.Float64(); err == nil {
						used[k] += int64(f)
					}
				}
			}
		}
		if m, ok := s["usage"].(map[string]any); ok {
			for k, n := range m {
				if num, ok := n.(json.Number); ok {
					if i, ok := isInt(num); ok {
						cost[k] += i
					}
				}
			}
		}
	}
	return Record{"task_id": id, "sessions": sessions, "used": used, "cost": cost}, nil
}

func (a *API) human(q *request) (any, error) {
	since := ""
	if s := q.q.get("since"); s != "" {
		t, naive, err := ParseISO(s)
		if err != nil {
			return nil, bad("since is an ISO time")
		}
		since = PyIsoformat(t, naive)
	}
	items, err := a.Store.HumanPending(q.ctx, q.now, since, q.v)
	if err != nil {
		return nil, err
	}
	truncated := len(items) > maxPending
	if truncated { // newest first from the store: the newest maxPending are kept, and shown oldest first as Python shows them
		items = items[:maxPending]
	}
	out := make([]Record, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		out = append(out, items[i])
	}
	if truncated {
		return Record{"items": out, "truncated": true}, nil
	}
	return Record{"items": out}, nil
}

func (a *API) listEvents(q *request) (any, error) {
	limit, ok := limitOf(q.q)
	if !ok {
		return nil, bad("limit must be a number")
	}
	f := EventFilter{App: strings.ToLower(q.q.get("app")), Agent: q.q.get("agent"), Source: q.q.get("source"), Key: q.q.get("key"), Subject: q.q.get("subject")}
	for _, k := range []string{"since", "until"} {
		s := q.q.get(k)
		if s == "" {
			continue
		}
		t, _, err := ParseISO(s)
		if err != nil {
			return nil, bad(k + " is an ISO time")
		}
		if k == "since" {
			f.Since = &t
		} else {
			f.Until = &t
		}
	}
	var after *EventCursor
	if s := q.q.get("after"); s != "" {
		ts, id, hasID := SplitCursor(s)
		t, _, err := ParseISO(ts)
		if err != nil || (hasID && (id == "" || !okFilter(id))) {
			return nil, bad("after is a cursor from a previous page")
		}
		after = &EventCursor{T: t, ID: id}
	}
	for _, s := range []string{f.App, f.Agent, f.Source, f.Key, f.Subject} {
		if !okFilter(s) {
			return page(nil, limit, "received_at"), nil
		}
	}
	items, err := a.Store.Events(q.ctx, f, q.v, limit, after)
	if err != nil {
		return nil, err
	}
	return page(items, limit, "received_at"), nil
}

func (a *API) listRuntimes(q *request) (any, error) {
	items := make([]Record, 0, len(a.Runtimes))
	for _, r := range a.Runtimes {
		items = append(items, r.View())
	}
	imgs := a.CustomImages
	if imgs == nil {
		imgs = []string{}
	}
	return Record{"items": items, "custom_images": imgs}, nil
}
