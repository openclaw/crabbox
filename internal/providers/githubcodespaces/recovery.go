package githubcodespaces

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func githubCodespacesClaimScope(cfg Config) string {
	endpoint := strings.TrimSpace(cfg.GitHubCodespaces.APIURL)
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err == nil && parsed.Host != "" {
			parsed.User = nil
			parsed.RawQuery = ""
			parsed.Fragment = ""
			parsed.Scheme = strings.ToLower(parsed.Scheme)
			parsed.Host = strings.ToLower(parsed.Host)
			parsed.Path = strings.TrimRight(parsed.Path, "/")
			parsed.RawPath = ""
			endpoint = parsed.String()
		} else {
			endpoint = strings.TrimRight(endpoint, "/")
		}
	}
	parts := make([]string, 0, 2)
	if endpoint != "" && !strings.EqualFold(endpoint, defaultAPIURL) {
		parts = append(parts, "endpoint:"+endpoint)
	}
	if repo := strings.ToLower(strings.TrimSpace(cfg.GitHubCodespaces.Repo)); repo != "" {
		parts = append(parts, "repo:"+repo)
	}
	return strings.Join(parts, "|")
}

func (b *backend) claimConfig(repo string) Config {
	cfg := b.repoConfig(repo)
	cfg.GitHubCodespaces.Repo = strings.TrimSpace(repo)
	if strings.TrimSpace(cfg.GitHubCodespaces.APIURL) == "" {
		cfg.GitHubCodespaces.APIURL = defaultAPIURL
	}
	return cfg
}

func (b *backend) validateClaimScope(claim LeaseClaim, user githubUser) error {
	if claim.Provider != providerName {
		return exit(4, "%q is claimed by provider %s", claim.LeaseID, claim.Provider)
	}
	repo := strings.TrimSpace(claim.Labels[labelRepository])
	expectedScope := providerClaimScope(b.claimConfig(repo))
	if expectedScope == "" || claim.ProviderScope != expectedScope {
		return exit(4, "github-codespaces claim %s scope mismatch: claim=%q current=%q", claim.LeaseID, claim.ProviderScope, expectedScope)
	}
	expectedUserID, err := strconv.ParseInt(strings.TrimSpace(claim.Labels[labelUserID]), 10, 64)
	if err != nil || expectedUserID <= 0 {
		return exit(4, "github-codespaces claim %s has no valid authenticated user identity", claim.LeaseID)
	}
	if expectedUserID != user.ID {
		return exit(3, "github-codespaces account mismatch: current login=%s id=%d does not match lease login=%s id=%d", user.Login, user.ID, claim.Labels[labelLogin], expectedUserID)
	}
	if repo == "" {
		return exit(4, "github-codespaces claim %s has no repository identity", claim.LeaseID)
	}
	return nil
}

func (b *backend) claimPendingCreate(leaseID, slug, repo string, user githubUser, displayName, recoveryNonce, repoRoot, release string, keep, reclaim bool) (LeaseClaim, error) {
	recoveryNonce = strings.TrimSpace(recoveryNonce)
	if recoveryNonce == "" {
		return LeaseClaim{}, exit(2, "github-codespaces pending claim requires a recovery nonce")
	}
	labels := b.labelsFor(leaseID, slug, repo, user.Login, keep, release, codespace{}, "provisioning", user)
	delete(labels, labelCodespaceName)
	delete(labels, labelCodespaceID)
	delete(labels, labelEnvironmentID)
	delete(labels, labelOwnerID)
	labels[labelDisplayName] = displayName
	labels[labelRecovery] = recoveryPreCreate
	labels[labelRecoveryNonce] = recoveryNonce
	server := Server{
		Provider: providerName,
		Name:     displayName,
		Status:   "provisioning",
		Labels:   labels,
	}
	return claimLeaseTargetForRepoConfigIfUnchangedDurable(
		leaseID,
		slug,
		b.claimConfig(repo),
		server,
		SSHTarget{},
		repoRoot,
		b.cfg.IdleTimeout,
		reclaim,
		LeaseClaim{},
		false,
	)
}

func (b *backend) recoverPendingClaim(api codespacesAPI, claim LeaseClaim, user githubUser) (LeaseClaim, codespace, error) {
	item, err := b.recoverPendingResource(api, claim, user)
	if err != nil {
		return claim, codespace{}, err
	}
	return b.bindCreatedClaim(claim, item)
}

func (b *backend) recoverPendingResource(api codespacesAPI, claim LeaseClaim, user githubUser) (codespace, error) {
	if err := b.validateClaimScope(claim, user); err != nil {
		return codespace{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
	defer cancel()
	items, err := api.listCodespaces(ctx)
	if err != nil {
		return codespace{}, errors.Join(
			exit(4, "github-codespaces create recovery is still pending for lease=%s; claim retained", claim.LeaseID),
			err,
		)
	}
	return b.matchPendingClaimFromInventory(claim, user, items)
}

func (b *backend) recoverPendingClaimFromInventory(claim LeaseClaim, user githubUser, items []codespace) (LeaseClaim, codespace, error) {
	item, err := b.matchPendingClaimFromInventory(claim, user, items)
	if err != nil {
		return claim, codespace{}, err
	}
	return b.bindCreatedClaim(claim, item)
}

func (b *backend) matchPendingClaimFromInventory(claim LeaseClaim, user githubUser, items []codespace) (codespace, error) {
	item, found, err := b.findPendingClaimInInventory(claim, user, items)
	if err != nil {
		return codespace{}, err
	}
	if !found {
		return codespace{}, exit(4, "github-codespaces create recovery is still pending for lease=%s; claim retained", claim.LeaseID)
	}
	return item, nil
}

func (b *backend) findPendingClaimInInventory(claim LeaseClaim, user githubUser, items []codespace) (codespace, bool, error) {
	if err := b.validateClaimScope(claim, user); err != nil {
		return codespace{}, false, err
	}
	if claim.Labels[labelRecovery] != recoveryPreCreate || strings.TrimSpace(claim.CloudID) != "" {
		return codespace{}, false, exit(4, "github-codespaces claim %s has no valid pending-create recovery state", claim.LeaseID)
	}
	displayName := strings.TrimSpace(claim.Labels[labelDisplayName])
	repo := strings.TrimSpace(claim.Labels[labelRepository])
	recoveryNonce := strings.TrimSpace(claim.Labels[labelRecoveryNonce])
	if recoveryNonce == "" {
		return codespace{}, false, exit(4, "github-codespaces legacy pending claim %s has no recovery nonce; manual recovery required; claim retained", claim.LeaseID)
	}
	expectedDisplayName := githubCodespacesDisplayName(claim.LeaseID, claim.Slug, recoveryNonce)
	if displayName == "" || repo == "" || displayName != expectedDisplayName {
		return codespace{}, false, exit(4, "github-codespaces claim %s has incomplete or inconsistent recovery identity; claim retained", claim.LeaseID)
	}
	matches := make([]codespace, 0, 2)
	for _, item := range items {
		if item.DisplayName == displayName && strings.EqualFold(item.Repository.FullName, repo) {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return codespace{}, false, nil
	case 1:
		if strings.TrimSpace(matches[0].Name) == "" {
			return codespace{}, false, exit(4, "matched github-codespaces recovery result has no resource identity; claim retained")
		}
		item, err := validateCreatedCodespaceIdentity(claim, matches[0])
		return item, err == nil, err
	default:
		return codespace{}, false, exit(4, "multiple github-codespaces resources match recovery display name %q repo %q; claim retained", displayName, repo)
	}
}

func rejectExistingRecoveryIdentity(items []codespace, displayName, repo string) error {
	for _, item := range items {
		if item.DisplayName == displayName && strings.EqualFold(strings.TrimSpace(item.Repository.FullName), strings.TrimSpace(repo)) {
			return exit(3, "refusing github-codespaces create because recovery identity display_name=%q repo=%q already exists before create", displayName, repo)
		}
	}
	return nil
}

func (b *backend) bindCreatedClaim(expected LeaseClaim, item codespace) (LeaseClaim, codespace, error) {
	item, err := validateCreatedCodespaceIdentity(expected, item)
	if err != nil {
		return expected, codespace{}, err
	}
	claim, err := b.bindValidatedCreatedClaim(expected, item)
	return claim, item, err
}

func validateCreatedCodespaceIdentity(expected LeaseClaim, item codespace) (codespace, error) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return codespace{}, exit(4, "github-codespaces create returned no resource identity; recovery claim retained")
	}
	displayName := strings.TrimSpace(expected.Labels[labelDisplayName])
	recoveryNonce := strings.TrimSpace(expected.Labels[labelRecoveryNonce])
	repo := strings.TrimSpace(expected.Labels[labelRepository])
	if recoveryNonce == "" || displayName != githubCodespacesDisplayName(expected.LeaseID, expected.Slug, recoveryNonce) {
		return codespace{}, exit(4, "github-codespaces create recovery identity changed before binding; claim retained")
	}
	if item.DisplayName != "" && item.DisplayName != displayName {
		return codespace{}, exit(4, "github-codespaces create display name mismatch: got %q want %q; recovery claim retained", item.DisplayName, displayName)
	}
	liveRepo := strings.TrimSpace(item.Repository.FullName)
	if liveRepo == "" {
		return codespace{}, exit(4, "github-codespaces create returned no repository identity; recovery claim retained")
	}
	if !strings.EqualFold(liveRepo, repo) {
		return codespace{}, exit(4, "github-codespaces create repository mismatch: got %q want %q; recovery claim retained", item.Repository.FullName, repo)
	}
	ownerLogin := strings.TrimSpace(item.Owner.Login)
	expectedUserID, userIDErr := strconv.ParseInt(strings.TrimSpace(expected.Labels[labelUserID]), 10, 64)
	if item.ID <= 0 || strings.TrimSpace(item.EnvironmentID) == "" || item.Owner.ID <= 0 || ownerLogin == "" || item.Repository.ID <= 0 {
		return codespace{}, exit(4, "github-codespaces create returned incomplete permanent resource identity; recovery claim retained")
	}
	if userIDErr != nil || expectedUserID <= 0 || item.Owner.ID != expectedUserID {
		return codespace{}, exit(4, "github-codespaces create owner mismatch: got login=%q id=%d want login=%q id=%d; recovery claim retained", ownerLogin, item.Owner.ID, expected.Labels[labelLogin], expectedUserID)
	}
	item.Repository.FullName = liveRepo
	item.Owner.Login = ownerLogin
	return item, nil
}

func (b *backend) bindValidatedCreatedClaim(expected LeaseClaim, item codespace) (LeaseClaim, error) {
	name := strings.TrimSpace(item.Name)
	displayName := strings.TrimSpace(expected.Labels[labelDisplayName])
	repo := firstNonEmpty(strings.TrimSpace(item.Repository.FullName), strings.TrimSpace(expected.Labels[labelRepository]))
	labels := cloneLabels(expected.Labels)
	delete(labels, labelRecovery)
	labels[labelCodespaceName] = name
	labels[labelCodespaceID] = strconv.FormatInt(item.ID, 10)
	labels[labelDisplayName] = displayName
	labels[labelEnvironmentID] = item.EnvironmentID
	labels[labelOwnerID] = strconv.FormatInt(item.Owner.ID, 10)
	labels[labelLogin] = item.Owner.Login
	labels[labelRepository] = repo
	labels[labelRepositoryID] = strconv.FormatInt(item.Repository.ID, 10)
	labels[labelMachine] = firstNonEmpty(item.Machine.Name, labels[labelMachine])
	labels[labelState] = "provisioning"
	server := b.serverFromCodespace(item, labels)
	claim, err := b.bindClaim(expected.LeaseID, expected, server)
	if err != nil {
		return expected, fmt.Errorf("persist github-codespaces resource identity %s: %w", name, err)
	}
	return claim, nil
}

func githubCodespacesCreateMayHaveSucceeded(err error) bool {
	status, ok := githubAPIStatus(err)
	if !ok {
		return true
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func discardPendingClaim(claim LeaseClaim) error {
	if strings.TrimSpace(claim.CloudID) != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
		return exit(4, "refusing to discard non-pending github-codespaces claim %s", claim.LeaseID)
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		return removeStoredSSHConfig(claim.LeaseID)
	})
}

func discardMissingBoundClaim(claim LeaseClaim) error {
	name := strings.TrimSpace(claim.CloudID)
	resourceID, resourceIDErr := strconv.ParseInt(strings.TrimSpace(claim.Labels[labelCodespaceID]), 10, 64)
	ownerID, ownerIDErr := strconv.ParseInt(strings.TrimSpace(claim.Labels[labelOwnerID]), 10, 64)
	if name == "" || claim.Labels[labelCodespaceName] != name || resourceIDErr != nil || resourceID <= 0 || ownerIDErr != nil || ownerID <= 0 || strings.TrimSpace(claim.Labels[labelEnvironmentID]) == "" {
		return exit(4, "refusing to discard github-codespaces claim %s without its exact bound resource identity", claim.LeaseID)
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		return removeStoredSSHConfig(claim.LeaseID)
	})
}

func rollbackCreatedCodespace(api codespacesAPI, claim LeaseClaim) error {
	name := strings.TrimSpace(claim.CloudID)
	if name == "" || claim.Labels[labelCodespaceName] != name {
		return exit(4, "refusing github-codespaces rollback for lease=%s without its exact bound resource identity", claim.LeaseID)
	}
	preflightCtx, preflightCancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
	defer preflightCancel()
	item, err := api.getCodespace(preflightCtx, name)
	if err != nil && !isGitHubNotFound(err) {
		return err
	}
	if err == nil {
		if err := validateCodespaceClaimResource(claim, item); err != nil {
			return err
		}
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
		defer cancel()
		item, err := api.getCodespace(ctx, name)
		if isGitHubNotFound(err) {
			return exit(4, "github-codespaces rollback could not confirm created resource %s for lease=%s; recovery claim retained", name, claim.LeaseID)
		}
		if err != nil {
			return err
		}
		if err := validateCodespaceClaimResource(claim, item); err != nil {
			return err
		}
		if err := api.deleteCodespace(ctx, name); err != nil && !isGitHubNotFound(err) {
			return fmt.Errorf("rollback github-codespaces codespace=%s lease=%s: %w", name, claim.LeaseID, err)
		}
		if err := waitForCodespaceDeleted(ctx, api, name, defaultPollInterval, func(item codespace) error {
			return validateCodespaceClaimResource(claim, item)
		}); err != nil {
			return err
		}
		return removeStoredSSHConfig(claim.LeaseID)
	})
}

func rollbackUnboundCreatedCodespace(api codespacesAPI, pending LeaseClaim, item codespace) error {
	if strings.TrimSpace(pending.CloudID) != "" || pending.Labels[labelRecovery] != recoveryPreCreate {
		return exit(4, "refusing unbound github-codespaces rollback for non-pending claim %s", pending.LeaseID)
	}
	if _, err := validateCreatedCodespaceIdentity(pending, item); err != nil {
		return err
	}
	name := strings.TrimSpace(item.Name)
	preflightCtx, preflightCancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
	defer preflightCancel()
	live, err := api.getCodespace(preflightCtx, name)
	if isGitHubNotFound(err) {
		return exit(4, "github-codespaces rollback could not yet confirm created resource %s for lease=%s; recovery claim retained", name, pending.LeaseID)
	}
	if err != nil {
		return err
	}
	if err := validatePendingRecoveryResource(pending, item, live); err != nil {
		return err
	}
	return removeLeaseClaimIfUnchangedAfter(pending.LeaseID, pending, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
		defer cancel()
		live, err := api.getCodespace(ctx, name)
		if isGitHubNotFound(err) {
			return exit(4, "github-codespaces rollback could not confirm created resource %s for lease=%s; recovery claim retained", name, pending.LeaseID)
		}
		if err != nil {
			return err
		}
		if err := validatePendingRecoveryResource(pending, item, live); err != nil {
			return err
		}
		if err := api.deleteCodespace(ctx, name); err != nil && !isGitHubNotFound(err) {
			return fmt.Errorf("rollback unbound github-codespaces codespace=%s lease=%s: %w", name, pending.LeaseID, err)
		}
		if err := waitForCodespaceDeleted(ctx, api, name, defaultPollInterval, func(live codespace) error {
			return validatePendingRecoveryResource(pending, item, live)
		}); err != nil {
			return err
		}
		return removeStoredSSHConfig(pending.LeaseID)
	})
}

func validatePendingRecoveryResource(pending LeaseClaim, expected, live codespace) error {
	nonce := strings.TrimSpace(pending.Labels[labelRecoveryNonce])
	displayName := strings.TrimSpace(pending.Labels[labelDisplayName])
	repo := strings.TrimSpace(pending.Labels[labelRepository])
	if nonce == "" || displayName != githubCodespacesDisplayName(pending.LeaseID, pending.Slug, nonce) {
		return exit(4, "refusing github-codespaces rollback for lease=%s with inconsistent recovery identity", pending.LeaseID)
	}
	if strings.TrimSpace(live.Name) != strings.TrimSpace(expected.Name) || live.ID != expected.ID || live.EnvironmentID != expected.EnvironmentID || live.Owner.ID != expected.Owner.ID || !strings.EqualFold(strings.TrimSpace(live.Repository.FullName), repo) {
		return exit(4, "refusing github-codespaces rollback for lease=%s after recovery resource identity changed", pending.LeaseID)
	}
	return nil
}
