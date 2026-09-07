//go:build windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() {
	if os.Getenv("CRABBOX_TEST_BOUNDED_SSH_CHILD") == "1" && strings.EqualFold(filepath.Base(os.Args[0]), "ssh.exe") {
		_, _ = io.WriteString(os.Stdout, "synthetic-private-output")
		os.Exit(0)
	}
}

func TestRunSSHOutputBoundedDiscardsOutputOnDeferredCleanupFailure(t *testing.T) {
	bin := t.TempDir()
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ssh.exe"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINDIR", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_BOUNDED_SSH_CHILD", "1")
	oldStage := stageWSLSpool
	t.Cleanup(func() { stageWSLSpool = oldStage })
	for _, read := range []struct {
		name string
		run  func(context.Context, SSHTarget, string) (string, error)
	}{
		{"public owner", func(ctx context.Context, target SSHTarget, remote string) (string, error) {
			return RunSSHOutputBounded(ctx, target, remote, 64<<10)
		}},
		{"credential owner", runVNCPasswordSSH},
	} {
		t.Run(read.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name   string
				cancel bool
			}{
				{name: "successful command"},
				{name: "canceled command", cancel: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					stageWSLSpool = func(spool *wslStageSpool, _ context.Context, _ *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
						// The launcher needs only the staged digest. Closing the local file
						// here makes its deferred Close fail after execution or cancellation.
						if err := spool.input.reader.Close(); err != nil {
							t.Fatal(err)
						}
						if scenario.cancel {
							cancel()
							return "", ctx.Err()
						}
						spool.shell = wslStageCMD
						return "0123456789abcdef0123456789abcdef", nil
					}
					target := SSHTarget{Host: "fixture.invalid", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
					out, err := read.run(ctx, target, "unused")
					if out != "" || !errors.Is(err, os.ErrClosed) {
						t.Fatalf("cleanup failure lost: empty=%t err=%v", out == "", err)
					}
					if strings.Contains(err.Error(), "synthetic-private-output") {
						t.Fatal("output exposed in cleanup error")
					}
					if errors.Is(err, context.Canceled) != scenario.cancel {
						t.Fatalf("caller cancellation lost: %v", err)
					}
				})
			}
		})
	}
}
