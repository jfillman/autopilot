"""Autopilot: Hangar's agents on Clearance.

Clearance (the clearance package) decides and audits; this package is what Hangar plugs into it: the
tool backends for Glidepath and Airframe (glidepath_api, airframe_api, airframe_tools), the AppSpec
planner (airframe_plan), the AgentRun manifest (manifests), triggers, Preflight, and gateway.py, which
builds the Hangar Gateway from core Clearance, the clearance.packs.hangar policy and these backends.
"""
