package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The start-side heartbeat is the single-writer fence: an async start whose
// spawn is delayed (wake budget, async limiter) or slow (17-30s live starts)
// re-stamps last_woke_at so every sibling destructive path — the pending-create
// rollback arms, the stuck-creating reaper, the stale-start stop — sees a live
// lease instead of an expired one, and defers. Without the refresh the lease
// anchored at enqueue time expires mid-flight and the rollback closes the bead
// out from under the running start (2026-09-10/11 churn: rollback → alias
// freed → same-name respawn → nudge delivered to a corpse).
func TestRefreshPendingStartInFlightLease_ReStampsStaleLeaseWhileClaimed(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC)}
	store := beads.NewMemStore()
	stale := clk.Now().Add(-90 * time.Second)
	session, err := store.Create(beads.Bead{
		ID:     "gc-lease-refresh",
		Title:  "lease refresh",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "lease-refresh",
			"template":             "lease-refresh",
			"instance_token":       "tok-refresh",
			"pending_create_claim": "true",
			"last_woke_at":         stale.Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessFront := sessionFrontDoor(store)
	info := sessiontest.SeedBead(t, session)
	startupTimeout := 60 * time.Second
	if pendingCreateStartInFlightInfo(info, clk, startupTimeout) {
		t.Fatal("precondition: stale lease must read as expired before the refresh")
	}

	refreshPendingStartInFlightLease(info.ID, sessFront, clk.Now().UTC(), io.Discard)

	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	info = sessiontest.SeedBead(t, updated)
	if !pendingCreateStartInFlightInfo(info, clk, startupTimeout) {
		t.Fatal("refresh did not re-stamp last_woke_at: rollback arms can still fire mid-start")
	}
}

func TestRefreshPendingStartInFlightLease_SkipsBeadWithoutClaim(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC)}
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		ID:     "gc-lease-noclaim",
		Title:  "lease noclaim",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name": "lease-noclaim",
			"template":     "lease-noclaim",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessFront := sessionFrontDoor(store)
	info := sessiontest.SeedBead(t, session)

	refreshPendingStartInFlightLease(info.ID, sessFront, clk.Now().UTC(), io.Discard)

	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want untouched for a bead without a pending-create claim", got)
	}
}

// stopStaleAsyncStartRuntime is the commit-time cleanup for a start that lost
// its lease. The fence: if the CURRENT bead is claimed by a DIFFERENT identity
// (a newer start re-claimed the same name after the rollback freed the alias),
// the stale start must NOT stop the runtime by name — the live tmux session
// may already belong to the newcomer (the runtime-meta probe reads the OLD
// stamp until the newcomer re-binds, so the probe alone cannot tell).
func TestStopStaleAsyncStartRuntime_VetoesStopWhenNewerStartOwnsBead(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC)}
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		ID:     "gc-stop-veto",
		Title:  "stop veto",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "stop-veto",
			"template":             "stop-veto",
			"instance_token":       "tok-newer",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "stop-veto", runtime.Config{Command: "worker"}); err != nil {
		t.Fatal(err)
	}
	// The live runtime still carries the STALE start's stamps: the newcomer has
	// not re-bound the name yet. This is exactly the TOCTOU window.
	if err := sp.SetMeta("stop-veto", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("stop-veto", "GC_INSTANCE_TOKEN", "tok-stale"); err != nil {
		t.Fatal(err)
	}

	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{
					ID:                  session.ID,
					InstanceToken:       "tok-stale",
					SessionNameMetadata: "stop-veto",
					PendingCreateClaim:  true,
				},
				tp: TemplateParams{SessionName: "stop-veto", TemplateName: "stop-veto"},
			},
		},
	}

	stopStaleAsyncStartRuntime(result, sp, store, io.Discard)

	for _, call := range sp.Calls {
		if call.Method == "Stop" {
			t.Fatalf("stale start stopped a runtime re-claimed by a newer start: %v", sp.Calls)
		}
	}
}

// No newer claim: the plain post-rollback cleanup still stops the spawn.
func TestStopStaleAsyncStartRuntime_StopsWhenBeadUnclaimed(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		ID:     "gc-stop-plain",
		Title:  "stop plain",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":   "stop-plain",
			"template":       "stop-plain",
			"instance_token": "tok-stale",
			// pending_create_claim cleared by the rollback; no newcomer.
			"pending_create_claim": "",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "stop-plain", runtime.Config{Command: "worker"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("stop-plain", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("stop-plain", "GC_INSTANCE_TOKEN", "tok-stale"); err != nil {
		t.Fatal(err)
	}

	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{
					ID:                  session.ID,
					InstanceToken:       "tok-stale",
					SessionNameMetadata: "stop-plain",
					PendingCreateClaim:  true,
				},
				tp: TemplateParams{SessionName: "stop-plain", TemplateName: "stop-plain"},
			},
		},
	}

	stopStaleAsyncStartRuntime(result, sp, store, io.Discard)

	stopped := false
	for _, call := range sp.Calls {
		if call.Method == "Stop" {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("expected Stop for an unclaimed bead after rollback, calls: %v", sp.Calls)
	}
}
