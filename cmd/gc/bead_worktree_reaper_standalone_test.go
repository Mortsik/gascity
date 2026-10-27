package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// reapGitOut runs a real git command in dir and returns stdout, failing the
// test on error.
func reapGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git -C %s %s: %v\nstderr: %s", dir, strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}

// addClosedStandaloneClone builds the 3zf46/ga0r1.1.1 incident shape: a
// STANDALONE clone (own .git DIRECTORY — invisible to `git worktree list`)
// under the rig's .gc/worktrees tree, named after a bead, carrying one
// unpushed commit, with a backdated creation signal so the freshness
// quarantine does not protect it.
func addClosedStandaloneClone(t *testing.T, rigRoot, cityPath, agentHome, beadID string) string {
	t.Helper()
	wtPath := filepath.Join(cityPath, ".gc", "worktrees", reapTestRigName, agentHome, beadID)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatalf("mkdir clone parent: %v", err)
	}
	mustGit(t, "", "clone", "-q", rigRoot, wtPath)
	mustGit(t, wtPath, "config", "user.email", "test@example.com")
	mustGit(t, wtPath, "config", "user.name", "Test")
	mustGit(t, wtPath, "checkout", "-q", "-b", "polecat/"+beadID)
	mustGit(t, wtPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unpushed ("+beadID+")")
	// Backdate the .git DIRECTORY's mtime (the creation-signal proxy) past
	// the freshness quarantine, exactly as linked-worktree tests backdate
	// their .git FILE.
	backdated := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(wtPath, ".git"), backdated, backdated); err != nil {
		t.Fatalf("backdate .git dir: %v", err)
	}
	return wtPath
}

// TestReapClosedBeadWorktrees_ReapsStandaloneCloneWithQuarantine: guard ON
// (default) — the reaper SEES the standalone clone (porcelain never listed
// it), the guard secures a quarantine copy of its unpushed branch into the
// rig repo, and the clone is reaped.
func TestReapClosedBeadWorktrees_ReapsStandaloneCloneWithQuarantine(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedStandaloneClone(t, rigRoot, cityPath, "builder", "ga-stnd01")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stnd01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-stnd01" {
		t.Fatalf("Reaped = %+v, want exactly ga-stnd01\nProtected: %+v\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("standalone clone %s still present after reap (stat err=%v)", wt, err)
	}
	refs := reapGitOut(t, rigRoot, "for-each-ref", "--format=%(refname)", "refs/gc-quarantine/")
	if !strings.Contains(refs, "refs/gc-quarantine/ga-stnd01/") {
		t.Fatalf("quarantine refs = %q, want refs/gc-quarantine/ga-stnd01/<branch>", refs)
	}
}

// TestReapClosedBeadWorktrees_SkipsOpenBeadStandaloneClone: an OPEN bead's
// standalone clone is never a candidate — closed-bead gate protects it.
func TestReapClosedBeadWorktrees_SkipsOpenBeadStandaloneClone(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedStandaloneClone(t, rigRoot, cityPath, "builder", "ga-open02")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-open02", Status: "open"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("open bead touched: Reaped=%+v Protected=%+v\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("clone for open bead %s was removed: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsStandaloneCloneWhenCopyFails: guard ON,
// rig repo unwritable — the guard cannot secure a copy of the unpushed
// branch, so the clone is PROTECTED with a reason naming branch-unsecured and
// stays on disk (this is the literal copy-failure path, not a probe error).
func TestReapClosedBeadWorktrees_ProtectsStandaloneCloneWhenCopyFails(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedStandaloneClone(t, rigRoot, cityPath, "builder", "ga-stnd03")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stnd03", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	gitDir := filepath.Join(rigRoot, ".git")
	if err := os.Chmod(gitDir, 0o555); err != nil {
		t.Fatalf("chmod .git read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when the copy cannot land\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "branch-unsecured") {
		t.Fatalf("Protected = %+v, want 1 entry with reason branch-unsecured", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("clone %s must stay intact after refused removal: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsStandaloneCloneWithUnsecuredSibling:
// the T4-review Important case — a standalone clone whose HEAD could be
// secured but which ALSO carries an unpushed sibling branch must NOT be
// reaped: the sibling's only copy dies with the directory, and no quarantine
// ref may be written (the refusal precedes the fetch).
func TestReapClosedBeadWorktrees_ProtectsStandaloneCloneWithUnsecuredSibling(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedStandaloneClone(t, rigRoot, cityPath, "builder", "ga-stnd06")
	mustGit(t, wt, "checkout", "-q", "-b", "sibling-work")
	mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "sibling-only commit")
	mustGit(t, wt, "checkout", "-q", "polecat/ga-stnd06")
	// The sibling round-trip refreshed the .git dir mtime; re-backdate past
	// the freshness quarantine so the git gates (not age) decide this case.
	backdated := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(wt, ".git"), backdated, backdated); err != nil {
		t.Fatalf("re-backdate .git dir: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stnd06", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0: the sibling branch would be lost\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "branch-unsecured") {
		t.Fatalf("Protected = %+v, want 1 entry with reason branch-unsecured", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("clone %s must stay intact: %v", wt, err)
	}
	refs := reapGitOut(t, rigRoot, "for-each-ref", "--format=%(refname)", "refs/gc-quarantine/")
	if strings.Contains(refs, "refs/gc-quarantine/") {
		t.Fatalf("quarantine refs = %q, want none (refusal precedes the fetch)", refs)
	}
}

// TestReapClosedBeadWorktrees_GuardOffStandaloneCloneStaysInvisible:
// kill-switch parity — guard OFF keeps porcelain-only discovery, so the
// standalone clone remains unseen (the legacy leak) and is never touched.
func TestReapClosedBeadWorktrees_GuardOffStandaloneCloneStaysInvisible(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedStandaloneClone(t, rigRoot, cityPath, "builder", "ga-stnd04")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stnd04", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	off := false
	cfg.Daemon.WorktreeRemovalGuard = &off
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("guard off must stay porcelain-only: Reaped=%+v Protected=%+v\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("clone %s must stay untouched with guard off: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_GuardSecuresDetachedOrphanClone is the guard-ON
// counterpart of ProtectsDetachedHEADWithOrphanCommits: a detached HEAD whose
// commit exists nowhere else is SECURED (fetched into the rig repo's
// quarantine namespace under a detached-<sha> ref) and then reaped — the copy
// makes removal lossless where the legacy gate could only refuse forever.
func TestReapClosedBeadWorktrees_GuardSecuresDetachedOrphanClone(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedStandaloneClone(t, rigRoot, cityPath, "builder", "ga-stnd05")
	mustGit(t, wt, "checkout", "-q", "--detach")
	// The clone's own branch still points at the detached commit; an
	// unsecured sibling pointing at it would (correctly) refuse. Drop it so
	// the detached commit is the only unpushed thing under test.
	mustGit(t, wt, "branch", "-D", "polecat/ga-stnd05")
	// The detached checkout touches .git (lock files refresh the dir's
	// mtime), so re-backdate the creation signal AFTER it — otherwise the
	// freshness quarantine protects the clone and nothing is reaped.
	backdated := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(wt, ".git"), backdated, backdated); err != nil {
		t.Fatalf("re-backdate .git dir: %v", err)
	}
	sha := strings.TrimSpace(reapGitOut(t, wt, "rev-parse", "HEAD"))
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stnd05", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-stnd05" {
		t.Fatalf("Reaped = %+v, want exactly ga-stnd05\nProtected: %+v\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	refs := reapGitOut(t, rigRoot, "for-each-ref", "--format=%(refname)", "refs/gc-quarantine/")
	want := "refs/gc-quarantine/ga-stnd05/detached-" + sha[:12]
	if !strings.Contains(refs, want) {
		t.Fatalf("quarantine refs = %q, want %s", refs, want)
	}
}
