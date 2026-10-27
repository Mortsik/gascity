// The real Probe over git subprocesses: every git invocation of this
// package lives here, so the decision core (guard.go) stays pure. All probes
// degrade to their fail-closed shapes on error — a failed probe proves
// nothing and never authorizes a removal.
package worktreeguard

import (
	"fmt"
	"os"
	"os/exec"
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
	_, err := os.Stat(g.repoPath + "/.git")
	return err == nil
}

// HeadInfo returns branch ("" when detached) and HEAD sha via one rev-parse
// pair. git prints the literal "HEAD" for a detached checkout — mapped to ""
// so callers never mint a ref named HEAD.
func (g *GitProbe) HeadInfo() (string, string, error) {
	branchOut, err := gitOut(g.repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
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

// HeadReachableInRigRepo is exit-0 of cat-file -e <sha>^{commit} in the rig
// repo: the object already exists outside the doomed directory.
func (g *GitProbe) HeadReachableInRigRepo() (bool, error) {
	_, headSha, err := g.HeadInfo()
	if err != nil {
		return false, err
	}
	if err := gitRun(g.rigRoot, "cat-file", "-e", headSha+"^{commit}"); err != nil {
		return false, nil
	}
	return true, nil
}

// FetchIntoQuarantine copies the branch (or the bare sha when detached)
// into QuarantineRefPrefix+tag+"/"+branch in the rig repo — a fetch-shaped
// copy that never touches the rig repo's refs/heads.
func (g *GitProbe) FetchIntoQuarantine(tag, branch, headSha string) error {
	name := sanitizeRefSegment(branch)
	refspec := fmt.Sprintf("+refs/heads/%s:%s%s/%s", branch, QuarantineRefPrefix, sanitizeRefSegment(tag), name)
	if branch == "" {
		name = "detached-" + headSha[:12]
		refspec = fmt.Sprintf("+%s:%s%s/%s", headSha, QuarantineRefPrefix, sanitizeRefSegment(tag), name)
	}
	return gitRun(g.rigRoot, "fetch", "--", g.repoPath, refspec)
}

// VerifyQuarantied confirms the copied object landed: a fetch that reports
// success but transferred nothing must never authorize a removal.
func (g *GitProbe) VerifyQuarantied(headSha string) error {
	return gitRun(g.rigRoot, "cat-file", "-e", headSha+"^{commit}")
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
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git -C %s %s: %w", dir, strings.Join(args, " "), err)
	}
	return string(out), nil
}
