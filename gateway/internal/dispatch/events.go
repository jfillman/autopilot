package dispatch

// Event history (engine/events.py): what the trigger bridge received and what it did with each event. One row per event, kept
// for RETENTION: the event's summary (the one a run is given: id, app, routing key, type, subject) and the outcome per agent
// (launched, deduped, braked, held, refused, launch-failed) with the path that decided it (stream, recent, reconcile). It is
// history, not evidence: nothing is decided or spent here, so it is not in the hash chain. The read API (clearance-api) serves it.

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Event history limits (events.py).
const (
	EventRetention  = 30 * 24 * time.Hour
	MaxSubjectBytes = 2000
	MaxOutcomes     = 50
)

// OutcomeKeys are the fields of an outcome that are kept.
var OutcomeKeys = []string{"agent", "result", "session", "run", "rule", "via"}

// EventRecord is one handled event.
type EventRecord struct {
	ID         string
	ReceivedAt time.Time
	Source     string
	RoutingKey *string
	Type       *string
	App        *string
	Subject    map[string]any
	Outcomes   []map[string]any
}

// View is the row's JSON document (EventRecord.view).
func (e *EventRecord) View() map[string]any {
	return map[string]any{"id": e.ID, "received_at": PyIso(e.ReceivedAt), "source": e.Source, "routing_key": optStr(e.RoutingKey), "type": optStr(e.Type),
		"app": optStr(e.App), "subject": e.Subject, "outcomes": e.Outcomes}
}

// EventsStore keeps the history.
type EventsStore interface {
	Record(ctx context.Context, e *EventRecord) error
}

// EventFromSummary is events.from_summary: the record for one handled event from the bridge's summary of it and what was done per
// agent. The subject is trimmed to MaxSubjectBytes, the outcomes to their known keys. An event needs an id.
func EventFromSummary(summary map[string]any, outcomes []map[string]any, now time.Time, source string) (*EventRecord, error) {
	id, _ := summary["id"].(string)
	if id == "" {
		return nil, errors.New("an event needs an id")
	}
	subject := map[string]any{}
	if s, ok := summary["subject"].(map[string]any); ok {
		for k, v := range s {
			subject[k] = v
		}
	}
	content := map[string]any{}
	if c, ok := subject["content"].(map[string]any); ok {
		for k, v := range c {
			content[k] = v
		}
	}
	subject["content"] = content
	for len(content) > 0 && pyJSONLen(subject) > MaxSubjectBytes {
		// Python pops the last-inserted key; Go maps have no order, so the longest value goes first (the same bound, a different
		// choice of what to drop).
		longest, n := "", -1
		for k, v := range content {
			if l := pyJSONLen(v); l > n || (l == n && k > longest) {
				longest, n = k, l
			}
		}
		delete(content, longest)
	}
	kept := []map[string]any{}
	for _, o := range outcomes {
		if o["agent"] == nil || o["agent"] == "" {
			continue
		}
		m := map[string]any{}
		for _, k := range OutcomeKeys {
			if v, ok := o[k]; ok {
				m[k] = v
			}
		}
		kept = append(kept, m)
	}
	if len(kept) > MaxOutcomes {
		kept = kept[:MaxOutcomes]
	}
	rec := &EventRecord{ID: id, ReceivedAt: now, Source: source, Subject: subject, Outcomes: kept}
	if t, ok := summary["receivedAt"].(string); ok && t != "" {
		if p, err := ParseEventTime(t); err == nil {
			rec.ReceivedAt = p
		}
	}
	if s, ok := summary["routingKey"].(string); ok {
		rec.RoutingKey = &s
	}
	if s, ok := summary["type"].(string); ok {
		rec.Type = &s
	}
	if s, ok := summary["app"].(string); ok && s != "" {
		l := strings.ToLower(s)
		rec.App = &l
	}
	return rec, nil
}

// ParseEventTime reads an RFC 3339 time as glidepath-api writes it (`Z`), or Python's isoformat; a time with no zone is UTC.
func ParseEventTime(s string) (time.Time, error) {
	return ParsePyIso(strings.Replace(s, "Z", "+00:00", 1))
}

// RecordEvent keeps what the trigger bridge received and did with one event, for the read API's history. Not a decision and not
// evidence: nothing is denied or spent, so it is not in the chain. False when this deployment keeps no history
// (Gateway.record_event).
func (g *Gateway) RecordEvent(ctx context.Context, summary map[string]any, outcomes []map[string]any) (bool, error) {
	if g.Events == nil {
		return false, nil
	}
	rec, err := EventFromSummary(summary, outcomes, g.Now(), "glidepath")
	if err != nil {
		return false, err
	}
	return true, g.Events.Record(ctx, rec)
}
