package scaleway

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

func TestScalewayReleaseGuestEndpointRequiresLiveServer(t *testing.T) {
	b, fake := newTestBackend(t)
	cfg := b.cfgForRun()
	const id = "cbx_abcdef123456"
	labels := core.DirectLeaseLabels(cfg, id, "guest", providerName, "", false, b.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	live := testServer("srv-guest", core.LeaseProviderName(id, "guest"), tagsFromLabels(labels), "192.0.2.70")
	fake.servers = []*instance.Server{live}
	fake.server = live
	server := b.serverFromScaleway(live)
	if err := core.ClaimLeaseTargetForConfig(id, "guest", cfg, server, core.SSHTarget{Host: "192.0.2.69"}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: id, ReleaseOnly: true})
	if err != nil || lease.SSH.Host != "192.0.2.70" {
		t.Fatalf("live endpoint=%s err=%v", lease.SSH.Host, err)
	}
	fake.servers = nil
	fake.server = nil
	fake.getErr = &scw.ResourceNotFoundError{}
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: id, ReleaseOnly: true})
	if err != nil || lease.SSH.Host != "" {
		t.Fatalf("absent endpoint=%s err=%v", lease.SSH.Host, err)
	}
}
