package githubcodespaces

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoveryNonceUsesCryptoEntropy(t *testing.T) {
	first, err := newGitHubCodespacesRecoveryNonce()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newGitHubCodespacesRecoveryNonce()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(first)
	if err != nil || len(decoded) != 16 || first == second {
		t.Fatalf("first=%q second=%q decoded=%d err=%v", first, second, len(decoded), err)
	}
}

func TestAcquireFailsBeforeCreateWhenRecoveryEntropyFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	b.newRecoveryNonce = func() (string, error) { return "", errors.New("entropy unavailable") }
	leaseID := "cbx_123456789b05"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "entropy-box",
	})
	if err == nil || !strings.Contains(err.Error(), "entropy unavailable") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.creates) != 0 {
		t.Fatalf("create called after entropy failure: %#v", fc.creates)
	}
	if _, ok, claimErr := readLeaseClaimWithPresence(leaseID); claimErr != nil || ok {
		t.Fatalf("claim persisted ok=%t err=%v", ok, claimErr)
	}
}

func TestAcquireRejectsPreExistingRecoveryIdentityBeforeCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789b01"
	slug := "preexisting-box"
	nonce := "0123456789abcdef0123456789abcdef"
	b.newRecoveryNonce = func() (string, error) { return nonce, nil }

	item := fakeCodespace("cs-preexisting", "Available")
	item.DisplayName = githubCodespacesDisplayName(leaseID, slug, nonce)
	item.Repository.FullName = "example-org/my-app"
	fc.items[item.Name] = item

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    slug,
	})
	if err == nil || (!strings.Contains(err.Error(), "pre-existing") && !strings.Contains(err.Error(), "already exists")) {
		t.Fatalf("err=%v", err)
	}
	if len(fc.creates) != 0 {
		t.Fatalf("create called for pre-existing recovery identity: %#v", fc.creates)
	}
	if _, ok, claimErr := readLeaseClaimWithPresence(leaseID); claimErr != nil || ok {
		t.Fatalf("claim persisted ok=%t err=%v", ok, claimErr)
	}
	if len(fc.starts) != 0 || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("unexpected mutations starts=%#v stops=%#v deletes=%#v", fc.starts, fc.stops, fc.deletes)
	}
}

func TestResolveRetainsLegacyPendingClaimWithoutRecoveryNonce(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789b02"
	before, displayName := persistPendingRecoveryClaimForTest(t, b, leaseID, "legacy-box", "legacy-display-nonce", false)

	item := fakeCodespace("cs-legacy-match", "Available")
	item.DisplayName = displayName
	item.Repository.FullName = "example-org/my-app"
	fc.items[item.Name] = item

	_, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "manual") || !strings.Contains(err.Error(), "claim retained") {
		t.Fatalf("err=%v", err)
	}
	after, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, claimErr)
	}
	if !reflect.DeepEqual(after, before) || after.CloudID != "" {
		t.Fatalf("legacy claim mutated\nbefore=%#v\nafter=%#v", before, after)
	}
	assertNoRecoverySideEffects(t, stateHome, leaseID, fc, fg)
}

func TestResolveNoLocalStateMutationsMatchesPendingRecoveryWithoutPersistence(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789b03"
	before, displayName := persistPendingRecoveryClaimForTest(t, b, leaseID, "readonly-pending-box", "readonly-recovery-nonce", true)

	item := fakeCodespace("cs-readonly-pending", "Available")
	item.DisplayName = displayName
	item.Repository.FullName = "Example-Org/My-App"
	fc.items[item.Name] = item

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != item.Name || lease.SSH.Host != "cs."+item.Name+".main" {
		t.Fatalf("lease=%#v", lease)
	}
	after, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, claimErr)
	}
	if !reflect.DeepEqual(after, before) || after.CloudID != "" {
		t.Fatalf("read-only resolve mutated claim\nbefore=%#v\nafter=%#v", before, after)
	}
	if len(fc.starts) != 0 || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("unexpected remote mutations starts=%#v stops=%#v deletes=%#v", fc.starts, fc.stops, fc.deletes)
	}
	if fg.configFor != item.Name {
		t.Fatalf("ssh config requested for %q", fg.configFor)
	}
	assertNoStoredRecoverySSHConfig(t, stateHome, leaseID)
}

func TestResolvePendingRecoveryRejectsAmbiguousNonceMatches(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789b04"
	before, displayName := persistPendingRecoveryClaimForTest(t, b, leaseID, "ambiguous-pending-box", "ambiguous-recovery-nonce", true)

	for _, name := range []string{"cs-ambiguous-one", "cs-ambiguous-two"} {
		item := fakeCodespace(name, "Available")
		item.DisplayName = displayName
		item.Repository.FullName = "example-org/my-app"
		fc.items[name] = item
	}

	_, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, NoLocalStateMutations: true})
	if err == nil || (!strings.Contains(err.Error(), "multiple") && !strings.Contains(err.Error(), "ambiguous")) {
		t.Fatalf("err=%v", err)
	}
	after, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, claimErr)
	}
	if !reflect.DeepEqual(after, before) || after.CloudID != "" {
		t.Fatalf("ambiguous recovery mutated claim\nbefore=%#v\nafter=%#v", before, after)
	}
	assertNoRecoverySideEffects(t, stateHome, leaseID, fc, fg)
}

func TestPendingRecoveryClaimReservesSlugForAllocation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newTestBackend(t, newFakeCodespacesClient(), &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	persistPendingRecoveryClaimForTest(t, b, "cbx_123456789ab6", "pending-reservation", "pending-reservation-nonce", true)

	slug, err := allocateDirectLeaseSlug("cbx_123456789ab7", "pending-reservation", nil)
	if err != nil {
		t.Fatal(err)
	}
	if slug == "pending-reservation" || !strings.HasPrefix(slug, "pending-reservation-") {
		t.Fatalf("pending claim did not reserve slug: %q", slug)
	}
}

func persistPendingRecoveryClaimForTest(t *testing.T, b *testBackend, leaseID, slug, nonce string, persistNonce bool) (LeaseClaim, string) {
	t.Helper()
	const (
		repo  = "example-org/my-app"
		login = "alice"
	)
	displayName := githubCodespacesDisplayName(leaseID, slug, nonce)
	labels := b.labelsFor(leaseID, slug, repo, login, false, releaseDelete, codespace{}, "provisioning", fakeGitHubUser(login))
	delete(labels, labelCodespaceName)
	delete(labels, labelCodespaceID)
	delete(labels, labelEnvironmentID)
	delete(labels, labelOwnerID)
	labels[labelDisplayName] = displayName
	labels[labelRecovery] = recoveryPreCreate
	if persistNonce {
		labels[labelRecoveryNonce] = nonce
	}
	server := Server{
		Provider: providerName,
		Name:     displayName,
		Status:   "provisioning",
		Labels:   labels,
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, b.claimConfig(repo), server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	return claim, displayName
}

func assertNoRecoverySideEffects(t *testing.T, stateHome, leaseID string, fc *fakeCodespacesClient, fg *fakeGH) {
	t.Helper()
	if len(fc.starts) != 0 || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("unexpected remote mutations starts=%#v stops=%#v deletes=%#v", fc.starts, fc.stops, fc.deletes)
	}
	if fg.configFor != "" {
		t.Fatalf("unexpected SSH config request for %q", fg.configFor)
	}
	assertNoStoredRecoverySSHConfig(t, stateHome, leaseID)
}

func assertNoStoredRecoverySSHConfig(t *testing.T, stateHome, leaseID string) {
	t.Helper()
	path := filepath.Join(stateHome, "crabbox", "github-codespaces", leaseID+".ssh_config")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stored SSH config err=%v path=%s", err, path)
	}
}
