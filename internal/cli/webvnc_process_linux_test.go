//go:build linux

package cli

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWebVNCDaemonProcessStartIdentityFromProc(t *testing.T) {
	first, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("unstable process start identity: first=%q second=%q", first, second)
	}
	if _, err := strconv.ParseUint(first, 10, 64); err != nil {
		t.Fatalf("non-numeric /proc start identity %q: %v", first, err)
	}
}

func TestLinuxProcessBootIdentityFromProc(t *testing.T) {
	first, err := processBootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := processBootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !validLinuxBootID(first) || first != second {
		t.Fatalf("unstable Linux boot identity: first=%q second=%q", first, second)
	}
}

func TestWebVNCDaemonStopDoesNotSignalPriorBootPID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	nonce := "abcdef0123456789abcdef0123456789"
	cmd := startTestWebVNCDaemonProcess(t, nonce)
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	bootID := currentProcessBootIdentityForTest(t)
	priorBootID := differentLinuxBootID(bootID)
	_, pidPath, err := webVNCDaemonPaths("prior-boot-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, webVNCDaemonIdentity{
		Version: webVNCDaemonIdentityVersion, WorkspaceID: "prior-boot-workspace", PID: cmd.Process.Pid,
		ProcessStarted: started, BootID: priorBootID, Nonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	stopped, err := (App{Stdout: &output, Stderr: io.Discard}).stopWebVNCDaemonIfRunning("prior-boot-workspace")
	if err != nil || !stopped || !strings.Contains(output.String(), "removed prior-boot identity") {
		t.Fatalf("prior-boot cleanup stopped=%t output=%q err=%v", stopped, output.String(), err)
	}
	if _, alive := webVNCDaemonProcessCommand(cmd.Process.Pid); !alive {
		t.Fatal("prior-boot identity signaled the recycled PID")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("prior-boot identity was not removed: %v", err)
	}
}

func TestDirectSSHWebVNCRemoteIdentityRejectsPriorBootPID(t *testing.T) {
	owner, err := directSSHWebVNCRemoteOwnerFromID(strings.Repeat("01", sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	started, err := webVNCDaemonProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	stateDir := filepath.Join(stateRoot, "crabbox", "direct-webvnc")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(stateDir, owner.ID+".identity")
	identity := fmt.Sprintf("%d %s %s %s %s %s\n", os.Getpid(), started,
		differentLinuxBootID(currentProcessBootIdentityForTest(t)), owner.ID, owner.PreferredPort, strings.Repeat("ab", 16))
	if err := os.WriteFile(identityPath, []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	command := directSSHWebVNCRemoteIdentityFunctions(owner) + `
if direct_webvnc_process_identity_valid; then
  echo "accepted prior-boot identity" >&2
  exit 91
fi
direct_webvnc_load_identity
[ "$boot_id" != "$(direct_webvnc_current_boot_id)" ]
kill -0 "$pid"
printf 'prior-boot-refused\n'`
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateRoot)
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "prior-boot-refused\n" {
		t.Fatalf("prior-boot direct identity output=%q err=%v", output, err)
	}
	if _, err := os.Stat(identityPath); err != nil {
		t.Fatalf("identity validation unexpectedly removed the durable handle: %v", err)
	}
}

func differentLinuxBootID(bootID string) string {
	replacement := byte('0')
	if bootID[0] == replacement {
		replacement = '1'
	}
	return string(replacement) + bootID[1:]
}
