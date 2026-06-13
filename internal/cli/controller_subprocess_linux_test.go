//go:build linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestControllerRecoveryDoesNotSignalPriorBootProcessGroup(t *testing.T) {
	const nonce = "00112233445566778899aabbccddeeff"
	child := startControllerRecoveryTestProcess(t, nonce)
	statePath, identityPath := writeControllerRecoveryTestIdentity(t, child.Process.Pid, nonce, differentLinuxBootID(currentProcessBootIdentityForTest(t)))
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{StateFile: statePath}}
	if err := runner.RecoverControllerChildren(context.Background(), statePath); err != nil {
		t.Fatal(err)
	}
	if _, alive := webVNCDaemonProcessCommand(child.Process.Pid); !alive {
		t.Fatal("prior-boot controller identity signaled the recycled process group")
	}
	if _, err := os.Stat(identityPath); !os.IsNotExist(err) {
		t.Fatalf("prior-boot controller identity was not removed: %v", err)
	}
}

func TestControllerRecoveryRequiresRecordedNonceMatch(t *testing.T) {
	const (
		actualNonce   = "11223344556677889900aabbccddeeff"
		recordedNonce = "ffeeddccbbaa00998877665544332211"
	)
	child := startControllerRecoveryTestProcess(t, actualNonce)
	statePath, identityPath := writeControllerRecoveryTestIdentity(t, child.Process.Pid, recordedNonce, currentProcessBootIdentityForTest(t))
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{StateFile: statePath}}
	err := runner.RecoverControllerChildren(context.Background(), statePath)
	if err == nil || !strings.Contains(err.Error(), "does not match its recorded process identity") {
		t.Fatalf("recovery error=%v, want nonce mismatch refusal", err)
	}
	if _, alive := webVNCDaemonProcessCommand(child.Process.Pid); !alive {
		t.Fatal("nonce mismatch signaled the unrelated controller process group")
	}
	if _, err := os.Stat(identityPath); err != nil {
		t.Fatalf("nonce-mismatched durable identity was removed: %v", err)
	}
}

func TestControllerRecoveryRefusesOrphanedGroupWithoutLeaderIdentity(t *testing.T) {
	const nonce = "aabbccddeeff00112233445566778899"
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	heartbeatPath := filepath.Join(dir, "heartbeat")
	script := `sh -c 'while :; do printf x >>"$1"; sleep 0.05; done' crabbox-controller-child "$2" & echo $! >"$1"; wait`
	cmd := exec.Command("sh", "-c", script, "crabbox-controller-child-"+nonce, childPIDPath, heartbeatPath)
	configureControllerCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = terminateControllerProcessGroup(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	waitForControllerRecoveryFixture(t, childPIDPath, heartbeatPath)
	statePath, identityPath := writeControllerRecoveryTestIdentity(t, cmd.Process.Pid, nonce, currentProcessBootIdentityForTest(t))
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	if !controllerProcessGroupAlive(cmd.Process.Pid) {
		t.Fatal("fixture descendant did not survive its controller child leader")
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{StateFile: statePath}}
	err := runner.RecoverControllerChildren(context.Background(), statePath)
	if err == nil || !strings.Contains(err.Error(), "without its recorded leader identity") {
		t.Fatalf("recovery error=%v, want missing leader refusal", err)
	}
	if !controllerProcessGroupAlive(cmd.Process.Pid) {
		t.Fatal("unverifiable controller process group was signaled")
	}
	if _, err := os.Stat(identityPath); err != nil {
		t.Fatalf("unverifiable durable identity was removed: %v", err)
	}
}

func startControllerRecoveryTestProcess(t *testing.T, nonce string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done", "crabbox-controller-child-"+nonce)
	configureControllerCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = terminateControllerProcessGroup(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func writeControllerRecoveryTestIdentity(t *testing.T, pid int, nonce, bootID string) (string, string) {
	t.Helper()
	started, err := webVNCDaemonProcessStartIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	dir := controllerChildStateDirectory(statePath)
	if err := ensureControllerStateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(dir, nonce+".json")
	if err := writeControllerChildIdentity(identityPath, controllerChildIdentity{
		Version: controllerChildIdentityVersion, PID: pid, ProcessStarted: started,
		BootID: bootID, Nonce: nonce, WorkspaceID: "recovery-test", Operation: "warmup",
	}); err != nil {
		t.Fatal(err)
	}
	return statePath, identityPath
}

func waitForControllerRecoveryFixture(t *testing.T, childPIDPath, heartbeatPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		childData, childErr := os.ReadFile(childPIDPath)
		heartbeat, heartbeatErr := os.Stat(heartbeatPath)
		if childErr == nil && heartbeatErr == nil && heartbeat.Size() > 0 {
			if childPID, err := strconv.Atoi(strings.TrimSpace(string(childData))); err == nil && childPID > 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("controller orphan fixture did not start")
}
