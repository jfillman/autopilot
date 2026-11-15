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
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/readapi"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// apiConfig is the environment of `autopilot-gateway api` (src/autopilot/api_serve.py): the Python read API's names.
type apiConfig struct {
	port                      int
	dsn                       string
	pool                      int32
	defsEvery                 time.Duration
	auditors, notifiers       []string
	delegates                 []string
	agentsDir, runtimeCatalog string
	gatewayURL, clearanceURL  string
	runtimeImages             []string
	backstageURL, tokenFile   string
	audience                  string
	runAudiences              []string
	auth                      authConfig
}

// checkReaderGlob is the rule for an auditor or notifier pattern (what may read everything, or the inbox feed of everyone): a glob
// that can match more than the one place or person named would make a caller who is neither an auditor. So:
//   - a person is `user:` and a literal name: no `*`, `?`, `[` or `\` anywhere (`user:*`, `user:?*`, `user:**`, `user:[a-z]*` are refused);
//   - a workload is `namespace/name` or `cluster/namespace/name`: every segment but the last is literal, and the last has at least one
//     literal character (it may be a pattern such as `poller-*`, never `*` or `?*`);
//   - anything else (`*`, `*:*`, a bare word) is neither, and is refused.
func checkReaderGlob(setting, g string) error {
	const wild = "*?[\\"
	bad := func(why string) error {
		return configError(setting + " names " + strconv.Quote(g) + ", which " + why + " (a person is user:<name> with no wildcard; a workload is [cluster/]namespace/name with a literal namespace and cluster and a name that has a literal character)")
	}
	if name, ok := strings.CutPrefix(g, "user:"); ok {
		if name == "" || strings.ContainsAny(name, wild) {
			return bad("could match more than one person")
		}
		return nil
	}
	parts := strings.Split(g, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return bad("is neither a person nor a workload")
	}
	for i, p := range parts {
		if p == "" {
			return bad("has an empty segment")
		}
		if i < len(parts)-1 && strings.ContainsAny(p, wild) {
			return bad("has a wildcard in the cluster or namespace")
		}
	}
	if last := parts[len(parts)-1]; strings.Trim(last, wild) == "" || strings.ContainsAny(last, "[\\") {
		return bad("could match every workload of the namespace")
	}
	return nil
}

func apiConfigFromEnv(env func(string) string) (*apiConfig, error) {
	if m := coreOnlySettings(env); m != "" {
		return nil, configError(m)
	}
	if m := retiredSettings(env); m != "" {
		return nil, configError(m)
	}
	c := &apiConfig{}
	if c.dsn = env("GATEWAY_API_DSN"); c.dsn == "" {
		return nil, configError("GATEWAY_API_DSN is required (the read-only role's DSN)")
	}
	// Retired at R-4: the read API reads only the gateway's own tables. `gateway` is accepted (the deployed value) until apron drops it.
	if t := getenv(env, "GATEWAY_API_TABLES", "gateway"); t != "gateway" {
		return nil, configError(fmt.Sprintf("GATEWAY_API_TABLES is retired (the read API reads only gateway_*): unset it, not %q", t))
	}
	pool, err := strconv.Atoi(getenv(env, "GATEWAY_API_POOL", "5"))
	if err != nil {
		return nil, configError("GATEWAY_API_POOL and GATEWAY_API_DEFINITIONS_INTERVAL are numbers")
	}
	every, err := strconv.ParseFloat(getenv(env, "GATEWAY_API_DEFINITIONS_INTERVAL", "30"), 64)
	if err != nil {
		return nil, configError("GATEWAY_API_POOL and GATEWAY_API_DEFINITIONS_INTERVAL are numbers")
	}
	if pool < 1 || pool > 50 {
		return nil, configError("GATEWAY_API_POOL is 1 to 50")
	}
	if !(every >= 1 && every <= 3600) {
		return nil, configError("GATEWAY_API_DEFINITIONS_INTERVAL is 1 to 3600 seconds")
	}
	c.pool, c.defsEvery = int32(pool), time.Duration(every*float64(time.Second))
	port, err := number(env, "PORT", 8080)
	if err != nil || port < 1 || port > 65535 || port != float64(int(port)) {
		return nil, configError("PORT is a port number")
	}
	c.port = int(port)
	c.auditors, c.notifiers, c.delegates = list(env("GATEWAY_API_AUDITORS")), list(env("GATEWAY_API_NOTIFIERS")), list(env("AUTOPILOT_DELEGATES"))
	for _, g := range c.auditors {
		if err := checkReaderGlob("GATEWAY_API_AUDITORS", g); err != nil {
			return nil, err
		}
	}
	for _, g := range c.notifiers {
		if err := checkReaderGlob("GATEWAY_API_NOTIFIERS", g); err != nil {
			return nil, err
		}
	}
	for _, d := range c.delegates {
		if err := dispatch.ValidateDelegateGlob(d); err != nil {
			return nil, configError("AUTOPILOT_DELEGATES " + err.Error())
		}
	}
	c.agentsDir = getenv(env, "AUTOPILOT_AGENTS_DIR", "/app/agents")
	c.runtimeCatalog = filepath.Join(filepath.Dir(filepath.Clean(c.agentsDir)), "runtime", "catalog.yaml")
	c.gatewayURL, c.clearanceURL = env("AUTOPILOT_GATEWAY_URL"), env("CLEARANCE_API_URL")
	c.runtimeImages = list(env("RUNTIME_IMAGES_EXTRA"))
	c.audience = getenv(env, "AUTOPILOT_AUDIENCE", "clearance")
	for _, a := range strings.Split(getenv(env, "AUTOPILOT_RUN_AUDIENCES", "autopilot"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.runAudiences = append(c.runAudiences, a)
		}
	}
	if len(c.runAudiences) == 0 {
		return nil, configError("AUTOPILOT_RUN_AUDIENCES names at least one audience")
	}
	if c.auth, err = authConfigFromEnv(env); err != nil {
		return nil, err
	}
	r, err := readBackends(env)
	if err != nil {
		return nil, err
	}
	c.backstageURL, c.tokenFile = r.backstageURL, r.backstageTokenFile
	return c, nil
}

// api is `autopilot-gateway api`: the read-only process that serves the runtime reads from the gateway database (internal/readapi).
// It opens the database through the read API's own store: no writer lock, no schema, no statement but SELECT.
func api(_ []string) int {
	log, closeLogs := newLoggerWith("autopilot-api", os.Getenv, dsnSecrets(os.Getenv("GATEWAY_API_DSN")),
		getenv(os.Getenv, "BACKSTAGE_TOKEN_FILE", backends.BackstageTokenFileDefault))
	defer closeLogs()
	slog.SetDefault(log)
	cfg, err := apiConfigFromEnv(os.Getenv)
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv, wg, err := buildAPI(ctx, cfg, log)
	if err != nil {
		log.Error("configuration: " + err.Error())
		return 2
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server: " + err.Error())
		}
	}
	stop()
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	waitFor(wg, 10*time.Second)
	return 0
}

// buildAPI wires the read API (the rest of api() is the process around it) and starts the definitions refresher on wg.
func buildAPI(ctx context.Context, cfg *apiConfig, log *slog.Logger) (*http.Server, *sync.WaitGroup, error) {
	reg := tools.Default()
	bundled, err := definitions.LoadDir(cfg.agentsDir, reg)
	if err != nil {
		return nil, nil, fmt.Errorf("agent definitions: %w", err)
	}
	runtimes, err := readapi.LoadRuntimes(cfg.runtimeCatalog)
	if err != nil {
		return nil, nil, err
	}
	set := definitions.NewSet(bundled)
	authn, prefetch, err := cfg.auth.build(cfg.audience, auth.NewWorkload(cfg.runAudiences[0], cfg.runAudiences[1:]...))
	if err != nil {
		return nil, nil, err
	}
	prefetch()
	access := &readapi.Access{Auth: authn, Delegates: cfg.delegates, Notifiers: cfg.notifiers, Auditors: cfg.auditors, Defs: set}
	if cfg.backstageURL != "" {
		b := readapi.NewBackstage(cfg.backstageURL, cfg.tokenFile)
		access.Directory, access.Ownership = b, b
	}
	store, err := readapi.OpenPg(ctx, cfg.dsn, cfg.pool)
	if err != nil {
		return nil, nil, err
	}
	// The role is checked now, before anything is served, not when the first request or the readiness probe happens to reach the database.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := store.CheckRole(cctx); err != nil {
		store.Close()
		return nil, nil, err
	}
	// The six indexes are `migrate`'s: without them the lists scan their tables. Refused by default (a silent slow path in production is what
	// migrate exists to prevent); GATEWAY_ALLOW_MISSING_INDEXES=1 serves anyway, loudly.
	if m, err := store.IndexProblems(cctx); err != nil {
		// a check that cannot be made is not a pass
		if !boolEnv(os.Getenv, "GATEWAY_ALLOW_MISSING_INDEXES", false) {
			store.Close()
			return nil, nil, fmt.Errorf("the read API's indexes could not be checked: %w (GATEWAY_ALLOW_MISSING_INDEXES=1 serves without the check)", err)
		}
		log.Error("serving without the index check (GATEWAY_ALLOW_MISSING_INDEXES=1): " + err.Error())
	} else if m != "" {
		if !boolEnv(os.Getenv, "GATEWAY_ALLOW_MISSING_INDEXES", false) {
			store.Close()
			return nil, nil, fmt.Errorf("the gateway database lacks indexes the read API needs: %s (GATEWAY_ALLOW_MISSING_INDEXES=1 serves without them)", m)
		}
		log.Error("serving without indexes the read API needs (GATEWAY_ALLOW_MISSING_INDEXES=1): " + m)
	}
	a, err := readapi.New(&readapi.API{Access: access, Store: store, Defs: set, Runtimes: runtimes, CustomImages: cfg.runtimeImages,
		GatewayURL: cfg.gatewayURL, ClearanceURL: cfg.clearanceURL})
	if err != nil {
		return nil, nil, err
	}
	wg := &sync.WaitGroup{}
	refresh := &readapi.Refresher{Set: set, Store: store, Reg: reg, Every: cfg.defsEvery, Log: log}
	refresh.Once(ctx)
	wg.Add(1)
	go func() { defer wg.Done(); refresh.Run(ctx) }()
	log.Info("autopilot-api (Go) starting", "version", gatewayVersion(), "port", cfg.port, "agents", len(set.Snapshot()), "auditors", cfg.auditors,
		"notifiers", cfg.notifiers, "delegates", cfg.delegates, "catalog", cfg.backstageURL != "")
	return &http.Server{Addr: fmt.Sprintf(":%d", cfg.port), Handler: a, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}, wg, nil
}
