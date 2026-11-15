package metrics

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type script struct {
	Calls      [][]json.RawMessage `json:"calls"`
	Exposition string              `json:"exposition"`
}

// Every script Python's TriggerMetrics ran renders, through the Go series of the same name, to the exact exposition Python rendered:
// names, types, help, label order, sorting, escaping, the 128-character cut and the %g number format.
func TestTheExpositionEqualsPythons(t *testing.T) {
	raw, err := os.ReadFile("testdata/metrics.json")
	if err != nil {
		t.Fatal(err)
	}
	var scripts map[string]script
	if err := json.Unmarshal(raw, &scripts); err != nil {
		t.Fatal(err)
	}
	if len(scripts) < 4 {
		t.Fatalf("%d scripts", len(scripts))
	}
	apply := map[string]string{"events_received": "autopilot_events_received_total", "stream_connected": "autopilot_events_stream_connected",
		"last_event": "autopilot_events_last_received_timestamp_seconds", "cursor_time": "autopilot_events_cursor_timestamp_seconds",
		"resumes": "autopilot_events_resume_total", "outcomes": "autopilot_trigger_outcomes_total", "missed": "autopilot_trigger_missed_total",
		"sweeps": "autopilot_reconcile_sweeps_total", "sweep_ok": "autopilot_reconcile_last_success_timestamp_seconds",
		"slot_launches": "autopilot_schedule_launches_total", "slot_missed": "autopilot_schedule_missed_total", "slot_lag": "autopilot_schedule_lag_seconds"}
	for name, sc := range scripts {
		t.Run(name, func(t *testing.T) {
			m := NewTrigger()
			for _, c := range sc.Calls {
				var what string
				var labels map[string]string
				var v float64
				_ = json.Unmarshal(c[0], &what)
				_ = json.Unmarshal(c[1], &labels)
				_ = json.Unmarshal(c[2], &v)
				s := m.Series(apply[what])
				if s == nil {
					t.Fatalf("no series for %s", what)
				}
				vals := make([]string, len(s.labels))
				for i, l := range s.labels {
					vals[i] = labels[l]
				}
				if s.kind == counterKind {
					s.Inc(v, vals...)
				} else {
					s.Set(v, vals...)
				}
			}
			got := m.Registry.Render()
			// Python's twelve are a prefix; what follows is Go's own, and only that.
			if !strings.HasPrefix(got, sc.Exposition) {
				t.Fatalf("the exposition differs from Python's:\n--- go\n%s\n--- python\n%s", got, sc.Exposition)
			}
			extra := strings.TrimPrefix(got, sc.Exposition)
			for _, line := range strings.Split(strings.TrimSpace(extra), "\n") {
				ok := false
				for _, n := range []string{"autopilot_events_dropped_total", "autopilot_trigger_stuck_attempts", "autopilot_trigger_poisoned_total", "autopilot_trigger_handler_panics_total"} {
					ok = ok || strings.Contains(line, n)
				}
				if !ok {
					t.Fatalf("an extra line that is not one of Go's own series: %q", line)
				}
			}
		})
	}
}

func TestPythonsNamesAreThePrefixOfTheRegistry(t *testing.T) {
	names := NewTrigger().Registry.Names()
	if len(names) != len(PythonNames)+4 {
		t.Fatalf("%d series", len(names))
	}
	for i, n := range PythonNames {
		if names[i] != n {
			t.Fatalf("series %d is %s, Python's is %s", i, names[i], n)
		}
	}
}

func TestOnlyGetMetricsIsServed(t *testing.T) {
	h := Handler(NewTrigger().Registry)
	for _, c := range []struct {
		method, path string
		status       int
	}{{"GET", "/metrics", 200}, {"GET", "/metrics?x=1", 200}, {"GET", "/", 404}, {"GET", "/metrics/", 404}, {"GET", "/v1/sessions", 404},
		{"POST", "/metrics", 405}, {"DELETE", "/metrics", 405}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.status {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.status)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "# TYPE autopilot_events_received_total counter") {
		t.Fatal("no exposition")
	}
}

// A label that grows without bound cannot grow the process: past MaxSeries new values are not recorded, and the series held keep counting.
func TestSeriesAreBounded(t *testing.T) {
	m := NewTrigger()
	for i := 0; i < MaxSeries+500; i++ {
		m.Outcome("agent-"+strings.Repeat("x", 3)+string(rune('a'+i%26))+itoa(i), "stream", "launched")
	}
	if n := strings.Count(m.Registry.Render(), "autopilot_trigger_outcomes_total{"); n != MaxSeries {
		t.Fatalf("%d series, want %d", n, MaxSeries)
	}
	m.Outcome("agent-xxxa0", "stream", "launched")
	if v := m.outcomes.Value("agent-xxxa0", "stream", "launched"); v != 2 {
		t.Fatalf("a held series stopped counting: %v", v)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
