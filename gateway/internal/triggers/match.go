package triggers

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// AMQPTopicMatch is RabbitMQ topic semantics: words split on '.', '*' is exactly one word, '#' is zero or more. It is a table
// over (pattern word, key word), linear in the product of their lengths: Python's recursion is exponential in the number of '#'
// words, and the key is an event's, which a source chooses.
func AMQPTopicMatch(pattern, key string) bool {
	p, k := strings.Split(pattern, "."), strings.Split(key, ".")
	// ok[i][j]: pattern[i:] matches key[j:]
	ok := make([][]bool, len(p)+1)
	for i := range ok {
		ok[i] = make([]bool, len(k)+1)
	}
	ok[len(p)][len(k)] = true
	for i := len(p) - 1; i >= 0; i-- {
		for j := len(k); j >= 0; j-- {
			switch {
			case p[i] == "#":
				ok[i][j] = ok[i+1][j] || (j < len(k) && ok[i][j+1])
			case j == len(k):
				ok[i][j] = false
			default:
				ok[i][j] = (p[i] == "*" || p[i] == k[j]) && ok[i+1][j+1]
			}
		}
	}
	return ok[0][0]
}

// pathAt reads a dotted path in a CDEvent; anything that is not an object on the way is nothing.
func pathAt(doc any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, false
		}
		if doc, ok = m[part]; !ok {
			return nil, false
		}
	}
	return doc, true
}

// Match is an agent whose Glidepath event trigger an envelope satisfies.
type Match struct {
	Agent   string
	Trigger definitions.Trigger
}

// MatchGlidepathEvent is (agent, trigger) for each agent with a Glidepath event trigger the /v1/events envelope satisfies: its
// bindingKey against the envelope's routingKey (AMQP topic semantics), and every `match` path, read in the CDEvent, equal to its
// string value. The first matching trigger per agent, agents in name order.
func MatchGlidepathEvent(defs map[string]*definitions.Definition, env map[string]any) []Match {
	key, ok := env["routingKey"].(string)
	event, ok2 := env["event"].(map[string]any)
	if !ok || !ok2 {
		return nil
	}
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Match
	for _, name := range names {
		for _, t := range defs[name].Triggers {
			if t.Type != "event" || !AMQPTopicMatch(t.BindingKey, key) {
				continue
			}
			all := true
			for p, want := range t.Match {
				got, found := pathAt(event, p)
				if s, isStr := got.(string); !found || !isStr || s != want {
					all = false
				}
			}
			if all {
				out = append(out, Match{name, t})
				break
			}
		}
	}
	return out
}

// EventTaskID is a stable task id per (agent, key), so a redelivered message maps to the same run: `t-` and 12 hex digits, the
// AgentRun's form (Python: triggers.event_task_id).
func EventTaskID(agent, key string) string {
	h := sha256.Sum256([]byte(agent + ":" + key))
	return "t-" + hex.EncodeToString(h[:])[:12]
}

// SlotTaskID is the task id of an interval slot (scheduler.slot_task_id).
func SlotTaskID(agent string, slot int64) string {
	h := sha256.Sum256([]byte("sched:" + agent + ":" + itoa(slot)))
	return "t-" + hex.EncodeToString(h[:])[:12]
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// RateLimiter is a sliding one-hour window: the brake on an event storm.
type RateLimiter struct {
	PerHour int
	mu      sync.Mutex
	hits    []time.Time
}

// Allow counts a hit at now unless the window is full.
func (r *RateLimiter) Allow(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.hits[:0]
	for _, h := range r.hits {
		if now.Sub(h) < time.Hour {
			kept = append(kept, h)
		}
	}
	r.hits = kept
	if len(r.hits) >= r.PerHour {
		return false
	}
	r.hits = append(r.hits, now)
	return true
}
