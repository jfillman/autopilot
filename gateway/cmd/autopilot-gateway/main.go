// Command autopilot-gateway is the Hangar AI gateway in Go (A-9): one binary, subcommands serve, api,
// conformance-control and version. See docs/admin/go-gateway.md for what this build carries and what is still the
// Python gateway's. The deployed image is still the Python one; nothing selects this binary until the managing
// cutover does.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "serve":
		code = serve(os.Args[2:])
	case "api":
		code = api(os.Args[2:])
	case "conformance-control":
		code = conformanceControl(os.Args[2:])
	case "version", "--version":
		fmt.Println("autopilot-gateway", gatewayVersion())
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: autopilot-gateway serve | api | conformance-control [--listen host:port] | version")
}

// gatewayVersion is the image's own version (AUTOPILOT_GATEWAY_VERSION, as the Python gateway reports it), else the
// build's.
func gatewayVersion() string {
	if v := os.Getenv("AUTOPILOT_GATEWAY_VERSION"); v != "" {
		return v
	}
	return version
}
