"""The Claude Agent SDK runtime (runtime/catalog.yaml `claude-agent-sdk`): Claude Code's agent loop, with Clearance as
its only tools and Clearance's model proxy as its only model.

The SDK runs the bundled Claude Code CLI. It is given no built-in tools (no Bash, no file edits, no web): each tool
this session may call is one in-process MCP tool that forwards to Clearance (hangar_agent.clearance), so every
action is decided and recorded there exactly as the generic runtime's are. The CLI calls the model through the model
proxy with the run's own model-proxy token and session header, and Clearance charges it to the session's budget
(clearance v0.26.0 relays the streaming answers the CLI asks for). The pod contract is hangar_agent's (env,
result.json, run_finish).
"""
