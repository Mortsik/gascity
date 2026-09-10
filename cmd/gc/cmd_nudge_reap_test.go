package main

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// zombieChildrenOf returns the pids of this process's children currently in
// zombie state (parsed from /proc/<pid>/stat: state 'Z', ppid == getpid()).
// A zombie's pid stays reserved until the parent collects the exit status,
// so "no zombie children" is exactly the steady-state the poller spawn path
// must reach.
func zombieChildrenOf(t *testing.T) []int {
	t.Helper()
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("reading /proc: %v", err)
	}
	var zombies []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue // raced with exit+reap; not a zombie anymore
		}
		stat := string(data)
		// comm can contain spaces and parens; fields start after the last ')'.
		if idx := strings.LastIndexByte(stat, ')'); idx >= 0 && idx+2 < len(stat) {
			fields := strings.Fields(stat[idx+2:])
			// fields[0]=state, fields[1]=ppid (after the comm paren block).
			if len(fields) >= 2 && fields[0] == "Z" {
				if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == self {
					zombies = append(zombies, pid)
				}
			}
		}
	}
	return zombies
}

func waitForNoZombieChildren(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(zombieChildrenOf(t)) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("zombie children %v still present after %s", zombieChildrenOf(t), timeout)
}

// TestEnsureNudgePollerReapsDetachedChild pins the no-zombie contract for the
// supervisor's poller spawn path (agent-forge-bk73): ensureNudgePoller
// detaches the poller, so a poller that dies while its spawner still lives —
// here the go test binary standing in for the long-lived supervisor — must be
// reaped, not parked as a zombie.
//
// The spawned argv is the test binary re-executed; arming the
// direct-child env spy (the package's sanctioned short-lived re-exec hook)
// makes it write its snapshot and exit(0) immediately — exactly the
// short-lived child the regression needs, through the production spawn path
// (pid file, Setpgid, product-metrics scrub).
func TestEnsureNudgePollerReapsDetachedChild(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("/proc zombie scan is linux-only")
	}
	cityPath := t.TempDir()
	t.Setenv(productMetricsDirectChildEnvSpyPath, filepath.Join(t.TempDir(), "child.env"))
	const (
		agentName   = "worker"
		sessionName = "session-worker"
	)
	if err := ensureNudgePoller(cityPath, agentName, sessionName); err != nil {
		t.Fatalf("ensureNudgePoller: %v", err)
	}

	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("reading poller pid file %s: %v", pidPath, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing poller pid %q: %v", data, err)
	}

	// kill(pid, 0) keeps succeeding while the pid exists — including as a
	// zombie. ESRCH therefore proves the detached child was reaped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			waitForNoZombieChildren(t, 2*time.Second)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nudge poller pid %d still exists after 5s; detached spawn leaked a zombie", pid)
}

// TestEnsureNudgePollerKillPathReaps covers the spawn-failure branch: when
// the pid file cannot be written, the child is killed and the detached Wait
// must reap it — the error path is where detached children historically got
// Release()d instead of waited for.
func TestEnsureNudgePollerKillPathReaps(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("/proc zombie scan is linux-only")
	}
	cityPath := t.TempDir()
	const (
		agentName   = "worker"
		sessionName = "session-kill-path"
	)
	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory where the pid file should be makes the write fail after
	// the child has already started.
	if err := os.Mkdir(pidPath, 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := ensureNudgePoller(cityPath, agentName, sessionName); err == nil {
		t.Fatal("ensureNudgePoller = nil error despite unwritable pid file")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("kill path took %s; the parked Wait should reap immediately", elapsed)
	}
	waitForNoZombieChildren(t, 5*time.Second)
}
