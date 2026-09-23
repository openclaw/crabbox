//go:build !windows

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEgressStopSessionLeavesDaemonStateAndScopesRemoteCommand(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	logPath := filepath.Join(dir, "ssh-command")
	script := "#!/bin/sh\nfor arg do cmd=$arg; done\nprintf '%s\\n' \"$cmd\" > \"$CRABBOX_TEST_SSH_LOG\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_TEST_SSH_LOG", logPath)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	_, pidPath, err := egressDaemonPaths("cbx_env_profile_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const marker = "unrelated daemon state"
	if err := os.WriteFile(pidPath, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).egressStop(t.Context(), []string{
		"--provider", "run-env-profile-test", "--id", "friendly-slug", "--session", "egress_owned",
	}); err != nil {
		t.Fatal(err)
	}
	command, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := remoteStopEgressClientSessionCommand("cbx_env_profile_test", "egress_owned")
	if strings.TrimSpace(string(command)) != want || strings.Contains(string(command), "pkill") {
		t.Fatalf("scoped remote cleanup = %q, want %q", command, want)
	}
	state, err := os.ReadFile(pidPath)
	if err != nil || string(state) != marker {
		t.Fatalf("scoped cleanup changed daemon state: %q, %v", state, err)
	}
}
