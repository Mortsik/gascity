package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// reuseHarness mints a fresh scoped leg per scan (the production shape: the
// scoped store is opened and closed by every scan), scripts the change token,
// and drives the reuse clock.
type reuseHarness struct {
	t        *testing.T
	mu       sync.Mutex
	minted   []*closeCountingStore
	legs     int // 1 = scoped only, 2 = scoped + shared binding
	binding  *closeCountingStore
	readyErr error

	token      string
	tokenCalls int
	now        time.Time
}

func newReuseHarness(t *testing.T) *reuseHarness {
	t.Helper()
	h := &reuseHarness{t: t, legs: 1, token: "hash-a", now: time.Unix(1_800_000_000, 0)}
	controlReadyReuseMu.Lock()
	controlReadyReuseCache = map[string]controlReadyReuseEntry{}
	controlReadyReuseMu.Unlock()
	t.Setenv(controlReadyReuseMaxAgeEnv, "")

	prevToken, prevNow, prevReusable := controlReadyChangeTokenFn, controlReadyNow, controlReadyLegReusable
	// The fixtures are MemStore-backed; production reuses *beads.BdStore legs only.
	controlReadyLegReusable = func(beads.Store) bool { return true }
	controlReadyChangeTokenFn = func(_, _ string, _ *config.City) string {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.tokenCalls++
		return h.token
	}
	controlReadyNow = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	t.Cleanup(func() {
		controlReadyChangeTokenFn, controlReadyNow, controlReadyLegReusable = prevToken, prevNow, prevReusable
		controlReadyReuseMu.Lock()
		controlReadyReuseCache = map[string]controlReadyReuseEntry{}
		controlReadyReuseMu.Unlock()
	})

	installControlReadyLegSourcesFn(t, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		f := newCloseCountingStore(t, true)
		f.readyErr = h.readyErr
		h.mu.Lock()
		h.minted = append(h.minted, f)
		h.mu.Unlock()
		switch h.legs {
		case 2:
			return []beads.Store{f, h.binding}, []beads.Store{f}, nil
		case 0: // relocated: the scan reads only the shared binding and owns nothing
			return []beads.Store{h.binding}, nil, nil
		}
		return []beads.Store{f}, []beads.Store{f}, nil
	})
	return h
}

func (h *reuseHarness) set(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn()
}

func (h *reuseHarness) reads() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int64
	for _, f := range h.minted {
		n += f.readies()
	}
	return n
}

func (h *reuseHarness) scan(dir string) []beads.Bead {
	h.t.Helper()
	ready, opened, err := controlReadyLegsReady(dir, dir, nil)
	if err != nil || !opened {
		h.t.Fatalf("controlReadyLegsReady = opened %t, err %v; want opened, nil", opened, err)
	}
	return ready
}

func TestControlReadyReuseSkipsTheReadWhileTheDatabaseIsUnchanged(t *testing.T) {
	h := newReuseHarness(t)
	dir := t.TempDir()

	first := h.scan(dir)
	h.set(func() { h.now = h.now.Add(10 * time.Second) })
	second := h.scan(dir)

	if got := h.reads(); got != 1 {
		t.Fatalf("Ready reads = %d, want 1: an unchanged token inside the max age must reuse the first answer", got)
	}
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Fatalf("reused answer %v differs from the fresh answer %v", second, first)
	}
	for i, f := range h.minted {
		if f.closes() != 1 {
			t.Fatalf("scan %d's scoped leg closed %d times, want 1 even when its read was skipped", i, f.closes())
		}
	}
}

func TestControlReadyReuseReadsAfreshWhenTheTokenChanges(t *testing.T) {
	h := newReuseHarness(t)
	dir := t.TempDir()

	h.scan(dir)
	h.set(func() { h.token = "hash-b" })
	h.scan(dir)
	h.scan(dir)

	if got := h.reads(); got != 2 {
		t.Fatalf("Ready reads = %d, want 2: a changed token forces one fresh read, then that answer is reused", got)
	}
}

func TestControlReadyReuseExpiresAtTheMaxAge(t *testing.T) {
	h := newReuseHarness(t)
	dir := t.TempDir()

	h.scan(dir)
	h.set(func() { h.now = h.now.Add(controlReadyReuseDefaultMaxAge) })
	h.scan(dir)

	if got := h.reads(); got != 2 {
		t.Fatalf("Ready reads = %d, want 2: an answer as old as the max age (a defer_until may have passed) must be re-read", got)
	}
}

func TestControlReadyReuseNeedsAToken(t *testing.T) {
	h := newReuseHarness(t)
	dir := t.TempDir()

	h.scan(dir)
	h.set(func() { h.token = "" })
	h.scan(dir)
	h.set(func() { h.token = "hash-a" })
	h.scan(dir)

	if got := h.reads(); got != 3 {
		t.Fatalf("Ready reads = %d, want 3: no token means a fresh read, and it drops the cached answer so a returning token cannot resurrect it", got)
	}
}

func TestControlReadyReuseKillSwitch(t *testing.T) {
	h := newReuseHarness(t)
	t.Setenv(controlReadyReuseMaxAgeEnv, "0")
	dir := t.TempDir()

	h.scan(dir)
	h.scan(dir)

	if got := h.reads(); got != 2 {
		t.Fatalf("Ready reads = %d, want 2 with %s=0", got, controlReadyReuseMaxAgeEnv)
	}
	if h.tokenCalls != 0 {
		t.Fatalf("token probed %d times with reuse disabled, want 0", h.tokenCalls)
	}
}

func TestControlReadyReuseCoversOnlyTheScopedLegOfAFederatedScope(t *testing.T) {
	h := newReuseHarness(t)
	h.legs = 2
	h.binding = newCloseCountingStore(t, false)
	dir := t.TempDir()

	h.scan(dir)
	h.scan(dir)

	if got := h.reads(); got != 1 {
		t.Fatalf("scoped Ready reads = %d, want 1: an unchanged token reuses the scope's own bd leg", got)
	}
	if got := h.binding.readies(); got != 2 {
		t.Fatalf("binding Ready reads = %d, want 2: the token hashes the scope's database only, so the shared binding leg reads every scan", got)
	}
}

func TestControlReadyReuseNeverAppliesToARelocatedScope(t *testing.T) {
	h := newReuseHarness(t)
	h.legs = 0
	h.binding = newCloseCountingStore(t, true)
	dir := t.TempDir()

	h.scan(dir)
	h.scan(dir)

	if got := h.binding.readies(); got != 2 {
		t.Fatalf("binding Ready reads = %d, want 2: a relocated scope owns no leg, so nothing is reused", got)
	}
	if h.tokenCalls != 0 {
		t.Fatalf("token probed %d times for a relocated scope, want 0", h.tokenCalls)
	}
}

func TestControlReadyReuseOnlyForBdStoreLegs(t *testing.T) {
	if controlReadyLegReusableDefault(beads.NewMemStore()) {
		t.Fatal("a MemStore leg was judged reusable; only *beads.BdStore legs read through the env-derived endpoint")
	}
	if !controlReadyLegReusableDefault(&beads.BdStore{}) {
		t.Fatal("a *beads.BdStore leg was judged not reusable")
	}
}

func TestControlReadyReuseNeverCachesAFailedRead(t *testing.T) {
	h := newReuseHarness(t)
	dir := t.TempDir()

	h.set(func() { h.readyErr = errors.New("dolt went away") })
	if _, _, err := controlReadyLegsReady(dir, dir, nil); err == nil {
		t.Fatal("controlReadyLegsReady swallowed the leg read error")
	}
	h.set(func() { h.readyErr = nil })
	h.scan(dir)

	if got := h.reads(); got != 2 {
		t.Fatalf("Ready reads = %d, want 2: a failed read must not leave an answer behind to reuse", got)
	}
}

func TestControlReadyReuseMaxAgeOverride(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      controlReadyReuseDefaultMaxAge,
		"0":     0,
		"5s":    5 * time.Second,
		"bogus": controlReadyReuseDefaultMaxAge,
		"-1s":   controlReadyReuseDefaultMaxAge,
	} {
		t.Setenv(controlReadyReuseMaxAgeEnv, raw)
		if got := controlReadyReuseMaxAge(); got != want {
			t.Errorf("%s=%q: max age %v, want %v", controlReadyReuseMaxAgeEnv, raw, got, want)
		}
	}
}

func TestControlReadyReuseTokenFailureDropsTheEntryEvenWhenTheReadFails(t *testing.T) {
	h := newReuseHarness(t)
	dir := t.TempDir()

	h.scan(dir)
	h.set(func() { h.token = ""; h.readyErr = errors.New("dolt went away") })
	if _, _, err := controlReadyLegsReady(dir, dir, nil); err == nil {
		t.Fatal("controlReadyLegsReady swallowed the leg read error")
	}
	h.set(func() { h.token = "hash-a"; h.readyErr = nil })
	h.scan(dir)

	if got := h.reads(); got != 3 {
		t.Fatalf("Ready reads = %d, want 3: a failed token probe must drop the old answer before the read, so a recovered token reads afresh", got)
	}
}

func TestControlReadyEndpointFromEnv(t *testing.T) {
	served := map[string]string{"BEADS_DOLT_SERVER_HOST": "127.0.0.1", "BEADS_DOLT_SERVER_PORT": "3307", "BEADS_DOLT_SERVER_USER": "gc", "BEADS_DOLT_PASSWORD": "pw"}
	with := func(base map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	for name, tc := range map[string]struct {
		env      map[string]string
		database string
		want     controlReadyEndpoint
		ok       bool
	}{
		"host/port, database from metadata": {served, "rig", controlReadyEndpoint{host: "127.0.0.1", port: "3307", user: "gc", password: "pw", database: "rig"}, true},
		"env database wins":                 {with(served, "BEADS_DOLT_SERVER_DATABASE", "envdb"), "rig", controlReadyEndpoint{host: "127.0.0.1", port: "3307", user: "gc", password: "pw", database: "envdb"}, true},
		"socket":                            {map[string]string{"BEADS_DOLT_SERVER_SOCKET": "/run/dolt.sock"}, "rig", controlReadyEndpoint{socket: "/run/dolt.sock", user: "root", database: "rig"}, true},
		"doltlite backend":                  {with(served, "BEADS_BACKEND", "doltlite"), "rig", controlReadyEndpoint{}, false},
		"doltlite gc backend":               {with(served, "GC_BEADS_BACKEND", " DoltLite "), "rig", controlReadyEndpoint{}, false},
		"no port, no socket":                {with(served, "BEADS_DOLT_SERVER_PORT", ""), "rig", controlReadyEndpoint{}, false},
		"no database":                       {served, "", controlReadyEndpoint{}, false},
	} {
		got, ok := controlReadyEndpointFromEnv(tc.env, tc.database)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: endpoint = %+v, %t; want %+v, %t", name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestControlReadyBdLegEnvFollowsTheRigEndpointOverride pins the token to the
// endpoint the leg's bd actually uses when a legacy rig override and the city
// endpoint disagree: the token's environment IS the control bd runner's.
func TestControlReadyBdLegEnvFollowsTheRigEndpointOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: demo\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "city-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: repo\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "rig-db.example.com", DoltPort: "3308"}}}

	got, err := controlReadyBdLegEnv(resolveStoreScopeRoot(cityPath, rigDir), cityPath, cfg)
	if err != nil {
		t.Fatalf("controlReadyBdLegEnv: %v", err)
	}
	want, err := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithErrorNoRecovery: %v", err)
	}
	gotEndpoint, gotOK := controlReadyEndpointFromEnv(got, "repo")
	wantEndpoint, wantOK := controlReadyEndpointFromEnv(want, "repo")
	if gotOK != wantOK || gotEndpoint != wantEndpoint {
		t.Fatalf("token endpoint %+v (%t) differs from the control bd runner's %+v (%t)", gotEndpoint, gotOK, wantEndpoint, wantOK)
	}
	if gotEndpoint.host != "rig-db.example.com" || gotEndpoint.port != "3308" {
		t.Fatalf("token endpoint = %s:%s, want the rig override rig-db.example.com:3308 the rig's bd connects to (not the city's city-db.example.com:4406)", gotEndpoint.host, gotEndpoint.port)
	}

	cityEnv, err := controlReadyBdLegEnv(resolveStoreScopeRoot(cityPath, cityPath), cityPath, cfg)
	if err != nil {
		t.Fatalf("controlReadyBdLegEnv(city): %v", err)
	}
	if cityEnv["BEADS_DOLT_SERVER_HOST"] != "city-db.example.com" || cityEnv["BEADS_DOLT_SERVER_PORT"] != "4406" {
		t.Fatalf("city scope env endpoint = %s:%s, want city-db.example.com:4406", cityEnv["BEADS_DOLT_SERVER_HOST"], cityEnv["BEADS_DOLT_SERVER_PORT"])
	}
}
