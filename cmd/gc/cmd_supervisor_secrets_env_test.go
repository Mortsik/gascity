package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// The tests in this file pin the no-plaintext-secrets contract for generated
// systemd units: a unit file is readable by anything running as the user
// (`systemctl --user cat`, `systemctl --user show -p Environment`), so
// credential values must live only in ${GC_HOME}/secrets.env (0600), which
// the unit references via EnvironmentFile. Values in these fixtures are
// fake; real tokens must never appear in test output.

func TestBuildSupervisorServiceDataUnitEnvExcludesProviderCreds(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd unit embedding only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example.test")
	t.Setenv("LC_ALL", "en_US.UTF-8")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	full := supervisorServiceEnvMap(data.ExtraEnv)
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-ant-token",
		"ANTHROPIC_BASE_URL":   "https://anthropic.example.test",
		"LC_ALL":               "en_US.UTF-8",
	} {
		if full[key] != want {
			t.Fatalf("ExtraEnv[%s] = %q, want %q (all env: %#v)", key, full[key], want, full)
		}
	}
	unit := supervisorServiceEnvMap(data.UnitExtraEnv)
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
		if _, ok := unit[key]; ok {
			t.Fatalf("UnitExtraEnv must not embed provider credential %s: %#v", key, unit)
		}
	}
	if unit["LC_ALL"] != "en_US.UTF-8" {
		t.Fatalf("UnitExtraEnv[LC_ALL] = %q, want non-credential env to stay embedded", unit["LC_ALL"])
	}
	if !data.LoadSecretsEnvFile {
		t.Fatal("LoadSecretsEnvFile = false without the provider-creds opt-out")
	}
	if want := filepath.Join(gcHome, supervisorSecretsEnvFileName); data.SecretsEnvFile != want {
		t.Fatalf("SecretsEnvFile = %q, want %q", data.SecretsEnvFile, want)
	}

	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatalf("render systemd template: %v", err)
	}
	if strings.Contains(content, "sk-ant-token") {
		t.Fatalf("generated unit leaked the token value:\n%s", content)
	}
	if !strings.Contains(content, "EnvironmentFile=-"+data.SecretsEnvFile) {
		t.Fatalf("generated unit missing EnvironmentFile reference:\n%s", content)
	}
}

// TestBuildSupervisorServiceDataUnitEnvExcludesFileTierKeys pins that keys
// sourced from ${GC_HOME}/secrets.env are not re-embedded into the unit:
// systemd's EnvironmentFile= already sets them, and embedding them would
// copy machine-local values into the readable unit.
func TestBuildSupervisorServiceDataUnitEnvExcludesFileTierKeys(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd unit embedding only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("LC_ALL", "")
	t.Setenv("LANG", "")

	writeSupervisorSecretsEnvFile(t, "LC_ALL=en_GB.UTF-8\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	if got := supervisorServiceEnvMap(data.ExtraEnv)["LC_ALL"]; got != "en_GB.UTF-8" {
		t.Fatalf("ExtraEnv[LC_ALL] = %q, want file value", got)
	}
	if _, ok := supervisorServiceEnvMap(data.UnitExtraEnv)["LC_ALL"]; ok {
		t.Fatal("UnitExtraEnv must not embed file-tier keys; EnvironmentFile loads them")
	}
}

// TestBuildSupervisorServiceDataOmitProviderCredsKeepsUnitEmbedding pins the
// opt-out semantics: with GC_SUPERVISOR_OMIT_PROVIDER_CREDS=1 the unit does
// not reference the secrets file either (the operator delivers credentials
// another way, per the flag's contract), and file-tier opt-in values stay
// embedded exactly as before so nothing is lost at runtime.
func TestBuildSupervisorServiceDataOmitProviderCredsKeepsUnitEmbedding(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd unit embedding only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", "CUSTOM_PROVIDER_TOKEN")
	t.Setenv(supervisorOmitProviderCredsEnv, "1")

	writeSupervisorSecretsEnvFile(t, "CUSTOM_PROVIDER_TOKEN=custom-from-file\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	if data.LoadSecretsEnvFile {
		t.Fatal("LoadSecretsEnvFile = true under the provider-creds opt-out; the unit must not consult the secrets file then")
	}
	if got := supervisorServiceEnvMap(data.UnitExtraEnv)["CUSTOM_PROVIDER_TOKEN"]; got != "custom-from-file" {
		t.Fatalf("UnitExtraEnv[CUSTOM_PROVIDER_TOKEN] = %q, want the file value to stay embedded under the opt-out", got)
	}
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatalf("render systemd template: %v", err)
	}
	if strings.Contains(content, "EnvironmentFile=") {
		t.Fatalf("unit must not reference the secrets file under the opt-out:\n%s", content)
	}
}

func TestPersistSupervisorSecretsEnvFile(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	t.Run("merges in place and preserves unrelated lines", func(t *testing.T) {
		writeSupervisorSecretsEnvFile(t, "# machine-local secrets\nANTHROPIC_AUTH_TOKEN=sk-old\nUNRELATED_KEY=keep-me\n")

		changed, err := persistSupervisorSecretsEnvFile([]supervisorServiceEnvVar{
			{Name: "ANTHROPIC_AUTH_TOKEN", Value: "sk-new"},
			{Name: "OPENAI_API_KEY", Value: "sk-openai"},
		})
		if err != nil {
			t.Fatalf("persistSupervisorSecretsEnvFile: %v", err)
		}
		if !changed {
			t.Fatal("changed = false on a real value update")
		}

		got, err := os.ReadFile(supervisorSecretsEnvFilePath())
		if err != nil {
			t.Fatal(err)
		}
		want := "# machine-local secrets\nANTHROPIC_AUTH_TOKEN=sk-new\nUNRELATED_KEY=keep-me\n\nOPENAI_API_KEY=sk-openai\n"
		if string(got) != want {
			t.Fatalf("secrets file = %q, want %q", got, want)
		}
		entries := supervisorSecretsEnvFileEntries()
		if entries["ANTHROPIC_AUTH_TOKEN"] != "sk-new" || entries["OPENAI_API_KEY"] != "sk-openai" || entries["UNRELATED_KEY"] != "keep-me" {
			t.Fatalf("parsed entries lost content: %#v", entries)
		}
	})

	t.Run("idempotent rewrite reports no change", func(t *testing.T) {
		entries := []supervisorServiceEnvVar{{Name: "ANTHROPIC_AUTH_TOKEN", Value: "sk-stable"}}
		if _, err := persistSupervisorSecretsEnvFile(entries); err != nil {
			t.Fatal(err)
		}
		changed, err := persistSupervisorSecretsEnvFile(entries)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatal("changed = true rewriting identical content")
		}
	})

	t.Run("creates the file 0600", func(t *testing.T) {
		if _, err := persistSupervisorSecretsEnvFile([]supervisorServiceEnvVar{{Name: "ANTHROPIC_AUTH_TOKEN", Value: "sk-fresh"}}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(supervisorSecretsEnvFilePath())
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("secrets file mode = %o, want 0600", perm)
		}
	})

	t.Run("refuses newline-bearing values", func(t *testing.T) {
		_, err := persistSupervisorSecretsEnvFile([]supervisorServiceEnvVar{{Name: "ANTHROPIC_AUTH_TOKEN", Value: "one\ntwo"}})
		if err == nil {
			t.Fatal("newline value must be refused, not truncated into the file")
		}
		if strings.Contains(err.Error(), "one\ntwo") {
			t.Fatalf("error leaked the value: %v", err)
		}
	})

	t.Run("no entries is a no-op", func(t *testing.T) {
		changed, err := persistSupervisorSecretsEnvFile(nil)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatal("changed = true with no entries")
		}
	})
}

func TestSupervisorSystemdUnitSecretEnv(t *testing.T) {
	unit := `[Unit]
Description=Gas City machine supervisor

[Service]
ExecStart=/usr/local/bin/gc supervisor run
Environment=GC_HOME="/home/user/.gc"
Environment=ANTHROPIC_AUTH_TOKEN="sk-live-unit"
Environment=OPENAI_API_KEY=sk-raw-value
Environment=CUSTOM_TOKEN="sk-custom"
Environment=EMPTY_CRED=
Environment=GEMINI_API_KEY="sk-one"
Environment=GEMINI_API_KEY="sk-two"
`
	got := supervisorSystemdUnitSecretEnv(unit)
	m := supervisorServiceEnvMap(got)
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-live-unit",
		"OPENAI_API_KEY":       "sk-raw-value",
		"GEMINI_API_KEY":       "sk-two",
	} {
		if m[key] != want {
			t.Fatalf("extracted[%s] = %q, want %q (all: %#v)", key, m[key], want, m)
		}
	}
	for _, key := range []string{"CUSTOM_TOKEN", "EMPTY_CRED", "GC_HOME"} {
		if _, ok := m[key]; ok {
			t.Fatalf("extraction must skip non-provider or empty key %s: %#v", key, m)
		}
	}
}

// TestInstallSupervisorSystemdMigratesEmbeddedSecrets is the acceptance test
// for existing installs: an older gc wrote the credential straight into the
// unit, and a reinstall from a shell without the token exported must move
// that value into ${GC_HOME}/secrets.env before the new unit drops the
// Environment= line — otherwise the credential would be the only thing the
// upgrade deletes.
func TestInstallSupervisorSystemdMigratesEmbeddedSecrets(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd install path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	currentBinary := filepath.Join(homeDir, "bin", "gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	setSupervisorInstallForceForTest(t, false)

	unitPath := supervisorSystemdServicePath()
	existingUnit := "[Unit]\nDescription=Gas City machine supervisor\n\n[Service]\n" +
		"ExecStart=" + currentBinary + " supervisor run\n" +
		"Environment=ANTHROPIC_AUTH_TOKEN=\"sk-live-unit\"\n"
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(existingUnit), 0o600); err != nil {
		t.Fatal(err)
	}

	// The reinstall shell carries no provider credentials (ExtraEnv is empty),
	// so the only surviving copy lives in the unit being replaced.
	data := supervisorInstallGuardServiceData(gcHome, currentBinary)
	data.SecretsEnvFile = supervisorSecretsEnvFilePath()
	data.LoadSecretsEnvFile = true

	stubSupervisorSystemctlForTest(t)
	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd = %d, stderr:\n%s", code, stderr.String())
	}

	unitContent, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unitContent), "sk-live-unit") {
		t.Fatalf("installed unit still embeds the credential:\n%s", unitContent)
	}
	if !strings.Contains(string(unitContent), "EnvironmentFile=-"+supervisorSecretsEnvFilePath()) {
		t.Fatalf("installed unit missing EnvironmentFile reference:\n%s", unitContent)
	}

	entries := supervisorSecretsEnvFileEntries()
	if entries["ANTHROPIC_AUTH_TOKEN"] != "sk-live-unit" {
		t.Fatalf("secrets.env ANTHROPIC_AUTH_TOKEN = %q, want the value migrated from the old unit (entries: %#v)", entries["ANTHROPIC_AUTH_TOKEN"], entries)
	}

	if out := stdout.String(); strings.Contains(out, "sk-live-unit") {
		t.Fatalf("install output leaked the credential value:\n%s", out)
	} else if !strings.Contains(out, supervisorSecretsEnvFilePath()) || !strings.Contains(out, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("install output missing the migration note (path + key name):\n%s", out)
	}
}

// TestInstallSupervisorSystemdCapturesShellSecretsToFile covers the fresh
// install path: a token exported in the calling shell reaches
// ${GC_HOME}/secrets.env, never the unit.
func TestInstallSupervisorSystemdCapturesShellSecretsToFile(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd install path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	currentBinary := filepath.Join(homeDir, "bin", "gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	setSupervisorInstallForceForTest(t, false)

	data := supervisorInstallGuardServiceData(gcHome, currentBinary)
	data.ExtraEnv = []supervisorServiceEnvVar{
		{Name: "ANTHROPIC_AUTH_TOKEN", Value: "sk-from-shell"},
		{Name: "LC_ALL", Value: "en_US.UTF-8"},
	}
	data.UnitExtraEnv = []supervisorServiceEnvVar{{Name: "LC_ALL", Value: "en_US.UTF-8"}}
	data.SecretsEnvFile = supervisorSecretsEnvFilePath()
	data.LoadSecretsEnvFile = true

	stubSupervisorSystemctlForTest(t)
	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd = %d, stderr:\n%s", code, stderr.String())
	}

	unitContent, err := os.ReadFile(supervisorSystemdServicePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unitContent), "sk-from-shell") {
		t.Fatalf("installed unit embeds the shell credential:\n%s", unitContent)
	}
	if !strings.Contains(string(unitContent), `Environment=LC_ALL="en_US.UTF-8"`) {
		t.Fatalf("installed unit lost non-credential env:\n%s", unitContent)
	}
	entries := supervisorSecretsEnvFileEntries()
	if entries["ANTHROPIC_AUTH_TOKEN"] != "sk-from-shell" {
		t.Fatalf("secrets.env ANTHROPIC_AUTH_TOKEN = %q, want the captured shell value (entries: %#v)", entries["ANTHROPIC_AUTH_TOKEN"], entries)
	}
}

func stubSupervisorSystemctlForTest(t *testing.T) {
	t.Helper()
	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	oldLingerEnabled := supervisorLingerEnabled
	supervisorSystemctlRun = func(_ ...string) error { return nil }
	supervisorSystemctlActive = func(_ string) bool { return false }
	supervisorLingerEnabled = func(_ string) bool { return true }
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
		supervisorLingerEnabled = oldLingerEnabled
	})
}

// TestRenderedSystemdUnitPassesSystemdAnalyzeVerify is the documented unit
// validity check for the acceptance criteria: the generated unit — including
// the optional EnvironmentFile reference — must verify clean. Skipped where
// systemd-analyze is absent (containers, non-linux dev hosts).
func TestRenderedSystemdUnitPassesSystemdAnalyzeVerify(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd unit verification only applies on linux")
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	data := &supervisorServiceData{
		GCPath:  exe,
		LogPath: filepath.Join(dir, ".gc", "supervisor.log"),
		GCHome:  filepath.Join(dir, ".gc"),
		Path:    "/usr/local/bin:/usr/bin:/bin",
		UnitExtraEnv: []supervisorServiceEnvVar{
			{Name: "LC_ALL", Value: "en_US.UTF-8"},
		},
		SecretsEnvFile:     filepath.Join(dir, ".gc", supervisorSecretsEnvFileName),
		LoadSecretsEnvFile: true,
		PortInUseExitCode:  supervisorExitCodePortInUse,
	}
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatalf("render systemd template: %v", err)
	}
	unitPath := filepath.Join(dir, "gascity-supervisor.service")
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(analyze, "verify", unitPath).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify failed: %v\nunit:\n%s\noutput:\n%s", err, content, out)
	}
}
