package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	checkpointKindParallels    = "parallels-snapshot"
	checkpointKindDockerCommit = "docker-commit"

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
	ServerType     string `json:"serverType,omitempty"`
	HostID         string `json:"hostId,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
	ArchivePath    string `json:"archivePath,omitempty"`
	ArchiveBytes   int64  `json:"archiveBytes,omitempty"`
	Native         struct {
		Provider    string            `json:"provider,omitempty"`
		ImageID     string            `json:"imageId,omitempty"`
		Kind        string            `json:"kind,omitempty"`
		Name        string            `json:"name,omitempty"`
		State       string            `json:"state,omitempty"`
		Region      string            `json:"region,omitempty"`
		AccountID   string            `json:"accountId,omitempty"`
		Project     string            `json:"project,omitempty"`
		Resource    string            `json:"resource,omitempty"`
		SnapshotIDs []string          `json:"snapshotIds,omitempty"`
		Direct      bool              `json:"direct,omitempty"`
		Strategy    string            `json:"strategy,omitempty"`
		NoReboot    bool              `json:"noReboot,omitempty"`
		Metadata    map[string]string `json:"metadata,omitempty"`
	} `json:"native,omitempty"`
	Repo struct {
		Root      string `json:"root,omitempty"`
		Name      string `json:"name,omitempty"`
		RemoteURL string `json:"remoteUrl,omitempty"`
		Head      string `json:"head,omitempty"`
		BaseRef   string `json:"baseRef,omitempty"`
	} `json:"repo"`
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
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
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
	server, target, leaseID, err := a.resolveNetworkLeaseTargetWithConfig(ctx, &cfg, *id, true)
	if err != nil {
		return err
	}
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
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
	createKind := checkpointCreateMode(*mode, *strategy, cfg, server, target, *recipeOnly)
	switch createKind {
	case checkpointKindRecipe, checkpointKindAWSAMI, checkpointKindAWSEBS, checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk, checkpointKindParallels, checkpointKindDockerCommit, checkpointKindArchive:
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
	case checkpointKindAWSAMI, checkpointKindAWSEBS, checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk, checkpointKindParallels, checkpointKindDockerCommit:
		image, metadata, err := a.createNativeCheckpoint(ctx, cfg, server, target, leaseID, record.Name, repo.Name, workdir, checkpointStrategyForKind(createKind), *noReboot, *wait, *waitTimeout)
		if image.ID != "" {
			applyNativeImageCheckpointRecord(&record, image, *noReboot)
			record.Native.Metadata = metadata
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
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
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
	if flagWasSet(fs, "parallels-template") {
		*provider = "parallels"
	}
	setIDFromFirstArg(fs, id)
	if strings.TrimSpace(*id) != "" || flagWasSet(fs, "provider") || flagWasSet(fs, "parallels-template") {
		cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
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
	cfg.Provider = "parallels"
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
	fmt.Fprintf(stdout, "id=%s\nkind=%s\nname=%s\ncreated=%s\nprovider=%s\nlease=%s\nrepo=%s\nhead=%s\nserver_type=%s\nworkdir=%s\narchive=%s\nbytes=%s\n",
		record.ID, record.Kind, blank(record.Name, "-"), record.CreatedAt, blank(record.Provider, "-"), blank(record.LeaseID, "-"), blank(record.Repo.Name, "-"), blank(record.Repo.Head, "-"), blank(record.ServerType, "-"), blank(record.Workdir, "-"), blank(record.ArchivePath, "-"), humanBytes(record.ArchiveBytes))
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
		cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
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
				server, _, _, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, true)
				if err != nil {
					return err
				}
				restoreCfg := cfg
				applyParallelsHostRefConfig(&restoreCfg, firstNonBlank(server.Labels["host"], cfg.Parallels.Host))
				if err := NewParallelsClient(restoreCfg, nil).SwitchSnapshot(ctx, server.CloudID, record.Native.ImageID, true); err != nil {
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
	server, target, leaseID, err := a.resolveNetworkLeaseTargetWithConfig(ctx, &cfg, *id, true)
	if err != nil {
		return err
	}
	workdir := checkpointRestoreWorkdir(cfg, leaseID, repo.Name, record.Workdir, workdirOverrideValue)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
		return err
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
	id := fs.String("id", "", "provider source VM id/name for provider-native fork")
	snapshot := fs.String("snapshot", "", "provider-native snapshot name or id")
	keep := fs.Bool("keep", true, "keep forked lease after restore")
	dryRun := fs.Bool("dry-run", false, "show provider-native fork target without cloning")
	workdirOverride := fs.String("workdir", "", "remote restore workdir")
	clear := fs.Bool("clear", true, "clear the remote workdir before restoring")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if flagWasSet(fs, "parallels-template") {
		*leaseFlags.Provider = "parallels"
	}
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*snapshot) != "" || flagWasSet(fs, "parallels-template") {
		if fs.NArg() != 0 {
			return exit(2, "usage: crabbox checkpoint fork --provider parallels --id <source-vm> --snapshot <name-or-id> [--slug <slug>]")
		}
		return a.checkpointForkParallelsSnapshot(ctx, fs, leaseFlags, *id, *snapshot, *keep, *reclaim, requestedSlug, *dryRun)
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
	nativeCheckpoint := isNativeCheckpointKind(record.Kind)
	if nativeCheckpoint && record.TargetOS == targetMacOS && !flagWasSet(fs, "market") {
		cfg.Capacity.Market = "on-demand"
	}
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return err
	}
	if record.Kind != checkpointKindArchive && !nativeCheckpoint {
		return exit(2, "checkpoint %s has kind=%s; fork requires %s or a native image checkpoint", record.ID, record.Kind, checkpointKindArchive)
	}
	if nativeCheckpoint {
		if nativeCheckpointResourceID(record) == "" {
			return exit(2, "checkpoint %s is pending; native provider resource is not recorded yet", record.ID)
		}
		if err := applyNativeCheckpointForkConfig(&cfg, fs, record); err != nil {
			return err
		}
	}
	if *dryRun {
		fmt.Fprintf(a.Stdout, "would fork checkpoint id=%s provider=%s resource=%s slug=%s keep=%t\n", record.ID, cfg.Provider, blank(nativeCheckpointResourceID(record), "-"), blank(requestedSlug, "-"), *keep)
		return nil
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
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: leaseOptionsFromConfig(cfg), Keep: *keep, Reclaim: *reclaim, RequestedSlug: requestedSlug})
	if err != nil {
		return err
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	defer func() {
		if err == nil && !*keep {
			a.releaseBackendLeaseBestEffort(context.Background(), sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		}
	}()
	applyResolvedServerConfig(&cfg, server)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, cfg, lease)
		return err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, cfg, lease)
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
			a.releaseBackendLeaseBestEffort(ctx, sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
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
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint forked id=%s lease=%s slug=%s workdir=%s\n", record.ID, leaseID, blank(serverSlug(server), "-"), workdir)
	return nil
}

func (a App) checkpointForkParallelsSnapshot(ctx context.Context, fs *flag.FlagSet, leaseFlags leaseCreateFlagValues, source, snapshot string, keep, reclaim bool, requestedSlug string, dryRun bool) (err error) {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
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
		fmt.Fprintf(a.Stdout, "would fork provider=parallels host=%s source=%s snapshot=%s name=%q slug=%s\n", blank(selected.Parallels.SelectedHost, "local"), cfg.Parallels.Source, snapshot.ID, snapshot.Name, blank(requestedSlug, "-"))
		return nil
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
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: leaseOptionsFromConfig(cfg), Keep: keep, Reclaim: reclaim, RequestedSlug: requestedSlug})
	if err != nil {
		return err
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	defer func() {
		if err == nil && !keep {
			a.releaseBackendLeaseBestEffort(context.Background(), sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		}
	}()
	applyResolvedServerConfig(&cfg, server)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, reclaim); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, cfg, lease)
		return err
	}
	fmt.Fprintf(a.Stdout, "checkpoint forked provider=parallels source=%s snapshot=%s lease=%s slug=%s\n", source, snapshot, leaseID, blank(serverSlug(server), "-"))
	return nil
}

func (a App) checkpointDelete(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint delete", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	sourceID := fs.String("id", "", "provider source VM id/name for provider-native snapshot")
	snapshot := fs.String("snapshot", "", "provider-native snapshot name or id")
	localOnly := fs.Bool("local-only", false, "delete only the local checkpoint record")
	dryRun := fs.Bool("dry-run", false, "show provider-native deletion target without deleting")
	yes := fs.Bool("yes", false, "allow deleting non-crabbox provider-native snapshots")
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
			return exit(2, "usage: crabbox checkpoint delete --provider parallels --id <source-vm> --snapshot <name-or-id>")
		}
		if *localOnly {
			return exit(2, "--local-only applies only to recorded checkpoints")
		}
		cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *sourceID})
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
	if *dryRun {
		record, _, err := store.Read(id)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "would delete checkpoint id=%s kind=%s provider=%s resource=%s local_only=%t\n", record.ID, record.Kind, blank(record.Provider, "-"), blank(nativeCheckpointDeleteID(record), "-"), *localOnly)
		return nil
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
		if provider, ok := nativeCheckpointLifecycleProvider(Config{Provider: record.nativeProvider()}, Server{}); ok {
			if err := provider.DeleteNativeCheckpoint(ctx, nativeCheckpointResourceRequest(record)); err != nil {
				return err
			}
			return store.Delete(id)
		}
		if record.Kind == checkpointKindParallels {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			applyParallelsCheckpointHostConfig(&cfg, record)
			if err := NewParallelsClient(cfg, nil).DeleteSnapshot(ctx, record.Native.Resource, providerID, false); err != nil {
				return err
			}
			return store.Delete(id)
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
			if err := client.DeleteImageCheckpoint(ctx, providerID, record.Native.SnapshotIDs, record.Native.AccountID); err != nil {
				return err
			}
			return store.Delete(id)
		}
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
		if cfg, ok := directAWSCheckpointConfig(record); ok {
			return verifyDirectAWSCheckpoint(ctx, audit, cfg, providerID, record.Native.AccountID), nil
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
		if kind, ok := nativeCheckpointKind(cfg, server, target, strategy); ok {
			return kind
		}
		if kind, ok := directNativeCheckpointKind(cfg, server, target, strategy); ok {
			return kind
		}
		if kind, ok := parallelsNativeCheckpointKind(cfg, server, strategy); ok {
			return kind
		}
		if isAutoCheckpointStrategy(strategy) {
			if kind, ok := directNativeCheckpointKind(cfg, server, target, checkpointStrategyImage); ok {
				return kind
			}
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
	return kind == checkpointKindAWSAMI || kind == checkpointKindAWSEBS || kind == checkpointKindAzure || kind == checkpointKindAzureOS || kind == checkpointKindGCP || kind == checkpointKindGCPDisk || kind == checkpointKindParallels || kind == checkpointKindDockerCommit
}

func checkpointProviderForKind(kind string) string {
	switch kind {
	case checkpointKindAWSAMI, checkpointKindAWSEBS:
		return "aws"
	case checkpointKindAzure, checkpointKindAzureOS:
		return "azure"
	case checkpointKindGCP, checkpointKindGCPDisk:
		return "gcp"
	case checkpointKindParallels:
		return "parallels"
	case checkpointKindDockerCommit:
		return "local-container"
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
