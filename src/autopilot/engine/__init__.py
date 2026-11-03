"""The gateway engine: the AI gateway's product-neutral half, moved from jfillman/clearance at v0.26.1 (R-3a).

The MCP surface (server.py), sessions, the run tree, budgets and requested limits (session.py, limits.py), tool
dispatch (dispatch.py), the tool registry and tier vocabulary (tiers.py), the model proxy (modelproxy.py), the backend
protocols and fakes (backends.py), agent definitions and their schema (profile.py, schemas/), the runtime catalog
(runtimes.py), the human inbox (inbox.py), person controls (control.py), run results (results.py) and trigger history
(events.py), callers' authentication (auth.py), the cluster pin set (definitions.py), the gateway's own database
(pg.py) and its outbox (outbox.py). autopilot.gateway assembles it for Hangar: Hangar's tools and backends, the run tree
as AgentRuns.

What the engine takes from Clearance (the decision and the record) comes through one module, core.py: the Clearance
core's HTTP API through its generated client, with the outage policy (R-3b, README.md).
"""
