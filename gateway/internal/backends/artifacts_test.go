package backends

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func artifactHandlers(t *testing.T, store ArtifactStore) (put, get func(*session.Session, map[string]any) (any, error)) {
	t.Helper()
	h, err := NewHandlers(Config{Artifacts: store, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	return func(s *session.Session, a map[string]any) (any, error) {
			return h["artifact.put"](context.Background(), s, a)
		},
		func(s *session.Session, a map[string]any) (any, error) {
			return h["artifact.get"](context.Background(), s, a)
		}
}

func ptr(s string) *string { return &s }

// The access rule (see artifacts.go): an artifact belongs to a task. A run tree (a parent and its children, siblings
// among them) shares one task id and so one set of artifacts; no other session, another person's or another run's or
// another agent's, reaches them, by name or any other way, and a session with no task has no artifacts.
func TestArtifactsAreKeptByTask(t *testing.T) {
	put, get := artifactHandlers(t, &MemoryArtifacts{})
	parent := &session.Session{ID: "s-parent", Principal: "user:alice", Agent: "orchestrator", TaskID: "t-1"}
	childA := &session.Session{ID: "s-a", Principal: "user:alice", Agent: "researcher", TaskID: "t-1", ParentID: ptr("s-parent"), Depth: 1}
	childB := &session.Session{ID: "s-b", Principal: "user:alice", Agent: "reviewer", TaskID: "t-1", ParentID: ptr("s-parent"), Depth: 1}
	grandchild := &session.Session{ID: "s-g", Principal: "user:alice", Agent: "researcher", TaskID: "t-1", ParentID: ptr("s-a"), Depth: 2}
	otherPerson := &session.Session{ID: "s-bob", Principal: "user:bob", Agent: "orchestrator", TaskID: "t-2"}
	laterRun := &session.Session{ID: "s-later", Principal: "user:alice", Agent: "orchestrator", TaskID: "t-3"}
	sameNameOtherTask := &session.Session{ID: "s-x", Principal: "user:alice", Agent: "orchestrator", TaskID: "t-1x"}
	noTask := &session.Session{ID: "s-none", Principal: "user:alice", Agent: "orchestrator"}

	if _, err := put(parent, map[string]any{"name": "plan.md", "content": "the plan"}); err != nil {
		t.Fatal(err)
	}
	for who, s := range map[string]*session.Session{"parent": parent, "child": childA, "sibling": childB, "grandchild": grandchild} {
		if got, err := get(s, map[string]any{"name": "plan.md"}); err != nil || got != "the plan" {
			t.Errorf("%s of the same task reads %v, %v", who, got, err)
		}
	}
	if _, err := put(childB, map[string]any{"name": "plan.md", "content": "a sibling replaced it"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := get(parent, map[string]any{"name": "plan.md"}); got != "a sibling replaced it" {
		t.Errorf("the tree shares one copy: %v", got)
	}
	for who, s := range map[string]*session.Session{"another person's": otherPerson, "a later run's": laterRun, "a task whose id starts the same": sameNameOtherTask} {
		if got, err := get(s, map[string]any{"name": "plan.md"}); err != nil || got != nil {
			t.Errorf("%s session reads %v, %v", who, got, err)
		}
		if _, err := put(s, map[string]any{"name": "plan.md", "content": "overwrite"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := get(parent, map[string]any{"name": "plan.md"}); got != "a sibling replaced it" {
		t.Errorf("another task overwrote the tree's artifact: %v", got)
	}
	if got, _ := get(otherPerson, map[string]any{"name": "plan.md"}); got != "overwrite" {
		t.Errorf("a task keeps its own: %v", got)
	}
	// a session with no task is refused, for a read as for a write: nothing is shared under an empty key
	if _, err := put(noTask, map[string]any{"name": "x", "content": "y"}); err == nil || errors.As(err, new(*core.Denied)) {
		t.Errorf("put without a task: %v", err)
	}
	if _, err := get(noTask, map[string]any{"name": "plan.md"}); err == nil {
		t.Error("get without a task")
	}
	// the task is the session's, never an argument: naming a task changes nothing
	if got, _ := get(otherPerson, map[string]any{"name": "plan.md", "task": "t-1", "task_id": "t-1"}); got != "overwrite" {
		t.Errorf("an argument chose a task: %v", got)
	}
	// names cannot reach another task's key either
	for _, name := range []string{"t-1/plan.md", "../t-1/plan.md", "t-1:plan.md", "plan.md\n", "plan.md ", "PLAN.MD"} {
		if got, err := get(otherPerson, map[string]any{"name": name}); got != nil || err != nil {
			t.Errorf("name %q: %v %v", name, got, err)
		}
	}
}

func TestArtifactsAreBoundedAndExpire(t *testing.T) {
	clock := t0
	m := &MemoryArtifacts{}
	h, _ := NewHandlers(Config{Artifacts: m, Now: func() time.Time { return clock }})
	s := &session.Session{ID: "s-1", TaskID: "t-1"}
	put := func(name string, size int) error {
		_, err := h["artifact.put"](context.Background(), s, map[string]any{"name": name, "content": strings.Repeat("a", size)})
		return err
	}
	if err := put("a", MaxArtifactBytes); err != nil {
		t.Fatal(err)
	}
	var den *core.Denied
	if err := put("b", MaxArtifactBytes+1); !errors.As(err, &den) || den.Rule != "ART-SIZE" {
		t.Fatalf("%v", err)
	}
	for i := 0; i < 7; i++ {
		if err := put(string(rune('b'+i)), MaxArtifactBytes); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if err := put("z", 1); !errors.As(err, &den) || den.Rule != "ART-QUOTA" {
		t.Fatalf("2 MiB is the most a task keeps: %v", err)
	}
	if err := put("a", 10); err != nil {
		t.Fatalf("a replacement frees its own bytes: %v", err)
	}
	clock = t0.Add(ArtifactTTL)
	got, _ := h["artifact.get"](context.Background(), s, map[string]any{"name": "a"})
	if got != nil {
		t.Fatalf("an artifact is gone after its 7 days: %v", got)
	}
	if err := put("z", 1); err != nil {
		t.Fatalf("an expired artifact counts for nothing: %v", err)
	}
}

func TestPutsFromManyCallsNeverPassTheCountLimit(t *testing.T) {
	m := &MemoryArtifacts{}
	h, _ := NewHandlers(Config{Artifacts: m, Now: func() time.Time { return t0 }})
	s := &session.Session{ID: "s-1", TaskID: "t-1"}
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h["artifact.put"](context.Background(), s, map[string]any{"name": "n" + strings.Repeat("x", i), "content": "x"})
		}()
	}
	wg.Wait()
	if n := len(m.rows); n != MaxArtifactCount {
		t.Fatalf("%d artifacts for one task: the limit is %d", n, MaxArtifactCount)
	}
}

func TestPythonReprAndStr(t *testing.T) {
	for in, want := range map[any]string{
		"a": "'a'", "it's": `"it's"`, `say "hi"'`: `'say "hi"\''`, "a\nb\t\\": `'a\nb\t\\'`, "é": "'é'", "\x00": `'\x00'`, "\x7f": `'\x7f'`,
		"\u200b": `'\u200b'`, "\U0001f600": "'😀'", nil: "None", true: "True", false: "False",
	} {
		if got := pyRepr(in); got != want {
			t.Errorf("repr(%#v) = %s, want %s", in, got, want)
		}
	}
	if got := pyRepr([]any{"a", nil, true}); got != "['a', None, True]" {
		t.Error(got)
	}
	if got := pyRepr(map[string]any{"b": 1.5, "a": []any{}}); got != "{'a': [], 'b': 1.5}" {
		t.Error(got)
	}
	third := 0.1
	third += 0.2
	for in, want := range map[float64]string{1.5: "1.5", 100: "100.0", 1e16: "1e+16", 1e15: "1000000000000000.0", 0.0001: "0.0001", 0.00001: "1e-05",
		123456789.125: "123456789.125", -2.5e-7: "-2.5e-07", 1.5e300: "1.5e+300", third: "0.30000000000000004"} {
		if got := pyFloatRepr(in); got != want {
			t.Errorf("repr(%v) = %s, want %s", in, got, want)
		}
	}
	if pyStr("x") != "x" || pyStr(nil) != "None" {
		t.Error("str")
	}
}
