package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/clearance/client"
)

// Persistence is where a Store keeps its sessions across restarts (pgstate.Sessions is the real one).
type Persistence interface {
	Load(ctx context.Context) ([]Record, error)
	// Save writes the records durably before it returns; implementations may group concurrent saves.
	Save(ctx context.Context, records []Record) error
}

// Store is every session in memory, written through to a Persistence when there is one. Operations on different
// sessions run in parallel; Flush saves the sessions an operation touched, durably, before the operation answers.
type Store struct {
	mu      sync.RWMutex
	s       map[string]*Session
	spare   []string
	persist Persistence
	saved   map[string]string
	newID   func() string
}

// NewStore loads what the persistence holds.
func NewStore(ctx context.Context, p Persistence) (*Store, error) {
	st := &Store{s: map[string]*Session{}, saved: map[string]string{}, persist: p, newID: randomID}
	if p == nil {
		return st, nil
	}
	recs, err := p.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		s, err := FromRecord(r)
		if err != nil {
			return nil, err
		}
		st.s[s.ID] = s
		b, _ := json.Marshal(r)
		st.saved[s.ID] = string(b)
	}
	return st, nil
}

// SetIDs replaces the id generator (tests).
func (st *Store) SetIDs(f func() string) { st.newID = f }

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "s-" + hex.EncodeToString(b)
}

// Get returns a session or nil.
func (st *Store) Get(id string) *Session {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s[id]
}

// All lists every session.
func (st *Store) All() []*Session {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*Session, 0, len(st.s))
	for _, s := range st.s {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID }) // a sweep visits sessions in a stable order
	return out
}

// NewID is the id the next session gets: the gateway names it in the core's evaluation (context.new_session_id), and
// the core's grant carries it back. An id the core did not grant goes back with GiveBack and is handed out next.
func (st *Store) NewID() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := len(st.spare); n > 0 {
		id := st.spare[n-1]
		st.spare = st.spare[:n-1]
		return id
	}
	return st.newID()
}

// GiveBack returns an id the core did not grant.
func (st *Store) GiveBack(id string) {
	if id == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, exists := st.s[id]; exists {
		return
	}
	for _, x := range st.spare {
		if x == id {
			return
		}
	}
	st.spare = append(st.spare, id)
}

// Adopt creates the session the core granted (an open's or a spawn's context.grant). For a spawn the parent takes the
// spend the core named (reserve on its reserved budgets, children on its used) and lists the child.
func (st *Store) Adopt(g *client.Grant, now time.Time, reserve *client.Budgets, children int64) *Session {
	s := &Session{ID: g.Session, Principal: g.Principal, Agent: g.Agent, Limits: g.Limits, CreatedAt: now.UTC(),
		ExpiresAt: g.ExpiresAt.UTC(), ParentID: g.Parent, Depth: g.Depth, TaskID: g.TaskID, Usage: NoUsage()}
	st.mu.Lock()
	defer st.mu.Unlock()
	if g.Parent != nil {
		if p := st.s[*g.Parent]; p != nil {
			p.Lock()
			if reserve != nil {
				for _, f := range BudgetFields {
					AddBudget(&p.Reserved, f, Budget(reserve, f))
				}
			}
			p.Used.Children += children
			p.Children = append(p.Children, s.ID)
			p.Unlock()
		}
	}
	st.s[s.ID] = s
	return s
}

// GrantError is a spawn grant the gateway refused to adopt.
type GrantError struct{ Why string }

func (e *GrantError) Error() string { return e.Why }

// AdoptChild creates the child session the core granted a session.spawn, after checking the grant itself against the
// parent, and reserves its budget on the parent, in one step under the parent's lock. The core cuts the grant; this is
// the gateway's own check, so a grant that is wider than the parent has left, names another principal, task or agent,
// reuses a session id, goes deeper than MaxDepth, outlives the parent or reserves other than the child's limits is refused
// and nothing is created. wantID is the id the gateway named in the evaluation; def is the child definition's limits.
// Because the check and the reservation share the parent's lock, concurrent spawns cannot reserve more than the parent
// has, whatever else is charging it (the model proxy's usage does not take the session's operation lock).
//
// callerID is the session that made the spawn call: the grant's parent must BE that session, not merely another live session
// of the same principal and task. The child is returned with its operation lock (Serial) already held, and release gives it
// up: the sweep cannot act on a child while its run is being made.
func (st *Store) AdoptChild(g *client.Grant, sp client.Spend, callerID, wantID, wantAgent string, def client.Limits, now time.Time) (*Session, func(), error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if g.Parent == nil {
		return nil, nil, &GrantError{"the grant names no parent"}
	}
	if *g.Parent != callerID {
		return nil, nil, &GrantError{"the grant's parent is not the session that asked"}
	}
	p := st.s[*g.Parent]
	if p == nil {
		return nil, nil, &GrantError{"the grant's parent is not a session of this gateway"}
	}
	if g.Session == "" || g.Session != wantID {
		return nil, nil, &GrantError{"the grant's session id is not the one the gateway named"}
	}
	if st.s[g.Session] != nil {
		return nil, nil, &GrantError{"the grant's session id is already a session"}
	}
	if g.Agent != wantAgent {
		return nil, nil, &GrantError{"the grant is for another agent than the one asked for"}
	}
	p.Lock()
	defer p.Unlock()
	if g.Principal != p.Principal || g.TaskID != p.TaskID {
		return nil, nil, &GrantError{"the grant names another principal or task than the parent's"}
	}
	if g.Depth != p.Depth+1 || g.Depth > MaxDepth {
		return nil, nil, &GrantError{"the grant's depth is not the parent's plus one, within the run tree's depth limit"}
	}
	if p.Expired(now) || p.Tripped {
		return nil, nil, &GrantError{"the parent is expired, closed or tripped"}
	}
	if !p.CanSpend("children", 1) {
		return nil, nil, &GrantError{"the parent has no children budget left"}
	}
	if over := Within(p, def, g.Limits, now); len(over) > 0 {
		return nil, nil, &GrantError{"the grant widens what the parent has left: " + strings.Join(over, ", ")}
	}
	if g.ExpiresAt.After(p.ExpiresAt) || g.ExpiresAt.After(now.Add(time.Duration(g.Limits.TTLMinutes)*time.Minute)) {
		return nil, nil, &GrantError{"the grant outlives the parent or its own ttl"}
	}
	if sp.Children != 1 || sp.Reserve == nil || sp.Reserve.Children != 0 {
		return nil, nil, &GrantError{"the spend does not count one child and reserve its budgets"}
	}
	for _, f := range BudgetFields {
		if f != "children" && Budget(sp.Reserve, f) != LimitOf(&g.Limits, f) {
			return nil, nil, &GrantError{"the spend reserves other than the child's " + f}
		}
	}
	s := &Session{ID: g.Session, Principal: g.Principal, Agent: g.Agent, Limits: g.Limits, CreatedAt: now.UTC(),
		ExpiresAt: g.ExpiresAt.UTC(), ParentID: g.Parent, Depth: g.Depth, TaskID: g.TaskID, Usage: NoUsage()}
	for _, f := range BudgetFields {
		AddBudget(&p.Reserved, f, Budget(sp.Reserve, f))
	}
	p.Used.Children += sp.Children
	p.Children = append(p.Children, s.ID)
	s.serial.Lock() // held until the caller has made the child's run
	st.s[s.ID] = s
	return s, s.serial.Unlock, nil
}

// Charge applies the spend the core's decision names to the session the request named (internal/decide doc.go): add
// the counters to used, count a denial, trip the breaker with Trip, and trip the session and every descendant with
// TripTree. A spawn's Children and Reserve are the parent's, applied by Adopt. Caller holds s's lock.
func (st *Store) Charge(s *Session, sp client.Spend) (tripTree string) {
	if s == nil {
		return ""
	}
	s.Used.ToolCalls += sp.ToolCalls
	s.Used.GithubCalls += sp.GithubCalls
	s.Used.OpenPRs += sp.OpenPRs
	if sp.Denial {
		s.Denials++
	}
	if sp.Trip != "" {
		s.Trip(sp.Trip)
	}
	return sp.TripTree
}

// TripTree trips a session and every descendant. The caller holds no session lock.
func (st *Store) TripTree(id, reason string) {
	s := st.Get(id)
	if s == nil {
		return
	}
	s.Lock()
	s.Trip(reason)
	kids := append([]string(nil), s.Children...)
	s.Unlock()
	for _, c := range kids {
		st.TripTree(c, "parent tripped: "+reason)
	}
}

// Close ends a session and its children, returning the reserved budget to the parent with what was used.
func (st *Store) Close(id string) { st.CloseTouched(id) }

// CloseTouched is Close, and returns every session it changed (the ones it closed and the parents that took their
// budget back), so the caller can save them together: a parent whose budget came back must not be left saved with it
// still reserved against a child that is saved closed.
func (st *Store) CloseTouched(id string) []*Session {
	s := st.Get(id)
	if s == nil {
		return nil
	}
	s.Lock()
	if s.Closed {
		s.Unlock()
		return nil
	}
	kids := append([]string(nil), s.Children...)
	s.Unlock()
	var touched []*Session
	for _, c := range kids {
		touched = append(touched, st.CloseTouched(c)...)
	}
	s.Lock()
	if s.Closed { // closed by another caller while its children were
		s.Unlock()
		return touched
	}
	s.Closed = true
	parent, limits, used := s.ParentID, s.Limits, s.Used
	s.Unlock()
	touched = append(touched, s)
	if parent != nil {
		if p := st.Get(*parent); p != nil {
			p.Lock()
			for _, f := range BudgetFields {
				if f != "children" {
					AddBudget(&p.Reserved, f, -LimitOf(&limits, f))
					AddBudget(&p.Used, f, Budget(&used, f))
				}
			}
			p.Unlock()
			touched = append(touched, p)
		}
	}
	return touched
}

// Flush saves the given sessions (the ones an operation touched) if they changed since their last save, and returns
// once the save is durable. A no-op without persistence.
func (st *Store) Flush(ctx context.Context, sessions ...*Session) error {
	if st.persist == nil {
		return nil
	}
	var recs []Record
	var blobs []string
	for _, s := range sessions {
		if s == nil {
			continue
		}
		s.Lock()
		r := s.ToRecord()
		s.Unlock()
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		st.mu.RLock()
		same := st.saved[r.ID] == string(b)
		st.mu.RUnlock()
		if !same {
			recs, blobs = append(recs, r), append(blobs, string(b))
		}
	}
	if len(recs) == 0 {
		return nil
	}
	if err := st.persist.Save(ctx, recs); err != nil {
		return err
	}
	st.mu.Lock()
	for i, r := range recs {
		st.saved[r.ID] = blobs[i]
	}
	st.mu.Unlock()
	return nil
}

// FlushAll saves every session that changed since its last save (Python: SessionStore.flush).
func (st *Store) FlushAll(ctx context.Context) error {
	return st.Flush(ctx, st.All()...)
}

// Sweep closes everything past its deadline and returns the ids. The AgentRun composition enforces expiry on its own;
// this only keeps the gateway's own view consistent.
func (st *Store) Sweep(now time.Time) []string {
	var gone []string
	for _, s := range st.All() {
		s.Lock()
		expired := !s.Closed && !now.Before(s.ExpiresAt)
		s.Unlock()
		if expired {
			gone = append(gone, s.ID)
		}
	}
	for _, id := range gone {
		st.Close(id)
	}
	return gone
}
