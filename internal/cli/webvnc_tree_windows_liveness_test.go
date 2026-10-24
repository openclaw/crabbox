//go:build windows

package cli

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsWebVNCDaemonIdentityStopsMatchingKilledProcessWithOpenHandle(t *testing.T) {
	cmd := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("ping unavailable: %v", err)
	}
	started, err := LocalProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	identity := windowsWebVNCDaemonProcessIdentity{pid: cmd.Process.Pid, started: started}
	if !windowsWebVNCDaemonIdentityStillMatches(identity) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("live process identity did not match")
	}
	// The os.Process handle stays open until Wait, which is what kept the
	// recorded start time readable after exit.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	// TerminateProcess returns before the process is signaled; wait on a
	// separate SYNCHRONIZE handle so cmd's own handle stays open.
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if ev, err := windows.WaitForSingleObject(h, 10000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("process did not exit: event=%d err=%v", ev, err)
	}
	if windowsWebVNCDaemonIdentityStillMatches(identity) {
		t.Fatal("killed process still matched its recorded identity")
	}
}
