// Package session holds a run's authority: identity, budgets, expiry, breaker and the run tree. It is the Go side of
// autopilot.engine.session: Record is the same JSON the Python gateway keeps in gateway_sessions.record, so either
// implementation can read the other's rows.
package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/jfillman/clearance/client"
)

// DenialTrip is how many denied calls trip a session's breaker.
const DenialTrip = 5

// BudgetFields are the five budget counters, in the order the Python gateway lists them.
var BudgetFields = []string{"tool_calls", "github_calls", "open_prs", "model_tokens", "children"}

// UsageFields are a session's running model cost (autopilot.engine.session.USAGE_FIELDS).
var UsageFields = []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
	"charged_tokens", "model_calls"}

// Budget reads one counter by name.
func Budget(b *client.Budgets, f string) int64 {
	switch f {
	case "tool_calls":
		return b.ToolCalls
	case "github_calls":
		return b.GithubCalls
	case "open_prs":
		return b.OpenPRs
	case "model_tokens":
		return b.ModelTokens
	case "children":
		return b.Children
	}
	return 0
}

// AddBudget adds n to one counter by name.
func AddBudget(b *client.Budgets, f string, n int64) {
	switch f {
	case "tool_calls":
		b.ToolCalls += n
	case "github_calls":
		b.GithubCalls += n
	case "open_prs":
		b.OpenPRs += n
	case "model_tokens":
		b.ModelTokens += n
	case "children":
		b.Children += n
	}
}

// LimitOf reads one budget limit by name.
func LimitOf(l *client.Limits, f string) int64 {
	switch f {
	case "tool_calls":
		return l.ToolCalls
	case "github_calls":
		return l.GithubCalls
	case "open_prs":
		return l.OpenPRs
	case "model_tokens":
		return l.ModelTokens
	case "children":
		return l.Children
	}
	return 0
}

// DeleteFail is a closed session's run whose delete keeps failing: how many attempts, when it first failed, when the gateway
// last wrote a record of it and when the sweep may try again. It is kept with the session so a restart continues the backoff
// and the hourly record limit. The Python gateway does not know it: it reads a session row by the keys it has, so the extra
// key is ignored, and its next save of that session leaves it out (the backoff then starts again).
type DeleteFail struct {
	Fails        int       `json:"fails"`
	FirstFailed  time.Time `json:"first_failed"`
	LastRecorded time.Time `json:"last_recorded"`
	Next         time.Time `json:"next"`
}

// Session is one agent run's authority. Call Lock around any read or write of its fields; Serial serializes whole
// operations on one session (a call's decision, spend and outcome), so a session never overspends by racing itself,
// while different sessions run in parallel.
type Session struct {
	mu       sync.Mutex
	serial   sync.Mutex
	runEpoch uint64

	ID         string
	Principal  string
	Agent      string
	Limits     client.Limits
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ParentID   *string
	Depth      int
	TaskID     string
	Used       client.Budgets
	Reserved   client.Budgets
	Children   []string
	Tripped    bool
	TripReason *string
	Denials    int
	Closed     bool
	Run        *string
	Input      map[string]any
	Outcome    map[string]any
	Usage      map[string]int64
	// Signals is what the gateway saw this session's calls do, for a run's warnings (Python: results.observe and
	// results.warnings, which the run slice ports). This slice keeps it whole across a load and a save, so a session the
	// Python gateway wrote keeps what it counted.
	Signals map[string]any
	// DeleteFail is the state of a delete that keeps failing, or nil.
	DeleteFail *DeleteFail
}

// Lock guards the session's fields.
func (s *Session) Lock() { s.mu.Lock() }

// Unlock releases Lock.
func (s *Session) Unlock() { s.mu.Unlock() }

// Serial takes the per-session operation lock; the returned func releases it.
func (s *Session) Serial() func() { s.serial.Lock(); return s.serial.Unlock }

// TrySerial takes the per-session operation lock if it is free, and says whether it did. The sweep uses it so it never waits
// behind (or stalls) a call on the session: a session that is busy is looked at again on the next sweep.
func (s *Session) TrySerial() (release func(), ok bool) {
	if !s.serial.TryLock() {
		return nil, false
	}
	return s.serial.Unlock, true
}

// RunEpoch counts the times the session's run was made, replaced or settled (BumpRunEpoch). The sweep reads a run's state
// without holding the session, then acts only if the epoch is still the one it read under.
func (s *Session) RunEpoch() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.runEpoch }

// RunEpochLocked is RunEpoch for a caller that holds the session's lock.
func (s *Session) RunEpochLocked() uint64 { return s.runEpoch }

// BumpRunEpoch records that the run changed (set, created, abandoned).
func (s *Session) BumpRunEpoch() { s.mu.Lock(); s.runEpoch++; s.mu.Unlock() }

// Remaining is what is left of a budget (limit less used less reserved). Caller holds the lock.
func (s *Session) Remaining(f string) int64 {
	return LimitOf(&s.Limits, f) - Budget(&s.Used, f) - Budget(&s.Reserved, f)
}

// CanSpend reports whether n of a budget is left. Caller holds the lock.
func (s *Session) CanSpend(f string, n int64) bool { return n <= s.Remaining(f) }

// Expired is closed or past its deadline. Caller holds the lock.
func (s *Session) Expired(now time.Time) bool { return s.Closed || !now.Before(s.ExpiresAt) }

// Trip opens the breaker (once). Caller holds the lock.
func (s *Session) Trip(reason string) {
	if !s.Tripped {
		s.Tripped = true
		s.TripReason = &reason
	}
}

// RecordDenial counts a denied call and trips the breaker at DenialTrip. Caller holds the lock.
func (s *Session) RecordDenial() {
	s.Denials++
	if s.Denials >= DenialTrip {
		s.Trip(itoa(s.Denials) + " denied calls")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// AddUsage adds one model call's usage (modelproxy.UsageRecord) to the running cost. Caller holds the lock.
func (s *Session) AddUsage(call map[string]int64) {
	if s.Usage == nil {
		s.Usage = NoUsage()
	}
	for _, k := range UsageFields {
		if k != "model_calls" {
			s.Usage[k] += call[k]
		}
	}
	s.Usage["model_calls"]++
}

// NoUsage is a zeroed running cost.
func NoUsage() map[string]int64 {
	m := make(map[string]int64, len(UsageFields))
	for _, k := range UsageFields {
		m[k] = 0
	}
	return m
}

// State is the session as the core's evaluation reads it (context.session). Caller holds the lock.
func (s *Session) State() *client.SessionState {
	l := s.Limits
	l.Tools, l.Models = nonNil(l.Tools), nonNil(l.Models)
	return &client.SessionState{ID: s.ID, Principal: s.Principal, Agent: s.Agent, TaskID: s.TaskID, ParentID: s.ParentID,
		Depth: s.Depth, Limits: l, Used: s.Used, Reserved: s.Reserved, ExpiresAt: s.ExpiresAt, Closed: s.Closed,
		Tripped: s.Tripped, Denials: s.Denials}
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// Record is a session as gateway_sessions keeps it (Session.to_record in the Python gateway).
type Record struct {
	ID         string           `json:"id"`
	Principal  string           `json:"principal"`
	Agent      string           `json:"agent"`
	Limits     client.Limits    `json:"limits"`
	CreatedAt  string           `json:"created_at"`
	ExpiresAt  string           `json:"expires_at"`
	ParentID   *string          `json:"parent_id"`
	Depth      int              `json:"depth"`
	TaskID     string           `json:"task_id"`
	Used       map[string]int64 `json:"used"`
	Reserved   map[string]int64 `json:"reserved"`
	Children   []string         `json:"children"`
	Tripped    bool             `json:"tripped"`
	TripReason *string          `json:"trip_reason"`
	Denials    int              `json:"denials"`
	Closed     bool             `json:"closed"`
	Run        *string          `json:"run"`
	Input      map[string]any   `json:"input"`
	Outcome    map[string]any   `json:"outcome"`
	Usage      map[string]int64 `json:"usage"`
	Signals    map[string]any   `json:"signals"`
	// DeleteFail is the Go gateway's own key (see DeleteFail); absent for a row the Python gateway wrote.
	DeleteFail *DeleteFail `json:"delete_failure,omitempty"`
}

// IsoFormat is Python's datetime.isoformat for an aware UTC time.
const IsoFormat = "2006-01-02T15:04:05.999999-07:00"

func budgetsMap(b *client.Budgets) map[string]int64 {
	m := map[string]int64{}
	for _, f := range BudgetFields {
		m[f] = Budget(b, f)
	}
	return m
}

func budgetsFrom(m map[string]int64) client.Budgets {
	var b client.Budgets
	for _, f := range BudgetFields {
		AddBudget(&b, f, m[f])
	}
	return b
}

// ToRecord is the durable form. Caller holds the lock.
func (s *Session) ToRecord() Record {
	l := s.Limits
	l.Tools, l.Models = nonNil(l.Tools), nonNil(l.Models)
	usage := NoUsage()
	for k, v := range s.Usage {
		usage[k] = v
	}
	signals := map[string]any{}
	for k, v := range s.Signals {
		signals[k] = v
	}
	return Record{ID: s.ID, Principal: s.Principal, Agent: s.Agent, Limits: l,
		CreatedAt: s.CreatedAt.UTC().Format(IsoFormat), ExpiresAt: s.ExpiresAt.UTC().Format(IsoFormat),
		ParentID: s.ParentID, Depth: s.Depth, TaskID: s.TaskID, Used: budgetsMap(&s.Used), Reserved: budgetsMap(&s.Reserved),
		Children: nonNil(s.Children), Tripped: s.Tripped, TripReason: s.TripReason, Denials: s.Denials, Closed: s.Closed,
		Run: s.Run, Input: s.Input, Outcome: s.Outcome, Usage: usage, Signals: signals, DeleteFail: copyDF(s.DeleteFail)}
}

// FromRecord is the inverse of ToRecord, for a row either gateway wrote.
func FromRecord(r Record) (*Session, error) {
	created, err := parseTime(r.CreatedAt)
	if err != nil {
		return nil, err
	}
	expires, err := parseTime(r.ExpiresAt)
	if err != nil {
		return nil, err
	}
	usage := NoUsage()
	for k, v := range r.Usage {
		usage[k] = v
	}
	signals := map[string]any{}
	for k, v := range r.Signals {
		signals[k] = v
	}
	return &Session{ID: r.ID, Principal: r.Principal, Agent: r.Agent, Limits: r.Limits, CreatedAt: created, ExpiresAt: expires,
		ParentID: r.ParentID, Depth: r.Depth, TaskID: r.TaskID, Used: budgetsFrom(r.Used), Reserved: budgetsFrom(r.Reserved),
		Children: r.Children, Tripped: r.Tripped, TripReason: r.TripReason, Denials: r.Denials, Closed: r.Closed, Run: r.Run,
		Input: r.Input, Outcome: r.Outcome, Usage: usage, Signals: signals, DeleteFail: copyDF(r.DeleteFail)}, nil
}

func parseTime(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999-07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, &time.ParseError{Value: v, Message: ": not an ISO 8601 time"}
}

func copyDF(d *DeleteFail) *DeleteFail {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}
