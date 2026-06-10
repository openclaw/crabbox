package multipass

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var multipassHostOS = runtime.GOOS

type listResponse struct {
	List []multipassInstance `json:"list"`
}

type infoResponse struct {
	Errors []any                         `json:"errors"`
	Info   map[string]multipassInfoEntry `json:"info"`
}

type multipassInstance struct {
	Name    string   `json:"name"`
	State   string   `json:"state"`
	IPv4    []string `json:"ipv4"`
	Release string   `json:"release"`
}

type multipassInfoEntry struct {
	State        string   `json:"state"`
	IPv4         []string `json:"ipv4"`
	Release      string   `json:"release"`
	ImageHash    string   `json:"image_hash"`
	ImageRelease string   `json:"image_release"`
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.TargetOS == targetLinux {
		cfg.WindowsMode = ""
	}
	cfg.SSHFallbackPorts = []string{}
	if cfg.Multipass.CLIPath == "" {
		cfg.Multipass.CLIPath = "multipass"
	}
	if cfg.Multipass.Image == "" {
		cfg.Multipass.Image = "26.04"
	}
	if cfg.Multipass.User == "" {
		cfg.Multipass.User = "crabbox"
	}
	if cfg.Multipass.WorkRoot == "" {
		if !core.IsDefaultWorkRoot(cfg.WorkRoot) {
			cfg.Multipass.WorkRoot = cfg.WorkRoot
		} else {
			cfg.Multipass.WorkRoot = "/work/crabbox"
		}
	}
	if cfg.Multipass.LaunchTimeout <= 0 {
		cfg.Multipass.LaunchTimeout = 20 * time.Minute
	}
	cfg.SSHUser = cfg.Multipass.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.Multipass.WorkRoot
	cfg.ServerType = cfg.Multipass.Image
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	leaseID := newLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := make([]Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			removeStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := leaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s cpus=%d memory=%s disk=%s keep=%v\n", providerName, leaseID, slug, cfg.Multipass.Image, cfg.Multipass.CPUs, blank(cfg.Multipass.Memory, "-"), blank(cfg.Multipass.Disk, "-"), req.Keep)
	if err := b.createInstance(ctx, cfg, name, leaseID, slug, publicKey); err != nil {
		_ = b.removeInstance(context.Background(), name)
		return LeaseTarget{}, err
	}
	if req.Keep {
		cleanupKey = false
	}
	claimCreated := false
	rollbackProvisioned := func(cause error) error {
		if req.Keep {
			return cause
		}
		if err := b.removeInstance(context.Background(), name); err != nil {
			return errors.Join(cause, fmt.Errorf("multipass cleanup failed for instance %s: %w", name, err))
		}
		if claimCreated {
			removeLeaseClaim(leaseID)
		}
		return cause
	}
	info, err := b.inspectInstance(ctx, name)
	if err != nil {
		return LeaseTarget{}, rollbackProvisioned(err)
	}
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = cfg.Multipass.Image
	labels["ssh_user"] = cfg.Multipass.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.Multipass.WorkRoot
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	lease, err := b.prepareLease(ctx, cfg, info.toInstance(name), claim, true)
	if err != nil {
		return LeaseTarget{}, rollbackProvisioned(err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return LeaseTarget{}, rollbackProvisioned(err)
	}
	claimCreated = true
	if err := updateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		return LeaseTarget{}, rollbackProvisioned(err)
	}
	if err := updateLeaseClaimCacheVolumes(leaseID, cacheVolumeStickyDiskSpecs(cfg.Cache.Volumes)); err != nil {
		return LeaseTarget{}, rollbackProvisioned(err)
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	inst, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: claim.LeaseID}, nil
	}
	if claim.LeaseID == "" {
		return LeaseTarget{}, exit(4, "multipass instance %q has no Crabbox lease claim; use `crabbox stop --provider multipass %s` to delete it or warm a new lease", inst.Name, inst.Name)
	}
	lease, err := b.prepareLease(ctx, cfg, inst, claim, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := providerClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(instances))
	for _, inst := range instances {
		claim := claims[inst.Name]
		if claim.LeaseID == "" && !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		views = append(views, b.serverFromInstance(inst, claim, cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	cfg := b.configForRun()
	version, err := b.multipass(ctx, []string{"version"}, nil, nil)
	if err != nil {
		return DoctorResult{}, commandError("multipass version", version, err)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	msg := fmt.Sprintf("cli=ready daemon=ready control_plane=local inventory=ready api=list mutation=false leases=%d runtime=%s image=%s ssh_probe=%s", len(instances), firstLine(version.Stdout+version.Stderr), cfg.Multipass.Image, probe)
	return DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	lease := req.Lease
	if lease.LeaseID == "" {
		lease.LeaseID = strings.TrimSpace(lease.Server.Labels["lease"])
	}
	name := strings.TrimSpace(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]))
	if name == "" && lease.LeaseID != "" {
		inst, claim, err := b.resolveInstance(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		name = inst.Name
		if lease.LeaseID == "" {
			lease.LeaseID = claim.LeaseID
		}
	}
	if name == "" {
		return exit(2, "provider=%s release requires a Multipass instance name", providerName)
	}
	if err := b.removeInstance(ctx, name); err != nil {
		return err
	}
	if lease.LeaseID != "" {
		removeLeaseClaim(lease.LeaseID)
		removeStoredTestboxKey(lease.LeaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return err
	}
	claims, err := providerClaims()
	if err != nil {
		return err
	}
	live := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, inst := range instances {
		claim := claims[inst.Name]
		if claim.LeaseID != "" {
			live[claim.LeaseID] = struct{}{}
		}
		server := b.serverFromInstance(inst, claim, cfg)
		shouldDelete, reason := shouldCleanup(server, claim, claim.LeaseID != "", now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%s\n", inst.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
		if err := b.removeInstance(ctx, inst.Name); err != nil {
			return err
		}
		if claim.LeaseID != "" {
			removeLeaseClaim(claim.LeaseID)
			removeStoredTestboxKey(claim.LeaseID)
		}
		removed++
	}
	claimsRemoved := 0
	for _, claim := range claims {
		if claim.LeaseID == "" {
			continue
		}
		if _, ok := live[claim.LeaseID]; ok {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing instance\n", claim.LeaseID, blank(claim.Slug, "-"))
		removeLeaseClaim(claim.LeaseID)
		removeStoredTestboxKey(claim.LeaseID)
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(instances))
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = touchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	for _, key := range []string{"image", "instance", "ssh_user", "ssh_port", "work_root"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

func (b *backend) createInstance(ctx context.Context, cfg Config, name, leaseID, slug, publicKey string) error {
	mounts, err := multipassCacheVolumeMounts(cfg.Cache.Volumes)
	if err != nil {
		return err
	}
	userData := cloudInitUserData(cfg, publicKey)
	tmp, err := os.CreateTemp("", "crabbox-multipass-*.cloud-init.yaml")
	if err != nil {
		return exit(2, "create multipass cloud-init file: %v", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(userData); err != nil {
		_ = tmp.Close()
		return exit(2, "write multipass cloud-init file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return exit(2, "close multipass cloud-init file: %v", err)
	}
	args := []string{"launch", "--name", name, "--cloud-init", tmpPath}
	if cfg.Multipass.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(cfg.Multipass.CPUs))
	}
	if memory := strings.TrimSpace(cfg.Multipass.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if disk := strings.TrimSpace(cfg.Multipass.Disk); disk != "" {
		args = append(args, "--disk", disk)
	}
	if cfg.Multipass.LaunchTimeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(durationSecondsCeil(cfg.Multipass.LaunchTimeout)))
	}
	useNativeMounts := len(mounts) > 0 && b.useNativeMounts(ctx)
	if !useNativeMounts {
		for _, mount := range mounts {
			args = append(args, "--mount", mount.arg())
		}
	}
	args = append(args, cfg.Multipass.Image)
	result, err := b.multipass(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return commandError("multipass launch", result, err)
	}
	if useNativeMounts {
		if err := b.attachNativeMounts(ctx, name, mounts); err != nil {
			return err
		}
	}
	return nil
}

type multipassCacheMount struct {
	hostPath  string
	guestPath string
}

func (m multipassCacheMount) arg() string {
	return m.hostPath + ":" + m.guestPath
}

func (b *backend) useNativeMounts(ctx context.Context) bool {
	if multipassHostOS != "darwin" {
		return false
	}
	result, err := b.multipass(ctx, []string{"get", "local.driver"}, nil, io.Discard)
	if err != nil {
		return false
	}
	return strings.TrimSpace(result.Stdout) == "qemu"
}

func (b *backend) attachNativeMounts(ctx context.Context, name string, mounts []multipassCacheMount) error {
	if len(mounts) == 0 {
		return nil
	}
	result, err := b.multipass(ctx, []string{"stop", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("multipass stop", result, err)
	}
	for _, mount := range mounts {
		result, err := b.multipass(ctx, []string{"mount", "--type", "native", mount.hostPath, name + ":" + mount.guestPath}, nil, b.rt.Stderr)
		if err != nil {
			return commandError("multipass mount", result, err)
		}
	}
	result, err = b.multipass(ctx, []string{"start", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("multipass start", result, err)
	}
	return nil
}

func multipassCacheVolumeMounts(volumes []core.CacheVolumeConfig) ([]multipassCacheMount, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	root, err := multipassCacheRoot()
	if err != nil {
		return nil, err
	}
	mounts := make([]multipassCacheMount, 0, len(volumes))
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
		hostPath := filepath.Join(root, multipassCacheVolumeName(key))
		if err := os.MkdirAll(hostPath, 0o777); err != nil {
			return nil, exit(2, "create multipass cache volume %s: %v", hostPath, err)
		}
		if err := os.Chmod(hostPath, 0o777); err != nil {
			return nil, exit(2, "make multipass cache volume writable %s: %v", hostPath, err)
		}
		mounts = append(mounts, multipassCacheMount{hostPath: hostPath, guestPath: path})
	}
	return mounts, nil
}

func multipassCacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", exit(2, "user cache directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "multipass-cache"), nil
}

func multipassCacheVolumeName(key string) string {
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

func (b *backend) listInstances(ctx context.Context) ([]multipassInstance, error) {
	result, err := b.multipass(ctx, []string{"list", "--format", "json"}, nil, nil)
	if err != nil {
		return nil, commandError("multipass list", result, err)
	}
	var out listResponse
	if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
		return nil, exit(2, "parse multipass list: %v", err)
	}
	return out.List, nil
}

func (b *backend) inspectInstance(ctx context.Context, name string) (multipassInfoEntry, error) {
	result, err := b.multipass(ctx, []string{"info", "--format", "json", name}, nil, nil)
	if err != nil {
		return multipassInfoEntry{}, commandError("multipass info", result, err)
	}
	var out infoResponse
	if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
		return multipassInfoEntry{}, exit(2, "parse multipass info for %s: %v", name, err)
	}
	if len(out.Errors) > 0 {
		return multipassInfoEntry{}, exit(4, "multipass info for %s returned %d error(s)", name, len(out.Errors))
	}
	info, ok := out.Info[name]
	if !ok {
		return multipassInfoEntry{}, exit(4, "multipass instance not found: %s", name)
	}
	return info, nil
}

func (b *backend) resolveInstance(ctx context.Context, identifier string) (multipassInstance, core.LeaseClaim, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return multipassInstance{}, core.LeaseClaim{}, exit(2, "provider=%s requires --id <lease-id-or-slug-or-instance>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return multipassInstance{}, core.LeaseClaim{}, err
	} else if ok {
		name := instanceNameFromClaim(claim)
		if name == "" {
			return multipassInstance{}, core.LeaseClaim{}, exit(4, "multipass lease %s has no instance name in its claim", claim.LeaseID)
		}
		info, err := b.inspectInstance(ctx, name)
		if err != nil {
			return multipassInstance{}, core.LeaseClaim{}, err
		}
		return info.toInstance(name), claim, nil
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return multipassInstance{}, core.LeaseClaim{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return multipassInstance{}, core.LeaseClaim{}, err
	}
	normalized := normalizeLeaseSlug(identifier)
	for _, inst := range instances {
		claim := claims[inst.Name]
		if inst.Name == identifier || claim.LeaseID == identifier || (normalized != "" && normalizeLeaseSlug(claim.Slug) == normalized) {
			if info, err := b.inspectInstance(ctx, inst.Name); err == nil {
				inst = info.toInstance(inst.Name)
			}
			return inst, claim, nil
		}
	}
	return multipassInstance{}, core.LeaseClaim{}, exit(4, "multipass lease not found: %s", identifier)
}

func (b *backend) prepareLease(ctx context.Context, cfg Config, inst multipassInstance, claim core.LeaseClaim, wait bool) (LeaseTarget, error) {
	server := b.serverFromInstance(inst, claim, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" {
		cfg.Multipass.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.Multipass.WorkRoot = root
		cfg.WorkRoot = root
	}
	host := inst.ip()
	if host == "" {
		return LeaseTarget{}, exit(5, "multipass instance %s has no IPv4 address", inst.Name)
	}
	if claim.LeaseID != "" {
		keyPath, err := testboxKeyPath(claim.LeaseID)
		if err == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				cfg.SSHKey = keyPath
			}
		}
	}
	target := sshTargetFromConfig(cfg, host)
	target.Port = sshPort
	target.FallbackPorts = []string{}
	target.ReadyCheck = "/usr/local/bin/crabbox-ready"
	if wait {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "multipass ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *backend) removeInstance(ctx context.Context, name string) error {
	result, err := b.multipass(ctx, []string{"delete", "--purge", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("multipass delete", result, err)
	}
	return nil
}

func (b *backend) serverFromInstance(inst multipassInstance, claim core.LeaseClaim, cfg Config) Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	if labels["crabbox"] == "" {
		labels["crabbox"] = "true"
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["instance"] == "" {
		labels["instance"] = inst.Name
	}
	if labels["lease"] == "" {
		labels["lease"] = claim.LeaseID
	}
	if labels["slug"] == "" {
		labels["slug"] = claim.Slug
	}
	if labels["state"] == "" {
		labels["state"] = multipassState(inst.State)
	}
	if labels["server_type"] == "" {
		labels["server_type"] = firstNonBlank(inst.Release, cfg.Multipass.Image)
	}
	if labels["image"] == "" {
		labels["image"] = cfg.Multipass.Image
	}
	if labels["ssh_user"] == "" {
		labels["ssh_user"] = cfg.Multipass.User
	}
	if labels["ssh_port"] == "" {
		labels["ssh_port"] = sshPort
	}
	if labels["work_root"] == "" {
		labels["work_root"] = cfg.Multipass.WorkRoot
	}
	status := multipassState(inst.State)
	if instanceRunning(inst.State) && labels["state"] == "ready" {
		status = "ready"
	}
	server := Server{
		CloudID:  inst.Name,
		Provider: providerName,
		Name:     inst.Name,
		Status:   status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = inst.ip()
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.Multipass.Image)
	return server
}

func providerClaims() (map[string]core.LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		name := instanceNameFromClaim(claim)
		if name == "" {
			continue
		}
		out[name] = claim
	}
	return out, nil
}

func instanceScope(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return "instance:" + name
}

func instanceNameFromClaim(claim core.LeaseClaim) string {
	if name := strings.TrimSpace(claim.Labels["instance"]); name != "" {
		return name
	}
	return instanceNameFromScope(claim.ProviderScope)
}

func instanceNameFromScope(scope string) string {
	return strings.TrimPrefix(strings.TrimSpace(scope), "instance:")
}

func shouldCleanup(server Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !instanceRunning(server.Status) && server.Status != "ready" {
		return true, "instance state=" + blank(server.Status, "unknown")
	}
	if hasClaim {
		lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
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
	return false, "missing claim"
}

func (b *backend) multipass(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	cfg := b.configForRun()
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   cfg.Multipass.CLIPath,
		Args:   args,
		Stdout: stdout,
		Stderr: stderr,
	})
}

func (i multipassInstance) ip() string {
	for _, ip := range i.IPv4 {
		ip = strings.TrimSpace(ip)
		if ip != "" && ip != "--" {
			return ip
		}
	}
	return ""
}

func (i multipassInfoEntry) toInstance(name string) multipassInstance {
	return multipassInstance{
		Name:    name,
		State:   i.State,
		IPv4:    append([]string(nil), i.IPv4...),
		Release: firstNonBlank(i.Release, i.ImageRelease),
	}
}

func instanceRunning(state string) bool {
	switch multipassState(state) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func multipassState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func commandError(action string, result LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return exit(code, "%s failed: %v", action, err)
}

func durationSecondsCeil(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
