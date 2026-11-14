package dispatch

// The human inbox (engine/inbox.py and the inbox parts of engine/dispatch.py): an agent's question to a person, held until
// someone allowed answers it. Non-blocking by design: human.request records the question and returns its id at once; the
// agent reads the answer with human.request.get whenever it chooses.
//
// Who may answer: the session's owner (its principal) and the members of the agent definition's `owner` group, resolved by a
// Directory (without one, only the owner). A person answers directly (their own sign-in) or through a delegate (Tower's
// backend) that names them; the evidence records who answered and through what. A request expires with its session.
//
// Never the same principal on both sides: a run (a workload) is not a person, and a delegate's on-behalf-of is honoured only
// for a delegate the deployment lists (Gateway.Person), so a run cannot answer its own request.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// The inbox's limits (engine/inbox.py).
const (
	MaxQuestion = 2000
	MaxOptions  = 10
	MaxOption   = 200
	MaxAnswer   = 4000
	MaxPending  = 5 // per session: a run cannot flood its people
	// InboxRetention is how long a request is kept after it expired. Python keeps them for ever; Go prunes (the sweep), so the
	// table is bounded by what the sessions' budgets and TTLs allow in this window.
	InboxRetention = 30 * 24 * time.Hour
)

var requestID = regexp.MustCompile(`^h-[0-9a-f]{12}$`)

// InboxError is a refusal the agent or the person can act on: a rule, a message, a hint and the HTTP status for people.
type InboxError struct {
	Rule, Message, Hint string
	Status              int
}

func (e *InboxError) Error() string { return e.Message }

// HumanRequest is one question (inbox.HumanRequest).
type HumanRequest struct {
	ID          string
	TaskID      string
	Session     string
	Agent       string
	Owner       string  // the session's principal
	Group       *string // the definition's owner group, if any
	Question    string
	Options     []string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Status      string // pending | answered (expired is computed)
	Answer      *string
	AnsweredBy  *string
	AnsweredVia *string // the delegate a person answered through, or nil
	AnsweredAt  *time.Time
	Extra       map[string]any
}

// State is the request's state at now: pending becomes expired at its deadline.
func (r *HumanRequest) State(now time.Time) string {
	if r.Status == "pending" && !now.Before(r.ExpiresAt) {
		return "expired"
	}
	return r.Status
}

func optStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func optTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return PyIso(*p)
}

// View is what a person (and human.request.get) is shown (HumanRequest.view): every field but extra, times as ISO strings.
func (r *HumanRequest) View(now time.Time) map[string]any {
	opts := r.Options
	if opts == nil {
		opts = []string{}
	}
	return map[string]any{"id": r.ID, "task_id": r.TaskID, "session": r.Session, "agent": r.Agent, "owner": r.Owner, "group": optStr(r.Group),
		"question": r.Question, "options": opts, "created_at": PyIso(r.CreatedAt), "expires_at": PyIso(r.ExpiresAt), "status": r.State(now),
		"answer": optStr(r.Answer), "answered_by": optStr(r.AnsweredBy), "answered_via": optStr(r.AnsweredVia), "answered_at": optTime(r.AnsweredAt)}
}

// Record is the row's JSON document (dataclasses.asdict of the request, times as ISO strings): the Python gateway reads it back.
func (r *HumanRequest) Record() map[string]any {
	m := r.View(r.CreatedAt)
	m["status"] = r.Status
	extra := r.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	m["extra"] = extra
	return m
}

// PyIso is Python's datetime.isoformat for an aware UTC time: no fraction at a whole second, else six digits.
func PyIso(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// ParsePyIso reads what PyIso (or Python) wrote.
func ParsePyIso(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("not an ISO 8601 time: %q", s)
}

// HumanRequestFromRecord is the inverse of Record, for a row either gateway wrote.
func HumanRequestFromRecord(m map[string]any) (*HumanRequest, error) {
	str := func(k string) string { s, _ := m[k].(string); return s }
	optS := func(k string) *string {
		if s, ok := m[k].(string); ok {
			return &s
		}
		return nil
	}
	tm := func(k string) (*time.Time, error) {
		s, ok := m[k].(string)
		if !ok || s == "" {
			return nil, nil
		}
		t, err := ParsePyIso(s)
		return &t, err
	}
	created, err := tm("created_at")
	if err != nil || created == nil {
		return nil, fmt.Errorf("a stored request has no created_at")
	}
	expires, err := tm("expires_at")
	if err != nil || expires == nil {
		return nil, fmt.Errorf("a stored request has no expires_at")
	}
	answered, err := tm("answered_at")
	if err != nil {
		return nil, err
	}
	r := &HumanRequest{ID: str("id"), TaskID: str("task_id"), Session: str("session"), Agent: str("agent"), Owner: str("owner"), Group: optS("group"),
		Question: str("question"), CreatedAt: *created, ExpiresAt: *expires, Status: str("status"), Answer: optS("answer"),
		AnsweredBy: optS("answered_by"), AnsweredVia: optS("answered_via"), AnsweredAt: answered}
	if opts, ok := m["options"].([]any); ok {
		for _, o := range opts {
			if s, ok := o.(string); ok {
				r.Options = append(r.Options, s)
			}
		}
	}
	if e, ok := m["extra"].(map[string]any); ok {
		r.Extra = e
	}
	return r, nil
}

// InboxStore keeps the requests (engine/inbox.InboxStore). Get returns nil when there is none. A returned request is the
// caller's own copy: changing it changes nothing until Save.
type InboxStore interface {
	Add(ctx context.Context, r *HumanRequest) error
	Get(ctx context.Context, id string) (*HumanRequest, error)
	Save(ctx context.Context, r *HumanRequest) error
	Pending(ctx context.Context, now time.Time) ([]*HumanRequest, error)
	CountPending(ctx context.Context, sessionID string, now time.Time) (int, error)
	// Prune deletes the requests that expired before the cutoff and returns how many.
	Prune(ctx context.Context, before time.Time) (int, error)
}

// Directory is the groups a person is in (Backstage group refs), for the owner-group rule. An implementation that cannot tell
// answers an empty set (the rule fails closed: no group, no access). The deployment's is Tower's catalog (a later slice carries
// the Backstage client; until then the Go gateway has none, and only a session's owner and the platform admins act).
type Directory interface {
	Groups(ctx context.Context, principal string) map[string]bool
}

// MemoryInbox is an InboxStore in memory (tests).
type MemoryInbox struct {
	mu   sync.Mutex
	rows map[string]*HumanRequest
}

func cloneRequest(r *HumanRequest) *HumanRequest {
	c := *r
	c.Options = append([]string(nil), r.Options...)
	return &c
}

// Add stores a request.
func (m *MemoryInbox) Add(_ context.Context, r *HumanRequest) error {
	return m.Save(context.Background(), r)
}

// Save stores a request, replacing one of the same id.
func (m *MemoryInbox) Save(_ context.Context, r *HumanRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*HumanRequest{}
	}
	m.rows[r.ID] = cloneRequest(r)
	return nil
}

// Get is a copy of the request, or nil.
func (m *MemoryInbox) Get(_ context.Context, id string) (*HumanRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil {
		return cloneRequest(r), nil
	}
	return nil, nil
}

// Pending lists the pending, unexpired requests oldest first.
func (m *MemoryInbox) Pending(_ context.Context, now time.Time) ([]*HumanRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*HumanRequest
	for _, r := range m.rows {
		if r.State(now) == "pending" {
			out = append(out, cloneRequest(r))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// CountPending is how many pending, unexpired requests a session has.
func (m *MemoryInbox) CountPending(_ context.Context, sessionID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.Session == sessionID && r.State(now) == "pending" {
			n++
		}
	}
	return n, nil
}

// Prune deletes the requests that expired before the cutoff.
func (m *MemoryInbox) Prune(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if r.ExpiresAt.Before(before) {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

// CheckQuestion validates a question and its options (inbox.check_question).
func CheckQuestion(question, options any) (string, []string, error) {
	q, ok := question.(string)
	if !ok || strings.TrimSpace(q) == "" || utf8.RuneCountInString(q) > MaxQuestion {
		return "", nil, &InboxError{"HR-QUESTION", fmt.Sprintf("a question is 1 to %d characters", MaxQuestion),
			"Ask one clear question; put detail in an artifact and name it.", 400}
	}
	bad := &InboxError{"HR-OPTIONS", fmt.Sprintf("options are up to %d distinct strings of up to %d characters", MaxOptions, MaxOption),
		"Offer a short list of choices, or none for a free-text answer.", 400}
	var opts []string
	if options != nil {
		list, ok := options.([]any)
		if !ok || len(list) > MaxOptions {
			return "", nil, bad
		}
		seen := map[string]bool{}
		for _, o := range list {
			s, ok := o.(string)
			if !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > MaxOption || seen[s] {
				return "", nil, bad
			}
			seen[s] = true
			opts = append(opts, strings.TrimSpace(s))
		}
	}
	return strings.TrimSpace(q), opts, nil
}

// MayAnswer is who may answer a request: its owner, and members of its owner group (inbox.may_answer).
func MayAnswer(r *HumanRequest, person string, groups map[string]bool) bool {
	return person == r.Owner || (r.Group != nil && groups[*r.Group])
}

// CheckAnswer validates an answer to a request at now (inbox.check_answer) and returns it trimmed.
func CheckAnswer(r *HumanRequest, answer any, now time.Time) (string, error) {
	switch state := r.State(now); {
	case state == "expired":
		return "", &InboxError{"HR-EXPIRED", "this request expired with its session", "The agent is gone; nothing to answer.", 410}
	case state != "pending":
		by := "<nil>"
		if r.AnsweredBy != nil {
			by = *r.AnsweredBy
		}
		return "", &InboxError{"HR-ANSWERED", "already answered by " + by, "Each request takes one answer.", 409}
	}
	a, ok := answer.(string)
	if !ok || strings.TrimSpace(a) == "" || utf8.RuneCountInString(a) > MaxAnswer {
		return "", &InboxError{"HR-ANSWER", fmt.Sprintf("an answer is 1 to %d characters", MaxAnswer), "Write the answer.", 400}
	}
	a = strings.TrimSpace(a)
	if len(r.Options) > 0 {
		found := false
		for _, o := range r.Options {
			found = found || o == a
		}
		if !found {
			return "", &InboxError{"HR-ANSWER", "the answer must be one of the request's options", "Pick one of: " + strings.Join(r.Options, ", "), 400}
		}
	}
	return a, nil
}

func newRequestID() string { return "h-" + randHex(6) }

// SetInbox gives the gateway a human inbox: the store, the directory (may be nil), and the handlers of human.request and
// human.request.get, which are the gateway's own because a request needs its session (Python: Gateway.__init__ with inbox).
// delegates are the principals (globs) that may answer for a person they name, e.g. Tower's backend.
func (g *Gateway) SetInbox(store InboxStore, dir Directory, delegates []string) {
	g.Inbox, g.Directory, g.Delegates = store, dir, delegates
	if g.Handlers == nil {
		g.Handlers = map[string]Handler{}
	}
	if store != nil {
		g.Handlers["human.request"] = g.humanRequest
		g.Handlers["human.request.get"] = g.humanGet
	}
}

func (g *Gateway) humanRequest(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
	question, options, err := CheckQuestion(a["question"], a["options"])
	if err != nil {
		var ie *InboxError
		errors.As(err, &ie)
		return nil, &core.Denied{Rule: ie.Rule, Message: ie.Message, Hint: ie.Hint}
	}
	now := g.Now()
	s.Lock()
	id, task, agent, principal, expires := s.ID, s.TaskID, s.Agent, s.Principal, s.ExpiresAt
	s.Unlock()
	n, err := g.Inbox.CountPending(ctx, id, now)
	if err != nil {
		return nil, err
	}
	if n >= MaxPending {
		return nil, &core.Denied{Rule: "HR-FLOOD", Message: fmt.Sprintf("this session already has %d unanswered requests", MaxPending),
			Hint: "Read their answers with human.request.get before asking more."}
	}
	var group *string
	if d := g.Def(agent); d != nil && d.Owner != nil {
		group = d.Owner
	}
	r := &HumanRequest{ID: newRequestID(), TaskID: task, Session: id, Agent: agent, Owner: principal, Group: group, Question: question,
		Options: options, CreatedAt: now, ExpiresAt: expires, Status: "pending"}
	if err := g.Inbox.Add(ctx, r); err != nil {
		return nil, err
	}
	return map[string]any{"request": r.ID, "status": "pending", "expires_at": PyIso(r.ExpiresAt),
		"hint": "Not answered yet. Read the answer with human.request.get whenever you choose; this call does not wait."}, nil
}

func (g *Gateway) humanGet(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
	rid, _ := a["request"].(string)
	var r *HumanRequest
	if requestID.MatchString(rid) {
		var err error
		if r, err = g.Inbox.Get(ctx, rid); err != nil {
			return nil, err
		}
	}
	s.Lock()
	task := s.TaskID
	s.Unlock()
	if r == nil || r.TaskID != task {
		return nil, &core.Denied{Rule: "HR-UNKNOWN", Message: fmt.Sprintf("no request %s in this task", pyRepr(a["request"])), Hint: "Pass an id human.request returned."}
	}
	return r.View(g.Now()), nil
}

// pyRepr is Python's repr of the values a request id may be in an error message (a string, else its JSON form).
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
	}
	if v == nil {
		return "None"
	}
	return fmt.Sprint(v)
}

// groupsOf is the groups of a person, or none without a directory.
func (g *Gateway) groupsOf(ctx context.Context, person string) map[string]bool {
	if g.Directory == nil {
		return nil
	}
	return g.Directory.Groups(ctx, person)
}

// HumanRequests are the pending requests this person may answer, oldest first (Gateway.human_requests). Without an inbox: none.
func (g *Gateway) HumanRequests(ctx context.Context, token, onBehalfOf string) ([]map[string]any, error) {
	if g.Inbox == nil {
		return []map[string]any{}, nil
	}
	person, _, err := g.Person(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	groups := g.groupsOf(ctx, person)
	now := g.Now()
	pending, err := g.Inbox.Pending(ctx, now)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range pending {
		if MayAnswer(r, person, groups) {
			out = append(out, r.View(now))
		}
	}
	return out, nil
}

// answerLocks serialize the answering of one request: two people answering at once give one answer and one refusal.
type answerLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
	n  map[string]int
}

func (a *answerLocks) lock(id string) func() {
	a.mu.Lock()
	if a.m == nil {
		a.m, a.n = map[string]*sync.Mutex{}, map[string]int{}
	}
	l := a.m[id]
	if l == nil {
		l = &sync.Mutex{}
		a.m[id] = l
	}
	a.n[id]++
	a.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		a.mu.Lock()
		if a.n[id]--; a.n[id] == 0 {
			delete(a.m, id)
			delete(a.n, id)
		}
		a.mu.Unlock()
	}
}

// HumanAnswer answers one request as the person (or a delegate naming them), and records it as human.answer (Gateway.human_answer).
//
// The answer stands only once its record does: an unreachable core refuses it and the person retries (503). The request is
// read, checked, recorded and saved under a lock of its own and read again under it, so two answers racing for one request give
// one answer (the second is HR-ANSWERED) and one record. A request that is not the person's to answer is "no such request for
// you", the same answer as one that does not exist.
func (g *Gateway) HumanAnswer(ctx context.Context, token, id string, answer any, onBehalfOf string) (map[string]any, error) {
	if g.Inbox == nil {
		return nil, &InboxError{"HR-OFF", "this deployment has no human inbox", "", 404}
	}
	person, via, err := g.Person(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	unknown := &InboxError{"HR-UNKNOWN", "no such request for you", "", 404}
	if !requestID.MatchString(id) {
		return nil, unknown
	}
	defer g.answers.lock(id)()
	r, err := g.Inbox.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil || !MayAnswer(r, person, g.groupsOf(ctx, person)) {
		return nil, unknown
	}
	now := g.Now()
	text, err := CheckAnswer(r, answer, now)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{"task_id": r.TaskID, "session": r.Session, "principal": person, "agent": r.Agent, "decision": "allow",
		"target": r.ID, "args_sha256": core.ArgsDigest(map[string]any{"answer": text}), "result": "answered" + viaSuffix(via)}
	rctx, cancel := g.bg(ctx)
	defer cancel()
	seq, rerr := g.Core.Record(rctx, "human.answer", fields, true)
	if rerr != nil {
		var un *core.Unreachable
		if errors.As(rerr, &un) {
			return nil, &InboxError{un.Rule(), "Clearance is unreachable: the answer was not recorded, so it is not given", "Answer again in a minute.", 503}
		}
		return nil, rerr
	}
	r.Status, r.Answer, r.AnsweredBy, r.AnsweredAt = "answered", &text, &person, &now
	if via != "" {
		r.AnsweredVia = &via
	} else {
		r.AnsweredVia = nil
	}
	if err := g.Inbox.Save(ctx, r); err != nil {
		return nil, err
	}
	out := r.View(now)
	out["audit_seq"] = nilSeq(seq)
	return out, nil
}

func viaSuffix(via string) string {
	if via == "" {
		return ""
	}
	return " via " + via
}

func nilSeq(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
