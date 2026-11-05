package kubevirt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var kubeVirtFixedKind = core.FixedLeaseKind{
	ClaimProvider: core.FixedKubeVirtClaimProvider, IntentVersion: 1, Label: "KubeVirt", DeletionState: "deleting",
	ResourcePlural:         "KubeVirt VMs",
	TerminalIdentityLabels: []string{"name", "namespace_uid", "vm_uid", "lease", "slug", "provider", "fixed_intent_sha256", "create_nonce"},
	AfterTerminal:          func(c core.LeaseClaim) error { return core.RemoveStoredTestboxConnectionArtifacts(c.LeaseID) },
}

func (*leaseBackend) SupportsRequestedLeaseID() bool { return true }

func (b *leaseBackend) namespaceUID(ctx context.Context) (string, error) {
	out, err := b.runKubectl(ctx, nil, "get", "namespace", b.cfg.KubeVirt.Namespace, "-o", "json")
	if err != nil {
		return "", err
	}
	var ns struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &ns); err != nil {
		return "", err
	}
	if ns.Metadata.UID == "" {
		return "", core.Exit(4, "KubeVirt namespace has no immutable UID")
	}
	return ns.Metadata.UID, nil
}

func (b *leaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if !core.IsCanonicalLeaseID(req.RequestedLeaseID) || req.RequestedCheckpointID != "" {
		return core.LeaseTarget{}, core.Exit(2, "KubeVirt fixed acquisition requires a canonical lease ID without a checkpoint")
	}
	if err := validateAcquireConfig(b.cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := req.RequestedLeaseID
	var keyPath, publicKey, namespaceUID, fingerprint string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: kubeVirtFixedKind, LeaseID: leaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: b.cfg.TTL, IdleTimeout: b.cfg.IdleTimeout,
	}, core.FixedLeaseOperations[kubeVirtItem]{
		Admission: &core.FixedAdmission{FreshOnly: true},
		DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
			var err error
			namespaceUID, err = b.namespaceUID(ctx)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			if exists && strings.TrimSpace(b.cfg.KubeVirt.SSHKey) == "" {
				if _, err := b.resolveSSHKey(leaseID); err != nil {
					return core.FixedLeaseBinding{}, err
				}
			}
			keyPath, publicKey, err = b.sshKey(leaseID)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			template, err := os.ReadFile(b.cfg.KubeVirt.Template)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			fingerprint, err = core.FixedIntentFingerprint("kubevirt-v1", core.FixedIntentFields{
				{Name: "scope", Value: b.claimScope()}, {Name: "namespaceUID", Value: namespaceUID},
				{Name: "templateSHA256", Value: fmt.Sprintf("%x", sha256.Sum256(template))},
				{Name: "publicKey", Value: publicKey}, {Name: "sshUser", Value: b.cfg.KubeVirt.SSHUser},
				{Name: "sshPort", Value: b.cfg.KubeVirt.SSHPort}, {Name: "workRoot", Value: b.cfg.WorkRoot},
				{Name: "slug", Value: core.NormalizeLeaseSlug(req.RequestedSlug)}, {Name: "keep", Value: req.Keep},
				{Name: "deleteOnRelease", Value: b.cfg.KubeVirt.DeleteOnRelease}, {Name: "ttl", Value: b.cfg.TTL}, {Name: "idle", Value: b.cfg.IdleTimeout},
			})
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			binding := core.FixedLeaseBinding{ProviderScope: b.claimScope(), Fingerprint: fingerprint}
			if !exists {
				items, err := b.List(ctx, core.ListRequest{})
				if err != nil {
					return binding, err
				}
				binding.AllocateSlug, binding.RequestedSlug, binding.Inventory = true, req.RequestedSlug, items
			}
			return binding, nil
		},
		Plan: func(_ context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			name := core.LeaseProviderName(leaseID, claim.Slug)
			labels := core.DirectLeaseLabels(b.cfg, leaseID, claim.Slug, providerName, "", req.Keep, time.Now().UTC())
			labels["name"], labels["namespace_uid"], labels["fixed_intent_sha256"] = name, namespaceUID, fingerprint
			labels["release"] = kubeVirtReleaseAction(b.cfg)
			return core.FixedAttemptPlan{Values: map[string]string{"name": name}, Labels: labels,
				NonceKey: "create_nonce", NonceBytes: 16, NonceLabel: "create_nonce"}, nil
		},
		ObserveExact: b.observeFixed,
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (kubeVirtItem, error) {
			c := tx.Claim
			item, err := b.createVMOwned(ctx, c.Labels["name"], leaseID, c.Slug, publicKey, c.Labels)
			if err != nil {
				return item, err
			}
			// The API creation acknowledgement, not a name/label inventory match,
			// grants custody of this incarnation. Persist it before starting a VM.
			if item.Metadata.UID == "" || item.Metadata.Name != c.Labels["name"] {
				return item, core.FixedUncertainCustody(leaseID)
			}
			if item.Metadata.Labels[leaseIDLabel] != leaseID || item.Metadata.Labels[slugLabel] != c.Slug || item.Metadata.Annotations[annotationBase+"create_nonce"] != c.Labels["create_nonce"] || item.Metadata.Annotations[annotationBase+"fixed_intent_sha256"] != fingerprint {
				return item, core.FixedUncertainCustody(leaseID)
			}
			if err := tx.Bind(b.fixedBinding(item)); err != nil {
				return item, err
			}
			return item, nil
		},
		Identity: b.fixedBinding,
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item kubeVirtItem) (core.LeaseTarget, error) {
			if err := b.validateFixedItem(ctx, *tx.Claim, item); err != nil {
				return core.LeaseTarget{}, err
			}
			if err := b.startVM(ctx, item.Metadata.Name); err != nil {
				return core.LeaseTarget{}, err
			}
			return b.prepareLease(ctx, item.Metadata.Name, leaseID, tx.Claim.Slug, keyPath, req.Keep, tx.Claim.Labels)
		},
	})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func (b *leaseBackend) fixedBinding(item kubeVirtItem) core.FixedResourceBinding {
	labels := leaseLabelsFromItem(item)
	labels["name"], labels["vm_uid"] = item.Metadata.Name, item.Metadata.UID
	return core.FixedResourceBinding{CloudID: b.cfg.KubeVirt.Namespace + "/" + item.Metadata.Name, ImmutableID: item.Metadata.UID, Labels: labels}
}

func (b *leaseBackend) validateFixedItem(ctx context.Context, claim core.LeaseClaim, item kubeVirtItem) error {
	uid, err := b.namespaceUID(ctx)
	if err != nil {
		return err
	}
	if uid != claim.Labels["namespace_uid"] || claim.ProviderScope != b.claimScope() {
		return core.Exit(4, "lease_id_conflict: KubeVirt namespace or connection identity changed")
	}
	if claim.CloudImmutableID == "" {
		return core.FixedUncertainCustody(claim.LeaseID)
	}
	if item.Metadata.Labels[managedByLabel] != "crabbox" || item.Metadata.UID != claim.CloudImmutableID || item.Metadata.Name != claim.Labels["name"] ||
		item.Metadata.Labels[leaseIDLabel] != claim.LeaseID || item.Metadata.Labels[slugLabel] != claim.Slug ||
		item.Metadata.Annotations[annotationBase+"create_nonce"] != claim.Labels["create_nonce"] ||
		item.Metadata.Annotations[annotationBase+"fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint {
		return core.Exit(4, "lease_id_conflict: KubeVirt VM does not match its bound UID and creation intent")
	}
	return nil
}

func (b *leaseBackend) observeFixed(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[kubeVirtItem], error) {
	c := tx.Claim
	var r core.FixedObservation[kubeVirtItem]
	if c.FixedCreateIntent.State == "released" {
		r.AbsenceProven = true
		return r, nil
	}
	uid, err := b.namespaceUID(ctx)
	if err != nil {
		return r, err
	}
	if c.Labels["namespace_uid"] != "" && uid != c.Labels["namespace_uid"] {
		return r, core.Exit(4, "lease_id_conflict: KubeVirt namespace identity changed")
	}
	items, err := b.listVMs(ctx)
	if err != nil {
		return r, err
	}
	name := c.Labels["name"]
	for _, item := range items {
		if item.Metadata.Name != name && item.Metadata.Labels[leaseIDLabel] != c.LeaseID {
			continue
		}
		if err := b.validateFixedItem(ctx, *c, item); err != nil {
			return r, err
		}
		r.Candidates = append(r.Candidates, item)
	}
	if len(r.Candidates) == 0 {
		// Unfiltered exact-name lookup prevents a removed managed-by label from
		// becoming false absence. A successful list includes every VM name.
		out, err := b.runKubectl(ctx, nil, "get", "virtualmachines.kubevirt.io", "-o", "json")
		if err != nil {
			return r, err
		}
		var all kubeVirtList
		if err := json.Unmarshal([]byte(out), &all); err != nil {
			return r, err
		}
		for _, item := range all.Items {
			if item.Metadata.Name == name || item.Metadata.UID == c.CloudImmutableID && c.CloudImmutableID != "" {
				return r, core.Exit(4, "lease_id_conflict: KubeVirt VM ownership labels changed")
			}
		}
		r.CanSubmit = len(c.FixedCreateIntent.Attempt) == 0
		r.AbsenceProven = c.CloudImmutableID != "" && mode == core.FixedObserveDelete
	}
	return r, nil
}

func (b *leaseBackend) resolveFixed(ctx context.Context, req core.ResolveRequest, claim core.LeaseClaim) (core.LeaseTarget, error) {
	return core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{Kind: kubeVirtFixedKind, Request: req, Expected: claim, Provider: providerName, ResourceName: claim.Labels["name"], TerminalState: "deleted"},
		func(ctx context.Context, c *core.LeaseClaim, _ func() error) (core.LeaseTarget, error) {
			item, err := b.getVM(ctx, c.Labels["name"])
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if err = b.validateFixedItem(ctx, *c, item); err != nil {
				return core.LeaseTarget{}, err
			}
			key, err := b.resolveSSHKey(c.LeaseID)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if err = b.startVM(ctx, item.Metadata.Name); err != nil {
				return core.LeaseTarget{}, err
			}
			return b.prepareLease(ctx, item.Metadata.Name, c.LeaseID, c.Slug, key, c.Labels["keep"] == "true", c.Labels)
		}, func(ctx context.Context, c core.LeaseClaim) (core.LeaseTarget, error) {
			if req.ReleaseOnly {
				// Release observes under the deletion journal lock, including the
				// case where the bound UID is already absent. Never adopt a name.
				return core.LeaseTarget{LeaseID: c.LeaseID, Server: core.Server{CloudID: c.CloudID, ImmutableID: c.CloudImmutableID, Provider: providerName, Name: c.Labels["name"], Labels: maps.Clone(c.Labels)}}, nil
			}
			item, err := b.getVM(ctx, c.Labels["name"])
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if err = b.validateFixedItem(ctx, c, item); err != nil {
				return core.LeaseTarget{}, err
			}
			return core.LeaseTarget{LeaseID: c.LeaseID, Server: b.itemToServer(item)}, nil
		})
}

func (b *leaseBackend) deleteFixed(ctx context.Context, req core.ReleaseLeaseRequest, claim core.LeaseClaim, outcome *core.ReleaseLeaseOutcome) error {
	return core.DeleteFixedResource(ctx, kubeVirtFixedKind, claim, core.FixedLeaseOperations[kubeVirtItem]{
		Release: &core.FixedReleasePolicy{Outcome: outcome, SkipTerminalObservation: true}, ObserveExact: b.observeFixed,
		DeleteExact: func(ctx context.Context, _ *core.FixedTransaction, item kubeVirtItem) error {
			return b.deleteVMUID(ctx, item.Metadata.Name, claim.CloudImmutableID)
		},
	})
}

// Only an existing durable binding can authorize name-based API operations.
func (b *leaseBackend) boundVM(ctx context.Context, name string) (kubeVirtItem, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return kubeVirtItem{}, err
	}
	var bound *core.LeaseClaim
	for _, c := range claims {
		if b.claimMatchesScope(c, b.claimScope()) && c.Labels["name"] == name {
			if bound != nil {
				return kubeVirtItem{}, core.Exit(4, "ambiguous KubeVirt VM binding")
			}
			copy := c
			bound = &copy
		}
	}
	if bound == nil || !kubeVirtFixedKind.IsFixedClaim(*bound) || bound.FixedCreateIntent.State == "released" {
		return kubeVirtItem{}, core.Exit(4, "KubeVirt VM has no active durable UID binding; refusing mutation")
	}
	item, err := b.getVM(ctx, name)
	if err != nil {
		return item, err
	}
	return item, b.validateFixedItem(ctx, *bound, item)
}
