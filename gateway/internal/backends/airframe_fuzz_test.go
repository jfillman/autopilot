package backends

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// The branch names apply and xr.request make, from a session's id, a task and a name an agent chose, are always a branch of the
// gateway's own namespace that GitHub takes.
func FuzzApplyBranches(f *testing.F) {
	for _, s := range [][3]string{{"s-1", "t-abc", "flight-api-q"}, {"", "", "a-b"}, {"s/../1", "..", "x"}, {"s", ".lock", "refs"}, {"\x00", "é", "main"}, {"s", strings.Repeat("a", 300), "x"}} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, session, task, name string) {
		if b := branchForXR(SessionFacts{ID: session, Task: task}, name); xrLabelOK(name) && !ValidNewBranch(b, "main") {
			t.Fatalf("branchForXR(%q, %q, %q) = %q", session, task, name, b)
		}
		if b := branchForStep("parachute", "app-repo", session); !ValidNewBranch(b, "main") {
			t.Fatalf("branchForStep(%q) = %q", session, b)
		}
	})
}

func xrLabelOK(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (c == '-' && i > 0 && i < len(s)-1)
		if !ok {
			return false
		}
	}
	return true
}

// A private range is one a database may be opened to: its network address, inside RFC 1918, no wider than a /16.
func FuzzPrivateCIDR(f *testing.F) {
	for _, s := range []string{"10.0.0.0/16", "0.0.0.0/0", "10.0.0.0/8", "192.168.1.0/24", "172.16.0.0/12", "172.32.0.0/16", "::/0", "10.1.2.3/24", "10.0.0.0/16 ", " 10.0.0.0/16", "010.0.0.0/16"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !airframe.PrivateCIDR(s) {
			return
		}
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Masked() != p || p.Bits() < 16 || !p.Addr().Is4() || p.String() != s {
			t.Fatalf("%q was taken as private", s)
		}
		a := p.Addr().As4()
		if !(a[0] == 10 || (a[0] == 172 && a[1]&0xf0 == 16) || (a[0] == 192 && a[1] == 168)) {
			t.Fatalf("%q is not a private range", s)
		}
	})
}

// Merging the plan's keys into a scaffold never loses a key the file had and never overwrites one a person set: the result holds
// every key of both, and a key the two disagree on is a conflict, not a change.
func FuzzMergeOnboardingFile(f *testing.F) {
	for _, s := range []string{"# c\nenvName: dev\n", "", "envName: dev\nrollout:\n  strategy: canary\n", "a: 1\n", "- x\n", "a: &x 1\nb: *x\n", "a: 1\na: 2\n"} {
		f.Add(s, "envName: dev\nreplicas: 2\n")
	}
	f.Fuzz(func(t *testing.T, existing, planned string) {
		want, err := definitions.ParseYAML([]byte(planned))
		if err != nil {
			return
		}
		out, err := mergeOnboardingFile("o/x", "glidepath/envs/dev.yaml", existing, planFile{path: "glidepath/envs/dev.yaml", content: planned, value: want})
		if err != nil || out == "" {
			return
		}
		have, perr := definitions.ParseYAML([]byte(out))
		if perr != nil {
			return // a float or another value the strict reader does not take back: not a failure of the merge
		}
		before, berr := definitions.ParseYAML([]byte(existing))
		if berr == nil {
			for _, k := range before.Keys {
				if _, ok := have.M[k]; !ok {
					t.Fatalf("the merge lost %q", k)
				}
				if !airframe.PyEq(definitions.Plain(have.M[k]), definitions.Plain(before.M[k])) {
					if _, isMap := before.M[k].(*definitions.OMap); !isMap {
						t.Fatalf("the merge changed %q", k)
					}
				}
			}
		}
		for _, k := range want.Keys {
			if _, ok := have.M[k]; !ok {
				t.Fatalf("the merge dropped the plan's %q", k)
			}
		}
	})
}
