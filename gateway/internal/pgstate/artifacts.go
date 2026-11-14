package pgstate

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/backends"
)

// Artifacts is backends.ArtifactStore on gateway_artifacts (engine/pg.py PgArtifacts): text a task's sessions keep for each
// other, keyed by task. A put is its own transaction (the Python gateway's joins the call's, which commits with the session):
// it first removes what has expired, counts the task's other artifacts under a lock on the task so two puts cannot both pass
// the quota, and upserts.
type Artifacts struct{ st *State }

// NewArtifacts returns the artifact store on st.
func NewArtifacts(st *State) *Artifacts { return &Artifacts{st: st} }

var _ backends.ArtifactStore = (*Artifacts)(nil)

// Put stores or replaces one artifact.
func (a *Artifacts) Put(ctx context.Context, task, name, content string, now time.Time) (map[string]any, error) {
	tx, err := a.st.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "gateway_artifacts/"+task); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM gateway_artifacts WHERE expires_at <= $1", now); err != nil {
		return nil, err
	}
	var count, total int64
	if err := tx.QueryRow(ctx, "SELECT count(*), coalesce(sum(bytes), 0) FROM gateway_artifacts WHERE task_id = $1 AND name <> $2",
		task, name).Scan(&count, &total); err != nil {
		return nil, err
	}
	size := len(content)
	if count+1 > backends.MaxArtifactCount || total+int64(size) > backends.MaxArtifactTotal {
		return nil, backends.QuotaDenied()
	}
	exp := now.Add(backends.ArtifactTTL)
	if _, err := tx.Exec(ctx, "INSERT INTO gateway_artifacts (task_id, name, content, bytes, expires_at) VALUES ($1, $2, $3, $4, $5) "+
		"ON CONFLICT (task_id, name) DO UPDATE SET content = EXCLUDED.content, bytes = EXCLUDED.bytes, "+
		"expires_at = EXCLUDED.expires_at, written_at = now()", task, name, content, size, exp); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return backends.PutResult(name, size, exp), nil
}

// Get is the artifact's content, or false when there is none or it has expired.
func (a *Artifacts) Get(ctx context.Context, task, name string, now time.Time) (string, bool, error) {
	var content string
	err := a.st.Pool.QueryRow(ctx, "SELECT content FROM gateway_artifacts WHERE task_id = $1 AND name = $2 AND expires_at > $3",
		task, name, now).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return content, err == nil, err
}
