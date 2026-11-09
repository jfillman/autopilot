package main

import (
	"fmt"
	"os"
)

// api is `autopilot-gateway api`: the read-only process that serves the runtime reads from the gateway database
// (src/autopilot/api_serve.py, read_api.py). It is not in this slice: the subcommand exists so the binary has the shape
// the card names, and refuses to start rather than answer a read it cannot.
func api(_ []string) int {
	fmt.Fprintln(os.Stderr, "autopilot-gateway api: not ported yet. The read API (clearance-api's runtime reads) is still the Python "+
		"gateway's `autopilot-gateway api`; see docs/admin/go-gateway.md.")
	return 2
}
