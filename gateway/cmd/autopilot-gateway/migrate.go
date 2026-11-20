package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/dbindex"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
)

// migrateConfig is the environment of `autopilot-gateway migrate`: the writer's DSN, and two bounds.
type migrateConfig struct {
	dsn                      string
	lockTimeout, stmtTimeout time.Duration
}

func migrateConfigFromEnv(env func(string) string) (*migrateConfig, error) {
	if m := retiredSettings(env); m != "" {
		return nil, configError(m)
	}
	c := &migrateConfig{dsn: env("GATEWAY_DSN")}
	if c.dsn == "" {
		return nil, configError("GATEWAY_DSN is required (the writer's DSN: migrate creates indexes)")
	}
	lt, err := number(env, "MIGRATE_LOCK_TIMEOUT_SECONDS", 5)
	if err != nil {
		return nil, err
	}
	st, err := number(env, "MIGRATE_STATEMENT_TIMEOUT_SECONDS", 900)
	if err != nil {
		return nil, err
	}
	if !(lt >= 0.1 && lt <= 300) {
		return nil, configError("MIGRATE_LOCK_TIMEOUT_SECONDS is between 0.1 and 300")
	}
	if !(st >= 1 && st <= 7200) {
		return nil, configError("MIGRATE_STATEMENT_TIMEOUT_SECONDS is between 1 and 7200")
	}
	c.lockTimeout, c.stmtTimeout = time.Duration(lt*float64(time.Second)), time.Duration(st*float64(time.Second))
	return c, nil
}

// migrate is `autopilot-gateway migrate [--check]`: it brings the gateway database to what the Go gateway needs (the schema, and the seven
// read API indexes built with CREATE INDEX CONCURRENTLY, under a lock timeout and a statement timeout), and may run while a gateway is
// writing. It is idempotent and safe to re-run after a failure. `--check` changes nothing: it exits 1 when an index is missing or invalid.
// Exit codes: 0 done (or all in place), 1 an index is missing (--check) or the database failed it, 2 configuration.
func migrate(args []string) int {
	log, closeLogs := newLoggerWith("autopilot-migrate", os.Getenv, dsnSecrets(os.Getenv("GATEWAY_DSN")))
	defer closeLogs()
	slog.SetDefault(log)
	check := false
	for _, a := range args {
		if a != "--check" {
			log.Error("usage: autopilot-gateway migrate [--check]")
			return 2
		}
		check = true
	}
	cfg, err := migrateConfigFromEnv(os.Getenv)
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if check {
		return checkIndexes(ctx, cfg.dsn, log)
	}
	created, err := pgstate.Migrate(ctx, cfg.dsn, pgstate.MigrateOptions{LockTimeout: cfg.lockTimeout, StatementTimeout: cfg.stmtTimeout,
		Log: func(m string) { log.Info("migrate: " + m) }})
	if err != nil {
		if errors.Is(err, pgstate.ErrMigrateRunning) {
			log.Error("migrate: " + err.Error())
		} else {
			log.Error("migrate failed (it is safe to run again): " + err.Error())
		}
		return 1
	}
	log.Info("migrate: done", "built", created)
	return 0
}

func checkIndexes(ctx context.Context, dsn string, log *slog.Logger) int {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Error("migrate --check: " + err.Error())
		return 1
	}
	defer func() { _ = conn.Close(context.Background()) }()
	states, err := dbindex.Check(ctx, dbindex.Pgx(conn))
	if err != nil {
		log.Error("migrate --check: " + err.Error())
		return 1
	}
	if m := dbindex.Problems(states); m != "" {
		log.Error("migrate --check: " + m)
		return 1
	}
	wrong, err := pgstate.WrongDefinitions(ctx, conn)
	if err != nil {
		log.Error("migrate --check: " + err.Error())
		return 1
	}
	if len(wrong) > 0 {
		log.Error(fmt.Sprintf("migrate --check: indexes with another definition than the expected one: %v: run `autopilot-gateway migrate` (it rebuilds them beside the old ones)", wrong))
		return 1
	}
	log.Info(fmt.Sprintf("migrate --check: all %d indexes are in place", len(states)))
	return 0
}
