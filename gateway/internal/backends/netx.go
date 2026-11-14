package backends

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// maxJSONDepth bounds how deeply an upstream's JSON may nest before it is decoded. Python's json.loads fails near 1000
// levels; no answer a backend here returns nests past a handful.
const maxJSONDepth = 256

// depthOK reports whether b nests no deeper than limit, without decoding it.
func depthOK(b []byte, limit int) bool {
	depth, inStr, esc := 0, false, false
	for _, c := range b {
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '[' || c == '{':
			if depth++; depth > limit {
				return false
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return true
}

// decodeJSON is json.loads for an upstream's answer: one value and nothing after it, numbers kept as written, nesting
// bounded before decoding. Any failure is the one error a caller turns into Python's ValueError.
func decodeJSON(b []byte) (any, error) {
	if !depthOK(b, maxJSONDepth) {
		return nil, errors.New("JSON nests too deeply")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON value")
	}
	return v, nil
}

// valueError is Python's error for an answer that is not JSON (json.JSONDecodeError, a ValueError) or a header value it
// cannot send (ValueError): not a BackendError there, so the outcome reads error:JSONDecodeError with no evidence, and the
// agent is told only "backend error".
func valueError(err error) error { return &dispatch.KindError{Kind: "JSONDecodeError", Err: err} }

func headerError() error {
	return &dispatch.KindError{Kind: "ValueError", Err: errors.New("invalid header value")}
}

// fileError names an unreadable credential or CA file as Python's open() does (FileNotFoundError, PermissionError,
// OSError). The path and the file's text are never in it.
func fileError(err error) error {
	kind := "OSError"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		kind = "FileNotFoundError"
	case errors.Is(err, fs.ErrPermission):
		kind = "PermissionError"
	}
	return &dispatch.KindError{Kind: kind, Err: errors.New("a file the backend needs cannot be read")}
}

// client builds an HTTP client with a pool of its own for one backend. A redirect is never followed (a redirect target
// would receive whatever the request carried), compression is off (so a size cap counts what arrives), no proxy from the
// environment is used, and the dial, handshake and header sizes are bounded.
func client(tlsCfg *tls.Config, dial time.Duration, conns int) *http.Client {
	tr := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: dial, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        tlsCfg,
		MaxIdleConns:           conns,
		MaxIdleConnsPerHost:    conns,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  0,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// limiter bounds how many requests one backend has in flight, so a slow upstream holds at most that many of the
// gateway's calls and can never take every one.
type limiter chan struct{}

func newLimiter(n int) limiter { return make(limiter, n) }

func (l limiter) acquire(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l limiter) release() { <-l }

// get sends one GET and reads at most limit bytes of the answer (and says when there was more). The request carries the
// call's context and its own timeout; nothing of the transport's own message is returned, only its class.
func get(ctx context.Context, c *http.Client, lim limiter, timeout time.Duration, url string, hdr http.Header, limit int64) (status int, body []byte, over bool, err error) {
	if err = lim.acquire(ctx); err != nil {
		return 0, nil, false, err
	}
	defer lim.release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, false, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, false, err
	}
	if int64(len(b)) > limit {
		return resp.StatusCode, b[:limit], true, nil
	}
	return resp.StatusCode, b, false, nil
}

// decodeUTF8Replace is bytes.decode("utf-8", "replace"): each maximal run of bytes that could begin a character and then
// fails becomes one U+FFFD (a truncated three-byte character is one, two stray bytes are two), as Python's decoder does.
func decodeUTF8Replace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		if r, n := utf8.DecodeRune(b); r != utf8.RuneError || n > 1 {
			sb.Write(b[:n])
			b = b[n:]
			continue
		}
		sb.WriteRune(utf8.RuneError)
		b = b[invalidPrefix(b):]
	}
	return sb.String()
}

// invalidPrefix is the length of the longest prefix of b (at least 1) that is the start of a well-formed character: the
// lead byte and as many continuation bytes as the Unicode table allows after it.
func invalidPrefix(b []byte) int {
	lo, hi := byte(0x80), byte(0xbf)
	need := 0
	switch c := b[0]; {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c >= 0xe1 && c <= 0xec, c == 0xee, c == 0xef:
		need = 2
	case c == 0xed:
		need, hi = 2, 0x9f
	case c == 0xf0:
		need, lo = 3, 0x90
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	case c == 0xf4:
		need, hi = 3, 0x8f
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(b); n++ {
		if b[n] < lo || b[n] > hi {
			break
		}
		lo, hi = 0x80, 0xbf
	}
	return n
}
