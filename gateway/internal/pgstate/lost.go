package pgstate

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/core"
)

// LostMarker is core.LostStore on gateway_lost_outcome: the time of the last outcome with paths that could be neither posted nor queued.
// There is one row at most; Mark only ever moves it forward.
type LostMarker struct{ st *State }

// NewLostMarker returns the lost-outcome marker on st.
func NewLostMarker(st *State) *LostMarker { return &LostMarker{st: st} }

var _ core.LostStore = (*LostMarker)(nil)

// Mark records a loss at at (an earlier time than the one held changes nothing).
func (l *LostMarker) Mark(ctx context.Context, at time.Time) error {
	_, err := l.st.Pool.Exec(ctx, `INSERT INTO gateway_lost_outcome (id, lost_at) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET lost_at = GREATEST(gateway_lost_outcome.lost_at, EXCLUDED.lost_at)`, at)
	return err
}

// Load reads the marker (zero: none).
func (l *LostMarker) Load(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := l.st.Pool.QueryRow(ctx, "SELECT lost_at FROM gateway_lost_outcome WHERE id").Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return at, err
}

// Clear removes the marker.
func (l *LostMarker) Clear(ctx context.Context) error {
	_, err := l.st.Pool.Exec(ctx, "DELETE FROM gateway_lost_outcome")
	return err
}
