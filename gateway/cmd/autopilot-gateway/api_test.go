package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
		{apiBase(map[string]string{"GATEWAY_API_AUDITORS": "*"}), "every caller"},
		{apiBase(map[string]string{"GATEWAY_API_NOTIFIERS": "user:alice,**"}), "every caller"},
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
	c, err := apiConfigFromEnv(envOf(apiBase(map[string]string{"GATEWAY_API_TABLES": "gateway", "GATEWAY_API_AUDITORS": "user:jfillman", "GATEWAY_API_NOTIFIERS": "hangar-prod/*/backstage-ingestor",
		"AUTOPILOT_DELEGATES": "hangar-prod/app-backstage-prod/backstage-ingestor", "BACKSTAGE_URL": "http://bs", "AUTOPILOT_GATEWAY_URL": "http://autopilot", "CLEARANCE_API_URL": "http://clearance-api",
		"AUTOPILOT_AGENTS_DIR": "/srv/app/agents", "RUNTIME_IMAGES_EXTRA": "ghcr.io/o/app-*", "PORT": "9000"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.port != 9000 || c.pool != 5 || c.auditors[0] != "user:jfillman" || c.notifiers[0] != "hangar-prod/*/backstage-ingestor" || c.backstageURL != "http://bs" ||
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
	write := regexp.MustCompile(`(?i)\b(insert\s+into|update\s+\w+\s+set|delete\s+from|create\s+(unique\s+)?(table|index|schema|role)|alter\s+(table|role|database)|drop\s+(table|index|schema|database)|truncate\s|grant\s|revoke\s|copy\s+\w+\s+(from|to)|pg_advisory\w*|set\s+(session|local|role)|vacuum|lock\s+table)\b`)
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		check(p)
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if m := write.FindString(lit.Value); m != "" {
					t.Errorf("%s: a statement that can write (%q): %s", p, m, lit.Value)
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
