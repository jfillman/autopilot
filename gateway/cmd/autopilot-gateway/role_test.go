package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
)

// scratchDB makes a database of its own on AUTOPILOT_TEST_PG, migrated and opened by the writer (the schema and the six indexes), and
// returns the writer's DSN.
func scratchDB(t *testing.T) (dsn string, admin *pgx.Conn) {
	t.Helper()
	base := os.Getenv("AUTOPILOT_TEST_PG")
	if base == "" {
		t.Skip("no scratch Postgres (set AUTOPILOT_TEST_PG)")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("roletest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if _, err := pgstate.Migrate(ctx, u.String(), pgstate.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := pgstate.Open(ctx, u.String(), 2)
	if err != nil {
		t.Fatal(err)
	}
	st.Close(ctx)
	return u.String(), admin
}

// buildBinary is the real binary, built once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "autopilot-gateway")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// The role file in gateway/sql makes a role the api accepts, with the real binary: the api starts and answers /healthz and /readyz as that
// role, and refuses to start as the writer's role.
func TestTheReadOnlyRoleFileMakesARoleTheAPIStartsWith(t *testing.T) {
	dsn, _ := scratchDB(t)
	ctx := context.Background()
	w, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	sqlText, err := os.ReadFile(filepath.Join("..", "..", "sql", "gateway-api-reader.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // twice: idempotent
		if _, err := w.Exec(ctx, string(sqlText)); err != nil {
			t.Fatalf("the role file (run %d): %v", i+1, err)
		}
	}
	u, _ := url.Parse(dsn)
	suffix := fmt.Sprint(time.Now().UnixNano())
	login := "autopilot_reader_" + suffix
	if _, err := w.Exec(ctx, "CREATE ROLE "+login+" LOGIN PASSWORD 'pw' IN ROLE gateway_api_reader"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.Exec(ctx, "DROP ROLE IF EXISTS "+login) })
	ro := *u
	ro.User = url.UserPassword(login, "pw")

	bin := buildBinary(t)
	agents, _ := filepath.Abs(filepath.Join("..", "..", "..", "agents"))
	env := func(extra ...string) []string {
		return append(os.Environ(), append([]string{"AUTOPILOT_AGENTS_DIR=" + agents, "LOG_LEVEL=INFO"}, extra...)...)
	}

	freePort := func() int {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	// The writer's own DSN: the api refuses (exit 2, naming a read-only role).
	cmd := exec.Command(bin, "api")
	cmd.Env = env("GATEWAY_API_DSN="+dsn, fmt.Sprintf("PORT=%d", freePort()))
	var out bytes.Buffer
	cmd.Stderr = &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(out.String(), "read-only role") {
			t.Fatalf("the writer's DSN: %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the api started with the writer's role:\n%s", out.String())
	}

	// The role this file makes: the api starts and serves.
	port := freePort()
	cmd = exec.Command(bin, "api")
	cmd.Env = env("GATEWAY_API_DSN="+ro.String(), fmt.Sprintf("PORT=%d", port))
	out.Reset()
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// stop ends the process and returns what it logged: stderr is copied by a goroutine until Wait returns, so it is read only after it.
	stop := func() string {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return out.String()
	}
	ok := false
	for i := 0; i < 100 && !ok; i++ {
		time.Sleep(200 * time.Millisecond)
		r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
		if err == nil {
			ok = r.StatusCode == 200
			_ = r.Body.Close()
		}
	}
	logged := stop()
	if !ok {
		t.Fatalf("the api did not become ready as the read-only role:\n%s", logged)
	}
	if strings.Contains(logged, "lacks indexes") {
		t.Errorf("the migrated database was said to lack indexes:\n%s", logged)
	}
}

// The api says what to run when the indexes are missing, and still serves.
func TestTheAPISaysWhatToRunWhenAnIndexIsMissing(t *testing.T) {
	dsn, _ := scratchDB(t)
	ctx := context.Background()
	w, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	if _, err := w.Exec(ctx, "DROP INDEX gateway_events_outcomes"); err != nil {
		t.Fatal(err)
	}
	bin := buildBinary(t)
	// The superuser DSN is not a read-only role, so run the check through `migrate --check`, the same dbindex check serve and api use.
	cmd := exec.Command(bin, "migrate", "--check")
	cmd.Env = append(os.Environ(), "GATEWAY_DSN="+dsn)
	var out bytes.Buffer
	cmd.Stderr = &out
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 || !strings.Contains(out.String(), "gateway_events_outcomes (missing)") || !strings.Contains(out.String(), "autopilot-gateway migrate") {
		t.Fatalf("migrate --check: %v\n%s", err, out.String())
	}
	cmd = exec.Command(bin, "migrate")
	cmd.Env = append(os.Environ(), "GATEWAY_DSN="+dsn)
	out.Reset()
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out.String())
	}
	cmd = exec.Command(bin, "migrate", "--check")
	cmd.Env = append(os.Environ(), "GATEWAY_DSN="+dsn)
	out.Reset()
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("migrate --check after migrate: %v\n%s", err, out.String())
	}
}
