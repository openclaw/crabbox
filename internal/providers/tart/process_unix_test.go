//go:build !windows

package tart

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if err := os.WriteFile(tart, []byte("#!/bin/sh\necho 'vm is locked' >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend := newBackend(
		Provider{}.Spec(),
		core.BaseConfig(),
		core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
	).(*backend)
	err := backend.startVM(context.Background(), core.BaseConfig(), "test-vm", true)
	if err == nil || !strings.Contains(err.Error(), "vm is locked") {
		t.Fatalf("err=%v", err)
	}
}
