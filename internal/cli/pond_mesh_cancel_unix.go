//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

const (
	pondMeshAnchorArg = "--crabbox-internal-pond-mesh-anchor"
	pondMeshAnchorEnv = "CRABBOX_INTERNAL_POND_MESH_ANCHOR"
)

type pondMeshPlatformState struct {
	cleanupOnce sync.Once
	anchor      *exec.Cmd
	anchorWrite *os.File
	cleanupErr  error
}

func init() {
	if os.Getenv(pondMeshAnchorEnv) != "1" || len(os.Args) != 2 || os.Args[1] != pondMeshAnchorArg {
		return
	}
	// Only a child created with Setpgid may act as an anchor. This prevents a
	// manually forged internal invocation from signaling its caller's group.
	if os.Getpid() != syscall.Getpgrp() {
		os.Exit(126)
	}
	parentLifetime := os.NewFile(3, "pond-mesh-parent-lifetime")
	if parentLifetime == nil {
		os.Exit(126)
	}
	var buffer [1]byte
	for {
		if _, err := parentLifetime.Read(buffer[:]); err != nil {
			break
		}
	}
	// This process is still the live group leader, so its PGID cannot have been
	// reused. Kill the anchor, SSH root, and every inherited helper atomically.
	if err := syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL); err != nil {
		os.Exit(126)
	}
	os.Exit(127)
}

func pondMeshTerminationContext(ctx context.Context) (context.Context, func()) {
	// Forward children are outside the terminal group. Convert hangup, quit, and
	// suspension into owned teardown; otherwise Ctrl-Z would stop Crabbox while
	// leaving live tunnels behind.
	return signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP)
}

func (h *pondMeshExecHandle) Start() error {
	if !h.managed {
		return h.cmd.Start()
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve pond mesh anchor executable: %w", err)
	}
	anchorRead, anchorWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pond mesh anchor pipe: %w", err)
	}
	anchor := exec.Command(executable, pondMeshAnchorArg)
	anchor.Env = append(os.Environ(), pondMeshAnchorEnv+"=1")
	anchor.ExtraFiles = []*os.File{anchorRead}
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := anchor.Start(); err != nil {
		_ = anchorRead.Close()
		_ = anchorWrite.Close()
		return fmt.Errorf("start pond mesh group anchor: %w", err)
	}
	_ = anchorRead.Close()
	h.platform.anchor = anchor
	h.platform.anchorWrite = anchorWrite
	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: anchor.Process.Pid}
	if err := h.cmd.Start(); err != nil {
		cleanupErr := h.finishPondMeshPlatform()
		if cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		return err
	}
	return nil
}

func (h *pondMeshExecHandle) Wait() error {
	err := h.cmd.Wait()
	if !h.managed {
		return err
	}
	cleanupErr := h.finishPondMeshPlatform()
	return h.joinCancellationError(errors.Join(err, cleanupErr))
}

func (h *pondMeshExecHandle) finishPondMeshPlatform() error {
	h.platform.cleanupOnce.Do(func() {
		if h.platform.anchorWrite != nil {
			if err := h.platform.anchorWrite.Close(); err != nil {
				h.platform.cleanupErr = fmt.Errorf("close pond mesh anchor pipe: %w", err)
			}
		}
		if h.platform.anchor == nil {
			return
		}
		err := h.platform.anchor.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGKILL {
				return
			}
		}
		if err != nil {
			h.platform.cleanupErr = errors.Join(h.platform.cleanupErr, fmt.Errorf("wait for pond mesh group anchor: %w", err))
		}
	})
	return h.platform.cleanupErr
}

func terminatePondMeshForwardProcess(h *pondMeshExecHandle) error {
	rootErr := h.cmd.Process.Kill()
	cleanupErr := h.finishPondMeshPlatform()
	if rootErr != nil {
		if errors.Is(rootErr, os.ErrProcessDone) {
			if cleanupErr != nil {
				return cleanupErr
			}
			return os.ErrProcessDone
		}
		return errors.Join(rootErr, cleanupErr)
	}
	return cleanupErr
}

// killAttributableToCancel reports, for a process WE cancelled, whether its
// terminal state is our own teardown rather than a genuine failure. Our
// cancellation uses a process-group SIGKILL, which is uncatchable
// and reported as WaitStatus.Signaled() (Exited()==false); a process that
// reached its own exit code before our kill landed is reported as Exited() and
// is a genuine result we must surface. A nil state (killed before it produced
// one) is our teardown.
func killAttributableToCancel(ps *os.ProcessState) bool {
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
