package githubcodespaces

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type codespacesAPI interface {
	createCodespace(context.Context, createCodespaceRequest) (codespace, error)
	listCodespaces(context.Context) ([]codespace, error)
	getCodespace(context.Context, string) (codespace, error)
	startCodespace(context.Context, string) (codespace, error)
	stopCodespace(context.Context, string) error
	deleteCodespace(context.Context, string) error
	listMachines(context.Context, string, string) ([]codespaceMachine, error)
}

type githubCLI interface {
	authStatus(context.Context) error
	authToken(context.Context) (string, error)
	userLogin(context.Context) (string, error)
	codespaceSSHConfig(context.Context, string) (string, error)
}

type backend struct {
	spec          ProviderSpec
	cfg           Config
	rt            Runtime
	clientFactory func(string) codespacesAPI
	ghFactory     func() githubCLI
	bindClaim     func(string, LeaseClaim, Server) (LeaseClaim, error)
	waitSSH       func(context.Context, *SSHTarget, string, time.Duration) error
	now           func() time.Time
	pollInterval  time.Duration
	readyTimeout  time.Duration
}

const githubCodespacesRollbackTimeout = 30 * time.Second

const (
	labelCodespaceName  = "codespace_name"
	labelDisplayName    = "codespace_display_name"
	labelEnvironmentID  = "codespace_environment_id"
	labelRepository     = "github_repository"
	labelRef            = "github_ref"
	labelMachine        = "github_machine"
	labelLogin          = "github_login"
	labelRelease        = "release"
	labelState          = "state"
	labelRecovery       = "recovery"
	recoveryPreCreate   = "pre-create"
	releaseDelete       = "delete"
	releaseStop         = "stop"
	defaultPollInterval = 3 * time.Second
	defaultReadyTimeout = 10 * time.Minute
)

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) *backend {
	b := &backend{spec: spec, cfg: cfg, rt: rt, pollInterval: defaultPollInterval, readyTimeout: defaultReadyTimeout}
	b.clientFactory = func(token string) codespacesAPI {
		return newClient(cfg.GitHubCodespaces, rt, token)
	}
	b.ghFactory = func() githubCLI {
		return newGHRunner(cfg.GitHubCodespaces, rt)
	}
	b.bindClaim = func(leaseID string, expected LeaseClaim, server Server) (LeaseClaim, error) {
		return updateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, SSHTarget{})
	}
	b.waitSSH = func(ctx context.Context, target *SSHTarget, phase string, timeout time.Duration) error {
		return waitForSSHReady(ctx, target, b.stderr(), phase, timeout)
	}
	b.now = func() time.Time {
		if rt.Clock != nil {
			return rt.Clock.Now()
		}
		return time.Now()
	}
	return b
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	gh, api, login, err := b.controlPlane(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	repo, err := b.resolveRepo(req.Repo)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.claimConfig(repo)
	if _, err := api.listMachines(ctx, repo, b.cfg.GitHubCodespaces.Ref); err != nil {
		return LeaseTarget{}, err
	}
	live, err := api.listCodespaces(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	existing, err := b.serversFromCodespaces(live)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	if leaseID == "" {
		leaseID = newLeaseID()
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, existing)
	if err != nil {
		return LeaseTarget{}, err
	}
	release := releaseDelete
	if !githubCodespacesDeleteOnRelease(LeaseTarget{}, cfg) {
		release = releaseStop
	}
	repoRoot, err := repoRootForClaim(req.Repo)
	if err != nil {
		return LeaseTarget{}, err
	}
	displayName := githubCodespacesDisplayName(leaseID, slug)
	claim, err := b.claimPendingCreate(leaseID, slug, repo, login, displayName, repoRoot, release, req.Keep, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	created, createErr := api.createCodespace(ctx, createCodespaceRequest{
		Repo:             repo,
		Ref:              strings.TrimSpace(b.cfg.GitHubCodespaces.Ref),
		Machine:          strings.TrimSpace(b.cfg.GitHubCodespaces.Machine),
		DevcontainerPath: strings.TrimSpace(b.cfg.GitHubCodespaces.DevcontainerPath),
		WorkingDirectory: strings.TrimSpace(b.cfg.GitHubCodespaces.WorkingDirectory),
		Geo:              strings.TrimSpace(b.cfg.GitHubCodespaces.Geo),
		IdleTimeout:      b.githubIdleTimeout(),
		RetentionPeriod:  b.cfg.GitHubCodespaces.RetentionPeriod,
		DisplayName:      displayName,
	})
	if createErr == nil && strings.TrimSpace(created.Name) == "" {
		createErr = errors.New("github-codespaces create returned no resource identity")
	}
	if createErr != nil {
		if !githubCodespacesCreateMayHaveSucceeded(createErr) {
			return LeaseTarget{}, errors.Join(createErr, discardPendingClaim(claim))
		}
		recoveredClaim, recovered, recoveryErr := b.recoverPendingClaim(api, claim, login)
		if recoveryErr != nil {
			return LeaseTarget{}, errors.Join(
				fmt.Errorf("github-codespaces create outcome is uncertain for lease=%s display_name=%q repo=%q; recovery claim retained: %w", leaseID, displayName, repo, createErr),
				recoveryErr,
			)
		}
		claim, created = recoveredClaim, recovered
	} else {
		created, err = validateCreatedCodespaceIdentity(claim, created)
		if err != nil {
			return LeaseTarget{}, err
		}
		claim, err = b.bindValidatedCreatedClaim(claim, created)
		if err != nil {
			if !req.Keep {
				err = errors.Join(err, rollbackUnboundCreatedCodespace(api, claim, created))
			}
			return LeaseTarget{}, err
		}
	}
	available, err := b.waitForAvailable(ctx, api, created.Name)
	if err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, err
	}
	labels := b.labelsFor(leaseID, slug, repo, login, req.Keep, release, available, "ready")
	labels[labelDisplayName] = displayName
	server := b.serverFromCodespace(available, labels)
	target, err := b.sshTarget(ctx, gh, leaseID, available.Name, repo, true)
	if err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, b.sshPrerequisiteError(err)
	}
	if err := b.waitSSH(ctx, &target, "github-codespaces ssh", b.readyTimeout); err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, b.sshPrerequisiteError(err)
	}
	lease := LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			if !req.Keep {
				err = errors.Join(err, rollbackCreatedCodespace(api, claim))
			}
			return LeaseTarget{}, err
		}
	}
	if _, err := updateLeaseClaimEndpointIfUnchanged(leaseID, claim, server, target); err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.stderr(), "provisioned provider=github-codespaces lease=%s slug=%s codespace=%s repo=%s state=%s\n", leaseID, slug, available.Name, repo, available.State)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	gh, api, login, err := b.controlPlane(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	live, err := api.listCodespaces(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if pending, ok, err := resolveLeaseClaimForProvider(req.ID, providerName); err != nil {
		return LeaseTarget{}, err
	} else if ok && strings.TrimSpace(pending.CloudID) == "" && pending.Labels[labelRecovery] == recoveryPreCreate {
		if _, _, err := b.recoverPendingClaimFromInventory(pending, login, live); err != nil {
			return LeaseTarget{}, err
		}
	}
	server, leaseID, err := b.resolveServer(live, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server.CloudID == "" {
		return LeaseTarget{}, exit(4, "github-codespaces lease not found: %s", req.ID)
	}
	claim, claimOK, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if claimOK {
		if err := b.validateClaimForServer(claim, server, login); err != nil {
			return LeaseTarget{}, err
		}
	}
	item, err := api.getCodespace(ctx, server.CloudID)
	if err != nil {
		if req.ReleaseOnly && isGitHubNotFound(err) && claimOK {
			server.Status = "deleted"
			server.Labels[labelState] = "deleted"
			return LeaseTarget{Server: server, LeaseID: leaseID}, nil
		}
		return LeaseTarget{}, err
	}
	server = b.mergeLiveServer(server, item)
	if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	if codespaceStopped(item.State) {
		item, err = api.startCodespace(ctx, item.Name)
		if err != nil {
			return LeaseTarget{}, err
		}
		item, err = b.waitForAvailable(ctx, api, item.Name)
		if err != nil {
			return LeaseTarget{}, err
		}
		server = b.mergeLiveServer(server, item)
	}
	if codespaceTerminal(item.State) {
		return LeaseTarget{}, exit(5, "github-codespaces codespace %s entered terminal state=%s", item.Name, item.State)
	}
	repo := firstNonEmpty(server.Labels[labelRepository], item.Repository.FullName)
	cfg := b.repoConfig(repo)
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels["work_root"] = cfg.WorkRoot
	server.Labels = touchDirectLeaseLabels(server.Labels, cfg, "ready", b.now().UTC())
	target, err := b.sshTarget(ctx, gh, leaseID, item.Name, repo, !req.NoLocalStateMutations)
	if err != nil {
		return LeaseTarget{}, b.sshPrerequisiteError(err)
	}
	if req.ReadyProbe {
		if err := b.waitSSH(ctx, &target, "github-codespaces ssh", b.readyTimeout); err != nil {
			return LeaseTarget{}, b.sshPrerequisiteError(err)
		}
	}
	if !req.NoLocalStateMutations {
		if err := updateLeaseClaimEndpoint(leaseID, server, target); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	_, api, _, err := b.controlPlane(ctx)
	if err != nil {
		return nil, err
	}
	live, err := api.listCodespaces(ctx)
	if err != nil {
		return nil, err
	}
	return b.serversFromCodespaces(live)
}

func (b *backend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, b.now().UTC())
	if server.Labels[labelCodespaceName] == "" && server.CloudID != "" {
		server.Labels[labelCodespaceName] = server.CloudID
	}
	if req.Lease.LeaseID != "" {
		if err := updateLeaseClaimEndpoint(req.Lease.LeaseID, server, req.Lease.SSH); err != nil {
			return Server{}, err
		}
	}
	return server, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	_, api, login, err := b.controlPlane(ctx)
	if err != nil {
		return err
	}
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		return exit(2, "github-codespaces release requires a lease id")
	}
	server := req.Lease.Server
	claim, claimOK, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !claimOK {
		return exit(2, "github-codespaces release requires a local claim for lease %s", leaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" && claim.Labels[labelRecovery] == recoveryPreCreate {
		claim, _, err = b.recoverPendingClaim(api, claim, login)
		if err != nil {
			return err
		}
		server = serverFromClaim(claim)
	}
	if server.CloudID == "" {
		server = serverFromClaim(claim)
	}
	if err := b.validateClaimForServer(claim, server, login); err != nil {
		return err
	}
	name := firstNonEmpty(server.CloudID, server.Name, server.Labels[labelCodespaceName])
	if name == "" {
		return exit(2, "github-codespaces release requires a claim-backed codespace name")
	}
	if githubCodespacesDeleteOnRelease(req.Lease, b.cfg) {
		item, err := api.getCodespace(ctx, name)
		if err != nil && !isGitHubNotFound(err) {
			return err
		}
		if err == nil {
			if err := validateDeleteSafe(item); err != nil {
				return b.stopCodespaceAndRetain(ctx, api, leaseID, claim, server, name)
			}
		}
		err = api.deleteCodespace(ctx, name)
		if err != nil && !isGitHubNotFound(err) {
			return err
		}
		if err := removeLeaseClaimIfUnchanged(leaseID, claim); err != nil {
			return err
		}
		return removeStoredSSHConfig(leaseID)
	}
	return b.stopCodespaceAndRetain(ctx, api, leaseID, claim, server, name)
}

func (b *backend) stopCodespaceAndRetain(ctx context.Context, api codespacesAPI, leaseID string, claim LeaseClaim, server Server, name string) error {
	server.Provider = providerName
	server.CloudID = name
	server.Name = name
	server.Status = "stopped"
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	// Core treats "stopped" as an inactive claim state, so an empty SSHTarget clears stale endpoints.
	server.Labels[labelState] = "stopped"
	server.Labels[labelRelease] = releaseStop
	server.Labels[labelCodespaceName] = name
	_, err := updateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, SSHTarget{}, func() error {
		return api.stopCodespace(ctx, name)
	})
	return err
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	if githubCodespacesClaimRelease(lease.LeaseID) == releaseStop {
		return fmt.Sprintf("stopped github-codespaces lease=%s codespace=%s retained=true", lease.LeaseID, firstNonEmpty(lease.Server.CloudID, lease.Server.Name))
	}
	if githubCodespacesDeleteOnRelease(lease, b.cfg) {
		return fmt.Sprintf("deleted github-codespaces lease=%s codespace=%s", lease.LeaseID, firstNonEmpty(lease.Server.CloudID, lease.Server.Name))
	}
	return fmt.Sprintf("stopped github-codespaces lease=%s codespace=%s retained=true", lease.LeaseID, firstNonEmpty(lease.Server.CloudID, lease.Server.Name))
}

func (b *backend) RetainLeaseClaimAfterRelease(lease LeaseTarget) bool {
	switch githubCodespacesClaimRelease(lease.LeaseID) {
	case releaseStop:
		return true
	case releaseDelete:
		return false
	}
	return !githubCodespacesDeleteOnRelease(lease, b.cfg)
}

func githubCodespacesClaimRelease(leaseID string) string {
	claim, ok, err := readLeaseClaimWithPresence(strings.TrimSpace(leaseID))
	if err != nil || !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(claim.Labels[labelRelease]))
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	_, api, login, err := b.controlPlane(ctx)
	if err != nil {
		return err
	}
	live, err := api.listCodespaces(ctx)
	if err != nil {
		return err
	}
	servers, err := b.serversFromCodespaces(live)
	if err != nil {
		return err
	}
	now := b.now().UTC()
	for _, server := range servers {
		shouldDelete, reason := shouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.stderr(), "skip codespace=%s reason=%s\n", server.DisplayID(), reason)
			continue
		}
		claim, ok, err := readLeaseClaimWithPresence(server.Labels["lease"])
		if err != nil {
			return err
		}
		if !ok {
			return exit(3, "refusing to cleanup github-codespaces codespace=%s without local claim", server.DisplayID())
		}
		if err := b.validateClaimForServer(claim, server, login); err != nil {
			return err
		}
		fmt.Fprintf(b.stderr(), "delete codespace=%s lease=%s dry_run=%t\n", server.DisplayID(), claim.LeaseID, req.DryRun)
		if req.DryRun {
			continue
		}
		item, err := api.getCodespace(ctx, server.CloudID)
		if err != nil && !isGitHubNotFound(err) {
			return err
		}
		if err == nil {
			if err := validateDeleteSafe(item); err != nil {
				return err
			}
		}
		if err := api.deleteCodespace(ctx, server.CloudID); err != nil && !isGitHubNotFound(err) {
			return err
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		if err := removeStoredSSHConfig(claim.LeaseID); err != nil {
			return err
		}
	}
	return nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	_, api, _, err := b.controlPlane(ctx)
	checks := []DoctorCheck{}
	if err != nil {
		return DoctorResult{
			Provider: providerName,
			Status:   "failed",
			Message:  "auth=failed control_plane=unchecked inventory=unchecked mutation=false",
			Checks:   append(checks, DoctorCheck{Status: "failed", Check: "auth", Message: err.Error()}),
		}, err
	}
	repo, repoErr := b.resolveRepo(Repo{})
	if repoErr == nil {
		if _, err := api.listMachines(ctx, repo, b.cfg.GitHubCodespaces.Ref); err != nil {
			checks = append(checks, DoctorCheck{Status: "failed", Check: "machines", Message: err.Error(), Details: map[string]string{"repo": repo}})
			return DoctorResult{Provider: providerName, Status: "failed", Message: "auth=ready control_plane=failed inventory=unchecked mutation=false", Checks: checks}, err
		}
		checks = append(checks, DoctorCheck{Status: "ok", Check: "machines", Details: map[string]string{"repo": repo}})
	} else {
		checks = append(checks, DoctorCheck{Status: "warning", Check: "repo", Message: repoErr.Error()})
	}
	live, err := api.listCodespaces(ctx)
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "inventory", Message: err.Error()})
		return DoctorResult{Provider: providerName, Status: "failed", Message: "auth=ready control_plane=ready inventory=failed mutation=false", Checks: checks}, err
	}
	leases := 0
	servers, err := b.serversFromCodespaces(live)
	if err == nil {
		leases = len(servers)
	}
	checks = append(checks, DoctorCheck{Status: "ok", Check: "inventory", Details: map[string]string{"leases": strconv.Itoa(leases)}})
	return DoctorResult{Provider: providerName, Message: fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=unchecked", leases), Checks: checks}, nil
}

func (b *backend) controlPlane(ctx context.Context) (githubCLI, codespacesAPI, string, error) {
	gh := b.ghFactory()
	if err := gh.authStatus(ctx); err != nil {
		return nil, nil, "", err
	}
	login, err := gh.userLogin(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	token := strings.TrimSpace(os.Getenv("GH_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	if token == "" {
		token, err = gh.authToken(ctx)
		if err != nil {
			return nil, nil, "", err
		}
	}
	return gh, b.clientFactory(token), login, nil
}

func (b *backend) waitForAvailable(ctx context.Context, api codespacesAPI, name string) (codespace, error) {
	waitCtx := ctx
	cancel := func() {}
	if b.readyTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, b.readyTimeout)
	}
	defer cancel()

	for {
		item, err := api.getCodespace(waitCtx, name)
		if err != nil {
			return codespace{}, err
		}
		if codespaceAvailable(item.State) {
			return item, nil
		}
		if codespaceTerminal(item.State) {
			return codespace{}, exit(5, "github-codespaces codespace %s entered terminal state=%s", name, item.State)
		}
		select {
		case <-waitCtx.Done():
			return codespace{}, waitCtx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

func (b *backend) sshTarget(ctx context.Context, gh githubCLI, leaseID, codespaceName, repo string, store bool) (SSHTarget, error) {
	data, err := gh.codespaceSSHConfig(ctx, codespaceName)
	if err != nil {
		return SSHTarget{}, err
	}
	if store {
		if _, err := storeSSHConfig(leaseID, data); err != nil {
			return SSHTarget{}, err
		}
	}
	cfg := b.cfg
	cfg = b.repoConfig(repo)
	return selectSSHTarget(cfg, data, codespaceName)
}

func (b *backend) sshPrerequisiteError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w; github-codespaces requires an SSH server in the devcontainer image (for example ghcr.io/devcontainers/features/sshd:1) and git, rsync, and tar in the codespace", err)
}

func (b *backend) resolveRepo(repo Repo) (string, error) {
	if configured := strings.TrimSpace(b.cfg.GitHubCodespaces.Repo); configured != "" {
		if !validRepo(configured) {
			return "", exit(2, "github-codespaces repo must be owner/name")
		}
		return configured, nil
	}
	if parsed := repoFromRemote(repo.RemoteURL); parsed != "" {
		return parsed, nil
	}
	return "", exit(2, "github-codespaces repo is required; set githubCodespaces.repo or --github-codespaces-repo")
}

func (b *backend) githubIdleTimeout() time.Duration {
	if b.cfg.GitHubCodespaces.IdleTimeout > 0 {
		return b.cfg.GitHubCodespaces.IdleTimeout
	}
	if b.cfg.IdleTimeout > 0 {
		return b.cfg.IdleTimeout
	}
	return time.Duration(defaultIdleTimeoutMinutes) * time.Minute
}

func (b *backend) effectiveWorkRoot(repo string) string {
	workRoot := strings.TrimSpace(b.cfg.GitHubCodespaces.WorkRoot)
	if workRootExplicit(&b.cfg) && strings.TrimSpace(b.cfg.WorkRoot) != "" && (workRoot == "" || workRoot == defaultWorkRoot) {
		return strings.TrimSpace(b.cfg.WorkRoot)
	}
	repoName := repoName(repo)
	if workRoot == "" {
		if repoName != "" {
			return "/workspaces/" + repoName
		}
		return defaultWorkRoot
	}
	if workRoot == defaultWorkRoot && repoName != "" && repoName != "crabbox" {
		return "/workspaces/" + repoName
	}
	return workRoot
}

func (b *backend) repoConfig(repo string) Config {
	cfg := b.cfg
	cfg.GitHubCodespaces.WorkRoot = b.effectiveWorkRoot(repo)
	cfg.WorkRoot = cfg.GitHubCodespaces.WorkRoot
	return cfg
}

func githubCodespacesDisplayName(leaseID, slug string) string {
	const maxDisplayNameLength = 48
	name := leaseProviderName(leaseID, slug)
	if len(name) <= maxDisplayNameLength {
		return name
	}
	const prefix = "crabbox-"
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix)+9 {
		return name[:maxDisplayNameLength]
	}
	suffix := name[len(name)-9:]
	slug = strings.Trim(name[len(prefix):len(name)-len(suffix)], "-")
	maxSlug := maxDisplayNameLength - len(prefix) - len(suffix)
	if maxSlug <= 0 {
		return (prefix + suffix[1:])[:maxDisplayNameLength]
	}
	if len(slug) > maxSlug {
		slug = strings.Trim(slug[:maxSlug], "-")
	}
	if slug == "" {
		slug = "lease"
	}
	return prefix + slug + suffix
}

func (b *backend) labelsFor(leaseID, slug, repo, login string, keep bool, release string, item codespace, state string) map[string]string {
	cfg := b.repoConfig(repo)
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, b.now().UTC())
	labels[labelState] = state
	labels[labelRelease] = release
	labels[labelCodespaceName] = item.Name
	labels[labelEnvironmentID] = item.EnvironmentID
	labels[labelRepository] = firstNonEmpty(item.Repository.FullName, repo)
	labels[labelRef] = strings.TrimSpace(b.cfg.GitHubCodespaces.Ref)
	labels[labelMachine] = firstNonEmpty(item.Machine.Name, b.cfg.GitHubCodespaces.Machine)
	labels[labelLogin] = strings.TrimSpace(login)
	labels["work_root"] = cfg.WorkRoot
	return labels
}

func (b *backend) serverFromCodespace(item codespace, labels map[string]string) Server {
	server := Server{
		CloudID:  item.Name,
		Provider: providerName,
		Name:     item.Name,
		Status:   item.State,
		Labels:   cloneLabels(labels),
	}
	server.ServerType.Name = firstNonEmpty(item.Machine.Name, b.cfg.GitHubCodespaces.Machine)
	return server
}

func (b *backend) serversFromCodespaces(items []codespace) ([]LeaseView, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	byName := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		name := firstNonEmpty(claim.CloudID, claim.Labels[labelCodespaceName])
		if name != "" {
			byName[name] = claim
		}
	}
	servers := make([]LeaseView, 0, len(items))
	for _, item := range items {
		claim, ok := byName[item.Name]
		if !ok {
			continue
		}
		server := b.serverFromCodespace(item, cloneLabels(claim.Labels))
		server.Labels[labelCodespaceName] = item.Name
		server.Labels[labelEnvironmentID] = firstNonEmpty(item.EnvironmentID, server.Labels[labelEnvironmentID])
		server.Labels[labelRepository] = firstNonEmpty(item.Repository.FullName, server.Labels[labelRepository])
		server.Labels[labelMachine] = firstNonEmpty(item.Machine.Name, server.Labels[labelMachine])
		servers = append(servers, server)
	}
	return servers, nil
}

func (b *backend) resolveServer(items []codespace, id string) (Server, string, error) {
	servers, err := b.serversFromCodespaces(items)
	if err != nil {
		return Server{}, "", err
	}
	server, leaseID, err := findServerByAlias(servers, id)
	if err != nil {
		return Server{}, "", err
	}
	if leaseID != "" || server.CloudID != "" {
		return server, leaseID, nil
	}
	claim, ok, err := resolveLeaseClaimForProvider(id, providerName)
	if err != nil {
		return Server{}, "", err
	}
	if ok {
		name := firstNonEmpty(claim.CloudID, claim.Labels[labelCodespaceName])
		for _, item := range items {
			if item.Name == name {
				return b.serverFromCodespace(item, cloneLabels(claim.Labels)), claim.LeaseID, nil
			}
		}
		if name != "" {
			return serverFromClaim(claim), claim.LeaseID, nil
		}
	}
	for _, item := range items {
		if item.Name == id {
			claim, ok, err := resolveLeaseClaimForProvider(item.Name, providerName)
			if err != nil {
				return Server{}, "", err
			}
			if !ok {
				return Server{}, "", exit(3, "refusing unmanaged github-codespaces codespace=%s without local claim", item.Name)
			}
			return b.serverFromCodespace(item, cloneLabels(claim.Labels)), claim.LeaseID, nil
		}
	}
	return Server{}, "", nil
}

func (b *backend) mergeLiveServer(server Server, item codespace) Server {
	server.CloudID = item.Name
	server.Provider = providerName
	server.Name = item.Name
	server.Status = item.State
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels[labelCodespaceName] = item.Name
	server.Labels[labelEnvironmentID] = firstNonEmpty(item.EnvironmentID, server.Labels[labelEnvironmentID])
	server.Labels[labelRepository] = firstNonEmpty(item.Repository.FullName, server.Labels[labelRepository])
	server.Labels[labelMachine] = firstNonEmpty(item.Machine.Name, server.Labels[labelMachine])
	server.ServerType.Name = firstNonEmpty(item.Machine.Name, server.ServerType.Name)
	return server
}

func (b *backend) validateClaimForServer(claim LeaseClaim, server Server, login string) error {
	if err := b.validateClaimScope(claim, login); err != nil {
		return err
	}
	if strings.TrimSpace(claim.CloudID) != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
		return exit(3, "github-codespaces claim cloud id mismatch: claim=%s live=%s", claim.CloudID, server.CloudID)
	}
	expectedName := strings.TrimSpace(claim.Labels[labelCodespaceName])
	if expectedName != "" && server.CloudID != "" && expectedName != server.CloudID {
		return exit(3, "github-codespaces claim codespace mismatch: claim=%s live=%s", expectedName, server.CloudID)
	}
	return nil
}

func (b *backend) stderr() io.Writer {
	if b.rt.Stderr != nil {
		return b.rt.Stderr
	}
	return io.Discard
}

func githubCodespacesDeleteOnRelease(lease LeaseTarget, cfg Config) bool {
	if deleteOnReleaseExplicit(cfg) {
		return cfg.GitHubCodespaces.DeleteOnRelease
	}
	if lease.Server.Labels != nil {
		switch strings.ToLower(strings.TrimSpace(lease.Server.Labels[labelRelease])) {
		case releaseDelete:
			return true
		case releaseStop:
			return false
		}
	}
	return cfg.GitHubCodespaces.DeleteOnRelease
}

func codespaceAvailable(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "available")
}

func codespaceStopped(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "shutdown", "shut_down", "stopped":
		return true
	default:
		return false
	}
}

func codespaceTerminal(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "failed", "unavailable", "deleted":
		return true
	default:
		return false
	}
}

func validateDeleteSafe(item codespace) error {
	status := item.GitStatus
	if status.HasUncommittedChanges || status.HasUnpushedChanges || status.Ahead > 0 {
		return exit(3, "refusing to delete github-codespaces codespace=%s with uncommitted or unpushed changes", item.Name)
	}
	return nil
}

func serverFromClaim(claim LeaseClaim) Server {
	server := Server{
		CloudID:  firstNonEmpty(claim.CloudID, claim.Labels[labelCodespaceName]),
		Provider: providerName,
		Name:     firstNonEmpty(claim.CloudID, claim.Labels[labelCodespaceName]),
		Status:   claim.Labels[labelState],
		Labels:   cloneLabels(claim.Labels),
	}
	server.ServerType.Name = claim.Labels[labelMachine]
	return server
}

func repoFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "git@github.com:") {
		return strings.TrimSuffix(strings.TrimPrefix(remote, "git@github.com:"), ".git")
	}
	parsed, err := url.Parse(remote)
	if err == nil && strings.EqualFold(parsed.Host, "github.com") {
		clean := strings.Trim(path.Clean(parsed.Path), "/")
		return strings.TrimSuffix(clean, ".git")
	}
	return ""
}

func repoName(repo string) string {
	_, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok {
		return ""
	}
	return strings.TrimSuffix(name, ".git")
}

func cloneLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func repoRootForClaim(repo Repo) (string, error) {
	if strings.TrimSpace(repo.Root) != "" {
		return repo.Root, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve github-codespaces claim working directory: %w", err)
	}
	return wd, nil
}
