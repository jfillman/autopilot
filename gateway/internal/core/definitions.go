package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jfillman/clearance/client"
)

// DefinitionsTimeout is the timeout of one attempt of PUT /v1/definitions, which carries the whole set (up to 256 pins, 4 MiB):
// longer than a decision's (Python: DEFINITIONS_TIMEOUT).
const DefinitionsTimeout = 5 * time.Second

// DefinitionsClientOptions are the options of the client the definition set is published with: the decisions' client options with
// the longer timeout, so a slow publish never counts against the decision path's breaker (it has a client, and a breaker, of its own).
func DefinitionsClientOptions(baseURL string, token client.TokenSource) client.Options {
	return ClientOptions(baseURL, token, DefinitionsTimeout)
}

// SetDefinitionsClient gives the definition set's publish its own client (nil leaves it on the decisions' one) and names the gateway
// build the bundled definitions ship in, which the core's evidence names (Python: Core(bundled_version=)).
func (c *Core) SetDefinitionsClient(cl *client.Client, bundledVersion string) {
	if cl != nil {
		c.definitions = cl
	}
	c.BundledVersion = bundledVersion
}

// A core that predates the `bundled` field refuses the whole request (400, unknown field): that is learned once, and the field is
// not sent again (Python: Core._core_takes_bundled). The flag is the Core's, so it outlives one publish.
type bundledFlag struct{ refused atomic.Bool }

// PublishDefinitions hands the core the whole cluster-pinned definition set (PUT /v1/definitions): the pins with their sources, owners
// and documents, the rejections with their reasons, and, when bundled is not nil, every definition that ships in this gateway's image
// (the core holds them beside the pins and never lets a pin shadow one; an empty non-nil slice clears the provider's set). The core
// decides every pin again and records what changed; publishing the same set again records nothing, so a publish is idempotent.
//
// It never panics and never queues: ok is false when the core did not take the set (unreachable, or it refused), and the caller
// publishes the whole set again later. A core that predates `bundled` refuses it as an unknown field; the set is then sent without
// it, and not again with it, so the gateway keeps running (its bundled agents' pull requests fail provenance until the core is upgraded).
func (c *Core) PublishDefinitions(ctx context.Context, pins []client.PinnedDefinition, rejected []client.RejectedPin, bundled []client.ProvidedDefinition) (*client.DefinitionsResult, bool) {
	req := &client.DefinitionsRequest{Pins: pins, Rejected: rejected}
	if bundled != nil && !c.noBundled.refused.Load() {
		req.Bundled = make([]client.ProvidedDefinition, len(bundled))
		for i, b := range bundled {
			if b.Version == "" {
				b.Version = "dev"
				if c.BundledVersion != "" {
					b.Version = c.BundledVersion
				}
			}
			req.Bundled[i] = b
		}
	}
	res, err := c.definitions.PublishDefinitions(ctx, req)
	var api *client.APIError
	if err != nil && req.Bundled != nil && errors.As(err, &api) && api.Status == 400 &&
		strings.Contains(api.Message, "unknown field") && strings.Contains(api.Message, `"bundled"`) {
		c.noBundled.refused.Store(true)
		c.Log.Warn("clearance core does not take bundled definitions yet: publishing the pins without them; the bundled agents' pull requests fail provenance until the core is upgraded",
			"err", api.Message)
		req.Bundled = nil
		res, err = c.definitions.PublishDefinitions(ctx, req)
	}
	switch {
	case err == nil:
		return res, true
	case errors.Is(err, client.ErrCoreUnavailable) || ctx.Err() != nil:
		c.Log.Warn("clearance core unavailable: the definition set waits for the next poll", "err", err.Error())
	default:
		c.Log.Error("clearance core refused the definition set", "err", err.Error())
	}
	return nil, false
}
