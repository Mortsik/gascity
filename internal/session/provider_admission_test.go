package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func writeAdmissionFile(t *testing.T, city, name, data string) {
	t.Helper()
	dir := filepath.Join(city, ".gc", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Missing enforcement at create must not manufacture failed attempts on every retry.
func TestProviderAdmissionCreateDeniesBeforeBead(t *testing.T) {
	for _, tc := range []struct{ name, marker, health string }{
		{"missing", `{"schema_version":1,"providers":["custom"]}`, ""},
		{"malformed", `{"schema_version":1,"providers":["custom"]}`, "{"},
		{"empty", `{"schema_version":1,"providers":["custom"]}`, `{}`},
		{"stale", `{"schema_version":1,"providers":["custom"]}`, `{"providers":[{"provider":"custom","status":"healthy","probed_at":1}]}`},
		{"expired-denial", `{"schema_version":1,"providers":["custom"]}`, `{"providers":[{"provider":"custom","status":"unhealthy","probed_at":1,"reason":"capacity unavailable","next_check_at":2}]}`},
		{"invalid-marker", "{", ""},
		{"null-marker", "null", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			writeAdmissionFile(t, city, "provider-health-required.json", tc.marker)
			if tc.health != "" {
				writeAdmissionFile(t, city, "provider-health.json", tc.health)
			}
			store, sp := beads.NewMemStore(), runtime.NewFake()
			for i := 0; i < 3; i++ {
				mgr := NewManagerWithOptions(store, sp, WithCityPath(city))
				_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Provider: "custom", Command: "fake", WorkDir: city})
				if err == nil {
					t.Error("create admitted unavailable provider")
				}
			}
			all, err := store.ListByLabel(LabelSession, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 0 {
				t.Errorf("created %d attempt beads during denial", len(all))
			}
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "Start" {
					t.Error("started runtime during denial")
				}
			}
		})
	}
}

// A denied wake must leave continuation and work metadata intact until recovery.
func TestProviderAdmissionWakeRetainsSessionAndRecovers(t *testing.T) {
	for _, runtimeOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(runtimeOnly), func(t *testing.T) {
			city := t.TempDir()
			store, sp := beads.NewMemStore(), runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp, WithCityPath(city))
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Provider: "custom", Command: "fake", WorkDir: city, BeadOnly: true, ExtraMeta: map[string]string{"continuation_reset_pending": "true", "currently_processing_bead_id": "work-1"}})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.Get(info.ID)
			writeAdmissionFile(t, city, "provider-health-required.json", `{"schema_version":1,"providers":["custom"]}`)
			writeAdmissionFile(t, city, "provider-health.json", `{"providers":[{"provider":"custom","status":"unhealthy","probed_at":1,"reason":"capacity unavailable","next_check_at":2}]}`)
			start := mgr.Start
			if runtimeOnly {
				start = mgr.StartRuntimeOnly
			}
			err = start(context.Background(), info.ID, "fake", runtime.Config{})
			if err == nil || !strings.Contains(err.Error(), "capacity unavailable") || !strings.Contains(err.Error(), "next_check_at=2") {
				t.Errorf("denial detail = %v", err)
			}
			after, _ := store.Get(info.ID)
			if !reflect.DeepEqual(before, after) {
				t.Error("denial changed session/work metadata")
			}
			if sp.CountCalls("Start", info.SessionName) != 0 || sp.CountCalls("Stop", info.SessionName) != 0 {
				t.Error("denial started or stopped runtime")
			}
			writeAdmissionFile(t, city, "provider-health.json", fmt.Sprintf(`{"providers":[{"provider":"custom","status":"healthy","probed_at":%d}]}`, time.Now().Unix()))
			if err := start(context.Background(), info.ID, "fake", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if sp.CountCalls("Start", info.SessionName) != 1 {
				t.Fatal("healthy publication did not restore start")
			}
		})
	}
}

func TestProviderAdmissionLiveNudgeDoesNotDeliver(t *testing.T) {
	city := t.TempDir()
	store, sp := beads.NewMemStore(), runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Provider: "custom", Command: "fake", WorkDir: city})
	if err != nil {
		t.Fatal(err)
	}
	writeAdmissionFile(t, city, "provider-health-required.json", `{"schema_version":1,"providers":["custom"]}`)
	before, _ := store.Get(info.ID)
	for _, immediate := range []bool{false, true} {
		delivered, err := mgr.sendLiveOnly(context.Background(), info.ID, "queued work", immediate)
		if delivered || err == nil {
			t.Errorf("denied live nudge: delivered=%v err=%v", delivered, err)
		}
	}
	delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "queue", "queued work")
	if delivered || err == nil {
		t.Errorf("denied idle nudge: delivered=%v err=%v", delivered, err)
	}
	after, _ := store.Get(info.ID)
	if !reflect.DeepEqual(before, after) {
		t.Error("denial modified session")
	}
}

func TestProviderAdmissionSubmitPrecedesRuntimeMutation(t *testing.T) {
	city := t.TempDir()
	store, sp := beads.NewMemStore(), runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Provider: "custom", Command: "fake", WorkDir: city})
	if err != nil {
		t.Fatal(err)
	}
	writeAdmissionFile(t, city, "provider-health-required.json", `{"schema_version":1,"providers":["custom"]}`)
	// Read-only probes (the bead loader's IsRunning) are allowed like on every
	// other admission boundary; mutating interactions with a denied provider
	// are not. Same convention as the sibling admission tests.
	methods := []string{"Start", "Stop", "Interrupt", "Nudge", "NudgeNow", "ResetInterruptedTurn"}
	before := make(map[string]int, len(methods))
	for _, method := range methods {
		before[method] = sp.CountCalls(method, info.SessionName)
	}
	for _, intent := range []SubmitIntent{SubmitIntentFollowUp, SubmitIntentInterruptNow} {
		_, err := mgr.Submit(context.Background(), info.ID, "work", "fake", runtime.Config{}, intent)
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("intent %v: got %v, want provider deferral", intent, err)
		}
	}
	for _, method := range methods {
		if sp.CountCalls(method, info.SessionName) != before[method] {
			t.Errorf("denied submit mutated runtime via %s", method)
		}
	}
}

// Regression for agent-forge-769p (incident 2026-09-12 ~21:41-22:30 CEST):
// the policy publisher writes probed_at with the upstream observation lag
// (observed_at = probe time - proxy quota-snapshot age) and republishes the
// frozen record from its cache between real probes, so a healthy record
// legitimately reads older than ProviderHealthTTL while the publisher is
// alive and the provider is healthy. Guard journal that night: probe run
// 20:36:45.228 UTC published probed_at 31.1 s in the past with
// next_check_at = probe+30s, and the next real probe landed at 20:37:39.951
// UTC. A strict probed_at TTL denied required providers ("provider
// health observation is stale" / JS "provider-state-unavailable") for most
// of every probe cycle. Admission must trust the published re-check
// deadline instead, bounded by ProviderRefreshGrace.
func TestProviderAdmissionCheckFreshnessTrustsPublishedNextCheckAt(t *testing.T) {
	const (
		probedAt = 1789245405.116 // observed_at: 31.1 s before the probe run
		// Reconcile time + 30 s = probe wall clock + 30 s = probed_at+61.1 s.
		nextCheckAt = 1789245466.216
	)
	at := func(offset float64) time.Time {
		return time.UnixMilli(1789245405116).Add(time.Duration(offset * float64(time.Second)))
	}
	record := func(next float64) string {
		if next <= 0 {
			return fmt.Sprintf(`{"providers":[{"provider":"custom","status":"healthy","probed_at":%v,"reason":"peak-open"}]}`, probedAt)
		}
		return fmt.Sprintf(`{"providers":[{"provider":"custom","status":"healthy","probed_at":%v,"reason":"peak-open","next_check_at":%v}]}`, probedAt, next)
	}
	for _, tc := range []struct {
		name        string
		nextCheckAt float64
		now         time.Time
		allowed     bool
	}{
		// Freshly published record reads young: allowed on both sides of the
		// legacy TTL boundary while the refresh deadline stands.
		{"mid-cycle", nextCheckAt, at(6), true},
		{"past-legacy-ttl-deadline-standing", nextCheckAt, at(65), true},
		{"late-cycle-before-deadline", nextCheckAt, at(90), true},
		// The refresh deadline plus one grace passed without a republish:
		// the publisher missed its contract, fail closed again.
		{"deadline-and-grace-exhausted", nextCheckAt, at(95), false},
		// A deadline implausibly far out is not trusted (no live publisher
		// writes one): fall back to the legacy probed_at TTL.
		{"far-future-deadline-untrusted", probedAt + 3600, at(65), false},
		// Legacy record without next_check_at keeps the old TTL exactly.
		{"no-deadline-legacy-ttl", 0, at(6), true},
		{"no-deadline-expired", 0, at(65), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			writeAdmissionFile(t, city, "provider-health-required.json", `{"schema_version":1,"providers":["custom"]}`)
			writeAdmissionFile(t, city, "provider-health.json", record(tc.nextCheckAt))
			d := LoadProviderHealthSnapshot(city, tc.now).Check("custom")
			if d.Allowed != tc.allowed {
				t.Errorf("Check at +%.0fs (next_check_at=%v): allowed=%v reason=%q", tc.now.Sub(at(0)).Seconds(), tc.nextCheckAt, d.Allowed, d.Reason)
			}
			if !tc.allowed {
				if !d.Observed || d.Reason == "" {
					t.Errorf("denial must carry an observed reason, got %+v", d)
				}
			}
		})
	}
}
