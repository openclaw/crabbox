package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestParallelsWindowsUploadRecoversAppliedChunk(t *testing.T) {
	for _, message := range []string{"PrlJob_GetResult: Invalid argument", "PrlJob_GetRetCode: Invalid argument"} {
		t.Run(message, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				script := strings.Repeat("Write-Output 'unicode π'\n", 40)
				runner := &windowsUploadRunner{t: t, data: []byte(strings.Repeat("Z", 4000)), failure: message, failWrite: 2}
				cfg := BaseConfig()
				cfg.TargetOS = targetWindows
				cfg.Parallels.StartupTimeout = time.Minute
				err := NewParallelsClient(cfg, runner).runWindowsPowerShellFile(context.Background(), "vm-fixture", "fixture", script)
				if err != nil {
					t.Fatal(err)
				}
				if runner.executions != 1 || runner.executed != script {
					t.Fatalf("executions=%d payload intact=%t", runner.executions, runner.executed == script)
				}
				if runner.failures != 1 || runner.probes == 0 {
					t.Fatalf("failures=%d readiness probes=%d", runner.failures, runner.probes)
				}
			})
		})
	}
}

func TestParallelsWindowsUploadDoesNotRetryTerminalFailure(t *testing.T) {
	runner := &windowsUploadRunner{t: t, failure: "Access is denied", failWrite: 1}
	cfg := BaseConfig()
	cfg.TargetOS = targetWindows
	err := NewParallelsClient(cfg, runner).runWindowsPowerShellFile(context.Background(), "vm-fixture", "fixture", "Write-Output 'fixture'")
	if err == nil || !strings.Contains(err.Error(), runner.failure) || runner.executions != 0 || runner.failures != 1 {
		t.Fatalf("err=%v executions=%d failures=%d", err, runner.executions, runner.failures)
	}
}

func TestParallelsWindowsUploadNeverReplaysExecution(t *testing.T) {
	runner := &windowsUploadRunner{t: t, executionFailure: true}
	cfg := BaseConfig()
	cfg.TargetOS = targetWindows
	err := NewParallelsClient(cfg, runner).runWindowsPowerShellFile(context.Background(), "vm-fixture", "fixture", "Write-Output 'fixture'")
	if err == nil || runner.executions != 1 {
		t.Fatalf("err=%v executions=%d", err, runner.executions)
	}
}

func TestParallelsWindowsUploadRetryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &windowsUploadRunner{t: t, failure: "PrlJob_GetResult: Invalid argument", failWrite: 1, alwaysFail: true}
		cfg := BaseConfig()
		cfg.TargetOS = targetWindows
		cfg.Parallels.StartupTimeout = 7 * time.Second
		start := time.Now()
		err := NewParallelsClient(cfg, runner).runWindowsPowerShellFile(context.Background(), "vm-fixture", "fixture", "Write-Output 'fixture'")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 7*time.Second || runner.executions != 0 {
			t.Fatalf("err=%v elapsed=%s executions=%d", err, time.Since(start), runner.executions)
		}
	})
}

// Models the guest file boundary, including a write that succeeds before Tools loses its result.
type windowsUploadRunner struct {
	t                *testing.T
	data             []byte
	executed         string
	executions       int
	probes           int
	failures         int
	failWrite        int
	writes           int
	failure          string
	alwaysFail       bool
	executionFailure bool
}

var windowsUploadOffset = regexp.MustCompile(`\$file\.Position = (\d+)`)
var windowsUploadBytes = regexp.MustCompile(`GetBytes\('([^']*)'\)`)
var windowsUploadAppend = regexp.MustCompile(`-Value '([^']*)'`)
var windowsUploadLength = regexp.MustCompile(`Substring\(0, (\d+)\)`)

func (r *windowsUploadRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.t.Helper()
	if req.Name != "prlctl" || len(req.Args) < 3 || req.Args[0] != "exec" {
		r.t.Fatalf("unexpected command: %s %v", req.Name, req.Args)
	}
	command := decodePowerShellCommand(r.t, strings.Join(req.Args[2:], " "))
	if strings.Contains(command, `"ok" | Out-Null`) {
		r.probes++
		return LocalCommandResult{}, nil
	}
	if strings.Contains(command, "New-Item") {
		if strings.Contains(command, "Remove-Item") {
			r.data = nil
		}
		return LocalCommandResult{}, nil
	}
	offset := -1
	var bytes []byte
	if match := windowsUploadOffset.FindStringSubmatch(command); match != nil {
		offset, _ = strconv.Atoi(match[1])
		payload := windowsUploadBytes.FindStringSubmatch(command)
		if payload == nil {
			r.t.Fatal("offset write has no payload")
		}
		bytes = []byte(payload[1])
	} else if strings.Contains(command, "Add-Content") {
		offset = len(r.data)
		payload := windowsUploadAppend.FindStringSubmatch(command)
		if payload == nil {
			r.t.Fatal("append has no payload")
		}
		bytes = []byte(payload[1] + "\r\n")
	}
	if offset >= 0 {
		for len(r.data) < offset+len(bytes) {
			r.data = append(r.data, 0)
		}
		copy(r.data[offset:], bytes)
		r.writes++
		if r.failure != "" && (r.writes == r.failWrite || r.alwaysFail) && (r.failures == 0 || r.alwaysFail) {
			r.failures++
			return LocalCommandResult{Stderr: r.failure, ExitCode: 255}, errors.New("exit status 255")
		}
		return LocalCommandResult{}, nil
	}
	if strings.Contains(command, "[Convert]::FromBase64String") {
		raw := string(r.data)
		if match := windowsUploadLength.FindStringSubmatch(command); match != nil {
			length, _ := strconv.Atoi(match[1])
			if len(raw) < length {
				return LocalCommandResult{}, fmt.Errorf("short upload")
			}
			raw = raw[:length]
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(raw), ""))
		if err != nil {
			return LocalCommandResult{}, err
		}
		r.executed = string(decoded)
	}
	if strings.Contains(command, "& powershell.exe") {
		r.executions++
		if r.executionFailure {
			return LocalCommandResult{Stderr: "PrlJob_GetResult: Invalid argument", ExitCode: 255}, errors.New("exit status 255")
		}
	}
	return LocalCommandResult{}, nil
}
