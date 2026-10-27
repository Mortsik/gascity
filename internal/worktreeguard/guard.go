// Package worktreeguard makes the no-lost-work guarantee structural for
// every removal of a directory under .gc/worktrees/: before a doomed
// directory may disappear, the reachability question is answered against
// refs OUTSIDE that directory (the rig repository's quarantine namespace),
// never against branches that live inside it. The incident class
// (agent-forge-ga0r1.1.1, 2026-10-14/15): a standalone executor clone held
// the ONLY copy of an unpushed branch and was deleted before review — a
// local-branch reachability probe reports such commits "reachable" precisely
// because their only ref dies with the directory.
//
// Semantics are a Go port of the proven agent-forge policy pair
// (decideBranchSecurity + secureWorktreeBranch): decide purely over a closed
// fact set, then execute the copy verdict over the Probe seam. Fail-closed
// everywhere: any probe error refuses the removal.
package worktreeguard

import (
	"fmt"
	"io"
)

// QuarantineRefPrefix is the rig-repo namespace every secured copy lands
// under — a fetch-shaped copy that never touches refs/heads of the rig repo.
const QuarantineRefPrefix = "refs/gc-quarantine/"

// Probe is the closed subprocess seam GuardRemoval executes over. Tests
// inject a fake; the real adapter is NewGitProbe. Every method reports raw
// results — GuardRemoval assembles facts and decides.
type Probe interface {
	// IsRepo reports whether the removal candidate is a git repository
	// (a .git file OR directory).
	IsRepo() bool
	// HeadInfo returns the checked-out branch ("" when detached) and the
	// HEAD commit sha; an error is a fail-closed fact.
	HeadInfo() (branch string, headSha string, err error)
	// HeadReachableInRigRepo reports whether the HEAD commit already exists
	// as an object in the rig repository — a copy outside the doomed
	// directory by construction.
	HeadReachableInRigRepo() (bool, error)
	// FetchIntoQuarantine copies the branch (or bare sha, branch "") into
	// QuarantineRefPrefix+tag+"/"+branch under the rig repository.
	FetchIntoQuarantine(tag, branch, headSha string) error
	// VerifyQuarantied confirms the copied object actually landed in the
	// rig repository — a fetch that reports success but transfers nothing
	// must never authorize a removal.
	VerifyQuarantied(headSha string) error
}

// Facts is the closed decision input: probe results only, with explicit
// error flags so Decide itself stays pure and total.
type Facts struct {
	IsRepo             bool
	Branch             string
	HeadSha            string
	HeadReachableInRig bool
	HeadProbeErr       bool
	ReachableProbeErr  bool
}

// Verdict is one removal decision. Secured true means a copy provably exists
// outside the doomed directory (or there is nothing to copy) and the caller
// may remove; Secured false is fail-loud — the notify line has fired and the
// removal must NOT proceed.
type Verdict struct {
	Action  string
	Secured bool
	Reason  string
}

// Verdict actions (closed vocabulary).
const (
	ActionAlreadySecured  = "already-secured"
	ActionSecuredCopy     = "secured-copy"
	ActionRefuse          = "refuse"
	ActionNothingToSecure = "nothing-to-secure"
)

// Decide maps facts to a verdict. Pure: no I/O, no clock. Fail-closed on
// every unknown — an unprovable fact never authorizes a removal.
func Decide(facts Facts) Verdict {
	switch {
	case !facts.IsRepo:
		return Verdict{Action: ActionRefuse, Secured: false, Reason: "not-a-repo"}
	case facts.HeadProbeErr:
		return Verdict{Action: ActionRefuse, Secured: false, Reason: "head-probe-failed"}
	case facts.HeadSha == "":
		return Verdict{Action: ActionNothingToSecure, Secured: true, Reason: "no-commits"}
	case facts.ReachableProbeErr:
		return Verdict{Action: ActionRefuse, Secured: false, Reason: "reachability-unknown"}
	case facts.HeadReachableInRig:
		return Verdict{Action: ActionAlreadySecured, Secured: true, Reason: "reachable-in-rig-repo"}
	default:
		// The copy is the caller's to execute: GuardRemoval runs it over the
		// Probe seam before this verdict becomes an authorization.
		return Verdict{Action: ActionSecuredCopy, Secured: false, Reason: "copy-absent-rig-repo-available"}
	}
}

// GuardRemoval decides and executes the securing of one removal candidate.
// tag names the quarantine sub-namespace (the caller's session/bead id,
// sanitized). Every non-secured verdict writes exactly one structured line
// to stderr — never silent.
func GuardRemoval(p Probe, repoPath, tag string, stderr io.Writer) Verdict {
	branch, headSha, headErr := p.HeadInfo()
	branch = sanitizeRefSegment(branch)
	reach, reachErr := p.HeadReachableInRigRepo()
	facts := Facts{
		IsRepo:             p.IsRepo(),
		Branch:             branch,
		HeadSha:            headSha,
		HeadReachableInRig: reach,
		HeadProbeErr:       headErr != nil,
		ReachableProbeErr:  reachErr != nil,
	}
	v := Decide(facts)
	if v.Action == ActionSecuredCopy {
		if err := p.FetchIntoQuarantine(tag, branch, headSha); err != nil {
			v = Verdict{
				Action: ActionSecuredCopy, Secured: false,
				Reason: fmt.Sprintf("fetch-failed: %.120s", err.Error()),
			}
		} else if err := p.VerifyQuarantied(headSha); err != nil {
			v = Verdict{
				Action: ActionSecuredCopy, Secured: false,
				Reason: fmt.Sprintf("post-fetch-verify-failed: %.120s", err.Error()),
			}
		} else {
			v = Verdict{Action: ActionSecuredCopy, Secured: true, Reason: "fetched-into-quarantine"}
		}
	}
	if !v.Secured {
		b, s := branch, headSha
		if b == "" {
			b = "-"
		}
		if s == "" {
			s = "-"
		}
		fmt.Fprintf(stderr, "worktreeguard: %s repo=%s branch=%s sha=%s reason=%s\n",
			v.Action, repoPath, b, s, v.Reason) //nolint:errcheck // notify line, best-effort
	}
	return v
}

// sanitizeRefSegment keeps a caller-supplied tag/branch inside ONE ref
// path component: path separators and whitespace collapse to "-", so a
// hostile or sloppy id can never escape the quarantine namespace.
func sanitizeRefSegment(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/' || c == '\\' || c == ' ' || c == '\t' || c == '\n' || c < 0x20 || c == 0x7f:
			out = append(out, '-')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
