package unikraftcloud

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestReconcileReadyClaimWriteReallocatesMissingClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "aaaaaaaaaaaa"
	repoRoot := t.TempDir()
	createReq := createInstanceRequest{
		Name:      leaseProviderName(leaseID, ""),
		Image:     b.cfg.UnikraftCloud.Image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	preflight, err := b.createIntentClaim(leaseID, "reused-slug", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: repoRoot}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeLeaseClaimIfUnchanged(intent.LeaseID, intent); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond("cbx_new_slug_owner", intent.Slug, "external", "", "", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	instance := ukcInstance{UUID: testInstanceUUID, Name: createReq.Name, State: "running"}
	recovered, err := b.reconcileReadyClaimWrite(intent, instance, errors.New("simulated lost ready claim"))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Slug == intent.Slug || !strings.HasPrefix(recovered.Slug, intent.Slug+"-") {
		t.Fatalf("recovered slug=%q want collision suffix for %q", recovered.Slug, intent.Slug)
	}
	if recovered.Labels["slug"] != recovered.Slug {
		t.Fatalf("recovered labels=%#v slug=%q", recovered.Labels, recovered.Slug)
	}
}

func TestReconcileReadyClaimWriteRejectsChangedRecoveryIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "bbbbbbbbbbbb"
	createReq := createInstanceRequest{
		Name:      leaseProviderName(leaseID, ""),
		Image:     b.cfg.UnikraftCloud.Image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	preflight, err := b.createIntentClaim(leaseID, "identity-check", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		t.Fatal(err)
	}
	instance := ukcInstance{UUID: testInstanceUUID, Name: createReq.Name, State: "running"}
	ready, err := b.publishReadyClaim(intent, instance)
	if err != nil {
		t.Fatal(err)
	}
	changed := ready
	changed.Labels = cloneLabels(ready.Labels)
	changed.Labels[ukcLabelRequestHash] = strings.Repeat("0", 64)
	if err := core.ReplaceLeaseClaimIfUnchanged(ready.LeaseID, ready, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reconcileReadyClaimWrite(intent, instance, errors.New("simulated ready write error")); err == nil || !strings.Contains(err.Error(), "changed recovery identity") {
		t.Fatalf("reconcile error=%v, want recovery identity refusal", err)
	}
	stored, exists, err := readLeaseClaimWithPresence(ready.LeaseID)
	if err != nil || !exists || stored.Labels[ukcLabelRequestHash] != changed.Labels[ukcLabelRequestHash] {
		t.Fatalf("stored=%#v exists=%v err=%v, want changed claim retained", stored, exists, err)
	}
}

func TestDiscardUnmutatedCreateClaimRemovesVisibleIntent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "cccccccccccc"
	createReq := createInstanceRequest{
		Name:      leaseProviderName(leaseID, ""),
		Image:     b.cfg.UnikraftCloud.Image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	preflight, err := b.createIntentClaim(leaseID, "discard-intent", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("simulated transition durability error")
	if err := discardUnmutatedUnikraftCloudCreateClaim(preflight, cause); !errors.Is(err, cause) {
		t.Fatalf("discard error=%v want %v", err, cause)
	}
	if stored, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("stored=%#v exists=%v err=%v, want no adoptable claim", stored, exists, err)
	}
}

func TestQuarantineRejectedCreateClaimNeverRemovesVisibleIntent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "dddddddddddd"
	createReq := createInstanceRequest{
		Name:      leaseProviderName(leaseID, ""),
		Image:     b.cfg.UnikraftCloud.Image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	preflight, err := b.createIntentClaim(leaseID, "quarantine-intent", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("simulated rejected-create transition error")
	if err := quarantineRejectedUnikraftCloudCreateClaim(intent, cause); !errors.Is(err, cause) {
		t.Fatalf("quarantine error=%v want %v", err, cause)
	}
	stored, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || stored.Labels["state"] != ukcStateCreateConflict || stored.CloudID != "" {
		t.Fatalf("stored=%#v exists=%v err=%v, want non-adoptable conflict", stored, exists, err)
	}
}

func TestCreateStateTransitionReconcilesErrorAfterRename(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "eeeeeeeeeeee"
	createReq := createInstanceRequest{Name: leaseProviderName(leaseID, ""), Image: b.cfg.UnikraftCloud.Image, MemoryMB: b.cfg.UnikraftCloud.MemoryMB, Autostart: true}
	preflight, err := b.createIntentClaim(leaseID, "rename-reconcile", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	originalReplace := replaceLeaseClaimIfUnchangedDurable
	t.Cleanup(func() { replaceLeaseClaimIfUnchangedDurable = originalReplace })
	injected := errors.New("sync failed after rename")
	calls := 0
	replaceLeaseClaimIfUnchangedDurable = func(leaseID string, current, replacement LeaseClaim) (LeaseClaim, error) {
		written, err := originalReplace(leaseID, current, replacement)
		if err != nil {
			return LeaseClaim{}, err
		}
		calls++
		if calls == 1 {
			return written, injected
		}
		return written, nil
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || intent.Labels["state"] != ukcStateCreateIntent {
		t.Fatalf("calls=%d intent=%#v", calls, intent)
	}
}

func TestCreateStateTransitionDoesNotReconcileGuardConflict(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "edededededed"
	createReq := createInstanceRequest{Name: leaseProviderName(leaseID, ""), Image: b.cfg.UnikraftCloud.Image, MemoryMB: b.cfg.UnikraftCloud.MemoryMB, Autostart: true}
	preflight, err := b.createIntentClaim(leaseID, "guard-conflict", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := preflight
	concurrent.Labels = cloneLabels(preflight.Labels)
	concurrent.Labels["state"] = ukcStateCreateIntent
	concurrent, err = replaceLeaseClaimIfUnchangedDurable(leaseID, preflight, concurrent)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("stale transition error=%v, want claim changed", err)
	}
	stored, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || !reflect.DeepEqual(stored, concurrent) {
		t.Fatalf("stored=%#v exists=%v err=%v, want concurrent=%#v", stored, exists, err, concurrent)
	}
}

func TestInitialPreflightWriteErrorAfterRenameRemovesUnusedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}, nil, nil)
	leaseID := leasePrefix + "abababababab"
	createReq := createInstanceRequest{Name: leaseProviderName(leaseID, ""), Image: b.cfg.UnikraftCloud.Image, MemoryMB: b.cfg.UnikraftCloud.MemoryMB, Autostart: true}
	originalClaim := claimLeaseTargetForRepoConfigScopeIfUnchangedDurable
	t.Cleanup(func() { claimLeaseTargetForRepoConfigScopeIfUnchangedDurable = originalClaim })
	injected := errors.New("preflight sync failed after rename")
	claimLeaseTargetForRepoConfigScopeIfUnchangedDurable = func(leaseID, slug string, cfg Config, providerScope string, server Server, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
		claim, err := originalClaim(leaseID, slug, cfg, providerScope, server, repoRoot, idleTimeout, reclaim, expected, expectedExists)
		if err != nil {
			return claim, err
		}
		return claim, injected
	}
	if _, err := b.createIntentClaim(leaseID, "initial-preflight", testClaimScope(t, "https://api.fra.unikraft.cloud"), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq); err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("createIntentClaim error=%v want %v", err, injected)
	}
	if stored, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("stored=%#v exists=%v err=%v, want unused preflight removed", stored, exists, err)
	}
}

func TestPreflightTransitionFailureDoesNotLeaveAdoptableIntent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	b := testBackend(api, nil, nil)
	leaseID := leasePrefix + "ffffffffffff"
	createReq := createInstanceRequest{Name: leaseProviderName(leaseID, ""), Image: b.cfg.UnikraftCloud.Image, MemoryMB: b.cfg.UnikraftCloud.MemoryMB, Autostart: true}
	preflight, err := b.createIntentClaim(leaseID, "failed-arm", testClaimScope(t, api.BaseURL()), testUserUUID, WarmupRequest{Repo: Repo{Root: t.TempDir()}}, createReq)
	if err != nil {
		t.Fatal(err)
	}
	originalReplace := replaceLeaseClaimIfUnchangedDurable
	t.Cleanup(func() { replaceLeaseClaimIfUnchangedDurable = originalReplace })
	injected := errors.New("persistent sync failure after rename")
	replaceLeaseClaimIfUnchangedDurable = func(leaseID string, current, replacement LeaseClaim) (LeaseClaim, error) {
		written, err := originalReplace(leaseID, current, replacement)
		if err != nil {
			return LeaseClaim{}, err
		}
		if replacement.Labels["state"] == ukcStateCreateIntent {
			return written, injected
		}
		return written, nil
	}
	if _, err := b.preflightCreateIntent(context.Background(), api, preflight); err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("preflight error=%v want %v", err, injected)
	}
	if stored, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("stored=%#v exists=%v err=%v, want no adoptable intent", stored, exists, err)
	}
	if len(api.created) != 0 {
		t.Fatalf("create calls=%#v, want none", api.created)
	}
}

func TestRejectedCreateTransitionFailureRetainsNonAdoptableClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:   "https://api.fra.unikraft.cloud",
		createErr: &unikraftCloudAPIError{StatusCode: http.StatusConflict, Message: "name already exists"},
	}
	b := testBackend(api, nil, nil)
	originalReplace := replaceLeaseClaimIfUnchangedDurable
	t.Cleanup(func() { replaceLeaseClaimIfUnchangedDurable = originalReplace })
	injected := errors.New("persistent conflict sync failure after rename")
	replaceLeaseClaimIfUnchangedDurable = func(leaseID string, current, replacement LeaseClaim) (LeaseClaim, error) {
		written, err := originalReplace(leaseID, current, replacement)
		if err != nil {
			return LeaseClaim{}, err
		}
		if replacement.Labels["state"] == ukcStateCreateConflict {
			return written, injected
		}
		return written, nil
	}
	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("Warmup error=%v want %v", err, injected)
	}
	claim := onlyTestClaim(t)
	if claim.CloudID != "" || claim.Labels["state"] != ukcStateCreateConflict {
		t.Fatalf("claim=%#v, want non-adoptable conflict", claim)
	}
}
