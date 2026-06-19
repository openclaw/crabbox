package scaleway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	marketplace "github.com/scaleway/scaleway-sdk-go/api/marketplace/v2"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const providerName = "scaleway"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator: core.CoordinatorNever,
	}
}

type flagValues struct {
	Region         *string
	Zone           *string
	Image          *string
	Type           *string
	ProjectID      *string
	OrganizationID *string
	SecurityGroup  *string
	SSHCIDRs       *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Region:         fs.String("scaleway-region", defaults.Scaleway.Region, "Scaleway region"),
		Zone:           fs.String("scaleway-zone", defaults.Scaleway.Zone, "Scaleway zone"),
		Image:          fs.String("scaleway-image", defaults.Scaleway.Image, "Scaleway image label or ID"),
		Type:           fs.String("scaleway-type", defaults.Scaleway.Type, "Scaleway Instances commercial type"),
		ProjectID:      fs.String("scaleway-project-id", defaults.Scaleway.ProjectID, "Scaleway project ID"),
		OrganizationID: fs.String("scaleway-organization-id", defaults.Scaleway.OrganizationID, "Scaleway organization ID"),
		SecurityGroup:  fs.String("scaleway-security-group", defaults.Scaleway.SecurityGroup, "Scaleway security group ID"),
		SSHCIDRs:       fs.String("scaleway-ssh-cidrs", "", "comma-separated Scaleway SSH source CIDRs"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "scaleway-region") {
		cfg.Scaleway.Region = *v.Region
		core.SetScalewayRegionExplicit(cfg)
	}
	if core.FlagWasSet(fs, "scaleway-zone") {
		cfg.Scaleway.Zone = *v.Zone
		core.SetScalewayZoneExplicit(cfg)
	}
	if core.FlagWasSet(fs, "scaleway-image") {
		cfg.Scaleway.Image = *v.Image
		core.SetScalewayImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "scaleway-type") {
		cfg.Scaleway.Type = *v.Type
		core.SetScalewayTypeExplicit(cfg)
	}
	if core.FlagWasSet(fs, "scaleway-project-id") {
		cfg.Scaleway.ProjectID = *v.ProjectID
	}
	if core.FlagWasSet(fs, "scaleway-organization-id") {
		cfg.Scaleway.OrganizationID = *v.OrganizationID
	}
	if core.FlagWasSet(fs, "scaleway-security-group") {
		cfg.Scaleway.SecurityGroup = *v.SecurityGroup
	}
	if core.FlagWasSet(fs, "scaleway-ssh-cidrs") {
		cfg.Scaleway.SSHCIDRs = splitCommaList(*v.SSHCIDRs)
	}
	return nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateFoundationConfig(cfg)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.Scaleway.Type != "" {
		return cfg.Scaleway.Type
	}
	return scalewayServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return scalewayServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return &Backend{spec: p.Spec(), cfg: cfg, rt: rt, newClient: newClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "scaleway doctor backend unavailable")
	}
	return doctor, nil
}

type Backend struct {
	spec      core.ProviderSpec
	cfg       core.Config
	rt        core.Runtime
	newClient func(core.Config, core.Runtime) (Client, error)
	waitSSH   func(context.Context, *core.SSHTarget, string, time.Duration) error
	now       func() time.Time
}

func (b *Backend) Spec() core.ProviderSpec { return b.spec }

func (b *Backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return core.DoctorResult{Provider: providerName, Message: err.Error(), Status: "failed", Checks: []core.DoctorCheck{{
			Status:  "failed",
			Check:   "auth",
			Message: err.Error(),
			Details: map[string]string{"mutation": "false"},
		}}}, nil
	}
	servers, err := b.listScalewayServers(ctx, client)
	if err != nil {
		return core.DoctorResult{}, err
	}
	count := 0
	for _, item := range servers {
		if b.ownedServer(item) {
			count++
		}
	}
	return core.DoctorResult{Provider: providerName, Message: fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d zone=%s project=%s", count, client.Zone(), client.ProjectID()), Status: "ok", Checks: []core.DoctorCheck{{
		Status:  "ok",
		Check:   "auth",
		Message: "auth=ready mutation=false",
		Details: map[string]string{"mutation": "false"},
	}}}, nil
}

func (b *Backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.rt, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *Backend) acquireOnce(ctx context.Context, req core.AcquireRequest) (target core.LeaseTarget, err error) {
	if err := validateFoundationConfig(b.cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.cfgForRun()
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=scaleway only supports target=linux")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s to contain a Tailscale auth key", cfg.Tailscale.AuthKeyEnv)
	}
	if len(cfg.Scaleway.SSHCIDRs) > 0 {
		return core.LeaseTarget{}, core.Exit(2, "provider=scaleway does not yet manage security-group SSH CIDRs; attach a preconfigured scaleway.securityGroup or remove scaleway.sshCIDRs")
	}
	client, err := b.newClient(cfg, b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	servers, err := b.listScalewayServers(ctx, client)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	coreServers := make([]core.Server, 0, len(servers))
	for _, item := range servers {
		if b.ownedServer(item) {
			coreServers = append(coreServers, b.serverFromScaleway(item))
		}
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, coreServers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	cfg.ServerType = serverTypeForConfig(cfg)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := b.clockNow()
	keyName := providerKeyName(leaseID)
	sshKey, err := client.IAM().CreateSSHKey(&iam.CreateSSHKeyRequest{Name: keyName, PublicKey: publicKey, ProjectID: client.ProjectID()}, scw.WithContext(ctx))
	if err != nil {
		if isAmbiguousScalewayError(err) {
			keyID := ""
			recovery := "ambiguous-key-create"
			reconcileCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			reconciled, reconcileErr := b.reconcileSSHKey(reconcileCtx, client, keyName, publicKey)
			cancel()
			if reconciled != nil {
				keyID = reconciled.ID
				recovery = "rollback-key-cleanup"
			}
			if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, client, "", "", keyID, keyName, recovery, req.Keep, now); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, reconcileErr, fmt.Errorf("persist Scaleway ambiguous key recovery: %w", claimErr))
			}
			cleanupKey = false
			return core.LeaseTarget{}, errors.Join(err, reconcileErr)
		}
		return core.LeaseTarget{}, err
	}
	var created *instance.Server
	committed := false
	forceRollback := false
	defer func() {
		if err == nil || committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if created == nil || created.ID == "" {
			if cleanupKey {
				keyErr := client.IAM().DeleteSSHKey(&iam.DeleteSSHKeyRequest{SSHKeyID: sshKey.ID}, scw.WithContext(cleanupCtx))
				if keyErr != nil && !isScalewayNotFound(keyErr) {
					claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, client, "", "", sshKey.ID, sshKey.Name, "rollback-key-cleanup", req.Keep, now)
					cleanupKey = false
					err = errors.Join(err, fmt.Errorf("scaleway rollback SSH key cleanup failed: %w", keyErr), claimErr)
				}
			}
			return
		}
		keepOnFailure := req.Keep && !forceRollback
		recovery := "rollback-cleanup"
		if keepOnFailure {
			recovery = "kept-after-failure"
		}
		claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, client, created.ID, publicIPv4(created), sshKey.ID, sshKey.Name, recovery, keepOnFailure, now)
		if !keepOnFailure {
			deleteErr := b.deleteServerResource(cleanupCtx, client, created.ID)
			keyErr := client.IAM().DeleteSSHKey(&iam.DeleteSSHKeyRequest{SSHKeyID: sshKey.ID}, scw.WithContext(cleanupCtx))
			if deleteErr != nil || keyErr != nil {
				cleanupKey = false
				err = errors.Join(err, fmt.Errorf("scaleway rollback cleanup failed"), claimErr, deleteErr, keyErr, cleanupCtx.Err())
				return
			}
			if claimErr == nil {
				core.RemoveLeaseClaim(leaseID)
			}
		} else {
			cleanupKey = false
			if claimErr != nil {
				err = errors.Join(err, fmt.Errorf("persist kept Scaleway recovery: %w", claimErr))
			}
		}
	}()
	labels := labelsFromTags(leaseTags(cfg, leaseID, slug, "provisioning", req.Keep, now))
	labels["scaleway_project"] = client.ProjectID()
	labels["scaleway_organization"] = client.OrganizationID()
	labels["scaleway_region"] = client.Region()
	labels["scaleway_zone"] = client.Zone()
	labels["scaleway_ssh_key_id"] = sshKey.ID
	labels["scaleway_ssh_key_name"] = sshKey.Name
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=scaleway lease=%s slug=%s type=%s zone=%s image=%s keep=%v\n", leaseID, slug, cfg.ServerType, client.Zone(), imageForConfig(cfg), req.Keep)
	imageID, err := b.resolveImage(ctx, client, cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	createReq := &instance.CreateServerRequest{
		Zone:              scw.Zone(client.Zone()),
		Name:              core.LeaseProviderName(leaseID, slug),
		DynamicIPRequired: scw.BoolPtr(true),
		CommercialType:    cfg.ServerType,
		Image:             scw.StringPtr(imageID),
		Project:           scw.StringPtr(client.ProjectID()),
		Tags:              replaceCrabboxTags(nil, tagsFromLabels(labels)),
	}
	if sg := strings.TrimSpace(cfg.Scaleway.SecurityGroup); sg != "" {
		createReq.SecurityGroup = scw.StringPtr(sg)
	}
	createResp, err := client.Instance().CreateServer(createReq, scw.WithContext(ctx))
	if err != nil {
		if isAmbiguousScalewayError(err) {
			if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, client, "", "", sshKey.ID, sshKey.Name, "ambiguous-create", req.Keep, now); claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist Scaleway ambiguous-create recovery: %w", claimErr))
			}
			cleanupKey = false
		}
		return core.LeaseTarget{}, err
	}
	created = createResp.Server
	if created == nil || created.ID == "" {
		err = core.Exit(5, "Scaleway create server response omitted server id")
		if claimErr := b.persistRecoveryClaim(leaseID, slug, cfg, req.Repo.Root, client, "", "", sshKey.ID, sshKey.Name, "ambiguous-create-response", req.Keep, now); claimErr != nil {
			err = errors.Join(err, fmt.Errorf("persist Scaleway ambiguous-create-response recovery: %w", claimErr))
		}
		cleanupKey = false
		return core.LeaseTarget{}, err
	}
	if err := client.Instance().SetServerUserData(&instance.SetServerUserDataRequest{
		Zone:     scw.Zone(client.Zone()),
		ServerID: created.ID,
		Key:      "cloud-init",
		Content:  strings.NewReader(core.CloudInitUserData(cfg, publicKey)),
	}, scw.WithContext(ctx)); err != nil {
		return core.LeaseTarget{}, err
	}
	if _, err := client.Instance().ServerAction(&instance.ServerActionRequest{
		Zone:     scw.Zone(client.Zone()),
		ServerID: created.ID,
		Action:   instance.ServerActionPoweron,
	}, scw.WithContext(ctx)); err != nil {
		return core.LeaseTarget{}, err
	}
	waited, err := b.waitForPublicIPv4(ctx, client, created.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	created = waited
	server := b.serverFromScaleway(created)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if b.waitSSH == nil {
		b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
			return core.WaitForSSHReady(ctx, target, b.rt.Stderr, phase, timeout)
		}
	}
	if err := b.waitSSH(ctx, &ssh, "scaleway bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	readyLabels := labelsFromTags(leaseTags(cfg, leaseID, slug, "ready", req.Keep, now))
	for key, value := range labels {
		if strings.HasPrefix(key, "scaleway_") {
			readyLabels[key] = value
		}
	}
	updateResp, err := client.Instance().UpdateServer(&instance.UpdateServerRequest{
		Zone:     scw.Zone(client.Zone()),
		ServerID: created.ID,
		Tags:     ptrTags(replaceCrabboxTags(created.Tags, tagsFromLabels(readyLabels))),
	}, scw.WithContext(ctx))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if updateResp != nil && updateResp.Server != nil {
		created = updateResp.Server
	}
	server = b.serverFromScaleway(created)
	server.Labels = readyLabels
	if req.OnAcquired != nil {
		if err := req.OnAcquired(core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}); err != nil {
			forceRollback = true
			return core.LeaseTarget{}, err
		}
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, err
	}
	committed = true
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s scaleway_server=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *Backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers, err := b.listScalewayServers(ctx, client)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	coreServers := make([]core.Server, 0, len(servers))
	byID := map[string]*instance.Server{}
	for _, item := range servers {
		if !b.ownedServer(item) {
			continue
		}
		server := b.serverFromScaleway(item)
		coreServers = append(coreServers, server)
		byID[server.CloudID] = item
	}
	server, leaseID, err := core.FindServerByAlias(coreServers, req.ID)
	if err == nil && leaseID != "" {
		return b.targetFromServer(ctx, client, byID[server.CloudID], req)
	}
	if item, ok := byID[strings.TrimSpace(req.ID)]; ok {
		return b.targetFromServer(ctx, client, item, req)
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/scaleway server not found: %s", req.ID)
}

func (b *Backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return nil, err
	}
	servers, err := b.listScalewayServers(ctx, client)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(servers))
	for _, item := range servers {
		if b.ownedServer(item) {
			out = append(out, b.serverFromScaleway(item))
		}
	}
	return out, nil
}

func (b *Backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return err
	}
	return b.deleteServer(ctx, client, req.Lease.Server)
}

func (b *Backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s scaleway_server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *Backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return core.Server{}, err
	}
	item, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: req.Lease.Server.CloudID}, scw.WithContext(ctx))
	if err != nil {
		return core.Server{}, err
	}
	if item.Server == nil {
		return core.Server{}, core.Exit(4, "scaleway server not found: %s", req.Lease.Server.CloudID)
	}
	live := b.serverFromScaleway(item.Server)
	if err := b.validateLiveServer(live, req.Lease.Server); err != nil {
		return core.Server{}, err
	}
	cfg := b.cfgForRun()
	labels := live.Labels
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		labels = make(map[string]string, len(live.Labels))
		for key, value := range live.Labels {
			labels[key] = value
		}
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.clockNow())
	updateResp, err := client.Instance().UpdateServer(&instance.UpdateServerRequest{
		Zone:     scw.Zone(client.Zone()),
		ServerID: item.Server.ID,
		Tags:     ptrTags(replaceCrabboxTags(item.Server.Tags, tagsFromLabels(labels))),
	}, scw.WithContext(ctx))
	if err != nil {
		return core.Server{}, err
	}
	if updateResp != nil && updateResp.Server != nil {
		live = b.serverFromScaleway(updateResp.Server)
	}
	live.Labels = labels
	claim, ok, claimErr := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if claimErr != nil {
		return core.Server{}, claimErr
	}
	if ok {
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(req.Lease.LeaseID, labels["slug"], cfg, live, req.Lease.SSH, claim.RepoRoot, req.IdleTimeout, false, claim, true); err != nil {
			return core.Server{}, err
		}
	}
	return live, nil
}

func (b *Backend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	server := lease.Server
	if err := validateScalewayLabels(server.Labels); err != nil {
		return core.Server{}, err
	}
	if lease.LeaseID == "" || lease.LeaseID != server.Labels["lease"] {
		return core.Server{}, core.Exit(2, "refusing to update Tailscale metadata for mismatched Scaleway lease")
	}
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return core.Server{}, err
	}
	resp, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: server.CloudID}, scw.WithContext(ctx))
	if err != nil {
		return core.Server{}, err
	}
	if resp == nil || resp.Server == nil {
		return core.Server{}, core.Exit(4, "scaleway server not found: %s", server.CloudID)
	}
	live := b.serverFromScaleway(resp.Server)
	if err := b.validateLiveServer(live, server); err != nil {
		return core.Server{}, err
	}
	if err := b.validateProviderIdentity(live.Labels, client); err != nil {
		return core.Server{}, err
	}
	labels := live.Labels
	applyTailscaleMetadata(labels, meta)
	updateResp, err := client.Instance().UpdateServer(&instance.UpdateServerRequest{
		Zone:     scw.Zone(client.Zone()),
		ServerID: resp.Server.ID,
		Tags:     ptrTags(replaceCrabboxTags(resp.Server.Tags, tagsFromLabels(labels))),
	}, scw.WithContext(ctx))
	if err != nil {
		return core.Server{}, err
	}
	if updateResp != nil && updateResp.Server != nil {
		live = b.serverFromScaleway(updateResp.Server)
	}
	live.Labels = labels
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		return core.Server{}, err
	}
	if exists {
		if claim.Provider != providerName || claim.CloudID != server.CloudID {
			return core.Server{}, core.Exit(2, "refusing to update Tailscale metadata from stale Scaleway claim")
		}
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, claim, labels); err != nil {
			return core.Server{}, err
		}
	}
	return live, nil
}

func (b *Backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	client, err := b.newClient(b.cfgForRun(), b.rt)
	if err != nil {
		return err
	}
	servers, err := b.listScalewayServers(ctx, client)
	if err != nil {
		return err
	}
	for _, item := range servers {
		server := b.serverFromScaleway(item)
		if !b.ownedServer(item) {
			fmt.Fprintf(b.rt.Stderr, "skip scaleway_server=%s reason=foreign-or-incomplete-ownership\n", item.ID)
			continue
		}
		remove, reason := core.ShouldCleanupServer(server, b.clockNow())
		if !remove {
			fmt.Fprintf(b.rt.Stderr, "skip scaleway_server=%s reason=%s\n", item.ID, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete scaleway_server=%s lease=%s reason=%s\n", item.ID, server.Labels["lease"], reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "delete scaleway_server=%s lease=%s reason=%s\n", item.ID, server.Labels["lease"], reason)
		if err := b.deleteServer(ctx, client, server); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) cfgForRun() core.Config {
	cfg := b.cfg
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = "root"
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = "/work/crabbox"
	}
	cfg.Scaleway.Region = regionForConfig(cfg)
	cfg.Scaleway.Zone = zoneForConfig(cfg)
	cfg.Scaleway.Image = imageForConfig(cfg)
	cfg.Scaleway.Type = serverTypeForConfig(cfg)
	if cfg.ServerType == "" {
		cfg.ServerType = cfg.Scaleway.Type
	}
	return cfg
}

func (b *Backend) listScalewayServers(ctx context.Context, client Client) ([]*instance.Server, error) {
	resp, err := client.Instance().ListServers(&instance.ListServersRequest{Zone: scw.Zone(client.Zone()), Project: scw.StringPtr(client.ProjectID()), Tags: []string{tagCrabbox, "crabbox:provider:" + providerName}}, scw.WithContext(ctx), scw.WithAllPages())
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Servers, nil
}

func (b *Backend) reconcileSSHKey(ctx context.Context, client Client, name, publicKey string) (*iam.SSHKey, error) {
	resp, err := client.IAM().ListSSHKeys(&iam.ListSSHKeysRequest{
		Name:      scw.StringPtr(name),
		ProjectID: scw.StringPtr(client.ProjectID()),
	}, scw.WithContext(ctx), scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("reconcile Scaleway SSH key create: %w", err)
	}
	var matches []*iam.SSHKey
	if resp != nil {
		for _, key := range resp.SSHKeys {
			if key != nil && key.Name == name && key.ProjectID == client.ProjectID() && strings.TrimSpace(key.PublicKey) == strings.TrimSpace(publicKey) {
				matches = append(matches, key)
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, core.Exit(5, "ambiguous Scaleway SSH key create: %d matching keys named %s", len(matches), name)
	}
}

func (b *Backend) resolveImage(ctx context.Context, client Client, cfg core.Config) (string, error) {
	image := imageForConfig(cfg)
	if image == "" {
		return "", core.Exit(2, "scaleway image is required")
	}
	local, err := client.Marketplace().GetLocalImageByLabel(&marketplace.GetLocalImageByLabelRequest{
		ImageLabel:     image,
		Zone:           scw.Zone(client.Zone()),
		CommercialType: serverTypeForConfig(cfg),
		Type:           marketplace.LocalImageTypeInstanceLocal,
	}, scw.WithContext(ctx))
	if err == nil && local != nil && local.ID != "" {
		return local.ID, nil
	}
	if strings.Contains(image, "-") || strings.Contains(image, "_") {
		return image, nil
	}
	return "", err
}

func (b *Backend) targetFromServer(ctx context.Context, client Client, item *instance.Server, req core.ResolveRequest) (core.LeaseTarget, error) {
	if item == nil {
		return core.LeaseTarget{}, core.Exit(4, "lease/scaleway server not found: %s", req.ID)
	}
	server := b.serverFromScaleway(item)
	if err := validateScalewayLabels(server.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.validateProviderIdentity(server.Labels, client); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := server.Labels["lease"]
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	ssh := core.SSHTargetFromConfig(b.cfgForRun(), server.PublicNet.IPv4.IP)
	core.UseStoredTestboxKey(&ssh, leaseID)
	if req.Repo.Root != "" && !req.NoLocalStateMutations {
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], b.cfgForRun(), server, ssh, req.Repo.Root, b.cfgForRun().IdleTimeout, req.Reclaim, claim, exists); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	_ = ctx
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
}

func (b *Backend) releaseTargetFromClaim(ctx context.Context, client Client, id string) (core.LeaseTarget, error) {
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
	if !ok {
		return core.LeaseTarget{}, core.Exit(4, "lease/scaleway server not found: %s", id)
	}
	if err := validateScalewayLabels(claim.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.validateProviderIdentity(claim.Labels, client); err != nil {
		return core.LeaseTarget{}, err
	}
	if strings.TrimSpace(claim.CloudID) != "" {
		resp, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: claim.CloudID}, scw.WithContext(ctx))
		if err == nil && resp != nil && resp.Server != nil {
			server := b.serverFromScaleway(resp.Server)
			if err := validateScalewayLabels(server.Labels); err != nil {
				return core.LeaseTarget{}, err
			}
			if err := b.validateProviderIdentity(server.Labels, client); err != nil {
				return core.LeaseTarget{}, err
			}
			if server.Labels["lease"] != claim.LeaseID || server.Labels["slug"] != claim.Slug {
				return core.LeaseTarget{}, core.Exit(2, "refusing to release Scaleway server %s from stale local claim", claim.CloudID)
			}
			return core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}, nil
		}
		if err != nil && !isScalewayNotFound(err) {
			return core.LeaseTarget{}, err
		}
	}
	server := core.Server{Provider: providerName, CloudID: claim.CloudID, Name: claim.Slug, Labels: claim.Labels}
	if parsed, err := strconv.ParseInt(claim.CloudID, 10, 64); err == nil {
		server.ID = parsed
	}
	server.PublicNet.IPv4.IP = claim.SSHHost
	ssh := core.SSHTargetFromConfig(b.cfgForRun(), claim.SSHHost)
	if claim.SSHPort > 0 {
		ssh.Port = strconv.Itoa(claim.SSHPort)
	}
	core.UseStoredTestboxKey(&ssh, claim.LeaseID)
	return core.LeaseTarget{Server: server, LeaseID: claim.LeaseID, SSH: ssh}, nil
}

func (b *Backend) deleteServer(ctx context.Context, client Client, server core.Server) error {
	if err := validateScalewayLabels(server.Labels); err != nil {
		return err
	}
	if err := b.validateProviderIdentity(server.Labels, client); err != nil {
		return err
	}
	leaseID := server.Labels["lease"]
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if exists && claim.Provider == providerName {
		if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
			return core.Exit(2, "refusing to release Scaleway server %s from stale local claim", server.CloudID)
		}
	}
	if server.CloudID == "" {
		if server.Labels["recovery"] == "rollback-key-cleanup" {
			return b.deleteIdentitylessRecoveryKey(ctx, client, server, claim, exists)
		}
		// Ambiguous create responses may represent a server that has not reached
		// inventory yet. Retain its access key and claim for explicit recovery.
		return core.Exit(4, "scaleway recovery claim for lease=%s has no server identity; credentials and claim retained", leaseID)
	}
	keyID := strings.TrimSpace(server.Labels["scaleway_ssh_key_id"])
	resp, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: server.CloudID}, scw.WithContext(ctx))
	if err != nil && !isScalewayNotFound(err) {
		return err
	}
	if err == nil {
		if resp == nil || resp.Server == nil {
			return core.Exit(4, "scaleway server not found: %s", server.CloudID)
		}
		live := b.serverFromScaleway(resp.Server)
		if err := b.validateLiveServer(live, server); err != nil {
			return err
		}
		if err := b.validateProviderIdentity(live.Labels, client); err != nil {
			return err
		}
		if liveKeyID := strings.TrimSpace(live.Labels["scaleway_ssh_key_id"]); liveKeyID != "" {
			if keyID != "" && keyID != liveKeyID {
				return core.Exit(2, "refusing to release Scaleway server %s with changed SSH key identity", server.CloudID)
			}
			keyID = liveKeyID
		}
		if err := b.deleteServerResource(ctx, client, server.CloudID); err != nil {
			return err
		}
	}
	if keyID != "" {
		if err := client.IAM().DeleteSSHKey(&iam.DeleteSSHKeyRequest{SSHKeyID: keyID}, scw.WithContext(ctx)); err != nil && !isScalewayNotFound(err) {
			return err
		}
	}
	if exists && claim.Provider == providerName {
		if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return fmt.Errorf("finalize Scaleway cleanup claim: %w", err)
		}
	} else {
		core.RemoveLeaseClaim(leaseID)
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *Backend) deleteServerResource(ctx context.Context, client Client, serverID string) error {
	zone := scw.Zone(client.Zone())
	resp, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: zone, ServerID: serverID}, scw.WithContext(ctx))
	if err != nil {
		if isScalewayNotFound(err) {
			return nil
		}
		return err
	}
	if resp == nil || resp.Server == nil {
		return core.Exit(4, "scaleway server not found: %s", serverID)
	}

	state := resp.Server.State
	if state != instance.ServerStateStopped && state != instance.ServerStateStoppedInPlace {
		if state != instance.ServerStateStopping {
			if _, err := client.Instance().ServerAction(&instance.ServerActionRequest{
				Zone:     zone,
				ServerID: serverID,
				Action:   instance.ServerActionPoweroff,
			}, scw.WithContext(ctx)); err != nil {
				if isScalewayNotFound(err) {
					return nil
				}
				return fmt.Errorf("power off Scaleway server %s before deletion: %w", serverID, err)
			}
		}

		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		for {
			resp, err = client.Instance().GetServer(&instance.GetServerRequest{Zone: zone, ServerID: serverID}, scw.WithContext(waitCtx))
			if err != nil {
				if isScalewayNotFound(err) {
					return nil
				}
				return err
			}
			if resp != nil && resp.Server != nil && (resp.Server.State == instance.ServerStateStopped || resp.Server.State == instance.ServerStateStoppedInPlace) {
				break
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-waitCtx.Done():
				timer.Stop()
				return fmt.Errorf("wait for Scaleway server %s to stop before deletion: %w", serverID, waitCtx.Err())
			case <-timer.C:
			}
		}
	}

	if err := client.Instance().DeleteServer(&instance.DeleteServerRequest{Zone: zone, ServerID: serverID}, scw.WithContext(ctx)); err != nil && !isScalewayNotFound(err) {
		return err
	}
	return nil
}

func (b *Backend) deleteIdentitylessRecoveryKey(ctx context.Context, client Client, server core.Server, claim core.LeaseClaim, claimExists bool) error {
	leaseID := server.Labels["lease"]
	keyID := strings.TrimSpace(server.Labels["scaleway_ssh_key_id"])
	if keyID == "" {
		return core.Exit(4, "scaleway recovery claim for lease=%s has no SSH key identity; claim retained", leaseID)
	}
	if err := client.IAM().DeleteSSHKey(&iam.DeleteSSHKeyRequest{SSHKeyID: keyID}, scw.WithContext(ctx)); err != nil && !isScalewayNotFound(err) {
		return err
	}
	if claimExists && claim.Provider == providerName {
		if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return fmt.Errorf("finalize Scaleway recovery-key cleanup claim: %w", err)
		}
	} else {
		core.RemoveLeaseClaim(leaseID)
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *Backend) waitForPublicIPv4(ctx context.Context, client Client, serverID string) (*instance.Server, error) {
	deadline := b.clockNow().Add(5 * time.Minute)
	for {
		resp, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: serverID}, scw.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if resp != nil && resp.Server != nil && publicIPv4(resp.Server) != "" {
			return resp.Server, nil
		}
		if b.clockNow().After(deadline) {
			return nil, core.Exit(5, "timed out waiting for Scaleway Instance public IPv4")
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *Backend) persistRecoveryClaim(leaseID, slug string, cfg core.Config, repoRoot string, client Client, serverID, host, keyID, keyName, recovery string, keep bool, now time.Time) error {
	labels := labelsFromTags(leaseTags(cfg, leaseID, slug, "provisioning", keep, now))
	labels["recovery"] = recovery
	labels["scaleway_project"] = client.ProjectID()
	labels["scaleway_organization"] = client.OrganizationID()
	labels["scaleway_region"] = client.Region()
	labels["scaleway_zone"] = client.Zone()
	labels["scaleway_ssh_key_id"] = keyID
	labels["scaleway_ssh_key_name"] = keyName
	server := core.Server{Provider: providerName, CloudID: serverID, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	server.PublicNet.IPv4.IP = host
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false)
}

func (b *Backend) serverFromScaleway(item *instance.Server) core.Server {
	if item == nil {
		return core.Server{Provider: providerName}
	}
	labels := labelsFromTags(item.Tags)
	if labels["provider_key"] == "" && labels["lease"] != "" {
		labels["provider_key"] = core.ProviderKeyForLease(labels["lease"])
	}
	if labels["scaleway_project"] == "" {
		labels["scaleway_project"] = item.Project
	}
	if labels["scaleway_organization"] == "" {
		labels["scaleway_organization"] = item.Organization
	}
	if labels["scaleway_zone"] == "" {
		labels["scaleway_zone"] = string(item.Zone)
	}
	server := core.Server{
		CloudID:  item.ID,
		Provider: providerName,
		Name:     item.Name,
		Status:   normalizeScalewayStatus(item.State),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = publicIPv4(item)
	server.ServerType.Name = item.CommercialType
	return server
}

func (b *Backend) ownedServer(item *instance.Server) bool {
	if item == nil {
		return false
	}
	labels := labelsFromTags(item.Tags)
	if err := validateScalewayLabels(labels); err != nil {
		return false
	}
	if labels["scaleway_project"] != "" && item.Project != "" && labels["scaleway_project"] != item.Project {
		return false
	}
	if labels["scaleway_zone"] != "" && item.Zone != "" && labels["scaleway_zone"] != string(item.Zone) {
		return false
	}
	return true
}

func (b *Backend) validateLiveServer(live, expected core.Server) error {
	if err := validateScalewayLabels(live.Labels); err != nil {
		return err
	}
	if live.CloudID != expected.CloudID ||
		live.Labels["lease"] != expected.Labels["lease"] ||
		live.Labels["slug"] != expected.Labels["slug"] ||
		live.Labels["provider_key"] != expected.Labels["provider_key"] {
		return core.Exit(2, "refusing to operate on changed Scaleway server %s", expected.CloudID)
	}
	return nil
}

func (b *Backend) validateProviderIdentity(labels map[string]string, client Client) error {
	if expected := strings.TrimSpace(labels["scaleway_project"]); expected != "" && client.ProjectID() != "" && expected != client.ProjectID() {
		return core.Exit(3, "scaleway project mismatch: current project %s does not match lease project %s", client.ProjectID(), expected)
	}
	if expected := strings.TrimSpace(labels["scaleway_zone"]); expected != "" && client.Zone() != "" && expected != client.Zone() {
		return core.Exit(3, "scaleway zone mismatch: current zone %s does not match lease zone %s", client.Zone(), expected)
	}
	return nil
}

func (b *Backend) clockNow() time.Time {
	if b.now != nil {
		return b.now().UTC()
	}
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func scalewayServerTypeForClass(class string) string {
	switch class {
	case "standard", "fast", "large", "beast":
		return "DEV1-S"
	default:
		return "DEV1-S"
	}
}

func validateScalewayLabels(labels map[string]string) error {
	if labels == nil ||
		labels[ownershipTagConflictLabel] != "" ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		labels["lease"] == "" ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox Scaleway Instance")
	}
	return nil
}

func replaceCrabboxTags(existing, desired []string) []string {
	tags := append([]string(nil), desired...)
	for _, tag := range existing {
		lower := strings.ToLower(strings.TrimSpace(tag))
		if lower == tagCrabbox || strings.HasPrefix(lower, tagPrefix) {
			continue
		}
		tags = append(tags, tag)
	}
	return normalizeTags(tags)
}

func publicIPv4(item *instance.Server) string {
	if item == nil {
		return ""
	}
	if item.PublicIP != nil && strings.Contains(item.PublicIP.Address.String(), ".") {
		return item.PublicIP.Address.String()
	}
	for _, ip := range item.PublicIPs {
		if ip != nil && strings.Contains(ip.Address.String(), ".") {
			return ip.Address.String()
		}
	}
	return ""
}

func normalizeScalewayStatus(state instance.ServerState) string {
	switch state {
	case instance.ServerStateRunning:
		return "ready"
	case "":
		return "unknown"
	default:
		return string(state)
	}
}

func providerKeyName(leaseID string) string {
	return strings.ReplaceAll("crabbox-"+leaseID, "_", "-")
}

func ptrTags(tags []string) *[]string {
	return &tags
}

func isScalewayNotFound(err error) bool {
	var notFound *scw.ResourceNotFoundError
	return errors.As(err, &notFound)
}

func isAmbiguousScalewayError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"eof", "timeout", "timed out", "connection reset", "broken pipe", "transport is closing"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func splitCommaList(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
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
