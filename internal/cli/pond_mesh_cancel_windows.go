//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pondMeshWindowsCancelExitCode is written atomically by TerminateJobObject to
// every still-running member. The root's final ProcessState therefore records
// whether cancellation actually terminated it, without a status-check race.
const pondMeshWindowsCancelExitCode uint32 = 0x43425801

var pondMeshTerminateJobObject = windows.TerminateJobObject

type pondMeshPlatformState struct {
	mu            sync.Mutex
	job           windows.Handle
	process       windows.Handle
	finished      bool
	jobTerminated bool
	cleanupErr    error
}

func pondMeshTerminationContext(ctx context.Context) (context.Context, func()) {
	return ctx, func() {}
}

func (h *pondMeshExecHandle) Start() error {
	if !h.managed {
		return h.cmd.Start()
	}
	h.platform.mu.Lock()
	defer h.platform.mu.Unlock()
	h.cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED,
	}
	if err := h.cmd.Start(); err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return h.failPondMeshStartLocked(fmt.Errorf("create pond mesh job: %w", err))
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return h.failPondMeshStartLocked(fmt.Errorf("configure pond mesh job: %w", err))
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		uint32(h.cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return h.failPondMeshStartLocked(fmt.Errorf("open suspended pond mesh process: %w", err))
	}
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(process)
		_ = windows.CloseHandle(job)
		return h.failPondMeshStartLocked(fmt.Errorf("assign pond mesh process to job: %w", err))
	}
	h.platform.job = job
	h.platform.process = process
	if err := resumePondMeshProcess(h.cmd.Process.Pid); err != nil {
		return h.failPondMeshStartLocked(err)
	}
	return nil
}

func (h *pondMeshExecHandle) failPondMeshStartLocked(startErr error) error {
	if h.platform.job != 0 {
		_ = pondMeshTerminateJobObject(h.platform.job, pondMeshWindowsCancelExitCode)
		h.platform.jobTerminated = true
	}
	_ = h.cmd.Process.Kill()
	_ = h.closePondMeshHandlesLocked()
	h.platform.finished = true
	// Let a concurrently firing CommandContext watcher observe finished instead
	// of deadlocking on the setup mutex while Wait receives its result.
	h.platform.mu.Unlock()
	_ = h.cmd.Wait()
	h.platform.mu.Lock()
	return startErr
}

func resumePondMeshProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot suspended pond mesh threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("enumerate suspended pond mesh threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == uint32(pid) {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open suspended pond mesh thread: %w", err)
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("resume pond mesh process: %w", resumeErr)
			}
			return nil
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return fmt.Errorf("suspended pond mesh process %d has no thread", pid)
			}
			return fmt.Errorf("enumerate suspended pond mesh threads: %w", err)
		}
	}
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
	h.platform.mu.Lock()
	defer h.platform.mu.Unlock()
	if h.platform.finished {
		return h.platform.cleanupErr
	}
	if h.platform.job != 0 && !h.platform.jobTerminated {
		if err := pondMeshTerminateJobObject(h.platform.job, pondMeshWindowsCancelExitCode); err != nil {
			h.platform.cleanupErr = fmt.Errorf("terminate pond mesh job after root exit: %w", err)
		} else {
			h.platform.jobTerminated = true
		}
	}
	h.platform.cleanupErr = errors.Join(h.platform.cleanupErr, h.closePondMeshHandlesLocked())
	h.platform.finished = true
	return h.platform.cleanupErr
}

func (h *pondMeshExecHandle) closePondMeshHandlesLocked() error {
	var closeErr error
	if h.platform.process != 0 {
		if err := windows.CloseHandle(h.platform.process); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close pond mesh process handle: %w", err))
		}
		h.platform.process = 0
	}
	if h.platform.job != 0 {
		if err := windows.CloseHandle(h.platform.job); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close pond mesh job handle: %w", err))
		}
		h.platform.job = 0
	}
	return closeErr
}

func terminatePondMeshForwardProcess(h *pondMeshExecHandle) error {
	h.platform.mu.Lock()
	defer h.platform.mu.Unlock()
	if h.platform.finished || h.platform.job == 0 || h.platform.process == 0 {
		return os.ErrProcessDone
	}
	if !h.platform.jobTerminated {
		if err := pondMeshTerminateJobObject(h.platform.job, pondMeshWindowsCancelExitCode); err != nil {
			// A failed job termination must not strand Wait on a live SSH root.
			// Post-check the exact retained handle first: TerminateProcess may still
			// succeed against an already-signaled process and overwrite its natural
			// exit code. Only hard-kill a root that is still observably live.
			waitResult, waitErr := windows.WaitForSingleObject(h.platform.process, 0)
			rootExited := waitErr == nil && waitResult == windows.WAIT_OBJECT_0
			var rootErr error
			if !rootExited {
				rootErr = windows.TerminateProcess(h.platform.process, pondMeshWindowsCancelExitCode)
			}
			// Closing the last kill-on-close Job handle is an independent full-tree
			// fallback for descendants even when the root already exited naturally.
			jobCloseErr := windows.CloseHandle(h.platform.job)
			if jobCloseErr == nil {
				h.platform.job = 0
				h.platform.jobTerminated = true
			}
			if rootExited && jobCloseErr == nil {
				return os.ErrProcessDone
			}
			return errors.Join(
				fmt.Errorf("terminate pond mesh job: %w", err),
				wrapPondMeshWindowsCleanupError("query pond mesh root fallback state", waitErr),
				wrapPondMeshWindowsCleanupError("terminate pond mesh root fallback", rootErr),
				wrapPondMeshWindowsCleanupError("close pond mesh job fallback", jobCloseErr),
			)
		}
		h.platform.jobTerminated = true
	}
	return nil
}

func wrapPondMeshWindowsCleanupError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

// killAttributableToCancel reports, for a process WE cancelled, whether its
// terminal state is our own teardown. A forced Windows tree termination is
// reported as Exited() with a non-zero
// code. That is indistinguishable from a genuine failure by inspection, so the
// cancelled flag the caller has already checked is necessary but insufficient:
// only the atomic job-termination sentinel proves the root was still running
// when our cancellation terminated it. A root that exited naturally first
// retains its genuine exit code.
func killAttributableToCancel(ps *os.ProcessState) bool {
	return ps == nil || ps.ExitCode() == int(pondMeshWindowsCancelExitCode)
}
