// The real Probe over git subprocesses: every git invocation of this
// package lives here, so the decision core (guard.go) stays pure. All probes
// degrade to their fail-closed shapes on error — a failed probe proves
// nothing and never authorizes a removal.
package worktreeguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitProbe answers the Probe questions over a doomed repository (repoPath)
// and the rig repository that receives quarantine copies (rigRoot).
type GitProbe struct {
	repoPath string
	rigRoot  string
}

// NewGitProbe binds a probe to the removal candidate and its rig repo.
func NewGitProbe(repoPath, rigRoot string) *GitProbe {
	return &GitProbe{repoPath: repoPath, rigRoot: rigRoot}
}

// IsRepo reports whether the candidate carries a .git file or directory.
func (g *GitProbe) IsRepo() bool {
	_, err := os.Stat(filepath.Join(g.repoPath, ".git"))
	return err == nil
}

// HeadInfo returns branch ("" when detached) and HEAD sha. The branch comes
// from symbolic-ref (the full ref, never an --abbrev-ref guess that an
// equally named tag can disambiguate into "heads/x"); exit 1 there is the
// detached fact. An UNBORN head (symbolic-ref resolves, rev-parse --verify
// exits 1) is a fact, not an error — it reports (branch, "", nil) so Decide
// reaches nothing-to-secure; such a directory holds no commits to lose.
func (g *GitProbe) HeadInfo() (string, string, error) {
	branch := ""
	refOut, code, err := gitOutCode(g.repoPath, "symbolic-ref", "-q", "HEAD")
	switch {
	case err == nil:
		branch = strings.TrimPrefix(strings.TrimSpace(refOut), "refs/heads/")
	case code != 1:
		return "", "", fmt.Errorf("branch probe: %w", err)
	}
	shaOut, code, err := gitOutCode(g.repoPath, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		if code == 1 && branch != "" {
			return branch, "", nil
		}
		return "", "", fmt.Errorf("head probe: %w", err)
	}
	return branch, strings.TrimSpace(shaOut), nil
}

// HeadReachableInRigRepo reports whether the HEAD commit is SECURED in the
// rig repository — reachable from a ref that survives the removal. Object
// existence alone is not enough: a linked worktree shares the rig's object
// store, so its orphan commits "exist" there until the next gc drops them.
func (g *GitProbe) HeadReachableInRigRepo() (bool, error) {
	_, headSha, err := g.HeadInfo()
	if err != nil {
		return false, err
	}
	return securedInRig(g.rigRoot, headSha)
}

// securedInRig: sha exists in the rig repo AND some branch, tag,
// remote-tracking or quarantine ref there reaches it. Deliberately not
// --all: that would count HEADs and the doomed worktree's own HEAD among
// them. Existence is checked first because rev-list on an absent sha exits
// 128 — indistinguishable from a broken rig by exit code.
func securedInRig(rigRoot, sha string) (bool, error) {
	exists, err := objectExists(rigRoot, sha)
	if err != nil || !exists {
		return false, err
	}
	out, err := gitOut(rigRoot, "rev-list", "-n1", sha, "--not",
		"--branches", "--tags", "--remotes", "--glob="+QuarantineRefPrefix+"*")
	if err != nil {
		return false, fmt.Errorf("rig repo reachability: %w", err)
	}
	return strings.TrimSpace(out) == "", nil
}

// objectExists: plain existence, no ^{commit} peel — git exits 1 for an
// absent object but 128 when the peel of an UNKNOWN sha fails (empirically
// pinned). Exit 1 = absent (a calm false); any other failure (128: not a
// repo, missing binary) is an ERROR, never a masked "absent".
func objectExists(dir, sha string) (bool, error) {
	cmd := exec.Command("git", "-C", dir, "cat-file", "-e", sha)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("rig repo cat-file: %w", err)
	}
	return true, nil
}

// FetchIntoQuarantine copies headSha into QuarantineRef(tag, branch,
// headSha) in the rig repo — a fetch-shaped copy that never touches the rig
// repo's refs/heads. The source is the exact sha (a branch that moved since
// HeadInfo cannot substitute other content), and the refspec is NOT forced:
// the destination is content-named, so an existing ref holding other content
// is a collision that must fail, never an overwrite of an earlier rescue.
func (g *GitProbe) FetchIntoQuarantine(tag, branch, headSha string) error {
	refspec := headSha + ":" + QuarantineRef(tag, branch, headSha)
	return gitRun(g.rigRoot, "fetch", "--", g.repoPath, refspec)
}

// VerifyQuarantied asks the reachability question again after the copy: a
// fetch that reports success but secured nothing must never authorize a
// removal (for a linked worktree the object pre-exists, so existence would
// prove nothing).
func (g *GitProbe) VerifyQuarantied(headSha string) error {
	ok, err := securedInRig(g.rigRoot, headSha)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("quarantined commit %s unreachable in rig repo after fetch", sha12(headSha))
	}
	return nil
}

// UnsecuredSiblingBranches lists every branch other than the checked-out
// one whose tip is not secured in the rig repository (securedInRig: a
// secured tip carries its whole history). A linked worktree (.git FILE) has
// no siblings: its refs/heads live in the shared repository and survive the
// directory's removal. A broken rig or a failed listing is an ERROR, never
// an empty list — the caller fails closed.
func (g *GitProbe) UnsecuredSiblingBranches() ([]string, error) {
	if fi, err := os.Lstat(filepath.Join(g.repoPath, ".git")); err == nil && fi.Mode().IsRegular() {
		return nil, nil
	}
	out, err := gitOut(g.repoPath, "for-each-ref", "refs/heads/", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, fmt.Errorf("sibling listing: %w", err)
	}
	headBranch, _, headErr := g.HeadInfo()
	if headErr != nil {
		// Without the HEAD branch name nothing can be exempted — treat all
		// branches as siblings; the head-probe failure refuses anyway.
		headBranch = ""
	}
	var unsecured []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, ref, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("sibling listing: malformed line %q", line)
		}
		name := strings.TrimPrefix(ref, "refs/heads/")
		if name == headBranch {
			continue // the checked-out branch is the head question, handled above
		}
		secured, err := securedInRig(g.rigRoot, sha)
		if err != nil {
			return nil, err
		}
		if !secured {
			unsecured = append(unsecured, name)
		}
	}
	return unsecured, nil
}

// sha12 clamps a short sha instead of panicking — FetchIntoQuarantine is an
// exported interface member and must not slice past a caller's short input.
func sha12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git -C %s %s: %w: %.200s", dir, strings.Join(args, " "), err, out)
	}
	return nil
}

// gitOutCode is gitOut plus the process exit code (-1 when git did not run),
// for probes where one specific exit code is a fact rather than a failure.
func gitOutCode(dir string, args ...string) (string, int, error) {
	out, err := gitOut(dir, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", exitErr.ExitCode(), err
		}
		return "", -1, err
	}
	return out, 0, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git -C %s %s: %w: %.200s", dir, strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}
