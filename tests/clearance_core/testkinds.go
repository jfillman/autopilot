//go:build ignore

// Overlaid into the clearance checkout's internal/server by tests/clearance_core/build.py (tests only): lets the
// test core register a POST /v1/records kind the core does not have yet (testcore.go).
package server

// AutopilotTestKind registers a runtime record kind with its required fields.
func AutopilotTestKind(kind string, required []string) {
	if _, ok := recordKinds[kind]; ok {
		return
	}
	req := []string{}
	for _, f := range required {
		if f != "" {
			req = append(req, f)
		}
	}
	recordKinds[kind] = kindSpec{required: req, boundary: "runtime"}
	RecordKinds = append(RecordKinds, kind)
}
