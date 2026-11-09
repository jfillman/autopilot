// Package mcpserver is the gateway's MCP surface on the official Go SDK (github.com/modelcontextprotocol/go-sdk):
// streamable HTTP, stateless, JSON responses, a bearer token verified before any tool runs. Deliberately thin, as the
// Python server is: the tools only delegate to dispatch.Gateway, which owns every decision.
//
//	tools_list    what this deployment offers, with tiers and required arguments (optionally one session's)
//	agents_list   the agent definitions: what each is for and where it runs
//	session_open  open a session for an agent definition, optionally narrowing it with requested limits
//	call          call one tool inside a session
//
// run_finish and session_close, which need the run backend, are not in this slice (see docs/admin/go-gateway.md).
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const instructions = "Governed access to Hangar. Open a session, then call tools inside it. Every call is checked " +
	"and audited. A denial names a rule id and a hint; do not retry a denied call unchanged."

func tokenOf(req *mcp.CallToolRequest) (string, error) {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		if t, ok := req.Extra.TokenInfo.Extra["token"].(string); ok && t != "" {
			return t, nil
		}
	}
	return "", &core.AuthError{Msg: "no verified bearer token on this request"}
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// value wraps a tool's return value the way the Python MCP SDK does: an object is the structured content as it is, any
// other value is wrapped in {"result": ...}; the first text content is the value as JSON.
func value(v any) *mcp.CallToolResult {
	raw, _ := json.Marshal(v)
	structured := v
	var probe any
	_ = json.Unmarshal(raw, &probe)
	if _, isObj := probe.(map[string]any); !isObj {
		structured = map[string]any{"result": probe}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}, StructuredContent: structured}
}

func failure(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func decode(req *mcp.CallToolRequest, into any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	d := json.NewDecoder(bytesReader(req.Params.Arguments))
	d.UseNumber() // keep numbers exactly as sent: the call's arguments digest is over them
	return d.Decode(into)
}

// Server builds the MCP server over a gateway.
func Server(g *dispatch.Gateway) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "clearance", Version: "1"}, &mcp.ServerOptions{Instructions: instructions})

	s.AddTool(&mcp.Tool{Name: "tools_list", Description: "List the tools this deployment offers, with their tier and required arguments. " +
		"With `session` (one of your own), only the tools that session may call and this deployment runs.",
		InputSchema: schema(map[string]any{"session": map[string]any{"type": "string"}})},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			token, err := tokenOf(req)
			if err != nil {
				return failure("unauthenticated"), nil
			}
			var in struct {
				Session string `json:"session"`
			}
			if err := decode(req, &in); err != nil {
				return failure("invalid arguments"), nil
			}
			var names map[string]bool
			if in.Session != "" {
				n, ok, err := g.SessionTools(ctx, token, in.Session)
				if err != nil {
					return failure("token not accepted"), nil
				}
				if !ok {
					return failure("no such session of yours"), nil
				}
				names = n
			}
			return value(g.ToolList(names)), nil
		})

	s.AddTool(&mcp.Tool{Name: "agents_list", Description: "List the agent definitions: what each is for (description), where it runs " +
		"(runs_in: \"client\" means your session is the agent, \"cluster\" means it also runs in-cluster), its kind, tier ceiling and tools. " +
		"Open a session on one with can_open_session true.", InputSchema: schema(map[string]any{})},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			token, err := tokenOf(req)
			if err != nil {
				return failure("unauthenticated"), nil
			}
			out, err := g.Agents(ctx, token)
			if err != nil {
				return failure("token not accepted"), nil
			}
			return value(out), nil
		})

	s.AddTool(&mcp.Tool{Name: "session_open", Description: "Open a session for an agent definition. `limits` may only narrow the definition's limits.",
		InputSchema: schema(map[string]any{"agent": map[string]any{"type": "string"}, "limits": map[string]any{"type": []string{"object", "null"}}}, "agent")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			token, err := tokenOf(req)
			if err != nil {
				return failure("unauthenticated"), nil
			}
			var in struct {
				Agent  string          `json:"agent"`
				Limits json.RawMessage `json:"limits"`
			}
			if err := decode(req, &in); err != nil || in.Agent == "" {
				return failure("agent is required"), nil
			}
			if string(in.Limits) == "null" {
				in.Limits = nil
			}
			r, err := g.Open(ctx, token, in.Agent, in.Limits, "")
			return answer(r, err)
		})

	s.AddTool(&mcp.Tool{Name: "call", Description: "Call a tool inside a session. Returns ok, the decision (rule and hint on a denial), and data.",
		InputSchema: schema(map[string]any{"session": map[string]any{"type": "string"}, "tool": map[string]any{"type": "string"},
			"args": map[string]any{"type": []string{"object", "null"}}}, "session", "tool")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			token, err := tokenOf(req)
			if err != nil {
				return failure("unauthenticated"), nil
			}
			var in struct {
				Session string         `json:"session"`
				Tool    string         `json:"tool"`
				Args    map[string]any `json:"args"`
			}
			if err := decode(req, &in); err != nil || in.Session == "" || in.Tool == "" {
				return failure("session and tool are required"), nil
			}
			r, err := g.Call(ctx, token, in.Session, in.Tool, in.Args)
			return answer(r, err)
		})
	return s
}

func answer(r *dispatch.Result, err error) (*mcp.CallToolResult, error) {
	var ae *core.AuthError
	switch {
	case errors.As(err, &ae):
		return failure("token not accepted"), nil
	case err != nil:
		return failure("the gateway could not handle this call"), nil
	}
	return value(r), nil
}

// Handler serves the MCP endpoint at path (bearer auth on every request: 401 without a token the gateway accepts), and
// an unauthenticated GET /healthz for probes.
func Handler(g *dispatch.Gateway, path, resourceURL, issuerURL string) http.Handler {
	srv := Server(g)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	verify := func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		p, err := g.Auth.Authenticate(ctx, token)
		if err != nil {
			return nil, sdkauth.ErrInvalidToken
		}
		exp := auth.JWTExpiry(token)
		return &sdkauth.TokenInfo{UserID: p.ID, Expiration: exp, Extra: map[string]any{"token": token}}, nil
	}
	mux := http.NewServeMux()
	mux.Handle(path, sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{
		AllowMissingExpiration: true,
		ResourceMetadataURL:    resourceURL + "/.well-known/oauth-protected-resource"})(h))
	mux.Handle("GET /.well-known/oauth-protected-resource", sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource: resourceURL, AuthorizationServers: []string{issuerURL}, BearerMethodsSupported: []string{"header"}}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tools": g.Tools.Len(), "core": g.Core.BreakerState()})
	})
	return mux
}
