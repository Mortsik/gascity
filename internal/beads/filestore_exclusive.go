package beads

import "errors"

var _ ExclusiveMutationStore = (*FileStore)(nil)

// WithExclusiveMutation runs fn while holding the FileStore's in-process and
// cross-process writer locks. It reloads the authoritative file after the lock
// is acquired, exactly like every ordinary FileStore mutator, then gives fn the
// embedded MemStore so callback writes cannot recursively acquire the FileStore
// locks. Successful callback writes are flushed once; callback/save failures
// restore the in-memory snapshot and publish no partial store mutation.
func (fs *FileStore) WithExclusiveMutation(fn func(Store) error) error {
	if fn == nil {
		return errors.New("exclusive mutation: nil callback")
	}
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}

	snap := fs.snapshotLocked()
	if err := fn(fs.MemStore); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}
