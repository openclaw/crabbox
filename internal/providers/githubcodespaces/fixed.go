package githubcodespaces

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var fixedLeaseKind = core.FixedLeaseKind{
	ClaimProvider: providerName, IntentVersion: 1, Label: "GitHub Codespaces",
	TerminalIdentityLabels: []string{"lease", "slug", "provider", labelRepository, labelUserID, labelLogin, labelRelease},
	AfterTerminal:          func(claim core.LeaseClaim) error { return removeStoredSSHConfig(claim.LeaseID) },
}

func (*backend) SupportsRequestedLeaseID() bool { return true }

// Display names allow 48 characters. Hash the complete tuple, rather than
// truncating the lease ID, fingerprint, or unpredictable attempt identity.
func fixedCodespacesDisplayName(leaseID, fingerprint, nonce string) string {
	digest := sha256.Sum256([]byte("github-codespaces-fixed-v1\x00" + leaseID + "\x00" + fingerprint + "\x00" + nonce))
	return "cbx_" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func (b *backend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	gh, api, user, err := b.controlPlane(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	repo, err := b.resolveRepo(req.Repo)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	repo = strings.ToLower(repo)
	cfg := b.claimConfig(repo)
	repoRoot, err := repoRootForClaim(req.Repo)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	input := createCodespaceRequest{Repo: repo, Ref: strings.TrimSpace(cfg.GitHubCodespaces.Ref), Machine: b.effectiveMachine(),
		DevcontainerPath: strings.TrimSpace(cfg.GitHubCodespaces.DevcontainerPath), WorkingDirectory: strings.TrimSpace(cfg.GitHubCodespaces.WorkingDirectory),
		Geo: strings.TrimSpace(cfg.GitHubCodespaces.Geo), IdleTimeout: b.githubIdleTimeout(), RetentionPeriod: cfg.GitHubCodespaces.RetentionPeriod, RetentionSet: core.GitHubCodespacesRetentionExplicit(cfg)}
	release := releaseDelete
	if !githubCodespacesDeleteOnRelease(core.LeaseTarget{}, cfg) {
		release = releaseStop
	}
	fingerprint, err := core.FixedIntentFingerprint("github-codespaces-v1", struct {
		Input                                     createCodespaceRequest
		UserID                                    int64
		WorkRoot, SSHUser, SSHPort, Slug, Release string
		Labels                                    map[string]string
		Keep                                      bool
		TTL, Idle                                 time.Duration
	}{input, user.ID, cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort, core.NormalizeLeaseSlug(req.RequestedSlug), release,
		core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, providerName, "", req.Keep, time.Unix(0, 0)), req.Keep, cfg.TTL, cfg.IdleTimeout})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	unlock, err := lockGitHubCodespacesLeaseOperation(ctx, req.RequestedLeaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	defer unlock()
	// Share the ordinary acquisition slug fence through durable claim publication.
	unlockSlug, err := lockGitHubCodespacesSlugAllocation(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	defer func() {
		if unlockSlug != nil {
			unlockSlug()
		}
	}()
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: repoRoot, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: b.now,
	}, core.FixedLeaseOperations[codespace]{
		Admission: &core.FixedAdmission{PendingKey: "submission", PendingValue: "pending", SubmittedValue: "submitted"},
		DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
			binding := core.FixedLeaseBinding{ProviderScope: providerClaimScope(cfg), Fingerprint: fingerprint}
			if exists {
				if claim.FixedCreateIntent.Fingerprint != fingerprint || claim.ProviderScope != binding.ProviderScope {
					return binding, core.Exit(4, "lease_id_conflict: github-codespaces account, repository, ref, machine, devcontainer or lease settings differ from the original intent")
				}
				return binding, nil
			}
			if _, err := api.listMachines(ctx, repo, input.Ref); err != nil {
				return binding, err
			}
			live, err := api.listCodespaces(ctx)
			if err != nil {
				return binding, err
			}
			binding.Inventory, err = b.serversFromCodespaces(live)
			binding.AllocateSlug, binding.RejectExistingLease, binding.RequestedSlug = true, true, req.RequestedSlug
			return binding, err
		},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[codespace], error) {
			// The engine has now published even the pristine claim, reserving its slug.
			if unlockSlug != nil {
				unlockSlug()
				unlockSlug = nil
			}
			return b.observeFixed(ctx, api, user, tx.Claim, mode)
		},
		Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			nonce, err := b.newRecoveryNonce()
			if err != nil || nonce == "" {
				return core.FixedAttemptPlan{}, errors.Join(errors.New("generate github-codespaces fixed recovery nonce"), err)
			}
			marker := fixedCodespacesDisplayName(claim.LeaseID, fingerprint, nonce)
			live, err := api.listCodespaces(ctx)
			if err != nil {
				return core.FixedAttemptPlan{}, err
			}
			if err := rejectExistingRecoveryIdentity(live, marker, repo); err != nil {
				return core.FixedAttemptPlan{}, err
			}
			labels := b.labelsFor(claim.LeaseID, claim.Slug, repo, user.Login, req.Keep, release, codespace{}, "provisioning", user)
			labels[labelDisplayName] = marker
			return core.FixedAttemptPlan{Values: map[string]string{"nonce": nonce, "submission": "pending"}, Labels: labels}, nil
		},
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (codespace, error) {
			input.DisplayName = tx.Claim.Labels[labelDisplayName]
			item, err := api.createCodespace(ctx, input)
			if err != nil {
				if !githubCodespacesCreateMayHaveSucceeded(err) {
					return codespace{}, &core.FixedCreateRejected{Err: err}
				}
				return codespace{}, fmt.Errorf("github-codespaces create outcome uncertain; replay or stop lease %s; claim retained: %w", tx.Claim.LeaseID, err)
			}
			return item, nil
		},
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item codespace) (core.LeaseTarget, error) {
			if err := validateFixedCodespace(*tx.Claim, item); err != nil {
				return core.LeaseTarget{}, err
			}
			if err := tx.Bind(fixedCodespaceBinding(*tx.Claim, item)); err != nil {
				return core.LeaseTarget{}, err
			}
			return b.prepareFixedAccess(ctx, gh, api, *tx.Claim, item)
		},
	})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func validateFixedCodespace(claim core.LeaseClaim, item codespace) error {
	intent := claim.FixedCreateIntent
	if !fixedLeaseKind.IsFixedClaim(claim) || intent.Version != 1 || intent.State == "released" || intent.ProviderScope != claim.ProviderScope || intent.Attempt["nonce"] == "" || intent.Fingerprint == "" {
		return core.Exit(4, "lease_id_conflict: invalid github-codespaces fixed intent; claim retained")
	}
	marker := fixedCodespacesDisplayName(claim.LeaseID, intent.Fingerprint, intent.Attempt["nonce"])
	if claim.Labels[labelDisplayName] != marker || item.DisplayName != marker ||
		item.Name == "" || item.ID <= 0 || item.EnvironmentID == "" || item.Repository.ID <= 0 || item.Owner.ID <= 0 ||
		strconv.FormatInt(item.Owner.ID, 10) != claim.Labels[labelUserID] ||
		!strings.EqualFold(item.Repository.FullName, claim.Labels[labelRepository]) {
		return core.Exit(4, "lease_id_conflict: github-codespaces ownership marker, account or repository mismatch; claim retained")
	}
	if claim.CloudID != "" {
		if claim.CloudImmutableID != strconv.FormatInt(item.ID, 10) {
			return core.Exit(4, "lease_id_conflict: github-codespaces immutable ID changed")
		}
		return validateCodespaceClaimResource(claim, item)
	}
	if item.Machine.Name != claim.Labels[labelMachine] {
		return core.Exit(4, "lease_id_conflict: github-codespaces recovery machine differs from create intent")
	}
	return nil
}

func fixedCodespaceBinding(claim core.LeaseClaim, item codespace) core.FixedResourceBinding {
	labels := maps.Clone(claim.Labels)
	labels[labelCodespaceName], labels[labelCodespaceID] = item.Name, strconv.FormatInt(item.ID, 10)
	labels[labelEnvironmentID], labels[labelOwnerID] = item.EnvironmentID, strconv.FormatInt(item.Owner.ID, 10)
	labels[labelRepositoryID] = strconv.FormatInt(item.Repository.ID, 10)
	return core.FixedResourceBinding{CloudID: item.Name, ImmutableID: strconv.FormatInt(item.ID, 10), Labels: labels}
}

func (b *backend) observeFixed(ctx context.Context, api codespacesAPI, user githubUser, claim *core.LeaseClaim, mode core.FixedObserveMode) (core.FixedObservation[codespace], error) {
	var out core.FixedObservation[codespace]
	intent := claim.FixedCreateIntent
	if !fixedLeaseKind.IsFixedClaim(*claim) || intent.Version != 1 {
		return out, core.Exit(4, "lease_id_conflict: invalid github-codespaces fixed claim")
	}
	// A pristine journal has no native side effect or labels yet.
	if len(intent.Attempt) == 0 && claim.CloudID == "" && intent.State == "prepared" {
		out.CanSubmit, out.AbsenceProven = true, true
		return out, nil
	}
	if err := b.validateClaimScope(*claim, user); err != nil {
		return out, err
	}
	if claim.CloudID != "" {
		item, err := api.getCodespace(ctx, claim.CloudID)
		if isGitHubNotFound(err) {
			out.AbsenceProven = mode == core.FixedObserveDelete
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if err := validateFixedCodespace(*claim, item); err != nil {
			return out, err
		}
		out.Candidates = []codespace{item}
	} else {
		items, err := api.listCodespaces(ctx)
		if err != nil {
			return out, err
		}
		marker := fixedCodespacesDisplayName(claim.LeaseID, intent.Fingerprint, intent.Attempt["nonce"])
		for _, item := range items {
			if item.DisplayName != marker {
				continue
			}
			if err := validateFixedCodespace(*claim, item); err != nil {
				return out, err
			}
			out.Candidates = append(out.Candidates, item)
		}
		out.CanSubmit = intent.Attempt["submission"] == "pending"
		out.AbsenceProven = out.CanSubmit
	}
	if len(out.Candidates) == 1 {
		binding := fixedCodespaceBinding(*claim, out.Candidates[0])
		out.Binding = &binding
	}
	return out, nil
}

func (b *backend) prepareFixedAccess(ctx context.Context, gh githubCLI, api codespacesAPI, claim core.LeaseClaim, item codespace) (core.LeaseTarget, error) {
	if err := validateFixedCodespace(claim, item); err != nil {
		return core.LeaseTarget{}, err
	}
	if codespaceStopping(item.State) {
		stopCtx, cancel := context.WithTimeout(ctx, b.readyTimeout)
		var err error
		item, err = b.waitForStopped(stopCtx, api, item.Name)
		cancel()
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateFixedCodespace(claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if codespaceStopped(item.State) {
		started, err := api.startCodespace(ctx, item.Name)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateFixedCodespace(claim, started); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	item, err := b.waitForAvailable(ctx, api, claim.CloudID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateFixedCodespace(claim, item); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateStopPreservesCodespace(item); err != nil {
		return core.LeaseTarget{}, err
	}
	target, sshConfig, err := b.sshTargetWithConfig(ctx, gh, item.Name, claim.Labels[labelRepository])
	if err != nil {
		return core.LeaseTarget{}, b.sshPrerequisiteError(err)
	}
	// Resolve also uses the persisted work root, independent of current flags.
	accessCfg := b.repoConfig(claim.Labels[labelRepository])
	accessCfg.WorkRoot = claim.Labels["work_root"]
	accessCfg.GitHubCodespaces.WorkRoot = claim.Labels["work_root"]
	target.ReadyCheck = githubCodespacesReadyCheck(accessCfg)
	if err := b.waitSSH(ctx, &target, "github-codespaces ssh", b.readyTimeout); err != nil {
		return core.LeaseTarget{}, b.sshPrerequisiteError(err)
	}
	if _, err := storeSSHConfig(claim.LeaseID, sshConfig); err != nil {
		return core.LeaseTarget{}, err
	}
	labels := core.TouchDirectLeaseLabels(claim.Labels, b.claimConfig(claim.Labels[labelRepository]), "ready", b.now().UTC())
	server := b.serverFromCodespace(item, labels)
	server.ImmutableID = claim.CloudImmutableID
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *backend) resolveFixed(ctx context.Context, gh githubCLI, api codespacesAPI, user githubUser, req core.ResolveRequest, claim core.LeaseClaim) (core.LeaseTarget, error) {
	if err := b.validateClaimScope(claim, user); err != nil {
		return core.LeaseTarget{}, err
	}
	inspect := func(ctx context.Context, claim core.LeaseClaim) (core.LeaseTarget, error) {
		observed, err := core.InspectFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[codespace]{
			ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[codespace], error) {
				mode := core.FixedObserveInspect
				if req.ReleaseOnly {
					mode = core.FixedObserveDelete
				}
				return b.observeFixed(ctx, api, user, tx.Claim, mode)
			},
		})
		if err != nil {
			return core.LeaseTarget{}, err
		}
		server := serverFromClaim(claim)
		server.ImmutableID = claim.CloudImmutableID
		if len(observed.Candidates) == 1 {
			server = b.serverFromCodespace(observed.Candidates[0], claim.Labels)
			server.ImmutableID = strconv.FormatInt(observed.Candidates[0].ID, 10)
			if codespaceStopped(server.Status) {
				server.Status = "stopped"
			}
		} else if !req.ReleaseOnly || !observed.AbsenceProven {
			return core.LeaseTarget{}, core.FixedUncertainCustody(claim.LeaseID)
		}
		lease := core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}
		if !req.ReleaseOnly && req.ReadyProbe && codespaceAvailable(server.Status) {
			target, _, err := b.sshTargetWithConfig(ctx, gh, server.CloudID, claim.Labels[labelRepository])
			if err != nil {
				return core.LeaseTarget{}, b.sshPrerequisiteError(err)
			}
			cfg := b.claimConfig(claim.Labels[labelRepository])
			cfg.GitHubCodespaces.WorkRoot = claim.Labels["work_root"]
			target.ReadyCheck = githubCodespacesReadyCheck(cfg)
			if err := b.waitSSH(ctx, &target, "github-codespaces ssh", b.readyTimeout); err != nil {
				return core.LeaseTarget{}, b.sshPrerequisiteError(err)
			}
			lease.SSH = target
		}
		return lease, nil
	}
	return core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{Kind: fixedLeaseKind, Request: req, Expected: claim,
		Provider: providerName, ResourceName: claim.CloudID, TerminalState: "deleted", Now: b.now,
	}, func(ctx context.Context, current *core.LeaseClaim, persist func() error) (core.LeaseTarget, error) {
		observed, err := core.InspectFixedResource(ctx, fixedLeaseKind, *current, core.FixedLeaseOperations[codespace]{ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[codespace], error) {
			return b.observeFixed(ctx, api, user, tx.Claim, mode)
		}})
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if len(observed.Candidates) != 1 {
			return core.LeaseTarget{}, core.FixedUncertainCustody(current.LeaseID)
		}
		if err := core.BindFixedClaim(current, *observed.Binding, persist); err != nil {
			return core.LeaseTarget{}, err
		}
		return b.prepareFixedAccess(ctx, gh, api, *current, observed.Candidates[0])
	}, inspect)
}

func (b *backend) releaseFixed(ctx context.Context, api codespacesAPI, user githubUser, req core.ReleaseLeaseRequest, claim core.LeaseClaim, outcome *core.ReleaseLeaseOutcome) error {
	if err := b.validateClaimScope(claim, user); err != nil {
		return err
	}
	if err := core.AuthorizeCheckpointRelease(claim, req.CheckpointID); err != nil {
		return err
	}
	if requested := req.Lease.Server.CloudID; requested != "" && claim.CloudID != "" && requested != claim.CloudID {
		return core.Exit(4, "lease_id_conflict: github-codespaces release target changed")
	}
	if claim.FixedCreateIntent.State == "released" {
		return core.DeleteFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[codespace]{
			Release: &core.FixedReleasePolicy{SkipTerminalObservation: true, Outcome: outcome},
			ObserveExact: func(context.Context, *core.FixedTransaction, core.FixedObserveMode) (core.FixedObservation[codespace], error) {
				return core.FixedObservation[codespace]{}, nil
			},
			DeleteExact: func(context.Context, *core.FixedTransaction, codespace) error { return nil },
		})
	}
	observed, err := core.InspectFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[codespace]{ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[codespace], error) {
		return b.observeFixed(ctx, api, user, tx.Claim, core.FixedObserveDelete)
	}})
	if err != nil {
		return err
	}
	if len(observed.Candidates) == 1 {
		item := observed.Candidates[0]
		if requested := req.Lease.Server.CloudID; requested != "" && requested != item.Name {
			return core.Exit(4, "lease_id_conflict: github-codespaces release target changed")
		}
		if claim.CloudID == "" {
			claim, err = core.CompareAndBindFixedClaim(claim, func(current *core.LeaseClaim, persist func() error) error {
				return core.BindFixedClaim(current, *observed.Binding, persist)
			})
			if err != nil {
				return err
			}
		}
		deleting := claim.FixedCreateIntent.Journal != nil && claim.FixedCreateIntent.Journal.Phase == "deleting"
		if !deleting && (claim.Labels[labelRelease] == releaseStop || validateDeleteSafe(item) != nil) {
			return b.stopCodespaceAndRetainWithOutcome(ctx, api, claim.LeaseID, claim, serverFromClaim(claim), item.Name, outcome)
		}
	}
	err = core.DeleteFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[codespace]{
		Release: &core.FixedReleasePolicy{CheckpointID: &req.CheckpointID, Outcome: outcome},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[codespace], error) {
			return b.observeFixed(ctx, api, user, tx.Claim, mode)
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, item codespace) error {
			deleteCtx, cancel := context.WithTimeout(ctx, githubCodespacesDeleteTimeout)
			defer cancel()
			if err := validateDeleteSafe(item); err != nil {
				return err
			}
			if err := validateStopPreservesCodespace(item); err != nil {
				return err
			}
			if err := api.stopCodespace(deleteCtx, item.Name); err != nil {
				if isGitHubNotFound(err) {
					return waitForCodespaceDeleted(deleteCtx, api, item.Name, b.pollInterval, func(live codespace) error { return validateFixedCodespace(*tx.Claim, live) })
				}
				return err
			}
			stopped, err := b.waitForStopped(deleteCtx, api, item.Name)
			if isGitHubNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := validateFixedCodespace(*tx.Claim, stopped); err != nil {
				return err
			}
			if err := validateDeleteSafe(stopped); err != nil {
				return err
			}
			if err := api.deleteCodespace(deleteCtx, item.Name); err != nil && !isGitHubNotFound(err) {
				return err
			}
			return waitForCodespaceDeleted(deleteCtx, api, item.Name, b.pollInterval, func(live codespace) error { return validateFixedCodespace(*tx.Claim, live) })
		},
	}, b.now)
	// A dirty-after-stop result preserves custody and blocks acquisition until
	// the already-admitted cleanup can safely finish.
	return err
}

func (b *backend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	if fixedLeaseKind.IsFixedClaim(previous) {
		current, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
		if err != nil {
			return false, err
		}
		if ok && current.FixedCreateIntent != nil && current.FixedCreateIntent.State != "released" && current.Labels[labelRelease] == releaseStop && current.Labels[labelState] == "stopped" {
			return true, nil
		}
		return fixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, true, nil, nil)
	}
	return b.RetainLeaseClaimAfterRelease(lease), nil
}

func (b *backend) cleanupFixed(ctx context.Context, api codespacesAPI, user githubUser, req core.CleanupRequest, snapshot core.LeaseClaim) error {
	unlock, err := lockGitHubCodespacesLeaseOperation(ctx, snapshot.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	claim, exists, err := core.ReadLeaseClaimWithPresence(snapshot.LeaseID)
	if err != nil || !exists {
		return err
	}
	if !fixedLeaseKind.IsFixedClaim(claim) {
		return core.Exit(4, "lease_id_conflict: github-codespaces claim changed during cleanup")
	}
	if claim.FixedCreateIntent.State == "released" {
		return nil
	}
	if err := b.validateClaimScope(claim, user); err != nil {
		return err
	}
	shouldCleanup, reason := core.ShouldCleanupServer(serverFromClaim(claim), b.now().UTC())
	if !shouldCleanup {
		return nil
	}
	fmt.Fprintf(b.stderr(), "cleanup fixed github-codespaces lease=%s reason=%s dry_run=%t\n", claim.LeaseID, reason, req.DryRun)
	if req.DryRun {
		_, err := core.InspectFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[codespace]{ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[codespace], error) {
			return b.observeFixed(ctx, api, user, tx.Claim, mode)
		}})
		return err
	}
	return b.releaseFixed(ctx, api, user, core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: claim.LeaseID}}, claim, &core.ReleaseLeaseOutcome{})
}
