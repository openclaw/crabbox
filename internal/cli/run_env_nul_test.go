package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSSHCommandEnvRejectsNULBeforeUpload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH executable fixture")
	}
	isolateTestUserDirs(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ssh-called")
	t.Setenv("CRABBOX_TEST_SSH_CALLED", marker)
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf called >> \"$CRABBOX_TEST_SSH_CALLED\"\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var diagnostics bytes.Buffer
	target := SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux}
	prepared, err := stageSSHCommandEnv(t.Context(), target, dir, map[string]string{"BUILD_VALUE": "synthetic-prefix\x00synthetic-suffix"}, &diagnostics)
	defer prepared.close()
	if err == nil || ExitCodeForError(err, 1) != 2 || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("invalid native environment value was not rejected: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid environment reached SSH: %v", err)
	}
	if strings.Contains(err.Error()+diagnostics.String(), "synthetic-prefix") {
		t.Fatal("invalid environment diagnostic disclosed its value")
	}
}
