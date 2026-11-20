package pgstate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

// Migrate brings the database to what the Go gateway needs: the schema (idempotent, as serve's own start does it) and the seven
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
	// The schema step is skipped when the database already has it (the Go-only columns and the lost-outcome table are the last things it adds): ALTER TABLE ... ADD COLUMN IF NOT EXISTS
	// takes an ACCESS EXCLUSIVE lock even when the column is there, so a routine re-run must not run it.
	var haveCols int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
		AND table_name = 'gateway_trigger_launches' AND column_name IN ('failures', 'first_failed_at', 'last_failed_at')`).Scan(&haveCols); err != nil {
		return nil, err
	}
	var haveLost bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('gateway_lost_outcome') IS NOT NULL`).Scan(&haveLost); err != nil {
		return nil, err
	}
	if haveCols == 3 && haveLost {
		o.Log("schema: present (no statement run)")
	} else {
		if _, err := conn.Exec(ctx, schema); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
		for _, stmt := range []string{goSchema, goLostSchema} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("schema: %w", err)
			}
		}
		o.Log("schema: created or completed")
	}
	states, err := dbindex.Check(ctx, dbindex.Pgx(conn))
	if err != nil {
		return nil, err
	}
	wrong, err := WrongDefinitions(ctx, conn)
	if err != nil {
		return nil, err
	}
	for _, s := range states {
		switch {
		case s.Valid && wrong[s.Name]:
			// Right name, wrong definition (an older release's, or one made by hand): build the right one beside it under a temporary name,
			// drop the old one, rename. The table is never without an index for the query, and no write waits for the build.
			o.Log("index " + s.Name + ": present with another definition: building the right one beside it")
			tmp := s.Name + "_new"
			if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+tmp); err != nil {
				return created, fmt.Errorf("index %s: %w", s.Name, err)
			}
			if _, err := conn.Exec(ctx, createIndex(dbindex.Index{Name: tmp, Table: s.Table, Def: s.Def})); err != nil {
				return created, fmt.Errorf("index %s: %w (a failed build leaves an invalid %s: run migrate again)", s.Name, err, tmp)
			}
			if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY "+s.Name); err != nil {
				return created, fmt.Errorf("index %s: %w", s.Name, err)
			}
			if _, err := conn.Exec(ctx, "ALTER INDEX "+tmp+" RENAME TO "+s.Name); err != nil {
				return created, fmt.Errorf("index %s: %w", s.Name, err)
			}
			created = append(created, s.Name)
			continue
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

// CheckIndexes is the state of the seven indexes, as serve asks it at start.
func (s *State) CheckIndexes(ctx context.Context) ([]dbindex.State, error) {
	return dbindex.Check(ctx, dbindex.Pgx(s.Pool))
}

var indexDefNorm = regexp.MustCompile(`^CREATE (UNIQUE )?INDEX \S+ ON \S+ `)

// WrongDefinitions names each of the seven indexes that exists, valid, with a definition other than the expected one. The expected text is not
// written out here (Postgres prints its own canonical form, which differs between versions): the same definition is built on an empty TEMP table
// of the same columns, and its pg_get_indexdef is compared with the real index's, each with the index and table names removed.
func WrongDefinitions(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	wrong := map[string]bool{}
	canon := func(def string) string { return indexDefNorm.ReplaceAllString(def, "CREATE INDEX ") }
	for _, ix := range dbindex.Indexes {
		var have string
		err := conn.QueryRow(ctx, `SELECT pg_get_indexdef(c.oid) FROM pg_class c WHERE c.relname = $1 AND c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`, ix.Name).Scan(&have)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var want string
		_, err = tx.Exec(ctx, "CREATE TEMP TABLE _ix_expected (LIKE "+ix.Table+") ON COMMIT DROP")
		if err == nil {
			_, err = tx.Exec(ctx, "CREATE INDEX _ix_expected_i ON _ix_expected "+ix.Def)
		}
		if err == nil {
			err = tx.QueryRow(ctx, "SELECT pg_get_indexdef('_ix_expected_i'::regclass)").Scan(&want)
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return nil, fmt.Errorf("the expected definition of %s could not be built: %w", ix.Name, err)
		}
		if canon(have) != canon(want) {
			wrong[ix.Name] = true
		}
	}
	return wrong, nil
}
