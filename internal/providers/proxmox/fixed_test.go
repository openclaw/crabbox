package proxmox

import (
	"context"
	"errors"
	"io"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedTestGeneration = "8be39656-32b0-4c47-b68c-0a9e1d3ef901"

type fixedProxmoxClient struct {
	*fakeProxmoxDoctorClient
	nextVMID       int
	nextCalls      int
	fixedCreates   int
	beforeClone    func(int, map[string]string) error
	fixedCreateErr error
}

func (c *fixedProxmoxClient) NextVMID(context.Context) (int, error) {
	c.nextCalls++
	return c.nextVMID, nil
}

func (c *fixedProxmoxClient) CreateServerWithVMID(_ context.Context, _ Config, _ string, leaseID, slug string, _ bool, vmid int, labels map[string]string) (Server, error) {
	c.fixedCreates++
	if c.beforeClone != nil {
		if err := c.beforeClone(vmid, labels); err != nil {
			return Server{}, err
		}
	}
	if c.fixedCreateErr != nil {
		return Server{}, c.fixedCreateErr
	}
	server := Server{
		Provider:    "proxmox",
		CloudID:     "417",
		HostID:      "pve1",
		ImmutableID: fixedTestGeneration,
		ID:          417,
		Name:        "crabbox-" + slug,
		Labels:      maps.Clone(labels),
	}
	server.Labels["lease"] = leaseID
	server.Labels["slug"] = slug
	server.Labels["provider"] = "proxmox"
	server.Labels["crabbox"] = "true"
	server.PublicNet.IPv4.IP = "192.0.2.17"
	c.servers = []Server{server}
	return server, nil
}

func fixedProxmoxFixture(t *testing.T) (*leaseBackend, *fixedProxmoxClient, AcquireRequest) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{
		Provider:    "proxmox",
		TargetOS:    "linux",
		SSHUser:     "crabbox",
		WorkRoot:    "/work/crabbox",
		IdleTimeout: 30 * time.Minute,
		Proxmox: core.ProxmoxConfig{
			APIURL:      "https://pve.example.test:8006",
			Node:        "pve1",
			TemplateID:  9400,
			Storage:     "local-lvm",
			Bridge:      "vmbr0",
			User:        "crabbox",
			WorkRoot:    "/work/crabbox",
			FullClone:   true,
			TokenID:     "runner@pve!crabbox",
			TokenSecret: "secret",
		},
	}
	client := &fixedProxmoxClient{fakeProxmoxDoctorClient: &fakeProxmoxDoctorClient{}, nextVMID: 417}
	previousClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return client, nil }
	t.Cleanup(func() { newClient = previousClient })
	previousWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { waitForSSHReadyFunc = previousWait })
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	req := AcquireRequest{RequestedLeaseID: "cbx_123456abcdef", RequestedSlug: "fixed-proxmox", Repo: core.Repo{Root: t.TempDir()}}
	return backend, client, req
}

func readFixedProxmoxClaim(t *testing.T, leaseID string) core.LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	return claim
}

func TestProxmoxFixedAcquirePersistsVMIDBeforeCloneAndReplaysExactVM(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	client.beforeClone = func(vmid int, labels map[string]string) error {
		claim := readFixedProxmoxClaim(t, req.RequestedLeaseID)
		if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" ||
			claim.FixedCreateIntent.Attempt["vmid"] != "417" ||
			claim.FixedCreateIntent.Attempt["node"] != "pve1" ||
			claim.CloudID != "417" || claim.CloudNumericID != 417 ||
			claim.FixedCreateIntent.Fingerprint == "" ||
			claim.Labels["fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint ||
			labels["fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint ||
			vmid != 417 {
			t.Fatalf("clone preceded durable intent/VMID: vmid=%d labels=%v claim=%+v", vmid, labels, claim)
		}
		return nil
	}
	first, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if client.nextCalls != 1 || client.fixedCreates != 1 || first.Server.CloudID != "417" || replayed.Server.CloudID != first.Server.CloudID || replayed.Server.ImmutableID != first.Server.ImmutableID {
		t.Fatalf("next=%d clones=%d first=%+v replay=%+v", client.nextCalls, client.fixedCreates, first.Server, replayed.Server)
	}
}

func TestProxmoxFixedAcquireIntentDriftConflicts(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Keep = true
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("intent drift err=%v", err)
	}
	if client.fixedCreates != 1 {
		t.Fatalf("clones=%d, want 1", client.fixedCreates)
	}
}

func TestProxmoxFixedAcquireRejectsIdentityMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.LeaseClaim, *Server)
	}{
		{"lease label", func(_ *core.LeaseClaim, server *Server) { server.Labels["lease"] = "cbx_aaaaaaaaaaaa" }},
		{"provider label", func(_ *core.LeaseClaim, server *Server) { server.Labels["provider"] = "other" }},
		{"source node label", func(_ *core.LeaseClaim, server *Server) { server.Labels["node"] = "pve2" }},
		{"VMID", func(_ *core.LeaseClaim, server *Server) { server.CloudID = "418" }},
		{"vmgenid", func(_ *core.LeaseClaim, server *Server) { server.ImmutableID = replacementGeneration }},
		{"provider scope", func(claim *core.LeaseClaim, _ *Server) { claim.ProviderScope += "/other" }},
		{"claim lease label", func(claim *core.LeaseClaim, _ *Server) { claim.Labels["lease"] = "cbx_aaaaaaaaaaaa" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, client, req := fixedProxmoxFixture(t)
			if _, err := backend.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			before := readFixedProxmoxClaim(t, req.RequestedLeaseID)
			claim := before
			claim.Labels = maps.Clone(before.Labels)
			server := client.servers[0]
			server.Labels = maps.Clone(server.Labels)
			tc.mutate(&claim, &server)
			if !reflect.DeepEqual(claim, before) {
				if err := core.ReplaceLeaseClaimIfUnchanged(before.LeaseID, before, claim); err != nil {
					t.Fatal(err)
				}
			}
			client.servers = []Server{server}
			if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("mismatch accepted: %v", err)
			}
			if client.fixedCreates != 1 {
				t.Fatalf("mismatch issued clone count=%d", client.fixedCreates)
			}
		})
	}
}

func TestProxmoxFixedAcquireRetainsUncertainAttemptWithoutSecondClone(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	client.fixedCreateErr = errors.New("clone response lost")
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected first ambiguous clone failure")
	}
	before := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if before.FixedCreateIntent.Attempt["vmid"] != "417" || before.FixedCreateIntent.State != "prepared" {
		t.Fatalf("uncertain attempt not retained: %+v", before)
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("replay err=%v", err)
	}
	after := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if client.nextCalls != 1 || client.fixedCreates != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("next=%d clones=%d before=%+v after=%+v", client.nextCalls, client.fixedCreates, before, after)
	}
}

func TestProxmoxFixedAcquireNeverAdoptsBySlug(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	client.fixedCreateErr = errors.New("clone response lost")
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected first ambiguous clone failure")
	}
	unrelated := Server{
		Provider: "proxmox", CloudID: "417", HostID: "pve1", ImmutableID: fixedTestGeneration,
		ID: 417, Name: "crabbox-" + req.RequestedSlug,
		Labels: map[string]string{
			"crabbox": "true", "provider": "proxmox", "lease": "cbx_aaaaaaaaaaaa",
			"slug": req.RequestedSlug, "provider_key": core.ProviderKeyForLease("cbx_aaaaaaaaaaaa"),
		},
	}
	client.servers = []Server{unrelated}
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("slug-only VM adopted: %v", err)
	}
	if client.fixedCreates != 1 {
		t.Fatalf("slug-only replay issued clone count=%d", client.fixedCreates)
	}
}

func TestProxmoxFixedConfirmedAbsentAttemptLeavesTerminalTombstone(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	client.fixedCreateErr = errors.New("clone response lost")
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected first ambiguous clone failure")
	}
	previous := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: req.RequestedLeaseID}}); err != nil {
		t.Fatal(err)
	}
	claim := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if claim.FixedCreateIntent.State != "released" || claim.CloudID != "417" {
		t.Fatalf("confirmed absence did not retain terminal VMID: %+v", claim)
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("confirmed-absent fixed ID was reusable")
	}
	if retained, err := backend.RetainLeaseClaimAfterReleaseWithClaim(LeaseTarget{LeaseID: req.RequestedLeaseID}, previous); err != nil || !retained {
		t.Fatalf("terminal claim retained=%t err=%v", retained, err)
	}
}

func TestProxmoxFixedReleaseOnlyConvergesOnProvenClusterAbsenceWithoutClaim(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	target, err := backend.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("release-only resolve: %v", err)
	}
	if target.LeaseID != req.RequestedLeaseID {
		t.Fatalf("target=%#v, want confirmed-absent target for %s", target, req.RequestedLeaseID)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if client.deleteCalls != 0 || client.mutated {
		t.Fatalf("deletes=%d mutated=%t, want no provider mutation", client.deleteCalls, client.mutated)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v, want no fabricated claim", exists, err)
	}
}

func TestProxmoxFixedReleaseOnlyAbsenceFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		mutate func(*leaseBackend, *fixedProxmoxClient)
		want   string
	}{
		{
			name: "unreadable inventory",
			mutate: func(_ *leaseBackend, client *fixedProxmoxClient) {
				client.clusterListErr = errors.New("cluster inventory unavailable")
			},
			want: "cluster inventory unavailable",
		},
		{
			name: "reused provider key",
			mutate: func(_ *leaseBackend, client *fixedProxmoxClient) {
				client.servers = []Server{{
					Provider: "proxmox", CloudID: "417", HostID: "pve1", ID: 417,
					Labels: map[string]string{
						"crabbox": "true", "provider": "proxmox", "lease": "cbx_aaaaaaaaaaaa",
						"slug": "other", "provider_key": core.ProviderKeyForLease("cbx_123456abcdef"),
					},
				}}
			},
			want: "lease_id_conflict",
		},
		{
			name:   "unresolved cluster scope",
			mutate: func(backend *leaseBackend, _ *fixedProxmoxClient) { backend.Cfg.Proxmox.Node = "" },
			want:   "cluster scope",
		},
		{
			name: "non-canonical identifier",
			id:   "fixed-proxmox-lookalike",
			want: "lease/server not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, client, req := fixedProxmoxFixture(t)
			if tc.mutate != nil {
				tc.mutate(backend, client)
			}
			_, err := backend.Resolve(context.Background(), ResolveRequest{ID: core.Blank(tc.id, req.RequestedLeaseID), ReleaseOnly: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolve err=%v, want %q", err, tc.want)
			}
			if client.deleteCalls != 0 || client.mutated {
				t.Fatalf("deletes=%d mutated=%t, want no provider mutation", client.deleteCalls, client.mutated)
			}
		})
	}
}

func TestProxmoxFixedReleaseLeavesTerminalTombstone(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	previous := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if claim.Provider != core.FixedProxmoxClaimProvider || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("invalid terminal claim: %+v", claim)
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal replay err=%v", err)
	}
	if client.fixedCreates != 1 || client.deleteCalls != 1 {
		t.Fatalf("clones=%d deletes=%d", client.fixedCreates, client.deleteCalls)
	}
	if retained, err := backend.RetainLeaseClaimAfterReleaseWithClaim(lease, previous); err != nil || !retained {
		t.Fatalf("terminal claim retained=%t err=%v", retained, err)
	}
}

func TestProxmoxFixedReleaseRejectsMismatchedLeaseLabel(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	lease.Server.Labels = maps.Clone(lease.Server.Labels)
	lease.Server.Labels["lease"] = "cbx_aaaaaaaaaaaa"
	before := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("mismatched release label was accepted")
	}
	if client.deleteCalls != 0 || !reflect.DeepEqual(before, readFixedProxmoxClaim(t, req.RequestedLeaseID)) {
		t.Fatal("mismatched release label changed resource or claim")
	}
}

func TestProxmoxFixedCleanupLeavesTerminalTombstone(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	client.servers[0].Labels["state"] = "failed"
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	if claim.FixedCreateIntent.State != "released" || client.deleteCalls != 1 {
		t.Fatalf("cleanup claim=%+v deletes=%d", claim, client.deleteCalls)
	}
}
