package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const linuxImageRequirements = "a generalized Debian or Ubuntu cloud VHDX with cloud-init NoCloud, DHCP and working Hyper-V integration services including hv_kvp_daemon (Ubuntu needs linux-cloud-tools-common plus the versioned and kernel-flavour linux-cloud-tools packages matching uname -r)"
const linuxStorageLayout = "linux-v1"

type linuxHostOperations struct {
	seed   func(context.Context, string, string, string) error
	create func(context.Context, core.Config, string) (hypervVM, error)
	start  func(context.Context, string, string) error
	ip     func(context.Context, string, time.Duration) (string, error)
	detach func(context.Context, string, string) error
	remove func(context.Context, string, string) error
	query  func(context.Context, string) (hypervVM, error)
}

func (b *backend) linuxOperations() linuxHostOperations {
	if b.linuxHost != nil {
		return *b.linuxHost
	}
	return linuxHostOperations{b.createNoCloudSeed, b.createOwnedLinuxVM, b.startLinuxVM, b.waitForIP, b.detachOwnedNoCloudSeed, b.removeLinuxVM, b.queryLinuxVM}
}

func validLinuxSSHUser(user string) bool {
	if len(user) == 0 || len(user) > 32 {
		return false
	}
	for i, c := range user {
		if c >= 'a' && c <= 'z' || c == '_' || i > 0 && (c >= '0' && c <= '9' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func linuxUserData(cfg core.Config, publicKey string) string {
	return strings.Replace(core.CloudInitUserData(cfg, publicKey), "#cloud-config\n",
		fmt.Sprintf("#cloud-config\nssh_pwauth: false\ndisable_root: %t\n", cfg.SSHUser != "root"), 1)
}

func validateLinuxConfig(cfg core.Config) error {
	if _, err := secureBootSettings(targetLinux, cfg.HyperV.SecureBoot); err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(cfg.HyperV.Image), ".vhdx") {
		return core.Exit(2, "provider=hyperv target=linux requires a generalized cloud VHDX via --hyperv-image: %s; ISO, QCOW2 and RAW are unsupported", linuxImageRequirements)
	}
	if !validLinuxSSHUser(cfg.HyperV.User) {
		return core.Exit(2, "Linux --hyperv-user must be 1-32 lowercase letters, digits, underscores or hyphens, starting with a letter or underscore")
	}
	root := cfg.HyperV.WorkRoot
	if !path.IsAbs(root) || path.Clean(root) == "/" || strings.ContainsAny(root, "\\\x00\r\n") {
		return core.Exit(2, "Linux --hyperv-work-root must be an absolute POSIX directory other than /")
	}
	if cfg.HyperV.InitPassword {
		return core.Exit(2, "--hyperv-init-password is supported only for target=windows")
	}
	info, err := os.Lstat(cfg.HyperV.Image)
	if err != nil {
		return fmt.Errorf("inspect prepared Linux VHDX: %w", err)
	}
	if !info.Mode().IsRegular() {
		return core.Exit(2, "prepared Linux VHDX must be a regular file, not a directory or link")
	}
	return nil
}

func (b *backend) acquireLinux(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if hypervHostOS != "windows" {
		return core.LeaseTarget{}, core.Exit(2, "provider=hyperv requires a Windows host with Hyper-V enabled")
	}
	cfg := b.configForRun()
	if err := validateLinuxConfig(cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	if strings.TrimSpace(req.Repo.Root) == "" {
		return core.LeaseTarget{}, core.Exit(2, "Hyper-V acquisition requires a repository root for durable ownership")
	}
	if err := ctx.Err(); err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var servers []core.Server
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfigContext(ctx, cfg, leaseID)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), b.cleanupBudget)
		defer cancel()
		return core.LeaseTarget{}, errors.Join(err, core.RemoveStoredTestboxConnectionArtifactsContext(cleanupCtx, leaseID))
	}
	cfg.SSHKey = keyPath
	name := core.LeaseProviderName(leaseID, slug)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, b.now().UTC())
	labels["instance"], labels["image"] = name, cfg.HyperV.Image
	labels["ssh_user"], labels["ssh_port"], labels["work_root"] = cfg.SSHUser, sshPort, cfg.WorkRoot
	labels["state"], labels["hyperv_storage_layout"] = "provisioning", linuxStorageLayout
	markHyperVTailscaleLabels(labels, cfg)
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	lease := core.LeaseTarget{LeaseID: leaseID, Server: b.serverFromInstance(hypervVM{Name: name, State: 3}, claim, cfg)}
	lease.Server.Status, lease.Server.Labels["state"] = "provisioning", "provisioning"
	if err := persistLease(leaseID, slug, name, cfg, req, &lease, nil); err != nil {
		core.RemoveStoredTestboxKey(leaseID)
		return core.LeaseTarget{}, fmt.Errorf("persist Linux ownership before creation: %w", err)
	}
	claim, _, _ = core.ServerLeaseClaimSnapshot(lease.Server)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=hyperv lease=%s instance=%s target=linux cpus=%d memory=%dMB keep=%v\n", leaseID, name, cfg.HyperV.CPUs, cfg.HyperV.Memory, req.Keep)
	var tailscaleCleanupTarget core.SSHTarget
	fail := func(cause error) (core.LeaseTarget, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), b.cleanupBudget)
		defer cancel()
		current, ok, readErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
		if readErr != nil || !ok {
			return core.LeaseTarget{}, errors.Join(cause, readErr, fmt.Errorf("lease %s ownership unavailable; cleanup not attempted", leaseID))
		}
		if tailscaleCleanupTarget.Host != "" {
			b.logoutLinuxTailscaleBestEffort(tailscaleCleanupTarget)
		}
		if req.Keep {
			if err := requireExactHyperVClaim(leaseID, name); err != nil {
				return core.LeaseTarget{}, errors.Join(cause, err)
			}
			if err := verifyLinuxStorage(name, leaseID); err != nil {
				return core.LeaseTarget{}, errors.Join(cause, err)
			}
			cleanupErr := b.dismountNoCloudSeed(cleanupCtx, name)
			if cfg.Tailscale.Enabled && cleanupErr == nil {
				vm, queryErr := b.linuxOperations().query(cleanupCtx, name)
				switch {
				case queryErr != nil:
					cleanupErr = queryErr
				case vm.State == hypervMissingState:
					cleanupErr = b.removeVHDFile(ownedCloudInitSeedPath(name))
				case current.CloudImmutableID == "" || !strings.EqualFold(vm.ID, current.CloudImmutableID):
					cleanupErr = errors.New("cannot scrub retained Tailscale seed without the exact VM identity")
				default:
					cleanupErr = b.linuxOperations().detach(cleanupCtx, name, current.CloudImmutableID)
				}
			}
			fmt.Fprintf(b.rt.Stderr, "retained lease=%s target=linux; recover with crabbox status --provider hyperv --id %s or stop this exact lease\n", leaseID, leaseID)
			return core.LeaseTarget{}, errors.Join(cause, cleanupErr)
		}
		return core.LeaseTarget{}, errors.Join(cause, b.releaseLinuxClaim(cleanupCtx, current))
	}
	if err := reserveLinuxStorage(name, leaseID); err != nil {
		// No host operation is safe when the generated storage name was already occupied.
		return core.LeaseTarget{}, fmt.Errorf("reserve Linux storage for lease %s (claim retained): %w", leaseID, err)
	}
	host := b.linuxOperations()
	if err := host.seed(ctx, ownedCloudInitSeedPath(name), linuxUserData(hypervTailscaleBootstrapConfig(cfg), publicKey), cloudInitMetaData(name)); err != nil {
		return fail(err)
	}
	vm, err := host.create(ctx, cfg, name)
	if err != nil {
		return fail(err)
	}
	if vm.Name != name || !validHyperVGUID(vm.ID) {
		return fail(errors.New("created Linux VM did not return its exact immutable identity"))
	}
	lease.Server = b.serverFromInstance(vm, claim, cfg)
	lease.Server.Status, lease.Server.Labels["state"] = "provisioning", "provisioning"
	if err := persistLease(leaseID, slug, name, cfg, req, &lease, nil); err != nil {
		return fail(fmt.Errorf("persist created VM identity: %w", err))
	}
	claim, _, _ = core.ServerLeaseClaimSnapshot(lease.Server)
	if err := host.start(ctx, name, vm.ID); err != nil {
		return fail(err)
	}
	vm.State = 2
	ip, err := host.ip(ctx, name, 5*time.Minute)
	if err != nil {
		return fail(fmt.Errorf("Linux IP discovery requires %s: %w", linuxImageRequirements, err))
	}
	if cfg.Tailscale.Enabled {
		tailscaleCleanupTarget = core.SSHTargetFromConfig(cfg, ip)
		tailscaleCleanupTarget.Port = sshPort
		tailscaleCleanupTarget.FallbackPorts = []string{}
	}
	lease, err = b.prepareLease(ctx, cfg, vm, ip, claim, true)
	if err != nil {
		return fail(err)
	}
	if err := host.detach(ctx, name, vm.ID); err != nil {
		return fail(err)
	}
	attachedCaches, err := b.attachConfiguredCacheVolumes(ctx, name, cfg, lease)
	if err != nil {
		return fail(err)
	}
	if err := persistLease(leaseID, slug, name, cfg, req, &lease, core.CacheVolumeStickyDiskSpecs(attachedCaches)); err != nil {
		return fail(err)
	}
	return lease, nil
}

func validHyperVGUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func reserveLinuxStorage(name, leaseID string) error {
	if err := validateLinuxStorageName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(hypervVHDDir(), 0o755); err != nil {
		return err
	}
	root := linuxStoragePath(name)
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	if err := core.SecurePrivatePath(root, true); err != nil {
		return errors.Join(err, os.Remove(root))
	}
	ownerPath := filepath.Join(root, "owner")
	file, err := os.OpenFile(ownerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(err, os.Remove(root))
	}
	return writeLinuxOwner(file, leaseID)
}

func writeLinuxOwner(file *os.File, leaseID string) error {
	_, writeErr := file.WriteString(leaseID)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func verifyLinuxStorage(name, leaseID string) error {
	if err := validateLinuxStorageName(name); err != nil {
		return err
	}
	root := linuxStoragePath(name)
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Linux storage root is not an owned regular directory")
	}
	owner := filepath.Join(root, "owner")
	info, err = os.Lstat(owner)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 128 {
		return errors.New("Linux storage ownership marker is not a bounded regular file")
	}
	data, err := os.ReadFile(owner)
	if err != nil {
		return err
	}
	if string(data) != leaseID {
		return errors.New("Linux storage ownership conflict")
	}
	return nil
}

func validateLinuxStorageName(name string) error {
	if !validHyperVVMName(name) {
		return errors.New("invalid Linux VM storage name")
	}
	if _, err := os.UserHomeDir(); err != nil {
		return fmt.Errorf("Linux Hyper-V storage requires the current user's home: %w", err)
	}
	return nil
}

func linuxVMCreateScript(cfg core.Config, name string) string {
	settings, _ := secureBootSettings(targetLinux, cfg.HyperV.SecureBoot)
	firmware := "Set-VMFirmware -VM $vm -EnableSecureBoot Off -ErrorAction Stop"
	if settings.enabled {
		firmware = fmt.Sprintf("Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate '%s' -ErrorAction Stop", escapePSString(settings.template))
	}
	// The NoCloud seed must be openable by the Hyper-V management service, which runs as SYSTEM.
	// Mounting a VHDX makes Windows replace the file's DACL with a protected one that drops
	// inheritance and grants only the mounting user, and dismounting does not restore it -- so the
	// seed arrives here with the SYSTEM entry missing and Add-VMHardDiskDrive fails with "Cannot
	// get information for attachment ... Access is denied (0x80070005)". Every Linux lease died
	// there.
	//
	// The grant is made immediately before the attach rather than when the seed is written,
	// because something between those two points re-protects the DACL a second time: a stopgap
	// that granted once at creation had its grant reverted and had to re-grant about eight seconds
	// later, which is this script's window. Which operation does it was not isolated; granting at
	// the point of use is correct regardless, since it is the last moment before the file is read.
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
if (Get-VM -Name '%[1]s' -ErrorAction SilentlyContinue) { throw 'Hyper-V VM name already exists' }
$switch = Get-VMSwitch -Name '%[2]s' -ErrorAction Stop
New-VHD -Path '%[3]s' -ParentPath '%[4]s' -Differencing -ErrorAction Stop | Out-Null
$vm = New-VM -Name '%[1]s' -MemoryStartupBytes %[5]d -Generation 2 -VHDPath '%[3]s' -Path '%[6]s' -ErrorAction Stop
$vm | Set-VM -ProcessorCount %[7]d -AutomaticCheckpointsEnabled $false -ErrorAction Stop
Set-VMMemory -VM $vm -DynamicMemoryEnabled $false -ErrorAction Stop
%[9]s
icacls.exe '%[8]s' /grant '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'NoCloud seed ACL update failed' }
Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 1 -Path '%[8]s' -ErrorAction Stop
Connect-VMNetworkAdapter -VMName '%[1]s' -SwitchName $switch.Name -ErrorAction Stop
$vm | Select-Object Name, State, @{Name='Id';Expression={[string]$_.Id}} | ConvertTo-Json -Compress
`, escapePSString(name), escapePSString(cfg.HyperV.Switch), escapePSString(linuxDiskPath(name)),
		escapePSString(cfg.HyperV.Image), int64(cfg.HyperV.Memory)*1024*1024,
		escapePSString(linuxStoragePath(name)), cfg.HyperV.CPUs, escapePSString(ownedCloudInitSeedPath(name)), firmware)
}

func (b *backend) createOwnedLinuxVM(ctx context.Context, cfg core.Config, name string) (hypervVM, error) {
	if _, err := secureBootSettings(targetLinux, cfg.HyperV.SecureBoot); err != nil {
		return hypervVM{}, err
	}
	result, err := b.powershell(ctx, linuxVMCreateScript(cfg, name))
	if err != nil {
		return hypervVM{}, shared.LocalCommandError("create owned Linux VM", result, err)
	}
	var vm hypervVM
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &vm); err != nil {
		return vm, fmt.Errorf("read created Linux VM identity: %w", err)
	}
	return vm, nil
}

func (b *backend) startLinuxVM(ctx context.Context, name, id string) error {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$vm = Get-VM -Name '%s' -ErrorAction Stop
if ([string]$vm.Id -ne '%s') { throw 'Hyper-V VM identity changed; refusing start' }
$vm | Start-VM -ErrorAction Stop`, escapePSString(name), escapePSString(id))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("start owned Linux VM", result, err)
	}
	return nil
}

func (b *backend) queryLinuxVM(ctx context.Context, name string) (hypervVM, error) {
	if err := validateLinuxStorageName(name); err != nil {
		return hypervVM{}, err
	}
	script := fmt.Sprintf(`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' } | Select-Object Name, State, @{Name='Id';Expression={[string]$_.Id}} | ConvertTo-Json -Compress`, escapePSString(name))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return hypervVM{}, shared.LocalCommandError("query Linux VM identity", result, err)
	}
	if strings.TrimSpace(result.Stdout) == "" || strings.TrimSpace(result.Stdout) == "null" {
		return hypervVM{Name: name, State: hypervMissingState}, nil
	}
	var vm hypervVM
	if err := json.Unmarshal([]byte(result.Stdout), &vm); err != nil {
		return vm, fmt.Errorf("parse Linux VM identity: %w", err)
	}
	if vm.Name != name || !validHyperVGUID(vm.ID) {
		return vm, errors.New("Linux VM query did not return the requested immutable identity")
	}
	return vm, nil
}

func (b *backend) removeLinuxVM(ctx context.Context, name, id string) error {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$vm = Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' }
if ($vm) {
  if ('%s' -eq '' -or [string]$vm.Id -ne '%s') { throw 'Hyper-V VM identity missing or changed; refusing removal' }
  if ([int]$vm.State -ne 3) { $vm | Stop-VM -Force -Confirm:$false -ErrorAction Stop }
  $vm | Remove-VM -Force -Confirm:$false -ErrorAction Stop
}`, escapePSString(name), escapePSString(id), escapePSString(id))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("remove exact owned Linux VM", result, err)
	}
	return nil
}

func (b *backend) releaseLinuxClaim(ctx context.Context, claim core.LeaseClaim) error {
	name := instanceNameFromClaim(claim)
	if err := validateLinuxStorageName(name); err != nil {
		return err
	}
	if err := requireExactHyperVClaim(claim.LeaseID, name); err != nil {
		return err
	}
	if claim.Labels["hyperv_storage_layout"] != linuxStorageLayout {
		return core.Exit(4, "Linux lease %s has no recognized storage ownership; refusing automatic cleanup", claim.LeaseID)
	}
	err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		_, statErr := os.Lstat(linuxStoragePath(name))
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if statErr == nil {
			if err := verifyLinuxStorage(name, claim.LeaseID); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.linuxOperations().remove(ctx, name, claim.CloudImmutableID); err != nil {
			return err
		}
		if err := b.dismountNoCloudSeed(ctx, name); err != nil {
			return err
		}
		for _, filename := range []string{"seed.vhdx", "disk.vhdx", "seed-host-mounted"} {
			if err := b.removeVHDFile(filepath.Join(linuxStoragePath(name), filename)); err != nil {
				return err
			}
		}
		if err := removeHyperVConfigFiles(filepath.Join(linuxStoragePath(name), name)); err != nil {
			return err
		}
		entries, err := os.ReadDir(linuxStoragePath(name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "user-data-") || strings.HasPrefix(entry.Name(), "meta-data-") {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return errors.New("NoCloud source remnant is not a regular file; ownership retained")
				}
				if err := b.removeVHDFile(filepath.Join(linuxStoragePath(name), entry.Name())); err != nil {
					return err
				}
				continue
			}
			if entry.Name() != "owner" {
				return fmt.Errorf("Linux lease storage still contains %q; ownership retained", entry.Name())
			}
		}
		if err := b.removeVHDFile(filepath.Join(linuxStoragePath(name), "owner")); err != nil {
			return err
		}
		if err := os.Remove(linuxStoragePath(name)); err != nil && !os.IsNotExist(err) {
			file, restoreErr := os.OpenFile(filepath.Join(linuxStoragePath(name), "owner"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if restoreErr == nil {
				restoreErr = writeLinuxOwner(file, claim.LeaseID)
			}
			return errors.Join(fmt.Errorf("remove owned Linux storage directory: %w", err), restoreErr)
		}
		return core.RemoveStoredTestboxConnectionArtifactsContext(ctx, claim.LeaseID)
	})
	return err
}
