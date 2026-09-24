package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// This file covers ga-qi9km: `gc rig add` and `gc supervisor run` silently
// rewrite a city's .beads/metadata.json from embedded to server, re-pointing
// the scope at a database that does not hold its beads, after which every
// work-store read answers `[]` with exit 0.
//
// agent-forge-teyh closed the root cause for scopes whose TRACKED contract is
// embedded Dolt: canonicalization now preserves their dolt_mode instead of
// flipping it — an embedded scope owns its ledger under its own .beads
// directory, and flipping it onto a server endpoint re-points every read at a
// database that does not hold its rows, or at a server that no longer exists
// (the live recurrence: `gc start` rewrote an embedded rig onto a retired
// Dolt server and bricked boot behind "explicit rig config requires
// dolt.port"). What remains of the rewrite is ADOPTION: a scope carrying the
// pre-registry "legacy" backend marker, or recording no mode at all, still
// canonicalizes to gc's managed server mode, and that flip is the one the
// flip-time announcement still describes.
//
// ga-clsfl closed the read-time half — as a NOTICE, never a refusal. The
// two experiment tests near the end of this file are why: they are the cases a
// refusal keyed on "a `.dolt` directory exists under the other mode's
// subdirectory" gets wrong, and that fact is the only evidence available
// without opening the second database. They now double as the acceptance
// tests for the notice, since a notice must leave every one of their answers
// exactly as it found them.
//
// Both fixtures are real directories with real files. The defect survived a
// suite that passes precisely because it lives in the disagreement between a
// JSON file and a directory listing, and a double for either one asserts the
// bug away.

// embeddedScopeWithBeads builds a scope whose .beads/ is an embedded-mode bd
// workspace with a Dolt repository under it — what `bd init -p <prefix>` leaves
// behind, and what the live proof's rig had before gc touched it.
//
// The Dolt repository is represented by the directory shape gc itself uses to
// recognize one (a `.dolt` subdirectory, the same test gc doctor's
// doltReposUnder applies). Standing up a real Dolt server to prove a
// path-and-JSON disagreement would test Dolt, not this.
func embeddedScopeWithBeads(t *testing.T, database string) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", database, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeMetadata(t, scope, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "embedded",
		"dolt_database": database,
	})
	return scope
}

// legacyAdoptionScopeFixture builds the adoption shape: the pre-registry
// "legacy" backend marker with a recorded embedded mode and a Dolt repository
// under .beads/embeddeddolt. Nothing this build serves can read it, so
// canonicalization adopts it onto gc's managed server mode — and that flip is
// what the storage-mode announcement still exists to make loud.
func legacyAdoptionScopeFixture(t *testing.T, database string) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", database, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeMetadata(t, scope, map[string]string{
		"database":      "legacy",
		"backend":       "legacy",
		"dolt_mode":     "embedded",
		"dolt_database": database,
	})
	return scope
}

func readScopeDoltMode(t *testing.T, scope string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scope, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.DoltMode
}

// captureStorageModeChanges redirects the sink the canonicalization announces
// storage-mode changes on and returns the buffer holding them.
func captureStorageModeChanges(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := storageModeChangeSink
	buf := &bytes.Buffer{}
	storageModeChangeSink = buf
	t.Cleanup(func() { storageModeChangeSink = orig })
	return buf
}

// emptyBdRunner answers every bd invocation with `[]` and exit 0 — the exact
// thing bd does after the rewrite, because bd is not broken: it connects to the
// database its metadata names, runs the query, and matches nothing.
func emptyBdRunner(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil }

// TestCanonicalizingAnEmbeddedContractScopePreservesItsModeSilently pins the
// agent-forge-teyh root-cause fix: an embedded-contract scope is mode-correct
// already, so canonicalization must keep its dolt_mode and stay silent —
// every boot re-runs it, and one line per scope per boot is a line nobody
// reads. Red before the fix: the rewrite happened on every `gc start` and the
// announcement was the only guard.
func TestCanonicalizingAnEmbeddedContractScopePreservesItsModeSilently(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	notices := captureStorageModeChanges(t)

	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}

	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q after canonicalization, want the embedded contract preserved", mode)
	}
	if notices.Len() != 0 {
		t.Errorf("preserving an embedded contract announced a storage-mode change: %q", notices.String())
	}
}

// TestTheAdoptionAnnouncementNamesARecoveryThatSURVIVESTheNextBoot is the
// operator-guidance half of ga-qi9km, now scoped to the only flip that still
// happens: adopting a scope canonicalization cannot serve (the pre-registry
// "legacy" marker) onto gc's managed server mode.
//
// It pins the two ways this message can be worse than useless. The first is
// advice gc itself undoes: ensureCanonicalScopeMetadata re-canonicalizes
// adopted scopes to server mode, and `gc start`, `gc rig add`, `gc supervisor
// run` and the controller's rig-create handler all run it — so "point
// metadata.json back at the embedded database" works until the next boot and
// then silently stops. The message must not offer it, and must say the edit
// does not hold.
//
// The second is overstating what is on disk. `bd init` creates the embedded
// repository before a single bead exists, so "holds a Dolt bead database" is
// all that is knowable — a claim that it holds ROWS is one gc cannot make
// without opening it, and a message that overstates gets ignored the next time
// it is right.
//
// The remediation named is `gc doctor`'s own (splitStoreFixHint), word for
// word on the load-bearing clause, so the two do not send an operator in
// different directions about the same two directories.
func TestTheAdoptionAnnouncementNamesARecoveryThatSURVIVESTheNextBoot(t *testing.T) {
	scope := legacyAdoptionScopeFixture(t, "jc")
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	notice := notices.String()

	left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
	for _, want := range []string{scope, "embedded", "server", left, "STOP reading"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the announcement never names %q; notices=%q", want, notice)
		}
	}
	for _, want := range []string{
		"gc doctor",
		"bd import --dry-run",
		// gc doctor's splitStoreFixHint prescribes exactly this state; the
		// announcement must not tell the operator to undo it.
		"keep both directories until reconciled",
		// The edit an operator reaches for first is the one gc reverts.
		"re-canonicalizes",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the announcement does not name %q: %q", want, notice)
		}
	}
	// Not a restatement: the check the announcement names is run against the
	// scope the announcement was printed for, so a drift in either text or a
	// regression that makes the check silent on this shape fails here.
	result := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("gc doctor's bd-split-store check reports %v (%q) for the scope the announcement steers it at; a diagnostic that answers OK on the state an operator was just warned about is a false all-clear",
			result.Status, result.Message)
	}
	if !strings.Contains(result.FixHint, "keep both directories until reconciled") {
		t.Errorf("gc doctor's fix hint %q no longer carries the clause the announcement mirrors; the two have drifted", result.FixHint)
	}
	for _, forbidden := range []string{
		// Naming this edit as a recovery sends the operator round a loop.
		`"dolt_mode": "embedded"`,
		"point .beads/metadata.json back",
		// Nothing on disk supports these.
		"lost", "deleted", "corrupt",
	} {
		if strings.Contains(strings.ToLower(notice), strings.ToLower(forbidden)) {
			t.Errorf("the announcement claims %q, which gc either cannot know or immediately reverts: %q", forbidden, notice)
		}
	}
}

// TestEveryDoorLeavesOneCoherentStorageMode closes the gap a per-command
// warning always has, and pins the split agent-forge-teyh draws between the
// two kinds of doors.
//
// The INIT door is the automatic one — every `gc start`, `gc rig add` and
// `gc supervisor run` runs it — and it preserves an embedded-contract scope
// silently: nobody asked to move the scope, so nobody may.
//
// The ENDPOINT doors are operator commands: `gc rig set-endpoint` and
// `gc beads city use-managed`/`use-external` reach their own canonicalizers
// (requireCanonicalizedScopeMetadata for the scope the command names,
// canonicalizeScopeMetadataIfPresent for the inherited rigs a city endpoint
// change sweeps along, both in cmd_rig_endpoint.go), and every endpoint
// choice names a SERVER topology. An explicit endpoint change deliberately
// transitions the scope to server mode and announces the flip — metadata is
// the routing identity, so leaving it embedded while the canonical config
// names a server endpoint would make the two files describe different stores
// (fix-loop 2, objection 2). A warning that depends on which door the scope
// arrived through is still a warning nobody can rely on: all endpoint doors
// behave identically.
func TestEveryDoorLeavesOneCoherentStorageMode(t *testing.T) {
	t.Run("init door preserves the embedded contract silently", func(t *testing.T) {
		scope := embeddedScopeWithBeads(t, "jc")
		notices := captureStorageModeChanges(t)
		if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
			t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
		}
		if mode := readScopeDoltMode(t, scope); mode != "embedded" {
			t.Fatalf("dolt_mode = %q, want the embedded contract preserved", mode)
		}
		if notices.Len() != 0 {
			t.Fatalf("preserving an embedded contract announced a change: %q", notices.String())
		}
	})

	for name, canonicalize := range map[string]func(scope string) error{
		"endpoint path, named scope": func(scope string) error {
			return requireCanonicalizedScopeMetadata(fsys.OSFS{}, scope)
		},
		"endpoint path, inherited rig": func(scope string) error {
			return canonicalizeScopeMetadataIfPresent(fsys.OSFS{}, scope)
		},
	} {
		t.Run(name, func(t *testing.T) {
			scope := embeddedScopeWithBeads(t, "jc")
			notices := captureStorageModeChanges(t)
			if err := canonicalize(scope); err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if mode := readScopeDoltMode(t, scope); mode != "server" {
				t.Fatalf("dolt_mode = %q, want the deliberate server transition", mode)
			}
			left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
			if !strings.Contains(notices.String(), left) {
				t.Fatalf("the transition did not name %q; notices=%q", left, notices.String())
			}
		})
	}
}

// TestCanonicalizingAnAlreadyCanonicalScopeIsSilent keeps the signal worth
// something. Every boot re-canonicalizes every scope; a line per scope per boot
// is a line nobody reads, and the one that matters would arrive inside it.
func TestCanonicalizingAnAlreadyCanonicalScopeIsSilent(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"already server":   {"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc"},
		"no mode recorded": {"database": "dolt", "backend": "dolt", "dolt_database": "jc"},
	} {
		t.Run(name, func(t *testing.T) {
			scope := t.TempDir()
			writeScopeMetadata(t, scope, meta)
			notices := captureStorageModeChanges(t)

			if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
				t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
			}
			if notices.Len() != 0 {
				t.Fatalf("a canonical scope announced a storage-mode change: %q", notices.String())
			}
		})
	}
}

// TestPreservingAnEmbeddedContractNeverChangesWhatAReadAnswers is the mutation
// proof for both halves of the read story: with the contract preserved there is
// no re-pointed read to warn about, so every read still answers `[]` with nil —
// now through the embedded database the metadata still names — and nothing is
// printed, because nothing changed.
func TestPreservingAnEmbeddedContractNeverChangesWhatAReadAnswers(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}

	var notices bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&notices))
	for _, name := range []string{"List", "Ready", "Children"} {
		reads := map[string]func() ([]beads.Bead, error){
			"List":     func() ([]beads.Bead, error) { return store.List(beads.ListQuery{AllowScan: true}) },
			"Ready":    func() ([]beads.Bead, error) { return store.Ready() },
			"Children": func() ([]beads.Bead, error) { return store.Children("jc-1") },
		}
		t.Run(name, func(t *testing.T) {
			got, err := reads[name]()
			if err != nil || len(got) != 0 {
				t.Fatalf("%s = (%d beads, %v), want (0, nil)", name, len(got), err)
			}
		})
	}
	// No flip, nothing left behind, nothing to announce at read time either.
	if notices.Len() != 0 {
		t.Fatalf("a preserved embedded contract produced a read-time notice: %q", notices.String())
	}
}

// TestAnEmptyReadIsNotEvidenceTheScopeIsReadingTheWrongDatabase is the first of
// the two cases that keep a read-time REFUSAL out of this change, and it is the
// one a populated city hits every minute.
//
// A refusal keyed on the presence of a second Dolt directory fires on the
// RESULT of one call, not on the store: `Ready()` returning zero rows is the
// steady state of an idle city and of every assignee-scoped probe, and the
// filtered reads below are answered by a store bd just handed rows for. A city
// that migrated deliberately and kept the old directory — which is the state
// `gc doctor`'s own fix hint tells operators to sit in — would have every one
// of these turn into an error, and `federateBeadLegs` aborts the whole
// federation on any leg error, so `gc ready` exits non-zero for the city and
// every worker's generated work query fails with it.
//
// It is also the acceptance case for the notice ga-clsfl ships: this store is
// demonstrably populated, so it must be answered AND left alone — no error, and
// nothing printed. The notice decides per STORE (has this store ever handed
// back a row?), which is why the first List below immunizes every read after
// it.
//
// Red before ga-clsfl's predecessor, on a scope with metadata pointing at the
// server store and a retained .beads/embeddeddolt/jc:
//
//	List  = (1 beads, <nil>)                    ← the active store is populated
//	Ready = (0 beads, bead store read returned empty while an unread bead database sits beside it…)
func TestAnEmptyReadIsNotEvidenceTheScopeIsReadingTheWrongDatabase(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	answering := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ready" {
			return []byte(`[]`), nil
		}
		return []byte(`[{"id":"jc-1","title":"real row","status":"open","assignee":"alice"}]`), nil
	}
	var notices bytes.Buffer
	store := beads.NewBdStore(scope, answering, beads.WithBdStoreNoticeSink(&notices))

	got, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List = (%d beads, %v), want (1, nil): this store is demonstrably the populated one", len(got), err)
	}
	t.Cleanup(func() {
		if notices.Len() != 0 {
			t.Errorf("a demonstrably populated store printed the unread-store notice: %q", notices.String())
		}
	})
	for name, read := range map[string]func() ([]beads.Bead, error){
		// The frontier is empty because nothing is claimable right now.
		"empty frontier on a populated store": func() ([]beads.Bead, error) { return store.Ready() },
		// bd answered with a row; the in-process assignee filter dropped it.
		"per-assignee frontier": func() ([]beads.Bead, error) {
			return store.Ready(beads.ReadyQuery{Assignee: "demo/worker"})
		},
		// bd answered with a row; the wisp-tier filter dropped it.
		"wisp tier over issue rows": func() ([]beads.Bead, error) {
			return store.Ready(beads.ReadyQuery{TierMode: beads.TierWisps})
		},
		// A leaf really has no children, and 26 non-test call sites walk them.
		"children of a leaf": func() ([]beads.Bead, error) { return store.Children("jc-9") },
		// An empty inbox is the normal state of a mail poll.
		"mail poll with no mail": func() ([]beads.Bead, error) {
			return store.List(beads.ListQuery{Type: "message", Status: "open", Assignee: "demo/worker"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := read()
			if err != nil {
				t.Fatalf("read returned %d beads and err = %v; an empty answer from a demonstrably populated store is a real answer, and refusing it fails `gc ready` for the whole city", len(got), err)
			}
		})
	}
}

// TestAdoptingAFreshlyInitializedWorkspaceStillReads is the second case, and it
// is why narrowing the refusal to "the active store is provably empty" does not
// rescue it either.
//
// `bd init` defaults to embedded mode and creates .beads/embeddeddolt/<db>/.dolt
// before a single bead exists (bd's own cmd/bd/init_embedded_test.go asserts
// that file). Since agent-forge-teyh, adopting such a workspace PRESERVES its
// embedded contract — the readiness gate `gc rig add` and `gc start` block on
// opens the embedded database the metadata still names, so a fresh rig reads as
// a fresh rig: zero beads, nil error, no sleep, and no announcement, because
// nothing was rewritten.
func TestAdoptingAFreshlyInitializedWorkspaceStillReads(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jkq") // `bd init -p jkq`: empty repo, embedded mode
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jkq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q after adoption, want the embedded contract preserved", mode)
	}
	if notices.Len() != 0 {
		t.Fatalf("preserving a fresh workspace's embedded contract announced a change: %q", notices.String())
	}

	var readNotices bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&readNotices))
	// The readiness gate `gc rig add` and `gc start` block adoption on.
	slept := 0
	if err := verifyCanonicalBdScopeStoreReady(store, func(time.Duration) { slept++ }); err != nil {
		t.Fatalf("verifyCanonicalBdScopeStoreReady = %v, want nil: a rig with no beads yet is not a broken rig", err)
	}
	if slept != 0 {
		t.Fatalf("adoption slept %d time(s) before succeeding; the gate must pass on the first attempt", slept)
	}
	if got, err := store.Ready(); err != nil || len(got) != 0 {
		t.Fatalf("Ready = (%d beads, %v), want (0, nil)", len(got), err)
	}
	// Nothing was re-pointed, so the read-time notice has nothing to say.
	if readNotices.Len() != 0 {
		t.Fatalf("a preserved embedded contract produced a read-time notice:\n%s", readNotices.String())
	}
}

// TestTheStorageModeAnnouncementIsNotSplitStoreSpecific states the scope of the
// fix out loud, because the program it lands in is a split-store program and
// the reflex is to assume this rides along with it.
//
// The fixture carries no [storage] section and no relocated coordination class.
// Everything about the defect — the metadata rewrite, the re-pointed workspace,
// the `[]` with exit 0 — happens on a city with exactly one store, and the
// preservation lands there too: one store, mode kept, silence kept.
func TestTheStorageModeAnnouncementIsNotSplitStoreSpecific(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "hq")
	if _, present := beads.BeadDatabaseDirForDoltMode(scope, "server", "hq"); present {
		t.Fatal("the fixture has a server database; nothing has been rewritten yet")
	}

	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "hq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q on a single-store city, want the embedded contract preserved", mode)
	}
	if notices.Len() != 0 {
		t.Fatalf("preservation announced a change on a single-store city: %q", notices.String())
	}
	if _, present := beads.BeadDatabaseDirForDoltMode(scope, "embedded", "hq"); !present {
		t.Fatal("the embedded database the scope still reads is no longer resolvable")
	}
}

// TestTheThreeMessagesAboutOneUnreadDatabaseAgree is the reconciliation pin for
// ga-clsfl's fourth acceptance criterion, scoped to the adoption flip — the one
// rewrite that still produces an unread database.
//
// Three things now talk about the same two directories, at three different
// moments: the announcement when gc adopts a legacy scope onto the managed
// server, `gc doctor`'s bd-split-store check when an operator goes looking, and
// the read-time notice when an empty answer is actually paid for. They were
// written months apart and nothing but this test stops them drifting into three
// different stories about one scope — which is the failure mode that produced
// the bug: `gc doctor` telling an operator to "keep both directories until
// reconciled" while a guard turned that exact state into a hard error.
//
// So all three run against ONE scope here, and the claims that have to line up
// are asserted across them: the same remediation clause, the same directory,
// the same evidential ceiling (nothing claims rows were lost), and doctor
// naming the override the notice tells the operator to set — because doctor's
// advice is what parks them in the shape the notice describes.
func TestTheThreeMessagesAboutOneUnreadDatabaseAgree(t *testing.T) {
	scope := legacyAdoptionScopeFixture(t, "jc")
	announcement := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	var readNotice bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&readNotice))
	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}
	diagnostic := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if diagnostic.Status != doctor.StatusWarning {
		t.Fatalf("gc doctor reports %v (%q) for the scope the other two messages are about; a diagnostic that answers OK on the state an operator was just warned about is a false all-clear",
			diagnostic.Status, diagnostic.Message)
	}

	messages := map[string]string{
		"flip-time announcement": announcement.String(),
		"read-time notice":       readNotice.String(),
		"gc doctor fix hint":     diagnostic.FixHint,
	}
	left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
	for name, msg := range messages {
		if !strings.Contains(msg, "keep both directories until reconciled") {
			t.Errorf("%s no longer carries the shared remediation clause: %q", name, msg)
		}
		if !strings.Contains(msg, "bd import --dry-run") {
			t.Errorf("%s no longer names the review step the other two do: %q", name, msg)
		}
		for _, forbidden := range []string{"lost", "deleted", "corrupt"} {
			if strings.Contains(strings.ToLower(msg), forbidden) {
				t.Errorf("%s claims %q, which no message here can know without opening the second database: %q", name, forbidden, msg)
			}
		}
	}
	for _, name := range []string{"flip-time announcement", "read-time notice"} {
		if !strings.Contains(messages[name], left) {
			t.Errorf("%s does not name %q, so the two point at different directories: %q", name, left, messages[name])
		}
		if !strings.Contains(messages[name], "gc doctor") {
			t.Errorf("%s does not steer at the diagnostic that enumerates both stores: %q", name, messages[name])
		}
	}
	// Doctor prescribes the state the notice fires in, so it has to name the
	// way to live in that state quietly.
	if !strings.Contains(diagnostic.FixHint, beads.AllowUnreadStoreReadEnvVar) {
		t.Errorf("gc doctor tells an operator to keep both directories but never names %s, the override that makes the read-time notice bearable while they do: %q",
			beads.AllowUnreadStoreReadEnvVar, diagnostic.FixHint)
	}
	if !strings.Contains(readNotice.String(), beads.AllowUnreadStoreReadEnvVar) {
		t.Errorf("the read-time notice does not name its own override: %q", readNotice.String())
	}
	// And doctor has to promise the bound the guard actually holds. The guard
	// memoizes per SCOPE PATH inside one process, and cmd/gc builds a throwaway
	// bd store per request on the paths internal/api reads through — so "a
	// one-time notice" was false there, at status-rebuild rate, in a tree with
	// a documented log-flood history. Telling an operator to expect less noise
	// than they will get is the same class of false statement as telling them
	// rows are gone.
	if strings.Contains(diagnostic.FixHint, "one-time notice") {
		t.Errorf("gc doctor promises a one-time notice; the guard bounds it per scope per process, and the API rebuilds its store per request: %q", diagnostic.FixHint)
	}
	if !strings.Contains(diagnostic.FixHint, "one notice per gc process") {
		t.Errorf("gc doctor does not state the bound the guard holds (one notice per gc process): %q", diagnostic.FixHint)
	}
}
