//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecCommandRunnerFileCapture(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	for _, tc := range []struct {
		mode      string
		code      int
		stdout    string
		stderr    string
		cancel    bool
		waitDelay bool
		overflow  bool
	}{
		{mode: "normal", stdout: "out", stderr: "err"},
		{mode: "nonzero", code: 7, stdout: "out", stderr: "err"},
		{mode: "early-close", cancel: true},
		{mode: "cancel", cancel: true},
		{mode: "stdout-overflow", code: 5, stdout: strings.Repeat("x", 1024), overflow: true},
		{mode: "stderr-overflow", code: 5, stderr: strings.Repeat("x", 1024), overflow: true},
		{mode: "overflow-exit", code: 5, stdout: strings.Repeat("x", 1024), overflow: true},
		{mode: "inherited-writer", waitDelay: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type outcome struct {
				result LocalCommandResult
				err    error
			}
			done := make(chan outcome, 1)
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-done
				}
			}()
			go func() {
				result, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
					Name: os.Args[0], Args: captureHelperArgs(tc.mode, dir),
					MaxCapturedOutputBytes: 1024, CaptureOutputToFiles: true,
				})
				done <- outcome{result, err}
			}()
			if tc.cancel {
				awaitCaptureMarker(t, filepath.Join(dir, "ready"))
				cancel()
			}
			got := <-done
			joined = true
			if !tc.cancel && ctx.Err() != nil {
				t.Fatal("command needed the test deadline to stop")
			}
			if tc.cancel {
				if got.err == nil || got.result.ExitCode == 0 {
					t.Fatalf("canceled child succeeded: %+v", got.result)
				}
			} else if tc.waitDelay {
				if !errors.Is(got.err, exec.ErrWaitDelay) || got.result.ExitCode == 0 {
					t.Fatalf("retained writer result=%+v err=%v", got.result, got.err)
				}
			} else if got.result.ExitCode != tc.code || (got.err != nil) != (tc.code != 0) {
				t.Fatalf("result=%+v err=%v want exit %d", got.result, got.err, tc.code)
			}
			if tc.overflow && !strings.Contains(got.err.Error(), "exceeded 1024-byte limit") {
				t.Fatalf("overflow error=%v", got.err)
			}
			if got.result.Stdout != tc.stdout || got.result.Stderr != tc.stderr {
				t.Fatalf("capture stdout bytes=%d stderr bytes=%d", len(got.result.Stdout), len(got.result.Stderr))
			}
			if tc.waitDelay {
				data, err := os.ReadFile(filepath.Join(dir, "leaf-pgid"))
				if err != nil {
					t.Fatal(err)
				}
				group, err := strconv.Atoi(string(data))
				if err != nil || group <= 0 || group == syscall.Getpgrp() {
					t.Fatalf("invalid helper process group %q", data)
				}
				defer terminateControllerProcessGroup(group)
				if err := waitForControllerProcessGroupExit(group, nil, controllerProcessGroupAlive, time.Now().Add(time.Second)); err != nil {
					t.Fatalf("retained writer survived the command owner: %v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "crabbox-command-output-") {
					t.Fatal("named capture survived command")
				}
			}
		})
	}
}

func awaitCaptureMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("capture helper did not become ready")
		case <-ticker.C:
		}
	}
}

func captureHelperArgs(mode, dir string) []string {
	return []string{"-test.run=^TestExecCommandRunnerFileCaptureHelper$", "--", mode, dir}
}

func TestExecCommandRunnerFileCaptureRejectsUnboundedOrStreamingRequests(t *testing.T) {
	for _, tc := range []LocalCommandRequest{
		{},
		{MaxCapturedOutputBytes: 1024, DisableOutputCapture: true},
		{MaxCapturedOutputBytes: 1024, Stdout: io.Discard},
		{MaxCapturedOutputBytes: 1024, Stderr: io.Discard},
	} {
		tc.Name = "must-not-start"
		tc.CaptureOutputToFiles = true
		if _, err := (execCommandRunner{}).Run(t.Context(), tc); err == nil || !strings.Contains(err.Error(), "requires a positive limit and no streaming writers") {
			t.Fatalf("invalid capture request err=%v", err)
		}
	}
}

func TestExecCommandRunnerFileCaptureStartFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	result, err := (execCommandRunner{}).Run(t.Context(), LocalCommandRequest{
		Name: filepath.Join(dir, "absent"), MaxCapturedOutputBytes: 1024, CaptureOutputToFiles: true,
	})
	if err == nil || result.ExitCode == 0 || result.Stdout != "" || result.Stderr != "" {
		t.Fatalf("startup result=%+v err=%v", result, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("capture files after startup failure=%v err=%v", entries, err)
	}
}

func TestExecCommandRunnerFileCaptureControllerRetainedWriter(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], captureHelperArgs("controller", dir)...)
	configureBoundedCommandCancellation(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer terminateControllerProcessGroup(cmd.Process.Pid)
	err := cmd.Wait()
	if err == nil || cmd.ProcessState.ExitCode() != 73 {
		t.Fatalf("outer owner did not receive capture failure: %v output=%q", err, output.String())
	}
	if err := terminateControllerProcessGroup(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	var result struct {
		RetainedWriter bool `json:"retainedWriter"`
		EmptyOutput    bool `json:"emptyOutput"`
		ProcessGroup   int  `json:"processGroup"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.RetainedWriter || !result.EmptyOutput || result.ProcessGroup != cmd.Process.Pid {
		t.Fatalf("inner capture changed outer ownership or hid failure: %+v", result)
	}
}

func TestExecCommandRunnerFileCaptureHelper(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	mode, dir := os.Args[index+1], os.Args[index+2]
	if mode == "controller" {
		_ = os.Setenv(controllerProcessTreeOwnedEnv, "1")
		_ = os.Setenv("TMPDIR", dir)
		result, err := (execCommandRunner{}).Run(context.Background(), LocalCommandRequest{
			Name: os.Args[0], Args: captureHelperArgs("inherited-writer", dir),
			MaxCapturedOutputBytes: 1024, CaptureOutputToFiles: true,
		})
		_, _ = fmt.Fprintf(os.Stdout, `{"retainedWriter":%t,"emptyOutput":%t,"processGroup":%d}`, errors.Is(err, exec.ErrWaitDelay), result.Stdout == "" && result.Stderr == "", syscall.Getpgrp())
		os.Exit(73)
	}
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		info, err := stream.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Sys().(*syscall.Stat_t).Nlink != 0 {
			os.Exit(96)
		}
	}
	switch mode {
	case "normal", "nonzero":
		_, _ = io.WriteString(os.Stdout, "out")
		_, _ = io.WriteString(os.Stderr, "err")
		if mode == "nonzero" {
			os.Exit(7)
		}
	case "early-close", "cancel":
		if mode == "early-close" {
			_ = os.Stdout.Close()
			_ = os.Stderr.Close()
		}
		_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600)
		time.Sleep(time.Hour)
	case "stdout-overflow", "stderr-overflow", "overflow-exit":
		stream := os.Stdout
		if mode == "stderr-overflow" {
			stream = os.Stderr
		}
		for {
			_, _ = io.WriteString(stream, strings.Repeat("x", 16<<10))
			if mode == "overflow-exit" {
				break
			}
			time.Sleep(time.Millisecond)
		}
	case "inherited-writer":
		child := exec.Command(os.Args[0], captureHelperArgs("leaf", dir)...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(97)
		}
		_, _ = io.WriteString(os.Stdout, "out")
	case "leaf":
		_ = os.WriteFile(filepath.Join(dir, "leaf-pgid"), []byte(strconv.Itoa(syscall.Getpgrp())), 0o600)
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

func TestExecCommandRunnerPipeCaptureAndStreaming(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("disabled=%t/stream=%t", disabled, stream), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				req := LocalCommandRequest{
					Name: "sh", Args: []string{"-c", "printf out; printf err >&2"},
					DisableOutputCapture: disabled, MaxCapturedOutputBytes: 1024,
				}
				if stream {
					req.Stdout, req.Stderr = &stdout, &stderr
				}
				result, err := (execCommandRunner{}).Run(t.Context(), req)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("pipe command result=%+v err=%v", result, err)
				}
				wantOut, wantErr := "out", "err"
				if disabled {
					wantOut, wantErr = "", ""
				}
				if result.Stdout != wantOut || result.Stderr != wantErr {
					t.Fatalf("captured result=%+v", result)
				}
				if !stream {
					wantOut, wantErr = "", ""
				} else {
					wantOut, wantErr = "out", "err"
				}
				if stdout.String() != wantOut || stderr.String() != wantErr {
					t.Fatalf("streamed stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}
