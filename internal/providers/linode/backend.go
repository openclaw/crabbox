package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type linodeAPI interface {
	AccountID(context.Context) (string, error)
	AccountSettings(context.Context) (accountSettings, error)
	ListLinodes(context.Context) ([]linodeInstance, error)
	GetLinode(context.Context, int64) (linodeInstance, error)
	CreateLinode(context.Context, createLinodeRequest) (linodeInstance, error)
	DeleteLinode(context.Context, int64) error
	UpdateLinodeTags(context.Context, int64, []string) error
}

type linodeLeaseBackend struct {
	shared.DirectSSHBackend
	clientFactory             func(core.Runtime) (linodeAPI, error)
	waitSSH                   func(context.Context, *core.SSHTarget, string, time.Duration) error
	recoveryGrace             time.Duration
	recoveryReconcilePolls    int
	recoveryReconcileInterval time.Duration
	acquireConfigErr          error
}

const (
	ambiguousCreateRecoveryGrace    = 2 * time.Minute
	ambiguousCreateRecoveryPolls    = 3
	ambiguousCreateRecoveryInterval = 2 * time.Second
	linodeAccountLabel              = "provider_account"
)

type ambiguousLinodeCreateError struct {
	err error
}

func (e *ambiguousLinodeCreateError) Error() string {
	return fmt.Sprintf("linode instance creation remains indeterminate; preserving SSH credentials for recovery: %v", e.err)
}

func (e *ambiguousLinodeCreateError) Unwrap() error {
	return e.err
}

func NewLinodeLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	return newLinodeLeaseBackend(spec, cfg, rt)
}

func newLinodeLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) *linodeLeaseBackend {
	cfg.Provider = providerName
	acquireConfigErr := validateFoundationConfig(cfg)
	applyLinodeDefaults(&cfg)
	b := &linodeLeaseBackend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt},
		acquireConfigErr: acquireConfigErr,
	}
	b.clientFactory = func(rt core.Runtime) (linodeAPI, error) { return newLinodeClient(rt) }
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.RT.Stderr, phase, timeout)
	}
	b.Delete = b.deleteServer
	return b
}

func (b *linodeLeaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *linodeLeaseBackend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	if b.acquireConfigErr != nil {
		return core.LeaseTarget{}, b.acquireConfigErr
	}
	cfg := b.Cfg
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=linode only supports target=linux")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", cfg.Tailscale.AuthKeyEnv)
	}
	firewallID, err := parseLinodeFirewallID(cfg.Linode.FirewallID)
	if err != nil {
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
	linodes, err := client.ListLinodes(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(linodes))
	for _, item := range linodes {
		if isOwnedLinode(item) {
			servers = append(servers, serverFromLinode(item, cfg))
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
	rootPass, err := generateLinodeRootPass()
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("generate linode root password: %w", err)
	}
	now := b.now()
	created := linodeInstance{}
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		var ambiguous *ambiguousLinodeCreateError
		if errors.As(err, &ambiguous) {
			return
		}
		if created.ID == 0 {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		claimErr := b.persistAcquireCleanupClaim(leaseID, slug, cfg, created, req.Repo.Root, accountID, req.Keep, now)
		claimPersisted := claimErr == nil
		if cleanupErr := rollbackLinodeAcquire(client, created.ID); cleanupErr != nil {
			err = fmt.Errorf("%v; linode cleanup failed: %w", err, errors.Join(claimErr, cleanupErr))
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
		cfg.ServerType = linodeServerTypeForConfig(cfg)
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	createReq := createLinodeRequest{
		Region:         linodeRegionForConfig(cfg),
		Type:           linodeServerTypeForConfig(cfg),
		Image:          linodeImageForConfig(cfg),
		Label:          core.LeaseProviderName(leaseID, slug),
		Tags:           leaseTags(cfg, leaseID, slug, "provisioning", req.Keep, now),
		AuthorizedKeys: []string{publicKey},
		RootPass:       rootPass,
		Metadata:       &linodeMetadata{UserData: linodeUserData(cfg, publicKey)},
	}
	if firewallID > 0 {
		settings, settingsErr := client.AccountSettings(ctx)
		if settingsErr != nil {
			return core.LeaseTarget{}, settingsErr
		}
		if configureErr := configureLinodeFirewall(&createReq, firewallID, settings.InterfacesForNewLinodes); configureErr != nil {
			return core.LeaseTarget{}, configureErr
		}
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=linode lease=%s slug=%s type=%s region=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, linodeRegionForConfig(cfg), linodeImageForConfig(cfg), req.Keep)
	created, err = client.CreateLinode(ctx, createReq)
	if err != nil {
		if isLinodeCreateAmbiguous(err) {
			if claimErr := b.persistAcquireRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, accountID, req.Keep, now); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(&ambiguousLinodeCreateError{err: err}, fmt.Errorf("persist linode ambiguous-create recovery: %w", claimErr))
			}
			return core.LeaseTarget{}, &ambiguousLinodeCreateError{err: err}
		}
		return core.LeaseTarget{}, err
	}
	waited, waitErr := b.waitForLinodeIP(ctx, client, created.ID)
	if waitErr != nil {
		return core.LeaseTarget{}, waitErr
	}
	created = waited
	server := serverFromLinode(created, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "linode bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	readyLabels := labelsFromTags(leaseTags(cfg, leaseID, slug, "ready", req.Keep, now))
	readyLabels[linodeAccountLabel] = accountID
	if err := client.UpdateLinodeTags(ctx, created.ID, replaceCrabboxTags(created.Tags, tagsFromLabels(readyLabels))); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels = readyLabels
	server.Labels[linodeAccountLabel] = accountID
	server.Status = "ready"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s linode=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *linodeLeaseBackend) persistAcquireRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot, accountID string, keep bool, now time.Time) error {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = "provisioning"
	labels["recovery"] = "ambiguous-create"
	labels[linodeAccountLabel] = accountID
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve recovery working directory: %w", err)
		}
	}
	server := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false)
}

func (b *linodeLeaseBackend) persistAcquireCleanupClaim(leaseID, slug string, cfg core.Config, created linodeInstance, repoRoot, accountID string, keep bool, now time.Time) error {
	server := core.Server{Provider: providerName, Name: core.LeaseProviderName(leaseID, slug)}
	if created.ID != 0 {
		server = serverFromLinode(created, cfg)
	}
	if err := validateLinodeLabels(server.Labels); err != nil {
		server.Labels = core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
		server.Labels["state"] = "provisioning"
	}
	server.Labels["recovery"] = "rollback-cleanup"
	server.Labels[linodeAccountLabel] = accountID
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve rollback cleanup working directory: %w", err)
		}
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false); err != nil {
		return fmt.Errorf("persist linode rollback cleanup claim: %w", err)
	}
	return nil
}

func (b *linodeLeaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	accountID, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	linodes, err := client.ListLinodes(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(linodes))
	byID := map[int64]linodeInstance{}
	for _, item := range linodes {
		if !isOwnedLinode(item) {
			continue
		}
		server := serverFromLinode(item, b.Cfg)
		servers = append(servers, server)
		byID[server.ID] = item
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromLinode(byID[server.ID], req, linodes, accountID)
	}
	if id, ok := parseLinodeID(req.ID); ok {
		if item, found := byID[id]; found {
			return b.targetFromLinode(item, req, linodes, accountID)
		}
		item, err := client.GetLinode(ctx, id)
		if err != nil {
			if req.ReleaseOnly && isLinodeNotFound(err) {
				return b.releaseTargetFromClaim(ctx, client, req.ID, accountID)
			}
			return core.LeaseTarget{}, err
		}
		return b.targetFromLinode(item, req, appendLinodeIfMissing(linodes, item), accountID)
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID, accountID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/linode not found: %s", req.ID)
}

func (b *linodeLeaseBackend) releaseTargetFromClaim(ctx context.Context, client linodeAPI, id, accountID string) (core.LeaseTarget, error) {
	var (
		claim core.LeaseClaim
		ok    bool
		err   error
	)
	var exact bool
	claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, providerName)
	if err == nil && exact && (!ok || claim.LeaseID != id) {
		return core.LeaseTarget{}, core.Exit(2, "linode exact lease identifier %q does not match a valid linode claim", id)
	}
	if err == nil && !ok {
		if linodeID, numeric := parseLinodeID(id); numeric {
			id = strconv.FormatInt(linodeID, 10)
			claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
		}
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/linode not found: %s", id)
	}
	if !core.LeaseClaimMatchesIdentifier(claim, id) {
		return core.LeaseTarget{}, core.Exit(2, "linode lease claim does not match requested identifier %q", id)
	}
	if err := validateLinodeLabels(claim.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateLinodeClaimIdentity(claim, claim.LeaseID, claim.Slug); err != nil {
		return core.LeaseTarget{}, err
	}
	expectedAccountID := strings.TrimSpace(claim.Labels[linodeAccountLabel])
	if expectedAccountID == "" {
		return core.LeaseTarget{}, core.Exit(3, "linode lease claim has no account identity; refusing claim-only recovery")
	}
	if expectedAccountID != accountID {
		return core.LeaseTarget{}, core.Exit(3, "linode account mismatch: current account %s does not match lease account %s", accountID, expectedAccountID)
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		recovery := claim.Labels["recovery"]
		if recovery == "rollback-cleanup" {
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: core.Server{Provider: providerName, Name: claim.Slug, Labels: claim.Labels}}, nil
		}
		if recovery != "ambiguous-create" {
			return core.LeaseTarget{}, core.Exit(2, "linode lease claim has invalid recovery state %q for lease=%s", recovery, claim.LeaseID)
		}
		grace := b.recoveryGrace
		if grace <= 0 {
			grace = ambiguousCreateRecoveryGrace
		}
		createdAt, _ := strconv.ParseInt(claim.Labels["created_at"], 10, 64)
		if createdAt <= 0 || b.now().Before(time.Unix(createdAt, 0).Add(grace)) {
			return core.LeaseTarget{}, core.Exit(4, "linode ambiguous-create recovery is still pending for lease=%s; retry stop later", claim.LeaseID)
		}
		if target, found, err := b.reconcilePendingRecovery(ctx, client, claim, accountID); err != nil {
			return core.LeaseTarget{}, err
		} else if found {
			return target, nil
		}
		return core.LeaseTarget{}, core.Exit(4, "linode ambiguous create remains indeterminate for lease=%s; credentials and recovery claim retained", claim.LeaseID)
	}
	linodeID, ok := parseLinodeID(claim.CloudID)
	if !ok {
		return core.LeaseTarget{}, core.Exit(4, "lease/linode not found: %s", id)
	}
	if item, err := client.GetLinode(ctx, linodeID); err == nil {
		labels := labelsFromTags(item.Tags)
		if err := validateLinodeLabels(labels); err != nil {
			return core.LeaseTarget{}, err
		}
		if item.Label != core.LeaseProviderName(claim.LeaseID, claim.Slug) ||
			labels["provider"] != providerName ||
			labels["lease"] != claim.LeaseID ||
			labels["slug"] != claim.Slug {
			return core.LeaseTarget{}, core.Exit(2, "refusing to release Linode instance %d from stale local claim", linodeID)
		}
		server := serverFromLinode(item, core.Config{})
		server.Labels[linodeAccountLabel] = accountID
		return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	} else if !isLinodeNotFound(err) {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: core.Server{Provider: providerName, CloudID: claim.CloudID, ID: linodeID, Name: claim.Slug, Labels: claim.Labels}}, nil
}

func (b *linodeLeaseBackend) reconcilePendingRecovery(ctx context.Context, client linodeAPI, claim core.LeaseClaim, accountID string) (core.LeaseTarget, bool, error) {
	polls := b.recoveryReconcilePolls
	if polls <= 0 {
		polls = ambiguousCreateRecoveryPolls
	}
	interval := b.recoveryReconcileInterval
	if interval <= 0 {
		interval = ambiguousCreateRecoveryInterval
	}
	for poll := 0; poll < polls; poll++ {
		linodes, err := client.ListLinodes(ctx)
		if err != nil {
			return core.LeaseTarget{}, false, err
		}
		matches := pendingRecoveryMatches(linodes, claim)
		switch len(matches) {
		case 1:
			server := serverFromLinode(matches[0], b.Cfg)
			server.Labels[linodeAccountLabel] = accountID
			if _, err := core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, server, core.SSHTarget{}); err != nil {
				return core.LeaseTarget{}, false, fmt.Errorf("persist recovered linode instance: %w", err)
			}
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, true, nil
		case 0:
		default:
			return core.LeaseTarget{}, false, core.Exit(2, "linode ambiguous create recovery found multiple instances for lease=%s", claim.LeaseID)
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

func (b *linodeLeaseBackend) targetFromLinode(item linodeInstance, req core.ResolveRequest, linodes []linodeInstance, accountID string) (core.LeaseTarget, error) {
	if err := validateLinodeLabels(labelsFromTags(item.Tags)); err != nil {
		return core.LeaseTarget{}, err
	}
	server := serverFromLinode(item, b.Cfg)
	server.Labels[linodeAccountLabel] = accountID
	leaseID := server.Labels["lease"]
	claim, claimExists, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil {
		return core.LeaseTarget{}, fmt.Errorf("read linode lease claim: %w", claimErr)
	}
	if claimExists {
		if claim.Provider != providerName {
			return core.LeaseTarget{}, core.Exit(2, "lease=%s is claimed by provider=%s; refusing linode claim rewrite", leaseID, claim.Provider)
		}
		if err := validateLinodeClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
			return core.LeaseTarget{}, err
		}
		expectedAccountID := strings.TrimSpace(claim.Labels[linodeAccountLabel])
		if expectedAccountID == "" {
			return core.LeaseTarget{}, core.Exit(3, "linode lease claim has no account identity; refusing to bind it to the current account")
		}
		if expectedAccountID != accountID {
			return core.LeaseTarget{}, core.Exit(3, "linode account mismatch: current account %s does not match lease account %s", accountID, expectedAccountID)
		}
		liveCloudID := firstNonBlank(server.CloudID, strconv.FormatInt(server.ID, 10))
		if claim.CloudID != "" && claim.CloudID != liveCloudID {
			return core.LeaseTarget{}, core.Exit(2, "refusing to resolve Linode instance %d from stale local claim", server.ID)
		}
		if claim.CloudID == "" && !isPendingRecoveryClaim(claim, leaseID) {
			return core.LeaseTarget{}, core.Exit(2, "linode lease claim has no instance identity or valid pending recovery state for lease=%s", leaseID)
		}
	}
	if req.ReleaseOnly {
		target := core.LeaseTarget{Server: server, LeaseID: leaseID}
		if err := b.persistPendingRecoveryServer(server, linodes, claim); err != nil {
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

func (b *linodeLeaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return nil, err
	}
	linodes, err := client.ListLinodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(linodes))
	for _, item := range linodes {
		if isOwnedLinode(item) {
			out = append(out, serverFromLinode(item, b.Cfg))
		}
	}
	return out, nil
}

func (b *linodeLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.AccountID(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	linodes, err := client.ListLinodes(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	count := 0
	for _, item := range linodes {
		if isOwnedLinode(item) {
			count++
		}
	}
	result := core.InventoryDoctorResult(providerName, count)
	result.Message += fmt.Sprintf(" default_type=%s region=%s image=%s", b.Cfg.ServerType, linodeRegionForConfig(b.Cfg), linodeImageForConfig(b.Cfg))
	return result, nil
}

func (b *linodeLeaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	return b.deleteServer(ctx, b.Cfg, req.Lease.Server)
}

func (b *linodeLeaseBackend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s linode=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *linodeLeaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if err := validateLinodeLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.GetLinode(ctx, server.ID)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateLiveLinode(item, server); err != nil {
		return core.Server{}, err
	}
	cfg := b.Cfg
	labels := normalizedLinodeLabels(item.Tags)
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
	if accountID := strings.TrimSpace(server.Labels[linodeAccountLabel]); accountID != "" {
		labels[linodeAccountLabel] = accountID
	}
	if err := client.UpdateLinodeTags(ctx, server.ID, replaceCrabboxTags(item.Tags, tagsFromLabels(labels))); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *linodeLeaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	server := lease.Server
	if err := validateLinodeLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.GetLinode(ctx, server.ID)
	if err != nil {
		return core.Server{}, err
	}
	if err := validateLiveLinode(item, server); err != nil {
		return core.Server{}, err
	}
	labels := normalizedLinodeLabels(item.Tags)
	if accountID := strings.TrimSpace(server.Labels[linodeAccountLabel]); accountID != "" {
		labels[linodeAccountLabel] = accountID
	}
	applyTailscaleMetadata(labels, meta)
	if err := client.UpdateLinodeTags(ctx, server.ID, replaceCrabboxTags(item.Tags, tagsFromLabels(labels))); err != nil {
		return core.Server{}, err
	}
	server.Labels = labels
	return server, nil
}

func (b *linodeLeaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *linodeLeaseBackend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if err := validateLinodeLabels(server.Labels); err != nil {
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
	if expected := strings.TrimSpace(cleanupServer.Labels[linodeAccountLabel]); expected != "" && expected != accountID {
		return core.Exit(3, "linode account mismatch: current account %s does not match lease account %s", accountID, expected)
	}
	cleanupServer.Labels[linodeAccountLabel] = accountID
	item := linodeInstance{}
	if server.ID != 0 {
		item, err = client.GetLinode(ctx, server.ID)
		if err == nil {
			if err := validateLiveLinode(item, server); err != nil {
				return err
			}
			cleanupServer = serverFromLinode(item, b.Cfg)
			cleanupServer.Labels[linodeAccountLabel] = accountID
		} else if !isLinodeNotFound(err) {
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
			linodes, err := client.ListLinodes(ctx)
			if err != nil {
				return err
			}
			claim, err = persistPendingRecoveryClaim(cleanupServer, linodes, claim)
			if err != nil {
				return err
			}
		} else {
			claim, err = core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, cleanupServer, core.SSHTarget{})
			if err != nil {
				return fmt.Errorf("persist recovered linode instance: %w", err)
			}
		}
	}
	foreignClaim := claim.Provider != providerName
	if item.ID != 0 {
		if err := client.DeleteLinode(ctx, server.ID); err != nil {
			return err
		}
	}
	if foreignClaim {
		return nil
	}
	if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
		return fmt.Errorf("finalize linode cleanup claim: %w", err)
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *linodeLeaseBackend) ensureCleanupClaim(server core.Server, liveLinodeVerified bool) (core.LeaseClaim, error) {
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("read linode cleanup claim: %w", err)
	}
	if claimExists {
		if claim.LeaseID != leaseID || claim.Provider == "" {
			return core.LeaseClaim{}, core.Exit(2, "linode lease claim is incomplete for lease=%s", leaseID)
		}
	} else {
		repoRoot, err := os.Getwd()
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("resolve cleanup claim working directory: %w", err)
		}
		claim, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], b.Cfg, server, core.SSHTarget{}, repoRoot, b.Cfg.IdleTimeout, false, claim, false)
		if err != nil {
			return core.LeaseClaim{}, fmt.Errorf("persist linode cleanup claim: %w", err)
		}
	}
	cloudID := firstNonBlank(server.CloudID, strconv.FormatInt(server.ID, 10))
	if claim.Provider == providerName && claim.CloudID != "" && claim.CloudID != cloudID {
		return core.LeaseClaim{}, core.Exit(2, "refusing to release Linode instance %d from stale local claim", server.ID)
	}
	if claim.Provider != providerName {
		return claim, nil
	}
	if err := validateLinodeClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
		return core.LeaseClaim{}, err
	}
	if claim.CloudID == "" {
		recovery := claim.Labels["recovery"]
		validRecovery := (liveLinodeVerified && recovery == "ambiguous-create") ||
			(server.ID == 0 && recovery == "rollback-cleanup")
		if !validRecovery {
			return core.LeaseClaim{}, core.Exit(2, "linode lease claim has no instance identity or valid cleanup recovery state for lease=%s", leaseID)
		}
	}
	currentAccountID := strings.TrimSpace(server.Labels[linodeAccountLabel])
	expectedAccountID := strings.TrimSpace(claim.Labels[linodeAccountLabel])
	if expectedAccountID == "" {
		return core.LeaseClaim{}, core.Exit(3, "linode lease claim has no account identity; refusing cleanup")
	}
	if currentAccountID != "" && expectedAccountID != currentAccountID {
		return core.LeaseClaim{}, core.Exit(3, "linode account mismatch: current account %s does not match lease account %s", currentAccountID, expectedAccountID)
	}
	return claim, nil
}

func (b *linodeLeaseBackend) persistPendingRecoveryServer(server core.Server, linodes []linodeInstance, claim core.LeaseClaim) error {
	leaseID := server.Labels["lease"]
	if !isPendingRecoveryClaim(claim, leaseID) {
		return nil
	}
	if err := validateLinodeClaimIdentity(claim, leaseID, server.Labels["slug"]); err != nil {
		return err
	}
	expected := strings.TrimSpace(claim.Labels[linodeAccountLabel])
	if expected == "" {
		return core.Exit(3, "linode recovery claim has no account identity; refusing to bind it to the current account")
	}
	if expected != server.Labels[linodeAccountLabel] {
		return core.Exit(3, "linode account mismatch: current account %s does not match lease account %s", server.Labels[linodeAccountLabel], expected)
	}
	_, err := persistPendingRecoveryClaim(server, linodes, claim)
	return err
}

func isPendingRecoveryClaim(claim core.LeaseClaim, leaseID string) bool {
	return claim.LeaseID == leaseID &&
		claim.Provider == providerName &&
		strings.TrimSpace(claim.CloudID) == "" &&
		claim.Labels["recovery"] == "ambiguous-create"
}

func validateLinodeClaimIdentity(claim core.LeaseClaim, leaseID, slug string) error {
	if claim.LeaseID != leaseID ||
		claim.Provider != providerName ||
		claim.Slug == "" ||
		(slug != "" && claim.Slug != slug) ||
		claim.Labels["lease"] != leaseID ||
		claim.Labels["slug"] != claim.Slug ||
		claim.Labels["provider"] != providerName {
		return core.Exit(2, "linode lease claim identity does not match lease=%s slug=%s", leaseID, slug)
	}
	return nil
}

func persistPendingRecoveryClaim(server core.Server, linodes []linodeInstance, claim core.LeaseClaim) (core.LeaseClaim, error) {
	matches := pendingRecoveryMatches(linodes, claim)
	if len(matches) > 1 {
		return core.LeaseClaim{}, core.Exit(2, "linode ambiguous create recovery found multiple instances for lease=%s", claim.LeaseID)
	}
	if len(matches) != 1 || matches[0].ID != server.ID {
		return core.LeaseClaim{}, core.Exit(2, "refusing to bind linode ambiguous create recovery to mismatched linode=%d lease=%s", server.ID, claim.LeaseID)
	}
	updated, err := core.UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(claim.LeaseID, claim, server, core.SSHTarget{})
	if err != nil {
		return core.LeaseClaim{}, fmt.Errorf("persist recovered linode instance: %w", err)
	}
	return updated, nil
}

func pendingRecoveryMatches(linodes []linodeInstance, claim core.LeaseClaim) []linodeInstance {
	name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
	matches := make([]linodeInstance, 0, 1)
	for _, item := range linodes {
		labels := labelsFromTags(item.Tags)
		if isOwnedLinode(item) &&
			item.Label == name &&
			labels["lease"] == claim.LeaseID &&
			labels["slug"] == claim.Slug {
			matches = append(matches, item)
		}
	}
	return matches
}

func appendLinodeIfMissing(linodes []linodeInstance, item linodeInstance) []linodeInstance {
	for _, existing := range linodes {
		if existing.ID == item.ID {
			return linodes
		}
	}
	return append(linodes, item)
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

func (b *linodeLeaseBackend) waitForLinodeIP(ctx context.Context, client linodeAPI, id int64) (linodeInstance, error) {
	deadline := b.now().Add(5 * time.Minute)
	for {
		item, err := client.GetLinode(ctx, id)
		if err != nil {
			return linodeInstance{}, err
		}
		if publicIPv4(item) != "" {
			return item, nil
		}
		if b.now().After(deadline) {
			return linodeInstance{}, core.Exit(5, "timed out waiting for Linode instance IP")
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return linodeInstance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *linodeLeaseBackend) now() time.Time {
	if b.RT.Clock != nil {
		return b.RT.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func rollbackLinodeAcquire(client linodeAPI, linodeID int64) error {
	if linodeID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.DeleteLinode(ctx, linodeID)
}

func validateLiveLinode(item linodeInstance, expected core.Server) error {
	labels := normalizedLinodeLabels(item.Tags)
	if err := validateLinodeLabels(labels); err != nil {
		return err
	}
	expectedProviderKey := expected.Labels["provider_key"]
	if expectedProviderKey == "" && expected.Labels["lease"] != "" {
		expectedProviderKey = providerKeyForLease(expected.Labels["lease"])
	}
	if item.ID != expected.ID ||
		item.Label != expected.Name ||
		labels["lease"] != expected.Labels["lease"] ||
		labels["slug"] != expected.Labels["slug"] ||
		labels["provider_key"] != expectedProviderKey {
		return core.Exit(2, "refusing to operate on changed Linode instance %d", expected.ID)
	}
	return nil
}

func serverFromLinode(item linodeInstance, cfg core.Config) core.Server {
	labels := normalizedLinodeLabels(item.Tags)
	server := core.Server{
		CloudID:  strconv.FormatInt(item.ID, 10),
		Provider: providerName,
		ID:       item.ID,
		Name:     item.Label,
		Status:   normalizeLinodeStatus(item.Status),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = publicIPv4(item)
	server.ServerType.Name = firstNonBlank(item.Type, cfg.ServerType)
	return server
}

func normalizedLinodeLabels(tags []string) map[string]string {
	labels := labelsFromTags(tags)
	if labels["provider_key"] == "" && labels["lease"] != "" {
		labels["provider_key"] = providerKeyForLease(labels["lease"])
	}
	return labels
}

func publicIPv4(item linodeInstance) string {
	for _, ip := range item.IPv4 {
		if strings.Contains(ip, ".") {
			return ip
		}
	}
	return ""
}

func normalizeLinodeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return "ready"
	case "":
		return "unknown"
	default:
		return status
	}
}

func parseLinodeID(id string) (int64, bool) {
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

func parseLinodeFirewallID(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, core.Exit(2, "linode firewall must be a positive numeric firewall ID")
	}
	return id, nil
}

func configureLinodeFirewall(req *createLinodeRequest, firewallID int64, interfaceSetting string) error {
	switch strings.TrimSpace(interfaceSetting) {
	case "legacy_config_only", "legacy_config_default_but_linode_allowed":
		req.InterfaceGeneration = "legacy_config"
		req.FirewallID = firewallID
	case "linode_default_but_legacy_config_allowed", "linode_only":
		req.InterfaceGeneration = "linode"
		req.Interfaces = []linodeInterface{{FirewallID: &firewallID, Public: &struct{}{}}}
	default:
		return core.Exit(3, "linode account returned unsupported interfaces_for_new_linodes value %q", interfaceSetting)
	}
	return nil
}

func generateLinodeRootPass() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "Cbx1!" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func providerKeyForLease(leaseID string) string {
	return "crabbox-" + leaseID
}

func applyLinodeDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.Linode.Region == "" {
		cfg.Linode.Region = defaultRegion
	}
	if cfg.Linode.Image == "" {
		cfg.Linode.Image = defaultImage
	}
	if cfg.Linode.Type == "" {
		cfg.Linode.Type = linodeServerTypeForClass(cfg.Class)
	}
	if !cfg.ServerTypeExplicit || cfg.ServerType == "" {
		cfg.ServerType = linodeServerTypeForClass(cfg.Class)
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

func isLinodeNotFound(err error) bool {
	var apiErr *linodeAPIError
	return errors.As(err, &apiErr) && apiErr.Status == 404
}

func isLinodeCreateAmbiguous(err error) bool {
	var apiErr *linodeAPIError
	if errors.As(err, &apiErr) && apiErr.Status >= 500 {
		return true
	}
	return !errors.As(err, &apiErr)
}
