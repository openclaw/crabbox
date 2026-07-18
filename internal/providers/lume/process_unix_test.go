//go:build !windows

package lume

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func fakeLumeOwner(t *testing.T) string {
	t.Helper()
	path := join(t.TempDir(), "fake-lume")
	mustNoError(t, os.WriteFile(path, []byte("#!/bin/sh\ntrap 'exit 0' INT TERM HUP\nwhile :; do sleep 0.1 & wait $!; done\n"), 0o700))
	return path
}

func newOwnerBackend(t *testing.T, runner *fakeRunner) *backend {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if runner == nil {
		runner = &fakeRunner{}
	}
	cfg := base()
	cfg.Provider, cfg.Lume.CLIPath = providerName, fakeLumeOwner(t)
	return newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
}

func TestStartVMReapsOwnerWhenPersistenceCallbackFails(t *testing.T) {
	b := newOwnerBackend(t, nil)
	owner, err := b.startVM(bg, b.configForRun(), "crabbox-owner-callback-failure", bootstrapTrust{}, "", func(lumeRunOwner) error {
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
	b := newOwnerBackend(t, nil)
	b.startupObserveTimeout = 25 * time.Millisecond
	callbackOwner := lumeRunOwner{}
	owner, err := b.startVM(bg, b.configForRun(), "crabbox-test-owner", bootstrapTrust{}, "", func(started lumeRunOwner) error {
		callbackOwner = started
		if !ownerProcessMatches(started) {
			t.Fatalf("owner was not live when persistence callback ran: %#v", started)
		}
		return nil
	})
	mustNoError(t, err)
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
	mustNoError(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode=%#o want 0600", info.Mode().Perm())
	}
	if err := syscall.Kill(owner.PID, 0); err != nil {
		t.Fatalf("detached owner is not running: %v", err)
	}
	process, err := os.FindProcess(owner.PID)
	mustNoError(t, err)
	mustNoError(t, process.Signal(os.Interrupt))
}

func TestRecoverPendingLaunchOwnerMatchesHandoffMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	token, err := newLaunchToken()
	mustNoError(t, err)
	handoff, err := prepareLaunchHandoff(token)
	mustNoError(t, err)
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 0.1; done", "crabbox-lume-launch-"+token)
	mustNoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(handoff.Dir)
	})
	mustNoError(t, os.WriteFile(handoff.OwnerPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600))
	claim := claim{LeaseID: "cbx_pending_live", Labels: labels{
		"run_owner_expected": "true",
		"run_owner_pending":  "true",
		"run_launch_token":   token,
	}}
	owner, err := recoverPendingLaunchOwner(claim)
	mustNoError(t, err)
	if owner.PID != cmd.Process.Pid || owner.StartIdentity == "" {
		t.Fatalf("owner=%#v pid=%d", owner, cmd.Process.Pid)
	}
}

func TestStopVMInterruptsTheIdentityFencedRunOwner(t *testing.T) {
	runner := &fakeRunner{responses: results{
		"get": {Stdout: `[{"name":"crabbox-stop-owner","status":"stopped"}]`},
	}}
	b := newOwnerBackend(t, runner)
	b.startupObserveTimeout = 25 * time.Millisecond
	b.stopObserveTimeout = 3 * time.Second
	b.stopPollInterval = 10 * time.Millisecond
	owner, err := b.startVM(bg, b.configForRun(), "crabbox-stop-owner", bootstrapTrust{}, "")
	mustNoError(t, err)
	t.Cleanup(func() {
		if process, findErr := os.FindProcess(owner.PID); findErr == nil {
			_ = process.Signal(os.Interrupt)
		}
	})
	mustNoError(t, b.stopVM(bg, b.configForRun(), "crabbox-stop-owner", owner))
	if ownerProcessMatches(owner) {
		t.Fatalf("identity-fenced owner pid %d survived stop", owner.PID)
	}
}

func TestOwnerSafeToSignalRejectsMismatchedOrUnverifiableIdentity(t *testing.T) {
	started, err := core.LocalProcessStartIdentity(os.Getpid())
	mustNoError(t, err)
	if ownerSafeToSignal(lumeRunOwner{PID: os.Getpid(), StartIdentity: started + "-mismatch"}) {
		t.Fatal("mismatched process identity was eligible for signaling")
	}
	if ownerSafeToSignal(lumeRunOwner{PID: 2147483647, StartIdentity: "unverifiable"}) {
		t.Fatal("unverifiable process identity was eligible for signaling")
	}
}
