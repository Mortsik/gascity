package tmux

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// rryr diagnostic instrumentation (agent-forge-rryr stage 2): opt-in, append-only
// log of every mutating tmux invocation (plus every failed one) that passes
// through runCtx. Enabled only when GC_TMUX_OPS_LOG names a writable path —
// production default stays byte-identical (no file opened, no output).
//
// The line carries a ms-precision wall clock and the gc process PID so a
// timeline assembled from several writers (test city reconciler, manual starts,
// dispatch retries) interleaves correctly.
var opsLogOnce struct {
	sync.Once
	f *os.File
}

func opsLogFile() *os.File {
	opsLogOnce.Do(func() {
		path := strings.TrimSpace(os.Getenv("GC_TMUX_OPS_LOG"))
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		opsLogOnce.f = f
	})
	return opsLogOnce.f
}

// mutatingTmuxOps are the server-state mutations the rryr timeline cares
// about: anything that creates, destroys, respawns, or re-parents sessions,
// windows, panes, or clients.
var mutatingTmuxOps = map[string]bool{
	"new-session":     true,
	"kill-session":    true,
	"kill-pane":       true,
	"respawn-pane":    true,
	"respawn-window":  true,
	"new-window":      true,
	"split-window":    true,
	"kill-window":     true,
	"rename-session":  true,
	"rename-window":   true,
	"detach-client":   true,
	"attach-session":  true,
	"switch-client":   true,
	"start-server":    true,
	"set-hook":        true,
	"set-option":      true,
	"source-file":     true,
	"send-keys":       true,
	"paste-buffer":    true,
	"link-window":     true,
	"unlink-window":   true,
	"swap-window":     true,
	"move-window":     true,
	"refresh-client":  true,
	"rotate-window":   true,
	"select-window":   true,
	"select-pane":     true,
	"select-layout":   true,
	"resize-pane":     true,
	"toggle-layout":   true,
	"respawn-session": true,
}

// logTmuxOp records one tmux invocation on the rryr ops timeline. op is the
// first non-flag argument (the tmux subcommand); err may be nil. Best-effort:
// any failure to write is silently dropped — diagnostics must never take the
// runtime down with them.
func logTmuxOp(args []string, err error) {
	f := opsLogFile()
	if f == nil {
		return
	}
	op := tmuxErrorPrefix(args)
	if !mutatingTmuxOps[firstSubcommand(args)] && err == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "err: " + firstLine(err.Error())
	}
	target := argAfterFlag(args, "-t")
	ts := time.Now().UnixNano() / int64(time.Millisecond)
	line := fmt.Sprintf("%d pid=%d op=%s target=%s %s\n", ts, os.Getpid(), op, target, status)
	_, _ = f.WriteString(line)
}

// firstSubcommand returns the first non-flag argv element (skipping the -L
// socket value), mirroring tmuxErrorPrefix's scan.
func firstSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-L" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		return args[i]
	}
	return ""
}

// argAfterFlag returns the value following the named flag, or "" when absent.
func argAfterFlag(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// firstLine trims a multi-line error to its first line so one wrapped tmux
// stderr ("no current client\nno current client") stays one timeline row.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
