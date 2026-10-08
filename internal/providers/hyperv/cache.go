package hyperv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	hypervCacheMetadataVersion = 1
	hypervCacheDefaultSizeGB   = 80
	hypervCacheDetachedGuard   = "crabbox-cache-detached-v1"
)

var hypervCacheLockWait = 10 * time.Second

type hypervCacheMetadata struct {
	Version      int    `json:"version"`
	Key          string `json:"key"`
	Target       string `json:"target"`
	Filesystem   string `json:"filesystem"`
	DiskID       string `json:"diskID"`
	FilesystemID string `json:"filesystemID,omitempty"`
	SizeGB       int    `json:"sizeGB"`
}

type hypervCacheAttachment struct {
	VMName             string `json:"VMName"`
	Path               string `json:"Path"`
	ControllerType     string `json:"ControllerType"`
	ControllerNumber   int    `json:"ControllerNumber"`
	ControllerLocation int    `json:"ControllerLocation"`
}

type hypervCacheAttachOutput struct {
	ControllerLocation int `json:"ControllerLocation"`
}

type hypervCacheCreateOutput struct {
	DiskID string `json:"DiskID"`
}

type hypervCacheVolumePaths struct {
	vhd      string
	metadata string
	lock     string
}

type hypervCacheLock struct {
	lock *flock.Flock
}

type hypervDetachedCacheVolume struct {
	volume   core.CacheVolumeConfig
	metadata hypervCacheMetadata
	paths    hypervCacheVolumePaths
	lock     *hypervCacheLock
	restore  bool
}

type hypervCacheLeaseFatalError struct {
	cause error
}

func (e hypervCacheLeaseFatalError) Error() string {
	return e.cause.Error()
}

func (e hypervCacheLeaseFatalError) Unwrap() error {
	return e.cause
}

func hypervCacheRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = `C:\Users\Public`
	}
	return filepath.Join(home, "Hyper-V", "Crabbox Cache Volumes")
}

func hypervCacheVolumeName(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return "cache-" + hex.EncodeToString(sum[:16])
}

func (b *backend) cacheVolumePaths(key string) hypervCacheVolumePaths {
	base := filepath.Join(b.cacheRoot, hypervCacheVolumeName(key))
	return hypervCacheVolumePaths{
		vhd:      base + ".vhdx",
		metadata: base + ".json",
		lock:     base + ".lock",
	}
}

func (b *backend) attachConfiguredCacheVolumes(ctx context.Context, vmName string, cfg core.Config, lease core.LeaseTarget) ([]core.CacheVolumeConfig, error) {
	if len(cfg.Cache.Volumes) == 0 {
		return nil, nil
	}
	if err := validateHyperVCacheVolumeSet(cfg); err != nil {
		return nil, err
	}
	attached := make([]core.CacheVolumeConfig, 0, len(cfg.Cache.Volumes))
	for _, volume := range cfg.Cache.Volumes {
		if err := b.attachCacheVolume(ctx, vmName, cfg, lease.SSH, volume); err != nil {
			var fatal hypervCacheLeaseFatalError
			if errors.As(err, &fatal) {
				return attached, err
			}
			if volume.Required {
				return attached, fmt.Errorf("attach required Hyper-V cache volume %q: %w", shared.FirstNonBlank(volume.Name, volume.Key), err)
			}
			fmt.Fprintf(b.rt.Stderr, "warning: skip optional Hyper-V cache volume %q: %v\n", shared.FirstNonBlank(volume.Name, volume.Key), err)
			continue
		}
		attached = append(attached, volume)
	}
	return attached, nil
}

func validateHyperVCacheVolumeSet(cfg core.Config) error {
	keys := map[string]struct{}{}
	paths := make([]string, 0, len(cfg.Cache.Volumes))
	for _, volume := range cfg.Cache.Volumes {
		if err := validateHyperVCacheMountPath(cfg, volume); err != nil {
			return err
		}
		key := strings.TrimSpace(volume.Key)
		if _, ok := keys[key]; ok {
			return core.Exit(2, "Hyper-V cache volume key %q is configured more than once", key)
		}
		keys[key] = struct{}{}
		mountPath := normalizedCacheMountPath(cfg.TargetOS, volume.Path)
		for _, existing := range paths {
			overlaps := false
			if cfg.TargetOS == targetWindows {
				overlaps = windowsPathWithin(existing, mountPath) || windowsPathWithin(mountPath, existing)
			} else {
				overlaps = posixPathWithin(existing, mountPath) || posixPathWithin(mountPath, existing)
			}
			if overlaps {
				return core.Exit(2, "Hyper-V cache volume path %q overlaps another configured cache mount", volume.Path)
			}
		}
		paths = append(paths, mountPath)
	}
	return nil
}

func validateHyperVCacheMountPath(cfg core.Config, volume core.CacheVolumeConfig) error {
	mountPath := strings.TrimSpace(volume.Path)
	workRoot := strings.TrimSpace(cfg.HyperV.WorkRoot)
	switch cfg.TargetOS {
	case targetWindows:
		normalized := normalizedWindowsCachePath(mountPath)
		if len(normalized) == 2 && normalized[1] == ':' {
			return core.Exit(2, "Hyper-V cache volume path %q must name a directory, not a drive root", volume.Path)
		}
		if strings.Contains(normalized, `\..\`) || strings.HasSuffix(normalized, `\..`) || strings.Contains(normalized, `\.\`) || strings.HasSuffix(normalized, `\.`) {
			return core.Exit(2, "Hyper-V cache volume path %q must not contain traversal segments", volume.Path)
		}
		normalizedWorkRoot := normalizedWindowsCachePath(workRoot)
		if workRoot != "" && (windowsPathWithin(normalizedWorkRoot, normalized) || windowsPathWithin(normalized, normalizedWorkRoot)) {
			return core.Exit(2, "Hyper-V cache volume path %q must be outside the synced work root %q", volume.Path, workRoot)
		}
	case targetLinux:
		normalized := path.Clean(mountPath)
		if normalized == "/" {
			return core.Exit(2, "Hyper-V cache volume path %q must name a directory, not the filesystem root", volume.Path)
		}
		if strings.TrimRight(mountPath, "/") != normalized {
			return core.Exit(2, "Hyper-V cache volume path %q must not contain traversal segments", volume.Path)
		}
		normalizedWorkRoot := path.Clean(workRoot)
		if workRoot != "" && (posixPathWithin(normalizedWorkRoot, normalized) || posixPathWithin(normalized, normalizedWorkRoot)) {
			return core.Exit(2, "Hyper-V cache volume path %q must be outside the synced work root %q", volume.Path, workRoot)
		}
	default:
		return core.Exit(2, "provider=%s cache volumes require target=linux or target=windows", providerName)
	}
	return nil
}

func normalizedCacheMountPath(targetOS, value string) string {
	if targetOS == targetWindows {
		return strings.ToLower(normalizedWindowsCachePath(value))
	}
	return path.Clean(strings.TrimSpace(value))
}

func normalizedWindowsCachePath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "/", `\`)
	for strings.Contains(value, `\\`) {
		value = strings.ReplaceAll(value, `\\`, `\`)
	}
	return strings.TrimRight(value, `\`)
}

func windowsPathWithin(root, candidate string) bool {
	root = strings.ToLower(strings.TrimRight(root, `\`))
	candidate = strings.ToLower(strings.TrimRight(candidate, `\`))
	return candidate == root || strings.HasPrefix(candidate, root+`\`)
}

func posixPathWithin(root, candidate string) bool {
	root = strings.TrimRight(root, "/")
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func (b *backend) attachCacheVolume(ctx context.Context, vmName string, cfg core.Config, target core.SSHTarget, volume core.CacheVolumeConfig) (err error) {
	paths := b.cacheVolumePaths(volume.Key)
	lock, err := acquireHyperVCacheLock(ctx, paths.lock)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, lock.release())
	}()
	return b.attachCacheVolumeLocked(ctx, vmName, cfg, target, volume, paths)
}

func (b *backend) attachCacheVolumeLocked(ctx context.Context, vmName string, cfg core.Config, target core.SSHTarget, volume core.CacheVolumeConfig, paths hypervCacheVolumePaths) (err error) {
	metadata, err := b.ensureHyperVCacheVolume(ctx, cfg, volume, paths)
	if err != nil {
		return err
	}
	_, added, err := b.attachHyperVCacheDisk(ctx, vmName, paths.vhd)
	defer func() {
		if err == nil || !added {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), hypervCheckpointCleanupTimeout)
		defer cancel()
		var rollbackErr error
		switch cfg.TargetOS {
		case targetWindows:
			rollbackErr = b.unmountWindowsCacheVolume(rollbackCtx, vmName, cfg.HyperV.User, volume, metadata)
		case targetLinux:
			if metadata.FilesystemID == "" {
				err = hypervCacheLeaseFatalError{cause: err}
				return
			}
			rollbackErr = b.unmountLinuxCacheVolume(rollbackCtx, target, volume, metadata)
		}
		if rollbackErr == nil {
			rollbackErr = b.detachHyperVCacheDisk(rollbackCtx, vmName, paths.vhd)
		}
		if rollbackErr != nil {
			err = hypervCacheLeaseFatalError{cause: errors.Join(err, rollbackErr)}
		}
	}()
	if err != nil {
		return err
	}

	switch cfg.TargetOS {
	case targetWindows:
		err = b.mountWindowsCacheVolume(ctx, vmName, cfg.HyperV.User, volume, metadata)
	case targetLinux:
		metadata.FilesystemID, err = b.mountLinuxCacheVolume(ctx, target, cfg.HyperV.User, volume, metadata)
		if err == nil {
			err = writeHyperVCacheMetadata(paths.metadata, metadata)
		}
	default:
		err = core.Exit(2, "provider=%s cache volumes require target=linux or target=windows", providerName)
	}
	return err
}

func (b *backend) ensureHyperVCacheVolume(ctx context.Context, cfg core.Config, volume core.CacheVolumeConfig, paths hypervCacheVolumePaths) (hypervCacheMetadata, error) {
	target, filesystem := hypervCacheFormat(cfg.TargetOS)
	sizeGB := volume.SizeGB
	if sizeGB <= 0 {
		sizeGB = cfg.Cache.MaxGB
	}
	if sizeGB <= 0 {
		sizeGB = hypervCacheDefaultSizeGB
	}
	if int64(sizeGB) > (1<<63-1)/(1024*1024*1024) {
		return hypervCacheMetadata{}, core.Exit(2, "Hyper-V cache volume %q sizeGB is too large", volume.Key)
	}
	expected := hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        strings.TrimSpace(volume.Key),
		Target:     target,
		Filesystem: filesystem,
		SizeGB:     sizeGB,
	}
	metadata, found, err := readHyperVCacheMetadata(paths.metadata)
	if err != nil {
		return hypervCacheMetadata{}, err
	}
	if !found {
		tempMetadata, tempFound, tempErr := readHyperVCacheMetadata(paths.metadata + ".tmp")
		if tempErr == nil && tempFound {
			if validateErr := validateHyperVCacheMetadata(tempMetadata, expected); validateErr == nil {
				if _, statErr := os.Stat(paths.vhd); statErr == nil {
					if renameErr := os.Rename(paths.metadata+".tmp", paths.metadata); renameErr != nil {
						return hypervCacheMetadata{}, core.Exit(2, "recover Hyper-V cache metadata %s: %v", paths.metadata, renameErr)
					}
					return tempMetadata, nil
				}
			}
		}
		_ = os.Remove(paths.metadata + ".tmp")
	}
	if found {
		if err := validateHyperVCacheMetadata(metadata, expected); err != nil {
			return hypervCacheMetadata{}, err
		}
		if _, err := os.Stat(paths.vhd); err != nil {
			return hypervCacheMetadata{}, core.Exit(2, "Hyper-V cache volume %q metadata exists but VHDX %s is unavailable: %v", volume.Key, paths.vhd, err)
		}
		return metadata, nil
	}
	if _, err := os.Stat(paths.vhd); err == nil {
		attachments, queryErr := b.queryHyperVCacheAttachments(ctx, paths.vhd)
		if queryErr != nil {
			return hypervCacheMetadata{}, queryErr
		}
		if len(attachments) > 0 {
			return hypervCacheMetadata{}, core.Exit(2, "refusing to recover Hyper-V cache VHDX %s without metadata while it is attached to VM %s", paths.vhd, attachments[0].VMName)
		}
		if err := os.Remove(paths.vhd); err != nil {
			return hypervCacheMetadata{}, core.Exit(2, "remove unattached Hyper-V cache VHDX without metadata %s: %v", paths.vhd, err)
		}
	} else if !os.IsNotExist(err) {
		return hypervCacheMetadata{}, err
	}
	_ = os.Remove(paths.metadata + ".tmp")
	if err := os.MkdirAll(b.cacheRoot, 0o700); err != nil {
		return hypervCacheMetadata{}, core.Exit(2, "create Hyper-V cache root %s: %v", b.cacheRoot, err)
	}
	sizeBytes := int64(sizeGB) * 1024 * 1024 * 1024
	script := hypervCacheCreateScript(paths.vhd, sizeBytes)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		_ = os.Remove(paths.vhd)
		return hypervCacheMetadata{}, shared.LocalCommandError("create Hyper-V cache VHDX", result, runErr)
	}
	var created hypervCacheCreateOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &created); err != nil || strings.TrimSpace(created.DiskID) == "" {
		_ = os.Remove(paths.vhd)
		if err != nil {
			return hypervCacheMetadata{}, core.Exit(2, "parse Hyper-V cache VHDX identity: %v", err)
		}
		return hypervCacheMetadata{}, core.Exit(2, "Hyper-V cache VHDX did not report a disk identity")
	}
	expected.DiskID = strings.TrimSpace(created.DiskID)
	if err := writeHyperVCacheMetadata(paths.metadata, expected); err != nil {
		_ = os.Remove(paths.vhd)
		return hypervCacheMetadata{}, err
	}
	return expected, nil
}

func hypervCacheCreateScript(vhdPath string, sizeBytes int64) string {
	return fmt.Sprintf(
		`$ErrorActionPreference='Stop'; New-VHD -Path '%s' -Dynamic -SizeBytes %d -ErrorAction Stop | Out-Null; `+
			`try { $disk=Mount-VHD -Path '%s' -NoDriveLetter -Passthru -ErrorAction Stop | Get-Disk -ErrorAction Stop; `+
			`[pscustomobject]@{DiskID=[string]$disk.UniqueId} | ConvertTo-Json -Compress `+
			`} finally { Dismount-VHD -Path '%s' -ErrorAction SilentlyContinue }`,
		escapePSString(vhdPath),
		sizeBytes,
		escapePSString(vhdPath),
		escapePSString(vhdPath),
	)
}

func hypervCacheFormat(targetOS string) (string, string) {
	if targetOS == targetWindows {
		return "windows/normal", "ntfs"
	}
	return targetLinux, "ext4"
}

func readHyperVCacheMetadata(metadataPath string) (hypervCacheMetadata, bool, error) {
	data, err := os.ReadFile(metadataPath)
	if os.IsNotExist(err) {
		return hypervCacheMetadata{}, false, nil
	}
	if err != nil {
		return hypervCacheMetadata{}, false, core.Exit(2, "read Hyper-V cache metadata %s: %v", metadataPath, err)
	}
	var metadata hypervCacheMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return hypervCacheMetadata{}, false, core.Exit(2, "parse Hyper-V cache metadata %s: %v", metadataPath, err)
	}
	return metadata, true, nil
}

func validateHyperVCacheMetadata(actual, expected hypervCacheMetadata) error {
	if actual.Version != hypervCacheMetadataVersion {
		return core.Exit(2, "Hyper-V cache volume %q uses unsupported metadata version %d", expected.Key, actual.Version)
	}
	if actual.Key != expected.Key || actual.Target != expected.Target || actual.Filesystem != expected.Filesystem {
		return core.Exit(2, "Hyper-V cache key %q is already owned by target=%s filesystem=%s, not target=%s filesystem=%s",
			expected.Key, actual.Target, actual.Filesystem, expected.Target, expected.Filesystem)
	}
	if strings.TrimSpace(actual.DiskID) == "" {
		return core.Exit(2, "Hyper-V cache volume %q metadata is missing its disk identity", expected.Key)
	}
	if expected.SizeGB > 0 && actual.SizeGB < expected.SizeGB {
		return core.Exit(2, "Hyper-V cache volume %q is %dGB but the request requires at least %dGB", expected.Key, actual.SizeGB, expected.SizeGB)
	}
	return nil
}

func writeHyperVCacheMetadata(metadataPath string, metadata hypervCacheMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tempPath := metadataPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return core.Exit(2, "write Hyper-V cache metadata %s: %v", tempPath, err)
	}
	if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tempPath)
		return core.Exit(2, "replace Hyper-V cache metadata %s: %v", metadataPath, err)
	}
	if err := os.Rename(tempPath, metadataPath); err != nil {
		_ = os.Remove(tempPath)
		return core.Exit(2, "publish Hyper-V cache metadata %s: %v", metadataPath, err)
	}
	return nil
}

func acquireHyperVCacheLock(ctx context.Context, lockPath string) (*hypervCacheLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, core.Exit(2, "create Hyper-V cache lock directory: %v", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, hypervCacheLockWait)
	defer cancel()
	fileLock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := fileLock.TryLockContext(lockCtx, 100*time.Millisecond)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, core.Exit(5, "Hyper-V cache volume is busy while another host operation holds %s", lockPath)
		}
		return nil, core.Exit(2, "acquire Hyper-V cache lock %s: %v", lockPath, err)
	}
	if !locked {
		return nil, core.Exit(5, "Hyper-V cache volume is busy while another host operation holds %s", lockPath)
	}
	return &hypervCacheLock{lock: fileLock}, nil
}

func (l *hypervCacheLock) release() error {
	if l == nil || l.lock == nil {
		return nil
	}
	return l.lock.Unlock()
}

func (b *backend) attachHyperVCacheDisk(ctx context.Context, vmName, vhdPath string) (hypervCacheAttachment, bool, error) {
	attachments, err := b.queryHyperVCacheAttachments(ctx, vhdPath)
	if err != nil {
		return hypervCacheAttachment{}, false, err
	}
	for _, attachment := range attachments {
		if strings.EqualFold(attachment.VMName, vmName) {
			return attachment, false, nil
		}
	}
	if len(attachments) > 0 {
		return hypervCacheAttachment{}, false, core.Exit(5, "Hyper-V cache VHDX %s is already attached to VM %s", vhdPath, attachments[0].VMName)
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $used=@(Get-VMHardDiskDrive -VMName '%s' -ControllerType SCSI -ControllerNumber 0 -ErrorAction Stop | ForEach-Object ControllerLocation); `+
			`$location=1..63 | Where-Object { $used -notcontains $_ } | Select-Object -First 1; if ($null -eq $location) { throw 'no free Hyper-V SCSI location' }; `+
			`Add-VMHardDiskDrive -VMName '%s' -ControllerType SCSI -ControllerNumber 0 -ControllerLocation $location -Path '%s' -ErrorAction Stop; `+
			`[pscustomobject]@{ControllerLocation=$location} | ConvertTo-Json -Compress`,
		escapePSString(vmName),
		escapePSString(vmName),
		escapePSString(vhdPath),
	)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		attachErr := shared.LocalCommandError("attach Hyper-V cache VHDX", result, runErr)
		reconcileCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		reconciled, queryErr := b.queryHyperVCacheAttachments(reconcileCtx, vhdPath)
		if queryErr != nil {
			return hypervCacheAttachment{}, false, errors.Join(attachErr, queryErr)
		}
		for _, attachment := range reconciled {
			if strings.EqualFold(attachment.VMName, vmName) {
				return attachment, true, attachErr
			}
		}
		return hypervCacheAttachment{}, false, attachErr
	}
	var output hypervCacheAttachOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &output); err != nil {
		return hypervCacheAttachment{}, true, core.Exit(2, "parse Hyper-V cache attachment: %v", err)
	}
	return hypervCacheAttachment{
		VMName:             vmName,
		Path:               vhdPath,
		ControllerType:     "SCSI",
		ControllerNumber:   0,
		ControllerLocation: output.ControllerLocation,
	}, true, nil
}

func (b *backend) queryHyperVCacheAttachments(ctx context.Context, vhdPath string) ([]hypervCacheAttachment, error) {
	script := fmt.Sprintf(
		`$items=@(Get-VM -ErrorAction Stop | ForEach-Object { Get-VMHardDiskDrive -VM $_ -ErrorAction Stop } | `+
			`Where-Object { [IO.Path]::GetFullPath($_.Path).Equals([IO.Path]::GetFullPath('%s'),[StringComparison]::OrdinalIgnoreCase) } | `+
			`Select-Object VMName,Path,@{Name='ControllerType';Expression={$_.ControllerType.ToString()}},ControllerNumber,ControllerLocation); `+
			`ConvertTo-Json -InputObject $items -Compress`,
		escapePSString(vhdPath),
	)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		return nil, shared.LocalCommandError("query Hyper-V cache attachments", result, runErr)
	}
	var attachments []hypervCacheAttachment
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &attachments); err != nil {
		return nil, core.Exit(2, "parse Hyper-V cache attachments: %v", err)
	}
	return attachments, nil
}

func (b *backend) detachHyperVCacheDisk(ctx context.Context, vmName, vhdPath string) error {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; Get-VMHardDiskDrive -VMName '%s' -ErrorAction SilentlyContinue | `+
			`Where-Object { [IO.Path]::GetFullPath($_.Path).Equals([IO.Path]::GetFullPath('%s'),[StringComparison]::OrdinalIgnoreCase) } | `+
			`Remove-VMHardDiskDrive -ErrorAction Stop`,
		escapePSString(vmName),
		escapePSString(vhdPath),
	)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		return shared.LocalCommandError("detach Hyper-V cache VHDX", result, runErr)
	}
	return nil
}

func (b *backend) mountWindowsCacheVolume(ctx context.Context, vmName, user string, volume core.CacheVolumeConfig, metadata hypervCacheMetadata) error {
	label := hypervCacheVolumeName(volume.Key)
	if len(label) > 32 {
		label = label[:32]
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $diskID='%s'.Replace('-','').Replace('{','').Replace('}',''); $target=[IO.Path]::GetFullPath('%s').TrimEnd('\'); $guard='%s'; `+
			`$targetRoot=[IO.Path]::GetPathRoot($target); if (-not (Test-Path -LiteralPath $targetRoot)) { throw ('Crabbox cache mount drive does not exist: '+$targetRoot) }; `+
			`$disk=$null; for ($attempt=0; $attempt -lt 30 -and -not $disk; $attempt++) { `+
			`$disk=Get-Disk | Where-Object { $_.UniqueId -and $_.UniqueId.Replace('-','').Replace('{','').Replace('}','').Trim().EndsWith($diskID,[StringComparison]::OrdinalIgnoreCase) } | Select-Object -First 1; `+
			`if (-not $disk) { Start-Sleep -Seconds 1 } }; `+
			`if (-not $disk) { throw 'attached Crabbox cache disk was not found by disk identity' }; `+
			`if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction Stop }; if ($disk.IsReadOnly) { Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction Stop }; `+
			`$newPartition=$false; if ($disk.PartitionStyle -eq 'RAW') { Initialize-Disk -Number $disk.Number -PartitionStyle GPT -ErrorAction Stop | Out-Null; $partition=New-Partition -DiskNumber $disk.Number -UseMaximumSize -ErrorAction Stop; $newPartition=$true } `+
			`else { $partition=Get-Partition -DiskNumber $disk.Number -ErrorAction Stop | Where-Object Type -eq 'Basic' | Select-Object -First 1 }; `+
			`if (-not $partition) { throw 'Crabbox cache disk has no usable partition' }; `+
			`if ($newPartition) { $volume=$partition | Format-Volume -FileSystem NTFS -NewFileSystemLabel '%s' -Confirm:$false -Force -ErrorAction Stop } `+
			`else { $volume=$partition | Get-Volume -ErrorAction SilentlyContinue; if (-not $volume -or [string]::IsNullOrWhiteSpace($volume.FileSystem)) { throw 'existing Crabbox cache filesystem could not be identified' }; `+
			`if ($volume.FileSystem -ne 'NTFS') { throw ('Crabbox cache filesystem mismatch: '+$volume.FileSystem) } }; `+
			`$mounted=@($partition.AccessPaths | ForEach-Object { if ($_){[IO.Path]::GetFullPath($_).TrimEnd('\')} }); `+
			`if ($mounted -notcontains $target) { `+
			`if (Test-Path -LiteralPath $target -PathType Leaf) { if ((Get-Content -Raw -LiteralPath $target) -ne $guard) { throw 'Crabbox cache mount path is occupied by an unexpected file' }; Remove-Item -LiteralPath $target -Force -ErrorAction Stop } `+
			`elseif ((Test-Path -LiteralPath $target) -and -not (Test-Path -LiteralPath $target -PathType Container)) { throw 'Crabbox cache mount path is not a directory' }; `+
			`if (-not (Test-Path -LiteralPath $target)) { New-Item -ItemType Directory -Path $target -Force -ErrorAction Stop | Out-Null }; `+
			`if (Get-ChildItem -LiteralPath $target -Force -ErrorAction Stop | Select-Object -First 1) { throw 'Crabbox cache mount path is not empty' }; `+
			`Add-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $partition.PartitionNumber -AccessPath $target -ErrorAction Stop }; `+
			`icacls.exe $target /grant ($env:USERNAME+':(OI)(CI)M') /C | Out-Null; if ($LASTEXITCODE -ne 0) { throw 'grant cache directory access failed' }`,
		escapePSString(metadata.DiskID),
		escapePSString(volume.Path),
		escapePSString(hypervCacheDetachedGuard),
		escapePSString(label),
	)
	return b.invokeInGuest(ctx, vmName, user, script, "mount Hyper-V Windows cache volume")
}

func (b *backend) unmountWindowsCacheVolume(ctx context.Context, vmName, user string, volume core.CacheVolumeConfig, metadata hypervCacheMetadata) error {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $diskID='%s'.Replace('-','').Replace('{','').Replace('}',''); $target=[IO.Path]::GetFullPath('%s').TrimEnd('\'); $guard='%s'; `+
			`$disk=Get-Disk | Where-Object { $_.UniqueId -and $_.UniqueId.Replace('-','').Replace('{','').Replace('}','').Trim().EndsWith($diskID,[StringComparison]::OrdinalIgnoreCase) } | Select-Object -First 1; `+
			`if (-not $disk) { return }; $partition=Get-Partition -DiskNumber $disk.Number -ErrorAction SilentlyContinue | Where-Object Type -eq 'Basic' | Select-Object -First 1; `+
			`if (-not $partition) { return }; $access=$partition.AccessPaths | Where-Object { $_ -and [IO.Path]::GetFullPath($_).TrimEnd('\').Equals($target,[StringComparison]::OrdinalIgnoreCase) } | Select-Object -First 1; `+
			`if ($access) { Remove-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $partition.PartitionNumber -AccessPath $access -ErrorAction Stop }; `+
			`if (Test-Path -LiteralPath $target -PathType Leaf) { if ((Get-Content -Raw -LiteralPath $target) -ne $guard) { throw 'Crabbox cache mount path is occupied by an unexpected file' } } `+
			`else { if (Test-Path -LiteralPath $target) { if (-not (Test-Path -LiteralPath $target -PathType Container)) { throw 'Crabbox cache mount path is not a directory' }; `+
			`if (Get-ChildItem -LiteralPath $target -Force -ErrorAction Stop | Select-Object -First 1) { throw 'Crabbox cache mount path changed after unmount' }; Remove-Item -LiteralPath $target -Force -ErrorAction Stop }; `+
			`Set-Content -NoNewline -Encoding ASCII -LiteralPath $target -Value $guard -ErrorAction Stop }; `+
			`Set-Disk -Number $disk.Number -IsOffline $true -ErrorAction Stop`,
		escapePSString(metadata.DiskID),
		escapePSString(volume.Path),
		escapePSString(hypervCacheDetachedGuard),
	)
	return b.invokeInGuest(ctx, vmName, user, script, "unmount Hyper-V Windows cache volume")
}

func (b *backend) mountLinuxCacheVolume(ctx context.Context, target core.SSHTarget, user string, volume core.CacheVolumeConfig, metadata hypervCacheMetadata) (string, error) {
	expectedUUID := strings.TrimSpace(metadata.FilesystemID)
	diskID := strings.ToLower(strings.NewReplacer("-", "", "{", "", "}", "").Replace(strings.TrimSpace(metadata.DiskID)))
	fstabPath := escapeFstabPath(path.Clean(volume.Path))
	label := hypervCacheVolumeName(volume.Key)
	if len(label) > 16 {
		label = label[:16]
	}
	script := strings.Join([]string{
		"set -eu",
		"expected_uuid=" + posixShellQuote(expectedUUID),
		"disk_id=" + posixShellQuote(diskID),
		"mount_path=" + posixShellQuote(path.Clean(volume.Path)),
		"fstab_path=" + posixShellQuote(fstabPath),
		"guard=" + posixShellQuote(hypervCacheDetachedGuard),
		"device=''",
		`[ -n "$disk_id" ] || { echo 'Crabbox cache disk identity is missing' >&2; exit 1; }`,
		`for attempt in $(seq 1 30); do`,
		`  for block in /sys/class/block/*; do`,
		`    name="$(basename "$block")"`,
		`    [ "$(lsblk -dn -o TYPE "/dev/$name" 2>/dev/null || true)" = disk ] || continue`,
		`    serial="$(lsblk -dn -o SERIAL "/dev/$name" 2>/dev/null || true)"`,
		`    normalized_serial="$(printf '%s' "$serial" | tr -cd '[:alnum:]' | tr '[:upper:]' '[:lower:]')"`,
		`    case "$normalized_serial" in *"$disk_id") device="/dev/$name"; break 2 ;; esac`,
		`  done`,
		`  sleep 1`,
		`done`,
		`[ -n "$device" ] || { echo 'attached Crabbox cache disk was not found by SCSI location' >&2; exit 1; }`,
		`fstype="$(sudo blkid -o value -s TYPE "$device" 2>/dev/null || true)"`,
		`if [ -n "$expected_uuid" ]; then`,
		`  [ "$fstype" = ext4 ] || { echo 'existing Crabbox cache filesystem could not be identified as ext4' >&2; exit 1; }`,
		`else`,
		`  if [ -z "$fstype" ]; then sudo mkfs.ext4 -F -L ` + posixShellQuote(label) + ` "$device" >/dev/null; fstype=ext4; fi`,
		`fi`,
		`[ "$fstype" = ext4 ] || { echo "Crabbox cache filesystem mismatch: $fstype" >&2; exit 1; }`,
		`uuid="$(sudo blkid -o value -s UUID "$device")"`,
		`if [ -n "$expected_uuid" ] && [ "$uuid" != "$expected_uuid" ]; then echo 'Crabbox cache UUID mismatch' >&2; exit 1; fi`,
		`if [ -f "$mount_path" ]; then`,
		`  [ "$(sudo cat "$mount_path")" = "$guard" ] || { echo 'Crabbox cache mount path is occupied by an unexpected file' >&2; exit 1; }`,
		`  sudo rm -f "$mount_path"`,
		`elif [ -e "$mount_path" ] && [ ! -d "$mount_path" ]; then`,
		`  echo 'Crabbox cache mount path is not a directory' >&2`,
		`  exit 1`,
		`fi`,
		`sudo mkdir -p "$mount_path"`,
		`if mountpoint -q "$mount_path"; then`,
		`  mounted_uuid="$(findmnt -n -o UUID --target "$mount_path" || true)"`,
		`  [ "$mounted_uuid" = "$uuid" ] || { echo 'Crabbox cache mount path is already occupied' >&2; exit 1; }`,
		`else`,
		`  [ -z "$(find "$mount_path" -mindepth 1 -maxdepth 1 -print -quit)" ] || { echo 'Crabbox cache mount path is not empty' >&2; exit 1; }`,
		`  sudo mount -U "$uuid" "$mount_path"`,
		`fi`,
		`entry="UUID=$uuid $fstab_path ext4 defaults,nofail 0 2"`,
		`fstab_tmp="$(mktemp)"`,
		`FSTAB_MOUNT_PATH="$fstab_path" awk 'NF < 2 || $2 != ENVIRON["FSTAB_MOUNT_PATH"] { print }' /etc/fstab >"$fstab_tmp"`,
		`printf '%s\n' "$entry" >>"$fstab_tmp"`,
		`sudo install -m 0644 "$fstab_tmp" /etc/fstab`,
		`rm -f "$fstab_tmp"`,
		`sudo chown ` + posixShellQuote(user+":"+user) + ` "$mount_path"`,
		`printf '%s\n' "$uuid"`,
	}, "\n")
	output, err := b.runSSHOutput(ctx, target, script)
	if err != nil {
		return "", fmt.Errorf("mount Hyper-V Linux cache volume: %w", err)
	}
	uuid := strings.TrimSpace(output)
	if uuid == "" {
		return "", core.Exit(2, "Hyper-V Linux cache mount did not report a filesystem UUID")
	}
	return uuid, nil
}

func escapeFstabPath(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\134`,
		" ", `\040`,
		"\t", `\011`,
		"\n", `\012`,
	)
	return replacer.Replace(value)
}

func (b *backend) unmountLinuxCacheVolume(ctx context.Context, target core.SSHTarget, volume core.CacheVolumeConfig, metadata hypervCacheMetadata) error {
	script := strings.Join([]string{
		"set -eu",
		"mount_path=" + posixShellQuote(path.Clean(volume.Path)),
		"fstab_path=" + posixShellQuote(escapeFstabPath(path.Clean(volume.Path))),
		"expected_uuid=" + posixShellQuote(strings.TrimSpace(metadata.FilesystemID)),
		"guard=" + posixShellQuote(hypervCacheDetachedGuard),
		`if mountpoint -q "$mount_path"; then`,
		`  mounted_uuid="$(findmnt -n -o UUID --target "$mount_path" || true)"`,
		`  [ -z "$expected_uuid" ] || [ "$mounted_uuid" = "$expected_uuid" ] || { echo 'Crabbox cache mount identity changed' >&2; exit 1; }`,
		`  sudo umount "$mount_path"`,
		`fi`,
		`fstab_tmp="$(mktemp)"`,
		`FSTAB_MOUNT_PATH="$fstab_path" awk 'NF < 2 || $2 != ENVIRON["FSTAB_MOUNT_PATH"] { print }' /etc/fstab >"$fstab_tmp"`,
		`sudo install -m 0644 "$fstab_tmp" /etc/fstab`,
		`rm -f "$fstab_tmp"`,
		`if [ -f "$mount_path" ]; then`,
		`  [ "$(sudo cat "$mount_path")" = "$guard" ] || { echo 'Crabbox cache mount path is occupied by an unexpected file' >&2; exit 1; }`,
		`else`,
		`  if [ -e "$mount_path" ]; then`,
		`    [ -d "$mount_path" ] || { echo 'Crabbox cache mount path is not a directory' >&2; exit 1; }`,
		`    [ -z "$(sudo find "$mount_path" -mindepth 1 -maxdepth 1 -print -quit)" ] || { echo 'Crabbox cache mount path changed after unmount' >&2; exit 1; }`,
		`    sudo rmdir "$mount_path"`,
		`  fi`,
		`  printf '%s' "$guard" | sudo tee "$mount_path" >/dev/null`,
		`  sudo chmod 000 "$mount_path"`,
		`fi`,
	}, "\n")
	if _, err := b.runSSHOutput(ctx, target, script); err != nil {
		return fmt.Errorf("unmount Hyper-V Linux cache volume: %w", err)
	}
	return nil
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (b *backend) lockLeaseCacheVolumes(ctx context.Context, leaseID string, cfg core.Config, target core.SSHTarget, requireMetadata bool) ([]hypervDetachedCacheVolume, core.SSHTarget, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return nil, target, err
	}
	if !ok || len(claim.CacheVolumes) == 0 {
		return nil, target, nil
	}
	applyStoredLeaseKey(&cfg, leaseID)
	if target.Host == "" && claim.SSHHost != "" {
		target = core.SSHTargetFromConfig(cfg, claim.SSHHost)
		if claim.SSHPort > 0 {
			target.Port = strconv.Itoa(claim.SSHPort)
		}
	} else if target.Key == "" {
		target.Key = cfg.SSHKey
	}
	detached := make([]hypervDetachedCacheVolume, 0, len(claim.CacheVolumes))
	for _, spec := range claim.CacheVolumes {
		volume, err := core.ParseCacheVolumeSpec(spec)
		if err != nil {
			return detached, target, err
		}
		volume.Required = true
		paths := b.cacheVolumePaths(volume.Key)
		lock, err := acquireHyperVCacheLock(ctx, paths.lock)
		if err != nil {
			return detached, target, err
		}
		detached = append(detached, hypervDetachedCacheVolume{volume: volume, paths: paths, lock: lock})
		if !requireMetadata {
			continue
		}
		metadata, found, err := readHyperVCacheMetadata(paths.metadata)
		if err != nil {
			return detached, target, err
		}
		if !found {
			return detached, target, core.Exit(2, "Hyper-V cache volume %q metadata is missing", volume.Key)
		}
		expectedTarget, expectedFilesystem := hypervCacheFormat(cfg.TargetOS)
		if err := validateHyperVCacheMetadata(metadata, hypervCacheMetadata{
			Version:    hypervCacheMetadataVersion,
			Key:        volume.Key,
			Target:     expectedTarget,
			Filesystem: expectedFilesystem,
		}); err != nil {
			return detached, target, err
		}
		detached[len(detached)-1].volume.SizeGB = metadata.SizeGB
		detached[len(detached)-1].metadata = metadata
	}
	return detached, target, nil
}

func (b *backend) detachLockedCacheVolumes(ctx context.Context, vmName string, cfg core.Config, target core.SSHTarget, detached []hypervDetachedCacheVolume, unmountGuest bool) ([]hypervDetachedCacheVolume, error) {
	for i := range detached {
		if unmountGuest {
			detached[i].restore = true
			if cfg.TargetOS == targetWindows {
				if err := b.unmountWindowsCacheVolume(ctx, vmName, cfg.HyperV.User, detached[i].volume, detached[i].metadata); err != nil {
					return detached, err
				}
			} else if cfg.TargetOS == targetLinux {
				if err := b.unmountLinuxCacheVolume(ctx, target, detached[i].volume, detached[i].metadata); err != nil {
					return detached, err
				}
			}
		}
		if err := b.detachHyperVCacheDisk(ctx, vmName, detached[i].paths.vhd); err != nil {
			return detached, err
		}
	}
	return detached, nil
}

func (b *backend) detachLeaseCacheVolumes(ctx context.Context, vmName, leaseID string, cfg core.Config, target core.SSHTarget, unmountGuest bool) ([]hypervDetachedCacheVolume, error) {
	locked, resolvedTarget, err := b.lockLeaseCacheVolumes(ctx, leaseID, cfg, target, true)
	if err != nil {
		return locked, err
	}
	return b.detachLockedCacheVolumes(ctx, vmName, cfg, resolvedTarget, locked, unmountGuest)
}

func (b *backend) restoreDetachedCacheVolumes(vmName string, cfg core.Config, target core.SSHTarget, volumes []hypervDetachedCacheVolume) error {
	if len(volumes) == 0 {
		return nil
	}
	restoreCtx, cancel := context.WithTimeout(context.Background(), hypervCheckpointCleanupTimeout)
	defer cancel()
	var errs []error
	for _, volume := range volumes {
		if volume.restore {
			if err := b.attachCacheVolumeLocked(restoreCtx, vmName, cfg, target, volume.volume, volume.paths); err != nil {
				errs = append(errs, err)
			}
		}
		if err := volume.lock.release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func releaseDetachedCacheVolumeLocks(volumes []hypervDetachedCacheVolume) error {
	var errs []error
	for _, volume := range volumes {
		if err := volume.lock.release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *backend) detachInheritedForkDisks(ctx context.Context, vmName string) error {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $drives=@(Get-VMHardDiskDrive -VMName '%s' -ErrorAction Stop | `+
			`Where-Object { $_.Path -and [IO.Path]::GetFileName($_.Path) -match '^cache-[0-9a-fA-F]{32}(?:_[0-9a-fA-F-]{36})?\.(?:vhdx|avhdx)$' }); `+
			`$drives | Remove-VMHardDiskDrive -ErrorAction Stop`,
		escapePSString(vmName),
	)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		return shared.LocalCommandError("exclude inherited cache disks from Hyper-V fork", result, runErr)
	}
	return nil
}
