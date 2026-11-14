// Package backends are the adapters behind tools. Each backend owns its HTTP client: its own timeout and its own
// connection pool, so a slow Loki cannot starve Prometheus, and neither can hold a dispatch past its deadline (every
// request also carries the call's context). This slice ports the two read adapters that need only a URL,
// metrics.query (Prometheus) and logs.query (Loki), exactly as reads.PrometheusAPI and reads.LokiAPI answer. A tool
// whose backend is not here has no handler, so a call to it is refused as having no backend, as in the Python gateway
// for a deployment that does not configure it. Nothing registers a fake.
package backends

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

const (
	MaxQuery   = 4096
	MaxSeries  = 200
	MaxLines   = 200
	MaxLine    = 2000
	MaxMessage = 500
	maxBody    = 16 << 20
)

// HTTP is one backend's client.
type HTTP struct {
	Base    string
	Timeout time.Duration
	Client  *http.Client
}

// NewHTTP builds a backend client with a pool of its own.
func NewHTTP(base string, timeout time.Duration) *HTTP {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &HTTP{Base: strings.TrimRight(base, "/"), Timeout: timeout, Client: &http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ReadError is a read backend that failed or could not be reached. Never shown to the agent. Its text is the Python
// ReadError's ("PromQL backend unreachable: URLError", "LogQL backend 502"): the evidence on the call's outcome record
// and in the gateway's log. It never carries the backend's address, an answer's body or the transport's own message.
type ReadError struct{ Msg string }

func (e *ReadError) Error() string { return e.Msg }

// Kind is the Python class name, so the outcome's result reads error:ReadError: ... in either gateway.
func (e *ReadError) Kind() string { return "ReadError" }

// Evidence is the text, clipped as Python's BackendError.evidence clips it.
func (e *ReadError) Evidence() string { return dispatch.ClipEvidence(e.Msg) }

// reason is the class of a transport failure, in the names the Python gateway's record carries: urllib raises a
// URLError for a refused or failed connection and a TimeoutError for a timeout.
func reason(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "TimeoutError"
	}
	return "URLError"
}

func clip(s any, n int) any {
	if str, ok := s.(string); ok && utf8.RuneCountInString(str) > n {
		return string([]rune(str)[:n]) + "..."
	}
	return s
}

// query returns (true, data) on success; (false, an answer for the agent) when the server rejected the query.
func (h *HTTP) query(ctx context.Context, path string, q url.Values, what string) (bool, map[string]any, error) {
	query := q.Get("query")
	if strings.TrimSpace(query) == "" {
		return false, nil, &core.Denied{Rule: "READ-QUERY", Message: "empty " + what + " query", Hint: "Pass a " + what + " expression."}
	}
	if utf8.RuneCountInString(query) > MaxQuery {
		return false, nil, &core.Denied{Rule: "READ-QUERY", Message: fmt.Sprintf("%s query longer than %d characters", what, MaxQuery), Hint: "Shorten the query."}
	}
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Base+path+"?"+q.Encode(), nil)
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return false, nil, &ReadError{Msg: what + " backend unreachable: " + reason(err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	if resp.StatusCode == 200 && doc["status"] == "success" {
		data, _ := doc["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}
		return true, data, nil
	}
	if resp.StatusCode == 400 || resp.StatusCode == 422 || (resp.StatusCode == 503 && doc["errorType"] == "timeout") {
		kind, _ := doc["errorType"].(string)
		if kind == "" {
			kind = "bad_data"
		}
		var msg any = doc["error"]
		if m, _ := msg.(string); m == "" {
			b := body
			if len(b) > MaxMessage {
				b = b[:MaxMessage]
			}
			msg = strings.ToValidUTF8(string(b), "�")
		}
		return false, map[string]any{"error": kind, "message": clip(msg, MaxMessage)}, nil
	}
	return false, nil, &ReadError{Msg: fmt.Sprintf("%s backend %d", what, resp.StatusCode)}
}

// Promql is an instant PromQL query (GET /api/v1/query).
func (h *HTTP) Promql(ctx context.Context, q string) (any, error) {
	ok, data, err := h.query(ctx, "/api/v1/query", url.Values{"query": {q}, "timeout": {"20s"}}, "PromQL")
	if err != nil {
		return nil, err
	}
	if !ok {
		return data, nil
	}
	rtype, _ := data["resultType"].(string)
	if list, isList := data["result"].([]any); isList && (rtype == "vector" || rtype == "matrix") {
		out := list
		if len(out) > MaxSeries {
			out = out[:MaxSeries]
		}
		return map[string]any{"resultType": rtype, "result": out, "series": len(list), "truncated": len(list) > MaxSeries}, nil
	}
	return map[string]any{"resultType": data["resultType"], "result": data["result"], "truncated": false}, nil
}

// Logql is a LogQL range query over the last hour, newest first. One line more than is returned is asked for, so a
// result Loki itself cut at the limit still says truncated.
func (h *HTTP) Logql(ctx context.Context, q string) (any, error) {
	ok, data, err := h.query(ctx, "/loki/api/v1/query_range", url.Values{"query": {q}, "since": {"1h"},
		"limit": {fmt.Sprint(MaxLines + 1)}, "direction": {"backward"}}, "LogQL")
	if err != nil {
		return nil, err
	}
	if !ok {
		return data, nil
	}
	rtype, _ := data["resultType"].(string)
	result, _ := data["result"].([]any)
	if rtype != "streams" {
		out := result
		if len(out) > MaxSeries {
			out = out[:MaxSeries]
		}
		return map[string]any{"resultType": data["resultType"], "result": out, "truncated": len(result) > MaxSeries}, nil
	}
	out := []any{}
	lines, truncated := 0, false
	for _, st := range result {
		sm, _ := st.(map[string]any)
		values := []any{}
		vs, _ := sm["values"].([]any)
		for _, v := range vs {
			if lines >= MaxLines {
				truncated = true
				break
			}
			pair, _ := v.([]any)
			if len(pair) == 2 {
				values = append(values, []any{pair[0], clip(pair[1], MaxLine)})
				lines++
			}
		}
		if len(values) > 0 {
			out = append(out, map[string]any{"stream": sm["stream"], "values": values})
		}
		if truncated {
			break
		}
	}
	return map[string]any{"resultType": rtype, "result": out, "lines": lines, "truncated": truncated}, nil
}

// Config says which backends this deployment has.
type Config struct {
	PrometheusURL string
	LokiURL       string
	Timeout       time.Duration
}

// Handlers builds a handler per tool whose backend is configured.
func Handlers(c Config) map[string]dispatch.Handler {
	h := map[string]dispatch.Handler{}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.PrometheusURL != "" {
		p := NewHTTP(c.PrometheusURL, c.Timeout)
		h["metrics.query"] = func(ctx context.Context, _ *session.Session, a map[string]any) (any, error) {
			q, _ := a["promql"].(string)
			return p.Promql(ctx, q)
		}
	}
	if c.LokiURL != "" {
		l := NewHTTP(c.LokiURL, c.Timeout)
		h["logs.query"] = func(ctx context.Context, _ *session.Session, a map[string]any) (any, error) {
			q, _ := a["logql"].(string)
			return l.Logql(ctx, q)
		}
	}
	return h
}
