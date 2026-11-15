package airframe_test

import (
	"errors"

	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/core"
)

func asDenied(err error, d **core.Denied) bool              { return errors.As(err, d) }
func asAirframe(err error, a **backends.AirframeError) bool { return errors.As(err, a) }
