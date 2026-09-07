//go:build linux

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// No test subreaper: production must reap its own guard and leader. A kernel
// that retains an orphan zombie must produce cleanup ambiguity, not success.
type wslLinuxFixture struct {
	directory string
	command   *exec.Cmd
	control   *os.File
	output    synchronizedBuffer
	done      chan error
	group     int
}

func startWSLLinuxFixture(t *testing.T, command string, payload []byte, declared int, helper string, grace int) *wslLinuxFixture {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Fatal("native fixture requires the documented setsid prerequisite")
	}
	f := &wslLinuxFixture{directory: filepath.Join(t.TempDir(), "owned"), done: make(chan error, 1)}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	f.control = writer
	f.command = exec.Command("sh", "-c", wslHelperBootstrap, "sh", strconv.Itoa(len(helper)), "0", "run", f.directory, "nonce", strconv.Itoa(declared), strconv.Itoa(len(payload)), "300", strconv.Itoa(grace))
	f.command.Stdin = reader
	f.command.Stdout = &f.output
	f.command.Stderr = &f.output
	if err := f.command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	go func() { f.done <- f.command.Wait() }()
	t.Cleanup(func() {
		_ = writer.Close()
		// These processes are fixture-created; never find groups by age/name.
		if f.group > 0 {
			_ = syscall.Kill(-f.group, syscall.SIGKILL)
		}
		_ = f.command.Process.Kill()
	})
	if _, err := io.Copy(writer, io.MultiReader(strings.NewReader(helper), strings.NewReader(command), bytes.NewReader(payload))); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *wslLinuxFixture) wait(t *testing.T, code int) {
	t.Helper()
	select {
	case err := <-f.done:
		if exitCode(err) != code {
			t.Fatalf("exit=%d want=%d: %v\n%s", exitCode(err), code, err, f.output.String())
		}
	case <-time.After(12 * time.Second):
		t.Fatalf("supervisor did not finish\n%s", f.output.String())
	}
}

func (f *wslLinuxFixture) guard(t *testing.T) (pid int, identity string) {
	t.Helper()
	waitForTestFile(t, filepath.Join(f.directory, ".armed"), 5*time.Second)
	data, err := os.ReadFile(filepath.Join(f.directory, ".crabbox-owned"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		t.Fatalf("guard record=%q", data)
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	f.group, err = strconv.Atoi(fields[2])
	if err != nil {
		t.Fatal(err)
	}
	group, start, _, err := wslLinuxProcess(pid)
	if err != nil || group != f.group || start != fields[1] {
		t.Fatalf("unproven guard: %q %v", data, err)
	}
	return pid, fields[1]
}

func wslLinuxProcess(pid int) (group int, start, state string, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, "", "", err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return 0, "", "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) < 20 {
		return 0, "", "", fmt.Errorf("short process stat")
	}
	group, err = strconv.Atoi(fields[2])
	return group, fields[19], fields[0], err
}

func wslLinuxGroupMembers(t *testing.T, group int) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var result []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		g, _, _, err := wslLinuxProcess(pid)
		if err == nil && g == group {
			result = append(result, pid)
		}
	}
	return result
}

func assertWSLLinuxAbsent(t *testing.T, f *wslLinuxFixture) {
	t.Helper()
	if f.group > 0 {
		if err := syscall.Kill(-f.group, 0); err != syscall.ESRCH {
			t.Fatalf("group remains: %v members=%v", err, wslLinuxGroupMembers(t, f.group))
		}
	}
	if _, err := os.Stat(f.directory); !os.IsNotExist(err) {
		t.Fatalf("evidence remains after successful cleanup: %v", err)
	}
}

func TestWorkspaceOwnerWSL2WatchdogAllowsCompletedFrameExecution(t *testing.T) {
	for _, test := range []struct {
		name  string
		grace time.Duration
		code  int
	}{
		{"short grace nonzero exit", 100 * time.Millisecond, 23},
		{"production grace success", wsl2SignalGrace, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := "printf stdout; printf stderr >&2; cat; sleep 1; exit " + strconv.Itoa(test.code) + "\n"
			payload := []byte{0, 1, 255, 13, 10}
			f := startWSLLinuxFixture(t, command, payload, len(command), wslLinuxHelper, int(test.grace.Milliseconds()))
			guard, _ := f.guard(t)
			if err := syscall.Kill(guard, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			if _, _, state, err := wslLinuxProcess(guard); err != nil || state == "Z" {
				t.Fatalf("guard did not survive TERM: %v", err)
			}
			// Deliberately keep the sole control writer open through workload exit.
			f.wait(t, test.code)
			if !bytes.Contains(f.output.Bytes(), append([]byte("stdout"), payload...)) && !bytes.Contains(f.output.Bytes(), payload) {
				t.Fatal("finite binary input changed")
			}
			if !strings.Contains(f.output.String(), "stderr") {
				t.Fatal("stderr lost")
			}
			assertWSLLinuxAbsent(t, f)
		})
	}
}

func installWSLResultWriteFault(t *testing.T, fault string) (string, *os.File) {
	t.Helper()
	root := t.TempDir()
	gate := filepath.Join(root, "gate")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(gate, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = file.WriteString("continue\n"); _ = file.Close() })
	paused := filepath.Join(root, "paused")
	env := filepath.Join(root, "bash-env")
	// Intercept the real shell's result write after redirection, without
	// changing the supervisor or using a timing race to expose an empty file.
	script := `printf() {
    local destination writer=$BASHPID
    destination=$(readlink "/proc/$writer/fd/1")
    case $destination in
        */.result|*/.result.tmp)
            case $CBX_TEST_RESULT_FAULT in
                pause)
                    : >"$CBX_TEST_RESULT_PAUSED"
                    IFS= read -r -t 5 <"$CBX_TEST_RESULT_GATE" || return 1
                    ;;
                empty) return 0;;
                invalid) builtin printf '%s\n' 'not-a-status'; return;;
                out-of-range) builtin printf '%s\n' 256; return;;
                unterminated) builtin printf '%s' 23; return;;
                extra-line) builtin printf '0\n23\n'; return;;
                extra-bytes) builtin printf '0\n23'; return;;
                write-failure) return 1;;
            esac
            ;;
    esac
    builtin printf "$@"
}
`
	if err := os.WriteFile(env, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", env)
	t.Setenv("CBX_TEST_RESULT_FAULT", fault)
	t.Setenv("CBX_TEST_RESULT_PAUSED", paused)
	t.Setenv("CBX_TEST_RESULT_GATE", gate)
	return paused, file
}

func TestWSL2ResultPublicationWaitsForCompleteStatus(t *testing.T) {
	paused, gate := installWSLResultWriteFault(t, "pause")
	command := "exit 23\n"
	f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 100)
	f.guard(t)
	waitForTestFile(t, paused, 5*time.Second)
	if _, err := os.Stat(filepath.Join(f.directory, ".result")); !os.IsNotExist(err) {
		t.Error("result published before its exit status was written")
	}
	select {
	case err := <-f.done:
		t.Fatalf("supervisor exited before result publication: %v\n%s", err, f.output.String())
	case <-time.After(250 * time.Millisecond):
	}
	if _, err := gate.WriteString("continue\n"); err != nil {
		t.Fatal(err)
	}
	f.wait(t, 23)
	assertWSLLinuxAbsent(t, f)
}

func TestWSL2InvalidResultFailsClosed(t *testing.T) {
	for _, fault := range []string{"empty", "invalid", "out-of-range", "unterminated", "extra-line", "extra-bytes", "write-failure"} {
		t.Run(fault, func(t *testing.T) {
			installWSLResultWriteFault(t, fault)
			command := "sleep .2; exit 23\n"
			f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 100)
			f.guard(t)
			f.wait(t, 74)
			assertWSLLinuxAbsent(t, f)
		})
	}
}

func TestWSL2OrdinaryShortFrameWatchdogCleansState(t *testing.T) {
	for _, closePipe := range []bool{false, true} {
		t.Run(fmt.Sprint(closePipe), func(t *testing.T) {
			f := startWSLLinuxFixture(t, "short", nil, 10, wslLinuxHelper, 100)
			if closePipe {
				_ = f.control.Close()
			}
			f.wait(t, 74)
			assertWSLLinuxAbsent(t, f)
		})
	}
}

func TestWSL2MarkerPublicationFailureLeavesUnarmedDiagnosticState(t *testing.T) {
	// Fail the actual atomic publication, after writing its temporary record.
	helper := strings.Replace(wslLinuxHelper, `mv "$directory/.crabbox-owned.tmp" "$directory/.crabbox-owned"`, `false`, 1)
	if helper == wslLinuxHelper {
		t.Fatal("publication injection missing")
	}
	f := startWSLLinuxFixture(t, "exit 0\n", nil, 7, helper, 100)
	f.wait(t, 74)
	if _, err := os.Stat(filepath.Join(f.directory, ".crabbox-owned.tmp")); err != nil {
		t.Fatal("unarmed diagnostic record lost")
	}
	if _, err := os.Stat(filepath.Join(f.directory, ".armed")); !os.IsNotExist(err) {
		t.Fatal("failed publication armed execution")
	}
}

func TestWSL2GuardSurvivesPublishedMarkerBeforeArm(t *testing.T) {
	helper := strings.Replace(wslLinuxHelper, `: >"$directory/.armed"`, `sleep .5; : >"$directory/.armed"`, 1)
	command := "exit 0\n"
	f := startWSLLinuxFixture(t, command, nil, len(command), helper, 100)
	waitForTestFile(t, filepath.Join(f.directory, ".crabbox-owned"), 5*time.Second)
	data, _ := os.ReadFile(filepath.Join(f.directory, ".crabbox-owned"))
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		t.Fatalf("record=%q", data)
	}
	pid, _ := strconv.Atoi(fields[0])
	f.group, _ = strconv.Atoi(fields[2])
	if _, err := os.Stat(filepath.Join(f.directory, ".armed")); !os.IsNotExist(err) {
		t.Fatal("fixture missed publication interruption")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = f.control.Close()
	f.wait(t, 74)
	assertWSLLinuxAbsent(t, f)
}

func TestWSL2ProductionCleanupKillsEntireStagingGroup(t *testing.T) {
	for _, leaderFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(leaderFirst), func(t *testing.T) {
			// A TERM-resistant shell child remains in the owned group.
			started := filepath.Join(t.TempDir(), "started")
			command := "trap '' TERM\n: >" + shellQuote(started) + "\nwhile :; do read -r -t 1 -u 7 ignored || :; done 7<>\"$1\"\n"
			fifo := filepath.Join(t.TempDir(), "wait")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			command = "set -- " + shellQuote(fifo) + "\n" + command
			f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 100)
			guard, _ := f.guard(t)
			waitForTestFile(t, started, 5*time.Second)
			if leaderFirst {
				// The workload helper is a direct child of the supervisor,
				// independently of the guard. Kill only that exact leader.
				data, err := os.ReadFile(filepath.Join(f.directory, ".supervisor"))
				if err != nil {
					t.Fatal(err)
				}
				supervisor := strings.Fields(string(data))[0]
				children, err := os.ReadFile("/proc/" + supervisor + "/task/" + supervisor + "/children")
				if err != nil {
					t.Fatal(err)
				}
				killed := false
				for _, child := range strings.Fields(string(children)) {
					pid, _ := strconv.Atoi(child)
					group, _, _, err := wslLinuxProcess(pid)
					if err == nil && group == f.group && pid != guard {
						if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
							t.Fatal(err)
						}
						killed = true
					}
				}
				if !killed {
					t.Fatal("no workload leader found")
				}
			}
			_ = f.control.Close()
			f.wait(t, 74)
			assertWSLLinuxAbsent(t, f)
		})
	}
	t.Run("foreground child cancellation", func(t *testing.T) {
		childPath := filepath.Join(t.TempDir(), "child")
		readyPath := childPath + ".ready"
		command := `sleep 60 &
child=$!
trap 'kill "$child" 2>/dev/null || :; wait "$child" 2>/dev/null || :; exit 74' TERM
printf '%s' "$child" >` + shellQuote(childPath) + ` && : >` + shellQuote(readyPath) + `
wait "$child"
`
		f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 500)
		f.guard(t)
		// Redirection creates childPath before printf has published its PID.
		waitForTestFile(t, readyPath, 5*time.Second)
		data, err := os.ReadFile(childPath)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatal(err)
		}
		group, start, _, err := wslLinuxProcess(pid)
		if err != nil || group != f.group {
			t.Fatalf("accidental child escaped stage group: %d %v", group, err)
		}
		_ = f.control.Close()
		f.wait(t, 74)
		assertWSLLinuxAbsent(t, f)
		if _, got, _, err := wslLinuxProcess(pid); err == nil && got == start {
			t.Fatal("foreground child survived cancellation")
		}
	})

}

func TestWSL2ProductionCleanupFailsClosedWithoutLiveGuard(t *testing.T) {
	for _, record := range []string{"4194303 1 %d\n", "corrupt 1 %d\n"} {
		t.Run(strings.Fields(record)[0], func(t *testing.T) {
			directory := t.TempDir()
			term := filepath.Join(t.TempDir(), "term")
			foreign := exec.Command("setsid", "bash", "-c", `trap 'printf term >"$1"' TERM; while :;do sleep 1;done`, "sh", term)
			if err := foreign.Start(); err != nil {
				t.Fatal(err)
			}
			group := foreign.Process.Pid
			t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL); _ = foreign.Wait() })
			marker := fmt.Sprintf(record, group)
			for name, data := range map[string]string{".nonce": "nonce", ".supervisor": "4194303 1 1\n", ".crabbox-owned": marker} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cleanup := exec.Command("sh", "-c", wslHelperBootstrap, "sh", strconv.Itoa(len(wslLinuxHelper)), "0", "cleanup", directory, "nonce", "0", "0", "300", "100")
			cleanup.Stdin = strings.NewReader(wslLinuxHelper)
			output, err := cleanup.CombinedOutput()
			if exitCode(err) != 74 {
				t.Fatalf("invalid guard accepted: %v %s", err, output)
			}
			if err := syscall.Kill(-group, 0); err != nil {
				t.Fatalf("unrelated group stopped: %v", err)
			}
			if _, err := os.Stat(term); !os.IsNotExist(err) {
				t.Fatal("unrelated group received TERM")
			}
			if data, err := os.ReadFile(filepath.Join(directory, ".crabbox-owned")); err != nil || string(data) != marker {
				t.Fatal("foreign marker changed")
			}
		})
	}
	command := "sleep 3\n"
	f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 100)
	guard, _ := f.guard(t)
	if err := syscall.Kill(guard, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = f.control.Close()
	// The supervisor cannot safely signal after witness loss, and output may
	// remain open in the preserved workload until it exits naturally.
	f.wait(t, 74)
	if _, err := os.Stat(filepath.Join(f.directory, ".crabbox-owned")); err != nil {
		t.Fatal("lost-guard evidence removed")
	}
	if !strings.Contains(f.output.String(), "group absence unconfirmed") {
		t.Fatal("lost guard did not report ambiguity")
	}
}

func TestWSL2ProductionCleanupFailsClosedWhenGuardMarkerChanges(t *testing.T) {
	for _, duringTERM := range []bool{false, true} {
		t.Run(fmt.Sprint(duringTERM), func(t *testing.T) {
			proof := t.TempDir()
			fifo := filepath.Join(proof, "wait")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			term, ready := filepath.Join(proof, "term"), filepath.Join(proof, "ready")
			command := "trap " + shellQuote("printf term >"+shellQuote(term)) + " TERM\n: >" + shellQuote(ready) + "\nwhile :; do read -r -t 1 -u 7 ignored || :; done 7<>" + shellQuote(fifo) + "\n"
			f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 500)
			guard, start := f.guard(t)
			waitForTestFile(t, ready, 5*time.Second)
			if duringTERM {
				_ = f.control.Close()
				waitForTestFile(t, term, 5*time.Second)
			}
			replacement := fmt.Sprintf("%d %s %d\n", guard, start+"0", f.group)
			path := filepath.Join(f.directory, ".crabbox-owned")
			if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
				t.Fatal(err)
			}
			_ = f.control.Close()
			waitForWSLLinuxSupervisorExit(t, f)
			if _, _, state, err := wslLinuxProcess(guard); err != nil || state == "Z" {
				t.Fatalf("changed witness authorized KILL: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != replacement {
				t.Fatal("changed witness was removed")
			}
			if !duringTERM {
				if _, err := os.Stat(term); !os.IsNotExist(err) {
					t.Fatal("changed witness authorized TERM")
				}
			}
			_ = syscall.Kill(-f.group, syscall.SIGKILL)
			f.wait(t, 74)
		})
	}
}

func waitForWSLLinuxSupervisorExit(t *testing.T, f *wslLinuxFixture) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.directory, ".supervisor"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.Fields(string(data))[0])
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, _, state, err := wslLinuxProcess(pid)
		if os.IsNotExist(err) || state == "Z" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("supervisor did not report bounded ambiguity")
}

func TestWorkspaceOwnerWSL2WatchdogCleansCompletedFrameAfterLauncherExit(t *testing.T) {
	command := "sleep 10\n"
	f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 100)
	f.guard(t)
	// Closing the only writer models launcher loss after exact materialization.
	// The independent supervisor must survive and finish group cleanup itself.
	_ = f.control.Close()
	f.wait(t, 74)
	assertWSLLinuxAbsent(t, f)
}

func waitForTestFile(t *testing.T, path string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}

func TestWorkspaceOwnerWSL2WatchdogPreservesIntentionalBackgroundWitness(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		t.Run(fmt.Sprintf("expiry=%t", expiry), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			owner := &workspaceOwner{target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, key: workspaceOwnerKey("background"), token: strings.Repeat("a", 64)}
			req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: owner.key, Token: owner.token, TTL: time.Minute}
			if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
				t.Fatalf("acquire=%q %v", out, err)
			}
			// Stop via the existing payload protocol even if an assertion fails.
			t.Cleanup(func() { _, _ = runPOSIXWorkspaceOwnerScript(t, home, owner.rsyncStopCommand()) })
			effects := filepath.Join(home, "effects")
			// Observe the actual rsync guard loop without replacing its stop,
			// token, expiry, or active-rsync checks.
			payload := strings.Replace(owner.rsyncGuardPayload(filepath.Join(home, "destination")), "sleep 0.2", "printf . >>"+shellQuote(effects)+"; sleep 0.2", 1)
			command := owner.wrapPOSIXBackgroundCommand(payload)
			f := startWSLLinuxFixture(t, command, nil, len(command), wslLinuxHelper, 500)
			f.guard(t)
			childPath := filepath.Join(home, ".crabbox", "workspace-owners", owner.key+".child")
			waitForTestFile(t, childPath, 5*time.Second)
			witness, err := os.ReadFile(childPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(witness)), "\n")
			if len(lines) != 2 {
				t.Fatalf("child witness=%q", witness)
			}
			pid, err := strconv.Atoi(lines[0])
			if err != nil {
				t.Fatal(err)
			}
			group, started, _, err := wslLinuxProcess(pid)
			if err != nil || group == f.group {
				t.Fatalf("background remained in stage group: %d %d %v", group, f.group, err)
			}
			f.wait(t, 0)
			assertWSLLinuxAbsent(t, f)
			before, err := os.ReadFile(effects)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(500 * time.Millisecond)
			after, err := os.ReadFile(effects)
			g, identity, state, processErr := wslLinuxProcess(pid)
			if err != nil || len(after) <= len(before) || processErr != nil || g != group || identity != started || state == "Z" {
				t.Fatalf("background stopped after stage completion: bytes=%d->%d identity=%s/%s state=%s err=%v/%v", len(before), len(after), started, identity, state, err, processErr)
			}
			actual, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
			if err != nil || strings.Join(strings.Fields(string(actual)), " ") != lines[1] {
				t.Fatalf("owner identity changed: %q %v", actual, err)
			}
			if expiry {
				statePath := filepath.Join(home, ".crabbox", "workspace-owners", owner.key+".owner")
				if err := os.WriteFile(statePath, []byte("v1\n"+owner.token+"\n1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if out, err := runPOSIXWorkspaceOwnerScript(t, home, owner.rsyncStopCommand()); err != nil {
				t.Fatalf("stop=%q %v", out, err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(childPath); os.IsNotExist(err) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := os.Stat(childPath); !os.IsNotExist(err) {
				t.Fatalf("witness not cleared after stop/expiry: %v", err)
			}
			if _, got, _, err := wslLinuxProcess(pid); err == nil && got == started {
				t.Fatal("owned child was not reaped")
			}
		})
	}
}
