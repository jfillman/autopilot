package dispatch_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// The definition set is replaced whole while the gateway serves (A-9 S4): a reader sees the old set or the new one, a removed pin stops
// being an agent at once, and a re-pin to a different definition is what the next call resolves.
func TestSetDefsReplacesTheSetForTheNextCallAndIsRaceFree(t *testing.T) {
	r := newRunRig(t)
	tok := r.person("alice")
	before, err := r.g.Agents(context.Background(), tok)
	if err != nil || len(before) < 8 {
		t.Fatalf("%d agents, %v", len(before), err)
	}
	old := r.g.Definitions()
	next := map[string]*definitions.Definition{}
	for n, d := range old {
		if n != "researcher" {
			next[n] = d
		}
	}
	tighter := *old["triage-agent"]
	tighter.Limits.ToolCalls = 1
	next["triage-agent"] = &tighter
	r.g.SetDefs(next)
	if r.g.Def("researcher") != nil || r.g.Def("triage-agent").Limits.ToolCalls != 1 {
		t.Fatal("the next call must resolve the new set")
	}
	after, _ := r.g.Agents(context.Background(), tok)
	if len(after) != len(before)-1 {
		t.Fatalf("a removed pin must stop being an agent: %d agents, was %d", len(after), len(before))
	}
	if old["researcher"] == nil || len(old) != len(before) {
		t.Fatal("the set a reader was handed is not written to")
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if (i+j)%3 == 0 {
					r.g.SetDefs(next)
				} else if (i+j)%3 == 1 {
					r.g.SetDefs(old)
				}
				_ = r.g.Def("researcher")
				_ = r.g.Definitions()
				if _, err := r.g.Agents(context.Background(), tok); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
