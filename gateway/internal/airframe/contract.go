// Package airframe is what the airframe.* tools know: Airframe's contract (the vendored contract/airframe-contract.json),
// the AppSpec schema and planner (airframe_plan.py), the read-only cluster state tools (airframe_tools.py) and the pieces
// airframe.apply and xr.request are built from. Nothing here makes a network request: the backends package does, through its
// closed lists, and hands the answers to this one.
package airframe

import (
	_ "embed"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// contractJSON and appSpecJSON are copies of schemas/airframe-contract.json and schemas/appspec.schema.json, made by
// tools/gen_gateway_airframe.py, which fails its --check (and tests/test_gateway_go.py fails) when a copy differs.
//
//go:embed contract.json
var contractJSON []byte

//go:embed appspec.schema.json
var appSpecJSON []byte

var theContract = mustOMap(contractJSON, "the Airframe contract")

func mustOMap(raw []byte, what string) *definitions.OMap {
	v, err := definitions.DecodeJSON(raw)
	if err != nil {
		panic(what + " does not parse: " + err.Error())
	}
	o, ok := v.(*definitions.OMap)
	if !ok {
		panic(what + " is not an object")
	}
	return o
}

// Contract is the vendored contract, read once, keys in file order. Callers must not change it.
func Contract() *definitions.OMap { return theContract }

// AppSpecSchemaJSON is the AppSpec JSON schema as vendored.
func AppSpecSchemaJSON() []byte { return appSpecJSON }

// Registry is what the planner knows about the fleet (airframe_plan.Registry).
type Registry struct {
	DevCluster    string
	UpperClusters []string
	TenantsRepo   string
	Owner         string
	// XR bounds what xr.request may ask for (the zero value is the safe default).
	XR XRPolicy
}
