package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// fakeGateClock pins the gate's clock at an adjustable time so the spacing and
// backoff decisions are asserted without wall-clock sleeps (the gate's own
// time.AfterFunc never fires under a fake clock because the tests never leave
// a trigger parked at the end; cancelPending covers the rest).
type fakeGateClock struct{ now time.Time }

func (c *fakeGateClock) Now() time.Time { return c.now }

// fixedJitter returns a jitter source pinned to r so backoff math is exact.
func fixedJitter(r float64) func() float64 { return func() float64 { return r } }

// healthySnapshot and failingSnapshot stub the bd exec health probe.
func healthySnapshot() beads.BDExecHealthSnapshot { return beads.BDExecHealthSnapshot{} }

func failingSnapshot(consecutive int) beads.BDExecHealthSnapshot {
	return beads.BDExecHealthSnapshot{ConsecutiveFailures: consecutive}
}

func newTestGate(c *fakeGateClock, consecutive int) *reconcileGate {
	return newReconcileGate(c.Now, fixedJitter(0.5), func() beads.BDExecHealthSnapshot {
		return failingSnapshot(consecutive)
	})
}

func TestReconcileGateFirstTickStartsImmediately(t *testing.T) {
	c := &fakeGateClock{now: time.Unix(1000, 0)}
	gate := newTestGate(c, 0)
	if !gate.trigger("patrol", 15*time.Second) {
		t.Fatalf("first trigger must start immediately, got parked")
	}
}

func TestReconcileGateEnforcesMinimumSpacing(t *testing.T) {
	c := &fakeGateClock{now: time.Unix(1000, 0)}
	gate := newTestGate(c, 0)

	if !gate.trigger("patrol", 15*time.Second) {
		t.Fatalf("first trigger must start immediately")
	}
	gate.tickCompleted(15 * time.Second)

	// A tick that ended 1s ago: the next tick must wait out the spacing.
	c.now = c.now.Add(time.Second)
	if gate.trigger("poke", 15*time.Second) {
		t.Fatalf("trigger 1s after tick end must be parked")
	}
	if got := gate.take(); got != "poke" {
		t.Fatalf("parked trigger = %q, want %q", got, "poke")
	}
	// Still parked just before the gap elapses (1s + 13s = 14s of 15s).
	c.now = c.now.Add(13 * time.Second)
	if gate.trigger("patrol", 15*time.Second) {
		t.Fatalf("trigger 14s after tick end must be parked")
	}
	// Eligible at exactly minGap after the tick end.
	c.now = c.now.Add(time.Second)
	if !gate.trigger("patrol", 15*time.Second) {
		t.Fatalf("trigger at minGap after tick end must start")
	}
}

func TestReconcileGateParksAndCoalescesTriggers(t *testing.T) {
	c := &fakeGateClock{now: time.Unix(1000, 0)}
	gate := newTestGate(c, 0)

	gate.trigger("patrol", time.Minute)
	gate.tickCompleted(time.Minute)

	// A burst of distinct triggers inside the spacing window collapses to the
	// last one: the eventual single tick re-reads authoritative state.
	for _, trig := range []string{"poke", "control-dispatcher", "poke"} {
		if gate.trigger(trig, time.Minute) {
			t.Fatalf("trigger %q inside spacing window must be parked", trig)
		}
	}
	if got := gate.take(); got != "poke" {
		t.Fatalf("parked trigger = %q, want last trigger %q", got, "poke")
	}
	if got := gate.take(); got != "" {
		t.Fatalf("take must clear the slot, got %q", got)
	}
	gate.cancelPending()
}

func TestReconcileGateZeroMinGapDisablesSpacing(t *testing.T) {
	c := &fakeGateClock{now: time.Unix(1000, 0)}
	gate := newTestGate(c, 0)

	gate.trigger("patrol", 0)
	gate.tickCompleted(0)
	if !gate.trigger("poke", 0) {
		t.Fatalf("min tick interval 0 must disable spacing entirely")
	}
	gate.tickCompleted(0)
	if !gate.trigger("patrol", 0) {
		t.Fatalf("min tick interval 0 must keep the gate open")
	}
}

func TestReconcileGateFailureBackoffCurve(t *testing.T) {
	// Jitter pinned to 0.5, so every rung's backoff is exactly 3/4 of its
	// pre-jitter value (equal jitter: fixed half + 0.5 * random half).
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{1, 3 * time.Second},   // 4s  pre-jitter
		{2, 6 * time.Second},   // 8s
		{3, 12 * time.Second},  // 16s
		{4, 24 * time.Second},  // 32s
		{8, 384 * time.Second}, // 512s
		// base<<failures overflows the 10m cap; the cap halves are exact too.
		{20, (10 * time.Minute) * 3 / 4},
	}
	for _, tc := range cases {
		c := &fakeGateClock{now: time.Unix(1000, 0)}
		gate := newTestGate(c, tc.consecutive)
		gate.trigger("patrol", 10*time.Second)
		gate.tickCompleted(10 * time.Second)
		want := c.now.Add(10*time.Second + tc.want)
		if !gate.eligibleAt.Equal(want) {
			t.Fatalf("consecutive=%d: eligibleAt = %v, want %v (minGap + %v)", tc.consecutive, gate.eligibleAt, want, tc.want)
		}
	}
}

func TestReconcileGateBackoffBoundsWithExtremeJitter(t *testing.T) {
	// Equal jitter must keep the backoff inside [half, full] of the computed
	// rung whatever the random source returns.
	for _, r := range []float64{0, 0.25, 1} {
		c := &fakeGateClock{now: time.Unix(1000, 0)}
		gate := newReconcileGate(c.Now, fixedJitter(r), func() beads.BDExecHealthSnapshot {
			return failingSnapshot(2)
		})
		gate.trigger("patrol", 0)
		gate.tickCompleted(0)
		low, high := c.now.Add(4*time.Second), c.now.Add(8*time.Second)
		if gate.eligibleAt.Before(low) || gate.eligibleAt.After(high) {
			t.Fatalf("jitter=%v: eligibleAt %v outside [%v, %v]", r, gate.eligibleAt, low, high)
		}
	}
}

func TestReconcileGateHealthyTickTakesNoBackoff(t *testing.T) {
	c := &fakeGateClock{now: time.Unix(1000, 0)}
	gate := newTestGate(c, 0)

	// A failing tick spaces the next one out...
	failing := newReconcileGate(c.Now, fixedJitter(0.5), func() beads.BDExecHealthSnapshot {
		return failingSnapshot(1)
	})
	failing.trigger("patrol", 15*time.Second)
	failing.tickCompleted(15 * time.Second)
	if before := c.now.Add(15*time.Second + 3*time.Second); !failing.eligibleAt.After(before.Add(-time.Millisecond)) {
		t.Fatalf("failing tick must add backoff, eligibleAt = %v", failing.eligibleAt)
	}

	// ...while a healthy tick pays only the configured gap: the gate cadences,
	// it does not punish a working loop.
	gate.trigger("patrol", 15*time.Second)
	gate.tickCompleted(15 * time.Second)
	if !gate.eligibleAt.Equal(c.now.Add(15 * time.Second)) {
		t.Fatalf("healthy tick eligibleAt = %v, want minGap only", gate.eligibleAt)
	}
}

func TestReconcileGateBackoffLadderFunction(t *testing.T) {
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{0, 0},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{8, 512 * time.Second}, // 8m32s — the last rung under the cap
		{9, reconcileGateBackoffCap},
		{100, reconcileGateBackoffCap},
	}
	for _, tc := range cases {
		if got := reconcileGateBackoff(tc.consecutive); got != tc.want {
			t.Fatalf("reconcileGateBackoff(%d) = %v, want %v", tc.consecutive, got, tc.want)
		}
	}
}

func TestDaemonMinTickInterval(t *testing.T) {
	cfg := &config.City{Daemon: config.DaemonConfig{}}

	if got := daemonMinTickInterval(cfg); got != config.DefaultMinTickInterval {
		t.Fatalf("empty config: got %v, want default %v", got, config.DefaultMinTickInterval)
	}

	t.Setenv("GC_MIN_TICK_INTERVAL", "45s")
	if got := daemonMinTickInterval(cfg); got != 45*time.Second {
		t.Fatalf("env override: got %v, want 45s", got)
	}

	// An unparseable env must not brick the loop: fall back to config.
	t.Setenv("GC_MIN_TICK_INTERVAL", "not-a-duration")
	if got := daemonMinTickInterval(cfg); got != config.DefaultMinTickInterval {
		t.Fatalf("unparseable env: got %v, want config default", got)
	}

	// Explicit disable paths.
	t.Setenv("GC_MIN_TICK_INTERVAL", "0s")
	if got := daemonMinTickInterval(cfg); got != 0 {
		t.Fatalf("env 0s: got %v, want 0 (disabled)", got)
	}
	t.Setenv("GC_MIN_TICK_INTERVAL", "")
	cfg.Daemon.MinTickInterval = "-5s"
	if got := daemonMinTickInterval(cfg); got != 0 {
		t.Fatalf("negative config: got %v, want 0 (disabled)", got)
	}

	if got := daemonMinTickInterval(nil); got != config.DefaultMinTickInterval {
		t.Fatalf("nil cfg: got %v, want default %v", got, config.DefaultMinTickInterval)
	}
}
