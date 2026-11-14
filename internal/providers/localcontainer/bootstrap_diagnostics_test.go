package localcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAcquirePreservesBootstrapDiagnosticsBeforeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name              string
		log               string
		exit              int
		logError          bool
		terminalAtInspect int
	}{
		{name: "missing sshd", log: "missing /usr/sbin/sshd; use a Debian/Ubuntu-compatible image or a prebuilt Crabbox runner image", exit: 127},
		{name: "late SSH failure", log: "sshd: no hostkeys available -- exiting.", exit: 1, terminalAtInspect: 4},
		{name: "missing host keys", log: "sshd: no hostkeys available -- exiting.", exit: 1},
		{name: "logs unavailable", exit: 1, logError: true},
		{name: "large logs", log: strings.Repeat("x", 20000) + "bootstrap-tail", exit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, runner, _, leaseID, bootstrapDir, containerPresent := pendingAcquireBackend(t)
			originalRun := runner.run
			logsRead := false
			inspectCount := 0
			if tc.terminalAtInspect > 1 {
				b.waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
					return errors.New("SSH transport closed")
				}
			}
			runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if firstArg(req.Args) == "logs" {
					if !*containerPresent {
						t.Fatal("logs read after container removal")
					}
					if strings.Join(req.Args, " ") != "logs --tail 100 container-pending" || req.MaxCapturedOutputBytes <= 0 || req.MaxCapturedOutputBytes > 16384 {
						t.Fatalf("unbounded or inexact logs request: %+v", req)
					}
					logsRead = true
					if tc.logError {
						return core.LocalCommandResult{}, errors.New("log driver unavailable")
					}
					return core.LocalCommandResult{Stderr: tc.log}, nil
				}
				if firstArg(req.Args) == "rm" && !logsRead {
					t.Error("removed failed container before reading logs")
				}
				result, err := originalRun(req)
				if err == nil && firstArg(req.Args) == "inspect" {
					inspectCount++
					if inspectCount < tc.terminalAtInspect {
						return result, nil
					}
					var containers []map[string]any
					if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
						t.Fatal(err)
					}
					containers[0]["State"] = map[string]any{"Status": "exited", "ExitCode": tc.exit}
					data, err := json.Marshal(containers)
					if err != nil {
						t.Fatal(err)
					}
					result.Stdout = string(data)
				}
				return result, err
			}
			started := time.Now()
			_, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
			if err == nil || !isExitCode(err, 5) || !strings.Contains(err.Error(), "terminal runtime state exited") {
				t.Fatalf("lost original readiness failure: %v", err)
			}
			if time.Since(started) > 5*time.Second || len(err.Error()) > 18000 || !logsRead {
				t.Fatalf("diagnostics not bounded or collected: logs=%t err=%v", logsRead, err)
			}
			if !strings.Contains(err.Error(), "process exit="+strconv.Itoa(tc.exit)) {
				t.Fatalf("missing process exit: %v", err)
			}
			if !tc.logError && !strings.Contains(err.Error(), tc.log[:min(len(tc.log), 100)]) {
				t.Fatalf("bootstrap cause lost: %v", err)
			}
			if *containerPresent {
				t.Fatal("failed container retained")
			}
			claim, err := core.ReadLeaseClaim(*leaseID)
			if err != nil || claim.LeaseID != "" {
				t.Fatalf("claim retained: %+v %v", claim, err)
			}
			key, err := core.TestboxKeyPath(*leaseID)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{key, *bootstrapDir} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("cleanup residue %s: %v", path, err)
				}
			}
		})
	}
}
