# Session `--if-state` Fence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a genuinely cross-process `gc session close/suspend --if-state <state>` primitive whose stale/gone refusal performs zero runtime and durable mutation.

**Architecture:** Production AgentForge uses GasCity's FileStore (`[beads] provider = "file"`). FileStore already serializes every writer with one cross-process flock and reloads authoritative bytes before mutation. Add a narrow optional exclusive-mutation capability that holds that same lock across authoritative state read, runtime side effect, and staged/persisted state write. Unsupported backends fail closed; no caller-side read/check/write fallback is allowed.

**Tech Stack:** Go, Cobra CLI, GasCity `internal/beads`, `internal/session`, FileStore flock, existing worker/session handle seams.

**Spec:** AgentForge `docs/af-convergence/fenced-mutation-primitives-drafts.md` / bead `agent-forge-y30h.3.30.1`.

## Global Constraints

- Mismatch or gone target under `--if-state` must perform zero runtime stop and zero durable mutation.
- The state test must happen under the same cross-process writer ownership as the mutation.
- `suspend` must fence both managed-controller and direct fallback paths.
- No-flag behavior must remain byte/semantics compatible.
- Unsupported backing stores fail closed rather than degrading to a caller-side check.
- Do not start `gascity-supervisor.service` or `gc-city-agentforge.service` during verification.

## Review Focus

- A competing FileStore writer advancing state while a fenced operation waits for the flock must make the fenced operation refuse after reload.
- Runtime stop must never happen on state mismatch/gone.
- Managed suspend must not bypass the same fence used by direct suspend.
- Close's auxiliary durable writes (wait cancellation / identifier retirement) must not deadlock by recursively acquiring the FileStore lock.
- A save/write error after runtime stop must retain existing failure semantics and must not be mislabeled as a state mismatch.

---

### Task 1: FileStore exclusive mutation capability

**Files:**
- Modify: `internal/beads/beads.go`
- Modify: `internal/beads/filestore.go`
- Create/Test: `internal/beads/filestore_exclusive_mutation_test.go`

**Interfaces:**
- Produces: optional `ExclusiveMutationStore` + narrow callback view with authoritative `Get`, `Update`, `SetMetadataBatch`, and `Close` methods that never reacquire FileStore locks.
- Produces: `ExclusiveMutationFor(Store)` capability discovery; only FileStore claims it initially.

- [ ] Write RED tests proving callback sees a post-flock reload, a second FileStore writer blocks until callback exits, callback error persists no staged store mutation, and unsupported MemStore does not claim capability.
- [ ] Run focused tests and confirm failure because the capability does not exist.
- [ ] Implement the minimal FileStore capability: `fmu` + cross-process locker + reload + snapshot; callback mutates the embedded MemStore through a restricted view; save once on success; restore snapshot on callback/save failure.
- [ ] Re-run focused tests to green.
- [ ] Commit the isolated capability.

### Task 2: Session-level fenced lifecycle operations

**Files:**
- Modify: `internal/session/manager.go`
- Modify/Create: session fence error/type file if that keeps manager focused.
- Test: existing/new `internal/session/*_test.go`

**Interfaces:**
- Consumes: `beads.ExclusiveMutationFor`.
- Produces: fenced close/suspend methods taking an expected `session.State` and returning typed `state-mismatch`, `state-gone`, or `state-fence-unsupported` errors.

- [ ] Write RED tests: mismatch active-vs-creating => runtime provider `Stop` count remains zero and store unchanged; match => normal suspend/close; closed/absent => gone; MemStore => unsupported before runtime effect.
- [ ] Add deterministic concurrency RED test with two FileStore handles: competing writer wins flock/advances state, stale fenced operation reloads and refuses without `Stop`.
- [ ] Implement minimal fenced lifecycle inner paths using the exclusive callback. Refactor helper writes only as necessary so they operate through the callback view and never recursively lock FileStore.
- [ ] Run session tests to green.
- [ ] Commit.

### Task 3: Cobra flags, managed/direct suspend parity, JSON refusal

**Files:**
- Modify: `cmd/gc/cmd_session.go`
- Test: `cmd/gc/cmd_session_test.go` plus focused command tests.

**Interfaces:**
- Consumes: fenced session methods.
- Produces: `gc session close --if-state <state>` and `gc session suspend --if-state <state>`.

- [ ] Write RED help/parser tests for `--if-state`; empty/unknown state is usage error.
- [ ] Write RED command tests for managed suspend, direct suspend, and close mismatch/gone JSON results. Assert no poke/runtime/store mutation on refusal.
- [ ] Thread the expected state into both suspend routes and close. Preserve existing route when flag absent.
- [ ] Emit stable machine-readable refusal codes (`state-mismatch`, `state-gone`, `state-fence-unsupported`) and current/expected state where available.
- [ ] Run focused command tests to green.
- [ ] Commit.

### Task 4: Integration verification and review

**Files:**
- Update docs only if implementation semantics differ from the spec.

- [ ] Run `gofmt` and focused `go test ./internal/beads ./internal/session ./cmd/gc`.
- [ ] Run required GasCity repository gates from `TESTING.md` / Makefile, including `make check-hooks` and the relevant test shards.
- [ ] Run `git diff --check` and inspect branch diff for scope creep.
- [ ] Request independent read-only review; fix every blocking finding and re-review.
- [ ] Merge to `agentforge/fork`, rebuild/install active `gc`, verify active `gc session close/suspend --help` exposes `--if-state`, and verify heavy AgentForge services remain disabled/inactive.
- [ ] Close `agent-forge-y30h.3.30.1` only after active-binary verification.
