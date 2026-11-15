// Package metrics is the gateway's Prometheus endpoint (src/autopilot/metrics.py): a few counters and gauges in the text format 0.0.4,
// with no client library, on a port of their own (METRICS_PORT, default 9090; 0 turns it off), so the route to /mcp never exposes
// them and a NetworkPolicy can admit Prometheus to this port alone. The names, types, help text, label names, label order, the
// sorting of series and the number format are Python's (a test renders both and compares), because dashboards and alerts read them.
//
// What differs, on purpose: a metric holds at most MaxSeries series (a label that grew without bound would otherwise fill the
// process's memory: the series past the cap are not recorded, and the number refused is logged once per metric), and a label value is
// cut at LabelValueMax characters when it is recorded, not when it is rendered.
package metrics

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// LabelValueMax is the longest label value rendered (Python: LABEL_VALUE_MAX), in characters.
const LabelValueMax = 128

// MaxSeries is the most series one metric keeps (Go only).
const MaxSeries = 5000

type kind string

const (
	counterKind kind = "counter"
	gaugeKind   kind = "gauge"
)

// metric is one named family.
type metric struct {
	name, help string
	kind       kind
	labels     []string

	mu      sync.Mutex
	values  map[string]*sample // by the label values joined with \x00
	refused int
}

type sample struct {
	labels []string
	v      float64
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= LabelValueMax {
		return s
	}
	r := []rune(s)
	return string(r[:LabelValueMax])
}

func (m *metric) slot(values []string) *sample {
	if len(values) != len(m.labels) {
		panic(fmt.Sprintf("metrics: %s takes labels %v, not %d values", m.name, m.labels, len(values)))
	}
	cl := make([]string, len(values))
	for i, v := range values {
		cl[i] = clip(v)
	}
	k := strings.Join(cl, "\x00")
	s := m.values[k]
	if s == nil {
		if len(m.values) >= MaxSeries {
			if m.refused == 0 {
				slog.Warn("a metric holds the most series it may: new label values are not recorded", "metric", m.name, "max", MaxSeries)
			}
			m.refused++
			return nil
		}
		s = &sample{labels: cl}
		m.values[k] = s
	}
	return s
}

// Value is a sample's value (0 when there is none).
func (m *metric) Value(values ...string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := make([]string, len(values))
	for i, v := range values {
		cl[i] = clip(v)
	}
	if s := m.values[strings.Join(cl, "\x00")]; s != nil {
		return s.v
	}
	return 0
}

// Inc adds n to a counter's sample.
func (m *metric) Inc(n float64, values ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.slot(values); s != nil {
		s.v += n
	}
}

// Set sets a gauge's sample.
func (m *metric) Set(v float64, values ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.slot(values); s != nil {
		s.v = v
	}
}

func escape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

// g is Python's `{v:g}`: six significant digits, an exponent form for the very large and very small, no trailing zeros.
func g(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

func less(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (m *metric) render(b *strings.Builder) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
	m.mu.Lock()
	items := make([]*sample, 0, len(m.values))
	for _, s := range m.values {
		items = append(items, &sample{labels: s.labels, v: s.v})
	}
	m.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return less(items[i].labels, items[j].labels) })
	// An unlabelled counter starts at 0; a gauge has no sample until it is set, so an alert on a timestamp gauge never fires for a
	// feature that is off or has not run yet.
	if len(items) == 0 && len(m.labels) == 0 && m.kind == counterKind {
		items = []*sample{{v: 0}}
	}
	for _, s := range items {
		if len(m.labels) == 0 {
			fmt.Fprintf(b, "%s %s\n", m.name, g(s.v))
			continue
		}
		parts := make([]string, len(m.labels))
		for i, l := range m.labels {
			parts[i] = fmt.Sprintf(`%s="%s"`, l, escape(s.labels[i]))
		}
		fmt.Fprintf(b, "%s{%s} %s\n", m.name, strings.Join(parts, ","), g(s.v))
	}
}

// Registry is the metrics of one process, in the order they were registered.
type Registry struct {
	mu      sync.Mutex
	metrics []*metric
}

func (r *Registry) add(name, help string, k kind, labels ...string) *metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.metrics {
		if x.name == name {
			panic("metrics: " + name + " registered twice")
		}
	}
	m := &metric{name: name, help: help, kind: k, labels: labels, values: map[string]*sample{}}
	r.metrics = append(r.metrics, m)
	return m
}

// Render is the exposition.
func (r *Registry) Render() string {
	r.mu.Lock()
	ms := append([]*metric(nil), r.metrics...)
	r.mu.Unlock()
	var b strings.Builder
	for _, m := range ms {
		m.render(&b)
	}
	return b.String()
}

// Names are the registered metric names, in order.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.metrics))
	for i, m := range r.metrics {
		out[i] = m.name
	}
	return out
}

// Trigger is the series the bridge, the sweep and the scheduler write (metrics.TriggerMetrics), and it is triggers.Metrics. The first
// twelve are Python's; the four after them are the Go gateway's own (the bridge drops, holds and poisons events Python has no counter
// for), registered last so Python's block is a prefix of the exposition.
type Trigger struct {
	Registry                                                                                           *Registry
	eventsReceived, streamConnected, lastEvent, cursorTime, resumes, outcomes, missed, sweeps, sweepOK *metric
	slotLaunches, slotMissed, slotLag, dropped, stuck, poisoned, panics                                *metric
}

// NewTrigger registers the series.
func NewTrigger() *Trigger {
	r := &Registry{}
	t := &Trigger{Registry: r}
	t.eventsReceived = r.add("autopilot_events_received_total", "Glidepath events the trigger bridge handled, by how they arrived.", counterKind, "via")
	t.streamConnected = r.add("autopilot_events_stream_connected", "1 while the bridge holds glidepath-api's event stream.", gaugeKind)
	t.lastEvent = r.add("autopilot_events_last_received_timestamp_seconds", "Unix time the bridge last handled an event.", gaugeKind)
	t.cursorTime = r.add("autopilot_events_cursor_timestamp_seconds", "Receive time carried by the bridge's saved cursor (the newest event id handled).", gaugeKind)
	t.resumes = r.add("autopilot_events_resume_total", "Catch-ups from the saved cursor at (re)connect, by result: caught-up, stale-cursor (older than glidepath-api's buffer: events may be gone), failed.", counterKind, "result")
	t.outcomes = r.add("autopilot_trigger_outcomes_total", "What a trigger firing became, per agent, by path and result.", counterKind, "agent", "via", "result")
	t.missed = r.add("autopilot_trigger_missed_total", "Trigger firings no event delivered, found by the reconciliation sweep (the gap), by what the sweep decided: launched, or held, braked, refused, launch-failed.", counterKind, "agent", "result")
	t.sweeps = r.add("autopilot_reconcile_sweeps_total", "Reconciliation sweeps, by result (ok, failed).", counterKind, "result")
	t.sweepOK = r.add("autopilot_reconcile_last_success_timestamp_seconds", "Unix time of the last reconciliation sweep that read glidepath-api's run records.", gaugeKind)
	t.slotLaunches = r.add("autopilot_schedule_launches_total", "Interval slots fired, per agent, by result.", counterKind, "agent", "result")
	t.slotMissed = r.add("autopilot_schedule_missed_total", "Interval slots whose grace window passed with no firing (the gateway was down).", counterKind, "agent")
	t.slotLag = r.add("autopilot_schedule_lag_seconds", "Seconds between the start of an agent's last fired slot and its firing.", gaugeKind, "agent")
	// Go's own.
	t.dropped = r.add("autopilot_events_dropped_total", "Glidepath events the bridge dropped without firing, by reason (oversize, malformed, bad-id, bad-key, unreadable-line).", counterKind, "reason")
	t.stuck = r.add("autopilot_trigger_stuck_attempts", "How many times in a row the handling of one agent's trigger failed without being decided (0 once decided).", gaugeKind, "agent")
	t.poisoned = r.add("autopilot_trigger_poisoned_total", "Triggers decided launch-failed because they kept failing, per agent.", counterKind, "agent")
	t.panics = r.add("autopilot_trigger_handler_panics_total", "Event handlers that panicked outside one agent's trigger.", counterKind)
	return t
}

// PythonNames are the metrics Python's TriggerMetrics registers, in its order.
var PythonNames = []string{
	"autopilot_events_received_total", "autopilot_events_stream_connected", "autopilot_events_last_received_timestamp_seconds",
	"autopilot_events_cursor_timestamp_seconds", "autopilot_events_resume_total", "autopilot_trigger_outcomes_total",
	"autopilot_trigger_missed_total", "autopilot_reconcile_sweeps_total", "autopilot_reconcile_last_success_timestamp_seconds",
	"autopilot_schedule_launches_total", "autopilot_schedule_missed_total", "autopilot_schedule_lag_seconds",
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func (t *Trigger) EventReceived(via string) { t.eventsReceived.Inc(1, via) }
func (t *Trigger) StreamConnected(up bool) {
	v := 0.0
	if up {
		v = 1
	}
	t.streamConnected.Set(v)
}
func (t *Trigger) LastEvent(at time.Time)            { t.lastEvent.Set(unix(at)) }
func (t *Trigger) CursorTime(at time.Time)           { t.cursorTime.Set(unix(at)) }
func (t *Trigger) Resume(result string)              { t.resumes.Inc(1, result) }
func (t *Trigger) Outcome(agent, via, result string) { t.outcomes.Inc(1, agent, via, result) }
func (t *Trigger) Missed(agent, result string)       { t.missed.Inc(1, agent, result) }
func (t *Trigger) Sweep(result string)               { t.sweeps.Inc(1, result) }
func (t *Trigger) SweepOK(at time.Time)              { t.sweepOK.Set(unix(at)) }
func (t *Trigger) SlotLaunch(agent, result string)   { t.slotLaunches.Inc(1, agent, result) }
func (t *Trigger) SlotMissed(agent string)           { t.slotMissed.Inc(1, agent) }
func (t *Trigger) SlotLag(agent string, sec float64) { t.slotLag.Set(sec, agent) }
func (t *Trigger) Dropped(reason string)             { t.dropped.Inc(1, reason) }
func (t *Trigger) Stuck(agent string, attempts int)  { t.stuck.Set(float64(attempts), agent) }
func (t *Trigger) Poisoned(agent string)             { t.poisoned.Inc(1, agent) }
func (t *Trigger) Panicked()                         { t.panics.Inc(1) }

// Raw access for the equivalence test: the Python script names the series, not the Go method.
func (t *Trigger) Series(name string) *metric {
	t.Registry.mu.Lock()
	defer t.Registry.mu.Unlock()
	for _, m := range t.Registry.metrics {
		if m.name == name {
			return m
		}
	}
	return nil
}
