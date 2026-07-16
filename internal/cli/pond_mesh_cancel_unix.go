//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// killAttributableToCancel reports, for a process WE cancelled, whether its
// terminal state is our own teardown rather than a genuine failure. Our
// cancellation uses exec.CommandContext's default SIGKILL, which is uncatchable
// and reported as WaitStatus.Signaled() (Exited()==false); a process that
// reached its own exit code before our kill landed is reported as Exited() and
// is a genuine result we must surface. A nil state (killed before it produced
// one) is our teardown. killFoundExited is a Windows-only concern, ignored here.
func killAttributableToCancel(ps *os.ProcessState, _ bool) bool {
	if ps == nil {
		return true
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		return ws.Signaled()
	}
	// No WaitStatus available on this platform: any non-Exited terminal state
	// is our signal-driven kill rather than a peer's own exit code.
	return !ps.Exited()
}
