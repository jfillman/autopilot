// Package otlplogs copies the gateway's log lines to an OpenTelemetry collector over OTLP/HTTP, JSON (src/autopilot/otlp_logs.py), so
// they reach Loki: the resource attributes (service.name, then OTEL_RESOURCE_ATTRIBUTES), the scope name ("autopilot"), the severity
// numbers and the body (the line, cut at MaxBody characters) are Python's.
//
// It never blocks a request: Handle puts the line on a bounded queue (QueueMax) and a goroutine posts batches (BatchMax lines, or every
// FlushSeconds) with a deadline; a full queue drops the line and counts it, and a failed post drops the batch and counts it, so a
// collector outage costs log lines, never the gateway.
//
// What differs from Python, on purpose:
//   - the endpoint must be https; plain http needs OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP=1 (off by default; Python takes any http(s) URL);
//   - nothing is followed on a redirect, and the collector's answer is not read past a few KiB;
//   - a line is redacted before it is queued (Redact): a field named like a credential, or a value shaped like one, never leaves the
//     process, and neither does a request body;
//   - the `thread` attribute is empty (Go has no thread names), and a line is its message followed by its fields as key=value.
package otlplogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// The limits are Python's (otlp_logs.py).
const (
	QueueMax     = 2000
	BatchMax     = 200
	FlushSeconds = 2.0
	Timeout      = 5 * time.Second
	MaxBody      = 4096
)

// Post sends one request; it returns the HTTP status.
type Post func(ctx context.Context, url string, body []byte) (int, error)

// ResourceAttributes is resource_attributes(): OTEL_RESOURCE_ATTRIBUTES (k=v,k=v) and service.name, which OTEL_SERVICE_NAME wins.
func ResourceAttributes(env func(string) string, defaultService string) map[string]string {
	attrs := map[string]string{}
	for _, pair := range strings.Split(env("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		k, v, ok := strings.Cut(pair, "=")
		k, v = pyStrip(k), pyStrip(v)
		if ok && k != "" && v != "" {
			attrs[k] = v
		}
	}
	name := env("OTEL_SERVICE_NAME")
	if name == "" {
		name = attrs["service.name"]
	}
	if name == "" {
		name = defaultService
	}
	attrs["service.name"] = name
	return attrs
}

func pyStrip(s string) string { return strings.TrimSpace(s) }

// Config is what New needs.
type Config struct {
	Endpoint  string
	Resource  map[string]string
	Logger    string // the `logger` attribute ("autopilot.api")
	AllowHTTP bool
	Post      Post // tests; nil posts over http
	Flush     time.Duration
	// Secrets are literal values (a token read from a file) that must never appear in a line.
	Secrets func() []string
}

// CheckEndpoint is the start-up rule: an http(s) URL with a host and no credentials, and https unless plain http was allowed.
func CheckEndpoint(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT is an http(s) URL with a host, without credentials")
	}
	if u.Scheme != "https" && !allowHTTP {
		return errors.New("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT must be an https URL (a plain http collector needs OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP=1)")
	}
	return nil
}

// Exporter queues and posts the lines.
type Exporter struct {
	cfg  Config
	q    chan item
	stop chan struct{}
	done chan struct{}
	post Post

	dropped, sent atomic.Int64
	closeOnce     sync.Once
}

type item struct {
	nanos string
	sev   int
	text  string
	body  string
}

// New starts the exporter (cfg.Endpoint was checked by CheckEndpoint).
func New(cfg Config) *Exporter {
	e := &Exporter{cfg: cfg, q: make(chan item, QueueMax), stop: make(chan struct{}), done: make(chan struct{}), post: cfg.Post}
	if e.post == nil {
		hc := &http.Client{Timeout: Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		e.post = func(ctx context.Context, u string, body []byte) (int, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
			if err != nil {
				return 0, err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := hc.Do(req)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			return resp.StatusCode, nil
		}
	}
	flush := cfg.Flush
	if flush <= 0 {
		flush = time.Duration(FlushSeconds * float64(time.Second))
	}
	go e.loop(flush)
	return e
}

// Stats are the lines sent and the lines dropped (the queue was full, or their batch's post failed).
func (e *Exporter) Stats() (sent, dropped int64) { return e.sent.Load(), e.dropped.Load() }

// Severity is the OTLP severity number of a level, and its text (Python's logging level names).
func Severity(l slog.Level) (int, string) {
	switch {
	case l < slog.LevelInfo:
		return 5, "DEBUG"
	case l < slog.LevelWarn:
		return 9, "INFO"
	case l < slog.LevelError:
		return 13, "WARNING"
	case l < slog.LevelError+4:
		return 17, "ERROR"
	}
	return 21, "CRITICAL"
}

func clipChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Enqueue puts one line on the queue; a full queue drops it. A line below INFO is not sent (DEBUG stays on stderr).
func (e *Exporter) Enqueue(t time.Time, l slog.Level, body string) {
	if l < slog.LevelInfo {
		return
	}
	n, text := Severity(l)
	it := item{nanos: strconv.FormatInt(t.UnixNano(), 10), sev: n, text: text, body: clipChars(Redact(body, e.secrets()...), MaxBody)}
	select {
	case e.q <- it:
	default:
		e.dropped.Add(1)
	}
}

func (e *Exporter) secrets() []string {
	if e.cfg.Secrets == nil {
		return nil
	}
	return e.cfg.Secrets()
}

type kv struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func str(k, v string) kv { return kv{k, map[string]any{"stringValue": v}} }

// Payload is the request body for a batch: compact, the resource attributes sorted by key.
func (e *Exporter) payload(batch []item) []byte {
	keys := make([]string, 0, len(e.cfg.Resource))
	for k := range e.cfg.Resource {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	res := make([]kv, 0, len(keys))
	for _, k := range keys {
		res = append(res, str(k, e.cfg.Resource[k]))
	}
	recs := make([]map[string]any, 0, len(batch))
	for _, it := range batch {
		recs = append(recs, map[string]any{"timeUnixNano": it.nanos, "severityNumber": it.sev, "severityText": it.text,
			"body": map[string]any{"stringValue": it.body}, "attributes": []kv{str("logger", e.cfg.Logger), str("thread", "")}})
	}
	doc := map[string]any{"resourceLogs": []any{map[string]any{"resource": map[string]any{"attributes": res},
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "autopilot"}, "logRecords": recs}}}}}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(doc)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// Requests are the bodies FlushOnce would post for what is queued now, in batches (the equivalence test reads them).
func (e *Exporter) FlushOnce(ctx context.Context) int {
	n := 0
	for {
		var batch []item
	fill:
		for len(batch) < BatchMax {
			select {
			case it := <-e.q:
				batch = append(batch, it)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			return n
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		status, err := e.post(cctx, e.cfg.Endpoint, e.payload(batch))
		cancel()
		if err == nil && status >= 200 && status < 300 {
			e.sent.Add(int64(len(batch)))
			n += len(batch)
		} else {
			e.dropped.Add(int64(len(batch)))
		}
	}
}

func (e *Exporter) loop(every time.Duration) {
	defer close(e.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			func() {
				defer func() { _ = recover() }() // a bug in a post is a lost batch, never a dead exporter
				e.FlushOnce(context.Background())
			}()
		}
	}
}

// Close stops the loop and sends what is queued, within the post timeout.
func (e *Exporter) Close() {
	e.closeOnce.Do(func() {
		close(e.stop)
		<-e.done
		ctx, cancel := context.WithTimeout(context.Background(), Timeout+time.Second)
		defer cancel()
		e.FlushOnce(ctx)
	})
}

// Handler is a slog.Handler that writes through to Inner and copies each line at INFO and above to the exporter.
type Handler struct {
	Inner slog.Handler
	X     *Exporter
	attrs []slog.Attr
	group string
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.Inner.Enabled(ctx, l) }

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		var b strings.Builder
		b.WriteString(r.Message)
		add := func(a slog.Attr) { writeAttr(&b, h.group, a) }
		for _, a := range h.attrs {
			add(a)
		}
		r.Attrs(func(a slog.Attr) bool { add(a); return true })
		t := r.Time
		if t.IsZero() {
			t = time.Now()
		}
		h.X.Enqueue(t, r.Level, b.String())
	}
	return h.Inner.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.Inner = h.Inner.WithAttrs(attrs)
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &c
}

func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.Inner = h.Inner.WithGroup(name)
	if name != "" {
		c.group = h.group + name + "."
	}
	return &c
}

func writeAttr(b *strings.Builder, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			writeAttr(b, group+a.Key+".", g)
		}
		return
	}
	if a.Key == "" {
		return
	}
	key := group + a.Key
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	if SensitiveKey(key) {
		b.WriteString(Redacted)
		return
	}
	v := a.Value.String()
	if strings.ContainsAny(v, " \t\n\"=") {
		v = fmt.Sprintf("%q", v)
	}
	b.WriteString(v)
}
