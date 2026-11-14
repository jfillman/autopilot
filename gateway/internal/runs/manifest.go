// Package runs is the run backend: what a session's run IS on Hangar, an AgentRun XR in the run namespace, created and
// deleted through the Kubernetes API as the gateway's own identity. It is the Go side of autopilot.manifests,
// autopilot.kube_runs and gateway.AgentRunBackend.
//
// Everything that reaches the API server is built here from the session and its definition and checked before a request
// exists: the object is an AgentRun in autopilot-runs, its name is a run name derived from the session id, every label is a
// valid Kubernetes label value, and the only three requests that can be sent are the create, the delete and the read of
// one named AgentRun (kube.go). RBAC is the outer limit; this is the inner one.
package runs

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

const (
	// Namespace is where every AgentRun lives (manifests.RUN_NAMESPACE).
	Namespace = "autopilot-runs"
	// ServiceAccount is the ServiceAccount a run pod runs as, in its own namespace agent-<run> (function-agentrun).
	ServiceAccount = "run"
	// APIVersion is the AgentRun's group and version.
	APIVersion = "catalog.hangar.io/v1alpha1"
)

var (
	// runName is the name function-agentrun accepts (compose.RUN_NAME): r- and 6 to 32 hex digits.
	runName = regexp.MustCompile(`^r-[a-f0-9]{6,32}$`)
	// dnsLabel is the check kube_runs makes of every name it sends (_NAME).
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// labelValue is a Kubernetes label value: at most 63 characters, alphanumeric at both ends, with - _ . inside.
	labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
)

// RunName is the name of a session's AgentRun: r- and the session id without its s- (manifests.run_name).
func RunName(sessionID string) string { return "r-" + strings.TrimPrefix(sessionID, "s-") }

// ValidRunName says whether name is one the AgentRun composition accepts.
func ValidRunName(name string) bool { return runName.MatchString(name) }

// Principal is the identity a run's pod authenticates as: the ServiceAccount `run` in the run's own namespace,
// agent-<run> (gateway.AgentRunBackend.principal). "" for a name no run has, so nothing can be bound to it.
func Principal(run string) string {
	if !ValidRunName(run) {
		return ""
	}
	return "agent-" + run + "/" + ServiceAccount
}

// Refused is a manifest or a name outside what this adapter may send, raised before any request is built
// (kube_runs.RunRefused).
type Refused struct{ Msg string }

func (e *Refused) Error() string    { return e.Msg }
func (e *Refused) Kind() string     { return "RunRefused" }
func (e *Refused) Evidence() string { return clip160(e.Msg) }

// Error is the API server refusing or failing a create, delete or read, or not being reachable (kube_runs.KubeRunError).
// Never shown to the agent: dispatch says "backend error". Its text is a status or a transport class, never a token, a
// header or a response body.
type Error struct{ Msg string }

func (e *Error) Error() string    { return e.Msg }
func (e *Error) Kind() string     { return "KubeRunError" }
func (e *Error) Evidence() string { return clip160(e.Msg) }

func clip160(s string) string {
	if r := []rune(s); len(r) > 160 {
		return string(r[:160])
	}
	return s
}

// ErrNoImage is a definition with no runtime image: a run needs one (Python: ValueError).
var ErrNoImage = errors.New("agent has no image; a run needs one")

// Manifest builds the AgentRun for a session (manifests.agentrun_manifest): the session's own limits (narrowed at open,
// not its definition's), its deadline, task and session ids, its parent's id for a child, the definition's image,
// framework, sandbox and sidecars, and the run's input. It is checked before it is returned: every label value is a
// valid Kubernetes label value and the name a run name. A caller supplies no namespace, service account, volume or
// image of its own: the namespace is fixed, the service account is the composition's, and the image is the definition's.
func Manifest(s *session.Session, d *definitions.Definition, input map[string]any) (map[string]any, error) {
	img := ""
	if d.Image != nil {
		img = *d.Image
	}
	if img == "" {
		return nil, fmt.Errorf("agent %q has no image; a run needs one", d.Name)
	}
	s.Lock()
	lim, id, task, parent, expires := s.Limits, s.ID, s.TaskID, s.ParentID, s.ExpiresAt
	s.Unlock()
	tools := append([]string{}, lim.Tools...)
	sort.Strings(tools)
	spec := map[string]any{
		"agent":     d.Name,
		"image":     img,
		"framework": d.Framework,
		"sandbox":   d.Sandbox,
		"compute":   lim.Compute,
		"network":   map[string]any{"mode": lim.Network},
		"expiresAt": core.Iso(expires),
		"taskId":    task,
		"sessionId": id,
		"limits": map[string]any{"toolCalls": lim.ToolCalls, "githubCalls": lim.GithubCalls, "modelTokens": lim.ModelTokens,
			"tools": tools},
	}
	if parent != nil {
		spec["parentSessionId"] = *parent
	}
	if len(d.Sidecars) > 0 {
		cars := make([]any, len(d.Sidecars))
		for i, c := range d.Sidecars {
			m := make(map[string]any, len(c))
			for k, v := range c {
				m[k] = v
			}
			cars[i] = m
		}
		spec["sidecars"] = cars
	}
	if len(input) > 0 {
		spec["input"] = input
	}
	name := RunName(id)
	m := map[string]any{
		"apiVersion": APIVersion, "kind": "AgentRun",
		"metadata": map[string]any{
			"name": name, "namespace": Namespace,
			"labels": map[string]any{"hangar.io/component": "autopilot", "hangar.io/agent": d.Name, "hangar.io/task": task,
				"hangar.io/session": id},
		},
		"spec": spec,
	}
	if err := check(m); err != nil {
		return nil, err
	}
	return m, nil
}

// check is what the manifest must be before the API server sees it, beyond what kube_runs.create checks of the object.
func check(m map[string]any) error {
	meta, _ := m["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if !ValidRunName(name) || !dnsLabel.MatchString(name) {
		return &Refused{"an AgentRun is named r-<6 to 32 hex>: " + safe(name)}
	}
	labels, _ := meta["labels"].(map[string]any)
	for k, v := range labels {
		s, _ := v.(string)
		if s == "" || !labelValue.MatchString(s) {
			return &Refused{"the label " + k + " is not a valid Kubernetes label value"}
		}
	}
	return nil
}

// safe names a value in an error without echoing more than a short, printable prefix of it.
func safe(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		r = r[:40]
	}
	for i, c := range r {
		if c < 0x20 || c == 0x7f {
			r[i] = '?'
		}
	}
	return string(r)
}
