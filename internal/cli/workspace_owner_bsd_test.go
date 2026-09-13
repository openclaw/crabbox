package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The outer provider transport records dispatch, while the functional operation
// runs the real local supervisor and completion controls with inert workers.
// This covers CLI continuation, not SSH or Python installation readiness.
func TestRunFunctionalPreflightContinuation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires the POSIX supervisor fixture")
	}
	for _, tc := range []struct {
		name, state                                          string
		code                                                 int
		timeout, canceled, lostAck, lostRetire, cleanedError bool
	}{
		{name: "ready", state: "ready"},
		{name: "missing", state: "missing-python3", code: 20},
		{name: "venv", state: "venv-unavailable", code: 21},
		{name: "pip", state: "pip-unavailable", code: 22},
		{name: "worker", state: "worker-failed", code: 23},
		{name: "timeout", state: "timed-out", code: 74, timeout: true},
		{name: "canceled", state: "canceled", canceled: true},
		{name: "unconfirmed acknowledgement", state: "unavailable", lostAck: true},
		{name: "unconfirmed retirement", state: "unavailable", code: 21, lostRetire: true},
		{name: "cleaned operational error", state: "unavailable", code: 21, cleanedError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)
			logPath := installRecordingSSH(t, dir)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			oldRun, oldControl := runOwnedFunctionalPreflight, runFunctionalPreflightControl
			t.Cleanup(func() { runOwnedFunctionalPreflight, runFunctionalPreflightControl = oldRun, oldControl })
			controlFailure := errors.New("synthetic owned control transport unavailable")
			calls := 0
			var operationStage string
			var operationDone <-chan struct{}
			runOwnedFunctionalPreflight = func(probeCtx context.Context, target SSHTarget, _ string, _ map[string]string, _ []string) (functionalPreflightCompletion, error) {
				calls++
				nonce, err := randomHex(16)
				if err != nil {
					t.Fatal(err)
				}
				stage := "/tmp/crabbox-command-" + nonce
				operationStage = stage
				if _, err := os.Lstat(stage); !os.IsNotExist(err) {
					t.Fatal("fixture stage already exists")
				}
				worker := "printf started >" + shellQuote(filepath.Join(stage, "scratch", "started")) + " || exit 75\n"
				budget := 10 * time.Second
				if tc.timeout || tc.canceled || tc.lostAck {
					worker += "mkfifo " + shellQuote(filepath.Join(stage, "scratch", "wait")) + "\nexec 7<>" + shellQuote(filepath.Join(stage, "scratch", "wait")) + "\ntrap 'exit 0' TERM\nwhile :; do read -r -t 1 -u 7 ignored || :; done\n"
					if tc.timeout {
						budget = time.Second
					}
				} else {
					worker += "exit " + strconv.Itoa(tc.code) + "\n"
				}
				helper := functionalPOSIXPreflightHelper(budget)
				remoteCtx, remoteCancel := context.WithTimeout(t.Context(), 25*time.Second)
				cmd := exec.CommandContext(remoteCtx, "/bin/bash", "-c", helper, "sh", "run", stage, nonce, strconv.Itoa(len(worker)), "0", "15000", "100")
				cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "CBX_HELPER=" + helper}
				cmd.Stdin = strings.NewReader(worker)
				cmd.WaitDelay = 5 * time.Second
				output := newSynchronizedBuffer(16 << 10)
				cmd.Stdout, cmd.Stderr = &output, &output
				if err := cmd.Start(); err != nil {
					remoteCancel()
					t.Fatal(err)
				}
				done := make(chan struct{})
				operationDone = done
				var runErr error
				go func() { runErr = cmd.Wait(); close(done) }()
				localControl := func(controlCtx context.Context, _ SSHTarget, gotNonce, action string) ([]byte, error) {
					text, err := functionalPreflightControlCommand(gotNonce, action)
					if err != nil {
						return nil, err
					}
					control := exec.CommandContext(controlCtx, "/bin/bash", "-c", text)
					control.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
					return control.Output()
				}
				t.Cleanup(func() {
					defer remoteCancel()
					cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
					defer stop()
					if _, err := os.Lstat(stage); err == nil {
						record, err := localControl(cleanupCtx, target, nonce, "cancel")
						if err != nil {
							t.Errorf("owned stage retained %s: %v %s", stage, err, output.String())
							return
						}
						if _, err := parseFunctionalPreflightCompletion(record, nonce); err != nil {
							t.Error(err)
							return
						}
						if _, err := localControl(cleanupCtx, target, nonce, "retire"); err != nil {
							t.Error(err)
							return
						}
					}
					select {
					case <-done:
					case <-cleanupCtx.Done():
						t.Error("owned supervisor reaping unconfirmed")
					}
				})
				runFunctionalPreflightControl = func(controlCtx context.Context, target SSHTarget, nonce, action string) ([]byte, error) {
					if tc.lostAck || tc.lostRetire && action == "retire" {
						return nil, controlFailure
					}
					return localControl(controlCtx, target, nonce, action)
				}
				cleanupCtx, stop := functionalPreflightCleanupBudget(probeCtx)
				defer stop()
				if tc.canceled || tc.lostAck {
					limit := time.Now().Add(5 * time.Second)
					for {
						if _, err := os.Stat(filepath.Join(stage, "scratch", "started")); err == nil {
							break
						}
						if time.Now().After(limit) {
							t.Fatal("worker did not start")
						}
						time.Sleep(10 * time.Millisecond)
					}
					if tc.canceled {
						cancel()
					} else {
						return finishFunctionalPreflight(probeCtx, cleanupCtx, target, nonce, controlFailure)
					}
					return finishFunctionalPreflight(probeCtx, cleanupCtx, target, nonce, context.Cause(probeCtx))
				}
				<-done
				if tc.cleanedError {
					runErr = errors.Join(runErr, controlFailure)
				}
				return finishFunctionalPreflight(probeCtx, cleanupCtx, target, nonce, runErr)
			}
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(ctx, []string{"--provider", "ssh", "--static-host", "127.0.0.1", "--static-user", "runner", "--static-work-root", filepath.Join(dir, "remote"), "--no-sync", "--preflight", "--preflight-tools", "python3-venv,python3-venv", "--", "printf", "cbx-after-functional"})
			blocked := tc.canceled || tc.lostAck || tc.lostRetire || tc.cleanedError
			if (err != nil) != blocked || calls != 1 {
				t.Fatalf("calls=%d err=%v stderr=%s", calls, err, stderr.String())
			}
			if tc.canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if (tc.lostAck || tc.lostRetire || tc.cleanedError) && !errors.Is(err, controlFailure) {
				t.Fatalf("control cause lost: %v", err)
			}
			if tc.lostAck {
				select {
				case <-operationDone:
					t.Fatal("unconfirmed-ack fixture did not retain the active worker")
				default:
				}
				if _, err := os.Stat(filepath.Join(operationStage, "scratch", "started")); err != nil {
					t.Fatal(err)
				}
			} else if tc.lostRetire {
				if _, err := os.Stat(filepath.Join(operationStage, ".completion")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(operationStage, "scratch")); !os.IsNotExist(err) {
					t.Fatal("completed worker scratch remains")
				}
			} else if _, err := os.Lstat(operationStage); !os.IsNotExist(err) {
				t.Fatalf("production control did not retire stage before continuation: %v", err)
			}
			log, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(log), "cbx-after-functional") == blocked {
				t.Fatalf("workload dispatch blocked=%t: %s", blocked, log)
			}
			cleanup := "confirmed"
			if tc.lostAck || tc.lostRetire {
				cleanup = "unconfirmed"
			}
			want := "remote preflight python3-venv=" + tc.state + " cleanup=" + cleanup
			if strings.Count(stderr.String(), "remote preflight python3-venv=") != 1 || !strings.Contains(stderr.String(), want) {
				t.Fatalf("diagnostic wanted %q: %s", want, stderr.String())
			}
		})
	}
}

func TestFunctionalPOSIXPreflightOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor requires a POSIX host")
	}
	for _, tc := range []struct {
		name, state     string
		code            int
		cancel, timeout bool
	}{
		{name: "ready", state: "ready"},
		{name: "capability unavailable", state: "venv-unavailable", code: 21},
		{name: "worker failed", state: "worker-failed", code: 23},
		{name: "caller cancellation", state: "canceled", cancel: true},
		{name: "worker deadline", state: "timed-out", code: 74, timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitFile := func(path string, limit time.Duration) {
				t.Helper()
				deadline := time.Now().Add(limit)
				for {
					if _, err := os.Stat(path); err == nil {
						return
					} else if !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if time.Now().After(deadline) {
						t.Fatalf("owned fixture did not publish %s", path)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			root, err := os.MkdirTemp("", "cbx-functional-owner-test-")
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(root, "owned")
			nonce := strings.Repeat("a", 32)
			tools := filepath.Join(root, "tools")
			if err := os.Mkdir(tools, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"bash", "ps", "mkdir", "head", "wc", "cat", "mkfifo", "mv", "rm", "sleep"} {
				path := filepath.Join("/bin", name)
				if _, err := os.Stat(path); err != nil {
					path = filepath.Join("/usr/bin", name)
				}
				if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
					t.Fatal(err)
				}
			}
			command := "test -d " + shellQuote(filepath.Join(directory, "scratch")) + " || exit 75\nprintf marker >" + shellQuote(filepath.Join(directory, "scratch", "marker")) + " || exit 75\n"
			budget := 10 * time.Second
			if tc.cancel || tc.timeout {
				command += "mkfifo " + shellQuote(filepath.Join(directory, "scratch", "wait")) + "\nexec 7<>" + shellQuote(filepath.Join(directory, "scratch", "wait")) + "\ntrap 'exit 0' TERM\nwhile :; do read -r -t 1 -u 7 ignored || :; done\n"
				if tc.timeout {
					budget = time.Second
				}
			} else {
				command += "sleep .2\nexit " + strconv.Itoa(tc.code) + "\n"
			}
			helper := functionalPOSIXPreflightHelper(budget)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", helper, "sh", "run", directory, nonce, strconv.Itoa(len(command)), "0", "15000", "100")
			cmd.Env = []string{"PATH=" + tools, "HOME=" + root, "CBX_HELPER=" + helper}
			cmd.Stdin = strings.NewReader(command)
			var out synchronizedBuffer = newSynchronizedBuffer(16 << 10)
			cmd.Stdout, cmd.Stderr = &out, &out
			cmd.WaitDelay = 5 * time.Second
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var processErr error
			go func() { processErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				if data, _ := os.ReadFile(filepath.Join(directory, ".nonce")); string(data) == nonce {
					_ = os.WriteFile(filepath.Join(directory, ".cancel"), nil, 0o600)
				}
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Errorf("owned supervisor unconfirmed; retained %s", root)
					return
				}
				record, _ := os.ReadFile(filepath.Join(directory, ".completion"))
				if _, err := parseFunctionalPreflightCompletion(record, nonce); err != nil {
					t.Errorf("owned stage cleanup unconfirmed; retained %s: %s", root, out.String())
					return
				}
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			if tc.cancel {
				waitFile(filepath.Join(directory, "scratch", "marker"), 20*time.Second)
				// Match caller transport cancellation, then issue only the
				// registered stage's ordinary cancellation control.
				cancel()
				if err := os.WriteFile(filepath.Join(directory, ".cancel"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(25 * time.Second):
				t.Fatal("supervisor did not return")
			}
			if !tc.cancel && exitCode(processErr) != tc.code {
				t.Fatalf("exit=%v: %s", processErr, out.String())
			}
			waitFile(filepath.Join(directory, ".completion"), 10*time.Second)
			record, err := os.ReadFile(filepath.Join(directory, ".completion"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseFunctionalPreflightCompletion(record, nonce)
			if err != nil || got.State != tc.state || !got.WorkerQuiesced || !got.ScratchRemoved {
				t.Fatalf("completion=%+v error=%v: %s", got, err, out.String())
			}
			if _, err := os.Lstat(filepath.Join(directory, "scratch")); !os.IsNotExist(err) {
				t.Fatalf("scratch remains: %v", err)
			}
		})
	}
}

// Stock BSD tools only: no flock, lockf, GNU stat/date, or /proc helpers.
func workspaceOwnerBSDPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX sh")
	}
	dir := t.TempDir()
	for _, name := range []string{"sh", "uname", "mkdir", "rmdir", "chmod", "sed", "mv", "rm", "ps", "tr", "cut", "awk", "sleep", "cat", "base64", "wc", "nohup"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	date, err := exec.LookPath("date")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "date"), []byte("#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = +%s ] || exit 64\nexec "+shellQuote(date)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkspaceOwnerBSDProtocol(t *testing.T) {
	path := workspaceOwnerBSDPath(t)
	key, token := workspaceOwnerKey("bsd-protocol"), strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, state, child, gate string
		action                   workspaceOwnerAction
		want                     string
		code                     int
	}{
		{name: "acquire", action: workspaceOwnerAcquire, want: "ACQUIRED"},
		{name: "busy", state: "live", action: workspaceOwnerAcquire, want: "BUSY"},
		{name: "renew", state: "live", action: workspaceOwnerRenew, want: "RENEWED"},
		{name: "inspect", state: "live", action: workspaceOwnerInspect, want: "OWNED"},
		{name: "release", state: "live", action: workspaceOwnerRelease, want: "RELEASED"},
		{name: "recovery", state: "expired", action: workspaceOwnerAcquire, want: "RECOVERED"},
		{name: "expired", state: "expired", action: workspaceOwnerRenew, want: "EXPIRED", code: 75},
		{name: "mismatch", state: "other", action: workspaceOwnerRenew, want: "MISMATCH", code: 75},
		{name: "malformed", state: "invalid", action: workspaceOwnerAcquire, want: "AMBIGUOUS", code: 74},
		{name: "malformed child", state: "expired", child: "bad", action: workspaceOwnerAcquire, want: "AMBIGUOUS", code: 74},
		{name: "live child blocks recovery", state: "expired", child: "live", action: workspaceOwnerAcquire, want: "CHILD"},
		{name: "live child blocks release", state: "live", child: "live", action: workspaceOwnerRelease, want: "CHILD", code: 75},
		{name: "occupied gate cannot be stolen", state: "expired", gate: "directory", action: workspaceOwnerAcquire, want: "BUSY"},
		{name: "invalid gate", gate: "file", action: workspaceOwnerAcquire, want: "AMBIGUOUS", code: 74},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.state != "" {
				stateToken, expiry := token, "9999999999"
				if tc.state == "expired" {
					expiry = "1"
				}
				if tc.state == "other" {
					stateToken = strings.Repeat("b", 64)
				}
				state := "v1\n" + stateToken + "\n" + expiry + "\n"
				if tc.state == "invalid" {
					state = "invalid"
				}
				if err := os.WriteFile(filepath.Join(root, key+".owner"), []byte(state), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.child != "" {
				child := tc.child
				if child == "live" {
					pid := strconv.Itoa(os.Getpid())
					identity, err := exec.Command("ps", "-o", "lstart=", "-p", pid).Output()
					if err != nil {
						t.Fatal(err)
					}
					child = pid + "\n" + strings.Join(strings.Fields(string(identity)), " ") + "\n"
				}
				if err := os.WriteFile(filepath.Join(root, key+".child"), []byte(child), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			gate := filepath.Join(root, key+".gate.portable")
			if tc.gate == "directory" {
				if err := os.Mkdir(gate, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.gate == "file" {
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remoteWorkspaceOwnerPOSIX(workspaceOwnerRemoteRequest{Action: tc.action, Key: key, Token: token, TTL: time.Minute}))
			cmd.Env = []string{"HOME=" + home, "PATH=" + path}
			out, err := cmd.CombinedOutput()
			if string(out) != tc.want || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("out=%q err=%v; want %s / %d", out, err, tc.want, tc.code)
			}
			if tc.gate == "" {
				if _, err := os.Stat(gate); !os.IsNotExist(err) {
					t.Fatalf("gate leaked: %v", err)
				}
			}
		})
	}
}

func TestWorkspaceOwnerBSDConcurrentAcquireAndWitness(t *testing.T) {
	path, home := workspaceOwnerBSDPath(t), t.TempDir()
	key, token := workspaceOwnerKey("bsd-concurrent"), strings.Repeat("a", 64)
	run := func(script string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + path}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
	results := make(chan string, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			out, err := run(remoteWorkspaceOwnerPOSIX(req))
			if err != nil {
				results <- err.Error()
			} else {
				results <- out
			}
		})
	}
	wg.Wait()
	close(results)
	winners := 0
	for out := range results {
		if out == "ACQUIRED" {
			winners++
		} else if out != "BUSY" {
			t.Errorf("contender: %q", out)
		}
	}
	if winners != 1 {
		t.Fatalf("acquisition winners=%d", winners)
	}
	for _, tc := range []struct {
		command string
		code    int
	}{{"printf witness-ok", 0}, {"exit 23", 23}, {"printf later-ok", 0}} {
		out, err := run(remoteWorkspaceOwnerPOSIXWitness(key, token, tc.command))
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != tc.code {
			t.Fatalf("witness %q out=%q err=%v", tc.command, out, err)
		}
	}
	req.Action = workspaceOwnerRelease
	if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
		t.Fatalf("release out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerBSDGateRequiresConfirmedDirectoryCreation(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		code         int
	}{
		{name: "success without creation"},
		{name: "output with failure", output: "created", code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nativeMkdir, err := exec.LookPath("mkdir")
			if err != nil {
				t.Fatal(err)
			}
			path, home := workspaceOwnerBSDPath(t), t.TempDir()
			mkdir := filepath.Join(path, "mkdir")
			if err := os.Remove(mkdir); err != nil {
				t.Fatal(err)
			}
			// Model mkdir implementations that return success after losing an
			// EEXIST race, and ensure failed commands cannot grant the gate.
			stub := "#!/bin/sh\ncase \"$*\" in *'.gate.portable'*) printf %s " + shellQuote(tc.output) + "; exit " + strconv.Itoa(tc.code) + " ;; esac\nexec " + shellQuote(nativeMkdir) + " \"$@\"\n"
			if err := os.WriteFile(mkdir, []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			key := workspaceOwnerKey("occupied-portable-gate")
			gate := filepath.Join(home, ".crabbox", "workspace-owners", key+".gate.portable")
			if err := os.MkdirAll(gate, 0o700); err != nil {
				t.Fatal(err)
			}
			req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: strings.Repeat("a", 64), TTL: time.Minute}
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", remoteWorkspaceOwnerPOSIX(req))
			cmd.Env = []string{"HOME=" + home, "PATH=" + path}
			if out, err := cmd.CombinedOutput(); err != nil || string(out) != "BUSY" {
				t.Fatalf("occupied gate: out=%q err=%v", out, err)
			}
			if info, err := os.Stat(gate); err != nil || !info.IsDir() {
				t.Fatalf("contender changed the existing gate: %v", err)
			}
		})
	}
}

func TestWorkspaceOwnerBSDDetachedAndRenewingCommand(t *testing.T) {
	testWorkspaceOwnerDetachedAndRenewingCommand(t, workspaceOwnerBSDPath(t))
}

func TestWorkspaceOwnerPOSIXDetachedAndRenewingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	testWorkspaceOwnerDetachedAndRenewingCommand(t, os.Getenv("PATH"))
}

func testWorkspaceOwnerDetachedAndRenewingCommand(t *testing.T, path string) {
	for _, tc := range []struct {
		name, command string
		code          int
	}{
		{name: "detached daemon", command: `nohup sleep 30 </dev/null >"$HOME/daemon.log" 2>&1 & daemon_pid=$!
ps -o lstart= -p "$daemon_pid" >"$HOME/daemon.identity" || exit 74
echo "$daemon_pid" >"$HOME/daemon.pid"`},
		{name: "renewed stream", command: `echo started >"$HOME/started.tmp"; mv "$HOME/started.tmp" "$HOME/started"
i=0; while [ ! -f "$HOME/continue" ]; do
  [ "$i" -lt 3000 ] || exit 124
  sleep .01; i=$((i+1))
done
i=0; while [ "$i" -lt 5 ]; do echo tick; sleep 1; i=$((i+1)); done; exit 23`, code: 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			clockBin := t.TempDir()
			// Only the protocol clock is controlled; retain native/BSD gate selection.
			if err := os.WriteFile(filepath.Join(clockBin, "date"), []byte("#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = +%s ] || exit 64\ncat \"$HOME/clock\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			setClock := func(now int) {
				t.Helper()
				tmp := filepath.Join(home, "clock.tmp")
				if err := os.WriteFile(tmp, []byte(strconv.Itoa(now)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, filepath.Join(home, "clock")); err != nil {
					t.Fatal(err)
				}
			}
			setClock(1000)
			key, token := workspaceOwnerKey(tc.name), strings.Repeat("c", 64)
			run := func(script string) (string, error) {
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
				cmd.Env = []string{"HOME=" + home, "PATH=" + clockBin + string(os.PathListSeparator) + path}
				cmd.WaitDelay = time.Second
				if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
					configureControllerCommand(cmd)
					cmd.Cancel = func() error { return stopControllerProcessGroup(cmd.Process.Pid) }
				}
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			stateExpiry := func() int {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(home, ".crabbox", "workspace-owners", key+".owner"))
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				if len(lines) != 3 || lines[0] != "v1" || lines[1] != token {
					t.Fatalf("unexpected owner state %q", data)
				}
				expiry, err := strconv.Atoi(lines[2])
				if err != nil {
					t.Fatal(err)
				}
				return expiry
			}
			req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: 3 * time.Second}
			if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
				t.Fatalf("acquire=%q %v", out, err)
			}
			defer func() {
				req.Action = workspaceOwnerRelease
				if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
					t.Errorf("release=%q %v", out, err)
				}
			}()
			var out string
			var err error
			if tc.name == "renewed stream" {
				originalExpiry := stateExpiry()
				if originalExpiry != 1003 {
					t.Fatalf("initial expiry=%d want 1003", originalExpiry)
				}
				type commandResult struct {
					out string
					err error
				}
				finished := make(chan commandResult, 1)
				go func() {
					out, err := run(remoteWorkspaceOwnerPOSIXWitness(key, token, tc.command))
					finished <- commandResult{out, err}
				}()
				joined := false
				// Unblock and join before test-context cancellation, including Fatal paths.
				defer func() {
					if !joined {
						_ = os.WriteFile(filepath.Join(home, "continue"), nil, 0o600)
						<-finished
					}
				}()
				deadline := time.NewTimer(15 * time.Second)
				defer deadline.Stop()
				for {
					if _, statErr := os.Stat(filepath.Join(home, "started")); statErr == nil {
						break
					} else if !os.IsNotExist(statErr) {
						t.Fatal(statErr)
					}
					select {
					case result := <-finished:
						joined = true
						t.Fatalf("command ended before start: %q %v", result.out, result.err)
					case <-deadline.C:
						t.Fatal("command did not signal start")
					case <-time.After(10 * time.Millisecond):
					}
				}
				// Advance only after user code starts, so pre-start admission is independent.
				setClock(1002)
				req.Action = workspaceOwnerRenew
				if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RENEWED" {
					t.Fatalf("renew=%q %v", out, err)
				}
				renewedExpiry := stateExpiry()
				if renewedExpiry != 1005 || renewedExpiry <= originalExpiry {
					t.Fatalf("renewed expiry=%d initial=%d want 1005", renewedExpiry, originalExpiry)
				}
				setClock(1004)
				req.Action = workspaceOwnerInspect
				if out, err := run(remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "CHILD" {
					t.Fatalf("live child beyond initial expiry=%q %v", out, err)
				}
				if err := os.WriteFile(filepath.Join(home, "continue"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				result := <-finished
				joined = true
				out, err = result.out, result.err
			} else {
				// Register cleanup before spawning, so assertion failures cannot retain it.
				defer func() {
					data, readErr := os.ReadFile(filepath.Join(home, "daemon.pid"))
					if readErr != nil {
						return
					}
					pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
					identity, identityErr := os.ReadFile(filepath.Join(home, "daemon.identity"))
					if parseErr != nil || identityErr != nil || len(strings.Fields(string(identity))) == 0 {
						t.Log("daemon cleanup skipped: creation identity unavailable")
						return
					}
					live, probeErr := run("ps -o lstart= -p " + strconv.Itoa(pid))
					if probeErr != nil || strings.Join(strings.Fields(live), " ") != strings.Join(strings.Fields(string(identity)), " ") {
						t.Log("daemon cleanup skipped: no matching live identity")
						return
					}
					process, findErr := os.FindProcess(pid)
					if findErr != nil {
						t.Errorf("find owned daemon: %v", findErr)
					} else if killErr := process.Kill(); killErr != nil {
						t.Errorf("stop owned daemon: %v", killErr)
					}
				}()
				out, err = run(remoteWorkspaceOwnerPOSIXWitness(key, token, tc.command))
			}
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("command=%q %v want %d", out, err, tc.code)
			}
			if tc.code == 23 && strings.Count(out, "tick\n") != 5 {
				t.Fatalf("lost stream output: %q", out)
			}
			if tc.name == "detached daemon" {
				data, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				if out, err := run(remoteWorkspaceOwnerPOSIXWitness(key, token, "kill -0 "+strconv.Itoa(pid))); err != nil {
					t.Fatalf("daemon did not survive subsequent command: %q %v", out, err)
				}
			}
		})
	}
}
