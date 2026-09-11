package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A transient send-keys error followed by a CONFIRMED-dead target must abort
// the retry loop immediately with a classified session-gone error. This is the
// 2026-09-10/11 live startup churn: the startup nudge's C-u landed, the -l
// paste failed with "no current client", and the loop burned the full 10s
// budget retrying against a session a sibling reconciler had already torn
// down. Retrying a corpse is wasted latency that stretches every failed start
// and widens the window for the churn to compound.
func TestSendKeysLiteralWithRetry_FastFailsWhenTargetVanishesMidRetry(t *testing.T) {
	exec := &fakeExecutor{errs: []error{
		// First paste attempt hits the mid-race teardown error.
		errors.New("tmux send-keys -t s: no current client\nno current client"),
		// Liveness probe: the target is verifiably gone (the classified form
		// realExecutor's wrapError produces for "can't find pane").
		ErrSessionNotFound,
	}}
	tm := NewTmuxWithConfig(Config{NudgeRetryInterval: time.Millisecond})
	tm.exec = exec

	start := time.Now()
	err := tm.sendKeysLiteralWithRetry("gt-vanished-target", "hello", 500*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("sendKeysLiteralWithRetry = nil, want session-gone error")
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("sendKeysLiteralWithRetry = %v, want errors.Is ErrSessionNotFound", err)
	}
	if got := len(exec.calls); got != 2 {
		t.Fatalf("tmux calls = %d (send-keys + liveness probe), want 2; calls: %v", got, exec.calls)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("fast-fail took %v, want immediate abort well under the 500ms budget", elapsed)
	}
	if !strings.Contains(err.Error(), "gt-vanished-target") {
		t.Fatalf("error %v should name the vanished target", err)
	}
}

// undecidableLivenessExecutor fails every paste attempt with the transient
// mid-race error and every liveness probe with a generic error: the retry loop
// can never confirm the target's death, so it must keep retrying (the c24b2ef
// respawn grace) until the budget runs out.
type undecidableLivenessExecutor struct {
	calls int
}

func (e *undecidableLivenessExecutor) execute(args []string) (string, error) {
	e.calls++
	for _, a := range args {
		if a == "send-keys" {
			return "", errors.New("tmux send-keys -t s: no current client\nno current client")
		}
	}
	return "", errors.New("probe blew up: liveness unknown")
}

func (e *undecidableLivenessExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// A transient error with an UNDECIDABLE liveness probe (probe itself failed
// with a generic error) keeps the old retry-until-deadline behavior: the
// c24b2ef respawn grace survives whenever the probe cannot confirm death.
func TestSendKeysLiteralWithRetry_KeepsRetryingWhenLivenessUnknown(t *testing.T) {
	exec := &undecidableLivenessExecutor{}
	tm := NewTmuxWithConfig(Config{NudgeRetryInterval: time.Millisecond})
	tm.exec = exec

	err := tm.sendKeysLiteralWithRetry("gt-liveness-unknown", "hello", 30*time.Millisecond)
	if err == nil {
		t.Fatal("sendKeysLiteralWithRetry = nil, want budget-exhausted error")
	}
	if !strings.Contains(err.Error(), "agent not ready for input after") {
		t.Fatalf("error = %v, want the budget-exhausted wrap for undecided liveness", err)
	}
	if exec.calls < 4 {
		t.Fatalf("tmux calls = %d, want the loop to keep retrying while liveness is unknown", exec.calls)
	}
}

// A transient error while the target still EXISTS stays transient: the loop
// keeps retrying and a subsequent success lands (the mid-race respawn case).
func TestSendKeysLiteralWithRetry_RetriesWhileTargetAlive(t *testing.T) {
	exec := &fakeExecutor{
		errs: []error{
			errors.New("tmux send-keys -t s: no current client\nno current client"),
			nil, // liveness probe succeeds (has-session rc=0)
		},
		outs: []string{"", "1"},
		out:  "1",
	}
	// After the scripted probes are exhausted, fakeExecutor falls back to
	// (out="1", err=nil) — every later paste attempt succeeds.
	tm := NewTmuxWithConfig(Config{NudgeRetryInterval: time.Millisecond})
	tm.exec = exec

	err := tm.sendKeysLiteralWithRetry("gt-respawned-target", "hello", 2*time.Second)
	if err != nil {
		t.Fatalf("sendKeysLiteralWithRetry = %v, want nil after the target came back", err)
	}
}
