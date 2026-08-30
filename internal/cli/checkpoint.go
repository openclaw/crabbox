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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	checkpointIDPrefix         = "chk_"
	checkpointMetaFile         = "checkpoint.json"
	checkpointArchive          = "workspace.tar.gz"
	checkpointKindRecipe       = "recipe"
	checkpointKindArchive      = "workspace-archive"
	checkpointKindAWSAMI       = "aws-ami"
	checkpointKindAWSEBS       = "aws-ebs-snapshot"
	checkpointKindAzure        = "azure-managed-image"
	checkpointKindAzureOS      = "azure-os-disk-snapshot"
	checkpointKindGCP          = "gcp-machine-image"
	checkpointKindGCPDisk      = "gcp-disk-snapshot"
	checkpointKindHetzner      = "hetzner-snapshot"
	checkpointKindMachine0     = "machine0-image"
	checkpointKindParallels    = "parallels-snapshot"
	checkpointKindDockerCommit = "docker-commit"
	checkpointKindDaytona      = "daytona-snapshot"
	checkpointKindIncus        = "incus-image"

	checkpointStrategyAuto         = "auto"
	checkpointStrategyImage        = "image"
	checkpointStrategyDiskSnapshot = "disk-snapshot"
)

type checkpointRecord struct {
	Capture          *NativeCheckpointCapture        `json:"capture,omitempty"`
	ID               string                          `json:"id"`
	Name             string                          `json:"name,omitempty"`
	Kind             string                          `json:"kind"`
	CreatedAt        string                          `json:"createdAt"`
	LastUsedAt       string                          `json:"lastUsedAt"`
	CrabboxVersion   string                          `json:"crabboxVersion"`
	Provider         string                          `json:"provider,omitempty"`
	LeaseID          string                          `json:"leaseId,omitempty"`
	Slug             string                          `json:"slug,omitempty"`
	TargetOS         string                          `json:"targetOS,omitempty"`
	WindowsMode      string                          `json:"windowsMode,omitempty"`
	Desktop          bool                            `json:"desktop,omitempty"`
	ServerType       string                          `json:"serverType,omitempty"`
	HostID           string                          `json:"hostId,omitempty"`
	Workdir          string                          `json:"workdir,omitempty"`
	ArchivePath      string                          `json:"archivePath,omitempty"`
	ArchiveBytes     int64                           `json:"archiveBytes,omitempty"`
	Ownership        *checkpointOwnership            `json:"ownership,omitempty"`
	Retention        *coordinatorCheckpointRetention `json:"retention,omitempty"`
	CoordinatorState string                          `json:"coordinatorState,omitempty"`
	Native           struct {
		Provider     string            `json:"provider,omitempty"`
		ImageID      string            `json:"imageId,omitempty"`
		Kind         string            `json:"kind,omitempty"`
		Name         string            `json:"name,omitempty"`
		State        string            `json:"state,omitempty"`
		Region       string            `json:"region,omitempty"`
		AccountID    string            `json:"accountId,omitempty"`
		Project      string            `json:"project,omitempty"`
		Resource     string            `json:"resource,omitempty"`
		Architecture string            `json:"architecture,omitempty"`
		SnapshotIDs  []string          `json:"snapshotIds,omitempty"`
		Direct       bool              `json:"direct,omitempty"`
		Strategy     string            `json:"strategy,omitempty"`
		NoReboot     bool              `json:"noReboot,omitempty"`
		Metadata     map[string]string `json:"metadata,omitempty"`
	} `json:"native,omitempty"`
	Repo struct {
		Root      string `json:"root,omitempty"`
		Name      string `json:"name,omitempty"`
		RemoteURL string `json:"remoteUrl,omitempty"`
		Head      string `json:"head,omitempty"`
		BaseRef   string `json:"baseRef,omitempty"`
	} `json:"repo"`
}

type checkpointOwnership struct {
	Mode   string `json:"mode"`
	Origin string `json:"origin,omitempty"`
}

func (record checkpointRecord) coordinatorManaged() bool {
	return record.Ownership != nil && record.Ownership.Mode == "coordinator"
}

func (a App) checkpointCreate(ctx context.Context, args []string) (err error) {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint create", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	name := fs.String("name", "", "checkpoint name")
	checkpointID := fs.String("checkpoint-id", "", "stable checkpoint ID for replayable source retirement")
	retireSource := fs.Bool("retire-source", false, "capture then retire the exact source without restoring it")
	prepareOnly := fs.Bool("prepare-only", false, "check source retirement admission without creating a checkpoint or changing the source")
	discardFailed := fs.Bool("discard-failed", false, "discard a verified failed image and finish its requested source retirement")
	mode := fs.String("mode", "auto", "checkpoint mode: auto, native, or archive")
	strategy := fs.String("strategy", checkpointStrategyAuto, "native checkpoint strategy: auto, disk-snapshot, or image")
	workdirOverride := fs.String("workdir", "", "remote workdir to archive")
	recipeOnly := fs.Bool("recipe-only", false, "record metadata without archiving the remote workdir")
	jsonOut := fs.Bool("json", false, "print the checkpoint record as JSON")
	wait := fs.Bool("wait", true, "wait for native provider snapshot availability")
	waitTimeout := fs.Duration("wait-timeout", 45*time.Minute, "maximum native snapshot wait duration")
	noReboot := fs.Bool("no-reboot", true, "avoid rebooting the source instance while creating a native snapshot")
	expireUnusedAfter := fs.String("expire-unused-after", "", "automatically expire an unused coordinator-managed native checkpoint after this duration")
	admin := fs.Bool("admin", false, "use the configured coordinator admin credential")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	ctx = withCheckpointAdmin(ctx, *admin)
	operationApp := a
	if *jsonOut {
		operationApp.Stdout = a.Stderr
	}
	if !validCheckpointStrategy(*strategy) {
		return exit(2, "checkpoint strategy must be auto, disk-snapshot, or image")
	}
	retentionDuration, err := parseCheckpointRetentionDuration(*expireUnusedAfter)
	if err != nil {
		return err
	}
	if retentionDuration > 0 && (*retireSource || *checkpointID != "" || *discardFailed || *prepareOnly) {
		return exit(2, "--expire-unused-after cannot be combined with source retirement")
	}
	if retentionDuration > 0 && (*recipeOnly || strings.EqualFold(strings.TrimSpace(*mode), "archive") || strings.EqualFold(strings.TrimSpace(*mode), "recipe")) {
		return exit(2, "--expire-unused-after requires a brokered native AWS, Azure, or GCP checkpoint")
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox checkpoint create --id <lease-id-or-slug> [--name <name>] [--mode auto|native|archive]", cfg); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *retireSource || *checkpointID != "" || *discardFailed || *prepareOnly {
		if !*retireSource || *checkpointID == "" || *reclaim || *recipeOnly || *mode != "native" || *workdirOverride != "" || *name != "" || *wait || (*prepareOnly && *discardFailed) {
			return exit(2, "--retire-source requires --checkpoint-id, --mode native, --wait=false and no --reclaim, --recipe-only, --workdir or --name")
		}
		return operationApp.checkpointRetire(ctx, cfg, repo, *id, *checkpointID, *strategy, *noReboot, *discardFailed, *prepareOnly, *jsonOut, a.Stdout)
	}
	var server Server
	var target SSHTarget
	var leaseID string
	selected, _ := ProviderFor(cfg.Provider)
	policy, apiSource := selected.(NativeCheckpointSourcePolicyProvider)
	requestedKind := checkpointCreateMode(*mode, *strategy, cfg, Server{Provider: cfg.Provider, CloudID: *id}, SSHTarget{TargetOS: cfg.TargetOS}, *recipeOnly)
	if apiSource && policy.NativeCheckpointSourceStatusOnly(cfg) && isNativeCheckpointKind(requestedKind) {
		server, target, leaseID, err = operationApp.resolveLeaseTargetWithRequestConfig(ctx, &cfg, ResolveRequest{Repo: repo, ID: *id, Reclaim: *reclaim, StatusOnly: true})
	} else {
		server, target, leaseID, err = operationApp.resolveNetworkLeaseTargetForRepoWithConfig(ctx, &cfg, *id, true, *reclaim)
	}
	if err != nil {
		return err
	}
	createKind := checkpointCreateMode(*mode, *strategy, cfg, server, target, *recipeOnly)
	if retentionDuration > 0 {
		driver, supported := nativeCheckpointCreateDriver(cfg, server, target, *strategy)
		_, coordinatorDriver := driver.(coordinatorCheckpointDriver)
		if !supported || !coordinatorDriver || !isNativeCheckpointKind(createKind) {
			return exit(2, "--expire-unused-after requires a brokered native AWS, Azure, or GCP checkpoint")
		}
		coord, coordErr := configuredCheckpointCoordinatorFor(ctx)
		if coordErr != nil {
			return coordErr
		}
		if _, probeErr := coord.Checkpoints(ctx); probeErr != nil {
			if checkpointRouteUnsupported(probeErr) {
				return checkpointUpgradeRequired(probeErr)
			}
			return probeErr
		}
	}
	if err := operationApp.claimResolvedLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, &server, target, repo.Root, *reclaim); err != nil {
		return err
	}
	workdir := strings.TrimSpace(*workdirOverride)
	if workdir == "" {
		if provider, ok := nativeCheckpointLifecycleProvider(cfg, server); ok {
			workdir = provider.NativeCheckpointWorkdir(NativeCheckpointWorkdirRequest{
				Config:   cfg,
				Server:   server,
				LeaseID:  leaseID,
				RepoName: repo.Name,
			})
		} else {
			workdir = remoteJoin(cfg, leaseID, repo.Name)
		}
	}
	record, dir, err := newCheckpointRecord(repo, cfg, server, target, leaseID, workdir, *name)
	if err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	switch createKind {
	case checkpointKindRecipe, checkpointKindAWSAMI, checkpointKindAWSEBS, checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk, checkpointKindHetzner, checkpointKindMachine0, checkpointKindParallels, checkpointKindDockerCommit, checkpointKindDaytona, checkpointKindIncus, checkpointKindArchive:
		record.Kind = createKind
	default:
		return exit(2, "checkpoint mode must be auto, native, or archive")
	}
	// All observers and final writes share deletion/prune's operation lock.
	// Take checkpoint before source-claim locks, as retirement does.
	return store.WithLock(record.ID, func() (err error) {
		var paths checkpointPaths
		if isNativeCheckpointKind(record.Kind) {
			claim, claimErr := readLeaseClaim(leaseID)
			if claimErr != nil {
				return claimErr
			}
			record, paths, err = reserveSourceCheckpoint(store, record, claim)
		} else {
			record, paths, err = store.Reserve(record)
		}
		if err != nil {
			return err
		}
		dir = paths.Dir
		recordWritten := isNativeCheckpointKind(createKind)
		defer func() {
			cleanupUncommittedCheckpointDir(dir, recordWritten, err)
		}()
		switch createKind {
		case checkpointKindRecipe:
		case checkpointKindAWSAMI, checkpointKindAWSEBS, checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk, checkpointKindHetzner, checkpointKindMachine0, checkpointKindParallels, checkpointKindDockerCommit, checkpointKindDaytona, checkpointKindIncus:
			createStrategy := checkpointCreateStrategy(*mode, *strategy, createKind)
			retention := checkpointRetentionFromDuration(retentionDuration)
			createContext := withCheckpointCreateContext(ctx, record, retention, func(managed bool) error {
				if managed {
					record.Ownership = &checkpointOwnership{Mode: "coordinator", Origin: checkpointCoordinatorOrigin(cfg.Coordinator)}
					record.Retention = &retention
					record.CoordinatorState = "creating"
				} else {
					record.Ownership, record.Retention, record.CoordinatorState = nil, nil, ""
				}
				return store.Write(record)
			})
			image, metadata, err := operationApp.createNativeCheckpointRequest(createContext, NativeCheckpointCreateRequest{
				Config: cfg, Server: server, Target: target, CheckpointID: record.ID,
				LeaseID: leaseID, Name: record.Name, RepoName: repo.Name, Workdir: workdir,
				Strategy: createStrategy, NoReboot: *noReboot, Wait: *wait,
				WaitTimeout: *waitTimeout, Stderr: operationApp.Stderr,
				Persist: func(result NativeCheckpointCreateResult) error {
					return store.WriteNativeProgress(&record, result, *noReboot)
				},
			})
			if image.ID != "" {
				applyNativeImageCheckpointRecord(&record, image, *noReboot)
				record.Native.Metadata = metadata
				if image.managedCheckpoint != nil {
					managed, managedErr := checkpointRecordFromCoordinator(*image.managedCheckpoint, checkpointCoordinatorOrigin(cfg.Coordinator))
					if managedErr != nil {
						return managedErr
					}
					managed.Native.State = image.State
					managed.Capture = record.Capture
					record = managed
				}
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
			if record.coordinatorManaged() {
				return fmt.Errorf("coordinator checkpoint %s is durable; recover with crabbox checkpoint inspect %s: %w", record.ID, record.ID, err)
			}
			return err
		}
		recordWritten = true
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(record)
		}
		if isNativeCheckpointKind(record.Kind) {
			fmt.Fprintf(a.Stdout, "checkpoint created id=%s kind=%s resource=%s state=%s region=%s workdir=%s\n", record.ID, record.Kind, record.Native.ImageID, record.Native.State, blank(record.Native.Region, "-"), record.Workdir)
			return nil
		}
		fmt.Fprintf(a.Stdout, "checkpoint created id=%s kind=%s bytes=%s workdir=%s\n", record.ID, record.Kind, humanBytes(record.ArchiveBytes), record.Workdir)
		return nil
	})
}

type checkpointAudit struct {
	Record        checkpointRecord `json:"record"`
	LocalState    string           `json:"localState"`
	ProviderState string           `json:"providerState,omitempty"`
	NextAction    string           `json:"nextAction"`
	Error         string           `json:"error,omitempty"`
}

type missingCheckpointAudit struct {
	ID            string `json:"id"`
	LocalState    string `json:"localState"`
	ProviderState string `json:"providerState"`
	NextAction    string `json:"nextAction"`
}

type checkpointProviderSnapshotView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Date     string `json:"date,omitempty"`
	State    string `json:"state,omitempty"`
	Current  bool   `json:"current"`
	Parent   string `json:"parent,omitempty"`
	Depth    int    `json:"depth,omitempty"`
	Forkable bool   `json:"forkable"`
	Reason   string `json:"reason,omitempty"`
	Source   string `json:"source"`
}

type checkpointParallelsListOptions struct {
	Tree         bool
	ForkableOnly bool
	CurrentOnly  bool
	Name         string
}

func (a App) checkpointList(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint list", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	verify := fs.Bool("verify", false, "verify local artifacts and provider resources")
	localOnly := fs.Bool("local-only", false, "list local checkpoint records without contacting the coordinator")
	admin := fs.Bool("admin", false, "use the configured coordinator admin credential")
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	id := fs.String("id", "", "provider source VM id/name for provider-native snapshots")
	tree := fs.Bool("tree", true, "show provider-native snapshots as a tree")
	forkableOnly := fs.Bool("forkable-only", false, "show only forkable provider-native snapshots")
	currentOnly := fs.Bool("current", false, "show only the current provider-native snapshot")
	nameFilter := fs.String("name", "", "provider-native snapshot name substring filter")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	ctx = withCheckpointAdmin(ctx, *admin)
	if flagWasSet(fs, "parallels-template") {
		*provider = "parallels"
	}
	setIDFromFirstArg(fs, id)
	if strings.TrimSpace(*id) != "" || flagWasSet(fs, "provider") || flagWasSet(fs, "parallels-template") {
		cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, ProviderResourceID: true})
		if err != nil {
			return err
		}
		if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
			return err
		}
		if strings.TrimSpace(*id) == "" {
			*id = firstNonBlank(cfg.Parallels.SourceID, cfg.Parallels.Source)
		}
		if cfg.Provider == "parallels" && strings.TrimSpace(*id) != "" {
			return a.checkpointListParallelsSnapshots(ctx, cfg, *id, *jsonOut, checkpointParallelsListOptions{Tree: *tree, ForkableOnly: *forkableOnly, CurrentOnly: *currentOnly, Name: *nameFilter})
		}
		if strings.TrimSpace(*id) != "" {
			return exit(2, "checkpoint list --id currently supports provider=parallels")
		}
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	var unreadable map[string]error
	records, err := store.List()
	if err != nil {
		if *localOnly {
			return err
		}
		records, unreadable, err = a.recoverableLocalCheckpointRecords(store)
		if err != nil {
			return err
		}
	}
	if !*localOnly {
		records, err = a.mergeCoordinatorCheckpoints(ctx, store, records)
		if err != nil {
			return err
		}
	}
	for id, readErr := range unreadable {
		recovered := false
		for _, record := range records {
			if record.ID == id && record.coordinatorManaged() {
				recovered = true
				break
			}
		}
		if !recovered {
			return readErr
		}
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
			fmt.Fprintf(a.Stdout, "%s kind=%s name=%q repo=%s lease=%s %s next=%s created=%s last_used=%s\n", record.ID, record.Kind, record.Name, record.Repo.Name, blank(record.LeaseID, "-"), extra, audit.NextAction, record.CreatedAt, record.LastUsedAt)
		}
		return nil
	}
	if *jsonOut {
		if records == nil {
			records = []checkpointRecord{}
		}
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
		fmt.Fprintf(a.Stdout, "%s kind=%s name=%q repo=%s lease=%s %s created=%s last_used=%s\n", record.ID, record.Kind, record.Name, record.Repo.Name, blank(record.LeaseID, "-"), extra, record.CreatedAt, record.LastUsedAt)
	}
	return nil
}

func (a App) mergeCoordinatorCheckpoints(ctx context.Context, store checkpointStore, local []checkpointRecord) ([]checkpointRecord, error) {
	cfg, err := loadConfig()
	if err != nil {
		// Local lifecycle records must remain usable without unrelated provider configuration.
		fmt.Fprintf(a.Stderr, "warning: coordinator checkpoint configuration unavailable; showing partial local inventory with unverified cached state: %v\n", err)
		return local, nil
	}
	if strings.TrimSpace(cfg.Coordinator) == "" {
		return local, nil
	}
	coord, err := configuredCheckpointCoordinatorFor(ctx)
	if err != nil {
		return nil, err
	}
	remote, err := coord.Checkpoints(ctx)
	if err != nil {
		if checkpointRouteUnsupported(err) {
			return local, nil
		}
		if checkpointInventoryUnavailable(ctx, err) {
			fmt.Fprintf(a.Stderr, "warning: coordinator checkpoint inventory unavailable; showing partial local inventory with unverified cached state: %v\n", err)
			return local, nil
		}
		return nil, fmt.Errorf("list coordinator checkpoints: %w", err)
	}
	origin := checkpointCoordinatorOrigin(coord.BaseURL)
	merged := make(map[string]checkpointRecord, len(local)+len(remote))
	for _, record := range local {
		if record.coordinatorManaged() && record.Ownership.Origin == origin {
			continue
		}
		merged[record.ID] = record
	}
	for _, checkpoint := range remote {
		record, convertErr := checkpointRecordFromCoordinator(checkpoint, origin)
		if convertErr != nil {
			return nil, convertErr
		}
		if existing, ok := merged[record.ID]; ok {
			if !existing.coordinatorManaged() {
				return nil, exit(2, "checkpoint %s collides with an operator-managed local checkpoint; refusing to replace local metadata", record.ID)
			}
			if existing.Ownership.Origin != origin || !canRefreshManagedCheckpointCache(existing, record) {
				return nil, exit(2, "checkpoint %s has conflicting coordinator ownership or provider identity", record.ID)
			}
		}
		if paths, pathsErr := store.Paths(record.ID); pathsErr == nil {
			if _, statErr := os.Stat(paths.Meta); statErr == nil {
				existing, _, existingErr := store.Read(record.ID)
				if existingErr != nil {
					fmt.Fprintf(a.Stderr, "warning: preserving unreadable checkpoint %s cache: %v\n", record.ID, existingErr)
					merged[record.ID] = record
					continue
				}
				if !existing.coordinatorManaged() || !canRefreshManagedCheckpointCache(existing, record) {
					return nil, exit(2, "checkpoint %s collides with an existing local checkpoint identity", record.ID)
				}
			}
		}
		merged[record.ID] = record
		if writeErr := writeSafeManagedCheckpointCache(store, record); writeErr != nil {
			var conflict managedCheckpointCacheConflict
			if errors.As(writeErr, &conflict) {
				return nil, writeErr
			}
			fmt.Fprintf(a.Stderr, "warning: could not cache coordinator checkpoint %s: %v\n", record.ID, writeErr)
		}
	}
	records := make([]checkpointRecord, 0, len(merged))
	for _, record := range merged {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339, records[i].CreatedAt)
		right, rightErr := time.Parse(time.RFC3339, records[j].CreatedAt)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.After(right)
		}
		return records[i].ID > records[j].ID
	})
	return records, nil
}

func checkpointInventoryUnavailable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var httpErr CoordinatorHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 500 && httpErr.StatusCode <= 599
	}
	var transportErr *url.Error
	return errors.As(err, &transportErr) || errors.Is(err, context.DeadlineExceeded)
}

func sameManagedCheckpointSource(local, remote checkpointRecord) bool {
	return local.coordinatorManaged() && remote.coordinatorManaged() && local.ID == remote.ID && local.Ownership.Origin == remote.Ownership.Origin && local.Provider == remote.Provider && local.LeaseID == remote.LeaseID
}

func sameManagedCheckpointIdentity(local, remote checkpointRecord) bool {
	if !sameManagedCheckpointSource(local, remote) || local.CreatedAt != remote.CreatedAt {
		return false
	}
	if local.Native.ImageID == "" || remote.Native.ImageID == "" {
		return true
	}
	return local.Native.ImageID == remote.Native.ImageID && local.Native.Resource == remote.Native.Resource && local.Native.AccountID == remote.Native.AccountID && local.Native.Project == remote.Native.Project && local.Native.Region == remote.Native.Region && local.Native.Kind == remote.Native.Kind
}

func canRefreshManagedCheckpointCache(local, remote checkpointRecord) bool {
	// The pre-response journal has a native kind and client timestamp. Reading may
	// hydrate it from the coordinator; it must not authorize deletion on its own.
	return sameManagedCheckpointIdentity(local, remote) || sameManagedCheckpointSource(local, remote) && local.Native.ImageID == "" && isNativeCheckpointKind(local.Kind) && local.Capture == nil
}

func (a App) recoverableLocalCheckpointRecords(store checkpointStore) ([]checkpointRecord, map[string]error, error) {
	entries, err := os.ReadDir(store.root)
	if errors.Is(err, os.ErrNotExist) {
		return []checkpointRecord{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	records := make([]checkpointRecord, 0, len(entries))
	unreadable := make(map[string]error)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, _, readErr := store.Read(entry.Name())
		if isCheckpointNotFound(readErr) {
			continue
		}
		if readErr != nil {
			unreadable[entry.Name()] = readErr
			continue
		}
		records = append(records, record)
	}
	return records, unreadable, nil
}

func (a App) readCheckpointRecord(ctx context.Context, store checkpointStore, id string) (checkpointRecord, checkpointPaths, error) {
	record, paths, localErr := store.Read(id)
	if localErr == nil && !record.coordinatorManaged() {
		return record, paths, nil
	}
	unsafeLocalMetadata := false
	if localErr != nil {
		if paths.Meta == "" {
			paths, _ = store.Paths(id)
		}
		if _, statErr := os.Stat(paths.Meta); statErr == nil || !os.IsNotExist(statErr) {
			unsafeLocalMetadata = true
			fmt.Fprintf(a.Stderr, "warning: preserving unreadable checkpoint %s cache: %v\n", id, localErr)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	if cfg.Coordinator == "" {
		if localErr != nil {
			return checkpointRecord{}, checkpointPaths{}, localErr
		}
		return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint %s requires coordinator %s", id, record.Ownership.Origin)
	}
	coord, err := configuredCheckpointCoordinatorFor(ctx)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	origin := checkpointCoordinatorOrigin(coord.BaseURL)
	if localErr == nil && record.Ownership.Origin != origin {
		return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint %s belongs to coordinator %s, not %s", id, record.Ownership.Origin, origin)
	}
	previous := record
	remote, err := coord.Checkpoint(ctx, id)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	record, err = checkpointRecordFromCoordinator(remote, origin)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	if localErr == nil && (!canRefreshManagedCheckpointCache(previous, record) || previous.Capture != nil && unresolvedCheckpoint(previous)) {
		return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint %s has conflicting local and coordinator provider identities", id)
	}
	if localErr == nil {
		record.Capture = previous.Capture
	}
	if !unsafeLocalMetadata {
		if err := writeSafeManagedCheckpointCache(store, record); err != nil {
			var conflict managedCheckpointCacheConflict
			if errors.As(err, &conflict) {
				return checkpointRecord{}, checkpointPaths{}, err
			}
			fmt.Fprintf(a.Stderr, "warning: could not cache coordinator checkpoint %s: %v\n", id, err)
		}
	}
	paths, err = store.Paths(id)
	return record, paths, err
}

func (a App) checkpointListParallelsSnapshots(ctx context.Context, cfg Config, id string, jsonOut bool, opts checkpointParallelsListOptions) error {
	cfg, vm, err := ResolveParallelsVM(ctx, cfg, nil, id)
	if err != nil {
		return err
	}
	snapshots, err := NewParallelsClient(cfg, nil).Snapshots(ctx, vm.ID)
	if err != nil {
		return err
	}
	views := parallelsSnapshotCheckpointViews(vm.ID, snapshots, opts)
	if jsonOut {
		return json.NewEncoder(a.Stdout).Encode(views)
	}
	if len(views) == 0 {
		kind := "snapshots"
		if opts.ForkableOnly {
			kind = "forkable snapshots"
		}
		fmt.Fprintf(a.Stdout, "no %s source=%s\n", kind, vm.ID)
		return nil
	}
	for _, view := range views {
		extra := ""
		if !view.Forkable && view.Reason != "" {
			extra = " reason=" + strconv.Quote(view.Reason)
		}
		indent := ""
		if opts.Tree && view.Depth > 0 {
			indent = strings.Repeat("  ", view.Depth)
		}
		fmt.Fprintf(a.Stdout, "%sid=%s name=%q state=%s current=%t forkable=%t source=%s date=%s%s\n", indent, view.ID, view.Name, blank(view.State, "-"), view.Current, view.Forkable, view.Source, blank(view.Date, "-"), extra)
	}
	return nil
}

func parallelsSnapshotCheckpointViews(source string, snapshots []ParallelsSnapshot, opts checkpointParallelsListOptions) []checkpointProviderSnapshotView {
	children := make(map[string][]ParallelsSnapshot)
	seen := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		seen[snapshot.ID] = true
		children[snapshot.Parent] = append(children[snapshot.Parent], snapshot)
	}
	for parent := range children {
		sortParallelsSnapshots(children[parent])
	}
	var ordered []ParallelsSnapshot
	var appendTree func(parent string, depth int)
	depths := make(map[string]int, len(snapshots))
	appendTree = func(parent string, depth int) {
		for _, snapshot := range children[parent] {
			depths[snapshot.ID] = depth
			ordered = append(ordered, snapshot)
			appendTree(snapshot.ID, depth+1)
		}
	}
	if opts.Tree {
		appendTree("", 0)
		for _, snapshot := range snapshots {
			if snapshot.Parent != "" && !seen[snapshot.Parent] {
				depths[snapshot.ID] = 0
				ordered = append(ordered, snapshot)
				appendTree(snapshot.ID, 1)
			}
		}
	} else {
		ordered = append([]ParallelsSnapshot(nil), snapshots...)
		sortParallelsSnapshots(ordered)
	}
	views := make([]checkpointProviderSnapshotView, 0, len(ordered))
	nameFilter := strings.ToLower(strings.TrimSpace(opts.Name))
	for _, snapshot := range ordered {
		view := parallelsSnapshotCheckpointView(source, snapshot)
		view.Depth = depths[snapshot.ID]
		if opts.ForkableOnly && !view.Forkable {
			continue
		}
		if opts.CurrentOnly && !view.Current {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(view.Name), nameFilter) {
			continue
		}
		views = append(views, view)
	}
	return views
}

func sortParallelsSnapshots(snapshots []ParallelsSnapshot) {
	sort.SliceStable(snapshots, func(i, j int) bool {
		if snapshots[i].Date != snapshots[j].Date {
			return snapshots[i].Date < snapshots[j].Date
		}
		return snapshots[i].Name < snapshots[j].Name
	})
}

func parallelsSnapshotCheckpointView(source string, snapshot ParallelsSnapshot) checkpointProviderSnapshotView {
	view := checkpointProviderSnapshotView{
		ID:       snapshot.ID,
		Name:     snapshot.Name,
		Date:     snapshot.Date,
		State:    snapshot.State,
		Current:  snapshot.Current,
		Parent:   snapshot.Parent,
		Source:   source,
		Forkable: strings.EqualFold(snapshot.State, "poweroff"),
	}
	if !view.Forkable {
		view.Reason = "linked clones require power-off snapshot"
	}
	return view
}

func applyParallelsCheckpointHostConfig(cfg *Config, record checkpointRecord) {
	setProviderSelection(cfg, "parallels", providerSelectionRecordedRun)
	applyParallelsHostRefConfig(cfg, record.Native.Region)
}

func applyParallelsHostRefConfig(cfg *Config, hostRef string) {
	hostRef = strings.TrimSpace(hostRef)
	if hostRef == "" || hostRef == "local" {
		return
	}
	cfg.Parallels.Host = hostRef
	for _, host := range cfg.Parallels.Hosts {
		if hostRef != host.Host && hostRef != host.Name {
			continue
		}
		cfg.Parallels.Host = host.Host
		cfg.Parallels.HostUser = host.User
		cfg.Parallels.HostKey = host.Key
		cfg.credentialProvenance.parallelsHost = host.hostSource
		cfg.credentialProvenance.parallelsHostKey = host.keySource
		cfg.Parallels.SelectedHost = firstNonBlank(host.Name, host.Host, "local")
		if host.VMRoot != "" {
			cfg.Parallels.VMRoot = host.VMRoot
		}
		return
	}
}

func parallelsHostRefForConfig(cfg Config) string {
	return firstNonBlank(cfg.Parallels.SelectedHost, cfg.Parallels.Host, "local")
}

func (a App) checkpointInspect(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint inspect", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	verify := fs.Bool("verify", false, "verify local artifact or provider resource")
	admin := fs.Bool("admin", false, "use the configured coordinator admin credential")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	ctx = withCheckpointAdmin(ctx, *admin)
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox checkpoint inspect <checkpoint-id>")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, _, err := a.readCheckpointRecord(ctx, store, fs.Arg(0))
	if err != nil {
		if *jsonOut && isCheckpointNotFound(err) {
			claims, readErr := listLeaseClaims()
			if readErr != nil {
				return readErr
			}
			for _, claim := range claims {
				if claim.CheckpointCapture != nil && claim.CheckpointCapture.ID == fs.Arg(0) {
					return json.NewEncoder(a.Stdout).Encode(missingCheckpointAudit{ID: fs.Arg(0), LocalState: "missing", ProviderState: "unknown", NextAction: "reconcile_capture"})
				}
			}
			return json.NewEncoder(a.Stdout).Encode(missingCheckpointAudit{
				ID:            fs.Arg(0),
				LocalState:    "missing",
				ProviderState: "missing",
				NextAction:    "forget",
			})
		}
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
	fmt.Fprintf(stdout, "id=%s\nkind=%s\nname=%s\ncreated=%s\nlast_used=%s\nprovider=%s\nlease=%s\nrepo=%s\nhead=%s\nserver_type=%s\nworkdir=%s\narchive=%s\nbytes=%s\n",
		record.ID, record.Kind, blank(record.Name, "-"), record.CreatedAt, record.LastUsedAt, blank(record.Provider, "-"), blank(record.LeaseID, "-"), blank(record.Repo.Name, "-"), blank(record.Repo.Head, "-"), blank(record.ServerType, "-"), blank(record.Workdir, "-"), blank(record.ArchivePath, "-"), humanBytes(record.ArchiveBytes))
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

func (a App) checkpointPolicy(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint policy", a.Stderr)
	manual := fs.Bool("manual", false, "disable automatic checkpoint expiry")
	expireUnusedAfter := fs.String("expire-unused-after", "", "expire the checkpoint after this period without a successful fork")
	jsonOut := fs.Bool("json", false, "print JSON")
	admin := fs.Bool("admin", false, "use the configured coordinator admin credential")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	ctx = withCheckpointAdmin(ctx, *admin)
	if fs.NArg() != 1 || *manual == (strings.TrimSpace(*expireUnusedAfter) != "") {
		return exit(2, "usage: crabbox checkpoint policy <checkpoint-id> (--manual | --expire-unused-after <duration>)")
	}
	duration, err := parseCheckpointRetentionDuration(*expireUnusedAfter)
	if err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, _, err := a.readCheckpointRecord(ctx, store, fs.Arg(0))
	if err != nil {
		return err
	}
	if !record.coordinatorManaged() {
		return exit(2, "checkpoint %s is operator-managed; retention policy requires a coordinator-managed brokered native checkpoint", record.ID)
	}
	coord, err := configuredCheckpointCoordinatorFor(ctx)
	if err != nil {
		return err
	}
	updated, err := coord.UpdateCheckpointRetention(ctx, record.ID, checkpointRetentionFromDuration(duration))
	if err != nil {
		return err
	}
	record, err = checkpointRecordFromCoordinator(updated, record.Ownership.Origin)
	if err != nil {
		return err
	}
	if err := writeSafeManagedCheckpointCache(store, record); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not refresh checkpoint %s cache after policy update: %v\n", record.ID, err)
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(record)
	}
	if *manual {
		fmt.Fprintf(a.Stdout, "checkpoint policy id=%s retention=manual\n", record.ID)
	} else {
		fmt.Fprintf(a.Stdout, "checkpoint policy id=%s retention=expire-unused unused_for=%s\n", record.ID, duration)
	}
	return nil
}

func (a App) checkpointRestore(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint restore", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	snapshot := fs.String("snapshot", "", "provider-native snapshot name or id")
	dryRun := fs.Bool("dry-run", false, "show provider-native restore target without switching snapshots")
	workdirOverride := fs.String("workdir", "", "remote restore workdir")
	clear := fs.Bool("clear", true, "clear the remote workdir before restoring")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if flagWasSet(fs, "parallels-template") {
		*provider = "parallels"
	}
	if strings.TrimSpace(*snapshot) != "" {
		if fs.NArg() != 0 {
			return exit(2, "usage: crabbox checkpoint restore --provider parallels --id <vm-or-lease> --snapshot <name-or-id>")
		}
		cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, ProviderResourceID: true})
		if err != nil {
			return err
		}
		if cfg.Provider != "parallels" {
			return exit(2, "checkpoint restore --snapshot currently supports provider=parallels")
		}
		if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
			return err
		}
		if strings.TrimSpace(*id) == "" {
			*id = firstNonBlank(cfg.Parallels.SourceID, cfg.Parallels.Source)
		}
		if err := requireLeaseID(*id, "crabbox checkpoint restore --provider parallels --id <vm-or-lease> --snapshot <name-or-id>", cfg); err != nil {
			return err
		}
		cfg, vm, err := ResolveParallelsVM(ctx, cfg, nil, *id)
		if err != nil {
			return err
		}
		snapshot, err := NewParallelsClient(cfg, nil).Snapshot(ctx, vm.ID, *snapshot)
		if err != nil {
			return err
		}
		if *dryRun {
			fmt.Fprintf(a.Stdout, "would restore provider=parallels source=%s snapshot=%s name=%q\n", vm.ID, snapshot.ID, snapshot.Name)
			return nil
		}
		if err := NewParallelsClient(cfg, nil).SwitchSnapshot(ctx, vm.ID, snapshot.ID, true); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "checkpoint restored provider=parallels source=%s snapshot=%s\n", vm.ID, snapshot.ID)
		return nil
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
	if unresolvedCheckpoint(record) {
		return exit(2, "checkpoint %s capture is unresolved; reconcile it before fork or restore", record.ID)
	}
	if record.Kind != checkpointKindArchive {
		if isNativeCheckpointKind(record.Kind) {
			if record.Kind == checkpointKindParallels {
				cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
				if err != nil {
					return err
				}
				applyParallelsCheckpointHostConfig(&cfg, record)
				if err := requireLeaseID(*id, "crabbox checkpoint restore <checkpoint-id> --id <lease-id-or-slug>", cfg); err != nil {
					return err
				}
				if *dryRun {
					fmt.Fprintf(a.Stdout, "would restore checkpoint id=%s lease=%s snapshot=%s\n", record.ID, *id, record.Native.ImageID)
					return nil
				}
				server, _, _, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, true, *reclaim)
				if err != nil {
					return err
				}
				restoreCfg := cfg
				applyParallelsHostRefConfig(&restoreCfg, firstNonBlank(server.Labels["host"], cfg.Parallels.Host))
				if err := NewParallelsClient(restoreCfg, nil).SwitchSnapshot(ctx, server.CloudID, record.Native.ImageID, true); err != nil {
					return err
				}
				if err := recordCheckpointUse(store, &record); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "checkpoint restored id=%s lease=%s snapshot=%s\n", record.ID, blank(server.Labels["lease"], server.CloudID), record.Native.ImageID)
				return nil
			}
			if record.Kind == checkpointKindDockerCommit {
				return exit(2, "checkpoint %s is a docker-commit image; use crabbox checkpoint fork %s to create a lease, crabbox checkpoint inspect %s --verify to verify it, or crabbox checkpoint delete %s to remove it", record.ID, record.ID, record.ID, record.ID)
			}
			return exit(2, "checkpoint %s is a VM image; use crabbox checkpoint fork %s to create a lease from it", record.ID, record.ID)
		}
		return exit(2, "checkpoint %s has kind=%s; restore requires %s", record.ID, record.Kind, checkpointKindArchive)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
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
	leaseID := strings.TrimSpace(*id)
	workdirOverrideValue := strings.TrimSpace(*workdirOverride)
	if *dryRun {
		claim, ok, claimErr := resolveLeaseClaimForProvider(leaseID, canonicalClaimProvider(cfg.Provider))
		if claimErr != nil {
			return claimErr
		}
		if ok {
			applyStoredLeaseClaimConfig(&cfg, claim)
			leaseID = firstNonBlank(claim.LeaseID, leaseID)
		}
		workdir := checkpointRestoreWorkdir(cfg, leaseID, repo.Name, record.Workdir, workdirOverrideValue)
		fmt.Fprintf(a.Stdout, "would restore checkpoint id=%s lease=%s workdir=%s clear=%t\n", record.ID, leaseID, workdir, *clear)
		return nil
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepoWithConfig(ctx, &cfg, *id, true, *reclaim)
	if err != nil {
		return err
	}
	workdir := checkpointRestoreWorkdir(cfg, leaseID, repo.Name, record.Workdir, workdirOverrideValue)
	if err := a.claimResolvedLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, &server, target, repo.Root, *reclaim); err != nil {
		return err
	}
	if err := restoreCheckpointArchive(ctx, target, checkpointArchivePath(paths, record), record.ID, workdir, *clear); err != nil {
		return err
	}
	if err := recordCheckpointUse(store, &record); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint restored id=%s lease=%s workdir=%s\n", record.ID, leaseID, workdir)
	return nil
}

func (a App) checkpointFork(ctx context.Context, args []string) (err error) {
	forkArgs, runArgs := splitCheckpointForkRunArgs(args)
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint fork", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	requestedLeaseID := fs.String("lease-id", "", "fixed lease ID for idempotent external-provider orchestration")
	jsonOut := fs.Bool("json", false, "print forked leases as JSON")
	id := fs.String("id", "", "provider source VM id/name for provider-native fork")
	snapshot := fs.String("snapshot", "", "provider-native snapshot name or id")
	keep := fs.Bool("keep", true, "keep forked lease after restore")
	count := fs.Int("count", 1, "number of forked leases to create")
	dryRun := fs.Bool("dry-run", false, "show provider-native fork target without cloning")
	workdirOverride := fs.String("workdir", "", "remote restore workdir")
	clear := fs.Bool("clear", true, "clear the remote workdir before restoring")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	admin := fs.Bool("admin", false, "use the configured coordinator admin credential")
	if err := parseInterspersedFlags(fs, forkArgs); err != nil {
		return err
	}
	ctx = withCheckpointAdmin(ctx, *admin)
	if flagWasSet(fs, "parallels-template") {
		*leaseFlags.Provider = "parallels"
	}
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
		return err
	}
	if *count < 1 {
		return exit(2, "--count must be at least 1")
	}
	fixedLeaseID := strings.TrimSpace(*requestedLeaseID)
	if flagWasSet(fs, "lease-id") {
		if !canonicalLeaseIDPattern.MatchString(fixedLeaseID) {
			return exit(2, "--lease-id must match cbx_<12 lowercase hex characters>")
		}
		if *count > 1 {
			return exit(2, "--lease-id cannot be combined with --count greater than 1")
		}
		if !*keep {
			return exit(2, "--lease-id cannot be combined with --keep=false")
		}
		if len(runArgs) != 0 {
			return exit(2, "--lease-id cannot be combined with checkpoint fork commands")
		}
		if flagWasSet(fs, "workdir") {
			return exit(2, "--lease-id cannot be combined with --workdir")
		}
	}
	if *jsonOut && *dryRun {
		return exit(2, "--json cannot be combined with --dry-run")
	}
	if strings.TrimSpace(*snapshot) != "" || flagWasSet(fs, "parallels-template") {
		if fixedLeaseID != "" {
			return exit(2, "provider=parallels does not support --lease-id fork with a direct snapshot")
		}
		if fs.NArg() != 0 {
			return exit(2, "usage: crabbox checkpoint fork --provider parallels --id <source-vm> --snapshot <name-or-id> [--slug <slug>]")
		}
		return a.checkpointForkParallelsSnapshot(ctx, fs, leaseFlags, *id, *snapshot, *keep, *reclaim, requestedSlug, *count, *dryRun, *jsonOut, runArgs)
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox checkpoint fork <checkpoint-id> [--class <class>]")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, paths, err := a.readCheckpointRecord(ctx, store, fs.Arg(0))
	if err != nil {
		return err
	}
	if fixedLeaseID != "" && !isNativeCheckpointKind(record.Kind) {
		if record.Kind == checkpointKindArchive {
			return exit(2, "archive checkpoints do not support --lease-id")
		}
		return exit(2, "checkpoint kind=%s does not support --lease-id", record.Kind)
	}
	if unresolvedCheckpoint(record) {
		return exit(2, "checkpoint %s capture is unresolved; reconcile it before fork", record.ID)
	}
	if record.Capture != nil && record.Capture.DiscardFailed {
		return exit(2, "checkpoint %s image was discarded; it cannot be forked", record.ID)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	nativeCheckpoint := isNativeCheckpointKind(record.Kind)
	if nativeCheckpoint && record.TargetOS == targetMacOS && !flagWasSet(fs, "market") {
		cfg.Capacity.Market = "on-demand"
	}
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return err
	}
	if record.coordinatorManaged() {
		if err := bindCheckpointCoordinatorCredential(ctx, &cfg); err != nil {
			return err
		}
	}
	if record.Kind != checkpointKindArchive && !nativeCheckpoint {
		return exit(2, "checkpoint %s has kind=%s; fork requires %s or a native image checkpoint", record.ID, record.Kind, checkpointKindArchive)
	}
	if nativeCheckpoint {
		if nativeCheckpointResourceID(record) == "" {
			return exit(2, "checkpoint %s is pending; native provider resource is not recorded yet", record.ID)
		}
		if err := applyNativeCheckpointForkConfigAndFlags(&cfg, fs, record, leaseFlags.ProviderFlags); err != nil {
			return err
		}
	}
	if *dryRun {
		if !providerSelectionIsActionable(cfg) {
			return exit(2, "%s", providerSelectionRequiredDiagnostic)
		}
		for i := 1; i <= *count; i++ {
			slug := checkpointForkFanoutSlug(requestedSlug, i, *count)
			expandedCommand := checkpointForkRunCommand(runArgs, checkpointForkRunContext{Index: i, Total: *count, Slug: slug})
			commandSuffix := ""
			if len(expandedCommand) > 0 {
				commandSuffix = " command=" + strconv.Quote(runCommandDisplay(expandedCommand, false))
			}
			if *count == 1 {
				fmt.Fprintf(a.Stdout, "would fork checkpoint id=%s provider=%s resource=%s slug=%s keep=%t%s\n", record.ID, cfg.Provider, blank(nativeCheckpointResourceID(record), "-"), blank(slug, "-"), *keep, commandSuffix)
			} else {
				fmt.Fprintf(a.Stdout, "would fork checkpoint id=%s provider=%s resource=%s slug=%s keep=%t index=%d/%d%s\n", record.ID, cfg.Provider, blank(nativeCheckpointResourceID(record), "-"), blank(slug, "-"), *keep, i, *count, commandSuffix)
			}
		}
		return nil
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	operationApp := a
	if *jsonOut {
		operationApp.Stdout = a.Stderr
	}
	backend, err := loadBackend(cfg, runtimeForApp(operationApp))
	if err != nil {
		return err
	}
	if fixedLeaseID != "" {
		capable, ok := backend.(IdempotentLeaseIDBackend)
		checkpointCapable, checkpointOK := backend.(CheckpointLeaseIDBackend)
		if !ok || !capable.SupportsRequestedLeaseID() || !checkpointOK || !checkpointCapable.SupportsRequestedCheckpointID() {
			return exit(2, "provider=%s does not support --lease-id fork", backend.Spec().Name)
		}
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support checkpoint fork", backend.Spec().Name)
	}
	results := make([]checkpointForkResult, 0, *count)
	for i := 1; i <= *count; i++ {
		slug := checkpointForkFanoutSlug(requestedSlug, i, *count)
		runOpts := checkpointForkRunOptions{Command: runArgs, Index: i, Total: *count, JSON: *jsonOut, Results: &results}
		if err := operationApp.checkpointForkRecordOnce(ctx, cfg, backend, sshBackend, repo, store, &record, paths, *keep, *reclaim, fixedLeaseID, slug, strings.TrimSpace(*workdirOverride), *clear, runOpts); err != nil {
			if *jsonOut && len(results) != 0 {
				if outputErr := writeCheckpointForkResults(a.Stdout, results, *count); outputErr != nil {
					return fmt.Errorf("%v (failed to report acquired checkpoint forks: %w)", err, outputErr)
				}
			}
			return err
		}
	}
	if *jsonOut {
		return writeCheckpointForkResults(a.Stdout, results, *count)
	}
	return nil
}

func splitCheckpointForkRunArgs(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

type checkpointForkRunOptions struct {
	Command []string
	Index   int
	Total   int
	JSON    bool
	Results *[]checkpointForkResult
}

type checkpointForkResult struct {
	CheckpointID string `json:"checkpointId"`
	LeaseID      string `json:"leaseId"`
	Slug         string `json:"slug"`
	Provider     string `json:"provider"`
	Workdir      string `json:"workdir"`
}

func writeCheckpointForkResults(stdout io.Writer, results []checkpointForkResult, requestedCount int) error {
	if requestedCount == 1 {
		return json.NewEncoder(stdout).Encode(results[0])
	}
	return json.NewEncoder(stdout).Encode(results)
}

type checkpointForkRunContext struct {
	Index   int
	Total   int
	LeaseID string
	Slug    string
}

func checkpointForkRunCommand(command []string, runCtx checkpointForkRunContext) []string {
	if len(command) == 0 {
		return nil
	}
	replacements := map[string]string{
		"{{index}}": strconv.Itoa(runCtx.Index),
		"{{total}}": strconv.Itoa(runCtx.Total),
		"{{lease}}": runCtx.LeaseID,
		"{{slug}}":  runCtx.Slug,
	}
	expanded := make([]string, 0, len(command))
	for _, arg := range command {
		for from, to := range replacements {
			arg = strings.ReplaceAll(arg, from, to)
		}
		expanded = append(expanded, arg)
	}
	return expanded
}

func checkpointForkFanoutSlug(requestedSlug string, index, total int) string {
	requestedSlug = strings.TrimSpace(requestedSlug)
	if total <= 1 || requestedSlug == "" {
		return requestedSlug
	}
	width := len(strconv.Itoa(total))
	suffix := fmt.Sprintf("-%0*d", width, index)
	baseLimit := maxRequestedLeaseSlugLength - len(suffix)
	base := strings.Trim(requestedSlug, "-")
	if len(base) > baseLimit {
		base = strings.Trim(base[:baseLimit], "-")
	}
	if base == "" {
		base = "fork"
	}
	return base + suffix
}

type checkpointForkProvision struct {
	Lease   LeaseTarget
	Workdir string
	Release func(context.Context)
}

func (a App) provisionCheckpointFork(ctx context.Context, cfg Config, backend Backend, sshBackend SSHLeaseBackend, repo Repo, record checkpointRecord, paths checkpointPaths, keep, reclaim bool, requestedSlug, workdirOverride string, clear bool) (checkpointForkProvision, error) {
	return a.provisionCheckpointForkWithLeaseID(ctx, cfg, backend, sshBackend, repo, record, paths, keep, reclaim, "", requestedSlug, workdirOverride, clear, nil)
}

func (a App) provisionCheckpointForkWithLeaseID(ctx context.Context, cfg Config, backend Backend, sshBackend SSHLeaseBackend, repo Repo, record checkpointRecord, paths checkpointPaths, keep, reclaim bool, requestedLeaseID, requestedSlug, workdirOverride string, clear bool, onAcquired func(LeaseTarget)) (checkpointForkProvision, error) {
	if record.coordinatorManaged() {
		return a.provisionManagedCheckpointFork(ctx, cfg, backend, sshBackend, repo, record, paths, keep, reclaim, requestedLeaseID, requestedSlug, workdirOverride, clear, onAcquired)
	}
	return a.provisionCheckpointForkWithoutClaim(ctx, cfg, backend, sshBackend, repo, record, paths, keep, reclaim, requestedLeaseID, requestedSlug, workdirOverride, clear, onAcquired)
}

func (a App) provisionManagedCheckpointFork(ctx context.Context, cfg Config, backend Backend, sshBackend SSHLeaseBackend, repo Repo, record checkpointRecord, paths checkpointPaths, keep, reclaim bool, requestedLeaseID, requestedSlug, workdirOverride string, clear bool, onAcquired func(LeaseTarget)) (checkpointForkProvision, error) {
	coord, err := configuredCheckpointCoordinatorFor(ctx)
	if err != nil {
		return checkpointForkProvision{}, err
	}
	if origin := checkpointCoordinatorOrigin(coord.BaseURL); origin != record.Ownership.Origin {
		return checkpointForkProvision{}, exit(2, "checkpoint %s belongs to coordinator %s, not %s", record.ID, record.Ownership.Origin, origin)
	}
	claim, err := coord.BeginCheckpointUse(ctx, record.ID)
	if err != nil {
		return checkpointForkProvision{}, err
	}
	claimCtx, cancel := context.WithCancel(ctx)
	renewalCtx, stopRenewal := context.WithCancel(ctx)
	var leaseCreated atomic.Bool
	renewalDone := make(chan struct{})
	renewalErrors := make(chan error, 1)
	go func() {
		defer close(renewalDone)
		interval := 40 * time.Second
		if expires, parseErr := time.Parse(time.RFC3339, claim.ExpiresAt); parseErr == nil {
			if remaining := time.Until(expires) / 3; remaining > 0 && remaining < interval {
				interval = remaining
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewalCtx.Done():
				return
			case <-ticker.C:
				if _, _, renewErr := coord.checkpointUseAction(renewalCtx, record.ID, claim.Claim, "renew"); renewErr != nil {
					if leaseCreated.Load() || renewalCtx.Err() != nil {
						return
					}
					var httpErr CoordinatorHTTPError
					var response struct {
						Error string `json:"error"`
					}
					if errors.As(renewErr, &httpErr) &&
						httpErr.StatusCode == http.StatusConflict &&
						json.Unmarshal([]byte(httpErr.Message), &response) == nil &&
						response.Error == "checkpoint_claim_invalid" {
						return
					}
					select {
					case renewalErrors <- renewErr:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	claimed := true
	defer func() {
		stopRenewal()
		cancel()
		<-renewalDone
		if claimed && !leaseCreated.Load() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			_, _, _ = coord.checkpointUseAction(cleanupCtx, record.ID, claim.Claim, "abort")
		}
	}()
	provisionContext := withCheckpointLeaseProvisioned(claimCtx, record.ID, claim.Claim, func() {
		leaseCreated.Store(true)
		stopRenewal()
	})
	provision, err := a.provisionCheckpointForkWithoutClaim(provisionContext, cfg, backend, sshBackend, repo, record, paths, keep, reclaim, requestedLeaseID, requestedSlug, workdirOverride, clear, onAcquired)
	if err != nil {
		select {
		case renewErr := <-renewalErrors:
			return checkpointForkProvision{}, fmt.Errorf("renew checkpoint use claim: %w", renewErr)
		default:
			return checkpointForkProvision{}, err
		}
	}
	stopRenewal()
	<-renewalDone
	select {
	case renewErr := <-renewalErrors:
		if requestedLeaseID == "" {
			provision.Release(context.Background())
		}
		return checkpointForkProvision{}, fmt.Errorf("renew checkpoint use claim: %w", renewErr)
	default:
	}
	claimed = false
	completed, err := coord.Checkpoint(ctx, record.ID)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not refresh checkpoint %s after successful coordinator-owned use: %v\n", record.ID, err)
		return provision, nil
	}
	updated, err := checkpointRecordFromCoordinator(completed, record.Ownership.Origin)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not refresh checkpoint %s cache after successful use: %v\n", record.ID, err)
		return provision, nil
	}
	if store, storeErr := defaultCheckpointStore(); storeErr != nil {
		fmt.Fprintf(a.Stderr, "warning: could not refresh checkpoint %s cache after successful use: %v\n", record.ID, storeErr)
	} else if writeErr := writeSafeManagedCheckpointCache(store, updated); writeErr != nil {
		fmt.Fprintf(a.Stderr, "warning: could not refresh checkpoint %s cache after successful use: %v\n", record.ID, writeErr)
	}
	return provision, nil
}

type managedCheckpointCacheConflict struct{ error }

func writeSafeManagedCheckpointCache(store checkpointStore, record checkpointRecord) error {
	if !record.coordinatorManaged() {
		return exit(2, "checkpoint %s is not coordinator-managed", record.ID)
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return err
	}
	return store.withLock(record.ID, false, func() error {
		existing, _, err := store.Read(record.ID)
		if err == nil {
			if !canRefreshManagedCheckpointCache(existing, record) || existing.Capture != nil && unresolvedCheckpoint(existing) {
				return managedCheckpointCacheConflict{exit(2, "checkpoint %s has conflicting local ownership, provider identity, or unresolved capture", record.ID)}
			}
			record.Capture = existing.Capture
		} else if !isCheckpointNotFound(err) {
			return fmt.Errorf("preserving unreadable checkpoint cache: %w", err)
		}
		return store.Write(record)
	})
}

func (a App) provisionCheckpointForkWithoutClaim(ctx context.Context, cfg Config, backend Backend, sshBackend SSHLeaseBackend, repo Repo, record checkpointRecord, paths checkpointPaths, keep, reclaim bool, requestedLeaseID, requestedSlug, workdirOverride string, clear bool, onAcquired func(LeaseTarget)) (checkpointForkProvision, error) {
	checkpointID := ""
	if requestedLeaseID != "" {
		checkpointID = record.ID
	}
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{
		Repo: repo, Options: leaseOptionsFromConfig(cfg), Keep: keep, Reclaim: reclaim,
		RequestedLeaseID: requestedLeaseID, RequestedCheckpointID: checkpointID, RequestedSlug: requestedSlug,
	})
	if err != nil {
		return checkpointForkProvision{}, err
	}
	if onAcquired != nil {
		onAcquired(lease)
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	var releaseOnce sync.Once
	release := func(releaseCtx context.Context) {
		releaseOnce.Do(func() {
			a.releaseBackendLeaseBestEffort(releaseCtx, sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		})
	}
	rollback := func() {
		// Fixed acquisition can adopt prior work; preserve its known ID for replay.
		if requestedLeaseID == "" {
			release(context.Background())
		}
	}
	applyResolvedServerConfig(&cfg, server)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, &server, target, repo.Root, reclaim); err != nil {
		rollback()
		return checkpointForkProvision{}, err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		rollback()
		return checkpointForkProvision{}, err
	} else {
		target = resolved.Target
		if resolved.FallbackReason != "" {
			fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
		}
	}
	forkLease := LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator}
	if isNativeCheckpointKind(record.Kind) {
		if record.TargetOS == targetWindows {
			return checkpointForkProvision{Lease: forkLease, Release: release}, nil
		}
		workdir := nativeCheckpointForkWorkdir(cfg, leaseID, repo.Name, workdirOverride)
		if err := validateCheckpointForkWorkdirs(ctx, backend, forkLease, record.Workdir, workdir); err != nil {
			rollback()
			return checkpointForkProvision{}, err
		}
		if err := relocateNativeCheckpointWorkdir(ctx, target, record.Workdir, workdir); err != nil {
			rollback()
			return checkpointForkProvision{}, err
		}
		return checkpointForkProvision{Lease: forkLease, Workdir: workdir, Release: release}, nil
	}
	workdir := strings.TrimSpace(workdirOverride)
	if workdir == "" {
		workdir = defaultCheckpointRestoreWorkdir(cfg, leaseID, repo.Name, record.Workdir)
	}
	if err := validateCheckpointForkWorkdirs(ctx, backend, forkLease, workdir); err != nil {
		rollback()
		return checkpointForkProvision{}, err
	}
	if err := restoreCheckpointArchive(ctx, target, checkpointArchivePath(paths, record), record.ID, workdir, clear); err != nil {
		rollback()
		return checkpointForkProvision{}, err
	}
	return checkpointForkProvision{Lease: forkLease, Workdir: workdir, Release: release}, nil
}

func (a App) checkpointForkRecordOnce(ctx context.Context, cfg Config, backend Backend, sshBackend SSHLeaseBackend, repo Repo, store checkpointStore, record *checkpointRecord, paths checkpointPaths, keep, reclaim bool, requestedLeaseID, requestedSlug, workdirOverride string, clear bool, runOpts checkpointForkRunOptions) error {
	if requestedLeaseID != "" {
		unlock, err := lockFixedLeaseAcquisition(ctx, requestedLeaseID)
		if err != nil {
			return err
		}
		defer unlock()
	}
	resultIndex := -1
	var onAcquired func(LeaseTarget)
	if runOpts.JSON && runOpts.Results != nil {
		onAcquired = func(lease LeaseTarget) {
			workdir := ""
			if record.Kind == checkpointKindArchive {
				workdir = strings.TrimSpace(workdirOverride)
				if workdir == "" {
					workdir = defaultCheckpointRestoreWorkdir(cfg, lease.LeaseID, repo.Name, record.Workdir)
				}
			} else if record.TargetOS != targetWindows {
				workdir = nativeCheckpointForkWorkdir(cfg, lease.LeaseID, repo.Name, workdirOverride)
			}
			resultIndex = len(*runOpts.Results)
			*runOpts.Results = append(*runOpts.Results, checkpointForkResult{
				CheckpointID: record.ID,
				LeaseID:      lease.LeaseID,
				Slug:         serverSlug(lease.Server),
				Provider:     firstNonBlank(lease.Server.Provider, cfg.Provider),
				Workdir:      workdir,
			})
		}
	}
	provision, err := a.provisionCheckpointForkWithLeaseID(ctx, cfg, backend, sshBackend, repo, *record, paths, keep, reclaim, requestedLeaseID, requestedSlug, workdirOverride, clear, onAcquired)
	if err != nil {
		return err
	}
	defer func() {
		if !keep {
			provision.Release(context.Background())
		}
	}()
	if record.coordinatorManaged() {
		if refreshed, _, readErr := store.Read(record.ID); readErr == nil {
			*record = refreshed
		}
	} else {
		if err := recordCheckpointUse(store, record); err != nil {
			if requestedLeaseID == "" {
				provision.Release(context.Background())
			}
			return err
		}
	}
	leaseID := provision.Lease.LeaseID
	slug := serverSlug(provision.Lease.Server)
	if runOpts.Results != nil {
		result := checkpointForkResult{
			CheckpointID: record.ID,
			LeaseID:      leaseID,
			Slug:         slug,
			Provider:     firstNonBlank(provision.Lease.Server.Provider, cfg.Provider),
			Workdir:      provision.Workdir,
		}
		if resultIndex >= 0 {
			(*runOpts.Results)[resultIndex] = result
		} else {
			*runOpts.Results = append(*runOpts.Results, result)
		}
	}
	if runOpts.JSON {
		return a.runCheckpointForkCommand(ctx, leaseID, slug, runOpts)
	}
	if isNativeCheckpointKind(record.Kind) {
		fmt.Fprintf(a.Stdout, "checkpoint forked id=%s lease=%s slug=%s image=%s workdir=%s\n", record.ID, leaseID, blank(slug, "-"), nativeCheckpointResourceID(*record), blank(provision.Workdir, "-"))
	} else {
		fmt.Fprintf(a.Stdout, "checkpoint forked id=%s lease=%s slug=%s workdir=%s\n", record.ID, leaseID, blank(slug, "-"), provision.Workdir)
	}
	return a.runCheckpointForkCommand(ctx, leaseID, slug, runOpts)
}

func (a App) runCheckpointForkCommand(ctx context.Context, leaseID, slug string, opts checkpointForkRunOptions) error {
	runCtx := checkpointForkRunContext{Index: opts.Index, Total: opts.Total, LeaseID: leaseID, Slug: slug}
	command := checkpointForkRunCommand(opts.Command, runCtx)
	if len(command) == 0 {
		return nil
	}
	if !opts.JSON {
		fmt.Fprintf(a.Stdout, "checkpoint fork command lease=%s slug=%s index=%d/%d command=%s\n", leaseID, blank(slug, "-"), opts.Index, opts.Total, runCommandDisplay(command, false))
	}
	runArgs := []string{"--id", leaseID, "--keep", "--"}
	runArgs = append(runArgs, command...)
	return a.runCommand(ctx, runArgs)
}

func validateCheckpointForkWorkdirs(ctx context.Context, backend Backend, lease LeaseTarget, workdirs ...string) error {
	validator, ok := backend.(CheckpointForkWorkdirValidator)
	if !ok {
		return nil
	}
	for _, workdir := range workdirs {
		if strings.TrimSpace(workdir) == "" {
			continue
		}
		if err := validator.ValidateCheckpointForkWorkdir(ctx, lease, workdir); err != nil {
			return err
		}
	}
	return nil
}

func (a App) checkpointForkParallelsSnapshot(ctx context.Context, fs *flag.FlagSet, leaseFlags leaseCreateFlagValues, source, snapshot string, keep, reclaim bool, requestedSlug string, count int, dryRun, jsonOut bool, runArgs []string) (err error) {
	cfg, err := loadCheckpointForkParallelsConfig(fs, leaseFlags)
	if err != nil {
		return err
	}
	if cfg.Provider != "parallels" {
		return exit(2, "checkpoint fork --snapshot currently supports provider=parallels")
	}
	if strings.TrimSpace(source) == "" {
		source = firstNonBlank(cfg.Parallels.SourceID, cfg.Parallels.Source)
	}
	if strings.TrimSpace(snapshot) == "" {
		snapshot = firstNonBlank(cfg.Parallels.SourceSnapshotID, cfg.Parallels.SourceSnapshot)
	}
	if strings.TrimSpace(source) == "" {
		return exit(2, "usage: crabbox checkpoint fork --provider parallels --id <source-vm> --snapshot <name-or-id> [--slug <slug>]")
	}
	if strings.TrimSpace(snapshot) == "" {
		return exit(2, "checkpoint fork --provider parallels requires --snapshot or a template sourceSnapshot")
	}
	cfg.Parallels.Source = strings.TrimSpace(source)
	cfg.Parallels.SourceID = ""
	cfg.Parallels.SourceSnapshot = strings.TrimSpace(snapshot)
	cfg.Parallels.SourceSnapshotID = ""
	if dryRun {
		selected, err := SelectParallelsFleetConfig(ctx, cfg, nil, cfg.Parallels.Source)
		if err != nil {
			return err
		}
		snapshot, err := NewParallelsClient(selected, nil).Snapshot(ctx, cfg.Parallels.Source, cfg.Parallels.SourceSnapshot)
		if err != nil {
			return err
		}
		if err := validateParallelsSnapshotCloneMode(snapshot, cfg.Parallels.CloneMode); err != nil {
			return err
		}
		for i := 1; i <= count; i++ {
			slug := checkpointForkFanoutSlug(requestedSlug, i, count)
			expandedCommand := checkpointForkRunCommand(runArgs, checkpointForkRunContext{Index: i, Total: count, Slug: slug})
			commandSuffix := ""
			if len(expandedCommand) > 0 {
				commandSuffix = " command=" + strconv.Quote(runCommandDisplay(expandedCommand, false))
			}
			if count == 1 {
				fmt.Fprintf(a.Stdout, "would fork provider=parallels host=%s source=%s snapshot=%s name=%q slug=%s%s\n", parallelsHostRefForConfig(selected), cfg.Parallels.Source, snapshot.ID, snapshot.Name, blank(slug, "-"), commandSuffix)
			} else {
				fmt.Fprintf(a.Stdout, "would fork provider=parallels host=%s source=%s snapshot=%s name=%q slug=%s index=%d/%d%s\n", parallelsHostRefForConfig(selected), cfg.Parallels.Source, snapshot.ID, snapshot.Name, blank(slug, "-"), i, count, commandSuffix)
			}
		}
		return nil
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	operationApp := a
	if jsonOut {
		operationApp.Stdout = a.Stderr
	}
	backend, err := loadBackend(cfg, runtimeForApp(operationApp))
	if err != nil {
		return err
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support checkpoint fork", backend.Spec().Name)
	}
	results := make([]checkpointForkResult, 0, count)
	for i := 1; i <= count; i++ {
		slug := checkpointForkFanoutSlug(requestedSlug, i, count)
		runOpts := checkpointForkRunOptions{Command: runArgs, Index: i, Total: count, JSON: jsonOut, Results: &results}
		if err := operationApp.checkpointForkParallelsSnapshotOnce(ctx, cfg, sshBackend, repo, source, snapshot, keep, reclaim, slug, runOpts); err != nil {
			if jsonOut && len(results) != 0 {
				if outputErr := writeCheckpointForkResults(a.Stdout, results, count); outputErr != nil {
					return fmt.Errorf("%v (failed to report acquired checkpoint forks: %w)", err, outputErr)
				}
			}
			return err
		}
	}
	if jsonOut {
		return writeCheckpointForkResults(a.Stdout, results, count)
	}
	return nil
}

func loadCheckpointForkParallelsConfig(fs *flag.FlagSet, leaseFlags leaseCreateFlagValues) (Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, err
	}
	setProviderSelection(&cfg, "parallels", providerSelectionFlag)
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (a App) checkpointForkParallelsSnapshotOnce(ctx context.Context, cfg Config, sshBackend SSHLeaseBackend, repo Repo, source, snapshot string, keep, reclaim bool, requestedSlug string, runOpts checkpointForkRunOptions) (err error) {
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: leaseOptionsFromConfig(cfg), Keep: keep, Reclaim: reclaim, RequestedSlug: requestedSlug})
	if err != nil {
		return err
	}
	resultIndex := -1
	if runOpts.JSON && runOpts.Results != nil {
		resultIndex = len(*runOpts.Results)
		*runOpts.Results = append(*runOpts.Results, checkpointForkResult{
			LeaseID:  lease.LeaseID,
			Slug:     serverSlug(lease.Server),
			Provider: cfg.Provider,
		})
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	released := false
	release := func(releaseCtx context.Context) {
		if released {
			return
		}
		released = true
		a.releaseBackendLeaseBestEffort(releaseCtx, sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
	}
	defer func() {
		if !keep {
			release(context.Background())
		}
	}()
	applyResolvedServerConfig(&cfg, server)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, &server, target, repo.Root, reclaim); err != nil {
		release(ctx)
		return err
	}
	if runOpts.Results != nil {
		result := checkpointForkResult{
			LeaseID:  leaseID,
			Slug:     serverSlug(server),
			Provider: cfg.Provider,
		}
		if resultIndex >= 0 {
			(*runOpts.Results)[resultIndex] = result
		} else {
			*runOpts.Results = append(*runOpts.Results, result)
		}
	}
	if !runOpts.JSON {
		fmt.Fprintf(a.Stdout, "checkpoint forked provider=parallels source=%s snapshot=%s lease=%s slug=%s\n", source, snapshot, leaseID, blank(serverSlug(server), "-"))
	}
	return a.runCheckpointForkCommand(ctx, leaseID, serverSlug(server), runOpts)
}

func (a App) checkpointDelete(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint delete", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	sourceID := fs.String("id", "", "provider source VM id/name for provider-native snapshot")
	snapshot := fs.String("snapshot", "", "provider-native snapshot name or id")
	localOnly := fs.Bool("local-only", false, "delete only the local checkpoint record")
	admin := fs.Bool("admin", false, "use the configured coordinator admin credential")
	dryRun := fs.Bool("dry-run", false, "show provider-native deletion target without deleting")
	yes := fs.Bool("yes", false, "allow deleting non-crabbox provider-native snapshots")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	ctx = withCheckpointAdmin(ctx, *admin)
	if flagWasSet(fs, "parallels-template") {
		*provider = "parallels"
	}
	if strings.TrimSpace(*snapshot) != "" {
		if fs.NArg() != 0 {
			return exit(2, "usage: crabbox checkpoint delete --provider parallels --id <source-vm> --snapshot <name-or-id>")
		}
		if *localOnly {
			return exit(2, "--local-only applies only to recorded checkpoints")
		}
		cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *sourceID, ProviderResourceID: true})
		if err != nil {
			return err
		}
		if cfg.Provider != "parallels" {
			return exit(2, "checkpoint delete --snapshot currently supports provider=parallels")
		}
		if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
			return err
		}
		if strings.TrimSpace(*sourceID) == "" {
			*sourceID = firstNonBlank(cfg.Parallels.SourceID, cfg.Parallels.Source)
		}
		if err := requireLeaseID(*sourceID, "crabbox checkpoint delete --provider parallels --id <source-vm> --snapshot <name-or-id>", cfg); err != nil {
			return err
		}
		cfg, vm, err := ResolveParallelsVM(ctx, cfg, nil, *sourceID)
		if err != nil {
			return err
		}
		client := NewParallelsClient(cfg, nil)
		snapshot, err := client.Snapshot(ctx, vm.ID, *snapshot)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(snapshot.Name, "crabbox-") && !*yes {
			return exit(2, "refusing to delete non-Crabbox Parallels snapshot %q without --yes", snapshot.Name)
		}
		if *dryRun {
			fmt.Fprintf(a.Stdout, "would delete provider=parallels source=%s snapshot=%s name=%q\n", vm.ID, snapshot.ID, snapshot.Name)
			return nil
		}
		if err := client.DeleteSnapshot(ctx, vm.ID, snapshot.ID, false); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "checkpoint deleted provider=parallels source=%s snapshot=%s\n", vm.ID, snapshot.ID)
		return nil
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
	record, _, localErr := store.Read(id)
	if !*localOnly && (localErr != nil || !record.coordinatorManaged()) {
		record, _, err = a.readCheckpointRecord(ctx, store, id)
		if err != nil {
			if isCheckpointNotFound(err) || isCheckpointNotFound(localErr) && isCoordinatorCheckpointNotFound(err) {
				fmt.Fprintf(a.Stdout, "checkpoint absent id=%s\n", id)
				return nil
			}
			return err
		}
	}
	if *dryRun {
		if *localOnly && localErr != nil {
			if isCheckpointNotFound(localErr) {
				fmt.Fprintf(a.Stdout, "checkpoint absent id=%s\n", id)
				return nil
			}
			return localErr
		}
		fmt.Fprintf(a.Stdout, "would delete checkpoint id=%s kind=%s provider=%s resource=%s local_only=%t\n", record.ID, record.Kind, blank(record.Provider, "-"), blank(nativeCheckpointDeleteID(record), "-"), *localOnly)
		return nil
	}
	deleteLocal := func() error { return deleteCheckpoint(ctx, store, id, *localOnly) }
	if record.coordinatorManaged() && !*localOnly {
		deleteLocal = func() error { return a.deleteManagedCheckpoint(ctx, store, record) }
	}
	if err := deleteLocal(); err != nil {
		if isCheckpointNotFound(err) {
			fmt.Fprintf(a.Stdout, "checkpoint absent id=%s\n", id)
			return nil
		}
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint deleted id=%s\n", id)
	return nil
}

func (a App) deleteManagedCheckpoint(ctx context.Context, store checkpointStore, remote checkpointRecord) error {
	deleteRemote := func(cacheWritable bool) error {
		current, _, readErr := store.Read(remote.ID)
		if readErr == nil {
			if !sameManagedCheckpointIdentity(current, remote) {
				return exit(2, "checkpoint %s has conflicting local ownership or provider identity", remote.ID)
			}
			if current.Capture != nil && unresolvedCheckpoint(current) {
				return exit(2, "checkpoint %s has an unresolved capture; retain its record and source", remote.ID)
			}
		}
		if err := deleteCheckpointResource(ctx, store, remote); err != nil {
			return err
		}
		if cacheWritable && readErr == nil {
			if err := store.Delete(remote.ID); err != nil {
				fmt.Fprintf(a.Stderr, "warning: checkpoint %s was deleted remotely but its cache remains: %v\n", remote.ID, err)
			}
		} else if readErr != nil && !isCheckpointNotFound(readErr) || !cacheWritable {
			fmt.Fprintf(a.Stderr, "warning: checkpoint %s was deleted remotely; preserving unavailable local cache metadata\n", remote.ID)
		}
		return nil
	}
	entered := false
	err := store.withLock(remote.ID, false, func() error { entered = true; return deleteRemote(true) })
	var busy checkpointBusyError
	if err == nil || entered || errors.As(err, &busy) {
		return err
	}
	// Cache I/O is optional; an active operation or a positively known conflict is not.
	return deleteRemote(false)
}

func deleteCheckpoint(ctx context.Context, store checkpointStore, id string, localOnly bool) error {
	return store.withRecord(id, false, func(record checkpointRecord) error {
		return deleteCheckpointRecord(ctx, store, record, localOnly)
	})
}

func deleteCheckpointRecord(ctx context.Context, store checkpointStore, record checkpointRecord, localOnly bool) error {
	id := record.ID
	if unresolvedCheckpoint(record) && (!record.coordinatorManaged() || record.Capture != nil) {
		return exit(2, "checkpoint %s is unresolved; retain its record and source and reconcile the original capture", id)
	}
	if !localOnly {
		if err := deleteCheckpointResource(ctx, store, record); err != nil {
			return err
		}
	}
	return store.Delete(id)
}

func deleteCheckpointResource(ctx context.Context, store checkpointStore, record checkpointRecord) error {
	if record.coordinatorManaged() {
		coord, err := configuredCheckpointCoordinatorFor(ctx)
		if err != nil {
			return err
		}
		origin := checkpointCoordinatorOrigin(coord.BaseURL)
		if record.Ownership.Origin != origin {
			return exit(2, "checkpoint %s belongs to coordinator %s, not %s", record.ID, record.Ownership.Origin, origin)
		}
		checkpoint, err := coord.Checkpoint(ctx, record.ID)
		if err != nil {
			if isCoordinatorCheckpointNotFound(err) {
				return coord.DeleteCheckpoint(ctx, record.ID)
			}
			return err
		}
		current, err := checkpointRecordFromCoordinator(checkpoint, origin)
		if err != nil {
			return err
		}
		if !sameManagedCheckpointIdentity(record, current) {
			if canRefreshManagedCheckpointCache(record, current) {
				return exit(2, "checkpoint %s has not confirmed its coordinator creation identity; run crabbox checkpoint inspect %s before deletion", record.ID, record.ID)
			}
			return exit(2, "checkpoint %s has conflicting local and coordinator provider identities", record.ID)
		}
		return coord.DeleteCheckpoint(ctx, record.ID)
	}
	providerID := nativeCheckpointDeleteID(record)
	if !isNativeCheckpointKind(record.Kind) || providerID == "" {
		return nil
	}
	if provider, ok := nativeCheckpointLifecycleProvider(Config{Provider: record.nativeProvider()}, Server{}); ok {
		request := nativeCheckpointResourceRequest(record)
		request.Persist = func(result NativeCheckpointCreateResult) error {
			return store.WriteNativeProgress(&record, result, record.Native.NoReboot)
		}
		return provider.DeleteNativeCheckpoint(ctx, request)
	}
	if record.Kind == checkpointKindParallels {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		applyParallelsCheckpointHostConfig(&cfg, record)
		return NewParallelsClient(cfg, nil).DeleteSnapshot(ctx, record.Native.Resource, providerID, false)
	}
	if cfg, ok := directAWSCheckpointConfig(record); ok {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return err
		}
		if err := client.GuardAccount(ctx, record.Native.AccountID); err != nil {
			return err
		}
		if len(record.Native.SnapshotIDs) == 0 {
			if image, err := client.GetImageCheckpoint(ctx, providerID); err == nil && len(image.SnapshotIDs) > 0 {
				record.Native.SnapshotIDs = image.SnapshotIDs
				if writeErr := store.Write(record); writeErr != nil {
					return writeErr
				}
			}
		}
		return client.DeleteImageCheckpoint(ctx, providerID, record.Native.SnapshotIDs, record.Native.AccountID)
	}
	if cfg, ok := directAzureCheckpointConfig(record); ok {
		client, err := NewAzureClient(ctx, cfg)
		if err != nil {
			return err
		}
		return client.DeleteOSDiskSnapshot(ctx, providerID)
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	ref := nativeCoordinatorImageRef(record)
	if err := coord.DeleteImage(ctx, providerID, ref); err != nil && !isCoordinatorImageNotFound(err, providerID) {
		if !isCoordinatorNotFound(err) {
			return err
		}
		if _, verifyErr := coord.Image(ctx, providerID, ref); !isCoordinatorImageNotFound(verifyErr, providerID) {
			if verifyErr != nil {
				return verifyErr
			}
			return err
		}
	}
	return nil
}

func isCoordinatorImageNotFound(err error, imageID string) bool {
	var coordinatorErr CoordinatorHTTPError
	if !errors.As(err, &coordinatorErr) || coordinatorErr.StatusCode != 404 {
		return false
	}
	var response struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(coordinatorErr.Message), &response) != nil {
		return false
	}
	return response.Error == "not_found" && response.Message == "image "+imageID+" not found"
}

func (a App) checkpointPrune(ctx context.Context, args []string) error {
	fs := newFlagSet("checkpoint prune", a.Stderr)
	olderThan := fs.String("older-than", "", "delete checkpoints older than this duration")
	unusedFor := fs.String("unused-for", "", "delete checkpoints unused for this duration")
	kind := fs.String("kind", "", "checkpoint kind filter: native or archive")
	dryRun := fs.Bool("dry-run", false, "print checkpoints that would be deleted")
	localOnly := fs.Bool("local-only", false, "delete local checkpoint records without deleting provider resources")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	usage := "usage: crabbox checkpoint prune [--older-than <duration>] [--unused-for <duration>] [--kind native|archive] [--dry-run]"
	if fs.NArg() != 0 {
		return exit(2, "%s", usage)
	}
	createdAge, err := parseCheckpointPruneDuration(*olderThan)
	if err != nil {
		return err
	}
	unusedAge, err := parseCheckpointPruneDurationFlag("--unused-for", *unusedFor)
	if err != nil {
		return err
	}
	invalidCreatedAge := strings.TrimSpace(*olderThan) != "" && createdAge <= 0
	invalidUnusedAge := strings.TrimSpace(*unusedFor) != "" && unusedAge <= 0
	if invalidCreatedAge || invalidUnusedAge || createdAge == 0 && unusedAge == 0 {
		return exit(2, "%s", usage)
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
	now := time.Now()
	createdCutoff := now.Add(-createdAge)
	unusedCutoff := now.Add(-unusedAge)
	matches := func(record checkpointRecord) (bool, error) {
		if record.coordinatorManaged() {
			return false, nil
		}
		created, err := time.Parse(time.RFC3339, record.CreatedAt)
		if err != nil {
			return false, exit(2, "checkpoint %s has invalid createdAt: %v", record.ID, err)
		}
		lastUsed, err := time.Parse(time.RFC3339, record.LastUsedAt)
		if err != nil {
			return false, exit(2, "checkpoint %s has invalid lastUsedAt: %v", record.ID, err)
		}
		matchesCreatedAge := createdAge == 0 || created.Before(createdCutoff)
		matchesUnusedAge := unusedAge == 0 || lastUsed.Before(unusedCutoff)
		return matchesCreatedAge && matchesUnusedAge && checkpointMatchesPruneKind(record, kindFilter), nil
	}
	matched := 0
	for _, record := range records {
		eligible, err := matches(record)
		if err != nil {
			return err
		}
		if !eligible {
			continue
		}
		if *dryRun {
			matched++
			fmt.Fprintf(a.Stdout, "would delete id=%s kind=%s created=%s last_used=%s\n", record.ID, record.Kind, record.CreatedAt, record.LastUsedAt)
			continue
		}
		var pruned *checkpointRecord
		err = store.withRecord(record.ID, false, func(current checkpointRecord) error {
			// A fork may have refreshed usage since the inventory was read.
			eligible, err := matches(current)
			if err != nil || !eligible {
				return err
			}
			if err := deleteCheckpointRecord(ctx, store, current, *localOnly); err != nil {
				return err
			}
			pruned = &current
			return nil
		})
		if isCheckpointNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if pruned != nil {
			matched++
			fmt.Fprintf(a.Stdout, "checkpoint pruned id=%s kind=%s created=%s last_used=%s\n", pruned.ID, pruned.Kind, pruned.CreatedAt, pruned.LastUsedAt)
		}
	}
	if matched == 0 {
		fmt.Fprintln(a.Stdout, "no checkpoints matched prune criteria")
	}
	return nil
}

func parseCheckpointPruneDuration(value string) (time.Duration, error) {
	return parseCheckpointPruneDurationFlag("--older-than", value)
}

func parseCheckpointRetentionDuration(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	duration, err := parseCheckpointPruneDurationFlag("--expire-unused-after", value)
	if err != nil {
		return 0, err
	}
	const maximum = 10 * 366 * 24 * time.Hour
	if duration <= 0 || duration > maximum || duration%time.Second != 0 {
		return 0, exit(2, "--expire-unused-after must be a positive whole-second duration no greater than 10 years")
	}
	return duration, nil
}

func parseCheckpointPruneDurationFlag(flagName, value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, nil
	}
	if strings.HasSuffix(trimmed, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(trimmed, "d"))
		if err != nil || days <= 0 {
			return 0, exit(2, "%s day duration must be a positive integer", flagName)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, exit(2, "parse %s: %v", flagName, err)
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
	if isNativeCheckpointKind(record.Kind) && record.Capture != nil && record.Capture.Phase != "retired" {
		if _, err := store.Paths(record.ID); err != nil {
			return checkpointAudit{}, err
		}
		return checkpointAudit{
			Record: record, LocalState: "metadata_available", ProviderState: "pending",
			NextAction: "replay_capture", Error: record.Capture.Error,
		}, nil
	}
	return a.verifyCheckpointResource(ctx, store, record)
}

// Resource verification is also used by retirement while its capture is still held.
func (a App) verifyCheckpointResource(ctx context.Context, store checkpointStore, record checkpointRecord) (checkpointAudit, error) {
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
			audit.ProviderState = "unresolved_capture"
			audit.NextAction = "reconcile_capture"
			return audit, nil
		}
		if record.coordinatorManaged() {
			coord, err := configuredCheckpointCoordinatorFor(ctx)
			if err != nil {
				audit.ProviderState = "unknown"
				audit.NextAction = "check_coordinator"
				audit.Error = err.Error()
				return audit, nil
			}
			if checkpointCoordinatorOrigin(coord.BaseURL) != record.Ownership.Origin {
				audit.ProviderState, audit.NextAction = "unknown", "check_coordinator"
				audit.Error = fmt.Sprintf("checkpoint belongs to coordinator %s, not %s", record.Ownership.Origin, checkpointCoordinatorOrigin(coord.BaseURL))
				return audit, nil
			}
			image, err := coord.CheckpointImage(ctx, record.ID)
			if err != nil {
				audit.ProviderState = "unknown"
				audit.NextAction = "check_coordinator_or_provider"
				audit.Error = err.Error()
				return audit, nil
			}
			applyCheckpointImageAudit(&audit, image)
			return audit, nil
		}
		if cfg, ok := directAWSCheckpointConfig(record); ok {
			return verifyDirectAWSCheckpoint(ctx, audit, cfg, providerID, record.Native.AccountID), nil
		}
		if cfg, ok := directAzureCheckpointConfig(record); ok {
			return verifyDirectAzureCheckpoint(ctx, audit, cfg, providerID), nil
		}
		if provider, ok := nativeCheckpointLifecycleProvider(Config{Provider: record.nativeProvider()}, Server{}); ok {
			result, err := provider.VerifyNativeCheckpoint(ctx, nativeCheckpointResourceRequest(record))
			if err != nil {
				audit.ProviderState = "unknown"
				audit.NextAction = "check_runtime"
				audit.Error = err.Error()
				return audit, nil
			}
			audit.ProviderState = result.ProviderState
			audit.NextAction = result.NextAction
			audit.Error = result.Error
			return audit, nil
		}
		if record.Kind == checkpointKindParallels {
			cfg, err := loadConfig()
			if err != nil {
				audit.ProviderState = "unknown"
				audit.NextAction = "check_config"
				audit.Error = err.Error()
				return audit, nil
			}
			applyParallelsCheckpointHostConfig(&cfg, record)
			snapshots, err := NewParallelsClient(cfg, nil).Snapshots(ctx, record.Native.Resource)
			if err != nil {
				audit.ProviderState = "unknown"
				audit.NextAction = "check_auth_or_provider"
				audit.Error = err.Error()
				return audit, nil
			}
			for _, snapshot := range snapshots {
				if snapshot.ID == providerID {
					audit.ProviderState = "available"
					audit.NextAction = "fork_restore_or_delete"
					return audit, nil
				}
			}
			audit.ProviderState = "missing"
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
		applyCheckpointImageAudit(&audit, image)
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

func newCheckpointRecord(repo Repo, cfg Config, server Server, target SSHTarget, leaseID, workdir, name string) (checkpointRecord, string, error) {
	id, err := newCheckpointID()
	if err != nil {
		return checkpointRecord{}, "", err
	}
	dir, err := checkpointDir(id)
	if err != nil {
		return checkpointRecord{}, "", err
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	record := checkpointRecord{
		ID:             id,
		Name:           strings.TrimSpace(name),
		Kind:           checkpointKindArchive,
		CreatedAt:      createdAt,
		LastUsedAt:     createdAt,
		CrabboxVersion: currentVersion(),
		Provider:       firstNonBlank(server.Provider, cfg.Provider),
		LeaseID:        leaseID,
		Slug:           serverSlug(server),
		TargetOS:       firstNonBlank(target.TargetOS, cfg.TargetOS),
		WindowsMode:    firstNonBlank(target.WindowsMode, cfg.WindowsMode),
		Desktop:        cfg.Desktop || labelBool(server.Labels["desktop"]),
		ServerType:     firstNonBlank(server.ServerType.Name, cfg.ServerType),
		HostID:         firstNonBlank(server.HostID, cfg.HostID, cfg.AWSMacHostID),
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
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		if kind, ok := nativeCheckpointKind(cfg, server, target, strategy); ok {
			return kind
		}
		if kind, ok := parallelsNativeCheckpointKind(cfg, server, strategy); ok {
			return kind
		}
		if !isAutoCheckpointStrategy(strategy) {
			if kind, ok := directNativeCheckpointKind(cfg, server, target, strategy); ok {
				return kind
			}
		}
		return checkpointKindArchive
	case "native", "provider-native", "vm":
		if capability, ok := nativeModeCheckpointCapability(cfg, server, target, strategy); ok {
			return capability.Kind
		}
		return "unsupported"
	case "ami", "image":
		if kind, ok := nativeCheckpointKind(cfg, server, target, checkpointStrategyImage); ok {
			return kind
		}
		if kind, ok := directNativeCheckpointKind(cfg, server, target, checkpointStrategyImage); ok {
			return kind
		}
		return "unsupported"
	case "snapshot", "disk-snapshot", "disk":
		if kind, ok := nativeCheckpointKind(cfg, server, target, checkpointStrategyDiskSnapshot); ok {
			return kind
		}
		if kind, ok := parallelsNativeCheckpointKind(cfg, server, checkpointStrategyDiskSnapshot); ok {
			return kind
		}
		if kind, ok := directNativeCheckpointKind(cfg, server, target, checkpointStrategyDiskSnapshot); ok {
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

func isAutoCheckpointStrategy(strategy string) bool {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", checkpointStrategyAuto:
		return true
	default:
		return false
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

func checkpointCreateStrategy(mode, strategy, kind string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "ami", "image":
		return checkpointStrategyImage
	case "snapshot", "disk-snapshot", "disk":
		return checkpointStrategyDiskSnapshot
	}
	if !isAutoCheckpointStrategy(strategy) {
		return normalizeCheckpointStrategy(strategy)
	}
	return checkpointStrategyForKind(kind)
}

func validCheckpointStrategy(strategy string) bool {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", checkpointStrategyAuto, checkpointStrategyDiskSnapshot, checkpointStrategyImage, "snapshot", "disk", "ami", "machine-image", "managed-image", "disk_snapshot":
		return true
	default:
		return false
	}
}

func defaultCheckpointRestoreWorkdir(cfg Config, leaseID, repoName, savedWorkdir string) string {
	return firstNonBlank(remoteJoin(cfg, leaseID, repoName), savedWorkdir)
}

func checkpointRestoreWorkdir(cfg Config, leaseID, repoName, savedWorkdir, override string) string {
	override = strings.TrimSpace(override)
	if override != "" {
		return override
	}
	return defaultCheckpointRestoreWorkdir(cfg, leaseID, repoName, savedWorkdir)
}

func nativeCheckpointForkWorkdir(cfg Config, leaseID, repoName, override string) string {
	override = strings.TrimSpace(override)
	if override != "" {
		return override
	}
	return remoteJoin(cfg, leaseID, repoName)
}

func isNativeCheckpointKind(kind string) bool {
	return kind == checkpointKindAWSAMI || kind == checkpointKindAWSEBS || kind == checkpointKindAzure || kind == checkpointKindAzureOS || kind == checkpointKindGCP || kind == checkpointKindGCPDisk || kind == checkpointKindHetzner || kind == checkpointKindMachine0 || kind == checkpointKindParallels || kind == checkpointKindDockerCommit || kind == checkpointKindDaytona || kind == checkpointKindIncus
}

func checkpointProviderForKind(kind string) string {
	switch kind {
	case checkpointKindAWSAMI, checkpointKindAWSEBS:
		return "aws"
	case checkpointKindAzure, checkpointKindAzureOS:
		return "azure"
	case checkpointKindGCP, checkpointKindGCPDisk:
		return "gcp"
	case checkpointKindHetzner:
		return "hetzner"
	case checkpointKindMachine0:
		return "machine0"
	case checkpointKindParallels:
		return "parallels"
	case checkpointKindDockerCommit:
		return "local-container"
	case checkpointKindDaytona:
		return "daytona"
	case checkpointKindIncus:
		return "incus"
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
	cmd := sshCommandContext(ctx, target, sshArgs(target, remoteCheckpointArchiveCommand(workdir))...)
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
		"elif ! test -e \"$src\" && test -d \"$dst\"; then\n" +
		"  :\n" +
		"fi"
	return "bash -lc " + shellQuote(script)
}
