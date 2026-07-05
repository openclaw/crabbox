package scaleway

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"strings"
	"testing"
	"time"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	marketplace "github.com/scaleway/scaleway-sdk-go/api/marketplace/v2"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestScalewayAcquireListResolveTouchReleaseLifecycle(t *testing.T) {
	backend, fake := newTestBackend(t)
	var observed core.LeaseTarget
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "blue-box",
		OnAcquired: func(target core.LeaseTarget) error {
			observed = target
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID == "" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
	}
	if observed.Server.CloudID != lease.Server.CloudID || observed.LeaseID != lease.LeaseID || observed.SSH.Host != lease.SSH.Host {
		t.Fatalf("OnAcquired observed=%#v lease=%#v", observed, lease)
	}
	if len(fake.keys) != 1 {
		t.Fatalf("created keys=%#v", fake.keys)
	}
	if fake.lastCreate == nil || fake.lastCreate.DynamicIPRequired == nil || !*fake.lastCreate.DynamicIPRequired {
		t.Fatalf("create request did not request dynamic IP: %#v", fake.lastCreate)
	}
	if fake.lastCreate.Project == nil || *fake.lastCreate.Project != "project-1" || fake.lastCreate.Zone != scw.Zone("fr-par-1") {
		t.Fatalf("create project/zone=%#v", fake.lastCreate)
	}
	if fake.lastCreate.SecurityGroup == nil || *fake.lastCreate.SecurityGroup != "sg-1" {
		t.Fatalf("security group=%#v", fake.lastCreate.SecurityGroup)
	}
	if fake.lastListOptions < 2 {
		t.Fatal("inventory list did not request all pages")
	}
	if !strings.Contains(fake.userData, "ssh_authorized_keys") {
		t.Fatalf("cloud-init user data missing ssh keys: %s", fake.userData)
	}
	if !fake.poweredOn {
		t.Fatal("server was not powered on after cloud-init user data was set")
	}
	if got := labelsFromTags(fake.server.Tags); got["state"] != "ready" || got["scaleway_project"] != "project-1" || got["scaleway_ssh_key_id"] == "" {
		t.Fatalf("ready tags labels=%#v tags=%v", got, fake.server.Tags)
	}

	list, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].CloudID != fake.server.ID {
		t.Fatalf("list=%#v", list)
	}
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "blue-box"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.LeaseID != lease.LeaseID || resolved.Server.CloudID != fake.server.ID {
		t.Fatalf("resolved=%#v", resolved)
	}
	touched, err := backend.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "running", IdleTimeout: 4 * time.Hour})
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if touched.Labels["state"] != "running" {
		t.Fatalf("touch labels=%#v", touched.Labels)
	}
	if touched.Labels["idle_timeout_secs"] != "14400" {
		t.Fatalf("touch did not persist idle timeout override: %#v", touched.Labels)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !fake.deletedServer || !fake.deletedKey {
		t.Fatalf("deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	if !fake.poweredOff {
		t.Fatal("running server was not powered off before deletion")
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil || ok {
		t.Fatalf("claim after release ok=%t err=%v", ok, err)
	}
}

func TestScalewayResolveReadOnlyIgnoresStaleClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_121212121212"
	slug := "stale-read-only"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.server = testServer("srv-live", core.LeaseProviderName(leaseID, slug), tagsFromLabels(labels), "203.0.113.21")
	claimServer := core.Server{Provider: providerName, CloudID: "srv-stale", Name: fake.server.Name, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != fake.server.ID {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.CloudID != "srv-stale" {
		t.Fatalf("read-only resolve changed stale claim: claim=%#v exists=%v err=%v", claim, exists, err)
	}
}

func TestScalewayNoMutationResolveDoesNotBindAmbiguousRecovery(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_131313131313"
	slug := "no-mutation-recovery"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-no-mutation"
	claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	delete(liveLabels, "recovery")
	fake.server = testServer("srv-no-mutation", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.22")

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "srv-no-mutation" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !exists || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("no-mutation resolve changed claim: claim=%#v exists=%t err=%v", claim, exists, claimErr)
	}
}

func TestStatusTouchClaimRequiresMatchingProviderIdentity(t *testing.T) {
	backend := &Backend{}
	labels := map[string]string{
		"scaleway_project":      "project-a",
		"scaleway_organization": "organization-a",
		"scaleway_zone":         "fr-par-1",
	}
	claim := core.LeaseClaim{Labels: maps.Clone(labels)}
	lease := core.LeaseTarget{Server: core.Server{Labels: maps.Clone(labels)}}
	if !backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("matching provider identity was rejected")
	}
	for _, key := range []string{"scaleway_project", "scaleway_zone"} {
		mismatched := lease
		mismatched.Server.Labels = maps.Clone(lease.Server.Labels)
		mismatched.Server.Labels[key] = "other"
		if backend.StatusTouchClaimMatches(mismatched, claim) {
			t.Fatalf("mismatched %s was accepted", key)
		}
		missing := claim
		missing.Labels = maps.Clone(claim.Labels)
		delete(missing.Labels, key)
		if backend.StatusTouchClaimMatches(lease, missing) {
			t.Fatalf("missing claim %s was accepted", key)
		}
	}
	withoutOrganization := claim
	withoutOrganization.Labels = maps.Clone(claim.Labels)
	delete(withoutOrganization.Labels, "scaleway_organization")
	if !backend.StatusTouchClaimMatches(lease, withoutOrganization) {
		t.Fatal("optional organization was required")
	}
	mismatchedOrganization := lease
	mismatchedOrganization.Server.Labels = maps.Clone(lease.Server.Labels)
	mismatchedOrganization.Server.Labels["scaleway_organization"] = "other"
	if backend.StatusTouchClaimMatches(mismatchedOrganization, claim) {
		t.Fatal("present organization mismatch was accepted")
	}
}

func TestScalewayAcquireOnAcquiredErrorRollsBack(t *testing.T) {
	backend, fake := newTestBackend(t)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "reject",
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected identity") {
		t.Fatalf("Acquire err=%v", err)
	}
	if !fake.deletedServer || !fake.deletedKey {
		t.Fatalf("rollback deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 0 {
		t.Fatalf("rollback should remove recovery claim after cleanup: %#v", claims)
	}
}

func TestScalewayAcquireOnAcquiredErrorRollsBackWithKeep(t *testing.T) {
	backend, fake := newTestBackend(t)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "reject-kept",
		Keep:          true,
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected kept identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected kept identity") {
		t.Fatalf("Acquire err=%v", err)
	}
	if !fake.deletedServer || !fake.deletedKey {
		t.Fatalf("rollback deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 0 {
		t.Fatalf("rollback should remove recovery claim after cleanup: %#v", claims)
	}
}

func TestScalewayAcquireRejectsUnsupportedSSHCIDRs(t *testing.T) {
	backend, fake := newTestBackend(t)
	backend.cfg.Scaleway.SSHCIDRs = []string{"203.0.113.0/24"}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cidrs"})
	if err == nil || !strings.Contains(err.Error(), "does not yet manage security-group SSH CIDRs") {
		t.Fatalf("Acquire err=%v", err)
	}
	if fake.lastCreate != nil {
		t.Fatalf("server was created despite unsupported CIDRs: %#v", fake.lastCreate)
	}
}

func TestScalewayAcquireRejectsUnsupportedPortableOSBeforeDefaultingImage(t *testing.T) {
	backend, fake := newTestBackend(t)
	backend.cfg.OSImage = "ubuntu:26.04"
	backend.cfg.Scaleway.Image = ""
	core.SetOSImageExplicit(&backend.cfg)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "unsupported-os"})
	if err == nil || !strings.Contains(err.Error(), "does not support os") {
		t.Fatalf("Acquire err=%v", err)
	}
	if fake.lastCreate != nil {
		t.Fatalf("server was created despite unsupported OS: %#v", fake.lastCreate)
	}
}

func TestScalewayCleanupSkipsForeignAndDeletesExpiredOwned(t *testing.T) {
	backend, fake := newTestBackend(t)
	now := backend.clockNow()
	ownedLabels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_111111111111", "owned", providerName, "", false, now.Add(-3*time.Hour))
	ownedLabels["scaleway_project"] = "project-1"
	ownedLabels["scaleway_zone"] = "fr-par-1"
	ownedLabels["scaleway_ssh_key_id"] = "key-owned"
	claimlessLabels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_000000000000", "claimless", providerName, "", false, now.Add(-3*time.Hour))
	claimlessLabels["scaleway_project"] = "project-1"
	claimlessLabels["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{
		testServer("srv-claimless", "crabbox-cbx-claimless", tagsFromLabels(claimlessLabels), "203.0.113.10"),
		testServer("srv-owned", "crabbox-cbx-owned", tagsFromLabels(ownedLabels), "203.0.113.11"),
		testServer("srv-foreign", "foreign", []string{"crabbox", "crabbox:provider:other"}, "203.0.113.12"),
	}
	claimServer := backend.serverFromScaleway(fake.servers[1])
	if err := core.ClaimLeaseTargetForConfig(
		"cbx_111111111111",
		"owned",
		backend.cfgForRun(),
		claimServer,
		core.SSHTarget{},
		backend.cfgForRun().IdleTimeout,
	); err != nil {
		t.Fatal(err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if !fake.deletedServer {
		t.Fatal("owned expired server was not deleted")
	}
	if fake.deletedServerID != "srv-owned" {
		t.Fatalf("deleted server id=%q", fake.deletedServerID)
	}
}

func TestScalewayCleanupRefusesMismatchedProject(t *testing.T) {
	backend, fake := newTestBackend(t)
	labels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_abcdefabcdef", "bad", providerName, "", false, backend.clockNow().Add(-3*time.Hour))
	labels["scaleway_project"] = "other-project"
	labels["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{testServer("srv-bad", "crabbox-cbx-bad", tagsFromLabels(labels), "203.0.113.13")}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("mismatched project should be skipped as foreign ownership, got: %v", err)
	}
	if fake.deletedServer {
		t.Fatal("mismatched project server was deleted")
	}
}

func TestScalewayAmbiguousCreatePersistsRecoveryClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.createErr = context.DeadlineExceeded
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%#v", claims)
	}
	if claims[0].Provider != providerName || claims[0].Labels["recovery"] != "ambiguous-create" || claims[0].Labels["scaleway_ssh_key_name"] == "" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claim=%#v", claims[0])
	}
	if fake.deletedKey {
		t.Fatal("ambiguous create must retain the managed Scaleway SSH key for recovery")
	}
}

func TestScalewayAmbiguousSSHKeyCreateReconcilesCleanableKey(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.createKeyErr = context.DeadlineExceeded
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.acquireOnce(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous-key"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "rollback-key-cleanup" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claims=%#v", claims)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: claims[0].LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("Resolve recovery claim: %v", err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("Release recovery claim: %v", err)
	}
	if !fake.deletedKey {
		t.Fatal("release did not delete reconciled SSH key")
	}
}

func TestScalewayReleaseOnlyPreservesAmbiguousSlugError(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	labelsA := core.DirectLeaseLabels(cfg, "cbx_222222222222", "duplicate", providerName, "", false, backend.clockNow())
	labelsA["scaleway_project"] = "project-1"
	labelsA["scaleway_zone"] = "fr-par-1"
	labelsB := core.DirectLeaseLabels(cfg, "cbx_333333333333", "duplicate", providerName, "", false, backend.clockNow())
	labelsB["scaleway_project"] = "project-1"
	labelsB["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{
		testServer("srv-a", "duplicate-a", tagsFromLabels(labelsA), "203.0.113.21"),
		testServer("srv-b", "duplicate-b", tagsFromLabels(labelsB), "203.0.113.22"),
	}
	claimServer := backend.serverFromScaleway(fake.servers[0])
	if err := core.ClaimLeaseTargetForConfig("cbx_222222222222", "duplicate", cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "duplicate", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "matches multiple active leases") {
		t.Fatalf("Resolve ambiguous slug err=%v", err)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "srv-a", ReleaseOnly: true})
	if err != nil || lease.Server.CloudID != "srv-a" {
		t.Fatalf("Resolve exact cloud id lease=%#v err=%v", lease, err)
	}
}

func TestScalewayReleaseOnlyDoesNotRetargetForeignExactClaimToSlug(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	const (
		requestedID = "cbx_222222222222"
		lookalikeID = "cbx_333333333333"
	)

	foreignCfg := cfg
	foreignCfg.Provider = "aws"
	foreignLabels := core.DirectLeaseLabels(foreignCfg, requestedID, "foreign", "aws", "", false, backend.clockNow())
	if err := core.ClaimLeaseTargetForConfig(requestedID, "foreign", foreignCfg, core.Server{Provider: "aws", CloudID: "i-foreign", Labels: foreignLabels}, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	labels := core.DirectLeaseLabels(cfg, lookalikeID, requestedID, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.server = testServer("srv-lookalike", "lookalike", tagsFromLabels(labels), "203.0.113.23")
	server := backend.serverFromScaleway(fake.server)
	if err := core.ClaimLeaseTargetForConfig(lookalikeID, requestedID, cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: requestedID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("retargeted cleanup deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayReleaseOnlyDoesNotFallBackToCanonicalClaimSlug(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	const (
		requestedID = "cbx_aaaaaaaaaaaa"
		lookalikeID = "cbx_bbbbbbbbbbbb"
	)
	labels := core.DirectLeaseLabels(cfg, lookalikeID, requestedID, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.server = testServer("srv-lookalike", "lookalike", tagsFromLabels(labels), "203.0.113.24")
	server := backend.serverFromScaleway(fake.server)
	if err := core.ClaimLeaseTargetForConfig(lookalikeID, requestedID, cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: requestedID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("retargeted cleanup deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayUpdateTailscaleMetadataPersistsTagsAndClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "tailnet"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	updated, err := backend.UpdateTailscaleMetadata(context.Background(), lease, core.TailscaleMetadata{
		Enabled:  true,
		Hostname: "tailnet-host",
		FQDN:     "tailnet-host.example.ts.net",
		IPv4:     "100.64.0.42",
		Tags:     []string{"tag:ci"},
		State:    "ready",
	})
	if err != nil {
		t.Fatalf("UpdateTailscaleMetadata: %v", err)
	}
	for key, want := range map[string]string{
		"tailscale":          "true",
		"tailscale_hostname": "tailnet-host",
		"tailscale_fqdn":     "tailnet-host.example.ts.net",
		"tailscale_ipv4":     "100.64.0.42",
		"tailscale_tags":     "tag:ci",
		"tailscale_state":    "ready",
	} {
		if got := updated.Labels[key]; got != want {
			t.Fatalf("updated label %s=%q want %q", key, got, want)
		}
		if got := labelsFromTags(fake.server.Tags)[key]; got != want {
			t.Fatalf("live tag %s=%q want %q", key, got, want)
		}
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels["tailscale_ipv4"] != "100.64.0.42" || claim.Labels["tailscale_fqdn"] != "tailnet-host.example.ts.net" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestScalewayFailedPreCreateKeyCleanupPersistsRecoveryClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	backend.cfg.Scaleway.Image = "missing"
	fake.deleteKeyErr = errors.New("iam unavailable")
	_, err := backend.acquireOnce(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-recovery"})
	if err == nil || !strings.Contains(err.Error(), "rollback SSH key cleanup failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "rollback-key-cleanup" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claims=%#v", claims)
	}
}

func TestScalewayMissingCreateIdentityPersistsRecoveryClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.createResponseWithoutServer = true
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "missing-id"})
	if err == nil || !strings.Contains(err.Error(), "omitted server id") {
		t.Fatalf("Acquire err=%v", err)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "ambiguous-create-response" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claims=%#v", claims)
	}
	if fake.deletedKey {
		t.Fatal("ambiguous create response deleted the access key")
	}
}

func TestScalewayReleaseRetainsIdentitylessRecoveryClaim(t *testing.T) {
	backend, _ := newTestBackend(t)
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_444444444444", "recover", providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	server := core.Server{Provider: providerName, Name: "recover", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_444444444444", "recover", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_444444444444", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "claim retained") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("cbx_444444444444", providerName); claimErr != nil || !ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
}

func TestScalewayAmbiguousCreateBindsUniqueServerBeforeCleanup(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_454545454545"
	slug := "recovered"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-recovered"
	claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	delete(liveLabels, "recovery")
	fake.server = testServer("srv-recovered", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.24")
	fake.deleteKeyErr = errors.New("key cleanup failed")

	err := backend.deleteServer(context.Background(), fake, backend.serverFromScaleway(fake.server))
	if err == nil || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("deleteServer err=%v", err)
	}
	if !fake.deletedServer {
		t.Fatal("recovered server was not deleted")
	}
	claim, ok, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "srv-recovered" || claim.Labels["scaleway_ssh_key_id"] != "key-recovered" {
		t.Fatalf("bound recovery claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestScalewayAmbiguousCreateRefusesDuplicateServers(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_464646464646"
	slug := "duplicates"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-duplicates"
	claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	delete(liveLabels, "recovery")
	fake.servers = []*instance.Server{
		testServer("srv-first", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.25"),
		testServer("srv-second", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.26"),
	}

	err := backend.deleteServer(context.Background(), fake, backend.serverFromScaleway(fake.servers[0]))
	if err == nil || !strings.Contains(err.Error(), "found 2 matching servers") {
		t.Fatalf("deleteServer err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("ambiguous cleanup mutated resources: server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	claim, ok, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "" {
		t.Fatalf("ambiguous claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestScalewayCleanupRefusesChangedLiveSSHKeyIdentity(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_474747474747"
	slug := "changed-key"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-claimed"
	claimServer := core.Server{Provider: providerName, CloudID: "srv-changed-key", Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	liveLabels["scaleway_ssh_key_id"] = "key-other"
	fake.server = testServer("srv-changed-key", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.27")

	err := backend.deleteServer(context.Background(), fake, claimServer)
	if err == nil || !strings.Contains(err.Error(), "SSH key identity does not match") {
		t.Fatalf("deleteServer err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("changed key cleanup mutated resources: server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayReleaseCleansIdentitylessRollbackKeyClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_555555555555", "key-recover", providerName, "", false, backend.clockNow())
	labels["recovery"] = "rollback-key-cleanup"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-recover"
	server := core.Server{Provider: providerName, Name: "key-recover", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_555555555555", "key-recover", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_555555555555", Server: server}}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !fake.deletedKey {
		t.Fatal("release did not delete the identityless recovery key")
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("cbx_555555555555", providerName); claimErr != nil || ok {
		t.Fatalf("claim after recovery-key cleanup ok=%t err=%v", ok, claimErr)
	}
}

func TestScalewayRejectsKeyOnlyRecoveryClaimForLiveServer(t *testing.T) {
	for _, recovery := range []string{"ambiguous-key-create", "rollback-key-cleanup", "rollback-cleanup"} {
		t.Run(recovery, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			cfg := backend.cfgForRun()
			leaseID := "cbx_565656565656"
			slug := "key-only-live"
			labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
			labels["recovery"] = recovery
			labels["scaleway_project"] = "project-1"
			labels["scaleway_organization"] = "organization-1"
			labels["scaleway_zone"] = "fr-par-1"
			labels["scaleway_ssh_key_id"] = "key-recover"
			claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
			if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
				t.Fatal(err)
			}
			fake.server = testServer("server-live", core.LeaseProviderName(leaseID, slug), tagsFromLabels(labels), "203.0.113.23")
			server := backend.serverFromScaleway(fake.server)
			err := backend.deleteServer(context.Background(), fake, server)
			if err == nil || !strings.Contains(err.Error(), "no server identity or valid recovery state") {
				t.Fatalf("deleteServer err=%v", err)
			}
			if fake.deletedServer || fake.deletedKey {
				t.Fatalf("key-only recovery mutated live resources: deletedServer=%t deletedKey=%t", fake.deletedServer, fake.deletedKey)
			}
		})
	}
}

func TestScalewayReleaseCleansClaimAndKeyWhenServerAlreadyDeleted(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.getErr = &scw.ResourceNotFoundError{}
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_666666666666", "gone", providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-gone"
	server := core.Server{Provider: providerName, CloudID: "srv-gone", Name: "gone", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_666666666666", "gone", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_666666666666", Server: server}}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !fake.deletedKey {
		t.Fatal("release did not delete managed SSH key after server not found")
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("cbx_666666666666", providerName); claimErr != nil || ok {
		t.Fatalf("claim after stale release ok=%t err=%v", ok, claimErr)
	}
}

func TestScalewayReleaseOnlyRefusesLiveServerWithChangedTags(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_777777777777", "stale", providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-stale"
	claimServer := core.Server{Provider: providerName, CloudID: "srv-stale", Name: "stale", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_777777777777", "stale", cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	fake.server = testServer("srv-stale", "changed", []string{"foreign"}, "203.0.113.20")
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_777777777777", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "non-Crabbox Scaleway") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
}

func TestScalewayReleaseLeaseRefusesLiveServerWithChangedTags(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-before-release"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	fake.server.Tags = []string{"foreign"}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "non-Crabbox Scaleway") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("changed live server cleanup deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayDoctorReportsInventoryAndMissingAuth(t *testing.T) {
	backend, fake := newTestBackend(t)
	labels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_888888888888", "doc", providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{testServer("srv-doc", "crabbox-cbx-doc", tagsFromLabels(labels), "203.0.113.14")}
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if result.Status != "ok" || !strings.Contains(result.Message, "leases=1") || strings.Contains(result.Message, "secret") {
		t.Fatalf("doctor=%#v", result)
	}
	if fake.lastConfig.Scaleway.Zone != "fr-par-1" {
		t.Fatalf("doctor did not use default-normalized config: %#v", fake.lastConfig.Scaleway)
	}

	backend.newClient = func(core.Config, core.Runtime) (Client, error) {
		return nil, core.Exit(3, "SCW_SECRET_KEY or Scaleway SDK secret_key is required")
	}
	result, err = backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor missing auth: %v", err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "SCW_SECRET_KEY") {
		t.Fatalf("missing auth doctor=%#v", result)
	}
}

func newTestBackend(t *testing.T) (*Backend, *fakeScalewayClient) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := newFakeScalewayClient()
	cfg := core.Config{
		Provider: providerName,
		TargetOS: core.TargetLinux,
		SSHUser:  "root",
		SSHPort:  "22",
		WorkRoot: "/work/crabbox",
		Class:    "standard",
		Scaleway: core.ScalewayConfig{
			Region:        "fr-par",
			Zone:          "fr-par-1",
			Image:         "ubuntu_noble",
			Type:          "DEV1-S",
			ProjectID:     "project-1",
			SecurityGroup: "sg-1",
		},
	}
	backend := &Backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (Client, error) {
			return fake, nil
		},
		waitSSH: func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil },
		now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	backend.newClient = func(cfg core.Config, rt core.Runtime) (Client, error) {
		fake.lastConfig = cfg
		return fake, nil
	}
	return backend, fake
}

type fakeScalewayClient struct {
	instance *fakeInstanceAPI
	iam      *fakeIAMAPI
	market   *fakeMarketplaceAPI

	servers                     []*instance.Server
	server                      *instance.Server
	keys                        []*iam.SSHKey
	lastCreate                  *instance.CreateServerRequest
	lastListOptions             int
	lastConfig                  core.Config
	userData                    string
	deletedServer               bool
	deletedServerID             string
	deletedKey                  bool
	poweredOn                   bool
	poweredOff                  bool
	createErr                   error
	createKeyErr                error
	getErr                      error
	deleteErr                   error
	deleteKeyErr                error
	createResponseWithoutServer bool
}

func newFakeScalewayClient() *fakeScalewayClient {
	f := &fakeScalewayClient{}
	f.instance = &fakeInstanceAPI{f: f}
	f.iam = &fakeIAMAPI{f: f}
	f.market = &fakeMarketplaceAPI{}
	return f
}

func (f *fakeScalewayClient) Instance() InstanceAPI       { return f.instance }
func (f *fakeScalewayClient) IAM() IAMAPI                 { return f.iam }
func (f *fakeScalewayClient) Marketplace() MarketplaceAPI { return f.market }
func (f *fakeScalewayClient) ProjectID() string           { return "project-1" }
func (f *fakeScalewayClient) OrganizationID() string      { return "org-1" }
func (f *fakeScalewayClient) Region() string              { return "fr-par" }
func (f *fakeScalewayClient) Zone() string                { return "fr-par-1" }

type fakeInstanceAPI struct{ f *fakeScalewayClient }

func (api *fakeInstanceAPI) ListServers(req *instance.ListServersRequest, opts ...scw.RequestOption) (*instance.ListServersResponse, error) {
	api.f.lastListOptions = len(opts)
	if api.f.servers != nil {
		return &instance.ListServersResponse{Servers: api.f.servers}, nil
	}
	if api.f.server == nil {
		return &instance.ListServersResponse{}, nil
	}
	return &instance.ListServersResponse{Servers: []*instance.Server{api.f.server}}, nil
}

func (api *fakeInstanceAPI) GetServer(req *instance.GetServerRequest, _ ...scw.RequestOption) (*instance.GetServerResponse, error) {
	for _, server := range append(api.f.servers, api.f.server) {
		if server != nil && server.ID == req.ServerID {
			return &instance.GetServerResponse{Server: server}, nil
		}
	}
	if api.f.getErr != nil {
		return nil, api.f.getErr
	}
	return nil, errors.New("not found")
}

func (api *fakeInstanceAPI) CreateServer(req *instance.CreateServerRequest, _ ...scw.RequestOption) (*instance.CreateServerResponse, error) {
	api.f.lastCreate = req
	if api.f.createErr != nil {
		return nil, api.f.createErr
	}
	api.f.server = testServer("srv-1", req.Name, req.Tags, "203.0.113.10")
	api.f.server.CommercialType = req.CommercialType
	if api.f.createResponseWithoutServer {
		return &instance.CreateServerResponse{}, nil
	}
	return &instance.CreateServerResponse{Server: api.f.server}, nil
}

func (api *fakeInstanceAPI) UpdateServer(req *instance.UpdateServerRequest, _ ...scw.RequestOption) (*instance.UpdateServerResponse, error) {
	server := api.f.server
	if server == nil {
		for _, candidate := range api.f.servers {
			if candidate.ID == req.ServerID {
				server = candidate
				break
			}
		}
	}
	if server == nil {
		return nil, errors.New("not found")
	}
	if req.Tags != nil {
		server.Tags = *req.Tags
	}
	return &instance.UpdateServerResponse{Server: server}, nil
}

func (api *fakeInstanceAPI) DeleteServer(req *instance.DeleteServerRequest, _ ...scw.RequestOption) error {
	for _, server := range append(api.f.servers, api.f.server) {
		if server != nil && server.ID == req.ServerID && server.State != instance.ServerStateStopped && server.State != instance.ServerStateStoppedInPlace {
			return errors.New("precondition failed: resource is still in use, instance should be powered off")
		}
	}
	api.f.deletedServer = true
	api.f.deletedServerID = req.ServerID
	if api.f.deleteErr != nil {
		return api.f.deleteErr
	}
	return nil
}

func (api *fakeInstanceAPI) SetServerUserData(req *instance.SetServerUserDataRequest, _ ...scw.RequestOption) error {
	data, err := io.ReadAll(req.Content)
	if err != nil {
		return err
	}
	api.f.userData = string(data)
	return nil
}

func (api *fakeInstanceAPI) ServerAction(req *instance.ServerActionRequest, _ ...scw.RequestOption) (*instance.ServerActionResponse, error) {
	if req.Action == instance.ServerActionPoweron {
		api.f.poweredOn = true
	}
	if req.Action == instance.ServerActionPoweroff {
		api.f.poweredOff = true
		for _, server := range append(api.f.servers, api.f.server) {
			if server != nil && server.ID == req.ServerID {
				server.State = instance.ServerStateStopped
			}
		}
	}
	return &instance.ServerActionResponse{}, nil
}

type fakeIAMAPI struct{ f *fakeScalewayClient }

func (api *fakeIAMAPI) ListSSHKeys(req *iam.ListSSHKeysRequest, _ ...scw.RequestOption) (*iam.ListSSHKeysResponse, error) {
	return &iam.ListSSHKeysResponse{SSHKeys: api.f.keys}, nil
}
func (api *fakeIAMAPI) GetSSHKey(req *iam.GetSSHKeyRequest, _ ...scw.RequestOption) (*iam.SSHKey, error) {
	for _, key := range api.f.keys {
		if key.ID == req.SSHKeyID {
			return key, nil
		}
	}
	return nil, errors.New("not found")
}
func (api *fakeIAMAPI) CreateSSHKey(req *iam.CreateSSHKeyRequest, _ ...scw.RequestOption) (*iam.SSHKey, error) {
	key := &iam.SSHKey{ID: "key-1", Name: req.Name, PublicKey: req.PublicKey, ProjectID: req.ProjectID}
	api.f.keys = append(api.f.keys, key)
	if api.f.createKeyErr != nil {
		return nil, api.f.createKeyErr
	}
	return key, nil
}
func (api *fakeIAMAPI) DeleteSSHKey(req *iam.DeleteSSHKeyRequest, _ ...scw.RequestOption) error {
	api.f.deletedKey = true
	return api.f.deleteKeyErr
}

type fakeMarketplaceAPI struct{}

func (api *fakeMarketplaceAPI) GetLocalImageByLabel(req *marketplace.GetLocalImageByLabelRequest, _ ...scw.RequestOption) (*marketplace.LocalImage, error) {
	if req.ImageLabel == "ubuntu_noble" && req.Zone == scw.Zone("fr-par-1") && strings.EqualFold(req.CommercialType, "DEV1-S") {
		return &marketplace.LocalImage{ID: "image-1", Label: req.ImageLabel, Zone: req.Zone, CompatibleCommercialTypes: []string{"DEV1-S"}}, nil
	}
	return nil, errors.New("no image")
}

func testServer(id, name string, tags []string, publicIP string) *instance.Server {
	return &instance.Server{
		ID:             id,
		Name:           name,
		Project:        "project-1",
		Organization:   "org-1",
		Zone:           scw.Zone("fr-par-1"),
		State:          instance.ServerStateRunning,
		CommercialType: "DEV1-S",
		Tags:           append([]string(nil), tags...),
		PublicIP:       &instance.ServerIP{Address: net.ParseIP(publicIP), Dynamic: true},
	}
}
