// provider_health_gate.go — reconciler-side provider health gate (ADR-0013 A1 M3a).
//
// Architecture: the pack's anthropic-failover-watcher writes
// provider-health.json; this file's code reads it. The reconciler is a
// pure consumer: it never classifies errors itself.
//
// Per-tick flow:
//  1. loadProviderHealthSnapshot reads the JSON file once at the start of
//     each reconciler tick and returns a providerHealthSnapshot.
//  2. snapshot.check(provider) is an in-memory lookup — no additional I/O.
//  3. providerHealthGate (reconciler-scoped, lives on CityRuntime) tracks
//     per-provider episode state across ticks so exactly one alert fires
//     per red episode.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/google/uuid"
)

const (
	// providerHealthCacheRelPath is the file written by provider-health-probe.
	// Resolved relative to cityPath so writer and reader agree without a
	// plist-injected env var.
	providerHealthCacheRelPath = ".gc/cache/provider-health.json"

	// providerHealthTTL matches the probe interval in the voxist-city pack.
	providerHealthTTL = 60 * time.Second
)

// --- snapshot (loaded once per tick) ---

// providerHealthSnapshot is an immutable, per-tick view of the health file.
// It is loaded once at the top of each reconciler tick via
// loadProviderHealthSnapshot; all per-session gate checks use the snapshot
// (no additional file I/O).
type providerHealthSnapshot struct {
	admission *session.ProviderHealthSnapshot
}

// loadProviderHealthSnapshot reads cityPath/.gc/cache/provider-health.json and
// returns a snapshot for this reconciler tick. It is safe to call when the
// file is absent; required-provider policy determines whether absence denies.
func loadProviderHealthSnapshot(cityPath string) *providerHealthSnapshot {
	return &providerHealthSnapshot{admission: session.LoadProviderHealthSnapshot(cityPath, time.Now())}
}

// check returns (healthy, registryPresent).
//   - registryPresent=false: no denial-scope entry for providerName — this
//     happens only for providers not opted into required health (file absent,
//     unreadable, or no fresh entry). Callers MUST treat this as healthy=true
//     (fail-open).
//   - registryPresent=true, healthy=false: provider is red; gate respawn.
//     A required provider's missing, malformed or stale evidence is reported
//     here as an observation, so gates fail closed for required providers.
//   - registryPresent=true, healthy=true: provider is green; allow respawn.
func (s *providerHealthSnapshot) check(providerName string) (healthy, registryPresent bool) {
	if s == nil {
		return true, false
	}
	d := s.admission.Check(providerName)
	return d.Allowed, d.Observed
}

// healthyProviders returns all provider names that are confirmed healthy in
// this snapshot. Used to flush green ticks after the Phase-2 loop.
func (s *providerHealthSnapshot) healthyProviders() []string {
	if s == nil {
		return nil
	}
	return s.admission.HealthyProviders()
}

// --- episode state (lives across ticks on CityRuntime) ---

type providerStatus int

const (
	providerStatusGreen providerStatus = iota
	providerStatusRed
)

// providerEpisodeState tracks one provider's red/green episode.
// A new episode begins on each green→red transition; AlertSent resets
// on green so the next red episode fires a fresh alert.
type providerEpisodeState struct {
	Status       providerStatus
	EpisodeID    string
	AlertSent    bool
	RedSince     time.Time
	SessionCount int
}

// providerHealthGate is reconciler-scoped, persistent across ticks.
// It lives on CityRuntime alongside sessionDrains.
type providerHealthGate struct {
	mu       sync.Mutex
	episodes map[string]*providerEpisodeState
}

// newProviderHealthGate allocates a gate with empty episode state.
func newProviderHealthGate() *providerHealthGate {
	return &providerHealthGate{episodes: make(map[string]*providerEpisodeState)}
}

// recordRedSkip notes that providerName is red this tick and a session respawn
// was parked. emitAlert is called exactly once per episode (idempotent guard
// on AlertSent). Safe to call concurrently.
func (g *providerHealthGate) recordRedSkip(
	providerName string,
	now time.Time,
	emitAlert func(provider, episodeID string, redSince time.Time, sessionCount int),
) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.episodeFor(providerName)
	if s.Status == providerStatusGreen {
		s.Status = providerStatusRed
		s.EpisodeID = uuid.New().String()
		s.AlertSent = false
		s.RedSince = now
		s.SessionCount = 1
	} else {
		s.SessionCount++
	}
	if !s.AlertSent {
		emitAlert(providerName, s.EpisodeID, s.RedSince, s.SessionCount)
		s.AlertSent = true
	}
}

// recordGreenTick clears red state so the next red transition opens a fresh
// episode and fires a new alert. Called once per tick per healthy provider.
func (g *providerHealthGate) recordGreenTick(providerName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.episodes[providerName]; ok {
		s.Status = providerStatusGreen
		s.AlertSent = false
		s.SessionCount = 0
	}
}

func (g *providerHealthGate) episodeFor(providerName string) *providerEpisodeState {
	if s, ok := g.episodes[providerName]; ok {
		return s
	}
	s := &providerEpisodeState{}
	g.episodes[providerName] = s
	return s
}

// --- alert emission ---

// emitProviderHealthGateAlert writes the ADR-0013 escalation alert to stdout
// and records a ProviderHealthGateAlert event. Called once per episode.
func emitProviderHealthGateAlert(
	rec events.Recorder,
	stdout io.Writer,
	provider, episodeID string,
	redSince time.Time,
	sessionCount int,
) {
	msg := fmt.Sprintf(
		"Provider health gate OPEN: provider=%s episode=%s since=%s sessions_parked=%d. "+
			"Respawn for %s sessions paused. "+
			"Resume is automatic when provider returns green. "+
			"Verify token with: gc provider status %s",
		provider, episodeID, redSince.UTC().Format(time.RFC3339), sessionCount,
		provider, provider,
	)
	fmt.Fprintln(stdout, msg) //nolint:errcheck
	if rec == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"provider":      provider,
		"episode_id":    episodeID,
		"red_since":     redSince.UTC().Format(time.RFC3339),
		"session_count": sessionCount,
	})
	rec.Record(events.Event{
		Type:    events.ProviderHealthGateAlert,
		Ts:      time.Now().UTC(),
		Actor:   "gc",
		Subject: provider,
		Message: msg,
		Payload: payload,
	})
}
