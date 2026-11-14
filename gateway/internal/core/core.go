// Package core is the one place the gateway talks to the Clearance core: the decision before every call, the outcome
// after it, the gateway's own records, and the outage policy. It is the Go side of autopilot.engine.core, on the Go
// client Clearance ships (github.com/jfillman/clearance/client). The gateway enforces; the core decides and records.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jfillman/clearance/client"
)

// Provider is the name the gateway is registered under at the core.
const Provider = "autopilot-gateway"

// DecisionTimeout is the decision path's timeout per attempt (Python: DECISION_TIMEOUT, serve: CLEARANCE_CORE_TIMEOUT).
// The Clearance client's own default is 250 ms; a ledger append took 306 ms live (2026-11-12) and the cancel cost two
// decisions and an outcome, so the gateway asks for 1 s. The client retries a timeout, a connection failure or a 503
// once (never a 4xx), and its breaker counts a call only after that retry also failed, so one slow write is one failure
// of five. Retries and the breaker stay the client's: DecisionRetries and BreakerThreshold are what the gateway passes
// (the client's own defaults), so a change to them is a change here.
const (
	DecisionTimeout  = time.Second
	DecisionRetries  = 1
	BreakerThreshold = 5
)

// ClientOptions are the decision path's options for the core's client: DecisionTimeout (or timeout, when the operator set
// CLEARANCE_CORE_TIMEOUT) per attempt, one retry, a breaker at five (Python: client_from_env).
func ClientOptions(baseURL string, token client.TokenSource, timeout time.Duration) client.Options {
	if timeout <= 0 {
		timeout = DecisionTimeout
	}
	return client.Options{BaseURL: baseURL, Token: token, Provider: Provider, Timeout: timeout, Retries: DecisionRetries,
		BreakerThreshold: BreakerThreshold}
}

// OutageRule is the rule a call carries when the core could not decide it, and RefusedRule when the core refused the
// request itself (fail closed: a 4xx or an unreadable answer is no decision).
const (
	OutageRule  = "CORE-UNAVAILABLE"
	RefusedRule = "CORE-REFUSED"
	OutageHint  = "Clearance is unreachable, so nothing that needs its decision runs. Retry in a minute."
)

var (
	refusalRule  = regexp.MustCompile(`^[A-Z][A-Z0-9]*(-[A-Z0-9]+)*$`)
	modelVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)
)

// Denied is a handler's refusal for a reason the core's rules cannot see.
type Denied struct{ Rule, Message, Hint string }

func (d *Denied) Error() string { return d.Message }

// AuthError is a token the authenticator refused.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// Unreachable is "no decision": the core is unreachable (the outage policy applies) or refused the request.
type Unreachable struct {
	Msg     string
	Refused bool
}

func (e *Unreachable) Error() string { return e.Msg }

// Rule is the rule the outage verdict carries.
func (e *Unreachable) Rule() string {
	if e.Refused {
		return RefusedRule
	}
	return OutageRule
}

// Verdict is one evaluation's answer, as the engine uses it.
type Verdict struct {
	Allow      bool
	Rule, Name string
	Message    string
	Hint       string
	Tier       *int
	Spend      client.Spend
	Grant      *client.Grant
	Problems   []client.Problem
	DecisionID string
	Seq        *int64
}

// AsMap is the decision as the agent sees it.
func (v *Verdict) AsMap() map[string]any {
	m := map[string]any{"allow": v.Allow, "rule": nilIfEmpty(v.Rule), "name": nilIfEmpty(v.Name), "message": v.Message,
		"hint": v.Hint, "tier": nil}
	if v.Tier != nil {
		m["tier"] = *v.Tier
	}
	if len(v.Problems) > 0 {
		ps := make([]map[string]any, len(v.Problems))
		for i, p := range v.Problems {
			ps[i] = map[string]any{"pointer": p.Pointer, "rule": p.Rule, "message": p.Message, "hint": p.Hint}
		}
		m["problems"] = ps
	}
	return m
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// OutageVerdict is the deny a call gets when there was no decision.
func OutageVerdict(e *Unreachable, tier *int) *Verdict {
	msg, name := "Clearance is unreachable: no decision", "core-unreachable"
	if e.Refused {
		msg, name = "Clearance refused the request: "+e.Msg, "core-refused"
	}
	return &Verdict{Rule: e.Rule(), Name: name, Message: msg, Hint: OutageHint, Tier: tier}
}

func toVerdict(res *client.EvaluationResult) *Verdict {
	c := res.Context
	v := &Verdict{Allow: res.Decision, Rule: c.Rule, Name: c.Name, Message: c.Message, Hint: c.Hint, Tier: c.Tier,
		Spend: c.Spend, Grant: c.Grant, Problems: c.Problems}
	if res.DecisionID != nil {
		v.DecisionID = *res.DecisionID
	}
	if res.Record != nil {
		seq := res.Record.Seq
		v.Seq = &seq
	}
	return v
}

// Canonical is the canonical JSON the records' digests are computed over: sorted keys, no spaces, no HTML escaping.
func Canonical(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var generic any
	d := json.NewDecoder(bytesReader(raw))
	d.UseNumber()
	if err := d.Decode(&generic); err != nil {
		return nil
	}
	var out []byte
	out = appendCanonical(out, generic)
	return out
}

// ArgsDigest is the sha256 of the arguments' canonical JSON (the record's args_sha256).
func ArgsDigest(args any) string {
	h := sha256.Sum256(Canonical(args))
	return hex.EncodeToString(h[:])
}

// ValidModelVersion says whether s is a model version a record may carry (evidence's MODEL_VERSION).
func ValidModelVersion(s string) bool { return modelVersion.MatchString(s) }

// ArgsDigestBytes is the sha256 of raw bytes, hex (Python: sha256_hex).
func ArgsDigestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Iso is the one timestamp format records use.
func Iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// Entry is an outbox entry: a record (POST /v1/records) or an outcome (POST /v1/decisions/{id}/outcome).
type Entry struct {
	Op         string // "record" | "outcome"
	Kind       string
	DecisionID string
	Body       map[string]any
}

// Outbox is what the gateway owes the core and could not hand it yet, kept in order.
type Outbox interface {
	Add(ctx context.Context, e Entry, reason string, errText string) (int64, error)
	Pending(ctx context.Context, limit int) ([]OutboxRow, error)
	HasPending(ctx context.Context) (bool, error)
	Done(ctx context.Context, id int64, seq *int64) error
	Refused(ctx context.Context, id int64, errText string) error
}

// OutboxRow is a pending entry with its id.
type OutboxRow struct {
	ID    int64
	Entry Entry
}

// Core is the gateway's Clearance core: one client (one breaker, one connection pool), the outbox, the outage switch.
type Core struct {
	Client      *client.Client
	Outbox      Outbox
	AllowReads  bool
	Log         *slog.Logger
	drainMu     sync.Mutex
	definitions *client.Client
	// OnDecision and OnPosted, when set, see each decision id the core answered and each entry it took with its
	// record reference. Only the conformance control interface sets them (to report what it was answered).
	OnDecision func(decisionID string)
	OnPosted   func(e Entry, ref client.RecordRef)
}

// New builds a Core. definitions is the client for PUT /v1/definitions (a longer timeout and its own breaker); nil
// uses c.
func New(c *client.Client, ob Outbox, allowReads bool) *Core {
	if ob == nil {
		ob = NewMemoryOutbox()
	}
	return &Core{Client: c, Outbox: ob, AllowReads: allowReads, Log: slog.Default(), definitions: c}
}

// Evaluate decides one evaluation. A returned *Unreachable means there is no decision (the caller denies).
func (c *Core) Evaluate(ctx context.Context, req *client.EvaluationRequest) (*Verdict, error) {
	res, err := c.Client.Evaluate(ctx, req)
	if err != nil {
		return nil, c.classify(ctx, err)
	}
	v := toVerdict(res)
	if c.OnDecision != nil && v.DecisionID != "" {
		c.OnDecision(v.DecisionID)
	}
	return v, nil
}

func (c *Core) classify(ctx context.Context, err error) error {
	var api *client.APIError
	switch {
	case ctx.Err() != nil:
		// The caller's own deadline or cancellation: no decision, not the core's fault.
		return &Unreachable{Msg: ctx.Err().Error()}
	case errors.Is(err, client.ErrCoreUnavailable):
		c.Log.Warn("clearance core unavailable", "err", err.Error())
		return &Unreachable{Msg: err.Error()}
	case errors.As(err, &api), errors.Is(err, client.ErrBadResponse):
		c.Log.Error("clearance core refused a request", "err", err.Error())
		return &Unreachable{Msg: err.Error(), Refused: true}
	default: // client.ErrToken: nothing was sent
		c.Log.Error("clearance core", "err", err.Error())
		return &Unreachable{Msg: err.Error()}
	}
}

// OutcomeArgs are an allowed decision's result.
type OutcomeArgs struct {
	Result       string
	Effect       *client.Effect
	Receipt      map[string]any
	RefusalRule  string
	RefusalMsg   string
	Usage        map[string]int64
	ModelVersion string
}

// Outcome reports an allowed decision's result. It never fails the caller: an outcome the core cannot take now waits
// in the outbox (the call already ran). Returns the outcome record's seq, or nil when it is queued.
func (c *Core) Outcome(ctx context.Context, decisionID string, a OutcomeArgs) *int64 {
	if decisionID == "" {
		return nil
	}
	body := &client.OutcomeRequest{Result: a.Result, Effect: a.Effect, Receipt: a.Receipt}
	if a.Usage != nil {
		ch := a.Usage["charged_tokens"]
		body.Usage = &client.Usage{InputTokens: a.Usage["input_tokens"], OutputTokens: a.Usage["output_tokens"],
			CacheCreationInputTokens: a.Usage["cache_creation_input_tokens"],
			CacheReadInputTokens:     a.Usage["cache_read_input_tokens"], ChargedTokens: &ch}
	}
	if a.ModelVersion != "" && modelVersion.MatchString(a.ModelVersion) {
		mv := a.ModelVersion
		body.ModelVersion = &mv
	}
	if a.RefusalRule != "" {
		if len(a.RefusalRule) <= 64 && refusalRule.MatchString(a.RefusalRule) && a.RefusalMsg != "" {
			body.Refusal = &client.Refusal{Rule: a.RefusalRule, Message: clipBytes(a.RefusalMsg, 4000)}
			body.Effect = nil // a refused call made no write
		}
	}
	raw, _ := json.Marshal(body)
	var bm map[string]any
	_ = json.Unmarshal(raw, &bm)
	return c.send(ctx, Entry{Op: "outcome", DecisionID: decisionID, Body: bm})
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := []byte(s)[:n]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// Record writes one of the gateway's own records; fields with nil values are dropped. When the core is unreachable it
// waits in the outbox, or with required it fails with *Unreachable so the caller fails closed and undoes what it did.
func (c *Core) Record(ctx context.Context, kind string, fields map[string]any, required bool) (*int64, error) {
	body := map[string]any{}
	for k, v := range fields {
		if v != nil {
			body[k] = v
		}
	}
	e := Entry{Op: "record", Kind: kind, Body: body}
	if required {
		return c.sendRequired(ctx, e)
	}
	return c.send(ctx, e), nil
}

// OutageRead queues a T0 read allowed while the core was unreachable, posted later as coverage-unavailable.
func (c *Core) OutageRead(ctx context.Context, fields map[string]any) {
	body := map[string]any{}
	for k, v := range fields {
		if v != nil {
			body[k] = v
		}
	}
	body["coverage"] = "coverage-unavailable"
	if _, err := c.Outbox.Add(ctx, Entry{Op: "record", Kind: "tool.call", Body: body}, "outage", ""); err != nil {
		c.Log.Error("outbox add failed", "err", err.Error())
	}
}

func (c *Core) send(ctx context.Context, e Entry) *int64 {
	seq, err := c.deliver(ctx, e, false)
	if err != nil {
		c.Log.Error("outbox", "err", err.Error())
	}
	return seq
}

func (c *Core) sendRequired(ctx context.Context, e Entry) (*int64, error) {
	return c.deliver(ctx, e, true)
}

func (c *Core) deliver(ctx context.Context, e Entry, required bool) (*int64, error) {
	// A caller whose own context ended must not lose the outcome: it is posted under a fresh bounded context.
	pending, err := c.Outbox.HasPending(ctx)
	if err != nil {
		return nil, err
	}
	if pending {
		c.Drain(ctx, 500)
		if pending, _ = c.Outbox.HasPending(ctx); pending {
			if required {
				return nil, &Unreachable{Msg: "the core has records still to take"}
			}
			_, err := c.Outbox.Add(ctx, e, "core-unavailable", "")
			return nil, err
		}
	}
	seq, perr := c.post(ctx, e)
	switch {
	case perr == nil:
		return &seq, nil
	case errors.Is(perr, client.ErrCoreUnavailable) || ctx.Err() != nil:
		if required {
			return nil, &Unreachable{Msg: perr.Error()}
		}
		c.Log.Warn("clearance core unavailable: queued", "kind", e.Kind)
		_, err := c.Outbox.Add(context.WithoutCancel(ctx), e, "core-unavailable", "")
		return nil, err
	default:
		// The core answered and refused: retrying it unchanged gets the same answer. Kept, with why, for a person.
		c.Log.Error("clearance core refused a record", "kind", e.Kind, "err", perr.Error())
		if required {
			return nil, &Unreachable{Msg: perr.Error(), Refused: true}
		}
		_, err := c.Outbox.Add(context.WithoutCancel(ctx), e, "refused", clipBytes(perr.Error(), 500))
		return nil, err
	}
}

func (c *Core) post(ctx context.Context, e Entry) (int64, error) {
	raw, _ := json.Marshal(e.Body)
	if e.Op == "outcome" {
		var req client.OutcomeRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return 0, fmt.Errorf("outbox outcome: %w", err)
		}
		ref, err := c.Client.Outcome(ctx, e.DecisionID, &req)
		if err != nil {
			return 0, err
		}
		if c.OnPosted != nil {
			c.OnPosted(e, *ref)
		}
		return ref.Seq, nil
	}
	var ev client.ProviderEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return 0, fmt.Errorf("outbox record: %w", err)
	}
	ref, err := c.Client.Record(ctx, &client.RecordRequest{Provider: Provider, Kind: e.Kind, Record: ev})
	if err != nil {
		return 0, err
	}
	return ref.Seq, nil
}

// Drain posts what waits in the outbox, oldest first, stopping at the first one the core cannot take now. Returns how
// many were posted.
func (c *Core) Drain(ctx context.Context, limit int) int {
	if !c.drainMu.TryLock() {
		return 0
	}
	defer c.drainMu.Unlock()
	rows, err := c.Outbox.Pending(ctx, limit)
	if err != nil {
		c.Log.Error("outbox", "err", err.Error())
		return 0
	}
	sent := 0
	for _, r := range rows {
		seq, err := c.post(ctx, r.Entry)
		var api *client.APIError
		switch {
		case err == nil:
			s := seq
			_ = c.Outbox.Done(ctx, r.ID, &s)
			sent++
			continue
		case errors.Is(err, client.ErrCoreUnavailable) || ctx.Err() != nil:
			return sent
		case errors.As(err, &api) && r.Entry.Op == "outcome" && api.Status == 409:
			_ = c.Outbox.Done(ctx, r.ID, nil) // it landed before (the answer was lost)
			continue
		default:
			c.Log.Error("clearance core refused a queued entry", "err", err.Error())
			_ = c.Outbox.Refused(ctx, r.ID, clipBytes(err.Error(), 500))
		}
	}
	return sent
}

// BreakerState is the decision client's breaker.
func (c *Core) BreakerState() string { return string(c.Client.BreakerState()) }

// SortedKeys is a helper for stable output.
func SortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
