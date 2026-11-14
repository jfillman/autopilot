package runs

import (
	"context"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// Store is where AgentRun XRs go (the Kubernetes API in Kube; a fake in tests): gateway.AgentRunStore.
type Store interface {
	Create(ctx context.Context, manifest map[string]any) error
	Delete(ctx context.Context, name string) error
	// Outcome is how the run's workload ended, "Gone" when it no longer exists, "" while it runs.
	Outcome(ctx context.Context, name string) (string, error)
}

// Backend is the run backend on Hangar: a session's run is an AgentRun XR (gateway.AgentRunBackend). It satisfies
// dispatch.RunBackend.
type Backend struct{ Store Store }

// Name is the name a session's run has, before it exists. The gateway writes it into the session before it creates the
// run, so a crash between the two leaves a session whose sweep deletes the run it may have started.
func (b *Backend) Name(s *session.Session) string { return RunName(s.ID) }

// Create builds the session's AgentRun, sends it, and returns its name.
func (b *Backend) Create(ctx context.Context, s *session.Session, d *definitions.Definition, input map[string]any) (string, error) {
	m, err := Manifest(s, d, input)
	if err != nil {
		return "", err
	}
	if err := b.Store.Create(ctx, m); err != nil {
		return "", err
	}
	return m["metadata"].(map[string]any)["name"].(string), nil
}

// Delete removes the session's AgentRun (by the name its id gives it).
func (b *Backend) Delete(ctx context.Context, s *session.Session) error {
	return b.Store.Delete(ctx, RunName(s.ID))
}

// Ended is how the session's run ended, for the sweep's backstop when a run exits without run_finish: "" while it runs.
func (b *Backend) Ended(ctx context.Context, s *session.Session) (string, error) {
	return b.Store.Outcome(ctx, RunName(s.ID))
}

// Principal is the identity a run's workload authenticates as (see the package-level Principal).
func (b *Backend) Principal(run string) string { return Principal(run) }
