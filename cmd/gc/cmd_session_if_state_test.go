package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func setupSessionIfStateCity(t *testing.T, state string) (string, beads.Store, beads.Bead) {
	t.Helper()
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "fenced worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-fenced",
			"template":     "worker",
			"state":        state,
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}
	return cityDir, store, b
}

func TestSessionCloseAndSuspendAdvertiseIfState(t *testing.T) {
	for _, cmd := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "close", cmd: newSessionCloseCmd(&bytes.Buffer{}, &bytes.Buffer{})},
		{name: "suspend", cmd: newSessionSuspendCmd(&bytes.Buffer{}, &bytes.Buffer{})},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			if flag := cmd.cmd.Flag("if-state"); flag == nil {
				t.Fatal("--if-state flag not registered")
			}
		})
	}
}

func TestCmdSessionCloseIfStateMismatchIsZeroMutation(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")
	var stdout, stderr bytes.Buffer
	code := cmdSessionCloseWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"})
	if code == 0 {
		t.Fatalf("cmdSessionCloseWithOptions mismatch exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "state-mismatch:") {
		t.Fatalf("stderr = %q, want machine-readable state-mismatch", stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Status == "closed" || got.Metadata["state"] != "active" {
		t.Fatalf("session mutated on mismatch: status=%q state=%q", got.Status, got.Metadata["state"])
	}
}

func TestCmdSessionSuspendIfStateMismatchJSONIsZeroMutation(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")
	var stdout, stderr bytes.Buffer
	code := cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{JSON: true, IfState: "draining"})
	if code == 0 {
		t.Fatalf("cmdSessionSuspendWithOptions mismatch exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var out cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not JSON error: %v; stdout=%q", err, stdout.String())
	}
	if out.Error.Code != "state-mismatch" {
		t.Fatalf("JSON error code = %q, want state-mismatch", out.Error.Code)
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state mutated on mismatch: %q", got.Metadata["state"])
	}
}

func TestCmdSessionCloseIfStateMatchPreservesBehavior(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "creating")
	var stdout, stderr bytes.Buffer
	code := cmdSessionCloseWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"})
	if code != 0 {
		t.Fatalf("cmdSessionCloseWithOptions = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestCmdSessionCloseIfStateMissingReturnsStateGone(t *testing.T) {
	cityDir, _, _ := setupSessionIfStateCity(t, "active")
	_ = cityDir
	var stdout, stderr bytes.Buffer
	code := cmdSessionCloseWithOptions([]string{"gc-missing"}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"})
	if code == 0 {
		t.Fatalf("missing fenced close exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "state-gone:") {
		t.Fatalf("stderr = %q, want state-gone", stderr.String())
	}
}

func TestCmdSessionCloseIfStateClosedReturnsStateGone(t *testing.T) {
	cityDir, store, sessionBead := setupSessionIfStateCity(t, "creating")
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := cmdSessionCloseWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"})
	if code == 0 {
		t.Fatalf("closed fenced close exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "state-gone:") {
		t.Fatalf("stderr = %q, want state-gone", stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(closed session): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("closed session status changed to %q", got.Status)
	}
}

func TestCmdSessionCloseWithoutIfStateKeepsLegacyUnconditionalBehavior(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")
	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("legacy close = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("legacy close status = %q, want closed", got.Status)
	}
}

func TestCmdSessionCloseIfStateMismatchDoesNotHealRepairableType(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "repairable fenced worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-repairable",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(repairable session): %v", err)
	}
	// FileStore.Create inherits MemStore's empty-Type default to "task". Rewrite
	// it to empty after creation to model the crash/migration-damaged shape that
	// the read path recognizes as repairable.
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatalf("clear type on repairable session: %v", err)
	}
	seeded, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(seed): %v", err)
	}
	if seeded.Type != "" {
		t.Fatalf("test seed Type = %q, want empty", seeded.Type)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionCloseWithOptions([]string{b.ID}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"}); code == 0 {
		t.Fatalf("mismatch exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Type != "" {
		t.Fatalf("mismatch healed session type to %q; want zero mutation and empty type", got.Type)
	}
	if got.Metadata["state"] != "active" || got.Status == "closed" {
		t.Fatalf("session mutated on mismatch: status=%q state=%q", got.Status, got.Metadata["state"])
	}
}

func TestCmdSessionSuspendIfStateMatchDirectPathPreservesBehavior(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")
	var stdout, stderr bytes.Buffer
	code := cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{IfState: "active"})
	if code != 0 {
		t.Fatalf("cmdSessionSuspendWithOptions(match) = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "suspended") {
		t.Fatalf("stdout = %q, want suspended confirmation", stdout.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if state := got.Metadata["state"]; state != "suspended" {
		t.Fatalf("state after matching fenced suspend = %q, want suspended", state)
	}
}

func TestCmdSessionSuspendIfStateManagedPathIsFenced(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")

	// Fake managed-reconciler deps route the command through the metadata-only
	// managed path (cityUsesManagedReconciler + pokeController) hermetically,
	// without standing up the controller socket listener.
	pokes := 0
	deps := sessionSuspendDeps{
		cityUsesManagedReconciler: func(string) bool { return true },
		pokeController:            func(string) error { pokes++; return nil },
	}

	// Mismatch under the fence: refuse with zero durable mutation — no
	// held_until patch, no state flip.
	var stdout, stderr bytes.Buffer
	code := cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{JSON: true, IfState: "creating", SuspendDeps: &deps})
	if code == 0 {
		t.Fatalf("managed mismatch exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var refusal cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &refusal); err != nil {
		t.Fatalf("managed mismatch stdout is not JSON error: %v; stdout=%q", err, stdout.String())
	}
	if refusal.Error.Code != "state-mismatch" {
		t.Fatalf("managed mismatch JSON code = %q, want state-mismatch", refusal.Error.Code)
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	afterRefusal, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(after refusal): %v", err)
	}
	if state := afterRefusal.Metadata["state"]; state != "active" {
		t.Fatalf("state after managed mismatch = %q, want active (zero mutation)", state)
	}
	if held := afterRefusal.Metadata["held_until"]; held != "" {
		t.Fatalf("held_until after managed mismatch = %q, want empty (no suspend patch)", held)
	}
	if intent := afterRefusal.Metadata["sleep_intent"]; intent != "" {
		t.Fatalf("sleep_intent after managed mismatch = %q, want empty", intent)
	}
	// A refusal must be poke-free: the reconciler trigger fires only after a
	// successful fenced patch, so a mismatch cannot cause a controller tick.
	if pokes != 0 {
		t.Fatalf("managed mismatch poke count = %d, want 0 (no poke before the fence)", pokes)
	}

	// Match under the fence: the managed metadata-only suspend patch lands.
	stdout.Reset()
	stderr.Reset()
	code = cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{JSON: true, IfState: "active", SuspendDeps: &deps})
	if code != 0 {
		t.Fatalf("managed match exit = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result sessionActionResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("managed match stdout is not JSON: %v; stdout=%q", err, stdout.String())
	}
	if result.Mode != "managed" || result.State != "suspended" {
		t.Fatalf("managed match result = %+v, want mode=managed state=suspended", result)
	}
	reopened, err = openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	afterMatch, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(after match): %v", err)
	}
	if state := afterMatch.Metadata["state"]; state != "suspended" {
		t.Fatalf("state after managed match = %q, want suspended", state)
	}
	if held := afterMatch.Metadata["held_until"]; held == "" {
		t.Fatal("held_until empty after managed match, want the user-hold patch")
	}
	if intent := afterMatch.Metadata["sleep_intent"]; intent != "user-hold" {
		t.Fatalf("sleep_intent after managed match = %q, want user-hold", intent)
	}
	if pokes != 1 {
		t.Fatalf("managed match poke count = %d, want 1 (the post-success reconciler tick; no pre-fence poke)", pokes)
	}
}

// TestCmdSessionSuspendIfStateManagedPathGoneRefusesZeroPokes pins the gone
// refusal on the managed path: a terminal (closed) target still resolves
// (allow-closed resolution), the fence reports state-gone, and no controller
// poke happens on the refusal path.
func TestCmdSessionSuspendIfStateManagedPathGoneRefusesZeroPokes(t *testing.T) {
	cityDir, store, sessionBead := setupSessionIfStateCity(t, "active")
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}
	pokes := 0
	deps := sessionSuspendDeps{
		cityUsesManagedReconciler: func(string) bool { return true },
		pokeController:            func(string) error { pokes++; return nil },
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{JSON: true, IfState: "active", SuspendDeps: &deps})
	if code == 0 {
		t.Fatalf("managed gone exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var refusal cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &refusal); err != nil {
		t.Fatalf("managed gone stdout is not JSON error: %v; stdout=%q", err, stdout.String())
	}
	if refusal.Error.Code != "state-gone" {
		t.Fatalf("managed gone JSON code = %q, want state-gone", refusal.Error.Code)
	}
	if pokes != 0 {
		t.Fatalf("managed gone poke count = %d, want 0 on the refusal path", pokes)
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(closed session): %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "active" {
		t.Fatalf("gone refusal mutated the bead: status=%q state=%q", got.Status, got.Metadata["state"])
	}
	if got.Metadata["held_until"] != "" {
		t.Fatalf("held_until after managed gone = %q, want empty (no suspend patch)", got.Metadata["held_until"])
	}
}

// swapSessionProviderForTest replaces the session-provider construction seam
// with a spy [runtime.Fake] the test keeps a handle on, so provider-level
// effects of a command (e.g. the runtime Stop issued by the direct suspend
// fallback) are observable from outside the command. Restored via t.Cleanup.
func swapSessionProviderForTest(t *testing.T) *runtime.Fake {
	t.Helper()
	fake := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fake, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	return fake
}

// stopCalls returns the Stop calls the provider recorded, in order.
func stopCalls(fake *runtime.Fake) []runtime.Call {
	var stops []runtime.Call
	for _, c := range fake.Calls {
		if c.Method == "Stop" {
			stops = append(stops, c)
		}
	}
	return stops
}

// TestCmdSessionSuspendIfStateManagedControllerDownIsMetadataOnly pins the
// fenced managed suspend on a managed city whose controller is DOWN: the
// single post-success poke fails and the failure is ignored, so the command
// still succeeds — but as a metadata-only suspend. The runtime is NOT
// stopped (no provider Stop, no worker-handle fallback; mode stays
// "managed"), and held_until lands durably far in the future so the
// self-healing reconciler finishes the stop once the controller is back.
func TestCmdSessionSuspendIfStateManagedControllerDownIsMetadataOnly(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")
	fake := swapSessionProviderForTest(t)

	pokes := 0
	deps := sessionSuspendDeps{
		cityUsesManagedReconciler: func(string) bool { return true },
		pokeController: func(string) error {
			pokes++
			return errors.New("controller down")
		},
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{JSON: true, IfState: "active", SuspendDeps: &deps})
	if code != 0 {
		t.Fatalf("fenced managed suspend with controller down exit = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result sessionActionResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON result: %v; stdout=%q", err, stdout.String())
	}
	if result.Mode != "managed" || result.State != "suspended" {
		t.Fatalf("result = %+v, want mode=managed state=suspended (metadata-only success, not the direct fallback)", result)
	}
	// Exactly one poke — the post-success reconciler tick. Its failure is
	// swallowed; there is no pre-fence availability poke to fail louder.
	if pokes != 1 {
		t.Fatalf("poke count = %d, want 1 (post-success tick only; no pre-fence poke)", pokes)
	}
	// The runtime must still be running: this command never reached the
	// worker-handle fallback, so it issued no provider Stop.
	if stops := stopCalls(fake); len(stops) != 0 {
		t.Fatalf("provider Stop calls = %d (%v), want 0 (controller down ⇒ runtime keeps running)", len(stops), stops)
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	after, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(after controller-down suspend): %v", err)
	}
	if state := after.Metadata["state"]; state != "suspended" {
		t.Fatalf("state after controller-down suspend = %q, want suspended", state)
	}
	if intent := after.Metadata["sleep_intent"]; intent != "user-hold" {
		t.Fatalf("sleep_intent after controller-down suspend = %q, want user-hold", intent)
	}
	held, err := time.Parse(time.RFC3339, after.Metadata["held_until"])
	if err != nil {
		t.Fatalf("held_until %q is not RFC3339, want the durable user-hold patch: %v", after.Metadata["held_until"], err)
	}
	// The patch is the indefinite-hold sentinel (~100 years out) — durable
	// enough for self-healing to act on whenever the controller returns.
	if !held.After(time.Now().Add(99 * 365 * 24 * time.Hour)) {
		t.Fatalf("held_until = %v, want the far-future indefinite hold", held)
	}
}

// TestCmdSessionSuspendManagedPokeFailureFallsThroughToDirectStop pins the
// contrast: on the SAME managed city with the controller down, an UNFENCED
// suspend uses its pre-poke as the liveness probe — the failed poke falls
// through to the direct worker-handle suspend, which stops the runtime
// (provider Stop) and records state=suspended without the managed
// held_until/sleep_intent patch.
func TestCmdSessionSuspendManagedPokeFailureFallsThroughToDirectStop(t *testing.T) {
	cityDir, _, sessionBead := setupSessionIfStateCity(t, "active")
	fake := swapSessionProviderForTest(t)

	pokes := 0
	deps := sessionSuspendDeps{
		cityUsesManagedReconciler: func(string) bool { return true },
		pokeController: func(string) error {
			pokes++
			return errors.New("controller down")
		},
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionSuspendWithOptions([]string{sessionBead.ID}, &stdout, &stderr, sessionMutationOptions{JSON: true, SuspendDeps: &deps})
	if code != 0 {
		t.Fatalf("unfenced managed suspend with controller down exit = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result sessionActionResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON result: %v; stdout=%q", err, stdout.String())
	}
	if result.Mode != "direct" {
		t.Fatalf("result mode = %q, want direct (failed pre-poke falls through to the worker-handle suspend)", result.Mode)
	}
	// The direct fallback stops the runtime: exactly one provider Stop for
	// the session's runtime name.
	stops := stopCalls(fake)
	if len(stops) != 1 || stops[0].Name != "worker-fenced" {
		t.Fatalf("provider Stop calls = %v, want exactly one Stop for worker-fenced (runtime stopped by the direct suspend)", stops)
	}
	if pokes != 1 {
		t.Fatalf("poke count = %d, want 1 (the failed liveness pre-poke)", pokes)
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	after, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(after direct suspend): %v", err)
	}
	if state := after.Metadata["state"]; state != "suspended" {
		t.Fatalf("state after direct suspend = %q, want suspended", state)
	}
	if held := after.Metadata["held_until"]; held != "" {
		t.Fatalf("held_until after direct suspend = %q, want empty (the managed patch never ran)", held)
	}
}

func TestSessionCloseAndSuspendIfStateEmptyFlagIsUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  func(io.Writer, io.Writer) *cobra.Command
	}{
		{name: "close", cmd: newSessionCloseCmd},
		{name: "suspend", cmd: newSessionSuspendCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := tc.cmd(&stdout, &stderr)
			if err := cmd.Flags().Set("if-state", ""); err != nil {
				t.Fatalf("set empty --if-state: %v", err)
			}
			err := cmd.RunE(cmd, []string{"gc-1"})
			if err == nil {
				t.Fatal("RunE with explicit empty --if-state returned nil, want usage error")
			}
			if !strings.Contains(stderr.String(), "--if-state must not be empty") {
				t.Fatalf("stderr = %q, want empty --if-state usage error", stderr.String())
			}
		})
	}
}

func TestValidateSessionIfStateFlagPassesWhenFlagAbsent(t *testing.T) {
	var stderr bytes.Buffer
	cmd := newSessionCloseCmd(&bytes.Buffer{}, &stderr)
	if err := validateSessionIfStateFlag(cmd, &stderr, ""); err != nil {
		t.Fatalf("validateSessionIfStateFlag(absent) = %v, want nil", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no usage error for absent flag", stderr.String())
	}
}

// seedUntypedFencedSession creates a session bead whose type is empty (the
// crash/migration-damaged shape that read paths treat as repairable) in a
// file-backed city, ready for a fenced mutation attempt.
func seedUntypedFencedSession(t *testing.T, cityToml string, metadata map[string]string) (string, beads.Store, beads.Bead) {
	t.Helper()
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, cityToml)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seed := map[string]string{"template": "worker", "state": "active"}
	for k, v := range metadata {
		seed[k] = v
	}
	b, err := store.Create(beads.Bead{
		Title:    "untyped fenced worker",
		Labels:   []string{session.LabelSession},
		Metadata: seed,
	})
	if err != nil {
		t.Fatalf("Create(untyped session): %v", err)
	}
	// FileStore.Create inherits MemStore's empty-Type default to "task". Rewrite
	// it to empty after creation to model the damaged shape the read path
	// recognizes as repairable.
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatalf("clear type on untyped session: %v", err)
	}
	return cityDir, store, b
}

func assertFencedMismatchLeftBeadUntyped(t *testing.T, cityDir string, b beads.Bead, stdout, stderr *bytes.Buffer, code int) {
	t.Helper()
	if code == 0 {
		t.Fatalf("fenced mismatch exit = 0; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "state-mismatch:") {
		t.Fatalf("stderr = %q, want machine-readable state-mismatch", stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(untyped session): %v", err)
	}
	if got.Type != "" {
		t.Fatalf("type mutated before state fence: got %q, want empty", got.Type)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state mutated on mismatch: %q", got.Metadata["state"])
	}
	if got.Status == "closed" {
		t.Fatal("bead closed on fenced mismatch")
	}
}

// TestReviewFencedQualifiedAliasMismatchDoesNotRepairType pins the read-only
// contract of the qualified-alias resolution door: resolving a bare identifier
// against a qualified alias must not persist the empty-type repair before the
// --if-state fence is acquired, so a mismatch refuses with the bead still
// byte-untyped.
func TestReviewFencedQualifiedAliasMismatchDoesNotRepairType(t *testing.T) {
	cityDir, _, sessionBead := seedUntypedFencedSession(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`, map[string]string{
		"session_name": "fenced-qualified",
		"alias":        "test-city/worker",
	})
	var stdout, stderr bytes.Buffer
	code := cmdSessionCloseWithOptions([]string{"worker"}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"})
	assertFencedMismatchLeftBeadUntyped(t, cityDir, sessionBead, &stdout, &stderr, code)
}

// TestReviewFencedConfiguredNameMismatchDoesNotRepairType pins the read-only
// contract of the configured named-session resolution door: the canonical
// lookup for a configured name must not persist the empty-type repair before
// the --if-state fence is acquired.
func TestReviewFencedConfiguredNameMismatchDoesNotRepairType(t *testing.T) {
	cityDir, _, sessionBead := seedUntypedFencedSession(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "worker"
mode = "on_demand"
`, map[string]string{
		"session_name":              "fenced-configured",
		"configured_named_session":  "true",
		"configured_named_identity": "worker",
	})
	var stdout, stderr bytes.Buffer
	code := cmdSessionCloseWithOptions([]string{"worker"}, &stdout, &stderr, sessionMutationOptions{IfState: "creating"})
	assertFencedMismatchLeftBeadUntyped(t, cityDir, sessionBead, &stdout, &stderr, code)
}

// TestSessionCloseAndSuspendIfStateUnknownStateIsUsageError pins the
// pre-flight vocabulary validation: an --if-state value outside the canonical
// lifecycle vocabulary is a usage error on BOTH doors, before any store or
// controller work, and the flag help names the accepted values.
func TestSessionCloseAndSuspendIfStateUnknownStateIsUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  func(io.Writer, io.Writer) *cobra.Command
	}{
		{name: "close", cmd: newSessionCloseCmd},
		{name: "suspend", cmd: newSessionSuspendCmd},
	} {
		t.Run(tc.name+" usage names vocabulary", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := tc.cmd(&stdout, &stderr)
			flag := cmd.Flag("if-state")
			if flag == nil {
				t.Fatal("--if-state flag not registered")
			}
			if !strings.Contains(flag.Usage, "one of: active,") || !strings.Contains(flag.Usage, "suspended") {
				t.Fatalf("--if-state usage = %q, want the accepted lifecycle values named", flag.Usage)
			}
		})
		t.Run(tc.name+" unknown value", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := tc.cmd(&stdout, &stderr)
			if err := cmd.Flags().Set("if-state", "garbage"); err != nil {
				t.Fatalf("set --if-state=garbage: %v", err)
			}
			err := cmd.RunE(cmd, []string{"gc-1"})
			if err == nil {
				t.Fatal("RunE with unknown --if-state returned nil, want usage error")
			}
			if !strings.Contains(stderr.String(), "--if-state must be one of:") {
				t.Fatalf("stderr = %q, want vocabulary usage error", stderr.String())
			}
			if !strings.Contains(stderr.String(), `"garbage"`) {
				t.Fatalf("stderr = %q, want the rejected value echoed", stderr.String())
			}
		})
	}
}
