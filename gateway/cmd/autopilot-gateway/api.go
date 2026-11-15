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

// refusesEverything: a glob that matches every caller makes every caller an auditor or a notifier; it is a mistake, not a policy.
func catchAll(g string) bool { return strings.Trim(g, "*") == "" }

func apiConfigFromEnv(env func(string) string) (*apiConfig, error) {
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
		if catchAll(g) {
			return nil, configError("GATEWAY_API_AUDITORS names a glob that matches every caller (" + g + "): name who may read everything")
		}
	}
	for _, g := range c.notifiers {
		if catchAll(g) {
			return nil, configError("GATEWAY_API_NOTIFIERS names a glob that matches every caller (" + g + ")")
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
	log, closeLogs := newLogger("autopilot-api", os.Getenv)
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
