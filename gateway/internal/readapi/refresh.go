package readapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// Refresher keeps the pinned definitions current from the rows the gateway wrote (agent_xrs.DefinitionsRefresher): every Every it
// rebuilds the cluster set from gateway_definitions. It fetches nothing and decides nothing: each row says loaded or rejected, as the writer
// (`serve`) decided it against the deployment's strict pin policy and stored it (serve keeps its decision at start and on every poll), and
// that is what is shown. The policy given to FromRecords below is only the floor a row must clear to be built into a definition at all. A
// failed refresh keeps the set it has.
type Refresher struct {
	Set   *definitions.Set
	Store interface {
		Definitions(context.Context) ([]definitions.Row, error)
	}
	Reg   *tools.Registry
	Every time.Duration
	Log   *slog.Logger
}

// Once reads the rows and replaces the cluster set.
func (r *Refresher) Once(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := r.Store.Definitions(cctx)
	if err != nil {
		r.Log.Warn("definitions: the refresh failed; the set stands", "err", err.Error())
		return
	}
	loaded, rejected := definitions.FromRecords(rows, r.Set.Bundled(), r.Reg, definitions.PinPolicy{Tools: r.Reg})
	if ch := r.Set.ReplaceCluster(loaded, rejected); ch.Any() {
		r.Log.Info("definitions: the cluster set changed", "loaded", len(loaded), "rejected", len(rejected))
	}
}

// Run refreshes until ctx ends.
func (r *Refresher) Run(ctx context.Context) {
	t := time.NewTicker(r.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if p := recover(); p != nil {
						r.Log.Error("definitions: the refresh panicked", "panic", p)
					}
				}()
				r.Once(ctx)
			}()
		}
	}
}
