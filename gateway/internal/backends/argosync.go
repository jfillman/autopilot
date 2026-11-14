package backends

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// argo.sync.lower (argo_sync.ArgoSync): start a sync of one lower-environment Argo CD Application, as the gateway's own
// identity. Argo CD starts a sync when an Application's `operation` field is set (what the argocd CLI and UI do through the
// Argo CD API), so no Argo CD account or token is needed: one merge PATCH through the Kubernetes API, and nothing else is
// ever sent:
//
//	PATCH /apis/argoproj.io/v1alpha1/namespaces/<ns>/applications/<name>   application/merge-patch+json
//
// The core decided the call before this runs: the tool's tier, the definition's tools, and R012, which reads "lower" from the
// `project` argument (a project named <app>-lower). The agent names that project, so this adapter checks the Application it
// would patch: it must be in one of the configured namespaces, its spec.project must be exactly the argument and end in
// -lower, and no operation may be in flight. The patch carries the resourceVersion that was read, so an Application changed
// in between (moved to another project) is a 409, not a sync. The body sets `operation` and the resourceVersion only: a sync
// of the Application's own target revision, with its own sync options.
//
// Three limits, outer to inner: RBAC (a Role that may get and patch Applications), admission (a ValidatingAdmissionPolicy
// that lets this identity change nothing but `operation`, in a -lower project), and these checks, before any request exists.

// LowerSuffix is what a lower-environment project's name ends in.
const LowerSuffix = "-lower"

// ArgoSyncError is the API server refusing or failing the patch. The agent is told only "backend error", unless the sync may
// have been requested (AgentMessage).
type ArgoSyncError struct {
	Msg   string
	agent string
}

func (e *ArgoSyncError) Error() string { return e.Msg }
func (e *ArgoSyncError) Kind() string  { return "ArgoSyncError" }

// Evidence is the status and the Application, not the API server's body (it can echo the patch): the text before the first
// ": ", at most 160 characters.
func (e *ArgoSyncError) Evidence() string {
	head, _, _ := strings.Cut(e.Msg, ": ")
	return dispatch.ClipEvidence(head)
}

// AgentMessage is empty unless the request was sent and not answered.
func (e *ArgoSyncError) AgentMessage() string { return e.agent }

// ArgoSync is the argo.sync.lower backend.
type ArgoSync struct {
	Kube       *KubeReader
	Namespaces []string
}

// NewArgoSync checks the namespaces (DNS labels: they go into a path) and builds the backend.
func NewArgoSync(kube *KubeReader, namespaces []string) (*ArgoSync, error) {
	if len(namespaces) == 0 {
		return nil, errors.New("argo.sync.lower needs at least one Application namespace (ARGO_APP_NAMESPACES)")
	}
	for _, n := range namespaces {
		if !nsPattern.MatchString(n) {
			return nil, fmt.Errorf("ARGO_APP_NAMESPACES: %s is not a namespace name (a DNS label)", pyStrRepr(n))
		}
	}
	if kube == nil {
		kube = NewKubeReader()
	}
	return &ArgoSync{Kube: kube, Namespaces: namespaces}, nil
}

// patchAllowed is the whole allowlist of the PATCH: one Application, by name, in one namespace.
func patchPath(ns, name string) (string, bool) {
	p := ApplicationsPath(ns, name)
	return p, name != "" && nsPattern.MatchString(ns) && segPattern.MatchString(name) && pathAllowed(p)
}

func operationInfo(o any) []any { return anyList(obj(obj(o)["operation"])["info"]) }

// ownsOperation says whether an operation in flight carries this session's marker: the operation this gateway asked for.
func ownsOperation(app map[string]any, session string) bool {
	if session == "" {
		return false
	}
	infos := operationInfo(app)
	infos = append(infos, operationInfo(obj(obj(app["status"])["operationState"]))...)
	for _, i := range infos {
		if m := obj(i); m["name"] == "session" && m["value"] == session {
			return true
		}
	}
	return false
}

func infoText(v string) string {
	return clipRunes(cleanMeta(v), 256)
}

// Sync is ArgoSync.sync. who names the agent, task, session, run and principal: Argo CD's own history shows who asked.
func (s *ArgoSync) Sync(ctx context.Context, app, project any, who map[string]string) (any, error) {
	proj, isStr := project.(string)
	if !isStr || !strings.HasSuffix(proj, LowerSuffix) {
		return nil, &core.Denied{Rule: "ARGO-PROJECT", Message: echo(project) + " is not a lower-environment project",
			Hint: "argo.sync.lower syncs Applications in an <app>-lower project only."}
	}
	name, isStr := app.(string)
	if !isStr || !segPattern.MatchString(name) {
		return nil, &core.Denied{Rule: "READ-ARGO-NAME", Message: echo(app) + " is not an Application name",
			Hint: "Pass the Argo CD Application's name, e.g. flight-api-dev."}
	}
	var found map[string]any
	ns := ""
	for _, n := range s.Namespaces {
		o, ok, err := s.Kube.Get(ctx, ApplicationsPath(n, name), nil)
		if err != nil {
			return nil, err
		}
		if ok {
			found, ns = obj(o), n
			break
		}
	}
	if found == nil {
		return map[string]any{"app": name, "found": false, "searched": stringsToAny(s.Namespaces)}, nil
	}
	actual := obj(found["spec"])["project"]
	if actual != proj {
		return nil, &core.Denied{Rule: "ARGO-PROJECT", Message: fmt.Sprintf("%s is in project %s, not %s", name, pyRepr(actual), pyStrRepr(proj)),
			Hint: "Pass the Application's own project (argo.app.get shows it); only -lower projects sync."}
	}
	state := obj(obj(found["status"])["operationState"])
	if pyTruthy(found["operation"]) || state["phase"] == "Running" {
		if ownsOperation(found, who["session"]) {
			// The operation in flight is this session's own: a call that asked for it and lost the answer. It is the sync
			// that was asked for, not someone else's.
			return syncAnswer(name, ns, proj, true), nil
		}
		return nil, &core.Denied{Rule: "ARGO-BUSY", Message: name + " already has an operation in progress",
			Hint: "Wait for it to finish (argo.app.get), then sync again if it is still needed."}
	}
	rv, _ := obj(found["metadata"])["resourceVersion"].(string)
	if rv == "" {
		return nil, &ArgoSyncError{Msg: "Application " + name + " has no resourceVersion: " + "the sync would not be conditional"}
	}
	agent := who["agent"]
	if agent == "" {
		agent = "agent"
	}
	var keys []string
	for k := range who {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	info := []any{}
	for _, k := range keys {
		if k != "agent" && who[k] != "" {
			info = append(info, map[string]any{"name": k, "value": infoText(who[k])})
		}
	}
	operation := map[string]any{"initiatedBy": map[string]any{"username": "clearance:" + infoText(agent)}, "info": info, "sync": map[string]any{}}
	status, err := s.patch(ctx, ns, name, map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "operation": operation})
	if err != nil {
		return nil, err
	}
	switch {
	case status == 200:
		return syncAnswer(name, ns, proj, false), nil
	case status == 409:
		return nil, &core.Denied{Rule: "ARGO-CHANGED", Message: name + " changed while the sync was being requested", Hint: "Read it again with argo.app.get and retry."}
	case status == 401 || status == 403:
		return nil, &core.Denied{Rule: "ARGO-K8S-403", Message: fmt.Sprintf("the API server refused to sync %s (%d)", name, status),
			Hint: "Clearance may sync lower-environment Applications only; ask a human to check its grant."}
	}
	return nil, &ArgoSyncError{Msg: fmt.Sprintf("API server %d patching %s", status, name)}
}

func syncAnswer(name, ns, proj string, replayed bool) map[string]any {
	out := map[string]any{"app": name, "namespace": ns, "project": proj, "requested": true,
		"hint": "Argo CD is syncing it; argo.app.get shows the operation's phase and the app's health."}
	if replayed {
		out["replayed"] = true
	}
	return out
}

func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// patch is the one PATCH: a merge patch of one Application, as the gateway's own identity, the token read now. It is not
// retried: a patch that was sent and not answered may have been applied.
func (s *ArgoSync) patch(ctx context.Context, ns, name string, body map[string]any) (int, error) {
	path, ok := patchPath(ns, name)
	if !ok {
		return 0, refusedPath(path)
	}
	tok, err := readToken(s.Kube.TokenFile)
	if err != nil {
		return 0, err
	}
	if strings.ContainsAny(tok, "\r\n") {
		return 0, headerError()
	}
	hc, err := s.Kube.httpClient()
	if err != nil {
		return 0, err
	}
	b, err := jsonBytes(body)
	if err != nil {
		return 0, err
	}
	full := strings.TrimRight(s.Kube.Server, "/") + path
	if u, perr := url.Parse(full); perr != nil || u.RawQuery != "" {
		return 0, refusedPath(path)
	}
	timeout := s.Kube.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hdr := http.Header{"Authorization": {"Bearer " + tok}, "Accept": {"application/json"}, "Content-Type": {"application/merge-patch+json"}}
	rp, err := send(ctx, hc, s.Kube.limiter(), timeout, http.MethodPatch, full, hdr, b, maxKubeBody, maxGPErr)
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			e := &ArgoSyncError{Msg: "API server unreachable: " + te.pyClass()}
			if te.sent {
				e.Msg = "API server unreachable (the request was sent and no answer came, so the sync may have been requested): " + te.pyClass()
				e.agent = "the API server did not answer; the sync may have been requested. Read argo.app.get before asking again: a second call finds the first one's operation."
			}
			return 0, e
		}
		return 0, err
	}
	return rp.status, nil
}
