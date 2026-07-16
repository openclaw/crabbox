//go:build windows

package cli

import "os"

// killAttributableToCancel reports, for a process WE cancelled, whether its
// terminal state is our own teardown. Windows has no signals: our
// TerminateProcess is reported as Exited() with a non-zero code, which is
// indistinguishable from a genuine failure by inspection, so the cancelled flag
// the caller has already checked governs — EXCEPT when our kill found the
// process had already exited on its own (killFoundExited). That process died
// before our cancellation could touch it, so its exit is genuine and must be
// surfaced.
func killAttributableToCancel(_ *os.ProcessState, killFoundExited bool) bool {
	return !killFoundExited
}
