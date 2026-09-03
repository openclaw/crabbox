package daytona

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	daytona "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	daytonaProvider      = "daytona"
	daytonaTokenRedacted = "<token>"
)

type daytonaFlagValues struct {
	APIURL           *string
	Snapshot         *string
	Target           *string
	User             *string
	WorkRoot         *string
	SSHGatewayHost   *string
	SSHAccessMinutes *int
}

func RegisterDaytonaProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return daytonaFlagValues{
		APIURL:           fs.String("daytona-api-url", defaults.Daytona.APIURL, "Daytona API URL"),
		Snapshot:         fs.String("daytona-snapshot", defaults.Daytona.Snapshot, "Daytona snapshot name"),
		Target:           fs.String("daytona-target", defaults.Daytona.Target, "Daytona compute target"),
		User:             fs.String("daytona-user", defaults.Daytona.User, "Daytona sandbox user"),
		WorkRoot:         fs.String("daytona-work-root", defaults.Daytona.WorkRoot, "Daytona sandbox work root"),
		SSHGatewayHost:   fs.String("daytona-ssh-gateway-host", defaults.Daytona.SSHGatewayHost, "Daytona SSH gateway host"),
		SSHAccessMinutes: fs.Int("daytona-ssh-access-minutes", defaults.Daytona.SSHAccessMinutes, "Daytona SSH access token TTL in minutes"),
	}
}

func ApplyDaytonaProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == daytonaProvider {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=daytona; choose CPU, memory, and disk in the Daytona snapshot")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=daytona; choose CPU, memory, and disk in the Daytona snapshot")
		}
	}
	v, ok := values.(daytonaFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "daytona-api-url") {
		cfg.Daytona.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "daytona-snapshot") {
		cfg.Daytona.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "daytona-target") {
		cfg.Daytona.Target = *v.Target
	}
	if flagWasSet(fs, "daytona-user") {
		cfg.Daytona.User = *v.User
	}
	if flagWasSet(fs, "daytona-work-root") {
		cfg.Daytona.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "daytona-ssh-gateway-host") {
		cfg.Daytona.SSHGatewayHost = *v.SSHGatewayHost
	}
	if flagWasSet(fs, "daytona-ssh-access-minutes") {
		cfg.Daytona.SSHAccessMinutes = *v.SSHAccessMinutes
	}
	return nil
}

func NewDaytonaLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = daytonaProvider
	return &daytonaLeaseBackend{spec: spec, cfg: cfg, rt: rt}
}

type daytonaLeaseBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *daytonaLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *daytonaLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	sandbox, leaseID, slug, err := b.createDaytonaSandbox(ctx, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, b.rollbackDaytonaSandbox(sandbox.GetId(), leaseID, err)
	}
	cfg := b.cfg
	cfg.WorkRoot = daytonaWorkRoot(cfg)
	server := daytonaSandboxToServer(sandbox, cfg)
	target, err := daytonaSSHTargetFor(ctx, client, cfg, server)
	if err != nil {
		return LeaseTarget{}, b.rollbackDaytonaSandbox(server.CloudID, leaseID, err)
	}
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "daytona ssh", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, b.rollbackDaytonaSandbox(server.CloudID, leaseID, err)
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return LeaseTarget{}, b.rollbackDaytonaSandbox(server.CloudID, leaseID, err)
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s sandbox=%s state=%s\n", leaseID, server.CloudID, server.Status)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *daytonaLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	return b.resolve(ctx, req, nil)
}

func (b *daytonaLeaseBackend) ResolveRunLeaseUnderClaim(ctx context.Context, req ResolveRequest, original core.LeaseClaim) (LeaseTarget, error) {
	return b.resolve(ctx, req, &original)
}

func (b *daytonaLeaseBackend) resolve(ctx context.Context, req ResolveRequest, original *LeaseClaim) (LeaseTarget, error) {
	if req.RejectAuthSecret {
		return LeaseTarget{}, exit(2, "crabbox connect does not support token-as-username SSH targets; use crabbox ssh --show-secret in a trusted terminal")
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return LeaseTarget{}, err
	}
	sandbox, leaseID, err := resolveDaytonaSandbox(ctx, client, b.cfg, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	server := daytonaSandboxToServer(sandbox, b.cfg)
	if req.StatusOnly {
		server.Labels["state"] = server.Status
	}
	if original != nil {
		// Core owns publication for run admission. Preserve the repository and
		// resource checks before Start or creating token-bearing SSH access.
		if err := validateExactDaytonaResourceClaim(leaseID, server.CloudID, *original, true); err != nil {
			return LeaseTarget{}, err
		}
		if err := core.CheckLeaseClaimRepositoryOwner(leaseID, *original, req.Repo.Root, false); err != nil {
			return LeaseTarget{}, err
		}
		if err := core.AuthorizeCheckpointRelease(*original, ""); err != nil {
			return LeaseTarget{}, err
		}
	} else {
		if req.Reclaim && !req.NoLocalStateMutations {
			if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), b.cfg, server, SSHTarget{}, req.Repo.Root, b.cfg.IdleTimeout, true); err != nil {
				return LeaseTarget{}, err
			}
		}
		if err := requireExactDaytonaClaim(leaseID, sandbox); err != nil {
			return LeaseTarget{}, err
		}
		if !req.Reclaim && !req.NoLocalStateMutations {
			if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), b.cfg, server, SSHTarget{}, req.Repo.Root, b.cfg.IdleTimeout, false); err != nil {
				return LeaseTarget{}, err
			}
		}
	}
	if req.StatusOnly {
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	if !daytonaStateReady(daytonaSandboxState(sandbox)) {
		if daytonaStateFailed(daytonaSandboxState(sandbox)) {
			return LeaseTarget{}, exit(5, "daytona sandbox %s entered terminal state=%s", sandbox.GetId(), daytonaSandboxState(sandbox))
		}
		sandbox, err = client.StartSandbox(ctx, sandbox.GetId())
		if err != nil {
			return LeaseTarget{}, daytonaError("start sandbox", err)
		}
		sandbox, err = waitForDaytonaReady(ctx, client, sandbox.GetId(), 5*time.Minute)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	server = daytonaSandboxToServer(sandbox, b.cfg)
	target, err := daytonaSSHTargetFor(ctx, client, b.cfg, server)
	if err != nil {
		return LeaseTarget{}, err
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *daytonaLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	sandboxes, err := client.ListCrabboxSandboxes(ctx)
	if err != nil {
		return nil, daytonaError("list sandboxes", err)
	}
	servers := make([]Server, 0, len(sandboxes))
	for i := range sandboxes {
		if _, owned := daytonaSandboxOwnership(&sandboxes[i]); !owned {
			continue
		}
		servers = append(servers, daytonaSandboxToServer(&sandboxes[i], b.cfg))
	}
	return servers, nil
}

func (b *daytonaLeaseBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: daytonaProvider,
		Message:  fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", len(servers)),
	}, nil
}

func (b *daytonaLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	ctx, cancel := context.WithTimeout(ctx, daytonaCleanupTimeout)
	defer cancel()
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	if req.Lease.Server.CloudID != "" {
		if err := requireExactDaytonaResourceClaim(req.Lease.LeaseID, req.Lease.Server.CloudID); err != nil {
			return err
		}
		if err := deleteOwnedDaytonaSandbox(ctx, client, req.Lease.Server.CloudID, req.Lease.LeaseID); err != nil {
			return daytonaError("delete sandbox", err)
		}
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *daytonaLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return req.Lease.Server, err
	}
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = daytonaTouchedLabels(server.Labels, b.cfg, req)
	if server.CloudID != "" {
		if req.IdleTimeoutOverride != nil {
			if err := client.SetAutoStopInterval(ctx, server.CloudID, *req.IdleTimeoutOverride); err != nil {
				return req.Lease.Server, daytonaError("update auto-stop interval", err)
			}
		}
		if err := client.ReplaceLabels(ctx, server.CloudID, server.Labels); err != nil {
			return server, daytonaError("replace labels", err)
		}
		if err := client.UpdateLastActivity(ctx, server.CloudID); err != nil {
			return server, daytonaError("update last activity", err)
		}
	}
	return server, nil
}

func waitForDaytonaReady(ctx context.Context, client daytonaAPI, id string, timeout time.Duration) (*daytona.Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	result, err := shared.Poll(context.WithoutCancel(ctx), 0, 3*time.Second,
		func(context.Context, time.Duration) error {
			if err := shared.SleepContext(ctx, 3*time.Second); err != nil {
				return ctx.Err()
			}
			return nil
		},
		func(context.Context) (*daytona.Sandbox, error) { return client.GetSandbox(ctx, id) },
		func(_ context.Context, sandbox *daytona.Sandbox, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, daytonaError("get sandbox", fetchErr)
			}
			state := daytonaSandboxState(sandbox)
			if daytonaStateReady(state) {
				return true, nil
			}
			if daytonaStateFailed(state) {
				return false, exit(5, "daytona sandbox %s entered terminal state=%s", id, state)
			}
			if time.Now().After(deadline) {
				return false, exit(5, "timed out waiting for daytona sandbox %s (state=%s)", id, state)
			}
			return false, nil
		}, nil)
	if err != nil {
		return nil, err
	}
	return result.Value, nil
}

func resolveDaytonaSandbox(ctx context.Context, client daytonaAPI, cfg Config, id string) (*daytona.Sandbox, string, error) {
	if id == "" {
		return nil, "", exit(2, "provider=daytona requires --id <sandbox-id-or-slug>")
	}
	sandboxes, err := client.ListCrabboxSandboxes(ctx)
	if err != nil {
		return nil, "", daytonaError("list sandboxes", err)
	}
	if isCanonicalLeaseID(id) {
		for i := range sandboxes {
			if leaseID, owned := daytonaSandboxOwnership(&sandboxes[i]); owned && leaseID == id {
				return &sandboxes[i], id, nil
			}
		}
	}
	slug := normalizeLeaseSlug(id)
	var matches []*daytona.Sandbox
	for i := range sandboxes {
		if _, owned := daytonaSandboxOwnership(&sandboxes[i]); owned && slug != "" && normalizeLeaseSlug(sandboxes[i].Labels["slug"]) == slug {
			matches = append(matches, &sandboxes[i])
		}
	}
	if len(matches) > 1 {
		return nil, "", exit(4, "daytona slug %q matches multiple sandboxes", id)
	}
	if len(matches) == 1 {
		return matches[0], matches[0].Labels["lease"], nil
	}
	for i := range sandboxes {
		if leaseID, owned := daytonaSandboxOwnership(&sandboxes[i]); owned && (sandboxes[i].GetId() == id || sandboxes[i].GetName() == id || sandboxes[i].Labels["lease_name"] == id) {
			return &sandboxes[i], leaseID, nil
		}
	}
	if claim, ok, err := resolveLeaseClaimForProvider(id, daytonaProvider); err != nil {
		return nil, "", err
	} else if ok {
		for i := range sandboxes {
			if leaseID, owned := daytonaSandboxOwnership(&sandboxes[i]); owned && leaseID == claim.LeaseID {
				return &sandboxes[i], claim.LeaseID, nil
			}
		}
		cloudID := strings.TrimSpace(claim.CloudID)
		if cloudID != "" {
			sandbox, getErr := client.GetSandbox(ctx, cloudID)
			if getErr != nil {
				if daytonaIsNotFoundError(getErr) {
					return nil, "", exit(4, "daytona claim %s is bound to missing sandbox %s", claim.LeaseID, cloudID)
				}
				return nil, "", daytonaError("get claimed sandbox", getErr)
			}
			if sandbox == nil || strings.TrimSpace(sandbox.GetId()) == "" {
				return nil, "", exit(4, "daytona claim %s is bound to missing sandbox %s", claim.LeaseID, cloudID)
			}
			leaseID, owned := daytonaSandboxOwnership(sandbox)
			if strings.TrimSpace(sandbox.GetId()) != cloudID || !owned || leaseID != claim.LeaseID {
				return nil, "", exit(4, "daytona sandbox %s does not match exact local claim for lease %s", cloudID, claim.LeaseID)
			}
			return sandbox, claim.LeaseID, nil
		}
	}
	sandbox, err := client.GetSandbox(ctx, id)
	if err == nil && sandbox != nil && sandbox.GetId() != "" {
		if leaseID, owned := daytonaSandboxOwnership(sandbox); owned {
			return sandbox, leaseID, nil
		}
		return nil, "", exit(4, "daytona sandbox %s is not owned by Crabbox", id)
	}
	if err != nil && !daytonaIsNotFoundError(err) {
		return nil, "", daytonaError("get sandbox", err)
	}
	_ = cfg
	return nil, "", exit(4, "daytona sandbox not found: %s", id)
}

func daytonaSandboxOwnership(sandbox *daytona.Sandbox) (string, bool) {
	if sandbox == nil || strings.TrimSpace(sandbox.GetId()) == "" {
		return "", false
	}
	labels := sandbox.GetLabels()
	leaseID := strings.TrimSpace(labels["lease"])
	return leaseID, strings.EqualFold(strings.TrimSpace(labels["crabbox"]), "true") &&
		strings.EqualFold(strings.TrimSpace(labels["provider"]), daytonaProvider) &&
		isCanonicalLeaseID(leaseID)
}

func establishDaytonaSandboxOwnership(ctx context.Context, client daytonaAPI, resourceID, leaseID string, labels map[string]string) (*daytona.Sandbox, error) {
	if err := client.ReplaceLabels(ctx, resourceID, labels); err != nil {
		return nil, daytonaError("replace labels", err)
	}
	sandbox, err := client.GetSandbox(ctx, resourceID)
	if err != nil {
		return nil, daytonaError("verify sandbox labels", err)
	}
	verifiedLeaseID, owned := daytonaSandboxOwnership(sandbox)
	if sandbox == nil || strings.TrimSpace(sandbox.GetId()) != strings.TrimSpace(resourceID) || !owned || verifiedLeaseID != strings.TrimSpace(leaseID) {
		return nil, exit(4, "daytona sandbox %s did not persist exact Crabbox ownership labels for lease %s", blank(resourceID, "-"), blank(leaseID, "-"))
	}
	return sandbox, nil
}

func requireExactDaytonaClaim(leaseID string, sandbox *daytona.Sandbox) error {
	resourceID := ""
	if sandbox != nil {
		resourceID = strings.TrimSpace(sandbox.GetId())
	}
	return requireExactDaytonaResourceClaim(leaseID, resourceID)
}

func requireExactDaytonaResourceClaim(leaseID, resourceID string) error {
	resourceID = strings.TrimSpace(resourceID)
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, daytonaProvider)
	if err != nil {
		return err
	}
	return validateExactDaytonaResourceClaim(leaseID, resourceID, claim, ok)
}

func validateExactDaytonaResourceClaim(leaseID, resourceID string, claim LeaseClaim, exists bool) error {
	if !exists || strings.TrimSpace(claim.LeaseID) != strings.TrimSpace(leaseID) || strings.TrimSpace(claim.CloudID) != resourceID {
		return exit(4, "daytona sandbox %s has no exact local claim for lease %s; use --reclaim from the owning repository before reuse or deletion", blank(resourceID, "-"), blank(leaseID, "-"))
	}
	return nil
}

func daytonaSSHTargetFor(ctx context.Context, client daytonaAPI, cfg Config, server Server) (SSHTarget, error) {
	access, err := client.CreateSSHAccess(ctx, server.CloudID, time.Duration(daytonaSSHAccessMinutes(cfg))*time.Minute)
	if err != nil {
		return SSHTarget{}, daytonaError("create ssh access", err)
	}
	return daytonaSSHTargetFromAccess(cfg, access)
}

func daytonaSSHTargetFromAccess(cfg Config, access daytonaSSHAccess) (SSHTarget, error) {
	user := strings.TrimSpace(access.Token)
	host := daytonaSSHGatewayHost(cfg)
	port := "22"
	if command := strings.TrimSpace(access.Command); command != "" {
		parsedUser, parsedHost, parsedPort, err := parseDaytonaSSHCommand(command)
		if err != nil {
			return SSHTarget{}, err
		}
		user = parsedUser
		host = parsedHost
		port = parsedPort
	}
	if user == "" {
		return SSHTarget{}, fmt.Errorf("daytona ssh access response missing token")
	}
	return SSHTarget{
		User:        user,
		Host:        host,
		Port:        port,
		Key:         "",
		TargetOS:    targetLinux,
		ReadyCheck:  "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
		AuthSecret:  true,
		NetworkKind: NetworkPublic,
	}, nil
}

func parseDaytonaSSHCommand(command string) (string, string, string, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", "", "", fmt.Errorf("daytona ssh command is empty")
	}
	if fields[0] == "ssh" {
		fields = fields[1:]
	}
	port := "22"
	destination := ""
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		switch {
		case field == "-p":
			if i+1 >= len(fields) || strings.TrimSpace(fields[i+1]) == "" {
				return "", "", "", fmt.Errorf("daytona ssh command missing -p value: %q", command)
			}
			i++
			port = fields[i]
		case strings.HasPrefix(field, "-p") && len(field) > 2:
			port = strings.TrimPrefix(field, "-p")
		case strings.HasPrefix(field, "-"):
			return "", "", "", fmt.Errorf("daytona ssh command has unsupported option %q", field)
		default:
			destination = field
		}
	}
	user, host, ok := strings.Cut(destination, "@")
	if !ok || strings.TrimSpace(user) == "" || strings.TrimSpace(host) == "" {
		return "", "", "", fmt.Errorf("daytona ssh command missing user@host destination: %q", command)
	}
	return user, host, port, nil
}

func daytonaSandboxesToServers(sandboxes []daytona.Sandbox, cfg Config) []Server {
	servers := make([]Server, 0, len(sandboxes))
	for i := range sandboxes {
		servers = append(servers, daytonaSandboxToServer(&sandboxes[i], cfg))
	}
	return servers
}

func daytonaSandboxToServer(sandbox *daytona.Sandbox, cfg Config) Server {
	labels := map[string]string{}
	if sandbox != nil && sandbox.Labels != nil {
		for k, v := range sandbox.Labels {
			labels[k] = v
		}
	}
	server := Server{Provider: daytonaProvider, Labels: labels}
	if sandbox != nil {
		server.CloudID = sandbox.GetId()
		server.Name = sandbox.GetName()
		server.Status = daytonaSandboxState(sandbox)
	}
	if server.Name == "" {
		server.Name = blank(labels["lease_name"], server.CloudID)
	}
	server.ServerType.Name = blank(labels["server_type"], serverTypeForProviderClass(cfg.Provider, cfg.Class))
	return server
}

func daytonaSandboxState(sandbox *daytona.Sandbox) string {
	if sandbox == nil || sandbox.State == nil {
		return ""
	}
	return string(*sandbox.State)
}

func daytonaStateReady(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "started", "running", "ready", "active":
		return true
	default:
		return false
	}
}

func daytonaStateFailed(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "error", "errored", "failed", "build_failed", "destroyed", "destroying", "deleted":
		return true
	default:
		return false
	}
}

func daytonaUser(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Daytona.User), "daytona")
}

func daytonaWorkRoot(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Daytona.WorkRoot), "/home/"+daytonaUser(cfg)+"/crabbox")
}

func daytonaSSHGatewayHost(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Daytona.SSHGatewayHost), "ssh.app.daytona.io")
}

func daytonaSSHAccessMinutes(cfg Config) int {
	if cfg.Daytona.SSHAccessMinutes > 0 {
		return cfg.Daytona.SSHAccessMinutes
	}
	return 30
}
