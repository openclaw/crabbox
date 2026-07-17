//go:build !windows

package lume

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStartVMReapsOwnerWhenPersistenceCallbackFails(t *testing.T) {
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
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-owner-callback-failure", bootstrapTrust{}, func(lumeRunOwner) error {
		return errors.New("claim write failed")
	})
	if err == nil || !strings.Contains(err.Error(), "persist owner identity") {
		t.Fatalf("startVM error=%v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for ownerProcessMatches(owner) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ownerProcessMatches(owner) {
		t.Fatalf("owner pid %d was not reaped after callback failure", owner.PID)
	}
}

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
	callbackOwner := lumeRunOwner{}
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-test-owner", bootstrapTrust{}, func(started lumeRunOwner) error {
		callbackOwner = started
		if !ownerProcessMatches(started) {
			t.Fatalf("owner was not live when persistence callback ran: %#v", started)
		}
		return nil
	})
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
	if callbackOwner.PID != owner.PID || callbackOwner.StartIdentity != owner.StartIdentity {
		t.Fatalf("callback owner=%#v final owner=%#v", callbackOwner, owner)
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
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-owner-fence", bootstrapTrust{})
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
	owner, err := b.startVM(context.Background(), b.configForRun(), "crabbox-stop-owner", bootstrapTrust{})
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
