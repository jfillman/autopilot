package dispatch

import (
	"context"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// Spawn is the spawn step of an allowed session.spawn, for the tests that drive it with a decision they build.
func (g *Gateway) Spawn(ctx context.Context, parent *session.Session, d *definitions.Definition, v *core.Verdict, newID string, args map[string]any, now time.Time) (any, error) {
	return g.spawn(ctx, parent, d, v, newID, args, now)
}

// RunInput is a spawn's input check.
func RunInput(v any) (map[string]any, error) { return runInput(v) }
