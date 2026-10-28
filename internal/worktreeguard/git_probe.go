// The real Probe over git subprocesses: every git invocation of this
// package lives here, so the decision core (guard.go) stays pure. All probes
// degrade to their fail-closed shapes on error — a failed probe proves
// nothing and never authorizes a removal. Every subprocess runs with
// gitutil.SanitizedEnv(): a GIT_DIR/GIT_WORK_TREE leaked from a hook or
// nested tooling would otherwise answer every question about the WRONG
// repository and let an unpushed clone look secured.
package worktreeguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gitutil "github.com/gastownhall/gascity/internal/git"
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

// SecuredInRig reports whether sha is SECURED in the rig repository —
// reachable from a ref that survives the removal. Object existence alone is
// not enough: a linked worktree shares the rig's object store, so its orphan
// commits "exist" there until the next gc drops them.
func (g *GitProbe) SecuredInRig(sha string) (bool, error) {
	return securedInRig(g.rigRoot, sha)
}

// RigOutsideRepo reports whether the rig repository's object storage — its
// common git dir, objects dir and every (transitive) alternate — lives
// outside the doomed directory, symlinks resolved. A rig misconfigured as
// the doomed clone itself, or borrowing objects from it, "secures" commits
// in storage that dies with the directory.
func (g *GitProbe) RigOutsideRepo() (bool, error) {
	doomed, err := resolvedAbs(g.repoPath)
	if err != nil {
		return false, fmt.Errorf("doomed path: %w", err)
	}
	commonOut, err := gitOut(g.rigRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("rig common dir: %w", err)
	}
	common, err := resolvedAbs(strings.TrimSpace(commonOut))
	if err != nil {
		return false, fmt.Errorf("rig common dir: %w", err)
	}
	if within(common, doomed) {
		return false, nil
	}
	pending := []string{filepath.Join(common, "objects")}
	seen := map[string]bool{}
	for len(pending) > 0 {
		dir := pending[0]
		pending = pending[1:]
		objects, err := resolvedAbs(dir)
		if err != nil {
			return false, fmt.Errorf("rig object dir %s: %w", dir, err)
		}
		if seen[objects] {
			continue
		}
		seen[objects] = true
		if within(objects, doomed) {
			return false, nil
		}
		data, err := os.ReadFile(filepath.Join(objects, "info", "alternates"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("rig alternates: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(objects, line)
			}
			pending = append(pending, line)
		}
	}
	return true, nil
}

func resolvedAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// within reports whether path is dir or lies beneath it (both resolved).
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
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
	cmd.Env = gitutil.SanitizedEnv()
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("rig repo cat-file: %w", err)
	}
	return true, nil
}

// FetchIntoQuarantine copies headSha into QuarantineRef(tag, branch,
// headSha) in the rig repo without touching its refs/heads: the exact sha's
// objects are fetched (a branch that moved since HeadInfo cannot substitute
// other content), then the ref is CREATED atomically (update-ref "create"
// verifies it does not exist). An existing ref holding other content — a
// sha12 collision, an earlier rescue — fails the copy; it is never replaced.
func (g *GitProbe) FetchIntoQuarantine(tag, branch, headSha string) error {
	// Objects only: git fetch updates refs outside refs/heads|tags even
	// without "+", so the fetch never names a destination.
	if err := gitRun(g.rigRoot, "fetch", "--no-write-fetch-head", "--", g.repoPath, headSha); err != nil {
		return err
	}
	ref := QuarantineRef(tag, branch, headSha)
	createErr := gitRunStdin(g.rigRoot, "create "+ref+" "+headSha+"\n", "update-ref", "--stdin")
	if createErr == nil {
		return nil
	}
	// An existing ref holding exactly this commit is a completed copy.
	if cur, err := gitOut(g.rigRoot, "rev-parse", "--verify", "-q", ref); err == nil && strings.TrimSpace(cur) == headSha {
		return nil
	}
	return fmt.Errorf("quarantine ref %s not created (exists with other content?): %w", ref, createErr)
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

// UnsecuredRefs lists every ref of the doomed repository — branches, local
// tags, notes, remote-tracking refs (a stale one can be the last reference
// to a force-pushed commit), any custom namespace, the stash — other than
// HEAD's own branch, whose target is not secured in the rig repository
// (securedInRig: a secured tip carries its whole history; an annotated tag
// object must itself exist there). The clone's OWN linked worktrees
// ("worktree:<name>") are reported unsecured outright: their HEADs and
// per-worktree refs live in <gitdir>/worktrees/ and die with the clone.
//
// A linked worktree (.git FILE) has none to lose: its refs live in the
// shared repository and survive the directory's removal. That premise holds
// because callers remove .git-file trees only through the rig's `git
// worktree remove` (which refuses unregistered paths) and reaper discovery
// never adds unregistered .git files. A broken rig or a failed listing is an
// ERROR, never an empty list — the caller fails closed.
func (g *GitProbe) UnsecuredRefs(headBranch string) ([]string, error) {
	gitDir := filepath.Join(g.repoPath, ".git")
	if fi, err := os.Lstat(gitDir); err == nil && fi.Mode().IsRegular() {
		return nil, nil
	}
	var unsecured []string
	entries, err := os.ReadDir(filepath.Join(gitDir, "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("linked worktree listing: %w", err)
	}
	for _, e := range entries {
		unsecured = append(unsecured, "worktree:"+e.Name())
	}
	out, err := gitOut(g.repoPath, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, fmt.Errorf("ref listing: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, ref, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("ref listing: malformed line %q", line)
		}
		if headBranch != "" && ref == "refs/heads/"+headBranch {
			continue // HEAD's branch is the head question
		}
		secured, err := securedInRig(g.rigRoot, sha)
		if err != nil {
			return nil, err
		}
		if !secured {
			unsecured = append(unsecured, strings.TrimPrefix(ref, "refs/heads/"))
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
	cmd.Env = gitutil.SanitizedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git -C %s %s: %w: %.200s", dir, strings.Join(args, " "), err, out)
	}
	return nil
}

// gitRunStdin is gitRun with input on stdin.
func gitRunStdin(dir, stdin string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitutil.SanitizedEnv()
	cmd.Stdin = strings.NewReader(stdin)
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
	cmd.Env = gitutil.SanitizedEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git -C %s %s: %w: %.200s", dir, strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}
