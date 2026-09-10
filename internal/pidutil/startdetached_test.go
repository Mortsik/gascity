package pidutil

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// waitForPidReaped polls until the pid no longer exists. A zombie still
// answers kill(pid, 0) with success — the kernel keeps the pid reserved
// until the parent collects the exit status — so reaching ESRCH proves the
// child was actually reaped, not merely exited.
func waitForPidReaped(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still exists after %s; exit status was never collected (zombie)", pid, timeout)
}

// TestStartDetachedReapsShortLivedChild pins the regression behind the bd
// zombie sightings: a detached child spawned with Start()+Process.Release()
// exits, but nothing ever waits for it, so under a long-lived parent it stays
// a zombie forever. StartDetached must reap it even though the caller never
// calls Wait.
func TestStartDetachedReapsShortLivedChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := StartDetached(cmd); err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	waitForPidReaped(t, cmd.Process.Pid, 5*time.Second)
}

// TestStartDetachedReapsKilledChild covers the fire-and-forget kill path used
// by poller spawn failure handling: Kill without Wait must still end in a
// reaped child, because the parked Wait goroutine collects the status.
func TestStartDetachedReapsKilledChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := StartDetached(cmd); err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForPidReaped(t, cmd.Process.Pid, 5*time.Second)
}

// TestStartDetachedStartFailurePropagates keeps Start's error contract: a
// command that cannot start must surface the error, not hide it behind the
// reap goroutine.
func TestStartDetachedStartFailurePropagates(t *testing.T) {
	cmd := exec.Command("/nonexistent/gascity-definitely-missing-binary")
	if err := StartDetached(cmd); err == nil {
		t.Fatal("StartDetached = nil error for a missing binary")
	}
}
