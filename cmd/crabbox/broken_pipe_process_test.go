//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBrokenPipeUnrelatedPipeDuringRun(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_PIPE_RUN") == "1" {
		root := os.Getenv("PIPE_FIXTURE")
		done := make(chan struct{})
		go func() {
			defer close(done)
			marker := "workload-started"
			if os.Getenv("PIPE_MODE") != "nonstdio" {
				marker = "consumer-closed"
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
					result := injectBrokenPipeDuringRun()
					_ = os.WriteFile(filepath.Join(root, "pipe-result"), []byte(result), 0o600)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		os.Args = []string{os.Args[0], "run", "--provider", "docker-sandbox", "--docker-sandbox-cli", filepath.Join(root, "sbx"), "--", "echo", "fixture"}
		code := run()
		<-done
		// A late exit diagnostic must not replace the exit status with a signal.
		_, _ = os.Stderr.Write([]byte("late-exit-diagnostic\n"))
		os.Exit(code)
	}

	cmd, root := brokenPipeRunFixture(t, "nonstdio", "stdout")
	output, err := cmd.CombinedOutput()
	result, readErr := os.ReadFile(filepath.Join(root, "pipe-result"))
	if readErr != nil || string(result) != "EPIPE on non-stdio pipe" {
		t.Fatalf("non-stdio failure was not exercised: %s: %v; %s", result, readErr, output)
	}
	if err != nil || !strings.Contains(string(output), "healthy-workload-finished") {
		t.Errorf("healthy run canceled by unrelated EPIPE: %v\n%s", err, output)
	}
	assertBrokenPipeRunCleanup(t, root)
}

func injectBrokenPipeDuringRun() string {
	mode := os.Getenv("PIPE_MODE")
	if mode == "nonstdio" {
		reader, writer, err := os.Pipe()
		if err == nil {
			reader.Close()
			_, err = writer.Write([]byte("child stdin"))
			writer.Close()
		}
		if errors.Is(err, syscall.EPIPE) {
			return "EPIPE on non-stdio pipe"
		}
		return "did not get EPIPE"
	}
	output := os.Stdout
	if os.Getenv("PIPE_STREAM") == "stderr" {
		output = os.Stderr
	}
	if mode == "direct" {
		if _, err := output.Write([]byte("direct output")); errors.Is(err, syscall.EPIPE) {
			return "EPIPE on direct stdio"
		}
		return "direct output did not fail"
	}
	// Inherit the actual file descriptor, bypassing the application writer.
	child := exec.Command("sh", "-c", "printf inherited-output")
	child.Stdout, child.Stderr = output, os.Stderr
	if err := child.Run(); err != nil {
		return "inherited child failed"
	}
	return "inherited child did not fail"
}

func TestBrokenPipeBypassedWriterDuringRun(t *testing.T) {
	for _, mode := range []string{"direct", "inherited"} {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(mode+"/"+stream, func(t *testing.T) {
				cmd, root := brokenPipeRunFixture(t, mode, stream)
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				defer writer.Close()
				var other bytes.Buffer
				cmd.Stdout, cmd.Stderr = writer, &other
				if stream == "stderr" {
					cmd.Stdout, cmd.Stderr = &other, writer
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				writer.Close()
				ready := false
				scanner := bufio.NewScanner(reader)
				for scanner.Scan() {
					if scanner.Text() == "workload-ready" {
						ready = true
						break
					}
				}
				reader.Close()
				if err := os.WriteFile(filepath.Join(root, "consumer-closed"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				err = cmd.Wait()
				var exit *exec.ExitError
				if !ready || !errors.As(err, &exit) || exit.ExitCode() != 141 {
					t.Fatalf("run ready=%t exit=%v; %s", ready, err, &other)
				}
				want := "EPIPE on direct stdio"
				if mode == "inherited" {
					want = "inherited child failed"
				}
				result, readErr := os.ReadFile(filepath.Join(root, "pipe-result"))
				if readErr != nil || string(result) != want {
					t.Errorf("bypassed writer was not exercised: %s: %v", result, readErr)
				}
				assertBrokenPipeRunCleanup(t, root)
			})
		}
	}
}

func brokenPipeRunFixture(t *testing.T, mode, stream string) (*exec.Cmd, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	script := `#!/bin/sh
set -eu
case "$1" in
create) touch "$PIPE_FIXTURE/resource" ;;
exec)
  touch "$PIPE_FIXTURE/workload-started"
  if [ "$PIPE_STREAM" = stderr ]; then exec 1>&2; fi
  printf 'workload-ready\n'
  while [ ! -f "$PIPE_FIXTURE/pipe-result" ]; do sleep 0.01; done
  sleep 0.2
  printf 'healthy-workload-finished\n'
  ;;
rm) rm "$PIPE_FIXTURE/resource"; touch "$PIPE_FIXTURE/removed" ;;
*) exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "sbx"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrokenPipeUnrelatedPipeDuringRun$")
	cmd.WaitDelay = time.Second
	cmd.Dir = repo
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_STATE_HOME=" + filepath.Join(root, "state"), "CRABBOX_CONFIG=" + filepath.Join(root, "missing.yaml"), "PIPE_FIXTURE=" + root, "PIPE_MODE=" + mode, "PIPE_STREAM=" + stream, "CRABBOX_TEST_PIPE_RUN=1"}
	return cmd, root
}

func assertBrokenPipeRunCleanup(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "removed")); err != nil {
		t.Errorf("one-shot resource was not cleaned up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "resource")); !os.IsNotExist(err) {
		t.Errorf("one-shot resource still present: %v", err)
	}
	claims, err := filepath.Glob(filepath.Join(root, "state", "crabbox", "claims", "*.json"))
	if err != nil || len(claims) != 0 {
		t.Errorf("one-shot claims remain: %v: %v", claims, err)
	}
}
