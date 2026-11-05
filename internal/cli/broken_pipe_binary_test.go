//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
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
)

// Exercise the real executable and Docker Sandbox adapter without a daemon.
// The fake sbx records resource deletion separately from Crabbox's claim store.
func TestBrokenPipeBinaryCleanup(t *testing.T) {
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		stream         string
		keepFlag       string
		reuse          bool
		mode           string
		signal         os.Signal
		closeStderr    bool
		cleanupFailure bool
		wantExit       int
	}{
		{name: "stdout", stream: "stdout", wantExit: 141},
		{name: "stderr", stream: "stderr", wantExit: 141},
		{name: "both", stream: "stdout", closeStderr: true, wantExit: 141},
		{name: "cleanup-failure", stream: "stdout", cleanupFailure: true, wantExit: 141},
		{name: "keep", stream: "stdout", keepFlag: "--keep", wantExit: 141},
		{name: "keep-on-failure", stream: "stdout", keepFlag: "--keep-on-failure", wantExit: 141},
		{name: "reused", stream: "stdout", reuse: true, wantExit: 141},
		{name: "full-output", stream: "stdout", mode: "finish"},
		{name: "command-failure", stream: "stdout", mode: "finish", wantExit: 7},
		{name: "interrupt", stream: "stdout", mode: "hold", signal: os.Interrupt, wantExit: 1},
		{name: "terminate", stream: "stdout", mode: "hold", signal: syscall.SIGTERM, wantExit: 1},
		{name: "interrupt-then-closed-stderr", stream: "stdout", mode: "hold", signal: os.Interrupt, closeStderr: true, wantExit: 141},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			fixture := filepath.Join(root, "sbx")
			script := `#!/bin/sh
set -eu
case "$1" in
create)
  printf '%s\n' "$3" > "$PIPE_FIXTURE/resource"
  ;;
exec)
  printf '%s\n' "$$" > "$PIPE_FIXTURE/workload-pid"
  if [ "$PIPE_STREAM" = stderr ]; then exec 1>&2; fi
  printf 'workload-ready\n'
  case "$PIPE_MODE" in
    finish) printf 'workload-finished\n'; exit "$PIPE_EXIT" ;;
    hold) exec sleep 60 ;;
  esac
  sleep 0.2
  printf '%65536s' x
  exec sleep 60
  ;;
rm)
  if [ "$PIPE_REMOVE_FAIL" = true ]; then echo "synthetic removal failure" >&2; exit 1; fi
  rm "$PIPE_FIXTURE/resource"
  printf 'removed\n' > "$PIPE_FIXTURE/removed"
  ;;
*) exit 90 ;;
esac
`
			if err := os.WriteFile(fixture, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			args := []string{"run", "--provider", "docker-sandbox", "--docker-sandbox-cli", fixture}
			if tc.keepFlag != "" {
				args = append(args, tc.keepFlag)
			}
			cmd := exec.CommandContext(ctx, binary)
			cmd.WaitDelay = time.Second

			cmd.Dir = repo
			// Keep operator credentials/configuration and state out of the fixture.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_STATE_HOME=" + filepath.Join(root, "state"), "CRABBOX_CONFIG=" + filepath.Join(root, "missing.yaml"), "PIPE_FIXTURE=" + root, "PIPE_STREAM=" + tc.stream, "PIPE_MODE=" + tc.mode, "PIPE_EXIT=" + strconv.Itoa(tc.wantExit), "PIPE_REMOVE_FAIL=" + strconv.FormatBool(tc.cleanupFailure)}
			if tc.reuse {
				warmup := exec.CommandContext(ctx, binary, "warmup", "--provider", "docker-sandbox", "--docker-sandbox-cli", fixture, "--keep")
				warmup.Dir, warmup.Env = cmd.Dir, cmd.Env
				if out, err := warmup.CombinedOutput(); err != nil {
					t.Fatalf("warmup: %v: %s", err, out)
				}
				name, err := os.ReadFile(filepath.Join(root, "resource"))
				if err != nil {
					t.Fatal(err)
				}
				args = append(args, "--id", "dsbx_"+strings.TrimSpace(string(name)))
			}
			cmd.Args = append(cmd.Args, append(args, "--", "echo", "fixture")...)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			var other bytes.Buffer
			if tc.stream == "stdout" {
				cmd.Stdout, cmd.Stderr = writer, &other
			} else {
				cmd.Stdout, cmd.Stderr = &other, writer
			}
			var stderrReader, stderrWriter *os.File
			if tc.closeStderr {
				stderrReader, stderrWriter, err = os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer stderrReader.Close()
				defer stderrWriter.Close()
				cmd.Stderr = stderrWriter
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			if stderrWriter != nil {
				stderrWriter.Close()
			}
			// Always remove the synthetic workload, including on an unfixed binary.
			workloadGone := false
			t.Cleanup(func() {
				if workloadGone {
					return
				}
				data, _ := os.ReadFile(filepath.Join(root, "workload-pid"))
				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			buffered := bufio.NewReader(reader)
			ready := false
			for {
				line, err := buffered.ReadString('\n')
				if err != nil {
					break
				}
				if strings.TrimSpace(line) == "workload-ready" {
					ready = true
					break
				}
			}
			if stderrReader != nil {
				stderrReader.Close()
			}
			if tc.signal != nil {
				if err := cmd.Process.Signal(tc.signal); err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, buffered)
			} else if tc.mode == "finish" {
				remaining, err := io.ReadAll(buffered)
				if err != nil || !strings.Contains(string(remaining), "workload-finished") {
					t.Errorf("full output missing: %q: %v", remaining, err)
				}
			}
			reader.Close()
			err = cmd.Wait()
			if !ready {
				t.Fatalf("workload never started: %v; %s", err, &other)
			}
			kept := tc.keepFlag != "" || tc.reuse || tc.cleanupFailure
			if tc.cleanupFailure && !strings.Contains(other.String(), "synthetic removal failure") {
				t.Error("cleanup failure diagnostic missing")
			}
			if _, statErr := os.Stat(filepath.Join(root, "removed")); (statErr == nil) == kept {
				t.Errorf("cleanup marker error = %v, kept = %t (process: %v; %s)", statErr, kept, err, &other)
			}
			if _, statErr := os.Stat(filepath.Join(root, "resource")); (statErr == nil) != kept {
				t.Errorf("resource error = %v, kept = %t", statErr, kept)
			}
			claims, globErr := filepath.Glob(filepath.Join(root, "state", "crabbox", "claims", "*.json"))
			wantClaims := 0
			if kept {
				wantClaims = 1
			}
			if globErr != nil || len(claims) != wantClaims {
				t.Errorf("claims remain: %v: %v", claims, globErr)
			}
			var exit *exec.ExitError
			code := 0
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.wantExit {
				t.Errorf("exit = %v, want %d; %s", err, tc.wantExit, &other)
			}
			if ctx.Err() != nil {
				t.Errorf("workload did not stop promptly: %v", ctx.Err())
			}
			data, readErr := os.ReadFile(filepath.Join(root, "workload-pid"))
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if readErr != nil || parseErr != nil {
				t.Fatalf("workload PID: %v", errors.Join(readErr, parseErr))
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("workload %d still alive: %v", pid, err)
			} else {
				workloadGone = true
			}
		})
	}
}

// Even commands that ignore formatting errors must report a broken pipe. Closing
// before the first write also exercises the synchronous exit-status latch.
func TestBrokenPipeBinaryImmediateExit(t *testing.T) {
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			reader.Close()
			defer writer.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var other bytes.Buffer
			args := []string{"--help"}
			if stream == "stderr" {
				args = []string{"not-a-command"}
			}
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Stdout, cmd.Stderr = writer, &other
			if stream == "stderr" {
				cmd.Stdout, cmd.Stderr = &other, writer
			}
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 141 {
				t.Fatalf("exit = %v, want 141; %s", err, &other)
			}
		})
	}
}
