package parallels

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// loadFixedVM attests both the service and the VM before any guest access.
func (b *leaseBackend) loadFixedVM(ctx context.Context, claim core.LeaseClaim) (core.Config, *core.ParallelsClient, core.ParallelsVM, error) {
	intent := claim.FixedCreateIntent
	if !parallelsFixedLeaseKind.IsFixedClaim(claim) || intent.Version != parallelsFixedLeaseKind.IntentVersion ||
		intent.State != "acquired" || intent.ProviderScope != claim.ProviderScope ||
		claim.CloudID == "" || intent.Attempt["name"] != core.ParallelsLeaseVMName(claim.LeaseID, intent.Slug) {
		return core.Config{}, nil, core.ParallelsVM{}, core.Exit(4, "lease_id_conflict: Parallels lease %s has no acquired fixed VM; replay or stop its recorded intent", claim.LeaseID)
	}
	cfg, client, err := parallelsFixedHostConfig(ctx, b.RT.Exec, b.Cfg, claim.LeaseID, claim.ProviderScope)
	if err != nil {
		return cfg, client, core.ParallelsVM{}, err
	}
	vms, err := client.ListVMsDetailed(ctx)
	if err != nil {
		return cfg, client, core.ParallelsVM{}, fmt.Errorf("attest Parallels lease %s (claim retained): %w", claim.LeaseID, err)
	}
	vm, found := parallelsVMByID(vms, claim.CloudID)
	if !found {
		return cfg, client, vm, core.Exit(4, "lease_id_conflict: Parallels lease %s no longer has its bound VM; stop the lease", claim.LeaseID)
	}
	if err := validateParallelsFixedVM(claim, intent, vm, intent.Attempt["name"]); err != nil {
		return cfg, client, vm, err
	}
	return cfg, client, vm, nil
}

func (b *leaseBackend) resolveFixed(ctx context.Context, req core.ResolveRequest, claim core.LeaseClaim) (core.LeaseTarget, error) {
	if req.Repo.Root != "" && claim.RepoRoot != "" && req.Repo.Root != claim.RepoRoot && !req.Reclaim {
		return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels lease %s is bound to another repository", claim.LeaseID)
	}
	cfg, _, vm, err := b.loadFixedVM(ctx, claim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.TargetOS, cfg.WindowsMode = claim.TargetOS, claim.WindowsMode
	cfg.SSHUser, cfg.WorkRoot = claim.Labels["ssh_user"], claim.Labels["work_root"]
	if claim.SSHPort > 0 && claim.SSHPort <= 65535 {
		cfg.SSHPort, cfg.SSHFallbackPorts = strconv.Itoa(claim.SSHPort), nil
	}
	if vm.IP == "" && strings.EqualFold(vm.State, "running") {
		discovered, err := core.NewParallelsClient(cfg, b.RT.Exec).WaitForIP(ctx, vm.ID, 30*time.Second)
		if err != nil && !req.StatusOnly {
			return core.LeaseTarget{}, err
		}
		if err == nil {
			vm = discovered
		}
	}
	server := core.Server{CloudID: vm.ID, ImmutableID: vm.ID, Provider: parallelsProviderName, Name: vm.Name, Status: strings.ToLower(vm.State), Labels: maps.Clone(claim.Labels)}
	server.Labels["host"] = parallelsHostName(cfg)
	server.ServerType.Name = core.ServerTypeForProviderClass(parallelsProviderName, cfg.Class)
	server.PublicNet.IPv4.IP = vm.IP
	if vm.IPSource != "" {
		server.Labels["ip_source"] = vm.IPSource
	}
	target := parallelsLeaseSSHTarget(cfg, vm.IP)
	if err := core.UseStoredTestboxKey(&target, claim.LeaseID); err != nil {
		return core.LeaseTarget{}, err
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *leaseBackend) AuthorizeStatusTouchClaim(ctx context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
	if claim.LeaseID != lease.LeaseID || lease.Server.CloudID == "" || lease.Server.CloudID != claim.CloudID {
		return core.Exit(4, "Parallels lifecycle touch requires an exact lease claim")
	}
	if parallelsFixedLeaseKind.IsFixedClaim(claim) {
		_, _, _, err := b.loadFixedVM(ctx, claim)
		return err
	}
	if claim.Provider != parallelsProviderName || claim.ProviderScope != core.ProviderClaimScope(parallelsProviderName, b.Cfg) || claim.Labels["host"] != lease.Server.Labels["host"] {
		return core.Exit(4, "Parallels lifecycle touch ownership mismatch")
	}
	return nil
}
