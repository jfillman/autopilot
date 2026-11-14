package backends

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// catalog.read: a search of Tower's Backstage catalog (reads.BackstageCatalog). GET {BACKSTAGE_URL}/api/catalog/entities/by-query
// with the gateway's own projected token for the backstage audience, re-read on every call. Only a summary of each
// entity comes back, at most MaxEntities, never annotations or the rest of spec.
//
// The URL is the configured base and one fixed path; the agent's query goes only into the query string, escaped.
// The token goes only to the configured host: a redirect is never followed (Python follows one and sends the token
// along). The answer is read to at most maxCatalogBody bytes and its JSON depth is bounded before it is decoded.

const (
	MaxEntities = 25
	// maxQueryChars is the longest catalog query (Python: 500 characters).
	maxQueryChars = 500
	// maxCatalogBody caps the answer. Python reads 16 MiB; a search asks for 25 entities and nine fields, so nothing
	// real is near this.
	maxCatalogBody = 4 << 20
	catalogPath    = "/api/catalog/entities/by-query"
)

var catalogFields = []string{"kind", "metadata.namespace", "metadata.name", "metadata.title", "metadata.description",
	"spec.type", "spec.owner", "spec.lifecycle", "spec.system"}
var searchFields = []string{"metadata.name", "metadata.title", "metadata.description"}

// Catalog is the catalog.read backend.
type Catalog struct {
	base      string
	TokenFile string
	Timeout   time.Duration
	hc        *http.Client
	lim       limiter
}

// NewCatalog is a catalog backend on base (an http(s) URL with a host, no credentials, query or fragment) reading its
// token from tokenFile.
func NewCatalog(base, tokenFile string, timeout time.Duration) (*Catalog, error) {
	b, err := checkBase("BACKSTAGE_URL", base)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Catalog{base: b, TokenFile: tokenFile, Timeout: timeout, hc: client(nil, 3*time.Second, 16), lim: newLimiter(16)}, nil
}

// checkBase validates an operator-configured base URL and returns it without its trailing slashes. The operator's own
// URL decides http or https; nothing about a host name is special.
func checkBase(name, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || strings.Contains(raw, "#") || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", fmt.Errorf("%s is an http(s) base URL with a host, without credentials, query or fragment", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

func readToken(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fileError(err)
	}
	return pyStrip(string(b)), nil
}

// catalogQuery is the query string reads.BackstageCatalog.catalog builds, in its order: limit, the fields, then a filter
// (a query with "=") or a full-text term and the fields it searches. urllib.parse.urlencode is quote_plus, which is
// url.QueryEscape.
func catalogQuery(query string) string {
	var parts []string
	add := func(k, v string) { parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v)) }
	add("limit", fmt.Sprint(MaxEntities))
	for _, f := range catalogFields {
		add("fields", f)
	}
	q := pyStrip(query)
	if strings.Contains(query, "=") {
		add("filter", q)
	} else {
		add("fullTextFilterTerm", q)
		for _, f := range searchFields {
			add("fullTextFilterFields", f)
		}
	}
	return strings.Join(parts, "&")
}

// Read answers catalog.read.
func (c *Catalog) Read(ctx context.Context, query any) (any, error) {
	q, _ := query.(string)
	if pyStrip(q) == "" {
		return nil, &core.Denied{Rule: "READ-QUERY", Message: "empty catalog query", Hint: "Pass search words, or a filter like kind=component."}
	}
	if utf8.RuneCountInString(q) > maxQueryChars {
		return nil, &core.Denied{Rule: "READ-QUERY", Message: "catalog query longer than 500 characters", Hint: "Shorten the query."}
	}
	token, err := readToken(c.TokenFile)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}}
	if strings.ContainsAny(token, "\r\n") {
		return nil, headerError()
	}
	status, body, over, err := get(ctx, c.hc, c.lim, c.Timeout, c.base+catalogPath+"?"+catalogQuery(q), hdr, maxCatalogBody)
	if err != nil {
		return nil, &ReadError{Msg: "catalog unreachable: " + reason(err)}
	}
	switch {
	case status == 401 || status == 403:
		return nil, &core.Denied{Rule: "READ-CATALOG-403", Message: fmt.Sprintf("Backstage refused Clearance's identity (%d)", status),
			Hint: "Clearance may read the catalog only; ask a human to check Backstage's externalAccess."}
	case status == 400:
		return map[string]any{"error": "bad-query", "message": clip(decodeUTF8Replace(body), MaxMessage)}, nil
	case status != 200:
		return nil, &ReadError{Msg: fmt.Sprintf("catalog %d", status)}
	}
	if over {
		return nil, valueError(errors.New("the answer is larger than the gateway reads"))
	}
	doc, err := decodeJSON(body)
	if err != nil {
		return nil, valueError(err)
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, &dispatch.KindError{Kind: "AttributeError", Err: errors.New("the catalog's answer is not an object")}
	}
	var items []any
	switch x := m["items"].(type) {
	case nil:
	case []any:
		items = x
	default:
		// Python slices whatever is there: a string yields characters (and fails on the first), a mapping raises KeyError
		// (3.12 and later: a slice is hashable), a number or true a TypeError. A falsy value is no items.
		switch {
		case !pyTruthy(x):
		case isString(x):
			return nil, &dispatch.KindError{Kind: "AttributeError", Err: errors.New("an entity is not an object")}
		case isMap(x):
			return nil, &dispatch.KindError{Kind: "KeyError", Err: errors.New("items is a mapping")}
		default:
			return nil, &dispatch.KindError{Kind: "TypeError", Err: errors.New("items is not a list")}
		}
	}
	shown := items
	if len(shown) > MaxEntities {
		shown = shown[:MaxEntities]
	}
	entities := make([]any, 0, len(shown))
	for _, e := range shown {
		em, ok := e.(map[string]any)
		if !ok {
			return nil, &dispatch.KindError{Kind: "AttributeError", Err: errors.New("an entity is not an object")}
		}
		entities = append(entities, entity(em))
	}
	page, _ := m["pageInfo"].(map[string]any)
	return map[string]any{"entities": entities, "totalItems": m["totalItems"],
		"truncated": pyTruthy(page["nextCursor"]) || len(items) > MaxEntities}, nil
}

// obj is `v or {}` for a value that should be an object.
func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// entity is reads._entity: a summary, the keys whose value is None left out.
func entity(e map[string]any) map[string]any {
	md, spec := obj(e["metadata"]), obj(e["spec"])
	kind, hasKind := e["kind"]
	if !hasKind {
		kind = ""
	}
	ns, hasNS := md["namespace"]
	if !hasNS {
		ns = "default"
	}
	out := map[string]any{
		"ref":         strings.ToLower(pyStr(kind)) + ":" + pyStr(ns) + "/" + pyStr(md["name"]),
		"kind":        e["kind"],
		"title":       md["title"],
		"description": clip(md["description"], MaxMessage),
	}
	for _, k := range []string{"type", "owner", "lifecycle", "system"} {
		out[k] = spec[k]
	}
	for k, v := range out {
		if v == nil {
			delete(out, k)
		}
	}
	return out
}

func isString(v any) bool { _, ok := v.(string); return ok }
func isMap(v any) bool    { _, ok := v.(map[string]any); return ok }
