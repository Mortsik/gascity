package worktreeguard

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real-git fixtures: a rig repo, a standalone clone of it holding an
// unpushed branch commit, and the corrupted-rig variant. These exercise the
// actual GitProbe subprocess surface, not fakes.

func git(t *testing.T, dir string, args ...string) string {
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

	cmd := exec.Command("git", "clone", "-q", rig, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
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
	reach, err := p.HeadReachableInRigRepo()
	if err != nil || reach {
		t.Fatalf("HeadReachableInRigRepo = %v %v, want false nil (unpushed)", reach, err)
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
	if v.Secured || v.Action != "refuse" || v.Reason != "reachability-unknown" {
		t.Fatalf("verdict = %+v, want refuse/reachability-unknown with unusable rig", v)
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

func TestGitProbeGuardSecuresDetachedHead(t *testing.T) {
	rig, clone := newStandaloneWithUnpushedBranch(t)
	git(t, clone, "checkout", "-q", "--detach")
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
