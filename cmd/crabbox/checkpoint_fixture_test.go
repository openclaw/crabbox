//go:build !windows

package main

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/cli"
)

var checkpointFixtureCLI string

func TestMain(m *testing.M) {
	if checkpointFixtureCLI == "1" {
		if os.Getenv("CRABBOX_CAPTURE_BINARY_FIXTURE") == "" {
			runMain(cli.Run)
			return
		}
		port := os.Getenv("CRABBOX_CAPTURE_SSH_PROBE_PORT")
		if _, err := net.LookupPort("tcp", port); err != nil || port == "" {
			panic("checkpoint fixture requires an owned SSH probe port")
		}
		runMain(func(ctx context.Context, args []string) error {
			app := cli.App{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin}
			app = app.WithSSHReadinessWaiter(func(ctx context.Context, target *cli.SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
				if phase != "machine0" {
					return cli.WaitForSSHReady(ctx, target, stderr, phase, timeout)
				}
				// Preserve the provider's lease endpoint and create-intent identity.
				// Only the probe uses the fixture's owned, dynamically allocated socket.
				probe := *target
				probe.Host, probe.Port = "127.0.0.1", port
				probe.FallbackPorts = nil
				return cli.WaitForSSHReady(ctx, &probe, stderr, phase, timeout)
			})
			return app.Run(ctx, args)
		})
		return
	}
	os.Exit(m.Run())
}
