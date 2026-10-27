// The real Probe over git subprocesses: every git invocation of this
// package lives here, so the decision core (guard.go) stays pure. All probes
// degrade to their fail-closed shapes on error — a failed probe proves
// nothing and never authorizes a removal.
package worktreeguard

import (
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

// HeadInfo returns branch ("" when detached) and HEAD sha. An UNBORN head
// (a repo with no commits yet: rev-parse fails, symbolic-ref still resolves)
// is a fact, not an error — it reports (branch, "", nil) so Decide reaches
// nothing-to-secure; such a directory holds no commits to lose.
func (g *GitProbe) HeadInfo() (string, string, error) {
	branchOut, err := gitOut(g.repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		// Unborn head? symbolic-ref resolves the branch name even with zero
		// commits; a repo where it also fails is genuinely unprobeable.
		if symOut, symErr := gitOut(g.repoPath, "symbolic-ref", "--short", "HEAD"); symErr == nil {
			return strings.TrimSpace(symOut), "", nil
		}
		return "", "", fmt.Errorf("branch probe: %w", err)
	}
	branch := strings.TrimSpace(branchOut)
	if branch == "HEAD" {
		branch = ""
	}
	shaOut, err := gitOut(g.repoPath, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("head probe: %w", err)
	}
	return branch, strings.TrimSpace(shaOut), nil
}

// HeadReachableInRigRepo reports whether the HEAD commit already exists as
// an object in the rig repository. Plain existence, no ^{commit} peel: git
// exits 1 for an absent object but 128 when the peel of an UNKNOWN sha fails
// (empirically pinned), which would make "absent" indistinguishable from
// "broken rig" by exit code. With the plain form: exit 1 = absent (a calm
// false); any other failure (128: not a repo, missing binary) is an ERROR —
// a broken rig surfaces as reachability-unknown, never as a masked
// "not reachable".
func (g *GitProbe) HeadReachableInRigRepo() (bool, error) {
	_, headSha, err := g.HeadInfo()
	if err != nil {
		return false, err
	}
	return objectExists(g.rigRoot, headSha)
}

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

// FetchIntoQuarantine copies the branch (or the bare sha when detached)
// into QuarantineRefPrefix+tag+"/"+name in the rig repo — a fetch-shaped
// copy that never touches the rig repo's refs/heads. The SOURCE side keeps
// the raw branch ref (it may contain slashes); only the DESTINATION name is
// sanitized into one path component.
func (g *GitProbe) FetchIntoQuarantine(tag, branch, headSha string) error {
	name := sanitizeRefSegment(branch)
	refspec := fmt.Sprintf("+refs/heads/%s:%s%s/%s", branch, QuarantineRefPrefix, sanitizeRefSegment(tag), name)
	if branch == "" {
		name = "detached-" + sha12(headSha)
		refspec = fmt.Sprintf("+%s:%s%s/%s", headSha, QuarantineRefPrefix, sanitizeRefSegment(tag), name)
	}
	return gitRun(g.rigRoot, "fetch", "--", g.repoPath, refspec)
}

// VerifyQuarantied confirms the copied object landed (plain existence —
// same exit-code semantics as the reachability probe): a fetch that reports
// success but transferred nothing must never authorize a removal.
func (g *GitProbe) VerifyQuarantied(headSha string) error {
	if ok, err := objectExists(g.rigRoot, headSha); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("quarantined object %s absent from rig repo after fetch", sha12(headSha))
	}
	return nil
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
