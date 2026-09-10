package beads

import (
	"errors"
	"testing"
	"time"
)

// resetBDExecHealth clears the process-global tracker. The tests below must
// stay sequential (no t.Parallel) because they swap this shared state.
func resetBDExecHealth() {
	globalBDExecHealth = bdExecHealth{}
}

func TestNoteBDExecOutcomeCountsFailures(t *testing.T) {
	resetBDExecHealth()

	noteBDExecOutcome(errors.New("exit status 1: connection refused"))
	noteBDExecOutcome(errors.New("exit status 1: dolt server unreachable"))
	h := BDExecHealth()
	if h.ConsecutiveFailures != 2 {
		t.Fatalf("ConsecutiveFailures = %d, want 2", h.ConsecutiveFailures)
	}
	if h.LastFailureAt.IsZero() {
		t.Fatalf("LastFailureAt must be stamped on failure")
	}

	// A healthy answer — including bd's "not found" and claim-conflict
	// responses, which are a working store doing its job — resets the ladder.
	noteBDExecOutcome(nil)
	if h := BDExecHealth(); h.ConsecutiveFailures != 0 {
		t.Fatalf("success must reset failures, got %d", h.ConsecutiveFailures)
	}
}

func TestNoteBDExecOutcomeBenignErrorsAreHealthy(t *testing.T) {
	resetBDExecHealth()

	noteBDExecOutcome(errors.New("bd show: no issue found matching abc"))
	noteBDExecOutcome(errors.New("bd update: issue already claimed by other"))
	if h := BDExecHealth(); h.ConsecutiveFailures != 0 {
		t.Fatalf("benign not-found/claim-conflict errors must count as healthy, got failures=%d", h.ConsecutiveFailures)
	}
}

func TestBDExecHealthLastFailureAtTracksMostRecent(t *testing.T) {
	resetBDExecHealth()

	globalBDExecHealth.recordFailure(time.Unix(1000, 0))
	globalBDExecHealth.recordFailure(time.Unix(2000, 0))
	h := BDExecHealth()
	if !h.LastFailureAt.Equal(time.Unix(2000, 0)) {
		t.Fatalf("LastFailureAt = %v, want the most recent failure", h.LastFailureAt)
	}
}
