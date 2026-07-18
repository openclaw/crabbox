package githubcodespaces

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSafetyResolveRejectsDuplicateClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	first := fakeCodespace("cs-duplicate-slug-a", "Available")
	second := fakeCodespace("cs-duplicate-slug-b", "Available")
	fc.items[first.Name] = first
	fc.items[second.Name] = second
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	mustCreateSafetyClaim(t, b, first, "cbx_347000000008", "shared-slug", releaseDelete, "ready", time.Now().Add(time.Hour), SSHTarget{})
	mustCreateSafetyClaim(t, b, second, "cbx_347000000009", "shared-slug", releaseDelete, "ready", time.Now().Add(time.Hour), SSHTarget{})

	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: "shared-slug"}); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("duplicate slug resolve err=%v", err)
	}
	if len(fc.starts) != 0 || fg.configFor != "" {
		t.Fatalf("ambiguous resolve mutated provider: starts=%#v config=%q", fc.starts, fg.configFor)
	}
}

func TestSafetyResolveCanonicalIDNeverFallsBackToSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-canonical-alias", "Available")
	fc.items[item.Name] = item
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	mustCreateSafetyClaim(t, b, item, "cbx_34700000000a", "cbx-aaaaaaaaaaaa", releaseDelete, "ready", time.Now().Add(time.Hour), SSHTarget{})

	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: "cbx_aaaaaaaaaaaa"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("canonical miss resolve err=%v", err)
	}
	if len(fc.starts) != 0 || fg.configFor != "" {
		t.Fatalf("canonical miss mutated provider: starts=%#v config=%q", fc.starts, fg.configFor)
	}
}

func TestSafetyResolveRevalidatesIdentityBeforeMutationAndSSH(t *testing.T) {
	for _, test := range []struct {
		name           string
		initialState   string
		sequence       func(codespace) []codespace
		wantStartCount int
	}{
		{
			name:         "get replacement before start",
			initialState: "Available",
			sequence: func(item codespace) []codespace {
				replacement := item
				replacement.EnvironmentID = "env-foreign-get"
				return []codespace{replacement}
			},
		},
		{
			name:           "replacement while waiting after start",
			initialState:   "Shutdown",
			wantStartCount: 1,
			sequence: func(item codespace) []codespace {
				beforeStart := item
				replacement := item
				replacement.State = "Available"
				replacement.EnvironmentID = "env-foreign-wait"
				return []codespace{beforeStart, replacement}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fc := newFakeCodespacesClient()
			item := fakeCodespace("cs-resolve-replacement", test.initialState)
			fc.items[item.Name] = item
			fc.getSeq[item.Name] = test.sequence(item)
			fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
			b := newTestBackend(t, fc, fg)
			leaseID := "cbx_34700000000e"
			mustCreateSafetyClaim(t, b, item, leaseID, "resolve-replacement", releaseDelete, "ready", time.Now().Add(time.Hour), SSHTarget{})

			if _, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID}); err == nil || !strings.Contains(err.Error(), "environment id changed") {
				t.Fatalf("resolve err=%v", err)
			}
			if len(fc.starts) != test.wantStartCount || fg.configFor != "" {
				t.Fatalf("starts=%#v config=%q", fc.starts, fg.configFor)
			}
			claim, ok, err := readLeaseClaimWithPresence(leaseID)
			if err != nil || !ok || claim.Labels[labelState] != "ready" || claim.Labels[labelEnvironmentID] != item.EnvironmentID {
				t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
			}
		})
	}
}

func TestSafetyAcquireCarriesFinalClaimSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{fakeCodespace("cs-1", "Available")}
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_34700000000b",
		RequestedSlug:    "snapshot-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := serverLeaseClaimSnapshot(lease.Server)
	persisted, ok, err := readLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if !set || !exists || !reflect.DeepEqual(snapshot, persisted) {
		t.Fatalf("snapshot set=%t exists=%t\nsnapshot=%#v\npersisted=%#v", set, exists, snapshot, persisted)
	}
}

func TestSafetyCleanupRejectsDuplicateCloudIDClaimsBeforeMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-duplicate-claim", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	expiresAt := time.Now().Add(-time.Hour)

	mustCreateSafetyClaim(t, b, item, "cbx_347000000001", "duplicate-a", releaseDelete, "ready", expiresAt, SSHTarget{})
	mustCreateSafetyClaim(t, b, item, "cbx_347000000002", "duplicate-b", releaseDelete, "ready", expiresAt, SSHTarget{})

	if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil {
		t.Fatal("cleanup accepted two claims bound to the same Codespace")
	}
	if len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("ambiguous claims mutated Codespace: stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
}

func TestSafetyCleanupRejectsDuplicateLiveCodespaceNamesBeforeMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-duplicate-live", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	mustCreateSafetyClaim(t, b, item, "cbx_347000000003", "duplicate-live", releaseDelete, "ready", time.Now().Add(-time.Hour), SSHTarget{})

	duplicateInventory := &duplicateCodespacesInventoryClient{
		fakeCodespacesClient: fc,
		inventory:            []codespace{item, item},
	}
	b.clientFactory = func(string) codespacesAPI { return duplicateInventory }

	if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil {
		t.Fatal("cleanup accepted duplicate Codespace names in live inventory")
	}
	if len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("ambiguous inventory mutated Codespace: stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
}

func TestSafetyClaimResourceRequiresPermanentIdentity(t *testing.T) {
	item := fakeCodespace("cs-permanent-identity", "Available")
	b := newTestBackend(t, newFakeCodespacesClient(), &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	baseClaim := LeaseClaim{
		LeaseID: "cbx_123456789ab3",
		CloudID: item.Name,
		Labels:  b.labelsFor("cbx_123456789ab3", "identity-box", "example-org/my-app", "alice", false, releaseDelete, item, "ready"),
	}
	for _, test := range []struct {
		name        string
		mutateClaim func(*LeaseClaim)
		mutateLive  func(*codespace)
		want        string
	}{
		{name: "codespace id changed", mutateLive: func(live *codespace) { live.ID++ }, want: "codespace id changed"},
		{name: "environment id changed", mutateLive: func(live *codespace) { live.EnvironmentID = "env-other" }, want: "environment id changed"},
		{name: "owner id changed", mutateLive: func(live *codespace) { live.Owner.ID++ }, want: "owner id changed"},
		{name: "repository id changed", mutateLive: func(live *codespace) { live.Repository.ID++ }, want: "repository id changed"},
		{name: "missing claim id", mutateClaim: func(claim *LeaseClaim) { delete(claim.Labels, labelCodespaceID) }, want: "without complete codespace id identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := baseClaim
			claim.Labels = cloneLabels(baseClaim.Labels)
			live := item
			if test.mutateClaim != nil {
				test.mutateClaim(&claim)
			}
			if test.mutateLive != nil {
				test.mutateLive(&live)
			}
			err := validateCodespaceClaimResource(claim, live)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSafetyClaimResourceAllowsRepositoryRenameWithStableID(t *testing.T) {
	item := fakeCodespace("cs-repository-renamed", "Available")
	b := newTestBackend(t, newFakeCodespacesClient(), &fakeGH{login: "alice", token: "test" + "-value"})
	claim := LeaseClaim{
		LeaseID: "cbx_123456789af5",
		CloudID: item.Name,
		Labels:  b.labelsFor("cbx_123456789af5", "repository-renamed", "example-org/my-app", "alice", false, releaseDelete, item, "ready"),
	}
	item.Repository.FullName = "renamed-org/renamed-app"
	if err := validateCodespaceClaimResource(claim, item); err != nil {
		t.Fatal(err)
	}
}

func TestSafetyTouchCannotRestoreStaleEndpointAfterRelease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-stale-touch", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	b.cfg.GitHubCodespaces.DeleteOnRelease = false
	leaseID := "cbx_347000000004"
	staleTarget := SSHTarget{Host: "cs.cs-stale-touch.main", Port: "22"}
	server := mustCreateSafetyClaim(t, b, item, leaseID, "stale-touch", releaseStop, "ready", time.Now().Add(time.Hour), staleTarget)
	staleServer := server
	staleServer.Labels = cloneLabels(server.Labels)
	releaseServer := server
	releaseServer.Labels = cloneLabels(server.Labels)

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{LeaseID: leaseID, Server: releaseServer, SSH: staleTarget},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Touch(context.Background(), TouchRequest{
		Lease: LeaseTarget{LeaseID: leaseID, Server: staleServer, SSH: staleTarget},
		State: "ready",
	}); err == nil {
		t.Fatal("stale touch revived a released claim")
	}

	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels[labelState] != "stopped" || claim.Labels[labelRelease] != releaseStop {
		t.Fatalf("stale touch changed stopped claim: %#v", claim)
	}
}

func TestSafetyTouchUsesCurrentClaimInsteadOfStaleLeaseSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-authoritative-touch", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_347000000007"
	currentTarget := SSHTarget{Host: "current.codespace.example", Port: "22"}
	staleTarget := SSHTarget{Host: "stale.codespace.example", Port: "2200"}
	staleServer := mustCreateSafetyClaim(t, b, item, leaseID, "authoritative-touch", releaseDelete, "ready", time.Now().Add(time.Hour), currentTarget)

	claim, ok, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	currentServer := serverFromClaim(claim)
	currentServer.Labels["authoritative"] = "current"
	if _, err := updateLeaseClaimEndpointIfUnchanged(leaseID, claim, currentServer, currentTarget); err != nil {
		t.Fatal(err)
	}
	staleServer.Labels["authoritative"] = "stale"

	touched, err := b.Touch(context.Background(), TouchRequest{
		Lease: LeaseTarget{LeaseID: leaseID, Server: staleServer, SSH: staleTarget},
		State: "in-use",
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["authoritative"] != "current" || touched.Labels[labelState] != "in-use" {
		t.Fatalf("touch returned stale state: %#v", touched)
	}
	claim, ok, err = readLeaseClaimWithPresence(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != currentTarget.Host || claim.SSHPort != 22 || claim.Labels["authoritative"] != "current" || claim.Labels[labelState] != "in-use" {
		t.Fatalf("touch restored stale snapshot: %#v", claim)
	}
}

func TestSafetyReleaseRefusesSameNameReplacementBeforeStop(t *testing.T) {
	for _, test := range []struct {
		name        string
		deleteLease bool
		dirty       bool
		leaseID     string
	}{
		{name: "retained stop", deleteLease: false, leaseID: "cbx_34700000000c"},
		{name: "dirty delete fallback", deleteLease: true, dirty: true, leaseID: "cbx_34700000000d"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fc := newFakeCodespacesClient()
			item := fakeCodespace("cs-replaced-before-stop", "Available")
			fc.items[item.Name] = item
			b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
			b.cfg.GitHubCodespaces.DeleteOnRelease = test.deleteLease
			release := releaseStop
			if test.deleteLease {
				release = releaseDelete
			}
			server := mustCreateSafetyClaim(t, b, item, test.leaseID, "replacement-stop", release, "ready", time.Now().Add(time.Hour), SSHTarget{})
			replacement := item
			replacement.EnvironmentID = "env-foreign-replacement"
			if test.dirty {
				replacement.GitStatus.HasUncommittedChanges = true
			}
			fc.items[item.Name] = replacement

			err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: test.leaseID, Server: server}})
			if err == nil || !strings.Contains(err.Error(), "environment id changed") {
				t.Fatalf("release err=%v", err)
			}
			if len(fc.stops) != 0 || len(fc.deletes) != 0 {
				t.Fatalf("replacement mutated: stops=%#v deletes=%#v", fc.stops, fc.deletes)
			}
			claim, ok, readErr := readLeaseClaimWithPresence(test.leaseID)
			if readErr != nil || !ok || claim.CloudID != item.Name || claim.Labels[labelState] != "ready" {
				t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, readErr)
			}
		})
	}
}

func TestSafetyCleanupSkipsClaimRenewedAfterInventorySnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-renewed", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_347000000005"
	now := time.Now().UTC()
	mustCreateSafetyClaim(t, b, item, leaseID, "renewed", releaseDelete, "ready", now.Add(-time.Hour), SSHTarget{})

	renewed := false
	var renewErr error
	b.now = func() time.Time {
		if !renewed {
			renewed = true
			claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
			if err != nil {
				renewErr = err
				return now
			}
			if !ok {
				renewErr = errors.New("claim disappeared before renewal")
				return now
			}
			server := serverFromClaim(claim)
			server.Labels["expires_at"] = now.Add(time.Hour).Format(time.RFC3339)
			renewErr = updateLeaseClaimEndpoint(leaseID, server, SSHTarget{})
		}
		return now
	}

	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if renewErr != nil {
		t.Fatalf("renew claim: %v", renewErr)
	}
	if !renewed {
		t.Fatal("renewal hook was not reached")
	}
	if len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("renewed claim was mutated: stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("renewed claim ok=%t err=%v", ok, err)
	}
	if expiresAt, err := time.Parse(time.RFC3339, claim.Labels["expires_at"]); err != nil || !expiresAt.After(now) {
		t.Fatalf("renewed expires_at=%q err=%v", claim.Labels["expires_at"], err)
	}
}

func TestSafetyCleanupDirtyFallbackStopsRetainsAndSucceeds(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-dirty-cleanup", "Available")
	fc.items[item.Name] = item
	fc.onStop = func(name string) {
		item := fc.items[name]
		item.GitStatus.HasUnpushedChanges = true
		fc.items[name] = item
	}
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_347000000006"
	target := SSHTarget{Host: "cs.cs-dirty-cleanup.main", Port: "22"}
	mustCreateSafetyClaim(t, b, item, leaseID, "dirty-cleanup", releaseDelete, "ready", time.Now().Add(-time.Hour), target)

	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("dirty fallback should stop and retain successfully: %v", err)
	}
	if len(fc.stops) == 0 || len(fc.deletes) != 0 {
		t.Fatalf("dirty fallback actions: stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("retained claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels[labelState] != "stopped" || claim.Labels[labelRelease] != releaseStop {
		t.Fatalf("dirty fallback claim=%#v", claim)
	}
}

type duplicateCodespacesInventoryClient struct {
	*fakeCodespacesClient
	inventory []codespace
}

func (f *duplicateCodespacesInventoryClient) listCodespaces(context.Context) ([]codespace, error) {
	return append([]codespace(nil), f.inventory...), nil
}

func mustCreateSafetyClaim(
	t *testing.T,
	b *testBackend,
	item codespace,
	leaseID string,
	slug string,
	release string,
	state string,
	expiresAt time.Time,
	target SSHTarget,
) Server {
	t.Helper()
	labels := b.labelsFor(leaseID, slug, "example-org/my-app", "alice", false, release, item, state)
	labels["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	server := b.serverFromCodespace(item, labels)
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, b.cfg, server, target, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	return server
}
