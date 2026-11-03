//go:build ignore

// Autopilot's test core: `clearance autopilot-testcore`, the Clearance core (internal/core, exactly as
// `clearance core --memory --insecure-dev-auth` builds it) with three test-only differences. It is compiled into a
// clearance checkout with `go build -overlay` (tests/clearance_core/build.py) and never ships.
//
//   - The evaluation clock-skew check is off (MaxClockSkew < 0): autopilot's tests run on a fixed clock
//     (2026-09-26), and `clearance core` has no setting for it (a needed core change, listed in the PR).
//   - GET /testcore/v1/records?from=N&limit=M returns the in-memory chain, so the tests read the records the core
//     wrote (the core has no read route; clearance-api is the reader, R-2).
//   - AUTOPILOT_TESTCORE_EXTRA_KINDS (kind=required,fields;kind2=...) registers POST /v1/records kinds the core
//     does not have yet, to prove the gateway's held-record replay once a kind exists.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jfillman/clearance/internal/cli"
	"github.com/jfillman/clearance/internal/core"
	"github.com/jfillman/clearance/internal/server"
)

func init() {
	cli.Register(cli.Command{Name: "autopilot-testcore", Summary: "autopilot's test core (tests only)", Run: testCore})
}

func testCore(ctx context.Context, args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("autopilot-testcore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	for _, spec := range strings.Split(os.Getenv("AUTOPILOT_TESTCORE_EXTRA_KINDS"), ";") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		kind, req, _ := strings.Cut(spec, "=")
		server.AutopilotTestKind(kind, strings.Split(req, ","))
	}
	getenv := func(k string) string {
		for _, a := range devSkippedEnv {
			if k == a {
				return ""
			}
		}
		return os.Getenv(k)
	}
	cfg, err := core.FromEnv(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "testcore:", err)
		return 2
	}
	cfg.Memory, cfg.Auth, cfg.MaxClockSkew, cfg.Version = true, core.DevAuthenticator(), -1, "testcore"
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := core.Build(ctx, cfg, nil, log)
	if err != nil {
		fmt.Fprintln(stderr, "testcore:", err)
		return 1
	}
	defer c.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /testcore/v1/records", func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		recs, err := c.Store.Records(r.Context(), from, 100000)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(recs)
	})
	mux.Handle("/", c.Server.Handler())
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "testcore:", err)
		return 1
	}
	// The address on stdout, one line: the test fixture reads it.
	fmt.Println(ln.Addr().String())
	hs := c.Server.HTTPServer(*listen)
	hs.Handler = mux
	go func() { <-ctx.Done(); _ = hs.Close() }()
	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(stderr, "testcore:", err)
		return 1
	}
	return 0
}
