package vultr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	vultrAccountLabel  = "provider_account"
	vultrKeyIDLabel    = "provider_key_id"
	vultrKeyOwnedLabel = "provider_key_owned"
)

var vultrInstanceIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type backend struct {
	shared.DirectSSHBackend
	clientFactory func(core.Runtime) (vultrAPI, error)
	waitSSH       func(context.Context, *core.SSHTarget, string, time.Duration) error
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	applyVultrDefaults(&cfg)
	b := &backend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true},
	}
	b.clientFactory = func(rt core.Runtime) (vultrAPI, error) { return newVultrClient(rt) }
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.RT.Stderr, phase, timeout)
	}
	b.Delete = b.deleteServer
	return b
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *backend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	cfg := b.Cfg
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=vultr only supports target=linux")
	}
	if cfg.Tailscale.Enabled {
		return core.LeaseTarget{}, core.Exit(2, "provider=vultr does not support --tailscale yet")
	}
	if len(cfg.Vultr.SSHCIDRs) > 0 && strings.TrimSpace(cfg.Vultr.FirewallGroup) == "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=vultr requires vultr.firewallGroup when vultr.sshCIDRs is set; managed firewall creation is not implemented yet")
	}
	if err := validateVultrUserScheme(cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	instances, err := client.ListCrabboxInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		servers = append(servers, serverFromInstance(item, cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	now := b.now()
	committed := false
	created := vultrInstance{}
	defer func() {
		if err == nil || committed {
			return
		}
		var ambiguousCreate *ambiguousInstanceCreateError
		var ambiguousKey *ambiguousSSHKeyCreateError
		if errors.As(err, &ambiguousCreate) || errors.As(err, &ambiguousKey) {
			return
		}
		var keyCleanup *vultrSSHKeyCleanupError
		if errors.As(err, &keyCleanup) {
			claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, accountID, req.Keep, now, "rollback-cleanup", "", keyCleanup.keyID, true)
			if claimErr != nil {
				err = errors.Join(err, fmt.Errorf("persist vultr SSH-key rollback recovery: %w", claimErr))
			}
			return
		}
		if created.ID == "" {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, accountID, req.Keep, now, "rollback-cleanup", created.ID, created.SSHKeyID, created.SSHKeyCreated)
		if cleanupErr := rollbackVultrAcquire(client, created.ID, created.SSHKeyID, created.SSHKeyCreated); cleanupErr != nil {
			err = fmt.Errorf("%v; vultr cleanup failed: %w", err, errors.Join(claimErr, cleanupErr))
			return
		}
		if claimErr == nil {
			core.RemoveLeaseClaim(leaseID)
		}
		core.RemoveStoredTestboxKey(leaseID)
	}()
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=vultr lease=%s slug=%s type=%s region=%s keep=%v\n", leaseID, slug, cfg.ServerType, vultrRegion(cfg), req.Keep)
	created, err = client.CreateInstance(ctx, cfg, publicKey, leaseID, slug, req.Keep, now)
	if err != nil {
		var ambiguousCreate *ambiguousInstanceCreateError
		var ambiguousKey *ambiguousSSHKeyCreateError
		switch {
		case errors.As(err, &ambiguousCreate):
			if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, accountID, req.Keep, now, "ambiguous-create", "", ambiguousCreate.keyID, ambiguousCreate.keyCreated); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist vultr ambiguous-create recovery: %w", claimErr))
			}
		case errors.As(err, &ambiguousKey):
			if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, accountID, req.Keep, now, "ambiguous-key-create", "", "", false); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist vultr ambiguous-key-create recovery: %w", claimErr))
			}
		}
		return core.LeaseTarget{}, err
	}
	waited, err := b.waitForInstanceReady(ctx, client, created.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	waited.SSHKeyID = created.SSHKeyID
	waited.SSHKeyCreated = created.SSHKeyCreated
	created = waited
	server := serverFromInstance(created, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "vultr bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	readyLabels := labelsFromTags(leaseTags(cfg, leaseID, slug, "ready", req.Keep, now))
	setVultrKeyIdentity(readyLabels, created.SSHKeyID, created.SSHKeyCreated)
	readyTags := tagsFromLabels(readyLabels)
	if err := client.UpdateInstanceTags(ctx, created.ID, readyTags); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels = readyLabels
	server.Labels[vultrAccountLabel] = accountID
	server.Status = "ready"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s instance=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := client.ListCrabboxInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	byCloudID := map[string]vultrInstance{}
	for _, item := range instances {
		server := serverFromInstance(item, b.Cfg)
		servers = append(servers, server)
		byCloudID[server.CloudID] = item
	}
	if isVultrInstanceID(req.ID) {
		if item, found := byCloudID[req.ID]; found {
			return b.targetFromInstance(item, req, accountID)
		}
		item, err := client.GetInstance(ctx, req.ID)
		if err != nil {
			if req.ReleaseOnly && isVultrNotFound(err) {
				return b.releaseTargetFromClaim(req.ID, accountID)
			}
			return core.LeaseTarget{}, err
		}
		return b.targetFromInstance(item, req, accountID)
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromInstance(byCloudID[server.CloudID], req, accountID)
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(req.ID, accountID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/instance not found: %s", req.ID)
}

func (b *backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return nil, err
	}
	instances, err := client.ListCrabboxInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(instances))
	for _, item := range instances {
		out = append(out, serverFromInstance(item, b.Cfg))
	}
	return out, nil
}

func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if err := validateInstanceLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.RT)
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
	cfg := b.Cfg
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	labels := labelsFromTags(item.Tags)
	if req.IdleTimeout > 0 {
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.now())
	preserveVultrIdentity(labels, server.Labels)
	if err := client.UpdateInstanceTags(ctx, item.ID, tagsFromLabels(labels)); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	return b.deleteServer(ctx, b.Cfg, req.Lease.Server)
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s instance=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.AccountID(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	instances, err := client.ListCrabboxInstances(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult(providerName, len(instances))
	result.Message += fmt.Sprintf(" default_type=%s region=%s user_scheme=%s", b.Cfg.ServerType, vultrRegion(b.Cfg), vultrUserScheme(b.Cfg))
	return result, nil
}

func (b *backend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if err := validateInstanceLabels(server.Labels); err != nil {
		return err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return err
	}
	if expected := strings.TrimSpace(server.Labels[vultrAccountLabel]); expected != "" && expected != accountID {
		return core.Exit(3, "vultr account mismatch: current account %s does not match lease account %s", accountID, expected)
	}
	cleanupServer := copyServer(server)
	cleanupServer.Labels[vultrAccountLabel] = accountID
	if cleanupServer.CloudID != "" {
		item, err := client.GetInstance(ctx, cleanupServer.CloudID)
		if err == nil {
			if err := validateLiveInstance(item, cleanupServer); err != nil {
				return err
			}
			cleanupServer = serverFromInstance(item, b.Cfg)
			cleanupServer.Labels[vultrAccountLabel] = accountID
			preserveVultrIdentity(cleanupServer.Labels, server.Labels)
		} else if !isVultrNotFound(err) {
			return err
		}
	}
	leaseID := cleanupServer.Labels["lease"]
	claim, err := b.ensureCleanupClaim(cleanupServer)
	if err != nil {
		return err
	}
	if strings.TrimSpace(claim.CloudID) == "" && claim.Labels["recovery"] == "ambiguous-create" {
		if strings.TrimSpace(cleanupServer.CloudID) == "" {
			return core.Exit(4, "vultr ambiguous create recovery is still indeterminate for lease=%s; local claim and credentials retained", leaseID)
		}
		claim, err = core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, cleanupServer, core.SSHTarget{})
		if err != nil {
			return fmt.Errorf("persist recovered vultr instance: %w", err)
		}
	}
	keyOwned := strings.TrimSpace(claim.Labels[vultrKeyOwnedLabel])
	keyID := strings.TrimSpace(claim.Labels[vultrKeyIDLabel])
	if keyOwned == "true" && keyID == "" {
		return core.Exit(2, "vultr lease=%s owns an SSH key but its immutable key id is missing", leaseID)
	}
	if keyOwned != "true" && keyOwned != "false" {
		return core.Exit(4, "vultr SSH key ownership remains indeterminate for lease=%s; local claim and credentials retained", leaseID)
	}
	if keyOwned == "true" {
		if err := authorizeVultrSSHKeyDelete(ctx, client, leaseID, keyID); err != nil {
			return err
		}
	}
	if cleanupServer.CloudID != "" {
		if err := client.DeleteInstance(ctx, cleanupServer.CloudID); err != nil {
			return err
		}
	}
	if keyOwned == "true" {
		if err := client.DeleteSSHKey(ctx, keyID); err != nil {
			return err
		}
	}
	if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
		return fmt.Errorf("finalize vultr cleanup claim: %w", err)
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *backend) targetFromInstance(item vultrInstance, req core.ResolveRequest, accountID string) (core.LeaseTarget, error) {
	if err := validateInstanceLabels(labelsFromTags(item.Tags)); err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromInstance(item, b.Cfg)
	server.Labels[vultrAccountLabel] = accountID
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("read vultr lease claim: %w", err)
	}
	if claimExists {
		if claim.Provider != providerName {
			return core.LeaseTarget{}, core.Exit(2, "lease=%s is claimed by provider=%s; refusing vultr claim rewrite", leaseID, claim.Provider)
		}
		if err := validateVultrClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
			return core.LeaseTarget{}, err
		}
		if expected := strings.TrimSpace(claim.Labels[vultrAccountLabel]); expected == "" || expected != accountID {
			return core.LeaseTarget{}, core.Exit(3, "vultr account mismatch: current account %s does not match lease account %s", accountID, expected)
		}
		if claim.CloudID != "" && claim.CloudID != server.CloudID {
			return core.LeaseTarget{}, core.Exit(2, "refusing to resolve Vultr instance %s from stale local claim", server.CloudID)
		}
		preserveVultrIdentity(server.Labels, claim.Labels)
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	ssh := core.SSHTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
	core.UseStoredTestboxKey(&ssh, leaseID)
	if req.Repo.Root != "" {
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], b.Cfg, server, ssh, req.Repo.Root, b.Cfg.IdleTimeout, req.Reclaim, claim, claimExists); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *backend) releaseTargetFromClaim(id, accountID string) (core.LeaseTarget, error) {
	var (
		claim core.LeaseClaim
		ok    bool
		err   error
	)
	if isVultrInstanceID(id) {
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
	} else {
		var exact bool
		claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, providerName)
		if err == nil && exact && (!ok || claim.LeaseID != id) {
			return core.LeaseTarget{}, core.Exit(2, "vultr exact lease identifier %q does not match a valid vultr claim", id)
		}
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/instance not found: %s", id)
	}
	if err := validateVultrClaimIdentity(claim, claim.LeaseID, claim.Slug); err != nil {
		return core.LeaseTarget{}, err
	}
	if expected := strings.TrimSpace(claim.Labels[vultrAccountLabel]); expected == "" || expected != accountID {
		return core.LeaseTarget{}, core.Exit(3, "vultr account mismatch: current account %s does not match lease account %s", accountID, expected)
	}
	return core.LeaseTarget{
		LeaseID: claim.LeaseID,
		Server: core.Server{
			Provider: providerName,
			CloudID:  claim.CloudID,
			Name:     core.LeaseProviderName(claim.LeaseID, claim.Slug),
			Labels:   claim.Labels,
		},
	}, nil
}

func (b *backend) persistRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot, accountID string, keep bool, now time.Time, recovery, cloudID, keyID string, keyCreated bool) error {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = "provisioning"
	labels["recovery"] = recovery
	labels[vultrAccountLabel] = accountID
	if keyID != "" || recovery != "ambiguous-key-create" {
		setVultrKeyIdentity(labels, keyID, keyCreated)
	}
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve vultr recovery working directory: %w", err)
		}
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  cloudID,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false)
}

func (b *backend) ensureCleanupClaim(server core.Server) (core.LeaseClaim, error) {
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("read vultr cleanup claim: %w", err)
	}
	if !claimExists {
		repoRoot, err := os.Getwd()
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("resolve cleanup claim working directory: %w", err)
		}
		claim, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], b.Cfg, server, core.SSHTarget{}, repoRoot, b.Cfg.IdleTimeout, false, claim, false)
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("persist vultr cleanup claim: %w", err)
		}
	}
	if err := validateVultrClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
		return core.LeaseClaim{}, err
	}
	if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
		return core.LeaseClaim{}, core.Exit(2, "refusing to release Vultr instance %s from stale local claim", server.CloudID)
	}
	expectedAccount := strings.TrimSpace(claim.Labels[vultrAccountLabel])
	currentAccount := strings.TrimSpace(server.Labels[vultrAccountLabel])
	if expectedAccount == "" {
		return core.LeaseClaim{}, core.Exit(3, "vultr lease claim has no account identity; refusing cleanup")
	}
	if currentAccount != "" && expectedAccount != currentAccount {
		return core.LeaseClaim{}, core.Exit(3, "vultr account mismatch: current account %s does not match lease account %s", currentAccount, expectedAccount)
	}
	return claim, nil
}

func validateVultrClaimIdentity(claim core.LeaseClaim, leaseID, slug string) error {
	if claim.LeaseID != leaseID ||
		claim.Provider != providerName ||
		claim.Slug == "" ||
		(slug != "" && claim.Slug != slug) ||
		claim.Labels["lease"] != leaseID ||
		claim.Labels["slug"] != claim.Slug ||
		claim.Labels["provider"] != providerName {
		return core.Exit(2, "vultr lease claim identity does not match lease=%s slug=%s", leaseID, slug)
	}
	return nil
}

func (b *backend) waitForInstanceReady(ctx context.Context, client vultrAPI, id string) (vultrInstance, error) {
	deadline := b.now().Add(5 * time.Minute)
	for {
		item, err := client.GetInstance(ctx, id)
		if err != nil {
			return vultrInstance{}, err
		}
		if instanceReady(item) {
			return item, nil
		}
		if b.now().After(deadline) {
			return vultrInstance{}, core.Exit(5, "timed out waiting for Vultr instance IP")
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return vultrInstance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func instanceReady(item vultrInstance) bool {
	if strings.TrimSpace(item.MainIP) == "" || item.MainIP == "0.0.0.0" {
		return false
	}
	return strings.EqualFold(item.Status, "active") &&
		strings.EqualFold(item.PowerStatus, "running") &&
		(item.ServerStatus == "" || strings.EqualFold(item.ServerStatus, "ok") || strings.EqualFold(item.ServerStatus, "installingboot") || strings.EqualFold(item.ServerStatus, "none"))
}

func rollbackVultrAcquire(client vultrAPI, instanceID, keyID string, keyCreated bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	if instanceID != "" {
		if err := client.DeleteInstance(ctx, instanceID); err != nil {
			errs = append(errs, err)
		}
	}
	if keyCreated && keyID != "" {
		if err := client.DeleteSSHKey(ctx, keyID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateLiveInstance(item vultrInstance, expected core.Server) error {
	labels := normalizedInstanceLabels(item.Tags)
	if err := validateInstanceLabels(labels); err != nil {
		return err
	}
	expectedProviderKey := expected.Labels["provider_key"]
	if expectedProviderKey == "" && expected.Labels["lease"] != "" {
		expectedProviderKey = providerKeyForLease(expected.Labels["lease"])
	}
	if item.ID != expected.CloudID ||
		item.Label != expected.Name ||
		labels["lease"] != expected.Labels["lease"] ||
		labels["slug"] != expected.Labels["slug"] ||
		labels["provider_key"] != expectedProviderKey {
		return core.Exit(2, "refusing to operate on changed Vultr instance %s", expected.CloudID)
	}
	return nil
}

func serverFromInstance(item vultrInstance, cfg core.Config) core.Server {
	labels := normalizedInstanceLabels(item.Tags)
	server := core.Server{
		CloudID:  item.ID,
		Provider: providerName,
		Name:     item.Label,
		Status:   normalizeInstanceStatus(item),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = item.MainIP
	server.ServerType.Name = firstNonBlank(item.Plan, cfg.ServerType)
	return server
}

func normalizedInstanceLabels(tags []string) map[string]string {
	labels := labelsFromTags(tags)
	if labels["provider_key"] == "" && labels["lease"] != "" {
		labels["provider_key"] = providerKeyForLease(labels["lease"])
	}
	return labels
}

func normalizeInstanceStatus(item vultrInstance) string {
	if instanceReady(item) {
		return "ready"
	}
	if item.Status != "" {
		return item.Status
	}
	return "unknown"
}

func setVultrKeyIdentity(labels map[string]string, keyID string, created bool) {
	labels[vultrKeyOwnedLabel] = fmt.Sprintf("%t", created)
	if keyID != "" {
		labels[vultrKeyIDLabel] = keyID
	}
}

func preserveVultrIdentity(labels, stored map[string]string) {
	for _, key := range []string{vultrAccountLabel, vultrKeyIDLabel, vultrKeyOwnedLabel} {
		if value := strings.TrimSpace(stored[key]); value != "" {
			labels[key] = value
		}
	}
}

func authorizeVultrSSHKeyDelete(ctx context.Context, client vultrAPI, leaseID, keyID string) error {
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	publicKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return core.Exit(4, "vultr SSH key deletion for lease=%s requires retained local public key; local claim and credentials retained", leaseID)
		}
		return fmt.Errorf("read retained vultr SSH public key: %w", err)
	}
	publicKey := strings.TrimSpace(string(publicKeyBytes))
	if publicKey == "" {
		return core.Exit(2, "retained vultr SSH public key is empty for lease=%s", leaseID)
	}
	key, found, err := client.FindSSHKey(ctx, providerKeyForLease(leaseID), publicKey)
	if err != nil {
		return err
	}
	if !found {
		return core.Exit(4, "vultr SSH key for lease=%s could not be verified; local claim and credentials retained", leaseID)
	}
	if key.ID != keyID {
		return core.Exit(2, "refusing to delete Vultr SSH key %s for lease=%s; verified key id is %s", keyID, leaseID, key.ID)
	}
	return nil
}

func validateVultrUserScheme(cfg core.Config) error {
	switch strings.ToLower(strings.TrimSpace(vultrUserScheme(cfg))) {
	case "root", "limited":
		return nil
	default:
		return core.Exit(2, "provider=vultr unsupported user_scheme=%q", cfg.Vultr.UserScheme)
	}
}

func applyVultrDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.Vultr.Region == "" {
		cfg.Vultr.Region = "ewr"
	}
	if cfg.Vultr.UserScheme == "" {
		cfg.Vultr.UserScheme = "root"
	}
	if !core.IsSSHUserExplicit(cfg) && strings.EqualFold(cfg.Vultr.UserScheme, "limited") {
		cfg.SSHUser = "limited"
	} else if cfg.SSHUser == "" {
		cfg.SSHUser = "root"
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = "/work/crabbox"
	}
	if cfg.ServerType == "" {
		cfg.ServerType = vultrServerTypeForClass(cfg.Class)
	}
}

func (b *backend) now() time.Time {
	if b.RT.Clock != nil {
		return b.RT.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func isVultrInstanceID(value string) bool {
	value = strings.TrimSpace(value)
	return vultrInstanceIDRe.MatchString(value)
}

func copyServer(server core.Server) core.Server {
	server.Labels = copyLabels(server.Labels)
	return server
}

func copyLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
