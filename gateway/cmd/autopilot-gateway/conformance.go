package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/conformance"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// conformanceControl is `autopilot-gateway conformance-control [--listen 127.0.0.1:9091]`, with
// GATEWAY_CONFORMANCE_CONTROL=1. Test only, off by default.
func conformanceControl(args []string) int {
	fs := flag.NewFlagSet("conformance-control", flag.ContinueOnError)
	listen := fs.String("listen", conformance.DefaultListen, "loopback host:port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if os.Getenv(conformance.EnableEnv) != "1" {
		log.Error("the conformance control interface is test-only and off: set " + conformance.EnableEnv + "=1 to serve it (never in a deployment)")
		return 2
	}
	addr, err := conformance.ListenAddress(*listen)
	if err != nil {
		log.Error(err.Error())
		return 2
	}
	log.Warn("TEST ONLY: serving the conformance control interface", "addr", addr, "provider", core.Provider, "version", gatewayVersion())
	srv := &http.Server{Addr: addr, Handler: conformance.NewControl(gatewayVersion(), tools.Default()).Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error(err.Error())
		return 1
	}
	_ = context.Background
	return 0
}
