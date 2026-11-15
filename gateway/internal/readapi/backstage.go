package readapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Backstage is the catalog the people's groups and apps come from: Tower's Backstage, read with the gateway's own projected token for
// the backstage audience (re-read on every call), as reads.BackstageDirectory and BackstageOwnership do. A Clearance person
// `user:<login>` is the catalog's `user:default/<login>` (Tower signs people in by GitHub login).
//
// Owner-group resolution rule, in one place:
//   - a person's groups are the memberOf relations of their User entity; the apps they own are the Components whose spec.owner is the
//     person or one of their groups;
//   - an answer is cached for TTL (a minute, Python's), a failure for FailTTL (10 s: a catalog outage is not hammered, and a person
//     is not held to their own sessions for a minute after it ends);
//   - a person the catalog does not know (404) has no groups: that is an answer, not a failure;
//   - anything else (an error, a status, an answer that does not parse, a catalog that is not configured) is an ERROR: the caller is held
//     to what is theirs alone (their own sessions, their own inbox items) and the response says so (X-Autopilot-Owners: unknown). Nobody
//     is ever given more than the catalog's last answer allows, and a stale answer is never used past its TTL;
//   - an auditor (a deployment's admin) never asks the catalog, so an outage cannot lock them out, and a person never gets an auditor's view
//     from an outage.
type Backstage struct {
	Base      string
	TokenFile string
	TTL       time.Duration
	FailTTL   time.Duration
	Timeout   time.Duration
	// MaxPages bounds the apps read (DefaultMaxPages pages of 500 when 0): a person who owns more than that, or a catalog that does not stop
	// paging, is an error, so the caller is held to what is theirs alone (as an outage does), never given a partial list as if it were whole.
	MaxPages int
	HTTP     *http.Client
	Now      func() time.Time

	mu     sync.Mutex
	groups map[string]entry
	apps   map[string]entry
	sf     singleflight.Group
}

type entry struct {
	at  time.Time
	set map[string]bool
	err error
}

// nameRe is a catalog entity name (reads._NAME).
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// DefaultMaxPages is how many pages of 500 components one person's apps may take (10,000 components, the table's own entry bound).
const DefaultMaxPages = 20

const (
	maxEntries   = 10000
	maxCatalogIn = 4 << 20
)

// NewBackstage is a catalog client on base (checked by the caller as the catalog.read backend's is).
func NewBackstage(base, tokenFile string) *Backstage {
	return &Backstage{Base: strings.TrimRight(base, "/"), TokenFile: tokenFile, TTL: time.Minute, FailTTL: 10 * time.Second, Timeout: 10 * time.Second,
		HTTP:   &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		groups: map[string]entry{}, apps: map[string]entry{}}
}

func (b *Backstage) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// nameOf is the catalog name of a person principal, or false for one that is not a person or could not be an entity's name.
func nameOf(principal string) (string, bool) {
	name, ok := strings.CutPrefix(principal, "user:")
	if !ok || !nameRe.MatchString(strings.ToLower(name)) {
		return "", false
	}
	return name, true
}

func (b *Backstage) cached(m map[string]entry, name string) (entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := m[name]
	if !ok {
		return e, false
	}
	ttl := b.TTL
	if e.err != nil {
		ttl = b.FailTTL
	}
	return e, b.now().Sub(e.at) < ttl
}

func (b *Backstage) store(m map[string]entry, name string, e entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(m) >= maxEntries {
		for k, old := range m { // the expired first; if none is, the table is dropped rather than grown
			if b.now().Sub(old.at) >= b.TTL {
				delete(m, k)
			}
		}
		if len(m) >= maxEntries {
			clear(m)
		}
	}
	m[name] = e
}

func (b *Backstage) token() (string, error) {
	raw, err := os.ReadFile(b.TokenFile)
	if err != nil {
		return "", errors.New("the catalog token could not be read")
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" || strings.ContainsAny(tok, "\r\n") {
		return "", errors.New("the catalog token is empty")
	}
	return tok, nil
}

func (b *Backstage) get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	tok, err := b.token()
	if err != nil {
		return 0, nil, err
	}
	u := b.Base + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return 0, nil, errors.New("the catalog is unreachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogIn+1))
	if err != nil || len(body) > maxCatalogIn {
		return 0, nil, errors.New("the catalog's answer could not be read")
	}
	return resp.StatusCode, body, nil
}

// Groups is the person's group refs ("group:default/team-a").
func (b *Backstage) Groups(ctx context.Context, person string) (map[string]bool, error) {
	name, ok := nameOf(person)
	if !ok {
		return map[string]bool{}, nil
	}
	if e, hit := b.cached(b.groups, name); hit {
		return e.set, e.err
	}
	v, _, _ := b.sf.Do("g/"+name, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.Timeout)
		defer cancel()
		set, err := b.fetchGroups(fctx, name)
		b.store(b.groups, name, entry{at: b.now(), set: set, err: err})
		return entry{set: set, err: err}, nil
	})
	e := v.(entry)
	return e.set, e.err
}

func (b *Backstage) fetchGroups(ctx context.Context, name string) (map[string]bool, error) {
	status, body, err := b.get(ctx, "/api/catalog/entities/by-name/user/default/"+url.PathEscape(name), nil)
	if err != nil {
		return map[string]bool{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return map[string]bool{}, nil
	default:
		return map[string]bool{}, fmt.Errorf("the catalog answered %d", status)
	}
	var doc struct {
		Relations []struct {
			Type      string `json:"type"`
			TargetRef string `json:"targetRef"`
		} `json:"relations"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return map[string]bool{}, errors.New("the catalog's answer does not parse")
	}
	out := map[string]bool{}
	for _, r := range doc.Relations {
		if r.Type == "memberOf" && strings.HasPrefix(r.TargetRef, "group:") && len(r.TargetRef) <= 300 {
			out[r.TargetRef] = true
		}
	}
	return out, nil
}

// Apps is the lower-cased names of the Components the person or one of their groups owns.
func (b *Backstage) Apps(ctx context.Context, person string) (map[string]bool, error) {
	name, ok := nameOf(person)
	if !ok {
		return map[string]bool{}, nil
	}
	if e, hit := b.cached(b.apps, name); hit {
		return e.set, e.err
	}
	v, _, _ := b.sf.Do("a/"+name, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.Timeout)
		defer cancel()
		set, err := b.fetchApps(fctx, name, person)
		b.store(b.apps, name, entry{at: b.now(), set: set, err: err})
		return entry{set: set, err: err}, nil
	})
	e := v.(entry)
	return e.set, e.err
}

func (b *Backstage) fetchApps(ctx context.Context, name, person string) (map[string]bool, error) {
	groups, err := b.Groups(ctx, person)
	if err != nil {
		return map[string]bool{}, err
	}
	owners := []string{"user:default/" + name}
	for g := range groups {
		owners = append(owners, g)
	}
	sort.Strings(owners[1:])
	sort.Strings(owners)
	q := url.Values{}
	for _, o := range owners {
		q.Add("filter", "kind=component,spec.owner="+o)
	}
	q.Add("fields", "metadata.name")
	q.Add("limit", "500")
	maxPages := b.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	apps := map[string]bool{}
	seen := map[string]bool{}
	for page := 1; ; page++ {
		status, body, err := b.get(ctx, "/api/catalog/entities/by-query", q)
		if err != nil {
			return map[string]bool{}, err
		}
		if status != http.StatusOK {
			return map[string]bool{}, fmt.Errorf("the catalog answered %d", status)
		}
		var doc struct {
			Items []struct {
				Metadata *struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
			PageInfo struct {
				NextCursor string `json:"nextCursor"`
			} `json:"pageInfo"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return map[string]bool{}, errors.New("the catalog's answer does not parse")
		}
		for _, it := range doc.Items {
			if it.Metadata != nil && it.Metadata.Name != "" {
				apps[strings.ToLower(it.Metadata.Name)] = true
			}
		}
		next := doc.PageInfo.NextCursor
		if next == "" {
			return apps, nil
		}
		if page >= maxPages {
			return map[string]bool{}, fmt.Errorf("the catalog has more than %d pages of components for this person: not read (a partial list would be a wrong one)", maxPages)
		}
		if seen[next] || len(next) > 4096 {
			return map[string]bool{}, errors.New("the catalog's paging does not advance")
		}
		seen[next] = true
		q.Set("cursor", next)
	}
}

// DispatchDirectory is the Backstage directory as the gateway's human inbox and person controls read it (dispatch.Directory): there a
// failure is an empty set (no group, no access), which is the same fail-closed rule; the error is logged by Access for the read API.
type DispatchDirectory struct{ B *Backstage }

func (d DispatchDirectory) Groups(ctx context.Context, principal string) map[string]bool {
	g, _ := d.B.Groups(ctx, principal)
	return g
}
