//go:build integration

package tmux

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// rryr stage-2 reproducer (agent-forge-rryr): production 2026-09-10/11 bursts
// showed startup nudges failing for a full 10s retry budget with
// `tmux send-keys -t <sess>: no current client` while a same-instant
// `list-panes -t <sess>` saw the session alive. Isolated shell replays of the
// exact argv never reproduce. This test drives the REAL NudgeSession sequence
// (C-u, survey dismissal, literal paste with retry, submit) under the same
// scrubbed headless environment as the supervisor, against a session a
// sibling writer keeps killing and recreating under the same name — the
// suspected mutator shape — and records which tmux error classes actually
// surface.
func TestRryrNudgeUnderSameNameKillRecreateRace(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux test (spins a throwaway tmux server)")
	}
	scrubHeadlessClientEnv(t)

	socket := "gc-rryr-ncc-race"
	p := NewProviderWithConfig(Config{
		SocketName:        socket,
		NudgeReadyTimeout: 2 * time.Second,
		NudgeLockTimeout:  5 * time.Second,
	})
	tm := p.Tmux()
	_, _ = tm.run("kill-server")
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	sess := "rryr-race"
	if _, err := tm.run("new-session", "-d", "-s", sess, "bash"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// keeper holds the server open so kills of the victim never take the
	// server down (production shape: hundreds of sibling sessions).
	if _, err := tm.run("new-session", "-d", "-s", sess+"-keeper", "bash"); err != nil {
		t.Fatalf("seed keeper: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = tm.run("kill-session", "-t", sess)
			time.Sleep(30 * time.Millisecond)
			_, _ = tm.run("new-session", "-d", "-s", sess, "bash")
			time.Sleep(70 * time.Millisecond)
		}
	}()

	errs := make(map[string]int)
	deadline := time.Now().Add(60 * time.Second)
	for iter := 0; time.Now().Before(deadline); iter++ {
		err := tm.NudgeSession(sess, "Run gc hook --claim --js probe")
		if err != nil {
			// Classify by the tmux layer's own transient/gone taxonomy plus
			// the production-mystery string.
			msg := err.Error()
			class := "other: " + firstLine(msg)
			switch {
			case strings.Contains(msg, "no current client"):
				class = "no-current-client"
			case strings.Contains(msg, "not in a mode"):
				class = "not-in-a-mode"
			case strings.Contains(msg, "can't find"):
				class = "cant-find"
			case strings.Contains(msg, "gone during nudge"):
				class = "target-gone-fastfail"
			case strings.Contains(msg, "nudge lock timeout"):
				class = "lock-timeout"
			case strings.Contains(msg, "unconfirmed"):
				class = "submit-unconfirmed"
			}
			errs[class]++
		}
	}
	close(stop)
	wg.Wait()

	t.Logf("nudge error classes over 60s race: %v", errs)
	if errs["no-current-client"] > 0 {
		t.Fatalf("REPRODUCED: no current client surfaced %d times under the kill/recreate race", errs["no-current-client"])
	}
}
