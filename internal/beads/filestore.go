package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fileData is the on-disk JSON format for the bead store.
type fileData struct {
	Seq   int    `json:"seq"`
	Beads []Bead `json:"beads"`
	Deps  []Dep  `json:"deps,omitempty"`
	// Revisions persists each bead's ConditionalWriter revision out of band,
	// because Bead.Revision is json:"-" and never survives the on-disk []Bead.
	// Without this, every reloadFromDisk (which runs before each write in
	// cross-process flock mode) would reset all revisions to 0, breaking the
	// FileStore's monotonic-never-reused guarantee. Absent (legacy files) ≡ all zero.
	Revisions map[string]int64 `json:"revisions,omitempty"`
	// RevisionsSealed marks a file written by a revisions-aware binary. An
	// OLDER binary's full rewrite drops both this marker and the revisions
	// map while keeping the beads — the exact state in which fresh-from-zero
	// revisions would REUSE previously issued tokens and break the
	// FileStore's monotonic-never-reused guarantee. Loading an unsealed file with beads
	// therefore re-seeds every revision at a deterministic floor far above
	// any counter a prior writer could have issued (see
	// applyBeadRevisionsSealed).
	RevisionsSealed bool `json:"revisions_sealed,omitempty"`
	// Fences persists each bead's ClaimFence out of band, because
	// Bead.ClaimFence is json:"-" and never survives the on-disk []Bead. Without
	// it every reloadFromDisk would reset all fences to 0, so a guarded release
	// holding an older fence could no longer match. Unlike Revisions this needs
	// no sealed/floor re-seed: a fence bumps only on ownership transitions (not
	// on every write), so a legacy binary dropping the map resets fences to 0,
	// which is FAIL-SAFE — a stale guard sees a mismatch and refuses, never
	// wrongly succeeds. Absent (legacy files) ≡ all zero.
	Fences map[string]int64 `json:"fences,omitempty"`
	// ArchiveTombstones (hot file only) lists archived copies that are no
	// longer part of the store — beads deleted while archived, or an earlier
	// incarnation of a reused ID — and may still be on disk until the archive
	// is trimmed; the load merge drops them. See fileArchive.
	ArchiveTombstones []string `json:"archive_tombstones,omitempty"`
	// Archive (hot file only) declares that an archive accompanied this hot
	// file when it was published, so a lock-free reader that then finds the
	// archive trimmed or gone knows to recheck the hot file.
	Archive bool `json:"archive,omitempty"`
	// Order records creation-order keys for beads whose key the ID does not
	// imply (see orderKey); the load merge interleaves hot and archive by it.
	Order map[string][2]int64 `json:"order,omitempty"`
	// ArchiveGen numbers archive rewrites. In the archive it is the file's
	// own generation; in the hot file it is the generation the archive had
	// when the hot file was published, which the trim that follows may bump
	// by one. A cached archive outside those two is stale whatever its mtime
	// and size say.
	ArchiveGen int64 `json:"archive_gen,omitempty"`
}

// beadFences extracts the out-of-band ClaimFence map for persistence. Zero
// fences are omitted (absent ≡ 0 on reload), so legacy files round-trip.
func beadFences(beads []Bead) map[string]int64 {
	fences := make(map[string]int64, len(beads))
	for _, b := range beads {
		if b.ClaimFence != 0 {
			fences[b.ID] = b.ClaimFence
		}
	}
	if len(fences) == 0 {
		return nil
	}
	return fences
}

// applyBeadFences stamps persisted fences back onto beads decoded from disk,
// whose ClaimFence fields are all 0 because of the json:"-" tag. Beads with no
// entry keep fence 0, matching files that predate the fences map.
func applyBeadFences(beads []Bead, fences map[string]int64) {
	if len(fences) == 0 {
		return
	}
	for i := range beads {
		if f, ok := fences[beads[i].ID]; ok {
			beads[i].ClaimFence = f
		}
	}
}

// beadRevisions extracts the out-of-band revision map for persistence. Zero
// revisions are omitted (absent ≡ 0 on reload), so legacy files round-trip.
func beadRevisions(beads []Bead) map[string]int64 {
	revs := make(map[string]int64, len(beads))
	for _, b := range beads {
		if b.Revision != 0 {
			revs[b.ID] = b.Revision
		}
	}
	if len(revs) == 0 {
		return nil
	}
	return revs
}

// applyBeadRevisions stamps persisted revisions back onto beads decoded from
// disk, whose Revision fields are all 0 because of the json:"-" tag. Beads with
// no entry keep revision 0, matching files that predate the revisions map.
func applyBeadRevisions(beads []Bead, revs map[string]int64) {
	if len(revs) == 0 {
		return
	}
	for i := range beads {
		if r, ok := revs[beads[i].ID]; ok {
			beads[i].Revision = r
		}
	}
}

// revisionContinuityFloor is the minimum re-seed value for revisions on an
// unsealed file. Any prior writer issued small per-mutation counters, so a
// floor around 2^40 (≈10^12) can never collide with a previously issued
// token, while staying far below int64 overflow for subsequent +1 bumps.
const revisionContinuityFloor = int64(1) << 40

// applyBeadRevisionsSealed applies the persisted revisions and, when the file
// is UNSEALED but carries beads (a pre-revisions legacy file, or a file an
// older binary rewrote — dropping the revisions map and the seal), re-seeds
// every bead's revision at a deterministic floor derived from the file's own
// timestamps. Determinism matters: the seed must be identical across
// processes and repeated reloads of the same bytes, or the reload-before-
// write path would invent a new revision on every pass and spuriously fail
// in-flight fences. The floor guarantees no previously issued token is ever
// reused; per-bead monotonicity resumes with ordinary +1 bumps from there.
func applyBeadRevisionsSealed(fd *fileData) {
	applyBeadRevisions(fd.Beads, fd.Revisions)
	if fd.RevisionsSealed || len(fd.Beads) == 0 {
		return
	}
	seed := revisionContinuityFloor
	for _, b := range fd.Beads {
		if v := b.UpdatedAt.UnixNano(); !b.UpdatedAt.IsZero() && v > seed {
			seed = v
		}
		if v := b.CreatedAt.UnixNano(); !b.CreatedAt.IsZero() && v > seed {
			seed = v
		}
	}
	for i := range fd.Beads {
		if fd.Beads[i].Revision < seed {
			fd.Beads[i].Revision = seed
		}
	}
}

// FileStore is a file-backed Store implementation. It embeds a MemStore for
// all bead logic and adds JSON persistence — load on open, flush on every
// write. Fine for Tutorial 01 volumes.
type FileStore struct {
	*MemStore
	fmu       sync.Mutex // guards mutate-then-save atomicity
	fs        fsys.FS
	path      string
	locker    Locker // cross-process file lock; nopLocker when unset
	freshness fileFreshness
	archive   *fileArchive // always set; see filestore_archive.go
}

var _ ConditionalAssignmentReleaser = (*FileStore)(nil)

// FileStore answers the edge-payload read through the MemStore it embeds, and
// the answer — no edge here carries a payload — is as honest for the file store
// as it is for the store inside it: FileStore adds JSON persistence around
// MemStore's bead logic and no metadata path of its own, and its DepAdd
// delegates straight through.
//
// Asserted rather than left to method promotion because a caller that cannot
// ask a store treats it as UNABLE TO ANSWER, not as answering no — the
// infra-class migration refuses such a source outright. A refactor that gave
// FileStore its own DepAdd surface without this method would turn a store with
// nothing to lose into a city that cannot cut over, and nothing else in the
// tree would notice.
var _ DepMetadataReader = (*FileStore)(nil)

type fileFreshness struct {
	known   bool
	exists  bool
	size    int64
	modTime time.Time
}

func (f fileFreshness) same(other fileFreshness) bool {
	if !f.known || !other.known {
		return false
	}
	if f.exists != other.exists {
		return false
	}
	if !f.exists {
		return true
	}
	return f.size == other.size && f.modTime.Equal(other.modTime)
}

// FileStoreOption configures a FileStore at open time.
type FileStoreOption func(*FileStore)

// WithFileStoreIDPrefix sets the bead ID prefix this store mints under, mirroring
// WithSQLiteStoreIDPrefix. Without it a file store mints "gc-<n>", so in a
// multi-rig city every rig's file store collides on gc-N and convoy resolution
// cannot uniquely address a bead. A blank/whitespace prefix keeps the default.
func WithFileStoreIDPrefix(prefix string) FileStoreOption {
	return func(s *FileStore) {
		if strings.TrimSpace(prefix) != "" {
			s.IDPrefix = normalizeIDPrefix(prefix)
		}
	}
}

// OpenFileStore opens or creates a file-backed bead store at path. All file
// I/O goes through fs for testability. If the file exists, its contents are
// loaded into memory. If it doesn't exist, the store starts empty. Parent
// directories are created as needed.
func OpenFileStore(fs fsys.FS, path string, opts ...FileStoreOption) (*FileStore, error) {
	if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	locker := Locker(nopLocker{})
	if _, ok := fs.(fsys.OSFS); ok {
		locker = NewFileFlock(path + ".lock")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			store := &FileStore{
				MemStore:  NewMemStore(),
				fs:        fs,
				path:      path,
				locker:    locker,
				freshness: fileFreshness{known: true},
				archive:   newFileArchive(path),
			}
			for _, o := range opts {
				o(store)
			}
			if err := store.mergeArchiveLocked(fileData{}); err != nil {
				return nil, fmt.Errorf("opening file store: %w", err)
			}
			return store, nil
		}
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}
	applyBeadRevisionsSealed(&fd)
	applyBeadFences(fd.Beads, fd.Fences)
	store := &FileStore{
		MemStore: NewMemStoreFrom(fd.Seq, fd.Beads, fd.Deps),
		fs:       fs,
		path:     path,
		locker:   locker,
		archive:  newFileArchive(path),
	}
	for _, o := range opts {
		o(store)
	}
	if err := store.mergeArchiveLocked(fd); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}
	// The JSON we just loaded and the file's current freshness can diverge if
	// another handle rewrites the store between ReadFile and a follow-up Stat.
	// Leave the cache unknown so the first read revalidates against disk.
	store.freshness = fileFreshness{}
	return store, nil
}

// SetLocker sets a cross-process Locker (typically a FileFlock). When set,
// every mutating operation acquires the lock and reloads from disk before
// writing — preventing ID collisions between the CLI and controller daemon.
func (fs *FileStore) SetLocker(l Locker) {
	fs.locker = l
}

// reloadFromDisk re-reads the store file and replaces the in-memory state.
// Must be called with fmu held. Used after acquiring a cross-process flock to
// pick up changes made by other processes since we last read.
func (fs *FileStore) reloadFromDisk() error {
	data, err := fs.fs.ReadFile(fs.path)
	if err != nil {
		if os.IsNotExist(err) {
			// The archive is part of the store even before (or after) the
			// hot file exists: it holds beads and the seq floor.
			if err := fs.archive.load(fs.fs, fileData{}); err != nil {
				return fmt.Errorf("reloading file store: %w", err)
			}
			if len(fs.archive.beads) > 0 {
				return fs.mergeArchiveLocked(fileData{})
			}
			// File hasn't been created yet — keep current in-memory state.
			return nil
		}
		return fmt.Errorf("reloading file store: %w", err)
	}
	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return fmt.Errorf("reloading file store: %w", err)
	}
	applyBeadRevisionsSealed(&fd)
	applyBeadFences(fd.Beads, fd.Fences)
	return fs.mergeArchiveLocked(fd)
}

// mergeArchiveLocked replaces the in-memory state with the hot file content
// fd unioned with the (refreshed) archive. Archived beads are deep-copied:
// MemStore mutates beads in place and the archive cache must keep mirroring
// the file.
func (fs *FileStore) mergeArchiveLocked(fd fileData) error {
	a := fs.archive
	if err := a.load(fs.fs, fd); err != nil {
		return fmt.Errorf("loading file store archive: %w", err)
	}
	archived := make([]Bead, len(a.beads))
	for i, b := range a.beads {
		archived[i] = cloneBead(b)
	}
	seq := fd.Seq
	if a.seq > seq {
		seq = a.seq
	}
	a.expected = fd.Archive
	a.committedSeq = seq
	merged, known := mergeArchive(fd.Beads, fd.Order, archived, a.order, fd.ArchiveTombstones)
	a.known = known
	fs.restoreFrom(seq, merged, fd.Deps)
	return nil
}

// archiveExists reports whether an archive file is on disk (stat only).
func (fs *FileStore) archiveExists() bool {
	cur, err := fs.archive.currentFreshness(fs.fs)
	return err != nil || cur.exists
}

// archiveFresh reports whether the cached archive still matches the file.
func (fs *FileStore) archiveFresh() bool {
	cur, err := fs.archive.currentFreshness(fs.fs)
	return err == nil && fs.archive.freshness.same(cur)
}

func (fs *FileStore) currentFreshness() (fileFreshness, error) {
	fi, err := fs.fs.Stat(fs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileFreshness{known: true}, nil
		}
		return fileFreshness{}, fmt.Errorf("stating file store: %w", err)
	}
	return fileFreshness{
		known:   true,
		exists:  true,
		size:    fi.Size(),
		modTime: fi.ModTime(),
	}, nil
}

func (fs *FileStore) refreshFreshnessCache() {
	current, err := fs.currentFreshness()
	if err != nil {
		fs.freshness = fileFreshness{}
		return
	}
	fs.freshness = current
}

// refreshReadStateLocked favors cross-process correctness for long-lived
// readers, but uses an mtime+size fast path to avoid full JSON reloads on
// every read. The remaining per-read Stat cost is acceptable for now; if
// polling latency becomes measurable, we can replace it with a lighter seq hint.
// Read wrappers intentionally skip the cross-process locker because writers
// publish complete JSON files with temp-file-plus-rename atomic replacement.
func (fs *FileStore) refreshReadStateLocked() error {
	current, err := fs.currentFreshness()
	if err != nil {
		if err := fs.reloadFromDisk(); err != nil {
			return err
		}
		fs.freshness = fileFreshness{}
		return nil
	}
	if fs.freshness.same(current) && fs.archiveFresh() {
		return nil
	}
	if !current.exists && len(fs.archive.ids) == 0 && !fs.archiveExists() {
		fs.restoreFrom(0, nil, nil)
		fs.freshness = current
		return nil
	}
	// Readers skip the writer lock, so with an archive a writer can publish
	// the hot file and then trim (or remove) the archive between our two
	// reads. Whenever an archive was or is in play — cached, found, or
	// declared by the hot file just read — retry until the hot file is
	// unchanged across the archive read.
	for attempt := 0; ; attempt++ {
		hadArchive := fs.archive.inPlay()
		if !current.exists {
			fs.restoreFrom(0, nil, nil)
		}
		if err := fs.reloadFromDisk(); err != nil {
			return err
		}
		if !(hadArchive || fs.archive.inPlay()) {
			break
		}
		after, err := fs.currentFreshness()
		if err == nil && after.same(current) {
			break
		}
		if err != nil || attempt >= 2 {
			// Writers keep landing between our two reads: read once under
			// the writer lock, where the pair cannot change.
			return fs.reloadUnderWriterLockLocked()
		}
		current = after
	}
	fs.freshness = current
	return nil
}

// reloadUnderWriterLockLocked reloads hot file and archive while holding the
// cross-process writer lock, so no save can interleave. Must be called with
// fmu held (the same order writers take the two locks in).
func (fs *FileStore) reloadUnderWriterLockLocked() error {
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	current, statErr := fs.currentFreshness()
	if statErr == nil && !current.exists {
		fs.restoreFrom(0, nil, nil)
	}
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	if statErr != nil {
		current = fileFreshness{}
	}
	fs.freshness = current
	return nil
}

// Create delegates to MemStore.Create and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back to keep
// the MemStore and file in sync.
func (fs *FileStore) Create(b Bead) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return Bead{}, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return Bead{}, err
	}
	snap := fs.snapshotLocked()
	result, err := fs.MemStore.Create(b)
	if err != nil {
		return Bead{}, err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return Bead{}, err
	}
	return result, nil
}

// Update delegates to MemStore.Update and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Update(id string, opts UpdateOpts) error {
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
	if err := fs.MemStore.Update(id, opts); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still
// has the expected assignee, while holding the file-store write lock.
func (fs *FileStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return false, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return false, err
	}
	snap := fs.snapshotLocked()
	released, err := fs.MemStore.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil || !released {
		return released, err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return false, err
	}
	return true, nil
}

// Close delegates to MemStore.Close and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Close(id string) error {
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
	if err := fs.MemStore.Close(id); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// Reopen delegates to MemStore.Reopen and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Reopen(id string) error {
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
	if err := fs.MemStore.Reopen(id); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// Delete delegates to MemStore.Delete and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Delete(id string) error {
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
	if err := fs.MemStore.Delete(id); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata, then flushes once.
func (fs *FileStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return 0, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return 0, err
	}
	snap := fs.snapshotLocked()
	closed, err := fs.MemStore.CloseAll(ids, metadata)
	if err != nil {
		return 0, err
	}
	if closed > 0 {
		if err := fs.save(); err != nil {
			fs.restoreFrom(snap.seq, snap.beads, snap.deps)
			return 0, err
		}
	}
	return closed, nil
}

// SetMetadata delegates to MemStore.SetMetadata and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) SetMetadata(id, key, value string) error {
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
	if err := fs.MemStore.SetMetadata(id, key, value); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// SetMetadataBatch delegates to MemStore.SetMetadataBatch and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) SetMetadataBatch(id string, kvs map[string]string) error {
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
	if err := fs.MemStore.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// Tx executes fn sequentially against the FileStore.
func (fs *FileStore) Tx(_ string, fn func(Tx) error) error {
	return runSequentialTx(fs, fn)
}

// Get reloads the on-disk store before reading a bead by ID.
func (fs *FileStore) Get(id string) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return Bead{}, err
	}
	return fs.MemStore.Get(id)
}

// List reloads the on-disk store before listing beads that match the query.
func (fs *FileStore) List(query ListQuery) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.List(query)
}

// ListOpen reloads the on-disk store before listing open beads.
func (fs *FileStore) ListOpen(status ...string) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListOpen(status...)
}

// Ready reloads the on-disk store before listing ready beads.
func (fs *FileStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.Ready(query...)
}

// ReadyContext vetoes ContextReadyReader for FileStore. Refreshing the JSON
// file is context-blind, so the promoted MemStore method would falsely promise
// cancellation and would skip the required on-disk refresh entirely.
func (fs *FileStore) ReadyContext(ctx context.Context, _ ...ReadyQuery) ([]Bead, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("reading ready beads from file store: %w", ErrReadyContextUnsupported)
}

// Children reloads the on-disk store before listing child beads.
func (fs *FileStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.Children(parentID, opts...)
}

// ListByLabel reloads the on-disk store before listing beads for a label.
func (fs *FileStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListByLabel(label, limit, opts...)
}

// ListByAssignee reloads the on-disk store before listing beads for an assignee.
func (fs *FileStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListByAssignee(assignee, status, limit)
}

// ListByMetadata reloads the on-disk store before listing beads by metadata.
func (fs *FileStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListByMetadata(filters, limit, opts...)
}

// Ping checks that the store file is accessible.
func (fs *FileStore) Ping() error {
	if err := fs.MemStore.Ping(); err != nil {
		return err
	}
	if _, err := fs.fs.ReadFile(fs.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pinging file store: %w", err)
	}
	return nil
}

// DepAdd delegates to MemStore.DepAdd and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) DepAdd(issueID, dependsOnID, depType string) error {
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
	if err := fs.MemStore.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// DepRemove delegates to MemStore.DepRemove and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) DepRemove(issueID, dependsOnID string) error {
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
	if err := fs.MemStore.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// DepList reloads the on-disk store before listing dependencies.
func (fs *FileStore) DepList(id, direction string) ([]Dep, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.DepList(id, direction)
}

// DepListBatch reloads the on-disk store before listing batched dependencies.
func (fs *FileStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.DepListBatch(ids)
}

// memSnapshot holds a snapshot of MemStore state for rollback.
type memSnapshot struct {
	seq   int
	beads []Bead
	deps  []Dep
}

// snapshotLocked takes a snapshot of MemStore state for rollback.
// Must be called with fmu held.
func (fs *FileStore) snapshotLocked() memSnapshot {
	fs.mu.Lock()
	seq, beads, deps := fs.snapshot()
	fs.mu.Unlock()
	return memSnapshot{seq: seq, beads: beads, deps: deps}
}

// save writes the full store state to disk atomically (temp file + rename).
// Called with fmu held, so snapshot under MemStore.mu then release before I/O.
func (fs *FileStore) save() error {
	fs.mu.Lock()
	seq, beads, deps := fs.snapshot()
	fs.mu.Unlock()

	if fs.archive.mode == archiveOn || fs.archive.inPlay() {
		err := fs.saveWithArchive(seq, beads, deps)
		fs.refreshFreshnessCache()
		return err
	}
	// ArchiveGen survives archive-less stretches: a later archive must not
	// reuse a generation some other handle still has cached.
	fd := fileData{Seq: seq, Beads: beads, Deps: deps, Revisions: beadRevisions(beads), RevisionsSealed: true, Fences: beadFences(beads), ArchiveGen: fs.archive.gen}
	if err := writeStoreFile(fs.fs, fs.path, fd); err != nil {
		return err
	}
	fs.refreshFreshnessCache()
	return nil
}
