package triggers

// The event trigger bridge: Glidepath events in, triggered agent runs out (event_bridge.py; Glidepath API spec P5, durable since
// A-5).
//
// It holds one stream of glidepath-api's `GET /v1/events` (server-sent events) as the gateway's own identity, which reads events
// through glidepath-api's reader role. For each event it finds the agent definitions with a Glidepath event trigger that
// matches (MatchGlidepathEvent), and for each one, under that trigger's storm brake, opens a triggered session and launches its
// run with the event as input.
//
// What makes this safe to run unattended:
//   - Once per (agent, trigger key), across restarts, replays and the reconciliation sweep: the key (TriggerKey) is claimed in
//     the gateway's database before anything is opened, and the decision is written back to the same row; a second claim loses.
//     A live duplicate is also answered by OpenTriggered, whose task id is a hash of the agent and the key.
//   - A cursor in the gateway's database: the newest event id handled, saved after each event, so a restart resumes from it
//     with `/v1/events/recent?since=` and then `Last-Event-ID`. A cursor older than glidepath-api's buffer (24 h) counts as
//     `stale-cursor`: events may be gone, and the reconciliation sweep recovers a missed pipelinerun.finished. The cursor does
//     not move past an event whose handling failed on the ledger (the stream ends and resumes from it), and it only follows
//     ids that have the form of glidepath-api's (16 digits of receive time in microseconds, a dash and a hash) and are not in the
//     future, so no event can push it past the ones still to come.
//   - A storm brake per agent: at most `maxPerHour` runs an hour (default 6). An event over the limit is dropped, recorded
//     `braked` in the ledger (so no later path launches it either) and logged.
//   - The run's identity is the definition's own workload identity, never the event's sender (OpenTriggered). What reaches the
//     run is a summary of the event (its id, app, routing key, type and subject), within the AgentRun's 4 KiB input cap together
//     with the definition's instructions.
//
// The bridge is a network client of a service that is trusted to be glidepath-api and not to be well-formed: it has its own HTTP
// client (dial, TLS and header timeouts, no redirect followed, answers and lines capped), validates every event before use, and
// drops and counts the ones that are malformed or too large, never panicking on them. It runs on goroutines of its own, calls the
// gateway with deadlines, and handles one event at a time, so it cannot stall a tool call.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The bridge's limits and defaults.
const (
	DefaultPerHour = 6
	SeenKept       = 5000
	MaxSubject     = 1500
	Source         = "glidepath"      // the cursor's row in gateway_event_cursors
	BufferAge      = 24 * time.Hour   // glidepath-api's api.events.maxAge: a cursor older than this may have lost events
	RecentLimit    = 10000            // /v1/events/recent's maximum
	MaxJSON        = 8 << 20          // bytes read from a JSON answer at most
	MaxLine        = 64 << 10         // bytes of one stream line; a longer line is dropped
	MaxEvent       = 256 << 10        // bytes of one event's data; a larger event is dropped
	MaxEventID     = 200              // characters of an event id
	MaxField       = 256              // characters of a routing key, an app, a type or a subject id
	EventTimeout   = 2 * time.Minute  // one event's handling, all its agents
	DBTimeout      = 15 * time.Second // one ledger call
	FutureSlack    = 5 * time.Minute  // an event id from further ahead than this does not move the cursor
	jsonTimeout    = 60 * time.Second // one JSON read
	headerTimeout  = 30 * time.Second // glidepath-api's answer to a request, headers
	idleDefault    = 90 * time.Second // silence on the stream (pings included) after which it is dropped and reconnected
	backoffMin     = 5 * time.Second  //
	backoffMax     = 60 * time.Second //
	healthyStream  = 30 * time.Second // a stream that stayed up this long resets the reconnect backoff
)

var (
	eventIDRe  = regexp.MustCompile(`^[A-Za-z0-9:._/@+=-]{1,200}$`)
	cursorIDRe = regexp.MustCompile(`^[0-9]{16}-[0-9A-Za-z]{1,64}$`)
)

// Gateway is what the triggers need of the gateway (dispatch.Gateway).
type Gateway interface {
	Definitions() map[string]*definitions.Definition
	OpenTriggered(ctx context.Context, agent, trigger, taskID string) (*dispatch.Result, error)
	Launch(ctx context.Context, sessionID string, input map[string]any) (*dispatch.Result, error)
	SessionHasRun(sessionID string) bool
	RecordEvent(ctx context.Context, summary map[string]any, outcomes []map[string]any) (bool, error)
}

// Outcome is what was done for one agent for one event. New is false when the ledger already had a decision.
type Outcome struct {
	Agent   string
	Result  string // launched, deduped, braked, held, refused, launch-failed
	Via     string
	New     bool
	Replay  bool // the ledger row was written for this very event
	Session string
	Run     string
	Rule    string
}

func (o Outcome) record() map[string]any {
	m := map[string]any{"agent": o.Agent, "result": o.Result, "via": o.Via}
	if o.Session != "" {
		m["session"] = o.Session
	}
	if o.Run != "" {
		m["run"] = o.Run
	}
	if o.Rule != "" {
		m["rule"] = o.Rule
	}
	return m
}

// BridgeConfig is how a bridge reaches glidepath-api.
type BridgeConfig struct {
	BaseURL   string // glidepath-api, http(s), no credentials in it
	TokenFile string // the gateway's own token for it, read on every request
	// Now, Client, Idle and the backoff are for tests; zero values are the defaults.
	Now        func() time.Time
	Client     *http.Client
	Idle       time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration
	// Wait, when set, replaces the wait between streams (a test records the delays and returns at once); it reports false when ctx ended.
	Wait func(ctx context.Context, d time.Duration) bool
}

// Bridge is the event bridge.
type Bridge struct {
	gw      Gateway
	ledger  Ledger
	metrics Metrics
	log     *slog.Logger
	cfg     BridgeConfig
	base    *url.URL
	hc      *http.Client
	started time.Time

	handling sync.Mutex // the stream and the sweep hand events in from two goroutines
	lastID   string     // the saved cursor
	resumed  bool
	seen     map[string]struct{}
	seenQ    []string
	brakes   map[string]*RateLimiter
}

// NewBridge checks the configuration and builds a bridge. It does not connect.
func NewBridge(gw Gateway, ledger Ledger, cfg BridgeConfig, m Metrics, log *slog.Logger) (*Bridge, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("GLIDEPATH_API_URL is an http(s) URL with a host, no credentials, no query")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("the event bridge needs a token file")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Idle <= 0 {
		cfg.Idle = idleDefault
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = backoffMin
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = backoffMax
	}
	if m == nil {
		m = Nop{}
	}
	if log == nil {
		log = slog.Default()
	}
	hc := cfg.Client
	if hc == nil {
		tr := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: headerTimeout,
			MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
			DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
		// A redirect is never followed: the bridge sends the gateway's token, and a redirect is the one thing that could carry it
		// to another host (the second header is not one Go strips). An answer that is a redirect is an error.
		hc = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	b := &Bridge{gw: gw, ledger: ledger, metrics: m, log: log, cfg: cfg, base: u, hc: hc, started: cfg.Now(),
		seen: map[string]struct{}{}, brakes: map[string]*RateLimiter{}}
	return b, nil
}

// Now is the bridge's clock.
func (b *Bridge) Now() time.Time { return b.cfg.Now() }

func (b *Bridge) dbCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, DBTimeout)
}

// -- one event ----------------------------------------------------------------------------------------------------------------

// Handle starts every run an event triggers and returns what was done per agent. via is stream, recent or reconcile. A ledger
// failure is the error, and the event is then neither marked seen nor to be advanced past by the caller; a malformed event is
// dropped (counted) with no error.
func (b *Bridge) Handle(ctx context.Context, env map[string]any, via string) ([]Outcome, error) {
	b.handling.Lock()
	defer b.handling.Unlock()
	return b.handle(ctx, env, via)
}

// validEnvelope is what an event must be before anything is read from it: an id of the form ids have, text fields of bounded
// length, an object for the event. It says why not (a DropReasons value) when it is not.
func validEnvelope(env map[string]any) (reason string) {
	eid, _ := env["id"].(string)
	if !eventIDRe.MatchString(eid) {
		return "bad-id"
	}
	for _, k := range []string{"routingKey", "app", "receivedAt"} {
		if v, present := env[k]; present && v != nil {
			s, ok := v.(string)
			if !ok {
				return "malformed"
			}
			if len(s) > MaxField {
				return "oversize"
			}
		}
	}
	if ev, present := env["event"]; present && ev != nil {
		m, ok := ev.(map[string]any)
		if !ok {
			return "malformed"
		}
		if c, ok := m["context"].(map[string]any); ok {
			if t, ok := c["type"].(string); ok && len(t) > MaxField {
				return "oversize"
			}
		}
		if sub, ok := m["subject"].(map[string]any); ok {
			if s, ok := sub["id"].(string); ok && len(s) > MaxField {
				return "oversize"
			}
		}
	}
	return ""
}

func (b *Bridge) handle(ctx context.Context, env map[string]any, via string) ([]Outcome, error) {
	if reason := validEnvelope(env); reason != "" {
		b.metrics.Dropped(reason)
		b.log.Warn("an event was dropped", "reason", reason, "via", via)
		return nil, nil
	}
	eid := env["id"].(string)
	if _, dup := b.seen[eid]; dup {
		return nil, nil
	}
	key := TriggerKey(env)
	if key == "" {
		b.metrics.Dropped("bad-key")
		return nil, nil
	}
	var done []Outcome
	var failed error
	for _, m := range MatchGlidepathEvent(b.gw.Definitions(), env) {
		now := b.Now()
		cctx, cancel := b.dbCtx(ctx)
		won, err := b.ledger.Claim(cctx, m.Agent, key, via, eid, now, StaleBefore(b.started, now))
		cancel()
		if err != nil {
			b.log.Error("the trigger ledger could not claim", "agent", m.Agent, "err", errKind(err))
			failed = err
			continue
		}
		if !won {
			gctx, cancel := b.dbCtx(ctx)
			row, _ := b.ledger.Get(gctx, m.Agent, key)
			cancel()
			o := Outcome{Agent: m.Agent, Result: "deduped", Via: via}
			if row != nil {
				o.Replay = row.EventID != nil && *row.EventID == eid
				o.Session, o.Run = deref(row.Session), deref(row.Run)
			}
			done = append(done, o)
			continue
		}
		o := b.fire(ctx, m, env, key, via)
		dctx, cancel := b.dbCtx(ctx)
		err = b.ledger.Decide(dctx, m.Agent, key, o.Result, o.Session, o.Run)
		cancel()
		if err != nil {
			// The run (if any) exists and the row stays `claimed`: after a restart the claim is an orphan, and the live session
			// answers it as a dedupe. The caller does not advance the cursor, so the event is seen again.
			b.log.Error("the trigger ledger could not write a decision", "agent", m.Agent, "err", errKind(err))
			failed = err
		}
		done = append(done, o)
	}
	if failed != nil {
		return done, failed
	}
	b.markSeen(eid)
	for _, d := range done {
		b.metrics.Outcome(d.Agent, via, d.Result)
		b.log.Info("event handled", "event", eid, "key", str(env["routingKey"]), "via", via, "agent", d.Agent, "result", d.Result, "decided_before", !d.New)
	}
	switch via {
	case "reconcile":
		fresh := false
		for _, d := range done {
			if d.New {
				fresh = true
				b.metrics.Missed(d.Agent, d.Result) // the event path lost it, whatever the sweep then decided
			}
		}
		if fresh {
			b.record(ctx, env, done)
		}
	default:
		replay := len(done) > 0
		for _, d := range done {
			replay = replay && d.Replay
		}
		if !replay { // a replay of an event already kept is not new history
			b.record(ctx, env, done)
		}
	}
	if via == "stream" || via == "recent" {
		b.metrics.EventReceived(via)
		b.metrics.LastEvent(b.Now())
	}
	return done, nil
}

func (b *Bridge) markSeen(eid string) {
	if _, ok := b.seen[eid]; ok {
		return
	}
	b.seen[eid] = struct{}{}
	b.seenQ = append(b.seenQ, eid)
	for len(b.seenQ) > SeenKept {
		delete(b.seen, b.seenQ[0])
		b.seenQ = b.seenQ[1:]
	}
}

func (b *Bridge) brake(agent string, perHour int) *RateLimiter {
	r := b.brakes[agent]
	if r == nil || r.PerHour != perHour {
		// A definition whose limit changed gets a fresh window at the new limit (Python keeps the first limit it saw for as long
		// as the process runs).
		hits := []time.Time(nil)
		if r != nil {
			r.mu.Lock()
			hits = append(hits, r.hits...)
			r.mu.Unlock()
		}
		r = &RateLimiter{PerHour: perHour, hits: hits}
		b.brakes[agent] = r
	}
	return r
}

// fire brakes, opens and launches one claimed trigger; the decision for its ledger row.
func (b *Bridge) fire(ctx context.Context, m Match, env map[string]any, key, via string) (out Outcome) {
	eid := env["id"].(string)
	out = Outcome{Agent: m.Agent, Via: via, New: true}
	perHour := m.Trigger.MaxPerHour
	if perHour == 0 {
		perHour = DefaultPerHour
	}
	if !b.brake(m.Agent, perHour).Allow(b.Now()) {
		b.log.Warn("an event is over its agent's storm brake and was dropped", "event", eid, "agent", m.Agent, "per_hour", perHour)
		out.Result = "braked"
		return out
	}
	fctx, cancel := context.WithTimeout(ctx, EventTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil { // one agent's failure must not skip the others for this event
			b.log.Error("a trigger panicked", "event", eid, "agent", m.Agent, "panic", fmt.Sprint(r))
			out.Result = "launch-failed"
		}
	}()
	r, err := b.gw.OpenTriggered(fctx, m.Agent, "glidepath:"+eid, EventTaskID(m.Agent, key))
	if err != nil {
		b.log.Error("a trigger could not open its session", "event", eid, "agent", m.Agent, "err", errKind(err))
		out.Result = "launch-failed"
		return out
	}
	if !r.OK {
		// R024: a person holds this agent's triggers; the event is history, not a run.
		out.Rule, _ = r.Decision["rule"].(string)
		out.Result = "refused"
		if out.Rule == "R024" {
			out.Result = "held"
		}
		return out
	}
	data, _ := r.Data.(map[string]any)
	sid := ""
	if r.Session != nil {
		sid = *r.Session
	}
	out.Session = sid
	if deduped, _ := data["deduped"].(bool); deduped && b.gw.SessionHasRun(sid) {
		out.Result = "deduped"
		return out
	}
	// A new session, or a live one an earlier firing of this trigger opened and died before it launched (nothing else makes a live
	// session of this key with no run: a firing that failed to launch is decided, and a decided row is never claimed again).
	// Launch refuses a session that already has a run, so a repair cannot make a second one.
	d := b.gw.Definitions()[m.Agent]
	input := map[string]any{"event": Summarize(env)}
	if d != nil && d.Instructions != nil && *d.Instructions != "" {
		input["instructions"] = *d.Instructions
	}
	launched, err := b.gw.Launch(fctx, sid, input)
	if err != nil {
		b.log.Error("a trigger's run was not launched", "event", eid, "agent", m.Agent, "err", errKind(err))
		out.Result = "launch-failed"
		return out
	}
	out.Result = "launch-failed"
	if launched.OK {
		out.Result = "launched"
		if data, ok := launched.Data.(map[string]any); ok {
			out.Run, _ = data["run"].(string)
		}
	}
	return out
}

// Summarize is the part of an event a run gets: enough to find the run in Glidepath, small enough for the input cap
// (event_bridge.summarize). Only the plain values of the subject's content are kept, each of at most 300 characters; while the
// subject is over MaxSubject bytes, the largest value goes.
func Summarize(env map[string]any) map[string]any {
	ev, _ := env["event"].(map[string]any)
	subject, _ := ev["subject"].(map[string]any)
	content, _ := subject["content"].(map[string]any)
	small := map[string]any{}
	for k, v := range content {
		switch x := v.(type) {
		case string:
			if len([]rune(x)) <= 300 {
				small[k] = x
			}
		case bool:
			small[k] = x
		case float64:
			small[k] = x
		case json.Number:
			if len(x) <= 300 {
				small[k] = x
			}
		}
	}
	typ, _ := ev["context"].(map[string]any)
	out := map[string]any{"id": env["id"], "app": env["app"], "routingKey": env["routingKey"], "receivedAt": env["receivedAt"], "type": typ["type"],
		"subject": map[string]any{"id": subject["id"], "content": small}}
	for len(small) > 0 {
		raw, _ := json.Marshal(out["subject"])
		if len(raw) <= MaxSubject {
			break
		}
		longest, n := "", -1
		for k, v := range small {
			raw, _ := json.Marshal(v)
			if len(raw) > n || (len(raw) == n && k > longest) {
				longest, n = k, len(raw)
			}
		}
		delete(small, longest)
	}
	return out
}

func (b *Bridge) record(ctx context.Context, env map[string]any, done []Outcome) {
	outs := make([]map[string]any, 0, len(done))
	for _, d := range done {
		outs = append(outs, d.record())
	}
	rctx, cancel := b.dbCtx(ctx)
	defer cancel()
	// History is best effort: it never stops a run.
	if _, err := b.gw.RecordEvent(rctx, Summarize(env), outs); err != nil {
		b.log.Warn("an event's history was not recorded", "event", str(env["id"]), "err", errKind(err))
	}
}

// IDTime is the receive time a glidepath-api event id carries (`<unix microseconds, 16 digits>-<hash>`), or false.
func IDTime(id string) (time.Time, bool) {
	if !cursorIDRe.MatchString(id) {
		return time.Time{}, false
	}
	var us int64
	for _, c := range id[:16] {
		us = us*10 + int64(c-'0')
	}
	return time.UnixMicro(us).UTC(), true
}

// advance moves the saved cursor to eid if it is newer (ids order by receive time) and it is an id the cursor may follow.
func (b *Bridge) advance(ctx context.Context, eid string) error {
	t, ok := IDTime(eid)
	if !ok || t.After(b.Now().Add(FutureSlack)) {
		return nil
	}
	if eid > b.lastID {
		b.lastID = eid
	}
	cctx, cancel := b.dbCtx(ctx)
	defer cancel()
	if err := b.ledger.Advance(cctx, Source, eid); err != nil {
		return err
	}
	if lt, ok := IDTime(b.lastID); ok {
		b.metrics.CursorTime(lt)
	}
	return nil
}

// handleOne handles one event from the stream or the catch-up and moves the cursor. An error is the ledger's: the stream must end.
func (b *Bridge) handleOne(ctx context.Context, env map[string]any, via string) error {
	hctx, cancel := context.WithTimeout(ctx, EventTimeout)
	defer cancel()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil { // one bad event must not end the stream
				b.log.Error("an event's handling panicked", "event", str(env["id"]), "panic", fmt.Sprint(r))
				b.metrics.Dropped("malformed")
			}
		}()
		_, err = b.Handle(hctx, env, via)
	}()
	if err != nil {
		return err
	}
	if eid, ok := env["id"].(string); ok && eventIDRe.MatchString(eid) {
		return b.advance(ctx, eid)
	}
	return nil
}

// -- glidepath-api -------------------------------------------------------------------------------------------------------------

func (b *Bridge) newRequest(ctx context.Context, path string, params url.Values, accept string) (*http.Request, error) {
	raw, err := os.ReadFile(b.cfg.TokenFile)
	if err != nil {
		return nil, errors.New("the token file could not be read")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("the token file is empty")
	}
	u := *b.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("the request could not be built")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Glidepath-Authorization", "Bearer "+token)
	return req, nil
}

// GetJSON is one JSON read from glidepath-api, as the gateway: its own timeout, at most MaxJSON bytes, and an answer that is not a
// 2xx (a redirect included) is an error that carries no body.
func (b *Bridge) GetJSON(ctx context.Context, path string, params url.Values) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, jsonTimeout)
	defer cancel()
	req, err := b.newRequest(ctx, path, params, "application/json")
	if err != nil {
		return nil, err
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", path, errKind(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxJSON+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %s", path, errKind(err))
	}
	if len(body) > MaxJSON {
		return nil, fmt.Errorf("%s: answer over %d bytes", path, MaxJSON)
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s: the answer is not JSON", path)
	}
	return out, nil
}

// Resume loads the saved cursor and handles what `/v1/events/recent?since=` holds after it. It returns the result counted as
// resume (`none` when there is no cursor yet: the first start).
func (b *Bridge) Resume(ctx context.Context) (string, error) {
	cctx, cancel := b.dbCtx(ctx)
	cur, err := b.ledger.Cursor(cctx, Source, b.Now())
	cancel()
	if err != nil {
		return "", err
	}
	b.resumed = true
	if cur.LastID == nil {
		return "none", nil
	}
	b.lastID = *cur.LastID
	t, hasTime := IDTime(b.lastID)
	if hasTime {
		b.metrics.CursorTime(t)
	}
	v, err := b.GetJSON(ctx, "/v1/events/recent", url.Values{"since": {b.lastID}, "limit": {fmt.Sprint(RecentLimit)}})
	if err != nil {
		b.log.Warn("the event catch-up failed; the stream resumes with Last-Event-ID", "from", b.lastID, "err", errKind(err))
		b.metrics.Resume("failed")
		return "failed", nil
	}
	n := 0
	if page, ok := v.(map[string]any); ok {
		list, _ := page["events"].([]any)
		for _, e := range list {
			if ctx.Err() != nil {
				break
			}
			env, ok := e.(map[string]any)
			if !ok {
				b.metrics.Dropped("malformed")
				continue
			}
			if n++; n > RecentLimit {
				break
			}
			if err := b.handleOne(ctx, env, "recent"); err != nil {
				return "", err
			}
		}
	}
	result := "caught-up"
	if hasTime && b.Now().Sub(t) > BufferAge {
		result = "stale-cursor"
		b.log.Warn("the event cursor is older than glidepath-api's buffer: events may be gone; the reconciliation sweep recovers a missed pipelinerun.finished", "cursor", b.lastID)
	}
	b.log.Info("event catch-up", "from", b.lastID, "events", n, "result", result)
	b.metrics.Resume(result)
	return result, nil
}

// RunOnce holds one stream until it ends or fails. Every event is handled; a malformed one is dropped, a ledger failure ends the
// stream (the error) so the cursor stays at the last event that was fully handled.
func (b *Bridge) RunOnce(ctx context.Context) (delivered int, err error) {
	if !b.resumed {
		if _, err := b.Resume(ctx); err != nil {
			return 0, err
		}
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	params := url.Values{}
	req, err := b.newRequest(sctx, "/v1/events", params, "text/event-stream")
	if err != nil {
		return 0, err
	}
	if b.lastID != "" && eventIDRe.MatchString(b.lastID) {
		req.Header.Set("Last-Event-ID", b.lastID)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("the event stream: %s", errKind(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the event stream: HTTP %d", resp.StatusCode)
	}
	b.metrics.StreamConnected(true)
	defer b.metrics.StreamConnected(false)
	// The stream is dropped when nothing, a ping included, arrives for the idle time.
	idle := time.AfterFunc(b.cfg.Idle, cancel)
	defer idle.Stop()
	rd := bufio.NewReaderSize(resp.Body, 32<<10)
	for {
		ev, rerr := readEvent(rd, idle, b.cfg.Idle)
		if ev.drop != "" {
			b.metrics.Dropped(ev.drop)
			b.log.Warn("an event on the stream was dropped", "reason", ev.drop)
		}
		if ev.data != "" || ev.id != "" {
			if env, ok := decodeEnvelope(ev); ok {
				if err := b.handleOne(ctx, env, "stream"); err != nil {
					return delivered, err
				}
				delivered++
			} else if ev.drop == "" && ev.data != "" {
				b.metrics.Dropped("malformed")
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return delivered, nil
			}
			if errors.Is(rerr, io.EOF) {
				return delivered, nil
			}
			return delivered, fmt.Errorf("the event stream: %s", errKind(rerr))
		}
		if ctx.Err() != nil {
			return delivered, nil
		}
	}
}

func decodeEnvelope(ev sseEvent) (map[string]any, bool) {
	if ev.drop != "" || ev.data == "" {
		return nil, false
	}
	var env map[string]any
	if json.Unmarshal([]byte(ev.data), &env) != nil || env == nil {
		return nil, false
	}
	if id, _ := env["id"].(string); id == "" && ev.id != "" {
		env["id"] = ev.id
	}
	return env, true
}

// Run holds the stream, reconnecting after a failure with a bounded, jittered backoff (5 s, doubling to 60 s; a stream that was
// up for 30 s or delivered an event starts it over), until ctx ends. A failure is logged by its kind, never by what it carried.
func (b *Bridge) Run(ctx context.Context) {
	wait := b.cfg.BackoffMin
	for ctx.Err() == nil {
		began := b.Now()
		n, err := b.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.log.Warn("the event stream ended; reconnecting", "err", errKind(err), "in", wait.String())
		}
		if n > 0 || b.Now().Sub(began) >= healthyStream {
			wait = b.cfg.BackoffMin
		}
		d := wait + time.Duration(rand.Int64N(int64(wait)/5+1))
		if b.cfg.Wait != nil {
			if !b.cfg.Wait(ctx, d) {
				return
			}
		} else {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}
		if n == 0 {
			wait = min(wait*2, b.cfg.BackoffMax)
		}
	}
}

// -- the server-sent events stream ----------------------------------------------------------------------------------------------

type sseEvent struct {
	id, data string
	drop     string // set when the event was too large or unreadable: it is dropped, and the stream goes on
}

// readEvent reads one server-sent event: comments (`: ping`) and other fields are skipped; every line read resets the idle timer.
// A line over MaxLine bytes, or an event over MaxEvent, is discarded to its end and the event is dropped (drop "oversize").
func readEvent(rd *bufio.Reader, idle *time.Timer, idleFor time.Duration) (ev sseEvent, err error) {
	var data []string
	size := 0
	for {
		line, tooLong, lerr := readLine(rd)
		idle.Reset(idleFor)
		if tooLong {
			ev.drop = "oversize"
		}
		if lerr != nil && line == "" {
			return sseEvent{drop: ev.drop}, lerr // an event the stream ended in the middle of is not delivered
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 || ev.drop != "" {
				if ev.drop == "" {
					ev.data = strings.Join(data, "\n")
				}
				return ev, lerr
			}
			ev.id = ""
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id:"):
			ev.id = strings.TrimSpace(line[3:])
			if len(ev.id) > MaxEventID {
				ev.id, ev.drop = "", "oversize"
			}
		case strings.HasPrefix(line, "data:"):
			if size += len(line); size > MaxEvent {
				ev.drop = "oversize"
				data = nil
			} else if ev.drop == "" {
				data = append(data, strings.TrimLeft(line[5:], " "))
			}
		}
		if lerr != nil {
			return sseEvent{drop: ev.drop}, lerr
		}
	}
}

// readLine reads one line of at most MaxLine bytes; a longer one is read to its end and thrown away (tooLong).
func readLine(rd *bufio.Reader) (line string, tooLong bool, err error) {
	var buf []byte
	for {
		part, isPrefix, e := rd.ReadLine()
		if e != nil {
			return string(buf), tooLong, e
		}
		if !tooLong {
			if len(buf)+len(part) > MaxLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, part...)
			}
		}
		if !isPrefix {
			return string(buf), tooLong, nil
		}
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func str(v any) string {
	s, _ := v.(string)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// errKind names an error without what it carries: a URL or a header value in it could hold a token.
func errKind(err error) string {
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	return fmt.Sprintf("%T", err)
}
