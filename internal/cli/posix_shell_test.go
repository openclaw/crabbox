package cli

import (
	"os/exec"
	"runtime"
	"testing"
)

func requirePOSIXShellTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("generated artifact scripts target POSIX remote hosts")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
}
