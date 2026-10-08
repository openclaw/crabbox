package hyperv

import (
	"context"
	"fmt"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"os"
	"path/filepath"
)

const cloudInitSeedSizeBytes = 64 * 1024 * 1024

func cloudInitSeedPath(name string) string {
	return filepath.Join(hypervVHDDir(), name+"-seed.vhdx")
}

func cloudInitMetaData(name string) string {
	return cloudInitMetaDataForInstance(name, name)
}

func cloudInitMetaDataForInstance(instanceID, hostname string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)
}

func (b *backend) createNoCloudSeed(ctx context.Context, path, userData, metaData string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return core.Exit(2, "create NoCloud seed directory %s: %v", dir, err)
	}
	userDataPath, err := writeNoCloudSource(dir, "user-data-", userData)
	if err != nil {
		return err
	}
	defer os.Remove(userDataPath) //nolint:errcheck
	metaDataPath, err := writeNoCloudSource(dir, "meta-data-", metaData)
	if err != nil {
		return err
	}
	defer os.Remove(metaDataPath) //nolint:errcheck

	script := fmt.Sprintf(
		`$ErrorActionPreference = 'Stop'; `+
			`$path = '%s'; `+
			`New-VHD -Path $path -Dynamic -SizeBytes %d -ErrorAction Stop | Out-Null; `+
			`$mounted = $false; `+
			`try { `+
			`$vhd = Mount-VHD -Path $path -Passthru -ErrorAction Stop; `+
			`$mounted = $true; `+
			`$disk = $vhd | Get-Disk -ErrorAction Stop; `+
			`if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false }; `+
			`if ($disk.IsReadOnly) { Set-Disk -Number $disk.Number -IsReadOnly $false }; `+
			`Initialize-Disk -Number $disk.Number -PartitionStyle MBR -ErrorAction Stop | Out-Null; `+
			`$partition = New-Partition -DiskNumber $disk.Number -UseMaximumSize -AssignDriveLetter -ErrorAction Stop; `+
			`$partition | Format-Volume -FileSystem FAT -NewFileSystemLabel 'cidata' -Confirm:$false -Force -ErrorAction Stop | Out-Null; `+
			`$root = "$($partition.DriveLetter):\"; `+
			`$utf8 = New-Object System.Text.UTF8Encoding($false); `+
			`$userData = [System.IO.File]::ReadAllText($env:_CRABBOX_USER_DATA_PATH); `+
			`$metaData = [System.IO.File]::ReadAllText($env:_CRABBOX_META_DATA_PATH); `+
			`[System.IO.File]::WriteAllText((Join-Path $root 'user-data'), $userData, $utf8); `+
			`[System.IO.File]::WriteAllText((Join-Path $root 'meta-data'), $metaData, $utf8) `+
			`} finally { if ($mounted) { Dismount-VHD -Path $path -ErrorAction SilentlyContinue } }`,
		escapePSString(path), cloudInitSeedSizeBytes,
	)
	env := append(os.Environ(),
		"_CRABBOX_USER_DATA_PATH="+userDataPath,
		"_CRABBOX_META_DATA_PATH="+metaDataPath,
	)
	result, err := b.powershellWithEnv(ctx, script, env)
	if err != nil {
		os.Remove(path) //nolint:errcheck
		return shared.LocalCommandError("create NoCloud seed disk", result, err)
	}
	return nil
}

func writeNoCloudSource(dir, pattern, content string) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create NoCloud source file: %w", err)
	}
	path := file.Name()
	remove := true
	defer func() {
		if remove {
			os.Remove(path) //nolint:errcheck
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		file.Close() //nolint:errcheck
		return "", fmt.Errorf("secure NoCloud source file %s: %w", path, err)
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close() //nolint:errcheck
		return "", fmt.Errorf("write NoCloud source file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close NoCloud source file %s: %w", path, err)
	}
	remove = false
	return path, nil
}

func (b *backend) attachNoCloudSeed(ctx context.Context, name, path string) error {
	script := fmt.Sprintf(
		`Add-VMHardDiskDrive -VMName '%s' -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 1 -Path '%s'`,
		escapePSString(name), escapePSString(path),
	)
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
		escapePSString(name), escapePSString(path),
	)
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
