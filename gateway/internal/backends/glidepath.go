package backends

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The glidepath.* tools: glidepath-api over plain HTTP (glidepath_api.GlidepathAPI; Glidepath docs/admin/api.md).
// The gateway calls it as its own identity, a bearer token read from a file on every call (a projected ServiceAccount token
// the kubelet rotates; nothing keeps it). glidepath-api authorizes that identity with a SubjectAccessReview in the app's
// -cicd namespace, so what the gateway may read at all is ordinary RBAC there; profile.glidepath.apps narrows it per agent,
// in the core before the call and in this gateway's own scope check (hangar.go).
//
// When the gateway is one of glidepath-api's `api.delegates` it also names the session's principal in
// X-Glidepath-On-Behalf-Of, which glidepath-api writes into its audit line (GLIDEPATH_ON_BEHALF_OF). The base URL is either
// the Service or an API server's services/proxy path to it; the API server drops Authorization before forwarding, so the
// token also goes in X-Glidepath-Authorization, as Tower does.
//
// The closed list of what this client sends (AllowedGlidepathRequests): the paths below under the configured base, each built
// from fixed text and arguments that matched their pattern, percent-escaped one segment at a time; the token goes to that base
// only; a redirect is never followed.

const (
	// LogTail is the most of a log or report an agent reads: a bigger one is cut to its end.
	LogTail    = 256 << 10
	maxGPOK    = 8 << 20
	maxGPErr   = 64 << 10
	maxGPBody  = 300 << 10
	watchDef   = 60
	watchMax   = 120
	pollEvery  = 3 * time.Second
	streamIdle = 30 * time.Second
)

// AllowedGlidepathRequests is the list of requests this client can make, for the documentation and its tests.
var AllowedGlidepathRequests = []string{
	"GET /v1/contract",
	"GET /v1/events/recent?app&type&since&limit",
	"GET /v1/apps/{app}/runs?sinceHours&limit",
	"GET /v1/apps/{app}/runs/{result}",
	"GET /v1/apps/{app}/runs/{result}/logs/{taskRun}",
	"GET /v1/apps/{app}/runs/{result}/explain",
	"GET /v1/apps/{app}/releases[/{chainId}]",
	"GET /v1/apps/{app}/releases/runs/{name}",
	"GET /v1/apps/{app}/live",
	"GET /v1/apps/{app}/live/stream",
	"GET /v1/apps/{app}/live/taskruns/{taskRun}/logs/{container}",
	"GET /v1/apps/{app}/evidence?image",
	"GET /v1/apps/{app}/tests?limit",
	"GET /v1/apps/{app}/tests/{id}/report",
	"GET /v1/apps/{app}/previews",
	"GET /v1/apps/{app}/environments",
	"GET /v1/apps/{app}/environments/{env}/rollback-plan?cluster",
	"POST /v1/apps/{app}/pipelineruns/{pipelinerun}/rerun",
	"POST /v1/apps/{app}/pipelineruns/{pipelinerun}/cancel",
	"POST /v1/apps/{app}/environments/{env}/promote",
	"POST /v1/apps/{app}/environments/{env}/rollback",
	"POST /v1/apps/{app}/cicd/validations",
}

// The patterns an argument must match to reach a URL, a query or a body: the Hangar policy bundle's argument checks
// (policy/hangar/bundle.json, arg_checks), anchored at both ends. The core refuses anything else before a handler runs; the
// client does not rely on that.
var gpPattern = map[string]*regexp.Regexp{
	"app":             regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`),
	"result":          regexp.MustCompile(`^[0-9a-f-]{36}$`),
	"taskRun":         regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`),
	"pipelinerun":     regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`),
	"chainId":         regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`),
	"env":             regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`),
	"cluster":         regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`),
	"sourceEnv":       regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`),
	"sourceCluster":   regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`),
	"sourceImageRepo": regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,254}$`),
	"sourceImageTag":  regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`),
	"container":       regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`),
	"name":            regexp.MustCompile(`^(promote|rollback)-[a-z0-9-]{1,56}$`),
	"image":           regexp.MustCompile(`^ghcr\.io/[a-z0-9._/-]{1,255}(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[0-9a-f]{64})?$`),
	"id":              regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`),
	"type":            regexp.MustCompile(`^[a-z0-9.]{1,256}$`),
	"since":           regexp.MustCompile(`^[0-9A-Za-z:.+_-]{1,64}$`),
	"targetReleaseId": regexp.MustCompile(`^[\x21-\x7e]{1,256}$`),
	"idempotency":     regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`),
}

// GlidepathError is glidepath-api failing or being unreachable. The agent is told only "backend error", unless the write may
// have happened (AgentMessage).
type GlidepathError struct {
	Msg   string
	agent string
}

func (e *GlidepathError) Error() string    { return e.Msg }
func (e *GlidepathError) Kind() string     { return "GlidepathError" }
func (e *GlidepathError) Evidence() string { return dispatch.ClipEvidence(e.Msg) }

// AgentMessage is empty for a failure that did not change anything; for a write whose answer was lost it says that the
// write may have happened.
func (e *GlidepathError) AgentMessage() string { return e.agent }

// Glidepath is the glidepath-api client.
type Glidepath struct {
	base       string
	tokenFile  string
	onBehalfOf bool
	timeout    time.Duration
	hc         *http.Client
	streamHC   *http.Client
	lim        limiter
	streamLim  limiter
	retry      retryPolicy
	// clock and sleep are the time a watch reads and waits on (a test makes them its own).
	clock func() float64
	sleep func(context.Context, time.Duration) bool
}

// NewGlidepath is the client for base (an http(s) URL; a path in it is kept: an API server's services/proxy path) reading
// its token from tokenFile.
func NewGlidepath(base, tokenFile string, onBehalfOf bool, timeout time.Duration, allowHTTP bool) (*Glidepath, error) {
	b, err := checkBase("GLIDEPATH_API_URL", base)
	if err == nil {
		err = requireHTTPS("GLIDEPATH_API_URL", b, allowHTTP)
	}
	if err != nil {
		return nil, err
	}
	if tokenFile == "" {
		return nil, errors.New("GLIDEPATH_TOKEN_FILE is the projected token glidepath-api authenticates")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	start := time.Now()
	sh := client(nil, 3*time.Second, 8)
	sh.Transport.(*http.Transport).ResponseHeaderTimeout = streamIdle
	return &Glidepath{base: b, tokenFile: tokenFile, onBehalfOf: onBehalfOf, timeout: timeout, hc: client(nil, 3*time.Second, 16),
		streamHC: sh, lim: newLimiter(16), streamLim: newLimiter(8), retry: defaultRetry(),
		clock: func() float64 { return time.Since(start).Seconds() }, sleep: sleepCtx}, nil
}

func deniedArg(name string, v any) error {
	return &core.Denied{Rule: "GP-ARG", Message: name + " " + echo(v) + " is not valid for this tool", Hint: "Pass the argument as the tool's description says."}
}

// seg is urllib.parse.quote(s, safe=""): one path segment, after the argument matched its pattern. A segment is never `.` or
// `..` (no pattern lets either through; this is the second guard).
func seg(kind string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || !gpPattern[kind].MatchString(s) || s == "." || s == ".." {
		return "", deniedArg(kind, v)
	}
	return segEscape(s), nil
}

// optStr is an optional string argument: absent or null is "", and a value must match its pattern.
func optStr(kind string, v any) (string, bool, error) {
	if v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok || !gpPattern[kind].MatchString(s) {
		return "", false, deniedArg(kind, v)
	}
	return s, true, nil
}

// optInt is an optional integer argument in [lo, hi]: absent or null is none. A float or a bool is not an integer.
func optInt(name string, v any, lo, hi int64) (int64, bool, error) {
	if v == nil {
		return 0, false, nil
	}
	var n int64
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, false, deniedArg(name, v)
		}
		n = i
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return 0, false, deniedArg(name, v)
	}
	if n < lo || n > hi {
		return 0, false, deniedArg(name, v)
	}
	return n, true, nil
}

type gpQuery struct{ k, v string }

// gpReq is one request.
type gpReq struct {
	method string
	path   string
	query  []gpQuery
	accept string
	extra  http.Header
	body   any
	hasBdy bool
	obo    string
}

// url is the base, the path and urllib.parse.urlencode's query (quote_plus, in the order given).
func (g *Glidepath) url(r gpReq) (string, error) {
	var sb strings.Builder
	sb.WriteString(g.base + r.path)
	for i, q := range r.query {
		if i == 0 {
			sb.WriteString("?")
		} else {
			sb.WriteString("&")
		}
		sb.WriteString(url.QueryEscape(q.k) + "=" + url.QueryEscape(q.v))
	}
	full := sb.String()
	// Defence in depth: whatever was built is still the configured scheme, host and base path, with no fragment.
	bu, berr := url.Parse(g.base)
	fu, ferr := url.Parse(full)
	if berr != nil || ferr != nil || fu.Scheme != bu.Scheme || fu.Host != bu.Host || fu.User != nil || fu.Fragment != "" ||
		!strings.HasPrefix(fu.EscapedPath(), bu.EscapedPath()+"/v1/") {
		return "", errors.New("the built URL is not under the configured base")
	}
	for _, s := range strings.Split(fu.EscapedPath(), "/") {
		if s == "." || s == ".." {
			return "", errors.New("the built URL has a dot segment")
		}
	}
	return full, nil
}

func (g *Glidepath) headers(r gpReq) (http.Header, string, error) {
	tok, err := readTokenMin(g.tokenFile)
	if err != nil {
		return nil, "", err
	}
	if strings.ContainsAny(tok, "\r\n") {
		return nil, "", headerError()
	}
	h := http.Header{"Authorization": {"Bearer " + tok}, "X-Glidepath-Authorization": {"Bearer " + tok}, "Accept": {r.accept}}
	for k, v := range r.extra {
		h[k] = v
	}
	if g.onBehalfOf && r.obo != "" {
		if strings.ContainsAny(r.obo, "\r\n") {
			return nil, "", headerError()
		}
		h["X-Glidepath-On-Behalf-Of"] = []string{r.obo}
	}
	return h, tok, nil
}

// do sends a request. A failure to get an answer is a GlidepathError naming the cause ("glidepath-api unreachable:
// URLError(ConnectionRefusedError)"); a write that was sent and not answered says the write may have happened.
func (g *Glidepath) do(ctx context.Context, r gpReq) (*reply, error) {
	if r.accept == "" {
		r.accept = "application/json"
	}
	full, err := g.url(r)
	if err != nil {
		return nil, &dispatch.KindError{Kind: "ValueError", Err: err}
	}
	h, tok, err := g.headers(r)
	if err != nil {
		return nil, err
	}
	var body []byte
	if r.hasBdy {
		h.Set("Content-Type", "application/json")
		if body, err = json.Marshal(r.body); err != nil {
			return nil, err
		}
		if len(body) > maxGPBody {
			return nil, &core.Denied{Rule: "GP-ARG", Message: "the request body is larger than glidepath-api takes", Hint: "Send a smaller change."}
		}
	}
	var rp *reply
	if r.method == http.MethodGet {
		rp, err = sendRead(ctx, g.retry, g.hc, g.lim, g.timeout, full, h, maxGPOK, maxGPErr)
	} else {
		rp, err = send(ctx, g.hc, g.lim, g.timeout, r.method, full, h, body, maxGPOK, maxGPErr)
	}
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			ge := &GlidepathError{Msg: "glidepath-api unreachable: " + te.urlErrorWhy()}
			if r.method == http.MethodPost && te.sent {
				ge.Msg += " (the request was sent and no answer came: the write may have been made)"
				ge.agent = "glidepath-api did not answer; the request may have been made. Asking again with the same arguments is safe: it carries an idempotency key or does nothing twice."
			}
			return nil, ge
		}
		return nil, err
	}
	rp.body = redact(rp.body, tok)
	return rp, nil
}

// -- refusals ---------------------------------------------------------------------------------------------------------

var ruleRx = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const maxFindingText = 1000

// errDoc reads an error body as Python's `json.loads(body)` then `.get`: an empty or unreadable body is {}; a JSON value
// that is not an object is an AttributeError.
func errDoc(body []byte) (map[string]any, error) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	v, err := decodeJSON(body)
	if err != nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &dispatch.KindError{Kind: "AttributeError", Err: errors.New("an error answer is not an object")}
	}
	return m, nil
}

// text reads a string field of an answer, cut at 1000 characters; anything that is not a string is "".
func text(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return clipRunes(s, maxFindingText)
}

func ruleOf(m map[string]any) string {
	if s, _ := m["rule"].(string); ruleRx.MatchString(s) {
		return s
	}
	return ""
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

const hint422 = "The PipelineRun is outside the shape Glidepath may start or change; ask a human."

// refusal is GlidepathAPI._refusal. A refusal about the caller is a denial (with glidepath-api's rule id). A refusal about
// the question (400, 404: no such app or run, no archived log) is an answer the agent acts on, returned as data. Anything
// else is a backend failure.
func refusal(status int, body []byte, hint422 string) (any, error) {
	e, err := errDoc(body)
	if err != nil {
		return nil, err
	}
	rule, message, hint := ruleOf(e), text(e, "error"), text(e, "hint")
	switch {
	case status == 401 || status == 403:
		return nil, &core.Denied{Rule: or(rule, "GP-AUTHZ-001"), Message: "glidepath-api refused: " + message,
			Hint: or(hint, "Clearance's identity may not do this for this app in Glidepath; ask a human to grant it.")}
	case status == 422:
		return nil, &core.Denied{Rule: or(rule, "GP-API-422"), Message: "glidepath-api refused the write: " + message, Hint: or(hint, hint422)}
	case (status == 400 || status == 404) && rule != "":
		return map[string]any{"error": map[string]any{"rule": rule, "message": message, "hint": hint}}, nil
	}
	return nil, &GlidepathError{Msg: strings.TrimSpace("glidepath-api " + strconv.Itoa(status) + " " + rule)}
}

func jsonOf(rp *reply) (any, error) {
	v, err := decodeJSON(rp.body)
	if err != nil {
		return nil, valueError(err)
	}
	return v, nil
}

func (g *Glidepath) getJSON(ctx context.Context, r gpReq) (any, error) {
	r.method = http.MethodGet
	rp, err := g.do(ctx, r)
	if err != nil {
		return nil, err
	}
	if rp.status == 200 {
		return jsonOf(rp)
	}
	return refusal(rp.status, rp.body, hint422)
}

// -- reads ------------------------------------------------------------------------------------------------------------

func appPath(app any, rest string) (string, error) {
	a, err := seg("app", app)
	if err != nil {
		return "", err
	}
	return "/v1/apps/" + a + rest, nil
}

// Runs is glidepath.runs.list.
func (g *Glidepath) Runs(ctx context.Context, app, sinceHours, limit any, obo string) (any, error) {
	p, err := appPath(app, "/runs")
	if err != nil {
		return nil, err
	}
	var q []gpQuery
	if n, ok, err := optInt("sinceHours", sinceHours, 1, 744); err != nil {
		return nil, err
	} else if ok {
		q = append(q, gpQuery{"sinceHours", strconv.FormatInt(n, 10)})
	}
	if n, ok, err := optInt("limit", limit, 1, 1000); err != nil {
		return nil, err
	} else if ok {
		q = append(q, gpQuery{"limit", strconv.FormatInt(n, 10)})
	}
	return g.getJSON(ctx, gpReq{path: p, query: q, obo: obo})
}

// Run is glidepath.run.get.
func (g *Glidepath) Run(ctx context.Context, app, result any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	r, err := seg("result", result)
	if err != nil {
		return nil, err
	}
	return g.getJSON(ctx, gpReq{path: "/v1/apps/" + a + "/runs/" + r, obo: obo})
}

// text answers a plain-text GET: 200 is the body as text (the caller shapes it), else a refusal.
func (g *Glidepath) getText(ctx context.Context, r gpReq, accept string) ([]byte, any, error) {
	r.method, r.accept = http.MethodGet, accept
	rp, err := g.do(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	if rp.status == 200 {
		return rp.body, nil, nil
	}
	v, err := refusal(rp.status, rp.body, hint422)
	return nil, v, err
}

// Log is glidepath.logs.get: one TaskRun's archived step logs.
func (g *Glidepath) Log(ctx context.Context, app, result, taskRun any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	r, err := seg("result", result)
	if err != nil {
		return nil, err
	}
	t, err := seg("taskRun", taskRun)
	if err != nil {
		return nil, err
	}
	body, other, err := g.getText(ctx, gpReq{path: "/v1/apps/" + a + "/runs/" + r + "/logs/" + t, obo: obo}, "text/plain")
	if err != nil || body == nil {
		return other, err
	}
	return map[string]any{"log": decodeUTF8Replace(body)}, nil
}

// Explain is glidepath.explain.
func (g *Glidepath) Explain(ctx context.Context, app, result any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	r, err := seg("result", result)
	if err != nil {
		return nil, err
	}
	return g.getJSON(ctx, gpReq{path: "/v1/apps/" + a + "/runs/" + r + "/explain", obo: obo})
}

// Releases is glidepath.releases.get: an app's release records, or one by chainId.
func (g *Glidepath) Releases(ctx context.Context, app, chainID any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	p := "/v1/apps/" + a + "/releases"
	if pyTruthy(chainID) {
		c, err := seg("chainId", chainID)
		if err != nil {
			return nil, err
		}
		p += "/" + c
	}
	return g.getJSON(ctx, gpReq{path: p, obo: obo})
}

// Contract is glidepath.contract.
func (g *Glidepath) Contract(ctx context.Context, obo string) (any, error) {
	return g.getJSON(ctx, gpReq{path: "/v1/contract", obo: obo})
}

// ReleaseRun is glidepath.releaseRun.get.
func (g *Glidepath) ReleaseRun(ctx context.Context, app, name any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	n, err := seg("name", name)
	if err != nil {
		return nil, err
	}
	return g.getJSON(ctx, gpReq{path: "/v1/apps/" + a + "/releases/runs/" + n, obo: obo})
}

// Evidence is glidepath.evidence.get.
func (g *Glidepath) Evidence(ctx context.Context, app, image any, obo string) (any, error) {
	p, err := appPath(app, "/evidence")
	if err != nil {
		return nil, err
	}
	img, ok, err := optStr("image", image)
	if err != nil {
		return nil, err
	}
	var q []gpQuery
	if ok {
		q = append(q, gpQuery{"image", img})
	}
	return g.getJSON(ctx, gpReq{path: p, query: q, obo: obo})
}

// Tests is glidepath.tests.list.
func (g *Glidepath) Tests(ctx context.Context, app, limit any, obo string) (any, error) {
	p, err := appPath(app, "/tests")
	if err != nil {
		return nil, err
	}
	var q []gpQuery
	if n, ok, err := optInt("limit", limit, 1, 1000); err != nil {
		return nil, err
	} else if ok {
		q = append(q, gpQuery{"limit", strconv.FormatInt(n, 10)})
	}
	return g.getJSON(ctx, gpReq{path: p, query: q, obo: obo})
}

// tail is _tail: a body over LogTail is cut to its last LogTail bytes.
func tail(body []byte, key string) map[string]any {
	if len(body) <= LogTail {
		return map[string]any{key: decodeUTF8Replace(body)}
	}
	return map[string]any{key: decodeUTF8Replace(body[len(body)-LogTail:]), "truncated": true, "bytes": int64(len(body))}
}

// TestReport is glidepath.tests.report: one test run's JUnit report, its last 256 KiB.
func (g *Glidepath) TestReport(ctx context.Context, app, id any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	i, err := seg("id", id)
	if err != nil {
		return nil, err
	}
	body, other, err := g.getText(ctx, gpReq{path: "/v1/apps/" + a + "/tests/" + i + "/report", obo: obo}, "application/xml")
	if err != nil || body == nil {
		return other, err
	}
	return tail(body, "report"), nil
}

// Events is glidepath.events.recent: the app's recent CDEvents (best effort, at most 24 hours).
func (g *Glidepath) Events(ctx context.Context, app, typePrefix, since, limit any, obo string) (any, error) {
	a, ok, err := optStr("app", app)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, deniedArg("app", app)
	}
	q := []gpQuery{{"app", a}}
	if t, ok, err := optStr("type", typePrefix); err != nil {
		return nil, err
	} else if ok {
		q = append(q, gpQuery{"type", t})
	}
	if s, ok, err := optStr("since", since); err != nil {
		return nil, err
	} else if ok {
		q = append(q, gpQuery{"since", s})
	}
	if n, ok, err := optInt("limit", limit, 1, 1000); err != nil {
		return nil, err
	} else if ok {
		q = append(q, gpQuery{"limit", strconv.FormatInt(n, 10)})
	}
	return g.getJSON(ctx, gpReq{path: "/v1/events/recent", query: q, obo: obo})
}

// Previews is glidepath.previews.list.
func (g *Glidepath) Previews(ctx context.Context, app any, obo string) (any, error) {
	p, err := appPath(app, "/previews")
	if err != nil {
		return nil, err
	}
	return g.getJSON(ctx, gpReq{path: p, obo: obo})
}

// Environments is glidepath.environments.list.
func (g *Glidepath) Environments(ctx context.Context, app any, obo string) (any, error) {
	p, err := appPath(app, "/environments")
	if err != nil {
		return nil, err
	}
	return g.getJSON(ctx, gpReq{path: p, obo: obo})
}

// RollbackPlan is glidepath.environments.rollbackPlan.
func (g *Glidepath) RollbackPlan(ctx context.Context, app, env, cluster any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	e, err := seg("env", env)
	if err != nil {
		return nil, err
	}
	c, ok, err := optStr("cluster", cluster)
	if err != nil {
		return nil, err
	}
	var q []gpQuery
	if ok {
		q = append(q, gpQuery{"cluster", c})
	}
	return g.getJSON(ctx, gpReq{path: "/v1/apps/" + a + "/environments/" + e + "/rollback-plan", query: q, obo: obo})
}

// -- writes -----------------------------------------------------------------------------------------------------------

// post is _post: a write. 200 and 201 are the answer; a 409 is an answer the agent acts on (an error as data); the rest is
// a refusal. The Idempotency-Key makes a retried call start nothing twice.
func (g *Glidepath) post(ctx context.Context, r gpReq, hint string) (any, error) {
	r.method = http.MethodPost
	rp, err := g.do(ctx, r)
	if err != nil {
		return nil, err
	}
	if rp.status == 200 || rp.status == 201 {
		return jsonOf(rp)
	}
	if rp.status == 409 {
		// Python reads this body with json.loads straight away: one that is not JSON is a JSONDecodeError, not an empty answer.
		e := map[string]any{}
		if len(rp.body) > 0 {
			v, err := decodeJSON(rp.body)
			if err != nil {
				return nil, valueError(err)
			}
			m, ok := v.(map[string]any)
			if !ok {
				return nil, &dispatch.KindError{Kind: "AttributeError", Err: errors.New("an error answer is not an object")}
			}
			e = m
		}
		rule := or(ruleOf(e), "GP-API-409")
		return map[string]any{"error": map[string]any{"rule": rule, "message": text(e, "error"), "hint": text(e, "hint")}}, nil
	}
	return refusal(rp.status, rp.body, hint)
}

func idemHeader(key string) (http.Header, error) {
	if !gpPattern["idempotency"].MatchString(key) {
		return nil, &dispatch.KindError{Kind: "ValueError", Err: errors.New("the idempotency key is not valid")}
	}
	return http.Header{"Idempotency-Key": {key}}, nil
}

// Rerun is glidepath.run.rerun: a copy of the PipelineRun's spec. The key names the session and the run, so retrying the
// same call starts no second run.
func (g *Glidepath) Rerun(ctx context.Context, app, pipelinerun any, key, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	p, err := seg("pipelinerun", pipelinerun)
	if err != nil {
		return nil, err
	}
	h, err := idemHeader(key)
	if err != nil {
		return nil, err
	}
	return g.post(ctx, gpReq{path: "/v1/apps/" + a + "/pipelineruns/" + p + "/rerun", extra: h, obo: obo}, hint422)
}

// Cancel is glidepath.run.cancel.
func (g *Glidepath) Cancel(ctx context.Context, app, pipelinerun any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	p, err := seg("pipelinerun", pipelinerun)
	if err != nil {
		return nil, err
	}
	return g.post(ctx, gpReq{path: "/v1/apps/" + a + "/pipelineruns/" + p + "/cancel", obo: obo}, hint422)
}

const release422 = "Read glidepath.environments.list (promote one stage at a time, to a Flight environment) or " +
	"glidepath.environments.rollbackPlan (roll back only to one of its targets); otherwise ask a human."

// release is _release: promote or rollback, which only start the app's release run (its open-release-pr step opens the
// gitops PR with the platform's GitHub App, and a human merges it). Dry run goes in the header, the form that also works
// through the API server's service proxy (which refuses ?dryRun).
func (g *Glidepath) release(ctx context.Context, op string, app, env any, body map[string]any, key string, dryRun bool, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	e, err := seg("env", env)
	if err != nil {
		return nil, err
	}
	h, err := idemHeader(key)
	if err != nil {
		return nil, err
	}
	h.Set("X-Glidepath-Dry-Run", strconv.FormatBool(dryRun))
	return g.post(ctx, gpReq{path: "/v1/apps/" + a + "/environments/" + e + "/" + op, extra: h, body: body, hasBdy: true, obo: obo}, release422)
}

// PromoteBody is the body of a promote: the cluster and the source (an environment and its cluster, or an image repository
// and tag), each argument matched to its pattern. Python sends the keys that are set.
func PromoteBody(a map[string]any) (map[string]any, error) {
	cl, err := argStr("cluster", a["cluster"])
	if err != nil {
		return nil, err
	}
	body := map[string]any{"cluster": cl}
	for _, k := range []string{"sourceEnv", "sourceCluster", "sourceImageRepo", "sourceImageTag"} {
		if pyTruthy(a[k]) {
			v, err := argStr(k, a[k])
			if err != nil {
				return nil, err
			}
			body[k] = v
		}
	}
	env, cluster, repo, tag := pyTruthy(a["sourceEnv"]), pyTruthy(a["sourceCluster"]), pyTruthy(a["sourceImageRepo"]), pyTruthy(a["sourceImageTag"])
	n := 0
	for _, b := range []bool{env, cluster, repo, tag} {
		if b {
			n++
		}
	}
	if !(n == 2 && ((env && cluster) || (repo && tag))) {
		return nil, &core.Denied{Rule: "GP-ARG", Message: "a promote names its source: sourceEnv and sourceCluster, or sourceImageRepo and sourceImageTag",
			Hint: "Pass one of the two pairs, not both and not a half."}
	}
	return body, nil
}

func argStr(kind string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || !gpPattern[kind].MatchString(s) {
		return "", deniedArg(kind, v)
	}
	return s, nil
}

// PromoteSource is hangar_tools promote_source: what the promotion comes from, for its idempotency key.
func PromoteSource(a map[string]any) string {
	if pyTruthy(a["sourceEnv"]) {
		return "env:" + pyStr(a["sourceEnv"]) + "@" + pyStr(a["sourceCluster"])
	}
	return "image:" + pyStr(a["sourceImageRepo"]) + ":" + pyStr(a["sourceImageTag"])
}

// Promote is glidepath.promote: propose promoting to the next Flight environment. It starts the release run that opens the
// gitops PR a human merges.
func (g *Glidepath) Promote(ctx context.Context, app, env any, body map[string]any, key string, dryRun bool, obo string) (any, error) {
	return g.release(ctx, "promote", app, env, body, key, dryRun, obo)
}

// RollbackBody is the body of a rollback.
func RollbackBody(a map[string]any) (map[string]any, error) {
	cl, err := argStr("cluster", a["cluster"])
	if err != nil {
		return nil, err
	}
	id, err := argStr("targetReleaseId", a["targetReleaseId"])
	if err != nil {
		return nil, err
	}
	reason, ok := a["reason"].(string)
	if r := pyStrip(reason); !ok || r == "" || utf8.RuneCountInString(r) > 1024 {
		return nil, deniedArg("reason", a["reason"])
	}
	return map[string]any{"cluster": cl, "targetReleaseId": id, "reason": pyStrip(reason)}, nil
}

// Rollback is glidepath.rollback: propose rolling a Flight environment back to one of its rollback plan's targets.
func (g *Glidepath) Rollback(ctx context.Context, app, env any, body map[string]any, key string, dryRun bool, obo string) (any, error) {
	return g.release(ctx, "rollback", app, env, body, key, dryRun, obo)
}

// ValidateCICD is glidepath.cicd.validate: what a merge patch to cicd.yaml would mean, as the cicd-validate check will judge
// it. Nothing is written. The verdict is the 200 body; a 400 about the request itself comes back as data.
func (g *Glidepath) ValidateCICD(ctx context.Context, app any, body map[string]any, obo string) (any, error) {
	a, err := seg("app", app)
	if err != nil {
		return nil, err
	}
	rp, err := g.do(ctx, gpReq{method: http.MethodPost, path: "/v1/apps/" + a + "/cicd/validations", body: body, hasBdy: true, obo: obo})
	if err != nil {
		return nil, err
	}
	if rp.status == 200 {
		return jsonOf(rp)
	}
	return refusal(rp.status, rp.body, hint422)
}

// String describes the client without its token file.
func (g *Glidepath) String() string { return fmt.Sprintf("glidepath-api at %s", g.base) }
