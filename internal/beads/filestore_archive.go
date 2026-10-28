package beads

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A FileStore rewrites its whole JSON file on every mutation, so the file
// size is the write volume. Long-lived cities accumulate closed beads that are
// never touched again (measured on a 65k-bead city: 85% closed, ~90 MB file,
// ~8 rewrites/min). With archiving on, closed beads whose last update is older
// than the retention live in a sibling archive file that is rewritten only
// when the archived set changes; the hot file keeps everything else.
//
// The in-memory MemStore still holds the union, so every query answers
// exactly as before — only the on-disk layout changes. Invariants:
//
//   - The archive is read whenever it exists, whatever the options say. The
//     options only decide what a save does with it: WithFileStoreArchive(d)
//     archives closed beads idle for d; WithFileStoreArchive(0) folds every
//     archived bead back into the hot file and removes the archive; no option
//     (a handle that could not load the config, say) keeps the archived set as
//     it is. No setting hides beads, and only an explicit "off" pays the cost
//     of rewriting the archive into the hot file.
//   - Every mutation bumps a bead's Revision and UpdatedAt, so a mutated
//     archived bead is no longer "old" (nor unchanged) and moves back to the
//     hot file.
//   - A bead's incarnation is its ID plus CreatedAt: pinned IDs (guarded
//     batches) can be deleted and created again, and the new incarnation
//     restarts its Revision. Copies of one incarnation merge by the higher
//     Revision, the hot copy on a tie; a hot copy of another incarnation
//     always wins, and archived copies the hot file tombstones (deleted, or
//     an earlier incarnation of a reused ID) are dropped.
//   - A save that changes the archived set never removes anything from the
//     archive before the hot file is published: (1) the archive grows to a
//     superset (newly archived beads plus the on-disk copy of everything it
//     held), (2) the hot file is published — this is the commit point; it
//     tombstones deleted archived beads — (3) the archive is trimmed. A
//     failure before (2) leaves the previous state; a failure in (3) is
//     logged and retried by the next save, never reported as a failed
//     mutation that in fact committed.
//   - The hot file declares whether an archive accompanied it (Archive), so
//     a reader that skips the writer lock knows to recheck the hot file when
//     the archive it then finds was already trimmed or removed.
//   - Seq and deps stay in the hot file, which is all a non-archiving
//     (older) binary reads: it keeps minting fresh IDs and never touches the
//     archive, whose beads reappear once an archiving binary opens the store.
//     Rolling back to such a binary is still lossy at the edges: it drops the
//     fields it does not know, so a delete whose archive trim had not yet
//     happened can resurrect, beads it deletes that also sit in the archive
//     come back, a pinned ID it re-creates (it cannot see the archived one)
//     replaces the archived bead for good, and beads without a numeric ID
//     suffix may move in the default order. Before a rollback fold the
//     archive back: set "off", restart long-running gc processes (they keep
//     the setting they started with and would archive again), let a write
//     happen and confirm the archive file is gone — a failed removal is only
//     logged.
//
// The cutoff moves in steps (retention/4), so a steady trickle of closes does
// not rewrite the archive on every save.
type fileArchive struct {
	path  string
	mode  archiveMode
	after time.Duration
	step  time.Duration

	// Last known on-disk archive content (loaded or written by this handle).
	// The cache is trusted only when its mtime+size still match AND its
	// generation is one the hot file just read allows: mtime+size alone miss
	// a same-size replacement within one mtime tick, and a writer acting on
	// such a stale archive would drop beads for good.
	freshness fileFreshness
	gen       int64
	seq       int
	beads     []Bead
	ids       map[string]beadIdent
	order     map[string][2]int64

	// expected: the hot file last read or written declared an archive.
	expected bool
	// known: the recorded order-key exceptions of both files (see orderKey).
	known map[string][2]int64
	// committedSeq: the seq of the store as last loaded or committed. Phase 1
	// of a save publishes the archive before the mutation commits, so it may
	// only carry this floor, never the mutation's own seq.
	committedSeq int
}

// beadIdent pins one stored copy: which incarnation, at which revision.
type beadIdent struct {
	rev     int64
	created time.Time
}

func identOf(b Bead) beadIdent { return beadIdent{rev: b.Revision, created: b.CreatedAt} }

func (i beadIdent) sameIncarnation(b Bead) bool { return i.created.Equal(b.CreatedAt) }

func (i beadIdent) sameCopy(b Bead) bool { return i.rev == b.Revision && i.sameIncarnation(b) }

func beadIdents(beads []Bead) map[string]beadIdent {
	if len(beads) == 0 {
		return nil
	}
	ids := make(map[string]beadIdent, len(beads))
	for _, b := range beads {
		ids[b.ID] = identOf(b)
	}
	return ids
}

type archiveMode int

const (
	archiveKeep archiveMode = iota // no option: leave the archived set as it is
	archiveOn                      // archive closed beads idle past the retention
	archiveFold                    // fold everything back into the hot file
)

func newFileArchive(storePath string) *fileArchive {
	return &fileArchive{path: archivePathFor(storePath)}
}

// WithFileStoreArchive moves closed beads not updated for longer than after
// into "<name>.archive.json" next to the store file. Zero or negative after
// folds an existing archive back into the store file on the next save.
// Without this option a store keeps whatever is archived as it is.
func WithFileStoreArchive(after time.Duration) FileStoreOption {
	return func(s *FileStore) {
		if after <= 0 {
			s.archive.mode = archiveFold
			return
		}
		step := after / 4
		if step <= 0 {
			step = after
		}
		s.archive.mode, s.archive.after, s.archive.step = archiveOn, after, step
	}
}

func archivePathFor(path string) string {
	if strings.HasSuffix(path, ".json") {
		return strings.TrimSuffix(path, ".json") + ".archive.json"
	}
	return path + ".archive"
}

// inPlay reports whether an archive may hold part of the store.
func (a *fileArchive) inPlay() bool {
	return len(a.ids) > 0 || a.expected
}

func (a *fileArchive) archivable(b Bead, now time.Time) bool {
	switch a.mode {
	case archiveOn:
		if b.Status != "closed" {
			return false
		}
		last := b.UpdatedAt
		if last.IsZero() {
			last = b.CreatedAt
		}
		return last.Before(now.Add(-a.after).Truncate(a.step))
	case archiveKeep:
		id, ok := a.ids[b.ID]
		return ok && id.sameCopy(b)
	default:
		return false
	}
}

func (a *fileArchive) currentFreshness(fs fsys.FS) (fileFreshness, error) {
	fi, err := fs.Stat(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileFreshness{known: true}, nil
		}
		return fileFreshness{}, fmt.Errorf("stating file store archive: %w", err)
	}
	return fileFreshness{known: true, exists: true, size: fi.Size(), modTime: fi.ModTime()}, nil
}

func (a *fileArchive) setCache(seq int, beads []Bead, order map[string][2]int64) {
	a.seq, a.beads, a.ids, a.order = seq, beads, beadIdents(beads), order
	if len(beads) == 0 {
		a.order = nil
	}
}

// cacheFits reports whether the cached archive is one the hot file hot
// allows (see fileData.ArchiveGen). A hot file without a generation (no
// archive yet, or written by an older binary that never touches the archive)
// cannot tell, and mtime+size decide alone.
func (a *fileArchive) cacheFits(hot fileData) bool {
	switch {
	case hot.Archive:
		return a.gen == hot.ArchiveGen || a.gen == hot.ArchiveGen+1
	case hot.ArchiveGen > 0:
		return len(a.ids) == 0 // published with no archive at all
	default:
		return true
	}
}

// load refreshes the cached archive content when the file changed on disk or
// the cache does not fit the hot file just read. Stat before read: a
// replacement racing the read leaves the cached freshness older than the
// content, which only costs one more reload later.
func (a *fileArchive) load(fs fsys.FS, hot fileData) error {
	cur, err := a.currentFreshness(fs)
	if err != nil {
		return err
	}
	if a.freshness.same(cur) && a.cacheFits(hot) {
		return nil
	}
	if !cur.exists {
		a.setCache(0, nil, nil)
		a.gen = max(a.gen, hot.ArchiveGen) // generations never repeat
		a.freshness = cur
		return nil
	}
	data, err := fs.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			a.setCache(0, nil, nil)
			a.gen = max(a.gen, hot.ArchiveGen)
			a.freshness = fileFreshness{known: true}
			return nil
		}
		return fmt.Errorf("reading file store archive: %w", err)
	}
	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return fmt.Errorf("reading file store archive: %w", err)
	}
	applyBeadRevisionsSealed(&fd)
	applyBeadFences(fd.Beads, fd.Fences)
	a.setCache(fd.Seq, fd.Beads, fd.Order)
	a.gen = fd.ArchiveGen
	a.freshness = cur
	return nil
}

// write publishes beads (sorted into creation order) as archive generation
// gen; an empty set removes the file.
func (a *fileArchive) write(fs fsys.FS, gen int64, seq int, beads []Bead, order map[string][2]int64) error {
	sortByOrderKey(beads, order)
	if len(beads) == 0 {
		if err := fs.Remove(a.path); err != nil && !os.IsNotExist(err) {
			a.freshness = fileFreshness{}
			return fmt.Errorf("removing file store archive: %w", err)
		}
	} else {
		fd := fileData{Seq: seq, Beads: beads, Revisions: beadRevisions(beads), RevisionsSealed: true, Fences: beadFences(beads), Order: order, ArchiveGen: gen}
		if err := writeStoreFile(fs, a.path, fd); err != nil {
			a.freshness = fileFreshness{}
			return fmt.Errorf("saving file store archive: %w", err)
		}
	}
	a.setCache(seq, beads, order)
	a.gen = gen
	if cur, err := a.currentFreshness(fs); err == nil {
		a.freshness = cur
	} else {
		a.freshness = fileFreshness{}
	}
	return nil
}

// sameSet reports whether beads is exactly the archived set already on disk.
func (a *fileArchive) sameSet(beads []Bead) bool {
	if len(beads) != len(a.ids) {
		return false
	}
	for _, b := range beads {
		if id, ok := a.ids[b.ID]; !ok || !id.sameCopy(b) {
			return false
		}
	}
	return true
}

// orderKey places a bead in the store's creation order, which is the slice
// order MemStore lists by. The hot file and the archive are each written in
// that order but rewritten at different times, so the merge needs a key
// stable across saves. For a store that mints IDs in sequence the key is the
// numeric ID suffix (seq is shared across prefixes); beads out of sequence —
// explicit or pinned IDs, IDs without a numeric suffix — get the running
// maximum key before them plus their offset after it when first saved, and
// only those exceptions are written (fileData.Order). A recorded key is kept
// for the bead's lifetime: recomputing it after a preceding bead is deleted
// would disagree with the copy in the file that was not rewritten.
type orderKey struct{ k, sub int64 }

func (o orderKey) less(p orderKey) bool {
	return o.k < p.k || (o.k == p.k && o.sub < p.sub)
}

func seqSuffix(id string) (int64, bool) {
	if i := strings.LastIndexByte(id, '-'); i >= 0 {
		if n, err := strconv.ParseInt(id[i+1:], 10, 64); err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}

// derivedOrderKey is the key of a bead with no recorded exception. Beads
// written without keys (older binaries) and lacking a numeric suffix sort
// last.
func derivedOrderKey(id string) orderKey {
	if n, ok := seqSuffix(id); ok {
		return orderKey{k: n}
	}
	return orderKey{k: math.MaxInt64}
}

func orderKeyOf(id string, order map[string][2]int64) orderKey {
	if v, ok := order[id]; ok {
		return orderKey{k: v[0], sub: v[1]}
	}
	return derivedOrderKey(id)
}

// creationOrderKeys assigns keys to a full store snapshot in its order,
// keeping every recorded key in known.
func creationOrderKeys(all []Bead, known map[string][2]int64) []orderKey {
	keys := make([]orderKey, len(all))
	cur := orderKey{k: -1, sub: -1}
	for i, b := range all {
		if v, ok := known[b.ID]; ok {
			if k := (orderKey{k: v[0], sub: v[1]}); cur.less(k) {
				cur = k
			}
			keys[i] = orderKey{k: v[0], sub: v[1]}
			continue
		}
		if n, ok := seqSuffix(b.ID); ok && n > cur.k {
			cur = orderKey{k: n}
		} else {
			cur.sub++
		}
		keys[i] = cur
	}
	return keys
}

// knownOrderKeys unions the recorded exceptions of the hot file and archive.
func knownOrderKeys(maps ...map[string][2]int64) map[string][2]int64 {
	var out map[string][2]int64
	for _, m := range maps {
		for id, v := range m {
			if out == nil {
				out = make(map[string][2]int64)
			}
			out[id] = v
		}
	}
	return out
}

// putOrderKey records key for id unless the ID already implies it.
func putOrderKey(order map[string][2]int64, id string, key orderKey) map[string][2]int64 {
	if key == derivedOrderKey(id) {
		return order
	}
	if order == nil {
		order = make(map[string][2]int64)
	}
	order[id] = [2]int64{key.k, key.sub}
	return order
}

func sortByOrderKey(beads []Bead, order map[string][2]int64) {
	sort.SliceStable(beads, func(i, j int) bool {
		return orderKeyOf(beads[i].ID, order).less(orderKeyOf(beads[j].ID, order))
	})
}

// mergeArchive folds archived beads into the hot list. The hot list's order
// is authoritative and kept as is; each archived bead is placed before the
// first hot bead with a larger order key, which restores the original order.
// Archived beads the hot file tombstones, holds as another incarnation, or
// holds at an equal or higher revision, are dropped. It also returns the
// order keys on record for the merged store: the hot file's, plus the
// archive's for the archived beads that made it in — never a dropped
// incarnation's, which a reused ID must not inherit.
func mergeArchive(hot []Bead, hotOrder map[string][2]int64, archived []Bead, archOrder map[string][2]int64, tombstones []string) ([]Bead, map[string][2]int64) {
	known := knownOrderKeys(hotOrder)
	if len(archived) == 0 {
		return hot, known
	}
	dead := make(map[string]bool, len(tombstones))
	for _, id := range tombstones {
		dead[id] = true
	}
	pos := make(map[string]int, len(hot))
	for i, b := range hot {
		pos[b.ID] = i
	}
	rest := make([]Bead, 0, len(archived))
	for _, b := range archived {
		if dead[b.ID] {
			continue
		}
		if i, ok := pos[b.ID]; ok {
			if identOf(b).sameIncarnation(hot[i]) && b.Revision > hot[i].Revision {
				hot[i] = b
			}
			continue
		}
		rest = append(rest, b)
		if v, ok := archOrder[b.ID]; ok {
			known = putOrderKey(known, b.ID, orderKey{k: v[0], sub: v[1]})
		}
	}
	sortByOrderKey(rest, archOrder)
	out := make([]Bead, 0, len(hot)+len(rest))
	i, j := 0, 0
	for i < len(hot) && j < len(rest) {
		if orderKeyOf(rest[j].ID, archOrder).less(orderKeyOf(hot[i].ID, hotOrder)) {
			out = append(out, rest[j])
			j++
		} else {
			out = append(out, hot[i])
			i++
		}
	}
	out = append(out, hot[i:]...)
	return append(out, rest[j:]...), known
}

// saveWithArchive persists a snapshot as hot + archive (see fileArchive).
// Called with fmu (and, cross-process, the store lock) held, after the
// pre-mutation reload refreshed the archive cache.
func (fs *FileStore) saveWithArchive(seq int, all []Bead, deps []Dep) error {
	a := fs.archive
	now := time.Now()
	keys := creationOrderKeys(all, a.known)
	var hot, arch []Bead
	var hotOrder, archOrder map[string][2]int64
	archSet := make(map[string]bool)
	snap := make(map[string]beadIdent, len(all))
	for i, b := range all {
		snap[b.ID] = identOf(b)
		if a.archivable(b, now) {
			arch = append(arch, b)
			archOrder = putOrderKey(archOrder, b.ID, keys[i])
			archSet[b.ID] = true
		} else {
			hot = append(hot, b)
			hotOrder = putOrderKey(hotOrder, b.ID, keys[i])
		}
	}

	// Phase 1: grow the archive to a superset. Every bead it holds on disk
	// stays (as its on-disk copy) until the hot file below is published:
	// beads leaving for the hot file must not vanish if that write fails, and
	// deleted beads must not disappear before the delete commits.
	super := append([]Bead(nil), arch...)
	var superOrder map[string][2]int64
	for id, v := range archOrder {
		superOrder = putOrderKey(superOrder, id, orderKey{k: v[0], sub: v[1]})
	}
	for _, b := range a.beads {
		if !archSet[b.ID] {
			super = append(super, b)
			superOrder = putOrderKey(superOrder, b.ID, orderKeyOf(b.ID, a.order))
		}
	}
	if !a.sameSet(super) {
		floor := a.committedSeq
		if seq < floor {
			floor = seq
		}
		if err := a.write(fs.fs, a.gen+1, floor, super, superOrder); err != nil {
			return err
		}
	}

	// Phase 2 (commit point): the hot file, tombstoning archived copies the
	// store no longer holds so the superset cannot resurrect them: deleted
	// beads and earlier incarnations of a reused ID. (A store copy at a lower
	// revision than the archived one cannot be the same bead either —
	// revisions only grow.)
	var tombstones []string
	for _, b := range a.beads {
		if cur, ok := snap[b.ID]; !ok || !cur.sameIncarnation(b) || cur.rev < b.Revision {
			tombstones = append(tombstones, b.ID)
		}
	}
	hotFD := fileData{
		Seq: seq, Beads: hot, Deps: deps,
		Revisions: beadRevisions(hot), RevisionsSealed: true, Fences: beadFences(hot),
		ArchiveTombstones: tombstones, Archive: len(super) > 0, Order: hotOrder,
		ArchiveGen: a.gen,
	}
	if err := writeStoreFile(fs.fs, fs.path, hotFD); err != nil {
		return err
	}
	a.expected = hotFD.Archive
	a.known = knownOrderKeys(hotOrder, archOrder)
	a.committedSeq = seq

	// Phase 3: trim. The mutation is already durable; a failure here leaves a
	// superset the merge reads correctly and the next save trims again.
	if !a.sameSet(arch) {
		if err := a.write(fs.fs, hotFD.ArchiveGen+1, seq, arch, archOrder); err != nil {
			log.Printf("beads file store: archive trim deferred to the next save: %v", err)
		}
	}
	return nil
}

// writeStoreFile publishes fd at path atomically (temp file + rename).
// Compact, not indented: the file size is the write volume.
func writeStoreFile(fs fsys.FS, path string, fd fileData) error {
	data, err := json.Marshal(fd)
	if err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	tmp := path + ".tmp"
	if err := fs.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	if err := fs.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	return nil
}
