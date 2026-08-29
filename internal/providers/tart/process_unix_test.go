//go:build !windows

package tart

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDetachCommandCreatesSession(t *testing.T) {
	cmd := exec.Command("tart", "run", "test")
	detachCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("detached command must start in a new session: %#v", cmd.SysProcAttr)
	}
}

func TestStartVMKeepPreservesStartupStderr(t *testing.T) {
	dir := t.TempDir()
	tart := filepath.Join(dir, "tart")
	if err := os.WriteFile(tart, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$TART_TEST_ARGV\"\necho 'vm is locked' >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := newBackend(
		Provider{}.Spec(),
		core.BaseConfig(),
		core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
	).(*backend)
	backend.startupObserveTimeout = 10 * time.Second
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			argvPath := filepath.Join(t.TempDir(), "argv")
			t.Setenv("TART_TEST_ARGV", argvPath)
			err := backend.startVM(context.Background(), "test-vm", keep)
			if err == nil || !strings.Contains(err.Error(), "vm is locked") {
				t.Fatalf("err=%v", err)
			}
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			want := commandKey([]string{"run", "test-vm", "--no-graphics", "--no-clipboard", "--no-audio"}) + "\x00"
			if string(argv) != want {
				t.Fatalf("tart argv=%q want %q", argv, want)
			}
		})
	}
}
