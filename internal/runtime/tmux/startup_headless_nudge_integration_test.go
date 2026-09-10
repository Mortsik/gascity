//go:build integration

package tmux

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Headless startup-nudge regression tests (agent-forge-njqm).
//
// The supervisor runs under systemd with no TERM, no TMUX/TMUX_PANE, and no
// attached tmux client — any tmux call on the start/resume path that resolves
// state through the "current client" instead of an explicit -t target fails
// there with "no current client" while working flawlessly from an interactive
// shell. These tests pin the whole launchOrchestration nudge step against a
// REAL, isolated tmux server (own -L socket, killed on cleanup) under exactly
// that scrubbed environment: if a future change reintroduces a
// client-dependent invocation (switch-client / refresh-client / an untargeted
// display-message), the nudge fails here first, not on a headless host.
//
// The test binary's stdin is already /dev/null under `go test`, and
// realExecutor leaves cmd.Stdin nil so every tmux child inherits /dev/null —
// the same shape as the supervisor. Only the environment needs scrubbing,
// because the parent shell running `go test` may be interactive.

// scrubHeadlessClientEnv removes every variable tmux uses to infer "I am
// running inside a client", so the code under test sees supervisor
// conditions: no TERM, no TMUX socket/pane identity.
func scrubHeadlessClientEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

func headlessNudgeTestProvider(socket string) *Provider {
	return NewProviderWithConfig(Config{
		SocketName:        socket,
		NudgeReadyTimeout: 10 * time.Second,
		NudgeLockTimeout:  10 * time.Second,
	})
}

// TestHeadlessStartDeliversStartupNudge runs the full fresh-start
// orchestration — session creation, readiness, dialogs, startup nudge — with
// provider credentials in the session env, which forces the staged
// `start-server ; source-file` new-session path the production supervisor
// always takes (it carries provider env). A bash pane never shows a busy
// indicator, so ErrNudgeSubmitUnconfirmed is the expected clean outcome: the
// keystrokes reached tmux, only the busy confirmation is absent.
func TestHeadlessStartDeliversStartupNudge(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux test (spins a throwaway tmux server)")
	}
	scrubHeadlessClientEnv(t)

	p := headlessNudgeTestProvider("gc-headless-nudge-start")
	tm := p.Tmux()
	_, _ = tm.run("kill-server")
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	cfg := runtime.Config{
		Command: "bash",
		Nudge:   "Run gc hook --claim --json now; if it returns minimal work, execute it.",
		Env: map[string]string{
			"GC_PROVIDER":          "claude",
			"ANTHROPIC_AUTH_TOKEN": "sk-ant-integration-secret-must-not-reach-argv",
			"ANTHROPIC_BASE_URL":   "http://127.0.0.1:18317",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sess := "headless-start"
	if err := p.Start(ctx, sess, cfg); err != nil && !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("headless Start with startup nudge: %v", err)
	}
	// The nudge text must actually be in the pane, not merely handed to tmux:
	// a start that reported success while dropping the paste would strand the
	// agent without its startup instruction.
	content, err := tm.CapturePane(sess, 50)
	if err != nil {
		t.Fatalf("capture-pane after nudge: %v", err)
	}
	if !strings.Contains(content, "gc hook --claim") {
		t.Fatalf("startup nudge text not visible in pane; got:\n%s", content)
	}
}

// TestHeadlessResumeAfterDeathDeliversStartupNudge covers the resume shape the
// reconciler drives: the session exists from a previous run, its pane process
// is gone, and Start must take the recreate-then-nudge path under the same
// scrubbed environment.
func TestHeadlessResumeAfterDeathDeliversStartupNudge(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux test (spins a throwaway tmux server)")
	}
	scrubHeadlessClientEnv(t)

	p := headlessNudgeTestProvider("gc-headless-nudge-resume")
	tm := p.Tmux()
	_, _ = tm.run("kill-server")
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	cfg := runtime.Config{
		Command: "bash",
		Nudge:   "resume probe: claim work now",
		Env: map[string]string{
			"GC_PROVIDER":          "claude",
			"ANTHROPIC_AUTH_TOKEN": "sk-ant-integration-secret-must-not-reach-argv",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sess := "headless-resume"
	if err := p.Start(ctx, sess, cfg); err != nil && !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("initial headless Start: %v", err)
	}
	// Kill the session to force the next Start down the zombie-recreate path.
	if _, err := tm.run("kill-session", "-t", sess); err != nil {
		t.Fatalf("kill-session: %v", err)
	}
	if err := p.Start(ctx, sess, cfg); err != nil && !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("headless resume Start with startup nudge: %v", err)
	}
	content, err := tm.CapturePane(sess, 50)
	if err != nil {
		t.Fatalf("capture-pane after resume nudge: %v", err)
	}
	if !strings.Contains(content, "resume probe") {
		t.Fatalf("resume nudge text not visible in pane; got:\n%s", content)
	}
}
