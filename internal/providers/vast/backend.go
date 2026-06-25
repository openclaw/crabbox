package vast

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	vastPollInterval       = 3 * time.Second
	vastPollTimeout        = 10 * time.Minute
	vastCleanupTimeout     = 30 * time.Second
	vastKeyIDLabel         = "provider_key_id"
	vastKeyOwnedLabel      = "provider_key_owned"
	vastOfferIDLabel       = "vast_offer_id"
	vastReadyCheck         = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null"
	vastReleaseActionLabel = "release_action"
)

type backend struct {
	shared.DirectSSHBackend
	cfg            core.Config
	rt             core.Runtime
	apiFactory     func(core.Runtime) (vastAPI, error)
	waitSSH        func(context.Context, *core.SSHTarget, string, time.Duration) error
	sleep          func(context.Context, time.Duration) error
	pollTimeout    time.Duration
	cleanupTimeout time.Duration
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) *backend {
	applyVastDefaults(&cfg)
	b := &backend{cfg: cfg, rt: rt, pollTimeout: vastPollTimeout, cleanupTimeout: vastCleanupTimeout}
	b.DirectSSHBackend = shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, Delete: b.deleteServer, StoredLeaseKeys: true}
	b.apiFactory = func(rt core.Runtime) (vastAPI, error) { return newVastClient(cfg.Vast, rt) }
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.stderr(), phase, timeout)
	}
	b.sleep = func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	return b
}

func applyVastDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if core.IsSSHUserExplicit(cfg) {
		// The generic SSH user is the operator-facing override. Vast.User only
		// provides the provider default when that override was not used.
	} else if cfg.Vast.User != "" {
		cfg.SSHUser = cfg.Vast.User
	} else if cfg.SSHUser == "" {
		cfg.SSHUser = "root"
	}
	if cfg.Vast.WorkRoot != "" {
		cfg.WorkRoot = cfg.Vast.WorkRoot
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = "/work/crabbox"
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	if cfg.Vast.InstanceType == "" {
		cfg.Vast.InstanceType = "ondemand"
	}
	if cfg.Vast.Runtype == "" {
		cfg.Vast.Runtype = "ssh_direct"
	}
	if cfg.Vast.Order == "" {
		cfg.Vast.Order = "dlperf_per_dphtotal desc"
	}
	if cfg.Vast.ReleaseAction == "" {
		cfg.Vast.ReleaseAction = "destroy"
	}
}

func (b *backend) stderr() io.Writer {
	if b.rt.Stderr != nil {
		return b.rt.Stderr
	}
	return io.Discard
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *backend) api() (vastAPI, error) {
	if b.apiFactory != nil {
		return b.apiFactory(b.rt)
	}
	return newVastClient(b.cfg.Vast, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := b.api()
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.CheckAuth(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	count := 0
	for _, item := range instances {
		if isOwnedVastInstance(item) {
			count++
		}
	}
	result := core.InventoryDoctorResult(providerName, count)
	result.Message += fmt.Sprintf(" default_order=%s runtype=%s user=%s", b.cfg.Vast.Order, b.cfg.Vast.Runtype, b.cfg.SSHUser)
	return result, nil
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.rt, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *backend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	if b.cfg.TargetOS != "" && b.cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, exit(2, "provider=%s supports target=linux only", providerName)
	}
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := serversFromInstances(instances, b.cfg, false)
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(b.cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.cfg
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	now := b.now()
	label := encodeVastOwnershipLabel(leaseID, slug, "provisioning")
	var (
		instanceID int
		keyID      string
		committed  bool
	)
	defer func() {
		if err == nil || committed {
			return
		}
		if instanceID == 0 {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		_ = b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, instanceID, keyID, "rollback-cleanup", req.Keep, now)
		if !req.Keep {
			cleanupErr := rollbackVastAcquire(client, instanceID, keyID)
			if cleanupErr != nil {
				err = fmt.Errorf("%v; vast cleanup failed: %w", err, cleanupErr)
				return
			}
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	offers, err := client.SearchOffers(ctx, vastOfferSearchInput{Config: cfg.Vast})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	offer, err := selectVastOffer(offers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.stderr(), "provisioning provider=vast lease=%s slug=%s offer=%d gpu=%s count=%d max_dph=%.4f keep=%v\n", leaseID, slug, offer.ID, offer.GPUName, offer.GPUCount, cfg.Vast.MaxDphTotal, req.Keep)
	created, err := client.CreateInstance(ctx, offer.ID, vastCreateInstanceInput{
		Config:      cfg.Vast,
		Label:       label,
		SSHKey:      publicKey,
		Environment: map[string]string{"CRABBOX": "1"},
	})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instanceID = firstNonZero(created.Instance.ID, created.NewContract)
	if instanceID == 0 {
		err = exit(5, "vast create returned no instance id")
		return core.LeaseTarget{}, err
	}
	if attach, attachErr := client.AttachInstanceSSHKey(ctx, instanceID, publicKey); attachErr != nil {
		err = attachErr
		return core.LeaseTarget{}, err
	} else {
		keyID = vastAttachedKeyID(attach)
	}
	instance, err := b.waitForInstanceReady(ctx, client, instanceID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	readyLabel := encodeVastOwnershipLabel(leaseID, slug, "ready")
	if _, err := client.ManageInstance(ctx, instanceID, vastManageInstanceInput{Label: readyLabel}); err != nil {
		return core.LeaseTarget{}, err
	}
	instance.Label = readyLabel
	server := serverFromInstance(instance, cfg)
	server.Labels = vastLeaseLabels(cfg, leaseID, slug, "ready", req.Keep, now)
	server.Labels[vastOfferIDLabel] = strconv.Itoa(offer.ID)
	if keyID != "" {
		server.Labels[vastKeyIDLabel] = keyID
	}
	server.Labels[vastKeyOwnedLabel] = fmt.Sprint(keyID != "")
	ssh, err := sshTargetFromInstance(cfg, instance)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.waitSSH(ctx, &ssh, "vast bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	target = core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(target); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.stderr(), "provisioned lease=%s vast=%d gpu=%s state=ready\n", leaseID, instanceID, server.ServerType.Name)
	return target, nil
}

func selectVastOffer(offers []vastOffer) (vastOffer, error) {
	for _, offer := range offers {
		if offer.ID != 0 && offer.Rentable && !offer.Rented {
			return offer, nil
		}
	}
	if len(offers) > 0 && offers[0].ID != 0 {
		return offers[0], nil
	}
	return vastOffer{}, exit(4, "vast found no eligible offers")
}

func vastAttachedKeyID(resp vastAttachSSHKeyResponse) string {
	if strings.TrimSpace(resp.Key.ID) != "" {
		return strings.TrimSpace(resp.Key.ID)
	}
	for _, key := range resp.Keys {
		if strings.TrimSpace(key.ID) != "" {
			return strings.TrimSpace(key.ID)
		}
	}
	return ""
}

func (b *backend) waitForInstanceReady(ctx context.Context, client vastAPI, id int) (vastInstance, error) {
	deadline := b.now().Add(b.pollTimeout)
	for {
		instance, err := client.GetInstance(ctx, id)
		if err != nil {
			return vastInstance{}, err
		}
		if isVastInstanceRunning(instance) && strings.TrimSpace(instance.SSHHost) != "" && instance.SSHPort > 0 {
			return instance, nil
		}
		if isTerminalVastStatus(instance.Status) {
			return vastInstance{}, exit(5, "vast instance %d reached terminal status %s", id, instance.Status)
		}
		if b.now().After(deadline) {
			return vastInstance{}, exit(5, "timed out waiting for Vast instance %d to expose SSH", id)
		}
		if err := b.sleep(ctx, vastPollInterval); err != nil {
			return vastInstance{}, err
		}
	}
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	byID := map[int]vastInstance{}
	for _, item := range instances {
		byID[item.ID] = item
	}
	servers := serversFromInstances(instances, b.cfg, false)
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		if id, ok := parseVastInstanceID(server.CloudID); ok {
			return b.targetFromInstance(byID[id], req)
		}
	}
	if claim, ok, claimErr := core.ResolveLeaseClaimForProvider(req.ID, providerName); claimErr != nil {
		return core.LeaseTarget{}, claimErr
	} else if ok {
		if id, parseOK := parseVastInstanceID(claim.CloudID); parseOK {
			item, getErr := client.GetInstance(ctx, id)
			if getErr == nil {
				return b.targetFromInstance(item, req)
			}
			if req.ReleaseOnly {
				return claimTarget(claim), nil
			}
			return core.LeaseTarget{}, getErr
		}
		if req.ReleaseOnly {
			return claimTarget(claim), nil
		}
	}
	if id, ok := parseVastInstanceID(req.ID); ok {
		item, found := byID[id]
		if !found {
			item, err = client.GetInstance(ctx, id)
			if err != nil {
				return b.releaseTargetFromClaim(req.ID, err, req.ReleaseOnly)
			}
		}
		return b.targetFromInstance(item, req)
	}
	return core.LeaseTarget{}, exit(4, "lease/instance not found: %s", req.ID)
}

func (b *backend) releaseTargetFromClaim(id string, cause error, releaseOnly bool) (core.LeaseTarget, error) {
	if !releaseOnly {
		return core.LeaseTarget{}, cause
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(id, providerName)
	if err != nil || !ok {
		if err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{}, cause
	}
	return claimTarget(claim), nil
}

func (b *backend) targetFromInstance(item vastInstance, req core.ResolveRequest) (core.LeaseTarget, error) {
	if !isOwnedVastInstance(item) {
		return core.LeaseTarget{}, exit(2, "refusing to operate on non-Crabbox Vast instance %d", item.ID)
	}
	if isTerminalVastStatus(item.Status) && !req.ReleaseOnly && !req.StatusOnly {
		return core.LeaseTarget{}, exit(5, "vast instance %d reached terminal status %s", item.ID, item.Status)
	}
	server := serverFromInstance(item, b.cfg)
	server = mergeVastClaimMetadata(server)
	leaseID := server.Labels["lease"]
	target := core.LeaseTarget{Server: server, LeaseID: leaseID}
	if !req.ReleaseOnly && (!req.StatusOnly || req.ReadyProbe) {
		ssh, err := sshTargetFromInstance(b.cfg, item)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		core.UseStoredTestboxKey(&ssh, leaseID)
		target.SSH = ssh
	}
	if req.Repo.Root != "" && !req.NoLocalStateMutations {
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, server.Labels["slug"], b.cfg, target.Server, target.SSH, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return target, nil
}

func (b *backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	return serversFromInstances(instances, b.cfg, req.All), nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	return b.deleteServer(ctx, b.cfg, req.Lease.Server)
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	action := effectiveVastReleaseAction(b.cfg, lease.Server.Labels)
	if action == "stop" || action == "keep" {
		return fmt.Sprintf("%s lease=%s vast=%s name=%s", action, lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
	}
	return fmt.Sprintf("destroyed lease=%s vast=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if err := validateVastServer(server); err != nil {
		return core.Server{}, err
	}
	cfg := b.cfg
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, b.now())
	if claim, ok, err := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID); err == nil && ok {
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(req.Lease.LeaseID, claim, server.Labels); err != nil {
			return core.Server{}, err
		}
	}
	return server, nil
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	servers, err = b.prepareCleanupServers(servers)
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *backend) prepareCleanupServers(servers []core.Server) ([]core.Server, error) {
	for i := range servers {
		updated, err := b.prepareCleanupServer(servers[i])
		if err != nil {
			return nil, err
		}
		servers[i] = updated
	}
	return servers, nil
}

func (b *backend) prepareCleanupServer(server core.Server) (core.Server, error) {
	if server.Provider != providerName {
		return server, nil
	}
	leaseID := strings.TrimSpace(server.Labels["lease"])
	if leaseID == "" {
		return server, nil
	}
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.Server{}, fmt.Errorf("read vast cleanup claim: %w", err)
	}
	if claimExists && claim.Provider == providerName && (claim.CloudID == "" || server.CloudID == "" || claim.CloudID == server.CloudID) {
		return server, nil
	}

	labels := cloneLabels(server.Labels)
	labels["state"] = "expired"
	labels["expires_at"] = core.LeaseLabelTime(b.now().Add(-time.Second))
	server.Labels = labels
	return server, nil
}

func (b *backend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if err := validateVastServer(server); err != nil {
		return err
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return fmt.Errorf("read vast cleanup claim: %w", err)
	}
	if !claimExists {
		return exit(2, "lease=%s has no local Vast claim; refusing destructive cleanup", leaseID)
	}
	if claim.Provider != providerName {
		return exit(2, "lease=%s is claimed by provider=%s; refusing Vast cleanup", leaseID, claim.Provider)
	}
	if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
		return exit(2, "refusing to release Vast instance %s from stale local claim", server.CloudID)
	}
	instanceID, ok := parseVastInstanceID(firstNonBlank(server.CloudID, claim.CloudID))
	if !ok {
		return exit(2, "provider=%s release requires a Vast instance id", providerName)
	}
	if live, getErr := client.GetInstance(ctx, instanceID); getErr == nil {
		if err := validateLiveVastInstance(live, server); err != nil {
			return err
		}
	} else if !isVastNotFound(getErr) {
		return getErr
	}
	action := effectiveVastReleaseAction(b.cfg, claim.Labels)
	switch action {
	case "keep":
		return nil
	case "stop":
		if _, err := client.ManageInstance(ctx, instanceID, vastManageInstanceInput{State: "stopped", Label: encodeVastOwnershipLabel(leaseID, server.Labels["slug"], "stopped")}); err != nil {
			return err
		}
		labels := cloneLabels(claim.Labels)
		labels["state"] = "stopped"
		labels[vastReleaseActionLabel] = "stop"
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
			return fmt.Errorf("finalize vast stop claim: %w", err)
		}
	default:
		if keyID := strings.TrimSpace(claim.Labels[vastKeyIDLabel]); keyID != "" && claim.Labels[vastKeyOwnedLabel] == "true" {
			if err := client.DetachInstanceSSHKey(ctx, instanceID, keyID); err != nil && !isVastNotFound(err) {
				return err
			}
		}
		if err := client.DestroyInstance(ctx, instanceID); err != nil && !isVastNotFound(err) {
			return err
		}
		if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return fmt.Errorf("finalize vast cleanup claim: %w", err)
		}
		core.RemoveStoredTestboxKey(leaseID)
	}
	return nil
}

func serversFromInstances(instances []vastInstance, cfg core.Config, includeAll bool) []core.Server {
	out := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		if !includeAll && !isOwnedVastInstance(item) {
			continue
		}
		server := serverFromInstance(item, cfg)
		if isOwnedVastInstance(item) {
			server = mergeVastClaimLabels(server)
		}
		out = append(out, server)
	}
	return out
}

func mergeVastClaimLabels(server core.Server) core.Server {
	leaseID := strings.TrimSpace(server.Labels["lease"])
	if leaseID == "" {
		return server
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !ok || claim.Provider != providerName {
		return server
	}
	if claim.CloudID != "" && claim.CloudID != server.CloudID {
		return server
	}
	if len(claim.Labels) > 0 {
		server.Labels = claim.Labels
	}
	return server
}

func mergeVastClaimMetadata(server core.Server) core.Server {
	leaseID := strings.TrimSpace(server.Labels["lease"])
	if leaseID == "" {
		return server
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !ok || claim.Provider != providerName {
		return server
	}
	if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
		return server
	}
	server.Labels = preserveVastClaimMetadata(server.Labels, claim.Labels)
	return server
}

func serverFromInstance(item vastInstance, cfg core.Config) core.Server {
	labels := labelsFromVastInstance(item, cfg)
	server := core.Server{
		CloudID:  strconv.Itoa(item.ID),
		Provider: providerName,
		Name:     firstNonBlank(labels["slug"], item.Label, strconv.Itoa(item.ID)),
		Status:   normalizeVastStatus(item.Status),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = strings.TrimSpace(item.SSHHost)
	server.ServerType.Name = firstNonBlank(item.GPUName, cfg.ServerType)
	return server
}

func labelsFromVastInstance(item vastInstance, cfg core.Config) map[string]string {
	if owner, ok := decodeVastOwnershipLabel(item.Label); ok {
		labels := vastLeaseLabels(cfg, owner.LeaseID, owner.Slug, owner.State, false, time.Now().UTC())
		labels["provider_key"] = core.ProviderKeyForLease(owner.LeaseID)
		labels[vastReleaseActionLabel] = normalizeVastReleaseAction(cfg.Vast.ReleaseAction)
		return labels
	}
	return map[string]string{"label": strings.TrimSpace(item.Label)}
}

func vastLeaseLabels(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) map[string]string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	labels[vastReleaseActionLabel] = normalizeVastReleaseAction(cfg.Vast.ReleaseAction)
	return labels
}

func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func preserveVastClaimMetadata(labels, existing map[string]string) map[string]string {
	out := cloneLabels(labels)
	for _, key := range []string{
		vastReleaseActionLabel,
		vastKeyIDLabel,
		vastKeyOwnedLabel,
		vastOfferIDLabel,
		"provider_key",
		"recovery",
	} {
		if value, ok := existing[key]; ok {
			out[key] = value
		}
	}
	return out
}

func isOwnedVastInstance(item vastInstance) bool {
	return isVastCrabboxOwnedLabel(item.Label)
}

func validateVastServer(server core.Server) error {
	if server.Provider != "" && server.Provider != providerName {
		return exit(2, "refusing to operate on provider=%s server as Vast", server.Provider)
	}
	leaseID := strings.TrimSpace(server.Labels["lease"])
	if leaseID == "" || strings.TrimSpace(server.Labels["slug"]) == "" {
		return exit(2, "refusing to operate on non-Crabbox Vast instance %s", server.DisplayID())
	}
	return nil
}

func validateLiveVastInstance(item vastInstance, expected core.Server) error {
	if !isOwnedVastInstance(item) {
		return exit(2, "refusing to operate on non-Crabbox Vast instance %d", item.ID)
	}
	owner, _ := decodeVastOwnershipLabel(item.Label)
	if strconv.Itoa(item.ID) != expected.CloudID ||
		owner.LeaseID != expected.Labels["lease"] ||
		owner.Slug != expected.Labels["slug"] {
		return exit(2, "refusing to operate on changed Vast instance %s", expected.CloudID)
	}
	return nil
}

func sshTargetFromInstance(cfg core.Config, item vastInstance) (core.SSHTarget, error) {
	host := strings.TrimSpace(item.SSHHost)
	if host == "" || item.SSHPort <= 0 {
		return core.SSHTarget{}, exit(5, "vast instance %d is missing SSH endpoint", item.ID)
	}
	ssh := core.SSHTargetFromConfig(cfg, host)
	ssh.Port = strconv.Itoa(item.SSHPort)
	ssh.User = firstNonBlank(cfg.SSHUser, cfg.Vast.User, "root")
	ssh.TargetOS = core.TargetLinux
	ssh.ReadyCheck = vastReadyCheck
	return ssh, nil
}

func isVastInstanceRunning(item vastInstance) bool {
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "running", "active", "ready":
		return true
	default:
		return false
	}
}

func isTerminalVastStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "exited", "cancelled", "canceled", "destroyed", "deleted", "dead":
		return true
	default:
		return false
	}
}

func normalizeVastStatus(status string) string {
	if isVastInstanceRunning(vastInstance{Status: status}) {
		return "ready"
	}
	if status = strings.TrimSpace(status); status != "" {
		return status
	}
	return "unknown"
}

func normalizeVastReleaseAction(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "stop":
		return "stop"
	case "keep":
		return "keep"
	default:
		return "destroy"
	}
}

func effectiveVastReleaseAction(cfg core.Config, labels map[string]string) string {
	if core.DeleteOnReleaseExplicit(cfg, providerName) {
		return normalizeVastReleaseAction(cfg.Vast.ReleaseAction)
	}
	return normalizeVastReleaseAction(firstNonBlank(labels[vastReleaseActionLabel], cfg.Vast.ReleaseAction))
}

func parseVastInstanceID(value string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	return id, err == nil && id > 0
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func claimTarget(claim core.LeaseClaim) core.LeaseTarget {
	server := core.Server{
		CloudID:  claim.CloudID,
		Provider: providerName,
		Name:     claim.Slug,
		Status:   claim.Labels["state"],
		Labels:   claim.Labels,
	}
	server.PublicNet.IPv4.IP = claim.SSHHost
	target := core.SSHTarget{Host: claim.SSHHost, Port: strconv.Itoa(claim.SSHPort), TargetOS: core.TargetLinux}
	core.UseStoredTestboxKey(&target, claim.LeaseID)
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server, SSH: target}
}

func (b *backend) persistRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot string, instanceID int, keyID, reason string, keep bool, now time.Time) error {
	label := encodeVastOwnershipLabel(leaseID, slug, reason)
	server := serverFromInstance(vastInstance{ID: instanceID, Label: label, Status: reason}, cfg)
	server.Labels = vastLeaseLabels(cfg, leaseID, slug, reason, keep, now)
	server.Labels["recovery"] = reason
	if keyID != "" {
		server.Labels[vastKeyIDLabel] = keyID
		server.Labels[vastKeyOwnedLabel] = "true"
	}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, true)
}

func rollbackVastAcquire(client vastAPI, instanceID int, keyID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), vastCleanupTimeout)
	defer cancel()
	var errs []error
	if keyID != "" {
		if err := client.DetachInstanceSSHKey(ctx, instanceID, keyID); err != nil && !isVastNotFound(err) {
			errs = append(errs, err)
		}
	}
	if instanceID != 0 {
		if err := client.DestroyInstance(ctx, instanceID); err != nil && !isVastNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isVastNotFound(err error) bool {
	var apiErr *vastAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}
