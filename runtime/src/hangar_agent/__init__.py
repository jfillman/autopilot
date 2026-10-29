"""hangar-agent: the generic agent runtime that runs inside an Autopilot AgentRun pod.

The pod is the run; the session it works in already exists (Clearance opened it and launched the run, and
HANGAR_SESSION_ID names it). The runtime is a plain tool-use loop: it asks the model proxy for the next step, sends
every tool call through Clearance, hands the decision back to the model, and writes /run/output/result.json when it
stops. It holds no authority of its own: Clearance decides every action and the model proxy meters every token, each
with its own short-lived projected token.

Modules, in call order: env (the pod contract), clearance (the gateway client), tools (Clearance tools as model tools),
model (the model-proxy client), loop (the agent loop), main (the entrypoint and result.json).
"""
