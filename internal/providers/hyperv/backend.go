package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
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
		cfg.HyperV.User = "crabbox"
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
	if cfg.HyperV.Disk <= 0 {
		cfg.HyperV.Disk = 50
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
		return LeaseTarget{}, exit(2, "provider=%s requires --hyperv-image (path to a VHDX template with OpenSSH pre-configured)", providerName)
	}
	if strings.HasSuffix(strings.ToLower(cfg.HyperV.Image), ".iso") {
		return LeaseTarget{}, exit(2, "provider=%s does not support ISO images; provide a pre-configured VHDX template with OpenSSH and a known user", providerName)
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
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s cpus=%d memory=%dMB disk=%dGB switch=%s keep=%v\n",
		providerName, leaseID, slug, cfg.HyperV.Image, cfg.HyperV.CPUs, cfg.HyperV.Memory, cfg.HyperV.Disk, cfg.HyperV.Switch, req.Keep)

	if err := b.createVM(ctx, cfg, name, publicKey); err != nil {
		_ = b.removeVM(context.Background(), name)
		return LeaseTarget{}, err
	}

	ip, err := b.waitForIP(ctx, name, 5*time.Minute)
	if err != nil {
		if !req.Keep {
			_ = b.removeVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}

	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = cfg.HyperV.Image
	labels["ssh_user"] = cfg.HyperV.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.HyperV.WorkRoot

	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	lease, err := b.prepareLease(ctx, cfg, hypervVM{Name: name, State: 2}, ip, claim, true)
	if err != nil {
		if !req.Keep {
			_ = b.removeVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.removeVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := updateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			_ = b.removeVM(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
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
	if claim.LeaseID == "" {
		return LeaseTarget{}, exit(4, "hyperv instance %q has no Crabbox lease claim; use `crabbox stop --provider hyperv %s` to delete it or warm a new lease", inst.Name, inst.Name)
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
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	msg := fmt.Sprintf("hyperv=%s control_plane=local inventory=ready api=powershell mutation=false leases=%d ssh_probe=%s",
		firstLine(state), len(instances), probe)
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
		removeLeaseClaim(lease.LeaseID)
		removeStoredTestboxKey(lease.LeaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
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
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
		removeLeaseClaim(claim.LeaseID)
		removeStoredTestboxKey(claim.LeaseID)
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

// createVM creates and starts a Hyper-V VM from the configured image and
// injects the SSH public key via PowerShell Direct.
func (b *backend) createVM(ctx context.Context, cfg Config, name, publicKey string) error {
	vhdDir := hypervVHDDir()
	if err := os.MkdirAll(vhdDir, 0o755); err != nil {
		return exit(2, "create VHD directory %s: %v", vhdDir, err)
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

	copyScript := fmt.Sprintf(`Copy-Item -LiteralPath '%s' -Destination '%s'`, escapePSString(cfg.HyperV.Image), escapePSString(vhdPath))
	result, err = b.powershell(ctx, copyScript)
	if err != nil {
		return commandError("copy VHDX template", result, err)
	}
	if cfg.HyperV.Disk > 0 {
		resizeScript := fmt.Sprintf(`Resize-VHD -Path '%s' -SizeBytes %d -ErrorAction SilentlyContinue`,
			escapePSString(vhdPath), int64(cfg.HyperV.Disk)*1024*1024*1024)
		b.powershell(ctx, resizeScript) //nolint:errcheck
	}
	createScript := fmt.Sprintf(
		`New-VM -Name '%s' -MemoryStartupBytes %d -Generation 2 -VHDPath '%s'`,
		escapePSString(name), memBytes, escapePSString(vhdPath),
	)
	result, err = b.powershell(ctx, createScript)
	if err != nil {
		return commandError("New-VM", result, err)
	}

	cpuScript := fmt.Sprintf(`Set-VM -Name '%s' -ProcessorCount %d`, escapePSString(name), cfg.HyperV.CPUs)
	result, err = b.powershell(ctx, cpuScript)
	if err != nil {
		return commandError("Set-VM", result, err)
	}

	switchScript := fmt.Sprintf(
		`Connect-VMNetworkAdapter -VMName '%s' -SwitchName '%s'`,
		escapePSString(name), escapePSString(cfg.HyperV.Switch),
	)
	result, err = b.powershell(ctx, switchScript)
	if err != nil {
		return commandError("Connect-VMNetworkAdapter", result, err)
	}

	startScript := fmt.Sprintf(`Start-VM -Name '%s'`, escapePSString(name))
	result, err = b.powershell(ctx, startScript)
	if err != nil {
		return commandError("Start-VM", result, err)
	}

	if publicKey != "" {
		if err := b.injectSSHKey(ctx, name, cfg.HyperV.User, publicKey); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: SSH key injection via PowerShell Direct failed (will retry during SSH wait): %v\n", err)
		}
	}

	return nil
}

// injectSSHKey writes the SSH public key into the VM via PowerShell Direct
// (Invoke-Command -VMName). Retries up to 5 times with backoff to allow the
// guest OS to boot. The VHDX template must have the configured user with the
// password matching CRABBOX_HYPERV_GUEST_PASSWORD (default: from config or
// image-level convention). This is best-effort: images with pre-configured
// authorized_keys and OpenSSH will work without injection.
func (b *backend) injectSSHKey(ctx context.Context, vmName, user, publicKey string) error {
	guestPassword := b.cfg.HyperV.GuestPassword
	if guestPassword == "" {
		guestPassword = "crabbox"
	}
	script := fmt.Sprintf(
		`$cred = New-Object PSCredential('%s', (ConvertTo-SecureString '%s' -AsPlainText -Force)); `+
			`Invoke-Command -VMName '%s' -Credential $cred -ScriptBlock { `+
			`$sshDir = Join-Path $env:USERPROFILE '.ssh'; `+
			`New-Item -ItemType Directory -Force -Path $sshDir | Out-Null; `+
			`$akPath = Join-Path $sshDir 'authorized_keys'; `+
			`Add-Content -Encoding ASCII -Path $akPath -Value '%s'; `+
			`$adminAK = Join-Path $env:ProgramData 'ssh\administrators_authorized_keys'; `+
			`if (Test-Path (Split-Path $adminAK)) { Add-Content -Encoding ASCII -Path $adminAK -Value '%s' } `+
			`}`,
		escapePSString(user), escapePSString(guestPassword), escapePSString(vmName),
		escapePSString(publicKey), escapePSString(publicKey),
	)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
			fmt.Fprintf(b.rt.Stderr, "retrying SSH key injection (%d/5)...\n", attempt+1)
		}
		result, err := b.powershell(ctx, script)
		if err == nil {
			return nil
		}
		lastErr = commandError("SSH key injection", result, err)
	}
	return lastErr
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

func (b *backend) listInstances(ctx context.Context) ([]hypervVM, error) {
	if hypervHostOS != "windows" {
		return nil, nil
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
	target.ReadyCheck = core.PowershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
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
	if !strings.HasPrefix(name, "crabbox-") {
		return exit(2, "refusing to remove non-Crabbox Hyper-V VM %q", name)
	}

	vhdPaths := b.queryVHDPaths(ctx, name)

	stopScript := fmt.Sprintf(`Stop-VM -Name '%s' -Force -Confirm:$false -ErrorAction SilentlyContinue`, escapePSString(name))
	b.powershell(ctx, stopScript) //nolint:errcheck

	removeScript := fmt.Sprintf(`Remove-VM -Name '%s' -Force -Confirm:$false`, escapePSString(name))
	result, err := b.powershell(ctx, removeScript)
	if err != nil {
		return commandError("Remove-VM", result, err)
	}

	expectedVHD := filepath.Join(hypervVHDDir(), name+".vhdx")
	if len(vhdPaths) > 0 {
		for _, p := range vhdPaths {
			if strings.EqualFold(filepath.Clean(p), filepath.Clean(expectedVHD)) {
				os.Remove(p) //nolint:errcheck
			}
		}
	} else {
		os.Remove(expectedVHD) //nolint:errcheck
	}

	return nil
}

func (b *backend) queryVM(ctx context.Context, name string) (hypervVM, error) {
	script := fmt.Sprintf(`Get-VM -Name '%s' | Select-Object Name, State | ConvertTo-Json -Compress`, escapePSString(name))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return hypervVM{}, commandError("Get-VM query", result, err)
	}
	stdout := strings.TrimSpace(result.Stdout)
	if stdout == "" || stdout == "null" {
		return hypervVM{}, exit(4, "hyperv VM %s not found", name)
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
	if labels["state"] == "" {
		labels["state"] = hypervState(inst.State)
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
	status := hypervState(inst.State)
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

func shouldCleanup(server Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if server.Status != "running" && server.Status != "ready" {
		return true, "instance state=" + blank(server.Status, "unknown")
	}
	if hasClaim {
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
	case 2:
		return "running"
	case 3:
		return "off"
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
			if isIPv4(ip) {
				return ip
			}
		}
		return ""
	}
	raw = strings.Trim(raw, `"`)
	if isIPv4(raw) {
		return raw
	}
	return ""
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
