"""The gateway engine: the AI gateway's product-neutral half, moved from jfillman/clearance at v0.26.1 (R-3a).

The MCP surface (server.py), sessions, the run tree, budgets and requested limits (session.py, limits.py), tool
dispatch (dispatch.py), the tool registry and tier vocabulary (tiers.py), the model proxy (modelproxy.py), the backend
protocols and fakes (backends.py), agent definitions and their schema (profile.py, schemas/), the runtime catalog
(runtimes.py), the human inbox (inbox.py), person controls (control.py), run results (results.py) and trigger history
(events.py). autopilot.gateway assembles it for Hangar: the Hangar pack, Hangar's backends, the run tree as AgentRuns.

What the engine still takes from Clearance (the decision and the record, the deployment gate, provenance) comes in
through one module, core.py: that is the seam R-3b replaces with the Clearance core's API (README.md).
"""
