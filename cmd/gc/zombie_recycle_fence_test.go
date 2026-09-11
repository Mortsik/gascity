package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// rryr stage 2 (agent-forge-rryr, mutator #3): the zombie recycle in
// startPreparedStartCandidate stops the runtime BY NAME whenever the pane is
// up but the agent process is not visible yet. Under load (2026-09-10/11:
// start_call 13-40s, six-way parallel start waves) that visibility window is
// exactly the cold start of a SIBLING in-flight start: its bead is still open,
// claimed, and inside its in-flight lease, and its nudge has not fired yet.
// Stop-by-name at that moment kills the sibling's session mid-startup — the
// startup nudge then burns its whole retry budget against a session another
// writer tore down ("sending startup nudge: agent not ready for input" /
// "no current client" bursts), the start rolls back, the next tick recreates
// the bead, and the same-name recycle repeats: a cyclic respawn under one
// name with two writers.
//
// The fence: the recycle must defer when ANOTHER OPEN bead holds a live
// in-flight pending-create lease on the same session name. A previous
// INCARNATION (closed bead, no live lease) still recycles — that is the
// ga-yms collide-loop fix this must not regress.

// zombieRecycleFixture seeds one open foreign bead mid-start (live lease) and
// a candidate for a different bead against the same session name.
func zombieRecycleFixture(t *testing.T) (sp *runtime.Fake, store beads.Store, item preparedStart, cfg *config.City, clk *clock.Fake, siblingID string) {
	t.Helper()
	clk = &clock.Fake{Time: time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC)}
	store = beads.NewMemStore()
	foreign, err := store.Create(beads.Bead{
		ID:     "gc-inflight-sibling",
		Title:  "in-flight sibling",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "shared-name",
			"template":             "worker",
			"instance_token":       "tok-sibling",
			"pending_create_claim": "true",
			"last_woke_at":         time.Now().UTC().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = sessiontest.SeedBead(t, foreign)
	newcomer, err := store.Create(beads.Bead{
		Title:  "newcomer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":   "shared-name",
			"template":       "worker",
			"instance_token": "tok-newcomer",
			"state":          "asleep",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = sessiontest.SeedBead(t, newcomer)

	sp = runtime.NewFake()
	// The sibling's start is mid-flight: the container exists (running) but
	// the agent process has not appeared yet (zombie read).
	if err := sp.Start(context.Background(), "shared-name", runtime.Config{ProcessNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	sp.Zombies["shared-name"] = true

	item = preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{
				ID:                  newcomer.ID,
				SessionName:         "shared-name",
				SessionNameMetadata: "shared-name",
				Template:            "worker",
				InstanceToken:       "tok-newcomer",
			},
			tp: TemplateParams{Command: "claude", SessionName: "shared-name", TemplateName: "worker"},
		},
		cfg: runtime.Config{Command: "claude", ProcessNames: []string{"claude"}},
	}
	cfg = &config.City{Session: config.SessionConfig{StartupTimeout: "60s"}}
	return sp, store, item, cfg, clk, foreign.ID
}

func TestStartPreparedStartCandidate_ZombieRecycleDefersToInFlightSiblingLease(t *testing.T) {
	sp, store, item, cfg, _, _ := zombieRecycleFixture(t)

	started, err := startPreparedStartCandidate(
		context.Background(),
		item,
		"",
		store,
		sp,
		cfg,
		nil,
		immediateSessionStaleKeyDetectionWaiter,
		nil,
	)
	if err != nil {
		t.Fatalf("zombie recycle must defer, not fail, while a sibling start is in flight: %v", err)
	}
	if started {
		t.Fatal("zombie recycle must not run a fresh start while the sibling lease is live (two writers on one name)")
	}
	if got := fakeRuntimeCallCount(sp, "Stop"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0: the recycle killed a session another in-flight start still owns", got)
	}
}

// The recycle must keep working once the sibling's lease is gone (closed bead
// or expired lease): that is the legacy zombie the ga-yms fix recycles.
func TestStartPreparedStartCandidate_ZombieRecycleStillFiresWithoutLiveLease(t *testing.T) {
	sp, store, item, cfg, _, siblingID := zombieRecycleFixture(t)
	// Expire the sibling's in-flight lease.
	stale := time.Now().UTC().Add(-90 * time.Second)
	if err := store.SetMetadata(siblingID, "last_woke_at", stale.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	started, err := startPreparedStartCandidate(
		context.Background(),
		item,
		"",
		store,
		sp,
		cfg,
		nil,
		immediateSessionStaleKeyDetectionWaiter,
		nil,
	)
	if err != nil {
		t.Fatalf("stale zombie must still recycle: %v", err)
	}
	if !started {
		t.Fatal("expected a fresh start after recycling the stale zombie")
	}
	if got := fakeRuntimeCallCount(sp, "Stop"); got != 1 {
		t.Fatalf("Stop calls = %d, want 1 (zombie recycle)", got)
	}
}
