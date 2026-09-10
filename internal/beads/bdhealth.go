package beads

import (
	"sync"
	"time"
)

// bdExecHealth tracks, process-wide, the outcome of every bd subprocess the
// stores exec. It exists so the controller's reconcile loop can back off when
// the backing store is failing instead of re-spawning bd at tick cadence: the
// demand reads deliberately bypass CachingStore (see listOpenForControllerDemand
// Live callers), so the cache's own circuit breaker and backoff never apply to
// them — this tracker is the only signal those paths feed.
//
// Process-global rather than per-BdStore on purpose: a controller hosts several
// stores over one bead ledger, and the failure that matters (managed Dolt down,
// disk stall, bd binary missing) strikes every store at once. The same
// single-process scope is already established by the reconcilerTickTrigger
// attribution variable (bdtrace.go).
type bdExecHealth struct {
	mu       sync.Mutex
	failures int
	lastFail time.Time
}

var globalBDExecHealth bdExecHealth

// BDExecHealthSnapshot is a point-in-time read of the bd subprocess health
// tracker. ConsecutiveFailures is 0 when the most recent bd invocation
// succeeded; otherwise it is the count of failures since the last success.
// LastFailureAt is the time of the most recent failure (zero when none).
type BDExecHealthSnapshot struct {
	ConsecutiveFailures int
	LastFailureAt       time.Time
}

// BDExecHealth returns the current process-wide bd subprocess health.
func BDExecHealth() BDExecHealthSnapshot {
	return globalBDExecHealth.snapshot(time.Now())
}

func (h *bdExecHealth) snapshot(now time.Time) BDExecHealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return BDExecHealthSnapshot{
		ConsecutiveFailures: h.failures,
		LastFailureAt:       h.lastFail,
	}
}

// noteBDExecOutcome records one completed bd invocation from the exec
// chokepoint. "Not found" and claim-conflict errors are healthy answers from a
// working store (bd did its job and reported an absence / a lost race), so they
// count as successes: treating them as failures would make a controller that
// simply has no matching work back off as if its store were down.
func noteBDExecOutcome(err error) {
	if err != nil && !isBdNotFound(err) && !isBdClaimConflictMessage(err.Error()) {
		globalBDExecHealth.recordFailure(time.Now())
		return
	}
	globalBDExecHealth.recordSuccess(time.Now())
}

func (h *bdExecHealth) recordFailure(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures++
	h.lastFail = now
}

func (h *bdExecHealth) recordSuccess(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures = 0
}
