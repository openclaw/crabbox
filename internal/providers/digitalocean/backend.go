package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type digitalOceanAPI interface {
	ListCrabboxDroplets(context.Context) ([]droplet, error)
	GetDroplet(context.Context, int64) (droplet, error)
	CreateDroplet(context.Context, core.Config, string, string, string, bool, time.Time) (droplet, error)
	DeleteDroplet(context.Context, int64) error
	DeleteSSHKeyByName(context.Context, string) error
	ReplaceDropletTags(context.Context, int64, []string, []string) error
}

type digitalOceanLeaseBackend struct {
	shared.DirectSSHBackend
	clientFactory             func(core.Runtime) (digitalOceanAPI, error)
	waitSSH                   func(context.Context, *core.SSHTarget, string, time.Duration) error
	recoveryGrace             time.Duration
	recoveryReconcilePolls    int
	recoveryReconcileInterval time.Duration
	acquireConfigErr          error
}

var claimLeaseTargetForRepoConfig = core.ClaimLeaseTargetForRepoConfig

const (
	ambiguousCreateRecoveryGrace    = 2 * time.Minute
	ambiguousCreateRecoveryPolls    = 3
	ambiguousCreateRecoveryInterval = 2 * time.Second
)

func NewDigitalOceanLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	acquireConfigErr := validateDigitalOceanAcquireConfig(cfg, core.OSImageWasExplicit(cfg))
	applyDigitalOceanDefaults(&cfg)
	b := &digitalOceanLeaseBackend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt},
		acquireConfigErr: acquireConfigErr,
	}
	b.clientFactory = func(rt core.Runtime) (digitalOceanAPI, error) { return newDigitalOceanClient(rt) }
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.RT.Stderr, phase, timeout)
	}
	b.Delete = b.deleteServer
	return b
}

func (b *digitalOceanLeaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *digitalOceanLeaseBackend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	if b.acquireConfigErr != nil {
		return core.LeaseTarget{}, b.acquireConfigErr
	}
	cfg := b.Cfg
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=digitalocean only supports target=linux")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(droplets))
	for _, item := range droplets {
		servers = append(servers, serverFromDroplet(item, cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	now := b.now()
	created := droplet{}
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		var ambiguous *ambiguousDropletCreateError
		if errors.As(err, &ambiguous) {
			return
		}
		claimPersisted := false
		var claimErr error
		if created.ID != 0 {
			claimErr = b.persistAcquireCleanupClaim(leaseID, slug, cfg, created, req.Repo.Root, req.Keep, now)
			claimPersisted = claimErr == nil
		}
		if cleanupErr := rollbackDigitalOceanAcquire(client, created.ID, providerKeyForLease(leaseID)); cleanupErr != nil {
			err = fmt.Errorf("%v; digitalocean cleanup failed: %w", err, errors.Join(claimErr, cleanupErr))
			return
		}
		if claimPersisted {
			core.RemoveLeaseClaim(leaseID)
		}
		core.RemoveStoredTestboxKey(leaseID)
	}()
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if !cfg.ServerTypeExplicit || cfg.ServerType == "" {
		cfg.ServerType = digitalOceanServerTypeForClass(cfg.Class)
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=digitalocean lease=%s slug=%s type=%s region=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, digitalOceanRegion(cfg), digitalOceanImage(cfg), req.Keep)
	created, err = client.CreateDroplet(ctx, cfg, publicKey, leaseID, slug, req.Keep, now)
	if err != nil {
		var ambiguous *ambiguousDropletCreateError
		if errors.As(err, &ambiguous) {
			labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, now)
			labels["state"] = "provisioning"
			labels["recovery"] = "ambiguous-create"
			repoRoot := req.Repo.Root
			if repoRoot == "" {
				repoRoot, _ = os.Getwd()
			}
			server := core.Server{
				Provider: providerName,
				Name:     core.LeaseProviderName(leaseID, slug),
				Labels:   labels,
			}
			if claimErr := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist digitalocean ambiguous-create recovery: %w", claimErr))
			}
		}
		return core.LeaseTarget{}, err
	}
	waited, waitErr := b.waitForDropletIP(ctx, client, created.ID)
	if waitErr != nil {
		return core.LeaseTarget{}, waitErr
	}
	created = waited
	server := serverFromDroplet(created, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "digitalocean bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	readyTags := leaseTags(cfg, leaseID, slug, "ready", req.Keep, now)
	if err := client.ReplaceDropletTags(ctx, created.ID, created.Tags, readyTags); err != nil {
		return core.LeaseTarget{}, err
	} else {
		server.Labels = labelsFromTags(readyTags)
	}
	server.Status = "ready"
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s droplet=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *digitalOceanLeaseBackend) persistAcquireCleanupClaim(leaseID, slug string, cfg core.Config, created droplet, repoRoot string, keep bool, now time.Time) error {
	server := serverFromDroplet(created, cfg)
	if err := validateDropletLabels(server.Labels); err != nil {
		server.Labels = core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
		server.Labels["state"] = "provisioning"
	}
	server.Labels["recovery"] = "rollback-cleanup"
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve rollback cleanup working directory: %w", err)
		}
	}
	if err := claimLeaseTargetForRepoConfig(
		leaseID,
		slug,
		cfg,
		server,
		core.SSHTarget{},
		repoRoot,
		cfg.IdleTimeout,
		false,
	); err != nil {
		return fmt.Errorf("persist digitalocean rollback cleanup claim: %w", err)
	}
	return nil
}

func (b *digitalOceanLeaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(droplets))
	byID := map[int64]droplet{}
	for _, item := range droplets {
		server := serverFromDroplet(item, b.Cfg)
		servers = append(servers, server)
		byID[server.ID] = item
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromDroplet(byID[server.ID], req, droplets)
	}
	if id, ok := parseDropletID(req.ID); ok {
		item, err := client.GetDroplet(ctx, id)
		if err != nil {
			if req.ReleaseOnly && isDigitalOceanNotFound(err) {
				return b.releaseTargetFromClaim(ctx, client, req.ID)
			}
			return core.LeaseTarget{}, err
		}
		return b.targetFromDroplet(item, req, appendDropletIfMissing(droplets, item))
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/droplet not found: %s", req.ID)
}

func (b *digitalOceanLeaseBackend) releaseTargetFromClaim(ctx context.Context, client digitalOceanAPI, id string) (core.LeaseTarget, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(id, providerName)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok {
		if _, numeric := parseDropletID(id); numeric {
			claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
			if err != nil {
				return core.LeaseTarget{}, err
			}
		}
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/droplet not found: %s", id)
	}
	if err := validateDropletLabels(claim.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		grace := b.recoveryGrace
		if grace <= 0 {
			grace = ambiguousCreateRecoveryGrace
		}
		createdAt, _ := strconv.ParseInt(claim.Labels["created_at"], 10, 64)
		if createdAt <= 0 || b.now().Before(time.Unix(createdAt, 0).Add(grace)) {
			return core.LeaseTarget{}, core.Exit(4, "digitalocean ambiguous create recovery is still pending for lease=%s; retry stop later", claim.LeaseID)
		}
		if target, found, err := b.reconcilePendingRecovery(ctx, client, claim); err != nil {
			return core.LeaseTarget{}, err
		} else if found {
			return target, nil
		}
		return core.LeaseTarget{}, core.Exit(4, "digitalocean ambiguous create remains indeterminate for lease=%s; credentials and recovery claim retained", claim.LeaseID)
	}
	dropletID, ok := parseDropletID(claim.CloudID)
	if !ok {
		return core.LeaseTarget{}, core.Exit(4, "lease/droplet not found: %s", id)
	}
	if item, err := client.GetDroplet(ctx, dropletID); err == nil {
		labels := labelsFromTags(item.Tags)
		if err := validateDropletLabels(labels); err != nil {
			return core.LeaseTarget{}, err
		}
		if item.Name != core.LeaseProviderName(claim.LeaseID, claim.Slug) ||
			labels["crabbox"] != "true" ||
			labels["created_by"] != "crabbox" ||
			labels["provider"] != providerName ||
			labels["lease"] != claim.LeaseID ||
			labels["slug"] != claim.Slug {
			return core.LeaseTarget{}, core.Exit(2, "refusing to release DigitalOcean Droplet %d from stale local claim", dropletID)
		}
		return core.LeaseTarget{
			LeaseID: claim.LeaseID,
			Server:  serverFromDroplet(item, core.Config{}),
		}, nil
	} else if !isDigitalOceanNotFound(err) {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{
		LeaseID: claim.LeaseID,
		Server: core.Server{
			Provider: providerName,
			CloudID:  claim.CloudID,
			ID:       dropletID,
			Name:     claim.Slug,
			Labels:   claim.Labels,
		},
	}, nil
}

func (b *digitalOceanLeaseBackend) reconcilePendingRecovery(ctx context.Context, client digitalOceanAPI, claim core.LeaseClaim) (core.LeaseTarget, bool, error) {
	polls := b.recoveryReconcilePolls
	if polls <= 0 {
		polls = ambiguousCreateRecoveryPolls
	}
	interval := b.recoveryReconcileInterval
	if interval <= 0 {
		interval = ambiguousCreateRecoveryInterval
	}
	for poll := 0; poll < polls; poll++ {
		droplets, err := client.ListCrabboxDroplets(ctx)
		if err != nil {
			return core.LeaseTarget{}, false, err
		}
		matches := pendingRecoveryMatches(droplets, claim)
		switch len(matches) {
		case 1:
			server := serverFromDroplet(matches[0], b.Cfg)
			if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, server, core.SSHTarget{}); err != nil {
				return core.LeaseTarget{}, false, fmt.Errorf("persist recovered digitalocean droplet: %w", err)
			}
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, true, nil
		case 0:
		default:
			return core.LeaseTarget{}, false, core.Exit(2, "digitalocean ambiguous create recovery found multiple droplets for lease=%s", claim.LeaseID)
		}
		if poll+1 < polls {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return core.LeaseTarget{}, false, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return core.LeaseTarget{}, false, nil
}

func validateDigitalOceanAcquireConfig(cfg core.Config, osImageExplicit bool) error {
	if !osImageExplicit ||
		strings.TrimSpace(cfg.DigitalOcean.Image) != "" ||
		cfg.OSImage == "ubuntu:24.04" {
		return nil
	}
	return core.Exit(2, "provider=digitalocean does not support --os %s; use --os ubuntu:24.04 or set digitalocean.image explicitly", cfg.OSImage)
}

func pendingRecoveryMatches(droplets []droplet, claim core.LeaseClaim) []droplet {
	name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
	matches := make([]droplet, 0, 1)
	for _, item := range droplets {
		labels := labelsFromTags(item.Tags)
		if isOwnedDroplet(item) &&
			item.Name == name &&
			labels["lease"] == claim.LeaseID &&
			labels["slug"] == claim.Slug {
			matches = append(matches, item)
		}
	}
	return matches
}

func appendDropletIfMissing(droplets []droplet, item droplet) []droplet {
	for _, existing := range droplets {
		if existing.ID == item.ID {
			return droplets
		}
	}
	return append(droplets, item)
}

func (b *digitalOceanLeaseBackend) persistPendingRecoveryServer(server core.Server, droplets []droplet) error {
	leaseID := server.Labels["lease"]
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: unable to inspect local claim for visible digitalocean lease=%s: %v\n", leaseID, err)
		return nil
	}
	if !isPendingRecoveryClaim(claim, leaseID) {
		return nil
	}
	return persistPendingRecoveryClaim(server, droplets, claim)
}

func isPendingRecoveryClaim(claim core.LeaseClaim, leaseID string) bool {
	return claim.LeaseID == leaseID &&
		claim.Provider == providerName &&
		strings.TrimSpace(claim.CloudID) == ""
}

func persistPendingRecoveryClaim(server core.Server, droplets []droplet, claim core.LeaseClaim) error {
	matches := pendingRecoveryMatches(droplets, claim)
	if len(matches) > 1 {
		return core.Exit(2, "digitalocean ambiguous create recovery found multiple droplets for lease=%s", claim.LeaseID)
	}
	if len(matches) != 1 || matches[0].ID != server.ID {
		return core.Exit(2, "refusing to bind digitalocean ambiguous create recovery to mismatched droplet=%d lease=%s", server.ID, claim.LeaseID)
	}
	if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, server, core.SSHTarget{}); err != nil {
		return fmt.Errorf("persist recovered digitalocean droplet: %w", err)
	}
	return nil
}

func (b *digitalOceanLeaseBackend) targetFromDroplet(item droplet, req core.ResolveRequest, droplets []droplet) (core.LeaseTarget, error) {
	if err := validateDropletLabels(labelsFromTags(item.Tags)); err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromDroplet(item, b.Cfg)
	leaseID := server.Labels["lease"]
	if req.ReleaseOnly {
		target := core.LeaseTarget{Server: server, LeaseID: leaseID}
		if err := b.persistPendingRecoveryServer(server, droplets); err != nil {
			return core.LeaseTarget{}, err
		}
		return target, nil
	}
	ssh := core.SSHTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			ssh.Key = keyPath
		}
	}
	if req.Repo.Root != "" {
		if err := claimLeaseTargetForRepoConfig(leaseID, server.Labels["slug"], b.Cfg, server, ssh, req.Repo.Root, b.Cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *digitalOceanLeaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return nil, err
	}
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(droplets))
	for _, item := range droplets {
		out = append(out, serverFromDroplet(item, b.Cfg))
	}
	return out, nil
}

func (b *digitalOceanLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	leases, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult(providerName, len(leases))
	result.Message += fmt.Sprintf(" default_type=%s region=%s image=%s", b.Cfg.ServerType, digitalOceanRegion(b.Cfg), digitalOceanImage(b.Cfg))
	return result, nil
}

func (b *digitalOceanLeaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	return b.deleteServer(ctx, b.Cfg, req.Lease.Server)
}

func (b *digitalOceanLeaseBackend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s droplet=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *digitalOceanLeaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if err := validateDropletLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.GetDroplet(ctx, server.ID)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateLiveDroplet(item, server); err != nil {
		return core.Server{}, err
	}
	cfg := b.Cfg
	labels := normalizedDropletLabels(item.Tags)
	liveTailscale := map[string]string{}
	for _, key := range tagLabelKeys() {
		if value, ok := labels[key]; ok && exactTagValueKey(key) {
			liveTailscale[key] = value
		}
	}
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		updated := make(map[string]string, len(labels))
		for key, value := range labels {
			updated[key] = value
		}
		labels = updated
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.now())
	for key, value := range liveTailscale {
		labels[key] = value
	}
	if err := client.ReplaceDropletTags(ctx, server.ID, item.Tags, tagsFromLabels(labels)); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *digitalOceanLeaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	server := lease.Server
	if err := validateDropletLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.GetDroplet(ctx, server.ID)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateLiveDroplet(item, server); err != nil {
		return core.Server{}, err
	}
	labels := normalizedDropletLabels(item.Tags)
	applyTailscaleMetadata(labels, meta)
	if err := client.ReplaceDropletTags(ctx, server.ID, item.Tags, tagsFromLabels(labels)); err != nil {
		return core.Server{}, err
	}
	server = serverFromDroplet(item, b.Cfg)
	server.Labels = labels
	return server, nil
}

func (b *digitalOceanLeaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func applyTailscaleMetadata(labels map[string]string, meta core.TailscaleMetadata) {
	if meta.Enabled {
		labels["tailscale"] = "true"
	}
	if meta.Hostname != "" {
		labels["tailscale_hostname"] = meta.Hostname
	}
	if meta.FQDN != "" {
		labels["tailscale_fqdn"] = meta.FQDN
	}
	if meta.IPv4 != "" {
		labels["tailscale_ipv4"] = meta.IPv4
	}
	if len(meta.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(meta.Tags, ",")
	}
	if meta.State != "" {
		labels["tailscale_state"] = meta.State
	}
	if meta.Error != "" {
		labels["tailscale_error"] = meta.Error
	} else {
		delete(labels, "tailscale_error")
	}
	if meta.ExitNode != "" {
		labels["tailscale_exit_node"] = meta.ExitNode
	}
	if meta.ExitNodeAllowLANAccess {
		labels["tailscale_exit_node_allow_lan_access"] = "true"
	}
}

func (b *digitalOceanLeaseBackend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if err := validateDropletLabels(server.Labels); err != nil {
		return err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return err
	}
	cleanupServer := server
	item, err := client.GetDroplet(ctx, server.ID)
	if err == nil {
		if err := validateLiveDroplet(item, server); err != nil {
			return err
		}
		cleanupServer = serverFromDroplet(item, b.Cfg)
	} else if !isDigitalOceanNotFound(err) {
		return err
	}
	leaseID := cleanupServer.Labels["lease"]
	claim, err := b.ensureCleanupClaim(cleanupServer)
	if err != nil {
		return err
	}
	if isPendingRecoveryClaim(claim, leaseID) {
		if item.ID != 0 {
			droplets, err := client.ListCrabboxDroplets(ctx)
			if err != nil {
				return err
			}
			if err := persistPendingRecoveryClaim(cleanupServer, droplets, claim); err != nil {
				return err
			}
		} else if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, cleanupServer, core.SSHTarget{}); err != nil {
			return fmt.Errorf("persist recovered digitalocean droplet: %w", err)
		}
	}
	if err := client.DeleteDroplet(ctx, server.ID); err != nil {
		return err
	}
	if keyName := providerKeyForLease(leaseID); core.ValidCrabboxProviderKey(keyName) {
		if err := client.DeleteSSHKeyByName(ctx, keyName); err != nil {
			return err
		}
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return err
	}
	cloudID := firstNonBlank(server.CloudID, strconv.FormatInt(server.ID, 10))
	if !ok {
		existing, err := core.ReadLeaseClaim(leaseID)
		if err != nil {
			return err
		}
		if existing.LeaseID != "" {
			return nil
		}
		core.RemoveStoredTestboxKey(leaseID)
		return nil
	}
	if claim.CloudID != "" && claim.CloudID != cloudID {
		return nil
	}
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *digitalOceanLeaseBackend) ensureCleanupClaim(server core.Server) (core.LeaseClaim, error) {
	leaseID := server.Labels["lease"]
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if claim.LeaseID == "" {
		repoRoot, err := os.Getwd()
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("resolve cleanup claim working directory: %w", err)
		}
		if err := claimLeaseTargetForRepoConfig(
			leaseID,
			server.Labels["slug"],
			b.Cfg,
			server,
			core.SSHTarget{},
			repoRoot,
			b.Cfg.IdleTimeout,
			false,
		); err != nil {
			return core.LeaseClaim{}, fmt.Errorf("persist digitalocean cleanup claim: %w", err)
		}
		claim, err = core.ReadLeaseClaim(leaseID)
		if err != nil {
			return core.LeaseClaim{}, err
		}
	}
	cloudID := firstNonBlank(server.CloudID, strconv.FormatInt(server.ID, 10))
	if claim.Provider == providerName && claim.CloudID != "" && claim.CloudID != cloudID {
		return core.LeaseClaim{}, core.Exit(2, "refusing to release DigitalOcean Droplet %d from stale local claim", server.ID)
	}
	return claim, nil
}

func (b *digitalOceanLeaseBackend) waitForDropletIP(ctx context.Context, client digitalOceanAPI, id int64) (droplet, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		item, err := client.GetDroplet(ctx, id)
		if err != nil {
			return droplet{}, err
		}
		if publicIPv4(item) != "" {
			return item, nil
		}
		if time.Now().After(deadline) {
			return droplet{}, core.Exit(5, "timed out waiting for DigitalOcean Droplet IP")
		}
		time.Sleep(3 * time.Second)
	}
}

func (b *digitalOceanLeaseBackend) now() time.Time {
	if b.RT.Clock != nil {
		return b.RT.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func rollbackDigitalOceanAcquire(client digitalOceanAPI, dropletID int64, keyName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var errs []error
	if dropletID != 0 {
		if err := client.DeleteDroplet(ctx, dropletID); err != nil {
			errs = append(errs, err)
		}
	}
	if keyName != "" {
		if err := client.DeleteSSHKeyByName(ctx, keyName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateLiveDroplet(item droplet, expected core.Server) error {
	labels := normalizedDropletLabels(item.Tags)
	if err := validateDropletLabels(labels); err != nil {
		return err
	}
	expectedProviderKey := expected.Labels["provider_key"]
	if expectedProviderKey == "" && expected.Labels["lease"] != "" {
		expectedProviderKey = providerKeyForLease(expected.Labels["lease"])
	}
	if item.ID != expected.ID ||
		item.Name != expected.Name ||
		labels["lease"] != expected.Labels["lease"] ||
		labels["slug"] != expected.Labels["slug"] ||
		labels["provider_key"] != expectedProviderKey {
		return core.Exit(2, "refusing to operate on changed DigitalOcean Droplet %d", expected.ID)
	}
	return nil
}

func serverFromDroplet(item droplet, cfg core.Config) core.Server {
	labels := normalizedDropletLabels(item.Tags)
	server := core.Server{
		CloudID:  strconv.FormatInt(item.ID, 10),
		Provider: providerName,
		ID:       item.ID,
		Name:     item.Name,
		Status:   normalizeDropletStatus(item.Status),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = publicIPv4(item)
	server.ServerType.Name = firstNonBlank(item.Size.Slug, cfg.ServerType)
	return server
}

func normalizedDropletLabels(tags []string) map[string]string {
	labels := labelsFromTags(tags)
	if labels["provider_key"] == "" && labels["lease"] != "" {
		labels["provider_key"] = providerKeyForLease(labels["lease"])
	}
	return labels
}

func publicIPv4(item droplet) string {
	for _, net := range item.Networks.V4 {
		if net.Type == "public" && net.IPAddress != "" {
			return net.IPAddress
		}
	}
	return ""
}

func normalizeDropletStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active":
		return "ready"
	case "":
		return "unknown"
	default:
		return status
	}
}

func parseDropletID(id string) (int64, bool) {
	if strings.TrimSpace(id) == "" || strings.HasPrefix(id, "cbx_") {
		return 0, false
	}
	parsed, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func applyDigitalOceanDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.DigitalOcean.Region == "" {
		cfg.DigitalOcean.Region = "nyc3"
	}
	if cfg.DigitalOcean.Image == "" {
		cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
	}
	if !cfg.ServerTypeExplicit || cfg.ServerType == "" {
		cfg.ServerType = digitalOceanServerTypeForClass(cfg.Class)
	}
	if cfg.SSHUser == "" || cfg.SSHUser == "crabbox" {
		cfg.SSHUser = "root"
	}
	if cfg.SSHPort == "" || cfg.SSHPort == core.BaseConfig().SSHPort {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
