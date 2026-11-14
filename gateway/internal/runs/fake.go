package runs

import (
	"context"
	"sync"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// Fake is a run backend that starts nothing: it names each run after its session, records what it was asked, and fails
// when told to. It is what the tests of everything above the Kubernetes API use. It satisfies dispatch.RunBackend.
type Fake struct {
	mu sync.Mutex
	// Live is the runs that exist, by session id.
	Live map[string]string
	// Calls is "create <name>" and "delete <name>", in order.
	Calls []string
	// Inputs is the input each create was given, by run name.
	Inputs map[string]map[string]any
	// CreateErr, DeleteErr and EndedErr, when set, are what the call returns.
	CreateErr, DeleteErr, EndedErr error
	// CreateThenFail makes Create start the run and then report a failure (a timeout after the API server took it).
	CreateThenFail error
	// Phases is how each run ended, by run name ("" or absent: still running).
	Phases map[string]string
	// NoPrincipal makes Principal answer "" (a backend that names no identity for its runs).
	NoPrincipal bool
	// OnCreate, when set, runs inside Create before it returns (a test's hook to stop the call mid-way).
	OnCreate func()
}

// NewFake is an empty Fake.
func NewFake() *Fake {
	return &Fake{Live: map[string]string{}, Inputs: map[string]map[string]any{}, Phases: map[string]string{}}
}

// Name is the run name a session gets.
func (f *Fake) Name(s *session.Session) string { return RunName(s.ID) }

// Create starts a run for the session.
func (f *Fake) Create(_ context.Context, s *session.Session, d *definitions.Definition, input map[string]any) (string, error) {
	if f.OnCreate != nil {
		f.OnCreate()
	}
	name := RunName(s.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return "", f.CreateErr
	}
	f.Calls = append(f.Calls, "create "+name)
	f.Live[s.ID] = name
	f.Inputs[name] = input
	if f.CreateThenFail != nil {
		return "", f.CreateThenFail
	}
	return name, nil
}

// Delete stops the session's run.
func (f *Fake) Delete(_ context.Context, s *session.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	f.Calls = append(f.Calls, "delete "+RunName(s.ID))
	delete(f.Live, s.ID)
	return nil
}

// Ended is how the session's run ended.
func (f *Fake) Ended(_ context.Context, s *session.Session) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EndedErr != nil {
		return "", f.EndedErr
	}
	return f.Phases[RunName(s.ID)], nil
}

// Principal is the identity a run's pod authenticates as.
func (f *Fake) Principal(run string) string {
	if f.NoPrincipal {
		return ""
	}
	return Principal(run)
}

// Set changes what the fake does, under its lock.
func (f *Fake) Set(fn func(*Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Count is how many of the recorded calls are exactly this one.
func (f *Fake) Count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == call {
			n++
		}
	}
	return n
}

// FakeStore is an AgentRun store that keeps the manifests it is given, for the tests of the real Backend.
type FakeStore struct {
	mu        sync.Mutex
	Manifests map[string]map[string]any
	Deleted   []string
	Outcomes  map[string]string
	Err       error
	// FailDelete names the runs whose delete fails (an API server error), until it is emptied.
	FailDelete map[string]bool
}

// NewFakeStore is an empty FakeStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{Manifests: map[string]map[string]any{}, Outcomes: map[string]string{}}
}

// Create keeps the manifest.
func (f *FakeStore) Create(_ context.Context, m map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Manifests[m["metadata"].(map[string]any)["name"].(string)] = m
	return nil
}

// Delete forgets a manifest.
func (f *FakeStore) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if f.FailDelete[name] {
		return &Error{Msg: "API server 500 deleting AgentRun " + name}
	}
	delete(f.Manifests, name)
	f.Deleted = append(f.Deleted, name)
	return nil
}

// Outcome is what the test set for the run ("" while it runs).
func (f *FakeStore) Outcome(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", f.Err
	}
	return f.Outcomes[name], nil
}

// SetFailDelete makes the delete of these runs fail (nil: none), under the store's lock.
func (f *FakeStore) SetFailDelete(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailDelete = map[string]bool{}
	for _, n := range names {
		f.FailDelete[n] = true
	}
}
