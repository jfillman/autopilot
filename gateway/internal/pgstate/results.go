package pgstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// Results is dispatch.ResultStore on gateway_results: a finished run's answer, by session, until it expires
// (engine/pg.py PgResults). A Put first deletes what has expired. Unlike the Python gateway's, a Put is its own
// transaction rather than part of the run_finish call's: the gateway writes the answer first and closes the session
// after, so a crash between the two leaves an answer kept for a session that is still open, which finishes again.
type Results struct{ st *State }

// NewResults returns the result store on st.
func NewResults(st *State) *Results { return &Results{st: st} }

var _ dispatch.ResultStore = (*Results)(nil)

// Put keeps a record; a session that already has one keeps the first.
func (r *Results) Put(ctx context.Context, rec map[string]any, now time.Time) error {
	id, _ := rec["session"].(string)
	task, _ := rec["task_id"].(string)
	exp, err := time.Parse(time.RFC3339, fmt.Sprint(rec["expires_at"]))
	if err != nil || id == "" {
		return errors.New("a result record needs a session and an expiry")
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tx, err := r.st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM gateway_results WHERE expires_at <= $1", now); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO gateway_results (session, task_id, record, expires_at) VALUES ($1, $2, $3, $4) "+
		"ON CONFLICT (session) DO NOTHING", id, task, raw, exp); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// Get is the record, or nil when there is none or it has expired.
func (r *Results) Get(ctx context.Context, sessionID string, now time.Time) (map[string]any, error) {
	var raw []byte
	err := r.st.Pool.QueryRow(ctx, "SELECT record FROM gateway_results WHERE session = $1 AND expires_at > $2", sessionID, now).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(raw, &out)
}
