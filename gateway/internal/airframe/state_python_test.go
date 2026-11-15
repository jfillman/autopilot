package airframe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/core"
)

type stateCase struct {
	Name     string `json:"name"`
	Tool     string `json:"tool"`
	Args     []any  `json:"args"`
	Requests []struct {
		Target string `json:"target"`
		Status int    `json:"status"`
		Body   any    `json:"body"`
	} `json:"requests"`
	Result any `json:"result"`
	Error  *struct {
		Class, Rule, Message, Hint string
	} `json:"error"`
	Go *struct {
		Requests []struct {
			Target string `json:"target"`
			Status int    `json:"status"`
			Body   any    `json:"body"`
		} `json:"requests"`
		Result any `json:"result"`
		Error  *struct {
			Class, Rule, Message, Hint string
		} `json:"error"`
	} `json:"go"`
	Why string `json:"why"`
}

func jsonRound(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeAPI serves recorded answers by target and records what it was asked.
type fakeAPI struct {
	mu   sync.Mutex
	by   map[string][]byte
	st   map[string]int
	seen []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := r.URL.RequestURI()
	f.seen = append(f.seen, r.Method+" "+target)
	body, ok := f.by[target]
	if !ok {
		w.WriteHeader(599)
		return
	}
	w.WriteHeader(f.st[target])
	_, _ = w.Write(body)
}

func newReader(t *testing.T, srv *httptest.Server) airframe.Reader {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("fixture-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return backends.StateReader{KubeReader: &backends.KubeReader{Server: srv.URL, TokenFile: tok, AllowHTTP: true}}
}

func call(ctx context.Context, s *airframe.State, c stateCase) (any, error) {
	str := func(i int) string { v, _ := c.Args[i].(string); return v }
	opt := func(i int) *string {
		if len(c.Args) > i {
			v := str(i)
			return &v
		}
		return nil
	}
	switch c.Tool {
	case "status":
		return s.Status(ctx, str(0), opt(1))
	case "describe":
		return s.Describe(ctx, str(0), str(1))
	}
	return s.Verify(ctx, str(0), str(1), str(2), opt(3))
}

func TestStateToolsEqualPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct{ Cases []stateCase }
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) < 100 {
		t.Fatalf("only %d cases", len(fx.Cases))
	}
	differ := 0
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			reqs, want, wantErr := c.Requests, c.Result, c.Error
			if c.Go != nil {
				differ++
				if c.Why == "" {
					t.Fatal("a go expectation needs a why")
				}
				reqs, want, wantErr = c.Go.Requests, c.Go.Result, c.Go.Error
			}
			f := &fakeAPI{by: map[string][]byte{}, st: map[string]int{}}
			var wantSeen []string
			for _, r := range reqs {
				b, _ := json.Marshal(r.Body)
				f.by[r.Target], f.st[r.Target] = b, r.Status
				wantSeen = append(wantSeen, "GET "+r.Target)
			}
			srv := httptest.NewServer(f)
			defer srv.Close()
			s := &airframe.State{Kube: newReader(t, srv), Cluster: "hangar-dev", Workers: 8}
			got, err := call(context.Background(), s, c)
			sort.Strings(wantSeen)
			sort.Strings(f.seen)
			if !reflect.DeepEqual(wantSeen, f.seen) && !(len(wantSeen) == 0 && len(f.seen) == 0) {
				t.Fatalf("requests differ\nwant %v\n got %v", wantSeen, f.seen)
			}
			if wantErr != nil {
				switch wantErr.Class {
				case "Denied":
					var d *core.Denied
					if !asDenied(err, &d) || d.Rule != wantErr.Rule || d.Message != wantErr.Message || d.Hint != wantErr.Hint {
						t.Fatalf("want Denied %+v, got %v", *wantErr, err)
					}
				default:
					var ae *backends.AirframeError
					if !asAirframe(err, &ae) || ae.Msg != wantErr.Message {
						t.Fatalf("want AirframeError %q, got %v", wantErr.Message, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if g := jsonRound(t, got); !reflect.DeepEqual(g, want) {
				gb, _ := json.MarshalIndent(g, "", " ")
				wb, _ := json.MarshalIndent(want, "", " ")
				t.Fatalf("result differs\nwant %s\n got %s", bytes.TrimSpace(wb), bytes.TrimSpace(gb))
			}
		})
	}
	t.Logf("%d cases, %d with a go expectation", len(fx.Cases), differ)
}
