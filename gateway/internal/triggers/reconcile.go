package triggers

// The reconciliation sweep (reconcile.py): a `pipelinerun.finished` the event bridge never received still launches, once.
//
// glidepath-api's event buffer is in memory: when every replica restarts at once, or the gateway is down longer than the buffer
// reaches back, events are gone and nothing says so. The run records are not: glidepath-api's `GET /v1/runs` reads Tekton Results,
// 30 days of finished PipelineRuns. Every interval the sweep reads the finished runs of the last window hours for the apps a
// `pipelinerun.finished` trigger can match, rebuilds the event each one's finally block sent (EnvelopeFromRun), and hands it to the
// bridge as `via reconcile`. The bridge claims its trigger key in the ledger: a run whose event arrived was claimed then, and
// loses here; a run whose event never arrived is claimed now, launched under the same storm brake and hold, counted in the missed
// series and kept in the event history under id `reconcile:<app>/<run>`.
//
// Two bounds keep it from launching what it should not: the ledger's floor (runs that finished before the bridge first kept a
// ledger are never reconciled), and Settle (a run that finished in the last two minutes is left to its event). A row is untrusted
// like an event: one that does not have an app and a name of the forms they have is skipped.

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// The sweep's constants (reconcile.py).
const (
	Settle         = 2 * time.Minute
	ReconcileEvery = 300 * time.Second
	WindowHours    = 24
	RunsLimit      = 1000 // GET /v1/runs's maximum
	EventType      = "dev.cdevents.pipelinerun.finished.0.3.0"
	maxSweepRows   = 20000
	maxSweepApps   = 200
)

var (
	outcomes = map[string]string{"succeeded": "success", "failed": "failure", "cancelled": "cancel"} // anything else: error
	// runNameRe is what a PipelineRun, an app and a namespace are called (DNS-1123 names).
	nameRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	nanosRe = regexp.MustCompile(`(\.\d{6})\d+`)
)

// ParseRunTime reads an RFC 3339 time as Tekton Results writes it (nanoseconds, `Z`), or false.
func ParseRunTime(s string) (time.Time, bool) {
	if s == "" || len(s) > 64 {
		return time.Time{}, false
	}
	s = strings.Replace(nanosRe.ReplaceAllString(s, "$1"), "Z", "+00:00", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05.999999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// EnvelopeFromRun is the `pipelinerun.finished` envelope a finished run (a GET /v1/runs row) sent, or false when the row lacks what
// the key needs (an app and a name of the forms they have).
func EnvelopeFromRun(row map[string]any) (map[string]any, bool) {
	app, _ := row["app"].(string)
	name, _ := row["name"].(string)
	ns, _ := row["namespace"].(string)
	if !nameRe.MatchString(app) || len(app) > MaxField || !nameRe.MatchString(name) || len(name) > MaxField || (ns != "" && !nameRe.MatchString(ns)) {
		return nil, false
	}
	status, _ := row["status"].(string)
	outcome, ok := outcomes[status]
	if !ok {
		outcome = "error"
	}
	content := map[string]any{"outcome": outcome}
	if p, ok := row["pipeline"].(string); ok && p != "" && len(p) <= MaxField {
		content["pipelineName"] = p
	}
	var source any
	if ns != "" {
		source = fmt.Sprintf("/glidepath/%s/%s", ns, name)
	}
	var received any
	if e, ok := row["endTime"].(string); ok && len(e) <= 64 {
		received = e
	}
	return map[string]any{"id": "reconcile:" + app + "/" + name, "app": app, "routingKey": app + ".pipelinerun.finished", "receivedAt": received,
		"event": map[string]any{"context": map[string]any{"type": EventType, "source": source},
			"subject": map[string]any{"id": name, "type": "pipelineRun", "content": content}}}, true
}

// WatchedApps is (any, apps): whether any Glidepath trigger can match a `<app>.pipelinerun.finished`, and the literal apps named;
// an empty set with any=true means a wildcard app (read the whole fleet).
func WatchedApps(defs map[string]*definitions.Definition) (bool, map[string]bool) {
	found, wildcard := false, false
	apps := map[string]bool{}
	for _, d := range defs {
		for _, t := range d.Triggers {
			if t.Type != "event" {
				continue
			}
			first := strings.SplitN(t.BindingKey, ".", 2)[0]
			probe := first + ".pipelinerun.finished"
			if first == "*" || first == "#" {
				probe = "x.pipelinerun.finished"
			}
			if !AMQPTopicMatch(t.BindingKey, probe) {
				continue
			}
			found = true
			if first == "*" || first == "#" {
				wildcard = true
			} else {
				apps[first] = true
			}
		}
	}
	if wildcard {
		return found, map[string]bool{}
	}
	return found, apps
}

// Reconciler is the sweep.
type Reconciler struct {
	bridge      *Bridge
	log         *slog.Logger
	Interval    time.Duration
	WindowHours int
}

// NewReconciler builds a sweep over a bridge (its client, its ledger, its gateway, its clock).
func NewReconciler(b *Bridge, interval time.Duration, windowHours int) *Reconciler {
	if interval <= 0 {
		interval = ReconcileEvery
	}
	if windowHours <= 0 {
		windowHours = WindowHours
	}
	return &Reconciler{bridge: b, log: b.log, Interval: interval, WindowHours: windowHours}
}

func (r *Reconciler) runs(ctx context.Context, apps map[string]bool) ([]map[string]any, error) {
	names := make([]string, 0, len(apps))
	for a := range apps {
		names = append(names, a)
	}
	sort.Strings(names)
	if len(names) == 0 {
		names = []string{""} // the whole fleet
	}
	if len(names) > maxSweepApps {
		names = names[:maxSweepApps]
	}
	var rows []map[string]any
	for _, app := range names {
		q := url.Values{"sinceHours": {fmt.Sprint(r.WindowHours)}, "limit": {fmt.Sprint(RunsLimit)}}
		if app != "" {
			q.Set("app", app)
		}
		v, err := r.bridge.GetJSON(ctx, "/v1/runs", q)
		if err != nil {
			return nil, err
		}
		page, _ := v.(map[string]any)
		list, _ := page["runs"].([]any)
		n := 0
		for _, x := range list {
			if m, ok := x.(map[string]any); ok && len(rows) < maxSweepRows {
				rows = append(rows, m)
				n++
			}
		}
		if n >= RunsLimit {
			r.log.Warn("reconcile: glidepath-api returned the most runs it gives: older runs in the window are not checked", "app", app, "runs", n)
		}
	}
	return rows, nil
}

// Sweep is one pass: what was newly decided (each a missed trigger). It fails when glidepath-api cannot be read.
func (r *Reconciler) Sweep(ctx context.Context) ([]Outcome, error) {
	b := r.bridge
	m := b.metrics
	pctx, cancel := b.dbCtx(ctx)
	_, _ = b.ledger.Prune(pctx, b.Now()) // retention holds on every path, matched or not
	cancel()
	found, apps := WatchedApps(b.gw.Definitions())
	if !found {
		m.Sweep("ok")
		m.SweepOK(b.Now())
		return nil, nil
	}
	now := b.Now()
	cctx, cancel := b.dbCtx(ctx)
	cur, err := b.ledger.Cursor(cctx, Source, now)
	cancel()
	if err != nil {
		m.Sweep("failed")
		return nil, err
	}
	rows, err := r.runs(ctx, apps)
	if err != nil {
		m.Sweep("failed")
		return nil, err
	}
	type cand struct {
		end time.Time
		env map[string]any
	}
	var cands []cand
	for _, row := range rows {
		es, _ := row["endTime"].(string)
		end, ok := ParseRunTime(es)
		if !ok || end.Before(cur.Floor) || now.Sub(end) < Settle {
			continue
		}
		if env, ok := EnvelopeFromRun(row); ok {
			cands = append(cands, cand{end, env})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].end.Before(cands[j].end) }) // oldest first, as the stream would have
	var fresh []Outcome
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		hctx, cancel := context.WithTimeout(ctx, EventTimeout)
		done, err := b.Handle(hctx, c.env, "reconcile")
		cancel()
		if err != nil {
			m.Sweep("failed")
			return fresh, err
		}
		for _, d := range done {
			if d.New {
				fresh = append(fresh, d)
			}
		}
	}
	if len(fresh) > 0 {
		var parts []string
		for _, d := range fresh {
			parts = append(parts, d.Agent+" "+d.Result)
		}
		r.log.Warn("reconcile: triggers no event delivered", "n", len(fresh), "what", strings.Join(parts, ", "))
	}
	m.Sweep("ok")
	m.SweepOK(b.Now())
	return fresh, nil
}

// Run sweeps every Interval (the first sweep waits one interval: at start the bridge's own catch-up goes first) until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		func() {
			defer func() {
				if x := recover(); x != nil {
					r.log.Error("a reconcile sweep panicked", "panic", fmt.Sprint(x))
				}
			}()
			if _, err := r.Sweep(ctx); err != nil {
				r.log.Error("a reconcile sweep failed", "err", errKind(err))
			}
		}()
	}
}
