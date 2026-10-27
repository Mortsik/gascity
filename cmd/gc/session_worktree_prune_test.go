package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worktreeguard"
)

// fakeGitProbe is a hand-rolled gitProbe stub. Each field controls one
// probe; WorktreeRemoveErr controls the destructive call, and removed
// records the (path, force) of every WorktreeRemove invocation so tests
// can assert which directory the removal targeted.
type fakeGitProbe struct {
	isRepo           bool
	currentBranch    string
	currentBranchErr error
	hasUncommitted   bool
	hasUnpushed      bool
	unpushedErr      error
	hasStashes       bool
	stashesErr       error
	worktreeRemove   func(path string, force bool) error
	removedPath      string
	removedForce     bool
	removeInvoked    bool
}

func (f *fakeGitProbe) IsRepo() bool { return f.isRepo }
func (f *fakeGitProbe) CurrentBranch() (string, error) {
	return f.currentBranch, f.currentBranchErr
}
func (f *fakeGitProbe) HasUncommittedWork() bool { return f.hasUncommitted }
func (f *fakeGitProbe) HasUnpushedCommitsResult() (bool, error) {
	return f.hasUnpushed, f.unpushedErr
}
func (f *fakeGitProbe) HasStashesResult() (bool, error) { return f.hasStashes, f.stashesErr }
func (f *fakeGitProbe) WorktreeRemove(path string, force bool) error {
	f.removeInvoked = true
	f.removedPath = path
	f.removedForce = force
	if f.worktreeRemove != nil {
		return f.worktreeRemove(path, force)
	}
	return nil
}

// assertWorktreeStaleMarker fails the test unless workerDir contains a
// .worktree-stale marker recording the given branch and reason. Shared by
// both the raw and session.Info test forms.
func assertWorktreeStaleMarker(t *testing.T, workerDir, wantBranch, wantReason string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workerDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("reading %s marker: %v", worktreeStaleFileName, err)
	}
	content := string(data)
	if !strings.Contains(content, "branch="+wantBranch) {
		t.Errorf("marker content = %q, want to contain %q", content, "branch="+wantBranch)
	}
	if !strings.Contains(content, "reason="+wantReason) {
		t.Errorf("marker content = %q, want to contain %q", content, "reason="+wantReason)
	}
}

// assertNoWorktreeStaleMarker fails the test if workerDir contains a
// .worktree-stale marker.
func assertNoWorktreeStaleMarker(t *testing.T, workerDir string) {
	t.Helper()
	if _, err := os.ReadFile(filepath.Join(workerDir, worktreeStaleFileName)); err == nil {
		t.Errorf("%s marker unexpectedly present in %s", worktreeStaleFileName, workerDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking %s marker: %v", worktreeStaleFileName, err)
	}
}

// pruneTestFixture wires a temp city directory, a writable worker_dir
// under .gc/worktrees with a .git marker, and a config with a rig that
// points at a separate "rig root" directory the prune call uses as the
// git -C target.
type pruneTestFixture struct {
	t          *testing.T
	cityPath   string
	rigRoot    string
	workerDir  string
	cfg        *config.City
	probesByWD map[string]*fakeGitProbe
}

func newPruneFixture(t *testing.T) *pruneTestFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-3")

	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rigRoot: %v", err)
	}
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("mkdir workerDir: %v", err)
	}
	// .git marker so the existence check passes; contents don't matter
	// because gitProbe is faked.
	if err := os.WriteFile(filepath.Join(workerDir, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git marker: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "demo", Path: rigRoot},
		},
	}

	fx := &pruneTestFixture{
		t:          t,
		cityPath:   cityPath,
		rigRoot:    rigRoot,
		workerDir:  workerDir,
		cfg:        cfg,
		probesByWD: make(map[string]*fakeGitProbe),
	}

	orig := newGitProbe
	t.Cleanup(func() { newGitProbe = orig })
	newGitProbe = func(workDir string) gitProbe {
		probe, ok := fx.probesByWD[workDir]
		if !ok {
			// Default: a healthy probe. The WorktreeRemove call goes
			// through this default for the rig-root invocation.
			probe = &fakeGitProbe{isRepo: true}
			fx.probesByWD[workDir] = probe
		}
		return probe
	}

	// The guard seam fakes a secured verdict so these probe-fake tests
	// exercise the prune wiring, not the guard itself — real-guard behavior
	// is covered by the realPruneFixture tests below. Tests that pin the
	// LEGACY unpushed gate turn the guard off explicitly.
	origGuard := worktreeGuard
	t.Cleanup(func() { worktreeGuard = origGuard })
	worktreeGuard = func(_ worktreeguard.Probe, _, _ string, _ io.Writer) worktreeguard.Verdict {
		return worktreeguard.Verdict{Action: worktreeguard.ActionAlreadySecured, Secured: true, Reason: "test-fake"}
	}

	return fx
}

func (fx *pruneTestFixture) setProbe(workDir string, probe *fakeGitProbe) {
	fx.probesByWD[workDir] = probe
}

func (fx *pruneTestFixture) sessionBead() beads.Bead {
	return beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"worker_dir":   fx.workerDir,
			"template":     "demo/polecat",
			"session_name": "demo/polecat-3",
		},
	}
}

func TestPruneAgentHomeWorktreeIfSafe_DisabledByConfig(t *testing.T) {
	fx := newPruneFixture(t)
	off := false
	fx.cfg.Daemon.AutoPruneWorkerDir = &off

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true while disabled")
	}
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called while config disabled prune")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NoWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	delete(session.Metadata, "worker_dir")

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with no worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_LegacyWorkDirKey(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	delete(session.Metadata, "worker_dir")
	session.Metadata["work_dir"] = fx.workerDir

	probe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, probe)
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatalf("prune returned false on legacy work_dir; stderr=%s", stderr.String())
	}
	if !rigProbe.removeInvoked || rigProbe.removedPath != fx.workerDir || !rigProbe.removedForce {
		t.Fatalf("expected WorktreeRemove(%q, true) on rig root; got invoked=%v path=%q force=%v",
			fx.workerDir, rigProbe.removeInvoked, rigProbe.removedPath, rigProbe.removedForce)
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_OutsideWorktreesTree(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	outside := filepath.Join(fx.cityPath, "elsewhere", "polecat-3")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	session.Metadata["worker_dir"] = outside

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true for path outside .gc/worktrees")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RejectsWorktreesRoot(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	wtRoot := filepath.Join(fx.cityPath, ".gc", "worktrees")
	if err := os.WriteFile(filepath.Join(wtRoot, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git on wtRoot: %v", err)
	}
	session.Metadata["worker_dir"] = wtRoot

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true for .gc/worktrees root itself")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RelativeWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	session.Metadata["worker_dir"] = "relative/path"

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true for relative worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_MissingDotGit(t *testing.T) {
	fx := newPruneFixture(t)
	if err := os.Remove(filepath.Join(fx.workerDir, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with missing .git pointer")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NotARepo(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: false})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true when IsRepo=false")
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_HasUncommitted(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUncommitted: true, currentBranch: "builder/ga-abc123"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with uncommitted work")
	}
	if !strings.Contains(stderr.String(), "uncommitted changes") {
		t.Errorf("expected uncommitted-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-abc123", "uncommitted-work")
}

func TestPruneAgentHomeWorktreeIfSafe_HasUnpushed(t *testing.T) {
	fx := newPruneFixture(t)
	// Pins the LEGACY gate: with the guard off, the unpushed probe drives the
	// refusal (guard on would secure-and-remove instead — see the
	// realPruneFixture tests).
	off := false
	fx.cfg.Daemon.WorktreeRemovalGuard = &off
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true, currentBranch: "builder/ga-def456"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with unpushed commits")
	}
	if !strings.Contains(stderr.String(), "unpushed commits") {
		t.Errorf("expected unpushed-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-def456", "unpushed-commits")
}

func TestPruneAgentHomeWorktreeIfSafe_UnpushedProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	// Pins the LEGACY gate (guard off): probe error fails closed.
	off := false
	fx.cfg.Daemon.WorktreeRemovalGuard = &off
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, unpushedErr: errors.New("boom")})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true after unpushed probe error")
	}
	if !strings.Contains(stderr.String(), "unpushed probe failed") {
		t.Errorf("expected unpushed-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_HasStashes(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasStashes: true, currentBranch: "builder/ga-ghi789"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with stashes")
	}
	if !strings.Contains(stderr.String(), "stashed work") {
		t.Errorf("expected stashes-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-ghi789", "stashed-work")
}

func TestPruneAgentHomeWorktreeIfSafe_StashProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, stashesErr: errors.New("boom")})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true after stash probe error")
	}
	if !strings.Contains(stderr.String(), "stash probe failed") {
		t.Errorf("expected stash-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_RigPathUnresolved(t *testing.T) {
	fx := newPruneFixture(t)
	// Pins the LEGACY shape (guard off): unresolved rig, no stale marker.
	// With the guard on, an unresolved rig refuses with branch-unsecured.
	off := false
	fx.cfg.Daemon.WorktreeRemovalGuard = &off
	fx.cfg.Rigs = nil
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true without rig path")
	}
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_RigPathEmpty(t *testing.T) {
	fx := newPruneFixture(t)
	// Pins the LEGACY shape (guard off): empty rig path, no stale marker.
	off := false
	fx.cfg.Daemon.WorktreeRemovalGuard = &off
	fx.cfg.Rigs = []config.Rig{{Name: "demo", Path: ""}}
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with empty rig path")
	}
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_RemoveFails(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})
	fx.setProbe(fx.rigRoot, &fakeGitProbe{
		isRepo:         true,
		worktreeRemove: func(_ string, _ bool) error { return errors.New("locked") },
	})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true when WorktreeRemove failed")
	}
	if !strings.Contains(stderr.String(), "pruning worker_dir") || !strings.Contains(stderr.String(), "locked") {
		t.Errorf("expected removal-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_HappyPath(t *testing.T) {
	fx := newPruneFixture(t)
	wdProbe := &fakeGitProbe{isRepo: true}
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, wdProbe)
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatalf("prune returned false on happy path; stderr=%s", stderr.String())
	}
	if wdProbe.removeInvoked {
		t.Error("WorktreeRemove invoked on worker_dir; should be invoked on rig root only")
	}
	if !rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove not invoked on rig root")
	}
	if rigProbe.removedPath != fx.workerDir {
		t.Errorf("WorktreeRemove path = %q, want %q", rigProbe.removedPath, fx.workerDir)
	}
	if !rigProbe.removedForce {
		t.Error("WorktreeRemove force flag = false, want true")
	}
	if !strings.Contains(stderr.String(), "pruned worker_dir") {
		t.Errorf("expected success log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_NilConfig(t *testing.T) {
	fx := newPruneFixture(t)
	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, nil, &stderr) {
		t.Fatal("prune returned true with nil cfg")
	}
}

// runPruneGit runs a real git command in dir, failing the test on error —
// for the real-git fixtures below (the probe-fake tests never call it).
func runPruneGit(t *testing.T, dir string, args ...string) string {
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

// realPruneFixture builds a REAL rig repo plus a standalone clone of it under
// the city's .gc/worktrees tree carrying one unpushed commit — the exact
// 3zf46/ga0r1.1.1 incident shape. newGitProbe and worktreeGuard are NOT
// faked: the real git subprocess surface runs end to end.
type realPruneFixture struct {
	t         *testing.T
	cityPath  string
	rigRoot   string
	workerDir string
	cfg       *config.City
}

func newRealPruneFixture(t *testing.T) *realPruneFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-7")

	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rigRoot: %v", err)
	}
	runPruneGit(t, rigRoot, "init", "-q", "--initial-branch=main")
	runPruneGit(t, rigRoot, "config", "user.email", "test@example.com")
	runPruneGit(t, rigRoot, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(rigRoot, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base.txt: %v", err)
	}
	runPruneGit(t, rigRoot, "add", "base.txt")
	runPruneGit(t, rigRoot, "commit", "-q", "-m", "base")

	if err := os.MkdirAll(filepath.Dir(workerDir), 0o755); err != nil {
		t.Fatalf("mkdir worker parent: %v", err)
	}
	cmd := exec.Command("git", "clone", "-q", rigRoot, workerDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runPruneGit(t, workerDir, "config", "user.email", "test@example.com")
	runPruneGit(t, workerDir, "config", "user.name", "Test")
	runPruneGit(t, workerDir, "checkout", "-q", "-b", "work")
	if err := os.WriteFile(filepath.Join(workerDir, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write work.txt: %v", err)
	}
	runPruneGit(t, workerDir, "add", "work.txt")
	runPruneGit(t, workerDir, "commit", "-q", "-m", "unpushed work")

	return &realPruneFixture{
		t:         t,
		cityPath:  cityPath,
		rigRoot:   rigRoot,
		workerDir: workerDir,
		cfg:       &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigRoot}}},
	}
}

func (fx *realPruneFixture) sessionBead() beads.Bead {
	return beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"worker_dir":   fx.workerDir,
			"template":     "demo/polecat",
			"session_name": "demo/polecat-7",
		},
	}
}

// Guard ON (default): an unpushed standalone clone is SECURED — copied into
// the rig repo under refs/gc-quarantine/ — and then removed. The guard's
// copy subsumes the old unpushed refusal, so no stale marker may survive.
func TestPruneAgentHomeWorktreeIfSafe_GuardSecuresStandaloneClone(t *testing.T) {
	fx := newRealPruneFixture(t)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatalf("prune returned false on secured standalone clone; stderr=%s", stderr.String())
	}
	if _, err := os.Stat(fx.workerDir); !os.IsNotExist(err) {
		t.Fatalf("worker dir still present after secured prune: %v", err)
	}
	refs := runPruneGit(t, fx.rigRoot, "for-each-ref", "--format=%(refname)", "refs/gc-quarantine/")
	if !strings.Contains(refs, "refs/gc-quarantine/session-1/work") {
		t.Fatalf("quarantine refs = %q, want refs/gc-quarantine/session-1/work", refs)
	}
}

// Guard ON, rig repo unusable: the guard cannot secure a copy, so removal is
// REFUSED, the dir stays intact, and the stale marker names the unsecured
// branch.
func TestPruneAgentHomeWorktreeIfSafe_GuardRefuseMarksBranchUnsecured(t *testing.T) {
	fx := newRealPruneFixture(t)
	notRig := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(notRig, 0o755); err != nil {
		t.Fatalf("mkdir notRig: %v", err)
	}
	fx.cfg.Rigs[0].Path = notRig

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with unusable rig")
	}
	if _, err := os.Stat(fx.workerDir); err != nil {
		t.Fatalf("worker dir must stay intact after refusal: %v", err)
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "work", "branch-unsecured")
}

// Kill-switch parity: guard OFF must reproduce today's unpushed-refusal
// outcome verbatim (false, dir intact, marker reason=unpushed-commits).
func TestPruneAgentHomeWorktreeIfSafe_GuardOffLegacyUnpushedRefusal(t *testing.T) {
	fx := newRealPruneFixture(t)
	off := false
	fx.cfg.Daemon.WorktreeRemovalGuard = &off

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with guard off and unpushed commits")
	}
	if _, err := os.Stat(fx.workerDir); err != nil {
		t.Fatalf("worker dir must stay intact (legacy refusal): %v", err)
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "work", "unpushed-commits")
}

// Guard ON + unresolved rig: the guard cannot run at all, so the refusal
// carries the branch-unsecured marker (fail-closed) — unlike the legacy
// rig-unresolved path, which leaves no marker.
func TestPruneAgentHomeWorktreeIfSafe_GuardOnRigUnresolved(t *testing.T) {
	fx := newPruneFixture(t)
	fx.cfg.Rigs = nil
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, currentBranch: "work"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with guard on and unresolved rig")
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "work", "branch-unsecured")
}

func TestLookupRigRootForSession(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "demo", Path: "/x/demo"},
			{Name: "other", Path: "/x/other"},
		},
	}
	cases := []struct {
		name     string
		template string
		want     string
	}{
		{"qualified match", "demo/polecat", "/x/demo"},
		{"other rig", "other/refinery", "/x/other"},
		{"unqualified", "polecat", ""},
		{"unknown rig", "missing/polecat", ""},
		{"empty", "", ""},
		{"leading slash", "/polecat", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bead := beads.Bead{Metadata: map[string]string{"template": c.template}}
			got := lookupRigRootForSession(bead, cfg)
			if got != c.want {
				t.Errorf("lookupRigRootForSession(%q) = %q, want %q", c.template, got, c.want)
			}
		})
	}
}
