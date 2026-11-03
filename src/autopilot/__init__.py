"""Autopilot: Hangar's agents on Clearance.

The Clearance core decides and records (over its API, engine/core.py); the gateway engine (engine/) enforces; this
package is what Hangar plugs into it: the tool backends for Glidepath and Airframe (glidepath_api, airframe_api,
airframe_tools), Hangar's tool specs (hangar_tools), the AppSpec planner (airframe_plan), the AgentRun manifest
(manifests), triggers, Preflight, and gateway.py, which builds the Hangar Gateway from the engine, Hangar's tools,
these backends and the core.
"""
