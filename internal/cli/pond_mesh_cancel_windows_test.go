//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

const pondMeshWindowsCancelHelperEnv = "CRABBOX_POND_MESH_WINDOWS_CANCEL_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(recordingSSHWindowsHelperEnv) == "1" {
		os.Exit(runRecordingSSHWindowsHelper())
	}
	if os.Getenv(pondMeshWindowsCancelHelperEnv) == "1" {
		if err := os.WriteFile(os.Getenv("CRABBOX_POND_MESH_WINDOWS_CANCEL_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	code := runCLITests(m)
	cleanupRecordingSSHWindowsExecutable()
	os.Exit(code)
}

func runRecordingSSHWindowsHelper() int {
	tool := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	if name := os.Getenv(recordingToolWindowsNameEnv); name != "" && strings.EqualFold(tool, name) {
		logPath := os.Getenv(recordingToolWindowsLogEnv)
		if logPath == "" {
			return 2
		}
		content := strings.Join(os.Args[1:], "\n") + "\n"
		if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
			return 2
		}
		return 0
	}
	if !strings.EqualFold(tool, "ssh") {
		// Tools published purely so the exercised code path finds an executable.
		return 0
	}
	var behavior scriptedSSHBehavior
	if raw := os.Getenv(scriptedSSHWindowsBehaviorEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &behavior); err != nil {
			return 2
		}
	}
	return runScriptedSSHWindowsHelper(behavior)
}

func runScriptedSSHWindowsHelper(behavior scriptedSSHBehavior) int {
	if behavior.ConfigQueryPassthrough {
		for _, arg := range os.Args[1:] {
			if arg != "-G" {
				continue
			}
			// Configuration queries must never enter the simulated
			// remote-command path.
			if behavior.RealSSH == "" {
				return scriptedSSHConfigQueryUnavailable
			}
			return runRealSSHConfigQuery(behavior.RealSSH)
		}
	}
	command := ""
	if len(os.Args) > 1 {
		command = os.Args[len(os.Args)-1]
	}
	port := ""
	for index := 1; index+1 < len(os.Args); index++ {
		if os.Args[index] == "-p" {
			port = os.Args[index+1]
		}
	}
	if argsPath := os.Getenv("CRABBOX_FAKE_SSH_ARGS_LOG"); argsPath != "" {
		if err := os.WriteFile(argsPath, []byte(strings.Join(os.Args[1:], " ")), 0o600); err != nil {
			return 2
		}
	}
	if portsPath := os.Getenv("CRABBOX_FAKE_SSH_PORTS"); portsPath != "" {
		if err := appendScriptedSSHFile(portsPath, port+"\n"); err != nil {
			return 2
		}
	}
	if callsPath := os.Getenv("CRABBOX_FAKE_SSH_CALLS"); callsPath != "" {
		if err := appendScriptedSSHFile(callsPath, port+":"+command+"\n"); err != nil {
			return 2
		}
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 2
	}
	if path := os.Getenv("CRABBOX_FAKE_SSH_STDIN_LOG"); path != "" {
		if err := appendScriptedSSHFile(path, string(stdin)); err != nil {
			return 2
		}
	}
	match := command
	decoded := ""
	if behavior.DecodePayload {
		decoded = strings.Join(decodeScriptedSSHPayloads(command), "\n")
		match = command + "\n" + decoded
	}
	if logPath := os.Getenv("CRABBOX_FAKE_SSH_LOG"); logPath != "" {
		entry := command + "\n---\n"
		if behavior.DecodePayload {
			entry = command + "\n" + decoded + "\n---\n"
		}
		if err := appendScriptedSSHFile(logPath, entry); err != nil {
			return 2
		}
	}
	if behavior.MatchStdin {
		match += "\n" + string(stdin)
	}
	if path := os.Getenv("CRABBOX_FAKE_SSH_DETAILED_LOG"); path != "" {
		entry := strings.Join(os.Args[1:], "\n") + "\n" + match + "\n---\n"
		if err := appendScriptedSSHFile(path, entry); err != nil {
			return 2
		}
	}
	if behavior.WorkspaceOwnerProtocol {
		for _, action := range []struct{ marker, reply string }{
			{"protocol_action='acquire'", "ACQUIRED"},
			{"protocol_action='renew'", "RENEWED"},
			{"protocol_action='inspect'", "OWNED"},
			{"protocol_action='release'", "RELEASED"},
		} {
			if strings.Contains(match, action.marker) {
				return writeScriptedSSHResult(action.reply, "", 0)
			}
		}
	}
	for _, rule := range behavior.Rules {
		if (rule.Port == "" || rule.Port == port) && strings.Contains(match, rule.Contains) {
			return writeScriptedSSHResult(rule.Stdout, rule.Stderr, rule.ExitCode)
		}
	}
	return writeScriptedSSHResult(behavior.Stdout, behavior.Stderr, behavior.ExitCode)
}

func runRealSSHConfigQuery(realSSH string) int {
	query := exec.Command(realSSH, os.Args[1:]...)
	query.Stdin = os.Stdin
	query.Stdout = os.Stdout
	query.Stderr = os.Stderr
	if err := query.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return scriptedSSHConfigQueryUnavailable
	}
	return 0
}

func appendScriptedSSHFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeScriptedSSHResult(stdout, stderr string, exitCode int) int {
	if exitCode < 0 || exitCode > 255 {
		return 2
	}
	if stdout != "" {
		if _, err := io.WriteString(os.Stdout, stdout); err != nil {
			return 2
		}
	}
	if stderr != "" {
		if _, err := io.WriteString(os.Stderr, stderr); err != nil {
			return 2
		}
	}
	return exitCode
}

func TestPondMeshCancelRunForwardsReturnsNoErrorOnWindows(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	fakeSSH := filepath.Join(binDir, "ssh.exe")
	if err := os.WriteFile(fakeSSH, helper, 0o755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("WINDIR", t.TempDir())
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(pondMeshWindowsCancelHelperEnv, "1")
	t.Setenv("CRABBOX_POND_MESH_WINDOWS_CANCEL_READY", ready)
	if resolved, err := exec.LookPath("ssh"); err != nil {
		t.Fatal(err)
	} else if !strings.EqualFold(resolved, fakeSSH) {
		t.Fatalf("ssh resolved to %q, want %q", resolved, fakeSSH)
	}

	members := []pondMember{{
		Name:  "peer-a",
		Lease: "lease-a",
		SSH: SSHTarget{
			User:                   "crab",
			Host:                   "127.0.0.1",
			Port:                   "22",
			DisableHostKeyChecking: true,
			NoControlMaster:        true,
		},
	}}
	summary := pondMeshSummary{Forwards: []pondMeshForward{{
		Peer:       "peer-a",
		RemotePort: 8080,
		LocalPort:  18080,
		LeaseID:    "lease-a",
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runPondMeshForwards(ctx, pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard}, members, summary)
	}()

	waitForPondMeshWindowsReady(t, ready, errCh)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("context cancellation must not be reported as a tunnel failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runPondMeshForwards did not return promptly after context cancellation")
	}
}

func waitForPondMeshWindowsReady(t *testing.T, path string, errCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-errCh:
			t.Fatalf("fake ssh helper exited before startup: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake ssh helper did not start")
}

func TestPondMeshForwardStartsSuspendedInJob(t *testing.T) {
	handle := pondMeshExecRunner{}.Command(context.Background(), SSHTarget{}, "cmd.exe", "/c", "exit", "0")
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
	handle := pondMeshExecRunner{}.Command(ctx, SSHTarget{}, os.Args[0], "-test.run=^TestPondMeshWindowsExitHelper$")
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
		pidPath := os.Getenv("CBX_POND_WINDOWS_CHILD_PID")
		// The parent polls this path; publish only the complete PID.
		if err := os.WriteFile(pidPath+".tmp", []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(9)
		}
		if err := os.Rename(pidPath+".tmp", pidPath); err != nil {
			os.Exit(9)
		}
		_ = child.Wait()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := pondMeshExecRunner{}.Command(ctx, SSHTarget{}, os.Args[0], "-test.run=^TestPondMeshCancelKillsWindowsProcessTree$")
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
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			<-waitCh
		}
	})
	childPID := waitForPondMeshWindowsChildPID(t, childPIDFile)

	cancel()
	err := <-waitCh
	waited = true
	if err == nil {
		t.Fatal("cancelled process tree returned success")
	}
	if !handle.WasTerminatedByOurCancel() {
		t.Fatal("cancelled process tree was not attributed to our teardown")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, alive := LocalProcessCommand(childPID); !alive {
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
	defer cancel()
	handle := pondMeshExecRunner{}.Command(ctx, SSHTarget{}, os.Args[0], "-test.run=^TestPondMeshCancelKillsWindowsProcessTree$")
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
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			<-waitCh
		}
	})
	childPID := waitForPondMeshWindowsChildPID(t, childPIDFile)

	originalTerminateJob := pondMeshTerminateJobObject
	forcedErr := errors.New("forced job termination failure")
	pondMeshTerminateJobObject = func(windows.Handle, uint32) error { return forcedErr }
	defer func() { pondMeshTerminateJobObject = originalTerminateJob }()

	cancel()
	select {
	case err := <-waitCh:
		waited = true
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
		// Windows can expose the renamed path before releasing its sharing lock.
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("tree helper did not publish its child pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPondMeshCancelChildPIDWaitsForSharingLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child-pid")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		_ = windows.CloseHandle(handle)
		t.Fatalf("fixture did not hold a sharing lock: %v", err)
	}
	unlocked := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		unlocked <- windows.CloseHandle(handle)
	}()
	t.Cleanup(func() {
		if err := <-unlocked; err != nil {
			t.Error(err)
		}
	})
	if pid := waitForPondMeshWindowsChildPID(t, path); pid != 12345 {
		t.Fatalf("published pid=%d", pid)
	}
}

func waitForPondMeshWindowsFile(t *testing.T, path, timeoutMessage string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
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
		if _, alive := LocalProcessCommand(pid); !alive {
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
	handle := pondMeshExecRunner{}.Command(ctx, SSHTarget{}, os.Args[0], "-test.run=^TestPondMeshWindowsExitHelper$")
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
