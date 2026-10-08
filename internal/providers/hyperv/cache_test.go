package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestHyperVCacheVolumeNameIsDigestDerivedAndSafe(t *testing.T) {
	first := hypervCacheVolumeName("example-org/my-app:windows:nuget")
	second := hypervCacheVolumeName("example-org/my-app:linux:nuget")
	if first == second {
		t.Fatalf("distinct keys produced the same name %q", first)
	}
	if !strings.HasPrefix(first, "cache-") || len(first) != len("cache-")+32 {
		t.Fatalf("unexpected cache name %q", first)
	}
	for _, r := range strings.TrimPrefix(first, "cache-") {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("cache name contains unsafe rune %q: %s", r, first)
		}
	}
}

func TestValidateHyperVCacheMountPathProtectsRootsAndWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		targetOS string
		workRoot string
		path     string
		wantErr  string
	}{
		{name: "windows cache", targetOS: targetWindows, workRoot: `C:\crabbox`, path: `D:\crabbox-cache\nuget`},
		{name: "linux cache", targetOS: targetLinux, workRoot: "/work/crabbox", path: "/var/cache/crabbox/pnpm"},
		{name: "windows drive root", targetOS: targetWindows, workRoot: `C:\crabbox`, path: `D:\`, wantErr: "drive root"},
		{name: "windows dot drive root", targetOS: targetWindows, workRoot: `C:\crabbox`, path: `D:\.`, wantErr: "traversal"},
		{name: "windows workspace", targetOS: targetWindows, workRoot: `C:\crabbox`, path: `C:\crabbox\cache`, wantErr: "outside the synced work root"},
		{name: "windows workspace parent", targetOS: targetWindows, workRoot: `C:\work\crabbox`, path: `C:\work`, wantErr: "outside the synced work root"},
		{name: "linux root", targetOS: targetLinux, workRoot: "/work/crabbox", path: "/", wantErr: "filesystem root"},
		{name: "linux workspace", targetOS: targetLinux, workRoot: "/work/crabbox", path: "/work/crabbox/cache", wantErr: "outside the synced work root"},
		{name: "linux workspace parent", targetOS: targetLinux, workRoot: "/work/crabbox", path: "/work", wantErr: "outside the synced work root"},
		{name: "linux traversal", targetOS: targetLinux, workRoot: "/work/crabbox", path: "/var/cache/../lib/cache", wantErr: "traversal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = tt.targetOS
			cfg.HyperV.WorkRoot = tt.workRoot
			err := validateHyperVCacheMountPath(cfg, core.CacheVolumeConfig{Key: "cache-key", Path: tt.path})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLockedCheckpointCacheUsesRecordedMetadataSize(t *testing.T) {
	setCheckpointTestState(t)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cacheRoot = t.TempDir()
	leaseID := "cbx_cache_size_restore"
	vmName := "crabbox-cache-size-restore"
	cacheVolume := core.CacheVolumeConfig{Key: "small-checkpoint-cache", Path: `C:\crabbox-cache\nuget`, SizeGB: 32}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        cacheVolume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     32,
	})
	server := core.Server{Provider: providerName, CloudID: vmName, Labels: map[string]string{"instance": vmName, "target": targetWindows}}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "cache-size", providerName, instanceScope(vmName), "", t.TempDir(), b.cfg.IdleTimeout, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimCacheVolumes(leaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}
	locked, _, err := b.lockLeaseCacheVolumes(context.Background(), leaseID, b.cfg, core.SSHTarget{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = releaseDetachedCacheVolumeLocks(locked) }()
	if len(locked) != 1 || locked[0].volume.SizeGB != 32 {
		t.Fatalf("locked cache volumes=%#v", locked)
	}
}

func TestValidateHyperVCacheVolumeSetRejectsOverlappingMounts(t *testing.T) {
	for _, tt := range []struct {
		name     string
		targetOS string
		workRoot string
		parent   string
		child    string
	}{
		{name: "windows", targetOS: targetWindows, workRoot: `C:\crabbox`, parent: `C:\cache`, child: `C:\cache\nuget`},
		{name: "linux", targetOS: targetLinux, workRoot: "/work/crabbox", parent: "/var/cache/crabbox", child: "/var/cache/crabbox/pnpm"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = tt.targetOS
			cfg.HyperV.WorkRoot = tt.workRoot
			cfg.Cache.Volumes = []core.CacheVolumeConfig{
				{Key: "parent", Path: tt.parent},
				{Key: "child", Path: tt.child},
			}
			if err := validateHyperVCacheVolumeSet(cfg); err == nil || !strings.Contains(err.Error(), "overlaps") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHyperVCacheVolumeValidationUsesSelectedTarget(t *testing.T) {
	cfg := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}}).cfg
	cfg.Cache.Volumes = []core.CacheVolumeConfig{{
		Key:      "windows-cache",
		Path:     `D:\crabbox-cache\nuget`,
		Required: true,
	}}
	if err := core.ValidateCacheVolumesForProvider(cfg); err != nil {
		t.Fatalf("Windows cache volume rejected: %v", err)
	}
	cfg.TargetOS = targetLinux
	cfg.HyperV.WorkRoot = "/work/crabbox"
	if err := core.ValidateCacheVolumesForProvider(cfg); err == nil || !strings.Contains(err.Error(), "POSIX absolute path") {
		t.Fatalf("Linux target accepted Windows cache path: %v", err)
	}
}

func TestEnsureHyperVCacheVolumeCreatesThenReusesMetadata(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	volume := core.CacheVolumeConfig{Key: "repo-windows-nuget-lock", Path: `D:\cache\nuget`, SizeGB: 32}
	paths := b.cacheVolumePaths(volume.Key)
	creates := 0
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		if !strings.Contains(script, "New-VHD") {
			return core.LocalCommandResult{}, nil, false
		}
		creates++
		if err := os.WriteFile(paths.vhd, []byte("vhdx"), 0o600); err != nil {
			t.Fatal(err)
		}
		return jsonResult(t, hypervCacheCreateOutput{DiskID: "11111111-2222-3333-4444-555555555555"}), nil, true
	}

	first, err := b.ensureHyperVCacheVolume(context.Background(), b.cfg, volume, paths)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.ensureHyperVCacheVolume(context.Background(), b.cfg, volume, paths)
	if err != nil {
		t.Fatal(err)
	}
	if creates != 1 || first != second || first.Filesystem != "ntfs" || first.SizeGB != 32 {
		t.Fatalf("creates=%d first=%#v second=%#v", creates, first, second)
	}
	data, err := os.ReadFile(paths.metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"target": "windows/normal"`)) {
		t.Fatalf("metadata=%s", data)
	}
}

func TestEnsureHyperVCacheVolumeRejectsIncompatibleKeyReuse(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cacheRoot = t.TempDir()
	volume := core.CacheVolumeConfig{Key: "shared-cache", Path: `D:\cache\nuget`}
	writeTestHyperVCacheVolume(t, b, volume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        volume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     80,
	})
	cfg := b.cfg
	cfg.TargetOS = targetLinux
	cfg.HyperV.WorkRoot = "/work/crabbox"
	_, err := b.ensureHyperVCacheVolume(context.Background(), cfg, volume, b.cacheVolumePaths(volume.Key))
	if err == nil || !strings.Contains(err.Error(), "already owned by target=windows/normal filesystem=ntfs") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnsureHyperVCacheVolumeRejectsSmallerExistingDisk(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cacheRoot = t.TempDir()
	volume := core.CacheVolumeConfig{Key: "small-cache", Path: `C:\crabbox-cache\nuget`, SizeGB: 40}
	writeTestHyperVCacheVolume(t, b, volume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        volume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     20,
	})
	if _, err := b.ensureHyperVCacheVolume(context.Background(), b.cfg, volume, b.cacheVolumePaths(volume.Key)); err == nil || !strings.Contains(err.Error(), "requires at least 40GB") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnsureHyperVCacheVolumeRecreatesUnattachedOrphan(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	volume := core.CacheVolumeConfig{Key: "orphan-cache", Path: `C:\crabbox-cache\nuget`}
	paths := b.cacheVolumePaths(volume.Key)
	if err := os.WriteFile(paths.vhd, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case strings.Contains(script, "New-VHD"):
			if _, err := os.Stat(paths.vhd); !os.IsNotExist(err) {
				t.Fatalf("orphan VHDX was not removed before recreation: %v", err)
			}
			if err := os.WriteFile(paths.vhd, []byte("recreated"), 0o600); err != nil {
				t.Fatal(err)
			}
			return jsonResult(t, hypervCacheCreateOutput{DiskID: "11111111-2222-3333-4444-555555555555"}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	if _, err := b.ensureHyperVCacheVolume(context.Background(), b.cfg, volume, paths); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.vhd)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "recreated" {
		t.Fatalf("VHDX contents=%q", data)
	}
}

func TestEnsureHyperVCacheVolumePublishesInterruptedMetadataCommit(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cacheRoot = t.TempDir()
	volume := core.CacheVolumeConfig{Key: "metadata-temp-cache", Path: `C:\crabbox-cache\nuget`}
	paths := b.cacheVolumePaths(volume.Key)
	if err := os.WriteFile(paths.vhd, []byte("vhdx"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        volume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     80,
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.metadata+".tmp", data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := b.ensureHyperVCacheVolume(context.Background(), b.cfg, volume, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got != metadata {
		t.Fatalf("metadata=%#v want %#v", got, metadata)
	}
	if _, err := os.Stat(paths.metadata); err != nil {
		t.Fatalf("metadata was not published: %v", err)
	}
	if _, err := os.Stat(paths.metadata + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("metadata temp still exists: %v", err)
	}
}

func TestEnsureHyperVCacheVolumeRemovesOrphanAfterCreateFailure(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	volume := core.CacheVolumeConfig{Key: "failed-cache", Path: `D:\cache\nuget`}
	paths := b.cacheVolumePaths(volume.Key)
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if !strings.Contains(commandScript(req), "New-VHD") {
			return core.LocalCommandResult{}, nil, false
		}
		if err := os.WriteFile(paths.vhd, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		return core.LocalCommandResult{Stderr: "failed after create"}, errors.New("powershell failed"), true
	}
	if _, err := b.ensureHyperVCacheVolume(context.Background(), b.cfg, volume, paths); err == nil {
		t.Fatal("cache creation unexpectedly succeeded")
	}
	if _, err := os.Stat(paths.vhd); !os.IsNotExist(err) {
		t.Fatalf("orphaned VHDX remains after create failure: %v", err)
	}
}

func TestAttachConfiguredCacheVolumesHandlesBusyRequiredAndOptional(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "required"}[required], func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			var stderr bytes.Buffer
			b := testBackend(runner)
			b.rt.Stderr = &stderr
			b.cacheRoot = t.TempDir()
			volume := core.CacheVolumeConfig{Key: "busy-cache", Path: `D:\cache\nuget`, Required: required}
			writeTestHyperVCacheVolume(t, b, volume, hypervCacheMetadata{
				Version:    hypervCacheMetadataVersion,
				Key:        volume.Key,
				Target:     "windows/normal",
				Filesystem: "ntfs",
				DiskID:     "11111111-2222-3333-4444-555555555555",
				SizeGB:     80,
			})
			runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if strings.Contains(commandScript(req), "ConvertTo-Json -InputObject $items") {
					return jsonResult(t, []hypervCacheAttachment{{
						VMName:             "crabbox-other",
						Path:               b.cacheVolumePaths(volume.Key).vhd,
						ControllerType:     "SCSI",
						ControllerNumber:   0,
						ControllerLocation: 1,
					}}), nil, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			cfg := b.cfg
			cfg.Cache.Volumes = []core.CacheVolumeConfig{volume}
			attached, err := b.attachConfiguredCacheVolumes(context.Background(), "crabbox-current", cfg, core.LeaseTarget{})
			if required {
				if err == nil || !strings.Contains(err.Error(), "already attached to VM crabbox-other") {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil || len(attached) != 0 || !strings.Contains(stderr.String(), "skip optional") {
				t.Fatalf("attached=%#v err=%v stderr=%q", attached, err, stderr.String())
			}
		})
	}
}

func TestAttachHyperVCacheDiskReconcilesAmbiguousFailure(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	queryCount := 0
	vhdPath := filepath.Join(t.TempDir(), "cache.vhdx")
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			queryCount++
			if queryCount == 1 {
				return core.LocalCommandResult{Stdout: "[]"}, nil, true
			}
			return jsonResult(t, []hypervCacheAttachment{{
				VMName:             "crabbox-current",
				Path:               vhdPath,
				ControllerType:     "SCSI",
				ControllerNumber:   0,
				ControllerLocation: 2,
			}}), nil, true
		case strings.Contains(script, "Add-VMHardDiskDrive"):
			return core.LocalCommandResult{Stderr: "timed out"}, context.DeadlineExceeded, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	attachment, added, err := b.attachHyperVCacheDisk(context.Background(), "crabbox-current", vhdPath)
	if err == nil || !added || attachment.ControllerLocation != 2 {
		t.Fatalf("attachment=%#v added=%v err=%v", attachment, added, err)
	}
}

func TestWindowsCacheMountUsesDiskIdentityNTFSAndDirectoryAccessPath(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.mountWindowsCacheVolume(context.Background(), "crabbox-test", "crabbox", core.CacheVolumeConfig{
		Key:  "nuget-cache",
		Path: `D:\crabbox-cache\nuget`,
	}, hypervCacheMetadata{DiskID: "11111111-2222-3333-4444-555555555555"}); err != nil {
		t.Fatal(err)
	}
	script := findScript(runner.calls, "Format-Volume -FileSystem NTFS")
	for _, want := range []string{
		"Get-Disk",
		"11111111-2222-3333-4444-555555555555",
		"Crabbox cache mount drive does not exist",
		"$newPartition=$false",
		"existing Crabbox cache filesystem could not be identified",
		"New-Partition -DiskNumber",
		"Add-PartitionAccessPath",
		`D:\crabbox-cache\nuget`,
		"icacls.exe",
		hypervCacheDetachedGuard,
		"PathType Leaf",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Windows cache mount script missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "DriveLetter") {
		t.Fatalf("Windows cache mount assigned a drive letter: %s", script)
	}
}

func TestWindowsCacheUnmountInstallsDetachedGuard(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.unmountWindowsCacheVolume(context.Background(), "crabbox-test", "crabbox", core.CacheVolumeConfig{
		Key:  "nuget-cache",
		Path: `D:\crabbox-cache\nuget`,
	}, hypervCacheMetadata{DiskID: "11111111-2222-3333-4444-555555555555"}); err != nil {
		t.Fatal(err)
	}
	script := findScript(runner.calls, "Remove-PartitionAccessPath")
	for _, want := range []string{
		hypervCacheDetachedGuard,
		"Crabbox cache mount path changed after unmount",
		"Set-Content -NoNewline",
		"Set-Disk -Number $disk.Number -IsOffline $true",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Windows cache unmount script missing %q: %s", want, script)
		}
	}
}

func TestLinuxCacheMountFormatsExt4AndPersistsUUIDMount(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	var remote string
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, command string) (string, error) {
		remote = command
		return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil
	}
	uuid, err := b.mountLinuxCacheVolume(context.Background(), core.SSHTarget{Host: "192.0.2.20"}, "crabbox", core.CacheVolumeConfig{
		Key:  "pnpm-cache",
		Path: "/var/cache/crabbox/pnpm",
	}, hypervCacheMetadata{DiskID: "11111111-2222-3333-4444-555555555555"})
	if err != nil {
		t.Fatal(err)
	}
	if uuid != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("uuid=%q", uuid)
	}
	for _, want := range []string{
		"lsblk -dn -o SERIAL",
		"11111111222233334444555555555555",
		`if [ -n "$expected_uuid" ]; then`,
		"existing Crabbox cache filesystem could not be identified as ext4",
		"mkfs.ext4",
		`sudo mount -U "$uuid" "$mount_path"`,
		"/etc/fstab",
		hypervCacheDetachedGuard,
		`ENVIRON["FSTAB_MOUNT_PATH"]`,
		`sudo install -m 0644 "$fstab_tmp" /etc/fstab`,
		"sudo chown 'crabbox:crabbox'",
	} {
		if !strings.Contains(remote, want) {
			t.Fatalf("Linux cache mount command missing %q: %s", want, remote)
		}
	}
}

func TestLinuxCacheMountShellQuotesFstabPath(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	var remote string
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, command string) (string, error) {
		remote = command
		return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil
	}
	_, err := b.mountLinuxCacheVolume(context.Background(), core.SSHTarget{}, "crabbox", core.CacheVolumeConfig{
		Key:  "special-cache",
		Path: `/var/cache/$HOME "pnpm"`,
	}, hypervCacheMetadata{DiskID: "11111111-2222-3333-4444-555555555555"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote, `fstab_path='/var/cache/$HOME\040"pnpm"'`) {
		t.Fatalf("fstab path was not shell-quoted: %s", remote)
	}
	if strings.Contains(remote, `entry="UUID=$uuid /var/cache/$HOME`) {
		t.Fatalf("fstab entry interpolates the mount path directly: %s", remote)
	}
}

func TestLinuxCacheUnmountVerifiesFilesystemIdentity(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	var remote string
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, command string) (string, error) {
		remote = command
		return "", nil
	}
	if err := b.unmountLinuxCacheVolume(context.Background(), core.SSHTarget{Host: "192.0.2.20"}, core.CacheVolumeConfig{
		Key:  "pnpm-cache",
		Path: "/var/cache/crabbox/pnpm",
	}, hypervCacheMetadata{FilesystemID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"findmnt -n -o UUID",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		`sudo umount "$mount_path"`,
		hypervCacheDetachedGuard,
		`ENVIRON["FSTAB_MOUNT_PATH"]`,
		`sudo chmod 000 "$mount_path"`,
	} {
		if !strings.Contains(remote, want) {
			t.Fatalf("Linux cache unmount command missing %q: %s", want, remote)
		}
	}
}

func TestRemoveVMStoragePreservesProviderCacheVHDX(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cacheRoot = t.TempDir()
	cachePath := b.cacheVolumePaths("preserved-cache").vhd
	if err := os.WriteFile(cachePath, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeVMStorage("crabbox-cache-preserve-test", []string{cachePath}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("provider cache VHDX was removed: %v", err)
	}
}

func TestReleaseLeaseStopsBeforeDetachingCacheAndToleratesCorruptMetadata(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	leaseID := "cbx_cache_release"
	vmName := "crabbox-cache-release"
	cacheVolume := core.CacheVolumeConfig{
		Key:      "release-cache",
		Path:     `D:\crabbox-cache\nuget`,
		Required: true,
	}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        cacheVolume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     80,
	})
	if err := os.WriteFile(b.cacheVolumePaths(cacheVolume.Key).metadata, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  vmName,
		Labels: map[string]string{
			"instance":  vmName,
			"target":    targetWindows,
			"ssh_user":  b.cfg.HyperV.User,
			"work_root": b.cfg.HyperV.WorkRoot,
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "cache-release", providerName, instanceScope(vmName), "", t.TempDir(), b.cfg.IdleTimeout, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimCacheVolumes(leaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		if strings.Contains(script, "Select-Object Name, State") {
			return jsonResult(t, hypervVM{Name: vmName, State: 2}), nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server},
	}); err != nil {
		t.Fatal(err)
	}
	stopIndex := findCallIndex(runner.calls, "stop Hyper-V VM before cache detach")
	if stopIndex < 0 {
		stopIndex = findCallIndex(runner.calls, "Stop-VM -VM $vm")
	}
	detachIndex := findCallIndex(runner.calls, "Remove-VMHardDiskDrive")
	removeIndex := findCallIndex(runner.calls, "Remove-VM -Name")
	if stopIndex < 0 || detachIndex <= stopIndex || removeIndex <= detachIndex {
		t.Fatalf("release order stop=%d detach=%d remove=%d", stopIndex, detachIndex, removeIndex)
	}
	if findCallIndex(runner.calls, "Invoke-Command") >= 0 {
		t.Fatal("release required a reachable guest")
	}
	if _, err := os.Stat(b.cacheVolumePaths(cacheVolume.Key).vhd); err != nil {
		t.Fatalf("release removed cache VHDX: %v", err)
	}
}

func TestReleaseLeaseRemovesStoppedVMWhenCacheDetachFails(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	leaseID := "cbx_cache_detach_failure"
	vmName := "crabbox-cache-detach-failure"
	cacheVolume := core.CacheVolumeConfig{
		Key:      "detach-failure-cache",
		Path:     `D:\crabbox-cache\nuget`,
		Required: true,
	}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        cacheVolume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     80,
	})
	server := core.Server{
		Provider: providerName,
		CloudID:  vmName,
		Labels: map[string]string{
			"instance":  vmName,
			"target":    targetWindows,
			"ssh_user":  b.cfg.HyperV.User,
			"work_root": b.cfg.HyperV.WorkRoot,
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "cache-detach-failure", providerName, instanceScope(vmName), "", t.TempDir(), b.cfg.IdleTimeout, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimCacheVolumes(leaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}
	detachFailed := false
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Remove-VMHardDiskDrive") && !detachFailed:
			detachFailed = true
			return core.LocalCommandResult{Stderr: "detach failed"}, errors.New("detach failed"), true
		case strings.Contains(script, "Select-Object Name, State"):
			return jsonResult(t, hypervVM{Name: vmName, State: 3}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if findCallIndex(runner.calls, "Remove-VM -Name") < 0 {
		t.Fatal("VM was not removed after cache detach failure")
	}
	if findCallIndex(runner.calls, "Invoke-Command") >= 0 {
		t.Fatal("release attempted guest access after cache detach failure")
	}
}

func TestReleaseLeaseDoesNotStopVMWhenCacheLockIsBusy(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	leaseID := "cbx_cache_lock_busy"
	vmName := "crabbox-cache-lock-busy"
	cacheVolume := core.CacheVolumeConfig{
		Key:      "lock-busy-cache",
		Path:     `D:\crabbox-cache\nuget`,
		Required: true,
	}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        cacheVolume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     80,
	})
	server := core.Server{
		Provider: providerName,
		CloudID:  vmName,
		Labels: map[string]string{
			"instance":  vmName,
			"target":    targetWindows,
			"ssh_user":  b.cfg.HyperV.User,
			"work_root": b.cfg.HyperV.WorkRoot,
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "cache-lock-busy", providerName, instanceScope(vmName), "", t.TempDir(), b.cfg.IdleTimeout, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimCacheVolumes(leaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}
	hostLock := flock.New(b.cacheVolumePaths(cacheVolume.Key).lock)
	locked, err := hostLock.TryLock()
	if err != nil || !locked {
		t.Fatalf("acquire competing cache lock locked=%v err=%v", locked, err)
	}
	t.Cleanup(func() { _ = hostLock.Unlock() })
	oldWait := hypervCacheLockWait
	hypervCacheLockWait = 50 * time.Millisecond
	t.Cleanup(func() { hypervCacheLockWait = oldWait })

	remoteCleanupCalled := false
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server},
		GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) {
			remoteCleanupCalled = true
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cache volume is busy") {
		t.Fatalf("err=%v", err)
	}
	if findCallIndex(runner.calls, "Stop-VM") >= 0 || findCallIndex(runner.calls, "Remove-VM") >= 0 {
		t.Fatal("release mutated the VM before acquiring cache locks")
	}
	if remoteCleanupCalled {
		t.Fatal("release ran remote cleanup before acquiring cache locks")
	}
}

func writeTestHyperVCacheVolume(t *testing.T, b *backend, volume core.CacheVolumeConfig, metadata hypervCacheMetadata) {
	t.Helper()
	paths := b.cacheVolumePaths(volume.Key)
	if err := os.MkdirAll(filepath.Dir(paths.vhd), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.vhd, []byte("vhdx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHyperVCacheMetadata(paths.metadata, metadata); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxCacheMountPropagatesSSHFailure(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.runSSHOutput = func(context.Context, core.SSHTarget, string) (string, error) {
		return "", errors.New("ssh failed")
	}
	_, err := b.mountLinuxCacheVolume(context.Background(), core.SSHTarget{}, "crabbox", core.CacheVolumeConfig{
		Key:  "pnpm-cache",
		Path: "/var/cache/crabbox/pnpm",
	}, hypervCacheMetadata{DiskID: "11111111-2222-3333-4444-555555555555"})
	if err == nil || !strings.Contains(err.Error(), "ssh failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestHyperVCacheMetadataJSONRoundTrip(t *testing.T) {
	metadata := hypervCacheMetadata{
		Version:      hypervCacheMetadataVersion,
		Key:          "cache-key",
		Target:       targetLinux,
		Filesystem:   "ext4",
		DiskID:       "disk-id",
		FilesystemID: "filesystem-id",
		SizeGB:       20,
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var decoded hypervCacheMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != metadata {
		t.Fatalf("decoded=%#v want %#v", decoded, metadata)
	}
}
