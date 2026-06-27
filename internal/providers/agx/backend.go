package agx

import (
	"context"
	"flag"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	core "github.com/openclaw/crabbox/internal/cli"
)

type agxFlagValues struct {
	Workspace *string
	User      *string
	WorkRoot  *string
}

func RegisterAGXProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return agxFlagValues{
		Workspace: fs.String("agx-workspace", defaults.AGX.Workspace, "AGX SSH workspace gateway host"),
		User:      fs.String("agx-user", defaults.AGX.User, "AGX in-VM SSH login user (the <user> in <user>+<instance>)"),
		WorkRoot:  fs.String("agx-work-root", defaults.AGX.WorkRoot, "AGX remote work root"),
	}
}

func ApplyAGXProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if v, ok := values.(agxFlagValues); ok {
		if flagWasSet(fs, "agx-workspace") {
			cfg.AGX.Workspace = *v.Workspace
		}
		if flagWasSet(fs, "agx-user") {
			cfg.AGX.User = *v.User
		}
		if flagWasSet(fs, "agx-work-root") {
			cfg.AGX.WorkRoot = *v.WorkRoot
		}
	}
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
	return nil
}

func NewAGXBackend(spec ProviderSpec, cfg Config, rt Runtime) (Backend, error) {
	if err := validateAGXOptions(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = agxProvider
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.AGX.WorkRoot) != "" {
		cfg.WorkRoot = cfg.AGX.WorkRoot
	}
	return &agxBackend{spec: spec, cfg: cfg, rt: rt}, nil
}

func validateAGXOptions(cfg Config) error {
	if cfg.Tailscale.Enabled {
		return exit(2, "--tailscale is not supported for provider=agx; AGX exposes SSH through its workspace gateway")
	}
	if cfg.Network == core.NetworkTailscale {
		return exit(2, "--network=tailscale is not supported for provider=agx; AGX exposes SSH through its workspace gateway")
	}
	if err := cleanAGXWorkspace(cfg.AGX.Workspace); err != nil {
		return err
	}
	if err := cleanAGXUser(cfg.AGX.User); err != nil {
		return err
	}
	if err := cleanAGXWorkRoot(cfg.AGX.WorkRoot); err != nil {
		return err
	}
	return nil
}

// agxBackend is an SSH-lease backend with no control-plane client. AGX exposes
// sandboxes only over SSH ("no SDK required, no custom client"), so Crabbox
// connects to <user>+<instance>@<workspace> with the operator's own SSH key and
// lets AGX provision the microVM on connect. There is no published API to
// create, enumerate, or delete instances, so List/Resolve are backed by local
// Crabbox lease claims and release only drops local state.
type agxBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *agxBackend) Spec() ProviderSpec { return b.spec }

func (b *agxBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	instance := agxInstanceName(slug)
	cfg := b.configForRun()
	target := b.agxSSHTarget(instance)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=agx lease=%s slug=%s instance=%s user=%s host=%s keep=%v\n", leaseID, slug, instance, target.User, target.Host, req.Keep)
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "agx ssh", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	server := b.instanceServer(leaseID, slug, instance, "ready")
	lease := LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, instance)
	return lease, nil
}

func (b *agxBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	if claim, ok, err := resolveLeaseClaimForProvider(req.ID, agxProvider); err != nil {
		return LeaseTarget{}, err
	} else if ok {
		lease := b.leaseFromClaim(claim)
		if req.ReleaseOnly {
			return LeaseTarget{Server: lease.Server, LeaseID: lease.LeaseID}, nil
		}
		if err := waitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "agx ssh", bootstrapWaitTimeout(b.cfg)); err != nil {
			return LeaseTarget{}, err
		}
		return lease, nil
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return LeaseTarget{}, exit(2, "provider=agx requires a Crabbox lease id, slug, or AGX instance name")
	}
	slug := normalizeLeaseSlug(id)
	leaseID := "agx_" + slug
	instance := agxInstanceName(slug)
	server := b.instanceServer(leaseID, slug, instance, "ready")
	if req.ReleaseOnly {
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	target := b.agxSSHTarget(instance)
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "agx ssh", bootstrapWaitTimeout(b.cfg)); err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseTargetForRepoConfig(leaseID, slug, b.configForRun(), server, target, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *agxBackend) List(_ context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != agxProvider {
			continue
		}
		out = append(out, b.leaseFromClaim(claim).Server)
	}
	return out, nil
}

func (b *agxBackend) Doctor(_ context.Context, _ DoctorRequest) (DoctorResult, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return DoctorResult{}, err
	}
	leases := 0
	for _, claim := range claims {
		if claim.Provider == agxProvider {
			leases++
		}
	}
	return DoctorResult{
		Provider: agxProvider,
		Message:  fmt.Sprintf("transport=ssh auth=ssh-key control_plane=none workspace=%s user=%s leases=%d runtime=unchecked", b.workspaceHost(""), b.vmUser(), leases),
	}, nil
}

func (b *agxBackend) ReleaseLease(_ context.Context, req ReleaseLeaseRequest) error {
	removeLeaseClaim(req.Lease.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s instance=%s (local claim removed; AGX reclaims idle sandboxes)\n", req.Lease.LeaseID, blank(req.Lease.Server.CloudID, "-"))
	return nil
}

func (b *agxBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released agx lease=%s instance=%s", lease.LeaseID, blank(lease.Server.CloudID, "-"))
}

func (b *agxBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *agxBackend) configForRun() Config {
	cfg := b.cfg
	cfg.Provider = agxProvider
	cfg.TargetOS = targetLinux
	cfg.Network = networkPublic
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.AGX.WorkRoot) != "" {
		cfg.WorkRoot = cfg.AGX.WorkRoot
	}
	return cfg
}

func (b *agxBackend) leaseFromClaim(claim LeaseClaim) LeaseTarget {
	instance := blank(claim.CloudID, blank(claim.Labels["instance_id"], agxInstanceName(claim.Slug)))
	target := b.agxSSHTarget(instance)
	if claim.SSHHost != "" {
		target.Host = claim.SSHHost
	}
	if claim.SSHPort != 0 {
		target.Port = fmt.Sprint(claim.SSHPort)
	}
	if claim.StaticUser != "" {
		target.User = claim.StaticUser
	}
	server := b.instanceServer(claim.LeaseID, claim.Slug, instance, "ready")
	return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}
}

func (b *agxBackend) instanceServer(leaseID, slug, instance, state string) Server {
	cfg := b.configForRun()
	labels := directLeaseLabels(cfg, leaseID, slug, agxProvider, "", false, time.Now().UTC())
	labels["name"] = instance
	labels["instance_id"] = instance
	labels["state"] = agxState(state)
	labels["work_root"] = cfg.WorkRoot
	server := Server{
		CloudID:  instance,
		HostID:   instance,
		Provider: agxProvider,
		Name:     instance,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = "microvm"
	server.PublicNet.IPv4.IP = b.workspaceHost("")
	return server
}

// agxSSHTarget builds the AGX gateway login. AGX routes to a sandbox through the
// `<user>+<instance>` SSH username (ssh user+instance@workspace.agx.so) and
// authenticates with the operator's own SSH key, so the key comes from the
// standard Crabbox SSH config (cfg.SSHKey) rather than a per-lease Crabbox key.
func (b *agxBackend) agxSSHTarget(instance string) SSHTarget {
	return SSHTarget{
		User:        b.vmUser() + "+" + instance,
		Host:        b.workspaceHost(""),
		Key:         strings.TrimSpace(b.cfg.SSHKey),
		Port:        "22",
		TargetOS:    targetLinux,
		NetworkKind: networkPublic,
	}
}

func (b *agxBackend) vmUser() string {
	return blank(strings.TrimSpace(b.cfg.AGX.User), defaultVMUser)
}

func (b *agxBackend) workspaceHost(override string) string {
	if host := strings.TrimSpace(override); host != "" {
		return host
	}
	return blank(strings.TrimSpace(b.cfg.AGX.Workspace), defaultWorkspace)
}

// agxInstanceName derives the AGX instance identifier from a Crabbox slug. The
// slug is stable and human-readable, so the same lease reconnects to the same
// `<user>+<instance>` address.
func agxInstanceName(slug string) string {
	return normalizeLeaseSlug(slug)
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

func cleanAGXUser(user string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil
	}
	if strings.HasPrefix(user, "-") {
		return exit(2, "agx.user %q must be a plain SSH login user, not an ssh option", user)
	}
	for _, r := range user {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return exit(2, "agx.user %q must not contain whitespace or control characters", user)
		}
		if !isAGXUserRune(r) {
			return exit(2, "agx.user %q may contain only letters, numbers, '.', '_', and '-'", user)
		}
	}
	return nil
}

func isAGXUserRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '.' ||
		r == '_' ||
		r == '-'
}

func cleanAGXWorkspace(workspace string) error {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return nil
	}
	if strings.HasPrefix(workspace, "-") {
		return exit(2, "agx.workspace %q must be a hostname, not an ssh option", workspace)
	}
	if strings.Contains(workspace, "://") ||
		strings.ContainsAny(workspace, "/\\@:") ||
		containsSpaceOrControl(workspace) {
		return exit(2, "agx.workspace %q must be a hostname only, without scheme, port, path, userinfo, or whitespace", workspace)
	}
	if len(workspace) > 253 {
		return exit(2, "agx.workspace %q is too long to be a hostname", workspace)
	}
	labels := strings.Split(workspace, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return exit(2, "agx.workspace %q must be a valid hostname", workspace)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return exit(2, "agx.workspace %q must be a valid hostname", workspace)
		}
		for _, r := range label {
			if !isAGXHostnameRune(r) {
				return exit(2, "agx.workspace %q must be a valid hostname", workspace)
			}
		}
	}
	return nil
}

func isAGXHostnameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-'
}

func containsSpaceOrControl(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}
