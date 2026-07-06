package namespaceinstance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "namespace-instance"

const namespaceRollbackTimeout = 30 * time.Second
const namespaceAmbiguousCreateRecoveryGrace = 5 * time.Minute

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

type instance struct {
	ClusterID string            `json:"cluster_id"`
	CreatedAt string            `json:"created_at"`
	Shape     instanceShape     `json:"shape"`
	Labels    map[string]string `json:"labels"`
}

type instanceShape struct {
	VirtualCPU      int    `json:"virtual_cpu"`
	MemoryMegabytes int    `json:"memory_megabytes"`
	MachineArch     string `json:"machine_arch"`
	OS              string `json:"os"`
}

type createOutput struct {
	ClusterID  string `json:"cluster_id"`
	InstanceID string `json:"instance_id"`
}

type ambiguousNamespaceCreateError struct {
	cause error
}

func (e *ambiguousNamespaceCreateError) Error() string { return e.cause.Error() }
func (e *ambiguousNamespaceCreateError) Unwrap() error { return e.cause }

type workspaceOutput struct {
	TenantID string `json:"tenant_id"`
}

func applyDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Network = "public"
	cfg.SSHUser = "root"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if cfg.NamespaceInstance.CLIPath == "" {
		cfg.NamespaceInstance.CLIPath = "nsc"
	}
	if cfg.NamespaceInstance.WorkRoot == "" {
		cfg.NamespaceInstance.WorkRoot = "/work/crabbox"
	}
	if cfg.NamespaceInstance.Duration > 0 {
		cfg.TTL = cfg.NamespaceInstance.Duration
	}
	cfg.WorkRoot = cfg.NamespaceInstance.WorkRoot
	cfg.ServerType = (Provider{}).ServerTypeForConfig(*cfg)
}

func machineTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", "standard":
		return "4x8"
	case "fast":
		return "8x16"
	case "large":
		return "16x32"
	case "beast":
		return "32x64"
	default:
		return strings.TrimSpace(class)
	}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *core.LeaseTarget, leaseID string) error {
	if err := core.UseLeaseKnownHosts(&target.SSH, leaseID); err != nil {
		return err
	}
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) configForRun() core.Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) scopedConfig(ctx context.Context) (core.Config, error) {
	cfg := b.configForRun()
	if strings.TrimSpace(cfg.NamespaceInstance.TenantID) != "" {
		return cfg, nil
	}
	result, err := b.nsc(ctx, cfg, []string{"workspace", "describe", "--output", "json"}, nil)
	if err != nil {
		return core.Config{}, commandError("nsc workspace describe", result, err)
	}
	var workspace workspaceOutput
	if err := json.Unmarshal([]byte(result.Stdout), &workspace); err != nil {
		return core.Config{}, core.Exit(5, "parse nsc workspace describe output: %v", err)
	}
	if strings.TrimSpace(workspace.TenantID) == "" {
		return core.Config{}, core.Exit(5, "nsc workspace describe output omitted tenant_id")
	}
	cfg.NamespaceInstance.TenantID = strings.TrimSpace(workspace.TenantID)
	return cfg, nil
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg, err := b.scopedConfig(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		if owned(item, cfg) {
			servers = append(servers, b.server(item, cfg))
		}
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, _, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()

	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, b.now())
	labels["namespace_tenant"] = cfg.NamespaceInstance.TenantID
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s machine_type=%s keep=%v\n", providerName, leaseID, slug, cfg.ServerType, req.Keep)
	id, err := b.create(ctx, cfg, keyPath+".pub", labels)
	if err != nil {
		var ambiguous *ambiguousNamespaceCreateError
		if errors.As(err, &ambiguous) {
			recoveryLabels := make(map[string]string, len(labels)+2)
			for key, value := range labels {
				recoveryLabels[key] = value
			}
			recoveryLabels["recovery"] = "ambiguous-create"
			recoveryLabels["state"] = "provisioning"
			server := core.Server{Provider: providerName, Name: slug, Labels: recoveryLabels}
			var claimErr error
			if req.Repo.Root != "" {
				claimErr = core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim)
			} else {
				claimErr = core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, core.SSHTarget{}, cfg.IdleTimeout)
			}
			if claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist Namespace ambiguous-create recovery: %w", claimErr))
			}
			cleanupKey = false
		}
		return core.LeaseTarget{}, err
	}
	persistRecovery := func(recovery string) error {
		recoveryLabels := make(map[string]string, len(labels)+3)
		for key, value := range labels {
			recoveryLabels[key] = value
		}
		recoveryLabels["namespace_instance"] = id
		recoveryLabels["recovery"] = recovery
		recoveryLabels["state"] = "provisioning"
		item := instance{ClusterID: id, Labels: recoveryLabels}
		lease, err := b.lease(item, cfg, leaseID)
		if err != nil {
			return err
		}
		if req.Repo.Root != "" {
			return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim)
		}
		return core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, lease.Server, lease.SSH, cfg.IdleTimeout)
	}
	rollback := func(cause error) error {
		if req.Keep {
			cleanupKey = false
			if claimErr := persistRecovery("kept-after-failure"); claimErr != nil {
				return errors.Join(cause, fmt.Errorf("persist kept Namespace recovery: %w", claimErr))
			}
			return cause
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), namespaceRollbackTimeout)
		defer cancel()
		if destroyErr := b.destroy(cleanupCtx, id); destroyErr != nil {
			cleanupKey = false
			claimErr := persistRecovery("rollback-cleanup")
			return errors.Join(cause, fmt.Errorf("destroy leaked Namespace instance %s: %w", id, destroyErr), claimErr)
		}
		return cause
	}
	item, err := b.findInstance(ctx, id)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	lease, err := b.lease(item, cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	if err := b.prepareSSH(ctx, cfg, &lease.SSH); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	lease.Server.Status = "ready"
	lease.Server.Labels["state"] = "ready"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s namespace_instance=%s state=ready\n", leaseID, id)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg, err := b.scopedConfig(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	item, leaseID, err := b.resolve(ctx, req.ID, cfg, req.ReleaseOnly)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err := b.lease(item, cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly || req.StatusOnly && !req.ReadyProbe {
		return lease, nil
	}
	if leaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "Namespace instance %s has no Crabbox lease id", item.ClusterID)
	}
	if req.ReadyProbe || !req.StatusOnly {
		if err := b.prepareSSH(ctx, cfg, &lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if req.Repo.Root != "" {
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, item.Labels["slug"], cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg, err := b.scopedConfig(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := namespaceClaims(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(instances))
	for _, item := range instances {
		if owned(item, cfg) {
			server := b.server(item, cfg)
			mergeClaimLabels(&server, claims[item.Labels["lease"]])
			out = append(out, server)
		}
	}
	return out, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	if result, err := b.nsc(ctx, cfg, []string{"version"}, nil); err != nil {
		return core.DoctorResult{}, commandError("nsc version", result, err)
	}
	if result, err := b.nsc(ctx, cfg, []string{"auth", "check-login"}, nil); err != nil {
		return core.DoctorResult{}, commandError("nsc auth check-login", result, err)
	}
	if _, err := b.scopedConfig(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.CLIDoctorResult(providerName, len(instances), "nsc"), nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	cfg, err := b.scopedConfig(ctx)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(req.Lease.Server.CloudID)
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if id == "" {
		item, resolvedLeaseID, err := b.resolve(ctx, leaseID, cfg, true)
		if err != nil {
			return err
		}
		id, leaseID = item.ClusterID, resolvedLeaseID
	}
	if leaseID == "" {
		return core.Exit(4, "refusing to destroy Namespace instance %s without a Crabbox lease id", id)
	}
	present, err := b.validateDestroyTarget(ctx, cfg, id, leaseID)
	if err != nil {
		return err
	}
	if present {
		if err := b.destroy(ctx, id); err != nil {
			return err
		}
	}
	if leaseID != "" {
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released lease=%s namespace_instance=%s", lease.LeaseID, lease.Server.CloudID)
}

func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	cfg, err := b.scopedConfig(ctx)
	if err != nil {
		return core.Server{}, err
	}
	now := b.now()
	if remaining := namespaceRemainingLifetime(req.Lease.Server.Labels, now); remaining > 0 {
		result, err := b.nsc(ctx, cfg, []string{"extend", req.Lease.Server.CloudID, "--ensure_minimum", remaining.String()}, b.rt.Stderr)
		if err != nil {
			return core.Server{}, commandError("nsc extend", result, err)
		}
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, now)
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID != "" {
		claim, ok, err := resolveNamespaceClaim(leaseID, cfg)
		if err != nil {
			return core.Server{}, err
		}
		idleTimeout := req.IdleTimeout
		if idleTimeout <= 0 {
			idleTimeout = cfg.IdleTimeout
		}
		if ok {
			if claim.RepoRoot != "" {
				_, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], cfg, server, req.Lease.SSH, claim.RepoRoot, idleTimeout, false, claim, true)
			} else {
				_, err = core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, server.Labels["slug"], cfg, server, req.Lease.SSH, idleTimeout, claim, true)
			}
		} else {
			err = core.ClaimLeaseTargetForConfig(leaseID, server.Labels["slug"], cfg, server, req.Lease.SSH, idleTimeout)
		}
		if err != nil {
			return core.Server{}, err
		}
	}
	return server, nil
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg, err := b.scopedConfig(ctx)
	if err != nil {
		return err
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return err
	}
	claims, err := namespaceClaims(cfg)
	if err != nil {
		return err
	}
	live := make(map[string]struct{}, len(instances))
	for _, item := range instances {
		if !owned(item, cfg) {
			continue
		}
		leaseID := item.Labels["lease"]
		live[leaseID] = struct{}{}
		claim := claims[leaseID]
		if claim.LeaseID == "" {
			refreshed, ok, err := resolveNamespaceClaim(leaseID, cfg)
			if err != nil {
				return err
			}
			if ok {
				claim = refreshed
			}
		}
		server := b.server(item, cfg)
		mergeClaimLabels(&server, claim)
		remove, reason := core.ShouldCleanupServer(server, b.now())
		if recoveryRemove, recoveryReason, handled := namespaceRecoveryCleanup(claim, b.now()); handled {
			remove, reason = recoveryRemove, recoveryReason
		}
		if !remove {
			fmt.Fprintf(b.rt.Stderr, "skip namespace_instance=%s reason=%s\n", item.ClusterID, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would destroy namespace_instance=%s lease=%s reason=%s\n", item.ClusterID, item.Labels["lease"], reason)
			continue
		}
		if claim.LeaseID != "" {
			labels := make(map[string]string, len(claim.Labels)+1)
			for key, value := range claim.Labels {
				labels[key] = value
			}
			labels["state"] = "releasing"
			claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
			if err != nil {
				return fmt.Errorf("claim Namespace instance %s for cleanup: %w", item.ClusterID, err)
			}
		}
		fmt.Fprintf(b.rt.Stdout, "destroy namespace_instance=%s lease=%s reason=%s\n", item.ClusterID, item.Labels["lease"], reason)
		if err := b.destroy(ctx, item.ClusterID); err != nil {
			return err
		}
		if claim.LeaseID != "" {
			if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return fmt.Errorf("finalize Namespace instance cleanup claim: %w", err)
			}
		}
		core.RemoveStoredTestboxKey(leaseID)
	}
	for leaseID, claim := range claims {
		if _, ok := live[leaseID]; ok || claim.LeaseID == "" {
			continue
		}
		if claim.Labels["recovery"] == "ambiguous-create" && namespaceRecoveryPending(claim, b.now()) {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=ambiguous create recovery pending\n", claim.LeaseID)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=missing Namespace instance\n", claim.LeaseID)
			continue
		}
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("remove missing Namespace instance claim: %w", err)
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
	}
	return nil
}

func (b *backend) create(ctx context.Context, cfg core.Config, publicKeyPath string, labels map[string]string) (string, error) {
	args := []string{"create", "--output", "json", "--ssh_key", publicKeyPath, "--purpose", "Crabbox remote test lease"}
	if cfg.NamespaceInstance.Bare {
		args = append(args, "--bare")
	}
	if cfg.ServerType != "" {
		args = append(args, "--machine_type", cfg.ServerType)
	}
	duration := cfg.NamespaceInstance.Duration
	if duration <= 0 {
		duration = cfg.TTL
	}
	if duration > 0 {
		args = append(args, "--duration", duration.String())
	}
	if leaseID := labels["lease"]; leaseID != "" {
		args = append(args, "--unique_tag", strings.ReplaceAll("crabbox-"+leaseID, "_", "-"))
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", namespaceLabelKey(key)+"="+labels[key])
	}
	for _, volume := range cfg.NamespaceInstance.Volumes {
		if volume = strings.TrimSpace(volume); volume != "" {
			args = append(args, "--volume", volume)
		}
	}
	result, err := b.nsc(ctx, cfg, args, b.rt.Stderr)
	if err != nil {
		if ambiguousNSCCreateError(result, err) {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if recovered, recoverErr := b.recoverByLease(recoveryCtx, cfg, labels["lease"], 30*time.Second); recoverErr == nil {
				fmt.Fprintf(b.rt.Stderr, "warning: nsc create returned an error, recovered namespace_instance=%s from lease label\n", recovered.ClusterID)
				return recovered.ClusterID, nil
			}
			return "", &ambiguousNamespaceCreateError{cause: commandError("nsc create", result, err)}
		}
		return "", commandError("nsc create", result, err)
	}
	var output createOutput
	if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if recovered, recoverErr := b.recoverByLease(recoveryCtx, cfg, labels["lease"], 30*time.Second); recoverErr == nil {
			fmt.Fprintf(b.rt.Stderr, "warning: recovered namespace_instance=%s after invalid nsc create output\n", recovered.ClusterID)
			return recovered.ClusterID, nil
		}
		return "", &ambiguousNamespaceCreateError{cause: core.Exit(5, "parse nsc create output: %v", err)}
	}
	id := strings.TrimSpace(output.InstanceID)
	if id == "" {
		id = strings.TrimSpace(output.ClusterID)
	}
	if id == "" {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if recovered, recoverErr := b.recoverByLease(recoveryCtx, cfg, labels["lease"], 30*time.Second); recoverErr == nil {
			fmt.Fprintf(b.rt.Stderr, "warning: recovered namespace_instance=%s after nsc create omitted its id\n", recovered.ClusterID)
			return recovered.ClusterID, nil
		}
		return "", &ambiguousNamespaceCreateError{cause: core.Exit(5, "nsc create output did not include instance_id")}
	}
	return id, nil
}

func ambiguousNSCCreateError(result core.LocalCommandResult, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	detail := strings.ToLower(result.Stdout + "\n" + result.Stderr + "\n" + err.Error())
	for _, marker := range []string{
		"broken pipe",
		"connection closed",
		"connection reset",
		"context deadline exceeded",
		"eof",
		"i/o timeout",
		"timed out",
		"transport is closing",
		"unavailable",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func (b *backend) listInstances(ctx context.Context) ([]instance, error) {
	cfg := b.configForRun()
	result, err := b.nsc(ctx, cfg, []string{"list", "--all", "--output", "json"}, nil)
	if err != nil {
		return nil, commandError("nsc list", result, err)
	}
	var instances []instance
	if strings.TrimSpace(result.Stdout) == "" || strings.TrimSpace(result.Stdout) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(result.Stdout), &instances); err != nil {
		return nil, core.Exit(5, "parse nsc list output: %v", err)
	}
	for i := range instances {
		instances[i].Labels = decodeNamespaceLabels(instances[i].Labels)
	}
	return instances, nil
}

func (b *backend) findInstance(ctx context.Context, id string) (instance, error) {
	item, ok, err := b.lookupInstance(ctx, id)
	if err != nil {
		return instance{}, err
	}
	if ok {
		return item, nil
	}
	return instance{}, core.Exit(4, "Namespace instance not found: %s", id)
}

func (b *backend) lookupInstance(ctx context.Context, id string) (instance, bool, error) {
	instances, err := b.listInstances(ctx)
	if err != nil {
		return instance{}, false, err
	}
	for _, item := range instances {
		if item.ClusterID == id {
			return item, true, nil
		}
	}
	return instance{}, false, nil
}

func (b *backend) findByLease(ctx context.Context, cfg core.Config, leaseID string) (instance, error) {
	instances, err := b.listInstances(ctx)
	if err != nil {
		return instance{}, err
	}
	var found []instance
	for _, item := range instances {
		if owned(item, cfg) && item.Labels["lease"] == leaseID {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		return instance{}, core.Exit(4, "expected one Namespace instance for lease %s, found %d", leaseID, len(found))
	}
	return found[0], nil
}

func (b *backend) recoverByLease(ctx context.Context, cfg core.Config, leaseID string, timeout time.Duration) (instance, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		item, err := b.findByLease(ctx, cfg, leaseID)
		if err == nil {
			return item, nil
		}
		lastErr = err
		select {
		case <-ticker.C:
		case <-deadline.C:
			return instance{}, lastErr
		case <-ctx.Done():
			return instance{}, ctx.Err()
		}
	}
}

func (b *backend) resolve(ctx context.Context, identifier string, cfg core.Config, allowMissing bool) (instance, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return instance{}, "", core.Exit(2, "provider=%s requires --id", providerName)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return instance{}, "", err
	}
	for _, item := range instances {
		if owned(item, cfg) && (item.ClusterID == identifier || item.Labels["lease"] == identifier) {
			return item, item.Labels["lease"], nil
		}
	}
	if claim, ok, err := resolveNamespaceClaim(identifier, cfg); err != nil {
		return instance{}, "", err
	} else if ok {
		id := strings.TrimSpace(claim.CloudID)
		if id == "" {
			id = strings.TrimSpace(claim.Labels["namespace_instance"])
		}
		if id == "" && claim.Labels["recovery"] == "ambiguous-create" {
			item, recoveryErr := b.findByLease(ctx, cfg, claim.LeaseID)
			if recoveryErr != nil {
				return instance{}, "", core.Exit(4, "Namespace ambiguous-create recovery is still pending for lease=%s; credentials retained", claim.LeaseID)
			}
			return item, claim.LeaseID, nil
		}
		var item instance
		found := false
		for _, candidate := range instances {
			if candidate.ClusterID == id {
				item, found = candidate, true
				break
			}
		}
		if !found {
			if allowMissing {
				return instance{ClusterID: id, Labels: claim.Labels}, claim.LeaseID, nil
			}
			return instance{}, "", core.Exit(4, "Namespace instance not found: %s", id)
		}
		if !owned(item, cfg) || item.Labels["lease"] != claim.LeaseID {
			return instance{}, "", core.Exit(4, "refusing Namespace instance %s: ownership labels do not match lease %s", id, claim.LeaseID)
		}
		return item, claim.LeaseID, nil
	}
	normalized := core.NormalizeLeaseSlug(identifier)
	var matched instance
	for _, item := range instances {
		if !owned(item, cfg) {
			continue
		}
		if core.NormalizeLeaseSlug(item.Labels["slug"]) == normalized {
			if matched.ClusterID != "" {
				return instance{}, "", core.Exit(2, "multiple Namespace instance leases match slug %s", identifier)
			}
			matched = item
		}
	}
	if matched.ClusterID != "" {
		return matched, matched.Labels["lease"], nil
	}
	return instance{}, "", core.Exit(4, "Namespace instance lease not found: %s", identifier)
}

func (b *backend) lease(item instance, cfg core.Config, leaseID string) (core.LeaseTarget, error) {
	target := core.SSHTarget{
		User:            "root",
		Host:            item.ClusterID,
		Key:             cfg.SSHKey,
		Port:            "22",
		TargetOS:        core.TargetLinux,
		ReadyCheck:      "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
		NoControlMaster: true,
		NetworkKind:     "public",
		SSHConfigProxy:  true,
		ProxyCommand:    proxyCommand(cfg, item.ClusterID),
	}
	if leaseID != "" {
		if err := core.UseLeaseKnownHosts(&target, leaseID); err != nil {
			return core.LeaseTarget{}, err
		}
		core.UseStoredTestboxKey(&target, leaseID)
	}
	server := b.server(item, cfg)
	if claim, ok, _ := resolveNamespaceClaim(leaseID, cfg); ok {
		mergeClaimLabels(&server, claim)
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) prepareSSH(ctx context.Context, cfg core.Config, target *core.SSHTarget) error {
	probe := *target
	probe.ReadyCheck = "true"
	if err := core.WaitForSSHReady(ctx, &probe, b.rt.Stderr, "namespace instance ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return err
	}
	target.Port = probe.Port
	if err := core.RunSSHQuiet(ctx, *target, namespaceToolBootstrapCommand()); err != nil {
		return core.Exit(1, "Namespace instance tool bootstrap failed: %v", err)
	}
	return core.WaitForSSHReady(ctx, target, b.rt.Stderr, "namespace instance tools", core.BootstrapWaitTimeout(cfg))
}

func namespaceToolBootstrapCommand() string {
	return strings.Join([]string{
		"set -e",
		"if command -v git >/dev/null 2>&1 && command -v rsync >/dev/null 2>&1 && command -v tar >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then exit 0; fi",
		"if command -v apt-get >/dev/null 2>&1; then",
		"  apt-get update >/tmp/crabbox-namespace-apt-update.log 2>&1",
		"  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends git rsync python3 >/tmp/crabbox-namespace-apt-install.log 2>&1",
		"elif command -v dnf >/dev/null 2>&1; then",
		"  dnf install -y git rsync python3 >/tmp/crabbox-namespace-dnf-install.log 2>&1",
		"elif command -v yum >/dev/null 2>&1; then",
		"  yum install -y git rsync python3 >/tmp/crabbox-namespace-yum-install.log 2>&1",
		"elif command -v apk >/dev/null 2>&1; then",
		"  apk add --no-cache git rsync python3 >/tmp/crabbox-namespace-apk-install.log 2>&1",
		"fi",
		"command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}, "\n")
}

func (b *backend) server(item instance, cfg core.Config) core.Server {
	labels := make(map[string]string, len(item.Labels)+4)
	for key, value := range item.Labels {
		labels[key] = value
	}
	labels["namespace_instance"] = item.ClusterID
	labels["namespace_tenant"] = cfg.NamespaceInstance.TenantID
	labels["work_root"] = cfg.WorkRoot
	if labels["state"] == "" {
		labels["state"] = "running"
	}
	labels["server_type"] = firstNonBlank(labels["server_type"], shapeName(item.Shape), cfg.ServerType)
	server := core.Server{
		CloudID:  item.ClusterID,
		Provider: providerName,
		Name:     firstNonBlank(labels["slug"], item.ClusterID),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = labels["server_type"]
	return server
}

func owned(item instance, cfg core.Config) bool {
	return item.Labels["provider"] == providerName &&
		item.Labels["crabbox"] == "true" &&
		item.Labels["created_by"] == "crabbox" &&
		item.Labels["lease"] != "" &&
		item.Labels["namespace_tenant"] != "" &&
		item.Labels["namespace_tenant"] == cfg.NamespaceInstance.TenantID
}

func namespaceLabelKey(key string) string {
	return strings.ReplaceAll(key, "_", "-")
}

func decodeNamespaceLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[strings.ReplaceAll(key, "-", "_")] = value
	}
	return out
}

func namespaceClaims(cfg core.Config) (map[string]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	return filterNamespaceClaims(claims, core.ProviderClaimScope(providerName, cfg), cfg.NamespaceInstance.TenantID), nil
}

func filterNamespaceClaims(claims []core.LeaseClaim, scope, tenantID string) map[string]core.LeaseClaim {
	out := make(map[string]core.LeaseClaim)
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == scope &&
			claim.Labels["namespace_tenant"] == tenantID && claim.LeaseID != "" {
			out[claim.LeaseID] = claim
		}
	}
	return out
}

func resolveNamespaceClaim(identifier string, cfg core.Config) (core.LeaseClaim, bool, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	scope := core.ProviderClaimScope(providerName, cfg)
	var exact core.LeaseClaim
	var slugMatch core.LeaseClaim
	normalized := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope ||
			claim.Labels["namespace_tenant"] != cfg.NamespaceInstance.TenantID {
			continue
		}
		if claim.LeaseID == identifier || claim.CloudID == identifier {
			if exact.LeaseID != "" {
				return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s scope=%s claims exactly match %s", providerName, scope, identifier)
			}
			exact = claim
			continue
		}
		if normalized != "" && core.NormalizeLeaseSlug(claim.Slug) == normalized {
			if slugMatch.LeaseID != "" {
				return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s scope=%s claims match slug %s", providerName, scope, identifier)
			}
			slugMatch = claim
		}
	}
	if exact.LeaseID != "" {
		return exact, true, nil
	}
	return slugMatch, slugMatch.LeaseID != "", nil
}

func namespaceRemainingLifetime(labels map[string]string, now time.Time) time.Duration {
	createdSeconds, createdErr := strconv.ParseInt(strings.TrimSpace(labels["created_at"]), 10, 64)
	ttlSeconds, ttlErr := strconv.ParseInt(strings.TrimSpace(labels["ttl_secs"]), 10, 64)
	if createdErr != nil || ttlErr != nil || createdSeconds <= 0 || ttlSeconds <= 0 {
		return 0
	}
	remaining := time.Unix(createdSeconds, 0).UTC().Add(time.Duration(ttlSeconds) * time.Second).Sub(now.UTC())
	if remaining <= 0 {
		return 0
	}
	return remaining.Truncate(time.Second)
}

func namespaceRecoveryPending(claim core.LeaseClaim, now time.Time) bool {
	createdSeconds, err := strconv.ParseInt(strings.TrimSpace(claim.Labels["created_at"]), 10, 64)
	if err != nil || createdSeconds <= 0 {
		return true
	}
	return now.UTC().Before(time.Unix(createdSeconds, 0).UTC().Add(namespaceAmbiguousCreateRecoveryGrace))
}

func namespaceRecoveryCleanup(claim core.LeaseClaim, now time.Time) (bool, string, bool) {
	switch claim.Labels["recovery"] {
	case "ambiguous-create":
		if namespaceRecoveryPending(claim, now) {
			return false, "ambiguous create recovery pending", true
		}
		return true, "ambiguous create recovery grace expired", true
	case "rollback-cleanup":
		return true, "failed acquisition rollback", true
	case "kept-after-failure":
		return false, "kept after failed acquisition", true
	default:
		return false, "", false
	}
}

func mergeClaimLabels(server *core.Server, claim core.LeaseClaim) {
	if claim.LeaseID == "" {
		return
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	for key, value := range claim.Labels {
		server.Labels[key] = value
	}
	server.Labels["lease"] = claim.LeaseID
	if claim.Slug != "" {
		server.Labels["slug"] = claim.Slug
	}
}

func shapeName(shape instanceShape) string {
	if shape.VirtualCPU > 0 && shape.MemoryMegabytes > 0 {
		return fmt.Sprintf("%dx%d", shape.VirtualCPU, shape.MemoryMegabytes/1024)
	}
	return ""
}

func proxyCommand(cfg core.Config, instanceID string) string {
	executable, err := os.Executable()
	if err != nil {
		executable = os.Args[0]
	}
	words := []string{executable, "__namespace-instance-proxy", "--nsc", cfg.NamespaceInstance.CLIPath}
	for _, item := range []struct {
		flag  string
		value string
	}{
		{"--endpoint", cfg.NamespaceInstance.Endpoint},
		{"--region", cfg.NamespaceInstance.Region},
		{"--keychain", cfg.NamespaceInstance.Keychain},
	} {
		if strings.TrimSpace(item.value) != "" {
			words = append(words, item.flag, item.value)
		}
	}
	words = append(words, instanceID)
	for i := range words {
		words[i] = quoteProxyWord(words[i])
	}
	return strings.Join(words, " ")
}

func quoteProxyWord(word string) string {
	word = strings.ReplaceAll(word, "%", "%%")
	if word != "" && strings.IndexFunc(word, func(r rune) bool {
		return !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("_-./:,@%+=", r))
	}) == -1 {
		return word
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(word) + `"`
}

func (b *backend) destroy(ctx context.Context, id string) error {
	cfg := b.configForRun()
	result, err := b.nsc(ctx, cfg, []string{"destroy", id, "--force"}, b.rt.Stderr)
	if err == nil {
		return nil
	}
	detail := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	if strings.Contains(detail, strings.ToLower(id)+": does not exist") {
		return nil
	}
	return commandError("nsc destroy", result, err)
}

func (b *backend) validateDestroyTarget(ctx context.Context, cfg core.Config, id, leaseID string) (bool, error) {
	instances, err := b.listInstances(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range instances {
		if item.ClusterID != id {
			continue
		}
		if !owned(item, cfg) {
			return false, core.Exit(4, "refusing to destroy Namespace instance %s without Crabbox ownership labels", id)
		}
		if leaseID != "" && item.Labels["lease"] != leaseID {
			return false, core.Exit(4, "refusing to destroy Namespace instance %s: lease label %q does not match %q", id, item.Labels["lease"], leaseID)
		}
		return true, nil
	}
	return false, nil
}

func (b *backend) nsc(ctx context.Context, cfg core.Config, args []string, stderr io.Writer) (core.LocalCommandResult, error) {
	global := make([]string, 0, 6+len(args))
	if cfg.NamespaceInstance.Endpoint != "" {
		global = append(global, "--endpoint", cfg.NamespaceInstance.Endpoint)
	}
	if cfg.NamespaceInstance.Region != "" {
		global = append(global, "--region", cfg.NamespaceInstance.Region)
	}
	if cfg.NamespaceInstance.Keychain != "" {
		global = append(global, "--keychain", cfg.NamespaceInstance.Keychain)
	}
	global = append(global, args...)
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   cfg.NamespaceInstance.CLIPath,
		Args:   global,
		Stderr: stderr,
	})
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func commandError(action string, result core.LocalCommandResult, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return core.Exit(result.ExitCode, "%s failed: %v: %s", action, err, detail)
	}
	return core.Exit(result.ExitCode, "%s failed: %v", action, err)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
