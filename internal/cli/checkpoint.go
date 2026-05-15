package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	checkpointIDPrefix    = "chk_"
	checkpointMetaFile    = "checkpoint.json"
	checkpointArchive     = "workspace.tar.gz"
	checkpointKindRecipe  = "recipe"
	checkpointKindArchive = "workspace-archive"
	checkpointKindAWSAMI  = "aws-ami"
	checkpointKindAWSEBS  = "aws-ebs-snapshot"
	checkpointKindAzure   = "azure-managed-image"
	checkpointKindAzureOS = "azure-os-disk-snapshot"
	checkpointKindGCP     = "gcp-machine-image"
	checkpointKindGCPDisk = "gcp-disk-snapshot"

	checkpointStrategyAuto         = "auto"
	checkpointStrategyImage        = "image"
	checkpointStrategyDiskSnapshot = "disk-snapshot"
)

type checkpointRecord struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Kind           string `json:"kind"`
	CreatedAt      string `json:"createdAt"`
	CrabboxVersion string `json:"crabboxVersion"`
	Provider       string `json:"provider,omitempty"`
	LeaseID        string `json:"leaseId,omitempty"`
	Slug           string `json:"slug,omitempty"`
	TargetOS       string `json:"targetOS,omitempty"`
	WindowsMode    string `json:"windowsMode,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
	ArchivePath    string `json:"archivePath,omitempty"`
	ArchiveBytes   int64  `json:"archiveBytes,omitempty"`
	Native         struct {
		Provider string `json:"provider,omitempty"`
		ImageID  string `json:"imageId,omitempty"`
		Kind     string `json:"kind,omitempty"`
		Name     string `json:"name,omitempty"`
		State    string `json:"state,omitempty"`
		Region   string `json:"region,omitempty"`
		Project  string `json:"project,omitempty"`
		Resource string `json:"resource,omitempty"`
		Strategy string `json:"strategy,omitempty"`
		NoReboot bool   `json:"noReboot,omitempty"`
	} `json:"native,omitempty"`
	Repo struct {
		Root      string `json:"root,omitempty"`
		Name      string `json:"name,omitempty"`
		RemoteURL string `json:"remoteUrl,omitempty"`
		Head      string `json:"head,omitempty"`
		BaseRef   string `json:"baseRef,omitempty"`
	} `json:"repo"`
}

func (a App) checkpoint(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		a.printCheckpointHelp()
		if len(args) == 0 {
			return exit(2, "missing checkpoint command")
		}
		return nil
	}
	switch args[0] {
	case "create":
		return a.checkpointCreate(ctx, args[1:])
	case "list":
		return a.checkpointList(ctx, args[1:])
	case "inspect":
		return a.checkpointInspect(ctx, args[1:])
	case "restore":
		return a.checkpointRestore(ctx, args[1:])
	case "fork":
		return a.checkpointFork(ctx, args[1:])
	case "delete":
		return a.checkpointDelete(ctx, args[1:])
	case "prune":
		return a.checkpointPrune(ctx, args[1:])
	default:
		return exit(2, "unknown checkpoint command %q", args[0])
	}
}

func (a App) printCheckpointHelp() {
	fmt.Fprintln(a.Stdout, `Usage:
  crabbox checkpoint create --id <lease-id-or-slug> [--name <name>] [--mode auto|native|archive] [--strategy auto|disk-snapshot|image]
  crabbox checkpoint list [--json]
  crabbox checkpoint inspect <checkpoint-id> [--json]
  crabbox checkpoint restore <checkpoint-id> --id <lease-id-or-slug> [--clear=false]
  crabbox checkpoint fork <checkpoint-id> [--class <class>] [--keep]
  crabbox checkpoint delete <checkpoint-id>
  crabbox checkpoint prune --older-than <duration> [--kind native|archive] [--dry-run]

Checkpoints use provider-native disk snapshots for brokered AWS, Azure, and GCP Linux leases, and portable workspace archives elsewhere.`)
}

func (a App) checkpointCreate(ctx context.Context, args []string) (err error) {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint create", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	name := fs.String("name", "", "checkpoint name")
	mode := fs.String("mode", "auto", "checkpoint mode: auto, native, or archive")
	strategy := fs.String("strategy", checkpointStrategyAuto, "native checkpoint strategy: auto, disk-snapshot, or image")
	workdirOverride := fs.String("workdir", "", "remote workdir to archive")
	recipeOnly := fs.Bool("recipe-only", false, "record metadata without archiving the remote workdir")
	wait := fs.Bool("wait", true, "wait for native provider snapshot availability")
	waitTimeout := fs.Duration("wait-timeout", 45*time.Minute, "maximum native snapshot wait duration")
	noReboot := fs.Bool("no-reboot", true, "avoid rebooting the source instance while creating a native snapshot")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if !validCheckpointStrategy(*strategy) {
		return exit(2, "checkpoint strategy must be auto, disk-snapshot, or image")
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox checkpoint create --id <lease-id-or-slug> [--name <name>] [--mode auto|native|archive]", cfg); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, true)
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	workdir := strings.TrimSpace(*workdirOverride)
	if workdir == "" {
		workdir = remoteJoin(cfg, leaseID, repo.Name)
	}
	record, dir, err := newCheckpointRecord(repo, cfg, server, target, leaseID, workdir, *name)
	if err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	createKind := checkpointCreateMode(*mode, *strategy, cfg, server, target, *recipeOnly)
	switch createKind {
	case checkpointKindRecipe, checkpointKindAWSAMI, checkpointKindAWSEBS, checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk, checkpointKindArchive:
		record.Kind = createKind
	default:
		return exit(2, "checkpoint mode must be auto, native, or archive")
	}
	var paths checkpointPaths
	record, paths, err = store.Reserve(record)
	if err != nil {
		return err
	}
	dir = paths.Dir
	recordWritten := false
	defer func() {
		cleanupUncommittedCheckpointDir(dir, recordWritten, err)
	}()
	switch createKind {
	case checkpointKindRecipe:
	case checkpointKindAWSAMI, checkpointKindAWSEBS, checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk:
		image, err := a.createNativeCheckpoint(ctx, cfg, server, target, leaseID, record.Name, repo.Name, checkpointStrategyForKind(createKind), *noReboot, *wait, *waitTimeout)
		if image.ID != "" {
			applyNativeImageCheckpointRecord(&record, image, *noReboot)
		}
		if err != nil {
			if record.Native.ImageID != "" {
				if writeErr := store.Write(record); writeErr != nil {
					return writeErr
				}
				recordWritten = true
			}
			return err
		}
	case checkpointKindArchive:
		if err := ensureCheckpointArchiveTarget(target); err != nil {
			return err
		}
		bytes, err := createCheckpointArchive(ctx, target, workdir, paths.Archive)
		if err != nil {
			return err
		}
		record.ArchivePath = checkpointArchive
		record.ArchiveBytes = bytes
	}
	if err := store.Write(record); err != nil {
		return err
	}
	recordWritten = true
	if isNativeCheckpointKind(record.Kind) {
		fmt.Fprintf(a.Stdout, "checkpoint created id=%s kind=%s resource=%s state=%s region=%s workdir=%s\n", record.ID, record.Kind, record.Native.ImageID, record.Native.State, blank(record.Native.Region, "-"), record.Workdir)
		return nil
	}
	fmt.Fprintf(a.Stdout, "checkpoint created id=%s kind=%s bytes=%s workdir=%s\n", record.ID, record.Kind, humanBytes(record.ArchiveBytes), record.Workdir)
	return nil
}

type checkpointAudit struct {
	Record        checkpointRecord `json:"record"`
	LocalState    string           `json:"localState"`
	ProviderState string           `json:"providerState,omitempty"`
	NextAction    string           `json:"nextAction"`
	Error         string           `json:"error,omitempty"`
}

func (a App) checkpointList(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint list", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	verify := fs.Bool("verify", false, "verify local artifacts and provider resources")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	records, err := store.List()
	if err != nil {
		return err
	}
	if *verify {
		audits, err := a.verifyCheckpointRecords(ctx, store, records)
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(audits)
		}
		if len(audits) == 0 {
			fmt.Fprintln(a.Stdout, "no checkpoints")
			return nil
		}
		for _, audit := range audits {
			record := audit.Record
			extra := fmt.Sprintf("local=%s", audit.LocalState)
			if audit.ProviderState != "" {
				extra += fmt.Sprintf(" provider=%s", audit.ProviderState)
			}
			if audit.Error != "" {
				extra += fmt.Sprintf(" error=%q", audit.Error)
			}
			fmt.Fprintf(a.Stdout, "%s kind=%s name=%q repo=%s lease=%s %s next=%s created=%s\n", record.ID, record.Kind, record.Name, record.Repo.Name, blank(record.LeaseID, "-"), extra, audit.NextAction, record.CreatedAt)
		}
		return nil
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(records)
	}
	if len(records) == 0 {
		fmt.Fprintln(a.Stdout, "no checkpoints")
		return nil
	}
	for _, record := range records {
		extra := fmt.Sprintf("bytes=%s", humanBytes(record.ArchiveBytes))
		if isNativeCheckpointKind(record.Kind) {
			extra = fmt.Sprintf("resource=%s state=%s region=%s", blank(record.Native.ImageID, "-"), blank(record.Native.State, "-"), blank(record.Native.Region, "-"))
		}
		fmt.Fprintf(a.Stdout, "%s kind=%s name=%q repo=%s lease=%s %s created=%s\n", record.ID, record.Kind, record.Name, record.Repo.Name, blank(record.LeaseID, "-"), extra, record.CreatedAt)
	}
	return nil
}

func (a App) checkpointInspect(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint inspect", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	verify := fs.Bool("verify", false, "verify local artifact or provider resource")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox checkpoint inspect <checkpoint-id>")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, _, err := store.Read(fs.Arg(0))
	if err != nil {
		return err
	}
	if *verify {
		audit, err := a.verifyCheckpointRecord(ctx, store, record)
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(audit)
		}
		printCheckpointInspect(a.Stdout, record)
		fmt.Fprintf(a.Stdout, "local_state=%s\nprovider_state=%s\nnext_action=%s\n", audit.LocalState, blank(audit.ProviderState, "-"), audit.NextAction)
		if audit.Error != "" {
			fmt.Fprintf(a.Stdout, "verify_error=%s\n", audit.Error)
		}
		return nil
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(record)
	}
	printCheckpointInspect(a.Stdout, record)
	return nil
}

func printCheckpointInspect(stdout io.Writer, record checkpointRecord) {
	fmt.Fprintf(stdout, "id=%s\nkind=%s\nname=%s\ncreated=%s\nprovider=%s\nlease=%s\nrepo=%s\nhead=%s\nworkdir=%s\narchive=%s\nbytes=%s\n",
		record.ID, record.Kind, blank(record.Name, "-"), record.CreatedAt, blank(record.Provider, "-"), blank(record.LeaseID, "-"), blank(record.Repo.Name, "-"), blank(record.Repo.Head, "-"), blank(record.Workdir, "-"), blank(record.ArchivePath, "-"), humanBytes(record.ArchiveBytes))
	if isNativeCheckpointKind(record.Kind) {
		fmt.Fprintf(stdout, "resource=%s\nresource_name=%s\nresource_state=%s\nresource_region=%s\nstrategy=%s\nno_reboot=%t\n",
			blank(record.Native.ImageID, "-"), blank(record.Native.Name, "-"), blank(record.Native.State, "-"), blank(record.Native.Region, "-"), blank(record.Native.Strategy, checkpointStrategyImage), record.Native.NoReboot)
		if record.Native.Project != "" {
			fmt.Fprintf(stdout, "image_project=%s\n", record.Native.Project)
		}
		if record.Native.Resource != "" {
			fmt.Fprintf(stdout, "image_resource=%s\n", record.Native.Resource)
		}
	}
}

func (a App) checkpointRestore(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint restore", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	workdirOverride := fs.String("workdir", "", "remote restore workdir")
	clear := fs.Bool("clear", true, "clear the remote workdir before restoring")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox checkpoint restore <checkpoint-id> --id <lease-id-or-slug>")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, paths, err := store.Read(fs.Arg(0))
	if err != nil {
		return err
	}
	if record.Kind != checkpointKindArchive {
		if isNativeCheckpointKind(record.Kind) {
			return exit(2, "checkpoint %s is a VM image; use crabbox checkpoint fork %s to create a lease from it", record.ID, record.ID)
		}
		return exit(2, "checkpoint %s has kind=%s; restore requires %s", record.ID, record.Kind, checkpointKindArchive)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox checkpoint restore <checkpoint-id> --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, true)
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	workdir := strings.TrimSpace(*workdirOverride)
	if workdir == "" {
		workdir = defaultCheckpointRestoreWorkdir(cfg, leaseID, repo.Name, record.Workdir)
	}
	if err := restoreCheckpointArchive(ctx, target, checkpointArchivePath(paths, record), record.ID, workdir, *clear); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint restored id=%s lease=%s workdir=%s\n", record.ID, leaseID, workdir)
	return nil
}

func (a App) checkpointFork(ctx context.Context, args []string) (err error) {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint fork", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	keep := fs.Bool("keep", true, "keep forked lease after restore")
	workdirOverride := fs.String("workdir", "", "remote restore workdir")
	clear := fs.Bool("clear", true, "clear the remote workdir before restoring")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox checkpoint fork <checkpoint-id> [--class <class>]")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, paths, err := store.Read(fs.Arg(0))
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return err
	}
	nativeCheckpoint := isNativeCheckpointKind(record.Kind)
	if record.Kind != checkpointKindArchive && !nativeCheckpoint {
		return exit(2, "checkpoint %s has kind=%s; fork requires %s or a native image checkpoint", record.ID, record.Kind, checkpointKindArchive)
	}
	if nativeCheckpoint {
		if nativeCheckpointResourceID(record) == "" {
			return exit(2, "checkpoint %s is pending; native provider resource is not recorded yet", record.ID)
		}
		applyNativeCheckpointForkConfig(&cfg, fs, record)
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support checkpoint fork", backend.Spec().Name)
	}
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: leaseOptionsFromConfig(cfg), Keep: *keep, Reclaim: *reclaim})
	if err != nil {
		return err
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	defer func() {
		if err == nil && !*keep {
			a.releaseBackendLeaseBestEffort(context.Background(), sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		}
	}()
	applyResolvedServerConfig(&cfg, server)
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, lease)
		return err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, lease)
		return err
	} else {
		target = resolved.Target
		if resolved.FallbackReason != "" {
			fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
		}
	}
	if isNativeCheckpointKind(record.Kind) {
		workdir := nativeCheckpointForkWorkdir(cfg, leaseID, repo.Name, *workdirOverride)
		if err := relocateNativeCheckpointWorkdir(ctx, target, record.Workdir, workdir); err != nil {
			a.releaseBackendLeaseBestEffort(ctx, sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
			return err
		}
		fmt.Fprintf(a.Stdout, "checkpoint forked id=%s lease=%s slug=%s image=%s workdir=%s\n", record.ID, leaseID, blank(serverSlug(server), "-"), nativeCheckpointResourceID(record), workdir)
		return nil
	}
	workdir := strings.TrimSpace(*workdirOverride)
	if workdir == "" {
		workdir = defaultCheckpointRestoreWorkdir(cfg, leaseID, repo.Name, record.Workdir)
	}
	if err := restoreCheckpointArchive(ctx, target, checkpointArchivePath(paths, record), record.ID, workdir, *clear); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint forked id=%s lease=%s slug=%s workdir=%s\n", record.ID, leaseID, blank(serverSlug(server), "-"), workdir)
	return nil
}

func (a App) checkpointDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint delete", a.Stderr)
	localOnly := fs.Bool("local-only", false, "delete only the local checkpoint record")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox checkpoint delete <checkpoint-id>")
	}
	id, err := validateCheckpointID(fs.Arg(0))
	if err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	if err := deleteCheckpoint(ctx, store, id, *localOnly); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint deleted id=%s\n", id)
	return nil
}

func deleteCheckpoint(ctx context.Context, store checkpointStore, id string, localOnly bool) error {
	record, _, err := store.Read(id)
	if err != nil {
		return err
	}
	providerID := nativeCheckpointDeleteID(record)
	if isNativeCheckpointKind(record.Kind) && providerID != "" && !localOnly {
		coord, err := configuredAdminCoordinator()
		if err != nil {
			return err
		}
		if err := coord.DeleteImage(ctx, providerID, nativeCoordinatorImageRef(record)); err != nil {
			return err
		}
	}
	return store.Delete(id)
}

func (a App) checkpointPrune(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint prune", a.Stderr)
	olderThan := fs.String("older-than", "", "delete checkpoints older than this duration")
	kind := fs.String("kind", "", "checkpoint kind filter: native or archive")
	dryRun := fs.Bool("dry-run", false, "print checkpoints that would be deleted")
	localOnly := fs.Bool("local-only", false, "delete local checkpoint records without deleting provider resources")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox checkpoint prune --older-than <duration> [--kind native|archive] [--dry-run]")
	}
	pruneAge, err := parseCheckpointPruneDuration(*olderThan)
	if err != nil {
		return err
	}
	if pruneAge <= 0 {
		return exit(2, "usage: crabbox checkpoint prune --older-than <duration> [--kind native|archive] [--dry-run]")
	}
	kindFilter := strings.TrimSpace(*kind)
	if kindFilter != "" && kindFilter != "native" && kindFilter != "archive" {
		return exit(2, "--kind must be native or archive")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	records, err := store.List()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-pruneAge)
	matched := 0
	for _, record := range records {
		created, err := time.Parse(time.RFC3339, record.CreatedAt)
		if err != nil {
			return exit(2, "checkpoint %s has invalid createdAt: %v", record.ID, err)
		}
		if !created.Before(cutoff) || !checkpointMatchesPruneKind(record, kindFilter) {
			continue
		}
		matched++
		if *dryRun {
			fmt.Fprintf(a.Stdout, "would delete id=%s kind=%s created=%s\n", record.ID, record.Kind, record.CreatedAt)
			continue
		}
		if err := deleteCheckpoint(ctx, store, record.ID, *localOnly); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "checkpoint pruned id=%s kind=%s created=%s\n", record.ID, record.Kind, record.CreatedAt)
	}
	if matched == 0 {
		fmt.Fprintln(a.Stdout, "no checkpoints matched prune criteria")
	}
	return nil
}

func parseCheckpointPruneDuration(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, nil
	}
	if strings.HasSuffix(trimmed, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(trimmed, "d"))
		if err != nil || days <= 0 {
			return 0, exit(2, "--older-than day duration must be a positive integer")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, exit(2, "parse --older-than: %v", err)
	}
	return duration, nil
}

func checkpointMatchesPruneKind(record checkpointRecord, kind string) bool {
	switch kind {
	case "":
		return true
	case "native":
		return isNativeCheckpointKind(record.Kind)
	case "archive":
		return record.Kind == checkpointKindArchive
	default:
		return false
	}
}

func (a App) verifyCheckpointRecords(ctx context.Context, store checkpointStore, records []checkpointRecord) ([]checkpointAudit, error) {
	audits := make([]checkpointAudit, 0, len(records))
	for _, record := range records {
		audit, err := a.verifyCheckpointRecord(ctx, store, record)
		if err != nil {
			return nil, err
		}
		audits = append(audits, audit)
	}
	return audits, nil
}

func (a App) verifyCheckpointRecord(ctx context.Context, store checkpointStore, record checkpointRecord) (checkpointAudit, error) {
	audit := checkpointAudit{
		Record:     record,
		LocalState: "metadata_available",
		NextAction: "inspect",
	}
	paths, err := store.Paths(record.ID)
	if err != nil {
		return checkpointAudit{}, err
	}
	switch {
	case record.Kind == checkpointKindArchive:
		archivePath := checkpointArchivePath(paths, record)
		info, err := os.Stat(archivePath)
		if err != nil {
			if os.IsNotExist(err) {
				audit.LocalState = "missing_archive"
				audit.ProviderState = "not_applicable"
				audit.NextAction = "delete_or_recreate"
				return audit, nil
			}
			return checkpointAudit{}, exit(2, "stat checkpoint archive %s: %v", record.ID, err)
		}
		if info.IsDir() {
			audit.LocalState = "invalid_archive"
			audit.ProviderState = "not_applicable"
			audit.NextAction = "delete_or_recreate"
			return audit, nil
		}
		audit.LocalState = "available"
		audit.ProviderState = "not_applicable"
		audit.NextAction = "restore_or_fork"
		return audit, nil
	case isNativeCheckpointKind(record.Kind):
		providerID := strings.TrimSpace(record.Native.ImageID)
		if providerID == "" {
			if nativeCheckpointResourceID(record) != "" {
				audit.ProviderState = "unverified_ref"
				audit.NextAction = "fork_or_delete_local"
				return audit, nil
			}
			audit.ProviderState = "missing_ref"
			audit.NextAction = "delete_local"
			return audit, nil
		}
		coord, err := configuredAdminCoordinator()
		if err != nil {
			audit.ProviderState = "unknown"
			audit.NextAction = "configure_admin_auth"
			audit.Error = err.Error()
			return audit, nil
		}
		image, err := coord.Image(ctx, providerID, nativeCoordinatorImageRef(record))
		if err != nil {
			if coordinatorStatusCode(err) == 404 {
				audit.ProviderState = "missing"
				audit.NextAction = "delete_local"
				return audit, nil
			}
			audit.ProviderState = "unknown"
			audit.NextAction = "check_auth_or_provider"
			audit.Error = err.Error()
			return audit, nil
		}
		audit.ProviderState = blank(image.State, "unknown")
		switch strings.ToLower(image.State) {
		case "available", "ready", "succeeded", "completed":
			audit.NextAction = "fork_or_delete"
		case "failed", "invalid":
			audit.NextAction = "delete"
		default:
			audit.NextAction = "wait_or_delete"
		}
		return audit, nil
	default:
		audit.LocalState = "metadata_only"
		audit.ProviderState = "not_applicable"
		audit.NextAction = "inspect"
		return audit, nil
	}
}

func checkpointArchivePath(paths checkpointPaths, record checkpointRecord) string {
	if record.ArchivePath == "" {
		return paths.Archive
	}
	if filepath.IsAbs(record.ArchivePath) {
		return record.ArchivePath
	}
	return filepath.Join(paths.Dir, record.ArchivePath)
}

func coordinatorStatusCode(err error) int {
	var httpErr CoordinatorHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

func applyNativeImageCheckpointRecord(record *checkpointRecord, image CoordinatorImage, noReboot bool) {
	record.Kind = checkpointKindForProviderImage(image)
	record.Native.Provider = firstNonBlank(image.Provider, checkpointProviderForKind(record.Kind), record.Provider)
	record.Native.ImageID = image.ID
	record.Native.Kind = image.Kind
	record.Native.Name = image.Name
	record.Native.State = image.State
	record.Native.Region = image.Region
	record.Native.Project = image.Project
	record.Native.Resource = image.ResourceID
	record.Native.Strategy = checkpointStrategyForKind(record.Kind)
	record.Native.NoReboot = noReboot
}

func applyAWSAMIImageCheckpointRecord(record *checkpointRecord, image CoordinatorImage, noReboot bool) {
	applyNativeImageCheckpointRecord(record, image, noReboot)
}

func nativeCheckpointResourceID(record checkpointRecord) string {
	switch record.Kind {
	case checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk:
		return firstNonBlank(record.Native.Resource, record.Native.ImageID)
	default:
		return record.Native.ImageID
	}
}

func nativeCheckpointDeleteID(record checkpointRecord) string {
	if imageID := strings.TrimSpace(record.Native.ImageID); imageID != "" {
		return imageID
	}
	switch record.Kind {
	case checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk:
		return strings.TrimSpace(record.Native.Resource)
	default:
		return ""
	}
}

func checkpointKindForProviderImage(image CoordinatorImage) string {
	switch image.Kind {
	case checkpointKindAWSEBS:
		return checkpointKindAWSEBS
	case checkpointKindAzureOS:
		return checkpointKindAzureOS
	case checkpointKindGCPDisk:
		return checkpointKindGCPDisk
	}
	switch image.Provider {
	case "azure":
		return checkpointKindAzure
	case "gcp":
		return checkpointKindGCP
	default:
		return checkpointKindAWSAMI
	}
}

func newCheckpointRecord(repo Repo, cfg Config, server Server, target SSHTarget, leaseID, workdir, name string) (checkpointRecord, string, error) {
	id, err := newCheckpointID()
	if err != nil {
		return checkpointRecord{}, "", err
	}
	dir, err := checkpointDir(id)
	if err != nil {
		return checkpointRecord{}, "", err
	}
	record := checkpointRecord{
		ID:             id,
		Name:           strings.TrimSpace(name),
		Kind:           checkpointKindArchive,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		CrabboxVersion: currentVersion(),
		Provider:       firstNonBlank(server.Provider, cfg.Provider),
		LeaseID:        leaseID,
		Slug:           serverSlug(server),
		TargetOS:       firstNonBlank(target.TargetOS, cfg.TargetOS),
		WindowsMode:    firstNonBlank(target.WindowsMode, cfg.WindowsMode),
		Workdir:        workdir,
	}
	record.Repo.Root = repo.Root
	record.Repo.Name = repo.Name
	record.Repo.RemoteURL = repo.RemoteURL
	record.Repo.Head = repo.Head
	record.Repo.BaseRef = repo.BaseRef
	return record, dir, nil
}

func cleanupUncommittedCheckpointDir(dir string, committed bool, err error) {
	if err == nil || committed || dir == "" {
		return
	}
	_ = os.RemoveAll(dir)
}

func newCheckpointID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", exit(2, "generate checkpoint id: %v", err)
	}
	return checkpointIDPrefix + hex.EncodeToString(raw[:]), nil
}

func checkpointCreateMode(mode, strategy string, cfg Config, server Server, target SSHTarget, recipeOnly bool) string {
	if recipeOnly {
		return checkpointKindRecipe
	}
	normalizedStrategy := normalizeCheckpointStrategy(strategy)
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		if kind, ok := nativeCheckpointKind(cfg, server, target, normalizedStrategy); ok {
			return kind
		}
		return checkpointKindArchive
	case "native", "provider-native", "vm":
		if kind, ok := nativeCheckpointKind(cfg, server, target, normalizedStrategy); ok {
			return kind
		}
		return "unsupported"
	case "ami", "image":
		if kind, ok := nativeCheckpointKind(cfg, server, target, checkpointStrategyImage); ok {
			return kind
		}
		return "unsupported"
	case "snapshot", "disk-snapshot", "disk":
		if kind, ok := nativeCheckpointKind(cfg, server, target, checkpointStrategyDiskSnapshot); ok {
			return kind
		}
		return "unsupported"
	case "archive", "workspace", "workspace-archive":
		return checkpointKindArchive
	case "recipe":
		return checkpointKindRecipe
	default:
		return "unsupported"
	}
}

func nativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	if cfg.Coordinator == "" || server.CloudID == "" || isWindowsNativeTarget(target) || firstNonBlank(target.TargetOS, cfg.TargetOS) != targetLinux {
		return "", false
	}
	strategy = normalizeCheckpointStrategy(strategy)
	switch server.Provider {
	case "aws":
		if strategy == checkpointStrategyImage {
			return checkpointKindAWSAMI, true
		}
		return checkpointKindAWSEBS, true
	case "azure":
		if strategy == checkpointStrategyImage {
			return checkpointKindAzure, true
		}
		return checkpointKindAzureOS, true
	case "gcp":
		if strategy == checkpointStrategyImage {
			return checkpointKindGCP, true
		}
		return checkpointKindGCPDisk, true
	default:
		return "", false
	}
}

func normalizeCheckpointStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", checkpointStrategyAuto, "snapshot", "disk":
		return checkpointStrategyDiskSnapshot
	case checkpointStrategyImage, "ami", "machine-image", "managed-image":
		return checkpointStrategyImage
	case checkpointStrategyDiskSnapshot, "disk_snapshot":
		return checkpointStrategyDiskSnapshot
	default:
		return checkpointStrategyDiskSnapshot
	}
}

func validCheckpointStrategy(strategy string) bool {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", checkpointStrategyAuto, checkpointStrategyDiskSnapshot, checkpointStrategyImage, "snapshot", "disk", "ami", "machine-image", "managed-image", "disk_snapshot":
		return true
	default:
		return false
	}
}

func checkpointStrategyForKind(kind string) string {
	switch kind {
	case checkpointKindAWSAMI, checkpointKindAzure, checkpointKindGCP:
		return checkpointStrategyImage
	case checkpointKindAWSEBS, checkpointKindAzureOS, checkpointKindGCPDisk:
		return checkpointStrategyDiskSnapshot
	default:
		return ""
	}
}

func (a App) createNativeCheckpoint(ctx context.Context, cfg Config, server Server, target SSHTarget, leaseID, name, repoName, strategy string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, error) {
	if cfg.Coordinator == "" {
		return CoordinatorImage{}, exit(2, "native checkpoints require a configured coordinator")
	}
	strategy = normalizeCheckpointStrategy(strategy)
	if _, ok := nativeCheckpointKind(cfg, server, target, strategy); !ok {
		return CoordinatorImage{}, exit(2, "native checkpoints currently support brokered AWS, Azure, and GCP Linux leases only")
	}
	if server.Provider == "azure" && strategy == checkpointStrategyImage {
		return CoordinatorImage{}, exit(2, "Azure managed images require a stopped/generalized source VM; use --strategy disk-snapshot for active Azure leases")
	}
	if name == "" {
		name = defaultNativeImageName(leaseID, repoName)
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return CoordinatorImage{}, err
	}
	if err := prepareNativeLinuxImageSource(ctx, target); err != nil {
		return CoordinatorImage{}, err
	}
	image, err := coord.CreateImage(ctx, leaseID, name, noReboot, strategy)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if wait {
		waited, err := waitForImage(ctx, coord, image.ID, imageRefFromCoordinatorImage(image), waitTimeout, a.Stderr)
		if err != nil {
			return image, err
		}
		return waited, nil
	}
	return image, nil
}

func (a App) createAWSAMICheckpoint(ctx context.Context, cfg Config, target SSHTarget, leaseID, name, repoName string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, error) {
	return a.createNativeCheckpoint(ctx, cfg, Server{Provider: "aws", CloudID: leaseID}, target, leaseID, name, repoName, checkpointStrategyImage, noReboot, wait, waitTimeout)
}

func defaultNativeImageName(leaseID, repoName string) string {
	repoName = strings.TrimSpace(repoName)
	if repoName == "" {
		repoName = "workspace"
	}
	base := "crabbox-" + safeCaptureName(repoName) + "-" + strings.ReplaceAll(leaseID, "_", "-") + "-" + time.Now().UTC().Format("20060102-150405")
	if len(base) > 128 {
		return base[:128]
	}
	return base
}

func prepareNativeLinuxImageSource(ctx context.Context, target SSHTarget) error {
	command := remotePrepareAWSAMICommand()
	if out, err := runSSHCombinedOutput(ctx, target, "bash -lc "+shellQuote(command)); err != nil {
		return exit(7, "prepare native checkpoint source cloud-init: %v: %s", err, trimFailureDetail(out))
	}
	return nil
}

func remotePrepareAWSAMICommand() string {
	return "if command -v cloud-init >/dev/null 2>&1; then sudo cloud-init clean --logs; fi; sync"
}

func defaultCheckpointRestoreWorkdir(cfg Config, leaseID, repoName, savedWorkdir string) string {
	return firstNonBlank(remoteJoin(cfg, leaseID, repoName), savedWorkdir)
}

func nativeCheckpointForkWorkdir(cfg Config, leaseID, repoName, override string) string {
	override = strings.TrimSpace(override)
	if override != "" {
		return override
	}
	return remoteJoin(cfg, leaseID, repoName)
}

func applyNativeCheckpointForkConfig(cfg *Config, fs *flag.FlagSet, record checkpointRecord) {
	cfg.Provider = firstNonBlank(record.Native.Provider, checkpointProviderForKind(record.Kind), record.Provider)
	if cfg.CoordAdminToken != "" {
		cfg.CoordToken = cfg.CoordAdminToken
	}
	switch record.Kind {
	case checkpointKindAWSAMI:
		cfg.AWSAMI = record.Native.ImageID
		if record.Native.Region != "" {
			cfg.AWSRegion = record.Native.Region
		}
	case checkpointKindAWSEBS:
		cfg.AWSSnapshot = record.Native.ImageID
		if record.Native.Region != "" {
			cfg.AWSRegion = record.Native.Region
		}
	case checkpointKindAzure:
		cfg.AzureImage = firstNonBlank(record.Native.Resource, record.Native.ImageID)
		if record.Native.Region != "" {
			cfg.AzureLocation = record.Native.Region
		}
	case checkpointKindAzureOS:
		cfg.AzureSnapshot = firstNonBlank(record.Native.Resource, record.Native.ImageID)
		if record.Native.Region != "" {
			cfg.AzureLocation = record.Native.Region
		}
	case checkpointKindGCP:
		cfg.GCPMachineImage = firstNonBlank(record.Native.Resource, record.Native.ImageID)
		if record.Native.Region != "" {
			cfg.GCPZone = record.Native.Region
		}
		if record.Native.Project != "" {
			cfg.GCPProject = record.Native.Project
			cfg.gcpProjectExplicit = true
		}
	case checkpointKindGCPDisk:
		cfg.GCPSnapshot = firstNonBlank(record.Native.Resource, record.Native.ImageID)
		if record.Native.Region != "" {
			cfg.GCPZone = record.Native.Region
		}
		if record.Native.Project != "" {
			cfg.GCPProject = record.Native.Project
			cfg.gcpProjectExplicit = true
		}
	}
	if record.TargetOS != "" {
		cfg.TargetOS = record.TargetOS
	}
	if record.WindowsMode != "" {
		cfg.WindowsMode = record.WindowsMode
	}
	if !flagWasSet(fs, "type") {
		cfg.ServerTypeExplicit = false
		cfg.ServerType = serverTypeForConfig(*cfg)
	}
}

func applyAWSAMICheckpointForkConfig(cfg *Config, fs *flag.FlagSet, record checkpointRecord) {
	applyNativeCheckpointForkConfig(cfg, fs, record)
}

func nativeCoordinatorImageRef(record checkpointRecord) CoordinatorImageRef {
	return CoordinatorImageRef{
		Provider: firstNonBlank(record.Native.Provider, checkpointProviderForKind(record.Kind), record.Provider),
		Region:   record.Native.Region,
		Project:  record.Native.Project,
		Kind:     firstNonBlank(record.Native.Kind, record.Kind),
	}
}

func isNativeCheckpointKind(kind string) bool {
	return kind == checkpointKindAWSAMI || kind == checkpointKindAWSEBS || kind == checkpointKindAzure || kind == checkpointKindAzureOS || kind == checkpointKindGCP || kind == checkpointKindGCPDisk
}

func checkpointProviderForKind(kind string) string {
	switch kind {
	case checkpointKindAWSAMI, checkpointKindAWSEBS:
		return "aws"
	case checkpointKindAzure, checkpointKindAzureOS:
		return "azure"
	case checkpointKindGCP, checkpointKindGCPDisk:
		return "gcp"
	default:
		return ""
	}
}

func parseInterspersedFlags(fs *flag.FlagSet, args []string) error {
	return parseFlags(fs, reorderInterspersedFlags(fs, args))
}

func reorderInterspersedFlags(fs *flag.FlagSet, args []string) []string {
	if len(args) == 0 {
		return args
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if cut, _, ok := strings.Cut(name, "="); ok {
			name = cut
		}
		if strings.Contains(arg, "=") || isBoolFlag(fs, name) || i+1 >= len(args) {
			continue
		}
		i++
		flags = append(flags, args[i])
	}
	return append(flags, positionals...)
}

func isBoolFlag(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	boolValue, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolValue.IsBoolFlag()
}

func validateCheckpointID(value string) (string, error) {
	id := strings.TrimSpace(value)
	if !strings.HasPrefix(id, checkpointIDPrefix) || len(id) <= len(checkpointIDPrefix) {
		return "", exit(2, "checkpoint id must start with %s", checkpointIDPrefix)
	}
	for _, r := range strings.TrimPrefix(id, checkpointIDPrefix) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return "", exit(2, "checkpoint id contains unsafe character %q", r)
	}
	return id, nil
}

func ensureCheckpointArchiveTarget(target SSHTarget) error {
	if isWindowsNativeTarget(target) {
		return exit(2, "workspace-archive checkpoints currently require POSIX SSH targets; use Windows WSL2 or a Linux/macOS lease")
	}
	return nil
}

func createCheckpointArchive(ctx context.Context, target SSHTarget, workdir, localPath string) (size int64, err error) {
	if err := ensureCheckpointArchiveTarget(target); err != nil {
		return 0, err
	}
	archiveDir := filepath.Dir(localPath)
	createdArchiveDir := false
	if _, statErr := os.Stat(archiveDir); os.IsNotExist(statErr) {
		createdArchiveDir = true
	}
	published := false
	defer func() {
		if err != nil && createdArchiveDir && !published {
			_ = os.RemoveAll(archiveDir)
		}
	}()
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		return 0, exit(2, "create checkpoint archive directory: %v", err)
	}
	tmpPath := localPath + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, exit(2, "create checkpoint archive: %v", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(target, remoteCheckpointArchiveCommand(workdir))...)
	cmd.Stdout = file
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := file.Close()
	if runErr != nil {
		return 0, exit(7, "archive checkpoint workdir %s: %v: %s", workdir, runErr, trimFailureDetail(stderr.String()))
	}
	if closeErr != nil {
		return 0, exit(2, "close checkpoint archive: %v", closeErr)
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return 0, exit(2, "stat checkpoint archive: %v", err)
	}
	if info.Size() == 0 {
		return 0, exit(7, "archive checkpoint workdir %s: empty archive", workdir)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		return 0, exit(2, "publish checkpoint archive: %v", err)
	}
	published = true
	return info.Size(), nil
}

func restoreCheckpointArchive(ctx context.Context, target SSHTarget, localPath, checkpointID, workdir string, clear bool) error {
	if err := ensureCheckpointArchiveTarget(target); err != nil {
		return err
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return exit(2, "read checkpoint archive: %v", err)
	}
	if info.IsDir() {
		return exit(2, "checkpoint archive is a directory: %s", localPath)
	}
	file, err := os.Open(localPath)
	if err != nil {
		return exit(2, "open checkpoint archive: %v", err)
	}
	defer func() { _ = file.Close() }()
	var stderr strings.Builder
	if err := runSSHInputStream(ctx, target, remoteCheckpointRestoreCommand(workdir, clear), file, io.Discard, &stderr); err != nil {
		return exit(7, "restore checkpoint %s: %v: %s", checkpointID, err, trimFailureDetail(stderr.String()))
	}
	return nil
}

func remoteCheckpointArchiveCommand(workdir string) string {
	script := "set -eu\n" +
		"test -d " + shellQuote(workdir) + "\n" +
		"tar -C " + shellQuote(workdir) + " --exclude './.crabbox/env' --exclude './.crabbox/scripts' -czf - ."
	return "bash -lc " + shellQuote(script)
}

func remoteCheckpointRestoreCommand(workdir string, clear bool) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("tmp=$(mktemp /tmp/crabbox-checkpoint.XXXXXX)\n")
	b.WriteString("cleanup() { rm -f -- \"$tmp\"; }\n")
	b.WriteString("trap cleanup EXIT INT TERM\n")
	b.WriteString("cat > \"$tmp\"\n")
	b.WriteString("mkdir -p ")
	b.WriteString(shellQuote(workdir))
	b.WriteByte('\n')
	if clear {
		b.WriteString("find ")
		b.WriteString(shellQuote(workdir))
		b.WriteString(" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +\n")
	}
	b.WriteString("tar -C ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" -xzf \"$tmp\"")
	return "bash -lc " + shellQuote(b.String())
}

func relocateNativeCheckpointWorkdir(ctx context.Context, target SSHTarget, sourceWorkdir, targetWorkdir string) error {
	command := remoteRelocateNativeCheckpointWorkdirCommand(sourceWorkdir, targetWorkdir)
	if command == "" {
		return nil
	}
	if out, err := runSSHCombinedOutput(ctx, target, command); err != nil {
		return exit(7, "relocate native checkpoint workdir: %v: %s", err, trimFailureDetail(out))
	}
	return nil
}

func remoteRelocateNativeCheckpointWorkdirCommand(sourceWorkdir, targetWorkdir string) string {
	sourceWorkdir = strings.TrimSpace(sourceWorkdir)
	targetWorkdir = strings.TrimSpace(targetWorkdir)
	if sourceWorkdir == "" || targetWorkdir == "" || sourceWorkdir == targetWorkdir {
		return ""
	}
	script := "set -eu\n" +
		"src=" + shellQuote(sourceWorkdir) + "\n" +
		"dst=" + shellQuote(targetWorkdir) + "\n" +
		"if test -d \"$src\" && ! test -e \"$dst\"; then\n" +
		"  mkdir -p \"$(dirname \"$dst\")\"\n" +
		"  mv \"$src\" \"$dst\"\n" +
		"fi"
	return "bash -lc " + shellQuote(script)
}
