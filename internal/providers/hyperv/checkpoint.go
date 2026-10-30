package hyperv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	hypervCheckpointKind                     = "hyperv-vhdx"
	hypervCheckpointRestoreReservationLabel  = "checkpoint_restore_reserved_until"
	hypervCheckpointRestoreReservationWindow = 15 * time.Minute
	hypervCheckpointCleanupTimeout           = 2 * time.Minute

	checkpointMetadataArtifactDir    = "artifact_dir"
	checkpointMetadataExportRoot     = "export_root"
	checkpointMetadataExportConfig   = "exported_configuration"
	checkpointMetadataSourceLease    = "source_lease"
	checkpointMetadataSourceVM       = "source_vm"
	checkpointMetadataSourceVMID     = "source_vm_id"
	checkpointMetadataSnapshotID     = "snapshot_id"
	checkpointMetadataSnapshotName   = "snapshot_name"
	checkpointMetadataCheckpointType = "checkpoint_type"
	checkpointMetadataTarget         = "target"
	checkpointMetadataProviderScope  = "provider_scope"
	checkpointMetadataSSHUser        = "ssh_user"
	checkpointMetadataWorkRoot       = "work_root"
	checkpointMetadataSwitch         = "switch"
)

var (
	_ core.NativeCheckpointProvider              = Provider{}
	_ core.NativeCheckpointLifecycleProvider     = Provider{}
	_ core.NativeCheckpointForkProvider          = Provider{}
	_ core.NativeCheckpointRestoreProvider       = Provider{}
	_ core.NativeCheckpointForkLifecycleProvider = (*backend)(nil)
)

type checkpointVM struct {
	Name  string `json:"Name"`
	ID    string `json:"ID"`
	State int    `json:"State"`
}

type checkpointCreateOutput struct {
	SourceVMID     string `json:"SourceVMID"`
	SnapshotID     string `json:"SnapshotID"`
	SnapshotName   string `json:"SnapshotName"`
	ExportedConfig string `json:"ExportedConfig"`
}

type checkpointLiveStatus struct {
	State string `json:"State"`
}

type checkpointImportOutput struct {
	ID    string   `json:"ID"`
	Disks []string `json:"Disks"`
}

func checkpointTarget(metadata map[string]string) (string, error) {
	target := strings.TrimSpace(metadata[checkpointMetadataTarget])
	if target != "" && target != core.TargetWindows {
		return "", core.Exit(2, "Hyper-V checkpoint metadata has unsupported target %q", target)
	}
	return core.TargetWindows, nil
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	target := shared.FirstNonBlank(req.Target.TargetOS, req.Config.TargetOS)
	if target != "" && target != core.TargetWindows {
		return core.NativeCheckpointCapability{}, false
	}
	switch strings.TrimSpace(req.Strategy) {
	case "", "auto", "disk-snapshot":
		return core.NativeCheckpointCapability{Kind: hypervCheckpointKind, Direct: true, PreferredForAuto: true}, true
	default:
		return core.NativeCheckpointCapability{
			Direct:            true,
			CreateUnsupported: "Hyper-V supports production checkpoints only; use --strategy disk-snapshot",
		}, true
	}
}

func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if override := strings.TrimSpace(req.Override); override != "" {
		return override
	}
	cfg := req.Config
	if workRoot := strings.TrimSpace(req.Server.Labels["work_root"]); workRoot != "" {
		cfg.WorkRoot = workRoot
	} else if workRoot := strings.TrimSpace(cfg.HyperV.WorkRoot); workRoot != "" {
		cfg.WorkRoot = workRoot
	}
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}

func (p Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	b, err := checkpointBackend(p, req.Config, req.Runtime)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.LeaseID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	if req.Capture == nil {
		resolved, resolvedExists, resolvedSet := core.ServerLeaseClaimSnapshot(req.Server)
		if !exists || resolvedSet && (!resolvedExists || !sameLeaseOwnership(resolved, claim)) {
			return core.NativeCheckpointCreateResult{}, core.NativeCheckpointNotSubmittedError{Cause: core.Exit(4, "Hyper-V checkpoint source lease %s was reassigned after it was resolved", req.LeaseID)}
		}
	} else if err := core.ValidateCheckpointCaptureClaim(claim, req.CheckpointID, req.Capture); err != nil {
		return core.NativeCheckpointCreateResult{}, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	var result core.NativeCheckpointCreateResult
	err = runUnderClaimFence(req.LeaseID, claim, func() error {
		var createErr error
		result, createErr = b.createNativeCheckpoint(ctx, req)
		return createErr
	})
	return result, err
}

// runUnderClaimFence runs action while the lease claim still equals expected.
// When the fence rejects the claim, action never ran, so the failure attests
// non-submission and lets the caller drop its native checkpoint reservation.
func runUnderClaimFence(leaseID string, expected core.LeaseClaim, action func() error) error {
	ran := false
	err := core.WithLeaseClaimUnchanged(leaseID, expected, func() error {
		ran = true
		return action()
	})
	if err != nil && !ran {
		return core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	return err
}

// sameLeaseOwnership compares the identity of the repository binding, ignoring
// volatile bookkeeping such as heartbeat timestamps.
func sameLeaseOwnership(a, b core.LeaseClaim) bool {
	return a.LeaseID == b.LeaseID && a.Slug == b.Slug && a.Provider == b.Provider &&
		a.ProviderScope == b.ProviderScope && a.RepoRoot == b.RepoRoot
}

func (p Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	if req.Metadata[checkpointMetadataSnapshotID] == "" {
		if _, err := validateCheckpointCleanupArtifact(req.ArtifactDir, req.Metadata); err != nil {
			return core.NativeCheckpointVerifyResult{}, err
		}
		return core.NativeCheckpointVerifyResult{
			ProviderState: "unknown", NextAction: "inspect_retained_capture",
			Error: "Hyper-V checkpoint submission has no final export identity; retain its cleanup journal",
		}, nil
	}
	paths, err := validateCheckpointArtifact(req.ArtifactDir, req.Metadata)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	if info, statErr := os.Stat(paths.config); statErr != nil {
		if os.IsNotExist(statErr) {
			return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
		}
		return core.NativeCheckpointVerifyResult{}, core.Exit(2, "stat exported Hyper-V checkpoint configuration: %v", statErr)
	} else if info.IsDir() {
		return core.NativeCheckpointVerifyResult{}, core.Exit(2, "exported Hyper-V checkpoint configuration is a directory: %s", paths.config)
	}

	result := core.NativeCheckpointVerifyResult{
		ProviderState: "available_exported",
		NextAction:    "fork_or_delete",
	}
	b, backendErr := checkpointBackend(p, req.Config, req.Runtime)
	if backendErr != nil {
		result.Error = backendErr.Error()
		return result, nil
	}
	status, liveErr := b.queryLiveCheckpoint(ctx, req.Metadata)
	if liveErr != nil {
		result.Error = liveErr.Error()
		return result, nil
	}
	if status == "live" {
		result.ProviderState = "available_live"
		result.NextAction = "fork_restore_or_delete"
	}
	return result, nil
}

func (p Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	if req.Metadata[checkpointMetadataSnapshotID] == "" {
		paths, err := validateCheckpointCleanupArtifact(req.ArtifactDir, req.Metadata)
		if err != nil {
			return err
		}
		b, err := checkpointBackend(p, req.Config, req.Runtime)
		if err != nil {
			return err
		}
		if err := b.removeLiveCheckpoint(ctx, req.Metadata[checkpointMetadataSourceVM], req.Metadata[checkpointMetadataSourceVMID], "", req.Metadata[checkpointMetadataSnapshotName]); err != nil {
			return err
		}
		return removeCheckpointExport(paths)
	}
	paths, err := validateCheckpointArtifact(req.ArtifactDir, req.Metadata)
	if err != nil {
		return err
	}
	b, err := checkpointBackend(p, req.Config, req.Runtime)
	if err != nil {
		return err
	}
	if err := b.deleteLiveCheckpoint(ctx, req.Metadata); err != nil {
		return err
	}
	return removeCheckpointExport(paths)
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Config == nil {
		return core.Exit(2, "Hyper-V checkpoint fork requires provider configuration")
	}
	if req.Record.Kind != hypervCheckpointKind {
		return core.Exit(2, "Hyper-V cannot fork checkpoint kind %q", req.Record.Kind)
	}
	if _, err := validateCheckpointArtifact(req.Record.ArtifactDir, req.Record.Metadata); err != nil {
		return err
	}
	req.Config.Provider = providerName
	target, err := checkpointTarget(req.Record.Metadata)
	if err != nil {
		return err
	}
	req.Config.TargetOS = target
	req.Config.WindowsMode = core.WindowsModeNormal
	if value := strings.TrimSpace(req.Record.Metadata[checkpointMetadataSSHUser]); value != "" {
		req.Config.HyperV.User = value
		req.Config.SSHUser = value
	}
	if value := strings.TrimSpace(req.Record.Metadata[checkpointMetadataWorkRoot]); value != "" {
		req.Config.HyperV.WorkRoot = value
		req.Config.WorkRoot = value
	}
	if value := strings.TrimSpace(req.Record.Metadata[checkpointMetadataSwitch]); value != "" {
		req.Config.HyperV.Switch = value
	}
	return nil
}

// ApplyNativeCheckpointForkFlags lets an explicit --hyperv-switch win over the
// switch recorded in the checkpoint metadata.
func (Provider) ApplyNativeCheckpointForkFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	flags, _ := values.(core.HyperVConfigFlagValues)
	if core.FlagWasSet(fs, "hyperv-switch") && flags.Switch != nil {
		if value := strings.TrimSpace(*flags.Switch); value != "" {
			cfg.HyperV.Switch = value
		}
	}
	return nil
}

func (p Provider) RestoreNativeCheckpoint(ctx context.Context, req core.NativeCheckpointRestoreRequest) (core.NativeCheckpointRestoreResult, error) {
	b, err := checkpointBackend(p, req.Config, req.Runtime)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	lease, err := b.restoreNativeCheckpoint(ctx, req)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	return core.NativeCheckpointRestoreResult{Lease: lease}, nil
}

func checkpointBackend(p Provider, cfg core.Config, rt core.Runtime) (*backend, error) {
	if rt.Exec == nil {
		return nil, core.Exit(2, "provider=%s checkpoint operation requires a local command runner", providerName)
	}
	if rt.Stdout == nil {
		rt.Stdout = io.Discard
	}
	if rt.Stderr == nil {
		rt.Stderr = io.Discard
	}
	return newBackend(p.Spec(), cfg, rt).(*backend), nil
}

type checkpointArtifactPaths struct {
	artifactDir string
	exportRoot  string
	config      string
}

func validateCheckpointCleanupArtifact(artifactDir string, metadata map[string]string) (checkpointArtifactPaths, error) {
	trusted, err := filepath.Abs(artifactDir)
	if err != nil || strings.TrimSpace(artifactDir) == "" {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint cleanup requires a trusted artifact directory")
	}
	recorded, err := filepath.Abs(metadata[checkpointMetadataArtifactDir])
	if err != nil || !strings.EqualFold(filepath.Clean(trusted), filepath.Clean(recorded)) {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint artifact ownership mismatch")
	}
	root := filepath.Join(trusted, "hyperv")
	if !strings.EqualFold(filepath.Clean(root), filepath.Clean(metadata[checkpointMetadataExportRoot])) ||
		!validHyperVVMName(metadata[checkpointMetadataSourceVM]) ||
		strings.TrimSpace(metadata[checkpointMetadataSourceVMID]) == "" ||
		strings.TrimSpace(metadata[checkpointMetadataSnapshotName]) == "" {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint cleanup metadata is missing exact resource ownership")
	}
	for _, path := range []string{trusted, root} {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return checkpointArtifactPaths{}, core.Exit(2, "refusing symlink Hyper-V checkpoint artifact %q", path)
		} else if err != nil && !os.IsNotExist(err) {
			return checkpointArtifactPaths{}, err
		}
	}
	return checkpointArtifactPaths{artifactDir: trusted, exportRoot: root}, nil
}

// grantExportRootSystemScript adds SYSTEM (the VMMS identity that performs
// Export-VMSnapshot) to the private export directory while keeping inheritance
// protected and the existing owner-only entries.
func grantExportRootSystemScript(exportRoot string) string {
	return fmt.Sprintf(
		`$exportDir=[IO.DirectoryInfo]::new('%s'); $exportAcl=$exportDir.GetAccessControl([Security.AccessControl.AccessControlSections]::Access); `+
			`$exportAcl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new('S-1-5-18'),'FullControl','ContainerInherit,ObjectInherit','None','Allow')); `+
			`$exportDir.SetAccessControl($exportAcl); `,
		escapePSString(exportRoot),
	)
}

func checkpointArtifactForCreate(artifactDir string) (checkpointArtifactPaths, error) {
	artifactDir = strings.TrimSpace(artifactDir)
	if artifactDir == "" {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint create requires a reserved artifact directory")
	}
	absolute, err := filepath.Abs(artifactDir)
	if err != nil {
		return checkpointArtifactPaths{}, core.Exit(2, "resolve Hyper-V checkpoint artifact directory: %v", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return checkpointArtifactPaths{}, core.Exit(2, "stat Hyper-V checkpoint artifact directory: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint artifact directory must be an owned directory: %s", absolute)
	}
	exportRoot := filepath.Join(absolute, "hyperv")
	if _, err := os.Lstat(exportRoot); err == nil {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint export directory already exists: %s", exportRoot)
	} else if !os.IsNotExist(err) {
		return checkpointArtifactPaths{}, core.Exit(2, "stat Hyper-V checkpoint export directory: %v", err)
	}
	return checkpointArtifactPaths{artifactDir: absolute, exportRoot: exportRoot}, nil
}

func validateCheckpointArtifact(artifactDir string, metadata map[string]string) (checkpointArtifactPaths, error) {
	trustedDir := strings.TrimSpace(artifactDir)
	recordedDir := strings.TrimSpace(metadata[checkpointMetadataArtifactDir])
	exportRoot := strings.TrimSpace(metadata[checkpointMetadataExportRoot])
	config := strings.TrimSpace(metadata[checkpointMetadataExportConfig])
	if trustedDir == "" || recordedDir == "" || exportRoot == "" || config == "" {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint metadata is missing owned artifact paths")
	}
	trustedAbs, err := filepath.Abs(trustedDir)
	if err != nil {
		return checkpointArtifactPaths{}, core.Exit(2, "resolve trusted checkpoint artifact directory: %v", err)
	}
	recordedAbs, err := filepath.Abs(recordedDir)
	if err != nil {
		return checkpointArtifactPaths{}, core.Exit(2, "resolve recorded checkpoint artifact directory: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(trustedAbs), filepath.Clean(recordedAbs)) {
		return checkpointArtifactPaths{}, core.Exit(2, "Hyper-V checkpoint artifact ownership mismatch")
	}
	expectedRoot := filepath.Join(trustedAbs, "hyperv")
	exportAbs, err := filepath.Abs(exportRoot)
	if err != nil {
		return checkpointArtifactPaths{}, core.Exit(2, "resolve Hyper-V checkpoint export directory: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(expectedRoot), filepath.Clean(exportAbs)) {
		return checkpointArtifactPaths{}, core.Exit(2, "refusing unowned Hyper-V checkpoint export directory %q", exportRoot)
	}
	configAbs, err := filepath.Abs(config)
	if err != nil {
		return checkpointArtifactPaths{}, core.Exit(2, "resolve exported Hyper-V configuration: %v", err)
	}
	if !pathWithin(exportAbs, configAbs) || !strings.EqualFold(filepath.Ext(configAbs), ".vmcx") {
		return checkpointArtifactPaths{}, core.Exit(2, "refusing unowned exported Hyper-V configuration %q", config)
	}
	if info, err := os.Lstat(trustedAbs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return checkpointArtifactPaths{}, core.Exit(2, "refusing symlink checkpoint artifact directory %q", trustedAbs)
	}
	if info, err := os.Lstat(exportAbs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return checkpointArtifactPaths{}, core.Exit(2, "refusing symlink Hyper-V checkpoint export directory %q", exportAbs)
	}
	return checkpointArtifactPaths{artifactDir: trustedAbs, exportRoot: exportAbs, config: configAbs}, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func removeCheckpointExport(paths checkpointArtifactPaths) error {
	if !strings.EqualFold(filepath.Clean(paths.exportRoot), filepath.Join(filepath.Clean(paths.artifactDir), "hyperv")) {
		return core.Exit(2, "refusing to remove unowned Hyper-V checkpoint export %q", paths.exportRoot)
	}
	if err := os.RemoveAll(paths.exportRoot); err != nil {
		return core.Exit(2, "remove Hyper-V checkpoint export %s: %v", paths.exportRoot, err)
	}
	return nil
}

func (b *backend) createNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (result core.NativeCheckpointCreateResult, err error) {
	if hypervHostOS != "windows" {
		return result, core.Exit(2, "provider=%s checkpoints require a Windows host with Hyper-V enabled", providerName)
	}
	sourceName := strings.TrimSpace(shared.FirstNonBlank(req.Server.CloudID, req.Server.Labels["instance"]))
	if sourceName == "" || !validHyperVVMName(sourceName) {
		return result, core.Exit(2, "Hyper-V checkpoint source must be an owned Crabbox VM")
	}
	if err := requireExactHyperVClaimFor(req.LeaseID, sourceName, "checkpoint"); err != nil {
		return result, err
	}
	// Nothing is mutated and no journal exists until the pending record is persisted,
	// so these failures attest non-submission and let the CLI resolve its reservation.
	source, err := b.queryCheckpointVM(ctx, sourceName)
	if err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	if source.State == hypervMissingState || source.ID == "" {
		return result, core.NativeCheckpointNotSubmittedError{Cause: core.Exit(4, "Hyper-V checkpoint source VM %s no longer exists", sourceName)}
	}
	_, ok, err := core.ResolveLeaseClaimForProvider(req.LeaseID, providerName)
	if err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	if !ok {
		return result, core.NativeCheckpointNotSubmittedError{Cause: core.Exit(4, "Hyper-V checkpoint source lease %s has no local claim", req.LeaseID)}
	}
	paths, err := checkpointArtifactForCreate(req.ArtifactDir)
	if err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	checkpointName, err := newHyperVCheckpointName(req.Name, req.LeaseID)
	if err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	if req.Persist != nil {
		// The deterministic snapshot name is the pending record's delete identity, so
		// `checkpoint delete` can run the idempotent cleanup if export cleanup failed
		// and the record would otherwise stay unresolved and block source release.
		pending := core.NativeCheckpointCreateResult{
			ArtifactDir: paths.artifactDir,
			Image: core.NativeCheckpointImage{
				ID: checkpointName, Name: checkpointName, State: "creating", Provider: providerName,
				Kind: hypervCheckpointKind, Direct: true,
			},
			Metadata: map[string]string{
				checkpointMetadataArtifactDir:    paths.artifactDir,
				checkpointMetadataExportRoot:     paths.exportRoot,
				checkpointMetadataSourceLease:    req.LeaseID,
				checkpointMetadataSourceVM:       sourceName,
				checkpointMetadataSourceVMID:     source.ID,
				checkpointMetadataSnapshotName:   checkpointName,
				checkpointMetadataProviderScope:  instanceScope(sourceName),
				checkpointMetadataCheckpointType: "ProductionOnly",
			},
		}
		if err := req.Persist(pending); err != nil {
			return result, core.NativeCheckpointNotSubmittedError{Cause: err}
		}
	}
	target := shared.FirstNonBlank(req.Target.TargetOS, req.Server.Labels["target"], req.Config.TargetOS)
	if target == "" {
		target = core.TargetWindows
	}
	if target != core.TargetWindows {
		return result, core.Exit(2, "Hyper-V checkpoints require target=windows")
	}
	cfg := b.configForRun()
	cfg.TargetOS = target
	if mode := strings.TrimSpace(shared.FirstNonBlank(req.Target.WindowsMode, req.Server.Labels["windows_mode"])); mode != "" {
		cfg.WindowsMode = mode
	}
	if user := strings.TrimSpace(req.Server.Labels["ssh_user"]); user != "" {
		cfg.HyperV.User = user
		cfg.SSHUser = user
	}
	if workRoot := strings.TrimSpace(req.Server.Labels["work_root"]); workRoot != "" {
		cfg.HyperV.WorkRoot = workRoot
		cfg.WorkRoot = workRoot
	}
	applyStoredLeaseKey(&cfg, req.LeaseID)
	if err := os.Mkdir(paths.exportRoot, 0o700); err != nil {
		return result, core.Exit(2, "create Hyper-V checkpoint export directory: %v", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), hypervCheckpointCleanupTimeout)
		defer cancel()
		cleanupErr := errors.Join(
			b.removeLiveCheckpoint(cleanupCtx, sourceName, source.ID, "", checkpointName),
			removeCheckpointExport(paths),
		)
		if cleanupErr == nil && err != nil {
			// Snapshot and export are verifiably gone, so no checkpoint was
			// produced; report it as not submitted so the record resolves.
			var unsubmitted core.NativeCheckpointNotSubmittedError
			if !errors.As(err, &unsubmitted) {
				err = core.NativeCheckpointNotSubmittedError{Cause: err}
			}
		}
		if err != nil {
			// The published result must not advertise the rolled-back export as usable.
			if cleanupErr == nil {
				result = core.NativeCheckpointCreateResult{}
			} else if result.Image.ID != "" {
				result.Image.State = "creating"
			}
		}
		if cleanupErr != nil {
			// Unverified rollback must not attest non-submission: the pending record
			// is the delete identity for the leftovers.
			var unsubmitted core.NativeCheckpointNotSubmittedError
			if errors.As(err, &unsubmitted) {
				err = unsubmitted.Cause
			}
		}
		err = errors.Join(err, cleanupErr)
	}()

	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$vm=Get-VM -Name '%s' -ErrorAction Stop; `+
			`if ($vm.Id.Guid -ne '%s') { throw 'source VM identity changed' }; `+
			`Set-VM -VM $vm -CheckpointType ProductionOnly -ErrorAction Stop; `+
			`$snapshot=$null; `+
			`try { `+
			`$snapshot=Checkpoint-VM -VM $vm -SnapshotName '%s' -Passthru -ErrorAction Stop; `+
			`Export-VMSnapshot -VMSnapshot $snapshot -Path '%s' -ErrorAction Stop; `+
			`$configs=@(Get-ChildItem -LiteralPath '%s' -Filter '*.vmcx' -File -Recurse | Where-Object { $_.Directory.Name -eq 'Virtual Machines' }); `+
			`if ($configs.Count -ne 1) { throw "Export-VMSnapshot produced $($configs.Count) importable .vmcx configurations; expected exactly one" }; `+
			`$config=$configs[0]; `+
			`[pscustomobject]@{SourceVMID=$vm.Id.Guid;SnapshotID=$snapshot.Id.Guid;SnapshotName=$snapshot.Name;ExportedConfig=$config.FullName} | ConvertTo-Json -Compress `+
			`} catch { if ($snapshot) { Remove-VMSnapshot -VMSnapshot $snapshot -Confirm:$false -ErrorAction SilentlyContinue }; throw }`,
		escapePSString(sourceName),
		escapePSString(source.ID),
		escapePSString(checkpointName),
		escapePSString(paths.exportRoot),
		escapePSString(paths.exportRoot),
	)
	// The VMMS service writes the export as SYSTEM; the owner-only private DACL denies it.
	script = grantExportRootSystemScript(paths.exportRoot) + script
	commandResult, runErr := b.powershellLong(ctx, script)
	if runErr != nil {
		return result, shared.LocalCommandError("create production Hyper-V checkpoint", commandResult, runErr)
	}
	var output checkpointCreateOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(commandResult.Stdout)), &output); err != nil {
		return result, core.Exit(2, "parse Hyper-V checkpoint export result: %v", err)
	}
	output.ExportedConfig = filepath.Clean(output.ExportedConfig)
	if !strings.EqualFold(output.SourceVMID, source.ID) || output.SnapshotID == "" || output.SnapshotName != checkpointName {
		return result, core.Exit(2, "Hyper-V checkpoint export returned inconsistent source or snapshot identity")
	}
	if !pathWithin(paths.exportRoot, output.ExportedConfig) ||
		!strings.EqualFold(filepath.Ext(output.ExportedConfig), ".vmcx") ||
		!strings.EqualFold(filepath.Base(filepath.Dir(output.ExportedConfig)), "Virtual Machines") {
		return result, core.Exit(2, "Hyper-V checkpoint export returned unowned configuration %q", output.ExportedConfig)
	}
	if _, err := os.Stat(output.ExportedConfig); err != nil {
		return result, core.Exit(2, "stat exported Hyper-V checkpoint configuration: %v", err)
	}
	paths.config = output.ExportedConfig
	metadata := map[string]string{
		checkpointMetadataArtifactDir:    paths.artifactDir,
		checkpointMetadataExportRoot:     paths.exportRoot,
		checkpointMetadataExportConfig:   paths.config,
		checkpointMetadataSourceLease:    req.LeaseID,
		checkpointMetadataSourceVM:       sourceName,
		checkpointMetadataSourceVMID:     source.ID,
		checkpointMetadataSnapshotID:     output.SnapshotID,
		checkpointMetadataSnapshotName:   output.SnapshotName,
		checkpointMetadataCheckpointType: "ProductionOnly",
		checkpointMetadataTarget:         target,
		checkpointMetadataProviderScope:  instanceScope(sourceName),
		checkpointMetadataSSHUser:        shared.FirstNonBlank(req.Server.Labels["ssh_user"], req.Config.HyperV.User),
		checkpointMetadataWorkRoot:       shared.FirstNonBlank(req.Server.Labels["work_root"], req.Config.HyperV.WorkRoot),
		checkpointMetadataSwitch:         req.Config.HyperV.Switch,
	}
	result = core.NativeCheckpointCreateResult{
		ArtifactDir: paths.artifactDir,
		Image: core.NativeCheckpointImage{
			ID:         output.SnapshotID,
			Name:       output.SnapshotName,
			State:      "available",
			Provider:   providerName,
			Kind:       hypervCheckpointKind,
			ResourceID: output.ExportedConfig,
			Direct:     true,
		},
		Metadata: metadata,
	}
	if req.Persist != nil {
		if err := req.Persist(result); err != nil {
			return result, err
		}
	}
	committed = true
	return result, nil
}

func (b *backend) queryCheckpointVM(ctx context.Context, name string) (checkpointVM, error) {
	script := fmt.Sprintf(
		`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' } | Select-Object Name,@{Name='ID';Expression={$_.Id.Guid}},State | ConvertTo-Json -Compress`,
		escapePSString(name),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return checkpointVM{}, shared.LocalCommandError("Get-VM checkpoint query", result, err)
	}
	stdout := strings.TrimSpace(result.Stdout)
	if stdout == "" || stdout == "null" {
		return checkpointVM{Name: name, State: hypervMissingState}, nil
	}
	var vm checkpointVM
	if err := json.Unmarshal([]byte(stdout), &vm); err != nil {
		return checkpointVM{}, core.Exit(2, "parse Hyper-V checkpoint VM %s: %v", name, err)
	}
	return vm, nil
}

func (b *backend) queryLiveCheckpoint(ctx context.Context, metadata map[string]string) (string, error) {
	sourceName, sourceID, snapshotID, err := checkpointSourceMetadata(metadata)
	if err != nil {
		return "", err
	}
	script := fmt.Sprintf(
		`$vm=Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' } | Select-Object -First 1; `+
			`if (-not $vm) { [pscustomobject]@{State='source_missing'} | ConvertTo-Json -Compress; return }; `+
			`if ($vm.Id.Guid -ne '%s') { [pscustomobject]@{State='identity_changed'} | ConvertTo-Json -Compress; return }; `+
			`$snapshot=Get-VMSnapshot -VM $vm -ErrorAction Stop | Where-Object { $_.Id.Guid -eq '%s' } | Select-Object -First 1; `+
			`[pscustomobject]@{State=$(if ($snapshot) {'live'} else {'snapshot_missing'})} | ConvertTo-Json -Compress`,
		escapePSString(sourceName),
		escapePSString(sourceID),
		escapePSString(snapshotID),
	)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		return "", shared.LocalCommandError("verify live Hyper-V checkpoint", result, runErr)
	}
	var status checkpointLiveStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &status); err != nil {
		return "", core.Exit(2, "parse live Hyper-V checkpoint status: %v", err)
	}
	return status.State, nil
}

func (b *backend) deleteLiveCheckpoint(ctx context.Context, metadata map[string]string) error {
	sourceName, sourceID, snapshotID, err := checkpointSourceMetadata(metadata)
	if err != nil {
		return err
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$vm=Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' } | Select-Object -First 1; `+
			`if (-not $vm -or $vm.Id.Guid -ne '%s') { return }; `+
			`$snapshot=Get-VMSnapshot -VM $vm -ErrorAction Stop | Where-Object { $_.Id.Guid -eq '%s' } | Select-Object -First 1; `+
			`if ($snapshot) { Remove-VMSnapshot -VMSnapshot $snapshot -Confirm:$false -ErrorAction Stop }`,
		escapePSString(sourceName),
		escapePSString(sourceID),
		escapePSString(snapshotID),
	)
	result, runErr := b.powershell(ctx, script)
	if runErr != nil {
		return shared.LocalCommandError("delete live Hyper-V checkpoint", result, runErr)
	}
	return nil
}

func (b *backend) removeLiveCheckpoint(ctx context.Context, sourceName, sourceID, snapshotID, snapshotName string) error {
	if sourceName == "" || sourceID == "" || snapshotID == "" && snapshotName == "" {
		return nil
	}
	selector := fmt.Sprintf(`$_.Id.Guid -eq '%s'`, escapePSString(snapshotID))
	if snapshotID == "" {
		selector = fmt.Sprintf(`$_.Name -eq '%s'`, escapePSString(snapshotName))
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; $vm=Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' }; `+
			`if (-not $vm -or $vm.Id.Guid -ne '%s') { return }; `+
			`Get-VMSnapshot -VM $vm -ErrorAction Stop | Where-Object { %s } | Remove-VMSnapshot -Confirm:$false -ErrorAction Stop`,
		escapePSString(sourceName),
		escapePSString(sourceID),
		selector,
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("remove retained Hyper-V checkpoint", result, err)
	}
	return nil
}

func checkpointSourceMetadata(metadata map[string]string) (string, string, string, error) {
	sourceName := strings.TrimSpace(metadata[checkpointMetadataSourceVM])
	sourceID := strings.TrimSpace(metadata[checkpointMetadataSourceVMID])
	snapshotID := strings.TrimSpace(metadata[checkpointMetadataSnapshotID])
	if sourceName == "" || sourceID == "" || snapshotID == "" {
		return "", "", "", core.Exit(2, "Hyper-V checkpoint metadata is missing source ownership")
	}
	if !validHyperVVMName(sourceName) {
		return "", "", "", core.Exit(2, "Hyper-V checkpoint metadata has invalid source VM %q", sourceName)
	}
	return sourceName, sourceID, snapshotID, nil
}

func newHyperVCheckpointName(requested, leaseID string) (string, error) {
	base := strings.TrimSpace(requested)
	if base == "" {
		base = "crabbox-" + strings.TrimPrefix(strings.TrimSpace(leaseID), "cbx_")
	}
	var safe strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			safe.WriteRune(r)
		} else {
			safe.WriteByte('-')
		}
	}
	trimmed := strings.Trim(safe.String(), "-.")
	if trimmed == "" {
		trimmed = "crabbox"
	}
	if len(trimmed) > 60 {
		trimmed = strings.Trim(trimmed[:60], "-.")
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", core.Exit(2, "generate Hyper-V checkpoint name: %v", err)
	}
	return trimmed + "-" + hex.EncodeToString(suffix[:]), nil
}

func (b *backend) restoreNativeCheckpoint(ctx context.Context, req core.NativeCheckpointRestoreRequest) (lease core.LeaseTarget, err error) {
	if req.Record.Kind != hypervCheckpointKind {
		return core.LeaseTarget{}, core.Exit(2, "Hyper-V cannot restore checkpoint kind %q", req.Record.Kind)
	}
	if _, err := validateCheckpointArtifact(req.Record.ArtifactDir, req.Record.Metadata); err != nil {
		return core.LeaseTarget{}, err
	}
	sourceName, sourceID, snapshotID, err := checkpointSourceMetadata(req.Record.Metadata)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	sourceLease := strings.TrimSpace(req.Record.Metadata[checkpointMetadataSourceLease])
	if sourceLease == "" || strings.TrimSpace(req.LeaseID) != sourceLease {
		return core.LeaseTarget{}, core.Exit(4, "Hyper-V restore requires the original source lease %s", sourceLease)
	}
	if err := requireExactHyperVClaimFor(sourceLease, sourceName, "restore"); err != nil {
		return core.LeaseTarget{}, err
	}
	previousClaim, claimExists, err := core.ReadLeaseClaimWithPresence(sourceLease)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !claimExists {
		return core.LeaseTarget{}, core.Exit(4, "Hyper-V source lease %s has no local claim", sourceLease)
	}
	cfg := b.configForRun()
	target, err := checkpointTarget(req.Record.Metadata)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.TargetOS = target
	if target == core.TargetWindows {
		cfg.WindowsMode = core.WindowsModeNormal
	} else {
		cfg.WindowsMode = ""
	}
	if value := strings.TrimSpace(req.Record.Metadata[checkpointMetadataSSHUser]); value != "" {
		cfg.HyperV.User = value
		cfg.SSHUser = value
	}
	if value := strings.TrimSpace(req.Record.Metadata[checkpointMetadataWorkRoot]); value != "" {
		cfg.HyperV.WorkRoot = value
		cfg.WorkRoot = value
	}
	applyStoredLeaseKey(&cfg, sourceLease)
	reservationServer := b.serverFromInstance(hypervVM{Name: sourceName, State: 2}, previousClaim, cfg)
	reservationTarget := core.SSHTargetFromConfig(cfg, previousClaim.SSHHost)
	if previousClaim.SSHPort > 0 {
		reservationTarget.Port = fmt.Sprintf("%d", previousClaim.SSHPort)
	}
	reservedClaim, err := core.ClaimLeaseForRepoProviderScopePondEndpointReservationIfUnchanged(
		sourceLease,
		previousClaim.Slug,
		providerName,
		instanceScope(sourceName),
		cfg.Pond,
		req.Repo.Root,
		cfg.IdleTimeout,
		req.Reclaim,
		reservationServer,
		reservationTarget,
		hypervCheckpointRestoreReservationLabel,
		hypervCheckpointRestoreReservationWindow,
		previousClaim,
		claimExists,
	)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	rollbackClaim := true
	defer func() {
		if !rollbackClaim {
			return
		}
		if restoreErr := core.RestoreLeaseClaimIfUnchanged(sourceLease, reservedClaim, previousClaim, claimExists); restoreErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: restore Hyper-V lease claim %s after checkpoint restore failure: %v\n", sourceLease, restoreErr)
		}
	}()
	claim := reservedClaim
	// The reservation transaction releases the claim lock, so every effect
	// below re-checks that this exact reservation still owns the lease.
	verifyClaim := func(c context.Context) error {
		return core.WithLeaseClaimUnchangedContext(c, sourceLease, reservedClaim, nil)
	}
	fenced := func(c context.Context, action func() error) error {
		return core.WithLeaseClaimUnchangedContext(c, sourceLease, reservedClaim, action)
	}
	if err := verifyClaim(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	prepareErr := fenced(ctx, func() error {
		vm, queryErr := b.queryCheckpointVM(ctx, sourceName)
		if queryErr != nil {
			return queryErr
		}
		if vm.State == hypervMissingState || !strings.EqualFold(vm.ID, sourceID) {
			return core.Exit(4, "Hyper-V source VM identity no longer matches checkpoint %s", req.Record.ImageID)
		}
		return nil
	})
	if prepareErr != nil {
		return core.LeaseTarget{}, prepareErr
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$vm=Get-VM -Name '%s' -ErrorAction Stop; `+
			`if ($vm.Id.Guid -ne '%s') { throw 'source VM identity changed' }; `+
			`$snapshot=Get-VMSnapshot -VM $vm -ErrorAction SilentlyContinue | Where-Object { $_.Id.Guid -eq '%s' } | Select-Object -First 1; `+
			`if (-not $snapshot) { throw 'source production checkpoint is missing' }; `+
			`Restore-VMSnapshot -VMSnapshot $snapshot -Confirm:$false -ErrorAction Stop`,
		escapePSString(sourceName),
		escapePSString(sourceID),
		escapePSString(snapshotID),
	)
	if err := ctx.Err(); err != nil {
		return core.LeaseTarget{}, err
	}
	var commandResult core.LocalCommandResult
	var runErr error
	if fenceErr := fenced(ctx, func() error {
		commandResult, runErr = b.powershellLong(ctx, script)
		return nil
	}); fenceErr != nil {
		return core.LeaseTarget{}, fenceErr
	}
	if runErr != nil {
		if ctx.Err() != nil {
			// A canceled host command may have reached Hyper-V before its process
			// was terminated, so ownership must stay with the requesting repo.
			rollbackClaim = false
		}
		return core.LeaseTarget{}, shared.LocalCommandError("restore Hyper-V production checkpoint", commandResult, runErr)
	}
	rollbackClaim = false
	startScript := fmt.Sprintf(
		`$vm=Get-VM -Name '%s' -ErrorAction Stop; `+
			`if ($vm.Id.Guid -ne '%s') { throw 'source VM identity changed after restore' }; `+
			`if ($vm.State -ne 'Running') { Start-VM -VM $vm -ErrorAction Stop }`,
		escapePSString(sourceName),
		escapePSString(sourceID),
	)
	var startResult core.LocalCommandResult
	var startErr error
	if fenceErr := fenced(ctx, func() error {
		startResult, startErr = b.powershellLong(ctx, startScript)
		return nil
	}); fenceErr != nil {
		return core.LeaseTarget{}, fenceErr
	}
	if startErr != nil {
		return core.LeaseTarget{}, shared.LocalCommandError("start restored Hyper-V VM", startResult, startErr)
	}
	ip, err := b.waitForIP(ctx, sourceName, 5*time.Minute)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	// prepareLease authenticates to the guest and must not run on stale authority;
	// holding the claim lock across guest readiness would starve heartbeats.
	if err := verifyClaim(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err = b.prepareLease(ctx, cfg, hypervVM{Name: sourceName, State: 2}, ip, claim, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if _, err := core.ClaimLeaseForRepoProviderScopePondEndpointIfUnchanged(sourceLease, claim.Slug, providerName, instanceScope(sourceName), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH, reservedClaim, true); err != nil {
		return core.LeaseTarget{}, err
	}
	rollbackClaim = false
	return lease, nil
}

func (b *backend) ForkNativeCheckpoint(ctx context.Context, req core.NativeCheckpointForkLifecycleRequest) (lease core.LeaseTarget, err error) {
	if req.Record.Kind != hypervCheckpointKind {
		return lease, core.Exit(2, "Hyper-V cannot fork checkpoint kind %q", req.Record.Kind)
	}
	paths, err := validateCheckpointArtifact(req.Record.ArtifactDir, req.Record.Metadata)
	if err != nil {
		return lease, err
	}
	if _, err := os.Stat(paths.config); err != nil {
		return lease, core.Exit(2, "stat exported Hyper-V checkpoint configuration: %v", err)
	}
	if hypervHostOS != "windows" {
		return lease, core.Exit(2, "provider=%s checkpoint fork requires a Windows host with Hyper-V enabled", providerName)
	}
	cfg := b.configForRun()
	target, err := checkpointTarget(req.Record.Metadata)
	if err != nil {
		return lease, err
	}
	cfg.TargetOS = target
	cfg.WindowsMode = core.WindowsModeNormal
	if strings.TrimSpace(cfg.HyperV.GuestPassword) == "" {
		return lease, core.Exit(2, "provider=%s checkpoint fork requires CRABBOX_HYPERV_GUEST_PASSWORD or trusted hyperv.guestPassword", providerName)
	}
	if !validHyperVSSHUser(cfg.HyperV.User) {
		return lease, core.Exit(2, "provider=%s checkpoint fork requires a valid Hyper-V guest user", providerName)
	}
	if strings.TrimSpace(req.Repo.Root) == "" {
		return lease, core.Exit(2, "provider=%s checkpoint fork requires a repository root", providerName)
	}

	leaseID := core.NewLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return lease, err
	}
	claims, err := providerClaims()
	if err != nil {
		return lease, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return lease, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfigContext(ctx, cfg, leaseID)
	if err != nil {
		return lease, err
	}
	cfg.SSHKey = keyPath
	name := core.LeaseProviderName(leaseID, slug)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = paths.config
	labels["ssh_user"] = cfg.HyperV.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.HyperV.WorkRoot
	labels["state"] = "provisioning"
	labels["checkpoint"] = req.Record.ImageID
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	provisional := core.LeaseTarget{Server: b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg), LeaseID: leaseID}
	acquireReq := core.AcquireRequest{Repo: req.Repo, Keep: req.Keep, Reclaim: req.Reclaim, RequestedSlug: req.RequestedSlug}
	if err := persistLease(leaseID, slug, name, cfg, acquireReq, &provisional); err != nil {
		core.RemoveStoredTestboxKey(leaseID)
		return lease, fmt.Errorf("persist Hyper-V checkpoint fork before import: %w", err)
	}
	claim, _, _ = core.ServerLeaseClaimSnapshot(provisional.Server)
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupErr := b.rollbackImportedFork(leaseID, claim, name)
		err = errors.Join(err, cleanupErr)
	}()

	importedID, err := b.importCheckpointVM(ctx, paths.config, name, target == core.TargetWindows)
	if err != nil {
		return lease, err
	}
	if strings.EqualFold(importedID, strings.TrimSpace(req.Record.Metadata[checkpointMetadataSourceVMID])) {
		return lease, core.Exit(2, "Import-VM did not generate a fresh Hyper-V VM identity")
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(provisional); err != nil {
			return lease, err
		}
	}
	hostname := forkGuestHostname(leaseID)
	if err := b.waitGuestReady(ctx, name, cfg.HyperV.User); err != nil {
		return lease, fmt.Errorf("forked guest did not become reachable over PowerShell Direct: %w", err)
	}
	if err := b.rotateForkGuestIdentity(ctx, name, cfg.HyperV.User, publicKey, hostname); err != nil {
		return lease, err
	}
	if err := b.restartVM(ctx, name); err != nil {
		return lease, err
	}
	if err := b.waitGuestReady(ctx, name, cfg.HyperV.User); err != nil {
		return lease, fmt.Errorf("forked guest did not return after identity rotation: %w", err)
	}
	if err := b.rotateForkGuestSecrets(ctx, name, cfg.HyperV.User); err != nil {
		return lease, err
	}
	if err := b.injectSSHKey(ctx, name, cfg.HyperV.User, publicKey); err != nil {
		return lease, fmt.Errorf("reapply forked Hyper-V guest SSH identity after reboot: %w", err)
	}
	if err := b.connectVMNetwork(ctx, name, cfg.HyperV.Switch); err != nil {
		return lease, err
	}
	ip, err := b.waitForIP(ctx, name, 5*time.Minute)
	if err != nil {
		return lease, err
	}
	lease, err = b.prepareLease(ctx, cfg, hypervVM{Name: name, State: 2}, ip, claim, true)
	if err != nil {
		return lease, err
	}
	if err := persistLease(leaseID, slug, name, cfg, acquireReq, &lease); err != nil {
		return lease, err
	}
	committed = true
	return lease, nil
}

func (b *backend) importCheckpointVM(ctx context.Context, configPath, name string, start bool) (string, error) {
	vmPath := filepath.Join(hypervVMDir(), name)
	snapshotPath := filepath.Join(vmPath, "Snapshots")
	vhdPath := filepath.Join(hypervVHDDir(), name)
	for _, path := range []string{vmPath, snapshotPath, vhdPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", core.Exit(2, "create Hyper-V checkpoint fork directory %s: %v", path, err)
		}
	}
	if err := os.MkdirAll(vhdPath, 0o700); err != nil {
		return "", core.Exit(2, "create Hyper-V checkpoint fork directory %s: %v", vhdPath, err)
	}
	startScript := ""
	if start {
		startScript = `Start-VM -VM $vm -ErrorAction Stop; `
	}
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$vm=$null; `+
			`try { `+
			`$vm=Import-VM -Path '%s' -Copy -GenerateNewId -VirtualMachinePath '%s' -SnapshotFilePath '%s' -VhdDestinationPath '%s' -ErrorAction Stop; `+
			`Rename-VM -VM $vm -NewName '%s' -ErrorAction Stop; `+
			`$vm=Get-VM -Id $vm.Id; `+
			`Get-VMNetworkAdapter -VM $vm | Set-VMNetworkAdapter -DynamicMacAddress -ErrorAction Stop; `+
			`Get-VMNetworkAdapter -VM $vm | Disconnect-VMNetworkAdapter -ErrorAction Stop; `+
			`Set-VM -VM $vm -AutomaticCheckpointsEnabled $false -CheckpointType ProductionOnly -ErrorAction Stop; `+
			`%s`+
			`[pscustomobject]@{ID=$vm.Id.Guid; Disks=@(Get-VMHardDiskDrive -VM $vm | ForEach-Object { $_.Path })} | ConvertTo-Json -Compress `+
			`} catch { `+
			`if ($vm) { $disks=@(Get-VMHardDiskDrive -VM $vm -ErrorAction SilentlyContinue | ForEach-Object { $_.Path }); Stop-VM -VM $vm -Force -Confirm:$false -ErrorAction SilentlyContinue; Remove-VM -VM $vm -Force -Confirm:$false -ErrorAction SilentlyContinue; `+
			`$root=[IO.Path]::GetFullPath('%s').TrimEnd('\')+'\'; foreach ($d in $disks) { $full=[IO.Path]::GetFullPath($d); if ($full.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) { Remove-Item -LiteralPath $full -Force -ErrorAction SilentlyContinue } } }; `+
			`throw }`,
		escapePSString(configPath),
		escapePSString(vmPath),
		escapePSString(snapshotPath),
		escapePSString(vhdPath),
		escapePSString(name),
		startScript,
		escapePSString(vhdPath),
	)
	result, err := b.powershellLong(ctx, script)
	if err != nil {
		return "", shared.LocalCommandError("import Hyper-V checkpoint fork", result, err)
	}
	var output checkpointImportOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &output); err != nil {
		return "", core.Exit(2, "parse imported Hyper-V VM identity: %v", err)
	}
	if output.ID == "" {
		return "", core.Exit(2, "Import-VM returned an empty generated identity")
	}
	if err := recordForkDisks(vhdPath, output.Disks); err != nil {
		return "", err
	}
	return output.ID, nil
}

// recordForkDisks persists the exact disks Import-VM copied for a fork so that
// retiring the fork deletes only those files.
func recordForkDisks(vhdPath string, disks []string) error {
	var names []string
	for _, d := range disks {
		clean := filepath.Clean(d)
		if strings.EqualFold(filepath.Dir(clean), filepath.Clean(vhdPath)) {
			names = append(names, filepath.Base(clean))
		}
	}
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(vhdPath, forkDisksManifest), data, 0o600); err != nil {
		return core.Exit(2, "record Hyper-V checkpoint fork disks: %v", err)
	}
	return nil
}

// rotateForkGuestIdentity renames the guest before the identity reboot. Only
// the rename needs that reboot; Restart-VM is a hard reset that discards
// registry and file writes still in the guest's cache, so per-guest secrets
// are rotated afterwards by rotateForkGuestSecrets.
func (b *backend) rotateForkGuestIdentity(ctx context.Context, vmName, user, publicKey, hostname string) error {
	identityScript := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`if ($env:COMPUTERNAME -ne '%s') { Rename-Computer -NewName '%s' -Force }; `,
		escapePSString(hostname),
		escapePSString(hostname),
	)
	if err := b.invokeInGuest(ctx, vmName, user, identityScript+sshAccessScript(user, publicKey, true), "fork identity rotation"); err != nil {
		return fmt.Errorf("rotate forked Hyper-V guest identity: %w", err)
	}
	return nil
}

// forkGuestSecretsScript gives a fork its own MachineGuid and, when present, VNC
// password, and flushes both to disk before the fork is connected or stopped.
const forkGuestSecretsScript = `$ErrorActionPreference='Stop'; ` +
	`$crypto=[Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SOFTWARE\Microsoft\Cryptography',$true); ` +
	`if ($null -eq $crypto) { throw 'Cryptography registry key is missing' }; ` +
	`try { $crypto.SetValue('MachineGuid',[guid]::NewGuid().ToString(),[Microsoft.Win32.RegistryValueKind]::String); $crypto.Flush() } finally { $crypto.Dispose() }; ` +
	`$vncPasswordPath='C:\ProgramData\crabbox\vnc.password'; ` +
	`$tightVNC=[Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SOFTWARE\TightVNC\Server',$true); ` +
	`try { if ((Test-Path -LiteralPath $vncPasswordPath) -or $tightVNC) { ` +
	`$bytes=New-Object byte[] 18; $rng=[Security.Cryptography.RandomNumberGenerator]::Create(); try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }; ` +
	`$vncPassword=[Convert]::ToBase64String($bytes); New-Item -ItemType Directory -Force -Path (Split-Path $vncPasswordPath) | Out-Null; ` +
	`$passwordBytes=[Text.Encoding]::ASCII.GetBytes($vncPassword); ` +
	`$passwordFile=[IO.FileStream]::new($vncPasswordPath,[IO.FileMode]::Create,[IO.FileAccess]::Write,[IO.FileShare]::None,4096,[IO.FileOptions]::WriteThrough); ` +
	`try { $passwordFile.Write($passwordBytes,0,$passwordBytes.Length); $passwordFile.Flush($true) } finally { $passwordFile.Dispose() }; ` +
	`$plain=New-Object byte[] 8; [Array]::Copy($passwordBytes,$plain,[Math]::Min($plain.Length,$passwordBytes.Length)); ` +
	`$key=[byte[]](0xE8,0x4A,0xD6,0x60,0xC4,0x72,0x1A,0xE0); $des=[Security.Cryptography.DES]::Create(); ` +
	`try { $des.Mode=[Security.Cryptography.CipherMode]::ECB; $des.Padding=[Security.Cryptography.PaddingMode]::None; $des.Key=$key; $encryptor=$des.CreateEncryptor(); ` +
	`try { $encrypted=$encryptor.TransformFinalBlock($plain,0,$plain.Length) } finally { $encryptor.Dispose() } } finally { $des.Dispose() }; ` +
	`if ($null -eq $tightVNC) { $tightVNC=[Microsoft.Win32.Registry]::LocalMachine.CreateSubKey('SOFTWARE\TightVNC\Server') }; ` +
	`$tightVNC.SetValue('Password',$encrypted,[Microsoft.Win32.RegistryValueKind]::Binary); ` +
	`$tightVNC.SetValue('ControlPassword',$encrypted,[Microsoft.Win32.RegistryValueKind]::Binary); $tightVNC.Flush() } } ` +
	`finally { if ($tightVNC) { $tightVNC.Dispose() } }; `

func (b *backend) rotateForkGuestSecrets(ctx context.Context, vmName, user string) error {
	if err := b.invokeInGuest(ctx, vmName, user, forkGuestSecretsScript, "fork secret rotation"); err != nil {
		return fmt.Errorf("rotate forked Hyper-V guest secrets: %w", err)
	}
	return nil
}

func (b *backend) restartVM(ctx context.Context, name string) error {
	script := fmt.Sprintf(`Restart-VM -Name '%s' -Force -ErrorAction Stop`, escapePSString(name))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("restart forked Hyper-V VM", result, err)
	}
	return nil
}

func (b *backend) removeImportedCheckpointVM(ctx context.Context, name string) error {
	var errs []error
	if err := b.removeImportedCheckpointRegistration(ctx, name); err != nil {
		errs = append(errs, err)
	}
	if err := b.removeVM(ctx, name); err != nil {
		errs = append(errs, err)
	}
	if err := removeOwnedHyperVLeaseTree(filepath.Join(hypervVHDDir(), name), hypervVHDDir(), name); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// rollbackImportedFork removes the fork only while the lease claim still belongs to it;
// a reassigned claim keeps both the claim and the imported resources.
func (b *backend) rollbackImportedFork(leaseID string, original core.LeaseClaim, name string) error {
	current, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if exists && !sameLeaseOwnership(original, current) {
		return core.Exit(2, "retaining forked Hyper-V VM %s: lease %s was reassigned during fork", name, leaseID)
	}
	err = core.CleanupLeaseClaimIfUnchangedAfter(leaseID, current, exists, func() error {
		return b.removeImportedCheckpointVM(context.Background(), name)
	})
	if err == nil {
		core.RemoveStoredTestboxKey(leaseID)
	}
	return err
}

func (b *backend) removeImportedCheckpointRegistration(ctx context.Context, name string) error {
	vmRoot := filepath.Join(hypervVMDir(), name)
	vhdRoot := filepath.Join(hypervVHDDir(), name)
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$name='%s'; `+
			`$roots=@('%s','%s') | ForEach-Object { [IO.Path]::GetFullPath($_).TrimEnd('\') }; `+
			`function Test-CrabboxOwnedPath([string]$path) { `+
			`if ([string]::IsNullOrWhiteSpace($path)) { return $false }; `+
			`$full=[IO.Path]::GetFullPath($path).TrimEnd('\'); `+
			`foreach ($root in $roots) { if ($full.Equals($root,[StringComparison]::OrdinalIgnoreCase) -or $full.StartsWith($root+'\',[StringComparison]::OrdinalIgnoreCase)) { return $true } }; `+
			`return $false }; `+
			`function Get-CrabboxImportedVM { `+
			`@(Get-VM -ErrorAction Stop | Where-Object { `+
			`$candidate=$_; $owned=$candidate.Name -eq $name; `+
			`if (-not $owned) { $owned=(Test-CrabboxOwnedPath $candidate.ConfigurationLocation) -or (Test-CrabboxOwnedPath $candidate.SnapshotFileLocation) }; `+
			`if (-not $owned) { foreach ($disk in @(Get-VMHardDiskDrive -VM $candidate -ErrorAction Stop)) { if (Test-CrabboxOwnedPath $disk.Path) { $owned=$true; break } } }; `+
			`$owned }) }; `+
			`$disks=@(); foreach ($candidate in @(Get-CrabboxImportedVM)) { foreach ($disk in @(Get-VMHardDiskDrive -VM $candidate -ErrorAction Stop)) { if (Test-CrabboxOwnedPath $disk.Path) { $disks += $disk.Path } }; Stop-VM -VM $candidate -Force -Confirm:$false -ErrorAction SilentlyContinue; Remove-VM -VM $candidate -Force -Confirm:$false -ErrorAction Stop }; `+
			`foreach ($path in $disks) { if (Test-Path -LiteralPath $path -PathType Leaf) { Remove-Item -LiteralPath $path -Force -ErrorAction Stop } }; `+
			`$remaining=@(Get-CrabboxImportedVM); `+
			`if ($remaining.Count -ne 0) { throw "partial checkpoint import cleanup left $($remaining.Count) registered VM(s)" }`,
		escapePSString(name),
		escapePSString(vmRoot),
		escapePSString(vhdRoot),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("remove partial Hyper-V checkpoint import", result, err)
	}
	return nil
}

func forkGuestHostname(leaseID string) string {
	suffix := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(leaseID), "cbx_"))
	var safe strings.Builder
	for _, r := range suffix {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			safe.WriteRune(r)
		}
	}
	value := safe.String()
	if len(value) > 11 {
		value = value[:11]
	}
	if value == "" {
		value = "checkpoint"
	}
	return "cbx-" + value
}
