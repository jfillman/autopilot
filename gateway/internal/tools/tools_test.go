package tools

import "testing"

func TestDefaultRegistry(t *testing.T) {
	r := Default()
	if r.Len() < 40 {
		t.Fatalf("only %d tools", r.Len())
	}
	s, ok := r.Get("repo.pr.open_upper")
	if !ok || s.Tier != T2 || !s.ProposeOnly || !s.WritesFiles || s.Target != "upper" {
		t.Fatalf("repo.pr.open_upper: %+v", s)
	}
	if _, ok := r.Get("glidepath.promote"); !ok {
		t.Fatal("the Hangar pack is registered")
	}
	if _, ok := r.Get("k8s.exec"); ok {
		t.Fatal("a tripwire is not a tool")
	}
	if len(r.BaselineDenyPaths) == 0 || len(r.Tripwires) == 0 {
		t.Fatal("baselines")
	}
}

func TestRefusesDuplicatesAndTripwires(t *testing.T) {
	if _, err := Parse([]byte(`{"tools":[{"name":"a"},{"name":"a"}]}`)); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := Parse([]byte(`{"tools":[{"name":"k8s.exec"}],"tripwires":["k8s.exec"]}`)); err == nil {
		t.Fatal("tripwire accepted")
	}
}
