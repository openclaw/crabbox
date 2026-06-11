package hyperv

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var hypervHostOS = runtime.GOOS

const (
	hypervMissingState           = -1
	hypervProvisioningClaimGrace = time.Hour
)

type hypervVM struct {
	Name  string `json:"Name"`
	State int    `json:"State"`
}

type hypervNetAdapter struct {
	IPAddresses []string `json:"IPAddresses"`
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetWindows
	}
	if cfg.WindowsMode == "" {
		cfg.WindowsMode = core.WindowsModeNormal
	}
	cfg.SSHFallbackPorts = []string{}
	if cfg.HyperV.User == "" {
		if cfg.SSHUser != "" && cfg.SSHUser != "crabbox" {
			cfg.HyperV.User = cfg.SSHUser
		} else {
			cfg.HyperV.User = "crabbox"
		}
	}
	if cfg.HyperV.WorkRoot == "" {
		if !core.IsDefaultWorkRoot(cfg.WorkRoot) {
			cfg.HyperV.WorkRoot = cfg.WorkRoot
		} else {
			cfg.HyperV.WorkRoot = `C:\crabbox`
		}
	}
	if cfg.HyperV.CPUs <= 0 {
		cfg.HyperV.CPUs = 4
	}
	if cfg.HyperV.Memory <= 0 {
		cfg.HyperV.Memory = 8192
	}
	if cfg.HyperV.Switch == "" {
		cfg.HyperV.Switch = "Default Switch"
	}
	cfg.SSHUser = cfg.HyperV.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.HyperV.WorkRoot
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if hypervHostOS != "windows" {
		return LeaseTarget{}, exit(2, "provider=%s requires a Windows host with Hyper-V enabled", providerName)
	}
	cfg := b.configForRun()
	if cfg.HyperV.Image == "" {
		return LeaseTarget{}, exit(2, "provider=%s requires --hyperv-image (path to a Windows VHDX template with a known administrator password; the provider installs OpenSSH if missing)", providerName)
	}
	if strings.HasSuffix(strings.ToLower(cfg.HyperV.Image), ".iso") {
		return LeaseTarget{}, exit(2, "provider=%s does not support ISO images; provide a Windows VHDX template with a reachable administrator account", providerName)
	}
	if strings.TrimSpace(cfg.HyperV.GuestPassword) == "" {
		return LeaseTarget{}, exit(2, "provider=%s requires an explicit CRABBOX_HYPERV_GUEST_PASSWORD or hyperv.guestPassword in trusted user config", providerName)
	}
	if cfg.HyperV.InitPassword {
		// Both values land inside a double-quoted cmd.exe RunOnce command at
		// first boot: a double quote would break out of the quoting and a
		// percent sign would expand as a cmd variable, so reject either in
		// either value rather than emitting a command that does something
		// other than what was configured.
		if strings.ContainsAny(cfg.HyperV.GuestPassword, `"%`) {
			return LeaseTarget{}, exit(2, "provider=%s --hyperv-init-password sets the password through cmd.exe, which cannot carry double quotes or percent signs; choose a different CRABBOX_HYPERV_GUEST_PASSWORD", providerName)
		}
		if strings.ContainsAny(cfg.HyperV.User, `"%`) {
			return LeaseTarget{}, exit(2, "provider=%s --hyperv-init-password sets the password through cmd.exe, which cannot carry double quotes or percent signs in the user name; choose a different --hyperv-user", providerName)
		}
	}
	if !validHyperVSSHUser(cfg.HyperV.User) {
		return LeaseTarget{}, exit(2, "provider=%s --hyperv-user must be a local account name containing only letters, digits, dot, underscore, or hyphen", providerName)
	}
	if strings.TrimSpace(req.Repo.Root) == "" {
		return LeaseTarget{}, exit(2, "provider=%s requires a repository root so the VM claim can be persisted before bootstrap", providerName)
	}
	leaseID := newLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := make([]Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			removeStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := leaseProviderName(leaseID, slug)
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = cfg.HyperV.Image
	labels["ssh_user"] = cfg.HyperV.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.HyperV.WorkRoot
	labels["state"] = "provisioning"
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s cpus=%d memory=%dMB switch=%s keep=%v\n",
		providerName, leaseID, slug, cfg.HyperV.Image, cfg.HyperV.CPUs, cfg.HyperV.Memory, cfg.HyperV.Switch, req.Keep)

	provisional := LeaseTarget{
		Server:  b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg),
		LeaseID: leaseID,
	}
	if err := persistLease(leaseID, slug, name, cfg, req, provisional); err != nil {
		return LeaseTarget{}, fmt.Errorf("persist hyperv lease before bootstrap: %w", err)
	}
	cleanupKey = false
	if err := b.createVM(ctx, cfg, name); err != nil {
		cleanupErr := b.removeVM(context.Background(), name)
		if cleanupErr == nil {
			pruneLeaseState(leaseID)
		}
		return LeaseTarget{}, errors.Join(err, cleanupErr)
	}
	cleanupFailedLease := func() error {
		if req.Keep {
			return nil
		}
		if err := b.removeVM(context.Background(), name); err != nil {
			return fmt.Errorf("remove failed hyperv lease %s: %w", leaseID, err)
		}
		removeLeaseClaim(leaseID)
		removeStoredTestboxKey(leaseID)
		return nil
	}

	if err := b.stageSSHKey(ctx, name, cfg.HyperV.User, publicKey); err != nil {
		return LeaseTarget{}, errors.Join(fmt.Errorf("pre-network SSH lockdown failed: %w", err), cleanupFailedLease())
	}
	if err := b.connectVMNetwork(ctx, name, cfg.HyperV.Switch); err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}

	ip, err := b.waitForIP(ctx, name, 5*time.Minute)
	if err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}

	if err := b.ensureOpenSSH(ctx, name, cfg.HyperV.User); err != nil {
		return LeaseTarget{}, errors.Join(fmt.Errorf("guest OpenSSH setup failed: %w", err), cleanupFailedLease())
	}

	if publicKey != "" {
		if retryErr := b.injectSSHKey(ctx, name, cfg.HyperV.User, publicKey); retryErr != nil {
			return LeaseTarget{}, errors.Join(fmt.Errorf("post-boot SSH key injection failed: %w", retryErr), cleanupFailedLease())
		}
	}
	if err := b.ensureGit(ctx, name, cfg.HyperV.User); err != nil {
		return LeaseTarget{}, errors.Join(fmt.Errorf("guest git setup failed: %w", err), cleanupFailedLease())
	}
	lease, err := b.prepareLease(ctx, cfg, hypervVM{Name: name, State: 2}, ip, claim, true)
	if err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	if err := persistLease(leaseID, slug, name, cfg, req, lease); err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupFailedLease())
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
}

// persistLease records ownership before VM creation, then atomically updates
// the same claim with its SSH endpoint after bootstrap.
func persistLease(leaseID, slug, name string, cfg Config, req AcquireRequest, lease LeaseTarget) error {
	return claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH)
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	inst, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: claim.LeaseID}, nil
	}
	if inst.State == hypervMissingState {
		return LeaseTarget{}, exit(4, "hyperv VM %s from claim %s no longer exists; run `crabbox stop --provider hyperv %s` to prune local lease state", inst.Name, claim.LeaseID, claim.LeaseID)
	}
	if claim.LeaseID == "" {
		return LeaseTarget{}, exit(4, "hyperv instance %q has no Crabbox lease claim; use `crabbox stop --provider hyperv %s` to delete it or warm a new lease", inst.Name, inst.Name)
	}
	if req.StatusOnly && !req.ReadyProbe {
		return LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: claim.LeaseID}, nil
	}
	ip := b.queryLiveIP(ctx, inst.Name)
	if ip == "" {
		ip = b.getIPFromClaim(claim)
	}
	lease, err := b.prepareLease(ctx, cfg, inst, ip, claim, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := providerClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(instances))
	for _, inst := range instances {
		claim := claims[inst.Name]
		if claim.LeaseID == "" && !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		views = append(views, b.serverFromInstance(inst, claim, cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	if hypervHostOS != "windows" {
		return DoctorResult{}, exit(2, "provider=%s requires a Windows host", providerName)
	}
	script := `(Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V).State`
	result, err := b.powershell(ctx, script)
	if err != nil {
		return DoctorResult{}, commandError("hyperv feature check", result, err)
	}
	state := strings.TrimSpace(result.Stdout)
	instances, err := b.listInstances(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	cfg := b.configForRun()
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	msg := fmt.Sprintf("hyperv=%s control_plane=local inventory=ready api=powershell mutation=false leases=%d image=%s ssh_probe=%s",
		firstLine(state), len(instances), cfg.HyperV.Image, probe)
	return DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	lease := req.Lease
	if lease.LeaseID == "" {
		lease.LeaseID = strings.TrimSpace(lease.Server.Labels["lease"])
	}
	name := strings.TrimSpace(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]))
	if name == "" && lease.LeaseID != "" {
		inst, _, err := b.resolveInstance(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		name = inst.Name
	}
	if name == "" {
		return exit(2, "provider=%s release requires a Hyper-V VM name", providerName)
	}
	if err := b.removeVM(ctx, name); err != nil {
		return err
	}
	if lease.LeaseID != "" {
		pruneLeaseState(lease.LeaseID)
	}
	return nil
}

func pruneLeaseState(leaseID string) {
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if errors.Is(err, errNotWindows) {
		fmt.Fprintf(b.rt.Stderr, "skip cleanup: %v\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	claims, err := providerClaims()
	if err != nil {
		return err
	}
	live := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, inst := range instances {
		claim := claims[inst.Name]
		if claim.LeaseID != "" {
			live[claim.LeaseID] = struct{}{}
		}
		server := b.serverFromInstance(inst, claim, cfg)
		shouldDelete, reason := shouldCleanup(server, claim, claim.LeaseID != "", now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%s\n", inst.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
		if err := b.removeVM(ctx, inst.Name); err != nil {
			return err
		}
		if claim.LeaseID != "" {
			removeLeaseClaim(claim.LeaseID)
			removeStoredTestboxKey(claim.LeaseID)
		}
		removed++
	}
	claimsRemoved := 0
	for _, claim := range claims {
		if claim.LeaseID == "" {
			continue
		}
		if _, ok := live[claim.LeaseID]; ok {
			continue
		}
		if ready, reason := missingClaimCleanupReady(claim, now); !ready {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s slug=%s reason=%s\n", claim.LeaseID, blank(claim.Slug, "-"), reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
		if name := instanceNameFromClaim(claim); name != "" {
			if err := b.removeVMStorage(name, nil); err != nil {
				return err
			}
		}
		pruneLeaseState(claim.LeaseID)
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(instances))
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = touchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	for _, key := range []string{"image", "instance", "ssh_user", "ssh_port", "work_root"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

// createVM creates and starts a disconnected Hyper-V VM. Acquire configures
// guest SSH over PowerShell Direct before connecting the network adapter.
func (b *backend) createVM(ctx context.Context, cfg Config, name string) error {
	vhdDir := hypervVHDDir()
	if err := os.MkdirAll(vhdDir, 0o755); err != nil {
		return exit(2, "create VHD directory %s: %v", vhdDir, err)
	}
	vmDir := hypervVMDir()
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return exit(2, "create VM directory %s: %v", vmDir, err)
	}
	vhdPath := filepath.Join(vhdDir, name+".vhdx")

	switchCheck := fmt.Sprintf(
		`if (-not (Get-VMSwitch -Name '%s' -ErrorAction SilentlyContinue)) { throw 'Hyper-V switch not found: %s' }`,
		escapePSString(cfg.HyperV.Switch), escapePSString(cfg.HyperV.Switch),
	)
	result, err := b.powershell(ctx, switchCheck)
	if err != nil {
		return commandError("switch validation", result, err)
	}

	memBytes := int64(cfg.HyperV.Memory) * 1024 * 1024

	// Back each lease with a differencing disk over the template instead of
	// copying the whole VHDX. Creating the child is near-instant and space-thin
	// (the lease only stores its own writes); the template stays read-only and is
	// shared across leases. The lease inherits the template's virtual size --
	// size the template to size the lease. On release only this child disk is
	// deleted; the template is left untouched.
	diffScript := fmt.Sprintf(
		`New-VHD -Path '%s' -ParentPath '%s' -Differencing -ErrorAction Stop | Out-Null`,
		escapePSString(vhdPath), escapePSString(cfg.HyperV.Image),
	)
	result, err = b.powershell(ctx, diffScript)
	if err != nil {
		os.Remove(vhdPath) //nolint:errcheck
		return commandError("create differencing disk", result, err)
	}

	if cfg.HyperV.InitPassword {
		if err := b.injectInitPassword(ctx, vhdPath, cfg.HyperV.User); err != nil {
			os.Remove(vhdPath) //nolint:errcheck
			return err
		}
	}

	createScript := fmt.Sprintf(
		`New-VM -Name '%s' -MemoryStartupBytes %d -Generation 2 -VHDPath '%s' -Path '%s'`,
		escapePSString(name), memBytes, escapePSString(vhdPath), escapePSString(vmDir),
	)
	result, err = b.powershell(ctx, createScript)
	if err != nil {
		os.Remove(vhdPath) //nolint:errcheck
		return commandError("New-VM", result, err)
	}

	// Disable automatic checkpoints: client Hyper-V enables them by default, which
	// makes Start-VM create a <name>_<guid>.avhdx differencing disk and attach it
	// in place of the base VHDX. That defeats removeVM's disk cleanup (it matches
	// the base <name>.vhdx) and leaks a disk-sized file on release. Lease VMs are
	// ephemeral and have no use for checkpoints.
	cpuScript := fmt.Sprintf(`Set-VM -Name '%s' -ProcessorCount %d -AutomaticCheckpointsEnabled $false`, escapePSString(name), cfg.HyperV.CPUs)
	result, err = b.powershell(ctx, cpuScript)
	if err != nil {
		return commandError("Set-VM", result, err)
	}

	startScript := fmt.Sprintf(`Start-VM -Name '%s'`, escapePSString(name))
	result, err = b.powershell(ctx, startScript)
	if err != nil {
		return commandError("Start-VM", result, err)
	}

	return nil
}

func (b *backend) connectVMNetwork(ctx context.Context, name, switchName string) error {
	script := fmt.Sprintf(
		`Connect-VMNetworkAdapter -VMName '%s' -SwitchName '%s'`,
		escapePSString(name), escapePSString(switchName),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return commandError("Connect-VMNetworkAdapter", result, err)
	}
	return nil
}

func (b *backend) guestPassword() string {
	return b.cfg.HyperV.GuestPassword
}

// injectInitPassword makes a password-less template (e.g. a stock Windows
// dev-environment VHDX, which auto-logs-on with no password set) usable:
// PowerShell Direct refuses empty credentials, so before first boot we mount
// the per-lease differencing disk, load its offline SOFTWARE hive, and write a
// RunOnce command that sets the guest account password at the template's
// auto-logon. Only the lease disk is modified -- the template stays untouched.
// The password reaches this host script via the _CRABBOX_GP env var, never on
// a command line; inside the guest it is visible to the guest itself (RunOnce
// value, then the net.exe command line at first logon), which the lease owns.
func (b *backend) injectInitPassword(ctx context.Context, vhdPath, user string) error {
	hiveName := hypervInitHiveName(vhdPath)
	script := fmt.Sprintf(
		`$ErrorActionPreference = 'Stop'; `+
			`$disk = Mount-VHD -Path '%s' -Passthru | Get-Disk; `+
			`try { `+
			`if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false }; `+
			`$letters = ($disk | Get-Partition | Where-Object DriveLetter).DriveLetter; `+
			`$sys = $letters | Where-Object { Test-Path ("$_" + ':\Windows\System32\config\SOFTWARE') } | Select-Object -First 1; `+
			`if (-not $sys) { throw 'no Windows system volume found in template' }; `+
			`reg.exe load HKLM\%s ("$sys" + ':\Windows\System32\config\SOFTWARE') | Out-Null; `+
			`if ($LASTEXITCODE -ne 0) { throw 'loading the offline SOFTWARE hive failed' }; `+
			`try { `+
			`$runOnce = 'HKLM:\%s\Microsoft\Windows\CurrentVersion\RunOnce'; `+
			`if (-not (Test-Path $runOnce)) { New-Item -Path $runOnce -Force | Out-Null }; `+
			`New-ItemProperty -Path $runOnce -Name 'CrabboxInitPassword' -PropertyType String -Force -Value ('cmd /c net user "%s" "' + $env:_CRABBOX_GP + '" /y') | Out-Null `+
			`} finally { `+
			`[gc]::Collect(); [gc]::WaitForPendingFinalizers(); `+
			`reg.exe unload HKLM\%s | Out-Null `+
			`} `+
			`} finally { Dismount-VHD -Path '%s' }`,
		escapePSString(vhdPath), hiveName, hiveName, escapePSString(user), hiveName, escapePSString(vhdPath),
	)
	env := append(os.Environ(), "_CRABBOX_GP="+b.guestPassword())
	result, err := b.powershellWithEnv(ctx, script, env)
	if err != nil {
		return commandError("init-password injection", result, err)
	}
	return nil
}

func hypervInitHiveName(vhdPath string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(vhdPath))))
	return fmt.Sprintf("crabbox-init-%x", sum[:8])
}

// invokeInGuest runs a PowerShell script block inside the guest over PowerShell
// Direct, authenticating as user with the configured guest password. It retries
// with backoff while the guest finishes booting. scriptBlock is the body of an
// Invoke-Command -ScriptBlock { ... }; the guest password is passed via the
// _CRABBOX_GP env var, never on the command line.
func (b *backend) invokeInGuest(ctx context.Context, vmName, user, scriptBlock, label string) error {
	script := fmt.Sprintf(
		`$cred = New-Object PSCredential('%s', (ConvertTo-SecureString $env:_CRABBOX_GP -AsPlainText -Force)); `+
			`Invoke-Command -VMName '%s' -Credential $cred -ScriptBlock { %s }`,
		escapePSString(user), escapePSString(vmName), scriptBlock,
	)
	env := append(os.Environ(), "_CRABBOX_GP="+b.guestPassword())
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
			fmt.Fprintf(b.rt.Stderr, "retrying %s (%d/5)...\n", label, attempt+1)
		}
		result, err := b.powershellWithEnv(ctx, script, env)
		if err == nil {
			return nil
		}
		lastErr = commandError(label, result, err)
	}
	return lastErr
}

// ensureOpenSSH installs the Windows OpenSSH server inside the guest, but keeps
// sshd stopped and its firewall rule disabled until injectSSHKey replaces the
// template credentials and host keys. This lets a plain Windows template be
// used as-is: it only needs a reachable administrator account
// (CRABBOX_HYPERV_GUEST_PASSWORD), not a pre-baked SSH setup. Idempotent;
// installing the capability needs guest internet (or a configured
// Features-on-Demand source).
func (b *backend) ensureOpenSSH(ctx context.Context, vmName, user string) error {
	scriptBlock := `$ErrorActionPreference='Stop'; ` +
		`$capName = 'OpenSSH.Server~~~~0.0.1.0'; ` +
		`$cap = Get-WindowsCapability -Online -Name $capName; ` +
		`if ($cap.State -ne 'Installed') { Add-WindowsCapability -Online -Name $capName | Out-Null }; ` +
		`Stop-Service -Name sshd -Force -ErrorAction SilentlyContinue; ` +
		`Set-Service -Name sshd -StartupType Manual; ` +
		`if (Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue) { ` +
		`Disable-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' | Out-Null }`
	return b.invokeInGuest(ctx, vmName, user, scriptBlock, "OpenSSH install")
}

// MinGit release pinned for the guest git bootstrap. The download runs inside
// the guest and lands in Program Files + machine PATH, so it must not float
// with "latest": a changed release response or substituted archive would be
// privileged guest code execution. Update the URL and SHA-256 together; the
// hash is the official checksum from the git-for-windows release notes for
// this exact asset.
const (
	minGitURL    = "https://github.com/git-for-windows/git/releases/download/v2.54.0.windows.1/MinGit-2.54.0-64-bit.zip"
	minGitSHA256 = "04f937e1f0918b17b9be6f2294cb2bb66e96e1d9832d1c298e2de088a1d0e668"
)

// ensureGit installs git in the guest when it is absent, so a plain Windows
// template (only a known admin password) satisfies Crabbox's Windows readiness
// check, which requires git for sync -- mirroring how the Linux cloud-init path
// installs git. Idempotent: a no-op when git is already on PATH (so a template
// that pre-bakes git skips the per-lease download). Uses the pinned MinGit
// release above, SHA-256-verified before extraction; needs guest internet.
//
// MinGit must NOT be extracted to C:\Program Files\Git: MinGit's etc\gitconfig
// deliberately includes C:/Program Files/Git/etc/gitconfig (to inherit a full
// Git-for-Windows install's system config), so extracting it there makes the
// include self-referential and every git command fails with "exceeded maximum
// include depth". At C:\Program Files\MinGit the include points at a missing
// file, which git ignores.
func (b *backend) ensureGit(ctx context.Context, vmName, user string) error {
	scriptBlock := `$ErrorActionPreference='Stop'; ` +
		`if (Get-Command git -ErrorAction SilentlyContinue) { return }; ` +
		`[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12; ` +
		`$zip=Join-Path $env:TEMP 'crabbox-mingit.zip'; ` +
		`Invoke-WebRequest -UseBasicParsing -Uri '` + minGitURL + `' -OutFile $zip; ` +
		`$hash=(Get-FileHash -Path $zip -Algorithm SHA256).Hash; ` +
		`if ($hash -ne '` + minGitSHA256 + `') { Remove-Item $zip -Force; throw ('MinGit SHA-256 mismatch: got ' + $hash) }; ` +
		`$dst='C:\Program Files\MinGit'; ` +
		`Expand-Archive -Path $zip -DestinationPath $dst -Force; ` +
		`$cmd=Join-Path $dst 'cmd'; ` +
		`$p=[Environment]::GetEnvironmentVariable('PATH','Machine'); ` +
		`if ($p -notlike ('*'+$cmd+'*')) { [Environment]::SetEnvironmentVariable('PATH',($p+';'+$cmd),'Machine') }; ` +
		`Restart-Service sshd`
	return b.invokeInGuest(ctx, vmName, user, scriptBlock, "git install")
}

func sshAccessScript(user, publicKey string, activate bool) string {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`if (Get-Service -Name sshd -ErrorAction SilentlyContinue) { `+
			`Stop-Service -Name sshd -Force -ErrorAction SilentlyContinue; `+
			`Set-Service -Name sshd -StartupType Disabled }; `+
			`if (Get-NetFirewallRule -Name 'Crabbox-SSH-Quarantine' -ErrorAction SilentlyContinue) { `+
			`Enable-NetFirewallRule -Name 'Crabbox-SSH-Quarantine' | Out-Null `+
			`} else { `+
			`New-NetFirewallRule -Name 'Crabbox-SSH-Quarantine' -DisplayName 'Crabbox SSH quarantine' -Enabled True -Direction Inbound -Protocol TCP -Action Block -LocalPort 22 | Out-Null }; `+
			`if (Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue) { `+
			`Disable-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' | Out-Null }; `+
			`$sshDir = Join-Path $env:USERPROFILE '.ssh'; `+
			`New-Item -ItemType Directory -Force -Path $sshDir | Out-Null; `+
			`$akPath = Join-Path $sshDir 'authorized_keys'; `+
			`Set-Content -Encoding ASCII -Path $akPath -Value '%s'; `+
			`$hostKeyDir = Join-Path $env:ProgramData 'ssh'; `+
			`New-Item -ItemType Directory -Force -Path $hostKeyDir | Out-Null; `+
			`$adminAK = Join-Path $env:ProgramData 'ssh\administrators_authorized_keys'; `+
			`Set-Content -Encoding ASCII -Path $adminAK -Value '%s'; `+
			`icacls.exe $adminAK /inheritance:r /grant '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null; `+
			`if ($LASTEXITCODE -ne 0) { throw 'administrators_authorized_keys ACL update failed' }; `+
			`$sshdConfig = Join-Path $env:ProgramData 'ssh\sshd_config'; `+
			`$sshdLines = if (Test-Path $sshdConfig) { Get-Content $sshdConfig } else { @() }; `+
			`$globalLines = @(); `+
			`foreach ($line in $sshdLines) { `+
			`if ($line -match '^\s*Match\s+') { break }; `+
			`if ($line -match '^\s*(Port|ListenAddress|AddressFamily|HostKey|AuthenticationMethods|PasswordAuthentication|PubkeyAuthentication|KbdInteractiveAuthentication|AllowUsers|AllowGroups|DenyUsers|DenyGroups|AuthorizedKeysFile|AuthorizedKeysCommand|AuthorizedKeysCommandUser|TrustedUserCAKeys|AuthorizedPrincipalsFile|Include)\s+') { continue }; `+
			`$globalLines += $line }; `+
			`$globalLines += 'Port 22'; `+
			`$globalLines += 'AddressFamily any'; `+
			`$globalLines += 'PubkeyAuthentication yes'; `+
			`$globalLines += 'PasswordAuthentication no'; `+
			`$globalLines += 'AuthenticationMethods publickey'; `+
			`$globalLines += 'AuthorizedKeysFile .ssh/authorized_keys'; `+
			`$globalLines += 'AllowUsers %s'; `+
			`$globalLines += 'Match Group administrators'; `+
			`$globalLines += '       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys'; `+
			`Set-Content -Encoding ASCII -Path $sshdConfig -Value ($globalLines -join [Environment]::NewLine); `,
		escapePSString(publicKey), escapePSString(publicKey), escapePSString(strings.ToLower(user)),
	)
	if !activate {
		return script
	}
	return script +
		`Get-ChildItem -Path $hostKeyDir -Filter 'ssh_host_*' -ErrorAction SilentlyContinue | Remove-Item -Force; ` +
		`$sshKeygen = Join-Path $env:WINDIR 'System32\OpenSSH\ssh-keygen.exe'; ` +
		`& $sshKeygen -A; ` +
		`if ($LASTEXITCODE -ne 0) { throw 'SSH host key generation failed' }; ` +
		`$systemSID = [System.Security.Principal.SecurityIdentifier]::new('S-1-5-18'); ` +
		`$adminsSID = [System.Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'); ` +
		`Get-ChildItem -Path $hostKeyDir -Filter 'ssh_host_*_key' | ForEach-Object { ` +
		`$hostKeyACL = New-Object System.Security.AccessControl.FileSecurity; ` +
		`$hostKeyACL.SetOwner($adminsSID); ` +
		`$hostKeyACL.SetAccessRuleProtection($true, $false); ` +
		`$hostKeyACL.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($systemSID, [System.Security.AccessControl.FileSystemRights]::FullControl, [System.Security.AccessControl.AccessControlType]::Allow)); ` +
		`$hostKeyACL.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($adminsSID, [System.Security.AccessControl.FileSystemRights]::FullControl, [System.Security.AccessControl.AccessControlType]::Allow)); ` +
		`Set-Acl -LiteralPath $_.FullName -AclObject $hostKeyACL }; ` +
		`$sshdExe = Join-Path $env:WINDIR 'System32\OpenSSH\sshd.exe'; ` +
		`& $sshdExe -t -f $sshdConfig; ` +
		`if ($LASTEXITCODE -ne 0) { throw 'sshd_config validation failed' }; ` +
		`Set-Service -Name sshd -StartupType Automatic; ` +
		`Start-Service sshd; ` +
		`if (Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue) { ` +
		`Enable-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' | Out-Null ` +
		`} else { ` +
		`New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null }; ` +
		`Remove-NetFirewallRule -Name 'Crabbox-SSH-Quarantine' -ErrorAction Stop`
}

// stageSSHKey replaces template credentials while the VM is disconnected. The
// quarantine rule remains active across capability installation and any guest
// service restart until injectSSHKey completes.
func (b *backend) stageSSHKey(ctx context.Context, vmName, user, publicKey string) error {
	return b.invokeInGuest(ctx, vmName, user, sshAccessScript(user, publicKey, false), "pre-network SSH lockdown")
}

// injectSSHKey repeats the credential lockdown after OpenSSH installation,
// validates the final config, rotates host keys, starts sshd, and removes the
// firewall quarantine last.
func (b *backend) injectSSHKey(ctx context.Context, vmName, user, publicKey string) error {
	scriptBlock := sshAccessScript(user, publicKey, true)
	return b.invokeInGuest(ctx, vmName, user, scriptBlock, "SSH key injection")
}

func validHyperVSSHUser(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		return false
	}
	for _, r := range user {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '.', '_', '-':
			continue
		default:
			return false
		}
	}
	return true
}

// waitForIP polls Get-VMNetworkAdapter until an IPv4 address appears.
func (b *backend) waitForIP(ctx context.Context, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	script := fmt.Sprintf(
		`Get-VMNetworkAdapter -VMName '%s' | Select-Object -ExpandProperty IPAddresses | ConvertTo-Json`,
		escapePSString(name),
	)
	for {
		if time.Now().After(deadline) {
			return "", exit(5, "hyperv VM %s did not acquire an IP within %s", name, timeout)
		}
		result, err := b.powershell(ctx, script)
		if err == nil && strings.TrimSpace(result.Stdout) != "" && strings.TrimSpace(result.Stdout) != "null" {
			ip := parseFirstIPv4(result.Stdout)
			if ip != "" {
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

var errNotWindows = fmt.Errorf("hyper-v inventory unavailable: host OS is %s (not windows)", hypervHostOS)

func (b *backend) listInstances(ctx context.Context) ([]hypervVM, error) {
	if hypervHostOS != "windows" {
		return nil, errNotWindows
	}
	script := `Get-VM | Where-Object { $_.Name -like 'crabbox-*' } | Select-Object Name, State | ConvertTo-Json -Compress`
	result, err := b.powershell(ctx, script)
	if err != nil {
		return nil, commandError("Get-VM list", result, err)
	}
	stdout := strings.TrimSpace(result.Stdout)
	if stdout == "" || stdout == "null" {
		return nil, nil
	}
	return parseVMList(stdout)
}

func (b *backend) resolveInstance(ctx context.Context, identifier string) (hypervVM, core.LeaseClaim, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return hypervVM{}, core.LeaseClaim{}, exit(2, "provider=%s requires --id <lease-id-or-slug-or-instance>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return hypervVM{}, core.LeaseClaim{}, err
	} else if ok {
		name := instanceNameFromClaim(claim)
		if name == "" {
			return hypervVM{}, core.LeaseClaim{}, exit(4, "hyperv lease %s has no instance name in its claim", claim.LeaseID)
		}
		vm, queryErr := b.queryVM(ctx, name)
		if queryErr != nil {
			return hypervVM{}, claim, exit(4, "hyperv VM %s from claim %s not reachable: %v", name, claim.LeaseID, queryErr)
		}
		return vm, claim, nil
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return hypervVM{}, core.LeaseClaim{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return hypervVM{}, core.LeaseClaim{}, err
	}
	normalized := normalizeLeaseSlug(identifier)
	for _, inst := range instances {
		claim := claims[inst.Name]
		if inst.Name == identifier || claim.LeaseID == identifier || (normalized != "" && normalizeLeaseSlug(claim.Slug) == normalized) {
			return inst, claim, nil
		}
	}
	return hypervVM{}, core.LeaseClaim{}, exit(4, "hyperv lease not found: %s", identifier)
}

func (b *backend) prepareLease(ctx context.Context, cfg Config, inst hypervVM, ip string, claim core.LeaseClaim, wait bool) (LeaseTarget, error) {
	server := b.serverFromInstance(inst, claim, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" {
		cfg.HyperV.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.HyperV.WorkRoot = root
		cfg.WorkRoot = root
	}
	if ip == "" {
		return LeaseTarget{}, exit(5, "hyperv instance %s has no IPv4 address", inst.Name)
	}
	server.PublicNet.IPv4.IP = ip
	if claim.LeaseID != "" {
		keyPath, err := testboxKeyPath(claim.LeaseID)
		if err == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				cfg.SSHKey = keyPath
			}
		}
	}
	target := sshTargetFromConfig(cfg, ip)
	target.Port = sshPort
	target.FallbackPorts = []string{}
	if wait {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "hyperv ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *backend) removeVM(ctx context.Context, name string) error {
	if !validHyperVVMName(name) {
		return exit(2, "refusing to remove non-Crabbox Hyper-V VM %q", name)
	}

	vm, err := b.queryVM(ctx, name)
	if err != nil {
		return err
	}
	var vhdPaths []string
	if vm.State != hypervMissingState {
		vhdPaths = b.queryVHDPaths(ctx, name)
		stopScript := fmt.Sprintf(`Stop-VM -Name '%s' -Force -Confirm:$false -ErrorAction SilentlyContinue`, escapePSString(name))
		b.powershell(ctx, stopScript) //nolint:errcheck

		removeScript := fmt.Sprintf(`Remove-VM -Name '%s' -Force -Confirm:$false`, escapePSString(name))
		result, removeErr := b.powershell(ctx, removeScript)
		if removeErr != nil {
			return commandError("Remove-VM", result, removeErr)
		}
	}
	return b.removeVMStorage(name, vhdPaths)
}

func (b *backend) removeVMStorage(name string, attachedPaths []string) error {
	if !validHyperVVMName(name) {
		return exit(2, "refusing to remove storage for invalid Crabbox Hyper-V VM %q", name)
	}
	var errs []error
	vhdDir := hypervVHDDir()
	expectedVHD := filepath.Join(vhdDir, name+".vhdx")
	if err := b.removeVHDFile(expectedVHD); err != nil {
		errs = append(errs, err)
	}
	for _, p := range attachedPaths {
		clean := filepath.Clean(p)
		if strings.EqualFold(clean, filepath.Clean(expectedVHD)) {
			continue
		}
		if ownedHyperVCheckpoint(clean, vhdDir, name) {
			if err := b.removeVHDFile(p); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if entries, err := os.ReadDir(vhdDir); err == nil {
		for _, entry := range entries {
			path := filepath.Join(vhdDir, entry.Name())
			if ownedHyperVCheckpoint(path, vhdDir, name) {
				if err := b.removeVHDFile(path); err != nil {
					errs = append(errs, err)
				}
			}
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("read Hyper-V VHD directory %s: %w", vhdDir, err))
	}
	if err := removeHyperVConfigFiles(filepath.Join(hypervVMDir(), name)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validHyperVVMName(name string) bool {
	if name != strings.TrimSpace(name) || len(name) <= len("crabbox-") || len(name) > 64 || !strings.HasPrefix(name, "crabbox-") {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return name[len(name)-1] != '-'
}

func ownedHyperVCheckpoint(path, vhdDir, name string) bool {
	clean := filepath.Clean(path)
	if !strings.EqualFold(filepath.Dir(clean), filepath.Clean(vhdDir)) {
		return false
	}
	base := filepath.Base(clean)
	ext := filepath.Ext(base)
	if !strings.EqualFold(ext, ".avhdx") {
		return false
	}
	stem := strings.TrimSuffix(base, ext)
	prefix := name + "_"
	if len(stem) != len(prefix)+36 || !strings.EqualFold(stem[:len(prefix)], prefix) {
		return false
	}
	guid := stem[len(prefix):]
	for i, r := range guid {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

func (b *backend) removeVHDFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove lease VHDX %s: %w", path, err)
	}
	return nil
}

func removeHyperVConfigFiles(root string) error {
	var dirs []string
	var errs []error
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			errs = append(errs, walkErr)
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if isVirtualDiskFile(path) {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove Hyper-V config file %s: %w", path, err))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !os.IsNotExist(err) {
			entries, readErr := os.ReadDir(dirs[i])
			if readErr == nil && len(entries) > 0 {
				continue
			}
			errs = append(errs, fmt.Errorf("remove Hyper-V config directory %s: %w", dirs[i], err))
		}
	}
	return errors.Join(errs...)
}

func isVirtualDiskFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".vhd", ".vhdx", ".avhd", ".avhdx", ".vhds":
		return true
	default:
		return false
	}
}

func (b *backend) queryVM(ctx context.Context, name string) (hypervVM, error) {
	script := fmt.Sprintf(`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' } | Select-Object Name, State | ConvertTo-Json -Compress`, escapePSString(name))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return hypervVM{}, commandError("Get-VM query", result, err)
	}
	stdout := strings.TrimSpace(result.Stdout)
	if stdout == "" || stdout == "null" {
		return hypervVM{Name: name, State: hypervMissingState}, nil
	}
	var vm hypervVM
	if err := json.Unmarshal([]byte(stdout), &vm); err != nil {
		return hypervVM{}, exit(2, "parse hyperv VM %s: %v", name, err)
	}
	return vm, nil
}

func (b *backend) queryLiveIP(ctx context.Context, name string) string {
	script := fmt.Sprintf(
		`Get-VMNetworkAdapter -VMName '%s' | Select-Object -ExpandProperty IPAddresses | ConvertTo-Json`,
		escapePSString(name),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return ""
	}
	return parseFirstIPv4(result.Stdout)
}

func (b *backend) queryVHDPaths(ctx context.Context, name string) []string {
	script := fmt.Sprintf(
		`Get-VMHardDiskDrive -VMName '%s' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path`,
		escapePSString(name),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

func (b *backend) serverFromInstance(inst hypervVM, claim core.LeaseClaim, cfg Config) Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	if labels["crabbox"] == "" {
		labels["crabbox"] = "true"
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["instance"] == "" {
		labels["instance"] = inst.Name
	}
	if labels["lease"] == "" {
		labels["lease"] = claim.LeaseID
	}
	if labels["slug"] == "" {
		labels["slug"] = claim.Slug
	}
	liveState := hypervState(inst.State)
	if inst.State != 2 || labels["state"] == "" {
		labels["state"] = liveState
	}
	if labels["image"] == "" {
		labels["image"] = cfg.HyperV.Image
	}
	if labels["ssh_user"] == "" {
		labels["ssh_user"] = cfg.HyperV.User
	}
	if labels["ssh_port"] == "" {
		labels["ssh_port"] = sshPort
	}
	if labels["work_root"] == "" {
		labels["work_root"] = cfg.HyperV.WorkRoot
	}
	status := liveState
	if inst.State == 2 && labels["state"] == "ready" {
		status = "ready"
	}
	server := Server{
		CloudID:  inst.Name,
		Provider: providerName,
		Name:     inst.Name,
		Status:   status,
		Labels:   labels,
	}
	server.ServerType.Name = "hyperv"
	if claim.SSHHost != "" {
		server.PublicNet.IPv4.IP = claim.SSHHost
	}
	return server
}

func (b *backend) getIPFromClaim(claim core.LeaseClaim) string {
	if claim.SSHHost != "" {
		return claim.SSHHost
	}
	return ""
}

func (b *backend) powershell(ctx context.Context, script string) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: "powershell",
		Args: []string{"-NoProfile", "-NonInteractive", "-Command", script},
	})
}

func (b *backend) powershellWithEnv(ctx context.Context, script string, env []string) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: "powershell",
		Args: []string{"-NoProfile", "-NonInteractive", "-Command", script},
		Env:  env,
	})
}

func providerClaims() (map[string]core.LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		name := instanceNameFromClaim(claim)
		if name == "" {
			continue
		}
		out[name] = claim
	}
	return out, nil
}

func instanceScope(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return "instance:" + name
}

func instanceNameFromClaim(claim core.LeaseClaim) string {
	if name := strings.TrimSpace(claim.Labels["instance"]); name != "" {
		return name
	}
	return instanceNameFromScope(claim.ProviderScope)
}

func instanceNameFromScope(scope string) string {
	return strings.TrimPrefix(strings.TrimSpace(scope), "instance:")
}

func missingClaimCleanupReady(claim core.LeaseClaim, now time.Time) (bool, string) {
	if strings.EqualFold(claim.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !strings.EqualFold(claim.Labels["state"], "provisioning") {
		return true, ""
	}
	var createdAt time.Time
	if display := core.LeaseLabelTimeDisplay(claim.Labels["created_at"]); display != "" {
		createdAt, _ = time.Parse(time.RFC3339, display)
	}
	if createdAt.IsZero() {
		createdAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(claim.ClaimedAt))
	}
	if createdAt.IsZero() || now.Before(createdAt.Add(hypervProvisioningClaimGrace)) {
		return false, "provisioning grace"
	}
	return true, ""
}

func shouldCleanup(server Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if server.Status != "running" && server.Status != "ready" {
		return true, "instance state=" + blank(server.Status, "unknown")
	}
	if hasClaim {
		expiresAt := strings.TrimSpace(server.Labels["expires_at"])
		if expiresAt == "" {
			expiresAt = strings.TrimSpace(claim.Labels["expires_at"])
		}
		if display := core.LeaseLabelTimeDisplay(expiresAt); display != "" {
			if parsed, err := time.Parse(time.RFC3339, display); err == nil && now.After(parsed) {
				return true, "claim expired"
			}
		}
		lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
		if err != nil || lastUsed.IsZero() {
			return false, "claim active"
		}
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		if idle <= 0 {
			return false, "claim active"
		}
		if now.After(lastUsed.Add(idle).Add(12 * time.Hour)) {
			return true, "claim expired"
		}
		return false, "claim active"
	}
	return false, "missing claim"
}

// hypervState maps Hyper-V State enum values to string labels.
// See: https://learn.microsoft.com/en-us/windows/win32/hyperv_v2/msvm-computersystem
func hypervState(state int) string {
	switch state {
	case hypervMissingState:
		return "missing"
	case 2:
		return "running"
	case 3:
		return "stopped"
	case 6:
		return "saved"
	case 9:
		return "paused"
	default:
		return "unknown"
	}
}

func hypervVHDDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = `C:\Users\Public`
	}
	return filepath.Join(home, "Hyper-V", "Virtual Hard Disks")
}

func hypervVMDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = `C:\Users\Public`
	}
	return filepath.Join(home, "Hyper-V", "Virtual Machines")
}

func parseVMList(raw string) ([]hypervVM, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}
	if strings.HasPrefix(raw, "[") {
		var vms []hypervVM
		if err := json.Unmarshal([]byte(raw), &vms); err != nil {
			return nil, exit(2, "parse hyperv VM list: %v", err)
		}
		return vms, nil
	}
	var vm hypervVM
	if err := json.Unmarshal([]byte(raw), &vm); err != nil {
		return nil, exit(2, "parse hyperv VM: %v", err)
	}
	return []hypervVM{vm}, nil
}

func parseFirstIPv4(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return ""
	}
	if strings.HasPrefix(raw, "[") {
		var ips []string
		if err := json.Unmarshal([]byte(raw), &ips); err != nil {
			return ""
		}
		for _, ip := range ips {
			if isUsableIPv4(ip) {
				return ip
			}
		}
		return ""
	}
	raw = strings.Trim(raw, `"`)
	if isUsableIPv4(raw) {
		return raw
	}
	return ""
}

func isUsableIPv4(s string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	return err == nil && addr.Is4() && addr.IsGlobalUnicast()
}

func isIPv4(s string) bool {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

func escapePSString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func commandError(action string, result LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return exit(code, "%s failed: %v", action, err)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
