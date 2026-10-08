package hyperv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const cloudInitSeedSizeBytes = 64 * 1024 * 1024

func linuxStoragePath(name string) string {
	return filepath.Join(hypervVHDDir(), name)
}

func cloudInitSeedPath(name string) string {
	return filepath.Join(hypervVHDDir(), name+"-seed.vhdx")
}

func ownedCloudInitSeedPath(name string) string {
	return filepath.Join(linuxStoragePath(name), "seed.vhdx")
}

func cloudInitMountMarker(seedPath string) string {
	return filepath.Join(filepath.Dir(seedPath), "seed-host-mounted")
}

func linuxDiskPath(name string) string {
	return filepath.Join(linuxStoragePath(name), "disk.vhdx")
}

func cloudInitMetaData(name string) string {
	return cloudInitMetaDataForInstance(name, name)
}

func cloudInitMetaDataForInstance(instanceID, hostname string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)
}

func writeNoCloudSource(dir, pattern, content string) (name string, err error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name = file.Name()
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, os.Remove(name))
		}
	}()
	if err = core.SecurePrivatePath(name, false); err != nil {
		return name, err
	}
	_, err = file.WriteString(content)
	return name, err
}

func noCloudSeedScript(seedPath string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$path = '%s';
$mountMarker = '%s'
$mounted = $false
try {
  New-VHD -Path $path -Dynamic -SizeBytes %d -ErrorAction Stop | Out-Null
  [IO.File]::WriteAllText($mountMarker, 'mount pending')
  $vhd = Mount-VHD -Path $path -Passthru -ErrorAction Stop
  $mounted = $true
  $disk = $vhd | Get-Disk -ErrorAction Stop
  if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction Stop }
  if ($disk.IsReadOnly) { Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction Stop }
  Initialize-Disk -Number $disk.Number -PartitionStyle MBR -ErrorAction Stop | Out-Null
  $partition = New-Partition -DiskNumber $disk.Number -UseMaximumSize -AssignDriveLetter -ErrorAction Stop
  $volume = $partition | Format-Volume -FileSystem FAT -NewFileSystemLabel 'cidata' -Confirm:$false -Force -ErrorAction Stop
  if (-not $volume.Path) { throw 'NoCloud seed volume has no accessible path' }
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [IO.File]::WriteAllText((Join-Path $volume.Path 'user-data'), [IO.File]::ReadAllText($env:_CRABBOX_USER_DATA_PATH), $utf8)
  [IO.File]::WriteAllText((Join-Path $volume.Path 'meta-data'), [IO.File]::ReadAllText($env:_CRABBOX_META_DATA_PATH), $utf8)
} finally {
  if ($mounted) {
    Dismount-VHD -Path $path -ErrorAction Stop
    [IO.File]::Delete($mountMarker)
  }
}`, escapePSString(seedPath), escapePSString(cloudInitMountMarker(seedPath)), cloudInitSeedSizeBytes)
}

func (b *backend) createNoCloudSeed(ctx context.Context, seedPath, userData, metaData string) (err error) {
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o700); err != nil {
		return err
	}
	userPath, err := writeNoCloudSource(filepath.Dir(seedPath), "user-data-", userData)
	if err != nil {
		return fmt.Errorf("prepare NoCloud user data: %w", err)
	}
	defer func() { err = errors.Join(err, os.Remove(userPath)) }()
	metaPath, err := writeNoCloudSource(filepath.Dir(seedPath), "meta-data-", metaData)
	if err != nil {
		return fmt.Errorf("prepare NoCloud metadata: %w", err)
	}
	defer func() { err = errors.Join(err, os.Remove(metaPath)) }()
	seedCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, runErr := b.powershellWithEnv(seedCtx, noCloudSeedScript(seedPath), append(os.Environ(),
		"_CRABBOX_USER_DATA_PATH="+userPath, "_CRABBOX_META_DATA_PATH="+metaPath))
	if runErr != nil {
		return shared.LocalCommandError("create NoCloud seed (requires same-user elevation for the new seed disk)", result, runErr)
	}
	return core.SecurePrivatePath(seedPath, false)
}

func (b *backend) dismountNoCloudSeed(ctx context.Context, name string) error {
	if err := validateLinuxStorageName(name); err != nil {
		return err
	}
	seed := ownedCloudInitSeedPath(name)
	// A seed attached to a running kept VM is not a host mount. Only our
	// persisted mount intent authorizes host-side dismount recovery.
	marker, err := os.Lstat(cloudInitMountMarker(seed))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if !marker.Mode().IsRegular() {
		return errors.New("NoCloud mount marker is not a regular file")
	}
	info, err := os.Lstat(seed)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("NoCloud seed is not a regular file; refusing dismount")
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$disk = Get-VHD -Path '%s' -ErrorAction Stop
if ($disk.Attached) { Dismount-VHD -Path '%s' -ErrorAction Stop }`, escapePSString(seed), escapePSString(seed))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("dismount owned NoCloud seed", result, err)
	}
	return b.removeVHDFile(cloudInitMountMarker(seed))
}

func (b *backend) detachOwnedNoCloudSeed(ctx context.Context, name, id string) error {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$vm = Get-VM -Name '%s' -ErrorAction Stop
if ([string]$vm.Id -ne '%s') { throw 'Hyper-V VM identity changed; refusing seed detach' }
Get-VMHardDiskDrive -VM $vm -ErrorAction Stop | Where-Object { $_.Path -eq '%s' } | Remove-VMHardDiskDrive -ErrorAction Stop
`, escapePSString(name), escapePSString(id), escapePSString(ownedCloudInitSeedPath(name)))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("detach owned NoCloud seed", result, err)
	}
	return b.removeVHDFile(ownedCloudInitSeedPath(name))
}

func (b *backend) attachNoCloudSeed(ctx context.Context, name, path string) error {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
	icacls.exe '%s' /grant '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null
	if ($LASTEXITCODE -ne 0) { throw 'NoCloud seed ACL update failed' }
	Add-VMHardDiskDrive -VMName '%s' -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 1 -Path '%s' -ErrorAction Stop`,
		escapePSString(path), escapePSString(name), escapePSString(path))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("attach NoCloud seed disk", result, err)
	}
	return nil
}

func (b *backend) detachNoCloudSeed(ctx context.Context, name, path string) error {
	script := fmt.Sprintf(
		`$disk = Get-VMHardDiskDrive -VMName '%s' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq '%s' }; `+
			`if ($disk) { $disk | Remove-VMHardDiskDrive -ErrorAction Stop }`,
		escapePSString(name), escapePSString(path))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("detach NoCloud seed disk", result, err)
	}
	return nil
}

func removeNoCloudSeed(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove NoCloud seed disk %s: %w", path, err)
	}
	return nil
}

func (b *backend) detachAndRemoveNoCloudSeed(ctx context.Context, name string) error {
	path := cloudInitSeedPath(name)
	if err := b.detachNoCloudSeed(ctx, name, path); err != nil {
		return err
	}
	return removeNoCloudSeed(path)
}
