package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestProviderAdmissionStartDoesNotMaterializeAttempt(t *testing.T) {
	city := t.TempDir()
	dir := filepath.Join(city, ".gc", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider-health-required.json"), []byte(`{"schema_version":1,"providers":["custom"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, sp := beads.NewMemStore(), runtime.NewFake()
	mgr := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(city))
	for i := 0; i < 3; i++ {
		h, err := NewSessionHandle(SessionHandleConfig{Manager: mgr, Session: SessionSpec{Template: "worker", Provider: "custom", Command: "fake", WorkDir: city}})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Start(context.Background()); err == nil {
			t.Fatal("unavailable provider admitted")
		}
	}
	attempts, err := store.ListByLabel(sessionpkg.LabelSession, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("provider denial created %d attempts", len(attempts))
	}
}
