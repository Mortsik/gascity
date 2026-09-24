package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// StateFenceCode is the stable machine-readable refusal reason for a fenced
// session mutation.
type StateFenceCode string

const (
	// StateFenceMismatch means the target exists but its current persisted
	// lifecycle state no longer equals the caller's expectation.
	StateFenceMismatch StateFenceCode = "state-mismatch"
	// StateFenceGone means the target is absent or already terminal/closed.
	StateFenceGone StateFenceCode = "state-gone"
	// StateFenceUnsupported means the resolved backend cannot prove that the
	// state check and mutation are serialized against ordinary writers.
	StateFenceUnsupported StateFenceCode = "state-fence-unsupported"
)

// StateFenceError reports a zero-mutation refusal from an --if-state style
// lifecycle operation. Code is stable machine vocabulary; Error remains useful
// for human stderr without requiring callers to parse it.
type StateFenceError struct {
	Code     StateFenceCode
	ID       string
	Expected State
	Actual   State
}

func (e *StateFenceError) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch e.Code {
	case StateFenceMismatch:
		return fmt.Sprintf("%s: session %s is %s, expected %s", e.Code, e.ID, displayFenceState(e.Actual), displayFenceState(e.Expected))
	case StateFenceGone:
		return fmt.Sprintf("%s: session %s expected %s", e.Code, e.ID, displayFenceState(e.Expected))
	case StateFenceUnsupported:
		return fmt.Sprintf("%s: session %s expected %s", e.Code, e.ID, displayFenceState(e.Expected))
	default:
		return fmt.Sprintf("session state fence refused for %s", e.ID)
	}
}

func displayFenceState(state State) string {
	if state == "" {
		return "<empty>"
	}
	return string(state)
}

// WithExpectedStateMutation runs mutate only when the target's authoritative
// persisted lifecycle state still equals expected. The check and the callback
// execute inside a backend writer critical section, so another ordinary writer
// cannot change the row between them. Unsupported backends fail closed.
//
// mutate must use the Store passed to it for all reads and writes; using the
// outer store would recursively acquire the backend lock and can deadlock.
func WithExpectedStateMutation(store beads.Store, id string, expected State, mutate func(beads.Store) error) error {
	expected = State(strings.TrimSpace(string(expected)))
	if expected == "" {
		return fmt.Errorf("state fence: expected state must not be empty")
	}
	if mutate == nil {
		return fmt.Errorf("state fence: nil mutation callback")
	}

	mutator, ok := beads.ExclusiveMutationFor(store)
	if !ok {
		return &StateFenceError{Code: StateFenceUnsupported, ID: id, Expected: expected}
	}
	return mutator.WithExclusiveMutation(func(locked beads.Store) error {
		b, err := locked.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return &StateFenceError{Code: StateFenceGone, ID: id, Expected: expected}
			}
			return err
		}
		actual := State(strings.TrimSpace(b.Metadata["state"]))
		if b.Status == "closed" || actual == StateClosed {
			return &StateFenceError{Code: StateFenceGone, ID: id, Expected: expected, Actual: actual}
		}
		if actual != expected {
			return &StateFenceError{Code: StateFenceMismatch, ID: id, Expected: expected, Actual: actual}
		}
		return mutate(locked)
	})
}
