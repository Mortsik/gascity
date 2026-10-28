package worktreeguard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

// Real-git fixtures: a rig repo, a standalone clone of it holding an
// unpushed branch commit, and the corrupted-rig variant. These exercise the
// actual GitProbe subprocess surface, not fakes.

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.RunGit(t, dir, args...)
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q", "--initial-branch=main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
}

func commitFile(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(name+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", "add "+name)
}

// newStandaloneWithUnpushedBranch builds rig (with one commit) and a
// standalone clone whose branch "work" carries a commit the rig does not
// have. Returns (rigDir, cloneDir).
func newStandaloneWithUnpushedBranch(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	rig := filepath.Join(base, "rig")
	clone := filepath.Join(base, "doomed")
	for _, d := range []string{rig, clone} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	initRepo(t, rig)
	commitFile(t, rig, "base.txt")

	git(t, base, "clone", "-q", rig, clone)
	git(t, clone, "config", "user.email", "test@example.com")
	git(t, clone, "config", "user.name", "Test")
	git(t, clone, "checkout", "-q", "-b", "work")
	commitFile(t, clone, "work.txt")
	return rig, clone
}

func TestGitProbeGuardSecuresStandaloneUnpushedBranch(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)

	p := NewGitProbe(clone, rig)
	if p.IsRepo() != true {
		t.Fatal("clone not detected as repo")
	}
	branch, sha, err := p.HeadInfo()
	if err != nil || branch != "work" || len(sha) < 12 {
		t.Fatalf("HeadInfo = %q %q %v", branch, sha, err)
	}
	reach, err := p.SecuredInRig(sha)
	if err != nil || reach {
		t.Fatalf("SecuredInRig = %v %v, want false nil (unpushed)", reach, err)
	}

	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-42", &stderr)
	if !v.Secured || v.Action != "secured-copy" {
		t.Fatalf("verdict = %+v, want secured-copy (stderr: %q)", v, stderr.String())
	}
	refs := git(t, rig, "for-each-ref", "--format=%(refname)", QuarantineRefPrefix)
	if !strings.Contains(refs, QuarantineRefPrefix+"sess-42/work") {
		t.Fatalf("quarantine refs = %q, want %ssess-42/work", refs, QuarantineRefPrefix)
	}
	// The quarantined copy must actually contain the head commit.
	git(t, rig, "cat-file", "-e", sha+"^{commit}")
}

func TestGitProbeGuardRefusesWhenRigUnusable(t *testing.T) {
	_, clone := newStandaloneWithUnpushedBranch(t)
	// Point the probe at a directory that is not a git repo.
	notRig := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(notRig, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	p := NewGitProbe(clone, notRig)
	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-43", &stderr)
	// The rig-independence probe runs first: an unusable rig cannot even
	// report where its object store lives, which refuses fail-closed.
	if v.Secured || v.Action != "refuse" || v.Reason != "rig-probe-failed" {
		t.Fatalf("verdict = %+v, want refuse/rig-probe-failed with unusable rig", v)
	}
	if !strings.Contains(stderr.String(), "worktreeguard: refuse repo="+clone) {
		t.Fatalf("stderr = %q, want refuse line", stderr.String())
	}
}

func TestGitProbeGuardAlreadySecuredWhenRigHasHead(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	// Push the branch to the rig repo (plain fetch into a real branch): the
	// head is then reachable outside the doomed directory.
	git(t, rig, "fetch", "-q", clone, "work:refs/heads/work")

	p := NewGitProbe(clone, rig)
	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-44", &stderr)
	if !v.Secured || v.Action != "already-secured" {
		t.Fatalf("verdict = %+v, want already-secured", v)
	}
	if stderr.Len() != 0 {
		t.Fatalf("secured verdict wrote stderr: %q", stderr.String())
	}
}

func TestGitProbeGuardSecuresSlashNamedBranch(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	// The standard executor naming shape: a branch containing '/'.
	git(t, clone, "checkout", "-q", "-b", "agentforge-executor/adhoc-x")
	// Drop the leftover work branch so the ONLY unpushed thing is the slash
	// branch under test (the sibling gate refuses on any unsecured sibling).
	git(t, clone, "branch", "-D", "work")
	commitFile(t, clone, "slash.txt")

	p := NewGitProbe(clone, rig)
	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-45", &stderr)
	if !v.Secured || v.Action != "secured-copy" {
		t.Fatalf("verdict = %+v, want secured-copy (stderr: %q)", v, stderr.String())
	}
	refs := git(t, rig, "for-each-ref", "--format=%(refname)", QuarantineRefPrefix)
	if !strings.Contains(refs, QuarantineRefPrefix+"sess-45/agentforge-executor-adhoc-x") {
		t.Fatalf("quarantine refs = %q, want sanitized destination %ssess-45/agentforge-executor-adhoc-x", refs, QuarantineRefPrefix)
	}
}

func TestGitProbeGuardNothingToSecureOnUnbornHead(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	// A fresh init inside the doomed tree: zero commits, unborn HEAD.
	empty := filepath.Join(clone, "inner-empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initRepo(t, empty)

	p := NewGitProbe(empty, rig)
	var stderr bytes.Buffer
	v := GuardRemoval(p, empty, "sess-46", &stderr)
	if !v.Secured || v.Action != "nothing-to-secure" {
		t.Fatalf("verdict = %+v, want nothing-to-secure on unborn head (stderr: %q)", v, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("secured verdict wrote stderr: %q", stderr.String())
	}
}

// TestGitProbeGuardRefusesOnUnsecuredSiblingBranch: HEAD's branch plus a
// SECOND branch whose only copy is inside the doomed clone — removal must
// refuse rather than secure HEAD and delete the sibling's commits with the
// directory.
func TestGitProbeGuardRefusesOnUnsecuredSiblingBranch(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "-b", "sibling")
	commitFile(t, clone, "sibling.txt")
	// Back onto the base branch: HEAD is secured in the rig, so the ONLY
	// unsecured thing is the sibling — the exact case this test isolates.
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")

	p := NewGitProbe(clone, rig)
	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-48", &stderr)
	if v.Secured || v.Action != "refuse" {
		t.Fatalf("verdict = %+v, want refuse (stderr: %q)", v, stderr.String())
	}
	if v.Reason != "non-head-refs-unsecured: sibling" {
		t.Fatalf("reason = %q, want non-head-refs-unsecured: sibling", v.Reason)
	}
	refs := git(t, rig, "for-each-ref", "--format=%(refname)", QuarantineRefPrefix)
	if strings.Contains(refs, QuarantineRefPrefix) {
		t.Fatalf("quarantine refs = %q, want none (refuse precedes the fetch)", refs)
	}
}

// TestGitProbeGuardSecuredWhenSiblingTipInRig: a sibling branch whose tip
// already exists in the rig repo does not block securing HEAD — the tip's
// presence carries the branch's whole reachable history with it. The sibling
// branches off MAIN (not the unpushed work branch), so fetching it into the
// rig does not incidentally carry the work commit along.
func TestGitProbeGuardSecuredWhenSiblingTipInRig(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "checkout", "-q", "-b", "sibling")
	commitFile(t, clone, "sibling.txt")
	git(t, rig, "fetch", "-q", clone, "sibling:refs/heads/sibling")
	git(t, clone, "checkout", "-q", "work")

	p := NewGitProbe(clone, rig)
	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-49", &stderr)
	if !v.Secured || v.Action != "secured-copy" {
		t.Fatalf("verdict = %+v, want secured-copy (stderr: %q)", v, stderr.String())
	}
	refs := git(t, rig, "for-each-ref", "--format=%(refname)", QuarantineRefPrefix)
	if !strings.Contains(refs, QuarantineRefPrefix+"sess-49/work") {
		t.Fatalf("quarantine refs = %q, want %ssess-49/work", refs, QuarantineRefPrefix)
	}
}

func TestGitProbeGuardSecuresDetachedHead(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "--detach")
	// The work branch still points at the detached commit; an unsecured
	// sibling pointing at it would (correctly) refuse. Drop it so the
	// detached commit is the only unpushed thing under test.
	git(t, clone, "branch", "-D", "work")
	sha := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))

	p := NewGitProbe(clone, rig)
	branch, headSha, err := p.HeadInfo()
	if err != nil || branch != "" || headSha != sha {
		t.Fatalf("HeadInfo = %q %q %v, want detached %q", branch, headSha, err, sha)
	}
	var stderr bytes.Buffer
	v := GuardRemoval(p, clone, "sess-47", &stderr)
	if !v.Secured || v.Action != "secured-copy" {
		t.Fatalf("verdict = %+v, want secured-copy for detached head (stderr: %q)", v, stderr.String())
	}
	refs := git(t, rig, "for-each-ref", "--format=%(refname)", QuarantineRefPrefix)
	want := QuarantineRefPrefix + "sess-47/detached-" + sha[:12]
	if !strings.Contains(refs, want) {
		t.Fatalf("quarantine refs = %q, want %s", refs, want)
	}
}

// newLinkedWorktree adds a linked worktree of rig (a .git FILE sharing the
// rig's object store) at a fresh path and returns that path. detach=true
// checks out a detached HEAD; otherwise a new branch named branch.
func newLinkedWorktree(t *testing.T, rig, branch string, detach bool) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "linked")
	if detach {
		git(t, rig, "worktree", "add", "-q", "--detach", wt)
	} else {
		git(t, rig, "worktree", "add", "-q", "-b", branch, wt)
	}
	return wt
}

func newRig(t *testing.T) string {
	t.Helper()
	rig := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initRepo(t, rig)
	commitFile(t, rig, "base.txt")
	return rig
}

// TestGitProbeGuardSecuresLinkedDetachedOrphan is the final review's
// Critical: a linked worktree SHARES the rig's object store, so its orphan
// detached commit "exists" in the rig while no ref there reaches it — the
// next gc would drop it. Existence is not reachability: the guard must
// quarantine it.
func TestGitProbeGuardSecuresLinkedDetachedOrphan(t *testing.T) {
	rig := newRig(t)
	wt := newLinkedWorktree(t, rig, "", true)
	commitFile(t, wt, "orphan.txt")
	sha := strings.TrimSpace(git(t, wt, "rev-parse", "HEAD"))

	p := NewGitProbe(wt, rig)
	reach, err := p.SecuredInRig(sha)
	if err != nil || reach {
		t.Fatalf("SecuredInRig = %v %v, want false nil (object present, unreferenced)", reach, err)
	}
	var stderr bytes.Buffer
	v := GuardRemoval(p, wt, "ga-lnk01", &stderr)
	if !v.Secured || v.Action != ActionSecuredCopy {
		t.Fatalf("verdict = %+v, want secured-copy (stderr: %q)", v, stderr.String())
	}
	ref := QuarantineRef("ga-lnk01", "", sha)
	if got := strings.TrimSpace(git(t, rig, "rev-parse", ref)); got != sha {
		t.Fatalf("%s = %q, want %q", ref, got, sha)
	}
}

// TestGitProbeGuardLinkedBranchAlreadySecured: a linked worktree's branch
// lives in the rig's refs/heads and survives the directory's removal, so its
// head is secured without a copy — and the rig's other branches are never
// "siblings" of the doomed directory.
func TestGitProbeGuardLinkedBranchAlreadySecured(t *testing.T) {
	rig := newRig(t)
	git(t, rig, "branch", "unrelated-unpushed")
	wt := newLinkedWorktree(t, rig, "polecat/ga-lnk02", false)
	commitFile(t, wt, "work.txt")

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(wt, rig), wt, "ga-lnk02", &stderr)
	if !v.Secured || v.Action != ActionAlreadySecured {
		t.Fatalf("verdict = %+v, want already-secured (stderr: %q)", v, stderr.String())
	}
}

// TestGitProbeGuardSecuresUnreferencedObjectInRig: the standalone shape of
// the same Critical — the head object reached the rig once (a fetch whose
// ref was later deleted) but nothing references it any more.
func TestGitProbeGuardSecuresUnreferencedObjectInRig(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, rig, "fetch", "-q", clone, "work:refs/heads/tmp")
	git(t, rig, "update-ref", "-d", "refs/heads/tmp")
	sha := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-50", &stderr)
	if !v.Secured || v.Action != ActionSecuredCopy {
		t.Fatalf("verdict = %+v, want secured-copy for unreferenced object (stderr: %q)", v, stderr.String())
	}
	ref := QuarantineRef("sess-50", "work", sha)
	if got := strings.TrimSpace(git(t, rig, "rev-parse", ref)); got != sha {
		t.Fatalf("%s = %q, want %q", ref, got, sha)
	}
}

// TestGitProbeGuardNeverOverwritesEarlierCopy: two removals under the same
// tag and branch name with DIVERGENT heads must leave two quarantine refs —
// a forced overwrite would orphan the first rescued commit.
func TestGitProbeGuardNeverOverwritesEarlierCopy(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	first := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))
	var stderr bytes.Buffer
	if v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-51", &stderr); !v.Secured {
		t.Fatalf("first verdict = %+v (stderr: %q)", v, stderr.String())
	}
	git(t, clone, "reset", "-q", "--hard", "main")
	commitFile(t, clone, "divergent.txt")
	second := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))
	if v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-51", &stderr); !v.Secured {
		t.Fatalf("second verdict = %+v (stderr: %q)", v, stderr.String())
	}
	for _, sha := range []string{first, second} {
		ref := QuarantineRef("sess-51", "work", sha)
		if got := strings.TrimSpace(git(t, rig, "rev-parse", ref)); got != sha {
			t.Fatalf("%s = %q, want %q", ref, got, sha)
		}
	}
}

// TestGitProbeAssessWritesNoRefs: Assess (the dry-run question) reports the
// copy verdict without landing anything in the rig.
func TestGitProbeAssessWritesNoRefs(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	v := Assess(NewGitProbe(clone, rig))
	if v.Action != ActionSecuredCopy || v.Secured {
		t.Fatalf("Assess = %+v, want secured-copy (not yet secured)", v)
	}
	if refs := git(t, rig, "for-each-ref", "--format=%(refname)", QuarantineRefPrefix); refs != "" {
		t.Fatalf("quarantine refs after Assess = %q, want none", refs)
	}
}

// TestGitProbeGuardIgnoresLeakedGitDir: gc running under a hook may inherit
// GIT_DIR; every probe must ask the doomed repo itself, never the leaked one
// (local critic, REVIEW-GATE 2026-10-28 — the leak made an unpushed clone
// look already-secured).
func TestGitProbeGuardIgnoresLeakedGitDir(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	sha := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))
	t.Setenv("GIT_DIR", filepath.Join(rig, ".git"))
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-60", &stderr)
	os.Unsetenv("GIT_DIR") //nolint:errcheck // t.Setenv restores at cleanup; the helpers below need a clean env now
	if !v.Secured || v.Action != ActionSecuredCopy {
		t.Fatalf("verdict = %+v, want secured-copy despite leaked GIT_DIR (stderr %q)", v, stderr.String())
	}
	ref := QuarantineRef("sess-60", "work", sha)
	if got := strings.TrimSpace(git(t, rig, "rev-parse", ref)); got != sha {
		t.Fatalf("%s = %q, want %q", ref, got, sha)
	}
}

// TestGitProbeGuardRefusesOnLocalOnlyTag: a standalone clone whose only copy
// of a commit is a LOCAL TAG (branch reset away from it) must refuse — the
// tag dies with the directory (Sol REWORK 2026-10-28 (1)).
func TestGitProbeGuardRefusesOnLocalOnlyTag(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "tag", "rescue-me")
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-61", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: refs/tags/rescue-me" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: refs/tags/rescue-me", v)
	}
}

// TestGitProbeGuardRefusesWhenRigIsTheDoomedRepo: a misconfigured rig that
// IS the doomed clone "secures" everything through refs that die with it.
func TestGitProbeGuardRefusesWhenRigIsTheDoomedRepo(t *testing.T) {
	_, clone := newStandaloneWithUnpushedBranch(t)
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, clone), clone, "sess-62", &stderr)
	if v.Secured || v.Reason != "rig-inside-doomed-tree" {
		t.Fatalf("verdict = %+v, want refuse rig-inside-doomed-tree", v)
	}
}

// TestGitProbeGuardRefusesWhenRigBorrowsObjectsFromDoomed: a rig whose
// alternates point into the doomed clone loses those objects with it.
func TestGitProbeGuardRefusesWhenRigBorrowsObjectsFromDoomed(t *testing.T) {
	_, clone := newStandaloneWithUnpushedBranch(t)
	rig2 := filepath.Join(t.TempDir(), "shared-rig")
	git(t, filepath.Dir(rig2), "clone", "-q", "--shared", clone, rig2)
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig2), clone, "sess-63", &stderr)
	if v.Secured || v.Reason != "rig-inside-doomed-tree" {
		t.Fatalf("verdict = %+v, want refuse rig-inside-doomed-tree", v)
	}
}

// TestGitProbeGuardNeverReplacesExistingQuarantineRef: git fetch updates
// refs outside refs/heads|tags even WITHOUT "+", so the copy must be a
// create-only ref update: an existing quarantine ref holding other content
// (a sha12 collision, an earlier rescue) is never replaced (web_sol round 2).
func TestGitProbeGuardNeverReplacesExistingQuarantineRef(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	sha := git(t, clone, "rev-parse", "HEAD")
	earlier := git(t, rig, "rev-parse", "HEAD")
	ref := QuarantineRef("sess-64", "work", sha)
	git(t, rig, "update-ref", ref, earlier)

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-64", &stderr)
	if v.Secured {
		t.Fatalf("verdict = %+v, want refuse when the quarantine name is taken by other content", v)
	}
	if got := git(t, rig, "rev-parse", ref); got != earlier {
		t.Fatalf("%s = %q, want the earlier rescue %q preserved", ref, got, earlier)
	}
}

// TestGitProbeFetchIntoQuarantineIsIdempotent: a quarantine ref that
// already holds exactly this commit is a completed copy, not a clash.
func TestGitProbeFetchIntoQuarantineIsIdempotent(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	sha := git(t, clone, "rev-parse", "HEAD")
	p := NewGitProbe(clone, rig)
	for i := 0; i < 2; i++ {
		if err := p.FetchIntoQuarantine("sess-65", "work", sha); err != nil {
			t.Fatalf("FetchIntoQuarantine #%d: %v", i+1, err)
		}
	}
	if got := git(t, rig, "rev-parse", QuarantineRef("sess-65", "work", sha)); got != sha {
		t.Fatalf("quarantine ref = %q, want %q", got, sha)
	}
}

// TestGitProbeGuardRefusesOnRemoteTrackingOnlyCommit: a stale
// remote-tracking ref (the remote branch was deleted or force-pushed) can be
// the last reference to a commit (web_sol round 2).
func TestGitProbeGuardRefusesOnRemoteTrackingOnlyCommit(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	work := git(t, clone, "rev-parse", "work")
	git(t, clone, "update-ref", "refs/remotes/origin/gone", work)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-66", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: refs/remotes/origin/gone" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: refs/remotes/origin/gone", v)
	}
}

// TestGitProbeGuardRefusesStandaloneWithOwnLinkedWorktree: a standalone
// clone hosting its OWN linked worktrees keeps their HEADs and per-worktree
// refs in <clone>/.git/worktrees/ — invisible to for-each-ref and deleted
// with the clone (local_critic round 2).
func TestGitProbeGuardRefusesStandaloneWithOwnLinkedWorktree(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")
	inner := filepath.Join(t.TempDir(), "inner-wt")
	git(t, clone, "worktree", "add", "-q", "--detach", inner)
	commitFile(t, inner, "inner.txt")
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-67", &stderr)
	if v.Secured || !strings.Contains(v.Reason, "worktree:inner-wt") {
		t.Fatalf("verdict = %+v, want refuse naming worktree:inner-wt", v)
	}
}

// TestGitProbeGuardRefusesStandaloneWithNestedRepo: a gitignored repository
// nested inside a standalone clone is invisible to status and to the
// clone's refs, and os.RemoveAll takes it along (local_critic round 2).
func TestGitProbeGuardRefusesStandaloneWithNestedRepo(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")
	nested := filepath.Join(clone, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".git", "info", "exclude"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	initRepo(t, nested)
	commitFile(t, nested, "only-copy.txt")
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-68", &stderr)
	if v.Secured || !strings.Contains(v.Reason, "nested-repo:vendor/lib") {
		t.Fatalf("verdict = %+v, want refuse naming nested-repo:vendor/lib", v)
	}
}

// TestGitProbeGuardRefusesOnLinkedWorktreePrivateRefs: a linked worktree's
// per-worktree refs (refs/worktree/*, refs/bisect/*, refs/rewritten/*) live
// in its private admin dir and die with `git worktree remove` — a commit
// only they reach must refuse even though HEAD's branch survives in the rig
// (Web Sol REWORK 2026-10-28 round 4 (1)).
func TestGitProbeGuardRefusesOnLinkedWorktreePrivateRefs(t *testing.T) {
	rig := newRig(t)
	wt := newLinkedWorktree(t, rig, "polecat/ga-lnk03", false)
	commitFile(t, wt, "orphan.txt")
	orphan := strings.TrimSpace(git(t, wt, "rev-parse", "HEAD"))
	git(t, wt, "reset", "-q", "--hard", "HEAD~1")
	git(t, wt, "update-ref", "refs/worktree/keep", orphan)
	git(t, wt, "update-ref", "refs/bisect/bad", orphan)

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(wt, rig), wt, "ga-lnk03", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: refs/bisect/bad, refs/worktree/keep" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: refs/bisect/bad, refs/worktree/keep", v)
	}
}

// TestGitProbeGuardLinkedPrivateRefToSecuredCommit: a per-worktree ref at a
// commit the rig's own refs reach is no loss — no false refusal.
func TestGitProbeGuardLinkedPrivateRefToSecuredCommit(t *testing.T) {
	rig := newRig(t)
	wt := newLinkedWorktree(t, rig, "polecat/ga-lnk04", false)
	git(t, wt, "update-ref", "refs/worktree/mark", "refs/heads/main")
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(wt, rig), wt, "ga-lnk04", &stderr)
	if !v.Secured || v.Action != ActionAlreadySecured {
		t.Fatalf("verdict = %+v, want already-secured (stderr: %q)", v, stderr.String())
	}
}

// TestGitProbeGuardRefusesOnAnnotatedTagObjectUnreferencedInRig: an
// annotated tag's OBJECT (message, tagger, signature) present in the rig but
// unreferenced there is not secured just because its target commit is —
// the clone's tag ref is its only surviving reference (Web Sol REWORK
// 2026-10-28 round 4 (3)).
func TestGitProbeGuardRefusesOnAnnotatedTagObjectUnreferencedInRig(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "tag", "-a", "t1", "-m", "release notes", "main")
	git(t, rig, "fetch", "-q", "--no-tags", clone, "refs/tags/t1:refs/tags/tmp")
	git(t, rig, "update-ref", "-d", "refs/tags/tmp")
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-70", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: refs/tags/t1" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: refs/tags/t1", v)
	}
}

// TestGitProbeGuardAnnotatedTagHeldByRigIsSecured: the same tag object
// referenced by the rig's own refs/tags survives — no false refusal.
func TestGitProbeGuardAnnotatedTagHeldByRigIsSecured(t *testing.T) {
	base := t.TempDir()
	rig := filepath.Join(base, "rig")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initRepo(t, rig)
	commitFile(t, rig, "base.txt")
	git(t, rig, "tag", "-a", "v1", "-m", "release v1")
	clone := filepath.Join(base, "doomed")
	git(t, base, "clone", "-q", rig, clone)

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-71", &stderr)
	if !v.Secured || v.Action != ActionAlreadySecured {
		t.Fatalf("verdict = %+v, want already-secured (stderr: %q)", v, stderr.String())
	}
}

// TestGitProbeGuardTrailingSlashPathIsNotNested: a doomed path given with a
// trailing slash must not report the clone's own .git as a nested repo
// (local critic REVIEW-GATE 2026-10-28 round 3 observation).
func TestGitProbeGuardTrailingSlashPathIsNotNested(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")
	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone+"/", rig), clone, "sess-72", &stderr)
	if !v.Secured || v.Action != ActionAlreadySecured {
		t.Fatalf("verdict = %+v, want already-secured (stderr: %q)", v, stderr.String())
	}
}

// TestGitProbeGuardRefusesLinkedWorktreeWithNestedRepo: a linked worktree's
// tree holds a gitignored nested repository with a commit only it has —
// `git worktree remove` deletes it with the tree, so the guard must refuse
// exactly as it does for a standalone clone (local critic REVIEW-GATE
// 2026-10-28 round 4 B1).
func TestGitProbeGuardRefusesLinkedWorktreeWithNestedRepo(t *testing.T) {
	rig := newRig(t)
	wt := newLinkedWorktree(t, rig, "polecat/ga-lnk05", false)
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	git(t, wt, "add", ".gitignore")
	git(t, wt, "commit", "-q", "-m", "ignore vendor")
	nested := filepath.Join(wt, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initRepo(t, nested)
	commitFile(t, nested, "lib.txt")

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(wt, rig), wt, "ga-lnk05", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: nested-repo:vendor/lib" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: nested-repo:vendor/lib", v)
	}
}

// TestGitProbeGuardRefusesStandaloneWithSubmoduleStore: a deinitialized
// submodule leaves its repository (and an unpushed local branch) under the
// clone's .git/modules/ while its worktree marker is gone and the clone's
// status is clean — os.RemoveAll would delete the only copy (Web Sol
// REVIEW-GATE 2026-10-28 round 5).
func TestGitProbeGuardRefusesStandaloneWithSubmoduleStore(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "branch", "-D", "work")
	subSrc := filepath.Join(t.TempDir(), "subsrc")
	if err := os.MkdirAll(subSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initRepo(t, subSrc)
	commitFile(t, subSrc, "s.txt")
	git(t, clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	git(t, clone, "commit", "-q", "-m", "add sub")
	sub := filepath.Join(clone, "sub")
	git(t, sub, "config", "user.email", "test@example.com")
	git(t, sub, "config", "user.name", "Test")
	git(t, sub, "checkout", "-q", "-b", "local-only")
	commitFile(t, sub, "local.txt")
	git(t, clone, "submodule", "deinit", "-q", "-f", "sub")
	git(t, rig, "fetch", "-q", clone, "main:refs/heads/carry")

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-73", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: submodule:sub" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: submodule:sub", v)
	}
}

// TestGitProbeGuardRefusesLinkedWorktreeWithSubmoduleStore: a linked
// worktree keeps its submodule repositories in its OWN admin dir
// (<rig>/.git/worktrees/<name>/modules/), which a forced `git worktree
// remove` deletes — after `deinit` nothing in the tree marks them (local
// critic REVIEW-GATE 2026-10-28 round 5 B2).
func TestGitProbeGuardRefusesLinkedWorktreeWithSubmoduleStore(t *testing.T) {
	rig := newRig(t)
	wt := newLinkedWorktree(t, rig, "polecat/ga-lnk06", false)
	subSrc := filepath.Join(t.TempDir(), "subsrc")
	if err := os.MkdirAll(subSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initRepo(t, subSrc)
	commitFile(t, subSrc, "s.txt")
	git(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	git(t, wt, "commit", "-q", "-m", "add sub")
	sub := filepath.Join(wt, "sub")
	git(t, sub, "config", "user.email", "test@example.com")
	git(t, sub, "config", "user.name", "Test")
	git(t, sub, "checkout", "-q", "-b", "local-only")
	commitFile(t, sub, "local.txt")
	git(t, wt, "submodule", "deinit", "-q", "-f", "sub")

	var stderr bytes.Buffer
	v := GuardRemoval(NewGitProbe(wt, rig), wt, "ga-lnk06", &stderr)
	if v.Secured || v.Reason != "non-head-refs-unsecured: submodule:sub" {
		t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: submodule:sub", v)
	}
}

// TestGitProbeGuardRefusesOnNestedGitDirWithoutMarker: repositories with no
// .git marker — a bare repo, or the git dir of a --separate-git-dir repo —
// inside an ignored directory hold commits only they reference; os.RemoveAll
// would delete them (Web Sol REVIEW-GATE 2026-10-28 round 6). Detection is
// git's own shape test (HEAD + objects/ + refs/), not the marker name.
func TestGitProbeGuardRefusesOnNestedGitDirWithoutMarker(t *testing.T) {
	for _, tc := range []struct{ name, rel string }{
		{"bare", "vendor/cache.git"},
		{"separate-git-dir", "vendor/sep.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, clone := newStandaloneWithUnpushedBranch(t)
			git(t, clone, "checkout", "-q", "main")
			git(t, clone, "branch", "-D", "work")
			if err := os.WriteFile(filepath.Join(clone, ".git", "info", "exclude"), []byte("vendor/\n"), 0o644); err != nil {
				t.Fatalf("write exclude: %v", err)
			}
			src := filepath.Join(t.TempDir(), "src")
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			target := filepath.Join(clone, filepath.FromSlash(tc.rel))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if tc.name == "bare" {
				initRepo(t, src)
				commitFile(t, src, "unique.txt")
				git(t, clone, "clone", "-q", "--bare", src, target)
			} else {
				git(t, src, "init", "-q", "--separate-git-dir", target)
				git(t, src, "config", "user.email", "test@example.com")
				git(t, src, "config", "user.name", "Test")
				commitFile(t, src, "unique.txt")
			}
			if out := strings.TrimSpace(git(t, clone, "status", "--porcelain")); out != "" {
				t.Fatalf("fixture: outer clone not clean: %q", out)
			}

			var stderr bytes.Buffer
			v := GuardRemoval(NewGitProbe(clone, rig), clone, "sess-74", &stderr)
			if v.Secured || v.Reason != "non-head-refs-unsecured: nested-repo:"+tc.rel {
				t.Fatalf("verdict = %+v, want refuse non-head-refs-unsecured: nested-repo:%s", v, tc.rel)
			}
		})
	}
}
