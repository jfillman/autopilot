package backends

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// The text of a read backend's failure is the Python ReadError's: it is the evidence on the call's outcome record, so it
// carries the failing status or a class of failure, and never the address, a body or the transport's own message.
func TestAReadFailureReadsAsThePythonReadErrorReads(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"secret":"token-in-a-body"}`))
	}))
	defer bad.Close()
	_, err := NewHTTP(bad.URL, time.Second).Promql(context.Background(), "up")
	var re *ReadError
	if !errors.As(err, &re) || re.Msg != "PromQL backend 502" || re.Kind() != "ReadError" || re.Evidence() != "PromQL backend 502" {
		t.Fatalf("%v", err)
	}
	if got := dispatch.ErrorResult(err); got != "error:ReadError: PromQL backend 502" {
		t.Fatalf("%s", got)
	}
	_, err = NewHTTP(bad.URL, time.Second).Logql(context.Background(), "{a=\"b\"}")
	if !errors.As(err, &re) || re.Msg != "LogQL backend 502" {
		t.Fatalf("%v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close() // a refused connection
	_, err = NewHTTP(url, time.Second).Promql(context.Background(), "up")
	if !errors.As(err, &re) || re.Msg != "PromQL backend unreachable: URLError" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(re.Msg, "127.0.0.1") {
		t.Fatalf("the address leaked into the evidence: %s", re.Msg)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	_, err = NewHTTP(slow.URL, 50*time.Millisecond).Promql(context.Background(), "up")
	if !errors.As(err, &re) || re.Msg != "PromQL backend unreachable: TimeoutError" {
		t.Fatalf("%v", err)
	}
}
