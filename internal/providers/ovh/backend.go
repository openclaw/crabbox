package ovh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Backend struct {
	shared.DirectSSHBackend
	clientFactory            func(core.Config, core.Runtime) (API, error)
	waitSSH                  func(context.Context, *core.SSHTarget, string, time.Duration) error
	ipWaitTimeout            time.Duration
	ipWaitInterval           time.Duration
	recoveryGrace            time.Duration
	recoveryPolls            int
	recoveryInterval         time.Duration
	beforeTouchClaimUpdate   func()
	beforeCleanupClaimUpdate func()
}

const (
	ovhProjectLabel              = "ovh_project"
	ovhRegionLabel               = "ovh_region"
	ovhSSHKeyIDLabel             = "ovh_ssh_key_id"
	ovhSSHKeyOwnedLabel          = "ovh_ssh_key_owned"
	ambiguousCreateRecoveryGrace = 2 * time.Minute
	defaultRecoveryPolls         = 3
	defaultRecoveryInterval      = 2 * time.Second
)

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) *Backend {
	cfg.Provider = providerName
	b := &Backend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt},
	}
	b.clientFactory = func(cfg core.Config, rt core.Runtime) (API, error) {
		return newClient(cfg, rt)
	}
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.RT.Stderr, phase, timeout)
	}
	b.Delete = b.deleteServer
	return b
}

func (b *Backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.Cfg
	if strings.TrimSpace(cfg.OVH.ProjectID) == "" {
		return doctorConfigFailure("CRABBOX_OVH_PROJECT_ID or ovh.projectId is required"), nil
	}
	client, err := b.clientFactory(cfg, b.RT)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.AuthTime(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	regions, err := client.ListRegions(ctx, cfg.OVH.ProjectID)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cfg.OVH.Region != "" && !regionExists(regions, cfg.OVH.Region) {
		return doctorCheckFailure("region", fmt.Sprintf("OVH region %q was not returned by the project", cfg.OVH.Region)), nil
	}
	flavors, err := client.ListFlavors(ctx, cfg.OVH.ProjectID, cfg.OVH.Region)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cfg.OVH.Flavor != "" && !flavorExists(flavors, cfg.OVH.Flavor) {
		return doctorCheckFailure("flavor", fmt.Sprintf("OVH flavor %q was not returned by the project", cfg.OVH.Flavor)), nil
	}
	images, err := client.ListImages(ctx, cfg.OVH.ProjectID, cfg.OVH.Region)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cfg.OVH.Image != "" && !imageExists(images, cfg.OVH.Image) {
		return doctorCheckFailure("image", fmt.Sprintf("OVH image %q was not returned by the project", cfg.OVH.Image)), nil
	}
	instances, err := client.ListInstances(ctx, cfg.OVH.ProjectID)
	if err != nil {
		return core.DoctorResult{}, err
	}
	leases := 0
	for _, instance := range instances {
		if isCrabboxInstance(instance) {
			leases++
		}
	}
	result := core.InventoryDoctorResult(providerName, leases)
	result.Message += fmt.Sprintf(" endpoint=%s region=%s image=%s flavor=%s", redactedEndpoint(cfg.OVH.Endpoint), blank(cfg.OVH.Region), blank(cfg.OVH.Image), blank(cfg.OVH.Flavor))
	result.Checks = []core.DoctorCheck{
		{Status: "passed", Check: "auth", Message: "OVH credentials accepted", Details: map[string]string{"mutation": "false"}},
		{Status: "passed", Check: "inventory", Message: fmt.Sprintf("found %d Crabbox-owned OVH instances", leases), Details: map[string]string{"mutation": "false"}},
	}
	return result, nil
}

func (b *Backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *Backend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	cfg, err := b.resolveAcquireConfig(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=ovh only supports target=linux")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(cfg, b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	instances, err := client.ListInstances(ctx, cfg.OVH.ProjectID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	seenCloudIDs := map[string]bool{}
	claimed, err := b.claimedLiveServers(instances)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	for _, item := range claimed {
		servers = append(servers, item.server)
		seenCloudIDs[item.server.CloudID] = true
	}
	for _, instance := range instances {
		if isOwnedInstance(instance) && !seenCloudIDs[instance.ID] {
			server := serverFromInstance(instance, cfg)
			servers = append(servers, server)
			seenCloudIDs[server.CloudID] = true
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
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := b.now()
	labels := ovhLeaseLabels(cfg, leaseID, slug, req.Keep, now, "provisioning")
	committed := false
	recovery := ""
	var created Instance
	var createdKey SSHKey
	keyCreated := false
	defer func() {
		if err == nil || committed {
			return
		}
		if recovery != "" {
			if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, labels, recovery, created, createdKey, keyCreated, req.Reclaim); claimErr != nil {
				err = errors.Join(err, fmt.Errorf("persist ovh recovery claim: %w", claimErr))
			}
			return
		}
		if created.ID == "" && createdKey.ID == "" {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		recoveryPersisted := false
		if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, labels, "rollback-cleanup", created, createdKey, keyCreated, req.Reclaim); claimErr == nil {
			recoveryPersisted = true
		} else {
			err = errors.Join(err, fmt.Errorf("persist ovh recovery claim: %w", claimErr))
		}
		if cleanupErr := rollbackOVHAcquire(client, cfg.OVH.ProjectID, created.ID, createdKey.ID, keyCreated); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("ovh cleanup failed: %w", cleanupErr))
			return
		}
		if recoveryPersisted {
			core.RemoveLeaseClaim(leaseID)
		}
		core.RemoveStoredTestboxKey(leaseID)
	}()
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=ovh lease=%s slug=%s flavor=%s region=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, cfg.OVH.Region, cfg.OVH.Image, req.Keep)
	createdKey, err = client.CreateSSHKey(ctx, cfg.OVH.ProjectID, cfg.ProviderKey, publicKey)
	if err != nil {
		if isDeterminateCreateError(err) {
			return core.LeaseTarget{}, err
		}
		reconciled, found, reconcileErr := b.reconcileSSHKey(ctx, client, cfg.OVH.ProjectID, cfg.ProviderKey, publicKey)
		if reconcileErr != nil || !found {
			recovery = "ambiguous-key-create"
			if reconcileErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("reconcile ovh SSH-key create: %w", reconcileErr))
			}
			return core.LeaseTarget{}, errors.Join(err, core.Exit(4, "ovh SSH-key create remains indeterminate for lease=%s", leaseID))
		}
		createdKey = reconciled
	}
	keyCreated = true
	labels[ovhSSHKeyIDLabel] = createdKey.ID
	labels[ovhSSHKeyOwnedLabel] = "true"
	created, err = client.CreateInstance(ctx, cfg.OVH.ProjectID, InstanceCreateRequest{
		Name:     core.LeaseProviderName(leaseID, slug),
		Region:   cfg.OVH.Region,
		FlavorID: cfg.ServerType,
		ImageID:  cfg.OVH.Image,
		SSHKeyID: createdKey.ID,
		UserData: core.CloudInitUserData(cfg, publicKey),
	})
	if err != nil {
		if !isDeterminateCreateError(err) {
			recovery = "ambiguous-create"
		}
		if recovered, ok, findErr := findCreatedInstance(ctx, client, cfg.OVH.ProjectID, leaseID, slug, labels); findErr != nil {
			return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("reconcile ovh create recovery: %w", findErr))
		} else if ok {
			recovery = "ambiguous-create"
			created = mergeInstanceLabels(recovered, labels)
			created.SSHKeyID = firstNonBlank(created.SSHKeyID, createdKey.ID)
		}
		return core.LeaseTarget{}, err
	}
	created = mergeInstanceLabels(created, labels)
	waited, err := b.waitForInstanceIP(ctx, client, cfg.OVH.ProjectID, created.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	waited = mergeInstanceLabels(waited, labels)
	waited.SSHKeyID = firstNonBlank(waited.SSHKeyID, createdKey.ID)
	server := serverFromInstance(waited, cfg)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "ovh bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", now)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s ovh_instance=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *Backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := client.ListInstances(ctx, b.Cfg.OVH.ProjectID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	byCloudID := map[string]Instance{}
	liveClaims, err := b.claimedLiveServers(instances)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	for _, claimed := range liveClaims {
		instance := mergeInstanceLabels(claimed.instance, claimed.server.Labels)
		servers = append(servers, claimed.server)
		byCloudID[instance.ID] = instance
	}
	if instance, ok := byCloudID[req.ID]; ok {
		return b.targetFromInstance(instance, req)
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID != "" {
		return b.targetFromInstance(byCloudID[server.CloudID], req)
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID)
	}
	instance, err := client.GetInstance(ctx, b.Cfg.OVH.ProjectID, req.ID)
	if err == nil {
		return b.targetFromInstance(instance, req)
	}
	if !isOVHNotFound(err) {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/ovh instance not found: %s", req.ID)
}

func (b *Backend) targetFromInstance(instance Instance, req core.ResolveRequest) (core.LeaseTarget, error) {
	server := serverFromInstance(instance, b.Cfg)
	if err := validateOVHServerOwnership(server); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := server.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("read ovh lease claim: %w", err)
	}
	if claimExists {
		if err := validateOVHClaim(claim, server); err != nil {
			return core.LeaseTarget{}, err
		}
		server = overlayClaimLabels(server, claim)
	} else if req.ReleaseOnly {
		return core.LeaseTarget{}, core.Exit(2, "refusing to release OVH instance %s without a local Crabbox claim", server.DisplayID())
	}
	if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
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

func (b *Backend) releaseTargetFromClaim(ctx context.Context, client API, id string) (core.LeaseTarget, error) {
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(id, providerName)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if exact && (!ok || claim.LeaseID != id) {
		return core.LeaseTarget{}, core.Exit(2, "ovh exact lease identifier %q does not match a valid ovh claim", id)
	}
	if !ok || claim.LeaseID == "" {
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if !ok || claim.LeaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "lease/ovh instance not found: %s", id)
	}
	if !core.LeaseClaimMatchesIdentifier(claim, id) {
		return core.LeaseTarget{}, core.Exit(2, "ovh lease claim does not match requested identifier %q", id)
	}
	if claim.Labels[ovhProjectLabel] != "" && claim.Labels[ovhProjectLabel] != b.Cfg.OVH.ProjectID {
		return core.LeaseTarget{}, core.Exit(3, "ovh project mismatch: current project %s does not match lease project %s", b.Cfg.OVH.ProjectID, claim.Labels[ovhProjectLabel])
	}
	if claim.CloudID == "" {
		instance, found, err := findCreatedInstance(ctx, client, b.Cfg.OVH.ProjectID, claim.LeaseID, claim.Slug, claim.Labels)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if found {
			server := overlayClaimLabels(serverFromInstance(instance, b.Cfg), claim)
			if err := validateOVHClaim(claim, server); err != nil {
				return core.LeaseTarget{}, err
			}
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
		}
		switch claim.Labels["recovery"] {
		case "ambiguous-key-create":
			target, found, err := b.reconcilePendingSSHKey(ctx, client, claim)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if found {
				return target, nil
			}
			return core.LeaseTarget{}, core.Exit(4, "ovh ambiguous SSH-key create remains indeterminate for lease=%s; credentials and recovery claim retained", claim.LeaseID)
		case "ambiguous-create":
			if b.recoveryStillPending(claim) {
				return core.LeaseTarget{}, core.Exit(4, "ovh ambiguous instance create recovery is still pending for lease=%s; retry stop later", claim.LeaseID)
			}
			instance, found, err := b.reconcileCreatedInstance(ctx, client, claim)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if found {
				server := overlayClaimLabels(serverFromInstance(instance, b.Cfg), claim)
				if err := validateOVHClaim(claim, server); err != nil {
					return core.LeaseTarget{}, err
				}
				return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
			}
			return core.LeaseTarget{}, core.Exit(4, "ovh ambiguous instance create remains indeterminate for lease=%s; credentials and recovery claim retained", claim.LeaseID)
		case "rollback-cleanup":
			return core.LeaseTarget{
				LeaseID: claim.LeaseID,
				Server:  claimOnlyServer(claim),
			}, nil
		default:
			return core.LeaseTarget{}, core.Exit(2, "ovh lease claim has invalid recovery state %q for lease=%s", claim.Labels["recovery"], claim.LeaseID)
		}
	}
	instance, err := client.GetInstance(ctx, b.Cfg.OVH.ProjectID, claim.CloudID)
	if err == nil {
		server := overlayClaimLabels(serverFromInstance(instance, b.Cfg), claim)
		if err := validateOVHClaim(claim, server); err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	}
	if !isOVHNotFound(err) {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{
		LeaseID: claim.LeaseID,
		Server:  core.Server{Provider: providerName, CloudID: claim.CloudID, Name: claim.Slug, Labels: claim.Labels},
	}, nil
}

func (b *Backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return nil, err
	}
	instances, err := client.ListInstances(ctx, b.Cfg.OVH.ProjectID)
	if err != nil {
		return nil, err
	}
	claimed, err := b.claimedLiveServers(instances)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(claimed))
	for _, item := range claimed {
		out = append(out, item.server)
	}
	return out, nil
}

func (b *Backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	return b.deleteServer(ctx, b.Cfg, req.Lease.Server)
}

func (b *Backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s ovh_instance=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *Backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if err := validateOVHServerOwnership(server); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return core.Server{}, err
	}
	instance, err := client.GetInstance(ctx, b.Cfg.OVH.ProjectID, server.CloudID)
	if err != nil {
		return core.Server{}, err
	}
	live := overlayExpectedOVHLabels(serverFromInstance(instance, b.Cfg), server)
	if err := validateLiveOVHInstance(live, server); err != nil {
		return core.Server{}, err
	}
	labels := copyLabels(server.Labels)
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
	if err != nil {
		return core.Server{}, err
	}
	if claimExists {
		if err := validateOVHClaim(claim, live); err != nil {
			return core.Server{}, err
		}
		if claim.Labels["state"] == "cleanup" {
			return core.Server{}, core.Exit(4, "ovh lease=%s cleanup is already in progress", claim.LeaseID)
		}
		labels = copyLabels(claim.Labels)
	}
	cfg := b.Cfg
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	tailscaleLabels := exactTailscaleLabels(labels)
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.now())
	for key, value := range tailscaleLabels {
		labels[key] = value
	}
	if claimExists {
		if b.beforeTouchClaimUpdate != nil {
			b.beforeTouchClaimUpdate()
		}
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(labels["lease"], claim, labels); err != nil {
			return core.Server{}, err
		}
	}
	live.Labels = labels
	return live, nil
}

func (b *Backend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	server := lease.Server
	if err := validateOVHServerOwnership(server); err != nil {
		return core.Server{}, err
	}
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return core.Server{}, err
	}
	instance, err := client.GetInstance(ctx, b.Cfg.OVH.ProjectID, server.CloudID)
	if err != nil {
		return core.Server{}, err
	}
	live := overlayExpectedOVHLabels(serverFromInstance(instance, b.Cfg), server)
	if err := validateLiveOVHInstance(live, server); err != nil {
		return core.Server{}, err
	}
	labels := copyLabels(server.Labels)
	if claim, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"]); err != nil {
		return core.Server{}, err
	} else if exists {
		if err := validateOVHClaim(claim, live); err != nil {
			return core.Server{}, err
		}
		if claim.Labels["state"] == "cleanup" {
			return core.Server{}, core.Exit(4, "ovh lease=%s cleanup is already in progress", claim.LeaseID)
		}
		labels = copyLabels(claim.Labels)
		applyTailscaleMetadata(labels, meta)
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
			return core.Server{}, err
		}
	} else {
		applyTailscaleMetadata(labels, meta)
	}
	live.Labels = labels
	return live, nil
}

func (b *Backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return err
	}
	instances, err := client.ListInstances(ctx, b.Cfg.OVH.ProjectID)
	if err != nil {
		return err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return fmt.Errorf("list ovh cleanup claims: %w", err)
	}
	now := b.now()
	for _, claim := range claims {
		if claim.Provider != providerName || claim.Slug == "" || !claimMatchesOVHProject(claim, b.Cfg.OVH.ProjectID) {
			continue
		}
		instance, found, err := findClaimedInstance(instances, claim)
		if err != nil {
			return err
		}
		server := claimOnlyServer(claim)
		if found {
			server = overlayClaimLabels(serverFromInstance(instance, b.Cfg), claim)
		}
		if err := validateOVHClaim(claim, server); err != nil {
			return err
		}
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if b.beforeCleanupClaimUpdate != nil {
			b.beforeCleanupClaimUpdate()
		}
		labels := copyLabels(claim.Labels)
		labels["state"] = "cleanup"
		transitioned, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
		if err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=claim changed during cleanup\n", server.DisplayID(), server.Name)
			continue
		}
		server.Labels = copyLabels(transitioned.Labels)
		if err := b.deleteServer(ctx, b.Cfg, server); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) deleteServer(ctx context.Context, _ core.Config, server core.Server) error {
	if err := validateOVHServerOwnership(server); err != nil {
		return err
	}
	client, err := b.clientFactory(b.Cfg, b.RT)
	if err != nil {
		return err
	}
	if server.CloudID != "" {
		instance, err := client.GetInstance(ctx, b.Cfg.OVH.ProjectID, server.CloudID)
		if err == nil {
			live := overlayExpectedOVHLabels(serverFromInstance(instance, b.Cfg), server)
			if err := validateLiveOVHInstance(live, server); err != nil {
				return err
			}
			server = live
		} else if !isOVHNotFound(err) {
			return err
		}
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
	if err != nil {
		return fmt.Errorf("read ovh cleanup claim: %w", err)
	}
	if !exists {
		return core.Exit(2, "refusing to delete OVH instance %s without a local Crabbox claim", server.DisplayID())
	}
	if err := validateOVHClaim(claim, server); err != nil {
		return err
	}
	if server.CloudID == "" {
		recovered, err := b.releaseTargetFromClaim(ctx, client, claim.LeaseID)
		if err != nil {
			return err
		}
		server = recovered.Server
		claim, exists, err = core.ReadLeaseClaimWithPresence(recovered.LeaseID)
		if err != nil {
			return fmt.Errorf("reread ovh cleanup claim: %w", err)
		}
		if !exists {
			return core.Exit(2, "ovh cleanup claim disappeared during recovery for lease=%s", recovered.LeaseID)
		}
	}
	if server.CloudID != "" {
		if err := client.DeleteInstance(ctx, b.Cfg.OVH.ProjectID, server.CloudID); err != nil && !isOVHNotFound(err) {
			return err
		}
	}
	if claim.Labels[ovhSSHKeyOwnedLabel] == "true" {
		keyID := strings.TrimSpace(claim.Labels[ovhSSHKeyIDLabel])
		if keyID == "" {
			return core.Exit(2, "ovh lease=%s owns an SSH key but its immutable key id is missing", claim.LeaseID)
		}
		if err := client.DeleteSSHKey(ctx, b.Cfg.OVH.ProjectID, keyID); err != nil && !isOVHNotFound(err) {
			return err
		}
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		return fmt.Errorf("finalize ovh cleanup claim: %w", err)
	}
	core.RemoveStoredTestboxKey(claim.LeaseID)
	return nil
}

func doctorConfigFailure(message string) core.DoctorResult {
	return doctorCheckFailure("configuration", message)
}

func doctorCheckFailure(check, message string) core.DoctorResult {
	return core.DoctorResult{
		Provider: providerName,
		Message:  "auth=configuration-incomplete control_plane=unchecked inventory=unchecked mutation=false runtime=unchecked",
		Status:   "failed",
		Checks: []core.DoctorCheck{{
			Status:  "failed",
			Check:   check,
			Message: message,
			Details: map[string]string{"mutation": "false"},
		}},
	}
}

func (b *Backend) resolveAcquireConfig(ctx context.Context) (core.Config, error) {
	cfg := b.Cfg
	if strings.TrimSpace(cfg.OVH.ProjectID) == "" {
		return core.Config{}, core.Exit(3, "CRABBOX_OVH_PROJECT_ID or ovh.projectId is required")
	}
	if strings.TrimSpace(cfg.OVH.Region) == "" {
		return core.Config{}, core.Exit(3, "CRABBOX_OVH_REGION or ovh.region is required")
	}
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if core.OSImageWasExplicit(cfg) && !core.OVHImageWasExplicit(cfg) && cfg.OSImage != "" && cfg.OSImage != "ubuntu:24.04" {
		return core.Config{}, core.Exit(2, "provider=ovh does not support --os %s; use --os ubuntu:24.04 or set ovh.image explicitly", cfg.OSImage)
	}
	if cfg.OVH.Image == "" {
		cfg.OVH.Image = "Ubuntu 24.04"
	}
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		cfg.OVH.Flavor = cfg.ServerType
	} else if cfg.OVH.Flavor == "" {
		cfg.OVH.Flavor = ovhServerTypeForClass(cfg.Class)
	}
	cfg.ServerType = cfg.OVH.Flavor
	client, err := b.clientFactory(cfg, b.RT)
	if err != nil {
		return core.Config{}, err
	}
	flavor, err := resolveFlavor(ctx, client, cfg.OVH.ProjectID, cfg.OVH.Region, cfg.OVH.Flavor)
	if err != nil {
		return core.Config{}, err
	}
	image, err := resolveImage(ctx, client, cfg.OVH.ProjectID, cfg.OVH.Region, cfg.OVH.Image)
	if err != nil {
		return core.Config{}, err
	}
	cfg.ServerType = flavor.ID
	cfg.OVH.Flavor = flavor.ID
	cfg.OVH.Image = image.ID
	return cfg, nil
}

func resolveFlavor(ctx context.Context, client API, projectID, region, value string) (Flavor, error) {
	flavors, err := client.ListFlavors(ctx, projectID, region)
	if err != nil {
		return Flavor{}, err
	}
	for _, flavor := range flavors {
		if flavor.Matches(value) {
			return flavor, nil
		}
	}
	return Flavor{}, core.Exit(2, "ovh flavor %q was not returned by project=%s region=%s", value, projectID, region)
}

func resolveImage(ctx context.Context, client API, projectID, region, value string) (Image, error) {
	images, err := client.ListImages(ctx, projectID, region)
	if err != nil {
		return Image{}, err
	}
	for _, image := range images {
		if image.Matches(value) {
			return image, nil
		}
	}
	return Image{}, core.Exit(2, "ovh image %q was not returned by project=%s region=%s", value, projectID, region)
}

func (b *Backend) persistRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot string, labels map[string]string, recovery string, instance Instance, key SSHKey, keyCreated bool, reclaim bool) error {
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve ovh recovery working directory: %w", err)
		}
	}
	recoveryLabels := make(map[string]string, len(labels)+4)
	for key, value := range labels {
		recoveryLabels[key] = value
	}
	recoveryLabels["state"] = "provisioning"
	recoveryLabels["recovery"] = recovery
	recoveryLabels["keep"] = "false"
	if recovery == "rollback-cleanup" {
		recoveryLabels["state"] = "failed"
	}
	if key.ID != "" {
		recoveryLabels[ovhSSHKeyIDLabel] = key.ID
		recoveryLabels[ovhSSHKeyOwnedLabel] = fmt.Sprint(keyCreated)
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  instance.ID,
		Name:     core.LeaseProviderName(leaseID, slug),
		Labels:   recoveryLabels,
	}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, reclaim)
}

func (b *Backend) recoveryStillPending(claim core.LeaseClaim) bool {
	createdAt, err := strconv.ParseInt(strings.TrimSpace(claim.Labels["created_at"]), 10, 64)
	if err != nil || createdAt <= 0 {
		return true
	}
	grace := b.recoveryGrace
	if grace <= 0 {
		grace = ambiguousCreateRecoveryGrace
	}
	return b.now().Before(time.Unix(createdAt, 0).Add(grace))
}

func (b *Backend) reconcileCreatedInstance(ctx context.Context, client API, claim core.LeaseClaim) (Instance, bool, error) {
	var lastErr error
	for attempt := 0; attempt < b.effectiveRecoveryPolls(); attempt++ {
		instance, found, err := findCreatedInstance(ctx, client, b.Cfg.OVH.ProjectID, claim.LeaseID, claim.Slug, claim.Labels)
		if err == nil && found {
			return instance, true, nil
		}
		if err != nil {
			lastErr = err
		}
		if attempt+1 < b.effectiveRecoveryPolls() {
			if err := sleepContext(ctx, b.effectiveRecoveryInterval()); err != nil {
				return Instance{}, false, err
			}
		}
	}
	return Instance{}, false, lastErr
}

func (b *Backend) reconcileSSHKey(ctx context.Context, client API, projectID, name, publicKey string) (SSHKey, bool, error) {
	var lastErr error
	for attempt := 0; attempt < b.effectiveRecoveryPolls(); attempt++ {
		keys, err := client.ListSSHKeys(ctx, projectID)
		if err == nil {
			key, found, selectErr := selectSSHKey(keys, name, publicKey)
			if selectErr != nil {
				return SSHKey{}, false, selectErr
			}
			if found {
				return key, true, nil
			}
		} else {
			lastErr = err
		}
		if attempt+1 < b.effectiveRecoveryPolls() {
			if err := sleepContext(ctx, b.effectiveRecoveryInterval()); err != nil {
				return SSHKey{}, false, err
			}
		}
	}
	return SSHKey{}, false, lastErr
}

func (b *Backend) reconcilePendingSSHKey(ctx context.Context, client API, claim core.LeaseClaim) (core.LeaseTarget, bool, error) {
	keyPath, err := core.TestboxKeyPath(claim.LeaseID)
	if err != nil {
		return core.LeaseTarget{}, false, err
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return core.LeaseTarget{}, false, fmt.Errorf("read retained OVH public key: %w", err)
	}
	key, found, err := b.reconcileSSHKey(ctx, client, b.Cfg.OVH.ProjectID, providerKeyForLease(claim.LeaseID), strings.TrimSpace(string(publicKey)))
	if err != nil || !found {
		return core.LeaseTarget{}, false, err
	}
	labels := copyLabels(claim.Labels)
	labels[ovhSSHKeyIDLabel] = key.ID
	labels[ovhSSHKeyOwnedLabel] = "true"
	updated, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
	if err != nil {
		return core.LeaseTarget{}, false, err
	}
	return core.LeaseTarget{LeaseID: updated.LeaseID, Server: claimOnlyServer(updated)}, true, nil
}

func (b *Backend) effectiveRecoveryPolls() int {
	if b.recoveryPolls > 0 {
		return b.recoveryPolls
	}
	return defaultRecoveryPolls
}

func (b *Backend) effectiveRecoveryInterval() time.Duration {
	if b.recoveryInterval > 0 {
		return b.recoveryInterval
	}
	return defaultRecoveryInterval
}

func selectSSHKey(keys []SSHKey, name, publicKey string) (SSHKey, bool, error) {
	publicKey = strings.TrimSpace(publicKey)
	var match SSHKey
	nameMatches := 0
	keyMatches := 0
	for _, key := range keys {
		if key.Name != name {
			continue
		}
		nameMatches++
		if strings.TrimSpace(key.PublicKey) != publicKey {
			continue
		}
		match = key
		keyMatches++
	}
	switch {
	case keyMatches == 1:
		return match, true, nil
	case keyMatches > 1:
		return SSHKey{}, false, core.Exit(4, "ovh SSH key %q has multiple entries matching the retained public key", name)
	case nameMatches > 0:
		return SSHKey{}, false, core.Exit(3, "ovh SSH key %q exists with a different public key", name)
	default:
		return SSHKey{}, false, nil
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func claimOnlyServer(claim core.LeaseClaim) core.Server {
	return core.Server{
		Provider: providerName,
		CloudID:  claim.CloudID,
		Name:     core.LeaseProviderName(claim.LeaseID, claim.Slug),
		Labels:   copyLabels(claim.Labels),
	}
}

func (b *Backend) waitForInstanceIP(ctx context.Context, client API, projectID, instanceID string) (Instance, error) {
	deadline := b.now().Add(5 * time.Minute)
	if b.ipWaitTimeout > 0 {
		deadline = b.now().Add(b.ipWaitTimeout)
	}
	interval := 3 * time.Second
	if b.ipWaitInterval > 0 {
		interval = b.ipWaitInterval
	}
	for {
		instance, err := client.GetInstance(ctx, projectID, instanceID)
		if err != nil {
			return Instance{}, err
		}
		if publicIPv4(instance) != "" {
			return instance, nil
		}
		if b.now().After(deadline) {
			return Instance{}, core.Exit(5, "timed out waiting for OVH instance IP")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Instance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *Backend) now() time.Time {
	if b.RT.Clock != nil {
		return b.RT.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func ovhLeaseLabels(cfg core.Config, leaseID, slug string, keep bool, now time.Time, state string) map[string]string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	labels[ovhProjectLabel] = cfg.OVH.ProjectID
	labels[ovhRegionLabel] = cfg.OVH.Region
	return labels
}

func mergeInstanceLabels(instance Instance, labels map[string]string) Instance {
	if instance.Labels == nil {
		instance.Labels = map[string]string{}
	}
	for key, value := range labels {
		if strings.TrimSpace(instance.Labels[key]) == "" {
			instance.Labels[key] = value
		}
	}
	return instance
}

func findCreatedInstance(ctx context.Context, client API, projectID, leaseID, slug string, labels map[string]string) (Instance, bool, error) {
	instances, err := client.ListInstances(ctx, projectID)
	if err != nil {
		return Instance{}, false, err
	}
	expectedName := core.LeaseProviderName(leaseID, slug)
	var found Instance
	matches := 0
	for _, instance := range instances {
		if instance.Name != expectedName {
			continue
		}
		if expectedKey := strings.TrimSpace(labels[ovhSSHKeyIDLabel]); expectedKey != "" &&
			instance.Labels[ovhSSHKeyIDLabel] != expectedKey &&
			instance.SSHKeyID != expectedKey {
			continue
		}
		found = instance
		matches++
	}
	if matches > 1 {
		return Instance{}, false, core.Exit(2, "refusing to recover ambiguous OVH create result for lease=%s slug=%s: matched %d instances", leaseID, slug, matches)
	}
	return found, matches == 1, nil
}

type claimedLiveServer struct {
	server   core.Server
	instance Instance
}

func (b *Backend) claimedLiveServers(instances []Instance) ([]claimedLiveServer, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, fmt.Errorf("list ovh lease claims: %w", err)
	}
	out := make([]claimedLiveServer, 0, len(claims))
	usedInstances := map[string]string{}
	for _, claim := range claims {
		if claim.Provider != providerName || claim.Slug == "" || !claimMatchesOVHProject(claim, b.Cfg.OVH.ProjectID) {
			continue
		}
		instance, found, err := findClaimedInstance(instances, claim)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if otherLease := usedInstances[instance.ID]; otherLease != "" {
			return nil, core.Exit(2, "refusing to use OVH instance %s for multiple local claims: %s and %s", instance.ID, otherLease, claim.LeaseID)
		}
		server := overlayClaimLabels(serverFromInstance(instance, b.Cfg), claim)
		if err := validateOVHClaim(claim, server); err != nil {
			return nil, err
		}
		usedInstances[instance.ID] = claim.LeaseID
		out = append(out, claimedLiveServer{server: server, instance: instance})
	}
	return out, nil
}

func findClaimedInstance(instances []Instance, claim core.LeaseClaim) (Instance, bool, error) {
	expectedName := core.LeaseProviderName(claim.LeaseID, claim.Slug)
	var found Instance
	matches := 0
	for _, instance := range instances {
		matchesClaim := false
		if claim.CloudID != "" {
			matchesClaim = instance.ID == claim.CloudID
		} else if expectedName != "" {
			matchesClaim = instance.Name == expectedName
		}
		if !matchesClaim {
			continue
		}
		found = instance
		matches++
	}
	if matches > 1 {
		return Instance{}, false, core.Exit(2, "refusing to use OVH lease claim %s: matched %d live instances", claim.LeaseID, matches)
	}
	return found, matches == 1, nil
}

func claimMatchesOVHProject(claim core.LeaseClaim, projectID string) bool {
	claimProject := strings.TrimSpace(claim.Labels[ovhProjectLabel])
	return claimProject == "" || projectID == "" || claimProject == projectID
}

func overlayClaimLabels(server core.Server, claim core.LeaseClaim) core.Server {
	merged := server
	merged.Labels = copyLabels(server.Labels)
	for key, value := range claim.Labels {
		if isOVHLiveIdentityLabel(key) && strings.TrimSpace(merged.Labels[key]) != "" {
			continue
		}
		merged.Labels[key] = value
	}
	return merged
}

func isOVHLiveIdentityLabel(key string) bool {
	switch key {
	case "crabbox", "created_by", "provider", "lease", "slug", ovhProjectLabel, ovhRegionLabel, ovhSSHKeyIDLabel:
		return true
	default:
		return false
	}
}

func overlayExpectedOVHLabels(live, expected core.Server) core.Server {
	merged := live
	merged.Labels = copyLabels(live.Labels)
	for _, key := range []string{"crabbox", "created_by", "provider", "lease", "slug", ovhProjectLabel, ovhRegionLabel, ovhSSHKeyIDLabel, ovhSSHKeyOwnedLabel} {
		if strings.TrimSpace(merged.Labels[key]) == "" && expected.Labels[key] != "" {
			merged.Labels[key] = expected.Labels[key]
		}
	}
	return merged
}

func copyLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
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

func exactTailscaleLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range []string{
		"tailscale",
		"tailscale_state",
		"tailscale_hostname",
		"tailscale_tags",
		"tailscale_ipv4",
		"tailscale_fqdn",
		"tailscale_error",
		"tailscale_exit_node",
		"tailscale_exit_node_allow_lan_access",
	} {
		if value, ok := labels[key]; ok {
			out[key] = value
		}
	}
	return out
}

func serverFromInstance(instance Instance, cfg core.Config) core.Server {
	labels := make(map[string]string, len(instance.Labels)+6)
	for key, value := range instance.Labels {
		labels[key] = value
	}
	if labels["provider"] == "" && isCrabboxInstance(instance) {
		labels["provider"] = providerName
	}
	if labels[ovhProjectLabel] == "" {
		labels[ovhProjectLabel] = cfg.OVH.ProjectID
	}
	if labels[ovhRegionLabel] == "" {
		labels[ovhRegionLabel] = firstNonBlank(instance.Region, cfg.OVH.Region)
	}
	if labels[ovhSSHKeyIDLabel] == "" && instance.SSHKeyID != "" {
		labels[ovhSSHKeyIDLabel] = instance.SSHKeyID
	}
	server := core.Server{
		Provider: providerName,
		CloudID:  instance.ID,
		Name:     instance.Name,
		Status:   instance.Status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = publicIPv4(instance)
	server.ServerType.Name = firstNonBlank(instance.Flavor.ID, instance.Flavor.Name, cfg.ServerType)
	return server
}

func publicIPv4(instance Instance) string {
	fallback := ""
	for _, ip := range instance.IPAddresses {
		value := strings.TrimSpace(ip.IP)
		if value == "" {
			continue
		}
		parsed := net.ParseIP(value)
		if parsed == nil || parsed.To4() == nil || !isPublicIPv4(parsed) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(ip.Type)) {
		case "public", "external", "floating":
			return value
		case "":
			if fallback == "" {
				fallback = value
			}
		}
	}
	return fallback
}

func isPublicIPv4(ip net.IP) bool {
	return ip.IsGlobalUnicast() &&
		!ip.IsPrivate() &&
		!ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified()
}

func isOwnedInstance(instance Instance) bool {
	labels := instance.Labels
	return labels["crabbox"] == "true" &&
		labels["created_by"] == "crabbox" &&
		labels["provider"] == providerName &&
		labels["lease"] != "" &&
		labels["slug"] != ""
}

func validateOVHServerOwnership(server core.Server) error {
	if server.Labels == nil ||
		server.Labels["crabbox"] != "true" ||
		server.Labels["created_by"] != "crabbox" ||
		server.Labels["provider"] != providerName ||
		server.Labels["lease"] == "" ||
		server.Labels["slug"] == "" {
		return core.Exit(2, "refusing to operate on non-Crabbox OVH instance: %s", server.DisplayID())
	}
	if server.Labels[ovhProjectLabel] == "" {
		return core.Exit(2, "refusing to operate on OVH instance %s without project ownership metadata", server.DisplayID())
	}
	return nil
}

func validateOVHClaim(claim core.LeaseClaim, server core.Server) error {
	if claim.LeaseID != server.Labels["lease"] ||
		claim.Provider != providerName ||
		claim.Slug == "" ||
		claim.Slug != server.Labels["slug"] ||
		claim.Labels["provider"] != providerName ||
		claim.Labels["lease"] != claim.LeaseID ||
		claim.Labels["slug"] != claim.Slug {
		return core.Exit(2, "ovh lease claim identity does not match lease=%s slug=%s", server.Labels["lease"], server.Labels["slug"])
	}
	if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
		return core.Exit(2, "refusing to release OVH instance %s from stale local claim", server.DisplayID())
	}
	if claim.Labels[ovhProjectLabel] == "" || server.Labels[ovhProjectLabel] == "" || claim.Labels[ovhProjectLabel] != server.Labels[ovhProjectLabel] {
		return core.Exit(3, "ovh project mismatch or missing project identity for lease=%s", claim.LeaseID)
	}
	if claim.Labels[ovhSSHKeyIDLabel] != "" && server.Labels[ovhSSHKeyIDLabel] != "" && claim.Labels[ovhSSHKeyIDLabel] != server.Labels[ovhSSHKeyIDLabel] {
		return core.Exit(2, "refusing to operate on OVH instance %s with mismatched SSH key identity", server.DisplayID())
	}
	return nil
}

func validateLiveOVHInstance(live, expected core.Server) error {
	if err := validateOVHServerOwnership(live); err != nil {
		return err
	}
	if live.CloudID != expected.CloudID ||
		live.Name != core.LeaseProviderName(expected.Labels["lease"], expected.Labels["slug"]) ||
		live.Labels["lease"] != expected.Labels["lease"] ||
		live.Labels["slug"] != expected.Labels["slug"] ||
		live.Labels[ovhProjectLabel] != expected.Labels[ovhProjectLabel] {
		return core.Exit(2, "refusing to operate on OVH instance %s from stale local claim", expected.DisplayID())
	}
	return nil
}

func rollbackOVHAcquire(client API, projectID, instanceID, keyID string, keyCreated bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var errs []error
	if instanceID != "" {
		if err := client.DeleteInstance(ctx, projectID, instanceID); err != nil && !isOVHNotFound(err) {
			errs = append(errs, err)
		}
	}
	if keyCreated && keyID != "" {
		if err := client.DeleteSSHKey(ctx, projectID, keyID); err != nil && !isOVHNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isOVHNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404
}

func isDeterminateCreateError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	default:
		return apiErr.Status >= 400 && apiErr.Status < 500
	}
}

func regionExists(regions []Region, name string) bool {
	for _, region := range regions {
		if region.Name == name {
			return true
		}
	}
	return false
}

func flavorExists(flavors []Flavor, value string) bool {
	for _, flavor := range flavors {
		if flavor.Matches(value) {
			return true
		}
	}
	return false
}

func imageExists(images []Image, value string) bool {
	for _, image := range images {
		if image.Matches(value) {
			return true
		}
	}
	return false
}

func isCrabboxInstance(instance Instance) bool {
	if strings.HasPrefix(instance.Name, "crabbox-") || strings.HasPrefix(instance.Name, "cbx_") {
		return true
	}
	if instance.Labels["managed_by"] == "crabbox" || instance.Labels["crabbox"] == "true" {
		return true
	}
	for _, tag := range instance.Tags {
		if tag == "crabbox" || strings.HasPrefix(tag, "crabbox:") {
			return true
		}
	}
	return false
}

func blank(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func providerKeyForLease(leaseID string) string {
	return core.ProviderKeyForLease(leaseID)
}
