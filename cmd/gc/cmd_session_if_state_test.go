package main

import (
	"bytes"
	"encoding/json"
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
