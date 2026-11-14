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

	mu      sync.Mutex // one tick at a time
	fired   map[slotRef]struct{}
	checked map[slotRef]struct{}
	floors  map[string]time.Time
	pruned  time.Time
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
		key := SlotKey(sc.everyS, slot)
		cctx, cancel := s.db(ctx)
		won, err := s.ledger.Claim(cctx, sc.agent, key, "schedule", "", now, StaleBefore(s.started, now))
		cancel()
		if err != nil {
			s.log.Error("the schedule's ledger failed", "agent", sc.agent, "err", errKind(err))
			continue // not marked fired: tried again on the next tick
		}
		s.fired[ref] = struct{}{}
		if !won {
			continue // fired before this process started (a restart inside the window)
		}
		out := s.fire(ctx, sc, slot, start)
		cctx, cancel = s.db(ctx)
		if err := s.ledger.Decide(cctx, sc.agent, key, out.Result, out.Session, out.Run); err != nil {
			s.log.Error("the schedule's decision could not be written", "agent", sc.agent, "err", errKind(err))
		}
		cancel()
		s.metrics.SlotLaunch(sc.agent, out.Result)
		if out.Result == "launched" {
			s.metrics.SlotLag(sc.agent, now.Sub(start).Seconds())
		}
		done = append(done, out)
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

// fire opens and launches one claimed slot; the decision for its ledger row.
func (s *Scheduler) fire(ctx context.Context, sc schedule, slot int64, start time.Time) (out SlotOutcome) {
	out = SlotOutcome{Agent: sc.agent, Slot: slot, Result: "launch-failed"}
	fctx, cancel := context.WithTimeout(ctx, EventTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil { // one agent's failed launch must not skip the others in this tick
			s.log.Error("a schedule panicked", "agent", sc.agent, "slot", slot, "panic", fmt.Sprint(r))
			out.Result = "launch-failed"
		}
	}()
	r, err := s.gw.OpenTriggered(fctx, sc.agent, "interval:"+sc.everyS, SlotTaskID(sc.agent, slot))
	if err != nil {
		s.log.Error("a schedule could not open its session", "agent", sc.agent, "slot", slot, "err", errKind(err))
		return out
	}
	if !r.OK {
		out.Rule, _ = r.Decision["rule"].(string)
		out.Result = "refused"
		if out.Rule == "R024" {
			out.Result = "held"
		}
		return out
	}
	if r.Session != nil {
		out.Session = *r.Session
	}
	data, _ := r.Data.(map[string]any)
	if deduped, _ := data["deduped"].(bool); deduped && s.gw.SessionHasRun(out.Session) {
		out.Result = "deduped"
		return out
	}
	input := map[string]any{"prompt": fmt.Sprintf("Scheduled run (%s, slot starting %s). Carry out your instructions.", sc.everyS, dispatch.PyIso(start))}
	if d := s.gw.Definitions()[sc.agent]; d != nil && d.Instructions != nil && *d.Instructions != "" {
		input["instructions"] = *d.Instructions
	}
	launched, err := s.gw.Launch(fctx, out.Session, input)
	if err != nil {
		s.log.Error("a schedule's run was not launched", "agent", sc.agent, "slot", slot, "err", errKind(err))
		return out
	}
	if launched.OK {
		out.Result = "launched"
		if d, ok := launched.Data.(map[string]any); ok {
			out.Run, _ = d["run"].(string)
		}
	}
	return out
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
