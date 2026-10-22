package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	testCheckpointLeaseID  = "cbx_checkpoint1234"
	testCheckpointVMName   = "crabbox-checkpoint-1234"
	testCheckpointVMID     = "11111111-2222-3333-4444-555555555555"
	testCheckpointSnapshot = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func TestNativeCheckpointCapabilityPrefersProductionCheckpointForAuto(t *testing.T) {
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config:   core.Config{Provider: providerName, TargetOS: core.TargetWindows},
		Target:   core.SSHTarget{TargetOS: core.TargetWindows},
		Strategy: "auto",
	})
	if !ok || capability.Kind != hypervCheckpointKind || !capability.Direct || !capability.PreferredForAuto {
		t.Fatalf("capability=%#v ok=%v", capability, ok)
	}
}

func TestNativeCheckpointCapabilitySupportsLinuxProductionCheckpoints(t *testing.T) {
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config:   core.Config{Provider: providerName, TargetOS: core.TargetLinux},
		Target:   core.SSHTarget{TargetOS: core.TargetLinux},
		Strategy: "auto",
	})
	if !ok || capability.Kind != hypervCheckpointKind || !capability.Direct || !capability.PreferredForAuto {
		t.Fatalf("Linux checkpoint capability=%#v ok=%v", capability, ok)
	}
}

func TestApplyNativeCheckpointForkConfigUsesRecordedLinuxTarget(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	configureLinuxCheckpointMetadata(metadata)
	cfg := core.BaseConfig()
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal

	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: checkpointForkRecord(paths, metadata),
	}); err != nil {
		t.Fatalf("ApplyNativeCheckpointForkConfig: %v", err)
	}
	if cfg.TargetOS != core.TargetLinux || cfg.WindowsMode != "" || cfg.WorkRoot != "/work/crabbox" {
		t.Fatalf("Linux fork config=%#v", cfg)
	}
}

func TestCreateNativeCheckpointExportsProductionMetadata(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Get-VM checkpoint query"):
			return core.LocalCommandResult{}, nil, false
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			configPath := filepath.Join(artifactDir, "hyperv", "exported", "Virtual Machines", "checkpoint.vmcx")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte("vmcx"), 0o600); err != nil {
				t.Fatal(err)
			}
			return jsonResult(t, checkpointCreateOutput{
				SourceVMID:     testCheckpointVMID,
				SnapshotID:     testCheckpointSnapshot,
				SnapshotName:   checkpointNameFromScript(script),
				ExportedConfig: configPath,
			}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	var journal []core.NativeCheckpointCreateResult
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		LeaseID:     testCheckpointLeaseID,
		Name:        "release-ready",
		RepoName:    "my-app",
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
		Persist: func(result core.NativeCheckpointCreateResult) error {
			if len(journal) == 0 && findScript(runner.calls, "Export-VMSnapshot") != "" {
				t.Fatal("checkpoint mutation preceded cleanup journal")
			}
			journal = append(journal, result)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CreateNativeCheckpoint: %v", err)
	}
	if result.Image.Kind != hypervCheckpointKind || result.Image.ID != testCheckpointSnapshot || result.Image.ResourceID == "" {
		t.Fatalf("image=%#v", result.Image)
	}
	if len(journal) != 2 || journal[0].Metadata[checkpointMetadataSourceVMID] != testCheckpointVMID ||
		journal[0].Metadata[checkpointMetadataSnapshotName] == "" || journal[0].Image.ID != journal[0].Metadata[checkpointMetadataSnapshotName] || journal[0].ArtifactDir != artifactDir ||
		journal[1].Image.ID != testCheckpointSnapshot || result.ArtifactDir != artifactDir {
		t.Fatalf("cleanup and completion journal=%#v", journal)
	}
	if result.Metadata[checkpointMetadataCheckpointType] != "ProductionOnly" ||
		result.Metadata[checkpointMetadataSourceVMID] != testCheckpointVMID ||
		result.Metadata[checkpointMetadataArtifactDir] != artifactDir {
		t.Fatalf("metadata=%#v", result.Metadata)
	}
	if _, err := os.Stat(result.Metadata[checkpointMetadataExportConfig]); err != nil {
		t.Fatalf("exported config: %v", err)
	}
	createScript := findScript(runner.calls, "Export-VMSnapshot")
	for _, expected := range []string{
		"-CheckpointType ProductionOnly",
		"$_.Directory.Name -eq 'Virtual Machines'",
		"$configs.Count -ne 1",
	} {
		if !strings.Contains(createScript, expected) {
			t.Fatalf("create script missing %q: %s", expected, createScript)
		}
	}
}

func TestCreateNativeCheckpointJournalFailurePreventsMutation(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		if strings.Contains(script, "Select-Object Name,@{Name='ID'") {
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		}
		t.Fatalf("journal failure dispatched a mutating command: %s", script)
		return core.LocalCommandResult{}, nil, true
	}}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	failure := errors.New("journal unavailable")
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: b.cfg, Runtime: b.rt, ArtifactDir: t.TempDir(),
		Server: checkpointSourceServer(b), LeaseID: testCheckpointLeaseID,
		Persist: func(core.NativeCheckpointCreateResult) error { return failure },
	})
	var notSubmitted core.NativeCheckpointNotSubmittedError
	if !errors.Is(err, failure) || !errors.As(err, &notSubmitted) {
		t.Fatalf("journal failure=%v", err)
	}
}

func TestCreateNativeCheckpointDetachesAndRestoresCacheVolumesOnSuccessAndFailure(t *testing.T) {
	for _, failExport := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failExport], func(t *testing.T) {
			setCheckpointTestState(t)
			artifactDir := t.TempDir()
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			b := testBackend(runner)
			b.cfg.TargetOS = targetLinux
			b.cacheRoot = t.TempDir()
			persistCheckpointSource(t, b)
			cacheVolume := core.CacheVolumeConfig{
				Key:      "checkpoint-nuget-cache",
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
			cacheLockPath := b.cacheVolumePaths(cacheVolume.Key).lock
			lockHeldDuringExport := false
			if err := core.UpdateLeaseClaimCacheVolumes(testCheckpointLeaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
				t.Fatal(err)
			}
			runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				script := commandScript(req)
				switch {
				case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
					return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
				case strings.Contains(script, "Export-VMSnapshot"):
					probe := flock.New(cacheLockPath)
					locked, lockErr := probe.TryLock()
					if lockErr == nil && !locked {
						lockHeldDuringExport = true
					}
					if locked {
						_ = probe.Unlock()
					}
					if failExport {
						return core.LocalCommandResult{Stderr: "export failed"}, errors.New("export failed"), true
					}
					configPath := filepath.Join(artifactDir, "hyperv", "exported", "Virtual Machines", "checkpoint.vmcx")
					if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(configPath, []byte("vmcx"), 0o600); err != nil {
						t.Fatal(err)
					}
					return jsonResult(t, checkpointCreateOutput{
						SourceVMID:     testCheckpointVMID,
						SnapshotID:     testCheckpointSnapshot,
						SnapshotName:   checkpointNameFromScript(script),
						ExportedConfig: configPath,
					}), nil, true
				case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
					return core.LocalCommandResult{Stdout: "[]"}, nil, true
				case strings.Contains(script, "Add-VMHardDiskDrive"):
					return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 1}), nil, true
				case strings.Contains(script, "Invoke-Command"):
					return core.LocalCommandResult{}, nil, true
				default:
					return core.LocalCommandResult{}, nil, false
				}
			}
			oldOS := hypervHostOS
			hypervHostOS = "windows"
			t.Cleanup(func() { hypervHostOS = oldOS })

			_, err := b.createNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
				Config:      b.cfg,
				Runtime:     b.rt,
				Server:      checkpointSourceServer(b),
				Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
				LeaseID:     testCheckpointLeaseID,
				Name:        "cache-safe",
				RepoName:    "my-app",
				ArtifactDir: artifactDir,
				Strategy:    "disk-snapshot",
			})
			if failExport && err == nil {
				t.Fatal("checkpoint unexpectedly succeeded")
			}
			if !failExport && err != nil {
				t.Fatal(err)
			}
			unmountIndex := findCallIndex(runner.calls, "Remove-PartitionAccessPath")
			detachIndex := findCallIndex(runner.calls, "Remove-VMHardDiskDrive -ErrorAction Stop")
			checkpointIndex := findCallIndex(runner.calls, "Checkpoint-VM")
			reattachIndex := findCallIndex(runner.calls, "Add-VMHardDiskDrive")
			if unmountIndex < 0 || detachIndex <= unmountIndex || checkpointIndex <= detachIndex || reattachIndex <= checkpointIndex {
				t.Fatalf("cache checkpoint order unmount=%d detach=%d checkpoint=%d reattach=%d", unmountIndex, detachIndex, checkpointIndex, reattachIndex)
			}
			if script := commandScript(runner.calls[unmountIndex]); !strings.Contains(script, "Set-Disk -Number $disk.Number -IsOffline $true") {
				t.Fatalf("Windows cache was not offlined before detach: %s", script)
			}
			if !lockHeldDuringExport {
				t.Fatal("cache host lock was not held across checkpoint export")
			}
			probe := flock.New(cacheLockPath)
			locked, lockErr := probe.TryLock()
			if lockErr != nil || !locked {
				t.Fatalf("cache host lock was not released: locked=%v err=%v", locked, lockErr)
			}
			if err := probe.Unlock(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateNativeCheckpointPreservesPausedCacheBackedLease(t *testing.T) {
	for _, state := range []struct {
		name          string
		value         int
		resumeCommand string
		pauseCommand  string
	}{
		{name: "saved", value: hypervStateSaved, resumeCommand: "Start-VM", pauseCommand: "Save-VM"},
		{name: "paused", value: hypervStatePaused, resumeCommand: "Resume-VM", pauseCommand: "Suspend-VM"},
	} {
		for _, failExport := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/export-failure=%t", state.name, failExport), func(t *testing.T) {
				setCheckpointTestState(t)
				oldOS := hypervHostOS
				hypervHostOS = "windows"
				t.Cleanup(func() { hypervHostOS = oldOS })
				artifactDir := t.TempDir()
				runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
				var cacheVHDPath string
				runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					script := commandScript(req)
					switch {
					case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
						return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: state.value}), nil, true
					case strings.Contains(script, "Export-VMSnapshot"):
						if failExport {
							return core.LocalCommandResult{Stderr: "export failed"}, errors.New("export failed"), true
						}
						configPath := filepath.Join(artifactDir, "hyperv", "exported", "Virtual Machines", "checkpoint.vmcx")
						if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(configPath, []byte("vmcx"), 0o600); err != nil {
							t.Fatal(err)
						}
						return jsonResult(t, checkpointCreateOutput{
							SourceVMID:     testCheckpointVMID,
							SnapshotID:     testCheckpointSnapshot,
							SnapshotName:   checkpointNameFromScript(script),
							ExportedConfig: configPath,
						}), nil, true
					case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
						return core.LocalCommandResult{Stdout: "[]"}, nil, true
					case strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
						return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 1}), nil, true
					case strings.Contains(script, "Invoke-Command"):
						return core.LocalCommandResult{}, nil, true
					default:
						return core.LocalCommandResult{}, nil, false
					}
				}
				b := testBackend(runner)
				b.cacheRoot = t.TempDir()
				persistCheckpointSource(t, b)
				cacheVolume := core.CacheVolumeConfig{
					Key:      "paused-checkpoint-cache",
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
				cacheVHDPath = b.cacheVolumePaths(cacheVolume.Key).vhd
				if err := core.UpdateLeaseClaimCacheVolumes(testCheckpointLeaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
					t.Fatal(err)
				}

				_, err := b.createNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
					Config:      b.cfg,
					Runtime:     b.rt,
					Server:      checkpointSourceServer(b),
					Target:      core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
					LeaseID:     testCheckpointLeaseID,
					Name:        "paused-cache",
					ArtifactDir: artifactDir,
				})
				if failExport && err == nil {
					t.Fatal("checkpoint unexpectedly succeeded")
				}
				if !failExport && err != nil {
					t.Fatal(err)
				}
				resumeIndex := findCallIndex(runner.calls, state.resumeCommand)
				unmountIndex := findCallIndex(runner.calls, "Remove-PartitionAccessPath")
				detachIndex := findCallIndex(runner.calls, "Remove-VMHardDiskDrive -ErrorAction Stop")
				checkpointIndex := findCallIndex(runner.calls, "Checkpoint-VM")
				reattachIndex := findCallIndexAll(runner.calls, "Add-VMHardDiskDrive", cacheVHDPath)
				pauseIndex := findCallIndex(runner.calls, state.pauseCommand)
				if !(resumeIndex >= 0 &&
					resumeIndex < unmountIndex &&
					unmountIndex < detachIndex &&
					detachIndex < checkpointIndex &&
					checkpointIndex < reattachIndex &&
					reattachIndex < pauseIndex) {
					t.Fatalf(
						"paused cache checkpoint order resume=%d unmount=%d detach=%d checkpoint=%d reattach=%d pause=%d",
						resumeIndex,
						unmountIndex,
						detachIndex,
						checkpointIndex,
						reattachIndex,
						pauseIndex,
					)
				}
			})
		}
	}
}

func TestResumeCheckpointCacheSourceUsesResolvedTarget(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object -ExpandProperty IPAddresses") {
			return core.LocalCommandResult{Stdout: `["192.0.2.88"]`}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	sshReady := false
	b.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		sshReady = true
		if target.TargetOS != core.TargetLinux || target.Host != "192.0.2.88" {
			t.Fatalf("resolved target=%#v", target)
		}
		return nil
	}
	target, transitioned, err := b.resumeCheckpointCacheSource(
		context.Background(),
		testCheckpointVMName,
		hypervStateSaved,
		b.cfg,
		core.SSHTarget{
			TargetOS:       core.TargetLinux,
			Key:            "per-lease-key",
			KnownHostsFile: "known-hosts",
			HostKeyAlias:   "checkpoint-source",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !transitioned ||
		!sshReady ||
		target.TargetOS != core.TargetLinux ||
		target.Host != "192.0.2.88" ||
		target.Key != "per-lease-key" ||
		target.KnownHostsFile != "known-hosts" ||
		target.HostKeyAlias != "checkpoint-source" {
		t.Fatalf("target=%#v transitioned=%t sshReady=%t", target, transitioned, sshReady)
	}
	if findCallIndex(runner.calls, "Invoke-Command") >= 0 {
		t.Fatal("resolved Linux target used Windows guest readiness")
	}
}

func TestCreateNativeCheckpointLinuxVerifiesProductionSupport(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var events []string
	var cacheVHDPath string
	runner.onRun = func(req core.LocalCommandRequest) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Get-VMIntegrationService"):
			events = append(events, "support")
		case strings.Contains(script, "Remove-VMHardDiskDrive -ErrorAction Stop"):
			events = append(events, "detach")
		case strings.Contains(script, "Export-VMSnapshot"):
			events = append(events, "export")
		case cacheVHDPath != "" && strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			events = append(events, "reattach")
		}
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Get-VMIntegrationService"):
			return jsonResult(t, linuxProductionCheckpointSupport{
				Found:   true,
				Enabled: true,
				Primary: "OK",
			}), nil, true
		case strings.Contains(script, "Get-VMFirmware"):
			return jsonResult(t, firmwareOutput{
				Enabled:  true,
				Template: secureBootTemplateLinux,
			}), nil, true
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			configPath := filepath.Join(artifactDir, "hyperv", "exported", "Virtual Machines", "checkpoint.vmcx")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte("vmcx"), 0o600); err != nil {
				t.Fatal(err)
			}
			return jsonResult(t, checkpointCreateOutput{
				SourceVMID:     testCheckpointVMID,
				SnapshotID:     testCheckpointSnapshot,
				SnapshotName:   checkpointNameFromScript(script),
				ExportedConfig: configPath,
			}), nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case cacheVHDPath != "" && strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 1}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	configureLinuxCheckpointBackend(b)
	b.cacheRoot = t.TempDir()
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, command string) (string, error) {
		switch {
		case strings.Contains(command, "sudo umount"):
			events = append(events, "unmount")
			return "", nil
		case strings.Contains(command, "/etc/fstab"):
			events = append(events, "remount")
			return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil
		default:
			return "", nil
		}
	}
	persistCheckpointSource(t, b)
	cacheVolume := core.CacheVolumeConfig{
		Key:      "linux-checkpoint-cache",
		Path:     "/var/cache/crabbox/go-build",
		Required: true,
	}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:      hypervCacheMetadataVersion,
		Key:          cacheVolume.Key,
		Target:       targetLinux,
		Filesystem:   "ext4",
		DiskID:       "11111111-2222-3333-4444-555555555555",
		FilesystemID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		SizeGB:       80,
	})
	cacheVHDPath = b.cacheVolumePaths(cacheVolume.Key).vhd
	if err := core.UpdateLeaseClaimCacheVolumes(testCheckpointLeaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	result, err := b.createNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{Host: "192.0.2.44", TargetOS: core.TargetLinux},
		LeaseID:     testCheckpointLeaseID,
		Name:        "linux-ready",
		RepoName:    "my-app",
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	if err != nil {
		t.Fatalf("CreateNativeCheckpoint Linux: %v", err)
	}
	if result.Metadata[checkpointMetadataTarget] != core.TargetLinux ||
		result.Metadata[checkpointMetadataSpecialization] != linuxForkSpecializationVersion ||
		result.Metadata[checkpointMetadataSecureBoot] != "true" ||
		result.Metadata[checkpointMetadataSecureTemplate] != secureBootTemplateLinux {
		t.Fatalf("Linux metadata=%#v", result.Metadata)
	}
	supportIndex := findCallIndex(runner.calls, "Get-VMIntegrationService")
	exportIndex := findCallIndex(runner.calls, "Export-VMSnapshot")
	if supportIndex < 0 || exportIndex <= supportIndex {
		t.Fatalf("production support was not verified before export: support=%d export=%d", supportIndex, exportIndex)
	}
	supportScript := commandScript(runner.calls[supportIndex])
	for _, expected := range []string{"-Name 'VSS'", "PrimaryStatusDescription", "SecondaryOperationalStatus"} {
		if !strings.Contains(supportScript, expected) {
			t.Fatalf("support script missing %q: %s", expected, supportScript)
		}
	}
	wantEvents := []string{"support", "unmount", "detach", "export", "reattach", "remount"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("Linux checkpoint cache events=%v want %v", events, wantEvents)
	}
}

func TestCreateNativeCheckpointLinuxFailsClearlyWithoutProductionSupport(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Get-VMIntegrationService"):
			return jsonResult(t, linuxProductionCheckpointSupport{
				Found:     true,
				Enabled:   true,
				Primary:   "No Contact",
				Secondary: "ProtocolMismatch",
			}), nil, true
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	configureLinuxCheckpointBackend(b)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetLinux},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: t.TempDir(),
		Strategy:    "disk-snapshot",
	})
	if err == nil || !strings.Contains(err.Error(), "hv_vss_daemon") || !strings.Contains(err.Error(), "fallback is disabled") {
		t.Fatalf("Linux production support error=%v", err)
	}
	if findCallIndex(runner.calls, "Checkpoint-VM") >= 0 {
		t.Fatal("checkpoint creation ran after Linux production support failed")
	}
}

func TestCreateNativeCheckpointProductionFailureDoesNotFallback(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "production checkpoint failed"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	if err == nil {
		t.Fatal("CreateNativeCheckpoint succeeded after production checkpoint failure")
	}
	var unsubmitted core.NativeCheckpointNotSubmittedError
	if !errors.As(err, &unsubmitted) {
		t.Fatalf("verified-clean export failure must resolve as not submitted so release is not blocked: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(commandScript(call), "CheckpointType Standard") {
			t.Fatalf("production checkpoint failure fell back to a standard checkpoint: %s", commandScript(call))
		}
	}
	if _, statErr := os.Stat(filepath.Join(artifactDir, "hyperv")); !os.IsNotExist(statErr) {
		t.Fatalf("failed create left export directory behind: %v", statErr)
	}
}

func TestCreateNativeCheckpointFailureWithUnverifiedCleanupStaysUnresolved(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Export-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "export failed"}, errors.New("exit 1"), true
		case strings.Contains(script, "Remove-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "cleanup failed"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	var unsubmitted core.NativeCheckpointNotSubmittedError
	if err == nil || errors.As(err, &unsubmitted) {
		t.Fatalf("an export failure whose snapshot cleanup failed must stay unresolved: %v", err)
	}
}

func TestCreateNativeCheckpointCanceledRequestStillCleansLiveSnapshot(t *testing.T) {
	setCheckpointTestState(t)
	artifactDir := t.TempDir()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object Name,@{Name='ID'") {
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	runner.blockUntilCtx = func(req core.LocalCommandRequest) bool {
		return strings.Contains(commandScript(req), "Export-VMSnapshot")
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (Provider{}).CreateNativeCheckpoint(ctx, core.NativeCheckpointCreateRequest{
		Config:      b.cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		Server:      checkpointSourceServer(b),
		Target:      core.SSHTarget{TargetOS: core.TargetWindows},
		LeaseID:     testCheckpointLeaseID,
		ArtifactDir: artifactDir,
		Strategy:    "disk-snapshot",
	})
	if err == nil {
		t.Fatal("CreateNativeCheckpoint succeeded with a canceled request")
	}
	if findCallIndex(runner.calls, "Remove-VMSnapshot") < 0 {
		t.Fatal("canceled create did not attempt live checkpoint cleanup with a fresh context")
	}
}

func TestVerifyNativeCheckpointUsesExportAfterSourceRelease(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "source_missing") {
			return jsonResult(t, checkpointLiveStatus{State: "source_missing"}), nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	result, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: paths.artifactDir,
		Metadata:    metadata,
	})
	if err != nil {
		t.Fatalf("VerifyNativeCheckpoint: %v", err)
	}
	if result.ProviderState != "available_exported" || result.NextAction != "fork_or_delete" {
		t.Fatalf("verify result=%#v", result)
	}
}

func TestRestoreNativeCheckpointRefreshesClaimEndpoint(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var cacheVHDPath string
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 2}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 1}), nil, true
		case strings.Contains(script, "Invoke-Command"):
			return core.LocalCommandResult{}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.cacheRoot = t.TempDir()
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	persistCheckpointSource(t, b)
	cacheVolume := core.CacheVolumeConfig{Key: "restore-cache", Path: `C:\crabbox-cache\nuget`, SizeGB: 32, Required: true}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        cacheVolume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     32,
	})
	cacheVHDPath = b.cacheVolumePaths(cacheVolume.Key).vhd
	if err := core.UpdateLeaseClaimCacheVolumes(testCheckpointLeaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}

	lease, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err != nil {
		t.Fatalf("restoreNativeCheckpoint: %v", err)
	}
	if lease.SSH.Host != "192.0.2.45" {
		t.Fatalf("restored SSH host=%q", lease.SSH.Host)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve refreshed claim: ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.45" {
		t.Fatalf("claim SSH host=%q", claim.SSHHost)
	}
	detachIndex := findCallIndex(runner.calls, "Remove-VMHardDiskDrive")
	restoreIndex := findCallIndex(runner.calls, "Restore-VMSnapshot")
	reattachIndex := findCallIndexAll(runner.calls, "Add-VMHardDiskDrive", cacheVHDPath)
	if detachIndex < 0 || restoreIndex <= detachIndex || reattachIndex <= restoreIndex {
		t.Fatalf("restore cache order detach=%d restore=%d reattach=%d", detachIndex, restoreIndex, reattachIndex)
	}
	if len(claim.CacheVolumes) != 1 || claim.CacheVolumes[0] != cacheVolume.Key+":"+cacheVolume.Path {
		t.Fatalf("restored claim cache volumes=%#v", claim.CacheVolumes)
	}
}

func TestRestoreNativeCheckpointLinuxPreservesIdentityAndRefreshesEndpoint(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	configureLinuxCheckpointMetadata(metadata)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.46"]`}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	configureLinuxCheckpointBackend(b)
	b.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if target.TargetOS != core.TargetLinux {
			t.Fatalf("restored target OS=%q", target.TargetOS)
		}
		return nil
	}
	persistCheckpointSource(t, b)

	lease, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err != nil {
		t.Fatalf("restoreNativeCheckpoint Linux: %v", err)
	}
	if lease.Server.CloudID != testCheckpointVMName || lease.SSH.Host != "192.0.2.46" || lease.SSH.TargetOS != core.TargetLinux {
		t.Fatalf("restored Linux lease=%#v", lease)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve refreshed Linux claim: ok=%v err=%v", ok, err)
	}
	if claim.ProviderScope != instanceScope(testCheckpointVMName) || claim.SSHHost != "192.0.2.46" {
		t.Fatalf("refreshed Linux claim=%#v", claim)
	}
	if findCallIndex(runner.calls, "Invoke-Command -VMName") >= 0 {
		t.Fatal("Linux restore used PowerShell Direct")
	}
}

func TestRestoreNativeCheckpointLinuxPreservesSavedCacheLease(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	configureLinuxCheckpointMetadata(metadata)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var events []string
	var cacheVHDPath string
	ipQueries := 0
	runner.onRun = func(req core.LocalCommandRequest) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Start-VM -Name"):
			events = append(events, "resume")
		case strings.Contains(script, "Remove-VMHardDiskDrive -ErrorAction Stop"):
			events = append(events, "detach")
		case strings.Contains(script, "Restore-VMSnapshot"):
			events = append(events, "restore")
		case strings.Contains(script, "Start-VM -VM $vm"):
			events = append(events, "start-restored")
		case cacheVHDPath != "" && strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			events = append(events, "reattach")
		case strings.Contains(script, "Save-VM"):
			events = append(events, "pause")
		}
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: hypervStateSaved}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			ipQueries++
			if ipQueries == 1 {
				return core.LocalCommandResult{Stdout: `["192.0.2.46"]`}, nil, true
			}
			return core.LocalCommandResult{Stdout: `["192.0.2.47"]`}, nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case cacheVHDPath != "" && strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 1}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	configureLinuxCheckpointBackend(b)
	b.cacheRoot = t.TempDir()
	b.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if target.TargetOS != core.TargetLinux {
			t.Fatalf("restored target OS=%q", target.TargetOS)
		}
		return nil
	}
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, command string) (string, error) {
		switch {
		case strings.Contains(command, "sudo umount"):
			events = append(events, "unmount")
			return "", nil
		case strings.Contains(command, "/etc/fstab"):
			events = append(events, "remount")
			return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil
		default:
			return "", nil
		}
	}
	persistCheckpointSource(t, b)
	cacheVolume := core.CacheVolumeConfig{
		Key:      "linux-restore-cache",
		Path:     "/var/cache/crabbox/go-build",
		Required: true,
	}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:      hypervCacheMetadataVersion,
		Key:          cacheVolume.Key,
		Target:       targetLinux,
		Filesystem:   "ext4",
		DiskID:       "11111111-2222-3333-4444-555555555555",
		FilesystemID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		SizeGB:       80,
	})
	cacheVHDPath = b.cacheVolumePaths(cacheVolume.Key).vhd
	if err := core.UpdateLeaseClaimCacheVolumes(testCheckpointLeaseID, core.CacheVolumeStickyDiskSpecs([]core.CacheVolumeConfig{cacheVolume})); err != nil {
		t.Fatal(err)
	}

	lease, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err != nil {
		t.Fatalf("restoreNativeCheckpoint Linux saved cache: %v", err)
	}
	if lease.SSH.Host != "192.0.2.47" ||
		lease.Server.Status != "paused" ||
		lease.Server.Labels["state"] != "paused" ||
		lease.Server.Labels["hyperv_state"] != "saved" {
		t.Fatalf("restored saved Linux lease=%#v", lease)
	}
	wantEvents := []string{"resume", "unmount", "detach", "restore", "start-restored", "reattach", "remount", "pause"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("Linux saved restore cache events=%v want %v", events, wantEvents)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve restored saved Linux claim: ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.47" ||
		claim.Labels["state"] != "paused" ||
		len(claim.CacheVolumes) != 1 ||
		claim.CacheVolumes[0] != cacheVolume.Key+":"+cacheVolume.Path {
		t.Fatalf("restored saved Linux claim=%#v", claim)
	}
}

func TestRestoreNativeCheckpointRejectsRepoConflictBeforeMutation(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	persistCheckpointSource(t, b)

	_, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("restore conflict err=%v", err)
	}
	if findCallIndex(runner.calls, "Restore-VMSnapshot") >= 0 {
		t.Fatal("restore mutated the VM before rejecting the repo ownership conflict")
	}
}

func TestRestoreNativeCheckpointRollsBackReclaimedLeaseOnFailure(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Select-Object Name,@{Name='ID'") {
			return core.LocalCommandResult{Stderr: "query failed"}, errors.New("exit 1"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	original, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve original claim: ok=%v err=%v", ok, err)
	}

	_, err = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err == nil {
		t.Fatal("restore succeeded after VM query failure")
	}
	restored, ok, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if resolveErr != nil || !ok {
		t.Fatalf("resolve rolled-back claim: ok=%v err=%v", ok, resolveErr)
	}
	if restored.RepoRoot != original.RepoRoot {
		t.Fatalf("claim repo=%q, want rollback to %q", restored.RepoRoot, original.RepoRoot)
	}
}

func TestRestoreNativeCheckpointRollsBackWhenSnapshotRestoreFails(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{Stderr: "restore failed"}, errors.New("exit 1"), true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	persistCheckpointSource(t, b)
	original, ok, err := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve original claim: ok=%v err=%v", ok, err)
	}

	_, err = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err == nil {
		t.Fatal("restore succeeded after Restore-VMSnapshot failed")
	}
	restored, ok, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if resolveErr != nil || !ok {
		t.Fatalf("resolve rolled-back claim: ok=%v err=%v", ok, resolveErr)
	}
	if restored.RepoRoot != original.RepoRoot {
		t.Fatalf("claim repo=%q, want rollback to %q", restored.RepoRoot, original.RepoRoot)
	}
}

func TestRestoreNativeCheckpointKeepsReclaimedLeaseAfterVMMutation(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	persistCheckpointSource(t, b)
	newRepo := t.TempDir()

	_, err := b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: newRepo},
		Reclaim: true,
	})
	if err == nil {
		t.Fatal("restore succeeded when SSH readiness failed")
	}
	claim, ok, resolveErr := core.ResolveLeaseClaimForProvider(testCheckpointLeaseID, providerName)
	if resolveErr != nil || !ok {
		t.Fatalf("resolve retained claim: ok=%v err=%v", ok, resolveErr)
	}
	if claim.RepoRoot != newRepo {
		t.Fatalf("claim repo=%q, want mutated VM retained by %q", claim.RepoRoot, newRepo)
	}
}

func TestRestoreNativeCheckpointHoldsClaimFenceAcrossVMEffects(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	var b *backend
	reassigned := make(chan struct{})
	var once sync.Once
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			// A concurrent actor tries to reassign the lease while restore effects run.
			once.Do(func() {
				go func() {
					defer close(reassigned)
					other := t.TempDir()
					if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(testCheckpointLeaseID, "other", providerName, instanceScope(testCheckpointVMName), b.cfg.Pond, other, b.cfg.IdleTimeout, true, core.Server{}, core.SSHTarget{}); err == nil {
						record("claim-reassigned")
					}
				}()
			})
			return jsonResult(t, checkpointVM{Name: testCheckpointVMName, ID: testCheckpointVMID, State: 3}), nil, true
		case strings.Contains(script, "Restore-VMSnapshot"):
			time.Sleep(200 * time.Millisecond)
			record("snapshot-restored")
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Start-VM"):
			record("vm-started")
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.45"]`}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b = testBackend(runner)
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	persistCheckpointSource(t, b)

	_, _ = b.restoreNativeCheckpoint(context.Background(), core.NativeCheckpointRestoreRequest{
		Config:  b.cfg,
		Record:  checkpointForkRecord(paths, metadata),
		LeaseID: testCheckpointLeaseID,
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	<-reassigned
	mu.Lock()
	defer mu.Unlock()
	pos := map[string]int{}
	for i, event := range events {
		pos[event] = i
	}
	for _, effect := range []string{"snapshot-restored", "vm-started"} {
		if at, ok := pos[effect]; ok {
			if re, ok := pos["claim-reassigned"]; ok && re < at {
				t.Fatalf("claim was reassigned before %s completed: %v", effect, events)
			}
		}
	}
	if _, ok := pos["snapshot-restored"]; !ok {
		t.Fatalf("restore did not run under its own authority: %v", events)
	}
}

func TestAcquireCheckpointCallbackFailureRollsBackNewFork(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	var imported bool
	var callbackLease core.LeaseTarget
	runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		if strings.Contains(script, "Import-VM") {
			imported = true
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		}
		if strings.Contains(script, "Invoke-Command") || strings.Contains(script, "Get-VMNetworkAdapter -VMName") ||
			strings.Contains(script, "ConvertTo-Json -InputObject $items") {
			t.Fatalf("callback failure reached fork readiness: %s", script)
		}
		return core.LocalCommandResult{}, nil, true
	}}
	b := testBackend(runner)
	record := checkpointForkRecord(paths, metadata)
	failure := errors.New("acquisition owner rejected fork")
	calls := 0
	_, err := b.Acquire(t.Context(), core.AcquireRequest{
		CheckpointSource: &record,
		Repo:             core.Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedSlug:    "callback-fork",
		OnAcquired: func(lease core.LeaseTarget) error {
			calls++
			if !imported || lease.LeaseID == "" || lease.LeaseID == testCheckpointLeaseID ||
				lease.Server.CloudID == "" || lease.Server.CloudID == testCheckpointVMName ||
				lease.Server.CloudID != lease.Server.Labels["instance"] {
				t.Fatalf("callback received unvalidated fork identity: %#v", lease)
			}
			callbackLease = lease
			return failure
		},
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("callback calls=%d error=%v", calls, err)
	}
	cleanup := findScript(runner.calls, "Remove-VM -VM $candidate")
	if !strings.Contains(cleanup, "$name='"+callbackLease.Server.CloudID+"'") ||
		strings.Contains(cleanup, testCheckpointVMName) {
		t.Fatal("callback failure did not target only the new imported registration")
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 0 {
		t.Fatalf("callback failure left fork claims: %#v error=%v", claims, err)
	}
	for _, path := range []string{
		filepath.Join(hypervVMDir(), callbackLease.Server.CloudID),
		filepath.Join(hypervVHDDir(), callbackLease.Server.CloudID),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("callback failure left fork storage %s: %v", path, err)
		}
	}
	if _, err := os.Stat(paths.config); err != nil {
		t.Fatalf("callback failure modified source checkpoint: %v", err)
	}
}

func TestForkNativeCheckpointCreatesFreshIdentityAndConnectsNetworkLast(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	var cacheVHDPath string
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Import-VM"):
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.55"]`}, nil, true
		case strings.Contains(script, "Invoke-Command"):
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "ConvertTo-Json -InputObject $items"):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 2}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	b.cfg.Tailscale.Enabled = true
	b.cfg.Tailscale.AuthKey = "fixture-only-invalid-value"
	b.cfg.Tailscale.Hostname = "crabbox-fork"
	b.cfg.Tailscale.Tags = []string{"tag:crabbox"}
	b.cacheRoot = t.TempDir()
	cacheVolume := core.CacheVolumeConfig{
		Key:      "fork-nuget-cache",
		Path:     `D:\crabbox-cache\nuget`,
		Required: true,
	}
	b.cfg.Cache.Volumes = []core.CacheVolumeConfig{cacheVolume}
	writeTestHyperVCacheVolume(t, b, cacheVolume, hypervCacheMetadata{
		Version:    hypervCacheMetadataVersion,
		Key:        cacheVolume.Key,
		Target:     "windows/normal",
		Filesystem: "ntfs",
		DiskID:     "11111111-2222-3333-4444-555555555555",
		SizeGB:     80,
	})
	cacheVHDPath = b.cacheVolumePaths(cacheVolume.Key).vhd
	b.ensureLeaseKey = func(core.Config, string) (string, string, error) {
		keyPath := filepath.Join(t.TempDir(), "id_ed25519")
		if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
			return "", "", err
		}
		return keyPath, "ssh-ed25519 AAAATEST fork@test", nil
	}
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	lease, err := b.ForkNativeCheckpoint(context.Background(), core.NativeCheckpointForkLifecycleRequest{
		Record:        checkpointForkRecord(paths, metadata),
		Repo:          core.Repo{Root: t.TempDir(), Name: "my-app"},
		Keep:          true,
		RequestedSlug: "checkpoint-fork",
	})
	if err != nil {
		t.Fatalf("ForkNativeCheckpoint: %v", err)
	}
	if lease.LeaseID == "" || lease.LeaseID == testCheckpointLeaseID || lease.SSH.Host != "192.0.2.55" {
		t.Fatalf("forked lease=%#v", lease)
	}
	importScript := findScript(runner.calls, "Import-VM")
	for _, expected := range []string{"-Copy", "-GenerateNewId", "-DynamicMacAddress", "Disconnect-VMNetworkAdapter", "Remove-VM -VM $vm"} {
		if !strings.Contains(importScript, expected) {
			t.Fatalf("import script missing %s: %s", expected, importScript)
		}
	}
	rotationIndex := findCallIndex(runner.calls, "Rename-Computer")
	restartIndex := findCallIndex(runner.calls, "Restart-VM")
	postRestartKeyIndex := findLastCallIndex(runner.calls, "ssh-keygen.exe")
	connectIndex := findCallIndex(runner.calls, "Connect-VMNetworkAdapter")
	if rotationIndex < 0 || restartIndex <= rotationIndex || postRestartKeyIndex <= restartIndex || connectIndex <= postRestartKeyIndex {
		t.Fatalf("network was not connected after post-reboot SSH identity rotation: rotation=%d restart=%d key=%d connect=%d", rotationIndex, restartIndex, postRestartKeyIndex, connectIndex)
	}
	importIndex := findCallIndex(runner.calls, "Import-VM")
	excludeIndex := findCallIndex(runner.calls, "GetFileName($_.Path)")
	cacheAttachIndex := findCallIndexAll(runner.calls, "Add-VMHardDiskDrive", cacheVHDPath)
	if excludeIndex <= importIndex || cacheAttachIndex <= excludeIndex {
		t.Fatalf("fork cache order import=%d exclude=%d attach=%d", importIndex, excludeIndex, cacheAttachIndex)
	}

	excludeScript := commandScript(runner.calls[excludeIndex])
	if strings.Contains(excludeScript, "ControllerLocation") || !strings.Contains(excludeScript, `^cache-[0-9a-fA-F]{32}`) {
		t.Fatalf("fork exclusion was not limited to provider cache disk names: %s", excludeScript)
	}
	rotationScript := commandScript(runner.calls[rotationIndex])
	for _, expected := range []string{"ssh-ed25519 AAAATEST", "ssh_host_*", "Tailscale", "vnc.password", "MachineGuid", "[guid]::NewGuid()"} {
		if !strings.Contains(rotationScript, expected) {
			t.Fatalf("identity rotation missing %s: %s", expected, rotationScript)
		}
	}
	tailscaleIndex := findCallIndex(runner.calls, "$upArgs.Add('up')")
	if tailscaleIndex <= connectIndex {
		t.Fatalf("fork Tailscale join did not run after network connection: connect=%d tailscale=%d", connectIndex, tailscaleIndex)
	}
	tailscaleCommand := commandScript(runner.calls[tailscaleIndex])
	if strings.Contains(tailscaleCommand, b.cfg.Tailscale.AuthKey) {
		t.Fatal("fork Tailscale auth key leaked into host argv")
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("read fork claim ok=%v err=%v", ok, err)
	}
	wantCache := cacheVolume.Key + ":" + cacheVolume.Path
	if len(claim.CacheVolumes) != 1 || claim.CacheVolumes[0] != wantCache {
		t.Fatalf("fork claim cache volumes=%#v want %q", claim.CacheVolumes, wantCache)
	}
}

func TestForkNativeCheckpointLinuxSpecializesTailscaleAndCachesAfterNetwork(t *testing.T) {
	setCheckpointTestState(t)
	paths, metadata := createCheckpointArtifact(t)
	configureLinuxCheckpointMetadata(metadata)
	var capturedUserData, capturedMetaData, seedPath string
	var cacheVHDPath, optionalCacheVHDPath string
	tailscaleOrder := -1
	var tailscaleConfig core.Config
	var tailscaleTarget core.SSHTarget
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		onRun: func(req core.LocalCommandRequest) {
			script := commandScript(req)
			if !strings.Contains(script, "NewFileSystemLabel 'cidata'") {
				return
			}
			capturedUserData = readRequestEnvFile(t, req, "_CRABBOX_USER_DATA_PATH")
			capturedMetaData = readRequestEnvFile(t, req, "_CRABBOX_META_DATA_PATH")
			seedPath = powerShellSeedPath(t, script)
			if err := os.MkdirAll(filepath.Dir(seedPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(seedPath, []byte("seed"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		script := commandScript(req)
		switch {
		case strings.Contains(script, "Import-VM"):
			return jsonResult(t, checkpointImportOutput{ID: "99999999-8888-7777-6666-555555555555"}), nil, true
		case strings.Contains(script, "Select-Object Name,@{Name='ID'"):
			return jsonResult(t, checkpointVM{Name: "fork", ID: "99999999-8888-7777-6666-555555555555", State: 3}), nil, true
		case strings.Contains(script, "Select-Object -ExpandProperty IPAddresses"):
			return core.LocalCommandResult{Stdout: `["192.0.2.56"]`}, nil, true
		case optionalCacheVHDPath != "" &&
			strings.Contains(script, "ConvertTo-Json -InputObject $items") &&
			strings.Contains(script, optionalCacheVHDPath):
			return jsonResult(t, []hypervCacheAttachment{{
				VMName: "crabbox-other",
				Path:   optionalCacheVHDPath,
			}}), nil, true
		case cacheVHDPath != "" &&
			strings.Contains(script, "ConvertTo-Json -InputObject $items") &&
			strings.Contains(script, cacheVHDPath):
			return core.LocalCommandResult{Stdout: "[]"}, nil, true
		case cacheVHDPath != "" && strings.Contains(script, "Add-VMHardDiskDrive") && strings.Contains(script, cacheVHDPath):
			return jsonResult(t, hypervCacheAttachOutput{ControllerLocation: 2}), nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	b := testBackend(runner)
	configureLinuxCheckpointBackend(b)
	b.cacheRoot = t.TempDir()
	b.cfg.Tailscale.Enabled = true
	b.cfg.Tailscale.AuthKey = "invalid-tailscale-fork-fixture"
	b.cfg.Tailscale.Hostname = "crabbox-linux-fork"
	b.cfg.Tailscale.Tags = []string{"tag:crabbox"}
	cacheVolume := core.CacheVolumeConfig{
		Key:      "linux-fork-go-build",
		Path:     "/var/cache/crabbox/go-build",
		Required: true,
	}
	optionalCacheVolume := core.CacheVolumeConfig{
		Key:  "linux-fork-optional",
		Path: "/var/cache/crabbox/optional",
	}
	b.cfg.Cache.Volumes = []core.CacheVolumeConfig{cacheVolume, optionalCacheVolume}
	for _, volume := range b.cfg.Cache.Volumes {
		writeTestHyperVCacheVolume(t, b, volume, hypervCacheMetadata{
			Version:      hypervCacheMetadataVersion,
			Key:          volume.Key,
			Target:       targetLinux,
			Filesystem:   "ext4",
			DiskID:       "11111111-2222-3333-4444-555555555555",
			FilesystemID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			SizeGB:       80,
		})
	}
	cacheVHDPath = b.cacheVolumePaths(cacheVolume.Key).vhd
	optionalCacheVHDPath = b.cacheVolumePaths(optionalCacheVolume.Key).vhd
	b.runSSHOutput = func(_ context.Context, _ core.SSHTarget, command string) (string, error) {
		if strings.Contains(command, "/etc/fstab") {
			return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil
		}
		return "", nil
	}
	b.bootstrapTailscale = func(_ context.Context, cfg core.Config, target core.SSHTarget) (string, error) {
		tailscaleOrder = len(runner.calls)
		tailscaleConfig = cfg
		tailscaleTarget = target
		return "", nil
	}
	b.ensureLeaseKey = func(core.Config, string) (string, string, error) {
		keyPath := filepath.Join(t.TempDir(), "id_ed25519")
		if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
			return "", "", err
		}
		return keyPath, "ssh-ed25519 AAAATEST fork@test", nil
	}
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	lease, err := b.ForkNativeCheckpoint(context.Background(), core.NativeCheckpointForkLifecycleRequest{
		Record:        checkpointForkRecord(paths, metadata),
		Repo:          core.Repo{Root: t.TempDir(), Name: "my-app"},
		Keep:          true,
		RequestedSlug: "linux-checkpoint-fork",
	})
	if err != nil {
		t.Fatalf("ForkNativeCheckpoint Linux: %v", err)
	}
	if lease.LeaseID == "" || lease.LeaseID == testCheckpointLeaseID || lease.SSH.Host != "192.0.2.56" || lease.SSH.TargetOS != core.TargetLinux {
		t.Fatalf("forked Linux lease=%#v", lease)
	}
	importScript := findScript(runner.calls, "Import-VM")
	if !strings.Contains(importScript, "Disconnect-VMNetworkAdapter") || strings.Contains(importScript, "Start-VM -VM $vm") {
		t.Fatalf("Linux import was not disconnected and stopped: %s", importScript)
	}
	startIndices := findCallIndices(runner.calls, "Start-VM")
	importIndex := findCallIndex(runner.calls, "Import-VM")
	inheritedDetachIndex := findCallIndex(runner.calls, "GetFileName($_.Path)")
	seedCreateIndex := findCallIndex(runner.calls, "NewFileSystemLabel 'cidata'")
	seedAttachIndex := findCallIndexAll(runner.calls, "Add-VMHardDiskDrive", seedPath)
	firmwareIndex := findCallIndex(runner.calls, "Set-VMFirmware")
	offIndex := findCallIndex(runner.calls, "Select-Object Name,@{Name='ID'")
	detachIndex := findCallIndexAll(runner.calls, "Remove-VMHardDiskDrive", seedPath)
	verifyIndex := findCallIndex(runner.calls, "offline specialization completion marker")
	connectIndex := findCallIndex(runner.calls, "Connect-VMNetworkAdapter")
	ipIndex := findCallIndex(runner.calls, "Select-Object -ExpandProperty IPAddresses")
	cacheAttachIndex := findCallIndexAll(runner.calls, "Add-VMHardDiskDrive", cacheVHDPath)
	if len(startIndices) != 2 ||
		!(importIndex < inheritedDetachIndex &&
			inheritedDetachIndex < seedCreateIndex &&
			seedCreateIndex < seedAttachIndex &&
			seedAttachIndex < firmwareIndex &&
			firmwareIndex < startIndices[0] &&
			startIndices[0] < offIndex &&
			offIndex < detachIndex &&
			detachIndex < verifyIndex &&
			verifyIndex < connectIndex &&
			connectIndex < startIndices[1] &&
			startIndices[1] < ipIndex &&
			ipIndex < tailscaleOrder &&
			tailscaleOrder < cacheAttachIndex) {
		t.Fatalf(
			"Linux specialization order import=%d inherited-detach=%d seed=%d seed-attach=%d firmware=%d starts=%v off=%d detach=%d verify=%d connect=%d ip=%d tailscale=%d cache-attach=%d",
			importIndex,
			inheritedDetachIndex,
			seedCreateIndex,
			seedAttachIndex,
			firmwareIndex,
			startIndices,
			offIndex,
			detachIndex,
			verifyIndex,
			connectIndex,
			ipIndex,
			tailscaleOrder,
			cacheAttachIndex,
		)
	}
	if firmwareScript := commandScript(runner.calls[firmwareIndex]); !strings.Contains(firmwareScript, secureBootTemplateLinux) {
		t.Fatalf("fork firmware did not preserve source template: %s", firmwareScript)
	}
	instanceID := linuxForkSpecializationInstanceID(lease.LeaseID)
	hostname := forkGuestHostname(lease.LeaseID)
	if !strings.Contains(capturedMetaData, "instance-id: "+instanceID) ||
		!strings.Contains(capturedMetaData, "local-hostname: "+hostname) ||
		strings.Contains(capturedMetaData, testCheckpointVMName) {
		t.Fatalf("specialization meta-data=%q", capturedMetaData)
	}
	for _, expected := range []string{
		"authorized_keys",
		`gid="$(id -g crabbox)"`,
		"rm -f /etc/ssh/ssh_host_*",
		"ssh-keygen -A",
		"hostnamectl set-hostname " + hostname,
		"systemctl cat tailscaled.service",
		"rm -f /var/lib/tailscale/tailscaled.state",
		"rm -f '/var/lib/crabbox'/tailscale-*",
		"current_instance_dir=\"/var/lib/cloud/instances/" + instanceID + "\"",
		"! -path \"$current_instance_dir\"",
		linuxForkSpecializationMarker,
		linuxForkSeedCompletionMarker,
		"blkid -L cidata",
		"systemctl poweroff",
	} {
		if !strings.Contains(capturedUserData, expected) {
			t.Fatalf("specialization user-data missing %q: %s", expected, capturedUserData)
		}
	}
	if strings.Contains(capturedUserData, b.cfg.Tailscale.AuthKey) ||
		strings.Contains(capturedMetaData, b.cfg.Tailscale.AuthKey) ||
		strings.Contains(capturedUserData, "tailscale up") {
		t.Fatal("Linux fork specialization artifact contains Tailscale join material")
	}
	if tailscaleOrder < 0 ||
		tailscaleTarget.Host != "192.0.2.56" ||
		tailscaleTarget.TargetOS != core.TargetLinux ||
		tailscaleConfig.Tailscale.AuthKey != b.cfg.Tailscale.AuthKey ||
		tailscaleConfig.Tailscale.ResetIdentityBeforeUp ||
		tailscaleConfig.Tailscale.ScrubCloudInitSecrets {
		t.Fatalf("Linux fork Tailscale rejoin cfg=%#v target=%#v order=%d", tailscaleConfig.Tailscale, tailscaleTarget, tailscaleOrder)
	}
	if seedPath == "" {
		t.Fatal("specialization seed path was not captured")
	}
	if _, err := os.Stat(seedPath); !os.IsNotExist(err) {
		t.Fatalf("specialization seed remains after network enable: %v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve forked Linux claim: ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.56" || claim.ProviderScope != instanceScope(lease.Server.CloudID) {
		t.Fatalf("forked Linux claim=%#v", claim)
	}
	wantCache := cacheVolume.Key + ":" + cacheVolume.Path
	if len(claim.CacheVolumes) != 1 || claim.CacheVolumes[0] != wantCache {
		t.Fatalf("forked Linux claim cache volumes=%#v want %q", claim.CacheVolumes, wantCache)
	}
	if findCallIndex(runner.calls, "Invoke-Command -VMName") >= 0 {
		t.Fatal("Linux checkpoint fork used PowerShell Direct")
	}
}

func TestRemoveImportedCheckpointVMFindsPartialRegistrationByStorage(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.removeImportedCheckpointVM(context.Background(), "crabbox-partial-import"); err != nil {
		t.Fatalf("removeImportedCheckpointVM: %v", err)
	}
	cleanupScript := findScript(runner.calls, "Get-CrabboxImportedVM")
	for _, expected := range []string{
		"ConfigurationLocation",
		"SnapshotFileLocation",
		"Get-VM -ErrorAction Stop",
		"Get-VMHardDiskDrive -VM $candidate -ErrorAction Stop",
		"partial checkpoint import cleanup left",
	} {
		if !strings.Contains(cleanupScript, expected) {
			t.Fatalf("partial import cleanup script missing %q: %s", expected, cleanupScript)
		}
	}
}

func TestRemoveImportedCheckpointVMReportsUnverifiedCleanup(t *testing.T) {
	setCheckpointTestState(t)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if strings.Contains(commandScript(req), "Get-CrabboxImportedVM") {
			return core.LocalCommandResult{Stderr: "registered VM remains"}, errors.New("cleanup failed"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)

	err := b.removeImportedCheckpointVM(context.Background(), "crabbox-partial-import")
	if err == nil || !strings.Contains(err.Error(), "remove partial Hyper-V checkpoint import") {
		t.Fatalf("cleanup error=%v", err)
	}
}

func TestDeleteNativeCheckpointIsIndependentAndIdempotent(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	sourceMarker := filepath.Join(t.TempDir(), "source.vm")
	if err := os.WriteFile(sourceMarker, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	req := core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: paths.artifactDir,
		Metadata:    metadata,
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("DeleteNativeCheckpoint: %v", err)
	}
	if _, err := os.Stat(paths.exportRoot); !os.IsNotExist(err) {
		t.Fatalf("export remains after delete: %v", err)
	}
	if _, err := os.Stat(sourceMarker); err != nil {
		t.Fatalf("checkpoint delete removed independent source marker: %v", err)
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("idempotent DeleteNativeCheckpoint: %v", err)
	}
}

func TestDeleteNativeCheckpointCleansPendingJournalWithoutSnapshotID(t *testing.T) {
	paths, metadata := createCheckpointArtifact(t)
	delete(metadata, checkpointMetadataSnapshotID)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	req := core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: paths.artifactDir,
		Metadata:    metadata,
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatalf("DeleteNativeCheckpoint pending journal: %v", err)
	}
	if _, err := os.Stat(paths.exportRoot); !os.IsNotExist(err) {
		t.Fatalf("export remains after pending cleanup: %v", err)
	}
	if findScript(runner.calls, "Remove-VMSnapshot") == "" {
		t.Fatal("pending cleanup did not remove the named snapshot")
	}
}

func TestDeleteNativeCheckpointRejectsPathTraversal(t *testing.T) {
	artifactDir := t.TempDir()
	victim := filepath.Join(filepath.Dir(artifactDir), "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(victim, "victim.vmcx")
	if err := os.WriteFile(config, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	metadata := checkpointMetadata(artifactDir, filepath.Join(artifactDir, "..", "victim"), config)
	err := (Provider{}).DeleteNativeCheckpoint(context.Background(), core.NativeCheckpointResourceRequest{
		Config:      testBackend(runner).cfg,
		Runtime:     core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		ArtifactDir: artifactDir,
		Metadata:    metadata,
	})
	if err == nil || !strings.Contains(err.Error(), "unowned") {
		t.Fatalf("path traversal delete err=%v", err)
	}
	if _, err := os.Stat(config); err != nil {
		t.Fatalf("path traversal validation removed victim: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("path traversal reached PowerShell: %#v", runner.calls)
	}
}

func TestReleaseStoragePreservesCheckpointExport(t *testing.T) {
	setCheckpointTestState(t)
	name := "crabbox-release-1234"
	vhdDir := hypervVHDDir()
	if err := os.MkdirAll(vhdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vhdDir, name+".vhdx"), []byte("lease"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, _ := createCheckpointArtifact(t)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	if err := b.removeVMStorage(name, nil); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	if _, err := os.Stat(paths.config); err != nil {
		t.Fatalf("source release removed checkpoint export: %v", err)
	}
}

func TestCleanupSkipsCheckpointRestoreReservation(t *testing.T) {
	now := time.Now().UTC()
	server := core.Server{Status: "stopped", Labels: map[string]string{
		hypervCheckpointRestoreReservationLabel: core.LeaseLabelTime(now.Add(time.Minute)),
	}}
	claim := core.LeaseClaim{LeaseID: testCheckpointLeaseID, Labels: server.Labels}
	if cleanup, reason := shouldCleanup(server, claim, true, now); cleanup || reason != "checkpoint restore reserved" {
		t.Fatalf("cleanup=%v reason=%q", cleanup, reason)
	}
}

func setCheckpointTestState(t *testing.T) {
	t.Helper()
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
}

func persistCheckpointSource(t *testing.T, b *backend) {
	t.Helper()
	cfg := b.configForRun()
	server := checkpointSourceServer(b)
	lease := core.LeaseTarget{Server: server, LeaseID: testCheckpointLeaseID}
	req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}
	if err := persistLease(testCheckpointLeaseID, "checkpoint-source", testCheckpointVMName, cfg, req, &lease, nil); err != nil {
		t.Fatalf("persist source lease: %v", err)
	}
}

func checkpointSourceServer(b *backend) core.Server {
	cfg := b.configForRun()
	labels := core.DirectLeaseLabels(cfg, testCheckpointLeaseID, "checkpoint-source", providerName, "", true, time.Now().UTC())
	labels["instance"] = testCheckpointVMName
	labels["ssh_user"] = cfg.HyperV.User
	labels["work_root"] = cfg.HyperV.WorkRoot
	claim := core.LeaseClaim{
		LeaseID:       testCheckpointLeaseID,
		Slug:          "checkpoint-source",
		Provider:      providerName,
		ProviderScope: instanceScope(testCheckpointVMName),
		Labels:        labels,
	}
	return b.serverFromInstance(hypervVM{Name: testCheckpointVMName, State: 2}, claim, cfg)
}

func createCheckpointArtifact(t *testing.T) (checkpointArtifactPaths, map[string]string) {
	t.Helper()
	artifactDir := t.TempDir()
	exportRoot := filepath.Join(artifactDir, "hyperv")
	config := filepath.Join(exportRoot, "exported", "Virtual Machines", "checkpoint.vmcx")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("vmcx"), 0o600); err != nil {
		t.Fatal(err)
	}
	return checkpointArtifactPaths{artifactDir: artifactDir, exportRoot: exportRoot, config: config}, checkpointMetadata(artifactDir, exportRoot, config)
}

func checkpointMetadata(artifactDir, exportRoot, config string) map[string]string {
	return map[string]string{
		checkpointMetadataArtifactDir:    artifactDir,
		checkpointMetadataExportRoot:     exportRoot,
		checkpointMetadataExportConfig:   config,
		checkpointMetadataSourceLease:    testCheckpointLeaseID,
		checkpointMetadataSourceVM:       testCheckpointVMName,
		checkpointMetadataSourceVMID:     testCheckpointVMID,
		checkpointMetadataSnapshotID:     testCheckpointSnapshot,
		checkpointMetadataSnapshotName:   "checkpoint",
		checkpointMetadataCheckpointType: "ProductionOnly",
		checkpointMetadataTarget:         core.TargetWindows,
		checkpointMetadataProviderScope:  instanceScope(testCheckpointVMName),
		checkpointMetadataSSHUser:        "crabbox",
		checkpointMetadataWorkRoot:       `C:\crabbox`,
		checkpointMetadataSwitch:         "Default Switch",
	}
}

func configureLinuxCheckpointBackend(b *backend) {
	b.cfg.TargetOS = core.TargetLinux
	b.cfg.WindowsMode = ""
	b.cfg.HyperV.Image = `C:\Images\debian-cloud.vhdx`
	b.cfg.HyperV.GuestPassword = ""
	b.cfg.HyperV.WorkRoot = "/work/crabbox"
	b.cfg.WorkRoot = "/work/crabbox"
	b.cfg.SSHUser = b.cfg.HyperV.User
}

func configureLinuxCheckpointMetadata(metadata map[string]string) {
	metadata[checkpointMetadataTarget] = core.TargetLinux
	metadata[checkpointMetadataWorkRoot] = "/work/crabbox"
	metadata[checkpointMetadataSpecialization] = linuxForkSpecializationVersion
	metadata[checkpointMetadataSecureBoot] = "true"
	metadata[checkpointMetadataSecureTemplate] = secureBootTemplateLinux
}

func checkpointForkRecord(paths checkpointArtifactPaths, metadata map[string]string) core.NativeCheckpointForkRecord {
	target, err := checkpointTarget(metadata)
	if err != nil {
		panic(err)
	}
	windowsMode := ""
	if target == core.TargetWindows {
		windowsMode = core.WindowsModeNormal
	}
	return core.NativeCheckpointForkRecord{
		Kind:        hypervCheckpointKind,
		ImageID:     testCheckpointSnapshot,
		Name:        metadata[checkpointMetadataSnapshotName],
		Resource:    paths.config,
		ArtifactDir: paths.artifactDir,
		TargetOS:    target,
		WindowsMode: windowsMode,
		Metadata:    metadata,
	}
}

func commandScript(req core.LocalCommandRequest) string {
	if len(req.Args) == 0 {
		return ""
	}
	return req.Args[len(req.Args)-1]
}

func findScript(calls []core.LocalCommandRequest, needle string) string {
	index := findCallIndex(calls, needle)
	if index < 0 {
		return ""
	}
	return commandScript(calls[index])
}

func findCallIndex(calls []core.LocalCommandRequest, needle string) int {
	for i, call := range calls {
		if strings.Contains(commandScript(call), needle) {
			return i
		}
	}
	return -1
}

func findLastCallIndex(calls []core.LocalCommandRequest, needle string) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if strings.Contains(commandScript(calls[i]), needle) {
			return i
		}
	}
	return -1
}

func findCallIndexAll(calls []core.LocalCommandRequest, needles ...string) int {
	for i, call := range calls {
		script := commandScript(call)
		matches := true
		for _, needle := range needles {
			if !strings.Contains(script, needle) {
				matches = false
				break
			}
		}
		if matches {
			return i
		}
	}
	return -1
}

func findCallIndices(calls []core.LocalCommandRequest, needle string) []int {
	var indices []int
	for i, call := range calls {
		if strings.Contains(commandScript(call), needle) {
			indices = append(indices, i)
		}
	}
	return indices
}

func readRequestEnvFile(t *testing.T, req core.LocalCommandRequest, name string) string {
	t.Helper()
	path := requestEnv(req, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func powerShellSeedPath(t *testing.T, script string) string {
	t.Helper()
	const marker = "$path = '"
	start := strings.Index(script, marker)
	if start < 0 {
		t.Fatalf("seed script has no path assignment: %s", script)
	}
	start += len(marker)
	end := strings.Index(script[start:], "';")
	if end < 0 {
		t.Fatalf("seed script has no path terminator: %s", script)
	}
	return strings.ReplaceAll(script[start:start+end], "''", "'")
}

func jsonResult(t *testing.T, value any) core.LocalCommandResult {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return core.LocalCommandResult{Stdout: string(data)}
}

func checkpointNameFromScript(script string) string {
	const marker = "-SnapshotName '"
	start := strings.Index(script, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	end := strings.Index(script[start:], "'")
	if end < 0 {
		return ""
	}
	return strings.ReplaceAll(script[start:start+end], "''", "'")
}
