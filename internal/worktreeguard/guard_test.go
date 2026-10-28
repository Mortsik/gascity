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
	rigInside   bool
	rigErr      error
	fetchErr    error
	verifyErr   error
	fetchCalls  int
	// laterShas, when set, are returned by HeadInfo calls after the first
	// (one per call, last one sticks) — a HEAD moving under the guard.
	laterShas []string
	headCalls int
	// laterRefs, when set, is what UnsecuredRefs reports after its first
	// call — a ref created under the guard.
	laterRefs []string
	refCalls  int
	// switchBranch, when set, makes HeadInfo calls after the first report
	// laterBranch instead of branch — a checkout under the guard.
	switchBranch bool
	laterBranch  string
}

func (f *fakeProbe) IsRepo() bool { return f.isRepo }

func (f *fakeProbe) HeadInfo() (string, string, error) {
	f.headCalls++
	if f.headErr {
		return "", "", errFake("head probe failed")
	}
	branch := f.branch
	if f.headCalls > 1 && f.switchBranch {
		branch = f.laterBranch
	}
	if f.headCalls > 1 && len(f.laterShas) > 0 {
		i := f.headCalls - 2
		if i >= len(f.laterShas) {
			i = len(f.laterShas) - 1
		}
		return branch, f.laterShas[i], nil
	}
	return branch, f.headSha, nil
}

func (f *fakeProbe) SecuredInRig(string) (bool, error) {
	if f.reachErr {
		return false, errFake("reachability probe failed")
	}
	return f.reach, nil
}

func (f *fakeProbe) RigOutsideRepo() (bool, error) {
	if f.rigErr != nil {
		return false, f.rigErr
	}
	return !f.rigInside, nil
}

func (f *fakeProbe) UnsecuredRefs(string) ([]string, error) {
	f.refCalls++
	if f.refCalls > 1 && f.laterRefs != nil {
		return f.laterRefs, nil
	}
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
			facts: Facts{IsRepo: true, Branch: "work", HeadSha: "abc123def4", HeadReachableInRig: true, UnsecuredRefs: []string{"feature/x"}},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "non-head-refs-unsecured: feature/x"},
		},
		{
			name:  "unsecured non-head branch refuses before the copy runs",
			facts: Facts{IsRepo: true, Branch: "work", HeadSha: "abc123def4", UnsecuredRefs: []string{"a", "b"}},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "non-head-refs-unsecured: a, b"},
		},
		{
			name:  "unborn head with unsecured siblings still refuses",
			facts: Facts{IsRepo: true, HeadSha: "", UnsecuredRefs: []string{"orphaned-work"}},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "non-head-refs-unsecured: orphaned-work"},
		},
		{
			name:  "ref probe failure refuses fail-closed",
			facts: Facts{IsRepo: true, HeadSha: "abc123def4", RefProbeErr: true},
			want:  Verdict{Action: "refuse", Secured: false, Reason: "ref-probe-failed"},
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
	if !strings.Contains(v.Reason, "non-head-refs-unsecured") {
		t.Fatalf("reason = %q, want non-head-refs-unsecured", v.Reason)
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
	v := Decide(Facts{IsRepo: true, HeadSha: "abc123def4", UnsecuredRefs: names})
	want := "non-head-refs-unsecured: b01, b02, b03, b04, b05, b06, b07, b08, b09, b10 (+2 more)"
	if v.Reason != want {
		t.Fatalf("reason = %q, want %q", v.Reason, want)
	}
}

// TestDecideRefusesWhenRigLivesInsideDoomedTree: a "copy" whose storage
// dies with the directory is no copy — Sol REWORK 2026-10-28 (2).
func TestDecideRefusesWhenRigLivesInsideDoomedTree(t *testing.T) {
	for _, tc := range []struct {
		facts Facts
		want  string
	}{
		{Facts{IsRepo: true, HeadSha: "abc123def4", HeadReachableInRig: true, RigInsideDoomed: true}, "rig-inside-doomed-tree"},
		{Facts{IsRepo: true, HeadSha: "abc123def4", HeadReachableInRig: true, RigProbeErr: true}, "rig-probe-failed"},
	} {
		v := Decide(tc.facts)
		if v.Secured || v.Action != ActionRefuse || v.Reason != tc.want {
			t.Fatalf("Decide(%+v) = %+v, want refuse/%s", tc.facts, v, tc.want)
		}
	}
}

// TestGuardRemovalRefusesWhenHeadMoves: the decision, the copy and the
// verification are bound to ONE HEAD reading; a HEAD that moved by the time
// the guard finishes refuses (Sol REWORK 2026-10-28 (3)).
func TestGuardRemovalRefusesWhenHeadMoves(t *testing.T) {
	for _, reach := range []bool{false, true} {
		p := &fakeProbe{isRepo: true, branch: "work", headSha: "aaaaaaaaaaaa1", reach: reach, laterShas: []string{"bbbbbbbbbbbb2"}}
		var stderr bytes.Buffer
		v := GuardRemoval(p, "/wt/x", "sess-1", &stderr)
		if v.Secured || v.Reason != "head-moved" {
			t.Fatalf("reach=%v: verdict = %+v, want refuse head-moved (stderr %q)", reach, v, stderr.String())
		}
	}
}

// TestGuardRemovalRefusesWhenBranchSwitches: HEAD's sha unchanged but the
// checkout switched branches (the original branch may have advanced and is
// no longer the excluded head question) refuses (Web Sol REWORK 2026-10-28
// round 4 (2)).
func TestGuardRemovalRefusesWhenBranchSwitches(t *testing.T) {
	for _, later := range []string{"main", ""} {
		for _, reach := range []bool{false, true} {
			p := &fakeProbe{isRepo: true, branch: "work", headSha: "aaaaaaaaaaaa1", reach: reach, switchBranch: true, laterBranch: later}
			var stderr bytes.Buffer
			v := GuardRemoval(p, "/wt/x", "sess-1", &stderr)
			if v.Secured || v.Reason != "head-moved" {
				t.Fatalf("later=%q reach=%v: verdict = %+v, want refuse head-moved", later, reach, v)
			}
		}
	}
}

// TestGuardRemovalRefusesWhenRefsMove: a ref created while the guard ran
// (after the facts were read) refuses.
func TestGuardRemovalRefusesWhenRefsMove(t *testing.T) {
	p := &fakeProbe{isRepo: true, branch: "work", headSha: "abc123def4", reach: true, laterRefs: []string{"refs/tags/new"}}
	var stderr bytes.Buffer
	if v := GuardRemoval(p, "/wt/x", "sess-1", &stderr); v.Secured || v.Reason != "refs-moved" {
		t.Fatalf("verdict = %+v, want refuse refs-moved", v)
	}
}

// TestGuardRemovalNotifyLineStaysOneLine: git stderr folded into a reason
// (fetch failures) must not break the one-structured-line contract.
func TestGuardRemovalNotifyLineStaysOneLine(t *testing.T) {
	p := &fakeProbe{isRepo: true, branch: "work", headSha: "abc123def4", fetchErr: errFake("fatal: one\nfatal: two\r\nthree")}
	var stderr bytes.Buffer
	GuardRemoval(p, "/wt/x\ny", "sess-1", &stderr)
	if n := strings.Count(stderr.String(), "\n"); n != 1 {
		t.Fatalf("stderr lines = %d, want exactly 1: %q", n, stderr.String())
	}
}
