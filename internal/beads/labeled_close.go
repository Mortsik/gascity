package beads

import "errors"

// ErrLabeledCloseUnsupported reports that a store (typically a wrapper whose
// inner store lacks the capability) cannot close with labels in one write.
// Callers fall back to Update followed by CloseAll.
var ErrLabeledCloseUnsupported = errors.New("labeled close unsupported")

// LabeledCloser is an optional Store capability: close beads, appending labels
// and setting metadata on each, as one write.
//
// A caller that stamps an outcome label and then closes the bead pays two
// writes for one transition; on a whole-file store each write rewrites the
// entire file. A Store that implements LabeledCloser collapses the pair.
// Callers holding a plain Store fall back to Update followed by CloseAll.
type LabeledCloser interface {
	// CloseAllWithLabels closes the open beads among ids, appending each label
	// a bead does not already carry and setting metadata on it — the result of
	// Update(Labels) followed by CloseAll(metadata), except that labels are not
	// duplicated and only the close is announced (no separate bead.updated) —
	// and returns how many beads it closed. Beads already closed and absent ids
	// are skipped.
	CloseAllWithLabels(ids, labels []string, metadata map[string]string) (int, error)
}
