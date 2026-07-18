package githubcodespaces

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	currentUser(context.Context) (githubUser, error)
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
	codespaceSSHConfig(context.Context, string) (string, error)
}

type backend struct {
	spec             ProviderSpec
	cfg              Config
	rt               Runtime
	clientFactory    func(string) codespacesAPI
	ghFactory        func() githubCLI
	bindClaim        func(string, LeaseClaim, Server) (LeaseClaim, error)
	newRecoveryNonce func() (string, error)
	waitSSH          func(context.Context, *SSHTarget, string, time.Duration) error
	now              func() time.Time
	pollInterval     time.Duration
	readyTimeout     time.Duration
}

const (
	githubCodespacesRollbackTimeout = 30 * time.Second
	githubCodespacesDeleteTimeout   = 2 * time.Minute
)

const (
	labelCodespaceName  = "codespace_name"
	labelCodespaceID    = "codespace_id"
	labelDisplayName    = "codespace_display_name"
	labelEnvironmentID  = "codespace_environment_id"
	labelRepository     = "github_repository"
	labelRepositoryID   = "github_repository_id"
	labelRef            = "github_ref"
	labelMachine        = "github_machine"
	labelLogin          = "github_login"
	labelUserID         = "github_user_id"
	labelOwnerID        = "github_owner_id"
	labelRelease        = "release"
	labelState          = "state"
	labelRecovery       = "recovery"
	labelRecoveryNonce  = "recovery_nonce"
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
	b.newRecoveryNonce = newGitHubCodespacesRecoveryNonce
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
	gh, api, user, err := b.controlPlane(ctx)
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
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	if leaseID == "" {
		leaseID = newLeaseID()
	}
	unlockLease, err := lockGitHubCodespacesLeaseOperation(ctx, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	defer unlockLease()

	unlockSlug, err := lockGitHubCodespacesSlugAllocation(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	defer func() {
		if unlockSlug != nil {
			unlockSlug()
		}
	}()
	live, err := api.listCodespaces(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	existing, err := b.serversFromCodespaces(live)
	if err != nil {
		return LeaseTarget{}, err
	}
	// The core allocator checks durable claim files as well as this live-server
	// snapshot, so pending and temporarily absent claims reserve their slugs.
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
	if b.newRecoveryNonce == nil {
		return LeaseTarget{}, exit(2, "generate github-codespaces recovery nonce")
	}
	recoveryNonce, err := b.newRecoveryNonce()
	recoveryNonce = strings.TrimSpace(recoveryNonce)
	if err != nil {
		return LeaseTarget{}, fmt.Errorf("generate github-codespaces recovery nonce: %w", err)
	}
	if recoveryNonce == "" {
		return LeaseTarget{}, exit(2, "generate github-codespaces recovery nonce")
	}
	displayName := githubCodespacesDisplayName(leaseID, slug, recoveryNonce)
	if err := rejectExistingRecoveryIdentity(live, displayName, repo); err != nil {
		return LeaseTarget{}, err
	}
	claim, err := b.claimPendingCreate(leaseID, slug, repo, user, displayName, recoveryNonce, repoRoot, release, req.Keep, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	// The durable pending claim reserves both the lease and slug. Provider
	// creation remains protected by the lease lock, but unrelated acquires may proceed.
	unlockSlug()
	unlockSlug = nil
	created, createErr := api.createCodespace(ctx, createCodespaceRequest{
		Repo:             repo,
		Ref:              strings.TrimSpace(b.cfg.GitHubCodespaces.Ref),
		Machine:          b.effectiveMachine(),
		DevcontainerPath: strings.TrimSpace(b.cfg.GitHubCodespaces.DevcontainerPath),
		WorkingDirectory: strings.TrimSpace(b.cfg.GitHubCodespaces.WorkingDirectory),
		Geo:              strings.TrimSpace(b.cfg.GitHubCodespaces.Geo),
		IdleTimeout:      b.githubIdleTimeout(),
		RetentionPeriod:  b.cfg.GitHubCodespaces.RetentionPeriod,
		RetentionSet:     retentionPeriodExplicit(b.cfg),
		DisplayName:      displayName,
	})
	if createErr == nil && strings.TrimSpace(created.Name) == "" {
		createErr = errors.New("github-codespaces create returned no resource identity")
	}
	if createErr != nil {
		if !githubCodespacesCreateMayHaveSucceeded(createErr) {
			return LeaseTarget{}, errors.Join(createErr, discardPendingClaim(claim))
		}
		recovered, recoveryErr := b.recoverPendingResource(api, claim, user)
		if recoveryErr != nil {
			return LeaseTarget{}, errors.Join(
				fmt.Errorf("github-codespaces create outcome is uncertain for lease=%s display_name=%q repo=%q; recovery claim retained: %w", leaseID, displayName, repo, createErr),
				recoveryErr,
			)
		}
		created = recovered
	} else {
		created, err = validateCreatedCodespaceIdentity(claim, created)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	if req.OnAcquired != nil {
		acquired := LeaseTarget{Server: b.serverFromCodespace(created, cloneLabels(claim.Labels)), LeaseID: leaseID}
		if err := req.OnAcquired(acquired); err != nil {
			return LeaseTarget{}, errors.Join(err, rollbackUnboundCreatedCodespace(api, claim, created))
		}
	}
	claim, err = b.bindValidatedCreatedClaim(claim, created)
	if err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackUnboundCreatedCodespace(api, claim, created))
		}
		return LeaseTarget{}, err
	}
	available, err := b.waitForAvailable(ctx, api, created.Name)
	if err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, err
	}
	if err := validateCodespaceClaimResource(claim, available); err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, err
	}
	if err := validateStopPreservesCodespace(available); err != nil {
		return LeaseTarget{}, errors.Join(err, rollbackCreatedCodespace(api, claim))
	}
	refreshedUser := user
	refreshedUser.Login = claim.Labels[labelLogin]
	labels := b.labelsFor(leaseID, slug, repo, refreshedUser.Login, req.Keep, release, available, "ready", refreshedUser)
	labels[labelDisplayName] = displayName
	labels[labelRecoveryNonce] = recoveryNonce
	server := b.serverFromCodespace(available, labels)
	target, sshConfig, err := b.sshTargetWithConfig(ctx, gh, available.Name, repo)
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
	finalClaim, err := updateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, target, func() error {
		_, err := storeSSHConfig(leaseID, sshConfig)
		return err
	})
	if err != nil {
		if !req.Keep {
			err = errors.Join(err, rollbackCreatedCodespace(api, claim))
		}
		return LeaseTarget{}, err
	}
	setServerLeaseClaimSnapshot(&lease.Server, finalClaim, true)
	fmt.Fprintf(b.stderr(), "provisioned provider=github-codespaces lease=%s slug=%s codespace=%s repo=%s state=%s\n", leaseID, slug, available.Name, repo, available.State)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	gh, api, user, err := b.controlPlane(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	live, err := api.listCodespaces(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	candidate, candidateOK, err := resolveUniqueGitHubCodespacesClaim(req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := ""
	if candidateOK {
		leaseID = candidate.LeaseID
	} else {
		_, leaseID, err = b.resolveServer(live, req.ID)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	if strings.TrimSpace(leaseID) == "" {
		return LeaseTarget{}, exit(4, "github-codespaces lease not found: %s", req.ID)
	}
	unlockOperation, err := lockGitHubCodespacesLeaseOperation(ctx, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	defer unlockOperation()
	currentCandidate, currentCandidateOK, err := resolveUniqueGitHubCodespacesClaim(req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if !currentCandidateOK || currentCandidate.LeaseID != leaseID {
		return LeaseTarget{}, exit(2, "github-codespaces identifier %s changed during resolve; retry", req.ID)
	}
	claim := currentCandidate

	live, err = api.listCodespaces(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if claim.Provider != providerName {
		return LeaseTarget{}, exit(4, "github-codespaces lease %s has no current local claim", leaseID)
	}

	server := Server{}
	pendingReadOnly := false
	if strings.TrimSpace(claim.CloudID) == "" && claim.Labels[labelRecovery] == recoveryPreCreate {
		if req.NoLocalStateMutations {
			item, err := b.matchPendingClaimFromInventory(claim, user, live)
			if err != nil {
				return LeaseTarget{}, err
			}
			server = b.mergeLiveServer(serverFromClaim(claim), item)
			pendingReadOnly = true
		} else {
			var item codespace
			claim, item, err = b.recoverPendingClaimFromInventory(claim, user, live)
			if err != nil {
				return LeaseTarget{}, err
			}
			server = b.mergeLiveServer(serverFromClaim(claim), item)
		}
	} else {
		var resolvedLeaseID string
		server, resolvedLeaseID, err = b.resolveServer(live, leaseID)
		if err != nil {
			return LeaseTarget{}, err
		}
		if resolvedLeaseID != "" && resolvedLeaseID != leaseID {
			return LeaseTarget{}, exit(3, "github-codespaces lease identity changed during resolve: expected=%s got=%s", leaseID, resolvedLeaseID)
		}
	}
	if server.CloudID == "" {
		return LeaseTarget{}, exit(4, "github-codespaces lease not found: %s", req.ID)
	}
	if err := b.validateClaimForServer(claim, server, user); err != nil {
		return LeaseTarget{}, err
	}
	validateResolvedItem := func(item codespace) error {
		if !pendingReadOnly {
			return validateCodespaceClaimResource(claim, item)
		}
		matched, err := b.matchPendingClaimFromInventory(claim, user, []codespace{item})
		if err != nil {
			return err
		}
		if matched.Name != server.CloudID {
			return exit(3, "github-codespaces pending recovery resource changed during resolve: expected=%s got=%s", server.CloudID, matched.Name)
		}
		return nil
	}
	resolveAction := func() (Server, SSHTarget, bool, error) {
		item, err := api.getCodespace(ctx, server.CloudID)
		if err != nil {
			if req.ReleaseOnly && isGitHubNotFound(err) {
				server.Status = "deleted"
				server.Labels[labelState] = "deleted"
				return server, SSHTarget{}, false, nil
			}
			return Server{}, SSHTarget{}, false, err
		}
		if err := validateResolvedItem(item); err != nil {
			return Server{}, SSHTarget{}, false, err
		}
		server = b.mergeLiveServer(server, item)
		if codespaceStopped(item.State) && (req.StatusOnly || req.NoLocalStateMutations) {
			server.Labels[labelState] = "stopped"
			return server, SSHTarget{}, false, nil
		}
		if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
			return server, SSHTarget{}, false, nil
		}
		if codespaceStopping(item.State) {
			waitCtx := ctx
			cancel := func() {}
			if b.readyTimeout > 0 {
				waitCtx, cancel = context.WithTimeout(ctx, b.readyTimeout)
			}
			item, err = b.waitForStopped(waitCtx, api, item.Name)
			cancel()
			if err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			if err := validateResolvedItem(item); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			server = b.mergeLiveServer(server, item)
		}
		if codespaceStopped(item.State) && (req.StatusOnly || req.NoLocalStateMutations) {
			server.Labels[labelState] = "stopped"
			return server, SSHTarget{}, false, nil
		}
		if codespaceStopped(item.State) {
			item, err = api.startCodespace(ctx, item.Name)
			if err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			if strings.TrimSpace(item.Name) != server.CloudID {
				return Server{}, SSHTarget{}, false, exit(3, "github-codespaces start returned a different resource: expected=%s got=%s", server.CloudID, item.Name)
			}
			item, err = b.waitForAvailable(ctx, api, item.Name)
			if err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			if err := validateResolvedItem(item); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			server = b.mergeLiveServer(server, item)
		}
		if codespaceTerminal(item.State) {
			return Server{}, SSHTarget{}, false, exit(5, "github-codespaces codespace %s entered terminal state=%s", item.Name, item.State)
		}
		if !codespaceAvailable(item.State) {
			item, err = b.waitForAvailable(ctx, api, item.Name)
			if err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			if err := validateResolvedItem(item); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			server = b.mergeLiveServer(server, item)
		}
		repo := firstNonEmpty(server.Labels[labelRepository], item.Repository.FullName)
		cfg := b.repoConfig(repo)
		if server.Labels == nil {
			server.Labels = map[string]string{}
		}
		server.Labels["work_root"] = cfg.WorkRoot
		server.Labels = touchDirectLeaseLabels(server.Labels, cfg, "ready", b.now().UTC())
		target, sshConfig, err := b.sshTargetWithConfig(ctx, gh, item.Name, repo)
		if err != nil {
			return Server{}, SSHTarget{}, false, b.sshPrerequisiteError(err)
		}
		if req.ReadyProbe {
			if err := b.waitSSH(ctx, &target, "github-codespaces ssh", b.readyTimeout); err != nil {
				return Server{}, SSHTarget{}, false, b.sshPrerequisiteError(err)
			}
		}
		if !req.NoLocalStateMutations {
			if _, err := storeSSHConfig(leaseID, sshConfig); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
		}
		return server, target, true, nil
	}
	target := SSHTarget{}
	if req.NoLocalStateMutations {
		if err := withLeaseClaimUnchanged(leaseID, claim, func() error {
			var actionErr error
			server, target, _, actionErr = resolveAction()
			return actionErr
		}); err != nil {
			return LeaseTarget{}, err
		}
	} else {
		_, server, target, err = updateLeaseClaimEndpointIfUnchangedAction(leaseID, claim, resolveAction)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func resolveUniqueGitHubCodespacesClaim(identifier string) (LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return LeaseClaim{}, false, nil
	}
	if isCanonicalLeaseID(identifier) {
		claim, exists, err := readLeaseClaimWithPresence(identifier)
		if err != nil {
			return LeaseClaim{}, false, err
		}
		if exists {
			if claim.Provider != providerName {
				return LeaseClaim{}, false, exit(4, "%q is claimed by provider %s", identifier, claim.Provider)
			}
			return claim, true, nil
		}
		return LeaseClaim{}, false, nil
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	var match LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || !leaseClaimMatchesIdentifier(claim, identifier) {
			continue
		}
		if match.LeaseID != "" {
			return LeaseClaim{}, false, exit(2, "multiple github-codespaces claims match identifier %s", identifier)
		}
		match = claim
	}
	return match, match.LeaseID != "", nil
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
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		return Server{}, exit(2, "github-codespaces touch requires a lease id")
	}
	unlockOperation, err := lockGitHubCodespacesLeaseOperation(ctx, leaseID)
	if err != nil {
		return Server{}, err
	}
	defer unlockOperation()

	claim, ok, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return Server{}, err
	}
	if !ok || claim.Provider != providerName {
		return Server{}, exit(4, "github-codespaces touch requires a current local claim for lease %s", leaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" || claim.Labels[labelRecovery] == recoveryPreCreate {
		return Server{}, exit(4, "github-codespaces lease %s is not active", leaseID)
	}
	state := strings.ToLower(strings.TrimSpace(claim.Labels[labelState]))
	if codespaceStopped(state) || codespaceTerminal(state) || state == "paused" || state == "deleting" || state == "deleted" {
		return Server{}, exit(4, "github-codespaces lease %s is not active (state=%s)", leaseID, state)
	}
	requestedName := firstNonEmpty(req.Lease.Server.CloudID, req.Lease.Server.Name, req.Lease.Server.Labels[labelCodespaceName])
	if requestedName != "" && requestedName != claim.CloudID {
		return Server{}, exit(3, "github-codespaces touch resource mismatch: claim=%s request=%s", claim.CloudID, requestedName)
	}
	repo := strings.TrimSpace(claim.Labels[labelRepository])
	if expectedScope := providerClaimScope(b.claimConfig(repo)); expectedScope == "" || claim.ProviderScope != expectedScope {
		return Server{}, exit(4, "github-codespaces claim %s scope mismatch: claim=%q current=%q", claim.LeaseID, claim.ProviderScope, expectedScope)
	}
	server := serverFromClaim(claim)
	server.Labels = touchDirectLeaseLabels(server.Labels, b.repoConfig(repo), req.State, b.now().UTC())
	updated, err := updateLeaseClaimEndpointIfUnchanged(leaseID, claim, server, SSHTarget{})
	if err != nil {
		return Server{}, err
	}
	return serverFromClaim(updated), nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		return exit(2, "github-codespaces release requires a lease id")
	}
	unlockOperation, err := lockGitHubCodespacesLeaseOperation(ctx, leaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	_, api, user, err := b.controlPlane(ctx)
	if err != nil {
		return err
	}
	requestedServer := req.Lease.Server
	claim, claimOK, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !claimOK {
		return exit(2, "github-codespaces release requires a local claim for lease %s", leaseID)
	}
	if strings.TrimSpace(claim.CloudID) == "" && claim.Labels[labelRecovery] == recoveryPreCreate {
		claim, _, err = b.recoverPendingClaim(api, claim, user)
		if err != nil {
			return err
		}
	}
	if requestedName := firstNonEmpty(requestedServer.CloudID, requestedServer.Name, requestedServer.Labels[labelCodespaceName]); requestedName != "" && requestedName != claim.CloudID {
		return exit(3, "github-codespaces release resource mismatch: claim=%s request=%s", claim.CloudID, requestedName)
	}
	server := serverFromClaim(claim)
	if err := b.validateClaimForServer(claim, server, user); err != nil {
		return err
	}
	name := firstNonEmpty(server.CloudID, server.Name, server.Labels[labelCodespaceName])
	if name == "" {
		return exit(2, "github-codespaces release requires a claim-backed codespace name")
	}
	authoritativeLease := req.Lease
	authoritativeLease.Server = server
	if githubCodespacesDeleteOnRelease(authoritativeLease, b.cfg) {
		item, err := api.getCodespace(ctx, name)
		if err != nil && !isGitHubNotFound(err) {
			return err
		}
		if err == nil {
			if err := validateDeleteSafe(item); err != nil {
				return b.stopCodespaceAndRetain(ctx, api, leaseID, claim, server, name)
			}
		}
		if err := b.deleteClaimedCodespace(ctx, api, claim, name); err != nil {
			var unsafe *unsafeCodespaceDeleteError
			if errors.As(err, &unsafe) {
				return b.stopCodespaceAndRetain(ctx, api, leaseID, claim, server, name)
			}
			return err
		}
		return nil
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
	preflight, err := api.getCodespace(ctx, name)
	if err != nil && !isGitHubNotFound(err) {
		return err
	}
	if err == nil {
		if err := validateCodespaceClaimResource(claim, preflight); err != nil {
			return err
		}
		if err := validateStopPreservesCodespace(preflight); err != nil {
			return err
		}
	}
	absent := false
	updated, err := updateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, SSHTarget{}, func() error {
		item, err := api.getCodespace(ctx, name)
		if isGitHubNotFound(err) {
			absent = true
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateCodespaceClaimResource(claim, item); err != nil {
			return err
		}
		if err := validateStopPreservesCodespace(item); err != nil {
			return err
		}
		err = api.stopCodespace(ctx, name)
		if isGitHubNotFound(err) {
			absent = true
			return nil
		}
		return err
	})
	if err != nil || !absent {
		return err
	}
	return removeLeaseClaimIfUnchangedAfter(leaseID, updated, func() error {
		return removeStoredSSHConfig(leaseID)
	})
}

func (b *backend) deleteClaimedCodespace(ctx context.Context, api codespacesAPI, claim LeaseClaim, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || claim.CloudID != name || claim.Labels[labelCodespaceName] != name {
		return exit(4, "refusing github-codespaces delete for lease=%s without its exact bound resource identity", claim.LeaseID)
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		deleteCtx, cancel := context.WithTimeout(ctx, githubCodespacesDeleteTimeout)
		defer cancel()

		item, err := api.getCodespace(deleteCtx, name)
		if isGitHubNotFound(err) {
			return removeStoredSSHConfig(claim.LeaseID)
		}
		if err != nil {
			return err
		}
		if err := validateCodespaceClaimResource(claim, item); err != nil {
			return err
		}
		if err := validateDeleteSafe(item); err != nil {
			return err
		}
		if err := validateStopPreservesCodespace(item); err != nil {
			return err
		}
		// Stop is idempotent. Always issue it so even an initially stopped
		// codespace gets a fresh status and identity check immediately before delete.
		if err := api.stopCodespace(deleteCtx, name); err != nil {
			if isGitHubNotFound(err) {
				return removeStoredSSHConfig(claim.LeaseID)
			}
			return err
		}
		item, err = b.waitForStopped(deleteCtx, api, name)
		if isGitHubNotFound(err) {
			return removeStoredSSHConfig(claim.LeaseID)
		}
		if err != nil {
			return err
		}
		if err := validateCodespaceClaimResource(claim, item); err != nil {
			return err
		}
		if err := validateDeleteSafe(item); err != nil {
			return err
		}
		if err := api.deleteCodespace(deleteCtx, name); err != nil && !isGitHubNotFound(err) {
			return err
		}
		if err := waitForCodespaceDeleted(deleteCtx, api, name, b.pollInterval, func(item codespace) error {
			return validateCodespaceClaimResource(claim, item)
		}); err != nil {
			return err
		}
		return removeStoredSSHConfig(claim.LeaseID)
	})
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
	_, api, user, err := b.controlPlane(ctx)
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
	seen := make(map[string]string, len(servers))
	for _, server := range servers {
		seen[strings.ToLower(strings.TrimSpace(server.CloudID))] = server.Labels["lease"]
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return err
	}
	boundOwners := make(map[string]string, len(claims))
	for _, claim := range claims {
		cloudID := strings.TrimSpace(claim.CloudID)
		if claim.Provider != providerName || cloudID == "" {
			continue
		}
		key := strings.ToLower(cloudID)
		if otherLease, ok := boundOwners[key]; ok && otherLease != claim.LeaseID {
			return exit(3, "multiple github-codespaces claims bind resource %s: leases=%s,%s", cloudID, otherLease, claim.LeaseID)
		}
		boundOwners[key] = claim.LeaseID
	}
	now := b.now().UTC()
	for _, snapshotClaim := range claims {
		cloudID := strings.TrimSpace(snapshotClaim.CloudID)
		if snapshotClaim.Provider != providerName || cloudID == "" {
			continue
		}
		if otherLease, ok := seen[strings.ToLower(cloudID)]; ok {
			if otherLease != snapshotClaim.LeaseID {
				return exit(3, "multiple github-codespaces claims bind resource %s: leases=%s,%s", cloudID, otherLease, snapshotClaim.LeaseID)
			}
			continue
		}
		claim, item, found, err := func() (LeaseClaim, codespace, bool, error) {
			unlockOperation, err := lockGitHubCodespacesLeaseOperation(ctx, snapshotClaim.LeaseID)
			if err != nil {
				return LeaseClaim{}, codespace{}, false, err
			}
			defer unlockOperation()

			claim, ok, err := readLeaseClaimWithPresence(snapshotClaim.LeaseID)
			if err != nil || !ok {
				return claim, codespace{}, false, err
			}
			cloudID := strings.TrimSpace(claim.CloudID)
			if claim.Provider != providerName || cloudID == "" {
				return claim, codespace{}, false, nil
			}
			if err := b.validateClaimScope(claim, user); err != nil {
				return claim, codespace{}, false, err
			}
			if otherLease, ok := seen[strings.ToLower(cloudID)]; ok {
				if otherLease != claim.LeaseID {
					return claim, codespace{}, false, exit(3, "multiple github-codespaces claims bind resource %s: leases=%s,%s", cloudID, otherLease, claim.LeaseID)
				}
				return claim, codespace{}, false, nil
			}
			shouldDiscard, reason := shouldDiscardMissingClaim(claim, now)
			if !shouldDiscard {
				fmt.Fprintf(b.stderr(), "skip lease=%s reason=claimed-resource-missing\n", claim.LeaseID)
				return claim, codespace{}, false, nil
			}
			item, err := api.getCodespace(ctx, cloudID)
			if err == nil {
				if err := validateCodespaceClaimResource(claim, item); err != nil {
					return claim, codespace{}, false, err
				}
				return claim, item, true, nil
			}
			if !isGitHubNotFound(err) {
				return claim, codespace{}, false, err
			}
			fmt.Fprintf(b.stderr(), "discard lease=%s reason=%s dry_run=%t\n", claim.LeaseID, reason, req.DryRun)
			if req.DryRun {
				return claim, codespace{}, false, nil
			}
			return claim, codespace{}, false, discardMissingBoundClaim(claim)
		}()
		if err != nil {
			return err
		}
		if found {
			key := strings.ToLower(strings.TrimSpace(item.Name))
			if otherLease, ok := seen[key]; ok {
				return exit(3, "multiple github-codespaces claims bind resource %s: leases=%s,%s", item.Name, otherLease, claim.LeaseID)
			}
			seen[key] = claim.LeaseID
			servers = append(servers, b.mergeLiveServer(serverFromClaim(claim), item))
		}
	}
	for _, snapshotClaim := range claims {
		if snapshotClaim.Provider != providerName || strings.TrimSpace(snapshotClaim.CloudID) != "" || snapshotClaim.Labels[labelRecovery] != recoveryPreCreate {
			continue
		}
		claim, item, found, err := func() (LeaseClaim, codespace, bool, error) {
			unlockOperation, err := lockGitHubCodespacesLeaseOperation(ctx, snapshotClaim.LeaseID)
			if err != nil {
				return LeaseClaim{}, codespace{}, false, err
			}
			defer unlockOperation()

			claim, ok, err := readLeaseClaimWithPresence(snapshotClaim.LeaseID)
			if err != nil || !ok {
				return claim, codespace{}, false, err
			}
			if claim.Provider != providerName || strings.TrimSpace(claim.CloudID) != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
				return claim, codespace{}, false, nil
			}
			currentLive, err := api.listCodespaces(ctx)
			if err != nil {
				return claim, codespace{}, false, err
			}
			item, found, err := b.findPendingClaimInInventory(claim, user, currentLive)
			if err != nil || found {
				return claim, item, found, err
			}
			shouldDiscard, reason := shouldDiscardMissingClaim(claim, now)
			if !shouldDiscard {
				fmt.Fprintf(b.stderr(), "skip lease=%s reason=create-recovery-pending\n", claim.LeaseID)
				return claim, codespace{}, false, nil
			}
			fmt.Fprintf(b.stderr(), "discard lease=%s reason=%s dry_run=%t\n", claim.LeaseID, reason, req.DryRun)
			if req.DryRun {
				return claim, codespace{}, false, nil
			}
			return claim, codespace{}, false, discardPendingClaim(claim)
		}()
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(item.Name))
		if otherLease, ok := seen[key]; ok {
			return exit(3, "multiple github-codespaces claims bind recovery resource %s: leases=%s,%s", item.Name, otherLease, claim.LeaseID)
		}
		seen[key] = claim.LeaseID
		servers = append(servers, b.mergeLiveServer(serverFromClaim(claim), item))
	}
	for _, listedServer := range servers {
		err := func() error {
			leaseID := strings.TrimSpace(listedServer.Labels["lease"])
			if leaseID == "" {
				return exit(3, "refusing to cleanup github-codespaces codespace=%s without a lease identity", listedServer.DisplayID())
			}
			unlockOperation, err := lockGitHubCodespacesLeaseOperation(ctx, leaseID)
			if err != nil {
				return err
			}
			defer unlockOperation()

			claim, ok, err := readLeaseClaimWithPresence(leaseID)
			if err != nil {
				return err
			}
			if !ok {
				return exit(3, "refusing to cleanup github-codespaces codespace=%s without local claim", listedServer.DisplayID())
			}
			server := serverFromClaim(claim)
			pending := strings.TrimSpace(claim.CloudID) == "" && claim.Labels[labelRecovery] == recoveryPreCreate
			pendingItem := codespace{}
			if pending {
				var found bool
				pendingItem, found, err = b.findPendingClaimInInventory(claim, user, live)
				if err != nil {
					return err
				}
				if !found {
					fmt.Fprintf(b.stderr(), "skip lease=%s reason=create-recovery-pending\n", leaseID)
					return nil
				}
				if listedServer.CloudID != "" && listedServer.CloudID != pendingItem.Name {
					return exit(2, "github-codespaces pending claim %s resource changed during cleanup; retry", leaseID)
				}
				server = b.mergeLiveServer(server, pendingItem)
			} else if listedServer.CloudID != "" && listedServer.CloudID != server.CloudID {
				return exit(2, "github-codespaces claim %s resource changed during cleanup; retry", leaseID)
			}
			if !pending {
				if err := b.validateClaimForServer(claim, server, user); err != nil {
					return err
				}
			}
			shouldDelete, reason := shouldCleanupServer(server, now)
			if !shouldDelete {
				fmt.Fprintf(b.stderr(), "skip codespace=%s reason=%s\n", server.DisplayID(), reason)
				return nil
			}
			if req.DryRun {
				item := pendingItem
				if !pending {
					item, err = api.getCodespace(ctx, server.CloudID)
					if err != nil {
						if isGitHubNotFound(err) {
							fmt.Fprintf(b.stderr(), "delete codespace=%s lease=%s dry_run=true reason=already-absent\n", server.DisplayID(), claim.LeaseID)
							return nil
						}
						return err
					}
					if err := validateCodespaceClaimResource(claim, item); err != nil {
						return err
					}
				}
				if err := validateDeleteSafe(item); err != nil {
					var unsafe *unsafeCodespaceDeleteError
					if !errors.As(err, &unsafe) {
						return err
					}
					if err := validateStopPreservesCodespace(item); err != nil {
						return err
					}
					fmt.Fprintf(b.stderr(), "retain codespace=%s lease=%s action=stop dry_run=true reason=uncommitted-or-unpushed-changes\n", server.DisplayID(), claim.LeaseID)
					return nil
				}
				if err := validateStopPreservesCodespace(item); err != nil {
					return err
				}
				fmt.Fprintf(b.stderr(), "delete codespace=%s lease=%s dry_run=true\n", server.DisplayID(), claim.LeaseID)
				return nil
			}
			fmt.Fprintf(b.stderr(), "delete codespace=%s lease=%s dry_run=false\n", server.DisplayID(), claim.LeaseID)
			if pending {
				claim, pendingItem, err = b.bindCreatedClaim(claim, pendingItem)
				if err != nil {
					return err
				}
				server = b.mergeLiveServer(serverFromClaim(claim), pendingItem)
				if err := b.validateClaimForServer(claim, server, user); err != nil {
					return err
				}
			}
			item, getErr := api.getCodespace(ctx, server.CloudID)
			if getErr != nil && !isGitHubNotFound(getErr) {
				return getErr
			}
			if getErr == nil {
				if err := validateCodespaceClaimResource(claim, item); err != nil {
					return err
				}
				if err := validateDeleteSafe(item); err != nil {
					var unsafe *unsafeCodespaceDeleteError
					if !errors.As(err, &unsafe) {
						return err
					}
					if retainErr := b.stopCodespaceAndRetain(ctx, api, claim.LeaseID, claim, server, server.CloudID); retainErr != nil {
						return errors.Join(err, retainErr)
					}
					fmt.Fprintf(b.stderr(), "retained codespace=%s lease=%s reason=uncommitted-or-unpushed-changes\n", server.DisplayID(), claim.LeaseID)
					return nil
				}
			}
			if err := b.deleteClaimedCodespace(ctx, api, claim, server.CloudID); err != nil {
				var unsafe *unsafeCodespaceDeleteError
				if !errors.As(err, &unsafe) {
					return err
				}
				if retainErr := b.stopCodespaceAndRetain(ctx, api, claim.LeaseID, claim, server, server.CloudID); retainErr != nil {
					return errors.Join(err, retainErr)
				}
				fmt.Fprintf(b.stderr(), "retained codespace=%s lease=%s reason=uncommitted-or-unpushed-changes\n", server.DisplayID(), claim.LeaseID)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func shouldDiscardMissingClaim(claim LeaseClaim, now time.Time) (bool, string) {
	server := serverFromClaim(claim)
	server.Labels = cloneLabels(server.Labels)
	server.Labels[labelState] = "provisioning"
	return shouldCleanupServer(server, now)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	_, api, user, err := b.controlPlane(ctx)
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
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "claims", Message: err.Error()})
		return DoctorResult{Provider: providerName, Status: "failed", Message: "auth=ready control_plane=ready inventory=unsafe mutation=false", Checks: checks}, err
	}
	leases = len(servers)
	claims, err := listLeaseClaims()
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "claims", Message: err.Error()})
		return DoctorResult{Provider: providerName, Status: "failed", Message: "auth=ready control_plane=ready inventory=unsafe mutation=false", Checks: checks}, err
	}
	liveNames := make(map[string]struct{}, len(live))
	for _, item := range live {
		liveNames[strings.ToLower(strings.TrimSpace(item.Name))] = struct{}{}
	}
	stranded := 0
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if err := b.validateClaimScope(claim, user); err != nil {
			checks = append(checks, DoctorCheck{Status: "failed", Check: "claim-scope", Message: err.Error(), Details: map[string]string{"lease": claim.LeaseID}})
			return DoctorResult{Provider: providerName, Status: "failed", Message: "auth=ready control_plane=ready inventory=unsafe mutation=false", Checks: checks}, err
		}
		name := strings.ToLower(strings.TrimSpace(claim.CloudID))
		if name != "" {
			if _, ok := liveNames[name]; !ok {
				stranded++
			}
		}
	}
	if stranded > 0 {
		checks = append(checks, DoctorCheck{Status: "warning", Check: "claims", Message: "local claims are absent from current GitHub Codespaces inventory", Details: map[string]string{"stranded": strconv.Itoa(stranded)}})
	}
	checks = append(checks, DoctorCheck{Status: "ok", Check: "inventory", Details: map[string]string{"leases": strconv.Itoa(leases)}})
	return DoctorResult{Provider: providerName, Message: fmt.Sprintf("auth=ready control_plane=ready inventory=ready api=list mutation=false leases=%d stranded=%d runtime=unchecked", leases, stranded), Checks: checks}, nil
}

func (b *backend) controlPlane(ctx context.Context) (githubCLI, codespacesAPI, githubUser, error) {
	gh := b.ghFactory()
	if err := gh.authStatus(ctx); err != nil {
		return nil, nil, githubUser{}, err
	}
	var err error
	token := githubCodespacesTokenFromEnv(b.cfg.GitHubCodespaces)
	if token == "" {
		token, err = gh.authToken(ctx)
		if err != nil {
			return nil, nil, githubUser{}, err
		}
	}
	api := b.clientFactory(token)
	user, err := api.currentUser(ctx)
	if err != nil {
		return nil, nil, githubUser{}, err
	}
	return gh, api, user, nil
}

func githubCodespacesTokenFromEnv(cfg GitHubCodespacesConfig) string {
	names := []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
	if githubCodespacesUsesDotcomTokenEnv(cfg) {
		names = []string{"GH_TOKEN", "GITHUB_TOKEN"}
	}
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
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

func (b *backend) waitForStopped(ctx context.Context, api codespacesAPI, name string) (codespace, error) {
	for {
		item, err := api.getCodespace(ctx, name)
		if err != nil {
			return codespace{}, err
		}
		if codespaceStopped(item.State) {
			return item, nil
		}
		if codespaceTerminal(item.State) {
			return codespace{}, exit(5, "github-codespaces codespace %s entered terminal state=%s while stopping", name, item.State)
		}
		select {
		case <-ctx.Done():
			return codespace{}, ctx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

func waitForCodespaceDeleted(ctx context.Context, api codespacesAPI, name string, pollInterval time.Duration, validate func(codespace) error) error {
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	for {
		item, err := api.getCodespace(ctx, name)
		if isGitHubNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if validate != nil {
			if err := validate(item); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("confirm github-codespaces deletion codespace=%s: %w", name, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func (b *backend) sshTargetWithConfig(ctx context.Context, gh githubCLI, codespaceName, repo string) (SSHTarget, string, error) {
	data, err := gh.codespaceSSHConfig(ctx, codespaceName)
	if err != nil {
		return SSHTarget{}, "", err
	}
	cfg := b.cfg
	cfg = b.repoConfig(repo)
	target, err := selectSSHTarget(cfg, data, codespaceName)
	return target, data, err
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

func (b *backend) effectiveMachine() string {
	if b.cfg.ServerTypeExplicit && strings.TrimSpace(b.cfg.ServerType) != "" {
		return strings.TrimSpace(b.cfg.ServerType)
	}
	return firstNonEmpty(strings.TrimSpace(b.cfg.GitHubCodespaces.Machine), defaultCodespaceMachine)
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

func githubCodespacesDisplayName(leaseID, slug, recoveryNonce string) string {
	const maxDisplayNameLength = 48
	name := leaseProviderName(leaseID, slug)
	const prefix = "crabbox-"
	nonceHash := sha256.Sum256([]byte(recoveryNonce))
	nonceSuffix := fmt.Sprintf("-%x", nonceHash[:8])
	if len(name)+len(nonceSuffix) <= maxDisplayNameLength {
		return name + nonceSuffix
	}
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix)+9 {
		trimmed := strings.TrimRight(name[:maxDisplayNameLength-len(nonceSuffix)], "-")
		return trimmed + nonceSuffix
	}
	leaseSuffix := name[len(name)-9:]
	slug = strings.Trim(name[len(prefix):len(name)-len(leaseSuffix)], "-")
	maxSlug := maxDisplayNameLength - len(prefix) - len(leaseSuffix) - len(nonceSuffix)
	if maxSlug <= 0 {
		return prefix + strings.TrimPrefix(leaseSuffix, "-") + nonceSuffix
	}
	if len(slug) > maxSlug {
		slug = strings.Trim(slug[:maxSlug], "-")
	}
	if slug == "" {
		slug = "lease"
	}
	return prefix + slug + leaseSuffix + nonceSuffix
}

func newGitHubCodespacesRecoveryNonce() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func (b *backend) labelsFor(leaseID, slug, repo, login string, keep bool, release string, item codespace, state string, users ...githubUser) map[string]string {
	cfg := b.repoConfig(repo)
	user := githubUser{ID: item.Owner.ID, Login: strings.TrimSpace(login)}
	if len(users) > 0 {
		user = users[0]
	}
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, b.now().UTC())
	labels[labelState] = state
	labels[labelRelease] = release
	labels[labelCodespaceName] = item.Name
	if item.ID > 0 {
		labels[labelCodespaceID] = strconv.FormatInt(item.ID, 10)
	}
	labels[labelEnvironmentID] = item.EnvironmentID
	if item.Owner.ID > 0 {
		labels[labelOwnerID] = strconv.FormatInt(item.Owner.ID, 10)
	}
	labels[labelRepository] = firstNonEmpty(item.Repository.FullName, repo)
	if item.Repository.ID > 0 {
		labels[labelRepositoryID] = strconv.FormatInt(item.Repository.ID, 10)
	}
	labels[labelRef] = strings.TrimSpace(b.cfg.GitHubCodespaces.Ref)
	labels[labelMachine] = firstNonEmpty(item.Machine.Name, b.effectiveMachine())
	labels[labelLogin] = strings.TrimSpace(user.Login)
	if user.ID > 0 {
		labels[labelUserID] = strconv.FormatInt(user.ID, 10)
	}
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
	server.ServerType.Name = firstNonEmpty(item.Machine.Name, b.effectiveMachine())
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
		cloudID := strings.TrimSpace(claim.CloudID)
		labelName := strings.TrimSpace(claim.Labels[labelCodespaceName])
		if cloudID == "" && labelName == "" {
			continue
		}
		if cloudID == "" || labelName == "" || cloudID != labelName {
			return nil, exit(3, "github-codespaces claim %s has inconsistent resource identity: cloud_id=%q label=%q", claim.LeaseID, cloudID, labelName)
		}
		key := strings.ToLower(cloudID)
		if other, exists := byName[key]; exists {
			return nil, exit(3, "multiple github-codespaces claims bind resource %s: leases=%s,%s", cloudID, other.LeaseID, claim.LeaseID)
		}
		byName[key] = claim
	}
	servers := make([]LeaseView, 0, len(items))
	seenLive := make(map[string]struct{}, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return nil, exit(3, "github-codespaces inventory returned a resource without a name")
		}
		key := strings.ToLower(name)
		if _, exists := seenLive[key]; exists {
			return nil, exit(3, "github-codespaces inventory returned duplicate resource name %s", name)
		}
		seenLive[key] = struct{}{}
		claim, ok := byName[key]
		if !ok {
			continue
		}
		if err := validateCodespaceClaimResource(claim, item); err != nil {
			return nil, err
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
	claim, ok, err := resolveUniqueGitHubCodespacesClaim(id)
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
			claim, ok, err := resolveUniqueGitHubCodespacesClaim(item.Name)
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
	if item.ID > 0 {
		server.Labels[labelCodespaceID] = strconv.FormatInt(item.ID, 10)
	}
	server.Labels[labelEnvironmentID] = firstNonEmpty(item.EnvironmentID, server.Labels[labelEnvironmentID])
	if item.Owner.ID > 0 {
		server.Labels[labelOwnerID] = strconv.FormatInt(item.Owner.ID, 10)
	}
	server.Labels[labelRepository] = firstNonEmpty(item.Repository.FullName, server.Labels[labelRepository])
	if item.Repository.ID > 0 {
		server.Labels[labelRepositoryID] = strconv.FormatInt(item.Repository.ID, 10)
	}
	server.Labels[labelMachine] = firstNonEmpty(item.Machine.Name, server.Labels[labelMachine])
	server.ServerType.Name = firstNonEmpty(item.Machine.Name, server.ServerType.Name)
	return server
}

func (b *backend) validateClaimForServer(claim LeaseClaim, server Server, user githubUser) error {
	if err := b.validateClaimScope(claim, user); err != nil {
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

func codespaceStopping(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "shuttingdown") || strings.EqualFold(strings.TrimSpace(state), "shutting_down")
}

func codespaceTerminal(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "failed", "unavailable", "deleted", "archived", "moved":
		return true
	default:
		return false
	}
}

func validateStopPreservesCodespace(item codespace) error {
	if item.RetentionPeriodMinutes == nil {
		return exit(3, "refusing to stop github-codespaces codespace=%s without an effective retention period", item.Name)
	}
	if *item.RetentionPeriodMinutes <= 0 {
		return exit(3, "refusing to stop github-codespaces codespace=%s because effective retention is zero", item.Name)
	}
	return nil
}

type unsafeCodespaceDeleteError struct {
	err error
}

func (e *unsafeCodespaceDeleteError) Error() string {
	return e.err.Error()
}

func (e *unsafeCodespaceDeleteError) Unwrap() error {
	return e.err
}

func validateDeleteSafe(item codespace) error {
	status := item.GitStatus
	if !status.deletionSafetyKnown() {
		return &unsafeCodespaceDeleteError{err: exit(3, "refusing to delete github-codespaces codespace=%s because Git status safety fields are missing", item.Name)}
	}
	if status.HasUncommittedChanges || status.HasUnpushedChanges || status.Ahead > 0 {
		return &unsafeCodespaceDeleteError{err: exit(3, "refusing to delete github-codespaces codespace=%s with uncommitted or unpushed changes", item.Name)}
	}
	return nil
}

func validateCodespaceClaimResource(claim LeaseClaim, item codespace) error {
	expectedName := strings.TrimSpace(claim.Labels[labelCodespaceName])
	if expectedName == "" || strings.TrimSpace(claim.CloudID) != expectedName || strings.TrimSpace(item.Name) != expectedName {
		return exit(4, "refusing github-codespaces delete for lease=%s after resource identity changed", claim.LeaseID)
	}
	for _, identity := range []struct {
		label string
		live  string
		name  string
	}{
		{label: labelCodespaceID, live: strconv.FormatInt(item.ID, 10), name: "codespace id"},
		{label: labelEnvironmentID, live: item.EnvironmentID, name: "environment id"},
		{label: labelOwnerID, live: strconv.FormatInt(item.Owner.ID, 10), name: "owner id"},
		{label: labelRepositoryID, live: strconv.FormatInt(item.Repository.ID, 10), name: "repository id"},
	} {
		expected := strings.TrimSpace(claim.Labels[identity.label])
		if expected == "" || strings.TrimSpace(identity.live) == "" {
			return exit(4, "refusing github-codespaces mutation for lease=%s without complete %s identity", claim.LeaseID, identity.name)
		}
		if strings.TrimSpace(identity.live) != expected {
			return exit(4, "refusing github-codespaces delete for lease=%s after %s changed", claim.LeaseID, identity.name)
		}
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
