package contract

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// These tests pin the canonical `types.custom` contract that EnsureCanonicalConfig
// now owns, moved out of gc-beads-bd.sh's former ensure_types_custom_in_yaml
// shell function (gascity #2154 / PR #2315 review followup). The shell function
// and its four op_init call sites were deleted; these Go tests carry its
// never-narrow merge + idempotency guarantees forward.

func readConfigFile(t *testing.T, path string) string {
	t.Helper()
	data, err := (fsys.OSFS{}).ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

// Merging a different existing set with the baseline must yield the union:
// existing entries (possibly pack/user-defined) are preserved and the baseline
// lands too.
func TestEnsureCanonicalConfigMergesCustomTypesWithExisting(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("issue_prefix: gc\ntypes.custom: legacy_a,legacy_b,legacy_c\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix: "gc",
		CustomTypes: []string{"alpha", "beta", "gamma"},
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report a change when merging new baseline types")
	}

	got := readConfigFile(t, path)
	for _, must := range []string{"legacy_a", "legacy_b", "legacy_c", "alpha", "beta", "gamma"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml missing type %q after merge:\n%s", must, got)
		}
	}
	// Current-first ordering: existing entries precede newly-added baseline.
	value, ok := scanConfigLineValueFromData([]byte(got), "types.custom:")
	if !ok {
		t.Fatalf("types.custom line missing:\n%s", got)
	}
	if want := "legacy_a,legacy_b,legacy_c,alpha,beta,gamma"; value != want {
		t.Fatalf("types.custom = %q, want %q", value, want)
	}
}

// When the on-disk value already equals the merged set, EnsureCanonicalConfig
// must not rewrite the key — no change reported, byte-identical file (the shell
// short-circuited to avoid mtime churn downstream watchers misread).
func TestEnsureCanonicalConfigCustomTypesIdempotentWhenMatching(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	baseline := []string{"alpha", "beta", "gamma"}
	// Prime the file to the fully-canonical form so only types.custom is under test.
	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{IssuePrefix: "gc", CustomTypes: baseline}); err != nil {
		t.Fatal(err)
	}
	before := readConfigFile(t, path)

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{IssuePrefix: "gc", CustomTypes: baseline})
	if err != nil {
		t.Fatalf("second EnsureCanonicalConfig() error = %v", err)
	}
	if changed {
		t.Fatalf("EnsureCanonicalConfig() should be idempotent for an unchanged types.custom:\n%s", before)
	}
	if after := readConfigFile(t, path); after != before {
		t.Fatalf("config.yaml changed on idempotent call:\nbefore: %q\nafter:  %q", before, after)
	}
}

// Never narrow: when the caller passes only the baseline but the file carries
// pack/user extensions beyond it, those extensions must survive.
func TestEnsureCanonicalConfigCustomTypesPreservesExtensions(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("issue_prefix: gc\ntypes.custom: alpha,beta,pack_custom_a,pack_custom_b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{IssuePrefix: "gc", CustomTypes: []string{"alpha", "beta"}}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}

	got := readConfigFile(t, path)
	for _, must := range []string{"alpha", "beta", "pack_custom_a", "pack_custom_b"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml narrowed away custom type %q:\n%s", must, got)
		}
	}
}

// A file carrying only extensions (no overlap with the baseline) must end up
// with both the extensions and the full baseline.
func TestEnsureCanonicalConfigCustomTypesAddsMissingBaseline(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("issue_prefix: gc\ntypes.custom: pack_only_a,pack_only_b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{IssuePrefix: "gc", CustomTypes: []string{"alpha", "beta", "gamma"}}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}

	got := readConfigFile(t, path)
	for _, must := range []string{"pack_only_a", "pack_only_b", "alpha", "beta", "gamma"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml missing expected type %q:\n%s", must, got)
		}
	}
}

// When CustomTypes is empty the key is untouched — existing callers that do not
// opt in keep today's passthrough behavior (no types.custom management).
func TestEnsureCanonicalConfigCustomTypesEmptyIsPassthrough(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("issue_prefix: gc\ntypes.custom: only_existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{IssuePrefix: "gc"}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}

	got := readConfigFile(t, path)
	if value, _ := scanConfigLineValueFromData([]byte(got), "types.custom:"); value != "only_existing" {
		t.Fatalf("types.custom must be untouched when CustomTypes empty, got %q:\n%s", value, got)
	}
}

// The fallback (malformed-YAML repair) path must also union CustomTypes: bd init
// emits a glued `sync.remote: "…"types.custom: …` line that routes through
// ensureCanonicalConfigFallback, and the old shell function unioned regardless
// of YAML validity, so parity requires the same here.
func TestEnsureCanonicalConfigCustomTypesUnionsInFallback(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.Join([]string{
		"issue_prefix: si",
		"issue-prefix: si",
		`sync.remote: "git+ssh://git@example.com/foo/svc.git"  types.custom: alpha,pack_extra`,
		"",
	}, "\n")
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix: "si",
		CustomTypes: []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report a change repairing+unioning the glued line")
	}

	got := readConfigFile(t, path)
	// Must parse as YAML after repair.
	if _, err := readConfigDoc(fs, path); err != nil {
		t.Fatalf("repaired config must parse as YAML, got %v\n%s", err, got)
	}
	// Union preserves the on-disk extension and adds the missing baseline.
	value, ok := scanConfigLineValueFromData([]byte(got), "types.custom:")
	if !ok {
		t.Fatalf("types.custom missing after fallback repair:\n%s", got)
	}
	for _, must := range []string{"alpha", "pack_extra", "beta"} {
		if !strings.Contains(value, must) {
			t.Errorf("fallback types.custom %q missing %q", value, must)
		}
	}
	occurrences := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "types.custom:") {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Fatalf("types.custom should appear exactly once after fallback, found %d:\n%s", occurrences, got)
	}
}

// A malformed line whose types.custom VALUE is quoted must not corrupt the
// merge: scanning raw bytes yields `"alpha,beta"`, which splits into `"alpha`
// and `beta"`. Without quote-stripping those never match the required set and
// get re-appended as garbage duplicates (the #2154 corruption, in the repair
// path). parseCustomTypesValue strips quotes to prevent this.
func TestEnsureCanonicalConfigCustomTypesFallbackStripsQuotes(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.Join([]string{
		"issue_prefix: si",
		"issue-prefix: si",
		`sync.remote: "git+ssh://git@example.com/foo/svc.git"  types.custom: "alpha,beta"`,
		"",
	}, "\n")
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix: "si",
		CustomTypes: []string{"alpha", "gamma"},
	}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}

	got := readConfigFile(t, path)
	if _, err := readConfigDoc(fs, path); err != nil {
		t.Fatalf("repaired config must parse as YAML, got %v\n%s", err, got)
	}
	value, ok := scanConfigLineValueFromData([]byte(got), "types.custom:")
	if !ok {
		t.Fatalf("types.custom missing after fallback repair:\n%s", got)
	}
	// Exact merged set: on-disk alpha,beta (unquoted) then missing baseline gamma.
	// No stray quote-bearing tokens like `"alpha` or `beta"`.
	if want := "alpha,beta,gamma"; value != want {
		t.Fatalf("fallback types.custom = %q, want %q (quote corruption?)", value, want)
	}
	if strings.Contains(value, `"`) {
		t.Fatalf("types.custom value retains quote characters: %q", value)
	}
}

func TestMergeCustomTypes(t *testing.T) {
	tests := []struct {
		name     string
		current  []string
		required []string
		want     []string
	}{
		{"empty current", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"empty required", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"current first then missing", []string{"z", "a"}, []string{"a", "b"}, []string{"z", "a", "b"}},
		{"dedup and trim", []string{" a ", "a", ""}, []string{"a", "b"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeCustomTypes(tt.current, tt.required)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("MergeCustomTypes(%v, %v) = %v, want %v", tt.current, tt.required, got, tt.want)
			}
		})
	}
}

// TestReadCustomTypesConfig pins the reader the gc doctor custom-types check
// uses as its embedded-mode fallback (GC-REG-23): `bd config get` answers only
// from the DB config table, so a scope whose types.custom lives solely in
// config.yaml — the key bd itself falls back to — must be readable straight
// from the file.
func TestReadCustomTypesConfig(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()

	t.Run("reads flat key and splits CSV", func(t *testing.T) {
		path := filepath.Join(dir, "a.yaml")
		if err := fs.WriteFile(path, []byte("issue_prefix: gc\ntypes.custom: alpha,beta, gamma\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadCustomTypesConfig(fs, path)
		if err != nil || !ok {
			t.Fatalf("ReadCustomTypesConfig() ok=%v err=%v, want ok=true err=nil", ok, err)
		}
		if strings.Join(got, ",") != "alpha,beta,gamma" {
			t.Fatalf("types = %v, want [alpha beta gamma]", got)
		}
	})

	t.Run("missing file reports absent without error", func(t *testing.T) {
		got, ok, err := ReadCustomTypesConfig(fs, filepath.Join(dir, "absent.yaml"))
		if err != nil || ok || got != nil {
			t.Fatalf("ReadCustomTypesConfig(absent) = %v, %v, %v; want nil, false, nil", got, ok, err)
		}
	})

	t.Run("missing or empty key reports absent", func(t *testing.T) {
		path := filepath.Join(dir, "b.yaml")
		if err := fs.WriteFile(path, []byte("issue_prefix: gc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := ReadCustomTypesConfig(fs, path); err != nil || ok {
			t.Fatalf("key-less config: ok=%v err=%v, want ok=false err=nil", ok, err)
		}

		empty := filepath.Join(dir, "c.yaml")
		if err := fs.WriteFile(empty, []byte("types.custom:\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := ReadCustomTypesConfig(fs, empty); err != nil || ok {
			t.Fatalf("empty value: ok=%v err=%v, want ok=false err=nil", ok, err)
		}
	})

	t.Run("unparseable YAML still answers via line scan", func(t *testing.T) {
		// bd init has emitted glued `key: value` lines that fail YAML parsing
		// (see EnsureCanonicalConfig's fallback path); the reader must still
		// answer from the raw line, not error out.
		path := filepath.Join(dir, "d.yaml")
		broken := "issue_prefix: gc\ntypes.custom: [unclosed\n"
		if err := fs.WriteFile(path, []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadCustomTypesConfig(fs, path)
		if err != nil {
			t.Fatalf("ReadCustomTypesConfig(broken) err = %v, want nil", err)
		}
		if !ok || len(got) == 0 {
			t.Fatalf("ReadCustomTypesConfig(broken) ok=%v got=%v, want the scanned value", ok, got)
		}
	})
}
