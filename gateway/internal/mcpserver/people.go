package mcpserver

// The person-facing routes (engine/server.py create_http_app): a person's own token, or a delegate's naming them in
// X-Hangar-On-Behalf-Of. They are not behind the MCP bearer middleware (Python's custom routes are not either): each does its own
// authentication through dispatch.Gateway.Person, which refuses a workload that is not a listed delegate, so a run's token opens
// none of them. A token the authenticator refuses is 401; a refusal the person can act on is its status with
// {"error", "rule", "hint"}; nothing else about a failure is said (a 500 carries no detail).

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// MaxBody is the largest request body a route reads (Python: 64 KiB).
const MaxBody = 64 << 10

// OnBehalfOfHeader names the person a delegate acts for.
const OnBehalfOfHeader = "X-Hangar-On-Behalf-Of"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

// readBody reads the body (at most MaxBody) and decodes it as JSON with numbers kept as sent. ok is false when an answer has been
// written. An empty body is nil (the caller decides whether that is an error), a body that is not an object is nil too.
func readBody(w http.ResponseWriter, r *http.Request) (obj map[string]any, empty bool, ok bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body could not be read"})
		}
		return nil, false, false
	}
	if len(raw) == 0 {
		return nil, true, true
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || d.More() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not JSON"})
		return nil, false, false
	}
	obj, _ = v.(map[string]any)
	return obj, false, true
}

// people wraps a route: a bearer token is required (401), the answer of the operation is JSON with the status of its refusal.
func people(g *dispatch.Gateway, op func(w http.ResponseWriter, r *http.Request, token, onBehalfOf string) (any, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("a person's route panicked", "route", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the gateway could not handle this request"})
			}
		}()
		token := bearer(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "a bearer token is required"})
			return
		}
		out, status, err := op(w, r, token, r.Header.Get(OnBehalfOfHeader))
		var ae *core.AuthError
		var ie *dispatch.InboxError
		var ce *dispatch.ControlError
		switch {
		case errors.Is(err, errAnswered):
			return
		case errors.As(err, &ae):
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "token not accepted"})
		case errors.As(err, &ie):
			writeJSON(w, ie.Status, map[string]any{"error": ie.Message, "rule": ie.Rule, "hint": ie.Hint})
		case errors.As(err, &ce):
			writeJSON(w, ce.Status, map[string]any{"error": ce.Message, "rule": ce.Rule, "hint": ce.Hint})
		case err != nil:
			slog.Error("a person's route failed", "route", r.URL.Path, "err", errorKind(err))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the gateway could not handle this request"})
		default:
			if status == 0 {
				status = http.StatusOK
			}
			writeJSON(w, status, out)
		}
	}
}

// errAnswered is what an operation returns after it has written the answer itself (a refused body).
var errAnswered = errors.New("answered")

func errorKind(err error) string {
	switch {
	case errors.Is(err, errAnswered):
		return "answered"
	}
	return "error"
}

// addPeopleRoutes mounts the person-facing routes.
func addPeopleRoutes(mux *http.ServeMux, g *dispatch.Gateway) {
	mux.HandleFunc("GET /v1/human/requests", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		out, err := g.HumanRequests(r.Context(), token, who)
		return out, 0, err
	}))
	mux.HandleFunc("POST /v1/human/requests/{request_id}/answer", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		body, empty, ok := readBody(w, r)
		if !ok {
			return nil, 0, errAnswered
		}
		if empty { // Python: json.loads(b"") is a 400
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not JSON"})
			return nil, 0, errAnswered
		}
		out, err := g.HumanAnswer(r.Context(), token, r.PathValue("request_id"), body["answer"], who)
		return out, 0, err
	}))
	control := func(op func(token, id string, reason any, who string, r *http.Request) (map[string]any, error), id string) http.HandlerFunc {
		return people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
			body, _, ok := readBody(w, r)
			if !ok {
				return nil, 0, errAnswered
			}
			out, err := op(token, r.PathValue(id), body["reason"], who, r)
			return out, 0, err
		})
	}
	mux.HandleFunc("POST /v1/sessions/{session_id}/freeze", control(func(token, id string, reason any, who string, r *http.Request) (map[string]any, error) {
		return g.FreezeSession(r.Context(), token, id, reason, who)
	}, "session_id"))
	mux.HandleFunc("POST /v1/sessions/{session_id}/end", control(func(token, id string, reason any, who string, r *http.Request) (map[string]any, error) {
		return g.EndSession(r.Context(), token, id, reason, who)
	}, "session_id"))
	mux.HandleFunc("POST /v1/agents/{agent}/hold", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		body, _, ok := readBody(w, r)
		if !ok {
			return nil, 0, errAnswered
		}
		out, err := g.HoldTriggers(r.Context(), token, r.PathValue("agent"), body["reason"], body["hours"], who)
		return out, 0, err
	}))
	mux.HandleFunc("DELETE /v1/agents/{agent}/hold", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		out, err := g.ReleaseTriggers(r.Context(), token, r.PathValue("agent"), who)
		return out, 0, err
	}))
	mux.HandleFunc("POST /v1/agents/{agent}/runs", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		body, _, ok := readBody(w, r)
		if !ok {
			return nil, 0, errAnswered
		}
		out, err := g.RunAgent(r.Context(), token, r.PathValue("agent"), body["input"], who, body["document"], body["budget"])
		return out, http.StatusCreated, err
	}))
	mux.HandleFunc("GET /v1/preflight/cases", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		var agent any
		if q, present := r.URL.Query()["agent"]; present && len(q) > 0 {
			agent = q[0]
		}
		out, err := g.PreflightCaseList(r.Context(), token, agent)
		return out, 0, err
	}))
	mux.HandleFunc("POST /v1/preflight/score", people(g, func(w http.ResponseWriter, r *http.Request, token, who string) (any, int, error) {
		body, _, ok := readBody(w, r)
		if !ok {
			return nil, 0, errAnswered
		}
		var b any
		if body != nil {
			b = body
		}
		out, err := g.PreflightScore(r.Context(), token, b, who)
		return out, 0, err
	}))
}
