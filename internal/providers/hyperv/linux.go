package hyperv

import (
	"context"
	"errors"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const linuxImageRequirements = "a generalized Debian or Ubuntu cloud VHDX with cloud-init, DHCP, and current Hyper-V integration services (including hv_kvp_daemon)"

func (b *backend) acquireLinux(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if hypervHostOS != "windows" {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s requires a Windows host with Hyper-V enabled", providerName)
	}
	cfg := b.configForRun()
	if cfg.HyperV.Image == "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s requires --hyperv-image (path to %s)", providerName, linuxImageRequirements)
	}
	if !strings.HasSuffix(strings.ToLower(cfg.HyperV.Image), ".vhdx") {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s target=linux requires a generalized cloud VHDX; QCOW2, RAW, ISO, and other image formats are not supported", providerName)
	}
	if cfg.HyperV.InitPassword {
		return core.LeaseTarget{}, core.Exit(2, "--hyperv-init-password is supported only for target=windows")
	}
	if !validLinuxSSHUser(cfg.HyperV.User) {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s target=linux --hyperv-user must be 1-32 characters, start with a lowercase letter or underscore, and contain only lowercase letters, digits, underscore, or hyphen", providerName)
	}
	if strings.TrimSpace(req.Repo.Root) == "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s requires a repository root so the VM claim can be persisted before bootstrap", providerName)
	}

	leaseID := core.NewLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := core.LeaseProviderName(leaseID, slug)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	markHyperVTailscaleLabels(labels, cfg)
	labels["instance"] = name
	labels["image"] = cfg.HyperV.Image
	labels["ssh_user"] = cfg.HyperV.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.HyperV.WorkRoot
	labels["state"] = "provisioning"
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s target=linux lease=%s slug=%s image=%s cpus=%d memory=%dMB switch=%s keep=%v\n",
		providerName, leaseID, slug, cfg.HyperV.Image, cfg.HyperV.CPUs, cfg.HyperV.Memory, cfg.HyperV.Switch, req.Keep)

	provisional := core.LeaseTarget{
		Server:  b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg),
		LeaseID: leaseID,
	}
	if err := persistLease(leaseID, slug, name, cfg, req, &provisional, nil); err != nil {
		return core.LeaseTarget{}, fmt.Errorf("persist hyperv lease before bootstrap: %w", err)
	}
	claim, _, _ = core.ServerLeaseClaimSnapshot(provisional.Server)
	cleanupKey = false
	var tailscaleCleanupTarget core.SSHTarget
	seedPath := cloudInitSeedPath(name)
	cleanupFailedLease := func() error {
		if req.Keep && !cfg.Tailscale.Enabled {
			return nil
		}
		if tailscaleCleanupTarget.Host != "" {
			b.logoutLinuxTailscaleBestEffort(tailscaleCleanupTarget)
		}
		seedErr := b.detachAndRemoveNoCloudSeed(context.Background(), name)
		if req.Keep {
			return seedErr
		}
		if err := b.removeVM(context.Background(), name); err != nil {
			return errors.Join(seedErr, fmt.Errorf("remove failed hyperv lease %s: %w", leaseID, err))
		}
		pruneLeaseState(leaseID)
		return seedErr
	}

	userData := core.CloudInitUserData(hypervTailscaleBootstrapConfig(cfg), publicKey)
	if err := b.createNoCloudSeed(ctx, seedPath, userData, cloudInitMetaData(name)); err != nil {
		return core.LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	if err := b.createLinuxVM(ctx, cfg, name, seedPath); err != nil {
		return core.LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	ip, err := b.waitForIP(ctx, name, 5*time.Minute)
	if err != nil {
		contractErr := fmt.Errorf("Linux IP discovery requires %s so Get-VMNetworkAdapter can report the DHCP address: %w", linuxImageRequirements, err)
		return core.LeaseTarget{}, errors.Join(contractErr, cleanupFailedLease())
	}
	if cfg.Tailscale.Enabled {
		tailscaleCleanupTarget = core.SSHTargetFromConfig(cfg, ip)
		tailscaleCleanupTarget.Port = sshPort
		tailscaleCleanupTarget.FallbackPorts = []string{}
	}
	lease, err := b.prepareLease(ctx, cfg, hypervVM{Name: name, State: 2}, ip, claim, true)
	if err != nil {
		return core.LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	if err := b.detachAndRemoveNoCloudSeed(ctx, name); err != nil {
		return core.LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	attachedCaches, err := b.attachConfiguredCacheVolumes(ctx, name, cfg, lease)
	if err != nil {
		return core.LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	if err := persistLease(leaseID, slug, name, cfg, req, &lease, core.CacheVolumeStickyDiskSpecs(attachedCaches)); err != nil {
		return core.LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s target=linux state=ready\n", leaseID, name)
	return lease, nil
}

func validLinuxSSHUser(user string) bool {
	if user != strings.TrimSpace(user) || len(user) == 0 || len(user) > 32 {
		return false
	}
	for i, r := range user {
		if i == 0 {
			if (r >= 'a' && r <= 'z') || r == '_' {
				continue
			}
			return false
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func (b *backend) createLinuxVM(ctx context.Context, cfg core.Config, name, seedPath string) error {
	vhdDir := hypervVHDDir()
	if err := os.MkdirAll(vhdDir, 0o755); err != nil {
		return core.Exit(2, "create VHD directory %s: %v", vhdDir, err)
	}
	vmDir := hypervVMDir()
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return core.Exit(2, "create VM directory %s: %v", vmDir, err)
	}
	vhdPath := filepath.Join(vhdDir, name+".vhdx")

	switchCheck := fmt.Sprintf(
		`if (-not (Get-VMSwitch -Name '%s' -ErrorAction SilentlyContinue)) { throw 'Hyper-V switch not found: %s' }`,
		escapePSString(cfg.HyperV.Switch), escapePSString(cfg.HyperV.Switch),
	)
	result, err := b.powershell(ctx, switchCheck)
	if err != nil {
		return shared.LocalCommandError("switch validation", result, err)
	}

	diffScript := fmt.Sprintf(
		`New-VHD -Path '%s' -ParentPath '%s' -Differencing -ErrorAction Stop | Out-Null`,
		escapePSString(vhdPath), escapePSString(cfg.HyperV.Image),
	)
	result, err = b.powershell(ctx, diffScript)
	if err != nil {
		os.Remove(vhdPath) //nolint:errcheck
		return shared.LocalCommandError("create differencing disk", result, err)
	}

	memBytes := int64(cfg.HyperV.Memory) * 1024 * 1024
	createScript := fmt.Sprintf(
		`New-VM -Name '%s' -MemoryStartupBytes %d -Generation 2 -VHDPath '%s' -Path '%s'`,
		escapePSString(name), memBytes, escapePSString(vhdPath), escapePSString(vmDir),
	)
	result, err = b.powershell(ctx, createScript)
	if err != nil {
		os.Remove(vhdPath) //nolint:errcheck
		return shared.LocalCommandError("New-VM", result, err)
	}

	cpuScript := fmt.Sprintf(`Set-VM -Name '%s' -ProcessorCount %d -AutomaticCheckpointsEnabled $false`, escapePSString(name), cfg.HyperV.CPUs)
	result, err = b.powershell(ctx, cpuScript)
	if err != nil {
		return shared.LocalCommandError("Set-VM", result, err)
	}
	if err := b.configureVMFirmware(ctx, cfg, name); err != nil {
		return err
	}

	if err := b.attachNoCloudSeed(ctx, name, seedPath); err != nil {
		return err
	}
	if err := b.connectVMNetwork(ctx, name, cfg.HyperV.Switch); err != nil {
		return err
	}

	startScript := fmt.Sprintf(`Start-VM -Name '%s'`, escapePSString(name))
	result, err = b.powershell(ctx, startScript)
	if err != nil {
		return shared.LocalCommandError("Start-VM", result, err)
	}
	return nil
}
