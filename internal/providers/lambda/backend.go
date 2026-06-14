package lambda

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type lambdaAPI interface {
	ListInstances(context.Context) ([]Instance, error)
	GetInstance(context.Context, string) (Instance, error)
	LaunchInstance(context.Context, LaunchInstanceRequest) (LaunchInstanceResponse, error)
	TerminateInstances(context.Context, []string) error
	ListSSHKeys(context.Context) ([]SSHKey, error)
	AddSSHKey(context.Context, AddSSHKeyRequest) (SSHKey, error)
	DeleteSSHKey(context.Context, string) error
	ListRegions(context.Context) ([]Region, error)
	ListInstanceTypes(context.Context) ([]InstanceType, error)
	ListImages(context.Context) ([]Image, error)
	ListFilesystems(context.Context) ([]Filesystem, error)
	ListFirewallRulesets(context.Context) ([]FirewallRuleset, error)
}

type ambiguousLambdaCreateError struct {
	err error
}

func (e *ambiguousLambdaCreateError) Error() string {
	return fmt.Sprintf("lambda instance creation remains indeterminate; preserving SSH credentials for recovery: %v", e.err)
}

func (e *ambiguousLambdaCreateError) Unwrap() error { return e.err }

type lambdaSSHKeyIdentity struct {
	ID      string
	Name    string
	Created bool
}

func (b *backend) initDirect() {
	b.cfg.Provider = providerName
	if b.cfg.TargetOS == "" {
		b.cfg.TargetOS = core.TargetLinux
	}
	if b.cfg.SSHUser == "" {
		b.cfg.SSHUser = defaultUser
	}
	if b.cfg.SSHPort == "" {
		b.cfg.SSHPort = defaultPort
	}
	if b.cfg.ServerType == "" {
		b.cfg.ServerType = typeForConfig(b.cfg)
	}
	if b.cfg.Lambda.Region == "" {
		b.cfg.Lambda.Region = defaultRegion
	}
	if b.cfg.Lambda.Type == "" {
		b.cfg.Lambda.Type = defaultType
	}
	if b.cfg.Lambda.Image == "" && b.cfg.Lambda.ImageFamily == "" {
		b.cfg.Lambda.ImageFamily = defaultImageFamily
	}
	b.DirectSSHBackend = shared.DirectSSHBackend{
		SpecValue:       b.spec,
		Cfg:             b.cfg,
		RT:              b.rt,
		Delete:          b.deleteServer,
		StoredLeaseKeys: true,
	}
	if b.waitSSH == nil {
		b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
			return core.WaitForSSHReady(ctx, target, b.rt.Stderr, phase, timeout)
		}
	}
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	b.initDirect()
	return shared.AcquireAttemptsRetry(b.rt, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *backend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	if err := validateConfig(b.cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	if b.cfg.TargetOS != "" && b.cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=lambda only supports target=linux")
	}
	if b.cfg.Tailscale.Enabled && b.cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", b.cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := ownedServers(instances, b.cfg)
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(b.cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.cfg
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	if cfg.SSHUser == "" {
		cfg.SSHUser = defaultUser
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = defaultPort
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	cfg.ServerType = typeForConfig(cfg)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := b.now()
	var (
		key        lambdaSSHKeyIdentity
		instanceID string
		committed  bool
	)
	defer func() {
		if err == nil || committed {
			return
		}
		var ambiguous *ambiguousLambdaCreateError
		if errors.As(err, &ambiguous) {
			return
		}
		if instanceID == "" && !key.Created {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		_ = b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, key, instanceID, "rollback-cleanup", req.Keep, now)
		cleanupErr := rollbackLambdaAcquire(client, instanceID, key)
		if cleanupErr != nil {
			err = fmt.Errorf("%v; lambda cleanup failed: %w", err, cleanupErr)
			return
		}
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}()
	key, err = b.ensureSSHKey(ctx, client, cfg.ProviderKey, publicKey)
	if err != nil {
		if !key.Created {
			return core.LeaseTarget{}, err
		}
		if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, key, "", "ambiguous-key-create", req.Keep, now); claimErr != nil {
			return core.LeaseTarget{}, errors.Join(&ambiguousLambdaCreateError{err: err}, fmt.Errorf("persist lambda SSH-key recovery: %w", claimErr))
		}
		return core.LeaseTarget{}, &ambiguousLambdaCreateError{err: err}
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=lambda lease=%s slug=%s type=%s region=%s image=%s image_family=%s keep=%v\n", leaseID, slug, cfg.ServerType, regionForConfig(cfg), imageForConfig(cfg), imageFamilyForConfig(cfg), req.Keep)
	launchReq := b.launchRequest(cfg, leaseID, slug, publicKey, key, req.Keep, now)
	launch, err := client.LaunchInstance(ctx, launchReq)
	if err != nil {
		if !isAmbiguousLambdaMutationError(err) {
			return core.LeaseTarget{}, err
		}
		if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, key, "", "ambiguous-create", req.Keep, now); claimErr != nil {
			return core.LeaseTarget{}, errors.Join(&ambiguousLambdaCreateError{err: err}, fmt.Errorf("persist lambda launch recovery: %w", claimErr))
		}
		return core.LeaseTarget{}, &ambiguousLambdaCreateError{err: err}
	}
	if len(launch.InstanceIDs) != 1 || strings.TrimSpace(launch.InstanceIDs[0]) == "" {
		err = core.Exit(5, "lambda launch returned %d instance ids; want exactly one", len(launch.InstanceIDs))
		return core.LeaseTarget{}, err
	}
	instanceID = strings.TrimSpace(launch.InstanceIDs[0])
	instance, err := b.waitForInstanceReady(ctx, client, instanceID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromInstance(instance, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "lambda bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels = lambdaLabelsWithKey(leaseTags(cfg, leaseID, slug, "ready", req.Keep, now), key)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s lambda=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *backend) launchRequest(cfg core.Config, leaseID, slug, publicKey string, key lambdaSSHKeyIdentity, keep bool, now time.Time) LaunchInstanceRequest {
	tags := lambdaProviderLaunchTags(leaseTags(cfg, leaseID, slug, "provisioning", keep, now), key)
	req := LaunchInstanceRequest{
		RegionName:       regionForConfig(cfg),
		InstanceTypeName: typeForConfig(cfg),
		Quantity:         1,
		SSHKeyNames:      []string{key.Name},
		Name:             lambdaInstanceName(leaseID, slug),
		UserData:         lambdaUserData(cfg, publicKey),
		Tags:             tags,
	}
	if image := imageForConfig(cfg); image != "" {
		req.ImageID = image
	} else {
		req.ImageFamily = imageFamilyForConfig(cfg)
	}
	if ruleset := strings.TrimSpace(cfg.Lambda.FirewallRuleset); ruleset != "" {
		req.FirewallRulesetName = ruleset
	}
	req.FileSystemNames = append([]string(nil), cfg.Lambda.FilesystemNames...)
	for _, mount := range cfg.Lambda.FilesystemMounts {
		if strings.TrimSpace(mount.Name) == "" {
			continue
		}
		req.FileSystemMounts = append(req.FileSystemMounts, FilesystemMountRequest{Name: strings.TrimSpace(mount.Name), MountPath: strings.TrimSpace(mount.MountPath)})
	}
	return req
}

func lambdaLabelsWithKey(labels map[string]string, key lambdaSSHKeyIdentity) map[string]string {
	labels[lambdaKeyIDLabel] = key.ID
	labels[lambdaKeyNameLabel] = key.Name
	labels[lambdaKeyOwnedLabel] = fmt.Sprint(key.Created)
	return labels
}

func lambdaProviderLaunchTags(labels map[string]string, key lambdaSSHKeyIdentity) map[string]string {
	labels = lambdaLabelsWithKey(labels, key)
	// Lambda has no safe tag-update method in the Plan 01 foundation. Provider
	// tags keep the launch-time expiry so provider-only orphan cleanup can
	// eventually reclaim billable instances; local claims carry fresh touch data.
	for _, field := range []string{"last_touched_at", "idle_timeout", "idle_timeout_secs"} {
		delete(labels, field)
	}
	return labels
}

func (b *backend) ensureSSHKey(ctx context.Context, client lambdaAPI, name, publicKey string) (lambdaSSHKeyIdentity, error) {
	keys, err := client.ListSSHKeys(ctx)
	if err != nil {
		return lambdaSSHKeyIdentity{}, err
	}
	for _, key := range keys {
		if key.Name != name {
			continue
		}
		if strings.TrimSpace(key.PublicKey) != strings.TrimSpace(publicKey) {
			return lambdaSSHKeyIdentity{}, core.Exit(2, "lambda SSH key %q already exists with different public key", name)
		}
		return lambdaSSHKeyIdentity{ID: key.ID, Name: key.Name, Created: false}, nil
	}
	key, err := client.AddSSHKey(ctx, AddSSHKeyRequest{Name: name, PublicKey: publicKey})
	if err != nil {
		return lambdaSSHKeyIdentity{Name: name, Created: isAmbiguousLambdaMutationError(err)}, err
	}
	return lambdaSSHKeyIdentity{ID: key.ID, Name: firstNonBlank(key.Name, name), Created: true}, nil
}

func (b *backend) waitForInstanceReady(ctx context.Context, client lambdaAPI, id string) (Instance, error) {
	deadline := b.now().Add(5 * time.Minute)
	for {
		item, err := client.GetInstance(ctx, id)
		if err != nil {
			return Instance{}, err
		}
		if strings.EqualFold(item.Status, "active") && strings.TrimSpace(item.IP) != "" {
			return item, nil
		}
		if isTerminalInstanceStatus(item.Status) {
			return Instance{}, core.Exit(5, "lambda instance %s reached terminal status %s", id, item.Status)
		}
		if b.now().After(deadline) {
			return Instance{}, core.Exit(5, "timed out waiting for Lambda instance %s to become active with public IP", id)
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Instance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	b.initDirect()
	client, err := b.clientFactory(b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := ownedServers(instances, b.cfg)
	byID := map[string]Instance{}
	for _, item := range instances {
		if isOwnedInstance(item) {
			byID[item.ID] = item
		}
	}
	if claim, ok, claimErr := core.ResolveLeaseClaimForProvider(req.ID, providerName); claimErr != nil {
		return core.LeaseTarget{}, claimErr
	} else if ok && claim.CloudID != "" {
		if item, found := byID[claim.CloudID]; found {
			return b.targetFromInstance(item, req)
		}
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err == nil && leaseID != "" {
		return b.targetFromInstance(byID[server.CloudID], req)
	}
	if item, ok := byID[req.ID]; ok {
		return b.targetFromInstance(item, req)
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(req.ID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/lambda instance not found: %s", req.ID)
}

func (b *backend) releaseTargetFromClaim(id string) (core.LeaseTarget, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(id, providerName)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok {
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/lambda instance not found: %s", id)
	}
	if err := validateLambdaLabels(claim.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	if claim.CloudID == "" && claim.Labels[lambdaRecoveryKeyLabel] != "rollback-cleanup" {
		return core.LeaseTarget{}, core.Exit(4, "lambda recovery is still pending for lease=%s; credentials and recovery claim retained", claim.LeaseID)
	}
	server := core.Server{Provider: providerName, CloudID: claim.CloudID, Name: claim.Slug, Labels: claim.Labels}
	server.PublicNet.IPv4.IP = claim.SSHHost
	server.ServerType.Name = claim.Labels["server_type"]
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
}

func (b *backend) targetFromInstance(item Instance, req core.ResolveRequest) (core.LeaseTarget, error) {
	if err := validateLambdaLabels(item.Tags); err != nil {
		return core.LeaseTarget{}, err
	}
	server, err := mergeLocalClaimLabels(serverFromInstance(item, b.cfg))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := server.Labels["lease"]
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	ssh := core.SSHTargetFromConfig(b.cfg, server.PublicNet.IPv4.IP)
	core.UseStoredTestboxKey(&ssh, leaseID)
	if req.Repo.Root != "" {
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, server.Labels["slug"], b.cfg, server, ssh, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	b.initDirect()
	servers, err := b.listOwnedServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(servers))
	for _, server := range servers {
		out = append(out, server)
	}
	return out, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	b.initDirect()
	return b.deleteServer(ctx, b.cfg, req.Lease.Server)
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s lambda=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	b.initDirect()
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	_ = ctx
	b.initDirect()
	server := req.Lease.Server
	if err := validateLambdaLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	cfg := b.cfg
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	labels := core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, b.now())
	labels[lambdaTouchLocalLabel] = "true"
	server.Labels = labels
	if claim, ok, err := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID); err == nil && ok {
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(req.Lease.LeaseID, claim, labels); err != nil {
			return core.Server{}, err
		}
	}
	return server, nil
}

func (b *backend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	_ = ctx
	b.initDirect()
	server := lease.Server
	if err := validateLambdaLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	labels := make(map[string]string, len(server.Labels)+8)
	for key, value := range server.Labels {
		labels[key] = value
	}
	applyTailscaleMetadata(labels, meta)
	labels[lambdaTouchLocalLabel] = "true"
	server.Labels = labels
	if claim, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err == nil && ok {
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, claim, labels); err != nil {
			return core.Server{}, err
		}
	}
	return server, nil
}

func (b *backend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if err := validateLambdaLabels(server.Labels); err != nil {
		return err
	}
	client, err := b.clientFactory(b.rt)
	if err != nil {
		return err
	}
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
	if err != nil {
		return fmt.Errorf("read lambda cleanup claim: %w", err)
	}
	if claimExists {
		if claim.Provider != providerName {
			return core.Exit(2, "lease=%s is claimed by provider=%s; refusing lambda cleanup", claim.LeaseID, claim.Provider)
		}
		if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
			return core.Exit(2, "refusing to release Lambda instance %s from stale local claim", server.CloudID)
		}
	}
	instanceID := firstNonBlank(server.CloudID, claim.CloudID)
	if server.CloudID == "" {
		server.CloudID = instanceID
	}
	liveFound := false
	if instanceID != "" {
		live, getErr := client.GetInstance(ctx, instanceID)
		if getErr == nil {
			liveFound = true
			if err := validateLiveInstance(live, server); err != nil {
				return err
			}
			server = serverFromInstance(live, b.cfg)
		} else if !isLambdaNotFound(getErr) {
			return getErr
		}
	}
	if liveFound {
		if err := client.TerminateInstances(ctx, []string{instanceID}); err != nil {
			return err
		}
	}
	keyID := firstNonBlank(server.Labels[lambdaKeyIDLabel], claim.Labels[lambdaKeyIDLabel])
	keyOwned := firstNonBlank(server.Labels[lambdaKeyOwnedLabel], claim.Labels[lambdaKeyOwnedLabel]) == "true"
	if keyOwned && keyID != "" {
		if err := client.DeleteSSHKey(ctx, keyID); err != nil && !isLambdaNotFound(err) {
			return err
		}
	}
	if claimExists {
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("finalize lambda cleanup claim: %w", err)
		}
	}
	core.RemoveStoredTestboxKey(server.Labels["lease"])
	return nil
}

func (b *backend) persistRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot string, key lambdaSSHKeyIdentity, cloudID, recovery string, keep bool, now time.Time) error {
	labels := leaseTags(cfg, leaseID, slug, "provisioning", keep, now)
	labels[lambdaRecoveryKeyLabel] = recovery
	labels[lambdaKeyIDLabel] = key.ID
	labels[lambdaKeyNameLabel] = firstNonBlank(key.Name, cfg.ProviderKey)
	labels[lambdaKeyOwnedLabel] = fmt.Sprint(key.Created)
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve lambda recovery working directory: %w", err)
		}
	}
	server := core.Server{Provider: providerName, Name: lambdaInstanceName(leaseID, slug), CloudID: cloudID, Labels: labels}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false)
}

func ownedServers(instances []Instance, cfg core.Config) []core.Server {
	out := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		if isOwnedInstance(item) {
			out = append(out, serverFromInstance(item, cfg))
		}
	}
	return out
}

func (b *backend) listOwnedServers(ctx context.Context) ([]core.Server, error) {
	client, err := b.clientFactory(b.rt)
	if err != nil {
		return nil, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		if !isOwnedInstance(item) {
			continue
		}
		server := serverFromInstance(item, b.cfg)
		server, err = mergeLocalClaimLabels(server)
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func mergeLocalClaimLabels(server core.Server) (core.Server, error) {
	leaseID := server.Labels["lease"]
	if leaseID == "" {
		return server, nil
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.Server{}, fmt.Errorf("read lambda lease claim: %w", err)
	}
	if !ok || claim.Provider != providerName {
		return server, nil
	}
	if claim.CloudID != "" && claim.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to list Lambda instance %s from stale local claim", server.CloudID)
	}
	if err := validateLambdaLabels(claim.Labels); err != nil {
		return core.Server{}, err
	}
	labels := make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		labels[key] = value
	}
	server.Labels = labels
	return server, nil
}

func serverFromInstance(item Instance, cfg core.Config) core.Server {
	labels := normalizeLambdaLabels(item.Tags)
	server := core.Server{
		CloudID:  item.ID,
		Provider: providerName,
		Name:     firstNonBlank(item.Name, item.Hostname, item.ID),
		Status:   normalizeInstanceStatus(item.Status),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = strings.TrimSpace(item.IP)
	server.ServerType.Name = firstNonBlank(item.Type, cfg.ServerType, typeForConfig(cfg))
	return server
}

func validateLiveInstance(item Instance, expected core.Server) error {
	labels := normalizeLambdaLabels(item.Tags)
	if err := validateLambdaLabels(labels); err != nil {
		return err
	}
	expectedProviderKey := expected.Labels["provider_key"]
	if expectedProviderKey == "" && expected.Labels["lease"] != "" {
		expectedProviderKey = providerKeyForLease(expected.Labels["lease"])
	}
	if item.ID != expected.CloudID ||
		labels["lease"] != expected.Labels["lease"] ||
		labels["slug"] != expected.Labels["slug"] ||
		labels["provider_key"] != expectedProviderKey {
		return core.Exit(2, "refusing to operate on changed Lambda instance %s", expected.CloudID)
	}
	return nil
}

func rollbackLambdaAcquire(client lambdaAPI, instanceID string, key lambdaSSHKeyIdentity) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	if instanceID != "" {
		if err := client.TerminateInstances(ctx, []string{instanceID}); err != nil {
			errs = append(errs, err)
		}
	}
	if key.Created && key.ID != "" {
		if err := client.DeleteSSHKey(ctx, key.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func normalizeInstanceStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active":
		return "ready"
	case "booting":
		return "starting"
	case "terminating":
		return "stopping"
	case "terminated":
		return "deleted"
	case "preempted":
		return "preempted"
	case "unhealthy":
		return "unhealthy"
	default:
		if strings.TrimSpace(status) == "" {
			return "unknown"
		}
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func isTerminalInstanceStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "terminated", "terminating", "preempted", "unhealthy":
		return true
	default:
		return false
	}
}

func isLambdaNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404
}

func isAmbiguousLambdaMutationError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.Status >= 500 || apiErr.Status == 408 || apiErr.Status == 429
}

func lambdaInstanceName(leaseID, slug string) string {
	name := strings.ToLower(core.LeaseProviderName(leaseID, slug))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" || out[0] == '-' {
		return "crabbox-" + strings.TrimPrefix(strings.ReplaceAll(leaseID, "_", "-"), "-")
	}
	return out
}

func providerKeyForLease(leaseID string) string {
	key := core.ProviderKeyForLease(leaseID)
	if len(key) > 64 {
		key = key[:64]
	}
	return strings.TrimRight(key, "-")
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}
