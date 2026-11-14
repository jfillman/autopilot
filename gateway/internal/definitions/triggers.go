package definitions

import (
	"encoding/json"
	"regexp"
	"time"
)

// Trigger is one entry of a definition's triggers, as far as the Go gateway acts on it: an interval ("every 24h") and a
// Glidepath event. Manual triggers are carried (a definition without a triggers list has one) but start nothing. Alert triggers
// and RabbitMQ events are in the schema and have no reader in the Python gateway either (event_bridge.py reads only Glidepath's
// stream; EVENTS_RABBITMQ_ENABLED refuses to start), so they are not carried and are dropped here.
//
// The definition set is trusted (the bundled agents and what the core accepted as a pin), but nothing here assumes it was
// validated against the schema: an entry that does not have the shape the schema gives it is dropped, so a malformed trigger
// fires nothing.
type Trigger struct {
	Type string // manual | interval | event
	// interval
	Every    string
	Interval time.Duration
	// event (source glidepath)
	BindingKey string
	Match      map[string]string
	MaxPerHour int // 0 until set: the bridge's default applies
}

// Limits of a trigger, from the schema.
const (
	MaxMaxPerHour = 600
	maxInterval   = 366 * 24 * time.Hour
)

var (
	everyRe   = regexp.MustCompile(`^([0-9]+)([mh])$`)
	bindingRe = regexp.MustCompile(`^[a-z0-9*#][a-z0-9*#.-]*$`)
	matchKey  = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)
)

// ParseInterval is triggers.parse_interval: "30m", "24h"; the whole number is at least 1. ok is false for anything else, and
// for an interval over a year (nothing valid is that long, and it would overflow a duration).
func ParseInterval(s string) (time.Duration, bool) {
	m := everyRe.FindStringSubmatch(s)
	if m == nil || len(m[1]) > 6 {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	if n < 1 {
		return 0, false
	}
	d := time.Duration(n) * time.Minute
	if m[2] == "h" {
		d = time.Duration(n) * time.Hour
	}
	return d, d <= maxInterval
}

func parseTriggers(raw []json.RawMessage, present bool) []Trigger {
	if !present {
		return []Trigger{{Type: "manual"}}
	}
	var out []Trigger
	for _, r := range raw {
		var t struct {
			Type       string          `json:"type"`
			Every      string          `json:"every"`
			Source     string          `json:"source"`
			BindingKey string          `json:"bindingKey"`
			Match      json.RawMessage `json:"match"`
			MaxPerHour json.RawMessage `json:"maxPerHour"`
		}
		if json.Unmarshal(r, &t) != nil {
			continue
		}
		switch {
		case t.Type == "manual":
			out = append(out, Trigger{Type: "manual"})
		case t.Type == "interval":
			if d, ok := ParseInterval(t.Every); ok {
				out = append(out, Trigger{Type: "interval", Every: t.Every, Interval: d})
			}
		case t.Type == "event" && t.Source == "glidepath":
			if !bindingRe.MatchString(t.BindingKey) || len(t.BindingKey) > 200 {
				continue
			}
			tr := Trigger{Type: "event", BindingKey: t.BindingKey}
			if len(t.Match) > 0 && string(t.Match) != "null" {
				var m map[string]string
				if json.Unmarshal(t.Match, &m) != nil || len(m) == 0 || len(m) > 20 {
					continue
				}
				ok := true
				for k, v := range m {
					ok = ok && matchKey.MatchString(k) && len(k) <= 200 && len(v) <= 500
				}
				if !ok {
					continue
				}
				tr.Match = m
			}
			if len(t.MaxPerHour) > 0 && string(t.MaxPerHour) != "null" {
				var n int
				if json.Unmarshal(t.MaxPerHour, &n) != nil || n < 1 || n > MaxMaxPerHour {
					continue
				}
				tr.MaxPerHour = n
			}
			out = append(out, tr)
		}
	}
	return out
}
