package utm

import (
	"context"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// Run admission already owns the exclusive claim fence. Reuse the same
// observation-only preparation as exec without taking the host lock.
func (b *backend) ResolveRunLeaseUnderClaim(ctx context.Context, req core.ResolveRequest, original core.LeaseClaim) (core.LeaseTarget, error) {
	return b.ResolveExecLeaseUnderClaim(ctx, req, original)
}

// ResolveExecLeaseUnderClaim runs under core's shared claim fence. It must not
// acquire utm-host.lock: release holds that lock while awaiting the exclusive
// claim fence. Read immutable ownership state and never publish/reenter claims.
func (b *backend) ResolveExecLeaseUnderClaim(ctx context.Context, req core.ResolveRequest, original core.LeaseClaim) (core.LeaseTarget, error) {
	var empty core.LeaseTarget
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if req.ID != original.LeaseID || !core.IsCanonicalLeaseID(req.ID) || original.Provider != "utm" || original.ProviderScope != (Provider{}).ClaimScope(b.cfg) || original.CloudID == "" || original.RepoRoot == "" || req.Repo.Root == "" || req.Reclaim || req.ReleaseOnly || req.StatusOnly {
		return empty, fmt.Errorf("UTM exec requires the exact completed resource-bound repository claim")
	}
	if err := core.CheckLeaseClaimRepositoryOwner(original.LeaseID, original, req.Repo.Root, false); err != nil {
		return empty, err
	}
	if err := shared.AuthorizeClaimActivity(original); err != nil {
		return empty, err
	}
	r, err := b.lookup(original.LeaseID)
	if err != nil {
		return empty, err
	}
	if r.VMID != original.CloudID || r.Slug != original.Slug || r.State != "ready" || !r.Configured {
		return empty, fmt.Errorf("UTM exec ownership journal does not match the completed claim")
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return empty, err
	}
	v, ok := findVM(vms, r.VMID)
	if !ok || v.Status != "started" {
		return empty, fmt.Errorf("UTM exec requires the owned VM to be running")
	}
	if err = validateClone(v, r); err != nil {
		return empty, err
	}
	lease, err := b.execLeaseTarget(ctx, r, vms)
	if err != nil {
		return empty, err
	}
	// The supplied immutable claim is authoritative while the fence is held.
	lease.Server.Labels = shared.CloneLabels(original.Labels)
	if err = validateExecTarget(lease, original, req.Options.TargetOS); err != nil {
		return empty, err
	}
	if err = core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity); err != nil {
		return empty, err
	}
	if err = waitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "UTM exec guest identity", b.cfg.UTM.StartupTimeout); err != nil {
		return empty, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, original, true)
	return lease, nil
}

// Native target extensions can refresh a local record copy here before producing
// access. This method must remain free of claim publication and host-lock waits.
func (b *backend) execLeaseTarget(ctx context.Context, r record, vms []vm) (core.LeaseTarget, error) {
	v, ok := findVM(vms, r.VMID)
	if !ok {
		return core.LeaseTarget{}, fmt.Errorf("UTM exec owned VM is absent")
	}
	if v.Backend == "apple" {
		native, ok := any(b).(interface {
			macOSExecLeaseTarget(context.Context, record, []vm) (core.LeaseTarget, error)
		})
		if !ok {
			return core.LeaseTarget{}, fmt.Errorf("UTM Apple guest exec requires the macOS continuing-access adapter")
		}
		return native.macOSExecLeaseTarget(ctx, r, vms)
	}
	if v.Backend != "qemu" {
		return core.LeaseTarget{}, fmt.Errorf("UTM exec backend %q is unsupported", v.Backend)
	}
	return b.target(r, vms), nil
}

func validateExecTarget(lease core.LeaseTarget, claim core.LeaseClaim, requestedTarget string) error {
	target := claim.TargetOS
	if target == "" {
		target = claim.Labels["target"]
	}
	if target != core.TargetLinux && target != core.TargetMacOS {
		return fmt.Errorf("UTM exec requires a Linux or macOS lease protocol")
	}
	if requestedTarget != "" && requestedTarget != target || lease.SSH.TargetOS != target || lease.Server.Labels["target"] != "" && lease.Server.Labels["target"] != target {
		return fmt.Errorf("UTM exec target protocol does not match the original claim")
	}
	if lease.LeaseID != claim.LeaseID || lease.Server.Provider != "utm" || lease.Server.CloudID != claim.CloudID || lease.SSH.AuthSecret || lease.SSH.Key == "" || lease.SSH.User == "" || lease.SSH.Host == "" || lease.SSH.Port == "" || lease.SSH.ReadyCheck == "" || !lease.SSH.NoControlMaster {
		return fmt.Errorf("UTM exec requires exact owned private SSH access with authenticated guest readiness")
	}
	return nil
}
