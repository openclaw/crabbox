package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type extendGuestTestProvider struct{ heartbeatDirectProvider }

func (extendGuestTestProvider) Spec() ProviderSpec {
	s := heartbeatDirectProvider{}.Spec()
	s.Name = "extend-guest-test"
	return s
}

var extendGuestBackendForTest *extendGuestTestBackend

func (extendGuestTestProvider) Configure(Config, Runtime) (Backend, error) {
	return extendGuestBackendForTest, nil
}
func init() { RegisterProvider(extendGuestTestProvider{}) }

type extendGuestTestBackend struct {
	heartbeatDirectBackend
	cfg    Config
	now    time.Time
	writes int
}

func (*extendGuestTestBackend) Spec() ProviderSpec { return extendGuestTestProvider{}.Spec() }
func (b *extendGuestTestBackend) ExtendLease(ctx context.Context, req LeaseExtendRequest) (LeaseExtendResult, error) {
	return ExtendDirectLease(ctx, b, b.cfg, req, b.now, func(_ context.Context, server Server, _ LeaseClaim) error {
		b.writes++
		b.lease.Server.Labels = cloneStringMap(server.Labels)
		return nil
	})
}
func extendGuestFixture(t *testing.T, provider string) *extendGuestTestBackend {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := defaultConfig()
	cfg.Provider = "extend-guest-test"
	now := time.Now().UTC().Truncate(time.Second)
	id := "cbx_guest_extend"
	labels := DirectLeaseLabels(cfg, id, "guest-extend", cfg.Provider, "", true, now.Add(-20*time.Minute))
	labels["last_touched_at"] = LeaseLabelTime(now)
	server := Server{Provider: cfg.Provider, CloudID: "fixture-instance", Labels: labels}
	target := SSHTarget{Host: "fixture", Port: "22", TargetOS: targetLinux}
	if err := ClaimLeaseTargetForRepoConfig(id, "guest-extend", cfg, server, target, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = provider
	b := &extendGuestTestBackend{cfg: cfg, now: now, heartbeatDirectBackend: heartbeatDirectBackend{lease: LeaseTarget{LeaseID: id, Server: server, SSH: target}}}
	extendGuestBackendForTest = b
	t.Cleanup(func() { extendGuestBackendForTest = nil })
	return b
}
func (b *extendGuestTestBackend) request(t *testing.T, by, ttl time.Duration) LeaseExtendRequest {
	t.Helper()
	claim, err := ReadLeaseClaim(b.lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	lease := b.lease
	SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	return LeaseExtendRequest{Lease: lease, By: by, TTL: ttl}
}
func TestExtendDirectGuestExpiry(t *testing.T) {
	for _, mode := range []string{"success", "refresh failure", "resolve failure", "absolute TTL retry", "aws", "gcp", "azure"} {
		t.Run(mode, func(t *testing.T) {
			provider := "hetzner"
			if mode == "aws" || mode == "gcp" || mode == "azure" {
				provider = mode
			}
			b := extendGuestFixture(t, provider)
			by, ttl := 30*time.Minute, time.Duration(0)
			if mode == "absolute TTL retry" {
				by, ttl = 0, time.Hour
			}
			req := b.request(t, by, ttl)
			oldDeadline := LeaseLabelTimeDisplay(req.Lease.Server.Labels["expires_at"])
			// Exercise shared target resolution instead of supplying an already resolved route.
			req.Lease.SSH = SSHTarget{}
			if mode == "resolve failure" {
				b.resolveFn = func() (LeaseTarget, error) { return LeaseTarget{}, errors.New("fixture resolution failed") }
			}
			oldRefresh := refreshGuestLeaseExpirySSH
			t.Cleanup(func() { refreshGuestLeaseExpirySSH = oldRefresh })
			calls := 0
			fail := mode == "refresh failure" || mode == "absolute TTL retry"
			refreshGuestLeaseExpirySSH = func(_ context.Context, target SSHTarget, _, content string) error {
				calls++
				claim, err := ReadLeaseClaim(b.lease.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if b.writes != 1 || claim.Labels["ttl_secs"] != "3600" || claim.PendingGuestExtension == nil {
					t.Fatalf("guest refreshed before labels/claim: writes=%d claim=%+v", b.writes, claim)
				}
				if target.Host != "fixture" || !strings.Contains(content, "expires_at="+b.lease.Server.Labels["expires_at"]+"\n") {
					t.Fatalf("target=%+v content=%s", target, content)
				}
				if fail {
					return errors.New("fixture SSH failure")
				}
				return nil
			}
			result, err := b.ExtendLease(t.Context(), req)
			if provider != "hetzner" {
				if err != nil || calls != 0 || b.resolves != 0 || b.writes != 1 {
					t.Fatalf("metadata provider wrote guest: result=%+v err=%v calls=%d", result, err, calls)
				}
				return
			}
			if len(b.requests) != 1 || !b.requests[0].NoLocalStateMutations {
				t.Fatalf("resolution=%+v", b.requests)
			}
			if mode == "success" {
				if err != nil || result.GuestExpiresAt != result.ExpiresAt || calls != 1 {
					t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
				}
			} else {
				if err == nil || !result.GuestExpiryRefreshPending || result.GuestExpiresAt != oldDeadline || result.ExpiresAt == oldDeadline {
					t.Fatalf("partial result=%+v err=%v", result, err)
				}
				for _, want := range []string{oldDeadline, result.ExpiresAt, "will power the machine off", "rerun crabbox extend"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("missing %q: %v", want, err)
					}
				}
				claim, readErr := ReadLeaseClaim(b.lease.LeaseID)
				if readErr != nil || claim.Labels["ttl_secs"] != "3600" || claim.PendingGuestExtension == nil || b.writes != 1 {
					t.Fatalf("rollback or no recovery state: %+v %v", claim, readErr)
				}
				// A new command gets a fresh snapshot and only retries the guest write.
				b.resolveFn = nil
				fail = false
				retry := b.request(t, by, ttl)
				retry.Lease.SSH = SSHTarget{}
				retried, retryErr := b.ExtendLease(t.Context(), retry)
				if retryErr != nil || b.writes != 1 || retried.ExpiresAt != result.ExpiresAt {
					t.Fatalf("non-idempotent retry: %+v err=%v writes=%d", retried, retryErr, b.writes)
				}
			}
			claim, err := ReadLeaseClaim(b.lease.LeaseID)
			if err != nil || claim.PendingGuestExtension != nil {
				t.Fatalf("pending state not cleared: %+v %v", claim, err)
			}
		})
	}
}
func TestExtendGuestFailureJSON(t *testing.T) {
	b := extendGuestFixture(t, "hetzner")
	old := refreshGuestLeaseExpirySSH
	t.Cleanup(func() { refreshGuestLeaseExpirySSH = old })
	refreshGuestLeaseExpirySSH = func(context.Context, SSHTarget, string, string) error { return errors.New("fixture SSH failure") }
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"extend", b.lease.LeaseID, "--provider", "extend-guest-test", "--json"})
	if err == nil {
		t.Fatal("guest failure returned success")
	}
	var result LeaseExtendResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("JSON: %v; output=%s err=%v stderr=%s", decodeErr, &stdout, err, &stderr)
	}
	if !result.GuestExpiryRefreshPending || result.GuestExpiresAt == "" || result.ExpiresAt == result.GuestExpiresAt || result.Error != err.Error() {
		t.Fatalf("partial JSON=%+v err=%v", result, err)
	}
}
