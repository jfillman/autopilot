package dispatch

import (
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

// Effect is the receipt a successful write's outcome carries, made from the tool's result (what the core's provenance checks
// a pull request against). It is exported so the tests of the tools that make one can hold it equal to the Python gateway's.
func Effect(spec *tools.Spec, args map[string]any, data any) *client.Effect {
	return effect(spec, args, data)
}
