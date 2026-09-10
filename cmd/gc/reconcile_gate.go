package main

import (
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The controller's reconcile loop can outlive its own patrol interval: a
// demand-phase pass on a contended city measured avg 6.4s / max 40.8s per
// cycle (gastownhall/gascity#2463), and the per-pass ready reads that used to
// dominate measured 3.6s avg on a live rig. While a tick runs, time.Ticker
// buffers exactly one patrol fire, so a tick longer than patrol_interval hands
// the loop an immediate next tick the moment it returns — back-to-back ticks
// with no gap, each one re-spawning the bd subprocess reads the demand path
// performs. reconcileGate turns that into a cadence: a minimum spacing,
// measured from the END of one tick to the START of the next, that applies to
// every tick trigger (patrol, poke, control-dispatcher) alike.
//
// It also owns the loop's failure backoff. The demand reads deliberately
// bypass CachingStore (listOpenForControllerDemandLive), so the cache's own
// circuit breaker never shields them: when bd/Dolt is down, every live read
// fails and the next tick fails again, a tight fail-fast loop of subprocess
// spawns against a dead backend. When a tick observed backing-store failures,
// the gate widens the next spacing exponentially (with jitter) instead —
// capped at the same 10m ceiling CachingStore's breaker uses, so the two
// backoff layers can never disagree by more than the jitter.

const (
	// reconcileGateBackoffBase is the first-order failure backoff added on top
	// of the configured minimum spacing. Doubles per consecutive failing tick,
	// mirroring CachingStore's cacheReconcileBaseBackoff ladder.
	reconcileGateBackoffBase = 2 * time.Second
	// reconcileGateBackoffCap bounds the failure backoff. Equal to CachingStore's
	// cacheReconcileMaxBackoff (caching_store.go): the cache reconciler's
	// circuit breaker and this gate must reach for their worst case together,
	// or whichever recovers first re-spawns reads the other is still cooling.
	reconcileGateBackoffCap = 10 * time.Minute
)

// reconcileGateJitter is the jitter source for the failure backoff. Package
// seam (default: math/rand) so tests can pin the random component and assert
// the exact curve.
var reconcileGateJitter = rand.Float64

// reconcileGateBackoff returns the pre-jitter backoff for a consecutive bd
// failure count. Same ladder shape as CachingStore's nextReconcileDelay
// (base<<failures, capped), so the cache reconciler and the reconcile loop
// widen together as the backend degrades.
func reconcileGateBackoff(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return 0
	}
	backoff := reconcileGateBackoffBase << uint(consecutiveFailures)
	if backoff > reconcileGateBackoffCap || backoff <= 0 {
		return reconcileGateBackoffCap
	}
	return backoff
}

// reconcileGate spaces reconcile ticks apart and coalesces the triggers that
// arrive inside the spacing window. It is deliberately structured like
// tickDebouncer (cap-1 channel + time.AfterFunc) so the run loop selects over
// it exactly as it selects over the debounce fires.
//
// The clock and health probe are injected so the decision logic is unit
// testable without wall-clock sleeps; production uses time.Now and the
// process-wide bd exec health tracker.
type reconcileGate struct {
	mu         sync.Mutex
	now        func() time.Time
	jitter     func() float64
	health     func() beads.BDExecHealthSnapshot
	eligibleAt time.Time // zero before the first tick: nothing to space from
	pending    string
	timer      *time.Timer
	fireCh     chan struct{}
}

// newReconcileGate allocates a gate. now and jitter must be non-nil; health
// may be nil and defaults to the process-wide bd exec tracker.
func newReconcileGate(now func() time.Time, jitter func() float64, health func() beads.BDExecHealthSnapshot) *reconcileGate {
	if now == nil {
		now = time.Now
	}
	if jitter == nil {
		jitter = reconcileGateJitter
	}
	if health == nil {
		health = beads.BDExecHealth
	}
	return &reconcileGate{
		now:     now,
		jitter:  jitter,
		health:  health,
		fireCh:  make(chan struct{}, 1),
	}
}

// trigger registers a tick request. It returns true when the tick may start
// immediately (no spacing or failure backoff is in force). Otherwise the
// trigger is parked — overwriting any previously parked trigger, because the
// eventual single tick re-reads authoritative state and covers every collapsed
// trigger — and a deferred fire is armed for the time the gate opens.
func (g *reconcileGate) trigger(trigger string, minGap time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.eligibleAt.IsZero() || !now.Before(g.eligibleAt) {
		g.eligibleAt = time.Time{} // consumed by the tick that is about to start
		return true
	}
	g.pending = trigger
	if g.timer == nil {
		g.timer = time.AfterFunc(g.eligibleAt.Sub(now), func() {
			g.mu.Lock()
			g.timer = nil
			g.mu.Unlock()
			select {
			case g.fireCh <- struct{}{}:
			default:
			}
		})
	}
	return false
}

// fired is the channel that emits once a parked trigger becomes eligible; the
// loop receives it and starts runTick(take()).
func (g *reconcileGate) fired() <-chan struct{} {
	return g.fireCh
}

// take returns the parked trigger after fired() emitted, clearing the slot.
func (g *reconcileGate) take() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.pending
	g.pending = ""
	return t
}

// cancelPending stops an armed deferred fire and discards a queued one, if
// any. Called on loop shutdown so a parked trigger cannot fire into a dead
// loop; mirrors tickDebouncer.cancelPending.
func (g *reconcileGate) cancelPending() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	select {
	case <-g.fireCh:
	default:
	}
}

// tickCompleted closes the tick that trigger() admitted and computes when the
// next one may start. minGap is the configured minimum spacing; the gate adds
// an exponential, jittered backoff on top when the just-finished tick saw the
// backing store fail (consecutive bd exec failures in the process-wide
// tracker). A healthy tick takes no backoff at all — the gate cadences, it
// never punishes a working loop.
func (g *reconcileGate) tickCompleted(minGap time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := minGap
	if h := g.health(); h.ConsecutiveFailures > 0 {
		backoff := reconcileGateBackoff(h.ConsecutiveFailures)
		// Equal jitter (half fixed, half random): guarantees at least half the
		// computed backoff — a controller that recovers mid-storm cannot
		// re-spawn bd instantly — while de-synchronizing peers that share the
		// dead backend.
		backoff = backoff/2 + time.Duration(g.jitter()*float64(backoff)/2)
		next = minGap + backoff
	}
	g.eligibleAt = g.now().Add(next)
}

// daemonMinTickInterval resolves the effective minimum reconcile-tick spacing:
// GC_MIN_TICK_INTERVAL overrides [daemon].min_tick_interval, read at the loop
// rather than at config load (the same env-over-config shape as
// GC_BACKUP_MAX_AGE_FOR_BULK_DELETE) so a supervisor-managed city can be
// re-tuned without rewriting city.toml. An unparseable value falls back to the
// configured one; a negative one means "disabled" exactly as in config.
func daemonMinTickInterval(cfg *config.City) time.Duration {
	if v := strings.TrimSpace(os.Getenv("GC_MIN_TICK_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			if d < 0 {
				return 0
			}
			return d
		}
	}
	if cfg == nil {
		return config.DefaultMinTickInterval
	}
	return cfg.Daemon.MinTickIntervalDuration()
}
