//go:build !windows

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
)

const brokenPipeExitCode = 141

func isBrokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }

func notifyBrokenPipe(fail func()) func() {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	// Notify, unlike Ignore, preserves the normal SIGPIPE disposition in
	// exec'd children. The parent owns cancellation and deferred cleanup.
	signal.Notify(ch, syscall.SIGPIPE)
	go func() {
		defer close(done)
		for range ch {
			fail()
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
		<-done
	}
}
