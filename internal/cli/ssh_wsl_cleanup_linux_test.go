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

type wslStageLinuxFixture struct {
	directory  string
	command    *exec.Cmd
	control    *os.File
	output     synchronizedBuffer
	diagnostic synchronizedBuffer
	done       chan error
	group      int
}

func startWSLStageLinuxFixture(t *testing.T, command string, input []byte) *wslStageLinuxFixture {
	return startWSLStageLinuxFixtureWithGrace(t, command, input, 300*time.Millisecond)
}

func startWSLStageLinuxFixtureWithGrace(t *testing.T, command string, input []byte, grace time.Duration) *wslStageLinuxFixture {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Fatal("WSL2 supervisor proof requires setsid")
	}
	fixture := &wslStageLinuxFixture{
		directory: filepath.Join(t.TempDir(), "owned"),
		done:      make(chan error, 1),
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fixture.control = writer
	fixture.command = exec.Command(
		"sh", "-c", wslStageHelperBootstrap, "sh",
		strconv.Itoa(len(wslStageLinuxSupervisor)), "0", "run",
		fixture.directory, "nonce", strconv.Itoa(len(command)), strconv.Itoa(len(input)),
		"300", strconv.FormatInt(grace.Milliseconds(), 10),
	)
	fixture.command.Stdin = reader
	fixture.command.Stdout = &fixture.output
	fixture.command.Stderr = &fixture.diagnostic
	if err := fixture.command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	go func() { fixture.done <- fixture.command.Wait() }()
	t.Cleanup(func() {
		_ = writer.Close()
		if fixture.group > 0 {
			_ = syscall.Kill(-fixture.group, syscall.SIGKILL)
		}
		_ = fixture.command.Process.Kill()
	})
	if _, err := io.Copy(writer, io.MultiReader(
		strings.NewReader(wslStageLinuxSupervisor),
		strings.NewReader(command),
		bytes.NewReader(input),
	)); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *wslStageLinuxFixture) wait(t *testing.T, code int) {
	t.Helper()
	select {
	case err := <-f.done:
		if exitCode(err) != code {
			t.Fatalf("exit=%d want=%d: %v\nstdout:\n%s\nstderr:\n%s", exitCode(err), code, err, f.output.String(), f.diagnostic.String())
		}
	case <-time.After(12 * time.Second):
		t.Fatalf("supervisor did not finish\nstdout:\n%s\nstderr:\n%s", f.output.String(), f.diagnostic.String())
	}
}

func (f *wslStageLinuxFixture) waitArmed(t *testing.T) (guard int) {
	t.Helper()
	waitForWSLStageFile(t, filepath.Join(f.directory, ".armed"))
	data, err := os.ReadFile(filepath.Join(f.directory, ".owned"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		t.Fatalf("invalid guard record %q", data)
	}
	guard, err = strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	f.group, err = strconv.Atoi(fields[2])
	if err != nil {
		t.Fatal(err)
	}
	group, start, state, err := wslStageLinuxProcess(guard)
	if err != nil || state == "Z" || group != f.group || start != fields[1] {
		t.Fatalf("unproven guard %q: group=%d start=%q state=%q err=%v", data, group, start, state, err)
	}
	return guard
}

func waitForWSLStageFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func wslStageLinuxProcess(pid int) (group int, start, state string, err error) {
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

func requireWSLStageLinuxClean(t *testing.T, fixture *wslStageLinuxFixture) {
	t.Helper()
	if fixture.group > 0 {
		if err := syscall.Kill(-fixture.group, 0); err != syscall.ESRCH {
			t.Fatalf("owned process group remains: %v", err)
		}
	}
	if _, err := os.Stat(fixture.directory); !os.IsNotExist(err) {
		t.Fatalf("nonce residue remains: %v", err)
	}
}

func TestWSLStageLinuxPassesExitOutputAndFiniteInput(t *testing.T) {
	input := []byte{0, 1, 255, '\n'}
	fixture := startWSLStageLinuxFixture(t, "printf stdout; printf stderr >&2; cat; exit 23\n", input)
	fixture.waitArmed(t)
	fixture.wait(t, 23)
	if want := append([]byte("stdout"), input...); !bytes.Equal(fixture.output.Bytes(), want) {
		t.Fatalf("workload stdout changed: %q", fixture.output.String())
	}
	if fixture.diagnostic.String() != "stderr" {
		t.Fatalf("workload stderr changed: %q", fixture.diagnostic.String())
	}
	requireWSLStageLinuxClean(t, fixture)
}

func TestWSLStageLinuxNormalExitSkipsSignalGrace(t *testing.T) {
	start := time.Now()
	fixture := startWSLStageLinuxFixtureWithGrace(t, "exit 0\n", nil, 5*time.Second)
	fixture.waitArmed(t)
	fixture.wait(t, 0)
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("normal completion waited for signal grace: %s", elapsed)
	}
	requireWSLStageLinuxClean(t, fixture)
}

func TestWSLStageLinuxDisconnectKillsExactProcessGroup(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	command := "trap '' TERM\n: >" + shellQuote(started) + "\nwhile :; do sleep 1; done\n"
	fixture := startWSLStageLinuxFixture(t, command, nil)
	fixture.waitArmed(t)
	waitForWSLStageFile(t, started)
	if err := fixture.control.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.wait(t, 74)
	requireWSLStageLinuxClean(t, fixture)
}

func TestWSLStageLinuxGuardMismatchPreservesEvidence(t *testing.T) {
	fixture := startWSLStageLinuxFixture(t, "while :; do sleep 1; done\n", nil)
	fixture.waitArmed(t)
	if err := os.WriteFile(filepath.Join(fixture.directory, ".owned"), []byte("1 1 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.control.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.wait(t, 74)
	if _, err := os.Stat(fixture.directory); err != nil {
		t.Fatalf("guard mismatch erased evidence: %v", err)
	}
}

func TestWSLStageLinuxPreOwnershipFailureRemovesExactResidue(t *testing.T) {
	head, err := exec.LookPath("head")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	failingHead := filepath.Join(bin, "head")
	script := "#!/bin/sh\nif [ \"$#\" -eq 2 ]; then exec " + shellQuote(head) + " \"$@\"; fi\nexit 1\n"
	if err := os.WriteFile(failingHead, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	fixture := startWSLStageLinuxFixture(t, "exit 0\n", []byte("finite-input"))
	fixture.wait(t, 74)
	if _, err := os.Stat(fixture.directory); !os.IsNotExist(err) {
		t.Fatalf("pre-ownership failure retained nonce residue: %v", err)
	}
}

func TestWSLStageLinuxNormalExitPreservesDetachedBackground(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "detached.pid")
	command := "setsid sh -c 'trap \"\" HUP TERM; while :; do sleep 1; done' </dev/null >/dev/null 2>&1 &\n" +
		"printf '%s' \"$!\" >" + shellQuote(pidPath) + "\nexit 19\n"
	fixture := startWSLStageLinuxFixture(t, command, nil)
	fixture.waitArmed(t)
	waitForWSLStageFile(t, pidPath)
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	_, start, state, err := wslStageLinuxProcess(pid)
	if err != nil || state == "Z" {
		t.Fatalf("detached process did not start: %v", err)
	}
	t.Cleanup(func() {
		if _, current, _, err := wslStageLinuxProcess(pid); err == nil && current == start {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	fixture.wait(t, 19)
	requireWSLStageLinuxClean(t, fixture)
	if _, current, state, err := wslStageLinuxProcess(pid); err != nil || current != start || state == "Z" {
		t.Fatalf("intentional detached process did not survive: state=%q err=%v", state, err)
	}
}
