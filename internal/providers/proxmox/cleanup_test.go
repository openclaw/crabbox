package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const cleanupGeneration = "8be39656-32b0-4c47-b68c-0a9e1d3ef901"
const replacementGeneration = "b71e0631-55ba-4cf7-9013-8fb79484b00c"

type cleanupClient struct {
	*fakeProxmoxDoctorClient
	listHook   func(int) error
	checkHook  func(int)
	deleteHook func()
	existsHook func() (bool, error)
	stopErr    error
	stopCalls  int
}

func (c *cleanupClient) ListCrabboxServersCluster(context.Context) ([]Server, error) {
	c.listCalls++
	if c.listHook != nil {
		if err := c.listHook(c.listCalls); err != nil {
			return nil, err
		}
	}
	if c.listErr != nil {
		return nil, c.listErr
	}
	result := append([]Server(nil), c.servers...)
	for i := range result {
		result[i].Labels = maps.Clone(result[i].Labels)
	}
	return result, nil
}

func (c *cleanupClient) DeleteServerOnNodeChecked(ctx context.Context, node, id string, check func(Server) error) error {
	for step := range 2 {
		if c.checkHook != nil {
			c.checkHook(step)
		}
		var live Server
		for _, server := range c.servers {
			if server.CloudID == id && server.HostID == node {
				live = server
			}
		}
		if err := check(live); err != nil {
			return err
		}
		if step == 0 {
			c.stopCalls++
			if c.stopErr != nil {
				return c.stopErr
			}
		}
	}
	if c.deleteHook != nil {
		c.deleteHook()
	}
	return c.DeleteServerOnNode(ctx, node, id)
}

func (c *cleanupClient) VMExistsInCluster(context.Context, string) (bool, error) {
	if c.existsHook != nil {
		return c.existsHook()
	}
	return len(c.servers) > 0, nil
}

func cleanupFixture(t *testing.T) (*leaseBackend, *cleanupClient, core.LeaseClaim, *strings.Builder) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_cleanup"
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
	server := expiredProxmoxServer("101", leaseID)
	server.Provider, server.HostID, server.ImmutableID = "proxmox", "pve1", cleanupGeneration
	server.Labels["provider"], server.Labels["crabbox"] = "proxmox", "true"
	server.Labels["provider_key"] = core.ProviderKeyForLease(leaseID)
	server.Labels["node"] = "pve1"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", cfg, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(cfg, leaseID); err != nil {
		t.Fatal(err)
	}
	claim := readCleanupClaim(t, leaseID)
	client := &cleanupClient{fakeProxmoxDoctorClient: &fakeProxmoxDoctorClient{servers: []Server{server}}}
	previous := newClient
	newClient = func(Config) (proxmoxClient, error) { return client, nil }
	t.Cleanup(func() { newClient = previous })
	output := &strings.Builder{}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: output}).(*leaseBackend)
	return backend, client, claim, output
}

func readCleanupClaim(t *testing.T, leaseID string) core.LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim present=%v err=%v", exists, err)
	}
	return claim
}

func assertCleanupClaimPreserved(t *testing.T, expected core.LeaseClaim) {
	t.Helper()
	if actual := readCleanupClaim(t, expected.LeaseID); !reflect.DeepEqual(actual, expected) {
		t.Fatal("claim changed during rejected cleanup")
	}
	assertStoredTestboxKeyExists(t, expected.LeaseID)
}

func TestProxmoxCleanupPreservesVMAndStoredKeyWhenClaimIsMissing(t *testing.T) {
	b, c, claim, output := cleanupFixture(t)
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if c.stopCalls != 0 || c.deleteCalls != 0 || !strings.Contains(output.String(), "skip server") {
		t.Fatalf("stop=%d delete=%d output=%q", c.stopCalls, c.deleteCalls, output.String())
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); exists || err != nil {
		t.Fatalf("claim adopted=%v err=%v", exists, err)
	}
	assertStoredTestboxKeyExists(t, claim.LeaseID)
}

func TestProxmoxCleanupRejectsUnboundOrAmbiguousOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(*core.LeaseClaim)
		vm    func(*Server)
	}{
		{name: "legacy scope", claim: func(c *core.LeaseClaim) { c.ProviderScope = "" }},
		{name: "legacy VMID", claim: func(c *core.LeaseClaim) { c.CloudID = "" }},
		{name: "legacy generation", claim: func(c *core.LeaseClaim) { c.CloudImmutableID = "" }},
		{name: "other provider", claim: func(c *core.LeaseClaim) { c.Provider = "tart" }},
		{name: "other cluster", claim: func(c *core.LeaseClaim) { c.ProviderScope += "-other" }},
		{name: "other VMID", claim: func(c *core.LeaseClaim) { c.CloudID = "202" }},
		{name: "other generation", claim: func(c *core.LeaseClaim) { c.CloudImmutableID = replacementGeneration }},
		{name: "other slug", claim: func(c *core.LeaseClaim) { c.Slug = "other" }},
		{name: "claim lease label", claim: func(c *core.LeaseClaim) { c.Labels["lease"] = "cbx_other" }},
		{name: "claim provider label", claim: func(c *core.LeaseClaim) { c.Labels["provider"] = "tart" }},
		{name: "claim key namespace", claim: func(c *core.LeaseClaim) { c.Labels["provider_key"] = "other" }},
		{name: "missing live generation", vm: func(s *Server) { s.ImmutableID = "" }},
		{name: "recreated VMID", vm: func(s *Server) { s.ImmutableID = replacementGeneration }},
		{name: "live provider", vm: func(s *Server) { s.Provider = "other" }},
		{name: "live provider label", vm: func(s *Server) { delete(s.Labels, "provider") }},
		{name: "live key namespace", vm: func(s *Server) { s.Labels["provider_key"] = "other" }},
		{name: "live ownership label", vm: func(s *Server) { delete(s.Labels, "crabbox") }},
		{name: "live slug", vm: func(s *Server) { s.Labels["slug"] = "other" }},
		{name: "invalid lease label", vm: func(s *Server) { s.Labels["lease"] = "../target" }},
		{name: "no lease label", vm: func(s *Server) { delete(s.Labels, "lease") }},
		{name: "no node", vm: func(s *Server) { s.HostID = "" }},
		{name: "neutral name", vm: func(s *Server) { s.Name = "my-vm" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c, claim, _ := cleanupFixture(t)
			if tc.claim != nil {
				next := claim
				next.Labels = maps.Clone(claim.Labels)
				tc.claim(&next)
				if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, next); err != nil {
					t.Fatal(err)
				}
				claim = readCleanupClaim(t, claim.LeaseID)
			}
			if tc.vm != nil {
				tc.vm(&c.servers[0])
			}
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if c.stopCalls != 0 || c.deleteCalls != 0 {
				t.Fatalf("unauthorized mutation: stops=%d deletes=%d", c.stopCalls, c.deleteCalls)
			}
			assertCleanupClaimPreserved(t, claim)
		})
	}
}

func TestProxmoxCleanupRejectsDuplicateBindings(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote lease", true: "local VMID"}[local], func(t *testing.T) {
			b, c, claim, _ := cleanupFixture(t)
			duplicate := c.servers[0]
			duplicate.Labels = maps.Clone(duplicate.Labels)
			if local {
				duplicate.Labels["lease"] = "cbx_duplicate"
				duplicate.Labels["provider_key"] = core.ProviderKeyForLease("cbx_duplicate")
				if err := core.ClaimLeaseTargetForRepoConfig("cbx_duplicate", "old", b.Cfg, duplicate, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
					t.Fatal(err)
				}
			} else {
				duplicate.CloudID, duplicate.ImmutableID = "202", replacementGeneration
				c.servers = append(c.servers, duplicate)
			}
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if c.stopCalls != 0 || c.deleteCalls != 0 {
				t.Fatal("ambiguous ownership mutated")
			}
			assertCleanupClaimPreserved(t, claim)
		})
	}
}

func TestProxmoxCleanupExactClaimAndMigration(t *testing.T) {
	for _, node := range []string{"pve1", "pve2"} {
		t.Run(node, func(t *testing.T) {
			b, c, claim, output := cleanupFixture(t)
			c.servers[0].HostID = node
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if c.stopCalls != 1 || c.deleteCalls != 1 || c.deletedNodes[0] != node || len(c.servers) != 0 {
				t.Fatalf("stop=%d delete=%d nodes=%v", c.stopCalls, c.deleteCalls, c.deletedNodes)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); exists || err != nil {
				t.Fatalf("claim exists=%v err=%v", exists, err)
			}
			assertStoredTestboxKeyExists(t, claim.LeaseID)
			if !strings.Contains(output.String(), "key_retained=true") {
				t.Fatal("missing key retention diagnostic")
			}
		})
	}
}

func TestProxmoxCleanupDryRunAndKeptLease(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry run", false: "kept"}[dry], func(t *testing.T) {
			b, c, claim, output := cleanupFixture(t)
			if !dry {
				c.servers[0].Labels["keep"] = "true"
			}
			if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: dry}); err != nil {
				t.Fatal(err)
			}
			if c.stopCalls != 0 || c.deleteCalls != 0 {
				t.Fatal("dry run or kept VM mutated")
			}
			if dry && !strings.Contains(output.String(), "would delete") {
				t.Fatal("no dry-run candidate")
			}
			assertCleanupClaimPreserved(t, claim)
		})
	}
}

func TestProxmoxCleanupRevalidatesLiveState(t *testing.T) {
	for _, tc := range []struct {
		name string
		step int
		edit func(*Server)
	}{
		{"generation before stop", 0, func(s *Server) { s.ImmutableID = replacementGeneration }},
		{"generation after stop", 1, func(s *Server) { s.ImmutableID = replacementGeneration }},
		{"keep before stop", 0, func(s *Server) { s.Labels["keep"] = "true" }},
		{"keep after stop", 1, func(s *Server) { s.Labels["keep"] = "true" }},
		{"ownership after stop", 1, func(s *Server) { s.Labels["lease"] = "cbx_other" }},
		{"node changed", 0, func(s *Server) { s.HostID = "pve2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c, claim, _ := cleanupFixture(t)
			c.checkHook = func(step int) {
				if step == tc.step {
					tc.edit(&c.servers[0])
				}
			}
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil {
				t.Fatal("expected changed live state rejection")
			}
			if c.deleteCalls != 0 || c.stopCalls != tc.step {
				t.Fatalf("stop=%d delete=%d", c.stopCalls, c.deleteCalls)
			}
			assertCleanupClaimPreserved(t, claim)
		})
	}
}

func TestProxmoxCleanupPreservesChangedClaim(t *testing.T) {
	b, c, claim, _ := cleanupFixture(t)
	c.listHook = func(call int) error {
		if call == 1 {
			next := claim
			next.LastUsedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, next); err != nil {
				return err
			}
			claim = readCleanupClaim(t, claim.LeaseID)
		}
		return nil
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil {
		t.Fatal("changed claim must reject cleanup")
	}
	if c.stopCalls != 0 || c.deleteCalls != 0 {
		t.Fatal("claim changed before mutation")
	}
	assertCleanupClaimPreserved(t, claim)
}

func TestProxmoxCleanupFencesClaimWriterThroughAbsence(t *testing.T) {
	b, c, claim, _ := cleanupFixture(t)
	started, done := make(chan struct{}), make(chan error, 1)
	c.deleteHook = func() {
		go func() {
			close(started)
			done <- core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, claim)
		}()
		<-started
	}
	c.existsHook = func() (bool, error) {
		select {
		case err := <-done:
			return true, fmt.Errorf("claim writer escaped cleanup fence: %v", err)
		case <-time.After(40 * time.Millisecond):
			return false, nil
		}
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stale writer recreated removed claim")
		}
	case <-time.After(time.Second):
		t.Fatal("claim writer remained blocked after cleanup")
	}
	assertStoredTestboxKeyExists(t, claim.LeaseID)
}

func TestProxmoxCleanupFailurePreservesClaim(t *testing.T) {
	for _, stage := range []string{"initial inventory", "fresh inventory", "stop", "delete", "final inventory", "absence read", "VM exists elsewhere"} {
		t.Run(stage, func(t *testing.T) {
			b, c, claim, _ := cleanupFixture(t)
			failure := errors.New("fixture failure")
			switch stage {
			case "initial inventory", "fresh inventory", "final inventory":
				at := map[string]int{"initial inventory": 1, "fresh inventory": 2, "final inventory": 3}[stage]
				c.listHook = func(call int) error {
					if call == at {
						return failure
					}
					return nil
				}
			case "stop":
				c.stopErr = failure
			case "delete":
				c.deleteErr = failure
			case "absence read":
				c.existsHook = func() (bool, error) { return false, failure }
			case "VM exists elsewhere":
				c.existsHook = func() (bool, error) { return true, nil }
			}
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil {
				t.Fatal("expected cleanup failure")
			}
			if (stage == "initial inventory" || stage == "fresh inventory" || stage == "stop") && c.deleteCalls != 0 {
				t.Fatal("deleted after failed prerequisite")
			}
			assertCleanupClaimPreserved(t, claim)
		})
	}
}

func TestProxmoxCleanupReconcilesAmbiguousPurgeAcrossCluster(t *testing.T) {
	b, c, claim, _ := cleanupFixture(t)
	c.deleteAcceptedErrByID = map[string]error{"101": &core.ProxmoxDeleteTaskError{Err: errors.New("task polling unavailable")}}
	reads := 0
	c.existsHook = func() (bool, error) { reads++; return reads == 1, nil }
	previous := proxmoxDeleteVerifyPollInterval
	proxmoxDeleteVerifyPollInterval = time.Millisecond
	t.Cleanup(func() { proxmoxDeleteVerifyPollInterval = previous })
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "task polling unavailable") {
		t.Fatalf("cleanup err=%v", err)
	}
	if reads != 3 {
		t.Fatalf("cluster absence reads=%d, want retry plus final check", reads)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); exists || err != nil {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	assertStoredTestboxKeyExists(t, claim.LeaseID)
}

func TestProxmoxEndpointUpdatesCannotRebindOwnership(t *testing.T) {
	for _, field := range []string{"generation", "VMID", "slug", "lease", "provider_key", "provider"} {
		t.Run(field, func(t *testing.T) {
			b, c, claim, _ := cleanupFixture(t)
			server := c.servers[0]
			server.Labels = maps.Clone(server.Labels)
			switch field {
			case "generation":
				server.ImmutableID = replacementGeneration
			case "VMID":
				server.CloudID = "202"
			default:
				server.Labels[field] = "other"
			}
			if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, server, SSHTarget{}); err == nil {
				t.Fatal("endpoint update rebound exact claim")
			}
			if err := core.ClaimLeaseTargetForRepoConfig(claim.LeaseID, claim.Slug, b.Cfg, server, SSHTarget{}, claim.RepoRoot, time.Minute, false); err == nil {
				t.Fatal("claim publication rebound exact claim")
			}
			assertCleanupClaimPreserved(t, claim)
		})
	}
}

func TestProxmoxEndpointRefreshDoesNotPromoteLegacyGeneration(t *testing.T) {
	b, c, claim, _ := cleanupFixture(t)
	legacy := claim
	legacy.CloudImmutableID = ""
	if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, legacy); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, c.servers[0], SSHTarget{Host: "192.0.2.4"}); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseTargetForRepoConfig(claim.LeaseID, claim.Slug, b.Cfg, c.servers[0], SSHTarget{}, claim.RepoRoot, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if current := readCleanupClaim(t, claim.LeaseID); current.CloudImmutableID != "" {
		t.Fatal("legacy generation silently promoted")
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if c.deleteCalls != 0 || c.stopCalls != 0 {
		t.Fatal("legacy endpoint refresh authorized cleanup")
	}
}

func TestProxmoxTouchPreservesGenerationBinding(t *testing.T) {
	b, c, claim, _ := cleanupFixture(t)
	server, err := b.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: claim.LeaseID, Server: c.servers[0]}, State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, server, SSHTarget{Host: "192.0.2.4"}); err != nil {
		t.Fatal(err)
	}
	current := readCleanupClaim(t, claim.LeaseID)
	if current.CloudImmutableID != claim.CloudImmutableID || current.ProviderScope != claim.ProviderScope || current.Labels["provider_key"] != claim.Labels["provider_key"] {
		t.Fatal("Touch lost cleanup ownership binding")
	}
}
