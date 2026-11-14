package dispatch

import (
	"fmt"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// checkOpenGrant is the gateway's own check of the session the core granted a top-level open (a person's run, a trigger's): the
// core cuts the grant, and this checks it against the definition it was asked about, as AdoptChild checks a spawn's. A grant for
// another agent, with a parent, deeper than 0, wider than the definition's limits (any budget, tier, tool, model, network or
// compute above them), for another principal than the one that asked (wantPrincipal, when the caller names one), for a session id
// the gateway did not name, or outliving the definition's TTL is refused and nothing is created. So a bug or a bad bundle in the
// core cannot widen what a trigger or a person's run may spend. Returns "" when the grant is acceptable.
func checkOpenGrant(gr *client.Grant, wantID, wantPrincipal string, d *definitions.Definition, now time.Time) string {
	switch {
	case gr == nil:
		return "no grant"
	case gr.Session == "" || gr.Session != wantID:
		return "the grant's session id is not the one the gateway named"
	case gr.Agent != d.Name:
		return "the grant is for another agent than the one asked for"
	case gr.Parent != nil || gr.Depth != 0:
		return "the grant names a parent: a top-level open has none"
	case gr.Principal == "" || (wantPrincipal != "" && gr.Principal != wantPrincipal):
		return "the grant names another principal than the one that asked"
	case gr.TaskID == "":
		return "the grant has no task"
	case gr.ExpiresAt.After(now.Add(time.Duration(d.Limits.TTLMinutes) * time.Minute)):
		return "the grant outlives the definition's ttl"
	}
	ceiling := &session.Session{Limits: d.Limits, ExpiresAt: now.Add(time.Duration(d.Limits.TTLMinutes) * time.Minute)}
	if over := session.Within(ceiling, d.Limits, gr.Limits, now); len(over) > 0 {
		return "the grant widens the definition's limits: " + strings.Join(over, ", ")
	}
	return ""
}

var errGrantRefused = fmt.Errorf("the core's grant was refused")
