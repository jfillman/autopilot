package readapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

var locales = []string{"C", "en_US.utf8"}

// Paging is lossless: rows that share a time (and ids that a locale would sort differently from bytes) come back once each, in one order,
// whatever the page size; the pages added up are the whole list. On a database with the C collation and on one with a locale's.
func TestPagingIsLosslessUnderTies(t *testing.T) {
	for _, loc := range locales {
		t.Run(loc, func(t *testing.T) {
			dsn, st := scratchLocale(t, loc)
			ctx := context.Background()
			rng := rand.New(rand.NewSource(7))
			base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
			stamps := []string{"2026-11-14T10:00:00+00:00", "2026-11-14T10:00:00.25+00:00", "2026-11-14T10:00:00.5+00:00", "2026-11-14T10:01:00+00:00"}
			idChars := []string{"s", "S", "-", "_", ".", "1", "a", "B", "9"}
			seen := map[string]bool{}
			for len(seen) < 60 {
				id := ""
				for i := 0; i < 4; i++ {
					id += idChars[rng.Intn(len(idChars))]
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				ts := stamps[rng.Intn(len(stamps))]
				who := []string{"user:alice", "user:bob"}[rng.Intn(2)]
				rec, _ := json.Marshal(map[string]any{"id": id, "principal": who, "agent": "triage-agent", "task_id": "t", "created_at": ts, "expires_at": ts, "closed": true})
				if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES ($1, $2::jsonb, $3, true)", id, string(rec), base); err != nil {
					t.Fatal(err)
				}
				at := base.Add(time.Duration(rng.Intn(4)) * time.Second)
				ev, _ := json.Marshal(map[string]any{"id": "e" + id, "received_at": at.Format("2006-01-02T15:04:05") + "+00:00", "outcomes": []any{}, "subject": map[string]string{"id": "x"}})
				if _, err := st.Pool.Exec(ctx, "INSERT INTO gateway_events (id, received_at, source, app, routing_key, record) VALUES ($1, $2, 's', 'flight-api', 'k', $3::jsonb)", "e"+id, at, string(ev)); err != nil {
					t.Fatal(err)
				}
			}
			store, err := OpenPg(ctx, dsn, 4)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			a, _ := New(&API{Access: accessWith(mapDir{"user:alice": nil}, mapOwn{"user:alice": {"flight-api"}}), Store: store, Defs: defsFrom(t, nil)})
			for _, who := range []string{"admin", "alice"} {
				for _, path := range []string{"/v1/sessions", "/v1/events"} {
					whole := ids(t, get(a, path+"?limit=200", who).Body.Bytes())
					if len(whole) < 20 {
						t.Fatalf("%s %s: %d rows", who, path, len(whole))
					}
					for size := 1; size <= 7; size++ {
						var walked []string
						next := ""
						for pages := 0; ; pages++ {
							if pages > 200 {
								t.Fatal("the cursor never ends")
							}
							url := fmt.Sprintf("%s?limit=%d", path, size)
							if next != "" {
								url += "&after=" + urlQ(next)
							}
							rec := get(a, url, who)
							if rec.Code != 200 {
								t.Fatalf("%s: %d %s", url, rec.Code, rec.Body.String())
							}
							var pg struct {
								Items []map[string]any `json:"items"`
								Next  *string          `json:"next"`
							}
							_ = json.Unmarshal(rec.Body.Bytes(), &pg)
							for _, it := range pg.Items {
								walked = append(walked, it["id"].(string))
							}
							if pg.Next == nil {
								break
							}
							next = *pg.Next
						}
						if fmt.Sprint(walked) != fmt.Sprint(whole) {
							t.Fatalf("%s %s pages of %d (%s): lost or repeated rows\n got  %v\n want %v", who, path, size, loc, walked, whole)
						}
						seenIDs := map[string]bool{}
						for _, id := range walked {
							if seenIDs[id] {
								t.Fatalf("%s repeated", id)
							}
							seenIDs[id] = true
						}
					}
				}
			}
		})
	}
}
