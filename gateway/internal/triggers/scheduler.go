package triggers

// Interval triggers (`{type: interval, every: 24h}`): the scheduled agents' clock (scheduler.py).
//
// Each interval is a slot aligned to the Unix epoch in UTC (every 24h: 00:00 UTC; every 6h: 00, 06, 12, 18). An agent fires once
// per slot, within Grace of the slot's start, as OpenTriggered(agent, "interval:<every>", task id of the slot) then Launch with
// its instructions. A held agent (R024) is skipped. Nothing here knows Glidepath.
//
// Once per slot, across restarts: the slot's key (`slot:<every>:<n>`) is claimed in the trigger ledger before the session is
// opened, so a gateway restarted inside the grace window after the slot has fired, its run finished or not, does not fire it
// again. The grace window keeps a restart from firing a slot long after it began: a slot whose window passed with no firing (the
// gateway was down for all of it) is recorded `missed` in the ledger, once per slot (back to the floor), and counted; it is not
// run late. Slots that began before the agent's floor (when the scheduler first saw that agent with a ledger) are never called
// missed.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The scheduler's constants (scheduler.py).
const (
	Grace          = 10 * time.Minute
	TickEvery      = 30 * time.Second
	PruneEvery     = time.Hour
	MissedLookback = 744 // slots checked back per agent at most (31 days of hourly slots)
	SchedulePrefix = "schedule"
)

type slotRef struct {
	agent string
	slot  int64
}

// SlotOutcome is what one slot did.
type SlotOutcome struct {
	Agent   string
	Slot    int64
	Result  string // launched, deduped, held, refused, launch-failed, missed
	Rule    string
	Session string
	Run     string
}

// Scheduler fires interval triggers.
type Scheduler struct {
	gw      Gateway
	ledger  Ledger
	metrics Metrics
	log     *slog.Logger
	now     func() time.Time
	Tick_   time.Duration
	started time.Time

	mu        sync.Mutex // one tick at a time
	fired     map[slotRef]struct{}
	undecided pendingDecisions
	checked   map[slotRef]struct{}
	floors    map[string]time.Time
	pruned    time.Time
}

// NewScheduler builds a scheduler; now is the clock (nil: the wall clock).
func NewScheduler(gw Gateway, ledger Ledger, now func() time.Time, m Metrics, log *slog.Logger) *Scheduler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if m == nil {
		m = Nop{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{gw: gw, ledger: ledger, metrics: m, log: log, now: now, Tick_: TickEvery, started: now(),
		fired: map[slotRef]struct{}{}, checked: map[slotRef]struct{}{}, floors: map[string]time.Time{}}
}

// SlotOf is the slot `now` falls in and when it started.
func SlotOf(now time.Time, every time.Duration) (int64, time.Time) {
	secs := int64(every / time.Second)
	n := now.Unix() / secs
	if now.Unix() < 0 && now.Unix()%secs != 0 {
		n-- // floor, as Python's integer division
	}
	return n, time.Unix(n*secs, 0).UTC()
}

type schedule struct {
	agent, everyS string
	every         time.Duration
}

func (s *Scheduler) schedules() []schedule {
	defs := s.gw.Definitions()
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []schedule
	for _, n := range names {
		d := defs[n]
		if d.Kind != "scheduled" || d.IdentityType != "workload" {
			continue
		}
		for _, t := range d.Triggers {
			if t.Type == "interval" {
				out = append(out, schedule{n, t.Every, t.Interval})
			}
		}
	}
	return out
}

func (s *Scheduler) db(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, DBTimeout)
}

// floor is when this agent's schedule was first seen with a ledger: a durable row per agent (gateway_event_cursors source
// `schedule:<agent>`), so an agent added to a running deployment never has earlier slots called missed.
func (s *Scheduler) floor(ctx context.Context, agent string, now time.Time) (time.Time, error) {
	if f, ok := s.floors[agent]; ok {
		return f, nil
	}
	cctx, cancel := s.db(ctx)
	defer cancel()
	c, err := s.ledger.Cursor(cctx, SchedulePrefix+":"+agent, now)
	if err != nil {
		return time.Time{}, err
	}
	s.floors[agent] = c.Floor
	return c.Floor, nil
}

// checkMissed records every slot whose grace window has passed, back to the ledger's floor (at most MissedLookback slots): each
// one nothing fired is recorded missed, once. A gateway down for three nights records three.
func (s *Scheduler) checkMissed(ctx context.Context, sc schedule, slot int64, start, now time.Time) ([]SlotOutcome, error) {
	if sc.every <= Grace { // windows overlap: a short interval's next slot comes before a miss could be told
		return nil, nil
	}
	last, lastStart := slot-1, start.Add(-sc.every)
	if now.Sub(start) > Grace {
		last, lastStart = slot, start
	}
	floor, err := s.floor(ctx, sc.agent, now)
	if err != nil {
		return nil, err
	}
	var out []SlotOutcome
	for n := int64(0); n < MissedLookback; n++ {
		sl, slStart := last-n, lastStart.Add(-time.Duration(n)*sc.every)
		ref := slotRef{sc.agent, sl}
		if _, checked := s.checked[ref]; slStart.Before(floor) || checked {
			break // before the ledger existed, or already checked (and so is everything older)
		}
		s.checked[ref] = struct{}{}
		if _, fired := s.fired[ref]; fired {
			break
		}
		key := SlotKey(sc.everyS, sl)
		cctx, cancel := s.db(ctx)
		row, err := s.ledger.Get(cctx, sc.agent, key)
		cancel()
		if err != nil {
			delete(s.checked, ref) // asked again on the next tick
			return out, err
		}
		// Any row means the slot was fired or already called missed, and every slot before it was accounted for then: stop. (A
		// `claimed` orphan a dead process left may have launched its run: not missed either.)
		if row != nil {
			if row.State == "claimed" { // an orphan: taken over and resolved (its session decides; past its grace nothing is started)
				o, ok := s.handleSlot(ctx, sc, sl, slStart, now)
				if !ok {
					delete(s.checked, ref) // not decided: looked at again on the next tick, not only after a restart
					return out, nil
				}
				if o != nil {
					out = append(out, *o)
				}
			}
			break
		}
		cctx, cancel = s.db(ctx)
		won, err := s.ledger.Claim(cctx, sc.agent, key, "schedule", "", now, now.Add(-Stale))
		cancel()
		if err != nil {
			delete(s.checked, ref)
			return out, err
		}
		if !won {
			continue
		}
		cctx, cancel = s.db(ctx)
		err = s.ledger.Decide(cctx, sc.agent, key, "missed", "", "")
		cancel()
		if err != nil {
			s.log.Error("a missed slot could not be recorded", "agent", sc.agent, "err", errKind(err))
		}
		s.metrics.SlotMissed(sc.agent)
		s.log.Warn("a schedule's slot passed with no firing: missed", "agent", sc.agent, "slot", sl, "start", dispatch.PyIso(slStart))
		out = append(out, SlotOutcome{Agent: sc.agent, Slot: sl, Result: "missed"})
	}
	return out, nil
}

// Tick fires what is due and records what was missed; what was done. A ledger failure ends the agent's turn this tick (the slot
// is looked at again on the next one, while its grace window lasts) and is logged.
func (s *Scheduler) Tick(ctx context.Context) []SlotOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var done []SlotOutcome
	if s.pruned.IsZero() || now.Sub(s.pruned) >= PruneEvery {
		cctx, cancel := s.db(ctx)
		if _, err := s.ledger.Prune(cctx, now); err != nil {
			s.log.Warn("the trigger ledger could not be pruned", "err", errKind(err))
		}
		cancel()
		s.pruned = now
	}
	current := map[string]int64{}
	for _, sc := range s.schedules() {
		slot, start := SlotOf(now, sc.every)
		current[sc.agent] = slot
		missed, err := s.checkMissed(ctx, sc, slot, start, now)
		done = append(done, missed...)
		if err != nil {
			s.log.Error("the schedule's ledger failed", "agent", sc.agent, "err", errKind(err))
			continue
		}
		ref := slotRef{sc.agent, slot}
		if _, fired := s.fired[ref]; fired || now.Sub(start) > Grace {
			continue
		}
		if out, ok := s.handleSlot(ctx, sc, slot, start, now); ok {
			s.fired[ref] = struct{}{} // only once the slot's row is decided
			if out != nil {
				done = append(done, *out)
			}
		}
	}
	// The sets of slots already handled are bounded: nothing older than the lookback is looked at again.
	for _, set := range []map[slotRef]struct{}{s.fired, s.checked} {
		for ref := range set {
			if cur, ok := current[ref.agent]; !ok || ref.slot < cur-MissedLookback-2 {
				delete(set, ref)
			}
		}
	}
	for _, d := range done {
		s.log.Info("schedule slot", "agent", d.Agent, "slot", d.Slot, "result", d.Result)
	}
	return done
}

// handleSlot takes a slot's claim (a row still `claimed` is an orphan and is taken over at once, see fire.go), carries the slot out and
// writes its decision. ok is false when the slot is not decided (the ledger failed, or the slot could not be carried out): it is not
// marked fired and is looked at again on the next tick. A slot whose row is already decided gives a nil outcome and ok true.
func (s *Scheduler) handleSlot(ctx context.Context, sc schedule, slot int64, start, now time.Time) (*SlotOutcome, bool) {
	key := SlotKey(sc.everyS, slot)
	cctx, cancel := s.db(ctx)
	won, err := s.ledger.Claim(cctx, sc.agent, key, "schedule", "", now, now)
	cancel()
	if err != nil {
		s.log.Error("the schedule's ledger failed", "agent", sc.agent, "err", errKind(err))
		return nil, false
	}
	if !won {
		s.undecided.drop(sc.agent, key)
		return nil, true // decided: fired before (or called missed)
	}
	var out SlotOutcome
	if p, ok := s.undecided.take(sc.agent, key); ok {
		out = SlotOutcome{Agent: sc.agent, Slot: slot, Result: p.Result, Rule: p.Rule, Session: p.Session, Run: p.Run}
	} else {
		var ferr error
		if out, ferr = s.fire(ctx, sc, slot, start, now); ferr != nil {
			s.log.Error("the schedule could not be carried out and stays claimed for the next tick", "agent", sc.agent, "err", errKind(ferr))
			return nil, false
		}
	}
	cctx, cancel = s.db(ctx)
	err = retry(cctx, RetryWaits, func() error { return s.ledger.Decide(cctx, sc.agent, key, out.Result, out.Session, out.Run) })
	cancel()
	if err != nil {
		s.log.Error("the schedule's decision could not be written", "agent", sc.agent, "err", errKind(err))
		s.undecided.put(sc.agent, key, fireOut{Result: out.Result, Session: out.Session, Run: out.Run, Rule: out.Rule})
		return nil, false
	}
	s.undecided.drop(sc.agent, key)
	s.metrics.SlotLaunch(sc.agent, out.Result)
	if out.Result == "launched" {
		s.metrics.SlotLag(sc.agent, now.Sub(start).Seconds())
	}
	return &out, true
}

// fire carries out one claimed slot; an error means it is neither decided nor handled (the claim stays for the next tick).
func (s *Scheduler) fire(ctx context.Context, sc schedule, slot int64, start, now time.Time) (out SlotOutcome, err error) {
	out = SlotOutcome{Agent: sc.agent, Slot: slot, Result: "launch-failed"}
	key := SlotKey(sc.everyS, slot)
	defer func() {
		if r := recover(); r != nil { // one agent's failed launch must not skip the others in this tick
			s.log.Error("a schedule panicked", "agent", sc.agent, "slot", slot, "panic", fmt.Sprint(r))
			err = fmt.Errorf("a schedule panicked") // not decided, not counted: the next tick resumes from what the row and the session recorded
		}
	}()
	fctx, cancel := context.WithTimeout(ctx, EventTimeout)
	defer cancel()
	gctx, gcancel := s.db(fctx)
	row, _ := s.ledger.Get(gctx, sc.agent, key)
	gcancel()
	prior := ""
	if row != nil && row.Session != nil {
		prior = *row.Session
	}
	r := &resumer{gw: s.gw, ledger: s.ledger, log: s.log, waits: RetryWaits, agent: sc.agent, key: key, task: SlotTaskID(sc.agent, slot),
		valid: func() bool { return now.Sub(start) <= Grace },
		input: func() map[string]any {
			input := map[string]any{"prompt": fmt.Sprintf("Scheduled run (%s, slot starting %s). Carry out your instructions.", sc.everyS, dispatch.PyIso(start))}
			if d := s.gw.Definitions()[sc.agent]; d != nil && d.Instructions != nil && *d.Instructions != "" {
				input["instructions"] = *d.Instructions
			}
			return input
		},
		open: func(ctx context.Context) (*opened, *fireOut, error) {
			res, oerr := s.gw.OpenTriggered(ctx, sc.agent, "interval:"+sc.everyS, SlotTaskID(sc.agent, slot))
			if oerr != nil {
				if dispatch.IsResumable(oerr) {
					return nil, nil, oerr
				}
				s.log.Error("a schedule could not open its session", "agent", sc.agent, "slot", slot, "err", errKind(oerr))
				return nil, &fireOut{Result: "launch-failed"}, nil
			}
			if !res.OK {
				rule, _ := res.Decision["rule"].(string)
				if rule == "R024" {
					return nil, &fireOut{Result: "held", Rule: rule}, nil
				}
				return nil, &fireOut{Result: "refused", Rule: rule}, nil
			}
			data, _ := res.Data.(map[string]any)
			sid := ""
			if res.Session != nil {
				sid = *res.Session
			}
			dd, _ := data["deduped"].(bool)
			return &opened{session: sid, deduped: dd}, nil, nil
		}}
	res, ferr := r.run(fctx, prior)
	if ferr != nil {
		return out, ferr
	}
	out.Result, out.Session, out.Run, out.Rule = res.Result, res.Session, res.Run, res.Rule
	return out, nil
}

// Run ticks until ctx ends. A failed tick is logged, never fatal.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.Tick_)
	defer t.Stop()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("a schedule tick panicked", "panic", fmt.Sprint(r))
				}
			}()
			s.Tick(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
