package gcp

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"io"
	"testing"
)

func TestTailscaleMetadataPersistsForFreshResolve(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}
	server := canonicalGCPTestServer("cbx_abcdef123456", "blue")
	server.Labels["state"] = "ready"
	server.Labels["expires_at"] = "9999999999"
	server.Labels["tailscale_error"] = "old-error"
	claimGCPTestServer(t, cfg, server)
	fake := &fakeGCPDoctorClient{get: map[string]core.Server{server.CloudID: server}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })
	backend := NewGCPLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
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
	fake.get[server.CloudID] = updated
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
	fresh := NewGCPLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard})
	resolved, err := fresh.(core.SSHLeaseBackend).Resolve(context.Background(), core.ResolveRequest{ID: server.CloudID, ReleaseOnly: true})
	if err != nil || resolved.Server.Labels["tailscale_ipv4"] != meta.IPv4 {
		t.Fatalf("fresh resolve=%+v err=%v", resolved, err)
	}
}
