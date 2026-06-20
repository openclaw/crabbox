package nebius

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	shared.DirectSSHBackend
	clientFactory func(Runtime) nebiusAPI
	waitSSH       func(context.Context, *core.SSHTarget, string, time.Duration) error
	now           func() time.Time
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	b := &backend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true},
		now:              time.Now,
	}
	b.clientFactory = func(rt Runtime) nebiusAPI { return newNebiusClient(cfg.Nebius, rt) }
	b.waitSSH = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, b.RT.Stderr, phase, timeout)
	}
	b.Delete = b.deleteServer
	return b
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req)
	})
}

func (b *backend) acquireOnce(ctx context.Context, req AcquireRequest) (target LeaseTarget, err error) {
	cfg := b.Cfg
	if err := validateNebiusAcquireConfig(cfg); err != nil {
		return LeaseTarget{}, err
	}
	client := b.clientFactory(b.RT)
	leaseID := core.NewLeaseID()
	existing, err := client.ListInstances(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := ownedServers(existing, cfg)
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	committed := false
	retainKey := false
	defer func() {
		if err != nil && !committed && !retainKey {
			if !isIndeterminateNebiusError(err) {
				core.RemoveStoredTestboxKey(leaseID)
			}
		}
	}()
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	cfg.ServerType = nebiusServerType(cfg)
	now := b.now().UTC()
	labels := nebiusLeaseLabels(cfg, leaseID, slug, "provisioning", req.Keep, now)
	created := nebiusInstance{}
	userData, err := renderNebiusCloudInit(cfg, publicKey)
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=nebius lease=%s slug=%s platform=%s preset=%s keep=%v\n", leaseID, slug, cfg.Nebius.Platform, cfg.Nebius.Preset, req.Keep)
	created, err = client.CreateInstance(ctx, nebiusCreateRequest{
		Name:      core.LeaseProviderName(leaseID, slug),
		Labels:    labels,
		UserData:  userData,
		PublicKey: publicKey,
	})
	if err != nil {
		if isIndeterminateNebiusError(err) {
			_ = b.persistRecoveryClaim(leaseID, slug, "", cfg, req.Repo.Root, labels, req.Reclaim)
		}
		return LeaseTarget{}, err
	}
	defer func() {
		if err == nil || committed || strings.TrimSpace(created.ID) == "" {
			return
		}
		recoveryLabels := cloneStringMap(labels)
		if req.Keep {
			recoveryLabels["recovery"] = "kept-after-failure"
			retainKey = true
			if claimErr := b.persistRecoveryClaim(leaseID, slug, created.ID, cfg, req.Repo.Root, recoveryLabels, req.Reclaim); claimErr != nil {
				err = errors.Join(err, fmt.Errorf("persist kept nebius recovery: %w", claimErr))
			}
			return
		}
		claimErr := b.persistRecoveryClaim(leaseID, slug, created.ID, cfg, req.Repo.Root, recoveryLabels, req.Reclaim)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := client.DeleteInstance(cleanupCtx, created.ID); cleanupErr != nil && !isNebiusInstanceNotFound(cleanupErr, created.ID) {
			err = fmt.Errorf("%v; nebius rollback failed: %w", err, errors.Join(claimErr, cleanupErr))
			return
		}
		if claimErr == nil {
			core.RemoveLeaseClaim(leaseID)
		}
		core.RemoveStoredTestboxKey(leaseID)
	}()
	if req.OnAcquired != nil {
		if err := req.OnAcquired(core.LeaseTarget{LeaseID: leaseID, Server: serverFromInstance(created, cfg)}); err != nil {
			return LeaseTarget{}, err
		}
	}
	ready, err := client.WaitInstance(ctx, created.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	server := serverFromInstance(ready, cfg)
	if server.PublicNet.IPv4.IP == "" {
		return LeaseTarget{}, core.Exit(5, "nebius instance %s has no public IP", server.DisplayID())
	}
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := b.waitSSH(ctx, &ssh, "nebius bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	readyLabels := nebiusLeaseLabels(cfg, leaseID, slug, "ready", req.Keep, now)
	if err := client.UpdateLabels(ctx, server.CloudID, readyLabels); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels = readyLabels
	server.Status = "ready"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return LeaseTarget{}, err
	}
	committed = true
	return LeaseTarget{LeaseID: leaseID, Server: server, SSH: ssh}, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client := b.clientFactory(b.RT)
	items, err := client.ListInstances(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := ownedServers(items, b.Cfg)
	byCloudID := make(map[string]nebiusInstance, len(items))
	for _, item := range items {
		byCloudID[item.ID] = item
	}
	server, leaseID, err := core.FindServerByAlias(servers, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if leaseID == "" {
		for _, s := range servers {
			if s.CloudID == req.ID || s.Name == req.ID {
				server = s
				leaseID = s.Labels["lease"]
				break
			}
		}
	}
	if leaseID == "" && req.ReleaseOnly {
		return b.releaseTargetFromClaim(req.ID)
	}
	if leaseID == "" {
		return LeaseTarget{}, core.Exit(4, "lease/nebius instance not found: %s", req.ID)
	}
	item := byCloudID[server.CloudID]
	if item.ID != "" {
		server = serverFromInstance(item, b.Cfg)
	}
	if err := validateNebiusOwnership(server.Labels, b.Cfg); err != nil {
		return LeaseTarget{}, err
	}
	ssh := core.SSHTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
	core.UseStoredTestboxKey(&ssh, leaseID)
	if req.Repo.Root != "" && !req.NoLocalStateMutations {
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return LeaseTarget{}, err
		}
		if _, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], b.Cfg, server, ssh, req.Repo.Root, b.Cfg.IdleTimeout, req.Reclaim, claim, exists); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{LeaseID: leaseID, Server: server, SSH: ssh}, nil
}

func (b *backend) releaseTargetFromClaim(id string) (LeaseTarget, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(id, providerName)
	if err != nil {
		return LeaseTarget{}, err
	}
	if !ok {
		claim, ok, err = core.ResolveLeaseClaimForProviderCloudID(id, providerName)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	if !ok {
		return LeaseTarget{}, core.Exit(4, "lease/nebius instance not found: %s", id)
	}
	if err := validateNebiusOwnership(claim.Labels, b.Cfg); err != nil {
		return LeaseTarget{}, err
	}
	server := Server{Provider: providerName, CloudID: claim.CloudID, Name: claim.Slug, Labels: claim.Labels}
	return LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	items, err := b.clientFactory(b.RT).ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	servers := ownedServers(items, b.Cfg)
	out := make([]LeaseView, 0, len(servers))
	for _, server := range servers {
		out = append(out, server)
	}
	return out, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	return b.deleteServer(ctx, b.Cfg, req.Lease.Server)
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s nebius_instance=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *backend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if err := validateNebiusOwnership(server.Labels, b.Cfg); err != nil {
		return Server{}, err
	}
	client := b.clientFactory(b.RT)
	live, err := client.GetInstance(ctx, server.CloudID)
	if err != nil {
		return Server{}, err
	}
	liveServer := serverFromInstance(live, b.Cfg)
	if err := validateNebiusOwnership(liveServer.Labels, b.Cfg); err != nil {
		return Server{}, err
	}
	if liveServer.Labels["lease"] != server.Labels["lease"] || liveServer.Labels["slug"] != server.Labels["slug"] {
		return Server{}, core.Exit(3, "nebius live ownership changed for instance %s; refusing touch", server.DisplayID())
	}
	leaseID := liveServer.Labels["lease"]
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return Server{}, err
	}
	if claimExists {
		if claim.Provider != providerName || (claim.CloudID != "" && claim.CloudID != liveServer.CloudID) || claim.Slug != liveServer.Labels["slug"] {
			return Server{}, core.Exit(3, "nebius local claim changed for instance %s; refusing touch", server.DisplayID())
		}
		if claim.Labels["state"] == "cleanup" {
			return Server{}, core.Exit(4, "nebius lease=%s cleanup is already in progress", leaseID)
		}
	}
	cfg := b.Cfg
	labels := liveServer.Labels
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
		labels = cloneStringMap(labels)
		delete(labels, "idle_timeout")
		delete(labels, "idle_timeout_secs")
	}
	labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.now().UTC())
	labels = addNebiusScopeLabels(labels, cfg)
	if err := client.UpdateLabels(ctx, server.CloudID, labels); err != nil {
		return Server{}, err
	}
	if claimExists {
		liveServer.Labels = labels
		var err error
		if claim.RepoRoot != "" {
			_, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, labels["slug"], cfg, liveServer, req.Lease.SSH, claim.RepoRoot, cfg.IdleTimeout, false, claim, true)
		} else {
			_, err = core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, labels["slug"], cfg, liveServer, req.Lease.SSH, cfg.IdleTimeout, claim, true)
		}
		if err != nil {
			return Server{}, err
		}
	}
	liveServer.Labels = labels
	return liveServer, nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	items, err := b.clientFactory(b.RT).ListInstances(ctx)
	if err != nil {
		return err
	}
	servers := make([]Server, 0, len(items))
	for _, item := range items {
		server := serverFromInstance(item, b.Cfg)
		if err := validateNebiusOwnership(server.Labels, b.Cfg); err != nil {
			if strings.EqualFold(server.Labels["crabbox"], "true") || strings.EqualFold(server.Labels[nebiusProviderLabel], providerName) || strings.EqualFold(server.Labels["provider"], providerName) {
				fmt.Fprintf(b.RT.Stderr, "skip nebius instance id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, err)
			}
			continue
		}
		servers = append(servers, server)
	}
	return b.CleanupServers(ctx, req, servers)
}

func (b *backend) deleteServer(ctx context.Context, _ Config, server Server) error {
	if err := validateNebiusOwnership(server.Labels, b.Cfg); err != nil {
		return err
	}
	leaseID := strings.TrimSpace(server.Labels["lease"])
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if claimExists {
		if claim.Provider != providerName || claim.LeaseID != leaseID || (claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID) || (claim.Slug != "" && claim.Slug != server.Labels["slug"]) {
			return core.Exit(3, "nebius local claim changed for instance %s; refusing release", server.DisplayID())
		}
	}
	if strings.TrimSpace(server.CloudID) == "" {
		return core.Exit(4, "nebius recovery claim for lease=%s has no instance identity; key and claim retained", leaseID)
	}
	client := b.clientFactory(b.RT)
	confirmedAbsent := false
	if server.CloudID != "" {
		live, err := client.GetInstance(ctx, server.CloudID)
		if err == nil {
			liveServer := serverFromInstance(live, b.Cfg)
			if err := validateNebiusOwnership(liveServer.Labels, b.Cfg); err != nil {
				return err
			}
			if liveServer.Labels["lease"] != server.Labels["lease"] || liveServer.Labels["slug"] != server.Labels["slug"] {
				return core.Exit(3, "nebius live ownership changed for instance %s; refusing release", server.DisplayID())
			}
		} else if !isNebiusInstanceNotFound(err, server.CloudID) {
			return err
		} else {
			confirmedAbsent = true
		}
	}
	if !confirmedAbsent {
		if err := client.DeleteInstance(ctx, server.CloudID); err != nil {
			if isNebiusInstanceNotFound(err, server.CloudID) {
				confirmedAbsent = true
			} else if isIndeterminateNebiusError(err) {
				return err
			} else {
				return err
			}
		}
	}
	if confirmedAbsent {
		fmt.Fprintf(b.RT.Stderr, "nebius instance id=%s already absent; cleaning local lease state\n", server.DisplayID())
	}
	if claimExists {
		if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return fmt.Errorf("finalize nebius cleanup claim: %w", err)
		}
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *backend) persistRecoveryClaim(leaseID, slug, cloudID string, cfg Config, repoRoot string, labels map[string]string, reclaim bool) error {
	server := Server{Provider: providerName, CloudID: cloudID, Name: core.LeaseProviderName(leaseID, slug), Labels: labels}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, repoRoot, cfg.IdleTimeout, reclaim)
}

func ownedServers(items []nebiusInstance, cfg Config) []Server {
	servers := make([]Server, 0, len(items))
	for _, item := range items {
		server := serverFromInstance(item, cfg)
		if validateNebiusOwnership(server.Labels, cfg) == nil {
			servers = append(servers, server)
		}
	}
	return servers
}

func validateNebiusAcquireConfig(cfg Config) error {
	if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
		return core.Exit(2, "provider=nebius supports target=linux only")
	}
	if strings.TrimSpace(cfg.Nebius.ParentID) == "" {
		return core.Exit(2, "nebius.parentId is required")
	}
	if strings.TrimSpace(cfg.Nebius.SubnetID) == "" {
		return core.Exit(2, "nebius.subnetId is required")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Nebius.PublicIP), "none") {
		return core.Exit(2, "provider=nebius requires public_ip=dynamic for direct SSH lifecycle")
	}
	return nil
}

func isIndeterminateNebiusError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return nebiusTimeoutToken.MatchString(text) ||
		strings.Contains(text, "timed out") ||
		strings.Contains(text, "timeout waiting") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection lost") ||
		strings.Contains(text, "lost create response") ||
		strings.Contains(text, "lost delete response") ||
		strings.Contains(text, "lost response") ||
		strings.Contains(text, "context deadline exceeded") ||
		strings.Contains(text, "deadline exceeded") ||
		strings.Contains(text, "transport is closing") ||
		strings.Contains(text, "unexpected eof") ||
		strings.Contains(text, "rpc error: code = unavailable")
}

var nebiusTimeoutToken = regexp.MustCompile(`(^|[^a-z0-9_])timeout([^a-z0-9_]|$)`)

func isNebiusInstanceNotFound(err error, id string) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	hasNotFound := strings.Contains(text, "not found") || strings.Contains(text, "404")
	if !hasNotFound {
		return false
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if id != "" && strings.Contains(text, id) {
		return true
	}
	return strings.Contains(text, "instance not found")
}

func nebiusServerType(cfg Config) string {
	if strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	return strings.TrimSpace(cfg.Nebius.Preset)
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

var _ core.SSHLeaseBackend = (*backend)(nil)
var _ core.CleanupBackend = (*backend)(nil)
var _ core.ReleaseLeaseReporter = (*backend)(nil)

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if b.RT.Exec == nil {
		return DoctorResult{}, exit(2, "provider=nebius doctor requires command runner")
	}
	client := newCLIRunner(b.Cfg.Nebius, b.RT)
	checks := []DoctorCheck{
		b.checkVersion(ctx, client),
		b.checkProfile(ctx, client),
		b.checkParentID(ctx, client),
		b.checkSubnet(ctx, client),
		b.checkPlatform(ctx, client),
		b.checkImage(ctx, client),
		b.checkJSON(ctx, client),
	}
	status := "ok"
	for _, check := range checks {
		if check.Status != "ok" {
			status = "error"
			break
		}
	}
	return DoctorResult{
		Provider: providerName,
		Status:   status,
		Message:  fmt.Sprintf("cli=%s control_plane=read_only mutation=false", status),
		Checks:   checks,
	}, nil
}

func (b *backend) checkVersion(ctx context.Context, client cliRunner) DoctorCheck {
	result, err := client.run(ctx, "version")
	if err != nil {
		return doctorCheck("cli", "error", err.Error(), nil)
	}
	return doctorCheck("cli", "ok", "nebius cli available", map[string]string{"version": redactNebiusText(firstNonBlank(result.Stdout, result.Stderr))})
}

func (b *backend) checkProfile(ctx context.Context, client cliRunner) DoctorCheck {
	result, err := client.run(ctx, "profile", "list")
	if err != nil {
		return doctorCheck("profile", "error", err.Error(), nil)
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return doctorCheck("profile", "error", "profile store is empty", nil)
	}
	return doctorCheck("profile", "ok", "profile store readable", nil)
}

func (b *backend) checkParentID(ctx context.Context, client cliRunner) DoctorCheck {
	parentID := strings.TrimSpace(b.Cfg.Nebius.ParentID)
	if parentID == "" {
		return doctorCheck("parent-id", "error", "nebius.parentId is required", nil)
	}
	result, err := client.run(ctx, "iam", "project", "get", parentID, "--format", "json")
	if err != nil {
		return doctorCheck("parent-id", "error", err.Error(), map[string]string{"parentId": parentID})
	}
	if !isJSON(result.Stdout) {
		return doctorCheck("parent-id", "error", "project lookup did not return JSON", map[string]string{"parentId": parentID})
	}
	return doctorCheck("parent-id", "ok", "project readable", map[string]string{"parentId": parentID})
}

func (b *backend) checkSubnet(ctx context.Context, client cliRunner) DoctorCheck {
	subnetID := strings.TrimSpace(b.Cfg.Nebius.SubnetID)
	if subnetID == "" {
		return doctorCheck("subnet", "error", "nebius.subnetId is required", nil)
	}
	result, err := client.run(ctx, "vpc", "subnet", "list", "--parent-id", b.Cfg.Nebius.ParentID, "--format", "json")
	if err != nil {
		return doctorCheck("subnet", "error", err.Error(), map[string]string{"subnetId": subnetID})
	}
	ok, err := containsIDOrName(result.Stdout, subnetID)
	if err != nil {
		return doctorCheck("subnet", "error", "subnet list did not return expected JSON", map[string]string{"subnetId": subnetID})
	}
	if !ok {
		return doctorCheck("subnet", "error", "configured subnet not found", map[string]string{"subnetId": subnetID})
	}
	return doctorCheck("subnet", "ok", "subnet readable", map[string]string{"subnetId": subnetID})
}

func (b *backend) checkPlatform(ctx context.Context, client cliRunner) DoctorCheck {
	platform := strings.TrimSpace(b.Cfg.Nebius.Platform)
	result, err := client.run(ctx, "compute", "platform", "list", "--parent-id", b.Cfg.Nebius.ParentID, "--format", "json")
	if err != nil {
		return doctorCheck("platform", "error", err.Error(), map[string]string{"platform": platform})
	}
	ok, err := containsIDOrName(result.Stdout, platform)
	if err != nil {
		return doctorCheck("platform", "error", "platform list did not return expected JSON", map[string]string{"platform": platform})
	}
	if !ok {
		return doctorCheck("platform", "error", "configured platform not found", map[string]string{"platform": platform})
	}
	return doctorCheck("platform", "ok", "platform readable", map[string]string{"platform": platform})
}

func (b *backend) checkImage(ctx context.Context, client cliRunner) DoctorCheck {
	imageFamily := strings.TrimSpace(b.Cfg.Nebius.ImageFamily)
	result, err := client.run(ctx, "compute", "image", "get-latest-by-family", "--image-family", imageFamily, "--format", "json")
	if err != nil {
		return doctorCheck("image", "error", err.Error(), map[string]string{"imageFamily": imageFamily})
	}
	object, err := parseJSONObject(result.Stdout)
	if err != nil {
		return doctorCheck("image", "error", "image lookup did not return expected JSON", map[string]string{"imageFamily": imageFamily})
	}
	if stringFromAny(firstPath(object, "metadata.id", "id")) == "" {
		return doctorCheck("image", "error", "configured image family not found", map[string]string{"imageFamily": imageFamily})
	}
	return doctorCheck("image", "ok", "image family readable", map[string]string{"imageFamily": imageFamily})
}

func (b *backend) checkJSON(ctx context.Context, client cliRunner) DoctorCheck {
	// A single page is enough to verify CLI JSON compatibility; lifecycle inventory uses --all.
	result, err := client.run(ctx, "compute", "instance", "list", "--parent-id", b.Cfg.Nebius.ParentID, "--format", "json")
	if err != nil {
		return doctorCheck("json", "error", err.Error(), nil)
	}
	if !isJSON(result.Stdout) {
		return doctorCheck("json", "error", "json output is unavailable", nil)
	}
	return doctorCheck("json", "ok", "json output available", nil)
}

func doctorCheck(name, status, message string, details map[string]string) DoctorCheck {
	return DoctorCheck{Check: name, Status: status, Message: redactNebiusText(message), Details: details}
}
