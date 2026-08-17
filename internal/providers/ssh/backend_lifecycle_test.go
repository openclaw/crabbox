package ssh

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type staticLifecycleClock struct{ now time.Time }

func (c staticLifecycleClock) Now() time.Time { return c.now }

func TestStaticSSHTouchPersistsExplicitIdleTimeoutForFreshResolve(t *testing.T) {
	cfg, lease, initial := acquireStaticLifecycleLease(t, 30*time.Minute)
	createdAt := initial.Labels["created_at"]
	initialTouchedAt := initial.Labels["last_touched_at"]
	touchedAt := unixLabelTime(t, initialTouchedAt).Add(2 * time.Minute)

	backend := newStaticLifecycleBackend(cfg, touchedAt)
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: initial.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.LeaseID != lease.LeaseID {
		t.Fatalf("resolved lease=%q want canonical %q", resolved.LeaseID, lease.LeaseID)
	}
	if resolved.Server.Labels["created_at"] != createdAt || resolved.Server.Labels["last_touched_at"] != initialTouchedAt {
		t.Fatalf("resolve fabricated timestamps: labels=%#v claim=%#v", resolved.Server.Labels, initial.Labels)
	}
	if resolved.Server.Labels["idle_timeout_secs"] != "1800" {
		t.Fatalf("resolved idle timeout=%q want persisted 1800", resolved.Server.Labels["idle_timeout_secs"])
	}

	override := 45 * time.Minute
	touched, err := backend.Touch(context.Background(), TouchRequest{
		Lease:               resolved,
		State:               "busy",
		IdleTimeout:         override,
		IdleTimeoutOverride: &override,
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched.ServerType.Name != resolved.Server.ServerType.Name {
		t.Fatalf("touch changed server identity: touched=%#v resolved=%#v", touched, resolved.Server)
	}
	if touched.Status != "busy" || touched.Labels["state"] != "busy" {
		t.Fatalf("touch response state=%q labels=%#v want busy", touched.Status, touched.Labels)
	}

	committed, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists {
		t.Fatalf("read committed claim exists=%t err=%v", exists, err)
	}
	if committed.IdleTimeoutSeconds != 2700 || touched.Labels["idle_timeout_secs"] != "2700" {
		t.Fatalf("timeout mismatch touched=%q claim=%d", touched.Labels["idle_timeout_secs"], committed.IdleTimeoutSeconds)
	}
	if committed.LastUsedAt != touchedAt.Format(time.RFC3339) || touched.Labels["last_touched_at"] != core.LeaseLabelTime(touchedAt) {
		t.Fatalf("last touch mismatch touched=%q claim=%q want=%s", touched.Labels["last_touched_at"], committed.LastUsedAt, touchedAt.Format(time.RFC3339))
	}
	if touched.Labels["created_at"] != createdAt || !reflect.DeepEqual(touched.Labels, committed.Labels) {
		t.Fatalf("touch response and committed labels differ: response=%#v claim=%#v", touched.Labels, committed.Labels)
	}
	if got := unixLabelTime(t, touched.Labels["expires_at"]); !got.Equal(touchedAt.Add(override)) {
		t.Fatalf("expires=%s want=%s", got, touchedAt.Add(override))
	}

	freshCfg := cfg
	freshCfg.IdleTimeout = 7 * time.Minute
	fresh, err := newStaticLifecycleBackend(freshCfg, touchedAt.Add(time.Minute)).Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Server.Labels["idle_timeout_secs"] != "2700" || fresh.Server.Labels["last_touched_at"] != touched.Labels["last_touched_at"] || fresh.Server.Labels["created_at"] != createdAt {
		t.Fatalf("fresh resolve disagrees with committed touch: fresh=%#v touched=%#v", fresh.Server.Labels, touched.Labels)
	}
	if fresh.Server.Status != "busy" || fresh.Server.Labels["state"] != "busy" {
		t.Fatalf("fresh resolve state=%q labels=%#v want busy", fresh.Server.Status, fresh.Server.Labels)
	}
}

func TestStaticSSHTouchRefreshesAcquiredLeaseCache(t *testing.T) {
	cfg, lease, initial := acquireStaticLifecycleLease(t, 30*time.Minute)
	touchedAt := unixLabelTime(t, initial.Labels["last_touched_at"]).Add(time.Minute)
	backend := newStaticLifecycleBackend(cfg, touchedAt)
	backend.rememberAcquiredLease(lease)

	override := 45 * time.Minute
	touched, err := backend.Touch(context.Background(), TouchRequest{
		Lease:               lease,
		State:               "busy",
		IdleTimeoutOverride: &override,
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.Labels["idle_timeout_secs"] != "2700" || !reflect.DeepEqual(resolved.Server.Labels, touched.Labels) {
		t.Fatalf("cached resolve did not observe touch: resolved=%#v touched=%#v", resolved.Server.Labels, touched.Labels)
	}
	listed, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !reflect.DeepEqual(listed[0].Labels, touched.Labels) {
		t.Fatalf("cached list did not observe touch: listed=%#v touched=%#v", listed, touched.Labels)
	}

	backend.RT.Clock = staticLifecycleClock{now: touchedAt.Add(time.Minute)}
	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: resolved, State: "ready"}); err != nil {
		t.Fatalf("touch using cached refreshed snapshot: %v", err)
	}
}

func TestStaticSSHTouchWithoutOverridePreservesPersistedIdleTimeout(t *testing.T) {
	cfg, lease, initial := acquireStaticLifecycleLease(t, 37*time.Minute)
	touchedAt := unixLabelTime(t, initial.Labels["last_touched_at"]).Add(time.Minute)
	backendCfg := cfg
	backendCfg.IdleTimeout = 5 * time.Minute
	backend := newStaticLifecycleBackend(backendCfg, touchedAt)
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	touched, err := backend.Touch(context.Background(), TouchRequest{
		Lease:       resolved,
		State:       "ready",
		IdleTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	committed, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists {
		t.Fatalf("read committed claim exists=%t err=%v", exists, err)
	}
	if committed.IdleTimeoutSeconds != 2220 || touched.Labels["idle_timeout_secs"] != "2220" {
		t.Fatalf("omitted override replaced timeout: response=%q claim=%d", touched.Labels["idle_timeout_secs"], committed.IdleTimeoutSeconds)
	}
	fresh, err := newStaticLifecycleBackend(backendCfg, touchedAt.Add(time.Minute)).Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Server.Labels["idle_timeout_secs"] != "2220" || fresh.Server.Labels["last_touched_at"] != core.LeaseLabelTime(touchedAt) {
		t.Fatalf("fresh resolve lost preserved lifecycle: %#v", fresh.Server.Labels)
	}
}

func TestStaticSSHTouchRejectsMismatchedIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *LeaseTarget)
	}{
		{name: "canonical lease ID", mutate: func(_ *testing.T, lease *LeaseTarget) { lease.LeaseID = lease.Server.Labels["slug"] }},
		{name: "provider", mutate: func(_ *testing.T, lease *LeaseTarget) { lease.Server.Provider = "aws" }},
		{name: "scope", mutate: func(t *testing.T, lease *LeaseTarget) {
			claim, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
			if !exists || !set {
				t.Fatal("resolved lease has no claim snapshot")
			}
			claim.ProviderScope = "other-scope"
			core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		}},
		{name: "resource", mutate: func(_ *testing.T, lease *LeaseTarget) { lease.Server.CloudID = "static_replacement" }},
		{name: "host", mutate: func(_ *testing.T, lease *LeaseTarget) { lease.SSH.Host = "replacement.example.test" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, lease, initial := acquireStaticLifecycleLease(t, 30*time.Minute)
			backend := newStaticLifecycleBackend(cfg, unixLabelTime(t, initial.Labels["last_touched_at"]).Add(time.Minute))
			resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, &resolved)
			if _, err := backend.Touch(context.Background(), TouchRequest{Lease: resolved, State: "ready"}); err == nil || !strings.Contains(err.Error(), "mismatch") {
				t.Fatalf("touch error=%v want identity mismatch", err)
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !exists || !reflect.DeepEqual(after, initial) {
				t.Fatalf("rejected touch changed claim: exists=%t err=%v before=%#v after=%#v", exists, err, initial, after)
			}
		})
	}
}

func TestStaticSSHTouchRejectsConcurrentClaimReplacement(t *testing.T) {
	cfg, lease, initial := acquireStaticLifecycleLease(t, 30*time.Minute)
	backend := newStaticLifecycleBackend(cfg, unixLabelTime(t, initial.Labels["last_touched_at"]).Add(time.Minute))
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	replacementLabels := make(map[string]string, len(initial.Labels)+1)
	for key, value := range initial.Labels {
		replacementLabels[key] = value
	}
	replacementLabels["replacement"] = "true"
	replacement, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, replacementLabels)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: resolved, State: "ready"}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("touch error=%v want claim changed", err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(after, replacement) {
		t.Fatalf("touch overwrote replacement: exists=%t err=%v replacement=%#v after=%#v", exists, err, replacement, after)
	}
}

func TestStaticSSHProviderReplacementFailsClosed(t *testing.T) {
	cfg, lease, initial := acquireStaticLifecycleLease(t, 30*time.Minute)
	backend := newStaticLifecycleBackend(cfg, unixLabelTime(t, initial.Labels["last_touched_at"]).Add(time.Minute))
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	var replacement core.LeaseClaim
	if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists {
			return errors.New("claim disappeared before replacement")
		}
		claim.Provider = "aws"
		claim.ProviderScope = "region:replacement"
		claim.CloudID = "i-replacement"
		claim.StaticHost = "replacement.example.test"
		if err := persist(); err != nil {
			return err
		}
		replacement = *claim
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: resolved, State: "ready"}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("touch error=%v want claim changed", err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(after, replacement) {
		t.Fatalf("provider replacement was not preserved: exists=%t err=%v replacement=%#v after=%#v", exists, err, replacement, after)
	}

	fresh := newStaticLifecycleBackend(cfg, time.Now().UTC())
	unclaimed, err := fresh.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, set := core.ServerLeaseClaimSnapshot(unclaimed.Server); exists || set {
		t.Fatalf("provider-mismatched exact claim authorized static resolve: %#v", unclaimed.Server)
	}
}

func TestStaticSSHTouchDoesNotRecreateRacedAwayClaim(t *testing.T) {
	cfg, lease, initial := acquireStaticLifecycleLease(t, 30*time.Minute)
	backend := newStaticLifecycleBackend(cfg, unixLabelTime(t, initial.Labels["last_touched_at"]).Add(time.Minute))
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	core.RemoveLeaseClaim(lease.LeaseID)

	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: resolved, State: "ready"}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("touch error=%v want claim changed", err)
	}
	if claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("raced-away claim was recreated: exists=%t err=%v claim=%#v", exists, err, claim)
	}
}

func acquireStaticLifecycleLease(t *testing.T, idleTimeout time.Duration) (Config, LeaseTarget, core.LeaseClaim) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldWait := waitForSSH
	waitForSSH = func(context.Context, *SSHTarget, io.Writer) error { return nil }
	t.Cleanup(func() { waitForSSH = oldWait })

	cfg := core.BaseConfig()
	cfg.Provider = staticProvider
	cfg.TargetOS = core.TargetLinux
	cfg.Static.Host = "static.example.test"
	cfg.Static.ID = "static_heartbeat"
	cfg.Static.Name = "heartbeat-static"
	cfg.IdleTimeout = idleTimeout
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*staticLeaseBackend)
	lease, err := backend.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists {
		t.Fatalf("read acquired claim exists=%t err=%v", exists, err)
	}
	return cfg, lease, claim
}

func newStaticLifecycleBackend(cfg Config, now time.Time) *staticLeaseBackend {
	return NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard, Clock: staticLifecycleClock{now: now}}).(*staticLeaseBackend)
}

func unixLabelTime(t *testing.T, value string) time.Time {
	t.Helper()
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse timestamp label %q: %v", value, err)
	}
	return time.Unix(seconds, 0).UTC()
}
