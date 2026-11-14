//go:build !windows

package sprites

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpritesBootstrapEnvScriptSkipsAfterConsumedMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "consumed")
	logPath := filepath.Join(dir, "sudo.log")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake sudo evaluates the marker test and only records privileged writes.
	fakeSudo := "#!/bin/sh\nif [ \"$1\" = test ]; then shift; exec test \"$@\"; fi\necho \"$1\" >>\"$SUDO_LOG\"\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(spritesBootstrapEnvScript, spritesBootstrapConsumedMarker, marker, 1)
	run := func() string {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", script, "sh", "CBX_TEST_SECRET")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SUDO_LOG="+logPath)
		cmd.Stdin = strings.NewReader("fake-secret-value")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bootstrap script failed: %v: %s", err, out)
		}
		data, _ := os.ReadFile(logPath)
		_ = os.Remove(logPath)
		return string(data)
	}
	if got := run(); got != "install\nsh\n" {
		t.Fatalf("unconsumed bootstrap must write the value, sudo calls %q", got)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "" {
		t.Fatalf("consumed bootstrap must not write the value, sudo calls %q", got)
	}
}
