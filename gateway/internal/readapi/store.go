package readapi

import (
	"context"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// Record is one stored document as the API shows it: the keys of the JSON object, the numbers as written (json.Number).
type Record = map[string]any

// SessionFilter is what a list of sessions is narrowed by (each empty: not narrowed).
type SessionFilter struct{ Agent, Principal, Task, State string }

// EventFilter is what a list of events is narrowed by.
type EventFilter struct {
	App, Agent, Source, Key, Subject string
	Since, Until                     *time.Time
}

// Store is what the API reads (read_api.ReadStore). Every method is scoped by the viewer in the query itself where the viewer is not an
// auditor, so a row another person owns is never read; every list has a limit.
type Store interface {
	Sessions(ctx context.Context, f SessionFilter, v *Viewer, limit int, after string, now time.Time) ([]Record, error)
	Session(ctx context.Context, id string, now time.Time) (Record, error)
	Result(ctx context.Context, sessionID string, now time.Time) (Record, error)
	// Task is the sessions of a task the viewer may see, oldest first, at most maxTaskSessions+1 (one more than the API shows: it says so).
	Task(ctx context.Context, taskID string, v *Viewer, now time.Time) ([]Record, error)
	// HumanPending is the pending requests the viewer may see (a notifier or auditor: every one), NEWEST first, at most maxPending+1.
	// since is compared as text with the request's created_at, as Python does.
	HumanPending(ctx context.Context, now time.Time, since string, v *Viewer) ([]Record, error)
	Events(ctx context.Context, f EventFilter, v *Viewer, limit int, after *EventCursor) ([]Record, error)
	Holds(ctx context.Context, now time.Time) (map[string]Record, error)
	Definitions(ctx context.Context) ([]definitions.Row, error)
	Ping(ctx context.Context) error
}
