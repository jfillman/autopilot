package readapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

// Walking every page by its cursor returns every session the viewer may see exactly once, newest first, whatever the page size.
func TestPagesAddUpToTheWholeList(t *testing.T) {
	f := loadFixture(t)
	dsn, st := scratch(t)
	f.seed(t, st)
	store, err := OpenPg(context.Background(), dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := f.api(t, store)
	for _, c := range []struct{ who, path string }{{"admin", "/v1/sessions"}, {"alice", "/v1/sessions"}, {"bob", "/v1/sessions"}, {"admin", "/v1/events"}, {"bob", "/v1/events"}} {
		key := "created_at"
		if c.path == "/v1/events" {
			key = "received_at"
		}
		whole := ids(t, do(api, "GET", c.path+"?limit=200", &c.who, nil).Body.Bytes())
		for _, size := range []int{1, 2, 3, 5} {
			var walked []string
			next, pages := "", 0
			for {
				url := fmt.Sprintf("%s?limit=%d", c.path, size)
				if next != "" {
					url += "&after=" + urlQ(next)
				}
				rec := do(api, "GET", url, &c.who, nil)
				if rec.Code != 200 {
					t.Fatalf("%s %s: %d %s", c.who, url, rec.Code, rec.Body.String())
				}
				var page struct {
					Items []map[string]any `json:"items"`
					Next  *string          `json:"next"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				for _, it := range page.Items {
					walked = append(walked, it["id"].(string))
				}
				if pages++; pages > 100 {
					t.Fatal("the cursor never ends")
				}
				if page.Next == nil {
					break
				}
				if len(page.Items) != size {
					t.Fatalf("a next cursor on a short page")
				}
				next = *page.Next
				_ = key
			}
			if fmt.Sprint(walked) != fmt.Sprint(whole) {
				t.Errorf("%s %s pages of %d: %v, want %v", c.who, c.path, size, walked, whole)
			}
		}
		if len(whole) < 2 {
			t.Errorf("%s %s: only %d rows", c.who, c.path, len(whole))
		}
	}
}

func ids(t *testing.T, body []byte) []string {
	t.Helper()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range page.Items {
		out = append(out, it["id"].(string))
	}
	return out
}

// Rows with the same time come back in one order, by id, on every read: the order is deterministic, not the planner's.
func TestTiesAreOrderedByID(t *testing.T) {
	dsn, st := scratch(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	ts := at.Format("2006-01-02T15:04:05") + "+00:00"
	var want []string
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("s-%02d", (i*5)%8)
		b, _ := json.Marshal(map[string]any{"id": id, "principal": "user:alice", "agent": "triage-agent", "task_id": "t", "created_at": ts, "expires_at": ts, "closed": true})
		if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2::jsonb, $3, true)", id, string(b), at); err != nil {
			t.Fatal(err)
		}
		eb, _ := json.Marshal(map[string]any{"id": "e-" + id, "received_at": ts, "outcomes": []any{}, "subject": map[string]string{"id": "x"}})
		if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_events (id, received_at, source, app, routing_key, record) VALUES ($1, $2, 's', 'a', 'k', $3::jsonb)", "e-"+id, at, string(eb)); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(want)))
	store, _ := OpenPg(ctx, dsn, 2)
	defer store.Close()
	now := at.Add(time.Hour)
	for _, v := range []*Viewer{{Role: RoleAuditor}, {Role: RolePerson, Person: "user:alice"}} {
		for i := 0; i < 3; i++ {
			rows, err := store.Sessions(ctx, SessionFilter{}, v, 50, "", now)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range rows {
				got = append(got, r["id"].(string))
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%v: %v, want %v", v.Role, got, want)
			}
		}
	}
	ev, err := store.Events(ctx, EventFilter{}, &Viewer{Role: RoleAuditor}, 50, nil)
	if err != nil || len(ev) != 8 || ev[0]["id"] != "e-s-07" || ev[7]["id"] != "e-s-00" {
		t.Fatalf("%v %d", err, len(ev))
	}
}

// A person's own sessions are the same whether the catalog says they have teams or not, and a team's sessions are theirs only with the team.
func TestAPersonSeesTheirTeamsSessionsOnlyWithTheTeam(t *testing.T) {
	f := loadFixture(t)
	dsn, st := scratch(t)
	f.seed(t, st)
	store, _ := OpenPg(context.Background(), dsn, 2)
	defer store.Close()
	now := f.now(t)
	ctx := context.Background()
	with := &Viewer{Role: RolePerson, Person: "user:dave", Agents: map[string]bool{"pin-a": true}}
	without := &Viewer{Role: RolePerson, Person: "user:dave", Agents: map[string]bool{}}
	a, err := store.Sessions(ctx, SessionFilter{}, with, 50, "", now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := store.Sessions(ctx, SessionFilter{}, without, 50, "", now)
	ida, idb := idsOf(a), idsOf(b)
	if fmt.Sprint(idb) != "[s-005]" || fmt.Sprint(ida) != "[s-005 s-003]" {
		t.Fatalf("with the team %v, without %v", ida, idb)
	}
	// 40 agents: the fallback to one branch for all of them returns the same rows.
	many := map[string]bool{"pin-a": true}
	for i := 0; i < 40; i++ {
		many[fmt.Sprintf("agent-%d", i)] = true
	}
	c, err := store.Sessions(ctx, SessionFilter{}, &Viewer{Role: RolePerson, Person: "user:dave", Agents: many}, 50, "", now)
	if err != nil || fmt.Sprint(idsOf(c)) != fmt.Sprint(ida) {
		t.Fatalf("%v %v", idsOf(c), err)
	}
}

func idsOf(rs []Record) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r["id"].(string))
	}
	return out
}
