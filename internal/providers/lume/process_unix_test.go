//go:build !windows

package lume

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStartVMDetachesOwnerAndKeepsPrivateLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	helper := filepath.Join(t.TempDir(), "fake-lume")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntrap 'exit 0' INT TERM HUP\nwhile :; do sleep 0.1 & wait $!; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.CLIPath = helper
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)
	b.startupObserveTimeout = 25 * time.Millisecond
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-test-owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process, findErr := os.FindProcess(owner.PID); findErr == nil {
			_ = process.Signal(os.Interrupt)
		}
	})
	if owner.PID <= 0 || owner.StartedAt.IsZero() || owner.StartIdentity == "" {
		t.Fatalf("owner=%#v", owner)
	}
	info, err := os.Stat(owner.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode=%#o want 0600", info.Mode().Perm())
	}
	if err := syscall.Kill(owner.PID, 0); err != nil {
		t.Fatalf("detached owner is not running: %v", err)
	}
	process, err := os.FindProcess(owner.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedGuestIsNotSafeWhileRecordedOwnerLives(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	helper := filepath.Join(t.TempDir(), "fake-lume")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntrap 'exit 0' INT TERM HUP\nwhile :; do sleep 0.1 & wait $!; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.CLIPath = helper
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"crabbox-owner-fence","status":"stopped"}]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	b.startupObserveTimeout = 25 * time.Millisecond
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-owner-fence")
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(owner.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Signal(os.Interrupt) })
	stopped, state, err := b.observeStoppedOrMissingVM(context.Background(), b.configForRun(), "crabbox-owner-fence", owner)
	if err != nil {
		t.Fatal(err)
	}
	if stopped || !strings.Contains(state, "owner running") {
		t.Fatalf("stopped=%v state=%q", stopped, state)
	}
}

func TestStopVMInterruptsTheIdentityFencedRunOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	helper := filepath.Join(t.TempDir(), "fake-lume")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntrap 'exit 0' INT TERM HUP\nwhile :; do sleep 0.1 & wait $!; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.CLIPath = helper
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"crabbox-stop-owner","status":"stopped"}]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	b.startupObserveTimeout = 25 * time.Millisecond
	b.stopObserveTimeout = 3 * time.Second
	b.stopPollInterval = 10 * time.Millisecond
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-stop-owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process, findErr := os.FindProcess(owner.PID); findErr == nil {
			_ = process.Signal(os.Interrupt)
		}
	})
	if err := b.stopVM(context.Background(), b.configForRun(), "crabbox-stop-owner", owner); err != nil {
		t.Fatal(err)
	}
	if ownerProcessMatches(owner) {
		t.Fatalf("identity-fenced owner pid %d survived stop", owner.PID)
	}
}

func TestOwnerSafeToSignalRejectsMismatchedOrUnverifiableIdentity(t *testing.T) {
	started, err := core.LocalProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if ownerSafeToSignal(lumeRunOwner{PID: os.Getpid(), StartIdentity: started + "-mismatch"}) {
		t.Fatal("mismatched process identity was eligible for signaling")
	}
	if ownerSafeToSignal(lumeRunOwner{PID: 2147483647, StartIdentity: "unverifiable"}) {
		t.Fatal("unverifiable process identity was eligible for signaling")
	}
}
