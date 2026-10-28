package beads_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// readStoreFile decodes one on-disk store file (hot or archive) into its bead
// IDs. A missing file is an empty set (an emptied archive is removed).
func readStoreFile(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]bool{}
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fd struct {
		Beads []beads.Bead `json:"beads"`
	}
	if err := json.Unmarshal(raw, &fd); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	ids := make(map[string]bool, len(fd.Beads))
	for _, b := range fd.Beads {
		ids[b.ID] = true
	}
	return ids
}

const testArchiveAfter = time.Hour

// archiveEnv is one store directory.
type archiveEnv struct {
	t           *testing.T
	path        string
	archivePath string
}

func newArchiveEnv(t *testing.T) *archiveEnv {
	t.Helper()
	dir := t.TempDir()
	return &archiveEnv{t: t, path: filepath.Join(dir, "beads.json"), archivePath: filepath.Join(dir, "beads.archive.json")}
}

// ageOut puts every bead so far past the retention cutoff (cutoff = now -
// after, truncated to after/4) and saves through s, which archives them when
// s archives.
func (e *archiveEnv) ageOut(s *beads.FileStore) {
	e.t.Helper()
	if err := beads.AgeFileStoreBeadsForTest(s, 2*testArchiveAfter); err != nil {
		e.t.Fatal(err)
	}
}

func (e *archiveEnv) openFS(fs fsys.FS, opts ...beads.FileStoreOption) *beads.FileStore {
	e.t.Helper()
	s, err := beads.OpenFileStore(fs, e.path, opts...)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// archived opens a handle with archiving on; plain opens one without options.
func (e *archiveEnv) archived() *beads.FileStore {
	e.t.Helper()
	return e.openFS(fsys.OSFS{}, beads.WithFileStoreArchive(testArchiveAfter))
}

func (e *archiveEnv) plain() *beads.FileStore {
	e.t.Helper()
	return e.openFS(fsys.OSFS{})
}

func (e *archiveEnv) hot() map[string]bool     { return readStoreFile(e.t, e.path) }
func (e *archiveEnv) archive() map[string]bool { return readStoreFile(e.t, e.archivePath) }

func (e *archiveEnv) archiveStat() os.FileInfo {
	e.t.Helper()
	fi, err := os.Stat(e.archivePath)
	if err != nil {
		e.t.Fatal(err)
	}
	return fi
}

// seed builds a store with n beads, closes those in closeIdx and ages them
// out, which archives them. It returns the created IDs.
func (e *archiveEnv) seed(n int, closeIdx ...int) []string {
	e.t.Helper()
	s := e.archived()
	var ids []string
	for i := 0; i < n; i++ {
		b, err := s.Create(beads.Bead{Title: "b"})
		if err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	for _, i := range closeIdx {
		if err := s.Close(ids[i]); err != nil {
			e.t.Fatal(err)
		}
	}
	e.ageOut(s)
	for _, i := range closeIdx {
		if !e.archive()[ids[i]] {
			e.t.Fatalf("precondition: %s not archived", ids[i])
		}
	}
	return ids
}

func listIDs(t *testing.T, s *beads.FileStore) []string {
	t.Helper()
	all, err := s.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range all {
		ids = append(ids, b.ID)
	}
	return ids
}

func countID(ids []string, id string) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}

// The hot file is rewritten on every mutation, so closed beads past the
// retention move to a separate archive that is rewritten only when the
// archived set changes — while the store keeps answering for all of them.
func TestFileStoreArchiveSplitsOldClosedBeads(t *testing.T) {
	e := newArchiveEnv(t)
	s := e.archived()
	old, err := s.Create(beads.Bead{Title: "old closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(old.ID); err != nil {
		t.Fatal(err)
	}
	open, err := s.Create(beads.Bead{Title: "still open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.archivePath); !os.IsNotExist(err) {
		t.Fatalf("archive written before anything aged out: %v", err)
	}

	// Past the retention: the next save archives the old closed bead.
	e.ageOut(s)
	recent, err := s.Create(beads.Bead{Title: "recent closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(recent.ID); err != nil {
		t.Fatal(err)
	}

	hot, arch := e.hot(), e.archive()
	if hot[old.ID] || !arch[old.ID] {
		t.Fatalf("old closed bead: hot=%v archive=%v, want archive only", hot[old.ID], arch[old.ID])
	}
	if !hot[open.ID] || !hot[recent.ID] || arch[open.ID] || arch[recent.ID] {
		t.Fatalf("open/recent beads must stay hot: hot=%v archive=%v", hot, arch)
	}

	// A fresh handle (another process) sees the union, in creation order.
	s2 := e.archived()
	got, err := s2.Get(old.ID)
	if err != nil || got.Status != "closed" {
		t.Fatalf("Get archived bead = %+v, %v", got, err)
	}
	if order, want := strings.Join(listIDs(t, s2), ","), strings.Join([]string{old.ID, open.ID, recent.ID}, ","); order != want {
		t.Fatalf("List order = %v, want %v", order, want)
	}

	// The archive is not rewritten by mutations that do not change the
	// archived set.
	before := e.archiveStat()
	if err := s2.SetMetadata(open.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}
	after := e.archiveStat()
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("archive rewritten by a hot-only mutation")
	}
}

// Mutating an archived bead (reopen) brings it back to the hot file; the
// revision must keep moving forward across the move.
func TestFileStoreArchiveReopenMovesBeadBackHot(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0)
	archived, err := e.archived().Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := e.archived().Reopen(ids[0]); err != nil {
		t.Fatal(err)
	}
	if !e.hot()[ids[0]] || e.archive()[ids[0]] {
		t.Fatalf("reopened bead must live in the hot file only")
	}
	got, err := e.archived().Get(ids[0])
	if err != nil || got.Status != "open" {
		t.Fatalf("Get reopened = %+v, %v", got, err)
	}
	if got.Revision <= archived.Revision {
		t.Fatalf("revision went backwards across archive move: %d -> %d", archived.Revision, got.Revision)
	}
}

// Revision (the ConditionalWriter token) and ClaimFence are persisted out of
// band; both must survive a bead's trip into the archive.
func TestFileStoreArchiveKeepsRevisionAndFence(t *testing.T) {
	e := newArchiveEnv(t)
	s := e.archived()
	b, err := s.Create(beads.Bead{Title: "claimed"})
	if err != nil {
		t.Fatal(err)
	}
	who := "worker-1"
	if err := s.Update(b.ID, beads.UpdateOpts{Assignee: &who}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	want, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want.ClaimFence == 0 {
		t.Fatalf("precondition: assignment did not bump the fence")
	}
	e.ageOut(s)
	if !e.archive()[b.ID] {
		t.Fatalf("precondition: bead not archived")
	}
	got, err := e.archived().Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != want.Revision || got.ClaimFence != want.ClaimFence {
		t.Fatalf("archived bead revision/fence = %d/%d, want %d/%d", got.Revision, got.ClaimFence, want.Revision, want.ClaimFence)
	}
}

// A crash between the archive and hot writes can leave a bead in both
// files. The higher revision wins; a tie keeps the hot copy.
func TestFileStoreArchiveDuplicateResolvesByRevision(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0)

	// Simulate the crash window (archive written, hot file not yet): put the
	// archived copy back into the hot file, as the previous hot file had it.
	var hot, arch map[string]json.RawMessage
	for path, dst := range map[string]*map[string]json.RawMessage{e.path: &hot, e.archivePath: &arch} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
	}
	var hotBeads, archBeads []json.RawMessage
	if err := json.Unmarshal(hot["beads"], &hotBeads); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(arch["beads"], &archBeads); err != nil {
		t.Fatal(err)
	}
	hotBeads = append(archBeads, hotBeads...)
	var err error
	if hot["beads"], err = json.Marshal(hotBeads); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(hot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if !e.hot()[ids[0]] || !e.archive()[ids[0]] {
		t.Fatalf("precondition: bead must be in both files")
	}

	s2 := e.archived()
	if n := countID(listIDs(t, s2), ids[0]); n != 1 {
		t.Fatalf("bead present %d times after crash-window load, want 1", n)
	}
	// The next save converges the files again.
	if _, err := s2.Create(beads.Bead{Title: "after"}); err != nil {
		t.Fatal(err)
	}
	if e.hot()[ids[0]] || !e.archive()[ids[0]] {
		t.Fatalf("files did not converge after the next save")
	}
}

// Deleting an archived bead removes it from the archive.
func TestFileStoreArchiveDeleteArchivedBead(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0)
	if err := e.archived().Delete(ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.archived().Get(ids[0]); err == nil {
		t.Fatalf("deleted archived bead still readable")
	}
	if e.archive()[ids[0]] {
		t.Fatalf("deleted bead left in the archive")
	}
}

// failFS fails writes whose path ends with failSuffix while armed — a
// disk-full or I/O error in the middle of a multi-file save.
type failFS struct {
	fsys.OSFS
	failSuffix string
	armed      bool
}

var errInjected = errors.New("injected write failure")

func (f *failFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	if f.armed && strings.HasSuffix(name, f.failSuffix) {
		return errInjected
	}
	return f.OSFS.WriteFile(name, data, perm)
}

func (f *failFS) Remove(name string) error {
	if f.armed && strings.HasSuffix(name, f.failSuffix) {
		return errInjected
	}
	return f.OSFS.Remove(name)
}

// A failed hot-file write must leave the store exactly as before the
// mutation — in particular a delete of an archived bead must not have already
// removed it from the archive.
func TestFileStoreArchiveFailedHotWriteKeepsState(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(3, 0, 2) // archive: ids[0], ids[2]
	before := listIDs(t, e.archived())

	ffs := &failFS{failSuffix: "beads.json.tmp"}
	s := e.openFS(ffs, beads.WithFileStoreArchive(testArchiveAfter))
	ffs.armed = true
	if err := s.Delete(ids[0]); !errors.Is(err, errInjected) {
		t.Fatalf("Delete with failing hot write = %v, want injected failure", err)
	}
	// Reopen moves ids[2] out of the archive; the superset written first is
	// out of creation order before sorting ([ids[0]... , ids[2]]).
	if err := s.Reopen(ids[2]); !errors.Is(err, errInjected) {
		t.Fatalf("Reopen with failing hot write = %v, want injected failure", err)
	}

	after := listIDs(t, e.archived())
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("store changed by failed mutations:\n before %v\n after  %v", before, after)
	}
	got, err := e.archived().Get(ids[2])
	if err != nil || got.Status != "closed" {
		t.Fatalf("failed reopen leaked: %+v, %v", got, err)
	}
}

// The archive grows before the hot file commits, so it must not carry the
// failed mutation's seq: a failed Create must not burn the ID it would have
// minted.
func TestFileStoreArchiveFailedCreateDoesNotAdvanceSeq(t *testing.T) {
	e := newArchiveEnv(t)
	plain := e.plain() // no archive yet: ages a closed bead inside the hot file
	b, err := plain.Create(beads.Bead{Title: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	e.ageOut(plain)
	if e.archive()[b.ID] || !e.hot()[b.ID] {
		t.Fatalf("precondition: aged closed bead must still be hot")
	}

	ffs := &failFS{failSuffix: "beads.json.tmp"}
	s := e.openFS(ffs, beads.WithFileStoreArchive(testArchiveAfter))
	ffs.armed = true
	if _, err := s.Create(beads.Bead{Title: "fails"}); !errors.Is(err, errInjected) {
		t.Fatalf("Create with failing hot write = %v, want injected failure", err)
	}
	if !e.archive()[b.ID] {
		t.Fatalf("precondition: phase 1 should have grown the archive")
	}

	next, err := e.archived().Create(beads.Bead{Title: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "gc-2"; next.ID != want {
		t.Fatalf("next minted ID = %s, want %s (the failed create must not burn an ID)", next.ID, want)
	}
}

// Once the hot file is published the mutation is committed: a failing archive
// trim must not report the mutation as failed, and the next save trims.
func TestFileStoreArchiveTrimFailureIsNotAMutationFailure(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0)

	ffs := &failFS{failSuffix: "beads.archive.json"}
	s := e.openFS(ffs, beads.WithFileStoreArchive(testArchiveAfter))
	// Reopen: phase 1 writes nothing new (superset == on-disk archive), the
	// hot write succeeds, and only the trim (here: removing the emptied
	// archive) fails.
	ffs.armed = true
	if err := s.Reopen(ids[0]); err != nil {
		t.Fatalf("Reopen committed but returned %v", err)
	}
	got, err := e.archived().Get(ids[0])
	if err != nil || got.Status != "open" {
		t.Fatalf("Get after trim failure = %+v, %v", got, err)
	}
	if n := countID(listIDs(t, e.archived()), ids[0]); n != 1 {
		t.Fatalf("bead listed %d times with an untrimmed archive", n)
	}

	ffs.armed = false
	if _, err := s.Create(beads.Bead{Title: "next"}); err != nil {
		t.Fatal(err)
	}
	if e.archive()[ids[0]] {
		t.Fatalf("next save did not trim the reopened bead from the archive")
	}
}

// A delete of an archived bead commits with a tombstone in the hot file; if
// the trim then fails, the stale archive copy must not resurrect the bead,
// and the next save trims it and drops the tombstone.
func TestFileStoreArchiveDeleteWithFailedTrimStaysDeleted(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(3, 0, 1) // archive: ids[0], ids[1]

	ffs := &failFS{failSuffix: "beads.archive.json.tmp"}
	s := e.openFS(ffs, beads.WithFileStoreArchive(testArchiveAfter))
	ffs.armed = true
	if err := s.Delete(ids[0]); err != nil {
		t.Fatalf("Delete committed but returned %v", err)
	}
	if !e.archive()[ids[0]] {
		t.Fatalf("precondition: trim should have failed, leaving the archive copy")
	}
	fresh := e.archived()
	if _, err := fresh.Get(ids[0]); err == nil {
		t.Fatalf("deleted bead resurrected from the untrimmed archive")
	}
	if n := countID(listIDs(t, fresh), ids[0]); n != 0 {
		t.Fatalf("deleted bead listed %d times", n)
	}
	if _, err := fresh.Get(ids[1]); err != nil {
		t.Fatalf("other archived bead lost: %v", err)
	}

	// The next save trims; the tombstone is written with it (the trim comes
	// after the hot file) and dropped by the save after that.
	ffs.armed = false
	for i := 0; i < 2; i++ {
		if _, err := s.Create(beads.Bead{Title: "next"}); err != nil {
			t.Fatal(err)
		}
	}
	if e.archive()[ids[0]] {
		t.Fatalf("next save did not trim the deleted bead")
	}
	if raw, err := os.ReadFile(e.path); err != nil || strings.Contains(string(raw), "archive_tombstones") {
		t.Fatalf("tombstone kept after the trim (err %v)", err)
	}
	if _, err := e.archived().Get(ids[0]); err == nil {
		t.Fatalf("deleted bead readable after the trim")
	}
}

// Reusing the ID of a deleted archived bead while the archive trim keeps
// failing: the committed replacement must win over the stale archived
// incarnation, which has the higher revision.
func TestFileStoreArchiveReusedIDBeatsUntrimmedIncarnation(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0) // archive holds only ids[0]
	old, err := e.archived().Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}

	ffs := &failFS{failSuffix: "beads.archive.json"}
	s := e.openFS(ffs, beads.WithFileStoreArchive(testArchiveAfter))
	s.HonorExplicitIDs = true
	ffs.armed = true // every archive trim and removal fails
	if err := s.Delete(ids[0]); err != nil {
		t.Fatalf("Delete committed but returned %v", err)
	}
	repl, err := s.Create(beads.Bead{ID: ids[0], Title: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	if repl.Revision >= old.Revision {
		t.Fatalf("precondition: replacement revision %d must be below the archived %d", repl.Revision, old.Revision)
	}
	if !e.archive()[ids[0]] {
		t.Fatalf("precondition: stale incarnation should still be archived")
	}
	check := func(when string) {
		t.Helper()
		got, err := e.archived().Get(ids[0])
		if err != nil || got.Title != "replacement" || got.Status != "open" {
			t.Fatalf("%s: Get = %+v, %v; want the replacement", when, got, err)
		}
		if n := countID(listIDs(t, e.archived()), ids[0]); n != 1 {
			t.Fatalf("%s: id listed %d times", when, n)
		}
	}
	check("untrimmed archive")
	if err := s.SetMetadata(ids[1], "k", "v"); err != nil { // another failing-trim save
		t.Fatal(err)
	}
	check("second untrimmed save")

	ffs.armed = false
	for i := 0; i < 2; i++ {
		if err := s.SetMetadata(ids[1], "k", strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if e.archive()[ids[0]] {
		t.Fatalf("stale incarnation not trimmed")
	}
	check("after the trim")
}

// A reused ID whose new incarnation reaches the stale archived copy's
// revision must still win: copies are told apart by incarnation, not by
// (ID, Revision). Covered both when the replacement is archived in turn
// (archiving on) and when a handle without the option keeps the archive.
func TestFileStoreArchiveReusedIDAtEqualRevision(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []beads.FileStoreOption
	}{
		{name: "archiving on", opts: []beads.FileStoreOption{beads.WithFileStoreArchive(testArchiveAfter)}},
		{name: "no option keeps the archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newArchiveEnv(t)
			ids := e.seed(2, 0) // archive holds only ids[0]
			old, err := e.archived().Get(ids[0])
			if err != nil {
				t.Fatal(err)
			}

			ffs := &failFS{failSuffix: "beads.archive.json"} // removing the archive fails
			s := e.openFS(ffs, tc.opts...)
			s.HonorExplicitIDs = true
			ffs.armed = true
			if err := s.Delete(ids[0]); err != nil {
				t.Fatalf("Delete committed but returned %v", err)
			}
			repl, err := s.Create(beads.Bead{ID: ids[0], Title: "replacement"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(repl.ID); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Get(repl.ID); err != nil || got.Revision != old.Revision {
				t.Fatalf("precondition: replacement revision = %d (%v), want the archived %d", got.Revision, err, old.Revision)
			}
			if tc.opts != nil {
				e.ageOut(s) // the replacement is archived in turn
			}
			got, err := e.archived().Get(ids[0])
			if err != nil || got.Title != "replacement" {
				t.Fatalf("Get = %+v, %v; want the replacement", got, err)
			}
			if n := countID(listIDs(t, e.archived()), ids[0]); n != 1 {
				t.Fatalf("id listed %d times", n)
			}
		})
	}
}

// A reused ID is a new bead at the end of the store: it must not inherit the
// order key of the incarnation still sitting in an untrimmed archive.
func TestFileStoreArchiveReusedIDTakesANewOrderKey(t *testing.T) {
	e := newArchiveEnv(t)
	s := e.archived()
	s.HonorExplicitIDs = true
	var ids []string
	for _, id := range []string{"x-abc", "", ""} {
		b, err := s.Create(beads.Bead{ID: id, Title: "b"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	for _, id := range []string{ids[0], ids[2]} {
		if err := s.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	e.ageOut(s)
	if a := e.archive(); !a[ids[0]] || !a[ids[2]] {
		t.Fatalf("precondition: archive = %v", a)
	}

	ffs := &failFS{failSuffix: "beads.archive.json.tmp"} // trims fail
	w := e.openFS(ffs, beads.WithFileStoreArchive(testArchiveAfter))
	w.HonorExplicitIDs = true
	ffs.armed = true
	if err := w.Delete(ids[0]); err != nil {
		t.Fatalf("Delete committed but returned %v", err)
	}
	if _, err := w.Create(beads.Bead{ID: ids[0], Title: "replacement"}); err != nil {
		t.Fatal(err)
	}
	if !e.archive()[ids[0]] {
		t.Fatalf("precondition: stale incarnation should still be archived")
	}
	want := strings.Join([]string{ids[1], ids[2], ids[0]}, ",")
	if got := strings.Join(listIDs(t, e.archived()), ","); got != want {
		t.Fatalf("untrimmed: List order = %v, want %v", got, want)
	}

	ffs.armed = false
	for i := 0; i < 2; i++ {
		if err := w.SetMetadata(ids[1], "k", strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(listIDs(t, e.archived()), ","); got != want {
		t.Fatalf("trimmed: List order = %v, want %v", got, want)
	}
}

// Explicit "off" (WithFileStoreArchive(0)) never hides archived beads, and
// the next save folds them back and removes the archive.
func TestFileStoreArchiveOffFoldsBack(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(3, 1)
	want := listIDs(t, e.archived())

	off := e.openFS(fsys.OSFS{}, beads.WithFileStoreArchive(0))
	if got := listIDs(t, off); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("store with archiving off sees %v, want %v", got, want)
	}
	if _, err := off.Create(beads.Bead{Title: "unarchived"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.archivePath); !os.IsNotExist(err) {
		t.Fatalf("archive not removed after folding back: %v", err)
	}
	if !e.hot()[ids[1]] {
		t.Fatalf("archived bead not folded back into the hot file")
	}
	if got := listIDs(t, e.plain()); countID(got, ids[1]) != 1 {
		t.Fatalf("folded-back bead missing: %v", got)
	}
}

// A handle opened without the option (a CLI that could not load the city
// config) sees archived beads and leaves the archive alone, except to move a
// bead it mutates back to the hot file.
func TestFileStoreArchiveNoOptionKeepsArchive(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(3, 0, 1)
	want := listIDs(t, e.archived())

	plain := e.plain()
	if got := listIDs(t, plain); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("store without the option sees %v, want %v", got, want)
	}
	before := e.archiveStat()
	if err := plain.SetMetadata(ids[2], "k", "v"); err != nil {
		t.Fatal(err)
	}
	if after := e.archiveStat(); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("archive rewritten by a hot-only mutation of an option-less handle")
	}
	if e.hot()[ids[0]] || e.hot()[ids[1]] {
		t.Fatalf("option-less handle folded the archive back")
	}
	if err := plain.Reopen(ids[0]); err != nil {
		t.Fatal(err)
	}
	if !e.hot()[ids[0]] || e.archive()[ids[0]] || !e.archive()[ids[1]] {
		t.Fatalf("reopen via option-less handle: hot=%v archive=%v", e.hot(), e.archive())
	}
	if got := listIDs(t, e.archived()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order after the move = %v, want %v", got, want)
	}
}

// Creation order survives the split for IDs the seq suffix cannot order:
// explicit IDs without a numeric suffix, in the hot file or the archive.
func TestFileStoreArchiveKeepsOrderOfNonSequentialIDs(t *testing.T) {
	e := newArchiveEnv(t)
	s := e.archived()
	s.HonorExplicitIDs = true
	var want []string
	add := func(id string, closed bool) {
		t.Helper()
		b, err := s.Create(beads.Bead{ID: id, Title: "b"})
		if err != nil {
			t.Fatal(err)
		}
		if closed {
			if err := s.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
		want = append(want, b.ID)
	}
	add("x-abc", false) // hot, before any minted ID
	add("", true)       // archived
	add("", false)
	add("x-def", true) // archived, between minted IDs
	add("", false)
	e.ageOut(s) // the save that archives
	add("", false)
	if a := e.archive(); !a[want[1]] || !a["x-def"] {
		t.Fatalf("precondition: archive = %v", a)
	}
	if got := listIDs(t, e.archived()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("List order = %v, want %v", got, want)
	}
}

// Recorded order keys stay with their beads: deleting a bead before a mixed
// hot/archive run must not reorder the run when the archive is not rewritten.
func TestFileStoreArchiveOrderSurvivesDeletingAPredecessor(t *testing.T) {
	e := newArchiveEnv(t)
	s := e.archived()
	s.HonorExplicitIDs = true
	var ids []string
	for _, c := range []struct {
		id     string
		closed bool
	}{{"", false}, {"", false}, {"x-abc", true}, {"x-def", false}, {"", false}} {
		b, err := s.Create(beads.Bead{ID: c.id, Title: "b"})
		if err != nil {
			t.Fatal(err)
		}
		if c.closed {
			if err := s.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, b.ID)
	}
	e.ageOut(s)
	if !e.archive()["x-abc"] {
		t.Fatalf("precondition: x-abc not archived")
	}
	before := e.archiveStat()
	if err := s.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	if after := e.archiveStat(); !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("precondition: deleting a hot bead rewrote the archive")
	}
	want := strings.Join([]string{ids[0], "x-abc", "x-def", ids[4]}, ",")
	if got := strings.Join(listIDs(t, e.archived()), ","); got != want {
		t.Fatalf("List order = %v, want %v", got, want)
	}
}

// coarseStatFS reports the archive with a constant size and mtime, like a
// filesystem whose mtime ticks are coarser than two rewrites of same-sized
// archives. Freshness alone then never sees the archive change.
type coarseStatFS struct {
	fsys.OSFS
	archivePath string
}

type coarseInfo struct{ os.FileInfo }

func (coarseInfo) Size() int64        { return 1 }
func (coarseInfo) ModTime() time.Time { return time.Unix(1, 0) }

func (f *coarseStatFS) Stat(name string) (os.FileInfo, error) {
	fi, err := f.OSFS.Stat(name)
	if err == nil && name == f.archivePath {
		return coarseInfo{fi}, nil
	}
	return fi, err
}

// A writer reloads under the lock before mutating; it must not act on a
// cached archive another process replaced when mtime and size cannot tell,
// or the save drops the beads that moved into the new archive.
func TestFileStoreArchiveWriterRereadsArchiveFreshnessCannotTell(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0) // archive {ids[0]}; ids[1] open and hot
	keep := e.plain()
	if err := keep.Close(ids[1]); err != nil {
		t.Fatal(err)
	}
	e.ageOut(keep) // ids[1]: closed and old, still hot (keep mode archives nothing new)
	if !e.hot()[ids[1]] || !e.archive()[ids[0]] {
		t.Fatalf("precondition: hot=%v archive=%v", e.hot(), e.archive())
	}

	w2 := e.openFS(&coarseStatFS{archivePath: e.archivePath}, beads.WithFileStoreArchive(testArchiveAfter))
	listIDs(t, w2)                                      // caches archive {ids[0]}
	if err := e.archived().Reopen(ids[0]); err != nil { // archive becomes {ids[1]}
		t.Fatal(err)
	}
	if a := e.archive(); a[ids[0]] || !a[ids[1]] {
		t.Fatalf("precondition: archive = %v", a)
	}
	if _, err := w2.Create(beads.Bead{Title: "w2"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := e.archived().Get(id); err != nil {
			t.Fatalf("%s lost after a write through a coarse-mtime handle: %v", id, err)
		}
	}
}

// Archive generations must not restart after the archive is folded away: a
// handle still caching the old archive would take a new one with the same
// generation (and, on a coarse filesystem, the same mtime and size) for its
// own and write the new archive's beads away.
func TestFileStoreArchiveGenerationsSurviveFoldBack(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0) // archive {ids[0]}
	w3 := e.openFS(&coarseStatFS{archivePath: e.archivePath}, beads.WithFileStoreArchive(testArchiveAfter))
	listIDs(t, w3) // caches archive {ids[0]}

	off := e.openFS(fsys.OSFS{}, beads.WithFileStoreArchive(0))
	for i := 0; i < 3; i++ { // fold back, then archive-less saves
		if _, err := off.Create(beads.Bead{Title: "fold"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(e.archivePath); !os.IsNotExist(err) {
		t.Fatalf("precondition: archive not folded away: %v", err)
	}

	if err := e.plain().Close(ids[1]); err != nil {
		t.Fatal(err)
	}
	w4 := e.archived() // fresh handle: one save writes the new archive
	e.ageOut(w4)
	if !e.archive()[ids[1]] {
		t.Fatalf("precondition: archive = %v", e.archive())
	}
	// Reopening the bead of its stale cache makes w3 trim that cache's set
	// away — on disk, the new archive with ids[1].
	if err := w3.Reopen(ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := e.archived().Get(id); err != nil {
			t.Fatalf("%s lost after a write through a handle with a pre-fold archive cache: %v", id, err)
		}
	}
}

// hookFS runs the next queued hook right after each read of the hot file:
// another process's save landing between a lock-free reader's two reads.
type hookFS struct {
	fsys.OSFS
	hotPath string
	hooks   []func()
}

func (f *hookFS) ReadFile(name string) ([]byte, error) {
	data, err := f.OSFS.ReadFile(name)
	if name == f.hotPath && len(f.hooks) > 0 {
		h := f.hooks[0]
		f.hooks = f.hooks[1:]
		h()
	}
	return data, err
}

// Readers skip the writer lock. When a writer publishes a hot file and then
// removes the archive between the reader's hot read and archive read, the
// reader must notice (via its cached archive or the hot file's declaration)
// and reread, not settle on a hot file without the archived beads.
func TestFileStoreArchiveLockFreeReaderRechecksRemovedArchive(t *testing.T) {
	for _, tc := range []struct {
		name        string
		readerFirst bool // reader opened before the archive existed
		busyWrites  int  // hot-only saves landing in the reader's earlier retries
	}{
		{name: "cached archive", readerFirst: false},
		{name: "declared by hot file", readerFirst: true},
		// The reader runs out of lock-free retries and must read under the
		// writer lock rather than settle on the last, inconsistent pair.
		{name: "writes outlast the retries", readerFirst: false, busyWrites: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newArchiveEnv(t)
			hfs := &hookFS{hotPath: e.path}
			var reader *beads.FileStore
			w := e.archived()
			if tc.readerFirst {
				if _, err := w.Create(beads.Bead{Title: "first"}); err != nil {
					t.Fatal(err)
				}
				reader = e.openFS(hfs)
				listIDs(t, reader)
			}
			x, err := w.Create(beads.Bead{Title: "only archived bead"})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(x.ID); err != nil {
				t.Fatal(err)
			}
			e.ageOut(w)
			if !e.archive()[x.ID] {
				t.Fatalf("precondition: bead not archived")
			}
			if !tc.readerFirst {
				reader = e.openFS(hfs)
				listIDs(t, reader)
				if _, err := w.Create(beads.Bead{Title: "stale the reader"}); err != nil {
					t.Fatal(err)
				}
			}

			// The reader reloads; right after it reads the hot file, the
			// writer reopens the only archived bead, which removes the archive.
			for i := 0; i < tc.busyWrites; i++ {
				hfs.hooks = append(hfs.hooks, func() {
					if _, err := w.Create(beads.Bead{Title: "busy"}); err != nil {
						t.Errorf("concurrent create: %v", err)
					}
				})
			}
			hfs.hooks = append(hfs.hooks, func() {
				if err := w.Reopen(x.ID); err != nil {
					t.Errorf("concurrent reopen: %v", err)
				}
			})
			got, err := reader.Get(x.ID)
			if err != nil || got.Status != "open" {
				t.Fatalf("reader Get = %+v, %v; want the reopened bead", got, err)
			}
			if _, err := os.Stat(e.archivePath); !os.IsNotExist(err) {
				t.Fatalf("precondition: archive should be gone: %v", err)
			}
		})
	}
}

// An older binary reads and rewrites only the hot file. Its writes keep
// seq and the archive intact, and an archiving binary sees both again.
func TestFileStoreArchiveSurvivesHotOnlyWriter(t *testing.T) {
	e := newArchiveEnv(t)
	ids := e.seed(2, 0)

	// What an older binary does: decode the hot file, append a bead minted
	// from its seq, rewrite the hot file (dropping fields it does not know).
	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	var hot struct {
		Seq   int          `json:"seq"`
		Beads []beads.Bead `json:"beads"`
	}
	if err := json.Unmarshal(raw, &hot); err != nil {
		t.Fatal(err)
	}
	hot.Seq++
	newID := "gc-" + strconv.Itoa(hot.Seq)
	if newID == ids[0] {
		t.Fatalf("hot seq would reuse archived id %s", newID)
	}
	hot.Beads = append(hot.Beads, beads.Bead{ID: newID, Title: "old binary", Status: "open", Type: "task", CreatedAt: time.Now()})
	out, err := json.Marshal(hot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path, out, 0o644); err != nil {
		t.Fatal(err)
	}

	again := e.archived()
	for _, id := range []string{ids[0], newID} {
		if _, err := again.Get(id); err != nil {
			t.Fatalf("Get(%s) after hot-only writer: %v", id, err)
		}
	}
}
