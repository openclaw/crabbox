package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func staticPowerFixture(t *testing.T) (core.Config, string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("power hooks require joined macOS/Linux process groups")
	}
	stubStaticArchitecture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := waitForSSH
	waitForSSH = func(context.Context, *core.SSHTarget, io.Writer) error { return nil }
	t.Cleanup(func() { waitForSSH = old })
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "hook")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s %%s\\n' \"$1\" \"$CRABBOX_LEASE_ID\" \"$CRABBOX_STATIC_HOST\" >> %q\nif [ -e %q ]; then echo 'hook failed' >&2; exit 9; fi\n", log, filepath.Join(dir, "fail"))
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = "ssh"
	cfg.TargetOS = core.TargetLinux
	cfg.Static.Host = "127.0.0.1"
	cfg.Static.User = "test"
	cfg.Static.Port = "2222"
	cfg.Static.Power.Dedicated = true
	cfg.Static.StartCommand = []string{script, "up"}
	cfg.Static.StopCommand = []string{script, "down"}
	return cfg, dir
}
func powerBackend(cfg core.Config) *staticLeaseBackend {
	return NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, core.RuntimeForProviderOperation(io.Discard)).(*staticLeaseBackend)
}
func powerAcquire(t *testing.T, cfg core.Config, id string) (*staticLeaseBackend, core.LeaseTarget) {
	t.Helper()
	cfg.Static.ID = id
	b := powerBackend(cfg)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return b, lease
}
func powerCalls(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' })
}
func requirePowerCalls(t *testing.T, dir string, want ...string) {
	t.Helper()
	calls := powerCalls(t, dir)
	if len(calls) != len(want) {
		t.Fatalf("calls=%q want=%q", calls, want)
	}
	for i, call := range calls {
		if !strings.HasPrefix(call, want[i]+" ") {
			t.Fatalf("calls=%q want=%q", calls, want)
		}
	}
}
func releasePower(b *staticLeaseBackend, lease core.LeaseTarget) error {
	return b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
}
func TestStaticPowerSingleLease(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	b, lease := powerAcquire(t, cfg, "static_one")
	requirePowerCalls(t, dir, "up")
	if got := powerCalls(t, dir)[0]; got != "up static_one 127.0.0.1" {
		t.Fatal(got)
	}
	if err := releasePower(b, lease); err != nil {
		t.Fatal(err)
	}
	requirePowerCalls(t, dir, "up", "down")
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if err := releasePower(b, lease); err == nil {
		t.Fatal("stale release accepted")
	}
	requirePowerCalls(t, dir, "up", "down")
}
func TestStaticPowerOverlappingLeases(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			cfg, dir := staticPowerFixture(t)
			a, first := powerAcquire(t, cfg, "static_a")
			b, second := powerAcquire(t, cfg, "static_b")
			requirePowerCalls(t, dir, "up")
			if reverse {
				a, b = b, a
				first, second = second, first
			}
			if err := releasePower(a, first); err != nil {
				t.Fatal(err)
			}
			requirePowerCalls(t, dir, "up")
			if err := releasePower(b, second); err != nil {
				t.Fatal(err)
			}
			requirePowerCalls(t, dir, "up", "down")
		})
	}
}
func TestStaticPowerConcurrentSameIDRefused(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	cfg.Static.ID = "static_same"
	req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := powerBackend(cfg).Acquire(context.Background(), req); results <- err })
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "overlapping acquisitions") {
			t.Fatal(err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successes=%d", succeeded)
	}
	requirePowerCalls(t, dir, "up")
	b := powerBackend(cfg)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: cfg.Static.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = releasePower(b, lease); err != nil {
		t.Fatal(err)
	}
	requirePowerCalls(t, dir, "up", "down")
}
func TestStaticStopFailureRetainsCustodyRegression(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	b, lease := powerAcquire(t, cfg, "static_retry")
	fail := filepath.Join(dir, "fail")
	if err := os.WriteFile(fail, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := releasePower(b, lease)
	if core.ExitCodeForError(err, 0) != 9 || !strings.Contains(err.Error(), "pending-stop") {
		t.Fatalf("err=%v", err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || !exists {
		t.Fatalf("lost custody exists=%v err=%v", exists, err)
	}
	h, unlock, err := b.lockPowerHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := h.record.References[lease.LeaseID].State
	unlock()
	if state != "pending-stop" {
		t.Fatalf("state=%s", state)
	}
	if err = os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	b = powerBackend(cfg)
	fresh, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if err = releasePower(b, fresh); err != nil {
		t.Fatal(err)
	}
	requirePowerCalls(t, dir, "up", "down", "down")
}
func TestStaticPowerAcquisitionRollback(t *testing.T) {
	for _, failStop := range []bool{false, true} {
		t.Run(fmt.Sprint(failStop), func(t *testing.T) {
			cfg, dir := staticPowerFixture(t)
			cfg.Static.ID = "static_rollback"
			waitForSSH = func(context.Context, *core.SSHTarget, io.Writer) error {
				if failStop {
					if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("readiness failed")
			}
			_, err := powerBackend(cfg).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
			if err == nil || !strings.Contains(err.Error(), "readiness failed") {
				t.Fatalf("err=%v", err)
			}
			requirePowerCalls(t, dir, "up", "down")
			_, exists, readErr := core.ReadLeaseClaimWithPresence(cfg.Static.ID)
			if readErr != nil || exists != failStop {
				t.Fatalf("exists=%v err=%v", exists, readErr)
			}
			if failStop {
				if err = os.Remove(filepath.Join(dir, "fail")); err != nil {
					t.Fatal(err)
				}
				b := powerBackend(cfg)
				lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: cfg.Static.ID})
				if err != nil {
					t.Fatal(err)
				}
				if err = releasePower(b, lease); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestStaticPowerStartFailureHasNoCustody(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	cfg.Static.ID = "static_start_failure"
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := powerBackend(cfg).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if core.ExitCodeForError(err, 0) != 9 {
		t.Fatalf("err=%v", err)
	}
	requirePowerCalls(t, dir, "up")
	if _, exists, err := core.ReadLeaseClaimWithPresence(cfg.Static.ID); err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}
func TestStaticPowerHostIdentityAndDedicatedContract(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	for _, host := range []string{"localhost", "buildbox.example.test"} {
		bad := cfg
		bad.Static.Host = host
		_, err := powerBackend(bad).Acquire(context.Background(), core.AcquireRequest{})
		if err == nil || !strings.Contains(err.Error(), "hostname aliases") {
			t.Fatalf("host=%s err=%v", host, err)
		}
	}
	bad := cfg
	bad.Static.Power.Dedicated = false
	if _, err := powerBackend(bad).Acquire(context.Background(), core.AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "dedicated: true") {
		t.Fatal(err)
	}
	b, lease := powerAcquire(t, cfg, "static_identity")
	for _, change := range []func(*core.Config){func(c *core.Config) { c.Static.User = "other" }, func(c *core.Config) { c.Static.Port = "22" }, func(c *core.Config) { c.Static.Power.HostID = "other" }, func(c *core.Config) { c.Static.StopCommand = []string{"/bin/false"} }} {
		bad := cfg
		change(&bad)
		if err := releasePower(powerBackend(bad), lease); err == nil || !strings.Contains(err.Error(), "contract changed") {
			t.Fatalf("err=%v", err)
		}
	}
	plain := cfg
	plain.Static.StartCommand = nil
	plain.Static.StopCommand = nil
	if err := releasePower(powerBackend(plain), lease); err == nil {
		t.Fatal("cleared hooks shed custody")
	}
	requirePowerCalls(t, dir, "up")
	if err := releasePower(b, lease); err != nil {
		t.Fatal(err)
	}
}
func TestStaticPowerStaleClaimAndOwnHeartbeat(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprint(own), func(t *testing.T) {
			cfg, dir := staticPowerFixture(t)
			b, lease := powerAcquire(t, cfg, "static_revision")
			toucher := b
			if !own {
				toucher = powerBackend(cfg)
			}
			updated, err := toucher.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
			if err != nil {
				t.Fatal(err)
			}
			err = releasePower(b, lease)
			if own {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("stale release accepted")
				}
				requirePowerCalls(t, dir, "up")
				lease.Server = updated
				if err = releasePower(toucher, lease); err != nil {
					t.Fatal(err)
				}
			}
			requirePowerCalls(t, dir, "up", "down")
		})
	}
}
func TestTailBufferKeepsBoundedSuffix(t *testing.T) {
	tail := &tailBuffer{limit: 8}
	tail.Write([]byte(strings.Repeat("x", 10000) + "12345678"))
	tail.Write([]byte("zz"))
	if tail.String() != "345678zz" || cap(tail.data) > 16 {
		t.Fatalf("tail=%q cap=%d", tail.String(), cap(tail.data))
	}
}

func TestStaticPowerCanceledLock(t *testing.T) {
	cfg, _ := staticPowerFixture(t)
	b := powerBackend(cfg)
	_, unlock, err := b.lockPowerHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = b.lockPowerHost(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestStaticPowerHostIDCannotAliasAnotherEndpoint(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	cfg.Static.Power.HostID = "lab"
	b, lease := powerAcquire(t, cfg, "static_hostid")
	alias := cfg
	alias.Static.ID = "static_alias"
	alias.Static.Host = "127.0.0.2"
	_, err := powerBackend(alias).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "contract changed") {
		t.Fatalf("err=%v", err)
	}
	requirePowerCalls(t, dir, "up")
	if err = releasePower(b, lease); err != nil {
		t.Fatal(err)
	}
}

func TestStaticPowerForcedRecoveryRequiresAcknowledgement(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	_, lease := powerAcquire(t, cfg, "static_recover")
	// Reproduce a reference that survived without a claim (reservation publication
	// precedes lease publication). Forced recovery must use the original token.
	core.RemoveLeaseClaim(lease.LeaseID)
	b := powerBackend(cfg)
	if err := b.ReclaimAndStop(context.Background(), core.StopRequest{ID: lease.LeaseID}); err == nil || !strings.Contains(err.Error(), "acknowledge") {
		t.Fatalf("err=%v", err)
	}
	requirePowerCalls(t, dir, "up")
	cfg.Static.PowerAcknowledgeStop = true
	b = powerBackend(cfg)
	if err := b.ReclaimAndStop(context.Background(), core.StopRequest{ID: lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	requirePowerCalls(t, dir, "up", "down")
	if err := b.ReclaimAndStop(context.Background(), core.StopRequest{ID: "unknown"}); err == nil {
		t.Fatal("forced unknown adoption")
	}
}

func TestStaticPowerCannotAdoptExistingUnpoweredLease(t *testing.T) {
	cfg, dir := staticPowerFixture(t)
	plain := cfg
	plain.Static.StartCommand = nil
	plain.Static.StopCommand = nil
	b, lease := powerAcquire(t, plain, "static_plain")
	cfg.Static.ID = "static_powered"
	_, err := powerBackend(cfg).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "untracked lease") {
		t.Fatalf("err=%v", err)
	}
	requirePowerCalls(t, dir)
	if err = releasePower(b, lease); err != nil {
		t.Fatal(err)
	}
}

func TestStaticPowerPinsTransport(t *testing.T) {
	cfg, _ := staticPowerFixture(t)
	b, lease := powerAcquire(t, cfg, "static_transport")
	assertTarget := func(target core.SSHTarget) {
		t.Helper()
		if target.FallbackPorts == nil || len(target.FallbackPorts) != 0 || target.SSHConfigFile == "" || !strings.Contains(string(target.SSHConfigData), "IdentitiesOnly yes") {
			t.Fatalf("power transport was not pinned: %#v", target)
		}
	}
	assertTarget(lease.SSH)
	fresh, err := powerBackend(cfg).Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	assertTarget(fresh.SSH)
	if err = releasePower(b, lease); err != nil {
		t.Fatal(err)
	}
}
