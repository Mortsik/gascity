package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCustomTypesCheck_NoBeadsDir(t *testing.T) {
	dir := t.TempDir()
	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (no .beads dir)", r.Status)
	}
}

func TestCustomTypesCheck_MissingTypes(t *testing.T) {
	// Scrub inherited beads env so the `bd config get` subprocess below
	// resolves to the empty .beads/ in the temp dir instead of an outer
	// gc city's beads database. Without this, bd can reach a live dolt
	// server (via GC_BEADS=bd + BEADS_DOLT_SERVER_PORT), reports all
	// required types as present, and the check returns StatusOK —
	// defeating the assertion. Clearing GC_BEADS and the dolt connection
	// vars prevents bd from connecting even if testenv.init() has not
	// yet added them to its LeakVectorVars scrub list.
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}

	// Scrubbing env vars alone is not enough: bd's config precedence falls
	// through to $HOME/.beads/config.yaml as a last resort, so a machine
	// HOME with dolt.shared-server: true still routes bd to the shared
	// server — which answers with every required type present and turns
	// this check StatusOK, defeating the assertion below. Pin a test-owned
	// HOME so that fallback file doesn't exist. See ga-zxpfic and
	// TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext.
	testOwnedHome(t)

	dir := guardedTempDir(t)
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewCustomTypesCheck(dir, "test")
	// This will fail because bd isn't initialized in the temp dir.
	// The check should report a warning (can't read config).
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status == StatusOK {
		t.Fatal("expected non-OK status when bd config fails")
	}
	if !c.CanFix() {
		t.Fatal("CanFix should return true")
	}
}

// TestCustomTypesCheck_TableDrift proves detect+heal of the bug this bead
// fixes: config.yaml's types.custom CSV can list a type (e.g. "step") that
// the normalized custom_types TABLE doesn't have a row for. bd's create
// validation reads the TABLE, not the CSV, so a store in this state rejects
// `bd create --type step ...` with "invalid issue type: step" even though
// `bd config get types.custom` reports the type present. This drift happens
// on stores an older bd wrote (or where the table row was dropped some
// other way) — bd itself keeps CSV and table in sync on `bd config set`,
// but nothing previously re-ran that set for existing stores.
//
// The test manufactures the drift directly (delete the table row via the
// dolt CLI) rather than depending on an old bd binary, then asserts Run
// catches it — even though the CSV alone is complete — and Fix heals it by
// re-running `bd config set types.custom <merged>`, which reinserts the
// missing table row as a side effect of bd's own set-path.
func TestCustomTypesCheck_TableDrift(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}

	// Scrub inherited beads env so the bd subprocesses below resolve to the
	// throwaway store in the temp dir instead of an outer gc city's beads
	// database. See TestCustomTypesCheck_MissingTypes for why each var
	// matters.
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}

	// Scrubbing env vars alone is not enough: bd's config precedence falls
	// through to $HOME/.beads/config.yaml as a last resort, so on a fleet
	// agent HOME with dolt.shared-server: true set there, bd still routes
	// to the shared server regardless of the vars above. Pin a test-owned
	// HOME so that fallback file doesn't exist. See ga-zxpfic and
	// TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext.
	testOwnedHome(t)

	dir := guardedTempDir(t)

	runBD := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bd", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	runBD("init", "--non-interactive", "-p", "tst", "--skip-hooks", "--skip-agents")
	runBD("config", "set", "types.custom", strings.Join(RequiredCustomTypes, ","))

	// Locate the embedded dolt DB directory the same way production code
	// does (internal/beads.(*BdStore).embeddedDoltDir), rather than
	// hand-deriving the sanitized database name from the prefix.
	metadataPath := filepath.Join(dir, ".beads", "metadata.json")
	dbName, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		t.Fatalf("ReadDoltDatabase(%s): ok=%v err=%v", metadataPath, ok, err)
	}
	doltDir := filepath.Join(dir, ".beads", "embeddeddolt", dbName)

	// Manufacture table drift: delete the "step" row directly from the
	// custom_types table while leaving config.yaml's CSV untouched.
	deleteCmd := exec.Command("dolt", "sql", "-q", "delete from custom_types where name='step'")
	deleteCmd.Dir = doltDir
	if out, err := deleteCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt sql delete: %v\n%s", err, out)
	}

	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("Run status = %v, want StatusError (table drift); message=%q", r.Status, r.Message)
	}
	if len(c.missing) != 0 {
		t.Fatalf("c.missing = %v, want empty — the CSV is complete, only the table is drifted", c.missing)
	}
	if !slices.Contains(c.tableMissing, "step") {
		t.Fatalf("c.tableMissing = %v, want it to contain %q", c.tableMissing, "step")
	}

	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	c2 := NewCustomTypesCheck(dir, "test")
	r2 := c2.Run(&CheckContext{CityPath: dir})
	if r2.Status != StatusOK {
		t.Fatalf("after Fix, Run status = %v, want StatusOK; message=%q", r2.Status, r2.Message)
	}

	out := runBD("create", "--type", "step", "drift healed check")
	if !strings.Contains(out, "Created issue") {
		t.Fatalf("bd create --type step failed after Fix, table still drifted: %s", out)
	}
}

// TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext is a regression
// test for ga-zxpfic: env-var scrubbing alone does not stop a machine-level
// dolt.shared-server config from leaking into the bd subprocesses this
// package's tests spawn. bd's config precedence falls through, as a last
// resort, to $HOME/.beads/config.yaml — so on any HOME that has
// dolt.shared-server: true set there (as fleet agent HOMEs do), scrubbing
// BEADS_DOLT_SERVER_PORT and friends changes nothing: bd still discovers the
// shared server via that config file, not an env var. Pinning a test-owned
// HOME via t.TempDir() removes the fallback file entirely, which is the only
// complete fix — this test asserts that isolation actually holds, not just
// that the drift check's Run/Fix behavior happens to look right.
func TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}

	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}

	home := testOwnedHome(t)

	dir := guardedTempDir(t)

	runBD := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bd", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	initOut := runBD("init", "--non-interactive", "-p", "tst2", "--skip-hooks", "--skip-agents")
	setOut := runBD("config", "set", "types.custom", strings.Join(RequiredCustomTypes, ","))

	homeConfigPath := filepath.Join(home, ".beads", "config.yaml")
	if _, err := os.Stat(homeConfigPath); !os.IsNotExist(err) {
		t.Fatalf("expected no config.yaml under test-owned HOME %s, but Stat returned err=%v", homeConfigPath, err)
	}

	metadataPath := filepath.Join(dir, ".beads", "metadata.json")
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState(%s): ok=%v err=%v", metadataPath, ok, err)
	}
	if meta.DoltMode != "embedded" {
		t.Fatalf("metadata.json dolt_mode = %q, want %q", meta.DoltMode, "embedded")
	}
	if meta.DoltDatabase == "" {
		t.Fatal("metadata.json dolt_database is empty, want it to match the embedded store")
	}

	for _, out := range []string{initOut, setOut} {
		if strings.Contains(out, "Dolt server at") {
			t.Fatalf("bd output leaked a shared-server connection: %s", out)
		}
		if strings.Contains(out, "shared-server mode is enabled") {
			t.Fatalf("bd output leaked shared-server mode: %s", out)
		}
	}
}

func TestCustomTypesCheck_RequiredTypesIncludeSpec(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "spec" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'spec'")
	}
}

// TestCustomTypesCheck_RequiredTypesIncludeConvergence verifies that
// "convergence" is in the required list. gc's convergence handler
// (internal/convergence/create.go) creates beads with Type="convergence"
// on every `gc converge create` call; if the type isn't registered in
// bd's types.custom, every convergence loop fails at creation with
// "invalid issue type: convergence".
func TestCustomTypesCheck_RequiredTypesIncludeConvergence(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "convergence" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'convergence' — gc's convergence handler requires this type")
	}
}

// TestMergeCustomTypes exercises the merge/dedup/preservation logic that
// backs CustomTypesCheck.Fix(). The regression it guards against is
// `--fix` overwriting user-defined types (which was the pre-PR behavior
// and still the failure mode if the merge is ever reverted).
func TestMergeCustomTypes(t *testing.T) {
	cases := []struct {
		name     string
		current  []string
		required []string
		want     []string
	}{
		{
			name:     "empty current gets required only",
			current:  nil,
			required: []string{"a", "b"},
			want:     []string{"a", "b"},
		},
		{
			name:     "preserves extra user types and appends missing required",
			current:  []string{"custom-foo", "molecule"},
			required: []string{"molecule", "spec", "convergence"},
			want:     []string{"custom-foo", "molecule", "spec", "convergence"},
		},
		{
			name:     "dedupes duplicates in current",
			current:  []string{"a", "a", "b", "a"},
			required: []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "drops empty and whitespace-only entries",
			current:  []string{"a", "", "  ", "b"},
			required: []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "trims whitespace around entries",
			current:  []string{" a ", "b\t"},
			required: []string{"a", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "dedupes when required entry already in current",
			current:  []string{"a", "b", "c"},
			required: []string{"b", "c", "d"},
			want:     []string{"a", "b", "c", "d"},
		},
		{
			name:     "preserves order of current entries",
			current:  []string{"z", "y", "x"},
			required: []string{"a"},
			want:     []string{"z", "y", "x", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contract.MergeCustomTypes(tc.current, tc.required)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("contract.MergeCustomTypes(%v, %v) = %v, want %v",
					tc.current, tc.required, got, tc.want)
			}
		})
	}
}

// TestParseCustomTypesJSON guards against the regression where
// `bd config get types.custom` on a store with an unset key returns
// "types.custom (not set)" and the old parser would persist that
// string as a fake custom type when Fix() merges. Switching to
// --json (+ this parser) eliminates the sentinel.
func TestParseCustomTypesJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "unset key returns nil",
			input: `{"key":"types.custom","value":""}`,
			want:  nil,
		},
		{
			name:  "whitespace-only value returns nil",
			input: `{"key":"types.custom","value":"   "}`,
			want:  nil,
		},
		{
			name:  "populated value splits on comma",
			input: `{"key":"types.custom","value":"molecule,spec,convergence"}`,
			want:  []string{"molecule", "spec", "convergence"},
		},
		{
			name:    "malformed JSON errors",
			input:   `not json`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCustomTypesJSON([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseCustomTypesJSON(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestTypesNotIn exercises the set-difference helper shared by the CSV
// completeness check and the custom_types table drift check.
func TestTypesNotIn(t *testing.T) {
	cases := []struct {
		name string
		want []string
		have []string
		out  []string
	}{
		{
			name: "nothing missing",
			want: []string{"a", "b"},
			have: []string{"a", "b", "c"},
			out:  nil,
		},
		{
			name: "some missing, preserves want order",
			want: []string{"a", "b", "c"},
			have: []string{"b"},
			out:  []string{"a", "c"},
		},
		{
			name: "everything missing when have is empty",
			want: []string{"a", "b"},
			have: nil,
			out:  []string{"a", "b"},
		},
		{
			name: "trims whitespace before comparing",
			want: []string{"a"},
			have: []string{" a "},
			out:  nil,
		},
		{
			name: "empty want yields nil regardless of have",
			want: nil,
			have: []string{"a"},
			out:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := typesNotIn(tc.want, tc.have)
			if !reflect.DeepEqual(got, tc.out) {
				t.Errorf("typesNotIn(%v, %v) = %v, want %v", tc.want, tc.have, got, tc.out)
			}
		})
	}
}

func TestCustomTypesCheck_RequiredTypesComplete(t *testing.T) {
	expected := map[string]bool{
		"molecule": true, "convoy": true, "message": true,
		"event": true, "gate": true, "merge-request": true,
		"agent": true, "role": true, "rig": true,
		"session": true, "spec": true, "convergence": true,
		"step": true,
	}
	for _, typ := range RequiredCustomTypes {
		if !expected[typ] {
			t.Errorf("unexpected required type: %q", typ)
		}
		delete(expected, typ)
	}
	for typ := range expected {
		t.Errorf("missing required type: %q", typ)
	}
}

// TestScopeEmbeddedDoltStoreClassification pins the GC-REG-23 scope
// classification: either signal — metadata.json's dolt_mode (what the
// events/convoy native-store gate reads) or the config.yaml dolt.mode the
// scope pins for itself — makes a scope embedded for the custom-types
// fallback, because the failing population (observed live 2026-10-03) is
// split stores: rigs migrated off embedded keep `dolt.mode: embedded` in
// config.yaml while their metadata already says server.
func TestScopeEmbeddedDoltStoreClassification(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeMetadata := func(mode string) {
		t.Helper()
		meta := fmt.Sprintf(`{"backend":"dolt","database":"dolt","dolt_mode":%q,"dolt_database":"db"}`, mode)
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig := func(body string) {
		t.Helper()
		if body == "" {
			os.Remove(filepath.Join(beadsDir, "config.yaml"))
			return
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name      string
		metadata  string
		config    string
		wantEmbed bool
	}{
		{name: "metadata embedded", metadata: "embedded", config: "", wantEmbed: true},
		{name: "metadata local", metadata: "local", config: "", wantEmbed: true},
		{
			// The live GC-REG-23 shape: metadata already migrated to server,
			// config.yaml still pins dolt.mode: embedded.
			name:      "split store config embedded metadata server",
			metadata:  "server",
			config:    "dolt.mode: embedded\n",
			wantEmbed: true,
		},
		{
			name:      "nested config embedded",
			metadata:  "server",
			config:    "dolt:\n  mode: embedded\n",
			wantEmbed: true,
		},
		{name: "metadata server no config", metadata: "server", config: "", wantEmbed: false},
		{name: "both server", metadata: "server", config: "dolt.mode: server\n", wantEmbed: false},
		{name: "no signals", metadata: "", config: "", wantEmbed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeMetadata(tc.metadata)
			writeConfig(tc.config)
			got, via := scopeEmbeddedDoltStore(dir)
			if got != tc.wantEmbed {
				t.Fatalf("scopeEmbeddedDoltStore(%s) = %v (via %q), want %v", tc.name, got, via, tc.wantEmbed)
			}
			if got && via == "" {
				t.Fatal("embedded classification must name the signal it came from")
			}
			if !got && via != "" {
				t.Fatalf("non-embedded scope must have empty via, got %q", via)
			}
		})
	}
}

// fakeBDScope builds a scope directory whose .beads/ carries the given
// metadata mode and config.yaml body, plus a fake `bd` first on PATH that
// answers `config get --json types.custom` with an empty DB-config value and
// `types --json` with the full required set — the exact answers real bd gives
// for the GC-REG-23 stores (DB config table without the key, complete
// custom_types table). It returns the scope dir.
func fakeBDScope(t *testing.T, metadataMode, configBody string, tableTypes []string) string {
	t.Helper()

	dir := guardedTempDir(t)
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"backend":"dolt","database":"dolt","dolt_mode":%q,"dolt_database":"db"}`, metadataMode)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if configBody != "" {
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tableJSON, err := json.Marshal(tableTypes)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  'config get') printf '{\"key\":\"types.custom\",\"schema_version\":1,\"value\":\"\"}\\n'; exit 0 ;;\n" +
		"  'types --json') printf '{\"core_types\":[],\"custom_types\":" + string(tableJSON) + ",\"schema_version\":1}\\n'; exit 0 ;;\n" +
		"esac\n" +
		"echo \"fake bd: unexpected args: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return dir
}

// TestCustomTypesCheck_EmbeddedConfigYAMLFallback proves the GC-REG-23 fix on
// the live failure shape: a scope whose DB config table carries no
// types.custom key (bd config get answers empty) but whose config.yaml pins
// dolt.mode: embedded and a complete types.custom CSV must report OK via the
// config.yaml fallback — never "missing 13 custom types" — while the
// custom_types table stays the independently checked source of truth.
func TestCustomTypesCheck_EmbeddedConfigYAMLFallback(t *testing.T) {
	csv := strings.Join(RequiredCustomTypes, ",")
	dir := fakeBDScope(t, "server", "dolt.mode: embedded\ntypes.custom: "+csv+"\n", RequiredCustomTypes)

	c := NewCustomTypesCheck(dir, "rig")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("Run status = %v, want OK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "config.yaml fallback") {
		t.Fatalf("message = %q, want it to name the config.yaml fallback", r.Message)
	}
	if len(c.missing) != 0 {
		t.Fatalf("c.missing = %v, want empty", c.missing)
	}
}

// TestCustomTypesCheck_EmbeddedFallbackReportsTrueGaps keeps the fallback
// honest: an embedded-mode scope whose config.yaml CSV itself lacks a type is
// reported with exactly that gap (missing 1: step), not with the full
// 13-type absence the empty bd-config answer alone would imply.
func TestCustomTypesCheck_EmbeddedFallbackReportsTrueGaps(t *testing.T) {
	short := make([]string, 0, len(RequiredCustomTypes)-1)
	for _, typ := range RequiredCustomTypes {
		if typ != "step" {
			short = append(short, typ)
		}
	}
	// The custom_types table knows every required type, so only the CSV gap
	// should surface.
	dir := fakeBDScope(t, "embedded", "types.custom: "+strings.Join(short, ",")+"\n", RequiredCustomTypes)

	c := NewCustomTypesCheck(dir, "rig")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("Run status = %v, want Error; message=%q", r.Status, r.Message)
	}
	if len(c.missing) != 1 || c.missing[0] != "step" {
		t.Fatalf("c.missing = %v, want [step]", c.missing)
	}
	if strings.Contains(r.Message, fmt.Sprintf("missing %d custom", len(RequiredCustomTypes))) {
		t.Fatalf("message reports the full %d-type absence, want the single true gap: %q", len(RequiredCustomTypes), r.Message)
	}
	if !strings.Contains(r.Message, "config.yaml fallback") {
		t.Fatalf("message = %q, want it to name the config.yaml fallback", r.Message)
	}
}

// TestCustomTypesCheck_ServerModeUnchanged pins the other half of the
// GC-REG-23 contract: a server-mode scope (no embedded signal on either
// metadata or config.yaml) with an unset DB-config key keeps the pre-fix
// behavior — the empty answer is reported as missing types, because for
// server scopes the DB config table IS where gc's lifecycle writes the key,
// and the drift it signals is real and fixable.
func TestCustomTypesCheck_ServerModeUnchanged(t *testing.T) {
	// config.yaml without any dolt.mode, types.custom absent from the file.
	dir := fakeBDScope(t, "server", "issue_prefix: rig\n", RequiredCustomTypes)

	c := NewCustomTypesCheck(dir, "rig")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("Run status = %v, want Error; message=%q", r.Status, r.Message)
	}
	if len(c.missing) != len(RequiredCustomTypes) {
		t.Fatalf("c.missing = %v, want all %d required types (server-mode drift is real)", c.missing, len(RequiredCustomTypes))
	}
	if strings.Contains(r.Message, "config.yaml fallback") {
		t.Fatalf("message = %q, server-mode scopes must not use the fallback", r.Message)
	}
}

// TestCustomTypesCheck_EmbeddedRealBDConfigYAMLOnly runs the check against a
// real bd-initialized embedded store whose types.custom exists ONLY in
// config.yaml (never `bd config set`): the store's custom_types table is
// complete (bd materializes it from the yaml key), so the check must pass
// through the fallback path. Skipped unless bd and dolt are on PATH, like
// TestCustomTypesCheck_TableDrift.
func TestCustomTypesCheck_EmbeddedRealBDConfigYAMLOnly(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}
	testOwnedHome(t)

	dir := guardedTempDir(t)
	initCmd := exec.Command("bd", "init", "--non-interactive", "-p", "tst3", "--skip-hooks", "--skip-agents")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v\n%s", err, out)
	}

	// types.custom ONLY in config.yaml — the embedded store shape GC-REG-23
	// describes; no `bd config set` ever ran.
	cfgPath := filepath.Join(dir, ".beads", "config.yaml")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("types.custom: " + strings.Join(RequiredCustomTypes, ",") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	c := NewCustomTypesCheck(dir, "rig")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("Run status = %v, want OK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "config.yaml fallback") {
		t.Fatalf("message = %q, want it to name the config.yaml fallback", r.Message)
	}
}
