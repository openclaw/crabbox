package hetzner

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"io"
	"testing"
)

func TestTailscaleMetadataPersistsForFreshResolve(t *testing.T) {
	installHetznerClaimState(t)
	server := normalizeHetznerServer(core.Server{ID: 42, Name: "crabbox-blue", Labels: map[string]string{"lease": "cbx_abcdef123456", "slug": "blue", "provider": "hetzner", "crabbox": "true", "created_by": "crabbox", "state": "ready", "expires_at": "9999999999", "tailscale_error": "old-error"}})
	seedHetznerClaim(t, server)
	fake := &fakeHetznerClient{servers: map[int64]core.Server{42: server}}
	old := newHetznerClient
	newHetznerClient = func() (hetznerClient, error) { return fake, nil }
	t.Cleanup(func() { newHetznerClient = old })
	cfg := core.Config{Provider: "hetzner"}
	backend := NewHetznerLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	claim, err := core.ReadLeaseClaim(server.Labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	writer, ok := any(backend).(core.TailscaleMetadataBackend)
	if !ok {
		t.Fatal("direct backend cannot persist metadata")
	}
	meta := core.TailscaleMetadata{Enabled: true, IPv4: "100.64.0.5", FQDN: "crabbox-blue.tail1234.ts.net.", Version: "1.104.2", DeviceID: "node123", State: "ready"}
	updated, err := writer.UpdateTailscaleMetadata(context.Background(), core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}, meta)
	if err != nil {
		t.Fatal(err)
	}
	labels := fake.labeledValues[0]
	fake.servers[42] = updated
	if labels["tailscale_ipv4"] != meta.IPv4 || labels["tailscale_fqdn"] != meta.FQDN || labels["tailscale_version"] != meta.Version || labels["tailscale_device_id"] != meta.DeviceID || labels["tailscale_state"] != "ready" {
		t.Fatalf("persisted labels=%v", labels)
	}
	if labels["state"] != server.Labels["state"] || labels["expires_at"] != server.Labels["expires_at"] {
		t.Fatal("metadata changed lifecycle fields")
	}
	if updated.Labels["tailscale_error"] != "" {
		t.Fatal("successful refresh retained stale diagnostic")
	}
	saved, err := core.ReadLeaseClaim(claim.LeaseID)
	if err != nil || saved.TailscaleIPv4 != meta.IPv4 {
		t.Fatalf("claim=%+v err=%v", saved, err)
	}
	fresh := NewHetznerLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	resolved, err := fresh.(core.SSHLeaseBackend).Resolve(context.Background(), core.ResolveRequest{ID: "42", ReleaseOnly: true})
	if err != nil || resolved.Server.Labels["tailscale_ipv4"] != meta.IPv4 {
		t.Fatalf("fresh resolve=%+v err=%v", resolved, err)
	}
}
