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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
)

// scratchDB makes a database of its own on AUTOPILOT_TEST_PG, migrated and opened by the writer (the schema and the seven indexes), and
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

// With the REAL binary: every role shape that could write is refused by `api` (exit 2, a read-only-role message naming the cause), and a
// role that is only SELECT starts. This is the end-to-end proof of readapi's CheckRole (TestTheAPIRefusesARoleThatCouldWrite) through the process.
func TestTheRealAPIRefusesEveryRoleShapeThatCouldWrite(t *testing.T) {
	dsn, _ := scratchDB(t)
	ctx := context.Background()
	w, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	u, _ := url.Parse(dsn)
	db := strings.TrimPrefix(u.Path, "/")
	sfx := fmt.Sprint(time.Now().UnixNano())
	bin := buildBinary(t)
	agents, _ := filepath.Abs(filepath.Join("..", "..", "..", "agents"))
	mk := func(name string, grants ...string) string {
		role := name + sfx
		for _, q := range append([]string{"CREATE ROLE " + role + " LOGIN PASSWORD 'pw'", "GRANT CONNECT ON DATABASE " + db + " TO " + role,
			"GRANT USAGE ON SCHEMA public TO " + role, "GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + role}, grants...) {
			if _, err := w.Exec(ctx, strings.ReplaceAll(q, "@", sfx)); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		c := *u
		c.User = url.UserPassword(role, "pw")
		return c.String()
	}
	run := func(dsn string) (int, string) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		cmd := exec.Command(bin, "api")
		cmd.Env = append(os.Environ(), "AUTOPILOT_AGENTS_DIR="+agents, "GATEWAY_API_DSN="+dsn, fmt.Sprintf("PORT=%d", port))
		var out bytes.Buffer
		cmd.Stderr = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), out.String()
			}
			return 0, out.String()
		case <-time.After(6 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return -1, out.String() // still running: it started
		}
	}
	cases := []struct{ name, dsn, want string }{
		{"inherit false set true writer", mk("sst", "CREATE ROLE wsr@ NOLOGIN", "GRANT INSERT, DELETE ON gateway_holds TO wsr@", "GRANT wsr@ TO sst@ WITH INHERIT FALSE, SET TRUE"), "gateway_holds"},
		{"insert on an auto-updatable view", mk("viw", "CREATE VIEW holds_v@ AS SELECT * FROM gateway_holds", "GRANT INSERT ON holds_v@ TO viw@"), "holds_v"},
		{"a column insert grant", mk("coi", "GRANT INSERT (agent, until, record) ON gateway_holds TO coi@"), "gateway_holds"},
		{"update on a sequence", mk("squ", "GRANT UPDATE ON SEQUENCE gateway_outbox_id_seq TO squ@"), "gateway_outbox_id_seq"},
		{"pg_write_all_data", mk("wad", "GRANT pg_write_all_data TO wad@"), "pg_write_all_data"},
		{"the writer's superuser", dsn, "superuser"},
	}
	for _, c := range cases {
		code, out := run(c.dsn)
		t.Logf("%s: exit %d", c.name, code)
		if code != 2 || !strings.Contains(out, "read-only role") || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want 2 naming %q:\n%s", c.name, code, c.want, out)
		}
	}
	if code, out := run(mk("ro")); code != -1 {
		t.Errorf("a SELECT-only role did not start: exit %d\n%s", code, out)
	}
	if code, out := run(mk("pgr", "GRANT pg_read_all_data TO pgr@")); code != -1 {
		t.Errorf("a pg_read_all_data role (the existing autopilot_reader shape) did not start: exit %d\n%s", code, out)
	}
}

var roleSQLStructure = []*regexp.Regexp{
	regexp.MustCompile(`^DO \$\$`), regexp.MustCompile(`^DECLARE t text`), regexp.MustCompile(`^BEGIN`),
	regexp.MustCompile(`^IF NOT EXISTS \(SELECT 1 FROM pg_roles WHERE rolname = 'gateway_api_reader'\) THEN`),
	regexp.MustCompile(`^FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = current_schema\(\) AND tablename LIKE 'gateway\\_%' LOOP`),
}

var roleSQLAllowed = []*regexp.Regexp{
	regexp.MustCompile(`^CREATE ROLE gateway_api_reader NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS$`),
	regexp.MustCompile(`^GRANT CONNECT ON DATABASE %I TO gateway_api_reader$`),
	regexp.MustCompile(`^GRANT USAGE ON SCHEMA %I TO gateway_api_reader$`),
	regexp.MustCompile(`^GRANT SELECT ON %I\.%I TO gateway_api_reader$`),
	// structure around them: the block, the existence test, the loop over gateway_* tables
	regexp.MustCompile(`^(END IF|END LOOP|END \$\$|END|\$\$)$`),
}

// The role file grants SELECT on the gateway tables and nothing else: every statement in it (the DO blocks' too, the EXECUTE format strings
// unwrapped) must match the allowlist, so adding an ALTER DEFAULT PRIVILEGES, an INSERT grant or a membership fails here.
func TestTheRoleFileHasOnlyTheStatementsItClaims(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "sql", "gateway-api-reader.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	exec := regexp.MustCompile(`^EXECUTE format\('([^']*)'.*\)$`)
	n := 0
	for _, st := range strings.Split(strings.Join(lines, " "), ";") {
		st = strings.Join(strings.Fields(st), " ")
		if st == "" {
			continue
		}
		// the block's structure in front of a statement: DO, DECLARE, BEGIN, the existence test, the loop over gateway_* tables
		for changed := true; changed; {
			changed = false
			for _, re := range roleSQLStructure {
				if loc := re.FindStringIndex(st); loc != nil && loc[0] == 0 && loc[1] < len(st)+1 && loc[1] > 0 {
					st, changed = strings.TrimSpace(st[loc[1]:]), true
				}
			}
		}
		if st == "" {
			continue
		}
		if m := exec.FindStringSubmatch(st); m != nil {
			st = m[1]
		}
		ok := false
		for _, re := range roleSQLAllowed {
			ok = ok || re.MatchString(st)
		}
		if !ok {
			t.Errorf("a statement the role file may not contain: %q", st)
		}
		n++
	}
	if n < 6 {
		t.Fatalf("only %d statements were read: the parser is not looking at the file", n)
	}
}

// Live: after the file is applied (twice), the group role has no default privileges, no membership, and no privilege other than SELECT on
// the gateway tables, CONNECT, and USAGE on the schema.
func TestTheRoleFileGrantsNothingButSelect(t *testing.T) {
	dsn, _ := scratchDB(t)
	ctx := context.Background()
	w, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	sqlText, _ := os.ReadFile(filepath.Join("..", "..", "sql", "gateway-api-reader.sql"))
	// roles are cluster-wide: another test's login role may be a member of the group already, so the file is held to adding none
	countMembers := func() int {
		var n int
		_ = w.QueryRow(ctx, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid IN (m.roleid, m.member) WHERE r.rolname = 'gateway_api_reader'`).Scan(&n)
		return n
	}
	before := countMembers()
	for i := 0; i < 2; i++ {
		if _, err := w.Exec(ctx, string(sqlText)); err != nil {
			t.Fatal(err)
		}
	}
	var nd int
	if err := w.QueryRow(ctx, `SELECT count(*) FROM pg_default_acl`).Scan(&nd); err != nil || nd != 0 {
		t.Fatalf("default privileges: %d %v", nd, err)
	}
	if after := countMembers(); after != before {
		t.Fatalf("the file changed memberships: %d -> %d", before, after)
	}
	var bad []string
	rows, err := w.Query(ctx, `SELECT c.relname, p FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace,
		unnest(ARRAY['INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p','v','m','f') AND has_table_privilege('gateway_api_reader', c.oid, p)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b string
		_ = rows.Scan(&a, &b)
		bad = append(bad, a+":"+b)
	}
	rows.Close()
	var seq, tables, selectable int
	_ = w.QueryRow(ctx, `SELECT count(*) FROM pg_class c WHERE c.relkind = 'S' AND has_sequence_privilege('gateway_api_reader', c.oid, 'USAGE') OR c.relkind = 'S' AND has_sequence_privilege('gateway_api_reader', c.oid, 'UPDATE')`).Scan(&seq)
	_ = w.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE has_table_privilege('gateway_api_reader', c.oid, 'SELECT')) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname LIKE 'gateway\_%'`).Scan(&tables, &selectable)
	var create bool
	_ = w.QueryRow(ctx, `SELECT has_schema_privilege('gateway_api_reader','public','CREATE') OR has_database_privilege('gateway_api_reader', current_database(), 'CREATE')`).Scan(&create)
	if len(bad) > 0 || seq != 0 || create || tables == 0 || tables != selectable {
		t.Fatalf("beyond SELECT: %v, sequences %d, create %v, tables %d selectable %d", bad, seq, create, tables, selectable)
	}
}

// Missing indexes: `api` and `serve` refuse to start by default and say what to run; GATEWAY_ALLOW_MISSING_INDEXES=1 starts them anyway.
func TestMissingIndexesRefuseTheStartUnlessAllowed(t *testing.T) {
	dsn, _ := scratchDB(t)
	ctx := context.Background()
	w, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	u, _ := url.Parse(dsn)
	db := strings.TrimPrefix(u.Path, "/")
	role := fmt.Sprintf("mi%d", time.Now().UnixNano())
	for _, q := range []string{"CREATE ROLE " + role + " LOGIN PASSWORD 'pw'", "GRANT CONNECT ON DATABASE " + db + " TO " + role, "GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + role, "DROP INDEX gateway_events_outcomes"} {
		if _, err := w.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	ro := *u
	ro.User = url.UserPassword(role, "pw")
	bin := buildBinary(t)
	agents, _ := filepath.Abs(filepath.Join("..", "..", "..", "agents"))
	run := func(sub string, extra ...string) (int, string) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		cmd := exec.Command(bin, sub)
		cmd.Env = append(os.Environ(), append([]string{"AUTOPILOT_AGENTS_DIR=" + agents, fmt.Sprintf("PORT=%d", port), "METRICS_PORT=0", "GATEWAY_API_DSN=" + ro.String(),
			"GATEWAY_DSN=" + dsn, "CLEARANCE_CORE_URL=http://127.0.0.1:9"}, extra...)...)
		var out bytes.Buffer
		cmd.Stderr = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), out.String()
			}
			return 0, out.String()
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return -1, out.String()
		}
	}
	for _, sub := range []string{"api", "serve"} {
		code, out := run(sub)
		if code == -1 || code == 0 || !strings.Contains(out, "gateway_events_outcomes (missing)") || !strings.Contains(out, "autopilot-gateway migrate") || !strings.Contains(out, "GATEWAY_ALLOW_MISSING_INDEXES=1") {
			t.Errorf("%s: exit %d, want a refusal that names the index, migrate and the override:\n%s", sub, code, out)
		}
		code, out = run(sub, "GATEWAY_ALLOW_MISSING_INDEXES=1")
		if code != -1 || !strings.Contains(out, "without indexes") && !strings.Contains(out, "lacks") {
			t.Errorf("%s with the override: exit %d, want it running and saying so:\n%s", sub, code, out)
		}
	}
}
