package backends

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// The write backends (glidepath-api, GitHub, the Kubernetes patch) share this one way of sending a request: through the
// backend's own client and limiter, under its own timeout and the call's context, reading an answer to a cap, and saying
// whether the request may have reached the server. Nothing is retried here; retry (below) is for reads only.

// reply is one answer: its status, its headers (for a rate limit) and its body read to a cap.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// transportError is a request that got no answer. sent says whether the request may have reached the server (a connection
// was made): a write that failed with sent set may have happened. bodyPhase says the failure came after the answer's
// headers, while the body was being read.
type transportError struct {
	err       error
	sent      bool
	bodyPhase bool
}

func (e *transportError) Error() string { return "request failed: " + osName(e.err) }
func (e *transportError) Unwrap() error { return e.err }

// osName is the name of the Python exception class a transport failure would be.
func osName(err error) string {
	var dns *net.DNSError
	var cert *x509.UnknownAuthorityError
	var host x509.HostnameError
	var tlsErr *tls.CertificateVerificationError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "TimeoutError"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ConnectionRefusedError"
	case errors.As(err, &dns):
		return "gaierror"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "ConnectionResetError"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "RemoteDisconnected"
	case errors.As(err, &cert), errors.As(err, &host), errors.As(err, &tlsErr):
		return "SSLCertVerificationError"
	case errors.Is(err, context.Canceled):
		return "CancelledError"
	}
	return "OSError"
}

// pyClass is type(e).__name__ for a transport failure as urllib raises it: a failure to connect (or to send) is a
// URLError; one while waiting for the answer is the OSError itself.
func (e *transportError) pyClass() string {
	if !e.sent {
		return "URLError"
	}
	return osName(e.err)
}

// urlErrorWhy is glidepath_api._why: the exception's type, and its cause's when urllib wrapped one
// ("URLError(ConnectionRefusedError)"); never its text.
func (e *transportError) urlErrorWhy() string {
	if !e.sent {
		return "URLError(" + osName(e.err) + ")"
	}
	return osName(e.err)
}

// send makes one request. A 2xx answer is read to at most capOK bytes, any other to capErr (Python reads 8 MiB and 64 KiB).
func send(ctx context.Context, hc *http.Client, lim limiter, timeout time.Duration, method, url string, hdr http.Header, body []byte, capOK, capErr int64) (*reply, error) {
	if err := lim.acquire(ctx); err != nil {
		return nil, &transportError{err: err}
	}
	defer lim.release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var conn atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { conn.Store(true) }})
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[http.CanonicalHeaderKey(k)] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &transportError{err: err, sent: conn.Load()}
	}
	defer resp.Body.Close()
	limit := capErr
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		limit = capOK
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, &transportError{err: err, sent: true, bodyPhase: true}
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// retryPolicy bounds the retries of a read: a few attempts, a backoff, and never a wait longer than maxWait (a rate limit
// that asks for longer is the answer, not something to sit out).
type retryPolicy struct {
	retries int
	base    time.Duration
	maxWait time.Duration
	now     func() time.Time
	sleep   func(context.Context, time.Duration) bool
}

func defaultRetry() retryPolicy {
	return retryPolicy{retries: 2, base: 250 * time.Millisecond, maxWait: 5 * time.Second, now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// wait says how long to wait before attempt n+1 after r, or false when r is final: a 429 or 5xx gateway answer, or a 403
// that says it is a rate limit. Retry-After (seconds) and GitHub's x-ratelimit-reset are respected; a wait over maxWait, or
// one the call's deadline cannot cover, is not taken.
func (p retryPolicy) wait(ctx context.Context, r *reply, n int) (time.Duration, bool) {
	if n >= p.retries {
		return 0, false
	}
	limited := r.header.Get("Retry-After") != "" || r.header.Get("X-RateLimit-Remaining") == "0"
	switch {
	case r.status == 429, r.status == 502, r.status == 503, r.status == 504:
	case r.status == 403 && limited:
	default:
		return 0, false
	}
	d := p.base << n
	if s := r.header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		}
	} else if reset := r.header.Get("X-RateLimit-Remaining"); reset == "0" {
		if at, err := strconv.ParseInt(strings.TrimSpace(r.header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			d = time.Unix(at, 0).Sub(p.now())
		}
	}
	if d < 0 {
		d = 0
	}
	if d > p.maxWait {
		return 0, false
	}
	if dl, ok := ctx.Deadline(); ok && p.now().Add(d+time.Second).After(dl) {
		return 0, false
	}
	return d, true
}

// sendRead is send for a GET, retried by the policy: only an answer that says "try again" is retried (never a transport
// failure after a connection, never a write).
func sendRead(ctx context.Context, p retryPolicy, hc *http.Client, lim limiter, timeout time.Duration, url string, hdr http.Header, capOK, capErr int64) (*reply, error) {
	for n := 0; ; n++ {
		r, err := send(ctx, hc, lim, timeout, http.MethodGet, url, hdr, nil, capOK, capErr)
		if err != nil {
			return nil, err
		}
		d, again := p.wait(ctx, r, n)
		if !again {
			return r, nil
		}
		if !p.sleep(ctx, d) {
			return r, nil
		}
	}
}

func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }

// redact removes a credential from an upstream's answer before the answer is read for text: an upstream that echoes the
// Authorization header into an error message cannot put the token into an error, a record or a result.
func redact(b []byte, secrets ...string) []byte {
	for _, s := range secrets {
		if len(s) >= 8 {
			b = bytes.ReplaceAll(b, []byte(s), []byte("[redacted]"))
		}
	}
	return b
}
