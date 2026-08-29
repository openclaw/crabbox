//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const sshDiagnosticOutputLimit = 64 * 1024

type sshDiagnosticCapture struct {
	detector sshMuxFailureDetector
	output   boundedSSHOutput
}

func runSSHCommandWithLocalDiagnostics(cmd *exec.Cmd, stdout, stderr io.Writer) (bool, error) {
	capture := sshDiagnosticCapture{output: boundedSSHOutput{limit: sshDiagnosticOutputLimit}}
	complete, runErr := runSSHCommandCapturingDiagnostics(cmd, stdout, stderr, &capture)
	// Forward only after capture has stopped and cleaned up. An arbitrary
	// caller-supplied writer cannot be made interruptible by the FIFO reader.
	var outputErr error
	if stderr != nil {
		_, outputErr = io.Copy(stderr, &capture.output.buffer)
		if outputErr == nil && capture.output.exceeded {
			_, outputErr = fmt.Fprintln(stderr, "\n[local SSH diagnostics truncated after 65536 bytes]")
		}
	}
	return complete && outputErr == nil && capture.detector.failed(), errors.Join(runErr, outputErr)
}

func runSSHCommandCapturingDiagnostics(cmd *exec.Cmd, stdout, stderr io.Writer, capture *sshDiagnosticCapture) (muxFailure bool, err error) {
	dir, err := os.MkdirTemp("", "crabbox-ssh-diagnostics-*")
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, "log")
	defer func() {
		cleanupErr := errors.Join(os.Remove(path), os.Remove(dir))
		if cleanupErr != nil {
			muxFailure = false
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return false, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	keeper, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	done, wake, err := os.Pipe()
	if err != nil {
		_ = unix.Close(keeper)
		return false, err
	}
	defer done.Close()
	defer wake.Close()

	// Keep OpenSSH's local log separate from remote stderr. A FIFO bounds
	// storage even when a persistent master inherits the diagnostic descriptor.
	cmd.Args = append([]string{cmd.Args[0], "-E", path}, cmd.Args[1:]...)
	type captureResult struct {
		complete bool
		err      error
	}
	result := make(chan captureResult, 1)
	go func() {
		complete, captureErr := drainSSHDiagnostics(fd, int(done.Fd()), keeper, capture)
		result <- captureResult{complete, captureErr}
	}()
	runErr := runSSHCommand(cmd, stdout, stderr)
	_ = wake.Close()
	captured := <-result
	return captured.complete && captured.err == nil, errors.Join(runErr, captured.err)
}

func drainSSHDiagnostics(fd, done, keeper int, capture *sshDiagnosticCapture) (bool, error) {
	defer func() {
		if keeper >= 0 {
			_ = unix.Close(keeper)
		}
	}()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(done), Events: unix.POLLIN}}
	var buf [32 * 1024]byte
	for {
		if _, err := unix.Poll(poll, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return false, err
		}
		finished := poll[1].Revents != 0
		if poll[0].Revents&unix.POLLNVAL != 0 {
			return false, unix.EBADF
		}
		if finished {
			// Closing our writer makes EOF meaningful. A persistent writer may
			// remain, so drain only already available bytes with a hard bound.
			_ = unix.Close(keeper)
			keeper = -1
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		for drained := 0; ; {
			n, err := unix.Read(fd, buf[:])
			if n > 0 {
				_, _ = capture.detector.Write(buf[:n])
				_, _ = capture.output.Write(buf[:n])
				drained += n
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EAGAIN) {
				if finished {
					return false, nil
				}
				break
			}
			if err != nil || n == 0 {
				return finished && err == nil, err
			}
			// Return to Poll regularly so a chatty master cannot hide the
			// foreground exit. Never infer a retry from an incomplete log.
			if drained >= 1024*1024 || time.Now().After(deadline) {
				if finished {
					return false, nil
				}
				break
			}
		}
	}
}
