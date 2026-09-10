package pidutil

import "os/exec"

// StartDetached starts cmd as a detached child and guarantees its exit
// status is collected: a goroutine parks on cmd.Wait(), so whenever the
// child exits, it is reaped instead of lingering as a zombie.
//
// Process.Release() is NOT a substitute — it only drops the handle without
// collecting the exit status. A detached child (poller, daemon) spawned that
// way from a long-lived parent becomes an unreapable zombie the moment it
// exits, because only the direct parent can wait(2) for it, and under load
// (frequent poller churn) those zombies accumulate until the parent dies.
// Waiting from a goroutine keeps the spawn fire-and-forget for the caller at
// the cost of one parked goroutine per child. cmd.Stdout and cmd.Stderr must
// already be set by the caller (an *os.File or io.Discard keeps Wait from
// blocking on copy goroutines).
func StartDetached(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
