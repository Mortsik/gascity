# Supervisor Idle / On-Demand Design

## Goal

Eliminate the current GasCity supervisor idle cost when AgentForge has no active agents and no work requiring reconciliation, without weakening restart/self-healing guarantees for real work.

## Current problem

The production `gascity-supervisor.service` remains enabled and active even when AgentForge has no running agents. A stale installed `gc` binary (2026-09-14) was observed consuming roughly 0.8–1.25 GiB RSS and 119–177% CPU in that state. The current fork already contains a runtime fix that disables Go heap profiling (`runtime.MemProfileRate = 0`) unless `GC_PPROF=1`, but the production binary predates that change.

## Design

1. Deploy the current `agentforge/fork` binary first so the already-landed runtime optimization is present in production.
2. Add an explicit supervisor idle policy for the AgentForge deployment. The supervisor may auto-stop only after an idle grace period when all of the following are true:
   - no provider runtime sessions are running;
   - no session bead represents active/start-pending work;
   - no ready/routed/assigned work requires a runtime to be started or resumed;
   - no explicit lifecycle request/reload is in flight;
   - no active convergence operation requires the city runtime;
   - the operator has not disabled idle shutdown.
3. Auto-stop is a graceful supervisor shutdown, preserving no agent sessions because the idle predicate already proves there are none.
4. Starting work remains on-demand. Existing user/operator entry points that need a running supervisor (`gc start`, AgentForge launcher/start path) must start/enable the service before attempting control-plane mutations.
5. Monitoring/watchdog logic must treat an idle-stopped supervisor as healthy when the idle marker/predicate says there is no demand; it must not resurrect the daemon solely because the unit is inactive.
6. Any uncertainty in demand/liveness is fail-closed: the supervisor stays running. Idle shutdown is allowed only on a complete, authoritative observation.

## Scope

### GasCity
- Add a small, testable idle-decision seam to supervisor/city runtime lifecycle code.
- Add configuration/environment knob for the idle grace period; disabled by default upstream, enabled for AgentForge deployment.
- Ensure the auto-stop path is distinct from crash/restart behavior so service-manager restart policy does not immediately resurrect an intentional idle exit.

### AgentForge deployment
- Enable the idle policy for the AgentForge supervisor deployment.
- Adjust the continuity/watchdog layer so intentional idle-stop is not treated as an outage and real demand still starts the service.

## Non-goals

- Do not change pool sizing semantics.
- Do not suspend or kill live agents to reach idle.
- Do not change project suspension state.
- Do not reduce correctness by replacing authoritative reads with guessed inactivity.
- Do not rely on `CPUQuota` as the fix.

## Safety / failure behavior

- Unknown session/provider state => not idle.
- Partial store read => not idle.
- Pending mutation/reload/convergence => not idle.
- A service-manager restart after intentional idle must not loop; the intentional idle result must be distinguishable from failure.
- Manual start remains possible at any time.

## Verification

1. Unit tests for idle predicate: true only for fully idle complete snapshots; false for running sessions, ready/routed/assigned work, partial/unknown reads, in-flight lifecycle activity, and disabled policy.
2. Lifecycle test proving intentional idle exit does not enter restart loop.
3. Existing supervisor/runtime tests remain green.
4. Host deployment test with 0 agents: after grace, supervisor inactive and CPU/RAM effectively zero.
5. Host wake test: creating/starting real work starts the supervisor and preserves normal reconciliation behavior.
