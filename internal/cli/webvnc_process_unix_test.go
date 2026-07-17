//go:build !windows

package cli

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStopProcessTerminatesForegroundTunnelProcessGroup(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
		wait   bool
	}{
		{name: "live leader", script: `trap '' TERM; sleep 60 & wait`},
		{name: "exited leader", script: `sleep 60 &`, wait: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", test.script)
			configureDaemonCommand(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			tunnel := &vncForegroundTunnel{
				cmd:    cmd,
				done:   make(chan struct{}),
				output: &strings.Builder{},
			}
			go func() {
				err := cmd.Wait()
				tunnel.mu.Lock()
				tunnel.err = err
				tunnel.mu.Unlock()
				close(tunnel.done)
			}()
			if test.wait {
				select {
				case <-tunnel.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("foreground tunnel leader did not exit")
				}
			}
			t.Cleanup(func() { _ = terminateWebVNCDaemonProcessTree(cmd.Process.Pid) })

			stopProcess(tunnel)
			deadline := time.Now().Add(2 * time.Second)
			for {
				err := syscall.Kill(-cmd.Process.Pid, 0)
				if err == syscall.ESRCH {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("foreground tunnel process group %d survived stop: %v", cmd.Process.Pid, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
