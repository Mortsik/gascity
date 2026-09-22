---
title: "Tmux Agent Slice (GC_AGENT_SLICE)"
---

Setting the `GC_AGENT_SLICE` environment variable to a systemd user slice
(for example `agents.slice`, the canonical fleet slice) makes the tmux
session provider wrap every pane's initial command through the canonical
containment entry point, `wm-cap` (installed by the dotfiles installer):

```
env WM_CAP_SLICE=<slice> wm-cap --name agent-w1-<base> --weight 1 -- sh -c '<command>'
```

The value of `GC_AGENT_SLICE` is passed through to the entry point as
`WM_CAP_SLICE`, so the scopes land in the slice it names; deployments that
never customized it get the entry point's own canonical default
(`agents.slice`). Deployments that run a different entry point can override
it with `GC_CONTAINMENT_ENTRY_POINT` (any binary implementing the same
`[--name NAME] [--weight W] -- CMD ARGS…` interface).

Default-off: when the variable is unset or empty, pane commands run
unwrapped exactly as before.

## Why

systemd-enabled tmux builds (stock Ubuntu) move every pane into a transient
`tmux-spawn-*.scope` under the default user slice, so agent processes escape
whatever slice the tmux server itself runs in. Wrapping the pane command
re-parents the agent's process tree into the host containment plane.

The wrap goes through `wm-cap` rather than calling `systemd-run` inline
(agent-forge-dbl.1) so gascity duplicates no containment policy: the slice
definition, per-scope `MemoryHigh`, OOM victim preference
(`oom_score_adj`) and the post-mortem record (`~/.local/state/wm-cap/history.tsv`)
all belong to the canonical plane (the dotfiles-installed helper, the
installer's `agents.slice` drop-in, and the RES-002 policy in agent-forge).
gascity owns only the wiring: picking the entry point, naming the scope, and
the fail-open fallback.

## Scope unit naming

Each wrapped spawn creates a NAMED, COUNTED scope: `agent-w1-<base>`, where
`<base>` is the tmux session name (new-session paths) or the pane target
(respawn-pane), canonicalized to `[A-Za-z0-9_-]` (anything else becomes
`-`). The `agent-w<N>-` prefix is the weight encoding parsed by host-global
admission (`agentctl`), which counts live scopes by exact unit name — this
shape is what makes the session visible to the host's admission accounting.
A respawn creates a fresh scope (the previous one died with the pane), so
the unit base differs between initial spawn and respawns.

No limit values are passed through by gascity (`WM_CAP_HIGH`/`WM_CAP_MEM`
are deliberately absent) — per-scope limits are policy owned by the
canonical plane, not wiring.

## Scope and activation

- **Tmux provider only.** The subprocess and exec session providers spawn
  children of the gc process directly, so those already inherit gc's own
  cgroup; only tmux panes escape and need re-parenting.
- **Env var, not `city.toml`.** This is a host-level deployment knob, not
  per-city configuration: it is set by whatever supervises the gc process
  (a systemd unit, shell profile, or CI environment) and applies to every
  city served by that process. The slice it names is host systemd state
  that must exist on the user manager, outside any city's config layering.
- **Keep the value stable for the process lifetime.** The availability
  probe runs at most once per tmux provider instance, for the first
  non-empty value that instance sees; a value changed while gc is running
  is embedded in later wrapper commands without being re-probed. gc
  constructs provider instances both long-lived (the orchestrator's
  reconcile loop) and fresh per operation (template session starts), so a
  changed or repaired slice takes effect on some spawn paths and not
  others. Restart gc to converge every path on one verdict.

## Probe and fallback

Before its first wrapped spawn, each tmux provider instance probes that the
entry point binary resolves in PATH (`GC_CONTAINMENT_ENTRY_POINT`, default
`wm-cap`). If the probe fails — no such binary on this host — that
instance logs one warning and every pane command it spawns runs unwrapped:

```
tmux agent slice: GC_AGENT_SLICE="..." set but the containment entry point "wm-cap" is unavailable; pane commands run unwrapped: ...
```

Availability beyond the binary is the entry point's own fail-open concern:
`wm-cap` detects a missing systemd user manager up front and executes the
wrapped command raw, so agent starts are never blocked by the containment
plane being down.

Because operations like template session starts construct fresh provider
instances, a persistently broken host repeats this warning as new instances
probe, while long-lived instances (the orchestrator's reconcile loop) keep
their first verdict until restart.

The probe runs in the gc process's environment, while pane commands execute
with the tmux server's environment. gc normally spawns the tmux server
itself, so the two match; if you point gc at a pre-existing tmux server
whose environment lacks the entry point in PATH, wrapped spawns can fail
even after a successful probe. The failure is visible in the dead pane's
captured output in startup diagnostics.

## User-manager lifecycle coupling

Wrapped agents live under the user's `user@<uid>.service` manager. Ending
that user session — `loginctl terminate-user`, or logging out without
lingering — kills every agent scope. For unattended hosts, enable
lingering so the user manager (and the agents) survive logout:

```bash
loginctl enable-linger <user>
```

## Resource attribution

Scopes are created with the canonical `agent-w1-<base>` names, so
`systemd-cgls --user` attributes each scope to its agent session directly;
the post-mortem record in `~/.local/state/wm-cap/history.tsv` (exit code,
memory peak, OOM counter) keys on the same unit name.

## Detection

A wrapped pane reports `pane_current_command` as the wrapper (`wm-cap`, or
`systemd-run` for scopes created by an older gc) instead of the agent
process name. All gc liveness, zombie-cleanup, and pane-finding paths
handle this by walking pane process descendants, so health patrol and
nudge targeting behave the same with wrapping on or off.
