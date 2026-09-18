# Supervisor Idle / On-Demand Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the AgentForge GasCity supervisor consume effectively zero CPU/RAM when no work or sessions exist, while preserving fail-closed liveness and automatic wake on real demand.

**Architecture:** First deploy the current fork's existing Go-runtime idle CPU fix, then add a narrow idle predicate and intentional-idle shutdown path in GasCity. AgentForge deployment/watchdog logic will recognize intentional idle as healthy and only start the supervisor when actual demand exists. Unknown or partial state always keeps the daemon alive.

**Tech Stack:** Go 1.26.6, systemd user services, GasCity supervisor/runtime, AgentForge shell/TypeScript orchestration as discovered in-repo.

**Spec:** `docs/superpowers/specs/2026-09-19-supervisor-idle-on-demand-design.md`

## Global Constraints

- Never stop or suspend a live agent merely to reach idle.
- Any unknown/partial demand or liveness observation means not idle.
- Idle shutdown is disabled by default in generic GasCity; AgentForge opts in explicitly.
- `CPUQuota` is not the fix.
- Preserve the existing GasCity remotes model and `agentforge/fork` production branch.
- Do not start AgentForge project workloads during verification.

---

### Task 1: Establish clean isolated worktree and baseline

**Files:**
- No production files modified.

**Interfaces:**
- Consumes: current `agentforge/fork` checkout.
- Produces: isolated branch/worktree with reproducible baseline.

- [ ] **Step 1: Detect current worktree state**

Run:
```bash
git rev-parse --git-dir
git rev-parse --git-common-dir
git branch --show-current
git status --short
```

- [ ] **Step 2: Create isolated worktree if current checkout is not already isolated**

Use project-local `.worktrees/` after verifying it is ignored, and create branch `fix/supervisor-idle-on-demand` from `agentforge/fork`.

- [ ] **Step 3: Run focused existing runtime test on the host**

Run under a transient user-systemd unit to avoid CatDesk sandbox resource limits:
```bash
/usr/local/go/bin/go test -p=1 ./cmd/gc -run '^TestConfigureSupervisorRuntime$' -count=1
```
Expected: PASS.

### Task 2: Add failing idle-predicate tests

**Files:**
- Create: `cmd/gc/supervisor_idle_test.go`
- Modify later: `cmd/gc/supervisor_idle.go`

**Interfaces:**
- Produces: `supervisorIdleObservation` and `supervisorShouldIdleStop(policy, observation) bool` contract.

- [ ] **Step 1: Write tests for the desired predicate**

Tests must cover:
```go
func TestSupervisorShouldIdleStop(t *testing.T) {
    // policy disabled => false
    // fully idle + complete observation + grace elapsed => true
    // running sessions => false
    // ready/routed/assigned work => false
    // lifecycle/reload/convergence in flight => false
    // partial or unknown observation => false
}
```

- [ ] **Step 2: Run focused test and verify RED**

Run:
```bash
go test ./cmd/gc -run '^TestSupervisorShouldIdleStop$' -count=1
```
Expected: FAIL because predicate/types do not exist.

- [ ] **Step 3: Implement minimal predicate**

Create `cmd/gc/supervisor_idle.go` with pure decision types/functions only. No shutdown wiring yet.

- [ ] **Step 4: Re-run focused test and verify GREEN**

### Task 3: Wire intentional idle shutdown into supervisor lifecycle

**Files:**
- Modify: `cmd/gc/cmd_supervisor.go`
- Modify: `internal/supervisor/config.go` or the actual supervisor config file discovered in repo.
- Test: focused supervisor lifecycle tests adjacent to existing service-manager tests.

**Interfaces:**
- Consumes: pure idle predicate from Task 2.
- Produces: opt-in idle grace configuration and a distinct intentional-idle exit path.

- [ ] **Step 1: Write failing lifecycle test**

Test that an idle-enabled supervisor with complete idle observation requests a graceful intentional shutdown only after grace and that the resulting exit classification is not treated as a crash/restart condition.

- [ ] **Step 2: Verify RED**

- [ ] **Step 3: Add config/env knob**

Use an opt-in duration, with zero/unset meaning disabled. Prefer an environment override for AgentForge deployment if that matches existing supervisor configuration patterns.

- [ ] **Step 4: Wire observation and shutdown**

Reuse authoritative runtime/session/work state already present in the supervisor/city runtime. Do not add a second expensive full scan. Unknown/partial inputs keep the supervisor alive.

- [ ] **Step 5: Verify GREEN and run adjacent supervisor tests**

### Task 4: Make systemd distinguish intentional idle from failure

**Files:**
- Modify: GasCity generated systemd unit code/tests where `Restart=` and `RestartPreventExitStatus=` are defined.

**Interfaces:**
- Consumes: intentional idle exit code from Task 3.
- Produces: systemd does not immediately restart an intentional idle exit, while genuine failures still restart.

- [ ] **Step 1: Write failing unit-generation test**

Assert the generated Linux user unit includes the intentional idle exit code in `RestartPreventExitStatus=` alongside existing non-restart statuses.

- [ ] **Step 2: Verify RED**

- [ ] **Step 3: Implement minimal unit-generation change**

- [ ] **Step 4: Verify GREEN**

### Task 5: AgentForge watchdog / wake integration

**Files:**
- Discover exact AgentForge continuity/watchdog files before editing.
- Add/modify focused tests beside those files.

**Interfaces:**
- Consumes: idle-stopped GasCity service and its intentional-idle marker/status.
- Produces: health logic that accepts intentional idle, and demand/start path that starts the service before control-plane mutation.

- [ ] **Step 1: Locate current y30h.12 continuity implementation and tests**

Search for `gascity-supervisor.service`, `enable_city_unit`, `supervisorAlive`, and systemd start/enable logic.

- [ ] **Step 2: Write failing test for idle-stopped health**

When no demand exists and supervisor stopped intentionally, watchdog must not issue a start-capable `systemctl` action.

- [ ] **Step 3: Verify RED**

- [ ] **Step 4: Write failing test for demand wake**

When real demand/start is requested, service start must occur before the control-plane action.

- [ ] **Step 5: Implement minimal AgentForge changes and verify GREEN**

### Task 6: Deploy current fork and verify host behavior

**Files:**
- No source changes beyond prior tasks.

**Interfaces:**
- Produces: current `gc` installed and systemd unit regenerated/reloaded with AgentForge idle policy enabled.

- [ ] **Step 1: Run focused and package tests**

At minimum:
```bash
go test ./cmd/gc -run 'Supervisor|Idle|Systemd' -count=1
go test ./internal/supervisor/... -count=1
```
Run AgentForge focused continuity tests discovered in Task 5.

- [ ] **Step 2: Build/install `gc` from the isolated worktree**

Use the repository Makefile install path and host-side transient systemd execution if CatDesk sandbox resource limits interfere.

- [ ] **Step 3: Regenerate/reload supervisor service safely**

Do not start project agents. Enable AgentForge idle policy explicitly.

- [ ] **Step 4: Measure idle**

Confirm zero running agents. Start supervisor only as needed for the test; after grace confirm:
```bash
systemctl --user is-active gascity-supervisor.service
systemctl --user show gascity-supervisor.service -p MemoryCurrent -p CPUUsageNSec -p MainPID
```
Expected: inactive after grace, no resident supervisor process.

- [ ] **Step 5: Verify wake path without launching project workload**

Exercise a safe lifecycle/control-plane start path that proves the service can be brought back, then return to idle and confirm it stops again.

### Task 7: Review, commit, and close related resource-efficiency work only if gates pass

**Files:**
- Source/tests from Tasks 2-5.
- Relevant Beads issue metadata only after verification.

**Interfaces:**
- Produces: reviewed commits and accurate issue state.

- [ ] **Step 1: Run verification-before-completion suite**

- [ ] **Step 2: Request code review / inspect diff**

- [ ] **Step 3: Check recent commit style**

Run:
```bash
git log --oneline -n 5
```

- [ ] **Step 4: Commit focused changes**

Use small commits by subsystem rather than one monolithic commit.

- [ ] **Step 5: Update/close Beads only for actually verified acceptance criteria**

Do not close the 72h zero-human soak merely because idle resource behavior passes.
