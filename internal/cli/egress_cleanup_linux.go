//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func stopLocalEgressClientSession(ctx context.Context, leaseID, sessionID string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve egress helper executable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stopPath, unlock, err := lockEgressClientSession(ctx, leaseID, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := stopEgressClientSessionAdmission(stopPath); err != nil {
		return err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("inspect egress client processes: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		matches := func() bool {
			dir := "/proc/" + entry.Name()
			image, err := os.Readlink(dir + "/exe")
			if err != nil || strings.TrimSuffix(image, " (deleted)") != executable {
				return false
			}
			data, err := os.ReadFile(dir + "/cmdline")
			return err == nil && egressClientSessionArgsMatch(strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), leaseID, sessionID)
		}
		if !matches() {
			continue
		}
		// Bind the signal to this kernel process, then recheck executable and
		// argv. A reused PID cannot redirect a signal sent through this fd.
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open egress client process handle (Linux pidfd support required): %w", err)
		}
		if matches() {
			err = stopEgressClientProcessHandle(ctx, fd)
		}
		_ = unix.Close(fd)
		if err != nil {
			return err
		}
	}
	return nil
}

func stopEgressClientProcessHandle(ctx context.Context, fd int) error {
	for _, signal := range []unix.Signal{unix.SIGTERM, unix.SIGKILL} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := unix.PidfdSendSignal(fd, signal, nil, 0); err != nil {
			if errors.Is(err, unix.ESRCH) {
				return nil
			}
			return fmt.Errorf("signal egress client session: %w", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if err := ctx.Err(); err != nil {
				return err
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, err := unix.Poll(fds, 100)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return fmt.Errorf("wait for egress client session: %w", err)
			}
			if fds[0].Revents&unix.POLLIN != 0 {
				return nil
			}
		}
	}
	return errors.New("egress client session did not stop")
}
