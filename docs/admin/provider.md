# The gateway as a Clearance runtime provider

Status: **Built** (A-1, 2026-11-03): the gateway passes Clearance's runtime conformance suite in CI, against the
released core image and the plain binary. **Unverified on hangar-dev** until a gateway tag with A-1 is deployed and
a live run's records are read back.

The Autopilot gateway is the enforcement point for agents. Clearance decides and records; the gateway asks it first,
acts only on yes, and reports what happened. Clearance calls such an enforcement point a **provider** and writes
the contract down in its [provider protocol](https://github.com/jfillman/clearance/blob/main/docs/provider-protocol.md).
This page is the gateway's side of it: what it promises, how the records name it, and how the promise is tested.

## What the gateway promises

| Rule | What the gateway does |
| --- | --- |
| Ask first | Every tool call, model call, session open and spawn is a `POST /access/v1/evaluation` before anything runs |
| Act only on yes | A handler runs only on `decision: true`; a deny returns the rule and hint and touches no backend |
| Apply the spend | The decision's `context.spend` is applied to the session the gateway holds, allow or deny (budgets, a denial, the breaker) |
| One outcome per allowed decision | `POST /v1/decisions/{id}/outcome` once the call ran: `ok`, `error:<Kind>`, or a handler's refusal. Tool calls and model calls alike |
| Fail closed | No answer, a timeout or a 5xx is a deny (`CORE-UNAVAILABLE`). The one exception is the recorded switch `GATEWAY_OUTAGE_ALLOW_READS` (unscoped T0 reads, posted later as `coverage-unavailable`) |
| Write its own runtime records | `session.open`, `run.launch`, `run.delete`, `run.finish`, `session.end`, `session.freeze`, `human.answer`, `trigger.hold`, `trigger.release` through `POST /v1/records` |
| Identify itself | A projected service-account token, audience the core's `CLEARANCE_AUDIENCE`, for the principal `CLEARANCE_PROVIDERS` registers |

## How the records name it

Every record has a `provider` (`{name, version}`), and the core sets it, never the gateway:

- **The gateway's own records** (outcomes, runtime records) carry the gateway's registration from the core's
  `CLEARANCE_PROVIDERS`: `{"name": "autopilot-gateway", "version": "<gateway tag>"}`. On Hangar, Apron renders the
  entry from `versions.autopilot.gateway`, so the version is the deployed image's tag (for example
  `gateway-v0.4.1`):

  ```json
  [{"name": "autopilot-gateway", "version": "gateway-v0.4.1",
    "principals": ["autopilot-system/clearance"], "definitions": true}]
  ```

- **Decision records** are the core's own (`provider` `clearance`, its version), with `caller`
  `{"provider": "autopilot-gateway", "principal": ...}`, which the core verifies.
- **Model calls** follow the tool-call shape: the decision is the core's record with `tool: model.call` and
  `caller.provider: autopilot-gateway`; its outcome is the gateway's record (`provider` autopilot-gateway at the
  gateway tag), linked by `outcome_of {seq, hash}`. The outcome's `receipt` says whether the call streamed and what
  the upstream answered, so a streamed call is provable from the chain:

  ```json
  {"tool": "model.call", "result": "ok", "outcome_of": {"seq": 812, "hash": "..."},
   "provider": {"name": "autopilot-gateway", "version": "gateway-v0.4.2"},
   "receipt": {"stream": true, "upstream_status": 200},
   "model_version": "claude-sonnet-5-5-20261015",
   "usage": {"input_tokens": 1000, "output_tokens": 50, "cache_creation_input_tokens": 0,
             "cache_read_input_tokens": 3000, "charged_tokens": 1350},
   "subject": {"kind": "model-call", "name": "claude-sonnet-5-5", "digests": {"sha256": "<args_sha256>"}}}
  ```

  Results: `ok` (a 2xx answer, and for a stream one that ran to its end), `error:Upstream<status>`,
  `error:ModelUnreachable`, `error:BadUpstreamAnswer`, `error:StreamInterrupted` (the stream ended early: the run
  went away or the provider dropped it), `error:UnsupportedBackend`, and `refused M006` when the gateway has no route
  for a model the core allowed. `receipt` is claimed (the gateway says it); `result`'s link to the decision and the
  copied identity fields are verified. A denied model call has no outcome.

  The subject kind is `model-call`, named by the model the call asked for (its `target`, the route alias), with
  the digest of its arguments: the core derives it for `tool: model.call` from evidence schema revision 3
  (clearance#53, A-4). The gateway sends no subject; the decision and its outcome both carry it. Older rows keep
  the `tool-call` subject they were written with. The same outcome carries A-4's `model_version` (the model that
  answered) and, for a call that used tokens, `usage` (the four token kinds and `charged_tokens`, the call's
  weighted charge), all claimed.

The start line (`serve.describe`) prints `provider=autopilot-gateway/<version>`: the image's own version (the
`gateway-v*` tag it was built from, `AUTOPILOT_GATEWAY_VERSION`). It should equal the core's registration. When it
does not, records name the registration, not the image: fix `CLEARANCE_PROVIDERS` (Apron `versions.autopilot.gateway`)
with the image pin.

## Closing a session from an MCP client

`session_close` (MCP tool) ends one of the caller's own sessions now instead of at its TTL: a person's MCP client
that opened a session, or an in-cluster run closing its own. It goes through the same path as the person control
End: the session and its children close and `session.end` is written through the core (result
`ended: <reason>`, the caller as principal); then its in-cluster runs and its children's are deleted as the expiry
sweep deletes them, each with a `run.delete` record. Only the session's own caller may; anyone else, the agent's
owner group and platform admins included, is refused (`CT-FORBIDDEN`) and uses End in Tower.

## How it is tested: the conformance suite

`clearance conformance --provider <url> --role runtime` (shipped in every Clearance release) starts an in-memory core
and a fake target and drives the provider through a test-only control interface: a denied call, an allowed call, the
outcome, the spend, an unavailable and an unreachable core, and the chain at the end. The gateway serves that
interface from its own subcommand:

```shell
GATEWAY_CONFORMANCE_CONTROL=1 autopilot-gateway conformance-control --listen 127.0.0.1:9091
clearance conformance --provider http://127.0.0.1:9091 --role runtime --report conformance.json
```

**Test only, off by default.** The subcommand refuses to start unless `GATEWAY_CONFORMANCE_CONTROL=1`, binds a
loopback address only, on its own port, and never starts the deployable gateway: it reads no `GATEWAY_DSN`, core
token, model key or backend credential. Each apply runs the gateway's own tool-call path (`Gateway.call`) on a
throwaway in-memory gateway against the core and target the harness names, with the bearers it hands over; the only
thing the harness controls is the evaluation the gateway sends. No chart, manifest or Apron template sets the switch
(`tests/test_conformance.py` checks the repo).

CI (`.github/workflows/test.yaml`) runs the suite twice: with the core binary built from the pinned Clearance tag,
and with the released image `ghcr.io/jfillman/clearance:<tag>` (`docker run --network host ... conformance`). The
reports are the job's `conformance-reports` artifact and its step summary. A broken variant that acts on a deny must
fail the suite (`runtime.deny-not-acted`), so a pass is not vacuous.

Output against clearance v0.30.0:

```text
PASS control.info                         the provider serves its control interface for this role: autopilot-gateway gateway-v0.4.2-test, roles [runtime]
PASS runtime.deny-not-acted               a denied call is not acted on: decision 0 deny R006, state "refused", target applies 0, outcome records 0
PASS runtime.allow-acted-once             an allowed call is acted on exactly once: decision 1 allow=true, state "applied", target applies 1
PASS runtime.outcome-references-decision  the outcome record references the decision: outcome seq 2 -> decision seq 1, hash linked true, provider autopilot-gateway, result "ok"
PASS runtime.spend-applied                the spend each decision carries is applied: deny: applied {"denial":true}, the core asked for {"denial":true}; allow: applied {"tool_calls":1,"github_calls":4,"open_prs":1}, the core asked for {"tool_calls":1,"github_calls":4,"open_prs":1}
PASS runtime.outage-fails-closed          an unavailable or unreachable core fails closed: rt-outage-503: state "refused", target applies 0; rt-outage-drop: state "refused", target applies 0
PASS chain.verify                         the evidence chain verifies from genesis: 3 records, head ba0bf181..., 1 written as autopilot-gateway
PASS clearance-conformance/v1 runtime provider http://127.0.0.1:9091 (autopilot-gateway gateway-v0.4.2-test): 7 assertions, 0 failed
```

## Not built

- **`provider.conformance` on records**: reserved in Clearance's v1 schema; `CLEARANCE_PROVIDERS` has no field for it
  yet. The report's `suite` (`clearance-conformance/v1`) is the value to stamp.
- **The suite's model-call scenario**: the harness drives tool calls only; model-call outcomes are tested in this
  repo (`tests/engine/test_run_identity.py`).
