//go:build windows

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const recordingSSHWindowsHelperEnv = "CRABBOX_RECORDING_SSH_WINDOWS_HELPER"
const scriptedSSHWindowsBehaviorEnv = "CRABBOX_SCRIPTED_SSH_WINDOWS_BEHAVIOR"
const recordingToolWindowsNameEnv = "CRABBOX_RECORDING_TOOL_WINDOWS_NAME"
const recordingToolWindowsLogEnv = "CRABBOX_RECORDING_TOOL_WINDOWS_LOG"

var (
	recordingSSHWindowsOnce sync.Once
	recordingSSHWindowsDir  string
	recordingSSHWindowsErr  error
)

// copyTestExecutable publishes the running test binary under a tool name so the
// exercised code path launches a real Windows executable instead of a POSIX
// script it could never run.
func copyTestExecutable(path string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.Link(executable, path); err == nil {
		return nil
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o755)
}

func installRecordingSSHExecutable(t *testing.T, dir string) string {
	t.Helper()
	return installScriptedSSHExecutable(t, dir, scriptedSSHBehavior{})
}

func installScriptedSSHExecutable(t *testing.T, dir string, behavior scriptedSSHBehavior) string {
	t.Helper()
	recordingSSHWindowsOnce.Do(func() {
		recordingSSHWindowsDir, recordingSSHWindowsErr = os.MkdirTemp("", "crabbox-recording-ssh-")
		if recordingSSHWindowsErr != nil {
			return
		}
		recordingSSHWindowsErr = copyTestExecutable(filepath.Join(recordingSSHWindowsDir, "ssh.exe"))
	})
	if recordingSSHWindowsErr != nil {
		t.Fatal(recordingSSHWindowsErr)
	}
	if behavior.ConfigQueryPassthrough && behavior.RealSSH == "" {
		behavior.RealSSH = resolveRealSSHExecutable(recordingSSHWindowsDir)
	}
	// directSSHExecutable prefers %WINDIR%\System32\OpenSSH\ssh.exe over PATH, so
	// a PATH-only fixture would never be consulted. Point WINDIR at the fixture
	// directory, which has no System32\OpenSSH\ssh.exe, to force PATH resolution.
	t.Setenv("WINDIR", recordingSSHWindowsDir)
	data, err := json.Marshal(behavior)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(recordingSSHWindowsHelperEnv, "1")
	t.Setenv(scriptedSSHWindowsBehaviorEnv, string(data))
	return recordingSSHWindowsDir
}

func installSuccessfulTestTool(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name+".exe")
	if _, err := os.Stat(path); err == nil {
		t.Setenv(recordingSSHWindowsHelperEnv, "1")
		return
	}
	if err := copyTestExecutable(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(recordingSSHWindowsHelperEnv, "1")
}

func installRecordingTestTool(t *testing.T, dir, name, logPath string) {
	t.Helper()
	installSuccessfulTestTool(t, dir, name)
	t.Setenv(recordingToolWindowsNameEnv, name)
	t.Setenv(recordingToolWindowsLogEnv, logPath)
}

func cleanupRecordingSSHWindowsExecutable() {
	if recordingSSHWindowsDir != "" {
		_ = os.RemoveAll(recordingSSHWindowsDir)
	}
}
