package pgstate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/dbindex"
)

// MigrateLockKey is "apg1": one migrate at a time (the writer's lock, "apg0", is the running gateway's, and a migrate must run beside it).
const MigrateLockKey = 0x61706731

// MigrateOptions bound what a migrate can do to a database that is in use.
type MigrateOptions struct {
	// LockTimeout is how long one statement waits for a lock (default 5 s). CREATE INDEX CONCURRENTLY takes a SHARE UPDATE EXCLUSIVE
	// lock, which conflicts with nothing a gateway does, but it waits for every transaction that began before it to finish; a
	// transaction held open for longer than this fails the migrate (re-run it) instead of queueing writes behind a DDL statement. ALTER TABLE in
	// the schema step needs an ACCESS EXCLUSIVE lock for a moment and is held to the same bound.
	LockTimeout time.Duration
	// StatementTimeout is the longest one index may take to build (default 15 minutes: the tables are some thousands of rows and take
	// a second; a table far past that wants a decision, not a wait).
	StatementTimeout time.Duration
	// Log, when set, is told what each step did.
	Log func(msg string)
}

func (o MigrateOptions) withDefaults() MigrateOptions {
	if o.LockTimeout <= 0 {
		o.LockTimeout = 5 * time.Second
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 15 * time.Minute
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	return o
}

// ErrMigrateRunning is returned when another migrate holds the migrate lock.
var ErrMigrateRunning = errors.New("another migrate is running against this database")

// Migrate brings the database to what the Go gateway needs: the schema (idempotent, as serve's own start does it) and the six read API
// indexes, each created with CREATE INDEX CONCURRENTLY, so no write to a gateway table waits for a build. It may run while a gateway (Go or
// Python) is writing. It is idempotent: a valid index is left alone, and an INVALID one (what an interrupted CONCURRENTLY build leaves) is
// dropped, concurrently, and built again. A failure leaves no half state that a second run does not repair. It returns the names it created.
func Migrate(ctx context.Context, dsn string, o MigrateOptions) (created []string, err error) {
	o = o.withDefaults()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_DSN: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(MigrateLockKey)).Scan(&got); err != nil {
		return nil, err
	}
	if !got {
		return nil, ErrMigrateRunning
	}
	for _, set := range []string{
		fmt.Sprintf("SET lock_timeout = %d", o.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET statement_timeout = %d", o.StatementTimeout.Milliseconds()),
	} {
		if _, err := conn.Exec(ctx, set); err != nil {
			return nil, err
		}
	}
	if _, err := conn.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if _, err := conn.Exec(ctx, goSchema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	states, err := dbindex.Check(ctx, dbindex.Pgx(conn))
	if err != nil {
		return nil, err
	}
	for _, s := range states {
		switch {
		case s.Valid:
			o.Log("index " + s.Name + ": present")
			continue
		case s.Present:
			o.Log("index " + s.Name + ": invalid (an interrupted build): dropping it and building it again")
			if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+s.Name); err != nil {
				return created, fmt.Errorf("index %s: %w", s.Name, err)
			}
		}
		start := time.Now()
		if _, err := conn.Exec(ctx, createIndex(s.Index)); err != nil {
			return created, fmt.Errorf("index %s: %w (a failed build leaves an invalid index: run migrate again; a timeout is a lock held open or a table past the statement timeout)", s.Name, err)
		}
		created = append(created, s.Name)
		o.Log(fmt.Sprintf("index %s: built in %s", s.Name, time.Since(start).Round(time.Millisecond)))
	}
	return created, nil
}

// createIndex is the statement for one index. IF NOT EXISTS keeps a concurrent creator harmless.
func createIndex(ix dbindex.Index) string {
	return fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s %s", ix.Name, ix.Table, ix.Def)
}

// CheckIndexes is the state of the six indexes, as serve asks it at start.
func (s *State) CheckIndexes(ctx context.Context) ([]dbindex.State, error) {
	return dbindex.Check(ctx, dbindex.Pgx(s.Pool))
}
