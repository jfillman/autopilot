package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	"github.com/jfillman/autopilot/gateway/internal/triggers"
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
	// The people's side (A-9 S3): who may act for a person (delegates) and who may manage every agent (admins), as globs; the
	// triggers (the event bridge on glidepath-api, the interval scheduler, the reconciliation sweep).
	delegates, admins    []string
	eventsEnabled        bool
	glidepathURL         string
	glidepathAllowHTTP   bool // GLIDEPATH_API_ALLOW_HTTP (Go only, default off): the URL may be plain http
	glidepathTokenFile   string
	schedulesEnabled     bool
	reconcileEvery       time.Duration // 0: the sweep is off
	reconcileWindowHours int
	runtimeImages        []string
	preflightCasesDir    string
	hangar               hangarSettings
	defs                 defsSettings // the definition set: the Agent XR loader (A-9 S4)
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
//
// A-9 S3 carries EVENTS_ENABLED (the event bridge) and with it GLIDEPATH_API_URL, which the bridge reads and nothing else of the Go
// gateway does yet: it is accepted only together with EVENTS_ENABLED (see configFromEnv). EVENTS_RABBITMQ_ENABLED is refused, as the
// Python gateway refuses it: the RabbitMQ source is designed and not built.
var unported = []string{"EVENTS_RABBITMQ_ENABLED", "AUTOPILOT_EPHEMERAL", "CLEARANCE_EPHEMERAL"}

// strictFlag is Python's _flag: 1/true/yes/on, 0/false/no/off, nothing is the default, anything else is an error.
func strictFlag(env func(string) string, k string, d bool) (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(env(k))); v {
	case "":
		return d, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, configError(fmt.Sprintf("%s must be 0 or 1, not %q", k, env(k)))
	}
}

// list is Python's _list: comma separated, trimmed, empties dropped.
func list(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func configFromEnv(env func(string) string) (*config, error) {
	var stale []string
	for _, k := range unported {
		if env(k) != "" {
			stale = append(stale, k)
		}
	}
	events, err := strictFlag(env, "EVENTS_ENABLED", false)
	if err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		return nil, configError(strings.Join(stale, ", ") + ": not carried by the Go gateway yet (docs/admin/go-gateway.md); unset them, or keep running the Python gateway")
	}
	if m := retiredSettings(env); m != "" {
		return nil, configError(m)
	}
	c := &config{}
	c.eventsEnabled = events
	if c.schedulesEnabled, err = strictFlag(env, "SCHEDULES_ENABLED", false); err != nil {
		return nil, err
	}
	// The reconciliation sweep: 0 turns it off, else at least 30 s; its window reaches back at most as far as glidepath-api's
	// sinceHours (744).
	rec := 300.0
	if strings.TrimSpace(env("RECONCILE_SECONDS")) == "0" {
		rec = 0
	} else if rec, err = number(env, "RECONCILE_SECONDS", 300); err != nil {
		return nil, err
	} else if rec <= 0 {
		return nil, configError("RECONCILE_SECONDS must be positive")
	}
	if rec != 0 && rec < 30 {
		return nil, configError("RECONCILE_SECONDS is at least 30 (or 0 to turn the sweep off)")
	}
	c.reconcileEvery = time.Duration(rec * float64(time.Second))
	win, err := number(env, "RECONCILE_WINDOW_HOURS", 24)
	if err != nil {
		return nil, err
	}
	if win <= 0 || win != float64(int(win)) {
		return nil, configError("RECONCILE_WINDOW_HOURS must be a positive whole number")
	}
	if win > float64(triggers.MaxWindowHours) {
		return nil, configError(fmt.Sprintf("RECONCILE_WINDOW_HOURS is at most %d: the trigger ledger keeps its rows %d hours, and a window that reached past them would launch a run whose row was pruned again (the Python gateway allows 744)",
			triggers.MaxWindowHours, int(triggers.Retention.Hours())))
	}
	c.reconcileWindowHours = int(win)
	c.delegates, c.admins = list(env("AUTOPILOT_DELEGATES")), list(env("AUTOPILOT_ADMINS"))
	for _, d := range c.delegates {
		// A delegate may name any person, so it is named by where it runs: a literal namespace (and cluster), and a name that may be a
		// pattern. `*`, a leading wildcard and a wildcard namespace are refused (the Python gateway takes any glob).
		if err := dispatch.ValidateDelegateGlob(d); err != nil {
			return nil, configError("AUTOPILOT_DELEGATES " + err.Error())
		}
	}
	c.runtimeImages = list(env("RUNTIME_IMAGES_EXTRA"))
	c.preflightCasesDir = env("PREFLIGHT_CASES_DIR")
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
	if c.hangar, err = hangarFromEnv(env, len(c.reads.argoNamespaces) > 0); err != nil {
		return nil, err
	}
	if c.defs, err = defsFromEnv(env, c.hangar.githubPRs); err != nil {
		return nil, err
	}
	if c.eventsEnabled {
		// The event bridge reads glidepath-api's stream with the same URL, token file and plain-http switch the glidepath.* tools use
		// (S5b parsed and checked them: an https URL unless GLIDEPATH_API_ALLOW_HTTP=1).
		if c.hangar.glidepathURL == "" {
			return nil, configError("EVENTS_ENABLED needs GLIDEPATH_API_URL (the events come from glidepath-api)")
		}
		c.glidepathURL, c.glidepathAllowHTTP, c.glidepathTokenFile = c.hangar.glidepathURL, c.hangar.glidepathAllowHTTP, c.hangar.glidepathTokenFile
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

	bundled, err := definitions.LoadDir(cfg.agentsDir, reg)
	if err != nil {
		log.Error("agent definitions: " + err.Error())
		return 2
	}
	set := definitions.NewSet(bundled)
	var stored []definitions.Row
	if rows, err := st.Definitions(ctx); err != nil {
		log.Warn("the stored pinned definitions could not be read: the loader starts from nothing", "err", err.Error())
	} else {
		stored = rowsOf(rows)
	}
	pinPolicy, err := cfg.defs.policy(reg, cfg.hangar.githubOwners, cfg.runtimeImages, bundled)
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	logNameOverrides(pinPolicy, log)
	switch {
	case cfg.defs.enabled:
		loadStored(set, stored, reg, pinPolicy, log)
	case len(stored) > 0:
		// Stored pins nothing keeps up to date are not run: the bundled definitions only (as the Python gateway with the loader off).
		log.Warn("the gateway database holds pinned definitions and the Agent XR loader is off (AGENT_XRS_ENABLED): they are not loaded", "rows", len(stored))
	}
	defs := set.Snapshot()
	artifacts := pgstate.NewArtifacts(st)
	// The Glidepath, repository and Argo CD sync tools decide a call's scope from the agent's definition; the gateway is built
	// after the handlers, so the lookup is made late.
	var gw *dispatch.Gateway
	if cfg.hangar.airframeNote != "" {
		log.Warn(cfg.hangar.airframeNote)
	}
	hangar, err := hangarConfig(cfg.hangar, cfg.reads.argoNamespaces, reg, func(agent string) *definitions.Definition { return gw.Def(agent) })
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	handlers, err := backends.NewHandlers(backends.Config{PrometheusURL: cfg.prometheusURL, LokiURL: cfg.lokiURL,
		BackstageURL: cfg.reads.backstageURL, BackstageTokenFile: cfg.reads.backstageTokenFile, AppAPIServices: cfg.reads.appAPIServices,
		ArgoNamespaces: cfg.reads.argoNamespaces, Artifacts: artifacts, Hangar: hangar})
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
	gw = g
	dcl, err := client.New(core.DefinitionsClientOptions(cfg.coreURL, client.FileToken(cfg.coreTokenFile)))
	if err != nil {
		log.Error("clearance client: " + err.Error())
		return 2
	}
	c.SetDefinitionsClient(dcl, clip64(gatewayVersion()))
	mgr := definitions.NewManager(set, st, c, g.SetDefs, log)
	mgr.Namespaces = len(cfg.defs.namespaces)
	// A candidate definition (Preflight's `document`) is read like any definition: the schema, strictly. It is not held to the pin
	// policy: it is a pull request's version of a live agent, which may be a bundled one, and it is clamped before it runs.
	g.CandidateParse = candidateParser(reg)
	g.CallTimeout = cfg.callTimeout
	g.KeepAnswer = map[string]bool{}
	for _, t := range backends.RemoteWrites {
		g.KeepAnswer[t] = true
	}
	// Runs are AgentRun XRs in the run namespace, as the gateway's own in-cluster identity (Python: KubeAgentRuns(), always);
	// a finished run's answer is kept in the gateway database; the pinned model version a run's records carry is the
	// alias's upstream model.
	g.SetRuns(&runs.Backend{Store: runs.NewKube()})
	g.Results = pgstate.NewResults(st)
	g.ModelVersions = cfg.modelRoutes
	// The people's side: the human inbox, the holds, the event history and Preflight. There is no Directory yet (the Backstage
	// client is a later slice), so a session's owner and the platform admins are who may act.
	g.SetInbox(pgstate.NewInbox(st), nil, cfg.delegates)
	g.Admins = cfg.admins
	g.Holds = pgstate.NewHolds(st)
	// The holds are read before anything is served or triggered. Until they have been, every open and launch fails closed (a hold that
	// cannot be known is not "no hold"), and each retries the read.
	if err := g.ReloadHolds(ctx); err != nil {
		log.Warn("the holds could not be read at start: triggers and people's runs wait until they can be", "err", err.Error())
	}
	g.Events = pgstate.NewEvents(st)
	g.RuntimeImages = cfg.runtimeImages
	g.Fingerprint = dispatch.Fingerprint
	g.PreflightLedger = &dispatch.PreflightLedger{}
	g.Preflight = g.PreflightLedger.Of
	casesDir := cfg.preflightCasesDir
	if casesDir == "" {
		casesDir = filepath.Join(filepath.Dir(filepath.Clean(cfg.agentsDir)), "preflight", "cases")
	}
	if fi, err := os.Stat(casesDir); err == nil && fi.IsDir() {
		if g.PreflightCases, err = dispatch.LoadCases(casesDir); err != nil {
			log.Error("preflight cases: " + err.Error())
			return 2
		}
	}
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
	defsWG := &sync.WaitGroup{}
	var reader definitions.XRLister
	var files definitions.FileReader
	if cfg.defs.enabled {
		kr := backends.NewKubeReader()
		if err := kr.Validate(); err != nil {
			log.Error("configuration: " + err.Error())
			return 2
		}
		reader, files = kr, githubFiles{hangar.GitHub}
	}
	runDefinitions(ctx, defsWG, cfg.defs, pinPolicy, mgr, set, reader, files, reg, stored, log)
	loops, err := startTriggers(ctx, cfg, g, pgstate.NewTriggers(st), log)
	if err != nil {
		log.Error("triggers: " + err.Error())
		return 2
	}
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
	stop() // the triggers end with the context; they are waited for below, bounded
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	waitFor(loops, 20*time.Second)
	waitFor(defsWG, 20*time.Second)
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

// startTriggers starts what the environment turns on: the interval scheduler (SCHEDULES_ENABLED), the event bridge (EVENTS_ENABLED) and
// the reconciliation sweep beside it. Each runs on goroutines of its own and ends with ctx. The trigger counters are not served yet
// (the Prometheus endpoint is a later slice): they are counted behind triggers.Metrics and dropped.
func startTriggers(ctx context.Context, cfg *config, g *dispatch.Gateway, ledger triggers.Ledger, log *slog.Logger) (*sync.WaitGroup, error) {
	wg := &sync.WaitGroup{}
	var m triggers.Metrics = triggers.Nop{}
	if cfg.schedulesEnabled {
		sc := triggers.NewScheduler(g, ledger, nil, m, log)
		wg.Add(1)
		go func() { defer wg.Done(); sc.Run(ctx) }()
	}
	if cfg.eventsEnabled {
		b, err := triggers.NewBridge(g, ledger, triggers.BridgeConfig{BaseURL: cfg.glidepathURL, TokenFile: cfg.glidepathTokenFile, AllowHTTP: cfg.glidepathAllowHTTP}, m, log)
		if err != nil {
			return wg, err
		}
		if plain, host := b.Plain(); plain {
			log.Warn("the event bridge talks plain http to glidepath-api (GLIDEPATH_API_ALLOW_HTTP): the gateway's bearer token crosses the network unencrypted", "host", host)
		}
		wg.Add(1)
		go func() { defer wg.Done(); b.Run(ctx) }()
		if cfg.reconcileEvery > 0 {
			rc := triggers.NewReconciler(b, cfg.reconcileEvery, cfg.reconcileWindowHours)
			wg.Add(1)
			go func() { defer wg.Done(); rc.Run(ctx) }()
		}
	}
	if os.Getenv("METRICS_PORT") != "" {
		log.Warn("METRICS_PORT is set: the Go gateway does not serve the trigger metrics yet (a later slice); they are counted and not exposed")
	}
	return wg, nil
}

// waitFor waits for the goroutines to end, for at most d.
func waitFor(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		slog.Warn("a trigger loop did not end in time")
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

func clip64(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}
