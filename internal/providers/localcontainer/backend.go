package localcontainer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName        = "local-container"
	sshPort             = "2222"
	workRootMarkerName  = ".crabbox-local-container-work-root"
	dockerSocketInGuest = "/var/run/docker.sock"
	rollbackTimeout     = 10 * time.Second
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

type inspectContainer struct {
	ID              string            `json:"Id"`
	Name            string            `json:"Name"`
	Created         string            `json:"Created"`
	Config          inspectConfig     `json:"Config"`
	State           inspectState      `json:"State"`
	NetworkSettings inspectNetworking `json:"NetworkSettings"`
}

type inspectConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type inspectState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
}

type inspectNetworking struct {
	Ports map[string][]inspectPort `json:"Ports"`
}

type inspectPort struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *core.LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := validateCheckpointFork(ctx, cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	containers, err := b.listContainers(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(containers))
	for _, container := range containers {
		servers = append(servers, b.serverFromContainer(container, cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := core.LeaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s runtime=%s image=%s keep=%v\n", providerName, leaseID, slug, cfg.LocalContainer.Runtime, cfg.LocalContainer.Image, req.Keep)
	containerID, bootstrapDir, err := b.createContainer(ctx, cfg, name, leaseID, slug, publicKey, req.Keep)
	if err != nil {
		if req.Keep && containerID != "" {
			cleanupKey = false
		}
		return core.LeaseTarget{}, err
	}
	if req.Keep {
		cleanupKey = false
	}
	cleanupContainer := func() {
		if req.Keep {
			return
		}
		if err := b.removeContainer(context.Background(), containerID); err != nil {
			return
		}
		if bootstrapDir != "" && trustedBootstrapDir(bootstrapDir) {
			_ = os.RemoveAll(bootstrapDir)
		}
	}
	container, err := b.inspectContainer(ctx, containerID)
	if err != nil {
		cleanupContainer()
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, cfg, container, leaseID, slug, true)
	if err != nil {
		cleanupContainer()
		return core.LeaseTarget{}, err
	}
	if err := core.ClaimLeaseForRepoProviderScopePondCacheVolumes(leaseID, slug, providerName, b.claimScope(ctx), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, core.CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes)); err != nil {
		cleanupContainer()
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		cleanupContainer()
		return core.LeaseTarget{}, err
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, shortID(container.ID))
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	container, leaseID, slug, err := b.resolveContainer(ctx, req.ID)
	if err != nil {
		if req.ReleaseOnly && isExitCode(err, 4) {
			if lease, ok, releaseErr := b.resolveMissingClaimForRelease(ctx, req.ID); releaseErr != nil {
				return core.LeaseTarget{}, releaseErr
			} else if ok {
				return lease, nil
			}
		}
		return core.LeaseTarget{}, err
	}
	cfg := b.configForRun()
	readOnlyStatus := req.StatusOnly && !req.ReleaseOnly
	if strings.TrimSpace(leaseID) == "" && (!readOnlyStatus || req.Reclaim) {
		return core.LeaseTarget{}, localContainerOwnershipError(leaseID, container.ID)
	}
	owned, conflict, err := b.localContainerClaimStatus(ctx, leaseID, container.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if (conflict && (!readOnlyStatus || req.Reclaim)) || (!owned && !readOnlyStatus && (req.ReleaseOnly || !req.Reclaim)) {
		return core.LeaseTarget{}, localContainerOwnershipError(leaseID, container.ID)
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: b.serverFromContainer(container, cfg), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, container, leaseID, slug, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" && (!readOnlyStatus || req.Reclaim) {
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, b.claimScope(ctx), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.LeaseView, 0, len(containers))
	for _, container := range containers {
		servers = append(servers, b.serverFromContainer(container, cfg))
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	runtime, contextName := b.runtimeInfo(ctx)
	containers, err := b.listContainers(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	cfg := b.configForRun()
	msg := fmt.Sprintf("cli=ready control_plane=local inventory=ready api=list mutation=false leases=%d runtime=%s context=%s ssh_probe=%s image=%s docker_socket=%v", len(containers), runtime, blank(contextName, "-"), probe, cfg.LocalContainer.Image, cfg.LocalContainer.DockerSocket)
	return core.DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	lease := req.Lease
	appliedScope, err := b.applyCheckpointScopeLabels(ctx, lease.Server.Labels)
	if err != nil {
		return err
	}
	if !appliedScope {
		identifier := firstNonBlank(lease.LeaseID, lease.Server.Labels["lease"])
		if claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName); err != nil {
			return err
		} else if ok {
			if _, err := b.applyCheckpointScopeLabels(ctx, claim.Labels); err != nil {
				return err
			}
		}
	}
	id := strings.TrimSpace(req.Lease.Server.CloudID)
	if id == "" {
		if b.releaseMissingClaim(lease) {
			return nil
		}
		container, leaseID, _, err := b.resolveContainer(ctx, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		id = container.ID
		if lease.LeaseID == "" {
			lease.LeaseID = leaseID
		}
		lease.Server = b.serverFromContainer(container, b.configForRun())
	}
	if id == "" {
		return core.Exit(2, "provider=%s release requires a container id", providerName)
	}
	if err := b.requireExactLocalContainerClaim(ctx, lease.LeaseID, id); err != nil {
		return err
	}
	hostLeaseRoot := hostLeaseWorkRoot(lease)
	bootstrapDir := strings.TrimSpace(lease.Server.Labels["bootstrap_dir"])
	if err := b.removeContainer(ctx, id); err != nil {
		return err
	}
	var cleanupErr error
	if hostLeaseRoot != "" {
		cleanupErr = os.RemoveAll(hostLeaseRoot)
	}
	if bootstrapDir != "" && trustedBootstrapDir(bootstrapDir) {
		if err := os.RemoveAll(bootstrapDir); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
	}
	core.RemoveLeaseClaim(lease.LeaseID)
	core.RemoveStoredTestboxKey(lease.LeaseID)
	if cleanupErr != nil {
		return core.Exit(2, "remove local-container host work root %s: %v", hostLeaseRoot, cleanupErr)
	}
	return nil
}

func (b *backend) localContainerClaimStatus(ctx context.Context, leaseID, containerID string) (owned, conflict bool, err error) {
	leaseID = strings.TrimSpace(leaseID)
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, providerName)
	if err != nil {
		return false, false, err
	}
	if !ok || !exact || claim.LeaseID != leaseID {
		return false, false, nil
	}
	claimScope := strings.TrimSpace(claim.ProviderScope)
	currentScope := strings.TrimSpace(b.claimScope(ctx))
	claimCloudID := strings.TrimSpace(claim.CloudID)
	if claimScope != "" && claimScope != currentScope {
		return false, true, nil
	}
	if claimCloudID != "" && claimCloudID != strings.TrimSpace(containerID) {
		return false, true, nil
	}
	if claimScope == "" || currentScope == "" || claimCloudID == "" {
		return false, false, nil
	}
	return true, false, nil
}

func localContainerOwnershipError(leaseID, containerID string) error {
	return core.Exit(4, "local-container lease %q has no exact local claim bound to container %q in the current runtime scope; adopt it with an explicit --reclaim reuse before stop", strings.TrimSpace(leaseID), strings.TrimSpace(containerID))
}

func (b *backend) requireExactLocalContainerClaim(ctx context.Context, leaseID, containerID string) error {
	owned, _, err := b.localContainerClaimStatus(ctx, leaseID, containerID)
	if err != nil {
		return err
	}
	if !owned {
		return localContainerOwnershipError(leaseID, containerID)
	}
	return nil
}

func (b *backend) releaseMissingClaim(lease core.LeaseTarget) bool {
	leaseID := strings.TrimSpace(firstNonBlank(lease.LeaseID, lease.Server.Labels["lease"]))
	if leaseID == "" || strings.TrimSpace(lease.Server.CloudID) != "" {
		return false
	}
	if lease.Server.Labels["missing_container"] != "1" {
		return false
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
	return true
}

func (b *backend) resolveMissingClaimForRelease(ctx context.Context, identifier string) (core.LeaseTarget, bool, error) {
	claim, ok, err := localContainerClaimByIDOrSlug(identifier)
	if err != nil || !ok {
		return core.LeaseTarget{}, false, err
	}
	if _, err := b.applyCheckpointScopeLabels(ctx, claim.Labels); err != nil {
		return core.LeaseTarget{}, false, err
	}
	if !b.claimMatchesCurrentScope(ctx, claim) {
		return core.LeaseTarget{}, false, nil
	}
	labels := map[string]string{
		"lease":             claim.LeaseID,
		"slug":              claim.Slug,
		"provider":          providerName,
		"missing_container": "1",
	}
	for key, value := range claim.Labels {
		if strings.TrimSpace(value) != "" {
			labels[key] = value
		}
	}
	return core.LeaseTarget{
		LeaseID: claim.LeaseID,
		Server: core.Server{
			Provider: providerName,
			Name:     core.LeaseProviderName(claim.LeaseID, claim.Slug),
			Status:   "missing",
			Labels:   labels,
		},
	}, true, nil
}

func localContainerClaimByIDOrSlug(identifier string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	exactClaim, err := core.ReadLeaseClaim(identifier)
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	if exactClaim.LeaseID == identifier && exactClaim.Provider == providerName {
		return exactClaim, true, nil
	}
	normalized := core.NormalizeLeaseSlug(identifier)
	if normalized == "" {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	var matches []core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider == providerName && core.NormalizeLeaseSlug(claim.Slug) == normalized {
			matches = append(matches, claim)
		}
	}
	if len(matches) > 1 {
		return core.LeaseClaim{}, false, core.Exit(2, "local-container slug %s is ambiguous across %d lease claims; use a lease id", identifier, len(matches))
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func (b *backend) claimMatchesCurrentScope(ctx context.Context, claim core.LeaseClaim) bool {
	claimScope := strings.TrimSpace(claim.ProviderScope)
	if claimScope == "" {
		return true
	}
	return claimScope == strings.TrimSpace(b.claimScope(ctx))
}

func isExitCode(err error, code int) bool {
	var exit core.ExitError
	return core.AsExitError(err, &exit) && exit.Code == code
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	containers, err := b.listContainers(ctx)
	if err != nil {
		return err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	claimScope := b.claimScope(ctx)
	claimsByLease := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName {
			claimsByLease[claim.LeaseID] = claim
		}
	}
	liveLeases := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, container := range containers {
		server := b.serverFromContainer(container, b.configForRun())
		leaseID := strings.TrimSpace(server.Labels["lease"])
		if leaseID != "" {
			liveLeases[leaseID] = struct{}{}
		}
		claim, hasClaim := claimsByLease[leaseID]
		shouldDelete, reason := shouldCleanupLocalContainer(server, claim, hasClaim, now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip container id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove container id=%s name=%s lease=%s reason=%s\n", server.DisplayID(), server.Name, blank(leaseID, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove container id=%s name=%s lease=%s reason=%s\n", server.DisplayID(), server.Name, blank(leaseID, "-"), reason)
		if err := b.removeContainer(ctx, container.ID); err != nil {
			return err
		}
		var cleanupErr error
		hostLeaseRoot := hostLeaseWorkRootFromLabels(leaseID, server.Labels)
		if hostLeaseRoot != "" {
			cleanupErr = os.RemoveAll(hostLeaseRoot)
		}
		bootstrapDir := strings.TrimSpace(server.Labels["bootstrap_dir"])
		if bootstrapDir != "" && trustedBootstrapDir(bootstrapDir) {
			if err := os.RemoveAll(bootstrapDir); err != nil && cleanupErr == nil {
				cleanupErr = err
			}
		}
		if leaseID != "" {
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
		if cleanupErr != nil {
			return core.Exit(2, "remove local-container host work root %s: %v", hostLeaseRoot, cleanupErr)
		}
		removed++
	}
	claimsRemoved := 0
	for leaseID, claim := range claimsByLease {
		if leaseID == "" {
			continue
		}
		if _, ok := liveLeases[leaseID]; ok {
			continue
		}
		if !localContainerClaimMatchesScope(claim, claimScope, now) {
			continue
		}
		reason := "missing container"
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=%s\n", leaseID, blank(claim.Slug, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=%s\n", leaseID, blank(claim.Slug, "-"), reason)
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(containers))
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = core.TouchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	preservedKeys := []string{"bootstrap_dir", "container_id", "docker_socket", "host_work_root", "image", "runtime", "runtime_context", "ssh_port", "ssh_user", "work_root"}
	preservedKeys = append(preservedKeys, checkpointScopeMetadataKeys...)
	for _, key := range preservedKeys {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

func (b *backend) ValidateCheckpointForkWorkdir(ctx context.Context, lease core.LeaseTarget, workdir string) error {
	cfg := b.configForRun()
	if len(cfg.LocalContainer.Volumes) == 0 {
		return nil
	}
	workdir = strings.TrimSpace(workdir)
	if !path.IsAbs(workdir) {
		return core.Exit(2, "local-container checkpoint fork workdir %q must be an absolute container path", workdir)
	}
	containerID := strings.TrimSpace(lease.Server.CloudID)
	if containerID == "" {
		return core.Exit(2, "local-container checkpoint fork workdir validation requires a container id")
	}
	args := []string{"exec", containerID, "/bin/sh", "-c", validateCheckpointForkWorkdirScript, "crabbox-validate-checkpoint-workdir", workdir}
	for _, volume := range cfg.LocalContainer.Volumes {
		destination, err := localContainerVolumeDestination(volume)
		if err != nil {
			return err
		}
		args = append(args, destination)
	}
	result, err := b.docker(ctx, args, nil, nil)
	if err != nil {
		return commandError("validate local-container checkpoint fork workdir", result, err)
	}
	return nil
}

func (b *backend) configForRun() core.Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	b.detectContainerRuntime(&cfg)
	return cfg
}

func (b *backend) detectContainerRuntime(cfg *core.Config) {
	if core.LocalContainerRuntimeExplicit(*cfg) {
		return
	}
	runtimeName := strings.TrimSpace(cfg.LocalContainer.Runtime)
	if runtimeName != "" && runtimeName != "docker" {
		return
	}
	if commandAvailable("docker") {
		cfg.LocalContainer.Runtime = "docker"
		return
	}
	if commandAvailable("podman") {
		cfg.LocalContainer.Runtime = "podman"
	}
}

func commandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func applyDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TargetOS == core.TargetLinux {
		cfg.WindowsMode = ""
	}
	cfg.SSHFallbackPorts = []string{}
	if cfg.LocalContainer.Runtime == "" {
		cfg.LocalContainer.Runtime = "docker"
	}
	if cfg.LocalContainer.Image == "" {
		cfg.LocalContainer.Image = "debian:bookworm"
	}
	if cfg.LocalContainer.User == "" {
		cfg.LocalContainer.User = "crabbox"
	}
	if cfg.LocalContainer.DockerSocket && !localContainerWorkRootExplicit(*cfg) && isDefaultWorkRoot(cfg.LocalContainer.WorkRoot) && isDefaultWorkRoot(cfg.WorkRoot) {
		if runtime.GOOS == "windows" {
			cfg.LocalContainer.WorkRoot = "/work/crabbox"
		} else {
			cfg.LocalContainer.WorkRoot = defaultDockerSocketWorkRoot()
		}
	}
	if cfg.LocalContainer.WorkRoot == "" {
		if !isDefaultWorkRoot(cfg.WorkRoot) {
			cfg.LocalContainer.WorkRoot = cfg.WorkRoot
		} else {
			cfg.LocalContainer.WorkRoot = "/work/crabbox"
		}
	}
	if cfg.LocalContainer.Network == "" {
		cfg.LocalContainer.Network = "bridge"
	}
	cfg.SSHUser = cfg.LocalContainer.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	cfg.ServerType = localContainerDisplayImage(*cfg)
}

func localContainerDisplayImage(cfg core.Config) string {
	if name := strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[checkpointMetadataForkName]); name != "" {
		return name
	}
	return cfg.LocalContainer.Image
}

func (b *backend) createContainer(ctx context.Context, cfg core.Config, name, leaseID, slug, publicKey string, keep bool) (string, string, error) {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["runtime"] = cfg.LocalContainer.Runtime
	labels["image"] = localContainerDisplayImage(cfg)
	for _, key := range checkpointScopeMetadataKeys {
		if value := strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[key]); value != "" {
			labels[key] = value
		}
	}
	if contextName := strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[checkpointMetadataContext]); contextName != "" {
		labels["runtime_context"] = contextName
	}
	labels["ssh_user"] = cfg.LocalContainer.User
	labels["ssh_port"] = sshPort
	labels["docker_socket"] = boolEnv(cfg.LocalContainer.DockerSocket)
	containerWorkRoot := cfg.LocalContainer.WorkRoot
	hostWorkRoot := ""
	if cfg.LocalContainer.DockerSocket {
		hostWorkRoot, containerWorkRoot = dockerSocketWorkRoots(cfg)
		labels["host_work_root"] = hostWorkRoot
	}
	hostLeaseWorkRoot := ""
	cleanupHostLeaseWorkRoot := false
	defer func() {
		if cleanupHostLeaseWorkRoot && hostLeaseWorkRoot != "" {
			_ = os.RemoveAll(hostLeaseWorkRoot)
		}
	}()
	labels["work_root"] = containerWorkRoot
	cacheVolumeMounts, err := localContainerCacheVolumeMounts(cfg.Cache.Volumes)
	if err != nil {
		return "", "", err
	}
	hostVolumeDestinations, err := validateLocalContainerHostVolumes(cfg, containerWorkRoot)
	if err != nil {
		return "", "", err
	}
	args := []string{
		"run", "-d",
		"--name", name,
		"--hostname", name,
		"--user", "root",
		"--network", cfg.LocalContainer.Network,
		"-p", "127.0.0.1::" + sshPort,
		"-e", "CRABBOX_AUTHORIZED_KEY=" + publicKey,
		"-e", "CRABBOX_SSH_USER=" + cfg.LocalContainer.User,
		"-e", "CRABBOX_WORK_ROOT=" + containerWorkRoot,
		"-e", "CRABBOX_SSH_PORT=" + sshPort,
		"-e", "CRABBOX_DESKTOP=" + boolEnv(cfg.Desktop),
		"-e", "CRABBOX_DESKTOP_ENV=" + core.NormalizedDesktopEnv(cfg.DesktopEnv),
		"-e", "CRABBOX_BROWSER=" + boolEnv(cfg.Browser),
		"-e", "CRABBOX_DOCKER_SOCKET=" + boolEnv(cfg.LocalContainer.DockerSocket),
	}
	for i, volume := range cfg.Cache.Volumes {
		args = append(args, "-e", fmt.Sprintf("CRABBOX_CACHE_VOLUME_PATH_%d=%s", i, strings.TrimSpace(volume.Path)))
	}
	for i, destination := range hostVolumeDestinations {
		args = append(args, "-e", fmt.Sprintf("CRABBOX_HOST_VOLUME_PATH_%d=%s", i, destination))
	}
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	if cfg.LocalContainer.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(cfg.LocalContainer.CPUs))
	}
	if memory := strings.TrimSpace(cfg.LocalContainer.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if cfg.LocalContainer.DockerSocket {
		if err := os.MkdirAll(hostWorkRoot, 0o755); err != nil {
			return "", "", core.Exit(2, "create local-container host work root %s: %v", hostWorkRoot, err)
		}
		if err := markLocalContainerWorkRoot(hostWorkRoot); err != nil {
			return "", "", core.Exit(2, "mark local-container host work root %s: %v", hostWorkRoot, err)
		}
		leaseWorkRoot := filepath.Join(hostWorkRoot, leaseID)
		hostLeaseWorkRoot = leaseWorkRoot
		leaseWorkRootPreexisting := true
		if _, err := os.Lstat(leaseWorkRoot); os.IsNotExist(err) {
			leaseWorkRootPreexisting = false
		} else if err != nil {
			return "", "", core.Exit(2, "stat local-container host lease work root %s: %v", leaseWorkRoot, err)
		}
		if err := os.MkdirAll(leaseWorkRoot, 0o777); err != nil {
			return "", "", core.Exit(2, "create local-container host lease work root %s: %v", leaseWorkRoot, err)
		}
		cleanupHostLeaseWorkRoot = !leaseWorkRootPreexisting
		if err := os.Chmod(leaseWorkRoot, 0o777); err != nil {
			return "", "", core.Exit(2, "make local-container host lease work root writable %s: %v", leaseWorkRoot, err)
		}
		args = append(args, "-v", hostWorkRoot+":"+containerWorkRoot)
		socketPath, err := b.dockerSocketMountPath(ctx)
		if err != nil {
			return "", "", err
		}
		args = append(args, "-v", socketPath+":"+dockerSocketInGuest)
		if isPodmanRuntime(cfg.LocalContainer.Runtime) {
			args = append(args, "--security-opt", "label=disable")
		}
	}
	for _, mount := range cacheVolumeMounts {
		args = append(args, "-v", mount)
	}
	for _, vol := range cfg.LocalContainer.Volumes {
		args = append(args, "-v", vol)
	}
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-*")
	if err != nil {
		return "", "", core.Exit(2, "create bootstrap script directory: %v", err)
	}
	bootstrapPath := filepath.Join(bootstrapDir, "bootstrap.sh")
	if err := os.WriteFile(bootstrapPath, []byte(bootstrapScript), 0o644); err != nil {
		os.RemoveAll(bootstrapDir)
		return "", "", core.Exit(2, "write bootstrap script: %v", err)
	}
	args = append(args, "--label", "bootstrap_dir="+bootstrapDir)
	args = append(args, "-v", bootstrapDir+":/tmp/crabbox-bootstrap:ro")
	args = append(args, cfg.LocalContainer.Image, "/bin/sh", "/tmp/crabbox-bootstrap/bootstrap.sh")
	result, err := b.docker(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		containerID, owned, inspectErr := b.ownedContainerID(cleanupCtx, leaseID, bootstrapDir)
		if owned && keep {
			cleanupHostLeaseWorkRoot = false
			return containerID, bootstrapDir, commandError("container run", result, err)
		}
		if owned {
			if removeErr := b.removeContainer(cleanupCtx, containerID); removeErr == nil {
				os.RemoveAll(bootstrapDir)
			} else {
				cleanupHostLeaseWorkRoot = false
			}
		} else if inspectErr == nil {
			os.RemoveAll(bootstrapDir)
		} else {
			cleanupHostLeaseWorkRoot = false
		}
		return "", "", commandError("container run", result, err)
	}
	id := strings.TrimSpace(result.Stdout)
	if id == "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		containerID, owned, inspectErr := b.ownedContainerID(cleanupCtx, leaseID, bootstrapDir)
		if owned {
			cleanupHostLeaseWorkRoot = false
			return containerID, bootstrapDir, nil
		}
		if inspectErr == nil {
			os.RemoveAll(bootstrapDir)
		} else if !keep {
			if removeErr := b.removeContainer(cleanupCtx, name); removeErr == nil {
				os.RemoveAll(bootstrapDir)
			} else {
				cleanupHostLeaseWorkRoot = false
			}
		} else {
			cleanupHostLeaseWorkRoot = false
		}
		return "", "", core.Exit(2, "%s run did not return a container id", cfg.LocalContainer.Runtime)
	}
	cleanupHostLeaseWorkRoot = false
	return id, bootstrapDir, nil
}

func localContainerCacheVolumeMounts(volumes []core.CacheVolumeConfig) ([]string, error) {
	mounts := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		key := strings.TrimSpace(volume.Key)
		path := strings.TrimSpace(volume.Path)
		if key == "" {
			return nil, core.Exit(2, "cache volume key is required")
		}
		if strings.Contains(key, ":") {
			return nil, core.Exit(2, "cache volume key %q must not contain ':'", key)
		}
		if path == "" {
			return nil, core.Exit(2, "cache volume path is required")
		}
		if !strings.HasPrefix(path, "/") {
			return nil, core.Exit(2, "cache volume path %q must be absolute", path)
		}
		mounts = append(mounts, localContainerCacheVolumeName(key)+":"+path)
	}
	return mounts, nil
}

func localContainerCacheVolumeName(key string) string {
	key = strings.TrimSpace(key)
	sum := sha256.Sum256([]byte(key))
	var safe strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
			safe.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			safe.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			safe.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			safe.WriteRune(r)
		default:
			safe.WriteByte('-')
		}
		if safe.Len() >= 80 {
			break
		}
	}
	name := strings.Trim(safe.String(), ".-_")
	if name == "" {
		name = "volume"
	}
	return fmt.Sprintf("crabbox-cache-%s-%x", name, sum[:6])
}

func (b *backend) dockerSocketMountPath(ctx context.Context) (string, error) {
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return dockerSocketMountPathFromHost(host)
	}
	if result, err := b.docker(ctx, []string{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"}, nil, nil); err == nil {
		host := strings.TrimSpace(result.Stdout)
		if host != "" && host != "<no value>" {
			var decoded string
			if err := json.Unmarshal([]byte(host), &decoded); err == nil {
				host = decoded
			}
			return dockerSocketMountPathFromHost(host)
		}
	}
	if runtime.GOOS != "linux" {
		return dockerSocketInGuest, nil
	}
	return validateDockerSocketMountPath(dockerSocketInGuest)
}

func dockerSocketMountPathFromHost(host string) (string, error) {
	return dockerSocketMountPathFromHostForGOOS(host, runtime.GOOS)
}

func dockerSocketMountPathFromHostForGOOS(host, goos string) (string, error) {
	if goos == "windows" && windowsDockerPipeHost(host) {
		return dockerSocketInGuest, nil
	}
	path, ok := localDockerSocketPath(host)
	if !ok {
		return "", core.Exit(2, "local-container socket pass-through requested but DOCKER_HOST %q is not a local Unix socket", host)
	}
	if goos != "linux" {
		return dockerSocketInGuest, nil
	}
	return validateDockerSocketMountPath(path)
}

func dockerSocketWorkRoots(cfg core.Config) (string, string) {
	return dockerSocketWorkRootsForGOOSExplicit(cfg.LocalContainer.WorkRoot, runtime.GOOS, localContainerWorkRootExplicit(cfg))
}

func dockerSocketWorkRootsForGOOS(workRoot, goos string) (string, string) {
	return dockerSocketWorkRootsForGOOSExplicit(workRoot, goos, false)
}

func localContainerWorkRootExplicit(cfg core.Config) bool {
	return core.IsWorkRootExplicit(&cfg) || core.LocalContainerWorkRootExplicit(cfg)
}

func dockerSocketWorkRootsForGOOSExplicit(workRoot, goos string, explicit bool) (string, string) {
	workRoot = strings.TrimSpace(workRoot)
	if workRoot == "" {
		workRoot = "/work/crabbox"
	}
	if goos == "windows" {
		if windowsHostPath(workRoot) {
			return workRoot, "/work/crabbox"
		}
		return defaultDockerSocketWorkRoot(), workRoot
	}
	// Default Docker-socket work roots are host cache paths. Keep that host
	// storage location, but mount it somewhere the unprivileged SSH user can
	// traverse inside the container.
	if !explicit && filepath.Clean(workRoot) == filepath.Clean(defaultDockerSocketWorkRoot()) {
		return workRoot, "/work/crabbox"
	}
	return workRoot, workRoot
}

func windowsHostPath(path string) bool {
	path = strings.TrimSpace(path)
	if len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	return strings.HasPrefix(path, `\\`)
}

func windowsDockerPipeHost(host string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(host)), "npipe:")
}

func localDockerSocketPath(host string) (string, bool) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", false
	}
	if strings.HasPrefix(host, "/") {
		return host, true
	}
	if strings.HasPrefix(host, "unix://") {
		u, err := url.Parse(host)
		if err == nil && u.Path != "" {
			return u.Path, true
		}
		path := strings.TrimPrefix(host, "unix://")
		if strings.HasPrefix(path, "/") {
			return path, true
		}
	}
	return "", false
}

func validateDockerSocketMountPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", core.Exit(2, "local-container socket pass-through requested but %s is not available: %v", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", core.Exit(2, "local-container socket pass-through requested but %s is not a socket", path)
	}
	return path, nil
}

func defaultDockerSocketWorkRoot() string {
	if cache, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cache) != "" {
		return filepath.Join(cache, "crabbox", "local-container-work")
	}
	return filepath.Join(os.TempDir(), "crabbox-local-container-work")
}

func markLocalContainerWorkRoot(root string) error {
	return os.WriteFile(filepath.Join(root, workRootMarkerName), []byte("crabbox local-container work root\n"), 0o644)
}

func (b *backend) prepareLease(ctx context.Context, cfg core.Config, container inspectContainer, leaseID, slug string, wait bool) (core.LeaseTarget, error) {
	server := b.serverFromContainer(container, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" {
		cfg.LocalContainer.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.LocalContainer.WorkRoot = root
		cfg.WorkRoot = root
	}
	host, port, err := containerSSHHostPort(container)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			cfg.SSHKey = keyPath
		}
	}
	target := core.SSHTargetFromConfig(cfg, host)
	target.Port = port
	target.ReadyCheck = localContainerReadyCheck(cfg)
	if wait {
		if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "local container ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) listContainers(ctx context.Context) ([]inspectContainer, error) {
	result, err := b.docker(ctx, []string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=" + providerName, "--format", "{{.ID}}"}, nil, nil)
	if err != nil {
		return nil, commandError("container list", result, err)
	}
	ids := strings.Fields(result.Stdout)
	containers := make([]inspectContainer, 0, len(ids))
	for _, id := range ids {
		container, err := b.inspectContainer(ctx, id)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (b *backend) inspectContainer(ctx context.Context, id string) (inspectContainer, error) {
	result, err := b.docker(ctx, []string{"inspect", id}, nil, nil)
	if err != nil {
		return inspectContainer{}, commandError("container inspect", result, err)
	}
	var containers []inspectContainer
	if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
		return inspectContainer{}, core.Exit(2, "parse container inspect for %s: %v", id, err)
	}
	if len(containers) == 0 {
		return inspectContainer{}, core.Exit(4, "container not found: %s", id)
	}
	return containers[0], nil
}

func (b *backend) resolveContainer(ctx context.Context, identifier string) (inspectContainer, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return inspectContainer{}, "", "", core.Exit(2, "provider=%s requires --id <lease-id-or-slug-or-container>", providerName)
	}
	exactClaim, err := core.ReadLeaseClaim(identifier)
	if err != nil {
		return inspectContainer{}, "", "", err
	}
	if exactClaim.LeaseID == identifier && exactClaim.Provider == providerName {
		if _, err := b.applyCheckpointScopeLabels(ctx, exactClaim.Labels); err != nil {
			return inspectContainer{}, "", "", err
		}
		return b.findContainerForClaim(ctx, exactClaim)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return inspectContainer{}, "", "", err
	}
	normalized := core.NormalizeLeaseSlug(identifier)
	slugClaims := make([]core.LeaseClaim, 0, 1)
	for i := range claims {
		claim := claims[i]
		if claim.Provider != providerName {
			continue
		}
		if normalized != "" && core.NormalizeLeaseSlug(claim.Slug) == normalized {
			slugClaims = append(slugClaims, claim)
		}
	}
	if len(slugClaims) > 1 {
		return inspectContainer{}, "", "", core.Exit(2, "local-container slug %s is ambiguous across %d lease claims; use a lease id", identifier, len(slugClaims))
	}
	containers, listErr := b.listContainers(ctx)
	for _, container := range containers {
		labels := container.Config.Labels
		leaseID := labels["lease"]
		slug := labels["slug"]
		name := strings.TrimPrefix(container.Name, "/")
		if container.ID == identifier || shortID(container.ID) == identifier || name == identifier || leaseID == identifier || (normalized != "" && core.NormalizeLeaseSlug(slug) == normalized) {
			if len(slugClaims) == 1 && slugClaims[0].LeaseID != leaseID {
				return inspectContainer{}, "", "", core.Exit(2, "local-container slug %s matches ambient lease %s and scoped lease %s; use a lease id", identifier, leaseID, slugClaims[0].LeaseID)
			}
			if len(slugClaims) == 1 {
				claim := slugClaims[0]
				if applied, err := b.applyCheckpointScopeLabels(ctx, claim.Labels); err != nil {
					return inspectContainer{}, "", "", err
				} else if applied {
					return b.findContainerForClaim(ctx, claim)
				}
			}
			return container, leaseID, slug, nil
		}
	}
	if len(slugClaims) == 1 {
		claim := slugClaims[0]
		if _, err := b.applyCheckpointScopeLabels(ctx, claim.Labels); err != nil {
			return inspectContainer{}, "", "", err
		}
		return b.findContainerForClaim(ctx, claim)
	}
	if listErr != nil {
		return inspectContainer{}, "", "", listErr
	}
	return inspectContainer{}, "", "", core.Exit(4, "local-container lease not found: %s", identifier)
}

func (b *backend) applyCheckpointScopeLabels(ctx context.Context, labels map[string]string) (bool, error) {
	metadata := checkpointScopeMetadataFromLabels(labels)
	if len(metadata) == 0 {
		return false, nil
	}
	scope := checkpointScopeFromMetadata(metadata, b.cfg.LocalContainer.Runtime)
	if err := validateCheckpointScope(ctx, scope); err != nil {
		return false, err
	}
	b.cfg.LocalContainer.CheckpointMetadata = metadata
	if runtimeName := strings.TrimSpace(metadata[checkpointMetadataRuntime]); runtimeName != "" {
		b.cfg.LocalContainer.Runtime = runtimeName
		core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	}
	return true, nil
}

func (b *backend) findContainerForClaim(ctx context.Context, claim core.LeaseClaim) (inspectContainer, string, string, error) {
	containers, err := b.listContainers(ctx)
	if err != nil {
		return inspectContainer{}, "", "", err
	}
	if boundID := strings.TrimSpace(claim.CloudID); boundID != "" {
		for _, container := range containers {
			if strings.TrimSpace(container.ID) == boundID {
				labels := container.Config.Labels
				return container, firstNonBlank(claim.LeaseID, labels["lease"]), firstNonBlank(claim.Slug, labels["slug"]), nil
			}
		}
		return inspectContainer{}, "", "", core.Exit(4, "local-container lease not found: %s", firstNonBlank(claim.Slug, claim.LeaseID))
	}
	var matched *inspectContainer
	for _, container := range containers {
		labels := container.Config.Labels
		if labels["lease"] == claim.LeaseID {
			if matched != nil {
				return inspectContainer{}, "", "", core.Exit(2, "local-container lease %s matches multiple containers; reclaim by exact container id", claim.LeaseID)
			}
			candidate := container
			matched = &candidate
		}
	}
	if matched != nil {
		labels := matched.Config.Labels
		return *matched, labels["lease"], labels["slug"], nil
	}
	return inspectContainer{}, "", "", core.Exit(4, "local-container lease not found: %s", firstNonBlank(claim.Slug, claim.LeaseID))
}

func (b *backend) removeContainer(ctx context.Context, id string) error {
	result, err := b.docker(ctx, []string{"rm", "-f", id}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("container remove", result, err)
	}
	return nil
}

func (b *backend) ownedContainerID(ctx context.Context, leaseID, bootstrapDir string) (string, bool, error) {
	result, err := b.docker(ctx, []string{"ps", "-aq", "--filter", "label=lease=" + leaseID}, nil, nil)
	if err != nil {
		return "", false, err
	}
	for _, id := range strings.Fields(result.Stdout) {
		container, err := b.inspectContainer(ctx, id)
		if err != nil {
			return "", false, err
		}
		labels := container.Config.Labels
		if labels["lease"] != leaseID || labels["bootstrap_dir"] != bootstrapDir {
			continue
		}
		containerID := strings.TrimSpace(container.ID)
		if containerID == "" {
			containerID = id
		}
		return containerID, true, nil
	}
	return "", false, nil
}

func (b *backend) runtimeInfo(ctx context.Context) (string, string) {
	cfg := b.configForRun()
	version, err := b.docker(ctx, []string{"version", "--format", "{{.Client.Version}}"}, nil, nil)
	if err != nil {
		return "unknown", ""
	}
	return strings.TrimSpace(version.Stdout), b.runtimeContext(ctx, cfg.LocalContainer.Runtime)
}

func (b *backend) runtimeContext(ctx context.Context, runtimeName string) string {
	if isPodmanRuntime(runtimeName) {
		return "default"
	}
	contextName, err := b.docker(ctx, []string{"context", "show"}, nil, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(contextName.Stdout)
}

func (b *backend) claimScope(ctx context.Context) string {
	cfg := b.configForRun()
	if metadata := cfg.LocalContainer.CheckpointMetadata; len(metadata) != 0 {
		scope := checkpointScopeFromMetadata(metadata, cfg.LocalContainer.Runtime)
		contextName := scope.Context
		if contextName == "" && scope.Host != "" {
			contextName = "default"
		}
		return localContainerClaimScope(scope.Runtime, contextName, scope.Host)
	}
	return localContainerClaimScope(
		cfg.LocalContainer.Runtime,
		b.runtimeContext(ctx, cfg.LocalContainer.Runtime),
		b.runtimeHost(ctx, cfg.LocalContainer.Runtime),
	)
}

func localContainerClaimScope(runtimeName, contextName string, hostValues ...string) string {
	runtimeName = strings.TrimSpace(runtimeName)
	contextName = strings.TrimSpace(contextName)
	host := ""
	if len(hostValues) > 0 {
		host = strings.TrimSpace(hostValues[0])
	}
	if runtimeName == "" || contextName == "" {
		return ""
	}
	scope := "runtime:" + runtimeName + "/context:" + contextName
	if host != "" {
		scope += "/host:" + host
	}
	return scope
}

func (b *backend) runtimeHost(ctx context.Context, runtimeName string) string {
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return host
	}
	if isPodmanRuntime(runtimeName) {
		return ""
	}
	result, err := b.docker(ctx, []string{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"}, nil, nil)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(result.Stdout)
	if host == "" || host == "<no value>" {
		return ""
	}
	var decoded string
	if err := json.Unmarshal([]byte(host), &decoded); err == nil {
		host = decoded
	}
	return strings.TrimSpace(host)
}

func localContainerClaimMatchesScope(claim core.LeaseClaim, currentScope string, now time.Time) bool {
	currentScope = strings.TrimSpace(currentScope)
	claimScope := strings.TrimSpace(claim.ProviderScope)
	if currentScope != "" && claimScope == currentScope {
		return true
	}
	return claimScope == "" && localContainerClaimExpired(claim, now)
}

func localContainerClaimExpired(claim core.LeaseClaim, now time.Time) bool {
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil || lastUsed.IsZero() {
		return false
	}
	idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
	if idle <= 0 {
		return false
	}
	return now.After(lastUsed.Add(idle).Add(12 * time.Hour))
}

func (b *backend) docker(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	cfg := b.configForRun()
	var env []string
	if metadata := cfg.LocalContainer.CheckpointMetadata; len(metadata) != 0 {
		scope := checkpointScopeFromMetadata(metadata, cfg.LocalContainer.Runtime)
		if scope.Context != "" {
			args = append([]string{"--context", scope.Context}, args...)
		}
		env = checkpointEnvForScope(scope)
	}
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   cfg.LocalContainer.Runtime,
		Args:   args,
		Env:    env,
		Stdout: stdout,
		Stderr: stderr,
	})
}

func (b *backend) serverFromContainer(container inspectContainer, cfg core.Config) core.Server {
	labels := map[string]string{}
	for key, value := range container.Config.Labels {
		if strings.TrimSpace(value) == "" {
			continue
		}
		labels[key] = value
	}
	labels["container_id"] = shortID(container.ID)
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["server_type"] == "" {
		labels["server_type"] = container.Config.Image
	}
	if labels["state"] == "" {
		labels["state"] = container.State.Status
	}
	host, port, _ := containerSSHHostPort(container)
	if port != "" {
		labels["ssh_port"] = port
	}
	server := core.Server{
		CloudID:  container.ID,
		Provider: providerName,
		Name:     strings.TrimPrefix(container.Name, "/"),
		Status:   container.State.Status,
		Labels:   labels,
	}
	if container.State.Running && labels["state"] == "ready" {
		server.Status = "ready"
	}
	server.PublicNet.IPv4.IP = host
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.LocalContainer.Image)
	return server
}

func containerSSHHostPort(container inspectContainer) (string, string, error) {
	ports := container.NetworkSettings.Ports[sshPort+"/tcp"]
	if len(ports) == 0 {
		return "", "", core.Exit(4, "container %s has no published SSH port", shortID(container.ID))
	}
	host := strings.TrimSpace(ports[0].HostIP)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return host, strings.TrimSpace(ports[0].HostPort), nil
}

func commandError(action string, result core.LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return core.Exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return core.Exit(code, "%s failed: %v", action, err)
}

func isPodmanRuntime(runtimeName string) bool {
	return filepath.Base(strings.TrimSpace(runtimeName)) == "podman"
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func hostLeaseWorkRoot(lease core.LeaseTarget) string {
	return hostLeaseWorkRootFromLabels(firstNonBlank(lease.LeaseID, lease.Server.Labels["lease"]), lease.Server.Labels)
}

func hostLeaseWorkRootFromLabels(leaseID string, labels map[string]string) string {
	if labels["docker_socket"] != "1" {
		return ""
	}
	root := strings.TrimSpace(firstNonBlank(labels["host_work_root"], labels["work_root"]))
	leaseID = strings.TrimSpace(leaseID)
	if root == "" || leaseID == "" || !filepath.IsAbs(root) {
		return ""
	}
	if !safeLocalContainerLeaseID(leaseID) {
		return ""
	}
	root = filepath.Clean(root)
	if !trustedLocalContainerWorkRoot(root) {
		return ""
	}
	leaseRoot := filepath.Join(root, leaseID)
	rel, err := filepath.Rel(root, leaseRoot)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return ""
	}
	return leaseRoot
}

func safeLocalContainerLeaseID(leaseID string) bool {
	if !strings.HasPrefix(leaseID, "cbx_") || len(leaseID) <= len("cbx_") {
		return false
	}
	for _, r := range strings.TrimPrefix(leaseID, "cbx_") {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func trustedBootstrapDir(dir string) bool {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return false
	}
	base := filepath.Base(dir)
	if !strings.HasPrefix(base, "crabbox-bootstrap-") {
		return false
	}
	parent := filepath.Dir(dir)
	return parent == filepath.Clean(os.TempDir())
}

func trustedLocalContainerWorkRoot(root string) bool {
	info, err := os.Stat(filepath.Join(root, workRootMarkerName))
	if err == nil && !info.IsDir() {
		return true
	}
	return filepath.Clean(root) == filepath.Clean(defaultDockerSocketWorkRoot())
}

func shouldCleanupLocalContainer(server core.Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	labels := server.Labels
	if labels == nil {
		return false, "missing labels"
	}
	if strings.EqualFold(labels["keep"], "true") {
		return false, "keep=true"
	}
	if !strings.EqualFold(server.Status, "running") && server.Status != "ready" {
		return true, "container state=" + blank(server.Status, "unknown")
	}
	if hasClaim {
		lastUsed, err := time.Parse(time.RFC3339, claim.LastUsedAt)
		if err != nil || lastUsed.IsZero() {
			return false, "claim active"
		}
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		if idle <= 0 {
			return false, "claim active"
		}
		expires := lastUsed.Add(idle)
		if now.After(expires.Add(12 * time.Hour)) {
			return true, "claim expired"
		}
		return false, "claim active"
	}
	if expires, ok := localContainerLabelTime(labels["expires_at"]); ok {
		if now.After(expires.Add(12 * time.Hour)) {
			return true, "expired"
		}
		return false, "not expired"
	}
	return false, "missing claim"
}

func localContainerLabelTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0).UTC(), true
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func localContainerReadyCheck(cfg core.Config) string {
	checks := []string{
		"git --version >/tmp/crabbox-ready.log 2>&1",
		"rsync --version >>/tmp/crabbox-ready.log 2>&1",
		"python3 --version >>/tmp/crabbox-ready.log 2>&1",
		"test -d " + shellQuote(cfg.LocalContainer.WorkRoot),
	}
	if cfg.Desktop {
		switch core.NormalizedDesktopEnv(cfg.DesktopEnv) {
		case "wayland", "gnome":
			checks = append(checks,
				"pgrep -x labwc >/dev/null",
				"pgrep -x wayvnc >/dev/null",
				"ss -ltn | grep -q '127.0.0.1:5900'",
				"test -s /var/lib/crabbox/vnc.password",
			)
		default:
			checks = append(checks,
				"pgrep -f 'Xvfb :99' >/dev/null",
				"pgrep -f 'x11vnc.*-rfbport 5900' >/dev/null",
				"ss -ltn | grep -q '127.0.0.1:5900'",
				"test -s /var/lib/crabbox/vnc.password",
			)
		}
	}
	if cfg.Browser {
		checks = append(checks,
			"test -s /var/lib/crabbox/browser.env",
			". /var/lib/crabbox/browser.env",
			"test -x \"$BROWSER\"",
			"\"$BROWSER\" --version >>/tmp/crabbox-ready.log 2>&1",
		)
	}
	for i, check := range checks {
		checks[i] = localContainerReadyCheckWithDiagnostics(check)
	}
	return strings.Join(checks, " && ")
}

func localContainerReadyCheckWithDiagnostics(check string) string {
	message := "local-container ready-check failed: " + check
	return "{ " + check + "; } || { status=$?; printf '%s\n' " + shellQuote(message) + " >&2; if [ -f /tmp/crabbox-ready.log ]; then printf '%s\n' '--- /tmp/crabbox-ready.log ---' >&2; cat /tmp/crabbox-ready.log >&2; fi; exit \"$status\"; }"
}

func validateLocalContainerHostVolumes(cfg core.Config, workRoot string) ([]string, error) {
	workRoot = strings.TrimSpace(workRoot)
	if len(cfg.LocalContainer.Volumes) > 0 && !path.IsAbs(workRoot) {
		return nil, core.Exit(2, "local-container work root %q must be an absolute container path when host volumes are mounted", workRoot)
	}
	workRoot = path.Clean(workRoot)
	user := strings.TrimSpace(cfg.LocalContainer.User)
	homeDir := path.Join("/home", user)
	if user == "root" {
		homeDir = "/root"
	}
	managedPaths := []string{
		workRoot,
		homeDir,
		"/bin",
		"/boot",
		"/dev",
		"/etc",
		"/home",
		"/lib",
		"/lib64",
		"/proc",
		"/root",
		"/run",
		"/sbin",
		"/sys",
		"/tmp",
		"/usr",
		"/var",
	}
	for _, volume := range cfg.Cache.Volumes {
		managedPaths = append(managedPaths, volume.Path)
	}
	destinations := make([]string, 0, len(cfg.LocalContainer.Volumes))
	for _, volume := range cfg.LocalContainer.Volumes {
		destination, err := localContainerVolumeDestination(volume)
		if err != nil {
			return nil, err
		}
		for _, managedPath := range managedPaths {
			if containerPathsOverlap(destination, managedPath) {
				return nil, core.Exit(2, "local-container volume %q targets %s, which overlaps bootstrap-managed path %s", volume, destination, path.Clean(managedPath))
			}
		}
		destinations = append(destinations, destination)
	}
	return destinations, nil
}

func localContainerVolumeDestination(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	original := spec
	lastColon := strings.LastIndex(spec, ":")
	if lastColon < 0 {
		return "", core.Exit(2, "invalid local-container volume %q; expected host:container[:options]", original)
	}
	destination := spec[lastColon+1:]
	if !strings.HasPrefix(destination, "/") {
		spec = spec[:lastColon]
		lastColon = strings.LastIndex(spec, ":")
		if lastColon < 0 {
			return "", core.Exit(2, "invalid local-container volume %q; expected host:container[:options]", original)
		}
		destination = spec[lastColon+1:]
	}
	if !strings.HasPrefix(destination, "/") {
		return "", core.Exit(2, "invalid local-container volume destination %q; expected an absolute container path", destination)
	}
	if strings.ContainsAny(destination, "\r\n") {
		return "", core.Exit(2, "invalid local-container volume destination %q; line breaks are not allowed", destination)
	}
	return path.Clean(destination), nil
}

func containerPathsOverlap(left, right string) bool {
	left = path.Clean(strings.TrimSpace(left))
	right = path.Clean(strings.TrimSpace(right))
	if left == "." || right == "." {
		return false
	}
	return left == right ||
		containerPathHasPrefix(left, right) ||
		containerPathHasPrefix(right, left)
}

func containerPathHasPrefix(value, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(value, "/")
	}
	return strings.HasPrefix(value, prefix+"/")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isDefaultWorkRoot(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == "/work/crabbox"
}

const localContainerDockerSigningKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"

const installVerifiedAPTKeyringScript = `
install_verified_apt_keyring() {
  apt_key_url="$1"
  apt_key_target="$2"
  apt_key_expected_fingerprint="$3"
  install -m 0755 -d "$(dirname "$apt_key_target")"
  apt_key_tmp_dir="$(mktemp -d "${apt_key_target}.tmp.XXXXXX")"
  apt_key_download="$apt_key_tmp_dir/downloaded.asc"
  apt_key_home="$apt_key_tmp_dir/gnupg"
  apt_key_output="$apt_key_tmp_dir/keyring.gpg"
  install -m 0700 -d "$apt_key_home"
  if curl -fsSL "$apt_key_url" >"$apt_key_download" &&
    GNUPGHOME="$apt_key_home" gpg --batch --import "$apt_key_download" >/dev/null 2>&1; then
    apt_key_actual_fingerprint="$(
      GNUPGHOME="$apt_key_home" gpg --batch --with-colons --fingerprint "$apt_key_expected_fingerprint" 2>/dev/null |
        awk -F: '$1 == "fpr" { print $10; exit }' || true
    )"
    if [ "$apt_key_actual_fingerprint" = "$apt_key_expected_fingerprint" ] &&
      GNUPGHOME="$apt_key_home" gpg --batch --export "$apt_key_expected_fingerprint" >"$apt_key_output" &&
      [ -s "$apt_key_output" ]; then
      chmod 0644 "$apt_key_output"
      mv -f "$apt_key_output" "$apt_key_target"
      rm -rf "$apt_key_tmp_dir"
      return 0
    fi
  fi
  rm -rf "$apt_key_tmp_dir"
  return 1
}
`

const bootstrapScript = `
set -eu
export DEBIAN_FRONTEND=noninteractive
user="${CRABBOX_SSH_USER:-crabbox}"
work_root="${CRABBOX_WORK_ROOT:-/work/crabbox}"
ssh_port="${CRABBOX_SSH_PORT:-2222}"
docker_signing_key_fingerprint="` + localContainerDockerSigningKeyFingerprint + `"
` + installVerifiedAPTKeyringScript + `
home_dir=""
if [ -r /etc/passwd ]; then
  while IFS=: read -r account _ _ _ _ account_home _; do
    if [ "$account" = "$user" ]; then
      home_dir="$account_home"
      break
    fi
  done < /etc/passwd
fi
if [ -z "$home_dir" ]; then
  if [ "$user" = root ]; then
    home_dir=/root
  else
    home_dir="/home/$user"
  fi
fi
container_paths_overlap() {
  left="${1%/}"
  right="${2%/}"
  [ -n "$left" ] || left=/
  [ -n "$right" ] || right=/
  [ "$left" = "$right" ] && return 0
  { [ "$left" = / ] || [ "$right" = / ]; } && return 0
  case "$left/" in "$right/"*) return 0 ;; esac
  case "$right/" in "$left/"*) return 0 ;; esac
  return 1
}
check_host_volume_path() {
  if ! command -v readlink >/dev/null 2>&1; then
    echo "local-container host volume validation requires readlink" >&2
    exit 127
  fi
  resolve_container_path() {
    candidate="${1%/}"
    [ -n "$candidate" ] || candidate=/
    probe="$candidate"
    suffix=""
    while :; do
      resolved="$(readlink -f "$probe" 2>/dev/null || true)"
      if [ -n "$resolved" ]; then
        printf '%s%s\n' "${resolved%/}" "$suffix"
        return
      fi
      [ "$probe" != / ] || break
      suffix="/${probe##*/}$suffix"
      probe="${probe%/*}"
      [ -n "$probe" ] || probe=/
    done
    printf '%s\n' "$candidate"
  }
  host_path="$(resolve_container_path "$1")"
  for managed_path in "$work_root" "$home_dir" /bin /boot /dev /etc /home /lib /lib64 /proc /root /run /sbin /sys /tmp /usr /var; do
    managed_path="$(resolve_container_path "$managed_path")"
    if container_paths_overlap "$host_path" "$managed_path"; then
      echo "local-container host volume target $host_path overlaps bootstrap-managed path $managed_path" >&2
      exit 2
    fi
  done
  env | sed -n 's/^CRABBOX_CACHE_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r cache_path; do
    [ -n "$cache_path" ] || continue
    cache_path="$(resolve_container_path "$cache_path")"
    if container_paths_overlap "$host_path" "$cache_path"; then
      echo "local-container host volume target $host_path overlaps bootstrap-managed cache path $cache_path" >&2
      exit 2
    fi
  done
}
env | sed -n 's/^CRABBOX_HOST_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r host_path; do
  [ -n "$host_path" ] || continue
  check_host_volume_path "$host_path"
done
need_install=0
	for tool in /usr/sbin/sshd git rsync curl sudo python3; do
	  command -v "$tool" >/dev/null 2>&1 || need_install=1
	done
	if [ "$need_install" = "1" ] && command -v apt-get >/dev/null 2>&1; then
	  apt-get update
	  apt-get install -y --no-install-recommends openssh-server ca-certificates git rsync curl sudo python3
	fi
if [ "${CRABBOX_DESKTOP:-0}" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
    if [ "${CRABBOX_DESKTOP_ENV:-xfce}" = "gnome" ]; then
      apt-get install -y --no-install-recommends labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard dbus-user-session xwayland xdg-desktop-portal-wlr xdg-desktop-portal-gtk gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme fonts-dejavu-core fonts-liberation iproute2 openssl procps netcat-openbsd novnc websockify
    else
      apt-get install -y --no-install-recommends labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr dbus-user-session xwayland xdg-desktop-portal-wlr fonts-dejavu-core fonts-liberation iproute2 openssl procps netcat-openbsd novnc websockify
    fi
  else
    apt-get install -y --no-install-recommends xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl arc-theme procps netcat-openbsd novnc websockify
  fi
fi
if [ "${CRABBOX_BROWSER:-0}" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  if apt-cache show chromium >/dev/null 2>&1; then
    apt-get install -y --no-install-recommends chromium || true
  fi
  if ! command -v chromium >/dev/null 2>&1 || ! chromium --version >/dev/null 2>&1; then
    rm -f /usr/local/bin/crabbox-browser
    if apt-cache show firefox-esr >/dev/null 2>&1; then
      apt-get install -y --no-install-recommends firefox-esr || true
    fi
  fi
  if ! command -v chromium >/dev/null 2>&1 && ! command -v firefox-esr >/dev/null 2>&1 && apt-cache show firefox >/dev/null 2>&1; then
    apt-get install -y --no-install-recommends firefox || true
  fi
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  install_docker_cli=0
  if command -v gpg >/dev/null 2>&1 || apt-get install -y --no-install-recommends gnupg; then
    if [ -r /etc/os-release ]; then
      . /etc/os-release
      case "${ID:-}" in
        debian|ubuntu)
          install -m 0755 -d /etc/apt/keyrings /etc/apt/sources.list.d
          if install_verified_apt_keyring "https://download.docker.com/linux/${ID}/gpg" /etc/apt/keyrings/docker.gpg "$docker_signing_key_fingerprint"; then
            codename="${VERSION_CODENAME:-}"
            if [ -n "$codename" ]; then
              arch="$(dpkg --print-architecture)"
              docker_source_tmp="$(mktemp /etc/apt/sources.list.d/docker.list.tmp.XXXXXX)"
              if printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/%s %s stable\n' "$arch" "$ID" "$codename" >"$docker_source_tmp" &&
                chmod 0644 "$docker_source_tmp" &&
                mv -f "$docker_source_tmp" /etc/apt/sources.list.d/docker.list; then
                rm -f /etc/apt/keyrings/docker.asc
                if apt-get update && apt-get install -y --no-install-recommends docker-ce-cli; then
                  install_docker_cli=1
                else
                  rm -f /etc/apt/sources.list.d/docker.list
                fi
              else
                rm -f "$docker_source_tmp"
              fi
            fi
          else
            echo "Docker APT signing key verification failed; falling back to distro Docker CLI" >&2
          fi
          ;;
      esac
    fi
  else
    echo "Docker APT signing key verification unavailable; falling back to distro Docker CLI" >&2
  fi
  if [ "$install_docker_cli" != "1" ]; then
    apt-get install -y --no-install-recommends docker.io
  fi
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker >/dev/null 2>&1; then
  echo "Docker-compatible socket requested but docker CLI is not installed; use a Debian/Ubuntu-compatible image or preinstall docker" >&2
  exit 127
fi
if ! command -v /usr/sbin/sshd >/dev/null 2>&1; then
  echo "missing /usr/sbin/sshd; use a Debian/Ubuntu-compatible image or a prebuilt Crabbox runner image" >&2
  exit 127
fi
if [ -f /etc/ssh/sshd_config ]; then
  if grep -qE '^[#[:space:]]*UsePAM[[:space:]]+' /etc/ssh/sshd_config; then
    sed -i 's/^[#[:space:]]*UsePAM[[:space:]].*/UsePAM no/' /etc/ssh/sshd_config
  else
    printf '\nUsePAM no\n' >> /etc/ssh/sshd_config
  fi
  if grep -qE '^[#[:space:]]*PasswordAuthentication[[:space:]]+' /etc/ssh/sshd_config; then
    sed -i 's/^[#[:space:]]*PasswordAuthentication[[:space:]].*/PasswordAuthentication no/' /etc/ssh/sshd_config
  else
    printf '\nPasswordAuthentication no\n' >> /etc/ssh/sshd_config
  fi
fi
if ! id "$user" >/dev/null 2>&1; then
  useradd -m -d "$home_dir" -s /bin/bash "$user"
fi
passwd -d "$user" >/dev/null 2>&1 || true
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && [ -S /var/run/docker.sock ]; then
  socket_gid="$(stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
  if [ -n "$socket_gid" ]; then
    socket_group="$(getent group "$socket_gid" | cut -d: -f1 || true)"
    if [ -z "$socket_group" ]; then
      socket_group="crabbox-docker"
      groupadd -g "$socket_gid" "$socket_group" 2>/dev/null || socket_group=""
    fi
    if [ -n "$socket_group" ]; then
      usermod -aG "$socket_group" "$user" || true
    fi
  fi
fi
mkdir -p /run/sshd "$work_root" "$home_dir/.ssh" /var/cache/crabbox/pnpm /var/cache/crabbox/npm
printf '%s\n' "$CRABBOX_AUTHORIZED_KEY" > "$home_dir/.ssh/authorized_keys"
chmod 700 "$home_dir/.ssh"
chmod 600 "$home_dir/.ssh/authorized_keys"
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ]; then
  chown -R "$user" "$home_dir/.ssh"
else
  chown -R "$user" "$home_dir/.ssh" "$work_root"
fi
chown -R "$user" /var/cache/crabbox
env | sed -n 's/^CRABBOX_CACHE_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r cache_path; do
  [ -n "$cache_path" ] || continue
  mkdir -p "$cache_path"
  chown -R "$user" "$cache_path"
done
if command -v sudo >/dev/null 2>&1; then
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" > /etc/sudoers.d/crabbox
  chmod 440 /etc/sudoers.d/crabbox
fi
if [ "${CRABBOX_DESKTOP:-0}" = "1" ]; then
  desktop_env="${CRABBOX_DESKTOP_ENV:-xfce}"
  case "$desktop_env" in
    xfce|wayland|gnome) ;;
    *) echo "CRABBOX_DESKTOP_ENV must be xfce, wayland, or gnome" >&2; exit 2 ;;
  esac
  install -d -m 0750 -o "$user" /var/lib/crabbox
  if [ ! -s /var/lib/crabbox/vnc.password ]; then
    (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
  fi
  if [ "$desktop_env" != "xfce" ]; then
    chown "$user" /var/lib/crabbox/vnc.password
    chmod 0600 /var/lib/crabbox/vnc.password
    runtime="/tmp/crabbox-runtime-$(id -u "$user")"
    install -d -m 0700 -o "$user" "$runtime" "$home_dir/.config" "$home_dir/.config/labwc" "$home_dir/.config/wayvnc"
    if [ "$desktop_env" = "gnome" ]; then
cat > "$home_dir/.config/labwc/autostart" <<'AUTOSTART'
wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
for _ in $(seq 1 20); do
  [ -S /tmp/.X11-unix/X0 ] && break
  sleep 0.2
done
export XDG_CURRENT_DESKTOP=GNOME
export XDG_SESSION_DESKTOP=gnome
theme="$(cat "$HOME/.config/crabbox/desktop-theme" 2>/dev/null || printf dark)"
if [ "$theme" = light ]; then
  export GTK_THEME=Adwaita
  gsettings set org.gnome.desktop.interface color-scheme prefer-light >/dev/null 2>&1 || true
  gsettings set org.gnome.desktop.interface gtk-theme Adwaita >/dev/null 2>&1 || true
else
  export GTK_THEME=Adwaita-dark
  gsettings set org.gnome.desktop.interface color-scheme prefer-dark >/dev/null 2>&1 || true
  gsettings set org.gnome.desktop.interface gtk-theme Adwaita-dark >/dev/null 2>&1 || true
fi
export DISPLAY="${DISPLAY:-:0}"
export GDK_BACKEND=x11
export MOZ_ENABLE_WAYLAND=0
wallpaper_file="$HOME/.config/crabbox/desktop-background-$theme.svg"
if command -v swaybg >/dev/null 2>&1; then
  (swaybg -i "$wallpaper_file" -m fill >/tmp/crabbox-swaybg.log 2>&1 || swaybg -c "#0d1117" >/tmp/crabbox-swaybg.log 2>&1) &
fi
gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &
nautilus --new-window "$HOME" >/tmp/crabbox-nautilus.log 2>&1 &
AUTOSTART
    else
cat > "$home_dir/.config/labwc/autostart" <<'AUTOSTART'
wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &
AUTOSTART
    fi
    chmod 0755 "$home_dir/.config/labwc/autostart"
    cat > "$home_dir/.config/wayvnc/config" <<'WAYVNC'
address=127.0.0.1
port=5900
enable_auth=false
xkb_layout=us
WAYVNC
    if [ "$desktop_env" = "gnome" ]; then
    cat >/usr/local/bin/crabbox-configure-desktop-theme <<'THEME'
#!/bin/sh
set -eu
requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"
user="${CRABBOX_DESKTOP_USER:-crabbox}"
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
config_dir="$home_dir/.config"
mode="$requested_mode"
if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
  mode="$(cat "$config_dir/crabbox/desktop-theme" 2>/dev/null || true)"
fi
case "$mode" in
  light|dark) ;;
  *) mode=dark ;;
esac
if [ "$mode" = "light" ]; then
  gtk_theme=Adwaita
  gtk_prefer_dark_ini=0
  gsettings_scheme=prefer-light
  terminal_fg="#1f2937"
  terminal_bg="#f8fafc"
  labwc_title_bg="#f3f4f6"
  labwc_title_fg="#111827"
  labwc_inactive_title_bg="#e5e7eb"
  labwc_inactive_title_fg="#374151"
  labwc_border="#cbd5e1"
  terminal_menu_bg="#f3f4f6"
  terminal_menu_fg="#111827"
  terminal_menu_hover_bg="#e5e7eb"
  wallpaper_bg="#e7eef7"
  wallpaper_panel="#d6e7f2"
  wallpaper_accent="#0891b2"
  wallpaper_grid="#b9c7d7"
else
  gtk_theme=Adwaita-dark
  gtk_prefer_dark_ini=1
  gsettings_scheme=prefer-dark
  terminal_fg="#e5e7eb"
  terminal_bg="#000000"
  labwc_title_bg="#1f2329"
  labwc_title_fg="#e5e7eb"
  labwc_inactive_title_bg="#111827"
  labwc_inactive_title_fg="#9ca3af"
  labwc_border="#30363d"
  terminal_menu_bg="#2b2f36"
  terminal_menu_fg="#d1d5db"
  terminal_menu_hover_bg="#374151"
  wallpaper_bg="#0d1117"
  wallpaper_panel="#111827"
  wallpaper_accent="#22d3ee"
  wallpaper_grid="#1f2937"
fi
if [ "$(id -u)" -eq 0 ]; then
  install -d -m 0700 -o "$user" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"
else
  mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
  chmod 0700 "$config_dir" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
fi
printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"
for gtk_dir in "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"; do
  cat > "$gtk_dir/settings.ini" <<EOF
[Settings]
gtk-theme-name=$gtk_theme
gtk-icon-theme-name=Adwaita
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
done
cat > "$home_dir/.gtkrc-2.0" <<EOF
gtk-theme-name="$gtk_theme"
gtk-icon-theme-name="Adwaita"
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
if [ "$(id -u)" -eq 0 ]; then
  chown -R "$user" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$home_dir/.gtkrc-2.0"
fi
if [ -f /var/lib/crabbox/desktop.env ]; then
  . /var/lib/crabbox/desktop.env
fi
display="${DISPLAY:-:0}"
runtime="${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u "$user")}"
dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"
if [ -z "$dbus_address" ]; then
  labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
  if [ -n "$labwc_pid" ] && [ -r "/proc/$labwc_pid/environ" ]; then
    dbus_address="$(tr '\0' '\n' < "/proc/$labwc_pid/environ" | sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p' | head -n1)"
  fi
fi
set_gnome_terminal_theme() {
  profiles="$(gsettings get org.gnome.Terminal.ProfilesList list 2>/dev/null | tr -d "[],'" || true)"
  default_profile="$(gsettings get org.gnome.Terminal.ProfilesList default 2>/dev/null | tr -d "'" || true)"
  if [ -n "$default_profile" ] && ! printf ' %s ' "$profiles" | grep -q " $default_profile "; then
    profiles="$profiles $default_profile"
  fi
  for profile in $profiles; do
    [ -n "$profile" ] || continue
    profile_path="/org/gnome/terminal/legacy/profiles:/:$profile/"
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-theme-colors false >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" foreground-color "$terminal_fg" >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" background-color "$terminal_bg" >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-transparent-background false >/dev/null 2>&1 || true
  done
}
set_gtk_chrome_theme() {
  cat > "$config_dir/gtk-3.0/gtk.css" <<EOF
menubar, .menubar {
  background-color: $terminal_menu_bg;
  color: $terminal_menu_fg;
}
menubar menuitem, menubar menuitem label {
  color: $terminal_menu_fg;
}
menubar menuitem:hover {
  background-color: $terminal_menu_hover_bg;
  color: $terminal_menu_fg;
}
EOF
}
set_labwc_theme() {
  mkdir -p "$config_dir/labwc"
  cat > "$config_dir/labwc/themerc-override" <<EOF
window.active.title.bg.color: $labwc_title_bg
window.active.label.text.color: $labwc_title_fg
window.inactive.title.bg.color: $labwc_inactive_title_bg
window.inactive.label.text.color: $labwc_inactive_title_fg
window.active.border.color: $labwc_border
window.inactive.border.color: $labwc_border
window.active.button.unpressed.image.color: $labwc_title_fg
window.inactive.button.unpressed.image.color: $labwc_inactive_title_fg
window.active.button.hover.image.color: $labwc_title_fg
window.inactive.button.hover.image.color: $labwc_inactive_title_fg
window.active.button.pressed.image.color: $labwc_title_fg
window.inactive.button.pressed.image.color: $labwc_inactive_title_fg
EOF
  if command -v labwc >/dev/null 2>&1; then
    labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
    if [ -n "$labwc_pid" ]; then
      LABWC_PID="$labwc_pid" XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" labwc --reconfigure >/dev/null 2>&1 || kill -HUP "$labwc_pid" >/dev/null 2>&1 || true
    fi
  fi
}
set_desktop_background() {
  wallpaper_file="$config_dir/crabbox/desktop-background-$mode.svg"
  cat > "$wallpaper_file" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1920 1080">
  <rect width="1920" height="1080" fill="$wallpaper_bg"/>
  <path d="M0 720 C360 620 520 760 860 650 C1210 540 1430 660 1920 520 L1920 1080 L0 1080 Z" fill="$wallpaper_panel"/>
  <g stroke="$wallpaper_grid" stroke-width="1" opacity="0.45">
    <path d="M0 180 H1920M0 360 H1920M0 540 H1920M0 720 H1920M0 900 H1920"/>
    <path d="M240 0 V1080M480 0 V1080M720 0 V1080M960 0 V1080M1200 0 V1080M1440 0 V1080M1680 0 V1080"/>
  </g>
  <path d="M220 740 C520 520 790 910 1090 670 S1510 520 1710 700" fill="none" stroke="$wallpaper_accent" stroke-width="18" stroke-linecap="round" opacity="0.8"/>
  <rect x="1320" y="180" width="360" height="170" rx="18" fill="$wallpaper_accent" opacity="0.12"/>
</svg>
EOF
  if command -v swaybg >/dev/null 2>&1; then
    pkill -u "$user" -x swaybg >/dev/null 2>&1 || true
    (XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -i "$wallpaper_file" -m fill >/tmp/crabbox-swaybg.log 2>&1 || XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -c "$wallpaper_bg" >/tmp/crabbox-swaybg.log 2>&1) &
  fi
}
target_uid="$(id -u "$user" 2>/dev/null || printf 0)"
if [ "$(id -u)" -eq 0 ] && [ "$target_uid" -ne 0 ]; then
  su "$user" -s /bin/sh -c "CRABBOX_DESKTOP_USER='$user' CRABBOX_DESKTOP_THEME='$mode' DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 /usr/local/bin/crabbox-configure-desktop-theme '$mode'" || true
  exit 0
fi
if command -v gsettings >/dev/null 2>&1; then
  if [ "$(id -u)" -eq 0 ]; then
    su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme' >/dev/null 2>&1 || true"
    su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme '$gtk_theme' >/dev/null 2>&1 || true"
  else
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme "$gtk_theme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 set_gnome_terminal_theme
  fi
fi
set_gtk_chrome_theme
set_labwc_theme
set_desktop_background
if [ "$(id -u)" -eq 0 ] && pgrep -u "$user" -x gnome-panel >/dev/null 2>&1; then
  pkill -TERM -u "$user" -x gnome-panel >/dev/null 2>&1 || true
  su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 GTK_THEME='$gtk_theme' nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &" >/dev/null 2>&1 || true
elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel >/dev/null 2>&1; then
  pkill -TERM -x gnome-panel >/dev/null 2>&1 || true
  DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
fi
previous_terminal_theme="$(cat "$config_dir/crabbox/gnome-terminal-theme" 2>/dev/null || true)"
printf '%s\n' "$mode" > "$config_dir/crabbox/gnome-terminal-theme"
if [ "$(id -u)" -ne 0 ] && [ "$mode" = dark ] && command -v gnome-terminal >/dev/null 2>&1 && { [ "$previous_terminal_theme" != "$mode" ] || ! pgrep -u "$(id -u)" -f '/gnome-terminal-server' >/dev/null 2>&1; }; then
  (sleep 0.4; DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" NO_AT_BRIDGE=1 gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &) >/dev/null 2>&1 &
fi
THEME
    chmod 0755 /usr/local/bin/crabbox-configure-desktop-theme
    fi
    chown -R "$user" "$home_dir/.config"
    cat >/usr/local/bin/crabbox-start-desktop <<'DESKTOP'
#!/bin/sh
set -eu
user="${CRABBOX_SSH_USER:-crabbox}"
desktop_env="${CRABBOX_DESKTOP_ENV:-wayland}"
case "$desktop_env" in
  wayland|gnome) ;;
  *) echo "CRABBOX_DESKTOP_ENV must be wayland or gnome for Wayland startup" >&2; exit 2 ;;
esac
runtime="/tmp/crabbox-runtime-$(id -u "$user")"
install -d -m 0700 -o "$user" "$runtime"
if ! pgrep -u "$user" -x labwc >/dev/null 2>&1; then
  rm -f /var/lib/crabbox/display.env
  su "$user" -s /bin/sh -c "CRABBOX_DESKTOP_ENV='$desktop_env' XDG_RUNTIME_DIR='$runtime' WLR_BACKENDS=headless WLR_LIBINPUT_NO_DEVICES=1 WLR_RENDERER=pixman MOZ_ENABLE_WAYLAND=1 dbus-run-session labwc >/tmp/crabbox-labwc.log 2>&1 &"
fi
display=""
for _ in $(seq 1 30); do
  for socket in "$runtime"/wayland-*; do
    [ -S "$socket" ] || continue
    display="${socket##*/}"
    break 2
  done
  sleep 1
done
[ -n "$display" ] || { echo "wayland socket not ready" >&2; exit 1; }
cat >/var/lib/crabbox/desktop.env <<EOF
CRABBOX_DESKTOP_ENV=$desktop_env
XDG_RUNTIME_DIR=$runtime
WAYLAND_DISPLAY=$display
EOF
if [ "$desktop_env" = "gnome" ]; then
  printf 'DISPLAY=:0\n' >>/var/lib/crabbox/desktop.env
  printf 'GDK_BACKEND=x11\n' >>/var/lib/crabbox/desktop.env
  printf 'MOZ_ENABLE_WAYLAND=0\n' >>/var/lib/crabbox/desktop.env
fi
chown "$user" /var/lib/crabbox/desktop.env
chmod 0644 /var/lib/crabbox/desktop.env
if [ "$desktop_env" = "gnome" ]; then
  CRABBOX_DESKTOP_USER="$user" /usr/local/bin/crabbox-configure-desktop-theme
fi
if ! ss -ltn | grep -q '127.0.0.1:5900'; then
  home_dir="$(getent passwd "$user" | cut -d: -f6)"
  su "$user" -s /bin/sh -c "XDG_RUNTIME_DIR='$runtime' WAYLAND_DISPLAY='$display' wayvnc --config '$home_dir/.config/wayvnc/config' --render-cursor --max-fps=60 >/tmp/crabbox-wayvnc.log 2>&1 &"
fi
DESKTOP
    chmod 0755 /usr/local/bin/crabbox-start-desktop
    CRABBOX_SSH_USER="$user" /usr/local/bin/crabbox-start-desktop
  else
  { head -c 8 /var/lib/crabbox/vnc.password; printf '\n'; head -c 8 /var/lib/crabbox/vnc.password; printf '\n\n'; } | x11vnc -storepasswd /var/lib/crabbox/vnc.pass >/dev/null 2>&1
  chown "$user" /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
  chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
  printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=:99\n' >/var/lib/crabbox/desktop.env
  chown "$user" /var/lib/crabbox/desktop.env
  chmod 0644 /var/lib/crabbox/desktop.env
  config_dir="$home_dir/.config"
  mode="${CRABBOX_DESKTOP_THEME:-}"
  if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
    mode="$(cat "$config_dir/crabbox/desktop-theme" 2>/dev/null || true)"
  fi
  case "$mode" in
    light|dark) ;;
    *) mode=dark ;;
  esac
  if [ "$mode" = "light" ]; then
    gtk_theme=Adwaita
    gtk_prefer_dark=false
    gtk_prefer_dark_ini=0
    panel_rgba="0.94 0.95 0.97 1"
    panel_css_bg="#eef2f7"
    panel_css_fg="#111827"
    gtk_candidates="Arc Greybird Adwaita"
    xfwm_candidates="Arc Greybird Daloa Default"
  else
    gtk_theme=Adwaita-dark
    gtk_prefer_dark=true
    gtk_prefer_dark_ini=1
    panel_rgba="0.12 0.13 0.15 1"
    panel_css_bg="#20242b"
    panel_css_fg="#e5e7eb"
    gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"
    xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"
  fi
  for candidate in $gtk_candidates; do
    if [ -d "/usr/share/themes/$candidate/gtk-3.0" ]; then
      gtk_theme="$candidate"
      break
    fi
  done
  xfwm_theme=Default
  for candidate in $xfwm_candidates; do
    if [ -d "/usr/share/themes/$candidate/xfwm4" ]; then
      xfwm_theme="$candidate"
      break
    fi
  done
  install -d -m 0700 -o "$user" "$config_dir/xfce4/xfconf/xfce-perchannel-xml" "$config_dir/xfce4/terminal" "$config_dir/gtk-3.0" "$config_dir/crabbox"
  printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"
  cat > "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml" <<XML
<?xml version="1.0" encoding="UTF-8"?>
<channel name="xsettings" version="1.0">
  <property name="Net" type="empty">
    <property name="ThemeName" type="string" value="$gtk_theme"/>
    <property name="IconThemeName" type="string" value="Adwaita"/>
  </property>
  <property name="Gtk" type="empty">
    <property name="ApplicationPreferDarkTheme" type="bool" value="$gtk_prefer_dark"/>
  </property>
</channel>
XML
  if [ ! -s "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml" ]; then
    cat > "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml" <<XML
<?xml version="1.0" encoding="UTF-8"?>
<channel name="xfwm4" version="1.0">
  <property name="general" type="empty">
    <property name="theme" type="string" value="$xfwm_theme"/>
    <property name="box_move" type="bool" value="false"/>
    <property name="box_resize" type="bool" value="false"/>
    <property name="move_opacity" type="int" value="100"/>
    <property name="resize_opacity" type="int" value="100"/>
    <property name="snap_resist" type="bool" value="false"/>
    <property name="snap_to_border" type="bool" value="false"/>
    <property name="snap_to_windows" type="bool" value="false"/>
    <property name="snap_width" type="int" value="0"/>
    <property name="tile_on_move" type="bool" value="false"/>
    <property name="use_compositing" type="bool" value="false"/>
    <property name="wrap_windows" type="bool" value="false"/>
  </property>
</channel>
XML
  fi
  if [ "$mode" = "light" ]; then
    terminal_fg="#1f2937"
    terminal_bg="#f8fafc"
    terminal_cursor="#111827"
  else
    terminal_fg="#e5e7eb"
    terminal_bg="#111827"
    terminal_cursor="#f3f4f6"
  fi
  cat > "$config_dir/xfce4/terminal/terminalrc" <<EOF
[Configuration]
ColorForeground=$terminal_fg
ColorBackground=$terminal_bg
ColorCursor=$terminal_cursor
MiscBell=FALSE
EOF
  cat > "$config_dir/gtk-3.0/settings.ini" <<EOF
[Settings]
gtk-theme-name=$gtk_theme
gtk-icon-theme-name=Adwaita
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
  cat > "$home_dir/.gtkrc-2.0" <<EOF
gtk-theme-name="$gtk_theme"
gtk-icon-theme-name="Adwaita"
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
  css_file="$config_dir/gtk-3.0/gtk.css"
  css_tmp="$(mktemp)"
  if [ -f "$css_file" ]; then
    sed '/^[/][*] crabbox desktop theme start [*][/]$/,/^[/][*] crabbox desktop theme end [*][/]$/d' "$css_file" > "$css_tmp" || true
  fi
  cat >> "$css_tmp" <<EOF
/* crabbox desktop theme start */
.xfce4-panel { background: $panel_css_bg; background-color: $panel_css_bg; color: $panel_css_fg; }
.xfce4-panel * { color: $panel_css_fg; text-shadow: none; -gtk-icon-shadow: none; }
.xfce4-panel button,
.xfce4-panel button.flat,
.xfce4-panel button:hover,
.xfce4-panel button:active,
.xfce4-panel button:checked,
.xfce4-panel button:focus,
.xfce4-panel button:backdrop,
.xfce4-panel .tasklist button,
.xfce4-panel .tasklist button:hover,
.xfce4-panel .tasklist button:active,
.xfce4-panel .tasklist button:checked,
.xfce4-panel .tasklist button:checked:hover,
.xfce4-panel .tasklist button:focus,
.xfce4-panel .tasklist button:backdrop,
.xfce4-panel .tasklist .toggle,
.xfce4-panel .tasklist .toggle:hover,
.xfce4-panel .tasklist .toggle:checked,
.xfce4-panel .tasklist .toggle:checked:hover,
.xfce4-panel .tasklist button:checked,
.xfce4-panel .tasklist button:active {
  background: $panel_css_bg;
  background-image: none;
  background-color: $panel_css_bg;
  border-image: none;
  border-color: $panel_css_fg;
  box-shadow: none;
  color: $panel_css_fg;
  outline-color: transparent;
  text-shadow: none;
  -gtk-icon-shadow: none;
}
.xfce4-panel .tasklist button label,
.xfce4-panel .tasklist .toggle label {
  color: $panel_css_fg;
  text-shadow: none;
}
/* crabbox desktop theme end */
EOF
  mv "$css_tmp" "$css_file"
  chown -R "$user" "$config_dir" "$home_dir/.gtkrc-2.0"
  cat >/usr/local/bin/crabbox-start-desktop <<'DESKTOP'
#!/bin/sh
set -eu
user="${CRABBOX_SSH_USER:-crabbox}"
runtime="/tmp/crabbox-runtime-$user"
requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
config_dir="$home_dir/.config"
mode="$requested_mode"
if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
  mode="$(cat "$config_dir/crabbox/desktop-theme" 2>/dev/null || true)"
fi
case "$mode" in
  light|dark) ;;
  *) mode=dark ;;
esac
if [ "$mode" = "light" ]; then
  gtk_theme=Adwaita
  gtk_prefer_dark=false
  gsettings_scheme=prefer-light
  root_color="#f4f6f8"
  terminal_fg="#1f2937"
  terminal_bg="#f8fafc"
  panel_rgba="0.94 0.95 0.97 1"
  panel_css_bg="#eef2f7"
  panel_css_fg="#111827"
  gtk_candidates="Arc Greybird Adwaita"
  xfwm_candidates="Arc Greybird Daloa Default"
else
  gtk_theme=Adwaita-dark
  gtk_prefer_dark=true
  gsettings_scheme=prefer-dark
  root_color="#20242b"
  terminal_fg="#e5e7eb"
  terminal_bg="#111827"
  panel_rgba="0.12 0.13 0.15 1"
  panel_css_bg="#20242b"
  panel_css_fg="#e5e7eb"
  gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"
  xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"
fi
for candidate in $gtk_candidates; do
  if [ -d "/usr/share/themes/$candidate/gtk-3.0" ]; then
    gtk_theme="$candidate"
    break
  fi
done
xfwm_theme=Default
for candidate in $xfwm_candidates; do
  if [ -d "/usr/share/themes/$candidate/xfwm4" ]; then
    xfwm_theme="$candidate"
    break
  fi
done
install -d -m 0700 -o "$user" "$config_dir/gtk-3.0" "$config_dir/crabbox"
printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"
css_file="$config_dir/gtk-3.0/gtk.css"
css_tmp="$(mktemp)"
if [ -f "$css_file" ]; then
  sed '/^[/][*] crabbox desktop theme start [*][/]$/,/^[/][*] crabbox desktop theme end [*][/]$/d' "$css_file" > "$css_tmp" || true
fi
cat >> "$css_tmp" <<EOF
/* crabbox desktop theme start */
.xfce4-panel { background: $panel_css_bg; background-color: $panel_css_bg; color: $panel_css_fg; }
.xfce4-panel * { color: $panel_css_fg; text-shadow: none; -gtk-icon-shadow: none; }
.xfce4-panel button,
.xfce4-panel button.flat,
.xfce4-panel button:hover,
.xfce4-panel button:active,
.xfce4-panel button:checked,
.xfce4-panel button:focus,
.xfce4-panel button:backdrop,
.xfce4-panel .tasklist button,
.xfce4-panel .tasklist button:hover,
.xfce4-panel .tasklist button:active,
.xfce4-panel .tasklist button:checked,
.xfce4-panel .tasklist button:checked:hover,
.xfce4-panel .tasklist button:focus,
.xfce4-panel .tasklist button:backdrop,
.xfce4-panel .tasklist .toggle,
.xfce4-panel .tasklist .toggle:hover,
.xfce4-panel .tasklist .toggle:checked,
.xfce4-panel .tasklist .toggle:checked:hover,
.xfce4-panel .tasklist button:checked,
.xfce4-panel .tasklist button:active {
  background: $panel_css_bg;
  background-image: none;
  background-color: $panel_css_bg;
  border-image: none;
  border-color: $panel_css_fg;
  box-shadow: none;
  color: $panel_css_fg;
  outline-color: transparent;
  text-shadow: none;
  -gtk-icon-shadow: none;
}
.xfce4-panel .tasklist button label,
.xfce4-panel .tasklist .toggle label {
  color: $panel_css_fg;
  text-shadow: none;
}
/* crabbox desktop theme end */
EOF
mv "$css_tmp" "$css_file"
chown -R "$user" "$config_dir/gtk-3.0" "$config_dir/crabbox"
install -d -m 0700 -o "$user" "$runtime"
if ! pgrep -u "$user" -f 'Xvfb :99' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "XDG_RUNTIME_DIR='$runtime' Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/crabbox-xvfb.log 2>&1 &"
fi
sleep 1
if ! pgrep -u "$user" -f 'xfce4-session|startxfce4' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' dbus-launch startxfce4 >/tmp/crabbox-desktop.log 2>&1 &"
fi
sleep 2
if command -v xfconf-query >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xsettings -p /Net/ThemeName -n -t string -s '$gtk_theme' >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xsettings -p /Net/IconThemeName -n -t string -s Adwaita >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme -n -t bool -s '$gtk_prefer_dark' >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/theme -n -t string -s '$xfwm_theme' >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/box_move -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/box_resize -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/move_opacity -n -t int -s 100 >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/resize_opacity -n -t int -s 100 >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/snap_resist -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/snap_to_border -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/snap_to_windows -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/snap_width -n -t int -s 0 >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/tile_on_move -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/use_compositing -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfwm4 -p /general/wrap_windows -n -t bool -s false >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfce4-panel -p /panels/dark-mode -n -t bool -s '$gtk_prefer_dark' >/dev/null 2>&1 || true"
  set -- $panel_rgba
  for panel_id in panel-1 panel-2; do
    su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfce4-panel -p /panels/$panel_id/background-style -n -t int -s 1 >/dev/null 2>&1 || true"
    su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfconf-query -c xfce4-panel -p /panels/$panel_id/background-rgba -n -a -t double -s '$1' -t double -s '$2' -t double -s '$3' -t double -s '$4' >/dev/null 2>&1 || true"
  done
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' pkill -TERM -x xfce4-panel >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' sh -c 'sleep 0.4; xfce4-panel >/tmp/crabbox-xfce4-panel-$user.log 2>&1 &' >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfwm4 --replace --compositor=off >/tmp/crabbox-xfwm4-replace-'$user'.log 2>&1 &"
fi
su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xsetroot -solid '$root_color' >/dev/null 2>&1 || true"
if command -v gsettings >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme' >/dev/null 2>&1 || true"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' gsettings set org.gnome.desktop.interface gtk-theme '$gtk_theme' >/dev/null 2>&1 || true"
fi
if command -v xfce4-terminal >/dev/null 2>&1 && ! pgrep -u "$user" -f 'xfce4-terminal.*Crabbox Desktop' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfce4-terminal --title='Crabbox Desktop' --geometry=110x32+48+48 >/tmp/crabbox-terminal.log 2>&1 &" || true
elif command -v xterm >/dev/null 2>&1 && ! pgrep -u "$user" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xterm -title 'Crabbox Desktop' -geometry 110x32+48+48 -bg '$terminal_bg' -fg '$terminal_fg' >/tmp/crabbox-terminal.log 2>&1 &" || true
fi
if ! ss -ltn | grep -q '127.0.0.1:5900'; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass -wait 16 -defer 8 -nowait_bog -o /tmp/crabbox-x11vnc.log >/tmp/crabbox-x11vnc.stdout.log 2>&1 &"
fi
DESKTOP
  chmod 0755 /usr/local/bin/crabbox-start-desktop
  CRABBOX_SSH_USER="$user" /usr/local/bin/crabbox-start-desktop
  fi
fi
if [ "${CRABBOX_BROWSER:-0}" = "1" ]; then
  browser_path=""
  for candidate in google-chrome chromium firefox-esr firefox; do
    if candidate_path="$(command -v "$candidate" 2>/dev/null)" && "$candidate_path" --version >/dev/null 2>&1; then
      browser_path="$candidate_path"
      break
    fi
  done
  if [ -z "$browser_path" ]; then
    echo "browser requested but no supported browser package is available for this image architecture" >&2
    exit 127
  fi
  browser_wrapper=/usr/local/bin/crabbox-browser
  case "$(basename "$browser_path")" in
    firefox*|iceweasel*)
      if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="${DISPLAY:-:0}"' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY MOZ_ENABLE_WAYLAND=1' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      else
        printf '%s\n' '#!/bin/sh' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      fi
      ;;
    *)
      if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="${DISPLAY:-:0}"' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" 'fi' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export MOZ_ENABLE_WAYLAND=1' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\"\$profile\" --ozone-platform=wayland --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      else
        printf '%s\n' '#!/bin/sh' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\"\$profile\" --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      fi
      ;;
  esac
  chmod 0755 "$browser_wrapper"
  install -d -m 0755 /var/lib/crabbox
  printf 'CHROME_BIN=%s\nBROWSER=%s\n' "$browser_wrapper" "$browser_wrapper" > /var/lib/crabbox/browser.env
  chown "$user" /var/lib/crabbox/browser.env
  chmod 0644 /var/lib/crabbox/browser.env
fi
exec /usr/sbin/sshd -D -e -p "$ssh_port"
`

const validateCheckpointForkWorkdirScript = `
set -eu
resolve_path() {
  python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$1"
}
paths_overlap() {
  left="${1%/}"
  right="${2%/}"
  [ -n "$left" ] || left=/
  [ -n "$right" ] || right=/
  [ "$left" = "$right" ] && return 0
  { [ "$left" = / ] || [ "$right" = / ]; } && return 0
  case "$left/" in "$right/"*) return 0 ;; esac
  case "$right/" in "$left/"*) return 0 ;; esac
  return 1
}
workdir="$(resolve_path "$1")"
shift
for mount_path in "$@"; do
  mount_path="$(resolve_path "$mount_path")"
  if paths_overlap "$workdir" "$mount_path"; then
    echo "checkpoint fork workdir $workdir overlaps local-container host volume target $mount_path" >&2
    exit 2
  fi
done
`
