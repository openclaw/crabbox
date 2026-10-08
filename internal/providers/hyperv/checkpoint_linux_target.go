package hyperv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	linuxForkSpecializationVersion = "nocloud-v2"
	linuxForkSpecializationMarker  = "/var/lib/crabbox/checkpoint-fork-specialized"
	linuxForkSeedCompletionMarker  = "crabbox-specialized"
	linuxForkSpecializationTimeout = 10 * time.Minute
)

type linuxProductionCheckpointSupport struct {
	Found     bool   `json:"Found"`
	Enabled   bool   `json:"Enabled"`
	Primary   string `json:"Primary"`
	Secondary string `json:"Secondary"`
}

func checkpointTarget(metadata map[string]string) (string, error) {
	target := strings.TrimSpace(metadata[checkpointMetadataTarget])
	if target == "" {
		return core.TargetWindows, nil
	}
	switch target {
	case core.TargetLinux, core.TargetWindows:
		return target, nil
	default:
		return "", core.Exit(2, "Hyper-V checkpoint metadata has unsupported target %q", target)
	}
}

func requireLinuxForkSpecializationMetadata(metadata map[string]string) error {
	if strings.TrimSpace(metadata[checkpointMetadataSpecialization]) != linuxForkSpecializationVersion {
		return core.Exit(2, "Hyper-V Linux checkpoint is missing supported offline fork specialization metadata")
	}
	return nil
}

func checkpointFirmwareSettings(metadata map[string]string) (firmwareSettings, error) {
	enabledValue := strings.TrimSpace(metadata[checkpointMetadataSecureBoot])
	if enabledValue == "" {
		return firmwareSettings{}, core.Exit(2, "Hyper-V Linux checkpoint is missing source firmware metadata")
	}
	enabled, err := strconv.ParseBool(enabledValue)
	if err != nil {
		return firmwareSettings{}, core.Exit(2, "Hyper-V Linux checkpoint has invalid secure boot metadata %q", enabledValue)
	}
	template := strings.TrimSpace(metadata[checkpointMetadataSecureTemplate])
	if enabled && template == "" {
		return firmwareSettings{}, core.Exit(2, "Hyper-V Linux checkpoint is missing its secure boot template")
	}
	return firmwareSettings{enabled: enabled, template: template}, nil
}

func (b *backend) verifyLinuxProductionCheckpointSupport(ctx context.Context, name, id string) error {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$vm=Get-VM -Name '%s' -ErrorAction Stop; `+
			`if ($vm.Id.Guid -ne '%s') { throw 'source VM identity changed' }; `+
			`$service=Get-VMIntegrationService -VM $vm -Name 'VSS' -ErrorAction SilentlyContinue | Select-Object -First 1; `+
			`if (-not $service) { `+
			`[pscustomobject]@{Found=$false;Enabled=$false;Primary='missing';Secondary=''} | ConvertTo-Json -Compress `+
			`} else { `+
			`[pscustomobject]@{Found=$true;Enabled=[bool]$service.Enabled;Primary=[string]$service.PrimaryStatusDescription;Secondary=[string]$service.SecondaryOperationalStatus} | ConvertTo-Json -Compress }`,
		escapePSString(name),
		escapePSString(id),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("verify Linux production checkpoint integration service", result, err)
	}
	var support linuxProductionCheckpointSupport
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &support); err != nil {
		return core.Exit(2, "parse Linux production checkpoint integration service status: %v", err)
	}
	if !support.Found || !support.Enabled || !strings.EqualFold(strings.TrimSpace(support.Primary), "OK") ||
		strings.EqualFold(strings.TrimSpace(support.Secondary), "ProtocolMismatch") {
		return core.Exit(
			2,
			"provider=%s target=linux production checkpoints require the enabled Hyper-V VSS/file-system-freeze integration service backed by a running hv_vss_daemon; status found=%t enabled=%t primary=%q secondary=%q; standard checkpoint fallback is disabled",
			providerName,
			support.Found,
			support.Enabled,
			support.Primary,
			support.Secondary,
		)
	}
	return nil
}

func linuxForkSpecializationInstanceID(leaseID string) string {
	return "crabbox-fork-" + strings.TrimPrefix(strings.TrimSpace(leaseID), "cbx_")
}

func linuxForkExternalStateResetCommands() []string {
	return strings.Split(core.TailscaleIdentityResetScript(core.TargetLinux, ""), "\n")
}

func linuxForkSpecializationUserData(user, publicKey, hostname, instanceID string) string {
	publicKeyBase64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(publicKey) + "\n"))
	externalReset := strings.Join(linuxForkExternalStateResetCommands(), "\n      ")
	return fmt.Sprintf(`#cloud-config
write_files:
  - path: /usr/local/sbin/crabbox-checkpoint-fork-specialize
    permissions: '0700'
    content: |
      #!/bin/sh
      set -eu
      awk -F: '$6 ~ /^\// { print $6 }' /etc/passwd | while IFS= read -r home; do
        rm -f "$home/.ssh/authorized_keys" "$home/.ssh/authorized_keys2"
      done
      home="$(getent passwd %[1]s | cut -d: -f6)"
      test -n "$home"
      uid="$(id -u %[1]s)"
      gid="$(id -g %[1]s)"
      install -d -m 0700 -o "$uid" -g "$gid" "$home/.ssh"
      printf '%%s' '%[2]s' | base64 -d >"$home/.ssh/authorized_keys"
      chown "$uid:$gid" "$home/.ssh/authorized_keys"
      chmod 0600 "$home/.ssh/authorized_keys"
      rm -f /etc/ssh/ssh_host_*
      ssh-keygen -A
      hostnamectl set-hostname %[3]s
      printf '%%s\n' '%[3]s' >/etc/hostname
      if grep -q '^127\.0\.1\.1[[:space:]]' /etc/hosts; then
        sed -i 's/^127\.0\.1\.1[[:space:]].*/127.0.1.1\t%[3]s/' /etc/hosts
      fi
      %[5]s
      rm -f /etc/machine-id /var/lib/dbus/machine-id
      systemd-machine-id-setup
      current_instance_dir="/var/lib/cloud/instances/%[4]s"
      test -d "$current_instance_dir"
      find /var/lib/cloud/instances -mindepth 1 -maxdepth 1 ! -path "$current_instance_dir" -exec rm -rf {} +
      install -d -m 0755 /var/lib/crabbox
      printf '%%s\n' '%[4]s' >%[6]s
      seed_device="$(blkid -L cidata)"
      test -b "$seed_device"
      seed_mount="$(findmnt -n -S "$seed_device" -o TARGET || true)"
      mounted_here=false
      if [ -z "$seed_mount" ]; then
        seed_mount=/run/crabbox-checkpoint-seed
        install -d -m 0700 "$seed_mount"
        mount "$seed_device" "$seed_mount"
        mounted_here=true
      fi
      printf '%%s\n' '%[4]s' >"$seed_mount/%[7]s"
      sync
      if [ "$mounted_here" = true ]; then
        umount "$seed_mount"
      fi
      systemctl poweroff
runcmd:
  - ["/usr/local/sbin/crabbox-checkpoint-fork-specialize"]
`, user, publicKeyBase64, hostname, instanceID, externalReset, linuxForkSpecializationMarker, linuxForkSeedCompletionMarker)
}

func (b *backend) specializeLinuxCheckpointFork(
	ctx context.Context,
	cfg core.Config,
	firmware firmwareSettings,
	name string,
	leaseID string,
	publicKey string,
	hostname string,
) (err error) {
	instanceID := linuxForkSpecializationInstanceID(leaseID)
	seedPath := cloudInitSeedPath(name)
	userData := linuxForkSpecializationUserData(cfg.HyperV.User, publicKey, hostname, instanceID)
	metaData := cloudInitMetaDataForInstance(instanceID, hostname)
	if err := b.createNoCloudSeed(ctx, seedPath, userData, metaData); err != nil {
		return err
	}
	cleanupSeed := true
	defer func() {
		if cleanupSeed {
			err = errors.Join(err, b.detachAndRemoveNoCloudSeed(context.Background(), name))
		}
	}()
	if err := b.attachNoCloudSeed(ctx, name, seedPath); err != nil {
		return err
	}
	if err := b.configureVMFirmwareSettings(ctx, name, firmware); err != nil {
		return err
	}
	if err := b.startCheckpointVM(ctx, name); err != nil {
		return err
	}
	if err := b.waitForVMOff(ctx, name, linuxForkSpecializationTimeout); err != nil {
		return err
	}
	if err := b.detachNoCloudSeed(ctx, name, seedPath); err != nil {
		return err
	}
	if err := b.verifyLinuxForkSpecializationMarker(ctx, seedPath, instanceID); err != nil {
		return err
	}
	if err := removeNoCloudSeed(seedPath); err != nil {
		return err
	}
	cleanupSeed = false
	if err := b.connectVMNetwork(ctx, name, cfg.HyperV.Switch); err != nil {
		return err
	}
	return b.startCheckpointVM(ctx, name)
}

func (b *backend) verifyLinuxForkSpecializationMarker(ctx context.Context, seedPath, instanceID string) error {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$path='%s'; $expected='%s'; $mounted=$false; `+
			`try { `+
			`$vhd=Mount-VHD -Path $path -ReadOnly -Passthru -ErrorAction Stop; $mounted=$true; `+
			`$disk=$vhd | Get-Disk -ErrorAction Stop; `+
			`if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction Stop }; `+
			`$partition=$disk | Get-Partition -ErrorAction Stop | Where-Object { $_.Type -ne 'Reserved' } | Select-Object -First 1; `+
			`if (-not $partition) { throw 'NoCloud seed partition is missing' }; `+
			`if (-not $partition.DriveLetter) { `+
			`$partitionNumber=$partition.PartitionNumber; `+
			`Add-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $partitionNumber -AssignDriveLetter -ErrorAction Stop; `+
			`$partition=$disk | Get-Partition -ErrorAction Stop | Where-Object { $_.PartitionNumber -eq $partitionNumber } }; `+
			`if (-not $partition.DriveLetter) { throw 'NoCloud seed drive letter is unavailable' }; `+
			`$marker=Join-Path "$($partition.DriveLetter):\" '%s'; `+
			`if (-not (Test-Path -LiteralPath $marker)) { throw 'offline specialization completion marker is missing' }; `+
			`$actual=[IO.File]::ReadAllText($marker).Trim(); `+
			`if ($actual -ne $expected) { throw 'offline specialization completion marker does not match this fork' } `+
			`} finally { if ($mounted) { Dismount-VHD -Path $path -ErrorAction SilentlyContinue } }`,
		escapePSString(seedPath),
		escapePSString(instanceID),
		escapePSString(linuxForkSeedCompletionMarker),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("verify Linux checkpoint fork specialization marker", result, err)
	}
	return nil
}

func (b *backend) startCheckpointVM(ctx context.Context, name string) error {
	script := fmt.Sprintf(
		`$vm=Get-VM -Name '%s' -ErrorAction Stop; if ($vm.State -ne 'Running') { Start-VM -VM $vm -ErrorAction Stop }`,
		escapePSString(name),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("start Hyper-V checkpoint VM", result, err)
	}
	return nil
}

func (b *backend) waitForVMOff(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		vm, err := b.queryCheckpointVM(ctx, name)
		if err != nil {
			return err
		}
		if vm.State == 3 {
			return nil
		}
		if vm.State == hypervMissingState {
			return core.Exit(4, "Hyper-V Linux checkpoint fork %s disappeared during offline specialization", name)
		}
		if time.Now().After(deadline) {
			return core.Exit(5, "Hyper-V Linux checkpoint fork %s did not power off after offline specialization within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
