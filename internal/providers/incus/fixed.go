package incus

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

var incusLeaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: providerName, DeletionState: "deleting"}

func (*backend) SupportsRequestedLeaseID() bool      { return true }
func (*backend) SupportsRequestedCheckpointID() bool { return true }

func (b *backend) acquireDurable(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := req.RequestedLeaseID
	if leaseID == "" {
		leaseID = core.NewLeaseID()
	}
	var publicKey string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: incusLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID, RepoRoot: req.Repo.Root,
		Reclaim: req.Reclaim, TargetOS: cfg.TargetOS, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, core.FixedLeaseOperations[*api.Instance]{Admission: &core.FixedAdmission{RepeatSameIdentity: true}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		identity, err := client.Identity()
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		if exists && claim.ProviderScope != identity.scope() {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Incus connection identity changed")
		}
		publicKey, err = core.PrepareFixedSSHKey(&cfg, leaseID, core.FixedKeyPolicy{PreserveProviderKey: true})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		profile, err := client.Profile(core.Blank(cfg.Incus.Profile, "default"))
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		fingerprint, err := core.FixedIntentFingerprint("", struct {
			Incus                                       core.IncusConfig
			Profile                                     api.ProfilePut
			Bootstrap                                   string
			Slug, Pond, Architecture, Class, ServerType string
			Keep                                        bool
			TTL, Idle                                   time.Duration
		}{cfg.Incus, profile.ProfilePut, core.CloudInitUserData(cfg, publicKey), req.RequestedSlug, cfg.Pond, cfg.Architecture, cfg.Class, cfg.ServerType, req.Keep, cfg.TTL, cfg.IdleTimeout})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: identity.scope(), Fingerprint: fingerprint}
		if exists {
			return binding, nil
		}
		instances, err := client.ListInstances()
		if err != nil {
			return binding, err
		}
		servers := make([]core.Server, 0, len(instances))
		for _, inst := range instances {
			servers = append(servers, serverFromInstance(inst, nil, cfg))
		}
		binding.AllocateSlug, binding.RequestedSlug, binding.Inventory = true, req.RequestedSlug, servers
		return binding, nil
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		name := core.LeaseProviderName(leaseID, claim.Slug)
		identity, err := client.Identity()
		if err != nil {
			return core.FixedAttemptPlan{}, err
		}
		cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
		labels := connectionMetadata(cfg, identity)
		labels["instance"], labels["image"] = name, cfg.Incus.Image
		labels["ssh_user"], labels["ssh_port"], labels["work_root"] = cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot
		labels["release"] = incusReleaseAction(cfg)
		if cfg.Incus.ProxyListenPort != "" {
			labels["proxy_port"], labels["proxy_host"] = cfg.Incus.ProxyListenPort, sshHostForConfig(cfg)
		}
		return core.FixedAttemptPlan{
			Values: map[string]string{"name": name, "uuid": uuid.NewString()},
			Labels: labels, DirectLabels: &core.FixedDirectLabels{Config: cfg, Provider: providerName, Keep: req.Keep, Now: time.Now().UTC()},
			FingerprintLabel: "fixed_intent_sha256", AttemptLabels: map[string]string{"incus_uuid": "uuid"},
		}, nil
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[*api.Instance], error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		name := core.LeaseProviderName(leaseID, intent.Slug)
		var result core.FixedObservation[*api.Instance]
		if _, err := core.ReadFixedAttempt[map[string]string](intent, core.FixedAttemptFormat{RejectEmptyObject: true, Equal: map[string]string{"name": name}, Required: []string{"uuid"}}); err != nil {
			return result, err
		}

		inst, _, err := client.GetInstance(name)
		if api.StatusErrorCheck(err, 404) {
			result.CanSubmit = true
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("reconcile Incus lease=%s instance=%s (claim and key retained): %w", leaseID, name, err)
		}
		if intent.Attempt == nil {
			return result, core.Exit(4, "lease_id_conflict: Incus create intent has no attempt")
		}
		if err := validateClaimInstance(client, *claim, *inst); err != nil {
			return result, err
		}
		return core.FixedObservation[*api.Instance]{Candidates: []*api.Instance{inst}}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (*api.Instance, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		name := intent.Attempt["name"]
		// A repeated submission uses the exact same daemon-unique name and UUID.
		// Incus rejects a concurrent create of that name, even after reply loss.
		create := api.InstancesPost{Name: name, Type: api.InstanceType(normalizeInstanceType(cfg.Incus.InstanceType)), InstancePut: api.InstancePut{
			Config: instanceConfigForCreate(cfg, claim.Labels, publicKey), Profiles: profilesForConfig(cfg), Devices: devicesForCreate(cfg),
		}, Source: imageSourceForConfig(cfg)}
		create.Config["volatile.uuid"] = intent.Attempt["uuid"]
		if len(cfg.Incus.CheckpointMetadata) != 0 {
			// Only checkpoint clones must remain stopped until inherited credentials are replaced.
			create.Config["boot.autostart"] = "false"
			if err := validateForkImage(client, cfg); err != nil {
				return nil, err
			}
			create.Source = api.InstanceSource{Type: "image", Fingerprint: cfg.Incus.Image}
		}
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=incus lease=%s slug=%s instance=%s\n", leaseID, intent.Slug, name)
		if err := client.CreateInstance(create); err != nil {
			return nil, fmt.Errorf("Incus create outcome uncertain for lease=%s instance=%s; claim and key retained, retry the same lease ID or stop it: %w", leaseID, name, err)
		}
		inst, _, err := client.GetInstance(name)
		if err != nil {
			return nil, fmt.Errorf("reconcile Incus lease=%s instance=%s (claim and key retained): %w", leaseID, name, err)
		}
		return inst, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, inst *api.Instance) (core.LeaseTarget, error) {
		claim := tx.Claim
		name := claim.FixedCreateIntent.Attempt["name"]

		if err := validateClaimInstance(client, *claim, *inst); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := tx.Bind(core.FixedResourceBinding{CloudID: name, ImmutableID: inst.Config["volatile.uuid"]}); err != nil {
			return core.LeaseTarget{}, err
		}
		if len(cfg.Incus.CheckpointMetadata) != 0 && inst.Config[labelKey("fork_identity")] != "ready" {
			if inst.IsActive() {
				return core.LeaseTarget{}, core.Exit(4, "Incus fork became active before identity replacement; refusing reuse")
			}
			if err := prepareForkIdentity(client, cfg, *inst, publicKey); err != nil {
				return core.LeaseTarget{}, fmt.Errorf("prepare stopped Incus fork %s (claim retained): %w", name, err)
			}
			labels := labelsFromInstance(*inst)
			labels["fork_identity"] = "ready"
			if err := setInstanceLabels(ctx, client, name, labels); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		if !inst.IsActive() {
			if err := client.SetInstanceState(name, api.InstanceStatePut{Action: "start", Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, ""); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		inst, _, err := b.waitForAddress(ctx, client, name)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateClaimInstance(client, *claim, *inst); err != nil {
			return core.LeaseTarget{}, err
		}
		server := serverFromInstance(*inst, nil, cfg)
		target := core.SSHTargetFromConfig(cfg, sshTargetHost(server, cfg))
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Now().UTC())
		if err := setInstanceLabels(ctx, client, name, server.Labels); err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}})
	if err != nil {
		// Fixed IDs keep their durable attempt on every failure. Generated IDs retain
		// the historical Keep/bootstrap retry contract, with ownership-checked cleanup.
		if req.RequestedLeaseID == "" {
			cleanup := func() error { return b.releaseDurable(ctx, client, leaseID, true, true) }
			if !req.Keep {
				err = errors.Join(err, cleanup())
			} else {
				err = &retainedAcquireError{err: err, cleanup: func() {
					if cleanupErr := cleanup(); cleanupErr != nil {
						fmt.Fprintf(b.rt.Stderr, "warning: Incus cleanup: %v\n", cleanupErr)
					}
				}}
			}
		}
		return core.LeaseTarget{}, err
	}
	return core.CompleteFixedAcquisition(lease, nil, req)
}

func (b *backend) releaseDurable(ctx context.Context, client instanceClient, leaseID string, remove, force bool) error {
	return b.releaseDurableWithOutcome(ctx, client, leaseID, remove, force, &core.ReleaseLeaseOutcome{})
}

func (b *backend) releaseDurableWithOutcome(ctx context.Context, client instanceClient, leaseID string, remove, force bool, outcome *core.ReleaseLeaseOutcome) error {
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !exists || !incusLeaseKind.IsFixedClaim(claim) {
		return core.Exit(4, "Incus lease %s has no durable ownership claim", leaseID)
	}
	if claim.FixedCreateIntent.State == "released" {
		outcome.Terminal = true
		return nil
	}
	if remove {
		if _, err := b.deleteDurableWithOutcome(ctx, client, claim, force, outcome); err != nil {
			return err
		}
		core.RemoveStoredTestboxKey(leaseID)
		return nil
	}
	if claim.FixedCreateIntent.State == "deleting" {
		return core.Exit(4, "Incus lease deletion is in progress; retry stop without --keep")
	}
	name := claim.FixedCreateIntent.Attempt["name"]
	if name == "" {
		return core.Exit(4, "Incus lease %s has no recorded creation attempt", leaseID)
	}
	action := func() error {
		if err := verifyConnection(client, claim.ProviderScope); err != nil {
			return err
		}
		inst, _, err := client.GetInstance(name)
		if err != nil {
			return err
		}
		if err := validateClaimInstance(client, claim, *inst); err != nil {
			return err
		}
		if inst.IsActive() {
			if err := client.SetInstanceState(name, api.InstanceStatePut{Action: "stop", Force: force, Timeout: durationSecondsCeil(b.cfg.Incus.StartTimeout)}, ""); err != nil {
				return err
			}
		}
		inst, _, err = client.GetInstance(name)
		if err != nil {
			return err
		}
		if err := validateClaimInstance(client, claim, *inst); err != nil {
			return err
		}
		labels := labelsFromInstance(*inst)
		labels["state"], labels["release"] = "stopped", "stop"
		delete(labels, "host")
		return setInstanceLabels(ctx, client, name, labels)
	}
	server := core.Server{CloudID: name, ImmutableID: claim.CloudImmutableID, Provider: providerName, Name: name, Labels: maps.Clone(claim.Labels), Status: "stopped"}
	server.Labels["state"], server.Labels["release"] = "stopped", "stop"
	delete(server.Labels, "host")
	_, err = core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, core.SSHTarget{}, action)
	return err
}

func (b *backend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	if !incusDeleteOnRelease(lease, b.configForRun()) {
		return true, nil
	}
	return incusLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_intent_sha256"] != "", nil, nil)
}

// Commit validated deletion before the remote effect. A prepared create's first
// 404 is inconclusive, whereas absence after this phase is safe to finalize.
func (b *backend) deleteDurable(ctx context.Context, client instanceClient, claim core.LeaseClaim, force bool) (bool, error) {
	return b.deleteDurableWithOutcome(ctx, client, claim, force, &core.ReleaseLeaseOutcome{})
}

func (b *backend) deleteDurableWithOutcome(ctx context.Context, client instanceClient, claim core.LeaseClaim, force bool, outcome *core.ReleaseLeaseOutcome) (bool, error) {
	name := claim.FixedCreateIntent.Attempt["name"]
	lookup := func() (*api.Instance, error) {
		if err := verifyConnection(client, claim.ProviderScope); err != nil {
			return nil, err
		}
		inst, _, err := client.GetInstance(name)
		if api.StatusErrorCheck(err, 404) && (claim.FixedCreateIntent.State == "acquired" || claim.FixedCreateIntent.State == "deleting") {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if err := validateClaimInstance(client, claim, *inst); err != nil {
			return nil, err
		}
		return inst, nil
	}
	if name == "" {
		return false, core.Exit(4, "Incus lease has no recorded creation attempt")
	}
	err := core.DeleteFixedResource(ctx, incusLeaseKind, claim, core.FixedLeaseOperations[*api.Instance]{
		Release: &core.FixedReleasePolicy{Outcome: outcome, Binding: &core.FixedResourceBinding{CloudID: name, ImmutableID: claim.FixedCreateIntent.Attempt["uuid"]}},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[*api.Instance], error) {
			claim = *tx.Claim
			inst, err := lookup()
			if err != nil {
				return core.FixedObservation[*api.Instance]{}, err
			}
			if inst == nil {
				return core.FixedObservation[*api.Instance]{AbsenceProven: true}, nil
			}
			return core.FixedObservation[*api.Instance]{Candidates: []*api.Instance{inst}}, nil
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, inst *api.Instance) error {
			claim = *tx.Claim
			if inst.IsActive() {
				if err := client.SetInstanceState(name, api.InstanceStatePut{Action: "stop", Force: force, Timeout: durationSecondsCeil(b.cfg.Incus.StartTimeout)}, ""); err != nil {
					return err
				}
			}
			// Stop waits on a native operation; re-attest before name deletion.
			if current, err := lookup(); err != nil || current == nil {
				return err
			}
			return client.DeleteInstance(name)
		},
	})
	return true, err
}
