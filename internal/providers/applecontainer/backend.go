package applecontainer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func isAppleContainerProvider(name string) bool {
	switch name {
	case providerName, "apple", "applecontainer":
		return true
	default:
		return false
	}
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
	if cfg.AppleContainer.CLIPath == "" {
		cfg.AppleContainer.CLIPath = "container"
	}
	if cfg.AppleContainer.Image == "" {
		cfg.AppleContainer.Image = core.BaseConfig().AppleContainer.Image
	}
	if cfg.AppleContainer.User == "" {
		cfg.AppleContainer.User = "crabbox"
	}
	if cfg.AppleContainer.WorkRoot == "" {
		if !isDefaultWorkRoot(cfg.WorkRoot) {
			cfg.AppleContainer.WorkRoot = cfg.WorkRoot
		} else {
			cfg.AppleContainer.WorkRoot = "/work/crabbox"
		}
	}
	cfg.SSHUser = cfg.AppleContainer.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.AppleContainer.WorkRoot
	cfg.ServerType = cfg.AppleContainer.Image
}

func isDefaultWorkRoot(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == "/work/crabbox"
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *core.LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) configForRun() core.Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

// container runs the Apple container CLI with the configured binary path.
func (b *backend) container(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	cfg := b.configForRun()
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   cfg.AppleContainer.CLIPath,
		Args:   args,
		Stdout: stdout,
		Stderr: stderr,
	})
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := requireMacOS(); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	containers, err := b.listContainers(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(containers))
	for _, c := range containers {
		servers = append(servers, b.serverFromContainer(c, cfg))
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
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s keep=%v\n", providerName, leaseID, slug, cfg.AppleContainer.Image, req.Keep)
	containerID, err := b.createContainer(ctx, cfg, name, leaseID, slug, publicKey, req.Keep)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Keep {
		cleanupKey = false
	}
	c, err := b.inspectContainer(ctx, containerID)
	if err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	c, err = b.waitForNetworkAddress(ctx, containerID, c, core.BootstrapWaitTimeout(cfg))
	if err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, cfg, c, leaseID, slug, true)
	if err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.ClaimLeaseForRepoProviderScopePondCacheVolumes(leaseID, slug, providerName, "", cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, core.CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes)); err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, c.id())
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := requireMacOS(); err != nil {
		return core.LeaseTarget{}, err
	}
	c, leaseID, slug, err := b.resolveContainer(ctx, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	readOnlyStatus := req.StatusOnly && !req.ReleaseOnly
	if strings.TrimSpace(leaseID) == "" && (!readOnlyStatus || req.Reclaim) {
		return core.LeaseTarget{}, appleContainerOwnershipError(leaseID, c.id())
	}
	owned, conflict, err := appleContainerClaimStatus(leaseID, c.id())
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if (conflict && (!readOnlyStatus || req.Reclaim)) || (!owned && !readOnlyStatus && (req.ReleaseOnly || !req.Reclaim)) {
		return core.LeaseTarget{}, appleContainerOwnershipError(leaseID, c.id())
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: b.serverFromContainer(c, cfg), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, c, leaseID, slug, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" && (!readOnlyStatus || req.Reclaim) {
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, "", cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	if err := requireMacOS(); err != nil {
		return nil, err
	}
	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0, len(containers))
	for _, c := range containers {
		views = append(views, b.serverFromContainer(c, cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	if err := requireMacOS(); err != nil {
		return core.DoctorResult{}, err
	}
	// `container system status` reports whether the background API service is
	// running. We use it as the readiness probe before listing.
	statusResult, statusErr := b.container(ctx, []string{"system", "status"}, nil, nil)
	if statusErr != nil {
		return core.DoctorResult{}, exit(2, "%s system status failed (is the container CLI installed and started with `container system start`?): %s", providerName, commandDetail(statusResult, statusErr))
	}
	// The lease path shells out to `container run` with detached mode, `--user`,
	// `--label`, and `--dns`. Probe that surface here so doctor fails fast on a
	// CLI whose service/list API exists but whose `run` subcommand is absent or
	// incompatible, instead of reporting ready and then failing on every
	// warmup/run.
	if err := b.checkRunSurface(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	containers, err := b.listContainers(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	msg := fmt.Sprintf("cli=ready run=ready control_plane=local system=ready inventory=ready mutation=false leases=%d image=%s", len(containers), cfg.AppleContainer.Image)
	return core.DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := requireMacOS(); err != nil {
		return err
	}
	lease := req.Lease
	id := strings.TrimSpace(req.Lease.Server.CloudID)
	if id == "" {
		c, leaseID, _, err := b.resolveContainer(ctx, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		id = c.id()
		if lease.LeaseID == "" {
			lease.LeaseID = leaseID
		}
	}
	if id == "" {
		return exit(2, "provider=%s release requires a container id", providerName)
	}
	if err := requireExactAppleContainerClaim(lease.LeaseID, id); err != nil {
		return err
	}
	if err := b.removeContainer(ctx, id); err != nil {
		return err
	}
	core.RemoveLeaseClaim(lease.LeaseID)
	core.RemoveStoredTestboxKey(lease.LeaseID)
	return nil
}

func appleContainerClaimStatus(leaseID, containerID string) (owned, conflict bool, err error) {
	leaseID = strings.TrimSpace(leaseID)
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, providerName)
	if err != nil {
		return false, false, err
	}
	if !ok || !exact || claim.LeaseID != leaseID {
		return false, false, nil
	}
	boundID := strings.TrimSpace(claim.CloudID)
	if boundID == "" {
		return false, false, nil
	}
	if boundID != strings.TrimSpace(containerID) {
		return false, true, nil
	}
	return true, false, nil
}

func appleContainerOwnershipError(leaseID, containerID string) error {
	return exit(4, "apple-container lease %q has no exact local claim bound to container %q; adopt it with an explicit --reclaim reuse before stop", strings.TrimSpace(leaseID), strings.TrimSpace(containerID))
}

func requireExactAppleContainerClaim(leaseID, containerID string) error {
	owned, _, err := appleContainerClaimStatus(leaseID, containerID)
	if err != nil {
		return err
	}
	if !owned {
		return appleContainerOwnershipError(leaseID, containerID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released lease=%s container=%s", lease.LeaseID, blank(lease.Server.CloudID, lease.Server.Labels["container_id"]))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	if err := requireMacOS(); err != nil {
		return err
	}
	containers, err := b.listContainers(ctx)
	if err != nil {
		return err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	claimsByLease := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName {
			claimsByLease[claim.LeaseID] = claim
		}
	}
	liveLeases := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, c := range containers {
		server := b.serverFromContainer(c, cfg)
		leaseID := strings.TrimSpace(server.Labels["lease"])
		if leaseID != "" {
			liveLeases[leaseID] = struct{}{}
		}
		claim, hasClaim := claimsByLease[leaseID]
		shouldDelete, reason := shouldCleanup(server, claim, hasClaim, now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip container id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove container id=%s name=%s lease=%s reason=%s\n", server.DisplayID(), server.Name, blank(leaseID, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove container id=%s name=%s lease=%s reason=%s\n", server.DisplayID(), server.Name, blank(leaseID, "-"), reason)
		if err := b.removeContainer(ctx, c.id()); err != nil {
			return err
		}
		if leaseID != "" {
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
		removed++
	}
	claimsRemoved := 0
	for leaseID := range claimsByLease {
		if leaseID == "" {
			continue
		}
		if _, ok := liveLeases[leaseID]; ok {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=missing container\n", leaseID)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s reason=missing container\n", leaseID)
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
	for _, key := range []string{"container_id", "image", "ssh_user", "ssh_port", "work_root"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

func (b *backend) createContainer(ctx context.Context, cfg core.Config, name, leaseID, slug, publicKey string, keep bool) (string, error) {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["image"] = cfg.AppleContainer.Image
	labels["ssh_user"] = cfg.AppleContainer.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.AppleContainer.WorkRoot
	cacheVolumeMounts, err := appleContainerCacheVolumeMounts(cfg.Cache.Volumes)
	if err != nil {
		return "", err
	}

	// `container run [<options>] <image> [<arguments>...]`. We detach (-d) and
	// pass the same Crabbox bootstrap contract used by local-container via
	// environment variables; command exec and file sync happen over standard
	// Crabbox SSH afterwards. Apple's runtime assigns a routable IP, so no
	// host port publishing is required.
	// Force the bootstrap to run as root: it installs packages, creates the SSH
	// user, writes /etc/sudoers.d, and starts sshd. An image with a non-root
	// default USER would otherwise exit before sshd starts and leave `crabbox
	// run` waiting on port 22 until timeout.
	args := []string{
		"run", "-d",
		"--name", name,
		"--user", "root",
		"-e", "CRABBOX_AUTHORIZED_KEY=" + publicKey,
		"-e", "CRABBOX_SSH_USER=" + cfg.AppleContainer.User,
		"-e", "CRABBOX_WORK_ROOT=" + cfg.AppleContainer.WorkRoot,
		"-e", "CRABBOX_SSH_PORT=" + sshPort,
	}
	for i, volume := range cfg.Cache.Volumes {
		args = append(args, "-e", fmt.Sprintf("CRABBOX_CACHE_VOLUME_PATH_%d=%s", i, strings.TrimSpace(volume.Path)))
	}
	// Labels keep ownership/lease metadata queryable from `container inspect`.
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	if cfg.AppleContainer.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(cfg.AppleContainer.CPUs))
	}
	if memory := strings.TrimSpace(cfg.AppleContainer.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	for _, mount := range cacheVolumeMounts {
		args = append(args, "--volume", mount)
	}
	if !appleContainerHasDNSArg(cfg.AppleContainer.ExtraRunArgs) {
		args = append(args, b.appleContainerDNSArgs(ctx)...)
	}
	args = append(args, cfg.AppleContainer.ExtraRunArgs...)
	args = append(args, cfg.AppleContainer.Image, "/bin/sh", "-lc", bootstrapScript)

	result, err := b.container(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return "", commandError("container run", result, err)
	}
	id := strings.TrimSpace(result.Stdout)
	if id == "" {
		// `container run --name X` uses the supplied name as the id; fall back
		// to it when the CLI does not echo an id on stdout.
		id = name
	}
	return id, nil
}

func appleContainerCacheVolumeMounts(volumes []core.CacheVolumeConfig) ([]string, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	root, err := appleContainerCacheRoot()
	if err != nil {
		return nil, err
	}
	mounts := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		key := strings.TrimSpace(volume.Key)
		path := strings.TrimSpace(volume.Path)
		if key == "" {
			return nil, exit(2, "cache volume key is required")
		}
		if strings.Contains(key, ":") {
			return nil, exit(2, "cache volume key %q must not contain ':'", key)
		}
		if path == "" {
			return nil, exit(2, "cache volume path is required")
		}
		if !strings.HasPrefix(path, "/") {
			return nil, exit(2, "cache volume path %q must be absolute", path)
		}
		hostPath := filepath.Join(root, appleContainerCacheVolumeName(key))
		if err := os.MkdirAll(hostPath, 0o777); err != nil {
			return nil, exit(2, "create apple-container cache volume %s: %v", hostPath, err)
		}
		if err := os.Chmod(hostPath, 0o777); err != nil {
			return nil, exit(2, "make apple-container cache volume writable %s: %v", hostPath, err)
		}
		mounts = append(mounts, hostPath+":"+path)
	}
	return mounts, nil
}

func appleContainerCacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", exit(2, "user cache directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "apple-container-cache"), nil
}

func appleContainerCacheVolumeName(key string) string {
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

func (b *backend) listContainers(ctx context.Context) ([]inspectContainer, error) {
	result, err := b.container(ctx, []string{"ls", "--all", "--format", "json"}, nil, nil)
	if err != nil {
		return nil, commandError("container ls", result, err)
	}
	all, err := decodeInspect([]byte(result.Stdout))
	if err != nil {
		return nil, exit(2, "parse container ls: %v", err)
	}
	out := make([]inspectContainer, 0, len(all))
	for _, c := range all {
		labels := c.labels()
		if labels["crabbox"] == "true" && labels["provider"] == providerName {
			out = append(out, c)
		}
	}
	return out, nil
}

func (b *backend) inspectContainer(ctx context.Context, id string) (inspectContainer, error) {
	result, err := b.container(ctx, []string{"inspect", id}, nil, nil)
	if err != nil {
		return inspectContainer{}, commandError("container inspect", result, err)
	}
	containers, err := decodeInspect([]byte(result.Stdout))
	if err != nil {
		return inspectContainer{}, exit(2, "parse container inspect for %s: %v", id, err)
	}
	if len(containers) == 0 {
		return inspectContainer{}, exit(4, "container not found: %s", id)
	}
	return containers[0], nil
}

func (b *backend) waitForNetworkAddress(ctx context.Context, id string, c inspectContainer, timeout time.Duration) (inspectContainer, error) {
	if c.ip() != "" {
		return c, nil
	}
	if appleContainerTerminalStatus(c.status()) {
		return inspectContainer{}, exit(5, "apple-container %s stopped before a network address was assigned", id)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return inspectContainer{}, ctx.Err()
		case <-deadline.C:
			return inspectContainer{}, exit(5, "apple-container %s has no network address yet", id)
		case <-tick.C:
			next, err := b.inspectContainer(ctx, id)
			if err != nil {
				return inspectContainer{}, err
			}
			if next.ip() != "" {
				return next, nil
			}
			if appleContainerTerminalStatus(next.status()) {
				return inspectContainer{}, exit(5, "apple-container %s stopped before a network address was assigned", id)
			}
		}
	}
}

func appleContainerTerminalStatus(status string) bool {
	switch status {
	case "stopped", "exited", "dead":
		return true
	default:
		return false
	}
}

func (b *backend) resolveContainer(ctx context.Context, identifier string) (inspectContainer, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return inspectContainer{}, "", "", exit(2, "provider=%s requires --id <lease-id-or-slug-or-container>", providerName)
	}
	var resolvedClaim core.LeaseClaim
	var wantLease, wantSlug string
	if claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return inspectContainer{}, "", "", err
	} else if ok {
		resolvedClaim = claim
		wantLease = claim.LeaseID
		wantSlug = claim.Slug
	}
	containers, err := b.listContainers(ctx)
	if err != nil {
		return inspectContainer{}, "", "", err
	}
	if boundID := strings.TrimSpace(resolvedClaim.CloudID); boundID != "" {
		for _, c := range containers {
			if c.id() == boundID {
				labels := c.labels()
				return c, firstNonBlank(resolvedClaim.LeaseID, labels["lease"]), firstNonBlank(resolvedClaim.Slug, labels["slug"]), nil
			}
		}
		return inspectContainer{}, "", "", exit(4, "apple-container lease not found: %s", identifier)
	}
	normalized := core.NormalizeLeaseSlug(identifier)
	var matched *inspectContainer
	var matchedLease, matchedSlug string
	for _, c := range containers {
		labels := c.labels()
		leaseID := labels["lease"]
		slug := labels["slug"]
		if wantLease != "" && leaseID == wantLease {
			if matched != nil {
				return inspectContainer{}, "", "", exit(2, "apple-container lease %s matches multiple containers; use an exact claimed container id", wantLease)
			}
			candidate := c
			matched, matchedLease, matchedSlug = &candidate, leaseID, slug
			continue
		}
		if wantSlug != "" && core.NormalizeLeaseSlug(slug) == core.NormalizeLeaseSlug(wantSlug) {
			if matched != nil {
				return inspectContainer{}, "", "", exit(2, "apple-container slug %s matches multiple containers; use a lease id", wantSlug)
			}
			candidate := c
			matched, matchedLease, matchedSlug = &candidate, leaseID, slug
			continue
		}
		if wantLease != "" || wantSlug != "" {
			continue
		}
		if c.id() == identifier || leaseID == identifier || (normalized != "" && core.NormalizeLeaseSlug(slug) == normalized) {
			return c, leaseID, slug, nil
		}
	}
	if matched != nil {
		return *matched, matchedLease, matchedSlug, nil
	}
	return inspectContainer{}, "", "", exit(4, "apple-container lease not found: %s", identifier)
}

func (b *backend) removeContainer(ctx context.Context, id string) error {
	// `container delete --force <id>` removes a running or stopped container.
	result, err := b.container(ctx, []string{"delete", "--force", id}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("container delete", result, err)
	}
	return nil
}

func (b *backend) prepareLease(ctx context.Context, cfg core.Config, c inspectContainer, leaseID, slug string, wait bool) (core.LeaseTarget, error) {
	server := b.serverFromContainer(c, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" {
		cfg.AppleContainer.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.AppleContainer.WorkRoot = root
		cfg.WorkRoot = root
	}
	host := c.ip()
	if host == "" {
		return core.LeaseTarget{}, exit(5, "apple-container %s has no network address yet", c.id())
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			cfg.SSHKey = keyPath
		}
	}
	target := core.SSHTargetFromConfig(cfg, host)
	target.Port = sshPort
	target.ReadyCheck = readyCheck(cfg)
	if wait {
		if err := b.waitForSSHReady(ctx, c.id(), &target, core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) waitForSSHReady(ctx context.Context, id string, target *core.SSHTarget, timeout time.Duration) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- core.WaitForSSHReady(waitCtx, target, b.rt.Stderr, "apple container ssh", timeout)
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			c, err := b.inspectContainer(ctx, id)
			if err != nil {
				continue
			}
			if appleContainerTerminalStatus(c.status()) {
				cancel()
				return b.exitedDuringBootstrapError(ctx, id, c.status())
			}
		}
	}
}

func (b *backend) exitedDuringBootstrapError(ctx context.Context, id, status string) error {
	logs := b.containerLogTail(ctx, id, 6000)
	hint := ""
	if strings.Contains(logs, "Temporary failure resolving") || strings.Contains(logs, "Failed to fetch") {
		hint = "; DNS failed during package bootstrap, retry with --apple-container-extra-run-args '--dns <resolver>' or configure appleContainer.extraRunArgs"
	}
	if strings.TrimSpace(logs) == "" {
		return exit(5, "apple-container %s stopped during SSH bootstrap status=%s%s", id, blank(status, "unknown"), hint)
	}
	return exit(5, "apple-container %s stopped during SSH bootstrap status=%s%s\ncontainer logs:\n%s", id, blank(status, "unknown"), hint, logs)
}

func (b *backend) containerLogTail(ctx context.Context, id string, limit int) string {
	result, err := b.container(ctx, []string{"logs", id}, nil, nil)
	if err != nil {
		return ""
	}
	logs := strings.TrimSpace(result.Stdout + result.Stderr)
	if limit > 0 && len(logs) > limit {
		logs = logs[len(logs)-limit:]
		if idx := strings.IndexByte(logs, '\n'); idx >= 0 {
			logs = logs[idx+1:]
		}
	}
	return logs
}

func (b *backend) appleContainerDNSArgs(ctx context.Context) []string {
	servers := b.hostDNSServers(ctx)
	args := make([]string, 0, len(servers)*2)
	for _, server := range servers {
		args = append(args, "--dns", server)
	}
	return args
}

func (b *backend) hostDNSServers(ctx context.Context) []string {
	servers := []string{}
	if result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "scutil", Args: []string{"--dns"}}); err == nil {
		servers = append(servers, parseAppleContainerDNSServers(result.Stdout+result.Stderr)...)
	}
	if len(servers) == 0 {
		if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
			servers = append(servers, parseAppleContainerDNSServers(string(data))...)
		}
	}
	return uniqueAppleContainerDNSServers(servers, 3)
}

func (b *backend) serverFromContainer(c inspectContainer, cfg core.Config) core.Server {
	labels := map[string]string{}
	for key, value := range c.labels() {
		labels[key] = value
	}
	labels["container_id"] = c.id()
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["server_type"] == "" {
		labels["server_type"] = firstNonBlank(c.image(), cfg.AppleContainer.Image)
	}
	if labels["state"] == "" {
		labels["state"] = c.status()
	}
	labels["ssh_port"] = sshPort
	status := c.status()
	if c.running() && labels["state"] == "ready" {
		status = "ready"
	}
	server := core.Server{
		CloudID:  c.id(),
		Provider: providerName,
		Name:     c.id(),
		Status:   status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = c.ip()
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.AppleContainer.Image)
	return server
}

// checkRunSurface verifies the `container run` subcommand exists and advertises
// the options the lease path depends on (`--user`, `--label`, `--dns`). It uses
// `run --help`, which has no side effects, so doctor can detect an incompatible
// CLI before any lease is attempted.
func (b *backend) checkRunSurface(ctx context.Context) error {
	result, err := b.container(ctx, []string{"run", "--help"}, nil, nil)
	if err != nil {
		return exit(2, "%s `container run` subcommand unavailable (incompatible container CLI?): %s", providerName, commandDetail(result, err))
	}
	help := result.Stdout + result.Stderr
	for _, opt := range []string{"--user", "--label", "--dns"} {
		if !strings.Contains(help, opt) {
			return exit(2, "%s `container run` is missing the required %s option; upgrade Apple's container CLI", providerName, opt)
		}
	}
	return nil
}

func requireMacOS() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return exit(2, "provider=%s requires macOS on Apple silicon; current host is %s/%s", providerName, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func shouldCleanup(server core.Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	labels := server.Labels
	if labels == nil {
		return false, "missing labels"
	}
	if strings.EqualFold(labels["keep"], "true") {
		return false, "keep=true"
	}
	if !hasClaim {
		return false, "missing claim"
	}
	leaseID := strings.TrimSpace(labels["lease"])
	containerID := strings.TrimSpace(server.CloudID)
	if claim.Provider != providerName || claim.LeaseID != leaseID || leaseID == "" || strings.TrimSpace(claim.CloudID) != containerID || containerID == "" {
		return false, "claim mismatch"
	}
	if !strings.EqualFold(server.Status, "running") && server.Status != "ready" {
		return true, "container state=" + blank(server.Status, "unknown")
	}
	lastUsed, err := time.Parse(time.RFC3339, claim.LastUsedAt)
	if err != nil || lastUsed.IsZero() {
		return false, "claim active"
	}
	idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
	if idle <= 0 {
		return false, "claim active"
	}
	if now.After(lastUsed.Add(idle).Add(12 * time.Hour)) {
		return true, "claim expired"
	}
	return false, "claim active"
}

func readyCheck(cfg core.Config) string {
	checks := []string{
		"git --version >/tmp/crabbox-ready.log 2>&1",
		"rsync --version >>/tmp/crabbox-ready.log 2>&1",
		"python3 --version >>/tmp/crabbox-ready.log 2>&1",
		"test -d " + shellQuote(cfg.AppleContainer.WorkRoot),
	}
	return strings.Join(checks, " && ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func commandError(action string, result core.LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	return exit(code, "%s failed: %s", action, commandDetail(result, err))
}

func commandDetail(result core.LocalCommandResult, err error) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return fmt.Sprintf("%v: %s", err, detail)
	}
	return fmt.Sprintf("%v", err)
}

func appleContainerHasDNSArg(args []string) bool {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "--dns" || arg == "--no-dns" {
			return true
		}
		if strings.HasPrefix(arg, "--dns=") {
			return true
		}
	}
	return false
}

func parseAppleContainerDNSServers(text string) []string {
	servers := []string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "nameserver") {
			continue
		}
		if !strings.HasPrefix(line, "nameserver[") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			servers = append(servers, fields[1])
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		servers = append(servers, strings.TrimSpace(line[idx+1:]))
	}
	return servers
}

func uniqueAppleContainerDNSServers(servers []string, limit int) []string {
	seen := map[string]bool{}
	unique := []string{}
	for _, server := range servers {
		server = strings.TrimSpace(server)
		addr, err := netip.ParseAddr(server)
		if err != nil || addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.Zone() != "" {
			continue
		}
		server = addr.String()
		if seen[server] {
			continue
		}
		seen[server] = true
		unique = append(unique, server)
		if limit > 0 && len(unique) >= limit {
			break
		}
	}
	return unique
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// bootstrapScript provisions sshd, the Crabbox SSH user and work root inside a
// Debian/Ubuntu-compatible image. It mirrors the local-container contract but
// is intentionally provider-owned so Apple-specific assumptions stay isolated.
const bootstrapScript = `
set -eu
export DEBIAN_FRONTEND=noninteractive
need_install=0
for tool in /usr/sbin/sshd git rsync curl sudo python3; do
  command -v "$tool" >/dev/null 2>&1 || need_install=1
done
if [ "$need_install" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends openssh-server ca-certificates git rsync curl sudo python3
fi
if ! command -v /usr/sbin/sshd >/dev/null 2>&1; then
  echo "missing /usr/sbin/sshd; use a Debian/Ubuntu-compatible image or a prebuilt Crabbox runner image" >&2
  exit 127
fi
user="${CRABBOX_SSH_USER:-crabbox}"
work_root="${CRABBOX_WORK_ROOT:-/work/crabbox}"
ssh_port="${CRABBOX_SSH_PORT:-22}"
if ! id "$user" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$user"
fi
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
mkdir -p /run/sshd "$work_root" "$home_dir/.ssh"
printf '%s\n' "$CRABBOX_AUTHORIZED_KEY" > "$home_dir/.ssh/authorized_keys"
chmod 700 "$home_dir/.ssh"
chmod 600 "$home_dir/.ssh/authorized_keys"
chown -R "$user" "$home_dir/.ssh" "$work_root"
env | sed -n 's/^CRABBOX_CACHE_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r cache_path; do
  [ -n "$cache_path" ] || continue
  mkdir -p "$cache_path"
  chown -R "$user" "$cache_path" 2>/dev/null || true
done
if command -v sudo >/dev/null 2>&1; then
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" > /etc/sudoers.d/crabbox
  chmod 440 /etc/sudoers.d/crabbox
fi
sed -i 's/^#\?Port .*/Port '"$ssh_port"'/' /etc/ssh/sshd_config 2>/dev/null || true
exec /usr/sbin/sshd -D -e
`
