package agx

import (
	"context"
	"flag"
	"fmt"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type agxFlagValues struct {
	APIURL    *string
	Workspace *string
	User      *string
	WorkRoot  *string
	Region    *string
	Image     *string
}

func RegisterAGXProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return agxFlagValues{
		APIURL:    fs.String("agx-api-url", defaults.AGX.APIURL, "AGX control-plane API URL"),
		Workspace: fs.String("agx-workspace", defaults.AGX.Workspace, "AGX SSH workspace gateway host"),
		User:      fs.String("agx-user", defaults.AGX.User, "AGX in-VM SSH login user"),
		WorkRoot:  fs.String("agx-work-root", defaults.AGX.WorkRoot, "AGX remote work root"),
		Region:    fs.String("agx-region", defaults.AGX.Region, "AGX region"),
		Image:     fs.String("agx-image", defaults.AGX.Image, "AGX base image or snapshot"),
	}
}

func ApplyAGXProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == agxProvider {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=agx")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=agx")
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=agx supports target=linux only")
		}
		if err := validateAGXOptions(*cfg); err != nil {
			return err
		}
	}
	v, ok := values.(agxFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "agx-api-url") {
		cfg.AGX.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "agx-workspace") {
		cfg.AGX.Workspace = *v.Workspace
	}
	if flagWasSet(fs, "agx-user") {
		cfg.AGX.User = *v.User
	}
	if flagWasSet(fs, "agx-work-root") {
		cfg.AGX.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "agx-region") {
		cfg.AGX.Region = *v.Region
	}
	if flagWasSet(fs, "agx-image") {
		cfg.AGX.Image = *v.Image
	}
	return nil
}

func NewAGXBackend(spec ProviderSpec, cfg Config, rt Runtime) (Backend, error) {
	if err := validateAGXOptions(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = agxProvider
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHUser = agxVMUser(cfg)
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.AGX.WorkRoot) != "" {
		cfg.WorkRoot = cfg.AGX.WorkRoot
	}
	if strings.TrimSpace(cfg.AGX.Token) == "" {
		return nil, exit(3, "provider=agx requires an API key in CRABBOX_AGX_API_KEY, AGX_API_KEY, or AGX_TOKEN")
	}
	client := newAGXClient(cfg, rt)
	return &agxBackend{spec: spec, cfg: cfg, rt: rt, client: client}, nil
}

func validateAGXOptions(cfg Config) error {
	if cfg.Tailscale.Enabled {
		return exit(2, "--tailscale is not supported for provider=agx; AGX exposes SSH through its workspace gateway")
	}
	if cfg.Network == core.NetworkTailscale {
		return exit(2, "--network=tailscale is not supported for provider=agx; AGX exposes SSH through its workspace gateway")
	}
	if err := cleanAGXWorkRoot(cfg.AGX.WorkRoot); err != nil {
		return err
	}
	return nil
}

type agxBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client agxAPI
}

func (b *agxBackend) Spec() ProviderSpec { return b.spec }

func (b *agxBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	useStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *agxBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := leaseProviderName(leaseID, slug)
	keyPath, publicKey, err := ensureTestboxKey(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.configForRun()
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=agx lease=%s slug=%s instance=%s keep=%v\n", leaseID, slug, name, req.Keep)
	instance, err := b.client.CreateInstance(ctx, agxCreateRequest{
		Name:      name,
		PublicKey: publicKey,
		Image:     strings.TrimSpace(cfg.AGX.Image),
		Region:    strings.TrimSpace(cfg.AGX.Region),
		Labels:    agxAPILabels(leaseID, slug),
	})
	if err != nil {
		return LeaseTarget{}, agxError("create instance", err)
	}
	if instance.ID == "" {
		instance.ID = name
	}
	cleanupFailedAcquire := func() {
		if req.Keep {
			return
		}
		if err := b.client.DeleteInstance(context.Background(), instance.ID); err == nil {
			removeStoredTestboxKey(leaseID)
		}
	}
	lease, err := b.prepareLease(ctx, instance, leaseID, slug, req.Keep, keyPath)
	if err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, agxProvider, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		cleanupFailedAcquire()
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, instance.ID)
	return lease, nil
}

func (b *agxBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	id, leaseID, slug, err := b.resolveInstanceID(ctx, req.ID, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		instance := agxInstance{ID: id, Name: leaseProviderName(leaseID, slug), Labels: agxAPILabels(leaseID, slug)}
		return LeaseTarget{Server: b.instanceToServer(instance, true), LeaseID: leaseID}, nil
	}
	instance, err := b.client.GetInstance(ctx, id)
	if err != nil {
		return LeaseTarget{}, agxError("get instance", err)
	}
	keyPath, _, err := ensureTestboxKey(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, instance, leaseID, slug, true, keyPath)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, agxProvider, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *agxBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	instances, err := b.client.ListInstances(ctx, leaseProviderNamePrefix())
	if err != nil {
		return nil, agxError("list instances", err)
	}
	out := make([]Server, 0, len(instances))
	for _, instance := range instances {
		if !isCrabboxInstance(instance) {
			continue
		}
		out = append(out, b.instanceToServer(instance, true))
	}
	return out, nil
}

func (b *agxBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(agxProvider, len(servers)), nil
}

func (b *agxBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	id := strings.TrimSpace(req.Lease.Server.CloudID)
	if id == "" {
		var err error
		id, _, _, err = b.resolveInstanceID(ctx, req.Lease.LeaseID, false)
		if err != nil {
			return err
		}
	}
	if ok, err := agxLeaseHasClaim(req.Lease.LeaseID); err != nil {
		return err
	} else if !ok {
		instance, err := b.client.GetInstance(ctx, id)
		if err != nil {
			if isAGXNotFound(err) {
				removeLeaseClaim(req.Lease.LeaseID)
				removeStoredTestboxKey(req.Lease.LeaseID)
				return nil
			}
			return agxError("get instance", err)
		}
		if !isCrabboxInstance(instance) {
			return exit(4, "agx instance %q is not Crabbox-managed; use --reclaim to adopt it before release", id)
		}
	}
	if err := b.client.DeleteInstance(ctx, id); err != nil {
		if !isAGXNotFound(err) {
			return agxError("delete instance", err)
		}
	}
	removeLeaseClaim(req.Lease.LeaseID)
	removeStoredTestboxKey(req.Lease.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s instance=%s\n", req.Lease.LeaseID, id)
	return nil
}

func (b *agxBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *agxBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if b.rt.Clock != nil {
		now = b.rt.Clock.Now().UTC()
	}
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stderr, "delete instance id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if err := b.client.DeleteInstance(ctx, server.CloudID); err != nil && !isAGXNotFound(err) {
			return agxError("delete instance", err)
		}
	}
	return nil
}

func (b *agxBackend) configForRun() Config {
	cfg := b.cfg
	cfg.Provider = agxProvider
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHUser = agxVMUser(cfg)
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.AGX.WorkRoot) != "" {
		cfg.WorkRoot = cfg.AGX.WorkRoot
	}
	return cfg
}

func (b *agxBackend) prepareLease(ctx context.Context, instance agxInstance, leaseID, slug string, keep bool, keyPath string) (LeaseTarget, error) {
	cfg := b.configForRun()
	if err := cleanAGXWorkRoot(cfg.WorkRoot); err != nil {
		return LeaseTarget{}, err
	}
	target := b.agxSSHTarget(instance, keyPath)
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
	server := b.instanceToServer(instance, keep)
	server.Labels["lease"] = leaseID
	server.Labels["slug"] = slug
	server.Labels["keep"] = fmt.Sprint(keep)
	server.Labels["work_root"] = cfg.WorkRoot
	server.Labels["state"] = "ready"
	server.Status = "ready"
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "agx ssh", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *agxBackend) resolveInstanceID(ctx context.Context, identifier string, reclaim bool) (string, string, string, error) {
	if strings.TrimSpace(identifier) == "" {
		return "", "", "", exit(2, "provider=agx requires a Crabbox lease id, slug, or AGX instance id")
	}
	if claim, ok, err := resolveLeaseClaim(identifier); err != nil {
		return "", "", "", err
	} else if ok {
		if claim.Provider != "" && claim.Provider != agxProvider {
			return "", "", "", exit(4, "lease %q is claimed for provider=%s, not agx", identifier, claim.Provider)
		}
		if id, ok := instanceIDFromClaim(claim); ok {
			return id, claim.LeaseID, claim.Slug, nil
		}
	}
	if strings.HasPrefix(identifier, "cbx_") {
		instance, err := b.findInstanceByLease(ctx, identifier)
		if err != nil {
			return "", "", "", err
		}
		return instance.ID, identifier, agxSlug(identifier, instance), nil
	}
	instance, err := b.client.GetInstance(ctx, identifier)
	if err != nil {
		if isAGXNotFound(err) {
			return "", "", "", exit(4, "agx lease or instance %q was not found", identifier)
		}
		return "", "", "", agxError("get instance", err)
	}
	if !isCrabboxInstance(instance) && !reclaim {
		return "", "", "", exit(4, "agx instance %q is not Crabbox-managed; use --reclaim to adopt it", identifier)
	}
	leaseID := agxLeaseID(instance)
	if leaseID == "" {
		leaseID = "agx_" + normalizeLeaseSlug(blank(instance.Name, instance.ID))
	}
	return instance.ID, leaseID, agxSlug(leaseID, instance), nil
}

func instanceIDFromClaim(claim LeaseClaim) (string, bool) {
	if id := strings.TrimSpace(claim.Labels["instance_id"]); id != "" {
		return id, true
	}
	if strings.HasPrefix(claim.LeaseID, "agx_") {
		return strings.TrimPrefix(claim.LeaseID, "agx_"), true
	}
	if strings.HasPrefix(claim.LeaseID, "cbx_") {
		return leaseProviderName(claim.LeaseID, claim.Slug), true
	}
	return "", false
}

func agxLeaseHasClaim(leaseID string) (bool, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return false, nil
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		return false, err
	}
	return claim.Provider == "" || claim.Provider == agxProvider, nil
}

func (b *agxBackend) findInstanceByLease(ctx context.Context, leaseID string) (agxInstance, error) {
	instances, err := b.client.ListInstances(ctx, leaseProviderNamePrefix())
	if err != nil {
		return agxInstance{}, agxError("list instances", err)
	}
	for _, instance := range instances {
		if agxLeaseID(instance) == leaseID {
			return instance, nil
		}
	}
	return agxInstance{}, exit(4, "agx lease %q was not found", leaseID)
}

func (b *agxBackend) instanceToServer(instance agxInstance, keep bool) Server {
	leaseID := agxLeaseID(instance)
	slug := agxSlug(leaseID, instance)
	cfg := b.configForRun()
	labels := directLeaseLabels(cfg, leaseID, slug, agxProvider, "", keep, time.Now().UTC())
	labels["name"] = instance.Name
	labels["instance_id"] = instance.ID
	labels["state"] = agxState(instance.Status)
	labels["work_root"] = cfg.WorkRoot
	if instance.Region != "" {
		labels["region"] = instance.Region
	}
	server := Server{
		CloudID:  instance.ID,
		HostID:   instance.ID,
		Provider: agxProvider,
		Name:     blank(instance.Name, instance.ID),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = "microvm"
	server.PublicNet.IPv4.IP = b.workspaceHost(instance)
	return server
}

func (b *agxBackend) agxSSHTarget(instance agxInstance, keyPath string) SSHTarget {
	port := "22"
	if instance.SSHPort > 0 {
		port = fmt.Sprint(instance.SSHPort)
	}
	return SSHTarget{
		User:                   b.sshUser(instance),
		Host:                   b.workspaceHost(instance),
		Key:                    keyPath,
		Port:                   port,
		TargetOS:               targetLinux,
		NetworkKind:            networkPublic,
		DisableHostKeyChecking: true,
	}
}

// sshUser builds the AGX gateway login. AGX routes to an instance through the
// `<user>+<instance>` SSH username (ssh user+instance@workspace.agx.so). When
// the control plane already returns a fully-formed ssh_user, trust it.
func (b *agxBackend) sshUser(instance agxInstance) string {
	if user := strings.TrimSpace(instance.SSHUser); user != "" {
		return user
	}
	id := strings.TrimSpace(instance.ID)
	if id == "" {
		return agxVMUser(b.cfg)
	}
	return agxVMUser(b.cfg) + "+" + id
}

func (b *agxBackend) workspaceHost(instance agxInstance) string {
	if host := strings.TrimSpace(instance.SSHHost); host != "" {
		return host
	}
	return blank(strings.TrimSpace(b.cfg.AGX.Workspace), "workspace.agx.so")
}

func agxVMUser(cfg Config) string {
	return blank(strings.TrimSpace(cfg.AGX.User), "root")
}

func leaseProviderNamePrefix() string {
	return "crabbox-"
}

func agxState(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "ready"
	}
	return status
}

func cleanAGXWorkRoot(workRoot string) error {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return exit(2, "agx.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var":
		return exit(2, "agx.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}
