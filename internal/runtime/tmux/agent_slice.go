package tmux

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// AgentSliceEnv names the environment variable that enables canonical
// containment wrapping of every pane's initial command (agent-forge-dbl.1).
// Any non-empty value routes pane commands through the canonical containment
// entry point instead of gascity invoking systemd-run itself:
//
//	env WM_CAP_SLICE=<value> wm-cap --name agent-w1-<base> --weight 1 -- sh -c '<command>'
//
// The value names the host user slice the scopes land in, passed through to
// the entry point as WM_CAP_SLICE (the entry point's own default is the
// canonical agents.slice): a deployment that never customized it keeps the
// canonical placement, one that did keeps its own. Default-off: when unset or
// empty, pane commands run unwrapped exactly as before.
//
// Division of ownership (the invariant of this wiring): gascity owns ONLY the
// wiring — picking the entry point, naming the scope unit so host-global
// admission counts the live session, and the fail-open fallback. All
// containment semantics (slice definition, per-scope memory ceiling, OOM
// victim preference, post-mortem evidence) belong to the canonical plane: the
// dotfiles-installed wm-cap helper, the installer's agents.slice drop-in, and
// the RES-002 policy in agent-forge. No limit values are passed through here
// (no WM_CAP_HIGH/WM_CAP_MEM) — limits are policy, not wiring.
const AgentSliceEnv = "GC_AGENT_SLICE"

// ContainmentEntryPointEnv names the environment variable that overrides the
// containment entry point binary (default wm-cap). It mirrors
// CONTAINMENT_ENTRY_POINT_DEFAULT in agent-forge's containment-launch adapter:
// a deployment can install a different entry point implementing the same
// `[--name NAME] [--weight W] -- CMD ARGS…` interface without touching this
// wiring.
const ContainmentEntryPointEnv = "GC_CONTAINMENT_ENTRY_POINT"

// containmentEntryPointDefault is the canonical entry point installed by the
// dotfiles installer (wm-cap): systemd-run --user --scope into agents.slice
// plus the per-scope MemoryHigh, oom_score_adj and the post-mortem record.
const containmentEntryPointDefault = "wm-cap"

// containmentDefaultWeight is the admission weight encoded in every scope
// unit name. Weights are a host-global admission concept (RES-001); gascity
// has no per-session weight axis, so every session counts as weight 1.
const containmentDefaultWeight = 1

// wrapperCommands lists pane-root wrapper binaries produced by pane-command
// wrapping. A wrapped pane reports the wrapper as pane_current_command for
// the pane's whole lifetime, so command-wait and detection paths must treat
// these like shells: the agent is identified through descendant inspection,
// never by the pane command itself.
//
// systemd-run stays listed alongside wm-cap: scopes created by an older gc
// (pre-dbl.1 wrapping ran systemd-run inline) live as long as their panes do.
var wrapperCommands = []string{"systemd-run", "wm-cap"}

// isWrapperCommand reports whether cmd is a known pane-root wrapper binary
// (see wrapperCommands).
func isWrapperCommand(cmd string) bool {
	for _, w := range wrapperCommands {
		if cmd == w {
			return true
		}
	}
	return false
}

// sanitizeContainmentUnitBase canonicalizes a session identity into the
// containment unit charset: every character outside [A-Za-z0-9_-] becomes '-'
// (mirrors wm-cap sanitize_unit and agent-forge's sanitizeUnitName). Session
// names are already held to this charset upstream (validSessionNameRe); the
// canonicalization earns its keep on respawn targets ("%0", "city:0.0"). The
// canonical form is load-bearing: agentctl's reservation dedup matches the
// EXACT unit name, so a non-canonical base would make the session count
// twice (reservation + live scope) for the whole admission TTL.
func sanitizeContainmentUnitBase(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// containmentUnitName builds the canonical scope unit name for one agent
// session: `agent-w<weight>-<base>`. The `agent-w<N>-` segment is the weight
// encoding parsed by host-global admission (RES-001 weightFromUnitName) —
// this exact shape is what makes the live scope COUNTED by agentctl's
// accounting once the session runs inside the named scope.
func containmentUnitName(base string, weight int) string {
	return fmt.Sprintf("agent-w%d-%s", weight, sanitizeContainmentUnitBase(base))
}

// probeContainmentEntry verifies the containment entry point is resolvable in
// the gc process's PATH. Deeper availability (user manager reachable, slice
// exists) is the entry point's own fail-open concern — wm-cap detects a
// missing systemd up front and execs the wrapped command raw, so agent starts
// are never blocked by the plane being down.
func probeContainmentEntry(entryPoint string) error {
	if _, err := exec.LookPath(entryPoint); err != nil {
		return fmt.Errorf("containment entry point not found: %w", err)
	}
	return nil
}

// agentSliceWrapper decides whether pane commands are wrapped through the
// canonical containment entry point. The availability probe runs at most once
// per Tmux instance; on failure it warns once and all subsequent commands run
// unwrapped (graceful fallback, mirroring the canonical plane's own
// fail-open).
type agentSliceWrapper struct {
	probe func(entryPoint string) error // test seam; nil means probeContainmentEntry
	warn  io.Writer                     // test seam; nil means the standard logger
	once  sync.Once
	ok    bool
}

// wrap returns command routed through the containment entry point for the
// given slice and session base, or command unchanged when the knob is off,
// the command is empty, the base has no canonical form, or the entry point is
// unavailable on this host.
func (w *agentSliceWrapper) wrap(slice, base, command string) string {
	if slice == "" || command == "" || sanitizeContainmentUnitBase(base) == "" {
		return command
	}
	entryPoint := strings.TrimSpace(os.Getenv(ContainmentEntryPointEnv))
	if entryPoint == "" {
		entryPoint = containmentEntryPointDefault
	}
	w.once.Do(func() {
		probe := w.probe
		if probe == nil {
			probe = probeContainmentEntry
		}
		if err := probe(entryPoint); err != nil {
			msg := fmt.Sprintf("%s=%q set but the containment entry point %q is unavailable; pane commands run unwrapped: %v",
				AgentSliceEnv, slice, entryPoint, err)
			if w.warn != nil {
				_, _ = fmt.Fprintln(w.warn, "gc: "+msg)
			} else {
				log.Printf("tmux agent slice: %s", msg)
			}
			return
		}
		w.ok = true
	})
	if !w.ok {
		return command
	}
	// workmux/ProviderSpec pattern (agent-forge containment wiring): the
	// entry point leads, the provider command and its arguments follow the
	// `--` separator UNTOUCHED — here as `sh -c <command>`, so the wrapped
	// command stays verbatim (spec: "launch wrappers SHALL pass commands
	// through untouched").
	argv := []string{
		"env", "WM_CAP_SLICE=" + slice,
		entryPoint,
		"--name", containmentUnitName(base, containmentDefaultWeight),
		"--weight", strconv.Itoa(containmentDefaultWeight),
		"--",
		"sh", "-c", command,
	}
	return shellquote.Join(argv)
}

// wrapPaneCommand applies the canonical containment wrapper to a pane's
// initial command. target is the scope's identity base: the tmux session
// name for new-session paths, the pane target for respawn-pane (a respawn
// replaces the dead scope with a fresh one, so each spawn gets its own
// counted scope). The environment variable is read per call but the
// availability probe result is cached, so the first non-empty slice value
// decides whether wrapping is active for this Tmux.
func (t *Tmux) wrapPaneCommand(target, command string) string {
	return t.agentSlice.wrap(os.Getenv(AgentSliceEnv), target, command)
}
