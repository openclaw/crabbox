package sprites

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Plain status must not wake a sleeping Sprite just to display its state, so it
// probes SSH only when the API already reports the Sprite running. --wait
// explicitly opts into an SSH probe in any state, never SSH installation.
func (b *spritesBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		return core.StatusView{}, err
	}
	server, target := lease.Server, lease.SSH
	view := core.StatusView{
		ID: lease.LeaseID, Slug: server.Labels["slug"], Provider: spritesProvider,
		TargetOS: targetLinux, State: server.Status, ServerID: server.CloudID,
		ProviderResourceID: server.Labels["sprites_resource_id"], ServerType: "sprite",
		Host: server.Name, Network: networkPublic, HasHost: target.Host != "",
		SSHHost: target.Host, SSHUser: target.User, SSHPort: target.Port, SSHKey: target.Key,
		Labels:      server.Labels,
		IdleTimeout: core.LeaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:   core.Blank(core.LeaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
	}
	if (req.Wait || server.Status == "running") && target.Key != "" {
		view.Ready = core.ProbeSSHReady(ctx, &target, 4*time.Second)
	}
	return view, nil
}
