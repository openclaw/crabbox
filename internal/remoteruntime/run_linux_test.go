package remoteruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

func TestNativeCommand(t *testing.T) {
	for _, tc := range []struct {
		name, command             string
		input, output, diagnostic []byte
		code                      int
		preflight                 bool
		state                     string
		deadline                  time.Duration
	}{
		{name: "binary streams", command: "cat; printf 'diagnostic\\n' >&2\n", input: []byte{0, 1, 255, '\r', '\n'}, output: []byte{0, 1, 255, '\r', '\n'}, diagnostic: []byte("diagnostic\n")},
		{name: "nonzero", command: "printf output; exit 7\n", output: []byte("output"), code: 7},
		{name: "preflight ready", command: "exit 0\n", preflight: true, state: "ready"},
		{name: "preflight unavailable", command: "exit 21\n", code: 21, preflight: true, state: "venv-unavailable"},
		{name: "preflight deadline", command: "exec sleep 10\n", code: failureCode, preflight: true, state: "timed-out", deadline: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nonce := nativeNonce(t)
			stagePath := "/tmp/crabbox-command-" + nonce
			command := nativeCommand(t, nonce, tc.command, tc.input, tc.deadline, tc.preflight, false)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code || !bytes.Equal(stdout.Bytes(), tc.output) || !bytes.Equal(stderr.Bytes(), tc.diagnostic) {
				t.Fatalf("exit=%d want=%d stdout=%q stderr=%q", code, tc.code, stdout.Bytes(), stderr.Bytes())
			}
			if tc.preflight {
				want := fmt.Sprintf("CBX-PREFLIGHT-1\n%s\n%s\nworker-quiesced\nscratch-removed\ncomplete\n", nonce, tc.state)
				data := nativeControl(t, nonce, "observe")
				if string(data) != want {
					t.Fatalf("completion %q", data)
				}
				if _, err := os.Lstat(filepath.Join(stagePath, "scratch")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("scratch retained: %v", err)
				}
				if output := nativeControl(t, nonce, "cleanup"); len(output) != 0 {
					t.Fatalf("cleanup output %q", output)
				}
				if data := nativeControl(t, nonce, "observe"); string(data) != want {
					t.Fatalf("cleanup changed functional completion %q", data)
				}
				if output := nativeControl(t, nonce, "retire"); len(output) != 0 {
					t.Fatalf("retirement output %q", output)
				}
			}
			if _, err := os.Lstat(stagePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage retained %s: %v", stagePath, err)
			}
			if output := nativeControl(t, nonce, "cleanup"); len(output) != 0 {
				t.Fatalf("terminal cleanup output %q", output)
			}
		})
	}
}

func TestNativeCommandEnvironment(t *testing.T) {
	nonce := nativeNonce(t)
	dir := t.TempDir()
	command := nativeCommand(t, nonce, "printf '%s\\n' \"$PWD\" \"$RUNTIME_TEST_VALUE\"; umask\n", nil, 0, false, false)
	command.Dir = dir
	command.Env = append(command.Environ(), "RUNTIME_TEST_VALUE=literal $ value")
	// The child is the test executable with the internal entry point, not Bash.
	// Use sh only to establish an ordinary inherited umask before exec.
	args := append([]string{"-c", "umask 027; exec \"$@\"", "sh", command.Path}, command.Args[1:]...)
	command.Path, command.Args = "/bin/sh", append([]string{"sh"}, args...)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != dir+"\nliteral $ value\n0027\n" {
		t.Fatalf("environment %q: %v", output, err)
	}
}

func TestNativeCommandShortFrame(t *testing.T) {
	nonce := nativeNonce(t)
	command := nativeCommand(t, nonce, "exit 0\n", nil, 0, false, false)
	command.Stdin = strings.NewReader("exit")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != failureCode || !strings.Contains(string(output), "EOF") {
		t.Fatalf("short frame: %q, %v", output, err)
	}
	if _, err := os.Lstat("/tmp/crabbox-command-" + nonce); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial stage retained: %v", err)
	}
}

func TestNativeCommandCancellation(t *testing.T) {
	for _, method := range []string{"disconnect", "signal", "hangup", "control"} {
		t.Run(method, func(t *testing.T) {
			nonce := nativeNonce(t)
			source := "printf ready; exec sleep 10\n"
			command := nativeCommand(t, nonce, source, nil, 0, true, method == "disconnect")
			command.Stdin = nil
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			command.Stderr = &diagnostic
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Write([]byte(source)); err != nil {
				t.Fatal(err)
			}
			var ready [5]byte
			if _, err := io.ReadFull(output, ready[:]); err != nil || string(ready[:]) != "ready" {
				t.Fatalf("worker readiness %q: %v", ready, err)
			}
			switch method {
			case "disconnect":
				err = input.Close()
			case "signal":
				err = command.Process.Signal(syscall.SIGTERM)
			case "hangup":
				err = command.Process.Signal(syscall.SIGHUP)
			case "control":
				data := nativeControl(t, nonce, "cancel")
				completion, parseErr := ParseCompletion(data, nonce)
				if parseErr != nil || completion.State != "canceled" {
					t.Fatalf("cancel acknowledgement %q: %v", data, parseErr)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			waitErr := command.Wait()
			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) || exit.ExitCode() != failureCode || diagnostic.Len() != 0 {
				t.Fatalf("cancel: %v, %s", waitErr, diagnostic.String())
			}
			path := "/tmp/crabbox-command-" + nonce
			completion := nativeControl(t, nonce, "observe")
			want := fmt.Sprintf("CBX-PREFLIGHT-1\n%s\ncanceled\nworker-quiesced\nscratch-removed\ncomplete\n", nonce)
			if string(completion) != want {
				t.Fatalf("completion %q", completion)
			}
			if _, err := os.Lstat(filepath.Join(path, "scratch")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch retained: %v", err)
			}
			if output := nativeControl(t, nonce, "retire"); len(output) != 0 {
				t.Fatalf("retirement output %q", output)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage retained: %v", err)
			}
		})
	}
}

func TestNativeCommandInputIdle(t *testing.T) {
	for _, progressing := range []bool{false, true} {
		t.Run(fmt.Sprintf("progress=%t", progressing), func(t *testing.T) {
			nonce := nativeNonce(t)
			source := "printf complete\n"
			command := nativeCommand(t, nonce, source, nil, 0, false, false)
			command.Stdin = nil
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			var output, diagnostic bytes.Buffer
			command.Stdout, command.Stderr = &output, &diagnostic
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			if progressing {
				// Total transfer exceeds the one-second idle allowance, but each
				// chunk arrives within it. This is not a total-transfer timeout.
				for start := 0; start < len(source); start += 3 {
					if start > 0 {
						time.Sleep(300 * time.Millisecond)
					}
					if _, err := io.WriteString(input, source[start:min(start+3, len(source))]); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if _, err := io.WriteString(input, source[:3]); err != nil {
					t.Fatal(err)
				}
			}
			waitErr := command.Wait()
			if progressing {
				if waitErr != nil || output.String() != "complete" || diagnostic.Len() != 0 {
					t.Fatalf("progress transfer: %v, %q, %q", waitErr, output.String(), diagnostic.String())
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(waitErr, &exit) || exit.ExitCode() != failureCode || output.Len() != 0 || !strings.Contains(diagnostic.String(), "transfer idle timeout") {
					t.Fatalf("idle transfer: %v, %q, %q", waitErr, output.String(), diagnostic.String())
				}
			}
			if _, err := os.Lstat(stagePath(nonce)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage retained: %v", err)
			}
		})
	}
}

func TestNativeCommandCancelDuringInput(t *testing.T) {
	nonce := nativeNonce(t)
	command := nativeCommand(t, nonce, "printf must-not-start\n", nil, 0, true, false)
	command.Stdin = nil
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(input, "pri"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(stagePath(nonce), "command")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stage never began receiving input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	data := nativeControl(t, nonce, "cancel")
	completion, err := ParseCompletion(data, nonce)
	if err != nil || completion.State != "canceled" {
		t.Fatalf("cancel acknowledgement %q: %v", data, err)
	}
	waitErr := command.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != failureCode || output.Len() != 0 || diagnostic.Len() != 0 {
		t.Fatalf("input cancellation: %v, %q, %q", waitErr, output.String(), diagnostic.String())
	}
	nativeControl(t, nonce, "retire")
	if _, err := os.Lstat(stagePath(nonce)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage retained: %v", err)
	}
}

func nativeControl(t *testing.T, nonce, action string) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, Command, "control", Protocol, nonce, action)
	if action == "cleanup" {
		cmd.Args = append(cmd.Args, strconv.FormatInt(MaxCleanupWait.Milliseconds(), 10))
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	output, err := cmd.Output()
	if err != nil || diagnostic.Len() != 0 {
		t.Fatalf("%s control: %v, %s", action, err, diagnostic.String())
	}
	return output
}

func nativeNonce(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw[:])
}

func nativeCommand(t *testing.T, nonce, source string, input []byte, execution time.Duration, preflight, watched bool) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode, inputMode := "command", "finite"
	if preflight {
		mode = "preflight"
	}
	if watched {
		inputMode = "watched"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, Command, "run", Protocol, nonce, strconv.Itoa(len(source)), strconv.Itoa(len(input)), "1000", "100", strconv.FormatInt(execution.Milliseconds(), 10), mode, inputMode)
	cmd.Stdin = bytes.NewReader(append([]byte(source), input...))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	return cmd
}
