//go:build darwin

package tart

import (
	"errors"
	"fmt"
	"syscall"
)

// Darwin's os.Process.Wait cannot wait without reaping (Go issue #13987).
// Observe exit with kqueue first, then serialize Wait with all signals so the
// child's PID cannot be recycled in the gap between reaping and marking it done.
func (p *startupProcess) wait() error {
	err := waitForTartExit(p.cmd.Process.Pid)
	p.mu.Lock()
	if err != nil {
		// No Wait has happened yet: this is still our exact, unreaped child.
		_ = p.cmd.Process.Kill()
		err = fmt.Errorf("observe tart run process: %w", err)
	}
	return errors.Join(err, p.cmd.Wait())
}

func waitForTartExit(pid int) error {
	syscall.ForkLock.RLock()
	kq, err := syscall.Kqueue()
	if err == nil {
		syscall.CloseOnExec(kq)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return err
	}
	defer syscall.Close(kq)
	changes := []syscall.Kevent_t{{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}}
	events := make([]syscall.Kevent_t, 1)
	for {
		n, err := syscall.Kevent(kq, changes, events, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		// An already-exited child can disappear from kqueue discovery, but its
		// PID is still reserved until our sole Wait reaps it.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		changes = nil
		if n > 0 {
			if events[0].Flags&syscall.EV_ERROR != 0 && events[0].Data != 0 && syscall.Errno(events[0].Data) != syscall.ESRCH {
				return syscall.Errno(events[0].Data)
			}
			return nil
		}
	}
}
