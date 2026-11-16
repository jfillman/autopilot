package dispatch_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// Defects of the Python gateway that the Go gateway fixes on purpose (docs/admin/go-gateway-release.md, "Fixes made for the
// release"). These tests assert the fixed behaviour; the recorded-Python parity tests keep asserting the rest.

// Options that differ only in surrounding whitespace are one option once stored (trimmed), so they are duplicates. Python
// compared them untrimmed: ["prod", " prod "] passed and was stored as ["prod", "prod"].
func TestCheckQuestionDedupesTheTrimmedOptions(t *testing.T) {
	for name, opts := range map[string][]any{
		"trailing":   {"prod", "prod "},
		"leading":    {" prod", "prod"},
		"both sides": {"\tprod\n", " prod "},
		"later pair": {"dev", "prod", " prod"},
	} {
		_, _, err := dispatch.CheckQuestion("Which?", opts)
		if ctlRule(err) != "HR-OPTIONS" {
			t.Errorf("%s: %v accepted (err %v)", name, opts, err)
		}
	}
	q, o, err := dispatch.CheckQuestion(" Which? ", []any{" dev ", "prod"})
	if err != nil || q != "Which?" || len(o) != 2 || o[0] != "dev" || o[1] != "prod" {
		t.Fatalf("distinct options were refused or stored untrimmed: %q %v %v", q, o, err)
	}
}

// failingSave is an inbox whose Save fails while fail is set.
type failingSave struct {
	dispatch.InboxStore
	fail  bool
	saves int
}

func (f *failingSave) Save(ctx context.Context, r *dispatch.HumanRequest) error {
	f.saves++
	if f.fail {
		return errors.New("inbox store unavailable")
	}
	return f.InboxStore.Save(ctx, r)
}

// A failed Save leaves the request pending, and the retry does not record the answer a second time: the record is reused, so
// the record and the inbox cannot disagree about a retry (one record, the same audit_seq).
func TestAnAnswerWhoseSaveFailedIsRecordedOnceOnRetry(t *testing.T) {
	p := newPeopleRig(t)
	const id = "h-0000000000ac"
	_ = p.inbox.Add(ctx, &dispatch.HumanRequest{ID: id, TaskID: "t-0000a1", Session: "s-0000a1", Agent: "orchestrator", Owner: "user:alice",
		Question: "q?", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), Status: "pending"})
	fs := &failingSave{InboxStore: p.inbox, fail: true}
	p.g.Inbox = fs
	alice := p.person("user:alice")
	if _, err := p.g.HumanAnswer(ctx, alice, id, "yes", ""); err == nil {
		t.Fatal("a failed Save answered")
	}
	if r, _ := p.inbox.Get(ctx, id); r.Status != "pending" || r.Answer != nil {
		t.Fatalf("a failed Save changed the request: %+v", r)
	}
	fs.fail = false
	out, err := p.g.HumanAnswer(ctx, alice, id, "yes", "")
	if err != nil {
		t.Fatal(err)
	}
	recs := p.recordsOf("human.answer")
	if len(recs) != 1 {
		t.Fatalf("%d human.answer records for one answered request, want 1", len(recs))
	}
	if out["audit_seq"] == nil {
		t.Fatalf("no audit_seq on the answer: %v", out)
	}
	// a different answer after a failed Save is a different answer: it is recorded as its own
	const id2 = "h-0000000000ad"
	_ = p.inbox.Add(ctx, &dispatch.HumanRequest{ID: id2, TaskID: "t-0000a1", Session: "s-0000a1", Agent: "orchestrator", Owner: "user:alice",
		Question: "q?", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), Status: "pending"})
	fs.fail = true
	_, _ = p.g.HumanAnswer(ctx, alice, id2, "yes", "")
	fs.fail = false
	if _, err := p.g.HumanAnswer(ctx, alice, id2, "no", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(p.recordsOf("human.answer")); n != 3 {
		t.Fatalf("%d records, want 3 (one, then the failed answer and the changed one)", n)
	}
}
