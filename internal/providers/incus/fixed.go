package incus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

var incusLeaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: providerName}

func (*backend) SupportsRequestedLeaseID() bool      { return true }
func (*backend) SupportsRequestedCheckpointID() bool { return true }

func (b *backend) acquireDurable(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := newClient(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := req.RequestedLeaseID
	if leaseID == "" {
		leaseID = core.NewLeaseID()
	}
	var publicKey string
	lease, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: incusLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID, RepoRoot: req.Repo.Root,
		Reclaim: req.Reclaim, TargetOS: cfg.TargetOS, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists && (claim.Provider != providerName || claim.FixedCreateIntent == nil) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: lease already has another owner")
		}
		identity, err := client.Identity()
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		if exists && claim.ProviderScope != identity.scope() {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Incus connection identity changed")
		}
		keyPath, key, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.SSHKey, publicKey = keyPath, key
		profile, err := client.Profile(core.Blank(cfg.Incus.Profile, "default"))
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		data, err := json.Marshal(struct {
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
		binding := core.FixedLeaseBinding{ProviderScope: identity.scope(), Fingerprint: fmt.Sprintf("%x", sha256.Sum256(data))}
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
		binding.Slug, err = core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
		return binding, err
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (LeaseTarget, error) {
		name := core.LeaseProviderName(leaseID, intent.Slug)
		if intent.Attempt == nil {
			if intent.State != "prepared" || claim.CloudID != "" {
				return LeaseTarget{}, core.Exit(4, "lease_id_conflict: Incus create intent has no attempt")
			}
			identity, err := client.Identity()
			if err != nil {
				return LeaseTarget{}, err
			}
			intent.Attempt = map[string]string{"name": name, "uuid": uuid.NewString()}
			cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
			labels := core.DirectLeaseLabels(cfg, leaseID, intent.Slug, providerName, "", req.Keep, time.Now().UTC())
			maps.Copy(labels, connectionMetadata(cfg, identity))
			labels["instance"], labels["image"] = name, cfg.Incus.Image
			labels["incus_uuid"] = intent.Attempt["uuid"]
			labels["ssh_user"], labels["ssh_port"], labels["work_root"] = cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot
			labels["release"], labels["fixed_intent_sha256"] = incusReleaseAction(cfg), intent.Fingerprint
			if cfg.Incus.ProxyListenPort != "" {
				labels["proxy_port"], labels["proxy_host"] = cfg.Incus.ProxyListenPort, sshHostForConfig(cfg)
			}
			claim.Labels = labels
			// Persist before submitting. Even a transport error can have created the instance.
			if err := persist(); err != nil {
				return LeaseTarget{}, err
			}
		}
		if intent.Attempt["name"] != name || intent.Attempt["uuid"] == "" {
			return LeaseTarget{}, core.Exit(4, "lease_id_conflict: Incus attempt identity changed")
		}
		inst, _, err := client.GetInstance(name)
		if api.StatusErrorCheck(err, 404) && intent.State == "prepared" && claim.CloudID == "" {
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
					return LeaseTarget{}, err
				}
				create.Source = api.InstanceSource{Type: "image", Fingerprint: cfg.Incus.Image}
			}
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=incus lease=%s slug=%s instance=%s\n", leaseID, intent.Slug, name)
			if err := client.CreateInstance(create); err != nil {
				return LeaseTarget{}, fmt.Errorf("Incus create outcome uncertain for lease=%s instance=%s; claim and key retained, retry the same lease ID or stop it: %w", leaseID, name, err)
			}
			inst, _, err = client.GetInstance(name)
		}
		if err != nil {
			return LeaseTarget{}, fmt.Errorf("reconcile Incus lease=%s instance=%s (claim and key retained): %w", leaseID, name, err)
		}

		if err := validateClaimInstance(client, *claim, *inst); err != nil {
			return LeaseTarget{}, err
		}
		claim.CloudID, claim.CloudImmutableID = name, inst.Config["volatile.uuid"]
		if err := persist(); err != nil {
			return LeaseTarget{}, err
		}
		if len(cfg.Incus.CheckpointMetadata) != 0 && inst.Config[labelKey("fork_identity")] != "ready" {
			if inst.IsActive() {
				return LeaseTarget{}, core.Exit(4, "Incus fork became active before identity replacement; refusing reuse")
			}
			if err := prepareForkIdentity(client, cfg, *inst, publicKey); err != nil {
				return LeaseTarget{}, fmt.Errorf("prepare stopped Incus fork %s (claim retained): %w", name, err)
			}
			labels := labelsFromInstance(*inst)
			labels["fork_identity"] = "ready"
			if err := setInstanceLabels(ctx, client, name, labels); err != nil {
				return LeaseTarget{}, err
			}
		}
		if !inst.IsActive() {
			if err := client.SetInstanceState(name, api.InstanceStatePut{Action: "start", Timeout: durationSecondsCeil(cfg.Incus.StartTimeout)}, ""); err != nil {
				return LeaseTarget{}, err
			}
		}
		inst, _, err = b.waitForAddress(ctx, client, name)
		if err != nil {
			return LeaseTarget{}, err
		}
		if err := validateClaimInstance(client, *claim, *inst); err != nil {
			return LeaseTarget{}, err
		}
		server := serverFromInstance(*inst, nil, cfg)
		target := core.SSHTargetFromConfig(cfg, sshTargetHost(server, cfg))
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Labels = touchInstanceLabels(server.Labels, cfg, "ready", time.Now().UTC())
		if err := setInstanceLabels(ctx, client, name, server.Labels); err != nil {
			return LeaseTarget{}, err
		}
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}, ctx)
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
		return LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) releaseDurable(ctx context.Context, client instanceClient, leaseID string, remove, force bool) error {
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !exists || !incusLeaseKind.IsFixedClaim(claim) {
		return core.Exit(4, "Incus lease %s has no durable ownership claim", leaseID)
	}
	if claim.FixedCreateIntent.State == "released" {
		return nil
	}
	if remove {
		if _, err := b.deleteDurable(ctx, client, claim, force); err != nil {
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

func (b *backend) RetainLeaseClaimAfterReleaseWithClaim(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	if !incusDeleteOnRelease(lease, b.configForRun()) {
		return true, nil
	}
	return incusLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_intent_sha256"] != "", nil, nil)
}

// Commit validated deletion before the remote effect. A prepared create's first
// 404 is inconclusive, whereas absence after this phase is safe to finalize.
func (b *backend) deleteDurable(ctx context.Context, client instanceClient, claim core.LeaseClaim, force bool) (bool, error) {
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
	if claim.FixedCreateIntent.State != "deleting" {
		deleting := claim
		intent := *claim.FixedCreateIntent
		intent.State = "deleting"
		deleting.FixedCreateIntent = &intent
		deleting.CloudID, deleting.CloudImmutableID = name, intent.Attempt["uuid"]
		updated, err := core.ReplaceLeaseClaimIfUnchangedDurableAfter(claim.LeaseID, claim, deleting, func() error { _, err := lookup(); return err })
		if err != nil {
			return false, err
		}
		claim = updated
	}
	err := incusLeaseKind.FinalizeAfterCleanup(claim, func() error {
		inst, err := lookup()
		if err != nil || inst == nil {
			return err
		}
		if inst.IsActive() {
			if err := client.SetInstanceState(name, api.InstanceStatePut{Action: "stop", Force: force, Timeout: durationSecondsCeil(b.cfg.Incus.StartTimeout)}, ""); err != nil {
				return err
			}
		}
		// Stop may wait for a remote operation; recheck the same incarnation
		// before the subsequent name-based deletion.
		if current, err := lookup(); err != nil || current == nil {
			return err
		}
		return client.DeleteInstance(name)
	})
	return true, err
}
