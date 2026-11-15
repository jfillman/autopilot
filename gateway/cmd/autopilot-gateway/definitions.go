package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// defsSettings are the settings of the agent definition set (A-9 S4): the Agent XR loader (Python: AGENT_XRS_ENABLED, AGENT_XRS_INTERVAL)
// and two the Go gateway adds, each stricter than Python.
//
//   - AGENT_XRS_ENABLED defaults to GITHUB_PR_ENABLED, as in Python, and needs it (the pinned files are read through the agents' App).
//   - AGENT_XRS_INTERVAL is seconds between polls: 60, at least 5.
//   - AGENT_XRS_NAMESPACES (Go only, no default): the namespaces whose Agent XRs the loader reads, one namespaced list each. Python lists
//     every namespace of the cluster; here the operator names the namespaces that hold reviewed Agent XRs (the tenants flow's), and an
//     XR anywhere else is never read. It is required when the loader is on, and it may not name a namespace an agent run or the platform
//     controls (agent-*, autopilot-runs, kube-*).
//   - AGENT_XRS_MAX_TIER (Go only, default T2, the schema's own highest): the highest tier ceiling a pinned definition may name.
//   - AGENT_XRS_NAMES (Go only): `name=namespace,...`, the agents whose names do not start with their app and the one namespace each is
//     pinned from. A pinned agent's name is its app's or starts with the app and a dash (decided from the XR alone, so two namespaces can
//     never contend for a name); an agent that was live before that rule is listed here. Cutover configuration: the live
//     `gate-reader` in `app-gate-api-cicd` is `gate-reader=app-gate-api-cicd`.
//   - AGENT_XRS_TOOLS (Go only): the tools a pinned definition may name, comma separated. The default is the read tools and the
//     propose-only lower ones (definitions.DefaultPinTools); setting it replaces the default, so promote, rollback, sync, apply, spawn
//     and the upper pull request tool are in a pin only when the platform lists them.
type defsSettings struct {
	enabled    bool
	interval   time.Duration
	namespaces []string
	maxTier    string
	tools      []string // nil: DefaultPinTools
	names      map[string]string
}

var nsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// maxXRNamespaces bounds how many namespaces one poll lists.
const maxXRNamespaces = 20

func defsFromEnv(env func(string) string, githubPRs bool) (defsSettings, error) {
	var d defsSettings
	var err error
	if d.enabled, err = flagEnv(env, "AGENT_XRS_ENABLED", githubPRs); err != nil {
		return d, err
	}
	if d.enabled && !githubPRs {
		return d, configError("AGENT_XRS_ENABLED needs GITHUB_PR_ENABLED=1 (pinned definitions are read through the agents' App)")
	}
	iv, err := number(env, "AGENT_XRS_INTERVAL", 60)
	if err != nil {
		return d, err
	}
	if iv < 5 {
		return d, configError("AGENT_XRS_INTERVAL is at least 5 seconds")
	}
	if iv > 86400 {
		return d, configError("AGENT_XRS_INTERVAL is at most a day")
	}
	d.interval = time.Duration(iv * float64(time.Second))
	d.maxTier = getenv(env, "AGENT_XRS_MAX_TIER", "T2")
	if d.maxTier != "T0" && d.maxTier != "T1" && d.maxTier != "T2" {
		return d, configError("AGENT_XRS_MAX_TIER is T0, T1 or T2")
	}
	d.tools = list(env("AGENT_XRS_TOOLS"))
	d.names = map[string]string{}
	for _, e := range list(env("AGENT_XRS_NAMES")) {
		n, ns, ok := strings.Cut(e, "=")
		if !ok || !nsName.MatchString(n) || !nsName.MatchString(ns) {
			return d, configError("AGENT_XRS_NAMES entries are name=namespace, not " + fmt.Sprintf("%q", e))
		}
		if _, dup := d.names[n]; dup {
			return d, configError("AGENT_XRS_NAMES lists " + n + " twice: a name belongs to one namespace")
		}
		d.names[n] = ns
	}
	for _, ns := range list(env("AGENT_XRS_NAMESPACES")) {
		switch {
		case !nsName.MatchString(ns):
			return d, configError("AGENT_XRS_NAMESPACES names " + fmt.Sprintf("%q", ns) + ", which is not a namespace name (no wildcards: each is listed on its own)")
		case strings.HasPrefix(ns, "agent-") || ns == "autopilot-runs" || strings.HasPrefix(ns, "kube-"):
			return d, configError("AGENT_XRS_NAMESPACES may not name " + ns + ": agent runs and the platform control it, and a definition decides what every agent may do")
		}
		d.namespaces = append(d.namespaces, ns)
	}
	if len(d.namespaces) > maxXRNamespaces {
		return d, configError(fmt.Sprintf("AGENT_XRS_NAMESPACES names more than %d namespaces", maxXRNamespaces))
	}
	for n, ns := range d.names {
		if !slices.Contains(d.namespaces, ns) {
			return d, configError("AGENT_XRS_NAMES puts " + n + " in " + ns + ", which AGENT_XRS_NAMESPACES does not name")
		}
	}
	if d.enabled && len(d.namespaces) == 0 {
		return d, configError("AGENT_XRS_ENABLED needs AGENT_XRS_NAMESPACES: the namespaces whose Agent XRs may define agents (the Python gateway reads every namespace; the Go gateway reads only the ones it is told to). " +
			"Set it, or set AGENT_XRS_ENABLED=0 to run with the bundled definitions only")
	}
	return d, nil
}

// policy is the pin policy this deployment holds every pin to (at load, on every reload and on stored rows): bound to its app by the
// namespace, the GitHub owners the gateway may use, the images the bundled definitions run and RUNTIME_IMAGES_EXTRA, and the tools above.
func (d defsSettings) policy(reg *tools.Registry, owners, extraImages []string, bundled map[string]*definitions.Definition) (definitions.PinPolicy, error) {
	allow := definitions.DefaultPinTools(reg)
	if d.tools != nil {
		allow = map[string]bool{}
		for _, t := range d.tools {
			if _, ok := reg.Get(t); !ok {
				return definitions.PinPolicy{}, configError("AGENT_XRS_TOOLS names " + fmt.Sprintf("%q", t) + ", which is not a tool of this gateway")
			}
			allow[t] = true
		}
	}
	for n := range d.names {
		if _, bundledName := bundled[n]; bundledName {
			return definitions.PinPolicy{}, configError("AGENT_XRS_NAMES lists " + n + ", which is a bundled definition's name")
		}
	}
	images := []string{}
	for _, g := range extraImages {
		images = append(images, g)
	}
	for _, def := range bundled {
		if def.Image != nil {
			images = append(images, *def.Image)
		}
		for _, sc := range def.Sidecars {
			images = append(images, sc["image"])
		}
	}
	return definitions.PinPolicy{Tools: reg, AllowTools: allow, MaxTier: d.maxTier, Owners: append([]string{}, owners...), ImageGlobs: images, BindApp: true, Names: d.names}, nil
}

// githubFiles is the pinned files' reader: repo.file.get's, through the agents' GitHub App.
type githubFiles struct{ g *backends.GitHub }

func (f githubFiles) ReadFile(ctx context.Context, repo, path, ref string) (map[string]any, error) {
	return f.g.ReadFile(ctx, repo, path, ref)
}

// rowsOf reads the stored rows (gateway_definitions' records) as rows.
func rowsOf(raw []json.RawMessage) []definitions.Row {
	var rows []definitions.Row
	for _, r := range raw {
		var row definitions.Row
		if json.Unmarshal(r, &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

// loadStored makes the stored cluster set the live one before anything is served, so a restart does not unload every pinned agent while
// the first poll is on its way: each row goes through the same decision a pin does (schema, strict reading, the pin rules, the policy).
// A row that no longer passes is a rejection, named in the log.
func loadStored(set *definitions.Set, rows []definitions.Row, reg *tools.Registry, policy definitions.PinPolicy, log *slog.Logger) {
	loaded, rejected := definitions.FromRecords(rows, set.Bundled(), reg, policy)
	set.ReplaceCluster(loaded, rejected)
	for _, r := range rejected {
		log.Warn("a stored pinned definition was not loaded", "agent", r.Name, "why", r.Reason)
	}
	log.Info("stored pinned definitions loaded", "loaded", len(loaded), "rejected", len(rejected))
}

// runDefinitions starts what keeps the definition set: the Agent XR loader, or, when it is off, the publish of the bundled set (until the
// core has taken it). It ends with ctx; wg is waited for at shutdown.
func runDefinitions(ctx context.Context, wg *sync.WaitGroup, cfg defsSettings, policy definitions.PinPolicy, mgr *definitions.Manager, set *definitions.Set, reader definitions.XRLister,
	files definitions.FileReader, reg *tools.Registry, stored []definitions.Row, log *slog.Logger) {
	if cfg.enabled {
		p := definitions.NewPoller()
		p.Namespaces, p.Lister, p.Files, p.Sink, p.Set, p.Baseline = cfg.namespaces, reader, files, mgr, set, reg
		p.Policy, p.Interval, p.Log = policy, cfg.interval, log
		p.Seed(stored)
		log.Info("agent pins on", "namespaces", cfg.namespaces, "every", cfg.interval.String(), "max_tier", cfg.maxTier)
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
		return
	}
	// No loader to publish on its polls: the bundled agents still reach the core (it must know every agent this gateway can run), at
	// start and again every 30 s until it takes them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if publishOnce(ctx, mgr, log) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
}

// publishOnce is one try at giving the core the bundled set; a panic is logged and the next tick tries again.
func publishOnce(ctx context.Context, mgr *definitions.Manager, log *slog.Logger) (done bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("bundled definitions: publish failed", "panic", fmt.Sprint(r))
			done = false
		}
	}()
	return mgr.Publish(ctx)
}

// candidateParser reads a candidate definition (Preflight's `document`, decoded from a request) like any definition: the schema, strictly.
// It is not held to the pin policy: a candidate is a pull request's version of a live agent, which may be a bundled one, and it is
// clamped before it runs.
func candidateParser(reg *tools.Registry) func(any) (*definitions.Definition, error) {
	return func(doc any) (*definitions.Definition, error) {
		om, ok := definitions.FromPlain(doc).(*definitions.OMap)
		if !ok {
			return nil, &definitions.DefinitionError{Msg: "?: <root>: the document is not an object"}
		}
		return definitions.ParseDoc(om, reg)
	}
}
