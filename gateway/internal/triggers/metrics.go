package triggers

import (
	"sync"
	"time"
)

// Metrics are the counters and gauges the bridge, the sweep and the scheduler write (metrics.TriggerMetrics). The Prometheus
// endpoint that serves them is S6's (internal/metrics.Trigger implements it); Nop is for a gateway with METRICS_PORT=0 and Recorder is what tests use.
// Every label is from a closed set or an agent name, so a source cannot grow a series.
type Metrics interface {
	EventReceived(via string)
	StreamConnected(up bool)
	LastEvent(t time.Time)
	CursorTime(t time.Time)
	Resume(result string) // caught-up, stale-cursor, failed
	Outcome(agent, via, result string)
	Missed(agent, result string)
	Sweep(result string) // ok, failed
	SweepOK(t time.Time)
	SlotLaunch(agent, result string)
	SlotMissed(agent string)
	SlotLag(agent string, seconds float64)
	// Dropped counts an event the bridge dropped: reason is one of DropReasons.
	Dropped(reason string)
	// Stuck says how many times in a row the handling of one agent's trigger failed without being decided (0 when it was decided).
	// Together with the cursor's lag it is what an operator alerts on: while an event is stuck the cursor does not move.
	Stuck(agent string, attempts int)
	// Poisoned counts an event whose trigger was decided launch-failed because it kept failing (see PoisonAttempts).
	Poisoned(agent string)
	// Panicked counts a handler that panicked outside one agent's trigger.
	Panicked()
}

// DropReasons are why an event is dropped, never fired.
var DropReasons = []string{"oversize", "malformed", "bad-id", "bad-key", "unreadable-line"}

// Nop is Metrics that count nothing.
type Nop struct{}

func (Nop) EventReceived(string)           {}
func (Nop) StreamConnected(bool)           {}
func (Nop) LastEvent(time.Time)            {}
func (Nop) CursorTime(time.Time)           {}
func (Nop) Resume(string)                  {}
func (Nop) Outcome(string, string, string) {}
func (Nop) Missed(string, string)          {}
func (Nop) Sweep(string)                   {}
func (Nop) SweepOK(time.Time)              {}
func (Nop) SlotLaunch(string, string)      {}
func (Nop) SlotMissed(string)              {}
func (Nop) SlotLag(string, float64)        {}
func (Nop) Dropped(string)                 {}
func (Nop) Stuck(string, int)              {}
func (Nop) Poisoned(string)                {}
func (Nop) Panicked()                      {}

// Recorder is Metrics that keep what they were told (tests; internal/metrics.Trigger is the one that is served).
type Recorder struct {
	mu sync.Mutex
	C  map[string]int
	G  map[string]float64
}

func (r *Recorder) inc(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.C == nil {
		r.C = map[string]int{}
	}
	r.C[k]++
}

func (r *Recorder) set(k string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.G == nil {
		r.G = map[string]float64{}
	}
	r.G[k] = v
}

// Count is a counter's value.
func (r *Recorder) Count(k string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.C[k]
}

// Gauge is a gauge's value.
func (r *Recorder) Gauge(k string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.G[k]
}

func (r *Recorder) EventReceived(via string) { r.inc("events_received/" + via) }
func (r *Recorder) StreamConnected(up bool) {
	v := 0.0
	if up {
		v = 1
	}
	r.set("stream_connected", v)
}
func (r *Recorder) LastEvent(t time.Time)           { r.set("last_event", float64(t.Unix())) }
func (r *Recorder) CursorTime(t time.Time)          { r.set("cursor_time", float64(t.Unix())) }
func (r *Recorder) Resume(result string)            { r.inc("resume/" + result) }
func (r *Recorder) Outcome(agent, via, res string)  { r.inc("outcome/" + agent + "/" + via + "/" + res) }
func (r *Recorder) Missed(agent, result string)     { r.inc("missed/" + agent + "/" + result) }
func (r *Recorder) Sweep(result string)             { r.inc("sweep/" + result) }
func (r *Recorder) SweepOK(t time.Time)             { r.set("sweep_ok", float64(t.Unix())) }
func (r *Recorder) SlotLaunch(agent, result string) { r.inc("slot_launch/" + agent + "/" + result) }
func (r *Recorder) SlotMissed(agent string)         { r.inc("slot_missed/" + agent) }
func (r *Recorder) SlotLag(agent string, s float64) { r.set("slot_lag/"+agent, s) }
func (r *Recorder) Dropped(reason string)           { r.inc("dropped/" + reason) }
func (r *Recorder) Stuck(agent string, n int)       { r.set("stuck/"+agent, float64(n)) }
func (r *Recorder) Poisoned(agent string)           { r.inc("poisoned/" + agent) }
func (r *Recorder) Panicked()                       { r.inc("panicked") }
