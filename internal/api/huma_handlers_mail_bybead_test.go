package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
)

func TestFilterMessagesByBead(t *testing.T) {
	base := time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)
	mk := func(id, from, subject, body string, at time.Time) mail.Message {
		return mail.Message{ID: id, From: from, Subject: subject, Body: body, CreatedAt: at}
	}
	msgs := []mail.Message{
		mk("m1", "poe-pricer/agentforge-core.agentforge-executor-adhoc-47827dd7c3", "READY: execute al6lr4 — pass", "body mentions poe2_pricer-qf1exx too", base),
		mk("m2", "human", "patrol-city: note", "unrelated", base.Add(time.Minute)),
		mk("m3", "poe-pricer--agentforge-core__agentforge-executor-1-pool", "READY 8hmy1w", "for poe2_pricer-qf1exx", base.Add(2*time.Minute)),
		mk("m4", "poe-pricer/agentforge-core.agentforge-executor-adhoc-779e1a0ae9", "BLOCKED: poe2_pricer-qf1exx — dup", "x", base.Add(3*time.Minute)),
	}

	// kind=any: subject OR body mention, newest first.
	got := filterMessagesByBead(msgs, "poe2_pricer-qf1exx", "any")
	if len(got) != 3 {
		t.Fatalf("any: len = %d, want 3 (subject match + body match + subject match)", len(got))
	}
	if got[0].ID != "m4" || got[1].ID != "m3" || got[2].ID != "m1" {
		t.Fatalf("any: order = %s,%s,%s; want m4,m3,m1", got[0].ID, got[1].ID, got[2].ID)
	}

	// kind=executor: the human-sender patrol note never matched anyway; a
	// human-sender message that MENTIONS the id is excluded under executor.
	humanMention := append(msgs, mk("m5", "human", "note on poe2_pricer-qf1exx", "y", base.Add(4*time.Minute)))
	got = filterMessagesByBead(humanMention, "poe2_pricer-qf1exx", "executor")
	if len(got) != 3 {
		t.Fatalf("executor: len = %d, want 3 (m5 excluded by sender)", len(got))
	}
	if got[0].ID != "m4" {
		t.Fatalf("executor: newest = %s, want m4", got[0].ID)
	}

	// No match and empty id.
	if got := filterMessagesByBead(msgs, "nonexistent-id", "any"); len(got) != 0 {
		t.Fatalf("no-match: len = %d, want 0", len(got))
	}
	if got := filterMessagesByBead(msgs, "  ", "any"); got != nil {
		t.Fatalf("empty id: want nil, got %d", len(got))
	}

	// Deterministic tie-break on id at equal timestamps.
	tie := []mail.Message{
		mk("b", "r/agentforge-executor", "READY tie-id", "", base),
		mk("a", "r/agentforge-executor", "READY tie-id", "", base),
	}
	got = filterMessagesByBead(tie, "tie-id", "any")
	if len(got) != 2 || got[0].ID != "a" {
		t.Fatalf("tie-break: got %v", []string{got[0].ID, got[1].ID})
	}
}

func TestMailByBeadEndpoint(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	mp := state.cityMailProv
	if _, err := mp.Send("poe-pricer/agentforge-core.agentforge-executor-adhoc-47827dd7c3", "human",
		"READY: execute al6lr4 — poe2_pricer-qf1exx pass", "done, landed 1234"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := mp.Send("human", "human", "patrol", "unrelated body"); err != nil {
		t.Fatalf("send: %v", err)
	}

	req := httptest.NewRequest("GET", cityURL(state, "/mail/by-bead/poe2_pricer-qf1exx")+"?kind=executor&limit=5", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var out MailListOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Body.Total != 1 || len(out.Body.Items) != 1 {
		t.Fatalf("total=%d items=%d, want 1/1", out.Body.Total, len(out.Body.Items))
	}
	if !contains(out.Body.Items[0].Subject, "READY") || !contains(out.Body.Items[0].From, "agentforge-executor") {
		t.Fatalf("wrong item: %+v", out.Body.Items[0])
	}

	// No-match bead id: empty list, 200 (a closed issue without executor mail
	// is normal, not an error).
	req = httptest.NewRequest("GET", cityURL(state, "/mail/by-bead/nope-123"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-match status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Body.Total != 0 || len(out.Body.Items) != 0 {
		t.Fatalf("no-match total=%d, want 0", out.Body.Total)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
