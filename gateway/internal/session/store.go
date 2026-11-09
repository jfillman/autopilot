package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
func (st *Store) Close(id string) {
	s := st.Get(id)
	if s == nil {
		return
	}
	s.Lock()
	if s.Closed {
		s.Unlock()
		return
	}
	kids := append([]string(nil), s.Children...)
	s.Unlock()
	for _, c := range kids {
		st.Close(c)
	}
	s.Lock()
	s.Closed = true
	parent, limits, used := s.ParentID, s.Limits, s.Used
	s.Unlock()
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
		}
	}
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
