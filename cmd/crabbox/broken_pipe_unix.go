//go:build !windows

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const brokenPipeExitCode = 141

func isBrokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }

func notifyBrokenPipe(fail func(), outputs ...*os.File) func() {
	var descriptors []unix.PollFd
	for _, file := range outputs {
		info, err := file.Stat()
		if err != nil || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
			continue
		}
		raw, err := file.SyscallConn()
		if err != nil {
			continue
		}
		// Unlike File.Fd, Control does not switch a nonblocking file to blocking.
		_ = raw.Control(func(fd uintptr) {
			// Darwin needs write readiness requested to report a pipe hangup.
			descriptors = append(descriptors, unix.PollFd{Fd: int32(fd), Events: unix.POLLOUT})
		})
	}
	broken := func() bool {
		if len(descriptors) == 0 {
			return false
		}
		if _, err := unix.Poll(descriptors, 0); err != nil {
			return false
		}
		for _, descriptor := range descriptors {
			if descriptor.Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
				return true
			}
		}
		return false
	}
	ch := make(chan os.Signal, 1)
	stopped, done := make(chan struct{}), make(chan struct{})
	// Notify keeps the parent alive without making children ignore SIGPIPE.
	// Signals from unrelated descriptors must never cancel a healthy run.
	signal.Notify(ch, syscall.SIGPIPE)
	go func() {
		defer close(done)
		var ticks <-chan time.Time
		if len(descriptors) != 0 {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			select {
			case <-stopped:
				if broken() {
					fail()
				}
				return
			case <-ch:
			case <-ticks:
			}
			// Periodic checks also cover quiet workloads and direct child
			// inheritance: the child's SIGPIPE is not sent to the parent.
			if broken() {
				fail()
				<-stopped
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(stopped)
		<-done
	}
}
