package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
)

// app.api.get: a GET on an application's own API (reads.AppAPIs). The deployment names each service and its base URL
// (APP_API_SERVICES, a JSON object {service: base URL}); an agent names a service its definition allows (the core checks
// profile.apis.allow) and a path. Nothing else is reachable:
//
//   - the URL is the configured base plus a path that BuildAppURL accepted: an absolute path of a short list of characters,
//     with no dot segment (written, or percent-encoded, or hidden behind a ;), no //, no encoded / \ % or control
//     character, no scheme, host, userinfo, fragment or space, so it cannot choose a host or leave the base path. The built
//     URL is parsed again and refused unless its scheme, host and base path are the configured ones;
//   - the request is a GET with an Accept header only: no credential of any kind, no cookie;
//   - a redirect is not followed (a 3xx is the answer, its status and body, as in Python, which also does not follow);
//   - the body is read to at most MaxAPIBody bytes and answered as JSON when it is, else as text.

const (
	MaxAPIPath = 512
	MaxAPIBody = 256 << 10
)

var (
	apiService = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// apiPath is a path and an optional query on the service's own base: no scheme, no authority, no fragment.
	apiPath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*(\?[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*)?$`)
	// hex2 is a percent sign that starts a well-formed escape.
	badEscape = regexp.MustCompile(`%([^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)`)
	// controlEscape is a percent-encoded C0 control or DEL (CR and LF among them).
	controlEscape = regexp.MustCompile(`(?i)%(0[0-9a-f]|1[0-9a-f]|7f)`)
)

func deniedPath(path any) error {
	return &core.Denied{Rule: "READ-API-PATH", Message: fmt.Sprintf("%s is not a path on the service", pyStrRepr(clipChars(pyStr(path), 80))),
		Hint: "Pass an absolute path such as /api/flights?date=2026-11-03 (no host, no '..')."}
}

func clipChars(s string, n int) string {
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}

// checkAppPath is the rule on the path an agent supplies (reads.AppAPIs.get, and what Go adds, marked below).
func checkAppPath(path any) (string, error) {
	p, ok := path.(string)
	if !ok || utf8.RuneCountInString(p) > MaxAPIPath || !apiPath.MatchString(p) || strings.Contains(p, "//") {
		return "", deniedPath(path)
	}
	low := strings.ToLower(p)
	for _, enc := range []string{"%2e", "%2f", "%5c", "%00", "%25"} { // %25: no double encoding
		if strings.Contains(low, enc) {
			return "", deniedPath(path)
		}
	}
	// Go adds: a % that does not start two hex digits, and a percent-encoded control character, are refused.
	if badEscape.MatchString(p) || controlEscape.MatchString(p) {
		return "", deniedPath(path)
	}
	pathPart, query, hasQuery := strings.Cut(p, "?")
	// Go adds, and decides on the DECODED form (a server may decode before it strips path parameters or removes dot segments,
	// so what matters is what a segment means once decoded, not how it is written). Every segment is percent-decoded once.
	// The decoded segment must be printable ASCII (0x20 to 0x7e): that refuses invalid UTF-8, overlong forms (%c0%ae, %c0%af),
	// Unicode dot and slash look-alikes (fullwidth U+FF0E and U+FF0F, U+2215, U+2044, U+2024, ...) and every other non-ASCII
	// character, without a list that could miss one; real application paths are ASCII. It must not hold / \ ? or #. The part
	// before its first ; (path parameters) must not be . or .. . The query's escapes must decode to well-formed UTF-8.
	for _, seg := range strings.Split(pathPart, "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", deniedPath(path)
		}
		for i := 0; i < len(dec); i++ {
			if c := dec[i]; c < 0x20 || c > 0x7e || c == '/' || c == '\\' || c == '?' || c == '#' {
				return "", deniedPath(path)
			}
		}
		if name, _, _ := strings.Cut(dec, ";"); name == "." || name == ".." {
			return "", deniedPath(path)
		}
	}
	if hasQuery {
		if dec, err := url.QueryUnescape(query); err != nil || !utf8.ValidString(dec) {
			return "", deniedPath(path)
		}
	}
	return p, nil
}

// BuildAppURL is the URL for path on the service at base (a base without trailing slash, as the configuration keeps
// it). The error is a *core.Denied READ-API-PATH. It is the whole of how an agent's text reaches a URL here.
func BuildAppURL(base, path string) (string, error) {
	p, err := checkAppPath(path)
	if err != nil {
		return "", err
	}
	full := base + p
	// Defence in depth: whatever the rules above let through, the URL must still be the configured scheme, host and
	// base path, with no userinfo or fragment.
	bu, berr := url.Parse(base)
	fu, ferr := url.Parse(full)
	if berr != nil || ferr != nil || fu.Scheme != bu.Scheme || fu.Host != bu.Host || fu.User != nil || fu.Fragment != "" ||
		!strings.HasPrefix(fu.EscapedPath(), bu.EscapedPath()+"/") {
		return "", deniedPath(path)
	}
	return full, nil
}

// AppAPIs is the app.api.get backend.
type AppAPIs struct {
	services map[string]*apiSvc
	Timeout  time.Duration
	hc       *http.Client
}

type apiSvc struct {
	base string
	lim  limiter
}

// NewAppAPIs is the backend for {service: base URL} (APP_API_SERVICES). Each key is a service name (a DNS label) and each
// value an http(s) URL with a host and no credentials, query or fragment.
func NewAppAPIs(services map[string]string, timeout time.Duration) (*AppAPIs, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	a := &AppAPIs{services: map[string]*apiSvc{}, Timeout: timeout, hc: client(nil, 3*time.Second, 16)}
	for k, v := range services {
		if !apiService.MatchString(k) {
			return nil, fmt.Errorf("APP_API_SERVICES[%s]: the key is a service name (a DNS label) and the value an http(s) base URL without credentials or query", pyStrRepr(k))
		}
		b, err := checkBase("APP_API_SERVICES["+pyStrRepr(k)+"]", v)
		if err != nil {
			return nil, fmt.Errorf("APP_API_SERVICES[%s]: the key is a service name (a DNS label) and the value an http(s) base URL without credentials or query", pyStrRepr(k))
		}
		a.services[k] = &apiSvc{base: b, lim: newLimiter(8)}
	}
	return a, nil
}

// ParseAppAPIServices reads APP_API_SERVICES: "" is none, else a JSON object of strings.
func ParseAppAPIServices(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	const msg = "APP_API_SERVICES is a JSON object {service: base URL}"
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("%s", msg)
	}
	out := map[string]string{}
	for k, u := range v {
		s, ok := u.(string)
		if !ok {
			return nil, fmt.Errorf("APP_API_SERVICES[%s]: the key is a service name (a DNS label) and the value an http(s) base URL without credentials or query", pyStrRepr(k))
		}
		out[k] = s
	}
	return out, nil
}

func (a *AppAPIs) known() string {
	names := make([]string, 0, len(a.services))
	for k := range a.services {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Get answers app.api.get.
func (a *AppAPIs) Get(ctx context.Context, service, path any) (any, error) {
	name, _ := service.(string)
	svc := a.services[name]
	if svc == nil || !apiService.MatchString(name) {
		return nil, &core.Denied{Rule: "READ-API-SERVICE", Message: echo(service) + " is not an application API this deployment knows",
			Hint: "Name one of the services your definition lists under profile.apis.allow; a person adds new ones to the gateway's APP_API_SERVICES. Known: " + a.known() + "."}
	}
	p, isStr := path.(string)
	if !isStr {
		return nil, deniedPath(path)
	}
	full, err := BuildAppURL(svc.base, p)
	if err != nil {
		return nil, err
	}
	status, body, truncated, err := get(ctx, a.hc, svc.lim, a.Timeout, full, http.Header{"Accept": {"application/json"}}, MaxAPIBody)
	if err != nil {
		return nil, &ReadError{Msg: name + " API unreachable: " + reason(err)}
	}
	out := map[string]any{"service": name, "path": path, "status": status, "truncated": truncated}
	var data any
	if len(body) > 0 && !truncated {
		if v, err := decodeJSON(body); err == nil {
			data = v
		}
	}
	if data != nil {
		out["json"] = data
	} else {
		out["text"] = decodeUTF8Replace(body)
	}
	return out, nil
}
