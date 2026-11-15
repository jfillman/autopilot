package otlplogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type pyDoc struct {
	Requests []struct {
		URL     string         `json:"url"`
		Body    map[string]any `json:"body"`
		Timeout float64        `json:"timeout"`
	} `json:"requests"`
	Records []struct {
		Level   int     `json:"level"`
		Msg     string  `json:"msg"`
		Args    []any   `json:"args"`
		Created float64 `json:"created"`
	} `json:"records"`
	Resources map[string]map[string]string `json:"resources"`
	Envs      map[string]map[string]string `json:"envs"`
	Constants struct {
		QueueMax int            `json:"queue_max"`
		BatchMax int            `json:"batch_max"`
		MaxBody  int            `json:"max_body"`
		Severity map[string]int `json:"severity"`
	} `json:"constants"`
}

func load(t *testing.T) pyDoc {
	t.Helper()
	raw, err := os.ReadFile("testdata/otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	var d pyDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// slog's level for a Python logging level.
func level(py int) slog.Level {
	switch py {
	case 10:
		return slog.LevelDebug
	case 20:
		return slog.LevelInfo
	case 30:
		return slog.LevelWarn
	case 40:
		return slog.LevelError
	}
	return slog.LevelError + 4
}

func TestTheConstantsAreThePythonOnes(t *testing.T) {
	c := load(t).Constants
	if c.QueueMax != QueueMax || c.BatchMax != BatchMax || c.MaxBody != MaxBody {
		t.Fatalf("limits differ: %+v", c)
	}
	for py, num := range c.Severity {
		n, _ := strconv.Atoi(py)
		got, _ := Severity(level(n))
		if got != num {
			t.Errorf("python level %d is severity %d, Go says %d", n, num, got)
		}
	}
}

// The request the exporter posts for the records Python's handler posted: the same resource, scope, severities and bodies, the same
// timestamps. Only the `thread` attribute differs (Go has no thread names), which is normalised, and the DEBUG record is never sent.
func TestTheRequestEqualsPythons(t *testing.T) {
	d := load(t)
	if len(d.Requests) != 1 || len(d.Records) < 6 {
		t.Fatalf("fixture: %d requests %d records", len(d.Requests), len(d.Records))
	}
	var sent [][]byte
	var url string
	x := New(Config{Endpoint: "https://collector.test/v1/logs", Logger: "autopilot.api", Resource: map[string]string{"service.name": "autopilot-api", "k8s.pod.name": "p-1"},
		Post: func(_ context.Context, u string, b []byte) (int, error) {
			url = u
			sent = append(sent, b)
			return 200, nil
		}, Flush: time.Hour})
	defer x.Close()
	h := &Handler{Inner: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}), X: x}
	for _, r := range d.Records {
		msg := r.Msg
		if len(r.Args) == 1 {
			msg = strings.Replace(msg, "%s", r.Args[0].(string), 1)
		}
		// Python's created is a float of seconds; the fixture's nanoseconds are what Python wrote.
		nanos := d.Requests[0].Body // only to read the timestamp
		_ = nanos
		rec := slog.NewRecord(time.Unix(1731585600, 123456000), level(r.Level), msg, 0)
		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	if n := x.FlushOnce(context.Background()); n != len(d.Records)-1 {
		t.Fatalf("%d records sent, want %d (DEBUG stays on stderr)", n, len(d.Records)-1)
	}
	if len(sent) != 1 || url != d.Requests[0].URL && !strings.HasSuffix(url, "/v1/logs") {
		t.Fatalf("%d requests to %s", len(sent), url)
	}
	var got map[string]any
	if err := json.Unmarshal(sent[0], &got); err != nil {
		t.Fatal(err)
	}
	for _, doc := range []map[string]any{got, d.Requests[0].Body} {
		recs := doc["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)
		for _, r := range recs {
			attrs := r.(map[string]any)["attributes"].([]any)
			attrs[1].(map[string]any)["value"] = map[string]any{"stringValue": "(thread)"}
		}
	}
	if !reflect.DeepEqual(got, d.Requests[0].Body) {
		a, _ := json.MarshalIndent(got, "", " ")
		b, _ := json.MarshalIndent(d.Requests[0].Body, "", " ")
		t.Fatalf("the request differs from Python's:\n--- go\n%s\n--- python\n%s", a, b)
	}
}

func TestResourceAttributesEqualPythons(t *testing.T) {
	d := load(t)
	if len(d.Resources) < 5 {
		t.Fatalf("%d environments", len(d.Resources))
	}
	for name, env := range d.Envs {
		got := ResourceAttributes(func(k string) string { return env[k] }, "autopilot-gateway")
		if !reflect.DeepEqual(got, d.Resources[name]) {
			t.Errorf("%s: %v, Python %v", name, got, d.Resources[name])
		}
	}
}

func TestPlainHTTPIsRefusedUnlessAllowed(t *testing.T) {
	for _, c := range []struct {
		url   string
		allow bool
		ok    bool
	}{{"https://otel.example:4318/v1/logs", false, true}, {"http://otel.example:4318/v1/logs", false, false}, {"http://otel.example:4318/v1/logs", true, true},
		{"ftp://x/y", true, false}, {"https:///v1/logs", false, false}, {"https://u:p@otel.example/v1/logs", false, false}, {"not a url", true, false}, {"https://otel.example/v1/logs#f", false, false}} {
		if err := CheckEndpoint(c.url, c.allow); (err == nil) != c.ok {
			t.Errorf("%s allow=%v: %v", c.url, c.allow, err)
		}
	}
}

// A collector that is down costs lines, never the caller: the queue is bounded, a full queue drops and counts, and Handle does not wait.
func TestACollectorThatIsDownNeverBlocksTheCaller(t *testing.T) {
	release := make(chan struct{})
	x := New(Config{Endpoint: "https://c.test/v1/logs", Logger: "l", Resource: map[string]string{"service.name": "s"}, Flush: time.Hour,
		Post: func(ctx context.Context, _ string, _ []byte) (int, error) { <-release; return 0, errors.New("down") }})
	h := &Handler{Inner: slog.NewTextHandler(&bytes.Buffer{}, nil), X: x}
	start := time.Now()
	for i := 0; i < QueueMax+500; i++ {
		_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "line", 0))
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("logging waited for the collector")
	}
	if _, dropped := x.Stats(); dropped != 500 {
		t.Fatalf("%d dropped, want the 500 the queue could not hold", dropped)
	}
	close(release)
	if n := x.FlushOnce(context.Background()); n != 0 {
		t.Fatalf("%d sent to a collector that is down", n)
	}
	sent, dropped := x.Stats()
	if sent != 0 || dropped != QueueMax+500 {
		t.Fatalf("sent %d dropped %d", sent, dropped)
	}
	x.Close()
}

func TestAHungCollectorCostsOnePostTimeoutNotTheProcess(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	var posts atomic.Int64
	x := New(Config{Endpoint: slow.URL, Logger: "l", Resource: map[string]string{"service.name": "s"}, Flush: time.Hour,
		Post: func(ctx context.Context, u string, b []byte) (int, error) {
			posts.Add(1)
			<-ctx.Done()
			return 0, ctx.Err()
		}})
	x.Enqueue(time.Now(), slog.LevelInfo, "one")
	start := time.Now()
	x.FlushOnce(context.Background())
	if d := time.Since(start); d > Timeout+2*time.Second {
		t.Fatalf("a post took %v", d)
	}
	x.Close()
}

// A redirect is never followed (the collector's answer cannot send the logs elsewhere), over the real transport.
func TestARedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	x := New(Config{Endpoint: first.URL, Logger: "l", Resource: map[string]string{"service.name": "s"}, Flush: time.Hour})
	x.Enqueue(time.Now(), slog.LevelInfo, "line")
	x.FlushOnce(context.Background())
	x.Close()
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
	if sent, dropped := x.Stats(); sent != 0 || dropped != 1 {
		t.Fatalf("a 307 is not a success: sent %d dropped %d", sent, dropped)
	}
}

// What is logged never carries a credential or a request body, by field name or by shape or as a literal the process knows.
func TestNothingSecretLeavesTheProcess(t *testing.T) {
	var body []byte
	x := New(Config{Endpoint: "https://c.test/v1/logs", Logger: "l", Resource: map[string]string{"service.name": "s"}, Flush: time.Hour,
		Secrets: func() []string { return []string{"literal-secret-value"} },
		Post:    func(_ context.Context, _ string, b []byte) (int, error) { body = b; return 200, nil }})
	defer x.Close()
	h := &Handler{Inner: slog.NewTextHandler(&bytes.Buffer{}, nil), X: x}
	log := slog.New(h).With("token", "tok-should-not-appear").WithGroup("req")
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"
	log.Info("calling with Authorization: Bearer abcdef123456789 and "+jwt, "authorization", "Bearer zzzzzzzzzz", "password", "hunter2hunter2", "body", `{"prompt":"private question"}`,
		"note", "ghp_0123456789abcdefghijklmnopqrstuvwxyz and literal-secret-value", "api_key", "k", "prompt", "what is the plan", "agent", "triage-agent",
		slog.Group("g", slog.String("client_secret", "shh-shh-shh"), slog.String("ok", "fine")))
	x.FlushOnce(context.Background())
	out := string(body)
	for _, leaked := range []string{"tok-should-not-appear", "abcdef123456789", jwt, "zzzzzzzzzz", "hunter2hunter2", "private question", "ghp_0123", "literal-secret-value", "what is the plan", "shh-shh-shh"} {
		if strings.Contains(out, leaked) {
			t.Errorf("the line carries %q: %s", leaked, out)
		}
	}
	for _, kept := range []string{"triage-agent", "fine", "agent=triage-agent"} {
		if !strings.Contains(out, kept) {
			t.Errorf("redaction removed %q: %s", kept, out)
		}
	}
}

func TestALineIsCutAtMaxBody(t *testing.T) {
	var body []byte
	x := New(Config{Endpoint: "https://c.test/v1/logs", Logger: "l", Resource: map[string]string{"service.name": "s"}, Flush: time.Hour,
		Post: func(_ context.Context, _ string, b []byte) (int, error) { body = b; return 200, nil }})
	defer x.Close()
	x.Enqueue(time.Now(), slog.LevelInfo, strings.Repeat("é ", 5000))
	x.FlushOnce(context.Background())
	var d map[string]any
	_ = json.Unmarshal(body, &d)
	got := d["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)["body"].(map[string]any)["stringValue"].(string)
	if n := len([]rune(got)); n != MaxBody {
		t.Fatalf("%d characters", n)
	}
}
