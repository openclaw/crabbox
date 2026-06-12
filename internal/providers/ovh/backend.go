package ovh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Backend struct {
	shared.DirectSSHBackend
	clientFactory  func(core.Config, core.Runtime) (API, error)
	waitSSH        func(context.Context, *core.SSHTarget, string, time.Duration) error
	ipWaitTimeout  time.Duration
	ipWaitInterval time.Duration
}

const (
	ovhProjectLabel     = "ovh_project"
	ovhRegionLabel      = "ovh_region"
	ovhSSHKeyIDLabel    = "ovh_ssh_key_id"
	ovhSSHKeyOwnedLabel = "ovh_ssh_key_owned"
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
	for _, instance := range instances {
		if isOwnedInstance(instance) {
			servers = append(servers, serverFromInstance(instance, cfg))
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
	ambiguousCreate := false
	var created Instance
	var createdKey SSHKey
	keyCreated := false
	defer func() {
		if err == nil || committed {
			return
		}
		if ambiguousCreate {
			if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, labels, created, createdKey, keyCreated, req.Reclaim); claimErr != nil {
				err = errors.Join(err, fmt.Errorf("persist ovh recovery claim: %w", claimErr))
			}
			return
		}
		if created.ID == "" && createdKey.ID == "" {
			core.RemoveStoredTestboxKey(leaseID)
			return
		}
		recoveryPersisted := false
		if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, labels, created, createdKey, keyCreated, req.Reclaim); claimErr == nil {
			recoveryPersisted = true
		} else {
			err = errors.Join(err, fmt.Errorf("persist ovh recovery claim: %w", claimErr))
		}
		if cleanupErr := rollbackOVHAcquire(client, cfg.OVH.ProjectID, created.ID, createdKey.ID, keyCreated); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("ovh cleanup failed: %w", cleanupErr))
			return
		}
		if !ambiguousCreate {
			if recoveryPersisted {
				core.RemoveLeaseClaim(leaseID)
			}
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=ovh lease=%s slug=%s flavor=%s region=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, cfg.OVH.Region, cfg.OVH.Image, req.Keep)
	createdKey, err = client.CreateSSHKey(ctx, cfg.OVH.ProjectID, cfg.ProviderKey, publicKey)
	if err != nil {
		return core.LeaseTarget{}, err
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
		Labels:   labels,
	})
	if err != nil {
		ambiguousCreate = true
		if recovered, ok, findErr := findCreatedInstance(ctx, client, cfg.OVH.ProjectID, leaseID, slug, labels); findErr != nil {
			return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("reconcile ovh create recovery: %w", findErr))
		} else if ok {
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
	for _, instance := range instances {
		if !isOwnedInstance(instance) {
			continue
		}
		server := serverFromInstance(instance, b.Cfg)
		servers = append(servers, server)
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
	if req.ReleaseOnly || req.StatusOnly {
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
			server := serverFromInstance(instance, b.Cfg)
			if err := validateOVHClaim(claim, server); err != nil {
				return core.LeaseTarget{}, err
			}
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
		}
		return core.LeaseTarget{
			LeaseID: claim.LeaseID,
			Server:  core.Server{Provider: providerName, Name: claim.Slug, Labels: claim.Labels},
		}, nil
	}
	instance, err := client.GetInstance(ctx, b.Cfg.OVH.ProjectID, claim.CloudID)
	if err == nil {
		server := serverFromInstance(instance, b.Cfg)
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
	out := make([]core.LeaseView, 0, len(instances))
	for _, instance := range instances {
		if isOwnedInstance(instance) {
			server := serverFromInstance(instance, b.Cfg)
			if claim, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"]); err != nil {
				return nil, err
			} else if exists {
				if err := validateOVHClaim(claim, server); err != nil {
					return nil, err
				}
				server = overlayClaimLabels(server, claim)
			}
			out = append(out, server)
		}
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
	live := serverFromInstance(instance, b.Cfg)
	if err := validateLiveOVHInstance(live, server); err != nil {
		return core.Server{}, err
	}
	cfg := b.Cfg
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		live.Labels = copyLabels(live.Labels)
		delete(live.Labels, "idle_timeout")
		delete(live.Labels, "idle_timeout_secs")
	}
	live.Labels = core.TouchDirectLeaseLabels(live.Labels, cfg, req.State, b.now())
	if claim, exists, err := core.ReadLeaseClaimWithPresence(live.Labels["lease"]); err != nil {
		return core.Server{}, err
	} else if exists {
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(live.Labels["lease"], claim, live.Labels); err != nil {
			return core.Server{}, err
		}
	}
	return live, nil
}

func (b *Backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	claimedServers := make([]core.LeaseView, 0, len(servers))
	for _, server := range servers {
		claim, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
		if err != nil {
			return fmt.Errorf("read ovh cleanup claim: %w", err)
		}
		if !exists {
			continue
		}
		if err := validateOVHClaim(claim, server); err != nil {
			return err
		}
		merged := server
		merged.Labels = make(map[string]string, len(server.Labels)+len(claim.Labels))
		for key, value := range server.Labels {
			merged.Labels[key] = value
		}
		for key, value := range claim.Labels {
			merged.Labels[key] = value
		}
		claimedServers = append(claimedServers, merged)
	}
	return b.CleanupServers(ctx, req, claimedServers)
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
			live := serverFromInstance(instance, b.Cfg)
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

func (b *Backend) persistRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot string, labels map[string]string, instance Instance, key SSHKey, keyCreated bool, reclaim bool) error {
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
	recoveryLabels["recovery"] = "ambiguous-create"
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
		if !isOwnedInstance(instance) {
			continue
		}
		if instance.Labels["lease"] != leaseID || instance.Labels["slug"] != slug {
			continue
		}
		if expectedProject := strings.TrimSpace(labels[ovhProjectLabel]); expectedProject != "" && instance.Labels[ovhProjectLabel] != expectedProject {
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

func overlayClaimLabels(server core.Server, claim core.LeaseClaim) core.Server {
	merged := server
	merged.Labels = copyLabels(server.Labels)
	for key, value := range claim.Labels {
		merged.Labels[key] = value
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
