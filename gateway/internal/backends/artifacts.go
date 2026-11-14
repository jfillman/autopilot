package backends

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
)

// artifact.put and artifact.get (engine/pg.py PgArtifacts, engine/backends.py core_handlers): text a task's sessions keep for
// each other (a child's report, a plan), in the gateway's own database and nowhere else. Both are the gateway's own reads
// and writes: nothing here reaches another service.
//
// THE ACCESS RULE (Python's, pinned by TestArtifactsAreKeptByTask and the equivalence fixtures): an artifact belongs to a
// task, never to a session. The key is (session.TaskID, name). A top-level session gets a task id of its own when it opens,
// and every child run it spawns, at any depth, takes its parent's: so a whole run tree reads and overwrites the same
// artifacts, and a sibling run in the tree sees what its sibling wrote. A session of another task (another person's,
// another agent's, a later run of the same agent) neither sees nor replaces any of them: it asks for a name under its own
// task id, which is another row. There is no list and no read by task id: an agent cannot name another task. A session with
// no task id has no access at all (Python would key all of them on "", and share).
//
// Bounds (Python's): a name is 1 to 100 of letters, digits, '.', '_' and '-'; content is text of at most 256 KiB; a task may
// keep 32 artifacts and 2 MiB; an artifact is gone 7 days after its last write; a put with an existing name replaces it.
// An artifact is returned as a JSON string inside a tool result: it is never served as a document, so what it holds is data
// to the agent and nothing a browser or shell will run.

const (
	MaxArtifactBytes = 256 << 10
	MaxArtifactCount = 32
	MaxArtifactTotal = 2 << 20
	ArtifactTTL      = 7 * 24 * time.Hour
)

var artifactName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// ArtifactStore keeps artifacts by task. Put validates nothing: ValidatePut did, and Put holds the quota.
type ArtifactStore interface {
	// Put stores (or replaces) one artifact and answers {stored, bytes, expires_at}; it refuses with an ART-QUOTA *core.Denied
	// when the task would hold more than MaxArtifactCount artifacts or MaxArtifactTotal bytes.
	Put(ctx context.Context, task, name, content string, now time.Time) (map[string]any, error)
	// Get is the content, or (_, false) when there is none or it has expired.
	Get(ctx context.Context, task, name string, now time.Time) (string, bool, error)
}

// ValidatePut is the checks of PgArtifacts.put before the database, with Python's text.
func ValidatePut(name, content any) (string, string, error) {
	n, ok := name.(string)
	if !ok || !artifactName.MatchString(n) {
		return "", "", &core.Denied{Rule: "ART-NAME", Message: echo(name) + " is not an artifact name", Hint: "Use letters, digits, '.', '_' or '-'."}
	}
	c, ok := content.(string)
	if !ok {
		return "", "", &core.Denied{Rule: "ART-CONTENT", Message: "an artifact's content is text", Hint: "Pass a string (JSON-encode structured data)."}
	}
	if size := len(c); size > MaxArtifactBytes {
		return "", "", &core.Denied{Rule: "ART-SIZE", Message: fmt.Sprintf("%d bytes is over the %d-byte limit for one artifact", size, MaxArtifactBytes), Hint: "Store a summary, or split it."}
	}
	if strings.ContainsRune(c, 0) {
		// Go is stricter: Python passes the NUL to the database driver, which fails the call as a DataError.
		return "", "", &core.Denied{Rule: "ART-CONTENT", Message: "an artifact's content is text", Hint: "Pass a string without a NUL character (JSON-encode structured data)."}
	}
	return n, c, nil
}

// QuotaDenied is the ART-QUOTA refusal.
func QuotaDenied() error {
	return &core.Denied{Rule: "ART-QUOTA", Message: fmt.Sprintf("this task may keep %d artifacts and %d bytes", MaxArtifactCount, MaxArtifactTotal),
		Hint: "Replace an existing artifact by its name instead of adding one."}
}

// PyIso is datetime.isoformat() of a UTC time, as Python writes expires_at.
func PyIso(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// PutResult is what artifact.put answers.
func PutResult(name string, size int, expires time.Time) map[string]any {
	return map[string]any{"stored": name, "bytes": size, "expires_at": PyIso(expires)}
}

// MemoryArtifacts is an ArtifactStore in memory with Python's rules, for tests and for a Go gateway without a database.
type MemoryArtifacts struct {
	mu   sync.Mutex
	rows map[[2]string]memArtifact
}

type memArtifact struct {
	content string
	expires time.Time
}

// Put implements ArtifactStore.
func (m *MemoryArtifacts) Put(_ context.Context, task, name, content string, now time.Time) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[[2]string]memArtifact{}
	}
	for k, r := range m.rows {
		if !r.expires.After(now) {
			delete(m.rows, k)
		}
	}
	count, total := 0, 0
	for k, r := range m.rows {
		if k[0] == task && k[1] != name {
			count++
			total += len(r.content)
		}
	}
	size := len(content)
	if count+1 > MaxArtifactCount || total+size > MaxArtifactTotal {
		return nil, QuotaDenied()
	}
	exp := now.Add(ArtifactTTL)
	m.rows[[2]string{task, name}] = memArtifact{content, exp}
	return PutResult(name, size, exp), nil
}

// Get implements ArtifactStore.
func (m *MemoryArtifacts) Get(_ context.Context, task, name string, now time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[[2]string{task, name}]
	if !ok || !r.expires.After(now) {
		return "", false, nil
	}
	return r.content, true, nil
}

var errNoTask = errors.New("this session has no task, so it has no artifacts")
