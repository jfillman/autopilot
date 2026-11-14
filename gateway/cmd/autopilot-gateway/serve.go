package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/mcpserver"
	"github.com/jfillman/autopilot/gateway/internal/modelproxy"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

const anthropicAPI = "https://api.anthropic.com"

// config is the environment of a Go gateway: the Python gateway's variable names, for the part this slice carries.
type config struct {
	port, modelPort         int
	coreURL, coreTokenFile  string
	dsn                     string
	agentsDir               string
	audience, modelAudience string
	runAudiences            []string
	coreTimeout             time.Duration
	resourceURL, issuerURL  string
	allowReads              bool
	expireEvery             time.Duration
	callTimeout             time.Duration
	prometheusURL, lokiURL  string
	modelRoutes             map[string]string
	modelKeyFile            string
	modelTimeout            time.Duration
	pgConns                 int32
	auth                    authConfig
	reads                   readSettings
}

type configError string

func (e configError) Error() string { return string(e) }

func getenv(env func(string) string, k, d string) string {
	if v := env(k); v != "" {
		return v
	}
	return d
}

func boolEnv(env func(string) string, k string, d bool) bool {
	v := strings.ToLower(strings.TrimSpace(env(k)))
	switch v {
	case "":
		return d
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return d
}

func number(env func(string) string, k string, d float64) (float64, error) {
	v := env(k)
	if v == "" {
		return d, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, configError(k + " is a number")
	}
	return f, nil
}

// unported are variables of the Python gateway whose features this slice does not carry. Starting with one set would
// mean running without something the operator asked for, so the gateway refuses (docs/admin/go-gateway.md).
var unported = []string{"GITHUB_PR_ENABLED", "AGENT_XRS_ENABLED", "EVENTS_ENABLED",
	"GLIDEPATH_API_URL", "ARGO_SYNC_ENABLED", "AUTOPILOT_EPHEMERAL", "CLEARANCE_EPHEMERAL"}

func configFromEnv(env func(string) string) (*config, error) {
	var stale []string
	for _, k := range unported {
		if env(k) != "" {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		return nil, configError(strings.Join(stale, ", ") + ": not carried by the Go gateway yet (docs/admin/go-gateway.md); unset them, or keep running the Python gateway")
	}
	if m := retiredSettings(env); m != "" {
		return nil, configError(m)
	}
	c := &config{}
	c.dsn = env("GATEWAY_DSN")
	if c.dsn == "" {
		return nil, configError("GATEWAY_DSN is required")
	}
	c.coreURL = env("CLEARANCE_CORE_URL")
	if c.coreURL == "" {
		return nil, configError("CLEARANCE_CORE_URL is required: the Clearance core decides every call")
	}
	if !strings.HasPrefix(c.coreURL, "http://") && !strings.HasPrefix(c.coreURL, "https://") {
		return nil, configError("CLEARANCE_CORE_URL is an http(s) URL")
	}
	c.coreTokenFile = getenv(env, "CLEARANCE_CORE_TOKEN_FILE", "/var/run/secrets/tokens/clearance")
	port, err := number(env, "PORT", 8080)
	if err != nil {
		return nil, err
	}
	mport, err := number(env, "MODEL_PORT", 8081)
	if err != nil {
		return nil, err
	}
	c.port, c.modelPort = int(port), int(mport)
	c.agentsDir = getenv(env, "AUTOPILOT_AGENTS_DIR", "/app/agents")
	// audience (AUTOPILOT_AUDIENCE) is the audience of Tower's foreign-cluster tokens and the core's, a wire contract
	// the foreign verifier of a later slice uses; the workload tokens this slice accepts are the run pods', which carry
	// runAudiences only (Python: build_auth). R-10b added `autopilot`; R-10e dropped the old `clearance` from the default.
	c.audience = getenv(env, "AUTOPILOT_AUDIENCE", "clearance")
	for _, a := range strings.Split(getenv(env, "AUTOPILOT_RUN_AUDIENCES", "autopilot"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.runAudiences = append(c.runAudiences, a)
		}
	}
	if len(c.runAudiences) == 0 {
		return nil, configError("AUTOPILOT_RUN_AUDIENCES names at least one audience")
	}
	ctmo, err := number(env, "CLEARANCE_CORE_TIMEOUT", core.DecisionTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	if !(ctmo >= 0.05 && ctmo <= 30) {
		return nil, configError("CLEARANCE_CORE_TIMEOUT is seconds per attempt, between 0.05 and 30")
	}
	c.coreTimeout = time.Duration(ctmo * float64(time.Second))
	c.modelAudience = getenv(env, "MODEL_TOKEN_AUDIENCE", "model-proxy")
	c.resourceURL = getenv(env, "AUTOPILOT_RESOURCE_URL", fmt.Sprintf("http://localhost:%d/mcp", c.port))
	c.issuerURL = env("AUTOPILOT_ISSUER_URL")
	if c.auth, err = authConfigFromEnv(env); err != nil {
		return nil, err
	}
	if c.issuerURL == "" { // the authorization server the metadata names: the OIDC issuer, else this server (as Python)
		c.issuerURL = c.resourceURL
		if c.auth.oidcIssuer != "" {
			c.issuerURL = c.auth.oidcIssuer
		}
	}
	c.allowReads = boolEnv(env, "GATEWAY_OUTAGE_ALLOW_READS", false)
	exp := 60.0
	if v := env("AUTOPILOT_EXPIRE_SECONDS"); v != "" {
		var err error
		if exp, err = number(func(string) string { return v }, "AUTOPILOT_EXPIRE_SECONDS", 60); err != nil {
			return nil, err
		}
	}
	c.expireEvery = time.Duration(exp * float64(time.Second))
	ct, err := number(env, "GATEWAY_CALL_TIMEOUT_SECONDS", 60)
	if err != nil {
		return nil, err
	}
	c.callTimeout = time.Duration(ct * float64(time.Second))
	c.prometheusURL, c.lokiURL = env("PROMETHEUS_URL"), env("LOKI_URL")
	if c.reads, err = readBackends(env); err != nil {
		return nil, err
	}
	c.modelRoutes = map[string]string{}
	for _, pair := range strings.Split(env("MODEL_ROUTES"), ",") {
		if pair = strings.TrimSpace(pair); pair != "" {
			alias, up, ok := strings.Cut(pair, "=")
			if !ok || strings.TrimSpace(alias) == "" || strings.TrimSpace(up) == "" {
				return nil, configError("MODEL_ROUTES entries are alias=upstream-model, not " + strconv.Quote(pair))
			}
			c.modelRoutes[strings.TrimSpace(alias)] = strings.TrimSpace(up)
		}
	}
	c.modelKeyFile = env("MODEL_KEY_FILE")
	if c.modelKeyFile != "" && len(c.modelRoutes) == 0 {
		return nil, configError("MODEL_KEY_FILE is set but MODEL_ROUTES names no model")
	}
	if c.modelKeyFile != "" && c.modelPort == c.port {
		return nil, configError("MODEL_PORT must differ from PORT")
	}
	mt, err := number(env, "MODEL_TIMEOUT_SECONDS", 300)
	if err != nil {
		return nil, err
	}
	c.modelTimeout = time.Duration(mt * float64(time.Second))
	pc, err := number(env, "GATEWAY_PG_CONNS", 16)
	if err != nil {
		return nil, err
	}
	c.pgConns = int32(pc)
	return c, nil
}

func serve(_ []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelFromEnv()}))
	slog.SetDefault(log)
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := tools.Default()
	st, err := pgstate.Open(ctx, cfg.dsn, cfg.pgConns)
	if err != nil {
		log.Error("gateway database: " + err.Error())
		return 1
	}
	defer st.Close(context.Background())
	store, err := session.NewStore(ctx, pgstate.NewSessions(ctx, st))
	if err != nil {
		log.Error("sessions: " + err.Error())
		return 1
	}
	cl, err := client.New(core.ClientOptions(cfg.coreURL, client.FileToken(cfg.coreTokenFile), cfg.coreTimeout))
	if err != nil {
		log.Error("clearance client: " + err.Error())
		return 2
	}
	c := core.New(cl, pgstate.NewOutbox(st), cfg.allowReads)

	defs, err := definitions.LoadDir(cfg.agentsDir, reg)
	if err != nil {
		log.Error("agent definitions: " + err.Error())
		return 2
	}
	if rows, err := st.Definitions(ctx); err == nil {
		cluster, skipped := definitions.FromRecords(rows, reg)
		for n, d := range cluster {
			defs[n] = d
		}
		for _, s := range skipped {
			log.Warn("a pinned definition was not loaded", "why", s)
		}
	}
	artifacts := pgstate.NewArtifacts(st)
	handlers, err := backends.NewHandlers(backends.Config{PrometheusURL: cfg.prometheusURL, LokiURL: cfg.lokiURL,
		BackstageURL: cfg.reads.backstageURL, BackstageTokenFile: cfg.reads.backstageTokenFile, AppAPIServices: cfg.reads.appAPIServices,
		ArgoNamespaces: cfg.reads.argoNamespaces, Artifacts: artifacts})
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	authn, prefetch, err := cfg.auth.build(cfg.audience, auth.NewWorkload(cfg.runAudiences[0], cfg.runAudiences[1:]...))
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	prefetch()
	g, err := dispatch.New(defs, store, c, authn, reg, handlers)
	if err != nil {
		log.Error(err.Error())
		return 2
	}
	g.CallTimeout = cfg.callTimeout
	// Runs are AgentRun XRs in the run namespace, as the gateway's own in-cluster identity (Python: KubeAgentRuns(), always);
	// a finished run's answer is kept in the gateway database; the pinned model version a run's records carry is the
	// alias's upstream model.
	g.SetRuns(&runs.Backend{Store: runs.NewKube()})
	g.Results = pgstate.NewResults(st)
	g.ModelVersions = cfg.modelRoutes
	// Crash-only: after a failed save the in-memory sessions are ahead of the database, and only a restart, which
	// reloads them, puts the two back in step.
	g.OnPersistError = func(err error) {
		log.Error("sessions not saved: exiting so a restart reloads them", "err", err.Error())
		stop()
		os.Exit(1)
	}

	servers := []*http.Server{{Addr: fmt.Sprintf(":%d", cfg.port), ReadHeaderTimeout: 10 * time.Second,
		Handler: mcpserver.Handler(g, "/mcp", cfg.resourceURL, cfg.issuerURL)}}
	if cfg.modelKeyFile != "" {
		routes := map[string]*modelproxy.Route{}
		for alias, up := range cfg.modelRoutes {
			routes[alias] = &modelproxy.Route{Alias: alias, Backend: "anthropic", Upstream: anthropicAPI, UpstreamModel: up}
		}
		m := &modelproxy.Models{Routes: routes, G: g, Auth: &auth.Cache{Inner: auth.NewWorkload(cfg.modelAudience), TTL: time.Minute}}
		servers = append(servers, &http.Server{Addr: fmt.Sprintf(":%d", cfg.modelPort), ReadHeaderTimeout: 10 * time.Second,
			Handler: modelproxy.NewProxy(m, cfg.modelKeyFile, cfg.modelTimeout).Handler()})
	}
	log.Info("autopilot-gateway (Go) starting", "version", gatewayVersion(), "tools", reg.Len(), "agents", len(defs), "port", cfg.port,
		"model_proxy", cfg.modelKeyFile != "")

	go sweep(ctx, g, cfg.expireEvery, artifacts.Purge)
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errc <- s.ListenAndServe() }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server: " + err.Error())
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return 0
}

// sweep closes sessions past their deadline, deletes their runs, and posts what the outbox holds once the core answers
// again (dispatch.Gateway.Expire). A panic in one sweep is logged, not fatal: the next one retries.
func sweep(ctx context.Context, g *dispatch.Gateway, every time.Duration, purge func(context.Context, time.Time, int, int) (int64, error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("the expiry sweep panicked", "panic", fmt.Sprint(r))
					}
				}()
				sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				if done := g.Expire(sctx); len(done) > 0 {
					slog.Info("expired sessions and deleted their runs", "n", len(done))
				}
				// Expired artifacts of every task, 500 rows at a time and at most 20 batches a sweep (a put cleans only its own task's).
				if n, err := purge(sctx, time.Now().UTC(), 500, 20); err != nil {
					slog.Warn("the artifact purge failed", "err", err.Error())
				} else if n > 0 {
					slog.Info("deleted expired artifacts", "n", n)
				}
			}()
		}
	}
}

func levelFromEnv() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warning", "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
