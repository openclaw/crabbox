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
	AccountID(context.Context) (string, error)
	ListCrabboxDroplets(context.Context) ([]droplet, error)
	GetDroplet(context.Context, int64) (droplet, error)
	CreateDroplet(context.Context, core.Config, string, string, string, bool, time.Time) (droplet, error)
	DeleteDroplet(context.Context, int64) error
	FindSSHKey(context.Context, string, string) (sshKey, bool, error)
	DeleteSSHKey(context.Context, int64) error
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
	ambiguousCreateRecoveryGrace         = 2 * time.Minute
	ambiguousCreateRecoveryPolls         = 3
	ambiguousCreateRecoveryInterval      = 2 * time.Second
	digitalOceanAccountLabel             = "provider_account"
	digitalOceanRecoveryKeyIDLabel       = "recovery_key_id"
	digitalOceanKeyOwnedLabel            = "provider_key_owned"
	digitalOceanKeyDeleteAuthorizedLabel = "provider_key_delete_authorized_id"
)

func NewDigitalOceanLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	acquireConfigErr := validateDigitalOceanAcquireConfig(cfg, core.OSImageWasExplicit(cfg))
	applyDigitalOceanDefaults(&cfg)
	b := &digitalOceanLeaseBackend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true},
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
	accountID, err := client.AccountID(ctx)
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
		var ambiguousDroplet *ambiguousDropletCreateError
		var ambiguousKey *ambiguousSSHKeyCreateError
		if errors.As(err, &ambiguousDroplet) || errors.As(err, &ambiguousKey) {
			return
		}
		keyID := created.SSHKeyID
		keyCreated := created.SSHKeyCreated
		keyOwnershipKnown := created.ID != 0 && created.SSHKeyID > 0
		cleanupKeyID := int64(0)
		if keyOwnershipKnown && keyCreated {
			cleanupKeyID = keyID
		}
		var keyCleanup *sshKeyCleanupError
		if errors.As(err, &keyCleanup) {
			keyID = keyCleanup.keyID
			keyCreated = true
			keyOwnershipKnown = true
			cleanupKeyID = keyCleanup.keyID
		}
		if created.ID == 0 && !keyOwnershipKnown {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		claimErr := b.persistAcquireCleanupClaim(
			leaseID,
			slug,
			cfg,
			created,
			keyID,
			keyCreated,
			keyOwnershipKnown,
			req.Repo.Root,
			accountID,
			req.Keep,
			now,
		)
		claimPersisted := claimErr == nil
		if cleanupErr := rollbackDigitalOceanAcquire(client, created.ID, cleanupKeyID); cleanupErr != nil {
			err = fmt.Errorf("%v; digitalocean cleanup failed: %w", err, errors.Join(claimErr, cleanupErr))
			return
		}
		if keyCleanup != nil {
			err = keyCleanup.cause
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
		recovery := ""
		var ambiguousDroplet *ambiguousDropletCreateError
		var ambiguousKey *ambiguousSSHKeyCreateError
		recoveryKeyID := int64(0)
		recoveryKeyCreated := false
		recoveryKeyOwnershipKnown := false
		switch {
		case errors.As(err, &ambiguousDroplet):
			recovery = "ambiguous-create"
			recoveryKeyID = ambiguousDroplet.keyID
			recoveryKeyCreated = ambiguousDroplet.keyCreated
			recoveryKeyOwnershipKnown = ambiguousDroplet.keyOwnershipKnown
		case errors.As(err, &ambiguousKey):
			recovery = "ambiguous-key-create"
		}
		if recovery != "" {
			if claimErr := b.persistAcquireRecoveryClaim(
				leaseID,
				slug,
				cfg,
				req.Repo.Root,
				accountID,
				req.Keep,
				now,
				recovery,
				recoveryKeyID,
				recoveryKeyCreated,
				recoveryKeyOwnershipKnown,
			); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist digitalocean %s recovery: %w", recovery, claimErr))
			}
		}
		return core.LeaseTarget{}, err
	}
	waited, waitErr := b.waitForDropletIP(ctx, client, created.ID)
	if waitErr != nil {
		return core.LeaseTarget{}, waitErr
	}
	waited.SSHKeyID = created.SSHKeyID
	waited.SSHKeyCreated = created.SSHKeyCreated
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
	server.Labels[digitalOceanAccountLabel] = accountID
	setDigitalOceanKeyIdentity(server.Labels, created.SSHKeyID, created.SSHKeyCreated, true)
	server.Status = "ready"
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s droplet=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *digitalOceanLeaseBackend) persistAcquireRecoveryClaim(
	leaseID, slug string,
	cfg core.Config,
	repoRoot, accountID string,
	keep bool,
	now time.Time,
	recovery string,
	keyID int64,
	keyCreated bool,
	keyOwnershipKnown bool,
) error {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = "provisioning"
	labels["recovery"] = recovery
	labels[digitalOceanAccountLabel] = accountID
	setDigitalOceanKeyIdentity(labels, keyID, keyCreated, keyOwnershipKnown)
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve recovery working directory: %w", err)
		}
	}
	server := core.Server{
		Provider: providerName,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   labels,
	}
	return claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false)
}

func (b *digitalOceanLeaseBackend) persistAcquireCleanupClaim(
	leaseID, slug string,
	cfg core.Config,
	created droplet,
	keyID int64,
	keyCreated, keyOwnershipKnown bool,
	repoRoot, accountID string,
	keep bool,
	now time.Time,
) error {
	server := core.Server{
		Provider: providerName,
		Name:     core.LeaseProviderName(leaseID, slug),
	}
	if created.ID != 0 {
		server = serverFromDroplet(created, cfg)
	}
	if err := validateDropletLabels(server.Labels); err != nil {
		server.Labels = core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
		server.Labels["state"] = "provisioning"
	}
	server.Labels["recovery"] = "rollback-cleanup"
	server.Labels[digitalOceanAccountLabel] = accountID
	setDigitalOceanKeyIdentity(server.Labels, keyID, keyCreated, keyOwnershipKnown)
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
	accountID, err := client.AccountID(ctx)
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
	if id, ok := parseDropletID(req.ID); ok {
		if item, found := byID[id]; found {
			return b.targetFromDroplet(item, req, droplets, accountID)
		}
		item, err := client.GetDroplet(ctx, id)
		if err != nil {
			if req.ReleaseOnly && isDigitalOceanNotFound(err) {
				return b.releaseTargetFromClaim(ctx, client, req.ID, accountID)
			}
			return core.LeaseTarget{}, err
		}
		return b.targetFromDroplet(item, req, appendDropletIfMissing(droplets, item), accountID)
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromDroplet(byID[server.ID], req, droplets, accountID)
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID, accountID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/droplet not found: %s", req.ID)
}

func (b *digitalOceanLeaseBackend) releaseTargetFromClaim(ctx context.Context, client digitalOceanAPI, id, accountID string) (core.LeaseTarget, error) {
	var (
		claim core.LeaseClaim
		ok    bool
		err   error
	)
	if dropletID, numeric := parseDropletID(id); numeric {
		id = strconv.FormatInt(dropletID, 10)
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
	} else {
		var exact bool
		claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, providerName)
		if err == nil && exact && (!ok || claim.LeaseID != id) {
			return core.LeaseTarget{}, core.Exit(2, "digitalocean exact lease identifier %q does not match a valid digitalocean claim", id)
		}
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/droplet not found: %s", id)
	}
	if !core.LeaseClaimMatchesIdentifier(claim, id) {
		return core.LeaseTarget{}, core.Exit(2, "digitalocean lease claim does not match requested identifier %q", id)
	}
	if err := validateDropletLabels(claim.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateDigitalOceanClaimIdentity(claim, claim.LeaseID, claim.Slug); err != nil {
		return core.LeaseTarget{}, err
	}
	expectedAccountID := strings.TrimSpace(claim.Labels[digitalOceanAccountLabel])
	if expectedAccountID == "" {
		return core.LeaseTarget{}, core.Exit(3, "digitalocean lease claim has no account identity; refusing claim-only recovery")
	}
	if expectedAccountID != accountID {
		return core.LeaseTarget{}, core.Exit(3, "digitalocean account mismatch: current account %s does not match lease account %s", accountID, expectedAccountID)
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		recovery := claim.Labels["recovery"]
		if recovery == "rollback-cleanup" {
			if expectedAccountID == "" {
				return core.LeaseTarget{}, core.Exit(3, "digitalocean key cleanup claim has no account identity; switch to the original account and retry")
			}
			return core.LeaseTarget{
				LeaseID: claim.LeaseID,
				Server: core.Server{
					Provider: providerName,
					Name:     claim.Slug,
					Labels:   claim.Labels,
				},
			}, nil
		}
		if recovery == "ambiguous-key-create" && strings.TrimSpace(claim.Labels[digitalOceanRecoveryKeyIDLabel]) != "" {
			return core.LeaseTarget{
				LeaseID: claim.LeaseID,
				Server: core.Server{
					Provider: providerName,
					Name:     claim.Slug,
					Labels:   claim.Labels,
				},
			}, nil
		}
		if recovery != "ambiguous-create" && recovery != "ambiguous-key-create" {
			return core.LeaseTarget{}, core.Exit(2, "digitalocean lease claim has invalid recovery state %q for lease=%s", recovery, claim.LeaseID)
		}
		grace := b.recoveryGrace
		if grace <= 0 {
			grace = ambiguousCreateRecoveryGrace
		}
		createdAt, _ := strconv.ParseInt(claim.Labels["created_at"], 10, 64)
		recoveryName := recovery
		if recoveryName == "" {
			recoveryName = "ambiguous-create"
		}
		if createdAt <= 0 || b.now().Before(time.Unix(createdAt, 0).Add(grace)) {
			return core.LeaseTarget{}, core.Exit(4, "digitalocean %s recovery is still pending for lease=%s; retry stop later", recoveryName, claim.LeaseID)
		}
		if recovery == "ambiguous-key-create" {
			if target, found, err := b.reconcilePendingKeyRecovery(ctx, client, claim); err != nil {
				return core.LeaseTarget{}, err
			} else if found {
				return target, nil
			}
			return core.LeaseTarget{}, core.Exit(4, "digitalocean ambiguous SSH-key create remains indeterminate for lease=%s; credentials and recovery claim retained", claim.LeaseID)
		}
		if target, found, err := b.reconcilePendingRecovery(ctx, client, claim, accountID); err != nil {
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
		server := serverFromDroplet(item, core.Config{})
		server.Labels[digitalOceanAccountLabel] = accountID
		preserveDigitalOceanKeyIdentity(server.Labels, claim.Labels)
		return core.LeaseTarget{
			LeaseID: claim.LeaseID,
			Server:  server,
		}, nil
	} else if !isDigitalOceanNotFound(err) {
		return core.LeaseTarget{}, err
	}
	if expectedAccountID == "" {
		return core.LeaseTarget{}, core.Exit(3, "digitalocean lease claim has no account identity; refusing claim-only cleanup after Droplet lookup returned not found")
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

func (b *digitalOceanLeaseBackend) reconcilePendingRecovery(ctx context.Context, client digitalOceanAPI, claim core.LeaseClaim, accountID string) (core.LeaseTarget, bool, error) {
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
			server.Labels[digitalOceanAccountLabel] = accountID
			preserveDigitalOceanKeyIdentity(server.Labels, claim.Labels)
			if _, err := core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, server, core.SSHTarget{}); err != nil {
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

func (b *digitalOceanLeaseBackend) reconcilePendingKeyRecovery(ctx context.Context, client digitalOceanAPI, claim core.LeaseClaim) (core.LeaseTarget, bool, error) {
	keyPath, err := core.TestboxKeyPath(claim.LeaseID)
	if err != nil {
		return core.LeaseTarget{}, false, err
	}
	publicKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return core.LeaseTarget{}, false, fmt.Errorf("read retained digitalocean SSH public key: %w", err)
	}
	publicKey := strings.TrimSpace(string(publicKeyBytes))
	if publicKey == "" {
		return core.LeaseTarget{}, false, core.Exit(2, "retained digitalocean SSH public key is empty for lease=%s", claim.LeaseID)
	}
	polls := b.recoveryReconcilePolls
	if polls <= 0 {
		polls = ambiguousCreateRecoveryPolls
	}
	interval := b.recoveryReconcileInterval
	if interval <= 0 {
		interval = ambiguousCreateRecoveryInterval
	}
	keyName := providerKeyForLease(claim.LeaseID)
	for poll := 0; poll < polls; poll++ {
		key, found, err := client.FindSSHKey(ctx, keyName, publicKey)
		if err != nil {
			return core.LeaseTarget{}, false, err
		}
		if found {
			if strings.TrimSpace(key.PublicKey) != publicKey {
				return core.LeaseTarget{}, false, core.Exit(2, "refusing to delete digitalocean SSH key %q with a different public key", keyName)
			}
			labels := make(map[string]string, len(claim.Labels)+1)
			for label, value := range claim.Labels {
				labels[label] = value
			}
			labels[digitalOceanRecoveryKeyIDLabel] = strconv.FormatInt(key.ID, 10)
			labels[digitalOceanKeyOwnedLabel] = "true"
			server := core.Server{
				Provider: providerName,
				Name:     claim.Slug,
				Labels:   labels,
			}
			if _, err := core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, server, core.SSHTarget{}); err != nil {
				return core.LeaseTarget{}, false, fmt.Errorf("persist recovered digitalocean SSH key identity: %w", err)
			}
			return core.LeaseTarget{
				LeaseID: claim.LeaseID,
				Server:  server,
			}, true, nil
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

func (b *digitalOceanLeaseBackend) persistPendingRecoveryServer(server core.Server, droplets []droplet, claim core.LeaseClaim) error {
	leaseID := server.Labels["lease"]
	if !isPendingRecoveryClaim(claim, leaseID) {
		return nil
	}
	if err := validateDigitalOceanClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
		return err
	}
	expected := strings.TrimSpace(claim.Labels[digitalOceanAccountLabel])
	if expected == "" {
		return core.Exit(3, "digitalocean recovery claim has no account identity; refusing to bind it to the current account")
	}
	if expected != server.Labels[digitalOceanAccountLabel] {
		return core.Exit(3, "digitalocean account mismatch: current account %s does not match lease account %s", server.Labels[digitalOceanAccountLabel], expected)
	}
	_, err := persistPendingRecoveryClaim(server, droplets, claim)
	return err
}

func isPendingRecoveryClaim(claim core.LeaseClaim, leaseID string) bool {
	return claim.LeaseID == leaseID &&
		claim.Provider == providerName &&
		strings.TrimSpace(claim.CloudID) == "" &&
		claim.Labels["recovery"] == "ambiguous-create"
}

func validateDigitalOceanClaimIdentity(claim core.LeaseClaim, leaseID, slug string) error {
	if claim.LeaseID != leaseID ||
		claim.Provider != providerName ||
		claim.Slug == "" ||
		(slug != "" && claim.Slug != slug) ||
		claim.Labels["lease"] != leaseID ||
		claim.Labels["slug"] != claim.Slug ||
		claim.Labels["provider"] != providerName {
		return core.Exit(2, "digitalocean lease claim identity does not match lease=%s slug=%s", leaseID, slug)
	}
	return nil
}

func persistPendingRecoveryClaim(server core.Server, droplets []droplet, claim core.LeaseClaim) (core.LeaseClaim, error) {
	matches := pendingRecoveryMatches(droplets, claim)
	if len(matches) > 1 {
		return core.LeaseClaim{}, core.Exit(2, "digitalocean ambiguous create recovery found multiple droplets for lease=%s", claim.LeaseID)
	}
	if len(matches) != 1 || matches[0].ID != server.ID {
		return core.LeaseClaim{}, core.Exit(2, "refusing to bind digitalocean ambiguous create recovery to mismatched droplet=%d lease=%s", server.ID, claim.LeaseID)
	}
	preserveDigitalOceanKeyIdentity(server.Labels, claim.Labels)
	updated, err := core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, server, core.SSHTarget{})
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("persist recovered digitalocean droplet: %w", err)
	}
	return updated, nil
}

func (b *digitalOceanLeaseBackend) targetFromDroplet(item droplet, req core.ResolveRequest, droplets []droplet, accountID string) (core.LeaseTarget, error) {
	if err := validateDropletLabels(labelsFromTags(item.Tags)); err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromDroplet(item, b.Cfg)
	server.Labels[digitalOceanAccountLabel] = accountID
	leaseID := server.Labels["lease"]
	claim, claimExists, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil {
		return core.LeaseTarget{}, fmt.Errorf("read digitalocean lease claim: %w", claimErr)
	}
	if claimExists {
		if claim.Provider != providerName {
			return core.LeaseTarget{}, core.Exit(2, "lease=%s is claimed by provider=%s; refusing digitalocean claim rewrite", leaseID, claim.Provider)
		}
		if err := validateDigitalOceanClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
			return core.LeaseTarget{}, err
		}
		expectedAccountID := strings.TrimSpace(claim.Labels[digitalOceanAccountLabel])
		if expectedAccountID == "" {
			return core.LeaseTarget{}, core.Exit(3, "digitalocean lease claim has no account identity; refusing to bind it to the current account")
		}
		if expectedAccountID != accountID {
			return core.LeaseTarget{}, core.Exit(3, "digitalocean account mismatch: current account %s does not match lease account %s", accountID, expectedAccountID)
		}
		liveCloudID := firstNonBlank(server.CloudID, strconv.FormatInt(server.ID, 10))
		if claim.CloudID != "" && claim.CloudID != liveCloudID {
			return core.LeaseTarget{}, core.Exit(2, "refusing to resolve DigitalOcean Droplet %d from stale local claim", server.ID)
		}
		if claim.CloudID == "" && !isPendingRecoveryClaim(claim, leaseID) {
			return core.LeaseTarget{}, core.Exit(2, "digitalocean lease claim has no Droplet identity or valid pending recovery state for lease=%s", leaseID)
		}
		preserveDigitalOceanKeyIdentity(server.Labels, claim.Labels)
	}
	if req.ReleaseOnly {
		target := core.LeaseTarget{Server: server, LeaseID: leaseID}
		if err := b.persistPendingRecoveryServer(server, droplets, claim); err != nil {
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
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], b.Cfg, server, ssh, req.Repo.Root, b.Cfg.IdleTimeout, req.Reclaim, claim, claimExists); err != nil {
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
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.AccountID(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	droplets, err := client.ListCrabboxDroplets(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult(providerName, len(droplets))
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
	accountID := strings.TrimSpace(server.Labels[digitalOceanAccountLabel])
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
	preserveDigitalOceanKeyIdentity(labels, server.Labels)
	if accountID != "" {
		labels[digitalOceanAccountLabel] = accountID
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
	preserveDigitalOceanKeyIdentity(labels, server.Labels)
	if accountID := strings.TrimSpace(server.Labels[digitalOceanAccountLabel]); accountID != "" {
		labels[digitalOceanAccountLabel] = accountID
	}
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
	cleanupServer.Labels = make(map[string]string, len(server.Labels)+1)
	for key, value := range server.Labels {
		cleanupServer.Labels[key] = value
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return err
	}
	if expected := strings.TrimSpace(cleanupServer.Labels[digitalOceanAccountLabel]); expected != "" && expected != accountID {
		return core.Exit(3, "digitalocean account mismatch: current account %s does not match lease account %s", accountID, expected)
	}
	cleanupServer.Labels[digitalOceanAccountLabel] = accountID
	item := droplet{}
	if server.ID != 0 {
		item, err = client.GetDroplet(ctx, server.ID)
		if err == nil {
			if err := validateLiveDroplet(item, server); err != nil {
				return err
			}
			cleanupServer = serverFromDroplet(item, b.Cfg)
			cleanupServer.Labels[digitalOceanAccountLabel] = accountID
			preserveDigitalOceanKeyIdentity(cleanupServer.Labels, server.Labels)
		} else if !isDigitalOceanNotFound(err) {
			return err
		}
	}
	leaseID := cleanupServer.Labels["lease"]
	claim, err := b.ensureCleanupClaim(cleanupServer, item.ID != 0)
	if err != nil {
		return err
	}
	if isPendingRecoveryClaim(claim, leaseID) {
		if item.ID != 0 {
			droplets, err := client.ListCrabboxDroplets(ctx)
			if err != nil {
				return err
			}
			claim, err = persistPendingRecoveryClaim(cleanupServer, droplets, claim)
			if err != nil {
				return err
			}
		} else {
			claim, err = core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, cleanupServer, core.SSHTarget{})
			if err != nil {
				return fmt.Errorf("persist recovered digitalocean droplet: %w", err)
			}
		}
	}
	recoveryKeyID := strings.TrimSpace(claim.Labels[digitalOceanRecoveryKeyIDLabel])
	keyOwned := strings.TrimSpace(claim.Labels[digitalOceanKeyOwnedLabel])
	foreignClaim := claim.Provider != providerName
	if server.ID != 0 {
		if err := client.DeleteDroplet(ctx, server.ID); err != nil {
			return err
		}
	}
	if !foreignClaim && (keyOwned == "" || keyOwned == "unknown") {
		keyID, owned, known, recoveryErr := b.recoverDigitalOceanKeyIdentity(ctx, client, leaseID)
		if recoveryErr != nil {
			return recoveryErr
		}
		if known {
			setDigitalOceanKeyIdentity(cleanupServer.Labels, keyID, owned, true)
		} else {
			delete(cleanupServer.Labels, digitalOceanRecoveryKeyIDLabel)
			cleanupServer.Labels[digitalOceanKeyOwnedLabel] = "unknown"
		}
		claim, err = core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, cleanupServer, core.SSHTarget{})
		if err != nil {
			return fmt.Errorf("persist recovered digitalocean SSH key identity: %w", err)
		}
		recoveryKeyID = strings.TrimSpace(claim.Labels[digitalOceanRecoveryKeyIDLabel])
		keyOwned = strings.TrimSpace(claim.Labels[digitalOceanKeyOwnedLabel])
	}
	if !foreignClaim && keyOwned == "true" && recoveryKeyID != "" {
		keyID, parseErr := strconv.ParseInt(recoveryKeyID, 10, 64)
		if parseErr != nil || keyID <= 0 {
			return core.Exit(2, "invalid digitalocean recovery SSH key id %q", recoveryKeyID)
		}
		labels := make(map[string]string, len(claim.Labels)+1)
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels[digitalOceanKeyDeleteAuthorizedLabel] = recoveryKeyID
		claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels)
		if err != nil {
			return fmt.Errorf("authorize digitalocean SSH key cleanup: %w", err)
		}
		if err := client.DeleteSSHKey(ctx, keyID); err != nil {
			return err
		}
	} else if !foreignClaim && keyOwned == "true" {
		return core.Exit(2, "digitalocean lease=%s owns an SSH key but its immutable key id is missing", leaseID)
	} else if !foreignClaim && keyOwned != "false" {
		return core.Exit(4, "digitalocean SSH key ownership remains indeterminate for lease=%s; local claim and credentials retained", leaseID)
	}
	if foreignClaim {
		return nil
	}
	if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
		return fmt.Errorf("finalize digitalocean cleanup claim: %w", err)
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *digitalOceanLeaseBackend) ensureCleanupClaim(server core.Server, liveDropletVerified bool) (core.LeaseClaim, error) {
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("read digitalocean cleanup claim: %w", err)
	}
	if claimExists {
		if claim.LeaseID != leaseID || claim.Provider == "" {
			return core.LeaseClaim{}, core.Exit(2, "digitalocean lease claim is incomplete for lease=%s", leaseID)
		}
	} else {
		repoRoot, err := os.Getwd()
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("resolve cleanup claim working directory: %w", err)
		}
		claim, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(
			leaseID,
			server.Labels["slug"],
			b.Cfg,
			server,
			core.SSHTarget{},
			repoRoot,
			b.Cfg.IdleTimeout,
			false,
			claim,
			false,
		)
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("persist digitalocean cleanup claim: %w", err)
		}
	}
	cloudID := firstNonBlank(server.CloudID, strconv.FormatInt(server.ID, 10))
	if claim.Provider == providerName && claim.CloudID != "" && claim.CloudID != cloudID {
		return core.LeaseClaim{}, core.Exit(2, "refusing to release DigitalOcean Droplet %d from stale local claim", server.ID)
	}
	if claim.Provider != providerName {
		return claim, nil
	}
	if err := validateDigitalOceanClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
		return core.LeaseClaim{}, err
	}
	if claim.CloudID == "" {
		recovery := claim.Labels["recovery"]
		validRecovery := (liveDropletVerified && recovery == "ambiguous-create") ||
			(server.ID == 0 && (recovery == "rollback-cleanup" || recovery == "ambiguous-key-create"))
		if !validRecovery {
			return core.LeaseClaim{}, core.Exit(2, "digitalocean lease claim has no Droplet identity or valid cleanup recovery state for lease=%s", leaseID)
		}
	}
	currentAccountID := strings.TrimSpace(server.Labels[digitalOceanAccountLabel])
	expectedAccountID := strings.TrimSpace(claim.Labels[digitalOceanAccountLabel])
	if expectedAccountID == "" {
		return core.LeaseClaim{}, core.Exit(3, "digitalocean lease claim has no account identity; refusing cleanup")
	}
	if currentAccountID != "" && expectedAccountID != currentAccountID {
		return core.LeaseClaim{}, core.Exit(3, "digitalocean account mismatch: current account %s does not match lease account %s", currentAccountID, expectedAccountID)
	}
	return claim, nil
}

func setDigitalOceanKeyIdentity(labels map[string]string, keyID int64, created, known bool) {
	if !known {
		return
	}
	labels[digitalOceanKeyOwnedLabel] = strconv.FormatBool(created)
	if keyID > 0 {
		labels[digitalOceanRecoveryKeyIDLabel] = strconv.FormatInt(keyID, 10)
	}
}

func preserveDigitalOceanKeyIdentity(labels, stored map[string]string) {
	for _, key := range []string{digitalOceanRecoveryKeyIDLabel, digitalOceanKeyOwnedLabel} {
		if value := strings.TrimSpace(stored[key]); value != "" {
			labels[key] = value
		}
	}
	authorizedKeyID := strings.TrimSpace(stored[digitalOceanKeyDeleteAuthorizedLabel])
	if authorizedKeyID != "" && authorizedKeyID == strings.TrimSpace(stored[digitalOceanRecoveryKeyIDLabel]) {
		labels[digitalOceanKeyDeleteAuthorizedLabel] = authorizedKeyID
	}
}

func (b *digitalOceanLeaseBackend) recoverDigitalOceanKeyIdentity(ctx context.Context, client digitalOceanAPI, leaseID string) (int64, bool, bool, error) {
	keyName := providerKeyForLease(leaseID)
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return 0, false, false, err
	}
	publicKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if errors.Is(err, os.ErrNotExist) {
		_, _, findErr := client.FindSSHKey(ctx, keyName, "")
		if findErr != nil {
			return 0, false, false, findErr
		}
		return 0, false, true, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("read retained digitalocean SSH public key: %w", err)
	}
	publicKey := strings.TrimSpace(string(publicKeyBytes))
	if publicKey == "" {
		return 0, false, false, core.Exit(2, "retained digitalocean SSH public key is empty for lease=%s", leaseID)
	}
	key, found, err := client.FindSSHKey(ctx, keyName, publicKey)
	if err != nil {
		var conflict *sshKeyConflictError
		if errors.As(err, &conflict) {
			return 0, false, true, nil
		}
		return 0, false, false, err
	}
	if !found {
		return 0, false, true, nil
	}
	// Identity matches, but a missing claim cannot prove Crabbox created the account key.
	return key.ID, false, true, nil
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

func rollbackDigitalOceanAcquire(client digitalOceanAPI, dropletID, keyID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var errs []error
	if dropletID != 0 {
		if err := client.DeleteDroplet(ctx, dropletID); err != nil {
			errs = append(errs, err)
		}
	}
	if keyID > 0 {
		if err := client.DeleteSSHKey(ctx, keyID); err != nil {
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
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "cbx_") {
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
	if !core.IsSSHUserExplicit(cfg) && (cfg.SSHUser == "" || cfg.SSHUser == "crabbox") {
		cfg.SSHUser = "root"
	}
	if !core.IsSSHPortExplicit(cfg) && (cfg.SSHPort == "" || cfg.SSHPort == core.BaseConfig().SSHPort) {
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
