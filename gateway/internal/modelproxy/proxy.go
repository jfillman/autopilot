package modelproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	anthropicVersion = "2023-06-01"
	// MaxBody caps a request body (the proxy reads it whole to rewrite `model` and estimate tokens).
	MaxBody = 4 << 20
	// maxCapture caps what is kept of a non-streamed response, to read its usage; the response itself is never held.
	maxCapture  = 4 << 20
	streamChunk = 16 << 10
)

// Proxy is the model proxy over HTTP: Anthropic's Messages API (POST /v1/messages, plain or streaming). The response
// body is relayed as it arrives, chunk by chunk with a flush, for streamed and plain calls alike; the proxy keeps only
// a bounded copy of a plain response to read its usage, and a running meter over a stream's events.
type Proxy struct {
	Models  *Models
	KeyFile string
	// Timeout bounds one upstream call end to end (a stream included).
	Timeout time.Duration
	// HTTP is the upstream client, with its own connection pool. Its Timeout must be zero (streams are long).
	HTTP *http.Client
}

// NewProxy builds a proxy with its own upstream pool.
func NewProxy(m *Models, keyFile string, timeout time.Duration) *Proxy {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Proxy{Models: m, KeyFile: keyFile, Timeout: timeout, HTTP: &http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Handler serves POST /v1/messages and GET /healthz.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", p.messages)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		routes := []string{}
		for r := range p.Models.Routes {
			routes = append(routes, r)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "routes": routes})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, kind, message, rule string) {
	e := map[string]any{"type": kind, "message": message}
	if rule != "" {
		e["rule"] = rule
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": e})
}

// betas are the client's anthropic-beta values the provider is sent: every one but OAuth's, since the proxy calls with
// the deployment's API key, never a person's OAuth token.
func betas(header string) string {
	var keep []string
	for _, b := range strings.Split(header, ",") {
		if b = strings.TrimSpace(b); b != "" && !strings.HasPrefix(b, "oauth-") {
			keep = append(keep, b)
		}
	}
	return strings.Join(keep, ",")
}

// estimate is the tokens a call may use before it is made: its max_tokens plus its input at ~4 bytes a token.
func estimate(payload map[string]any, raw []byte) int64 {
	var mt int64
	if n, ok := payload["max_tokens"].(json.Number); ok {
		var err error
		if mt, err = n.Int64(); err != nil {
			if f, ferr := n.Float64(); ferr == nil && f > 0 {
				mt = math.MaxInt64 / 2 // Python ints are unbounded; a number beyond int64 saturates and is refused as over budget
			}
		}
	}
	if mt < 0 {
		mt = 0
	}
	return mt + int64(len(raw))/4
}

func resultOf(status int) string {
	if status >= 200 && status < 300 {
		return "ok"
	}
	return "error:Upstream" + itoa(status)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (p *Proxy) messages(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("x-api-key")
	if token == "" {
		if a := r.Header.Get("authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
			token = a[7:]
		}
	}
	sessionID := r.Header.Get("x-hangar-session")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		apiError(w, 413, "request_too_large", "request body too large", "")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		apiError(w, 400, "invalid_request_error", "body is not JSON", "")
		return
	}
	model, _ := payload["model"].(string)
	if payload == nil || model == "" {
		apiError(w, 400, "invalid_request_error", "a Messages API body with a model is required", "")
		return
	}
	if token == "" || sessionID == "" {
		apiError(w, 401, "authentication_error", "x-api-key (the run's model-proxy token) and x-hangar-session are required", "")
		return
	}
	ctx := r.Context()
	// The decision and the final charge each run alone on their session, as a tool call does, so the budget is checked
	// and charged on a state nobody else changes; the session is free while the response streams.
	unlock := p.lock(sessionID)
	d, err := p.Models.Authorize(ctx, token, sessionID, model, estimate(payload, raw))
	p.save(ctx, sessionID)
	unlock()
	if err != nil {
		apiError(w, 401, "authentication_error", "token refused", "")
		return
	}
	switch {
	case !d.Allow && d.Rule == "CORE-UNAVAILABLE":
		apiError(w, 503, "api_error", d.Message, d.Rule)
		return
	case !d.Allow && d.Rule == "M007":
		apiError(w, 429, "rate_limit_error", d.Message, d.Rule)
		return
	case !d.Allow:
		apiError(w, 403, "permission_error", d.Message, d.Rule)
		return
	}
	streamed, _ := payload["stream"].(bool)
	if d.Route.Backend != "anthropic" {
		p.report(ctx, d, sessionID, "error:UnsupportedBackend", streamed, 0, nil, "")
		apiError(w, 501, "api_error", "backend \""+d.Route.Backend+"\" is not forwarded by this proxy", "")
		return
	}
	payload["model"] = d.Route.UpstreamModel
	body, _ := json.Marshal(payload)
	key, err := os.ReadFile(p.KeyFile)
	if err != nil {
		p.report(ctx, d, sessionID, "error:ModelUnreachable", streamed, 0, nil, "")
		apiError(w, 502, "api_error", "the model provider is unreachable", "")
		return
	}
	uctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(uctx, http.MethodPost, strings.TrimRight(d.Route.Upstream, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		p.report(ctx, d, sessionID, "error:ModelUnreachable", streamed, 0, nil, "")
		apiError(w, 502, "api_error", "the model provider is unreachable", "")
		return
	}
	req.Header.Set("x-api-key", strings.TrimSpace(string(key)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", firstNonEmpty(r.Header.Get("anthropic-version"), anthropicVersion))
	if b := betas(r.Header.Get("anthropic-beta")); b != "" {
		req.Header.Set("anthropic-beta", b)
	}
	if streamed {
		req.Header.Set("accept", "text/event-stream")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		p.report(ctx, d, sessionID, "error:ModelUnreachable", streamed, 0, nil, "")
		apiError(w, 502, "api_error", "the model provider is unreachable", "")
		return
	}
	defer resp.Body.Close()

	// Relay: headers first, then each chunk as it arrives. Nothing here waits for the end of the body.
	ct := resp.Header.Get("content-type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("content-type", ct)
	if streamed && resp.StatusCode == 200 {
		w.Header().Set("cache-control", "no-cache")
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	var meter StreamUsage
	var capture bytes.Buffer
	buf := make([]byte, streamChunk)
	ended := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if streamed && resp.StatusCode == 200 {
				meter.Feed(chunk)
			} else if capture.Len() < maxCapture {
				capture.Write(chunk[:min(n, maxCapture-capture.Len())])
			}
			if _, werr := w.Write(chunk); werr != nil {
				break // the run went away
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			ended = errors.Is(rerr, io.EOF)
			break
		}
	}

	if streamed && resp.StatusCode == 200 {
		result := "ok"
		if !ended {
			// A stream that did not run to its end (the run went away, the provider dropped it) is not "ok"; what was
			// metered before it broke is still charged and recorded.
			result = "error:StreamInterrupted"
		}
		p.report(ctx, d, sessionID, result, true, resp.StatusCode, meter.Usage(), meter.Model)
		return
	}
	var doc map[string]any
	dd := json.NewDecoder(bytes.NewReader(capture.Bytes()))
	dd.UseNumber()
	jerr := dd.Decode(&doc)
	result := resultOf(resp.StatusCode)
	switch {
	case !ended:
		result = "error:StreamInterrupted"
	case jerr != nil && resp.StatusCode < 300:
		result = "error:BadUpstreamAnswer"
	}
	var usage map[string]any
	if u, ok := doc["usage"].(map[string]any); ok {
		usage = u
	}
	answered, _ := doc["model"].(string)
	p.report(ctx, d, sessionID, result, streamed, resp.StatusCode, usage, answered)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// report is the call's one outcome. A call that used tokens is charged first (the budget, the session's running
// cost) and its usage and charge go on this outcome with the model that answered (the answer's own `model`, else the
// route's upstream model).
func (p *Proxy) report(ctx context.Context, d Decision, sessionID, result string, streamed bool, status int, usage map[string]any, answered string) {
	defer p.lock(sessionID)()
	defer p.save(ctx, sessionID)
	version := answered
	if version == "" {
		version = d.Route.UpstreamModel
	}
	var rec map[string]int64
	if len(usage) > 0 {
		rec = UsageRecord(usage)
		p.Models.RecordUsage(sessionID, rec["charged_tokens"], rec)
	}
	p.Models.Outcome(ctx, d.DecisionID, result, streamed, status, rec, version)
}

// lock takes the session's operation lock (a no-op for an unknown session) and returns its release.
func (p *Proxy) lock(sessionID string) func() {
	if s := p.Models.G.Store.Get(sessionID); s != nil {
		return s.Serial()
	}
	return func() {}
}

func (p *Proxy) save(ctx context.Context, sessionID string) {
	if s := p.Models.G.Store.Get(sessionID); s != nil {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := p.Models.G.Store.Flush(fctx, s); err != nil {
			p.Models.G.OnPersistError(err)
		}
	}
}

// StreamUsage is the usage of a streaming Messages answer, read from its server-sent events as they pass:
// message_start carries the input side (input, cache write, cache read), message_delta the output so far
// (cumulative, and on newer APIs the input side again). Only `data:` lines are parsed; a line split across chunks is
// joined first.
type StreamUsage struct {
	usage map[string]any
	Model string
	buf   []byte
}

// Feed adds a chunk.
func (m *StreamUsage) Feed(chunk []byte) {
	m.buf = append(m.buf, chunk...)
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		line := m.buf[:i]
		m.buf = m.buf[i+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line[5:]))
		dec.UseNumber()
		var ev map[string]any
		if dec.Decode(&ev) != nil || ev == nil {
			continue
		}
		typ, _ := ev["type"].(string)
		var u any
		if typ == "message_start" {
			msg, _ := ev["message"].(map[string]any)
			if mdl, ok := msg["model"].(string); ok {
				m.Model = mdl
			}
			u = msg["usage"]
		} else {
			u = ev["usage"]
		}
		if um, ok := u.(map[string]any); ok && (typ == "message_start" || typ == "message_delta") {
			if m.usage == nil {
				m.usage = map[string]any{}
			}
			for k, v := range um {
				if _, num := v.(json.Number); num {
					m.usage[k] = v
				}
			}
		}
	}
	if len(m.buf) > 1<<20 { // a line this long is not an event; do not grow without bound
		m.buf = nil
	}
}

// Usage is what was metered.
func (m *StreamUsage) Usage() map[string]any { return m.usage }
