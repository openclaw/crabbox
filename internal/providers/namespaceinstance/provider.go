package namespaceinstance

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }
func (Provider) Aliases() []string {
	return []string{providerAlias}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "namespace",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterNamespaceInstanceProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyNamespaceInstanceProviderFlags(cfg, fs, values)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return namespaceInstanceServerTypeForConfig(cfg)
}

func (Provider) ServerTypeForClass(class string) string {
	return namespaceInstanceServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return newBackend(p.Spec(), cfg, rt), nil
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) *backend {
	applyNamespaceInstanceDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	count, err := client.CheckReadiness(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Status:   "ok",
		Message:  "nsc=ready auth=ready list=ready mutation=false",
		Checks: []DoctorCheck{
			{Status: "ok", Check: "nsc", Message: "nsc=ready", Details: map[string]string{"mutation": "false"}},
			{Status: "ok", Check: "auth", Message: "auth=ready", Details: map[string]string{"mutation": "false"}},
			{Status: "ok", Check: "inventory", Message: "list=ready", Details: map[string]string{"leases": count, "mutation": "false"}},
		},
	}, nil
}

func (b *backend) acquire(ctx context.Context, req AcquireRequest) (lease LeaseTarget, err error) {
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.cfg
	leaseID := newLeaseID()
	instances, err := client.ListInstances(ctx, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := serversFromInstances(instances)
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, _, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	now := b.now()
	labels := namespaceLabels(cfg, leaseID, slug, req.Keep, now)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s type=%s keep=%v\n", providerName, leaseID, slug, namespaceInstanceServerTypeForConfig(cfg), req.Keep)
	instance, err := client.CreateInstance(ctx, createInstanceRequest{
		MachineType:   namespaceInstanceServerTypeForConfig(cfg),
		Duration:      namespaceDuration(cfg),
		Ephemeral:     cfg.NamespaceInstance.Ephemeral,
		PublicKeyPath: keyPath + ".pub",
		UniqueTag:     providerKeyForLease(leaseID),
		Labels:        labels,
		Volumes:       cfg.NamespaceInstance.Volumes,
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	createdID := instance.ID
	rollback := true
	defer func() {
		if !rollback || strings.TrimSpace(createdID) == "" {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if cleanupErr := client.DestroyInstance(cleanupCtx, createdID); cleanupErr != nil && !nscNotFoundError(cleanupErr) {
			fmt.Fprintf(b.rt.Stderr, "warning: cleanup namespace-instance lease=%s after acquire failure: %v\n", leaseID, cleanupErr)
		}
	}()
	described, err := client.DescribeInstance(ctx, createdID)
	if err == nil && described.ID != "" {
		instance = mergeInstance(instance, described)
	}
	instance.Labels = mergeLabels(labels, instance.Labels)
	server := serverFromInstance(instance, cfg)
	target, err := client.ResolveSSH(instance, cfg, keyPath)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	if err := claimLeaseTargetForConfig(leaseID, slug, cfg, server, target, req.Options.IdleTimeout); err != nil {
		return LeaseTarget{}, err
	}
	rollback = false
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return b.acquire(ctx, req)
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	if claim, ok, err := resolveLeaseClaimForProvider(req.ID, providerName); err != nil {
		return LeaseTarget{}, err
	} else if ok {
		return b.resolveClaim(ctx, client, claim, req)
	}
	if claim, ok, err := resolveLeaseClaimForProviderCloudID(req.ID, providerName); err != nil {
		return LeaseTarget{}, err
	} else if ok {
		return b.resolveClaim(ctx, client, claim, req)
	}
	instance, err := client.DescribeInstance(ctx, req.ID)
	if err == nil && instance.ID != "" {
		if !isOwnedInstance(instance) {
			return LeaseTarget{}, exit(4, "lease/server not found: %s (instance is not Crabbox-managed)", req.ID)
		}
		return b.leaseFromInstance(ctx, client, instance, LeaseClaim{}, req)
	}
	instances, listErr := client.ListInstances(ctx, true)
	if listErr != nil {
		return LeaseTarget{}, listErr
	}
	var matches []namespaceInstance
	for _, instance := range instances {
		if !isOwnedInstance(instance) {
			continue
		}
		if instanceMatches(instance, req.ID) {
			matches = append(matches, instance)
		}
	}
	if len(matches) > 1 {
		return LeaseTarget{}, exit(2, "ambiguous namespace-instance lease/server alias: %s", req.ID)
	}
	if len(matches) == 1 {
		return b.leaseFromInstance(ctx, client, matches[0], LeaseClaim{}, req)
	}
	if err != nil && !nscNotFoundError(err) {
		return LeaseTarget{}, err
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *backend) resolveClaim(ctx context.Context, client *nscClient, claim LeaseClaim, req ResolveRequest) (LeaseTarget, error) {
	instance := instanceFromClaim(claim)
	if claim.CloudID != "" {
		described, err := client.DescribeInstance(ctx, claim.CloudID)
		if err == nil && described.ID != "" {
			instance = mergeInstance(instance, described)
		} else if err != nil && !nscNotFoundError(err) {
			return LeaseTarget{}, err
		}
	}
	if instance.ID == "" {
		instance.ID = claim.CloudID
	}
	if instance.ID == "" {
		return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
	}
	return b.leaseFromInstance(ctx, client, instance, claim, req)
}

func (b *backend) leaseFromInstance(ctx context.Context, client *nscClient, instance namespaceInstance, claim LeaseClaim, req ResolveRequest) (LeaseTarget, error) {
	server := serverFromInstance(instance, b.cfg)
	leaseID := firstNonEmpty(instance.Labels["lease"], claim.LeaseID)
	if leaseID == "" {
		leaseID = req.ID
	}
	target := sshTargetFromConfig(b.cfg, firstNonEmpty(instance.SSHHost, claim.SSHHost))
	target.Key = b.cfg.SSHKey
	if leaseID != "" {
		useStoredTestboxKey(&target, leaseID)
	}
	if instance.SSHUser != "" {
		target.User = instance.SSHUser
	}
	if instance.SSHPort != "" {
		target.Port = instance.SSHPort
	} else if claim.SSHPort > 0 {
		target.Port = fmt.Sprint(claim.SSHPort)
	}
	if target.User == "" {
		target.User = "root"
	}
	if target.Port == "" {
		target.Port = "22"
	}
	if !req.ReleaseOnly && !req.StatusOnly {
		resolved, err := client.ResolveSSH(instance, b.cfg, target.Key)
		if err != nil {
			return LeaseTarget{}, err
		}
		target = resolved
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	instances, err := client.ListInstances(ctx, req.All)
	if err != nil {
		return nil, err
	}
	var out []LeaseView
	for _, instance := range instances {
		if !req.All && !isOwnedInstance(instance) {
			continue
		}
		out = append(out, serverFromInstance(instance, b.cfg))
	}
	return out, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	leaseID := req.Lease.LeaseID
	if leaseID == "" {
		leaseID = req.Lease.Server.Labels["lease"]
	}
	claim, claimed, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return err
	}
	instanceID := firstNonEmpty(req.Lease.Server.CloudID, claim.CloudID)
	if instanceID == "" {
		return exit(2, "namespace-instance release requires a claimed instance id")
	}
	if !claimed && !isOwnedServer(req.Lease.Server) {
		return exit(2, "refusing to release unclaimed namespace-instance server: %s", req.Lease.Server.DisplayID())
	}
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	if err := client.DestroyInstance(ctx, instanceID); err != nil && !nscNotFoundError(err) {
		return err
	}
	if leaseID != "" {
		removeLeaseClaim(leaseID)
		removeStoredTestboxKey(leaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *backend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	labels := touchDirectLeaseLabels(server.Labels, b.cfg, req.State, b.now())
	server.Labels = labels
	if strings.TrimSpace(server.CloudID) == "" {
		return server, exit(2, "namespace-instance touch requires an instance id")
	}
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return server, err
	}
	duration := req.IdleTimeout
	if duration <= 0 {
		duration = namespaceDuration(b.cfg)
	}
	if err := client.ExtendInstance(ctx, server.CloudID, duration); err != nil {
		return server, err
	}
	return server, nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newNSCClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	instances, err := client.ListInstances(ctx, true)
	if err != nil {
		return err
	}
	now := b.now()
	for _, instance := range instances {
		if !isOwnedInstance(instance) {
			name := firstNonEmpty(instance.Name, instance.ID)
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=not crabbox namespace-instance\n", instance.ID, name)
			continue
		}
		server := serverFromInstance(instance, b.cfg)
		shouldDelete, reason := shouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stderr, "delete server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
		if !req.DryRun {
			if err := client.DestroyInstance(ctx, server.CloudID); err != nil && !nscNotFoundError(err) {
				return err
			}
			if leaseID := server.Labels["lease"]; leaseID != "" {
				removeLeaseClaim(leaseID)
				removeStoredTestboxKey(leaseID)
			}
		}
	}
	return nil
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func namespaceDuration(cfg Config) time.Duration {
	if cfg.NamespaceInstance.Duration > 0 {
		return cfg.NamespaceInstance.Duration
	}
	if cfg.TTL > 0 {
		return cfg.TTL
	}
	return time.Hour
}

func namespaceLabels(cfg Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["provider"] = providerName
	labels["state"] = "provisioning"
	labels["slug"] = slug
	labels["machine_type"] = namespaceInstanceServerTypeForConfig(cfg)
	if cfg.NamespaceInstance.Region != "" {
		labels["namespace_region"] = cfg.NamespaceInstance.Region
	}
	if cfg.NamespaceInstance.Endpoint != "" {
		labels["namespace_endpoint"] = cfg.NamespaceInstance.Endpoint
	}
	return labels
}

func serverFromInstance(instance namespaceInstance, cfg Config) Server {
	labels := mergeLabels(instance.Labels, map[string]string{})
	if labels == nil {
		labels = map[string]string{}
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["crabbox"] == "" && labels["lease"] != "" {
		labels["crabbox"] = "true"
	}
	if instance.Region != "" {
		labels["namespace_region"] = instance.Region
	}
	status := normalizeInstanceStatus(instance.Status)
	if status != "" && labels["state"] == "" {
		labels["state"] = status
	}
	server := Server{
		CloudID:  instance.ID,
		Provider: providerName,
		Name:     firstNonEmpty(instance.Name, labels["slug"], instance.ID),
		Status:   status,
		Labels:   labels,
	}
	server.ServerType.Name = firstNonEmpty(instance.MachineType, namespaceInstanceServerTypeForConfig(cfg))
	server.PublicNet.IPv4.IP = instance.SSHHost
	return server
}

func serversFromInstances(instances []namespaceInstance) []Server {
	servers := make([]Server, 0, len(instances))
	for _, instance := range instances {
		servers = append(servers, serverFromInstance(instance, Config{}))
	}
	return servers
}

func instanceFromClaim(claim LeaseClaim) namespaceInstance {
	labels := mergeLabels(claim.Labels, map[string]string{})
	return namespaceInstance{
		ID:      claim.CloudID,
		Name:    firstNonEmpty(labels["slug"], claim.Slug, claim.CloudID),
		Status:  labels["state"],
		Labels:  labels,
		SSHHost: claim.SSHHost,
		SSHPort: fmt.Sprint(claim.SSHPort),
	}
}

func mergeInstance(base, overlay namespaceInstance) namespaceInstance {
	if overlay.ID != "" {
		base.ID = overlay.ID
	}
	if overlay.Name != "" {
		base.Name = overlay.Name
	}
	if overlay.Status != "" {
		base.Status = overlay.Status
	}
	if overlay.MachineType != "" {
		base.MachineType = overlay.MachineType
	}
	if overlay.Region != "" {
		base.Region = overlay.Region
	}
	if overlay.Deadline != "" {
		base.Deadline = overlay.Deadline
	}
	if overlay.SSHHost != "" {
		base.SSHHost = overlay.SSHHost
	}
	if overlay.SSHUser != "" {
		base.SSHUser = overlay.SSHUser
	}
	if overlay.SSHPort != "" {
		base.SSHPort = overlay.SSHPort
	}
	base.Labels = mergeLabels(base.Labels, overlay.Labels)
	return base
}

func mergeLabels(primary, secondary map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range primary {
		if strings.TrimSpace(k) != "" {
			out[k] = v
		}
	}
	for k, v := range secondary {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isOwnedInstance(instance namespaceInstance) bool {
	return instance.Labels != nil && instance.Labels["crabbox"] == "true" && instance.Labels["provider"] == providerName && instance.Labels["lease"] != ""
}

func isOwnedServer(server Server) bool {
	return server.Labels != nil && server.Labels["crabbox"] == "true" && server.Labels["provider"] == providerName && server.Labels["lease"] != ""
}

func instanceMatches(instance namespaceInstance, id string) bool {
	id = strings.TrimSpace(id)
	slug := normalizeLeaseSlug(id)
	return id != "" && (instance.ID == id || instance.Labels["lease"] == id || normalizeLeaseSlug(instance.Labels["slug"]) == slug || normalizeLeaseSlug(instance.Name) == slug)
}

func normalizeInstanceStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "ready", "running", "active":
		return "ready"
	case "pending", "creating", "provisioning", "starting":
		return "provisioning"
	case "deleted", "destroyed", "stopped":
		return "released"
	case "failed", "error":
		return "failed"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}
