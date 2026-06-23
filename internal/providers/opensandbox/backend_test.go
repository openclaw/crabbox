package opensandbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "opensandbox" {
		t.Fatalf("Name=%q want opensandbox", p.Name())
	}
	if len(p.Aliases()) != 0 {
		t.Fatalf("v1 should not register aliases, got %#v", p.Aliases())
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want [{linux}]", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive-sync", spec.Features)
	}
	if !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("features=%#v want cleanup", spec.Features)
	}
	if !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%#v want run-session", spec.Features)
	}
}

func TestProviderForResolvesNameOnly(t *testing.T) {
	got, err := core.ProviderFor("opensandbox")
	if err != nil {
		t.Fatalf("ProviderFor(opensandbox): %v", err)
	}
	if got.Name() != "opensandbox" {
		t.Fatalf("Name=%q want opensandbox", got.Name())
	}
	for _, alias := range []string{"osb", "open-sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "opensandbox" {
			t.Fatalf("alias %q unexpectedly resolves to opensandbox", alias)
		}
	}
}

func TestOpenSandboxFlagsRejectNegativeTimeouts(t *testing.T) {
	for _, flagName := range []string{"opensandbox-timeout-secs", "opensandbox-exec-timeout-secs"} {
		t.Run(flagName, func(t *testing.T) {
			cfg := testConfig()
			cfg.Provider = providerName
			fs := flag.NewFlagSet(flagName, flag.ContinueOnError)
			values := RegisterOpenSandboxProviderFlags(fs, cfg)
			if err := fs.Parse([]string{"--" + flagName, "-1"}); err != nil {
				t.Fatal(err)
			}
			err := ApplyOpenSandboxProviderFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "must be non-negative") {
				t.Fatalf("err=%v, want non-negative timeout rejection", err)
			}
		})
	}
}

func TestOpenSandboxConfigRejectsTTLBelowCommandTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.OpenSandbox.TimeoutSecs = 600
	cfg.OpenSandbox.ExecTimeoutSecs = 900
	err := validateOpenSandboxRunConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "effective lifetime 10m0s") {
		t.Fatalf("err=%v, want TTL/command timeout validation", err)
	}
}

func TestOpenSandboxLifecycleConfigAllowsLegacyLongCommandBudget(t *testing.T) {
	cfg := testConfig()
	cfg.OpenSandbox.ExecTimeoutSecs = 3600
	if err := validateOpenSandboxConfig(cfg); err != nil {
		t.Fatalf("lifecycle config validation must not strand status/stop: %v", err)
	}
	if err := validateOpenSandboxRunConfig(cfg); err != nil {
		t.Fatalf("90m TTL should cover legacy 60m command budget: %v", err)
	}
}

func TestWarmupRejectsUnusableLifetimeBeforeCreate(t *testing.T) {
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cfg.TTL = 10 * time.Minute
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}})
	if err == nil || !strings.Contains(err.Error(), "effective lifetime") {
		t.Fatalf("err=%v, want unusable lifetime rejection", err)
	}
	if fake.created.Image != "" {
		t.Fatalf("created=%#v, want validation before create", fake.created)
	}
}

func TestWarmupRejectsInvalidWorkdirBeforeCreate(t *testing.T) {
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cfg.OpenSandbox.Workdir = "/workspace"
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}})
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v, want workdir rejection", err)
	}
	if fake.created.Image != "" {
		t.Fatalf("created=%#v, want validation before create", fake.created)
	}
}

func TestRunAndWarmupRejectTailscaleBeforeCreate(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		fake := newFakeClient()
		backend := newTestBackend(fake)
		_, err := backend.Run(context.Background(), RunRequest{
			Repo: Repo{Name: "my-app", Root: "/repo"},
			Options: core.LeaseOptions{
				Tailscale: core.TailscaleConfig{Enabled: true},
			},
			Command: []string{"true"},
		})
		if err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
			t.Fatalf("err=%v, want Tailscale rejection", err)
		}
		if fake.created.Image != "" {
			t.Fatalf("created=%#v, want validation before create", fake.created)
		}
	})

	t.Run("warmup", func(t *testing.T) {
		fake := newFakeClient()
		backend := newTestBackend(fake)
		err := backend.Warmup(context.Background(), WarmupRequest{
			Repo: Repo{Name: "my-app", Root: "/repo"},
			Options: core.LeaseOptions{
				Tailscale: core.TailscaleConfig{Enabled: true},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
			t.Fatalf("err=%v, want Tailscale rejection", err)
		}
		if fake.created.Image != "" {
			t.Fatalf("created=%#v, want validation before create", fake.created)
		}
	})
}

func TestWarmupCleansUpWhenActualExpirationCannotFitRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.createExpiresIn = 20 * time.Minute
	backend := newTestBackend(fake)
	err := backend.Warmup(context.Background(), WarmupRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "less than the") {
		t.Fatalf("err=%v, want actual expiration rejection", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v want warmup rollback", fake.deleted)
	}
	if claim, err := readLeaseClaim(leasePrefix + fake.sandbox.ID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim=%#v want rollback removal", claim)
	}
}

func TestOpenSandboxRunBudgetsIncludeRequiredRemoteOperations(t *testing.T) {
	cfg := testConfig()
	commandBudget := 10*time.Minute + 30*time.Second
	for _, tc := range []struct {
		name     string
		noSync   bool
		syncOnly bool
		want     time.Duration
	}{
		{name: "sync and command", want: 15*time.Minute + commandBudget},
		{name: "no sync still creates workdir then runs command", noSync: true, want: 2 * commandBudget},
		{name: "sync only", syncOnly: true, want: 15 * time.Minute},
		{name: "no sync sync only creates workdir", noSync: true, syncOnly: true, want: commandBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openSandboxRunBudgetForConfig(cfg, tc.noSync, tc.syncOnly); got != tc.want {
				t.Fatalf("budget=%s want %s", got, tc.want)
			}
		})
	}
}

func TestRunRejectsRequestBudgetBeforeCreate(t *testing.T) {
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cfg.OpenSandbox.TimeoutSecs = 1200
	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: "/repo"}, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "sync/command budget") {
		t.Fatalf("err=%v, want request budget rejection", err)
	}
	if fake.created.Image != "" {
		t.Fatalf("created=%#v, want validation before create", fake.created)
	}
}

func TestRunReconcilesAmbiguousCreateFailureByOwnershipMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.createErr = &ambiguousOpenSandboxCreateError{cause: context.DeadlineExceeded}
	backend := newTestBackend(fake)

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want ambiguous create cause", err)
	}
	if len(fake.listFilters) != 1 {
		t.Fatalf("list filters=%#v, want ownership reconciliation", fake.listFilters)
	}
	scope := fake.created.Metadata[openSandboxClaimKey]
	if scope == "" || fake.listFilters[0][openSandboxClaimKey] != scope {
		t.Fatalf("scope=%q list filters=%#v", scope, fake.listFilters)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v, want ambiguous sandbox cleanup", fake.deleted)
	}
	if claim, claimErr := readLeaseClaim(leasePrefix + fake.sandbox.ID); claimErr != nil {
		t.Fatal(claimErr)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim=%#v, want no local claim after ambiguous create", claim)
	}
	if claims, claimErr := listOpenSandboxLeaseClaims(); claimErr != nil {
		t.Fatal(claimErr)
	} else if len(claims) != 0 {
		t.Fatalf("claims=%#v, want reconciled recovery removed", claims)
	}
}

func TestRunReconcilesAmbiguousCreateAfterDelayedVisibility(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.createErr = &ambiguousOpenSandboxCreateError{cause: context.DeadlineExceeded}
	fake.listEmptyCount = 1
	backend := newTestBackend(fake)
	backend.cleanupTimeoutOverride = 200 * time.Millisecond
	backend.reconcilePollOverride = time.Millisecond

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want ambiguous create cause", err)
	}
	if len(fake.listFilters) != 2 {
		t.Fatalf("list filters=%#v, want delayed ownership reconciliation", fake.listFilters)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v, want delayed ambiguous sandbox cleanup", fake.deleted)
	}
}

func TestRunRetriesTransientAmbiguousCreateReconciliationFailures(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.createErr = &ambiguousOpenSandboxCreateError{cause: context.DeadlineExceeded}
	fake.listErr = context.DeadlineExceeded
	fake.listErrCount = 1
	fake.deleteErr = context.DeadlineExceeded
	fake.deleteErrCount = 1
	backend := newTestBackend(fake)
	backend.cleanupTimeoutOverride = 200 * time.Millisecond
	backend.reconcilePollOverride = time.Millisecond

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want ambiguous create cause", err)
	}
	if len(fake.listFilters) != 3 {
		t.Fatalf("list filters=%#v, want retries after transient list and delete failures", fake.listFilters)
	}
	if len(fake.deleted) != 2 {
		t.Fatalf("deleted=%#v, want delete retry", fake.deleted)
	}
}

func TestRunDoesNotReconcileUnmarkedCreateFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.createErr = context.DeadlineExceeded
	backend := newTestBackend(fake)

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want create cause", err)
	}
	if len(fake.listFilters) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("list filters=%#v deleted=%#v, want no reconciliation for a client-cleaned failure", fake.listFilters, fake.deleted)
	}
}

func TestRunRetainsAmbiguousCreateRecoveryForLaterCleanup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.createErr = &ambiguousOpenSandboxCreateError{cause: context.DeadlineExceeded}
	fake.listEmptyCount = 1000
	backend := newTestBackend(fake)
	backend.cleanupTimeoutOverride = 5 * time.Millisecond
	backend.reconcilePollOverride = time.Millisecond

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want ambiguous create cause", err)
	}
	if claims, claimErr := listOpenSandboxLeaseClaims(); claimErr != nil {
		t.Fatal(claimErr)
	} else if len(claims) != 0 {
		t.Fatalf("claims=%#v, recovery must not appear as a normal lease", claims)
	}
	claims, claimErr := listOpenSandboxCleanupClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || !strings.HasPrefix(claims[0].LeaseID, recoveryPrefix) {
		t.Fatalf("claims=%#v, want retained recovery", claims)
	}
	if claims[0].ProviderScope != fake.created.Metadata[openSandboxClaimKey] || !strings.Contains(err.Error(), claims[0].LeaseID) {
		t.Fatalf("claim=%#v err=%v, want discoverable ownership recovery", claims[0], err)
	}
	if claims[0].Pond != "" {
		t.Fatalf("claim=%#v, recovery must not join a pond", claims[0])
	}
	if claim, ok, resolveErr := resolveOpenSandboxLeaseClaim(claims[0].LeaseID, fake.BaseURL()); resolveErr != nil || ok {
		t.Fatalf("claim=%#v ok=%t err=%v, recovery must not resolve as a normal lease", claim, ok, resolveErr)
	}

	fake.listEmptyCount = 0
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v, want recovered sandbox cleanup", fake.deleted)
	}
	if claims, claimErr = listOpenSandboxCleanupClaims(); claimErr != nil {
		t.Fatal(claimErr)
	} else if len(claims) != 0 {
		t.Fatalf("claims=%#v, want recovery removed after cleanup", claims)
	}
}

func TestAmbiguousCreateReconciliationUsesRecoveryOperationLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cleanupTimeoutOverride = 5 * time.Millisecond
	scope := testOpenSandboxScope(t, fake.BaseURL())
	recoveryLeaseID, err := backend.recordAmbiguousCreate(scope, Repo{Root: tempGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockOpenSandboxLeaseOperation(context.Background(), recoveryLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	cause := &ambiguousOpenSandboxCreateError{cause: context.DeadlineExceeded}
	err = backend.reconcileAmbiguousCreateFailure(context.Background(), fake, scope, recoveryLeaseID, cause)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "lock opensandbox create recovery") {
		t.Fatalf("err=%v, want retained cause and lock failure", err)
	}
	if len(fake.listFilters) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("list filters=%#v deleted=%#v, reconciliation must not run without the recovery lock", fake.listFilters, fake.deleted)
	}
}

func TestOpenSandboxCreateAmbiguityClassification(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		io.ErrUnexpectedEOF,
		syscall.ECONNRESET,
		syscall.EPIPE,
	} {
		if !isOpenSandboxAmbiguousCreateError(err) {
			t.Errorf("err=%v, want ambiguous", err)
		}
	}
	for _, err := range []error{
		syscall.ECONNREFUSED,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
	} {
		if isOpenSandboxAmbiguousCreateError(err) {
			t.Errorf("err=%v, want definitive pre-request failure", err)
		}
	}
}

func TestOpenSandboxReadinessRetriesStartupConnectionFailures(t *testing.T) {
	for _, err := range []error{
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
	} {
		if !isOpenSandboxReadinessPending(err) {
			t.Errorf("err=%v, want retryable readiness failure", err)
		}
	}
}

func TestOpenSandboxWorkdirRejectsBroadPaths(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/workspace", "/workspace/.."} {
		t.Run(workdir, func(t *testing.T) {
			cfg := testConfig()
			cfg.OpenSandbox.Workdir = workdir
			if _, err := openSandboxWorkdir(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
				t.Fatalf("err=%v, want too broad rejection", err)
			}
		})
	}
}

func TestOpenSandboxClaimScopeIsMetadataLabelSafe(t *testing.T) {
	scope, err := newOpenSandboxClaimScope("https://opensandbox.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) > 63 {
		t.Fatalf("scope length=%d want <=63: %q", len(scope), scope)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`).MatchString(scope) {
		t.Fatalf("scope is not a valid metadata label value: %q", scope)
	}
}

func TestRunCreatesSandboxForwardsEnvAndCleansUp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"printenv", "API_TOKEN"},
		Env:     map[string]string{"API_TOKEN": "secret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("result.Session is nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != leasePrefix+fake.sandbox.ID {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.Reused {
		t.Fatalf("session.Reused=true, want false")
	}
	if result.Session.Kept {
		t.Fatalf("session.Kept=true, want false after cleanup")
	}
	if !strings.Contains(result.Session.CleanupCommand, "crabbox stop --provider opensandbox") {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	if fake.created.Image != "ubuntu:24.04" || fake.created.Metadata[openSandboxClaimKey] == "" || fake.created.Metadata[openSandboxNameKey] == "" {
		t.Fatalf("create request not populated: %#v", fake.created)
	}
	if fake.created.TimeoutSecs != 5400 {
		t.Fatalf("create timeout=%d want Crabbox TTL cap 5400", fake.created.TimeoutSecs)
	}
	if got := fake.runs[len(fake.runs)-1].Env["API_TOKEN"]; got != "secret-value" {
		t.Fatalf("env forwarded as %q", got)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v want cleanup of created sandbox", fake.deleted)
	}
	if claims, err := listOpenSandboxLeaseClaims(); err != nil {
		t.Fatal(err)
	} else if len(claims) != 0 {
		t.Fatalf("claim not cleaned up: %#v", claims)
	}
}

func TestRunRefreshesMissingCreateExpirationAfterCleanupIsArmed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.omitCreateExpiration = true
	backend := newTestBackend(fake)
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	}); err != nil {
		t.Fatal(err)
	}
	if fake.getCalls == 0 {
		t.Fatal("expected expiration refresh after create omitted expiresAt")
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted=%#v want normal cleanup", fake.deleted)
	}
}

func TestRunDoesNotPublishClaimWhenCreationLockIsCanceled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	unlock, err := lockOpenSandboxLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = backend.Run(ctx, RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want operation lock deadline", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v want unpublished sandbox rollback", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim=%#v must not be published without operation lock", claim)
	}
}

func TestRunNoSyncEnsuresWorkspaceWithPortableShell(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.runs) < 2 {
		t.Fatalf("runs=%#v, want workspace setup and user command", fake.runs)
	}
	if !strings.HasPrefix(fake.runs[0].Command, "sh -lc ") {
		t.Fatalf("workspace command=%q, want sh -lc", fake.runs[0].Command)
	}
}

func TestRunTimingJSONRemainsFinalLineWhenActivityRefreshFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "timing", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	fake.afterRun = func(req runCommandRequest) {
		if req.Workdir != "" {
			_ = os.WriteFile(filepath.Join(stateDir, "claims", leaseID+".json"), []byte("{"), 0o600)
		}
	}

	_, err = backend.Run(context.Background(), RunRequest{
		ID: "timing", Repo: Repo{Name: "my-app", Root: "/repo"}, NoSync: true, Command: []string{"true"}, TimingJSON: true,
	})
	if err == nil {
		t.Fatal("expected activity refresh failure")
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); jsonErr != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], jsonErr)
	}
	if report["exitCode"] != float64(1) {
		t.Fatalf("timing exitCode=%v want 1", report["exitCode"])
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "provider-error" {
		t.Fatalf("timing outcome status=%v kind=%v", report["runStatus"], report["errorKind"])
	}
}

func TestRunTimingJSONRemainsFinalLineWhenCleanupFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.deleteErr = errors.New("provider delete unavailable")
	backend := newTestBackend(fake)
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr

	if _, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempGitRepo(t)}, NoSync: true, Command: []string{"true"}, TimingJSON: true,
	}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); jsonErr != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], jsonErr)
	}
	if !strings.Contains(stderr.String(), "provider delete unavailable") {
		t.Fatalf("stderr=%q want cleanup warning", stderr.String())
	}
}

func TestRunPreservesBashLoginShellForExplicitInvocation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"bash", "-lc", "echo hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := fake.runs[len(fake.runs)-1].Command
	want := shellScriptFromArgv([]string{"bash", "-lc", "echo hello"})
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestRunPreservesBashLoginShellForAutoWrappedMetachars(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"pnpm", "install", "&&", "pnpm", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := fake.runs[len(fake.runs)-1].Command
	inner := shellScriptFromArgv([]string{"pnpm", "install", "&&", "pnpm", "test"})
	want := shellScriptFromArgv([]string{"bash", "-lc", inner})
	if got != want {
		t.Fatalf("command=%q want %q", got, want)
	}
}

func TestRunKeepsSandboxOnFailureWhenRequested(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.runExit = 7
	backend := newTestBackend(fake)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:        true,
		Command:       []string{"false"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "exited 7") {
		t.Fatalf("err=%v, want exit error", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v, want kept on failure", fake.deleted)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.Reused {
		t.Fatalf("session=%#v, want retained new sandbox", result.Session)
	}
	claim, err := readLeaseClaim(leasePrefix + fake.sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestRunRejectsNonLinuxPlatformOS(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cfg.OpenSandbox.PlatformOS = "windows"
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "only supports Linux") {
		t.Fatalf("err=%v, want Linux-only platform rejection", err)
	}
	if fake.created.Image != "" {
		t.Fatalf("created sandbox despite invalid platform: %#v", fake.created)
	}
}

func TestRunRejectsPartialPlatformConstraint(t *testing.T) {
	for _, tc := range []struct {
		name string
		os   string
		arch string
	}{
		{name: "OS only", os: "linux"},
		{name: "architecture only", arch: "amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := newFakeClient()
			backend := newTestBackend(fake)
			backend.cfg.OpenSandbox.PlatformOS = tc.os
			backend.cfg.OpenSandbox.PlatformArch = tc.arch
			_, err := backend.Run(context.Background(), RunRequest{
				Repo:    Repo{Name: "my-app", Root: tempGitRepo(t)},
				NoSync:  true,
				Command: []string{"true"},
			})
			if err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("err=%v, want partial platform rejection", err)
			}
			if fake.created.Image != "" {
				t.Fatalf("created sandbox despite partial platform: %#v", fake.created)
			}
		})
	}
}

func TestRunVerifiesOwnershipBeforeReclaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, testOpenSandboxScope(t, fake.baseURL), "", "/original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = "different"
	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/replacement"}, Reclaim: true, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership mismatch", err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/original" {
		t.Fatalf("repo root=%q changed before ownership verification", claim.RepoRoot)
	}
}

func TestRunResumesPausedSandboxBeforeReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.sandbox.State = "Paused"
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope
	result, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/repo"}, NoSync: true, Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained reused sandbox", result.Session)
	}
	if result.Session.LeaseID != leaseID || result.Session.Slug != "mine" {
		t.Fatalf("session=%#v, want lease=%s slug=mine", result.Session, leaseID)
	}
	if len(fake.resumed) != 1 || fake.resumed[0] != fake.sandbox.ID {
		t.Fatalf("resumed=%#v", fake.resumed)
	}
}

func TestRunReuseUsesActualExpirationInsteadOfCreationConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.cfg.OpenSandbox.TimeoutSecs = 1200
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "actual-expiration", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	if _, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/repo"}, NoSync: true, Command: []string{"true"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunDoesNotResumePausedSandboxBeforeRepoAuthorization(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.sandbox.State = "Paused"
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, scope, "", "/original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/other"}, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "is claimed by repo /original") {
		t.Fatalf("err=%v, want repository claim rejection", err)
	}
	if len(fake.resumed) != 0 {
		t.Fatalf("resumed=%#v, want no remote mutation before authorization", fake.resumed)
	}
}

func TestRunDoesNotResumeOrReclaimPausedSandboxBeforeLifetimePreflight(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.sandbox.State = "Paused"
	expiresAt := time.Now().Add(5 * time.Minute)
	fake.sandbox.ExpiresAt = &expiresAt
	backend := newTestBackend(fake)
	stderr := &bytes.Buffer{}
	backend.rt.Stderr = stderr
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, scope, "", "/original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/other"}, Reclaim: true, NoSync: true, KeepOnFailure: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "sync/command budget") {
		t.Fatalf("err=%v, want lifetime preflight rejection", err)
	}
	if len(fake.resumed) != 0 {
		t.Fatalf("resumed=%#v, want no remote mutation before lifetime preflight", fake.resumed)
	}
	if strings.Contains(stderr.String(), "rerun:") {
		t.Fatalf("stderr=%q, want no unusable pre-reclaim rerun hint", stderr.String())
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/original" {
		t.Fatalf("repo root=%q changed before lifetime preflight", claim.RepoRoot)
	}
}

func TestRunRechecksLifetimeAfterResumeBeforeReclaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	fake := newFakeClient()
	fake.sandbox.State = "Paused"
	expiresAt := now.Add(25 * time.Minute)
	fake.sandbox.ExpiresAt = &expiresAt
	backend := newTestBackend(fake)
	backend.rt.Clock = fixedClock{now: now}
	fake.afterResume = func() {
		backend.rt.Clock = fixedClock{now: now.Add(10 * time.Minute)}
	}
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, scope, "", "/original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/other"}, Reclaim: true, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "remaining after resume") {
		t.Fatalf("err=%v, want post-resume lifetime rejection", err)
	}
	if len(fake.resumed) != 1 {
		t.Fatalf("resumed=%#v, want one validated resume attempt", fake.resumed)
	}
	if len(fake.runs) != 0 {
		t.Fatalf("runs=%#v, want no command after post-resume lifetime rejection", fake.runs)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/original" {
		t.Fatalf("repo root=%q changed before post-resume lifetime preflight", claim.RepoRoot)
	}
}

func TestRunReclaimPersistsOnlyAfterReusableSandboxValidation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.sandbox.State = "Stopped"
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, scope, "", "/original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	_, err := backend.Run(context.Background(), RunRequest{
		ID: leaseID, Repo: Repo{Name: "my-app", Root: "/other"}, Reclaim: true, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be reused") {
		t.Fatalf("err=%v, want reusable sandbox rejection", err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/original" {
		t.Fatalf("repo root=%q changed before reusable sandbox validation", claim.RepoRoot)
	}
}

func TestStopRejectsOwnershipMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, testOpenSandboxScope(t, fake.baseURL), "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = "different"
	err := backend.Stop(context.Background(), StopRequest{ID: "mine"})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership mismatch", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v after ownership mismatch", fake.deleted)
	}
}

func TestStopForgetMissingRemovesClaimOnlyWhenExplicit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.notFound = true
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "stale", providerName, testOpenSandboxScope(t, fake.baseURL), "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: "stale"}); err == nil {
		t.Fatal("expected missing sandbox to fail without forget flag")
	}
	backend.cfg.OpenSandbox.ForgetMissing = true
	if err := backend.Stop(context.Background(), StopRequest{ID: "stale"}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim still present after forget-missing: %#v", claim)
	}
}

func TestStopSerializesWithActiveLeaseRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.runStarted = make(chan struct{}, 1)
	fake.runRelease = make(chan struct{})
	backend := newTestBackend(fake)
	backend.rt.Stdout = io.Discard
	backend.rt.Stderr = io.Discard
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "active", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	runDone := make(chan error, 1)
	go func() {
		_, err := backend.Run(context.Background(), RunRequest{
			ID: "active", Repo: Repo{Name: "my-app", Root: "/repo"}, NoSync: true, Command: []string{"true"},
		})
		runDone <- err
	}()
	select {
	case <-fake.runStarted:
	case <-time.After(time.Second):
		t.Fatal("run did not reach user command")
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- backend.Stop(context.Background(), StopRequest{ID: "active"})
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("stop completed while run held operation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(fake.runRelease)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v want stopped sandbox", fake.deleted)
	}
}

func TestCleanupDeletesOwnedIdleSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "idle", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v want idle sandbox", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim=%#v want removed", claim)
	}
}

func TestCleanupDryRunPreservesOwnedIdleSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "idle", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v after dry run", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID == "" {
		t.Fatal("claim removed during dry run")
	}
}

func TestCleanupRejectsIdleSandboxOwnershipMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	backend := newTestBackend(fake)
	backend.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "idle", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = "different"

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership mismatch", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v after ownership mismatch", fake.deleted)
	}
}

func TestCleanupRemovesMissingSandboxClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.notFound = true
	backend := newTestBackend(fake)
	backend.cfg.OpenSandbox.ForgetMissing = true
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "missing", providerName, scope, "", "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim=%#v want stale claim removed", claim)
	}
}

func TestCleanupPreservesAmbiguousMissingSandboxClaimByDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.notFound = true
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "missing", providerName, scope, "", "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID == "" {
		t.Fatal("ambiguous missing claim was removed without forget-missing")
	}
}

func TestCleanupSerializesWithLeaseReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.deleteStarted = make(chan struct{}, 1)
	fake.deleteRelease = make(chan struct{})
	backend := newTestBackend(fake)
	backend.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "idle", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- backend.Cleanup(context.Background(), CleanupRequest{})
	}()
	select {
	case <-fake.deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start deleting")
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := backend.Run(context.Background(), RunRequest{
			ID: "idle", Repo: Repo{Name: "my-app", Root: "/repo"}, NoSync: true, Command: []string{"true"},
		})
		runDone <- err
	}()
	select {
	case err := <-runDone:
		t.Fatalf("reuse completed while cleanup held operation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(fake.deleteRelease)
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err == nil || !strings.Contains(err.Error(), "not claimed") {
		t.Fatalf("reuse err=%v, want missing claim after serialized cleanup", err)
	}
	if len(fake.runs) != 0 {
		t.Fatalf("runs=%#v, deleted sandbox must not execute", fake.runs)
	}
}

func TestOpenSandboxOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockOpenSandboxLeaseOperation(context.Background(), leasePrefix+"lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := lockOpenSandboxLeaseOperation(ctx, leasePrefix+"lock-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestStatusWaitsForExecdHealth(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.pingFailures = 1
	backend := newTestBackend(fake)
	backend.statusPollOverride = time.Millisecond
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "status-test", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	view, err := backend.Status(context.Background(), StatusRequest{
		ID: "status-test", Wait: true, WaitTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !view.Ready || fake.pingCalls != 2 {
		t.Fatalf("view=%#v pingCalls=%d want ready after second ping", view, fake.pingCalls)
	}
}

func TestStatusWithoutWaitBoundsExecdHealthProbe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.pingWaitForCancel = true
	backend := newTestBackend(fake)
	backend.statusProbeOverride = 20 * time.Millisecond
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "status-bounded", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	start := time.Now()
	view, err := backend.Status(context.Background(), StatusRequest{ID: "status-bounded"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Ready {
		t.Fatalf("view=%#v want not ready after timed out health probe", view)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("status probe took %s, want under 1s", elapsed)
	}
}

func TestStatusSurfacesHardExecdHealthFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.pingErr = errors.New("malformed execd endpoint")
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "status-hard-failure", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-hard-failure"})
	if err == nil || !strings.Contains(err.Error(), "malformed execd endpoint") {
		t.Fatalf("err=%v, want hard health failure", err)
	}
}

func TestStatusSurfacesTLSExecdHealthFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeClient()
	fake.pingErr = &url.Error{
		Op:  "Get",
		URL: "https://execd.example.test/health",
		Err: errors.New("tls: failed to verify certificate"),
	}
	backend := newTestBackend(fake)
	leaseID := leasePrefix + fake.sandbox.ID
	scope := testOpenSandboxScope(t, fake.baseURL)
	if err := claimLeaseForRepoProviderScopePond(leaseID, "status-tls-failure", providerName, scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Metadata[openSandboxClaimKey] = scope

	_, err := backend.Status(context.Background(), StatusRequest{
		ID: "status-tls-failure", Wait: true, WaitTimeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "failed to verify certificate") {
		t.Fatalf("err=%v, want TLS health failure", err)
	}
	if fake.pingCalls != 1 {
		t.Fatalf("pingCalls=%d want 1; permanent TLS failure must not be retried", fake.pingCalls)
	}
}

func TestOpenSandboxReadinessRetriesTransientTransportErrors(t *testing.T) {
	for name, err := range map[string]error{
		"EOF":            &url.Error{Op: "Get", URL: "https://execd.example.test/ping", Err: io.EOF},
		"unexpected EOF": &url.Error{Op: "Get", URL: "https://execd.example.test/ping", Err: io.ErrUnexpectedEOF},
		"temporary DNS":  &url.Error{Op: "Get", URL: "https://execd.example.test/ping", Err: &net.DNSError{Err: "starting", Name: "execd.example.test", IsTemporary: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if !isOpenSandboxReadinessPending(err) {
				t.Fatalf("err=%v should remain retryable during startup", err)
			}
		})
	}
}

func TestNewOpenSandboxClientRequiresExplicitAPIURL(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	_, err := newOpenSandboxClient(testConfig(), Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "trusted API URL") {
		t.Fatalf("err=%v, want trusted API URL requirement", err)
	}
}

func TestSecureOpenSandboxHTTPClientInstallsSDKTransport(t *testing.T) {
	client := secureOpenSandboxHTTPClient(&http.Client{})
	redirectTransport, ok := client.Transport.(openSandboxRedirectTransport)
	if !ok {
		t.Fatalf("transport=%T want redirect guard", client.Transport)
	}
	transport, ok := redirectTransport.base.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatalf("transport=%T want SDK HTTP transport", redirectTransport.base)
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.VerifyConnection == nil {
		t.Fatalf("TLS config=%#v want SDK TLS hardening", transport.TLSClientConfig)
	}
}

func TestSecureOpenSandboxHTTPClientAllowsRedirectsRelativeToRequestOrigin(t *testing.T) {
	var redirected bool
	execd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/ready", http.StatusTemporaryRedirect)
			return
		}
		redirected = r.URL.Path == "/ready"
		w.WriteHeader(http.StatusNoContent)
	}))
	defer execd.Close()

	client := secureOpenSandboxHTTPClient(execd.Client())
	response, err := client.Get(execd.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !redirected {
		t.Fatal("same-origin execd redirect was not followed")
	}
}

func TestSecureOpenSandboxHTTPClientRejectsCrossOriginRedirect(t *testing.T) {
	var destinationHit bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/redirect?signature=secret-value", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := secureOpenSandboxHTTPClient(source.Client())
	request, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-EXECD-ACCESS-TOKEN", "secret")
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "cross-origin redirect") {
		t.Fatalf("err=%v, want cross-origin redirect rejection", err)
	} else if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("redirect error leaked signature: %v", err)
	}
	if destinationHit {
		t.Fatal("cross-origin redirect forwarded the request")
	}
}

func TestSDKClientCreateUsesHeadersAndRequestBody(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var createPath, gotAuth string
	var gotBody struct {
		Image struct {
			URI string `json:"uri"`
		} `json:"image"`
		ResourceLimits map[string]string `json:"resourceLimits"`
		Metadata       map[string]string `json:"metadata"`
		Entrypoint     []string          `json:"entrypoint"`
		Timeout        int               `json:"timeout"`
		Platform       struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"platform"`
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			createPath = r.URL.Path
			gotAuth = r.Header.Get("OPEN-SANDBOX-API-KEY")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-sdk","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-sdk":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-sdk","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-sdk/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image:        "ubuntu:test",
		CPU:          "500m",
		Memory:       "512Mi",
		PlatformOS:   "linux",
		PlatformArch: "amd64",
		Metadata:     map[string]string{openSandboxClaimKey: "scope"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if createPath != "/v1/sandboxes" || gotAuth != "test-key" {
		t.Fatalf("path=%q auth=%q", createPath, gotAuth)
	}
	if gotBody.Image.URI != "ubuntu:test" || gotBody.ResourceLimits["cpu"] != "500m" || gotBody.ResourceLimits["memory"] != "512Mi" || gotBody.Metadata[openSandboxClaimKey] != "scope" || gotBody.Platform.OS != "linux" || gotBody.Platform.Arch != "amd64" {
		t.Fatalf("body=%#v", gotBody)
	}
	if gotBody.Timeout != openSandboxExecTimeoutSecs {
		t.Fatalf("timeout=%d want Crabbox fallback %d", gotBody.Timeout, openSandboxExecTimeoutSecs)
	}
	if strings.Join(gotBody.Entrypoint, "\x00") != strings.Join(sdk.DefaultEntrypoint, "\x00") {
		t.Fatalf("entrypoint=%#v want %#v", gotBody.Entrypoint, sdk.DefaultEntrypoint)
	}
}

func TestSDKClientLifecycleRequestsAreBounded(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sdkClient := client.(*sdkOpenSandboxClient)
	sdkClient.requestTimeoutOverride = 20 * time.Millisecond

	start := time.Now()
	err = client.Probe(context.Background())
	if err == nil {
		t.Fatal("expected stalled lifecycle request to time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("lifecycle timeout took %s, want under 1s", elapsed)
	}
}

func TestSDKClientMarksCreateRequestTimeoutAsAmbiguous(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	client.(*sdkOpenSandboxClient).requestTimeoutOverride = 20 * time.Millisecond

	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	var ambiguous *ambiguousOpenSandboxCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err=%v, want ambiguous create marker", err)
	}
}

func TestSDKClientMarksSuccessfulCreateDecodeFailuresAsAmbiguous(t *testing.T) {
	for name, response := range map[string]string{
		"syntax": "{",
		"type":   "[]",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/sandboxes" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()

			cfg := testConfig()
			cfg.OpenSandbox.APIURL = server.URL
			client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
				Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
			})
			var ambiguous *ambiguousOpenSandboxCreateError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("err=%v, want ambiguous create marker", err)
			}
		})
	}
}

func TestSDKClientMarksSuccessfulCreateWithoutIDAsAmbiguous(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
	}{
		"empty object": {status: http.StatusOK, body: "{}"},
		"null":         {status: http.StatusOK, body: "null"},
		"no content":   {status: http.StatusNoContent},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/sandboxes" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			cfg := testConfig()
			cfg.OpenSandbox.APIURL = server.URL
			client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
				Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
			})
			var ambiguous *ambiguousOpenSandboxCreateError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("err=%v, want ambiguous create marker", err)
			}
		})
	}
}

func TestSDKClientDoesNotDispatchPreCanceledCreate(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	hit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit <- struct{}{}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.CreateSandbox(ctx, createSandboxOptions{
		Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want canceled", err)
	}
	var ambiguous *ambiguousOpenSandboxCreateError
	if errors.As(err, &ambiguous) {
		t.Fatalf("err=%v, pre-dispatch cancellation must not be ambiguous", err)
	}
	select {
	case <-hit:
		t.Fatal("pre-canceled create reached the server")
	default:
	}
}

func TestSDKClientCreateWaitsForRunningAndExecdPing(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	statusPolls := 0
	endpointHits := 0
	pingHits := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-wait","status":{"state":"Pending"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-wait":
			statusPolls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-wait","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-wait/endpoints/44772":
			endpointHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			pingHits++
			if got := r.Header.Get("X-EXECD-ACCESS-TOKEN"); got != "exec-token" {
				t.Errorf("ping auth=%q want exec-token", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.CreateSandbox(context.Background(), createSandboxOptions{
		Image:    "ubuntu:test",
		CPU:      "500m",
		Memory:   "512Mi",
		Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "sb-wait" || info.State != "Running" {
		t.Fatalf("info=%#v", info)
	}
	if statusPolls == 0 {
		t.Fatal("expected create to poll sandbox status until running")
	}
	if endpointHits == 0 || pingHits == 0 {
		t.Fatalf("endpointHits=%d pingHits=%d, want readiness ping", endpointHits, pingHits)
	}
}

func TestSDKClientReadyTimeoutUsesProviderBudget(t *testing.T) {
	client := &sdkOpenSandboxClient{cfg: testConfig()}
	if got := client.readyTimeout(); got != openSandboxReadyTimeout {
		t.Fatalf("ready timeout=%s want %s", got, openSandboxReadyTimeout)
	}
	client.cfg.OpenSandbox.TimeoutSecs = 900
	if got := client.readyTimeout(); got != openSandboxReadyTimeout {
		t.Fatalf("ready timeout=%s want capped %s", got, openSandboxReadyTimeout)
	}
	client.cfg.OpenSandbox.TimeoutSecs = 120
	if got := client.readyTimeout(); got != 2*time.Minute {
		t.Fatalf("ready timeout=%s want effective 2m lifetime", got)
	}
}

func TestSDKClientRunningWaitHonorsDiscoveredExpiration(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	deleted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-expiring","status":{"state":"Pending"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-expiring":
			w.Header().Set("Content-Type", "application/json")
			expiresAt := time.Now().Add(250 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			_, _ = io.WriteString(w, `{"id":"sb-expiring","status":{"state":"Pending"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z","expiresAt":"`+expiresAt+`"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-expiring":
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err == nil || !strings.Contains(err.Error(), "expired before reaching Running") {
		t.Fatalf("err=%v, want discovered expiration failure", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expiration wait took %s", elapsed)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d want rollback", deleted)
	}
}

func TestSDKClientRefreshesMissingCreateExpirationBeforeReadiness(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	getHits := 0
	deleted := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-missing-expiry","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-missing-expiry":
			getHits++
			w.Header().Set("Content-Type", "application/json")
			expiresAt := time.Now().Add(250 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			_, _ = io.WriteString(w, `{"id":"sb-missing-expiry","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z","expiresAt":"`+expiresAt+`"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-missing-expiry/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			http.Error(w, "starting", http.StatusServiceUnavailable)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-missing-expiry":
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err=%v, want readiness expiration", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("missing expiration readiness took %s", elapsed)
	}
	if getHits != 1 || deleted != 1 {
		t.Fatalf("getHits=%d deleted=%d want expiration refresh and rollback", getHits, deleted)
	}
}

func TestSDKClientUsesRefreshedExpirationForSecondRunningWait(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var getHits atomic.Int32
	var deleted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-refresh-pending","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-refresh-pending":
			if getHits.Add(1) > 1 {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			expiresAt := time.Now().Add(250 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			_, _ = io.WriteString(w, `{"id":"sb-refresh-pending","status":{"state":"Pending"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z","expiresAt":"`+expiresAt+`"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-refresh-pending":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err == nil || !strings.Contains(err.Error(), "wait for running after expiration refresh") {
		t.Fatalf("err=%v, want bounded second running wait", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("refreshed expiration wait took %s", elapsed)
	}
	if getHits.Load() != 2 || deleted.Load() != 1 {
		t.Fatalf("getHits=%d deleted=%d want two status requests and rollback", getHits.Load(), deleted.Load())
	}
}

func TestSDKClientCreateDeletesSandboxWhenReadinessFails(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	deleted := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-cleanup","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z","expiresAt":"2099-01-01T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-cleanup/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/ping":
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-cleanup":
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = client.CreateSandbox(ctx, createSandboxOptions{
		Image:    "ubuntu:test",
		CPU:      "500m",
		Memory:   "512Mi",
		Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err == nil || !strings.Contains(err.Error(), "wait until ready") {
		t.Fatalf("err=%v, want readiness failure", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d want 1 cleanup delete", deleted)
	}
}

func TestSDKClientCreateSurfacesPermanentReadinessFailureImmediately(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	deleted := 0
	endpointHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"sb-hard-failure","status":{"state":"Running"},"metadata":{"crabbox.claim":"scope"},"createdAt":"2026-06-11T00:00:00Z","expiresAt":"2099-01-01T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-hard-failure/endpoints/44772":
			endpointHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"http://198.51.100.10:44772","headers":{"X-EXECD-ACCESS-TOKEN":"secret"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-hard-failure":
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.CreateSandbox(context.Background(), createSandboxOptions{
		Image: "ubuntu:test", Metadata: map[string]string{openSandboxClaimKey: "scope"},
	})
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS unless it is loopback") {
		t.Fatalf("err=%v, want permanent readiness failure", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("permanent readiness failure took %s", elapsed)
	}
	if endpointHits != 1 || deleted != 1 {
		t.Fatalf("endpointHits=%d deleted=%d want one probe and rollback", endpointHits, deleted)
	}
}

func TestSDKClientProxyExecdAddsAccessTokenWhenEndpointOmitsIt(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "proxy-key")
	var gotEndpointQuery, gotExecdAuth string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-proxy/endpoints/44772":
			gotEndpointQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			gotExecdAuth = r.Header.Get("X-EXECD-ACCESS-TOKEN")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	cfg.OpenSandbox.UseServerProxy = true
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-proxy", runCommandRequest{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("exit=%d", exitCode)
	}
	if gotEndpointQuery != "use_server_proxy=true" {
		t.Fatalf("endpoint query=%q", gotEndpointQuery)
	}
	if gotExecdAuth != "proxy-key" {
		t.Fatalf("X-EXECD-ACCESS-TOKEN=%q want proxy-key", gotExecdAuth)
	}
}

func TestSDKClientProxyExecdHonorsCaseInsensitiveAccessTokenHeader(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "proxy-key")
	var gotExecdAuth string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-proxy-case/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"x-execd-access-token":"endpoint-key"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			gotExecdAuth = r.Header.Get("X-EXECD-ACCESS-TOKEN")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	cfg.OpenSandbox.UseServerProxy = true
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunCommand(context.Background(), "sb-proxy-case", runCommandRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if gotExecdAuth != "endpoint-key" {
		t.Fatalf("X-EXECD-ACCESS-TOKEN=%q want endpoint-key", gotExecdAuth)
	}
}

func TestSDKClientRejectsPlaintextPublicExecdEndpoint(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-public/endpoints/44772" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"http://198.51.100.10:44772/exec?signature=secret-value","headers":{"X-EXECD-ACCESS-TOKEN":"secret"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RunCommand(context.Background(), "sb-public", runCommandRequest{Command: "true"})
	if err == nil || !strings.Contains(err.Error(), `endpoint host "198.51.100.10:44772" must use HTTPS unless it is loopback`) {
		t.Fatalf("err=%v, want public plaintext endpoint rejection", err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error leaked endpoint signature: %v", err)
	}
}

func TestSDKClientPreservesExecdEndpointQueryAcrossPaths(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var pingQuery, commandQuery string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-signed/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`/exec?signature=secret-value","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/exec/ping":
			pingQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/exec/command":
			commandQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PingSandbox(context.Background(), "sb-signed"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunCommand(context.Background(), "sb-signed", runCommandRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if pingQuery != "signature=secret-value" || commandQuery != "signature=secret-value" {
		t.Fatalf("ping query=%q command query=%q", pingQuery, commandQuery)
	}
}

func TestSDKClientRunCommandSendsTimeoutMillis(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var gotTimeout int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-timeout/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			var body struct {
				Timeout int64 `json:"timeout"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			gotTimeout = body.Timeout
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-timeout", runCommandRequest{
		Command:     "true",
		TimeoutSecs: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("exit=%d", exitCode)
	}
	if gotTimeout != int64(time.Hour/time.Millisecond) {
		t.Fatalf("timeout sent=%d, want %d milliseconds for 3600 seconds", gotTimeout, int64(time.Hour/time.Millisecond))
	}
}

func TestSDKClientRunCommandRejectsPrematureEOF(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-truncated/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"stdout\",\"data\":\"partial\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"result\",\"data\":\"intermediate\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-truncated", runCommandRequest{Command: "true"})
	if err == nil || !strings.Contains(err.Error(), "stream ended before terminal event") {
		t.Fatalf("err=%v, want premature EOF failure", err)
	}
	if exitCode != 1 {
		t.Fatalf("exit=%d want 1", exitCode)
	}
}

func TestSDKClientRunCommandBoundsEndpointDiscovery(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	commandHit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-discovery-stalled/endpoints/44772":
			<-r.Context().Done()
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			commandHit = true
			http.Error(w, "unexpected command", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	client.(*sdkOpenSandboxClient).execTimeoutOverride = 20 * time.Millisecond

	start := time.Now()
	_, err = client.RunCommand(context.Background(), "sb-discovery-stalled", runCommandRequest{
		Command:     "true",
		TimeoutSecs: 3600,
	})
	if err == nil {
		t.Fatal("expected stalled endpoint discovery to time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("endpoint discovery timeout took %s, want under 1s", elapsed)
	}
	if commandHit {
		t.Fatal("command request started after endpoint discovery timed out")
	}
}

func TestSDKClientRunCommandBoundsStreamingRequest(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	var interrupted string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-stalled/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+server.URL+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"init\",\"text\":\"cmd-stalled\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodDelete && r.URL.Path == "/command":
			interrupted = r.URL.Query().Get("id")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	client.(*sdkOpenSandboxClient).execTimeoutOverride = 20 * time.Millisecond

	start := time.Now()
	_, err = client.RunCommand(context.Background(), "sb-stalled", runCommandRequest{
		Command:     "sleep 3600",
		TimeoutSecs: 3600,
	})
	if err == nil {
		t.Fatal("expected stalled streaming request to time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("streaming timeout took %s, want under 1s", elapsed)
	}
	if interrupted != "cmd-stalled" {
		t.Fatalf("interrupted=%q want cmd-stalled", interrupted)
	}
}

func TestSDKClientRunCommandAddsConfiguredSchemeToBareEndpoint(t *testing.T) {
	t.Setenv("CRABBOX_OPENSANDBOX_API_KEY", "test-key")
	commandHit := false
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-bare/endpoints/44772":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"endpoint":"`+strings.TrimPrefix(server.URL, "https://")+`","headers":{"X-EXECD-ACCESS-TOKEN":"exec-token"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/command":
			commandHit = true
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.OpenSandbox.APIURL = server.URL
	client, err := newOpenSandboxClient(cfg, Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	exitCode, err := client.RunCommand(context.Background(), "sb-bare", runCommandRequest{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 || !commandHit {
		t.Fatalf("exit=%d commandHit=%v", exitCode, commandHit)
	}
}

func TestCommandEventErrorDefaultsToFailureExit(t *testing.T) {
	var stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: &stderr}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"error","error":{"evalue":"command failed"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.errorEvent || result.exitCode == nil || *result.exitCode != 1 {
		t.Fatalf("result=%#v, want default failure exit", result)
	}
	if !result.terminal {
		t.Fatalf("result=%#v, want terminal error event", result)
	}
	if !strings.Contains(stderr.String(), "command failed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCommandNumericErrorEventDoesNotWriteExitMetadataToStderr(t *testing.T) {
	var stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: &stderr}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"error","error":{"evalue":"7"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode == nil || *result.exitCode != 7 || !result.terminal {
		t.Fatalf("result=%#v, want terminal exit 7", result)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q, want no numeric exit metadata", stderr.String())
	}
}

func TestCommandNumericPrefixErrorRemainsVisibleDiagnostic(t *testing.T) {
	var stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: &stderr}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"error","error":{"evalue":"7 files failed"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode == nil || *result.exitCode != 1 || !result.terminal {
		t.Fatalf("result=%#v, want default terminal failure", result)
	}
	if stderr.String() != "7 files failed\n" {
		t.Fatalf("stderr=%q, want visible diagnostic", stderr.String())
	}
}

func TestCommandEventHonorsExplicitExitCode(t *testing.T) {
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"execution_complete","exit_code":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode == nil || *result.exitCode != 42 {
		t.Fatalf("result=%#v, want exit 42", result)
	}
	if !result.terminal {
		t.Fatalf("result=%#v, want terminal completion event", result)
	}
}

func TestCommandEventCapturesExecutionID(t *testing.T) {
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"init","text":"cmd-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.executionID != "cmd-123" || result.terminal {
		t.Fatalf("result=%#v want execution ID", result)
	}
}

func TestCommandResultWithoutExitCodeIsNotTerminal(t *testing.T) {
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	result, err := client.handleCommandEvent(streamEvent(`{"type":"result","data":"intermediate"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.terminal {
		t.Fatalf("result=%#v, want nonterminal result without exit code", result)
	}
}

func TestCommandEventRoutesRawStderrEvent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(commandStreamEvent{Event: "stderr", Data: "warn"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "" || stderr.String() != "warn" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventPreservesJSONLookingRawStdoutStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	stdoutPayload := `{"type":"stdout","exit_code":0}`
	stderrPayload := `{"type":"stderr","exit_code":0}`
	if result, err := client.handleCommandEvent(commandStreamEvent{Event: "stdout", Data: stdoutPayload}); err != nil {
		t.Fatal(err)
	} else if result.terminal || result.errorEvent {
		t.Fatalf("stdout result=%#v, want raw nonterminal output", result)
	}
	if result, err := client.handleCommandEvent(commandStreamEvent{Event: "stderr", Data: stderrPayload}); err != nil {
		t.Fatal(err)
	} else if result.terminal || result.errorEvent {
		t.Fatalf("stderr result=%#v, want raw nonterminal output", result)
	}
	if stdout.String() != stdoutPayload || stderr.String() != stderrPayload {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventUsesStructuredJSONWhenTypePresent(t *testing.T) {
	var stdout bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: io.Discard}}
	if _, err := client.handleCommandEvent(commandStreamEvent{Event: "stdout", Data: `{"type":"stdout","text":"hello"}`, Structured: true}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandEventUsesDataFieldForOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(streamEvent(`{"type":"stdout","data":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.handleCommandEvent(streamEvent(`{"type":"stderr","data":"warn"}`)); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" || stderr.String() != "warn" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventReconstructsStructuredOutputLines(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		streamEvent(`{"type":"stdout","text":"line1"}`),
		streamEvent(`{"type":"stderr","text":"warn1"}`),
		streamEvent(`{"type":"stdout","text":"line2"}`),
		streamEvent(`{"type":"stderr","text":"warn2"}`),
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "line1\nline2" || stderr.String() != "warn1\nwarn2" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventDoesNotDuplicateStructuredLineTerminators(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		streamEvent("{\"type\":\"stdout\",\"text\":\"line1\\n\"}"),
		streamEvent("{\"type\":\"stderr\",\"text\":\"warn1\\n\"}"),
		streamEvent("{\"type\":\"stdout\",\"text\":\"line2\\n\"}"),
		streamEvent("{\"type\":\"stderr\",\"text\":\"warn2\\n\"}"),
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "line1\nline2\n" || stderr.String() != "warn1\nwarn2\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventReconstructsRawSSEOutputLines(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		{Event: "stdout", Data: "line1"},
		{Event: "stderr", Data: "warn1"},
		{Event: "stdout", Data: "line2"},
		{Event: "stderr", Data: "warn2"},
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "line1\nline2" || stderr.String() != "warn1\nwarn2" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventDoesNotDuplicateRawSSELineTerminators(t *testing.T) {
	var stdout bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: io.Discard}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		{Event: "stdout", Data: "line1\n"},
		{Event: "stdout", Data: "line2\n"},
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "line1\nline2\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandEventPreservesOutputWhitespace(t *testing.T) {
	var stdout, stderr bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
	if _, err := client.handleCommandEvent(streamEvent("{\"type\":\"stdout\",\"text\":\"  ok\\n\"}")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.handleCommandEvent(streamEvent("{\"type\":\"stderr\",\"data\":\"\\n\"}")); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "  ok\n" || stderr.String() != "\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCommandEventPreservesRawSSEWhitespaceChunks(t *testing.T) {
	var stdout bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: io.Discard}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		{Event: "stdout", Data: ""},
		{Event: "stdout", Data: "  "},
		{Event: "stdout", Data: "done"},
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "\n  \ndone" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandEventPreservesTrailingBlankOutputRecord(t *testing.T) {
	var stdout bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: io.Discard}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		streamEvent("{\"type\":\"stdout\",\"text\":\"line1\\n\"}"),
		streamEvent(`{"type":"stdout","text":""}`),
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "line1\n\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandEventPreservesLineStateAcrossResultOutput(t *testing.T) {
	var stdout bytes.Buffer
	client := &sdkOpenSandboxClient{rt: Runtime{Stdout: &stdout, Stderr: io.Discard}}
	state := commandOutputState{}
	for _, event := range []commandStreamEvent{
		streamEvent("{\"type\":\"stdout\",\"text\":\"a\\n\"}"),
		streamEvent(`{"type":"result","text":"b"}`),
		streamEvent(`{"type":"stdout","text":"c"}`),
	} {
		if _, err := client.handleCommandEventWithState(event, &state); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "a\nb\nc" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCommandStreamTracksWireFraming(t *testing.T) {
	var events []commandStreamEvent
	input := "event: stdout\ndata: {\"type\":\"stdout\",\"text\":\"raw-json\"}\n\n" +
		"{\"type\":\"stderr\",\"data\":\"structured\"}\n\n"
	if err := streamOpenSandboxCommand(context.Background(), strings.NewReader(input), func(event commandStreamEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Structured || !events[1].Structured {
		t.Fatalf("events=%#v, want explicit SSE then structured NDJSON", events)
	}
}

func streamEvent(data string) commandStreamEvent {
	return commandStreamEvent{Data: data, Structured: true}
}

func newTestBackend(fake *fakeOpenSandboxClient) *openSandboxBackend {
	cfg := testConfig()
	return &openSandboxBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: &bytes.Buffer{},
			Stderr: &bytes.Buffer{},
		},
		newClient: func(Config, Runtime) (openSandboxClient, error) {
			return fake, nil
		},
		cleanupTimeoutOverride: 10 * time.Millisecond,
	}
}

func testConfig() Config {
	cfg := Config{}
	cfg.Provider = providerName
	cfg.OpenSandbox.Image = "ubuntu:24.04"
	cfg.OpenSandbox.Workdir = "/workspace/crabbox"
	cfg.OpenSandbox.CPU = "1"
	cfg.OpenSandbox.Memory = "2Gi"
	cfg.OpenSandbox.ExecTimeoutSecs = 600
	cfg.OpenSandbox.PlatformOS = "linux"
	cfg.OpenSandbox.PlatformArch = "amd64"
	cfg.TTL = 90 * time.Minute
	cfg.IdleTimeout = 30 * time.Minute
	cfg.Sync.Timeout = 15 * time.Minute
	return cfg
}

func tempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(path.Join(dir, "go.mod"), []byte("module example.test/my-app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testOpenSandboxScope(t *testing.T, baseURL string) string {
	t.Helper()
	scope, err := newOpenSandboxClaimScope(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

type fakeOpenSandboxClient struct {
	baseURL              string
	sandbox              sandboxInfo
	created              createSandboxOptions
	runs                 []runCommandRequest
	deleted              []string
	resumed              []string
	uploads              []string
	runExit              int
	runErr               error
	createErr            error
	notFound             bool
	pingCalls            int
	pingFailures         int
	pingWaitForCancel    bool
	pingErr              error
	omitCreateExpiration bool
	getCalls             int
	createExpiresIn      time.Duration
	deleteStarted        chan struct{}
	deleteRelease        chan struct{}
	deleteErr            error
	deleteErrCount       int
	listFilters          []map[string]string
	listEmptyCount       int
	listErr              error
	listErrCount         int
	afterResume          func()
	afterRun             func(runCommandRequest)
	runStarted           chan struct{}
	runRelease           chan struct{}
}

func newFakeClient() *fakeOpenSandboxClient {
	expiresAt := time.Now().Add(30 * time.Minute)
	return &fakeOpenSandboxClient{
		baseURL: "https://opensandbox.example.test",
		sandbox: sandboxInfo{
			ID:    "sb-test123",
			State: "Running",
			Metadata: map[string]string{
				openSandboxClaimKey: "pending",
			},
			ExpiresAt: &expiresAt,
		},
	}
}

func (f *fakeOpenSandboxClient) BaseURL() string { return f.baseURL }

func (f *fakeOpenSandboxClient) CreateSandbox(_ context.Context, req createSandboxOptions) (sandboxInfo, error) {
	f.created = req
	f.sandbox.Metadata = cloneStringMap(req.Metadata)
	expiresIn := time.Duration(req.TimeoutSecs) * time.Second
	if f.createExpiresIn > 0 {
		expiresIn = f.createExpiresIn
	}
	expiresAt := time.Now().Add(expiresIn)
	f.sandbox.ExpiresAt = &expiresAt
	created := f.sandbox
	if f.omitCreateExpiration {
		created.ExpiresAt = nil
	}
	if f.createErr != nil {
		return sandboxInfo{}, f.createErr
	}
	return created, nil
}

func (f *fakeOpenSandboxClient) ListSandboxes(_ context.Context, metadata map[string]string) ([]sandboxInfo, error) {
	f.listFilters = append(f.listFilters, cloneStringMap(metadata))
	if f.listErr != nil {
		if f.listErrCount > 0 {
			f.listErrCount--
			err := f.listErr
			if f.listErrCount == 0 {
				f.listErr = nil
			}
			return nil, err
		}
		return nil, f.listErr
	}
	if f.listEmptyCount > 0 {
		f.listEmptyCount--
		return nil, nil
	}
	return []sandboxInfo{f.sandbox}, nil
}

func (f *fakeOpenSandboxClient) GetSandbox(context.Context, string) (sandboxInfo, error) {
	f.getCalls++
	if f.notFound {
		return sandboxInfo{}, errOpenSandboxNotFound
	}
	return f.sandbox, nil
}

func (f *fakeOpenSandboxClient) DeleteSandbox(_ context.Context, sandboxID string) error {
	f.deleted = append(f.deleted, sandboxID)
	if f.deleteStarted != nil {
		select {
		case f.deleteStarted <- struct{}{}:
		default:
		}
	}
	if f.deleteRelease != nil {
		<-f.deleteRelease
	}
	if f.deleteErr != nil {
		if f.deleteErrCount > 0 {
			f.deleteErrCount--
			err := f.deleteErr
			if f.deleteErrCount == 0 {
				f.deleteErr = nil
			}
			return err
		}
		return f.deleteErr
	}
	if f.notFound {
		return errOpenSandboxNotFound
	}
	return nil
}

func (f *fakeOpenSandboxClient) ResumeSandbox(_ context.Context, sandboxID string) error {
	f.resumed = append(f.resumed, sandboxID)
	f.sandbox.State = "Running"
	if f.afterResume != nil {
		f.afterResume()
	}
	return nil
}

func (f *fakeOpenSandboxClient) PingSandbox(ctx context.Context, _ string) error {
	f.pingCalls++
	if f.pingWaitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.pingErr != nil {
		return f.pingErr
	}
	if f.pingFailures > 0 {
		f.pingFailures--
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeOpenSandboxClient) UploadFile(_ context.Context, _ string, remotePath string, _ io.Reader) error {
	f.uploads = append(f.uploads, remotePath)
	return nil
}

func (f *fakeOpenSandboxClient) RunCommand(_ context.Context, _ string, req runCommandRequest) (int, error) {
	f.runs = append(f.runs, req)
	if req.Workdir != "" && f.runStarted != nil {
		select {
		case f.runStarted <- struct{}{}:
		default:
		}
	}
	if req.Workdir != "" && f.runRelease != nil {
		<-f.runRelease
	}
	if f.afterRun != nil {
		f.afterRun(req)
	}
	return f.runExit, f.runErr
}

func (f *fakeOpenSandboxClient) Probe(context.Context) error { return nil }
