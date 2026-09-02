//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func workspaceOwnerSetupFixture(t *testing.T) (string, *workspaceOwner) {
	t.Helper()
	home := t.TempDir()
	owner := &workspaceOwner{key: workspaceOwnerKey("setup-test"), token: strings.Repeat("a", 64)}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: owner.key, Token: owner.token, TTL: time.Minute}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
		t.Fatalf("fixture acquisition failed: %v", err)
	}
	return home, owner
}

// Override signal probes only inside the real locked protocol body. All kill
// operations are denied, including termination; no AppArmor or privileges needed.
func workspaceOwnerDenySignals(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lock, err := exec.LookPath("flock")
	args := ` -x -w "$3" "$4"`
	if err != nil {
		lock, err = exec.LookPath("lockf")
		args = ` -t "$3" "$4"`
	}
	if err != nil {
		t.Fatal("POSIX owner lock tool unavailable")
	}
	script := "#!/bin/sh\nexec " + shellQuote(lock) + args + ` /bin/sh -c 'kill() { return 1; }; '"$7"` + "\n"
	writeExecutable(t, filepath.Join(dir, "flock"), script)
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func boundedWorkspaceOwnerCommand(t *testing.T, home, path, script string) (*exec.Cmd, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + path}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Cancel()
			_ = cmd.Wait()
		}
	})
	return cmd, ctx
}

func TestWorkspaceOwnerPOSIXSetupSignalDenialClosesStreams(t *testing.T) {
	home, owner := workspaceOwnerSetupFixture(t)
	path := workspaceOwnerDenySignals(t)
	root := filepath.Join(home, ".crabbox", "workspace-owners")
	statePath := filepath.Join(root, owner.key+".owner")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "workload-started")
	cmd, ctx := boundedWorkspaceOwnerCommand(t, home, path, remoteWorkspaceOwnerPOSIXWitness(owner.key, owner.token, "touch "+shellQuote(marker)))
	out, err := cmd.CombinedOutput() // Wait also requires EOF on both inherited streams.
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		t.Fatal("setup failure did not close inherited streams within its own bound")
	}
	if err == nil || exitCode(err) != 74 || !bytes.Contains(out, []byte("registration")) {
		t.Fatalf("missing bounded setup failure: exit=%d diagnostic-bytes=%d", exitCode(err), len(out))
	}
	if len(out) > 512 || bytes.Contains(out, []byte(owner.token)) || bytes.Contains(out, []byte(home)) {
		t.Fatal("setup diagnostic exceeded its bound or exposed private state")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("workload ran after denied setup observation")
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("setup failure changed owner authority")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".run.") || strings.Contains(entry.Name(), ".launcher.") || strings.HasSuffix(entry.Name(), ".child") {
			t.Fatal("failed setup left a waiter or private staging state")
		}
	}
}

func TestWorkspaceOwnerPOSIXClosedDiagnosticPipeExitsCooperatively(t *testing.T) {
	for _, shellName := range []string{"/bin/sh", "bash", "dash"} {
		t.Run(shellName, func(t *testing.T) {
			shell, err := exec.LookPath(shellName)
			if err != nil {
				t.Skip("optional shell is unavailable")
			}
			testWorkspaceOwnerClosedDiagnosticPipe(t, shell)
		})
	}
}

func testWorkspaceOwnerClosedDiagnosticPipe(t *testing.T, shell string) {
	t.Helper()
	home, owner := workspaceOwnerSetupFixture(t)
	marker := filepath.Join(home, "workload-started")
	script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "touch "+shellQuote(marker), "test-setup-marker")
	// Exercise each shell's trap semantics without changing the host /bin/sh.
	script = strings.ReplaceAll(script, "/bin/sh", shell)
	// Keep recursive-trap regressions bounded without leaving a core dump.
	cmd, ctx := boundedWorkspaceOwnerCommand(t, home, os.Getenv("PATH"), "ulimit -c 0; ulimit -s 512; "+script)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	cmd.Stderr = writer
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	if ctx.Err() != nil || exitCode(err) != 74 {
		t.Fatalf("closed setup stderr did not exit cooperatively: exit=%d deadline=%v", exitCode(err), ctx.Err())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("workload ran after setup diagnostic lost its reader")
	}
	root := filepath.Join(home, ".crabbox", "workspace-owners")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".run.") || strings.HasSuffix(entry.Name(), ".child") {
			t.Fatal("closed setup stderr left private staging or child authority")
		}
	}
}

func TestWorkspaceOwnerPOSIXSignalDenialPreservesLiveWitness(t *testing.T) {
	for _, action := range []workspaceOwnerAction{workspaceOwnerAcquire, workspaceOwnerInspect, workspaceOwnerRelease} {
		t.Run(string(action), func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			statePath := filepath.Join(root, owner.key+".owner")
			if action == workspaceOwnerAcquire {
				if err := os.WriteFile(statePath, []byte("v1\n"+owner.token+"\n1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			identity, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid())).Output()
			if err != nil {
				t.Fatal(err)
			}
			childPath := filepath.Join(root, owner.key+".child")
			child := []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), strings.Join(strings.Fields(string(identity)), " ")))
			if err := os.WriteFile(childPath, child, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			req := workspaceOwnerRemoteRequest{Action: action, Key: owner.key, Token: owner.token, TTL: time.Minute}
			cmd, ctx := boundedWorkspaceOwnerCommand(t, home, workspaceOwnerDenySignals(t), remoteWorkspaceOwnerPOSIX(req))
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil || err == nil || string(out) != "AMBIGUOUS" {
				t.Fatalf("denied liveness did not fail closed: exit=%d", exitCode(err))
			}
			after, stateErr := os.ReadFile(statePath)
			retained, childErr := os.ReadFile(childPath)
			if stateErr != nil || childErr != nil || !bytes.Equal(before, after) || !bytes.Equal(child, retained) {
				t.Fatal("denied liveness changed owner or child authority")
			}
		})
	}
}

func TestWorkspaceOwnerPOSIXSetupStagesAreCredentialFree(t *testing.T) {
	for _, phase := range []string{"staging", "identity", "input"} {
		t.Run(phase, func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			tools := t.TempDir()
			tool := map[string]string{"staging": "mkdir", "identity": "ps", "input": "cat"}[phase]
			writeExecutable(t, filepath.Join(tools, tool), "#!/bin/sh\nprintf private-tool-detail >&2\nexit 1\n")
			marker := filepath.Join(home, "workload-started")
			inputSize := int64(0)
			prepared, err := prepareWorkspaceOwnerRemote(contextWithWorkspaceOwner(context.Background(), owner), SSHTarget{TargetOS: targetLinux}, "touch "+shellQuote(marker), &inputSize)
			if err != nil {
				t.Fatal(err)
			}
			cmd, ctx := boundedWorkspaceOwnerCommand(t, home, tools+string(os.PathListSeparator)+os.Getenv("PATH"), prepared.command)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil || err == nil || exitCode(err) != 74 || !bytes.Contains(out, []byte("failed "+phase)) {
				t.Fatalf("missing %s setup diagnostic: exit=%d diagnostic-bytes=%d", phase, exitCode(err), len(out))
			}
			if len(out) > 512 || bytes.Contains(out, []byte("private-tool-detail")) || bytes.Contains(out, []byte(owner.token)) || bytes.Contains(out, []byte(home)) {
				t.Fatal("setup diagnostics leaked tool output or private state")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("failed setup executed the workload")
			}
		})
	}
}

func TestWaitForSSHReadyWorkspaceOwnerSetupFailure(t *testing.T) {
	for _, scenario := range []string{"direct", "proxy", "diagnostic", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			tools := t.TempDir()
			path := workspaceOwnerDenySignals(t)
			phase := "registration"
			if scenario == "identity" {
				writeExecutable(t, filepath.Join(tools, "ps"), "#!/bin/sh\nexit 1\n")
				path = tools + string(os.PathListSeparator) + path
				phase = "identity"
			}
			// Execute the generated remote script, without recording subprocess argv.
			fakeSSH := "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\n"
			if scenario == "diagnostic" {
				fakeSSH += "if [ ! -f " + shellQuote(filepath.Join(home, "readiness-failed")) + " ]; then touch " + shellQuote(filepath.Join(home, "readiness-failed")) + "; exit 1; fi\n"
			}
			fakeSSH += "HOME=" + shellQuote(home) + "\nPATH=" + shellQuote(path) + "\nexport HOME PATH\nexec /bin/sh -c \"$remote\"\n"
			writeExecutable(t, filepath.Join(tools, "ssh"), fakeSSH)
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), FallbackPorts: []string{}, TargetOS: targetLinux, ReadyCheck: "true", NoControlMaster: true}
			if scenario == "proxy" {
				target.SSHConfigProxy, target.ProxyCommand = true, "unused-test-proxy"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var progress bytes.Buffer
			err = waitForSSHReady(contextWithWorkspaceOwner(ctx, owner), &target, &progress, "before sync", 2*time.Second)
			if err == nil || !strings.Contains(err.Error(), "remote workspace owner setup failed ("+phase+")") || ctx.Err() != nil {
				t.Fatalf("readiness did not preserve setup failure promptly: %v", err)
			}
			text := err.Error() + progress.String()
			for _, forbidden := range []string{"ssh-auth", "ports=", owner.token, home, "timed out"} {
				if strings.Contains(text, forbidden) {
					t.Fatal("readiness misclassified setup or exposed private state")
				}
			}
		})
	}
}

func TestWorkspaceOwnerSSHWorkloadExit74IsNotSetupFailure(t *testing.T) {
	home, owner := workspaceOwnerSetupFixture(t)
	tools := t.TempDir()
	writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nHOME="+shellQuote(home)+"\nexport HOME\nexec /bin/sh -c \"$remote\"\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux, NoControlMaster: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = contextWithWorkspaceOwner(ctx, owner)
	const diagnostic = "CRABBOX_OWNER_SETUP_V1 workload-output failed registration\n"
	var stdout, stderr bytes.Buffer
	err := runSSHInput(ctx, target, "cat; printf %s "+shellQuote(diagnostic)+" >&2; exit 74", strings.NewReader("input-preserved"), &stdout, &stderr)
	if err == nil || exitCode(err) != 74 || strings.Contains(err.Error(), "workspace owner setup") {
		t.Fatalf("workload exit was misclassified: %v", err)
	}
	if stdout.String() != "input-preserved" || stderr.String() != diagnostic {
		t.Fatal("owner protocol altered workload streams")
	}
	var combined bytes.Buffer
	payload := `(i=0; while [ "$i" -lt 2000 ]; do printf 'stdout\n'; i=$((i+1)); done) &
i=0; while [ "$i" -lt 2000 ]; do printf 'stderr\n' >&2; i=$((i+1)); done
wait`
	code, err := runSSHStreamResult(ctx, target, payload, &combined, &combined)
	if err != nil || code != 0 || strings.Count(combined.String(), "stdout\n") != 2000 || strings.Count(combined.String(), "stderr\n") != 2000 {
		t.Fatal("owner protocol lost shared stdout/stderr output")
	}
}

func TestWorkspaceOwnerPOSIXGateWaitClosesStreamsWithoutSupervisor(t *testing.T) {
	for _, releaseLock := range []bool{false, true} {
		t.Run(fmt.Sprintf("release-lock-%t", releaseLock), func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			ready := filepath.Join(home, "lock-held")
			lock, err := exec.LookPath("flock")
			options := " -x "
			if err != nil {
				lock, err = exec.LookPath("lockf")
				options = " -k "
			}
			if err != nil {
				t.Fatal("POSIX owner lock tool unavailable")
			}
			holder, _ := boundedWorkspaceOwnerCommand(t, home, os.Getenv("PATH"), "exec "+shellQuote(lock)+options+shellQuote(filepath.Join(root, owner.key+".gate"))+" /bin/sh -c "+shellQuote("touch "+shellQuote(ready)+"; exec sleep 10"))
			if err := holder.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if holder.ProcessState == nil {
					_ = holder.Cancel()
					_ = holder.Wait()
				}
			}()
			waitForWorkspaceOwnerTestFile(t, ready)
			// Record only the registrar PID, using its already-supported identity probe.
			tools := t.TempDir()
			realPS, err := exec.LookPath("ps")
			if err != nil {
				t.Fatal(err)
			}
			registrar := filepath.Join(home, "registrar")
			writeExecutable(t, filepath.Join(tools, "ps"), "#!/bin/sh\nif [ \"$1\" = -o ] && [ \"$2\" = lstart= ]; then printf '%s\\n' \"$4\" > "+shellQuote(registrar)+"; fi\nexec "+shellQuote(realPS)+" \"$@\"\n")
			marker := filepath.Join(home, "workload-started")
			// Use the raw witness so cmd.Process is exactly its supervising shell.
			script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "touch "+shellQuote(marker), "")
			cmd, ctx := boundedWorkspaceOwnerCommand(t, home, tools+string(os.PathListSeparator)+os.Getenv("PATH"), script)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			// Allow the registrar's existing five-second lock deadline to close pipes.
			cmd.WaitDelay = 8 * time.Second
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitForWorkspaceOwnerTestFile(t, registrar)
			if err := cmd.Process.Kill(); err != nil {
				_ = cmd.Cancel()
				_ = cmd.Wait()
				t.Fatal(err)
			}
			if releaseLock {
				_ = holder.Cancel()
				_ = holder.Wait()
			}
			err = cmd.Wait()
			if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
				_ = cmd.Cancel()
				t.Fatal("orphaned pre-start registrar retained SSH streams")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("orphaned pre-start registrar ran the workload")
			}

			pidData, err := os.ReadFile(registrar)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil || pid <= 0 {
				t.Fatal("invalid registrar PID")
			}
			state, _ := exec.Command(realPS, "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
			if value := strings.TrimSpace(string(state)); value != "" && !strings.HasPrefix(value, "Z") {
				t.Fatal("pre-start registrar remained live after stream closure")
			}

		})
	}
}

func waitForWorkspaceOwnerTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture did not reach its bounded synchronization point")
}
