package beads

import "errors"

// ErrExclusiveMutationUnsupported reports that a store cannot serialize a
// caller-owned read/effect/write decision against every ordinary writer. A
// caller that needs this capability must fail closed rather than degrade to a
// read-check-write sequence with a race window.
var ErrExclusiveMutationUnsupported = errors.New("exclusive mutation unsupported")

// ExclusiveMutationStore is an optional capability for operations whose
// correctness depends on keeping an observed precondition stable while an
// external side effect and its durable write are performed. The callback gets
// a Store view that is already inside the backend's writer critical section;
// it must use that view for every store access until it returns.
//
// A callback error commits no store mutation. Implementations must serialize
// the whole callback against the same writer authority used by their ordinary
// mutating methods.
type ExclusiveMutationStore interface {
	WithExclusiveMutation(func(Store) error) error
}

// ExclusiveMutationFor resolves wrapper-declared backing stores and returns an
// exclusive mutation capability only when the concrete backend can prove it.
func ExclusiveMutationFor(store Store) (ExclusiveMutationStore, bool) {
	if store == nil {
		return nil, false
	}
	store = followConditionalWritesResolveTarget(store)
	mutator, ok := store.(ExclusiveMutationStore)
	return mutator, ok
}
