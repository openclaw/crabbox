package tart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestInjectSSHKeyWaitsForGuestAgent(t *testing.T) {
	probe := commandKey([]string{"exec", "vm", "/usr/bin/true"})
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}, errors: map[string]error{}}
	attempts := 0
	runner.onRun = func(req core.LocalCommandRequest) {
		if commandKey(req.Args) != probe {
			return
		}
		attempts++
		if attempts == 1 {
			runner.responses[probe] = core.LocalCommandResult{Stderr: "GRPCConnectionPoolError: is the Tart Guest Agent running?"}
			runner.errors[probe] = fmt.Errorf("native exec failed")
		} else {
			delete(runner.responses, probe)
			delete(runner.errors, probe)
		}
	}
	b := newBackend((Provider{}).Spec(), core.BaseConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	if err := b.injectSSHKey(context.Background(), "vm", "admin", "ssh-ed25519 AAAA test"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(runner.calls) != 3 {
		t.Fatalf("probes=%d calls=%d", attempts, len(runner.calls))
	}
	last := runner.calls[2].Args
	if len(last) != 5 || last[2] != "bash" || !strings.Contains(last[4], "authorized_keys") {
		t.Fatalf("key write did not follow readiness: %v", last)
	}
}

func TestInjectSSHKeyDoesNotRetryMutation(t *testing.T) {
	runner := &recordingRunner{errors: map[string]error{}, responses: map[string]core.LocalCommandResult{}}
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 5 && req.Args[2] == "bash" {
			key := commandKey(req.Args)
			runner.errors[key] = fmt.Errorf("GRPCConnectionPoolError")
		}
	}
	b := newBackend((Provider{}).Spec(), core.BaseConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	if err := b.injectSSHKey(context.Background(), "vm", "admin", "ssh-ed25519 AAAA test"); err == nil {
		t.Fatal("mutation failure was swallowed")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("key write was retried: %d calls", len(runner.calls))
	}
}

func TestGuestAgentWaitStopsOnCancellationAndPermanentError(t *testing.T) {
	for _, mode := range []string{"deadline", "canceled", "permanent"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			detail := "GRPCConnectionPoolError"
			if mode == "canceled" {
				cancel()
			}
			if mode == "permanent" {
				detail = "permission denied"
			}
			runner := &recordingRunner{errors: map[string]error{"exec": fmt.Errorf("exec failed")}, responses: map[string]core.LocalCommandResult{"exec": {Stderr: detail}}}
			b := newBackend((Provider{}).Spec(), core.BaseConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
			err := b.injectSSHKey(ctx, "vm", "admin", "ssh-ed25519 AAAA test")
			if err == nil {
				t.Fatal("unavailable agent was accepted")
			}
			if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline lost: %v", err)
			}
			if mode == "canceled" && (!errors.Is(err, context.Canceled) || len(runner.calls) != 0) {
				t.Fatalf("canceled: err=%v calls=%d", err, len(runner.calls))
			}
			if mode == "permanent" && len(runner.calls) != 1 {
				t.Fatalf("permanent error retried: %d calls", len(runner.calls))
			}
			for _, call := range runner.calls {
				if len(call.Args) != 3 || call.Args[2] != "/usr/bin/true" {
					t.Fatalf("injected key before readiness: %v", call.Args)
				}
			}
		})
	}
}
