package readapi

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pingStore struct {
	captureStore
	pings atomic.Int64
	err   atomic.Value // error or nil
	delay time.Duration
}

func (p *pingStore) Ping(context.Context) error {
	p.pings.Add(1)
	time.Sleep(p.delay)
	if e, _ := p.err.Load().(error); e != nil {
		return e
	}
	return nil
}

// /readyz is open and does not take the in-flight limit, so it must never be a way to reach the database per request: it answers from a
// probe refreshed every ProbeEvery by one caller at a time.
func TestReadyzIsServedFromACachedProbe(t *testing.T) {
	st := &pingStore{delay: 20 * time.Millisecond}
	api, _ := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: st, Defs: defsFrom(t, nil), ProbeEvery: 200 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := get(api, "/readyz", ""); rec.Code != 200 {
				t.Errorf("%d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if n := st.pings.Load(); n != 1 {
		t.Fatalf("%d database probes for 300 requests", n)
	}
	for i := 0; i < 50; i++ {
		get(api, "/readyz", "")
	}
	if n := st.pings.Load(); n != 1 {
		t.Fatalf("a probe inside its lifetime reached the database: %d", n)
	}
	time.Sleep(250 * time.Millisecond)
	st.err.Store(context.DeadlineExceeded)
	if rec := get(api, "/readyz", ""); rec.Code != 503 || rec.Body.String() != `{"ok":false}` || st.pings.Load() != 2 {
		t.Fatalf("%d %s probes=%d", rec.Code, rec.Body.String(), st.pings.Load())
	}
	if rec := get(api, "/readyz", ""); rec.Code != 503 || st.pings.Load() != 2 {
		t.Fatalf("a failure was not cached too: %d probes=%d", rec.Code, st.pings.Load())
	}
}

type hangStore struct{ captureStore }

func (*hangStore) Ping(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// A database that never answers makes /readyz a 503 after the probe's limit (3 s), not a request that waits for ever, and the failure is
// then what the probe says for its interval.
func TestAHungReadinessProbeIsCutOffAtItsLimit(t *testing.T) {
	api, _ := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: &hangStore{}, Defs: defsFrom(t, nil)})
	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- get(api, "/readyz", "").Code }()
	select {
	case code := <-done:
		if d := time.Since(start); code != 503 || d < 2*time.Second || d > 8*time.Second {
			t.Fatalf("%d after %v: want a 503 after about 3 s", code, d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the probe never ended")
	}
}
