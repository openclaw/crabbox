package runpod

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedLeaseMarker = "CRABBOX_LEASE_ID"
const fixedAttemptMarker = "CRABBOX_FIXED_ATTEMPT"
const fixedIntentMarker = "CRABBOX_FIXED_INTENT"

var errFixedPodAbsent = errors.New("fixed RunPod pod is absent")

var fixedLeaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: "RunPod",
	TerminalIdentityLabels: []string{"name", "lease", "slug", "provider"},
	AfterTerminal:          func(claim core.LeaseClaim) error { core.RemoveStoredTestboxKey(claim.LeaseID); return nil },
}

type fixedPodCreator interface {
	DeployFixedPod(context.Context, runpodDeployInput) (runpodPod, error)
}

func (*runpodLeaseBackend) SupportsRequestedLeaseID() bool { return true }

func (b *runpodLeaseBackend) fixedScope(ctx context.Context, client runpodAPI) (string, error) {
	account, err := client.Whoami(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(account.ID) == "" || account.ID == "authenticated" {
		return "", core.Exit(2, "RunPod account identity is missing")
	}
	return strings.TrimRight(b.configForRun().Runpod.APIURL, "/") + ":" + account.ID, nil
}

func (b *runpodLeaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	creator, ok := client.(fixedPodCreator)
	if !ok {
		return core.LeaseTarget{}, core.Exit(2, "RunPod client cannot create fixed leases")
	}
	scope, err := b.fixedScope(ctx, client)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.configForRun()
	input := runpodDeployInput{ImageName: cfg.Runpod.Image, InstanceID: cfg.Runpod.InstanceID, CloudType: cfg.Runpod.CloudType, TemplateID: cfg.Runpod.TemplateID, ContainerDiskInGb: cfg.Runpod.DiskGB, Ports: "22/tcp"}
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: func() time.Time { return core.ClockNow(b.rt.Clock) },
	}, core.FixedLeaseOperations[runpodPod]{Admission: &core.FixedAdmission{},
		DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
			if exists && (!fixedLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != scope) {
				return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: RunPod owner or account changed")
			}
			input.PublicKey, err = core.PrepareFixedSSHKey(&cfg, req.RequestedLeaseID, core.FixedKeyPolicy{RequireExisting: exists && claim.FixedCreateIntent.Attempt != nil, UseStored: true})
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			fingerprint, err := core.FixedIntentFingerprint("", struct {
				Input                runpodDeployInput
				Labels               map[string]string
				Slug, User, WorkRoot string
				TTL, Idle            time.Duration
			}{input, core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, providerName, "", req.Keep, time.Unix(0, 0)), core.NormalizeLeaseSlug(req.RequestedSlug), cfg.SSHUser, cfg.WorkRoot, cfg.TTL, cfg.IdleTimeout})
			binding := core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint}
			if err != nil || exists {
				return binding, err
			}
			binding.Inventory, err = b.listServersFromClient(ctx, client, true)
			binding.AllocateSlug, binding.RejectExistingLease, binding.RequestedSlug = true, true, req.RequestedSlug
			return binding, err
		},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[runpodPod], error) {
			if tx.Claim.FixedCreateIntent.Attempt == nil {
				return core.FixedObservation[runpodPod]{CanSubmit: true}, nil
			}
			pod, err := b.loadFixedPod(ctx, client, *tx.Claim)
			return core.FixedObservation[runpodPod]{Candidates: []runpodPod{pod}}, err
		},
		Plan: func(_ context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			return core.FixedAttemptPlan{NonceKey: "nonce", FingerprintLabel: fixedIntentMarker, NonceLabel: fixedAttemptMarker,
				Labels:       map[string]string{fixedLeaseMarker: claim.LeaseID, "name": core.LeaseProviderName(claim.LeaseID, claim.Slug)},
				DirectLabels: &core.FixedDirectLabels{Config: cfg, Provider: providerName, Keep: req.Keep},
			}, nil
		},
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (runpodPod, error) {
			input.Name = tx.Claim.Labels["name"]
			input.Env = map[string]string{fixedLeaseMarker: tx.Claim.LeaseID, fixedAttemptMarker: tx.Claim.FixedCreateIntent.Attempt["nonce"], fixedIntentMarker: tx.Claim.FixedCreateIntent.Fingerprint}
			return creator.DeployFixedPod(ctx, input)
		},
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, pod runpodPod) (core.LeaseTarget, error) {
			if err := validateFixedPod(*tx.Claim, pod); err != nil {
				return core.LeaseTarget{}, err
			}
			if err := tx.Bind(core.FixedResourceBinding{CloudID: pod.ID, ImmutableID: pod.ID}); err != nil {
				return core.LeaseTarget{}, err
			}
			return b.prepareFixedPod(ctx, client, cfg, *tx.Claim)
		},
	})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func validateFixedPod(claim core.LeaseClaim, pod runpodPod) error {
	intent := claim.FixedCreateIntent
	if !fixedLeaseKind.IsFixedClaim(claim) || intent.Version != 1 || intent.State == "released" || intent.ProviderScope != claim.ProviderScope ||
		intent.Attempt["nonce"] == "" || pod.ID == "" || pod.Name != core.LeaseProviderName(claim.LeaseID, claim.Slug) ||
		pod.Env[fixedLeaseMarker] != claim.LeaseID || pod.Env[fixedAttemptMarker] != intent.Attempt["nonce"] || pod.Env[fixedIntentMarker] != intent.Fingerprint ||
		(claim.CloudID != "" && pod.ID != claim.CloudID) || (claim.CloudImmutableID != "" && pod.ID != claim.CloudImmutableID) {
		return core.Exit(4, "lease_id_conflict: RunPod pod does not match fixed create intent")
	}
	return nil
}

func (b *runpodLeaseBackend) loadFixedPod(ctx context.Context, client runpodAPI, claim core.LeaseClaim) (runpodPod, error) {
	scope, err := b.fixedScope(ctx, client)
	if err != nil {
		return runpodPod{}, err
	}
	if claim.ProviderScope != scope {
		return runpodPod{}, core.Exit(4, "lease_id_conflict: RunPod account or endpoint changed")
	}
	return core.LookupFixedResource(ctx, fixedLeaseKind, claim, func(ctx context.Context, claim core.LeaseClaim) (runpodPod, error) {
		var pod runpodPod
		if claim.CloudID != "" {
			pod, err = client.GetPod(ctx, claim.CloudID)
			var response *runpodAPIError
			if errors.As(err, &response) && response.StatusCode == http.StatusNotFound {
				return pod, errFixedPodAbsent
			}
		} else {
			var pods []runpodPod
			pods, err = client.ListPods(ctx)
			if err != nil {
				return pod, err
			}
			var found bool
			pod, found, err = core.SelectFixedCandidate(fixedLeaseKind, claim.LeaseID, pods, func(p runpodPod) bool {
				return p.Name == core.LeaseProviderName(claim.LeaseID, claim.Slug) || p.Env[fixedLeaseMarker] == claim.LeaseID
			})
			if err == nil && !found {
				err = core.FixedUncertainCustody(claim.LeaseID)
			}
		}
		if err == nil {
			err = validateFixedPod(claim, pod)
		}
		return pod, err
	})
}

func (b *runpodLeaseBackend) prepareFixedPod(ctx context.Context, client runpodAPI, cfg core.Config, claim core.LeaseClaim) (core.LeaseTarget, error) {
	pod, err := b.waitForPodSSH(ctx, client, claim.CloudID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateFixedPod(claim, pod); err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, cfg, pod, claim.LeaseID, claim.Slug, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	lease.Server = projectRunpodClaim(lease.Server, claim)
	return lease, nil
}

func (b *runpodLeaseBackend) resolveFixed(ctx context.Context, client runpodAPI, req core.ResolveRequest, claim core.LeaseClaim) (core.LeaseTarget, error) {
	scope, err := b.fixedScope(ctx, client)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if claim.ProviderScope != scope {
		return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: RunPod account or endpoint changed")
	}
	inspect := func(ctx context.Context, claim core.LeaseClaim) (core.LeaseTarget, error) {
		pod, err := b.loadFixedPod(ctx, client, claim)
		if req.ReleaseOnly && errors.Is(err, errFixedPodAbsent) {
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: core.Server{Provider: providerName, CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Name: claim.Labels["name"], Labels: maps.Clone(claim.Labels)}}, nil
		}
		if err != nil {
			return core.LeaseTarget{}, err
		}
		lease, err := b.prepareLease(ctx, b.configForRun(), pod, claim.LeaseID, claim.Slug, false)
		lease.Server = projectRunpodClaim(lease.Server, claim)
		if req.ReadyProbe || req.IncludeDiagnostics {
			err = core.UseStoredTestboxKey(&lease.SSH, claim.LeaseID)
		}
		return lease, err
	}
	return core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{Kind: fixedLeaseKind, Request: req, Expected: claim,
		Provider: providerName, ResourceName: core.LeaseProviderName(claim.LeaseID, claim.Slug), TerminalState: "terminated",
	}, func(ctx context.Context, claim *core.LeaseClaim, persist func() error) (core.LeaseTarget, error) {
		pod, err := b.loadFixedPod(ctx, client, *claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.BindFixedClaim(claim, core.FixedResourceBinding{CloudID: pod.ID, ImmutableID: pod.ID}, persist); err != nil {
			return core.LeaseTarget{}, err
		}
		cfg := b.configForRun()
		if _, err := core.PrepareFixedSSHKey(&cfg, claim.LeaseID, core.FixedKeyPolicy{RequireExisting: true, UseStored: true}); err != nil {
			return core.LeaseTarget{}, err
		}
		return b.prepareFixedPod(ctx, client, cfg, *claim)
	}, inspect)
}

func (b *runpodLeaseBackend) releaseFixed(ctx context.Context, req core.ReleaseLeaseRequest, claim core.LeaseClaim) error {
	if req.Lease.LeaseID != claim.LeaseID {
		return core.Exit(4, "lease_id_conflict: RunPod release lease identity changed")
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	scope, err := b.fixedScope(ctx, client)
	if err != nil {
		return err
	}
	if claim.ProviderScope != scope {
		return core.Exit(4, "lease_id_conflict: RunPod account or endpoint changed")
	}
	return core.DeleteFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[runpodPod]{
		Release: &core.FixedReleasePolicy{CheckpointID: &req.CheckpointID, SkipTerminalObservation: true},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[runpodPod], error) {
			pod, err := b.loadFixedPod(ctx, client, *tx.Claim)
			if errors.Is(err, errFixedPodAbsent) {
				return core.FixedObservation[runpodPod]{AbsenceProven: true}, nil
			}
			if err != nil {
				return core.FixedObservation[runpodPod]{}, err
			}
			if pod.ID != req.Lease.Server.CloudID {
				return core.FixedObservation[runpodPod]{}, core.Exit(4, "lease_id_conflict: RunPod release target changed")
			}
			return core.FixedObservation[runpodPod]{Candidates: []runpodPod{pod}, Binding: &core.FixedResourceBinding{CloudID: pod.ID, ImmutableID: pod.ID}}, nil
		},
		DeleteExact: func(ctx context.Context, _ *core.FixedTransaction, pod runpodPod) error {
			return client.TerminatePod(ctx, pod.ID)
		},
	})
}

func (*runpodLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels[fixedAttemptMarker] != "", nil, nil)
}
