package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

type checkpointNativeCreateRequest struct {
	Cfg         Config
	Server      Server
	Target      SSHTarget
	LeaseID     string
	Name        string
	RepoName    string
	Workdir     string
	Strategy    string
	NoReboot    bool
	Wait        bool
	WaitTimeout time.Duration
	Stderr      io.Writer
}

type checkpointNativeCreateDriver interface {
	Create(context.Context, checkpointNativeCreateRequest) (CoordinatorImage, error)
}

type directAWSAMICheckpointDriver struct{}

func (directAWSAMICheckpointDriver) Create(ctx context.Context, req checkpointNativeCreateRequest) (CoordinatorImage, error) {
	name := req.Name
	if name == "" {
		name = defaultNativeImageName(req.LeaseID, req.RepoName)
	}
	client, err := newAWSClient(ctx, req.Cfg)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if _, err := client.ValidateImageCheckpointSource(ctx, req.Server.CloudID); err != nil {
		return CoordinatorImage{}, err
	}
	if !isWindowsNativeTarget(req.Target) {
		if err := prepareNativeImageSource(ctx, req.Target); err != nil {
			return CoordinatorImage{}, err
		}
	}
	image, err := client.CreateImageCheckpoint(ctx, req.Server.CloudID, name, req.NoReboot)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if req.Wait {
		waited, err := waitForDirectAWSImage(ctx, client, image.ID, image.AccountID, req.WaitTimeout, req.Stderr)
		if err != nil {
			return image, err
		}
		return waited, nil
	}
	return image, nil
}

type coordinatorCheckpointDriver struct{}

func (coordinatorCheckpointDriver) Create(ctx context.Context, req checkpointNativeCreateRequest) (CoordinatorImage, error) {
	if req.Cfg.Coordinator == "" {
		return CoordinatorImage{}, exit(2, "native checkpoints require a configured coordinator")
	}
	strategy := normalizeCheckpointStrategy(req.Strategy)
	capability, ok := providerNativeCheckpointCapability(req.Cfg, req.Server, req.Target, req.Strategy)
	if !ok || capability.Direct || capability.Kind == "" {
		return CoordinatorImage{}, exit(2, "native checkpoints support brokered AWS Linux/macOS leases and brokered Azure/GCP Linux leases only")
	}
	if capability.CreateUnsupported != "" {
		return CoordinatorImage{}, exit(2, "%s", capability.CreateUnsupported)
	}
	name := req.Name
	if name == "" {
		name = defaultNativeImageName(req.LeaseID, req.RepoName)
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return CoordinatorImage{}, err
	}
	if err := prepareNativeImageSource(ctx, req.Target); err != nil {
		return CoordinatorImage{}, err
	}
	image, err := coord.CreateImage(ctx, req.LeaseID, name, req.NoReboot, strategy)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if req.Wait {
		waited, err := waitForImage(ctx, coord, image.ID, imageRefFromCoordinatorImage(image), req.WaitTimeout, req.Stderr)
		if err != nil {
			return image, err
		}
		return waited, nil
	}
	return image, nil
}

type directParallelsCheckpointDriver struct {
	Runner CommandRunner
}

func (d directParallelsCheckpointDriver) Create(ctx context.Context, req checkpointNativeCreateRequest) (image CoordinatorImage, err error) {
	name := req.Name
	if name == "" {
		name = defaultNativeImageName(req.LeaseID, req.RepoName)
	}
	cfg := req.Cfg
	applyParallelsHostRefConfig(&cfg, firstNonBlank(req.Server.Labels["host"], req.Cfg.Parallels.Host))
	client := NewParallelsClient(cfg, d.Runner)
	vm, err := client.GetVM(ctx, req.Server.CloudID)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if !parallelsPowerOffState(vm.State) {
		if req.NoReboot {
			return CoordinatorImage{}, exit(2, "Parallels native checkpoints require a powered-off VM for forkable linked clones; stop the VM first or rerun with --no-reboot=false")
		}
		restartAfter := parallelsRunningState(vm.State)
		if err := client.Stop(ctx, req.Server.CloudID); err != nil {
			return CoordinatorImage{}, err
		}
		if restartAfter {
			defer func() {
				if startErr := client.Start(ctx, req.Server.CloudID); err == nil && startErr != nil {
					err = startErr
				}
			}()
		}
	}
	snapshot, err := client.CreateSnapshot(ctx, req.Server.CloudID, name, "crabbox checkpoint "+req.LeaseID)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if !parallelsPowerOffState(snapshot.State) {
		_ = client.DeleteSnapshot(ctx, req.Server.CloudID, snapshot.ID, false)
		return CoordinatorImage{}, exit(5, "Parallels snapshot %q state=%s is not forkable; expected poweroff", snapshot.Name, blank(snapshot.State, "unknown"))
	}
	return CoordinatorImage{
		ID:         snapshot.ID,
		Name:       snapshot.Name,
		State:      snapshot.State,
		Provider:   "parallels",
		Kind:       checkpointKindParallels,
		ResourceID: req.Server.CloudID,
		Region:     parallelsHostRefForConfig(cfg),
		Direct:     true,
	}, nil
}

func parallelsPowerOffState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "poweroff", "powered off", "stopped":
		return true
	default:
		return false
	}
}

func parallelsRunningState(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "running")
}

func nativeCheckpointCreateDriver(cfg Config, server Server, target SSHTarget, strategy string) (checkpointNativeCreateDriver, bool) {
	if _, ok := directParallelsNativeCheckpointKind(cfg, server, target, strategy); ok {
		return directParallelsCheckpointDriver{}, true
	}
	if kind, ok := directNativeCheckpointKind(cfg, server, target, strategy); ok {
		switch kind {
		case checkpointKindAWSAMI:
			return directAWSAMICheckpointDriver{}, true
		}
	}
	if _, ok := nativeCheckpointKind(cfg, server, target, strategy); ok {
		return coordinatorCheckpointDriver{}, true
	}
	return nil, false
}

func (a App) createNativeCheckpoint(ctx context.Context, cfg Config, server Server, target SSHTarget, leaseID, name, repoName, workdir, strategy string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, map[string]string, error) {
	if provider, ok := nativeCheckpointLifecycleProvider(cfg, server); ok {
		result, err := provider.CreateNativeCheckpoint(ctx, NativeCheckpointCreateRequest{
			Config:      cfg,
			Server:      server,
			Target:      target,
			LeaseID:     leaseID,
			Name:        name,
			RepoName:    repoName,
			Workdir:     workdir,
			Strategy:    strategy,
			NoReboot:    noReboot,
			Wait:        wait,
			WaitTimeout: waitTimeout,
			Stderr:      a.Stderr,
		})
		return coordinatorImageFromNativeCheckpoint(result.Image), result.Metadata, err
	}
	driver, ok := nativeCheckpointCreateDriver(cfg, server, target, strategy)
	if !ok {
		if cfg.Coordinator == "" {
			return CoordinatorImage{}, nil, exit(2, "native checkpoints require a configured coordinator")
		}
		return CoordinatorImage{}, nil, exit(2, "native checkpoints support brokered AWS Linux/macOS leases and brokered Azure/GCP Linux leases only")
	}
	image, err := driver.Create(ctx, checkpointNativeCreateRequest{
		Cfg:         cfg,
		Server:      server,
		Target:      target,
		LeaseID:     leaseID,
		Name:        name,
		RepoName:    repoName,
		Workdir:     workdir,
		Strategy:    strategy,
		NoReboot:    noReboot,
		Wait:        wait,
		WaitTimeout: waitTimeout,
		Stderr:      a.Stderr,
	})
	return image, nil, err
}

func (a App) createAWSAMICheckpoint(ctx context.Context, cfg Config, target SSHTarget, leaseID, name, repoName string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, error) {
	image, _, err := a.createNativeCheckpoint(ctx, cfg, Server{Provider: "aws", CloudID: leaseID}, target, leaseID, name, repoName, "", checkpointStrategyImage, noReboot, wait, waitTimeout)
	return image, err
}

func coordinatorImageFromNativeCheckpoint(image NativeCheckpointImage) CoordinatorImage {
	return CoordinatorImage{
		ID:         image.ID,
		Name:       image.Name,
		State:      image.State,
		Provider:   image.Provider,
		Kind:       image.Kind,
		Region:     image.Region,
		ResourceID: image.ResourceID,
		Direct:     image.Direct,
	}
}

func nativeCheckpointLifecycleProvider(cfg Config, server Server) (NativeCheckpointLifecycleProvider, bool) {
	providerName := firstNonBlank(server.Provider, cfg.Provider)
	provider, err := ProviderFor(providerName)
	if err != nil {
		return nil, false
	}
	lifecycle, ok := provider.(NativeCheckpointLifecycleProvider)
	return lifecycle, ok
}

func (a App) createDirectAWSAMICheckpoint(ctx context.Context, cfg Config, server Server, target SSHTarget, leaseID, name, repoName string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, error) {
	return directAWSAMICheckpointDriver{}.Create(ctx, checkpointNativeCreateRequest{
		Cfg:         cfg,
		Server:      server,
		Target:      target,
		LeaseID:     leaseID,
		Name:        name,
		RepoName:    repoName,
		NoReboot:    noReboot,
		Wait:        wait,
		WaitTimeout: waitTimeout,
		Stderr:      a.Stderr,
	})
}

func waitForDirectAWSImage(ctx context.Context, client *AWSClient, imageID, accountID string, timeout time.Duration, stderr io.Writer) (CoordinatorImage, error) {
	deadline := time.Now().Add(timeout)
	var last CoordinatorImage
	for {
		image, err := client.GetImageCheckpoint(ctx, imageID)
		if err != nil {
			return CoordinatorImage{}, err
		}
		if image.AccountID == "" {
			image.AccountID = accountID
		}
		last = image
		state := strings.ToLower(image.State)
		if state == "available" || state == "ready" || state == "succeeded" || state == "completed" {
			return image, nil
		}
		if state == "failed" || state == "invalid" {
			return CoordinatorImage{}, exit(5, "image %s failed", imageID)
		}
		if time.Now().After(deadline) {
			return CoordinatorImage{}, exit(5, "timed out waiting for image %s; last state=%s", imageID, last.State)
		}
		_, _ = fmt.Fprintf(stderr, "waiting image=%s state=%s\n", imageID, blank(image.State, "pending"))
		select {
		case <-ctx.Done():
			return CoordinatorImage{}, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
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

func prepareNativeImageSource(ctx context.Context, target SSHTarget) error {
	command := remotePrepareNativeImageCommand()
	if out, err := runSSHCombinedOutput(ctx, target, "bash -lc "+shellQuote(command)); err != nil {
		return exit(7, "prepare native checkpoint source: %v: %s", err, trimFailureDetail(out))
	}
	return nil
}

func remotePrepareNativeImageCommand() string {
	return "if command -v cloud-init >/dev/null 2>&1; then sudo cloud-init clean --logs; fi; sync"
}

func nativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	capability, ok := providerNativeCheckpointCapability(cfg, server, target, strategy)
	if !ok || capability.Direct {
		return "", false
	}
	if capability.Kind == "" {
		return "", false
	}
	return capability.Kind, true
}

func parallelsNativeCheckpointKind(cfg Config, server Server, strategy string) (string, bool) {
	return directParallelsNativeCheckpointKind(cfg, server, SSHTarget{}, strategy)
}

func directParallelsNativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	capability, ok := providerNativeCheckpointCapability(cfg, server, target, strategy)
	if !ok || !capability.Direct || capability.Kind != checkpointKindParallels {
		return "", false
	}
	return capability.Kind, true
}

func directNativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	capability, ok := providerNativeCheckpointCapability(cfg, server, target, strategy)
	if !ok || !capability.Direct {
		return "", false
	}
	if capability.Kind == "" {
		return "", false
	}
	return capability.Kind, true
}

func providerNativeCheckpointCapability(cfg Config, server Server, target SSHTarget, strategy string) (NativeCheckpointCapability, bool) {
	providerName := firstNonBlank(server.Provider, cfg.Provider)
	provider, err := ProviderFor(providerName)
	if err != nil {
		return NativeCheckpointCapability{}, false
	}
	capabilityProvider, ok := provider.(NativeCheckpointProvider)
	if !ok {
		return NativeCheckpointCapability{}, false
	}
	return capabilityProvider.NativeCheckpointCapability(NativeCheckpointRequest{
		Config:           cfg,
		Server:           server,
		Target:           target,
		Strategy:         normalizeCheckpointStrategy(strategy),
		StrategyExplicit: !isAutoCheckpointStrategy(strategy),
	})
}

func (record checkpointRecord) nativeProvider() string {
	return firstNonBlank(record.Native.Provider, checkpointProviderForKind(record.Kind), record.Provider)
}

func (record checkpointRecord) nativeResourceID() string {
	switch record.Kind {
	case checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk:
		return firstNonBlank(record.Native.Resource, record.Native.ImageID)
	default:
		return record.Native.ImageID
	}
}

func (record checkpointRecord) nativeDeleteID() string {
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

func (record checkpointRecord) isDirectAWSAMI() bool {
	return record.nativeProvider() == "aws" && record.Kind == checkpointKindAWSAMI && record.Native.Direct
}

func (record *checkpointRecord) applyNativeImage(image CoordinatorImage, noReboot bool) {
	record.Kind = checkpointKindForProviderImage(image)
	record.Native.Provider = firstNonBlank(image.Provider, checkpointProviderForKind(record.Kind), record.Provider)
	record.Native.ImageID = image.ID
	record.Native.Kind = image.Kind
	record.Native.Name = image.Name
	record.Native.State = image.State
	record.Native.Region = image.Region
	record.Native.AccountID = image.AccountID
	record.Native.Project = image.Project
	record.Native.Resource = image.ResourceID
	record.Native.SnapshotIDs = image.SnapshotIDs
	record.Native.Direct = image.Direct
	record.Native.Strategy = checkpointStrategyForKind(record.Kind)
	record.Native.NoReboot = noReboot
}

func applyNativeImageCheckpointRecord(record *checkpointRecord, image CoordinatorImage, noReboot bool) {
	record.applyNativeImage(image, noReboot)
}

func applyAWSAMIImageCheckpointRecord(record *checkpointRecord, image CoordinatorImage, noReboot bool) {
	record.applyNativeImage(image, noReboot)
}

func nativeCheckpointResourceID(record checkpointRecord) string {
	return record.nativeResourceID()
}

func nativeCheckpointDeleteID(record checkpointRecord) string {
	return record.nativeDeleteID()
}

func nativeCheckpointResourceRequest(record checkpointRecord) NativeCheckpointResourceRequest {
	return NativeCheckpointResourceRequest{
		Image: NativeCheckpointImage{
			ID:         record.Native.ImageID,
			Name:       record.Native.Name,
			State:      record.Native.State,
			Provider:   record.nativeProvider(),
			Kind:       record.Kind,
			Region:     record.Native.Region,
			ResourceID: record.Native.Resource,
			Direct:     record.Native.Direct,
		},
		Metadata: record.Native.Metadata,
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
	case checkpointKindDockerCommit:
		return checkpointKindDockerCommit
	}
	switch image.Provider {
	case "azure":
		return checkpointKindAzure
	case "gcp":
		return checkpointKindGCP
	case "parallels":
		return checkpointKindParallels
	case "local-container":
		return checkpointKindDockerCommit
	default:
		return checkpointKindAWSAMI
	}
}

func checkpointStrategyForKind(kind string) string {
	switch kind {
	case checkpointKindAWSAMI, checkpointKindAzure, checkpointKindGCP, checkpointKindDockerCommit:
		return checkpointStrategyImage
	case checkpointKindAWSEBS, checkpointKindAzureOS, checkpointKindGCPDisk, checkpointKindParallels:
		return checkpointStrategyDiskSnapshot
	default:
		return ""
	}
}

func directAWSCheckpointConfig(record checkpointRecord) (Config, bool) {
	if !record.isDirectAWSAMI() {
		return Config{}, false
	}
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, false
	}
	cfg.Provider = "aws"
	if record.Native.Region != "" {
		cfg.AWSRegion = record.Native.Region
	}
	return cfg, true
}

func verifyDirectAWSCheckpoint(ctx context.Context, audit checkpointAudit, cfg Config, providerID, expectedAccountID string) checkpointAudit {
	client, clientErr := newAWSClient(ctx, cfg)
	if clientErr != nil {
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = clientErr.Error()
		return audit
	}
	return verifyDirectAWSCheckpointWithClient(ctx, audit, client, providerID, expectedAccountID)
}

func verifyDirectAWSCheckpointWithClient(ctx context.Context, audit checkpointAudit, client *AWSClient, providerID, expectedAccountID string) checkpointAudit {
	if guardErr := client.GuardAccount(ctx, expectedAccountID); guardErr != nil {
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = guardErr.Error()
		return audit
	}
	image, imageErr := client.GetImageCheckpoint(ctx, providerID)
	if imageErr != nil {
		if strings.Contains(imageErr.Error(), "InvalidAMIID.NotFound") || strings.Contains(imageErr.Error(), "aws image not found") {
			audit.ProviderState = "missing"
			audit.NextAction = "delete_local"
			return audit
		}
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = imageErr.Error()
		return audit
	}
	applyCheckpointImageAudit(&audit, image)
	return audit
}

func applyCheckpointImageAudit(audit *checkpointAudit, image CoordinatorImage) {
	audit.ProviderState = blank(image.State, "unknown")
	switch strings.ToLower(image.State) {
	case "available", "ready", "succeeded", "completed":
		audit.NextAction = "fork_or_delete"
	case "failed", "invalid":
		audit.NextAction = "delete"
	default:
		audit.NextAction = "wait_or_delete"
	}
}

func nativeCoordinatorImageRef(record checkpointRecord) CoordinatorImageRef {
	return CoordinatorImageRef{
		Provider: record.nativeProvider(),
		Region:   record.Native.Region,
		Project:  record.Native.Project,
		Kind:     firstNonBlank(record.Native.Kind, record.Kind),
	}
}

func nativeCheckpointForkRecord(record checkpointRecord) NativeCheckpointForkRecord {
	return NativeCheckpointForkRecord{
		Kind:        record.Kind,
		ImageID:     record.Native.ImageID,
		Name:        record.Native.Name,
		Resource:    record.Native.Resource,
		Region:      record.Native.Region,
		Project:     record.Native.Project,
		Direct:      record.Native.Direct,
		HostID:      record.HostID,
		TargetOS:    record.TargetOS,
		WindowsMode: record.WindowsMode,
		ServerType:  record.ServerType,
		Metadata:    record.Native.Metadata,
	}
}

func coordinatorStatusCode(err error) int {
	var httpErr CoordinatorHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

func applyNativeCheckpointForkConfig(cfg *Config, fs *flag.FlagSet, record checkpointRecord) error {
	cfg.Provider = record.nativeProvider()
	if record.Native.Direct {
		cfg.Coordinator = ""
		cfg.CoordToken = ""
		cfg.CoordTokenCommand = nil
	} else if cfg.CoordAdminToken != "" {
		cfg.CoordToken = cfg.CoordAdminToken
		cfg.CoordTokenCommand = nil
	}
	if record.TargetOS != "" {
		cfg.TargetOS = record.TargetOS
	}
	if record.WindowsMode != "" {
		cfg.WindowsMode = record.WindowsMode
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	forkProvider, ok := provider.(NativeCheckpointForkProvider)
	if !ok {
		return exit(2, "provider=%s does not support native checkpoint fork config", cfg.Provider)
	}
	azureOSDisk := ""
	azureOSDiskExplicit := flagWasSet(fs, "azure-os-disk")
	if azureOSDiskExplicit {
		azureOSDisk = fs.Lookup("azure-os-disk").Value.String()
	}
	if err := forkProvider.ApplyNativeCheckpointForkConfig(NativeCheckpointForkRequest{
		Config:              cfg,
		Record:              nativeCheckpointForkRecord(record),
		MarketExplicit:      flagWasSet(fs, "market"),
		AzureOSDisk:         azureOSDisk,
		AzureOSDiskExplicit: azureOSDiskExplicit,
	}); err != nil {
		return err
	}
	if !flagWasSet(fs, "type") {
		if record.ServerType != "" && !flagWasSet(fs, "class") {
			cfg.ServerType = record.ServerType
			cfg.ServerTypeExplicit = true
		} else {
			cfg.ServerTypeExplicit = false
			cfg.ServerType = serverTypeForConfig(*cfg)
		}
	}
	return nil
}

func applyNativeCheckpointForkConfigAndFlags(cfg *Config, fs *flag.FlagSet, record checkpointRecord, providerFlags providerFlagValues) error {
	if err := applyNativeCheckpointForkConfig(cfg, fs, record); err != nil {
		return err
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	flagProvider, ok := provider.(NativeCheckpointForkFlagProvider)
	if !ok {
		return nil
	}
	return flagProvider.ApplyNativeCheckpointForkFlags(cfg, fs, providerFlags[provider.Name()])
}

func applyAWSAMICheckpointForkConfig(cfg *Config, fs *flag.FlagSet, record checkpointRecord) error {
	return applyNativeCheckpointForkConfig(cfg, fs, record)
}
