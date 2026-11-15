package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func apiBase(extra map[string]string) map[string]string {
	m := map[string]string{"GATEWAY_API_DSN": "postgres://reader@pg/gateway"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestTheAPIRefusesWhatPythonRefuses(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "GATEWAY_API_DSN is required"},
		{apiBase(map[string]string{"GATEWAY_API_TABLES": "clearance"}), "GATEWAY_API_TABLES is retired"},
		{apiBase(map[string]string{"GATEWAY_API_TABLES": "evidence"}), "GATEWAY_API_TABLES"},
		{apiBase(map[string]string{"GATEWAY_API_POOL": "x"}), "numbers"},
		{apiBase(map[string]string{"GATEWAY_API_POOL": "99"}), "1 to 50"},
		{apiBase(map[string]string{"GATEWAY_API_POOL": "0"}), "1 to 50"},
		{apiBase(map[string]string{"CLEARANCE_GATEWAY_URL": "x"}), "retired settings"},
		{apiBase(map[string]string{"AUTOPILOT_DELEGATES": "*"}), "AUTOPILOT_DELEGATES"},
		{apiBase(map[string]string{"AUTOPILOT_DELEGATES": "*/*/*"}), "AUTOPILOT_DELEGATES"},
		{apiBase(map[string]string{"GATEWAY_API_AUDITORS": "*"}), "GATEWAY_API_AUDITORS"},
		{apiBase(map[string]string{"GATEWAY_API_NOTIFIERS": "user:alice,**"}), "GATEWAY_API_NOTIFIERS"},
		{apiBase(map[string]string{"BACKSTAGE_URL": "ftp://x"}), "BACKSTAGE_URL"},
		{apiBase(map[string]string{"AUTOPILOT_OIDC_ISSUER": "https://dex"}), "AUTOPILOT_OIDC"},
		{apiBase(map[string]string{"GATEWAY_API_DEFINITIONS_INTERVAL": "0"}), "INTERVAL"},
	} {
		if _, err := apiConfigFromEnv(envOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.env, err, c.want)
		}
	}
}

func TestTheAPIReadsPythonsSettings(t *testing.T) {
	c, err := apiConfigFromEnv(envOf(apiBase(map[string]string{"GATEWAY_API_TABLES": "gateway", "GATEWAY_API_AUDITORS": "user:jfillman", "GATEWAY_API_NOTIFIERS": "hangar-prod/monitoring/poller-*",
		"AUTOPILOT_DELEGATES": "hangar-prod/app-backstage-prod/backstage-ingestor", "BACKSTAGE_URL": "https://bs", "AUTOPILOT_GATEWAY_URL": "http://autopilot", "CLEARANCE_API_URL": "http://clearance-api",
		"AUTOPILOT_AGENTS_DIR": "/srv/app/agents", "RUNTIME_IMAGES_EXTRA": "ghcr.io/o/app-*", "PORT": "9000"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.port != 9000 || c.pool != 5 || c.auditors[0] != "user:jfillman" || c.notifiers[0] != "hangar-prod/monitoring/poller-*" || c.backstageURL != "https://bs" ||
		c.gatewayURL != "http://autopilot" || c.clearanceURL != "http://clearance-api" || c.runtimeCatalog != "/srv/app/runtime/catalog.yaml" || c.runtimeImages[0] != "ghcr.io/o/app-*" {
		t.Fatalf("%+v", c)
	}
	// The old pre-R-3c names configure nothing.
	c, err = apiConfigFromEnv(envOf(apiBase(map[string]string{"CLEARANCE_API_AUDITORS": "user:old", "CLEARANCE_API_NOTIFIERS": "ns/old"})))
	if err != nil || len(c.auditors) != 0 || len(c.notifiers) != 0 {
		t.Fatalf("%v %+v", err, c)
	}
}

// The read API is read-only: it does not call the writer's Open (which takes the advisory lock and creates the schema and indexes), it does
// not import the writer's package at all, and nothing in its package or in api*.go can write: no SQL statement but SELECT is anywhere in it.
// writePattern is a statement that can write, read after the string has been folded and its whitespace made single spaces.
const writePattern = `(?i)\b(insert\s+into|update\s+\w+\s+(?:\w+\s+)?set|delete\s+from|create\s+(unique\s+)?(table|index|schema|role)|alter\s+(table|role|database)|drop\s+(table|index|schema|database)|truncate\s|grant\s|revoke\s|copy\s+\w+\s+(from|to)|pg_advisory\w*|set\s+(session|local|role)|vacuum|lock\s+table)\b`

func TestTheAPIProcessCannotWrite(t *testing.T) {
	fset := token.NewFileSet()
	check := func(path string) {
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "internal/pgstate") {
				t.Errorf("%s imports the writer's package", path)
			}
		}
	}
	check("api.go")
	files, _ := filepath.Glob("../../internal/readapi/*.go")
	write := regexp.MustCompile(writePattern)
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		check(p)
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		consts := map[string]string{}
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, sp := range g.Specs {
					if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Names) == len(vs.Values) {
						for i, n := range vs.Names {
							if s, ok := fold(vs.Values[i], consts); ok {
								consts[n.Name] = s
							}
						}
					}
				}
			}
		}
		// every string, and every string folded through + (a statement split across literals, or a constant), is read as SQL would be
		ast.Inspect(f, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			switch e.(type) {
			case *ast.BasicLit, *ast.BinaryExpr:
				if s, ok := fold(e, consts); ok {
					if m := write.FindString(strings.Join(strings.Fields(strings.ReplaceAll(s, "\x00", " ")), " ")); m != "" {
						t.Errorf("%s: a statement that can write (%q): %s", p, m, s)
					}
				}
			}
			return true
		})
	}
	src, _ := os.ReadFile("api.go")
	if bytes.Contains(src, []byte("pgstate.")) {
		t.Fatal("api.go uses pgstate")
	}
}

func TestMetricsPort(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		port int
		err  string
	}{{nil, 9090, ""}, {map[string]string{"METRICS_PORT": "0"}, 0, ""}, {map[string]string{"METRICS_PORT": "9100"}, 9100, ""},
		{map[string]string{"METRICS_PORT": "x"}, 0, "METRICS_PORT must be a number, not 'x'"}, {map[string]string{"METRICS_PORT": "-1"}, 0, "must be positive"},
		{map[string]string{"METRICS_PORT": "70000"}, 0, "at most 65535"}, {map[string]string{"METRICS_PORT": "8080"}, 0, "must differ from PORT"},
		{map[string]string{"METRICS_PORT": "8081", "MODEL_KEY_FILE": "/k", "MODEL_ROUTES": "a=b"}, 0, "must differ from PORT and MODEL_PORT"}} {
		cfg, err := configFromEnv(envOf(base(c.env)))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: %v, want %q", c.env, err, c.err)
			}
			continue
		}
		if err != nil || cfg.metricsPort != c.port {
			t.Errorf("%v: %v %+v", c.env, err, cfg)
		}
	}
}

func TestLogExportSettings(t *testing.T) {
	for _, c := range []struct {
		env map[string]string
		err string
	}{{nil, ""}, {map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://otel:4318/v1/logs"}, ""},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://otel:4318/v1/logs"}, "must be an https URL"},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://otel:4318/v1/logs", "OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP": "1"}, ""},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "otel:4318"}, "http(s) URL"},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP": "maybe"}, "must be 0 or 1"}} {
		if _, err := logSettingsFromEnv(envOf(c.env)); (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%v: %v, want %q", c.env, err, c.err)
		}
	}
}

func TestTheLoggerAttributeIsPythons(t *testing.T) {
	if loggerName("autopilot-api") != "autopilot.api" || loggerName("autopilot-gateway") != "autopilot.gateway" {
		t.Fatal(loggerName("autopilot-api"), loggerName("autopilot-gateway"))
	}
}

// With the writer's DSN the api does not start (a superuser could switch the session's read-only default off), and says why.
func TestTheAPIDoesNotStartOnTheWritersRole(t *testing.T) {
	dsn := os.Getenv("AUTOPILOT_TEST_PG")
	if dsn == "" {
		t.Skip("no scratch Postgres (set AUTOPILOT_TEST_PG)")
	}
	cfg, err := apiConfigFromEnv(envOf(apiBase(map[string]string{"GATEWAY_API_DSN": dsn, "AUTOPILOT_AGENTS_DIR": "../../../agents"})))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if srv, _, err := buildAPI(context.Background(), cfg, log); err == nil || srv != nil || !strings.Contains(err.Error(), "read-only role") || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("%v %v", srv, err)
	}
}

// The shapes that would make everyone an auditor or a notifier are refused at start, whatever the setting; the named ones are accepted.
func TestAuditorAndNotifierGlobsCannotMatchEveryone(t *testing.T) {
	for _, g := range []string{"*", "**", "?", "?*", "user:", "user:*", "user:?*", "user:**", "user:[a-z]*", "user:a*", "user:a?", "user:a\\b", "*:*", "*:?", "user", "alice",
		"*/*", "*/*/*", "a/*/b", "*/ns/name", "cluster/*/name", "cluster/ns/*", "cluster/ns/?*", "cluster/ns/**", "ns/*", "ns/**", "ns/[a-z]*", "a/b/c/d", "a//b", "/a/b", "ns/name/"} {
		for _, setting := range []string{"GATEWAY_API_AUDITORS", "GATEWAY_API_NOTIFIERS"} {
			if err := checkReaderGlob(setting, g); err == nil {
				t.Errorf("%s accepted %q", setting, g)
			}
			if _, err := apiConfigFromEnv(envOf(apiBase(map[string]string{setting: g}))); err == nil {
				t.Errorf("the config accepted %s=%q", setting, g)
			}
		}
	}
	for _, g := range []string{"user:jfillman", "user:a@b.example", "ns/name", "ns/poller-*", "cluster/ns/name", "cluster/ns/poller-?", "hangar-prod/monitoring/*-poller"} {
		if err := checkReaderGlob("GATEWAY_API_AUDITORS", g); err != nil {
			t.Errorf("%q refused: %v", g, err)
		}
	}
}

// The process's own credentials never reach the collector: the token files it holds and its DSN, as literals, whatever the line says.
func TestTheLogExportScrubsTheProcessesOwnCredentials(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		prev, _ := got.Load().(string)
		got.Store(prev + string(b))
	}))
	defer srv.Close()
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("catalog-token-plainvalue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": srv.URL, "OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP": "1"})
	log, closeLogs := newLoggerWith("autopilot-api", env, dsnSecrets("postgres://reader:dsn-pw-plain@pg/gateway"), tokFile)
	log.Info("calling the catalog with catalog-token-plainvalue and connecting as reader:dsn-pw-plain via dsn-pw-plain", "agent", "triage-agent")
	closeLogs()
	body, _ := got.Load().(string)
	if body == "" || strings.Contains(body, "catalog-token-plainvalue") || strings.Contains(body, "dsn-pw-plain") || !strings.Contains(body, "triage-agent") {
		t.Fatalf("%s", body)
	}
}

// fold is the string an expression of literals and constants is, with a NUL for what is not constant (a variable joined in).
func fold(e ast.Expr, consts map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return fold(x.X, consts)
	case *ast.Ident:
		s, ok := consts[x.Name]
		return s, ok
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, lok := fold(x.X, consts)
		r, rok := fold(x.Y, consts)
		if !lok && !rok {
			return "", false
		}
		if !lok {
			l = "\x00"
		}
		if !rok {
			r = "\x00"
		}
		return l + r, true
	}
	return "", false
}

// The scan above is held to what it claims: planted in a package file, a statement split across literals or written in capitals is found.
func TestTheWriteScanFindsConcatenatedAndUpperCaseSQL(t *testing.T) {
	for _, src := range []string{`package p
const a = "DELETE "
const b = a + "FROM gateway_sessions"`, `package p
var q = "INSERT " + "INTO gateway_holds VALUES (1)"`, `package p
var q = "UPDATE gateway_sessions " + col + " SET closed = true"`, `package p
var q = "  uPdAtE   gateway_sessions\n set closed = true"`, `package p
var q = "dr" + "op table gateway_sessions"`} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		consts := map[string]string{}
		found := false
		write := regexp.MustCompile(writePattern)
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok {
				for _, sp := range g.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if s, ok := fold(vs.Values[i], consts); ok {
							if g.Tok == token.CONST {
								consts[n.Name] = s
							}
							found = found || write.MatchString(strings.Join(strings.Fields(strings.ReplaceAll(s, "\x00", " ")), " "))
						}
					}
				}
			}
		}
		if !found {
			t.Errorf("not found: %s", src)
		}
	}
}
