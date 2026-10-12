package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestWaitIdleNudgeDeliversForClaudeDerivativeFamily pins the 2026-10-12 fix
// (finding k): the wait-idle live delivery path gated on providerKind=="claude"
// exactly, so claude-* derivatives (claude-glm fleet slots) never received the
// post-turn claim nudge — text landed in the composer without a submit while
// the queue drained into paste-only injections. Derivatives must deliver like
// claude; non-claude families stay excluded.
func TestWaitIdleNudgeDeliversForClaudeDerivativeFamily(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     bool
	}{
		{"claude", true},
		{"claude-glm", true},
		{"claude-haiku", true},
		{"codex", false},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			city := t.TempDir()
			store, sp := beads.NewMemStore(), runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp, WithCityPath(city))
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Provider: tc.provider, Command: "fake", WorkDir: city})
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: city}); err != nil {
				t.Fatal(err)
			}
			// The Fake's WaitForIdle returns ErrInteractionUnsupported unless a
			// nil error is explicitly registered — herdr's real implementation
			// blocks until idle and returns nil. Register nil: "supported".
			sp.WaitForIdleErrors[info.SessionName] = nil
			t.Logf("pre: IsRunning(%q)=%v", info.SessionName, sp.IsRunning(info.SessionName))
			delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "queue", "claim the next step")
			if err != nil {
				t.Fatalf("TryWaitIdleNudgeLiveOnly: %v", err)
			}
			if delivered != tc.want {
				t.Fatalf("delivered = %v, want %v for provider %q", delivered, tc.want, tc.provider)
			}
			if tc.want {
				found := false
				for _, call := range sp.Calls {
					if call.Method == "NudgeNow" && call.Name == info.SessionName {
						found = true
					}
				}
				if !found {
					t.Fatal("expected NudgeNow (immediate submit-capable inject) on the claude-family path")
				}
			}
		})
	}
}
