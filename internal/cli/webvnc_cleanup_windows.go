//go:build windows

package cli

import (
	"context"
	"os/exec"
)

const webVNCDaemonCleanupSupported = false

func retainWebVNCDaemonTerminationSignals() func() { return func() {} }

func superviseWebVNCDaemonCleanup(ctx context.Context, _, _ string, run func(context.Context) error) error {
	return run(ctx)
}

func prepareWebVNCDaemonChildCleanup(context.Context, *exec.Cmd) (func(bool) error, error) {
	return func(bool) error { return nil }, nil
}

func beginSupervisedWebVNCCleanup(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func trackSupervisedWebVNCTunnel(context.Context) func(error) { return func(error) {} }
