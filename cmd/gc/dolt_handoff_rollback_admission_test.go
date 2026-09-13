package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRestoredHandoffJournal builds the shape a compensated handoff leaves:
// a journal at phase, owned by the legacy side, whose checkpoint carries the
// three workspace artifacts bd restored. The files are written with those exact
// bytes unless drift says otherwise; checkpoint varies the captured config so a
// caller can describe a second, different restoration of the same scope.
func writeRestoredHandoffJournal(t *testing.T, city, phase string, drift bool, checkpoint ...string) []byte {
	t.Helper()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"backend":"dolt","dolt_database":"beads"}`)
	config := []byte("gc.endpoint_origin: managed_city\ndolt.auto-start: false\ntypes.custom: session,molecule\n")
	if len(checkpoint) == 1 {
		config = []byte(checkpoint[0])
	}
	port := []byte("3307\n")

	onDisk := config
	if drift {
		onDisk = []byte("gc.endpoint_origin: managed_city\ndolt.auto-start: true\n")
	}
	for _, file := range []struct {
		name string
		body []byte
	}{{"metadata.json", metadata}, {"config.yaml", onDisk}, {"dolt-server.port", port}} {
		if err := os.WriteFile(filepath.Join(beadsDir, file.name), file.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var journal handoffProjectionJournal
	journal.Request.CityRoot, journal.Request.Root = city, city
	journal.Request.Database, journal.Request.Workspace = "beads", "test"
	journal.Request.Endpoint.Host, journal.Request.Endpoint.Port = "127.0.0.1", 3307
	journal.Request.Owner = "legacy-gc"
	journal.Phase, journal.Owner = phase, "legacy-gc"
	journal.SnapshotCaptured, journal.MutationOccurred = true, true
	setProjectionEligibleSnapshot(t, &journal)
	journal.Snapshot.WorkspaceMetadata = metadata
	journal.Snapshot.WorkspaceConfig = config
	journal.Snapshot.WorkspacePort = port
	journal.Snapshot.WorkspaceMetadataPresent = true
	journal.Snapshot.WorkspaceConfigPresent = true
	journal.Snapshot.WorkspacePortPresent = true
	journal.Snapshot.WorkspaceMetadataMode = 0o600
	journal.Snapshot.WorkspaceConfigMode = 0o600
	journal.Snapshot.WorkspacePortMode = 0o600

	body, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

// The rollback's last step is gc restarting the legacy owner, and that restart
// reconciles the scope's canonical config — the very file the journal's
// checkpoint is compared against. Re-running the comparison afterwards refused
// every later gc command on a city that had been successfully handed back.
func TestRolledBackHandoffAdmissionSurvivesGCsOwnCanonicalRewrite(t *testing.T) {
	for _, phase := range []string{"legacy_config_restored", "rolled_back"} {
		t.Run(phase, func(t *testing.T) {
			city := handoffGuardTestCity(t)
			restored := writeRestoredHandoffJournal(t, city, phase, false)

			owned, err := committedBeadsHandoffOwnsScope(city)
			if err != nil {
				t.Fatalf("a byte-exact restored journal was refused: %v", err)
			}
			if owned {
				t.Fatal("a restored journal reported the scope as still bd's")
			}
			marker := rolledBackHandoffAdmissionPath(city)
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("gc admitted the rollback without recording it: %v", err)
			}

			// gc's own lifecycle canonicalisation, which is what the restart
			// performs: the bead vocabulary this build knows about is merged
			// into the config the journal captured before it existed.
			rewritten := strings.Replace(string(restored), "types.custom: session,molecule",
				"types.custom: session,molecule,startup-health-episode", 1)
			if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(rewritten), 0o600); err != nil {
				t.Fatal(err)
			}

			owned, err = committedBeadsHandoffOwnsScope(city)
			if err != nil {
				t.Fatalf("an admitted rollback refused gc's own canonical rewrite: %v", err)
			}
			if owned {
				t.Fatal("an admitted rollback reported the scope as bd's")
			}
			if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
				t.Fatalf("an admitted rollback still blocks the managed lifecycle: %v", err)
			}
		})
	}
}

// The admission is one-time, not unconditional. A journal whose artifacts were
// never found byte-exact has not proven the rollback completed, and the scope
// stays refused however many times it is asked.
func TestNeverMatchedRollbackStaysRefused(t *testing.T) {
	city := handoffGuardTestCity(t)
	writeRestoredHandoffJournal(t, city, "legacy_config_restored", true)

	for attempt := range 2 {
		if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
			t.Fatalf("attempt %d admitted a rollback whose artifacts never matched", attempt+1)
		}
	}
	if _, err := os.Stat(rolledBackHandoffAdmissionPath(city)); !os.IsNotExist(err) {
		t.Fatalf("a refused rollback recorded an admission: %v", err)
	}
}

// A marker is scoped to the restoration it was written for. A later handoff
// generation that restores different bytes gets its own gate rather than
// inheriting this one.
func TestRollbackAdmissionDoesNotCarryToAnotherRestoration(t *testing.T) {
	city := handoffGuardTestCity(t)
	writeRestoredHandoffJournal(t, city, "rolled_back", false)
	if _, err := committedBeadsHandoffOwnsScope(city); err != nil {
		t.Fatalf("first admission: %v", err)
	}

	marker, err := os.ReadFile(rolledBackHandoffAdmissionPath(city))
	if err != nil {
		t.Fatal(err)
	}
	var admission rolledBackHandoffAdmission
	if err := json.Unmarshal(marker, &admission); err != nil {
		t.Fatalf("admission marker is not readable: %v\n%s", err, marker)
	}
	if admission.Version != rolledBackHandoffAdmissionVersion || !samePath(admission.Scope, city) || admission.Restored == "" {
		t.Fatalf("admission marker = %+v", admission)
	}

	// A second generation: same scope, different captured bytes, and this one
	// did not land on disk.
	writeRestoredHandoffJournal(t, city, "legacy_config_restored", true,
		"gc.endpoint_origin: managed_city\ndolt.auto-start: false\ntypes.custom: session,molecule,step\n")
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("a stale admission admitted a different restoration")
	}
}

// A marker gc cannot parse is not an admission, and must not be one: the
// byte-exact gate runs again and answers on the artifacts themselves.
func TestUnreadableRollbackAdmissionFallsBackToTheGate(t *testing.T) {
	city := handoffGuardTestCity(t)
	writeRestoredHandoffJournal(t, city, "rolled_back", true)
	path := rolledBackHandoffAdmissionPath(city)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("an unreadable marker was treated as an admission")
	}
}
