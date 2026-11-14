package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/session"
)

// A run's result (autopilot.engine.results): what an in-cluster run answered when it finished. The runtime hands it to the
// gateway before it exits (the MCP tool run_finish), the answer is kept RETENTION by session for the read API, and the
// session keeps only the outcome (status, turns, counts, the answer's size and sha256). The evidence record names the
// status, never the text: the chain records what was decided, not what an agent said.
const (
	Retention    = 30 * 24 * time.Hour
	MaxTextBytes = 16 << 10 // the answer, cut at this many bytes (truncated: true)
	MaxToolCalls = 200      // the per-call summary kept with the answer
	MaxReason    = 500
	MaxPlanCalls = 3 // a run that needed more than this to get an AppSpec past airframe.plan was guessing
)

// Statuses are the ways a run may say it ended.
var Statuses = []string{"done", "budget", "expired", "error"}

// ResultStore keeps finished runs' answers by session until they expire (results.ResultStore).
type ResultStore interface {
	// Put keeps a record; a session that already has one keeps the first (a finish that is retried writes once).
	Put(ctx context.Context, record map[string]any, now time.Time) error
	Get(ctx context.Context, sessionID string, now time.Time) (map[string]any, error)
	// Delete removes a session's record (a finish that lost to the deadline takes back the answer it kept).
	Delete(ctx context.Context, sessionID string) error
}

// MemoryResults is a ResultStore in memory (tests).
type MemoryResults struct {
	mu   sync.Mutex
	Rows map[string]map[string]any
}

// Put keeps the record, dropping what has expired.
func (m *MemoryResults) Put(_ context.Context, rec map[string]any, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Rows == nil {
		m.Rows = map[string]map[string]any{}
	}
	for k, r := range m.Rows {
		if exp, err := time.Parse(time.RFC3339, fmt.Sprint(r["expires_at"])); err == nil && !exp.After(now) {
			delete(m.Rows, k)
		}
	}
	id, _ := rec["session"].(string)
	if _, kept := m.Rows[id]; !kept {
		m.Rows[id] = rec
	}
	return nil
}

// Delete removes a record.
func (m *MemoryResults) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Rows, id)
	return nil
}

// Get is the record, or nil when there is none or it has expired.
func (m *MemoryResults) Get(_ context.Context, id string, now time.Time) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.Rows[id]
	if r == nil {
		return nil, nil
	}
	if exp, err := time.Parse(time.RFC3339, fmt.Sprint(r["expires_at"])); err != nil || !exp.After(now) {
		return nil, nil
	}
	return r, nil
}

// Observe is what one call that reached its handler adds to the session's signals: airframe.plan calls are counted
// (answered or refused), and each airframe.apply that answered ok is counted with whether it carried a PR (opened or
// resumed) and the state it ended in (results.observe). The handlers that make these calls are a later slice's; the
// counting is here so the signals a session the Python gateway wrote keeps counting.
func Observe(signals map[string]any, tool string, data any) {
	switch tool {
	case "airframe.plan":
		signals["plan_calls"] = sigInt(signals["plan_calls"]) + 1
	case "airframe.apply":
		m, ok := data.(map[string]any)
		if !ok {
			return
		}
		signals["apply_ok"] = sigInt(signals["apply_ok"]) + 1
		if truthy(m["pr"]) {
			signals["apply_pr"] = sigInt(signals["apply_pr"]) + 1
		}
		state := ""
		if truthy(m["state"]) {
			state = pyStr(m["state"])
		}
		signals["apply_state"] = firstChars(state, 40)
	}
}

// Warnings flags a finished run: provisioning that guessed (airframe.plan called more than MaxPlanCalls times), or whose
// airframe.apply answered ok but never opened a PR (and did not end at agent-steps-merged, where nothing is left to open).
func Warnings(signals map[string]any) []string {
	out := []string{}
	if plans := sigInt(signals["plan_calls"]); plans > MaxPlanCalls {
		out = append(out, fmt.Sprintf("airframe.plan was called %d times (more than %d): the run was guessing the AppSpec", plans, MaxPlanCalls))
	}
	ok, prs := sigInt(signals["apply_ok"]), sigInt(signals["apply_pr"])
	state, _ := signals["apply_state"].(string)
	if ok > 0 && prs == 0 && state != "agent-steps-merged" {
		last := state
		if last == "" {
			last = "none"
		}
		out = append(out, fmt.Sprintf("airframe.apply answered ok %d time(s) but opened no PR (last state: %s)", ok, last))
	}
	return out
}

// ClipText is the answer cut to MaxTextBytes on a character boundary, and whether it was cut.
func ClipText(text string) (string, bool) {
	if len(text) <= MaxTextBytes {
		return text, false
	}
	b := []byte(text)[:MaxTextBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b), true
}

// ToolCall is one entry of the per-call summary a result keeps.
type ToolCall struct {
	Tool string  `json:"tool"`
	OK   bool    `json:"ok"`
	Rule *string `json:"rule"`
}

// ToolCallSummary reduces the runtime's per-call list to what it may claim: a tool name (100 characters), ok, and the
// refusing rule (40 characters). At most MaxToolCalls.
func ToolCallSummary(calls any) []ToolCall {
	out := []ToolCall{}
	list, _ := calls.([]any)
	for _, c := range list {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if tool, ok := m["tool"].(string); ok {
			tc := ToolCall{Tool: firstChars(tool, 100), OK: truthy(m["ok"])}
			if rule, ok := m["rule"].(string); ok {
				r := firstChars(rule, 40)
				tc.Rule = &r
			}
			out = append(out, tc)
		}
		if len(out) >= MaxToolCalls {
			break
		}
	}
	return out
}

func firstChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// sigInt is int(x or 0) for a value a signals map may hold after a round trip through JSON.
func sigInt(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// truthy is Python's truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr is str(x) for the scalar a signal's state may be.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// observe adds one call's signals to the session.
func (g *Gateway) observe(s *session.Session, tool string, data any) {
	s.Lock()
	defer s.Unlock()
	if s.Signals == nil {
		s.Signals = map[string]any{}
	}
	Observe(s.Signals, tool, data)
}
