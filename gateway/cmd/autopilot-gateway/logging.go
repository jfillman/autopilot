package main

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/otlplogs"
)

// logSettings are what the log export reads: OTEL_EXPORTER_OTLP_LOGS_ENDPOINT (unset: off), OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES
// (otlplogs.ResourceAttributes) and, Go only, OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP (off by default: the endpoint is https).
type logSettings struct {
	endpoint  string
	allowHTTP bool
}

func logSettingsFromEnv(env func(string) string) (logSettings, error) {
	var s logSettings
	var err error
	if s.allowHTTP, err = strictFlag(env, "OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP", false); err != nil {
		return s, err
	}
	if s.endpoint = strings.TrimSpace(env("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")); s.endpoint != "" {
		if err := otlplogs.CheckEndpoint(s.endpoint, s.allowHTTP); err != nil {
			return s, configError(err.Error())
		}
	}
	return s, nil
}

// secretsFrom is the contents of credential files, re-read at most every 30 seconds, for the log redaction: a token that reached a log
// line as a literal is removed whatever the line says around it.
func secretsFrom(files ...string) func() []string {
	var mu sync.Mutex
	var at time.Time
	var cached []string
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < 30*time.Second {
			return cached
		}
		at, cached = time.Now(), nil
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil {
				if t := strings.TrimSpace(string(b)); len(t) >= 6 && len(t) < 16<<10 {
					cached = append(cached, t)
				}
			}
		}
		return cached
	}
}

// newLogger is the process's logger: JSON lines on stderr, and a copy of the INFO-and-above lines to the OTLP collector when one is
// configured. A collector setting that is wrong is logged and the export stays off: a log destination never stops the service.
func newLogger(service string, env func(string) string, secretFiles ...string) (*slog.Logger, func()) {
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelFromEnv()})
	stderr := slog.New(h)
	ls, err := logSettingsFromEnv(env)
	if err != nil {
		stderr.Error("log export is off: " + err.Error())
		return stderr, func() {}
	}
	if ls.endpoint == "" {
		return stderr, func() {}
	}
	x := otlplogs.New(otlplogs.Config{Endpoint: ls.endpoint, Resource: otlplogs.ResourceAttributes(env, service), Logger: loggerName(service),
		AllowHTTP: ls.allowHTTP, Secrets: secretsFrom(secretFiles...)})
	return slog.New(&otlplogs.Handler{Inner: h, X: x}), x.Close
}

// loggerName is the `logger` attribute Python sends (its logger's name): autopilot.api for the read API, autopilot.gateway for serve.
func loggerName(service string) string {
	return "autopilot." + strings.TrimPrefix(service, "autopilot-")
}
