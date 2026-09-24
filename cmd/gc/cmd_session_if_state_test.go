package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
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
	if pokes < 2 {
		t.Fatalf("managed match poke count = %d, want the pre-patch poke and the post-patch reconciler tick", pokes)
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
