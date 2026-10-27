package worktreeguard

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// fakeProbe is the injectable Probe seam for the GuardRemoval tests: every
// fact and failure is scripted per-case, no git involved.
type fakeProbe struct {
	isRepo      bool
	branch      string
	headSha     string
	headErr     bool
	reach       bool
	reachErr    bool
	siblings    []string
	siblingsErr error
	fetchErr    error
	verifyErr   error
	fetchCalls  int
}

func (f *fakeProbe) IsRepo() bool { return f.isRepo }

func (f *fakeProbe) HeadInfo() (string, string, error) {
	if f.headErr {
		return "", "", errFake("head probe failed")
	}
	return f.branch, f.headSha, nil
}

func (f *fakeProbe) HeadReachableInRigRepo() (bool, error) {
	if f.reachErr {
		return false, errFake("reachability probe failed")
	}
	return f.reach, nil
}

func (f *fakeProbe) UnsecuredSiblingBranches() ([]string, error) {
	if f.siblingsErr != nil {
		return nil, f.siblingsErr
	}
	return f.siblings, nil
}

func (f *fakeProbe) FetchIntoQuarantine(tag, branch, headSha string) error {
	f.fetchCalls++
	if f.fetchErr != nil {
		return f.fetchErr
	}
	return nil
}

func (f *fakeProbe) VerifyQuarantied(headSha string) error { return f.verifyErr }

type errFake string

func (e errFake) Error() string { return string(e) }

func TestDecideTable(t *testing.T) {
	cases := []struct {
		name  string
		facts Facts
		want  Verdict
	}{
		{
			name:  "not a repo refuses",
			facts: Facts{IsRepo: false},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "not-a-repo"},
		},
		{
			name:  "head probe failure refuses fail-closed",
			facts: Facts{IsRepo: true, HeadProbeErr: true},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "head-probe-failed"},
		},
		{
			name:  "no commits nothing to secure",
			facts: Facts{IsRepo: true, HeadSha: ""},
			want:  Verdict{Action: "nothing-to-secure", Secured: true, Reason: "no-commits"},
		},
		{
			name:  "reachability probe failure refuses fail-closed",
			facts: Facts{IsRepo: true, HeadSha: "abc123def4", ReachableProbeErr: true},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "reachability-unknown"},
		},
		{
			name:  "head reachable in rig already secured",
			facts: Facts{IsRepo: true, HeadSha: "abc123def4", HeadReachableInRig: true},
			want:  Verdict{Action: "already-secured", Secured: true, Reason: "reachable-in-rig-repo"},
		},
		{
			name:  "unreachable head needs a copy",
			facts: Facts{IsRepo: true, Branch: "work", HeadSha: "abc123def4"},
			want:  Verdict{Action: "secured-copy", Secured: false, Reason: "copy-absent-rig-repo-available"},
		},
		{
			name:  "detached head without branch still needs a copy",
			facts: Facts{IsRepo: true, Branch: "", HeadSha: "abc123def4567890"},
			want:  Verdict{Action: "secured-copy", Secured: false, Reason: "copy-absent-rig-repo-available"},
		},
		{
			name:  "unsecured non-head branch refuses even when head is secured",
			facts: Facts{IsRepo: true, Branch: "work", HeadSha: "abc123def4", HeadReachableInRig: true, NonHeadUnsecured: []string{"feature/x"}},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "non-head-branches-unsecured: feature/x"},
		},
		{
			name:  "unsecured non-head branch refuses before the copy runs",
			facts: Facts{IsRepo: true, Branch: "work", HeadSha: "abc123def4", NonHeadUnsecured: []string{"a", "b"}},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "non-head-branches-unsecured: a, b"},
		},
		{
			name:  "unborn head with unsecured siblings still refuses",
			facts: Facts{IsRepo: true, HeadSha: "", NonHeadUnsecured: []string{"orphaned-work"}},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "non-head-branches-unsecured: orphaned-work"},
		},
		{
			name:  "sibling probe failure refuses fail-closed",
			facts: Facts{IsRepo: true, HeadSha: "abc123def4", NonHeadProbeErr: true},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "sibling-probe-failed"},
		},
		{
			name:  "secured siblings do not block an otherwise secured head",
			facts: Facts{IsRepo: true, Branch: "work", HeadSha: "abc123def4", HeadReachableInRig: true},
			want:  Verdict{Action: "already-secured", Secured: true, Reason: "reachable-in-rig-repo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.facts)
			if got != tc.want {
				t.Fatalf("Decide(%+v) = %+v, want %+v", tc.facts, got, tc.want)
			}
		})
	}
}

func TestGuardRemovalSecuredCopySuccess(t *testing.T) {
	p := &fakeProbe{isRepo: true, branch: "work", headSha: "abc123def4", reach: false}
	var stderr bytes.Buffer
	v := GuardRemoval(p, "/wt/x", "sess-1", &stderr)
	if !v.Secured || v.Action != "secured-copy" {
		t.Fatalf("verdict = %+v, want secured-copy", v)
	}
	if p.fetchCalls != 1 {
		t.Fatalf("fetch calls = %d, want 1", p.fetchCalls)
	}
	if !strings.Contains(stderr.String(), "secured-copy") {
		t.Fatalf("secured copy must name itself on stderr: %q", stderr.String())
	}
}

func TestGuardRemovalFetchFailRefuses(t *testing.T) {
	p := &fakeProbe{
		isRepo: true, branch: "work", headSha: "abc123def4", reach: false,
		fetchErr: errFake("no such path"),
	}
	var stderr bytes.Buffer
	v := GuardRemoval(p, "/wt/x", "sess-1", &stderr)
	if v.Secured {
		t.Fatalf("verdict = %+v, want refused", v)
	}
	if !strings.Contains(v.Reason, "fetch-failed") {
		t.Fatalf("reason = %q, want fetch-failed detail", v.Reason)
	}
	line := stderr.String()
	if !strings.HasPrefix(line, "worktreeguard: secured-copy repo=/wt/x") {
		t.Fatalf("stderr line = %q, want structured refuse line", line)
	}
	if !strings.Contains(line, "reason=fetch-failed") {
		t.Fatalf("stderr line = %q, want reason=fetch-failed", line)
	}
}

func TestGuardRemovalVerifyFailRefuses(t *testing.T) {
	p := &fakeProbe{
		isRepo: true, branch: "work", headSha: "abc123def4", reach: false,
		verifyErr: errFake("object missing"),
	}
	var stderr bytes.Buffer
	v := GuardRemoval(p, "/wt/x", "sess-1", &stderr)
	if v.Secured {
		t.Fatalf("verdict = %+v, want refused after verify failure", v)
	}
	if !strings.Contains(stderr.String(), "worktreeguard: secured-copy repo=/wt/x") {
		t.Fatalf("stderr = %q, want refuse line", stderr.String())
	}
}

func TestGuardRemovalRefuseWritesExactlyOneLine(t *testing.T) {
	p := &fakeProbe{isRepo: false}
	var stderr bytes.Buffer
	GuardRemoval(p, "/wt/y", "sess-2", &stderr)
	if n := strings.Count(stderr.String(), "\n"); n != 1 {
		t.Fatalf("stderr lines = %d, want exactly 1: %q", n, stderr.String())
	}
}

// TestGuardRemovalSiblingRefuseSkipsFetch: an unsecured sibling refuses
// BEFORE the head's quarantine fetch — a refused removal must not leave
// half-written quarantine state behind.
func TestGuardRemovalSiblingRefuseSkipsFetch(t *testing.T) {
	p := &fakeProbe{isRepo: true, branch: "work", headSha: "abc123def4", reach: false, siblings: []string{"feature/x"}}
	var stderr bytes.Buffer
	v := GuardRemoval(p, "/wt/x", "sess-9", &stderr)
	if v.Secured || v.Action != "refuse" {
		t.Fatalf("verdict = %+v, want refuse on unsecured sibling", v)
	}
	if !strings.Contains(v.Reason, "non-head-branches-unsecured") {
		t.Fatalf("reason = %q, want non-head-branches-unsecured", v.Reason)
	}
	if p.fetchCalls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (refuse must precede the copy)", p.fetchCalls)
	}
	if !strings.Contains(stderr.String(), "worktreeguard: refuse repo=/wt/x") {
		t.Fatalf("stderr = %q, want refuse line", stderr.String())
	}
}

// TestQuarantineRefNames: a branch copy is named per CONTENT
// (<tag>/<branch>-<sha12>) so a later copy under the same tag and branch can
// never overwrite an earlier one; a detached copy already carries its sha.
func TestQuarantineRefNames(t *testing.T) {
	cases := []struct {
		tag, branch, sha, want string
	}{
		{"sess/1", "feature/x", "abc123def4567890", "refs/gc-quarantine/sess-1/feature-x-abc123def456"},
		{"ga-1", "work", "abc123def4", "refs/gc-quarantine/ga-1/work-abc123def4"},
		{"t", "", "abc123def4567890", "refs/gc-quarantine/t/detached-abc123def456"},
	}
	for _, c := range cases {
		if got := QuarantineRef(c.tag, c.branch, c.sha); got != c.want {
			t.Errorf("QuarantineRef(%q, %q, %q) = %q, want %q", c.tag, c.branch, c.sha, got, c.want)
		}
	}
}

// TestGuardRemovalSecuredCopyNamesRef: rescued work must never vanish
// silently — a secured copy writes exactly one line naming where it went.
func TestGuardRemovalSecuredCopyNamesRef(t *testing.T) {
	p := &fakeProbe{isRepo: true, branch: "work", headSha: "abc123def4", reach: false}
	var stderr bytes.Buffer
	GuardRemoval(p, "/wt/x", "sess-1", &stderr)
	line := stderr.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Fatalf("stderr lines = %d, want exactly 1: %q", n, line)
	}
	if !strings.HasPrefix(line, "worktreeguard: secured-copy repo=/wt/x") {
		t.Fatalf("stderr = %q, want secured-copy line", line)
	}
	if !strings.Contains(line, "ref=refs/gc-quarantine/sess-1/work-abc123def4") {
		t.Fatalf("stderr = %q, want ref=refs/gc-quarantine/sess-1/work-abc123def4", line)
	}
}

// TestAssessHasNoSideEffects: Assess answers what GuardRemoval WOULD do
// (dry-run surfaces) without copying anything or writing a line.
func TestAssessHasNoSideEffects(t *testing.T) {
	p := &fakeProbe{isRepo: true, branch: "work", headSha: "abc123def4", reach: false}
	v := Assess(p)
	if v.Action != ActionSecuredCopy || v.Secured {
		t.Fatalf("Assess = %+v, want secured-copy (not yet secured)", v)
	}
	if p.fetchCalls != 0 {
		t.Fatalf("fetch calls = %d, want 0", p.fetchCalls)
	}
}

// TestDecideCapsSiblingList: a repo with many unsecured branches yields a
// bounded reason (first ten names + count), not an unbounded log line.
func TestDecideCapsSiblingList(t *testing.T) {
	var names []string
	for i := 1; i <= 12; i++ {
		names = append(names, fmt.Sprintf("b%02d", i))
	}
	v := Decide(Facts{IsRepo: true, HeadSha: "abc123def4", NonHeadUnsecured: names})
	want := "non-head-branches-unsecured: b01, b02, b03, b04, b05, b06, b07, b08, b09, b10 (+2 more)"
	if v.Reason != want {
		t.Fatalf("reason = %q, want %q", v.Reason, want)
	}
}
