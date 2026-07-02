package tencentcloud

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Backend struct {
	shared.DirectSSHBackend
	clientFactory func(core.Config, core.Runtime) (tencentCloudAPI, error)
	waitSSH       func(context.Context, *core.SSHTarget, string, time.Duration) error
	now           func() time.Time
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg = cfgForRun(cfg)
	b := &Backend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true},
		clientFactory: func(cfg core.Config, rt core.Runtime) (tencentCloudAPI, error) {
			return newClient(cfg, rt)
		},
		waitSSH: func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
			return core.WaitForSSHReady(ctx, target, rt.Stderr, phase, timeout)
		},
		now: time.Now,
	}
	if rt.Clock != nil {
		b.now = func() time.Time { return rt.Clock.Now().UTC() }
	}
	b.Delete = b.deleteServer
	return b
}

func (b *Backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return core.DoctorResult{Provider: providerName, Message: err.Error(), Status: "failed", Checks: []core.DoctorCheck{{
			Status:  "failed",
			Check:   "auth",
			Message: err.Error(),
			Details: map[string]string{"mutation": "false"},
		}}}, nil
	}
	if _, err := client.AccountID(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	count := 0
	for _, item := range instances {
		if ownedLabels(labelsFromTags(item.Tags)) {
			count++
		}
	}
	result := core.InventoryDoctorResult(providerName, count)
	result.Message += fmt.Sprintf(" region=%s zone=%s type=%s image=%s", regionForConfig(b.Cfg), zoneForConfig(b.Cfg), serverTypeForConfig(b.Cfg), blank(imageForConfig(b.Cfg), "-"))
	return result, nil
}

func (b *Backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *Backend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	cfg := cfgForRun(b.Cfg)
	if err := validateAcquireConfig(cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(cfg, b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		if ownedLabels(labelsFromTags(item.Tags)) {
			servers = append(servers, serverFromInstance(item, cfg))
		}
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	createdID := ""
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		if createdID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			cleanupErr := client.TerminateInstance(cleanupCtx, createdID)
			cancel()
			if cleanupErr != nil {
				err = fmt.Errorf("%v; tencentcloud cleanup failed: %w", err, cleanupErr)
			}
		}
		core.RemoveStoredTestboxKey(leaseID)
	}()
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	cfg.ServerType = serverTypeForConfig(cfg)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := b.clockNow()
	createTags := leaseTags(cfg, leaseID, slug, "provisioning", req.Keep, now)
	createTags = append(createTags, tag{Key: accountLabel, Value: accountID})
	createReq := buildRunInstanceRequest(cfg, leaseID, slug, publicKey, createTags)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=tencentcloud lease=%s slug=%s type=%s region=%s zone=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, regionForConfig(cfg), zoneForConfig(cfg), imageForConfig(cfg), req.Keep)
	createdID, err = client.RunInstance(ctx, createReq)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instance, err := b.waitForInstanceIP(ctx, client, createdID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromInstance(instance, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "tencentcloud bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	readyLabels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, now)
	readyLabels["state"] = "ready"
	readyLabels[accountLabel] = accountID
	if err := client.ReplaceInstanceTags(ctx, createdID, instance.Tags, tagsFromLabels(readyLabels)); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels = readyLabels
	server.Status = "ready"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s tencentcloud_instance=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func buildRunInstanceRequest(cfg core.Config, leaseID, slug, publicKey string, tags []tag) runInstanceRequest {
	req := runInstanceRequest{
		InstanceChargeType: "POSTPAID_BY_HOUR",
		Placement:          placement{Zone: zoneForConfig(cfg)},
		ImageID:            imageForConfig(cfg),
		InstanceType:       serverTypeForConfig(cfg),
		InstanceCount:      1,
		InstanceName:       core.LeaseProviderName(leaseID, slug),
		UserData:           base64.StdEncoding.EncodeToString([]byte(core.CloudInitUserData(cfg, publicKey))),
		ClientToken:        strings.ReplaceAll(core.ProviderKeyForLease(leaseID), "_", "-"),
		TagSpecification: []tagSpecification{{
			ResourceType: "instance",
			Tags:         tags,
		}},
	}
	if cfg.TencentCloud.RootGB > 0 {
		req.SystemDisk = &systemDisk{DiskSize: cfg.TencentCloud.RootGB}
	}
	if cfg.TencentCloud.VPCID != "" || cfg.TencentCloud.SubnetID != "" {
		req.VirtualPrivateCloud = &virtualPrivateCloud{VPCID: cfg.TencentCloud.VPCID, SubnetID: cfg.TencentCloud.SubnetID}
	}
	if cfg.TencentCloud.InternetMaxBandwidthOut > 0 {
		req.InternetAccessible = &internetAccessible{
			InternetChargeType:      cfg.TencentCloud.InternetChargeType,
			InternetMaxBandwidthOut: cfg.TencentCloud.InternetMaxBandwidthOut,
			PublicIPAssigned:        true,
		}
	}
	if cfg.TencentCloud.SecurityGroupID != "" {
		req.SecurityGroupIDs = []string{cfg.TencentCloud.SecurityGroupID}
	}
	return req
}

func (b *Backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := cfgForRun(b.Cfg)
	client, err := b.clientFactory(cfg, b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	byID := map[string]instance{}
	for _, item := range instances {
		if !ownedLabels(labelsFromTags(item.Tags)) {
			continue
		}
		server := serverFromInstance(item, cfg)
		servers = append(servers, server)
		byID[server.CloudID] = item
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromInstance(byID[server.CloudID], req, accountID)
	}
	if isTencentInstanceID(req.ID) {
		item, err := client.GetInstance(ctx, req.ID)
		if err != nil {
			if req.ReleaseOnly && isNotFound(err) {
				return b.releaseTargetFromClaim(req.ID, accountID)
			}
			return core.LeaseTarget{}, err
		}
		return b.targetFromInstance(item, req, accountID)
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(req.ID, accountID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/tencentcloud instance not found: %s", req.ID)
}

func (b *Backend) releaseTargetFromClaim(id, accountID string) (core.LeaseTarget, error) {
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(id, providerName)
	if err == nil && exact && (!ok || claim.LeaseID != id) {
		return core.LeaseTarget{}, core.Exit(2, "tencentcloud exact lease identifier %q does not match a valid tencentcloud claim", id)
	}
	if err == nil && !ok && isTencentInstanceID(id) {
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/tencentcloud instance not found: %s", id)
	}
	if !core.LeaseClaimMatchesIdentifier(claim, id) {
		return core.LeaseTarget{}, core.Exit(2, "tencentcloud lease claim does not match requested identifier %q", id)
	}
	if err := validateClaimIdentity(claim, claim.LeaseID, claim.Slug); err != nil {
		return core.LeaseTarget{}, err
	}
	if expected := strings.TrimSpace(claim.Labels[accountLabel]); expected != "" && expected != accountID {
		return core.LeaseTarget{}, core.Exit(3, "tencentcloud account mismatch: current account %s does not match lease account %s", accountID, expected)
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  claim.CloudID,
		Name:     core.LeaseProviderName(claim.LeaseID, claim.Slug),
		Labels:   claim.Labels,
	}
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
}

func (b *Backend) targetFromInstance(item instance, req core.ResolveRequest, accountID string) (core.LeaseTarget, error) {
	labels := labelsFromTags(item.Tags)
	if !ownedLabels(labels) {
		return core.LeaseTarget{}, core.Exit(2, "refusing to operate on non-crabbox Tencent Cloud instance %s", item.InstanceID)
	}
	labels[accountLabel] = accountID
	server := serverFromInstance(item, b.Cfg)
	server.Labels = labels
	leaseID := labels["lease"]
	if leaseID == "" {
		return core.LeaseTarget{}, core.Exit(2, "tencentcloud instance %s is missing lease tag", item.InstanceID)
	}
	claim, claimExists, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil {
		return core.LeaseTarget{}, fmt.Errorf("read tencentcloud lease claim: %w", claimErr)
	}
	if claimExists {
		if claim.Provider != providerName {
			return core.LeaseTarget{}, core.Exit(2, "lease=%s is claimed by provider=%s; refusing tencentcloud claim rewrite", leaseID, claim.Provider)
		}
		if err := validateClaimIdentity(claim, leaseID, labels["slug"]); err != nil {
			return core.LeaseTarget{}, err
		}
		if expected := strings.TrimSpace(claim.Labels[accountLabel]); expected != "" && expected != accountID {
			return core.LeaseTarget{}, core.Exit(3, "tencentcloud account mismatch: current account %s does not match lease account %s", accountID, expected)
		}
		if claim.CloudID != "" && claim.CloudID != server.CloudID {
			return core.LeaseTarget{}, core.Exit(2, "refusing to resolve Tencent Cloud instance %s from stale local claim", server.CloudID)
		}
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	cfg := cfgForRun(b.Cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			ssh.Key = keyPath
		}
	}
	if req.Repo.Root != "" {
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, labels["slug"], cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, claim, claimExists); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *Backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return nil, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(instances))
	for _, item := range instances {
		if ownedLabels(labelsFromTags(item.Tags)) {
			out = append(out, serverFromInstance(item, b.Cfg))
		}
	}
	return out, nil
}

func (b *Backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	return b.deleteServer(ctx, b.Cfg, req.Lease.Server)
}

func (b *Backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s tencentcloud_instance=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *Backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if !ownedLabels(server.Labels) {
		return core.Server{}, core.Exit(2, "refusing to touch non-crabbox Tencent Cloud instance %s", server.DisplayID())
	}
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.GetInstance(ctx, server.CloudID)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateLiveInstance(item, server); err != nil {
		return core.Server{}, err
	}
	labels := labelsFromTags(item.Tags)
	if accountID := strings.TrimSpace(server.Labels[accountLabel]); accountID != "" {
		labels[accountLabel] = accountID
	}
	cfg := b.Cfg
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.clockNow())
	if err := client.ReplaceInstanceTags(ctx, server.CloudID, item.Tags, tagsFromLabels(labels)); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *Backend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	server := lease.Server
	if !ownedLabels(server.Labels) {
		return core.Server{}, core.Exit(2, "refusing to update tailscale metadata on non-crabbox Tencent Cloud instance %s", server.DisplayID())
	}
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.GetInstance(ctx, server.CloudID)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateLiveInstance(item, server); err != nil {
		return core.Server{}, err
	}
	labels := labelsFromTags(item.Tags)
	if accountID := strings.TrimSpace(server.Labels[accountLabel]); accountID != "" {
		labels[accountLabel] = accountID
	}
	applyTailscaleMetadata(labels, meta)
	if err := client.ReplaceInstanceTags(ctx, server.CloudID, item.Tags, tagsFromLabels(labels)); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *Backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *Backend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if !ownedLabels(server.Labels) {
		return core.Exit(2, "refusing to delete non-crabbox Tencent Cloud instance %s", server.DisplayID())
	}
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return err
	}
	if expected := strings.TrimSpace(server.Labels[accountLabel]); expected != "" && expected != accountID {
		return core.Exit(3, "tencentcloud account mismatch: current account %s does not match lease account %s", accountID, expected)
	}
	live := false
	if server.CloudID != "" {
		item, err := client.GetInstance(ctx, server.CloudID)
		if err == nil {
			if err := validateLiveInstance(item, server); err != nil {
				return err
			}
			live = true
		} else if !isNotFound(err) {
			return err
		}
	}
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return fmt.Errorf("read tencentcloud cleanup claim: %w", err)
	}
	if claimExists {
		if claim.Provider == providerName {
			if err := validateClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
				return err
			}
			if claim.CloudID != "" && claim.CloudID != server.CloudID {
				return core.Exit(2, "refusing to release Tencent Cloud instance %s from stale local claim", server.CloudID)
			}
		}
	}
	if live {
		if err := client.TerminateInstance(ctx, server.CloudID); err != nil {
			return err
		}
	}
	if claimExists && claim.Provider == providerName {
		if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return fmt.Errorf("finalize tencentcloud cleanup claim: %w", err)
		}
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *Backend) waitForInstanceIP(ctx context.Context, client tencentCloudAPI, id string) (instance, error) {
	deadline := b.clockNow().Add(5 * time.Minute)
	for {
		item, err := client.GetInstance(ctx, id)
		if err != nil {
			return instance{}, err
		}
		if publicIPv4(item) != "" {
			return item, nil
		}
		if b.clockNow().After(deadline) {
			return instance{}, core.Exit(5, "timed out waiting for Tencent Cloud instance IP")
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return instance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *Backend) clockNow() time.Time {
	if b.now != nil {
		return b.now().UTC()
	}
	return time.Now().UTC()
}

func serverFromInstance(item instance, cfg core.Config) core.Server {
	labels := labelsFromTags(item.Tags)
	server := core.Server{
		CloudID:  item.InstanceID,
		Provider: providerName,
		Name:     item.InstanceName,
		Status:   normalizeInstanceState(item.InstanceState),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = publicIPv4(item)
	server.ServerType.Name = firstNonBlank(item.InstanceType, cfg.ServerType, serverTypeForConfig(cfg))
	return server
}

func publicIPv4(item instance) string {
	for _, ip := range item.PublicIPAddresses {
		if strings.Contains(ip, ".") {
			return ip
		}
	}
	return ""
}

func normalizeInstanceState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "RUNNING":
		return "ready"
	case "":
		return "unknown"
	default:
		return strings.ToLower(state)
	}
}

func validateLiveInstance(item instance, expected core.Server) error {
	labels := labelsFromTags(item.Tags)
	if !ownedLabels(labels) {
		return core.Exit(2, "refusing to operate on non-crabbox Tencent Cloud instance %s", item.InstanceID)
	}
	expectedProviderKey := expected.Labels["provider_key"]
	if expectedProviderKey == "" && expected.Labels["lease"] != "" {
		expectedProviderKey = core.ProviderKeyForLease(expected.Labels["lease"])
	}
	if item.InstanceID != expected.CloudID ||
		item.InstanceName != expected.Name ||
		labels["lease"] != expected.Labels["lease"] ||
		labels["slug"] != expected.Labels["slug"] ||
		labels["provider_key"] != expectedProviderKey {
		return core.Exit(2, "refusing to operate on changed Tencent Cloud instance %s", expected.DisplayID())
	}
	return nil
}

func validateClaimIdentity(claim core.LeaseClaim, leaseID, slug string) error {
	if claim.LeaseID != leaseID ||
		claim.Provider != providerName ||
		claim.Slug == "" ||
		(slug != "" && claim.Slug != slug) ||
		claim.Labels["lease"] != leaseID ||
		claim.Labels["slug"] != claim.Slug ||
		claim.Labels["provider"] != providerName {
		return core.Exit(2, "tencentcloud lease claim identity does not match lease=%s slug=%s", leaseID, slug)
	}
	return nil
}

func isTencentInstanceID(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), "ins-")
}

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
