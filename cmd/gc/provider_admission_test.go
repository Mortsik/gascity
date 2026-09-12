package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/worker"
)

func requireAdmissionProvider(t *testing.T, city string) {
	t.Helper()
	dir := filepath.Join(city, ".gc", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider-health-required.json"), []byte(`{"schema_version":1,"providers":["custom"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredProviderQueuedNudgeDenialPrecedesObservation(t *testing.T) {
	// The manager runs on a MemStore, but the queue helpers below open the
	// city's real nudge store; without the file backend they provision a
	// managed dolt sql-server under the temp city and the leak guard fires.
	t.Setenv("GC_BEADS", "file")
	city := t.TempDir()
	store := beads.NewMemStore()
	fake := runtime.NewFake()
	mgr := session.NewManagerWithOptions(store, fake, session.WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Provider: "custom", Command: "fake", WorkDir: city})
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueQueuedNudge(city, newQueuedNudge("worker", "preserve this work", time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	requireAdmissionProvider(t, city)
	beforeCalls := len(fake.SnapshotCalls())
	delivered, err := tryDeliverQueuedNudgesByPoller(nudgeTarget{cityPath: city, agent: config.Agent{Name: "worker"}, sessionID: info.ID, sessionName: info.SessionName,
		resolved: &config.ResolvedProvider{Name: "custom"}}, store, store, fake, 0, worker.LiveObservation{Running: true})
	if delivered || err != nil {
		t.Fatalf("provider hold should leave work queued: delivered=%v err=%v", delivered, err)
	}
	if len(fake.SnapshotCalls()) != beforeCalls {
		t.Error("provider hold probed or nudged runtime")
	}
	pending, inFlight, dead, err := listQueuedNudges(city, "worker", time.Now())
	if err != nil || len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue not preserved: pending=%d inFlight=%d dead=%d err=%v", len(pending), len(inFlight), len(dead), err)
	}
}

func TestRequiredProviderSnapshotDoesNotFailOpen(t *testing.T) {
	city := t.TempDir()
	requireAdmissionProvider(t, city)
	for _, state := range []string{"missing", "unhealthy", "healthy"} {
		if state != "missing" {
			writeHealthCache(t, city, "custom", state, 1)
		}
		snap := loadProviderHealthSnapshot(city)
		if healthy, present := snap.check("custom"); healthy || !present {
			t.Errorf("%s admitted: healthy=%v present=%v", state, healthy, present)
		}
		if healthy, _ := snap.check("second"); !healthy {
			t.Error("optional second provider denied")
		}
	}
}

// Pool refill must reject before allocating slots or resolving trigger work.
func TestRequiredProviderPoolDenialPrecedesPlanning(t *testing.T) {
	city := t.TempDir()
	requireAdmissionProvider(t, city)
	usedSlots := map[int]bool{}
	bp := &agentBuildParams{cityPath: city, providerHealthSnapshot: loadProviderHealthSnapshot(city)}
	_, _, plan, err := selectOrPlanPoolSessionBead(bp, &config.Agent{Name: "helper", Provider: "custom"}, "helper", nil, SessionRequest{}, time.Now(), map[string]bool{}, usedSlots)
	if err != errPoolSessionCreateProviderRed {
		t.Fatalf("got %v; want provider refusal before planning", err)
	}
	if plan != nil || len(usedSlots) != 0 {
		t.Fatal("denial allocated a pool slot")
	}
}

// Rechecking after the tick snapshot must precede wake bookkeeping.
func TestRequiredProviderPreparedStartPreservesWakeMetadata(t *testing.T) {
	city := t.TempDir()
	requireAdmissionProvider(t, city)
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"provider": "custom", "state": "asleep", "session_name": "helper", "template": "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := startCandidate{info: sessiontest.SeedBead(t, b), tp: TemplateParams{TemplateName: "helper", ResolvedProvider: &config.ResolvedProvider{Name: "custom"}}}
	_, err = prepareStartCandidateForCity(candidate, city, "test", &config.City{}, runtime.NewFake(), store, clock.Real{}, io.Discard, nil)
	if err == nil {
		t.Error("prepared an unavailable provider")
	}
	after, _ := store.Get(b.ID)
	if !reflect.DeepEqual(b, after) {
		t.Error("denial modified wake bookkeeping")
	}
}
