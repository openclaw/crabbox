//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"testing"
)

func isolatedArtifactTestUmask(t *testing.T) (os.FileMode, bool) {
	t.Helper()
	const key = "CRABBOX_ARTIFACT_TEST_UMASK"
	if raw := os.Getenv(key); raw != "" {
		mask, err := strconv.ParseUint(raw, 8, 32)
		if err != nil || (mask != 0o022 && mask != 0o077) {
			t.Fatalf("invalid isolated artifact umask %q: %v", raw, err)
		}
		syscall.Umask(int(mask))
		return os.FileMode(mask), true
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testName := t.Name()
	for _, mask := range []string{"022", "077"} {
		t.Run("umask-"+mask, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^"+regexp.QuoteMeta(testName)+"$", "-test.count=1", "-test.timeout=1m")
			cmd.Env = append(os.Environ(), key+"="+mask)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("artifact permissions under umask %s: %v\n%s", mask, err, output)
			}
		})
	}
	return 0, false
}
