//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestVNCSecretBoundaryForeground(t *testing.T) {
	for _, route := range []bool{false, true} {
		for _, ending := range []string{"cancel", "exit", "cancel-before-ready"} {
			name := ending
			if route {
				name += "/config-route"
			} else {
				name += "/managed"
			}
			t.Run(name, func(t *testing.T) {
				mode := "ready"
				if ending == "cancel-before-ready" {
					mode = "unready"
				}
				f := newForwardBoundaryFixture(t, route, mode)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type started struct {
					tunnel *vncForegroundTunnel
					err    error
				}
				result := make(chan started, 1)
				port := boundaryPort(t)
				go func() {
					tunnel, _, err := startVNCForegroundTunnelOnReservedPort(ctx, f.target, port, "127.0.0.1", "5900")
					result <- started{tunnel, err}
				}()
				r := f.waitRecord(t)
				f.assertAliveBoundary(t, r)
				if ending == "cancel-before-ready" {
					select {
					case <-result:
						t.Fatal("unready tunnel was published")
					default:
					}
					cancel()
				}
				var got started
				select {
				case got = <-result:
				case <-time.After(10 * time.Second):
					t.Fatal("VNC owner did not complete startup")
				}
				if ending == "cancel-before-ready" {
					// Cancellation can race the existing Wait notification, producing
					// either the context cause or the killed-child error.
					if got.tunnel != nil || got.err == nil {
						t.Fatalf("startup cancellation result: %v", got.err)
					}
				} else {
					if got.err != nil {
						t.Fatal(got.err)
					}
					stopped := false
					t.Cleanup(func() {
						if !stopped {
							stopProcess(got.tunnel)
						}
					})
					if got.tunnel.PID() != r.PID {
						t.Fatal("readiness tracked a different process")
					}
					// This is also the WebVNC bridge's real tunnel-exit callback.
					bridgeCtx, stopBridge := vncForegroundTunnelContext(ctx, got.tunnel, nil)
					defer stopBridge(context.Canceled)
					if ending == "exit" {
						f.exit(t)
					} else {
						cancel()
					}
					select {
					case <-bridgeCtx.Done():
					case <-time.After(8 * time.Second):
						t.Fatal("WebVNC tunnel callback failed to cancel the bridge")
					}
					stopProcess(got.tunnel)
					stopped = true
					if ending == "exit" && got.tunnel.ExitError() == nil {
						t.Error("natural SSH exit was hidden")
					}
				}
				f.assertReapedBoundary(t, r)
			})
		}
	}
}

func TestVNCSecretBoundaryDetached(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pid, err := startVNCTunnel(ctx, f.target, boundaryPort(t), "127.0.0.1", "5900")
	if err != nil {
		t.Fatal(err)
	}
	r := f.waitRecord(t)
	if pid != r.PID {
		t.Fatal("detached VNC returned a different pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Release()
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		_ = stopDaemonProcess(proc, pid)
	})
	cancel()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("detached VNC failed to survive return/caller cancellation")
	}
	f.assertDetachedBoundary(t, r)
	_ = stopDaemonProcess(proc, pid)
	reaped = true
	f.assertReapedBoundary(t, r, true)
}

type forwardBoundaryFailingWriter struct{}

func (forwardBoundaryFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic handoff sink failure")
}

func TestVNCSecretBoundaryHandoffFailure(t *testing.T) {
	f := newForwardBoundaryFixture(t, false, "ready")
	err := runVNCNativeHandoff(context.Background(), forwardBoundaryFailingWriter{}, f.target,
		boundaryPort(t), vncEndpoint{Managed: true, Host: "127.0.0.1", Port: "5900"}, "viewer", "synthetic-vnc-password")
	if err == nil || err.Error() != "write native VNC handoff: synthetic handoff sink failure" {
		t.Fatalf("handoff error was not preserved: %v", err)
	}
	r := f.waitRecord(t)
	// The child is already reaped here; use the child's entry-time observation.
	if r.ArgvSecret || r.EnvSecret || !r.ConfigPrivate || !r.ConfigContainsUser {
		t.Error("handoff tunnel subprocess crossed the synthetic credential/private-config boundary")
	}
	f.assertReapedBoundary(t, r)
}
