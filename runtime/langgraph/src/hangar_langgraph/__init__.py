"""The LangGraph runtime (runtime/catalog.yaml `langgraph`): the generic runtime's loop as a LangGraph graph.

Two nodes: `model` asks the model through Clearance's model proxy (hangar_agent.model), `tools` runs each tool call
the model asked for through Clearance (hangar_agent.clearance), and the edges loop between them until the model ends
its turn, the session's time or turns run out, or something fails. The pod contract is hangar_agent's (env,
result.json, run_finish), so a run on this image looks the same to Clearance and Tower as one on the generic image.

It is a starting point for a graph of your own: FROM this image, import hangar_langgraph.graph.build, and add nodes.
"""
