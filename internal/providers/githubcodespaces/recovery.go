package githubcodespaces

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (b *backend) claimConfig(repo string) Config {
	cfg := b.repoConfig(repo)
	cfg.GitHubCodespaces.Repo = strings.TrimSpace(repo)
	if strings.TrimSpace(cfg.GitHubCodespaces.APIURL) == "" {
		cfg.GitHubCodespaces.APIURL = defaultAPIURL
	}
	return cfg
}

func (b *backend) validateClaimScope(claim LeaseClaim, login string) error {
	if claim.Provider != providerName {
		return exit(4, "%q is claimed by provider %s", claim.LeaseID, claim.Provider)
	}
	repo := strings.TrimSpace(claim.Labels[labelRepository])
	expectedScope := providerClaimScope(b.claimConfig(repo))
	if expectedScope == "" || claim.ProviderScope != expectedScope {
		return exit(4, "github-codespaces claim %s scope mismatch: claim=%q current=%q", claim.LeaseID, claim.ProviderScope, expectedScope)
	}
	if expectedLogin := strings.TrimSpace(claim.Labels[labelLogin]); expectedLogin == "" || login == "" || !strings.EqualFold(expectedLogin, login) {
		return exit(3, "github-codespaces login mismatch: current login %s does not match lease login %s", login, expectedLogin)
	}
	if repo == "" {
		return exit(4, "github-codespaces claim %s has no repository identity", claim.LeaseID)
	}
	return nil
}

func (b *backend) claimPendingCreate(leaseID, slug, repo, login, displayName, repoRoot, release string, keep, reclaim bool) (LeaseClaim, error) {
	labels := b.labelsFor(leaseID, slug, repo, login, keep, release, codespace{}, "provisioning")
	delete(labels, labelCodespaceName)
	delete(labels, labelEnvironmentID)
	labels[labelDisplayName] = displayName
	labels[labelRecovery] = recoveryPreCreate
	server := Server{
		Provider: providerName,
		Name:     displayName,
		Status:   "provisioning",
		Labels:   labels,
	}
	return claimLeaseTargetForRepoConfigIfUnchanged(
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

func (b *backend) recoverPendingClaim(api codespacesAPI, claim LeaseClaim, login string) (LeaseClaim, codespace, error) {
	if err := b.validateClaimScope(claim, login); err != nil {
		return claim, codespace{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
	defer cancel()
	items, err := api.listCodespaces(ctx)
	if err != nil {
		return claim, codespace{}, errors.Join(
			exit(4, "github-codespaces create recovery is still pending for lease=%s; claim retained", claim.LeaseID),
			err,
		)
	}
	return b.recoverPendingClaimFromInventory(claim, login, items)
}

func (b *backend) recoverPendingClaimFromInventory(claim LeaseClaim, login string, items []codespace) (LeaseClaim, codespace, error) {
	if err := b.validateClaimScope(claim, login); err != nil {
		return claim, codespace{}, err
	}
	if claim.Labels[labelRecovery] != recoveryPreCreate || strings.TrimSpace(claim.CloudID) != "" {
		return claim, codespace{}, exit(4, "github-codespaces claim %s has no valid pending-create recovery state", claim.LeaseID)
	}
	displayName := strings.TrimSpace(claim.Labels[labelDisplayName])
	repo := strings.TrimSpace(claim.Labels[labelRepository])
	if displayName == "" || repo == "" {
		return claim, codespace{}, exit(4, "github-codespaces claim %s has incomplete recovery identity", claim.LeaseID)
	}
	matches := make([]codespace, 0, 2)
	for _, item := range items {
		if item.DisplayName == displayName && strings.EqualFold(item.Repository.FullName, repo) {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return claim, codespace{}, exit(4, "github-codespaces create recovery is still pending for lease=%s; claim retained", claim.LeaseID)
	case 1:
		if strings.TrimSpace(matches[0].Name) == "" {
			return claim, codespace{}, exit(4, "matched github-codespaces recovery result has no resource identity; claim retained")
		}
		return b.bindCreatedClaim(claim, matches[0])
	default:
		return claim, codespace{}, exit(4, "multiple github-codespaces resources match recovery display name %q repo %q; claim retained", displayName, repo)
	}
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
	repo := strings.TrimSpace(expected.Labels[labelRepository])
	if item.DisplayName != "" && item.DisplayName != displayName {
		return codespace{}, exit(4, "github-codespaces create display name mismatch: got %q want %q; recovery claim retained", item.DisplayName, displayName)
	}
	liveRepo := strings.TrimSpace(item.Repository.FullName)
	if liveRepo != "" && !strings.EqualFold(liveRepo, repo) {
		return codespace{}, exit(4, "github-codespaces create repository mismatch: got %q want %q; recovery claim retained", item.Repository.FullName, repo)
	}
	item.DisplayName = displayName
	item.Repository.FullName = firstNonEmpty(liveRepo, repo)
	return item, nil
}

func (b *backend) bindValidatedCreatedClaim(expected LeaseClaim, item codespace) (LeaseClaim, error) {
	name := strings.TrimSpace(item.Name)
	displayName := strings.TrimSpace(expected.Labels[labelDisplayName])
	repo := firstNonEmpty(strings.TrimSpace(item.Repository.FullName), strings.TrimSpace(expected.Labels[labelRepository]))
	labels := cloneLabels(expected.Labels)
	delete(labels, labelRecovery)
	labels[labelCodespaceName] = name
	labels[labelDisplayName] = displayName
	labels[labelEnvironmentID] = item.EnvironmentID
	labels[labelRepository] = repo
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

func rollbackCreatedCodespace(api codespacesAPI, claim LeaseClaim) error {
	name := strings.TrimSpace(claim.CloudID)
	if name == "" || claim.Labels[labelCodespaceName] != name {
		return exit(4, "refusing github-codespaces rollback for lease=%s without its exact bound resource identity", claim.LeaseID)
	}
	return removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
		defer cancel()
		if err := api.deleteCodespace(ctx, name); err != nil && !isGitHubNotFound(err) {
			return fmt.Errorf("rollback github-codespaces codespace=%s lease=%s: %w", name, claim.LeaseID, err)
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
	return removeLeaseClaimIfUnchangedAfter(pending.LeaseID, pending, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), githubCodespacesRollbackTimeout)
		defer cancel()
		if err := api.deleteCodespace(ctx, name); err != nil && !isGitHubNotFound(err) {
			return fmt.Errorf("rollback unbound github-codespaces codespace=%s lease=%s: %w", name, pending.LeaseID, err)
		}
		return removeStoredSSHConfig(pending.LeaseID)
	})
}
