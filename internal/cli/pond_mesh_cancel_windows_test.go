//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestPondMeshForwardStartsSuspendedInJob(t *testing.T) {
	handle := pondMeshExecRunner{}.Command(context.Background(), "cmd.exe", "/c", "exit", "0")
	execHandle := handle.(*pondMeshExecHandle)
	if err := handle.Start(); err != nil {
		t.Fatal(err)
	}
	if execHandle.cmd.SysProcAttr == nil || execHandle.cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatalf("forward missing CREATE_NEW_PROCESS_GROUP: %#v", execHandle.cmd.SysProcAttr)
	}
	if execHandle.cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Fatalf("forward missing CREATE_SUSPENDED: %#v", execHandle.cmd.SysProcAttr)
	}
	if execHandle.platform.job == 0 {
		t.Fatal("forward started without a Job Object")
	}
	if err := handle.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPondMeshNaturalExitBeforeCancelSurvivesOnWindows(t *testing.T) {
	err, terminated := runPondMeshNaturalExitBeforeCancelWindows(t, 7)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("natural exit = %v, want exit code 7", err)
	}
	if terminated {
		t.Fatal("natural exit racing cancellation was suppressed as our teardown")
	}
}

func TestPondMeshNaturalSuccessBeforeCancelSurvivesOnWindows(t *testing.T) {
	err, terminated := runPondMeshNaturalExitBeforeCancelWindows(t, 0)
	if err != nil {
		t.Fatalf("natural success became a cancellation error: %v", err)
	}
	if terminated {
		t.Fatal("natural success racing cancellation was classified as our teardown")
	}
}

func TestPondMeshNaturalExitDuringJobTerminationSurvivesOnWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := pondMeshExecRunner{}.Command(ctx, os.Args[0], "-test.run=^TestPondMeshWindowsExitHelper$")
	execHandle := handle.(*pondMeshExecHandle)
	stateDir := t.TempDir()
	ready := filepath.Join(stateDir, "ready")
	release := filepath.Join(stateDir, "release")
	execHandle.cmd.Env = append(os.Environ(),
		"CBX_POND_WINDOWS_EXIT_CODE=7",
		"CBX_POND_WINDOWS_READY="+ready,
		"CBX_POND_WINDOWS_WAIT="+release,
	)
	if err := handle.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPondMeshWindowsFile(t, ready, "exit helper did not start")

	originalTerminateJob := pondMeshTerminateJobObject
	pondMeshTerminateJobObject = func(job windows.Handle, exitCode uint32) error {
		if err := os.WriteFile(release, []byte("exit"), 0o600); err != nil {
			return err
		}
		result, err := windows.WaitForSingleObject(execHandle.platform.process, 5000)
		if err != nil {
			return err
		}
		if result != windows.WAIT_OBJECT_0 {
			return errors.New("natural exit did not complete before job termination")
		}
		return originalTerminateJob(job, exitCode)
	}
	defer func() { pondMeshTerminateJobObject = originalTerminateJob }()

	waitCh := make(chan error, 1)
	go func() { waitCh <- handle.Wait() }()
	cancel()
	err := <-waitCh
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("natural exit during job termination = %v, want exit code 7", err)
	}
	if handle.WasTerminatedByOurCancel() {
		t.Fatal("natural exit during job termination was suppressed as our teardown")
	}
}

func TestPondMeshCancelKillsWindowsProcessTree(t *testing.T) {
	if os.Getenv("CBX_POND_WINDOWS_TREE_HELPER") == "1" {
		child := exec.Command("cmd.exe", "/c", "ping -t 127.0.0.1 >NUL")
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		if err := os.WriteFile(os.Getenv("CBX_POND_WINDOWS_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(9)
		}
		_ = child.Wait()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	handle := pondMeshExecRunner{}.Command(ctx, os.Args[0], "-test.run=^TestPondMeshCancelKillsWindowsProcessTree$")
	execHandle := handle.(*pondMeshExecHandle)
	childPIDFile := filepath.Join(t.TempDir(), "child-pid")
	execHandle.cmd.Env = append(os.Environ(),
		"CBX_POND_WINDOWS_TREE_HELPER=1",
		"CBX_POND_WINDOWS_CHILD_PID="+childPIDFile,
	)
	if err := handle.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- handle.Wait() }()
	childPID := waitForPondMeshWindowsChildPID(t, childPIDFile)
	t.Cleanup(func() {
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(childPID), "/T", "/F").Run()
	})

	cancel()
	if err := <-waitCh; err == nil {
		t.Fatal("cancelled process tree returned success")
	}
	if !handle.WasTerminatedByOurCancel() {
		t.Fatal("cancelled process tree was not attributed to our teardown")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, alive := webVNCDaemonProcessCommand(childPID); !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("forward descendant %d survived cancellation", childPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPondMeshJobTerminationFailureStillKillsWindowsProcessTree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handle := pondMeshExecRunner{}.Command(ctx, os.Args[0], "-test.run=^TestPondMeshCancelKillsWindowsProcessTree$")
	execHandle := handle.(*pondMeshExecHandle)
	childPIDFile := filepath.Join(t.TempDir(), "child-pid")
	execHandle.cmd.Env = append(os.Environ(),
		"CBX_POND_WINDOWS_TREE_HELPER=1",
		"CBX_POND_WINDOWS_CHILD_PID="+childPIDFile,
	)
	if err := handle.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := waitForPondMeshWindowsChildPID(t, childPIDFile)
	t.Cleanup(func() {
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(childPID), "/T", "/F").Run()
	})

	originalTerminateJob := pondMeshTerminateJobObject
	forcedErr := errors.New("forced job termination failure")
	pondMeshTerminateJobObject = func(windows.Handle, uint32) error { return forcedErr }
	defer func() { pondMeshTerminateJobObject = originalTerminateJob }()

	waitCh := make(chan error, 1)
	go func() { waitCh <- handle.Wait() }()
	cancel()
	select {
	case err := <-waitCh:
		if err == nil || !strings.Contains(err.Error(), forcedErr.Error()) {
			t.Fatalf("job termination failure = %v, want cleanup diagnostic", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait hung after job termination failure")
	}
	if handle.WasTerminatedByOurCancel() {
		t.Fatal("failed job termination was suppressed as clean cancellation")
	}
	waitForPondMeshWindowsProcessExit(t, childPID)
}

func waitForPondMeshWindowsChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("tree helper did not publish its child pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPondMeshWindowsFile(t *testing.T, path, timeoutMessage string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(timeoutMessage)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPondMeshWindowsProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, alive := webVNCDaemonProcessCommand(pid); !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("forward descendant %d survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPondMeshWindowsExitHelper(t *testing.T) {
	codeText := os.Getenv("CBX_POND_WINDOWS_EXIT_CODE")
	if codeText == "" {
		return
	}
	code, err := strconv.Atoi(codeText)
	if err != nil {
		os.Exit(9)
	}
	if err := os.WriteFile(os.Getenv("CBX_POND_WINDOWS_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(8)
	}
	if waitFile := os.Getenv("CBX_POND_WINDOWS_WAIT"); waitFile != "" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(waitFile); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(10)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	os.Exit(code)
}

func runPondMeshNaturalExitBeforeCancelWindows(t *testing.T, exitCode int) (error, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := pondMeshExecRunner{}.Command(ctx, os.Args[0], "-test.run=^TestPondMeshWindowsExitHelper$")
	execHandle := handle.(*pondMeshExecHandle)
	ready := filepath.Join(t.TempDir(), "ready")
	execHandle.cmd.Env = append(os.Environ(),
		"CBX_POND_WINDOWS_EXIT_CODE="+strconv.Itoa(exitCode),
		"CBX_POND_WINDOWS_READY="+ready,
	)
	if err := handle.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- handle.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let the child exit while Wait and the context watcher run concurrently.
	// The exact Job/process handles must preserve the natural result either way.
	time.Sleep(200 * time.Millisecond)
	cancel()
	err := <-waitCh
	return err, handle.WasTerminatedByOurCancel()
}
