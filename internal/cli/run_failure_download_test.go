package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunExitWitnessCommand(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		code         int
	}{
		{"nonzero", `printf out; printf err >&2; exit 23`, 23},
		{"255 is workload exit", `exit 255`, 255},
		{"exec", `exec sh -c 'exit 37'`, 37},
		{"errexit", `set -e; false; printf should-not-run`, 1},
		{"signal", `kill -TERM $$`, 143},
		{"stdin", `read value; printf '%s' "$value"; exit 23`, 23},
		{"success", `true`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			w, err := newRunExitWitness(&stderr)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", w.command(t.TempDir(), nil, nil, []string{tc.script}, true, nil))
			cmd.Stdout = &out
			cmd.Stderr = w
			cmd.Stdin = strings.NewReader("input\n")
			runErr := cmd.Run()
			if runErr != nil {
				t.Fatalf("transport did not normalize completed exit: %v stderr=%s", runErr, stderr.String())
			}
			code, err, eligible := w.finish(context.Background(), 0, nil)
			if code != tc.code || err != nil || eligible != (tc.code != 0) {
				t.Fatalf("code=%d err=%v eligible=%t", code, err, eligible)
			}
			if strings.Contains(stderr.String(), "CRABBOX_RUN_") {
				t.Fatal("marker leaked into stderr")
			}
			if tc.name == "nonzero" && (out.String() != "out" || stderr.String() != "err") {
				t.Fatalf("output changed: %q %q", out.String(), stderr.String())
			}
			if tc.name == "errexit" && out.Len() != 0 {
				t.Fatal("errexit was suppressed")
			}
			if tc.name == "stdin" && out.String() != "input" {
				t.Fatalf("stdin=%q", out.String())
			}
		})
	}
}

func TestRunExitWitnessExcludesSetup(t *testing.T) {
	for _, setup := range []string{"missing workdir", "failed env file", "failed login"} {
		t.Run(setup, func(t *testing.T) {
			dir := t.TempDir()
			workdir := dir
			var envFiles []string
			env := map[string]string{}
			if setup == "missing workdir" {
				workdir = filepath.Join(dir, "missing")
			} else {
				file := filepath.Join(dir, "setup.sh")
				if err := os.WriteFile(file, []byte("exit 47\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if setup == "failed env file" {
					envFiles = []string{file}
				} else {
					env["BASH_ENV"] = file
				}
			}
			var stdout, stderr bytes.Buffer
			w, err := newRunExitWitness(&stderr)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", w.command(workdir, env, envFiles, []string{"echo should-not-run; exit 23"}, true, nil))
			cmd.Stderr = w
			cmd.Stdout = &stdout
			runErr := cmd.Run()
			transportCode := 0
			if runErr != nil {
				transportCode = exitCode(runErr)
			}
			code, _, eligible := w.finish(context.Background(), transportCode, runErr)
			if eligible || code == 0 || strings.Contains(stdout.String()+stderr.String(), "should-not-run") {
				t.Fatalf("setup eligible=%t code=%d stdout=%q stderr=%q", eligible, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunExitWitnessScriptAndDeadline(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "proof.sh")
	if err := os.WriteFile(scriptPath, []byte("printf '%s' \"$1\"; exit 29\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	w, err := newRunExitWitness(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", w.command(dir, nil, nil, []string{"literal ' $value"}, false, &RunScriptSpec{RemotePath: scriptPath}))
	cmd.Stdout, cmd.Stderr = &output, w
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if code, err, eligible := w.finish(context.Background(), 0, nil); code != 29 || err != nil || !eligible || output.String() != "literal ' $value" {
		t.Fatalf("code=%d err=%v eligible=%t output=%q", code, err, eligible, output.String())
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err, eligible := w.finish(ctx, 0, nil); !errors.Is(err, context.DeadlineExceeded) || eligible {
		t.Fatalf("deadline eligible=%t err=%v", eligible, err)
	}
}

func TestRunExitWitnessFramesAndTransportAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, records string
		transport     int
		cancel        bool
		eligible      bool
		want          int
	}{
		{"split frames", "start|exit:23", 0, false, true, 23},
		{"transport after exit", "start|exit:23", 255, false, false, 255},
		{"canceled after exit", "start|exit:23", 0, true, false, 7},
		{"duplicate exit", "start|exit:23|exit:0", 0, false, false, 7},
		{"duplicate start", "start|start|exit:23", 0, false, false, 7},
		{"invalid code", "start|exit:256", 0, false, false, 7},
		{"missing exit", "start", 0, false, false, 7},
		{"setup nonzero", "exit:47", 0, false, false, 47},
		{"setup zero", "exit:0", 0, false, false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			w, err := newRunExitWitness(&output)
			if err != nil {
				t.Fatal(err)
			}
			stream := "before"
			for _, r := range strings.Split(tc.records, "|") {
				stream += string(w.prefix) + r + "\x1f"
			}
			stream += "after"
			for _, b := range []byte(stream) {
				if _, err := w.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			var transportErr error
			if tc.transport != 0 {
				transportErr = errors.New("transport lost")
			}
			code, _, eligible := w.finish(ctx, tc.transport, transportErr)
			if code != tc.want || eligible != tc.eligible || output.String() != "beforeafter" {
				t.Fatalf("code=%d eligible=%t output=%q", code, eligible, output.String())
			}
		})
	}
	w, _ := newRunExitWitness(io.Discard)
	old, _ := newRunExitWitness(io.Discard)
	_, _ = w.Write(append(append(old.prefix, []byte("start\x1f")...), append(old.prefix, []byte("exit:23\x1f")...)...))
	if code, _, eligible := w.finish(context.Background(), 0, nil); code != 7 || eligible {
		t.Fatal("accepted another invocation's markers")
	}
}

func TestFailureDownloadPreflight(t *testing.T) {
	for _, flags := range [][]string{
		{"--provider", "e2b"},
		{"--provider", "daytona"},
		{"--provider", "ssh", "--target", "macos"},
		{"--provider", "ssh", "--target", "windows", "--windows-mode", "wsl2"},
		{"--provider", "ssh", "--target", "windows"},
		{"--provider", "run-env-profile-test", "--sync-only"},
		{"--provider", "run-env-profile-test", "--download", "proof=out"},
		{"--provider", "run-env-profile-test", "--capture-stderr", "out"},
		{"--provider", "run-env-profile-test", "--emit-proof", "out"},
		{"--provider", "run-env-profile-test", "--attest", "out"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			acquired := false
			runEnvProfileTestAcquireHook = func(AcquireRequest) { acquired = true }
			t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })
			args := append(append([]string{}, flags...), "--download-on-failure", "proof=out", "--", "true")
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), args)
			if exitCodeForError(err, 0) != 2 || acquired {
				t.Fatalf("preflight err=%v acquired=%t", err, acquired)
			}
		})
	}
}

func TestFailureDownloadsE2E(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		transport bool
	}{
		{"failed", 23, false}, {"255 workload", 255, false}, {"success", 0, false}, {"transport after markers", 255, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			_, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			ssh := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
case "$cmd" in
  *CRABBOX_RUN_*)
    printf 'workload\n' >> "$CRABBOX_TEST_WORKLOAD_LOG"
    sh -c "$cmd"
    result=$?
    if [ "$CRABBOX_TEST_TRANSPORT_LOSS" = 1 ]; then exit 255; fi
    exit "$result" ;;
  mkdir\ -p*|cd\ *|bash\ -lc*|/bin/bash\ -lc*) exec sh -c "$cmd" ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(ssh), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", port)
			t.Setenv("CRABBOX_WORK_ROOT", filepath.Join(dir, "remote"))
			t.Setenv("CRABBOX_TEST_WORKLOAD_LOG", filepath.Join(dir, "workloads.log"))
			if tc.transport {
				t.Setenv("CRABBOX_TEST_TRANSPORT_LOSS", "1")
			}
			first := filepath.Join(dir, "downloads", "first.json")
			second := filepath.Join(dir, "downloads", "second.json")
			missing := filepath.Join(dir, "downloads", "missing.json")
			success := filepath.Join(dir, "success.json")
			wantDownloads := tc.code != 0 && !tc.transport
			releases := 0
			runEnvProfileTestReleaseHook = func() error {
				releases++
				_, err := os.Stat(second)
				if wantDownloads && err != nil {
					return fmt.Errorf("download missing before teardown: %w", err)
				}
				return nil
			}
			t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })
			workloadCode := tc.code
			if tc.transport {
				workloadCode = 23
			}
			command := fmt.Sprintf("mkdir -p reports; printf first > reports/first.json; printf second > reports/second.json; printf output; printf diagnostic >&2; exit %d", workloadCode)
			var stdout, stderr bytes.Buffer
			err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{"--provider", "run-env-profile-test", "--no-sync", "--stop-after", "always", "--download-on-failure", "reports/first.json=" + first, "--download-on-failure", "reports/missing.json=" + missing, "--download-on-failure", "reports/second.json=" + second, "--download", "reports/first.json=" + success, "--shell", "--", command})
			if exitCodeForError(err, 0) != tc.code {
				t.Fatalf("err=%v stderr=%s", err, stderr.String())
			}
			for file, want := range map[string]string{first: "first", second: "second"} {
				data, readErr := os.ReadFile(file)
				if wantDownloads {
					if readErr != nil || string(data) != want {
						t.Fatalf("file=%s data=%q err=%v stderr=%s", file, data, readErr, stderr.String())
					}
					info, _ := os.Stat(file)
					if info.Mode().Perm() != 0600 {
						t.Fatal("download is not private")
					}
				} else if !os.IsNotExist(readErr) {
					t.Fatalf("ineligible download exists: %s", file)
				}
			}
			if _, err := os.Stat(missing); !os.IsNotExist(err) {
				t.Fatal("missing evidence created output residue")
			}
			if _, err := os.Stat(success); (err == nil) != (tc.code == 0) {
				t.Fatal("success-only download behavior changed")
			}
			if wantDownloads {
				info, _ := os.Stat(filepath.Dir(first))
				if info.Mode().Perm() != 0700 {
					t.Fatal("download directory is not private")
				}
				if !strings.Contains(stderr.String(), "warning: --download-on-failure") {
					t.Fatal("missing evidence was silent")
				}
				if strings.Index(stderr.String(), "downloaded after failure") > strings.Index(stderr.String(), "failure-bundle local=") {
					t.Fatal("download ran after failure bundle")
				}
			}
			if stdout.String() != "output" || strings.Contains(stderr.String(), "CRABBOX_RUN_") {
				t.Fatalf("stream changed stdout=%q stderr=%s", stdout.String(), stderr.String())
			}
			calls, err := os.ReadFile(filepath.Join(dir, "workloads.log"))
			if err != nil || string(calls) != "workload\n" {
				t.Fatalf("workload replayed: %q %v", calls, err)
			}
			if releases != 1 {
				t.Fatalf("releases=%d", releases)
			}
		})
	}
}
