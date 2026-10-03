package scaleway

import (
	"context"
	"maps"
	"strings"
	"time"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var fixedLeaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: "Scaleway", DeletionState: "deleting"}

func (*Backend) SupportsRequestedLeaseID() bool { return true }

func (b *Backend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if err := validateFoundationConfig(b.cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.cfgForRun()
	if cfg.TargetOS != core.TargetLinux || len(cfg.Scaleway.SSHCIDRs) != 0 {
		return core.LeaseTarget{}, core.Exit(2, "Scaleway fixed leases require Linux and a preconfigured security group")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.newClient(cfg, b.rt)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.Scaleway.ProjectID, cfg.Scaleway.OrganizationID = client.ProjectID(), client.OrganizationID()
	cfg.ServerType, cfg.Scaleway.Region, cfg.Scaleway.Zone = cfg.Scaleway.Type, client.Region(), client.Zone()
	var publicKey string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: b.clockNow,
	}, core.FixedLeaseOperations[*instance.Server]{
		// Replay attaches the same journaled root, whose ownership and detachment
		// are checked before admission; it never allocates replacement children.
		Admission: &core.FixedAdmission{RepeatSameIdentity: true, PendingKey: "server", PendingValue: "pending", SubmittedValue: "submitted"}, DeferredAdmission: true,
		DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
			if exists && (!fixedLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != client.ProjectID()) {
				return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Scaleway owner or project changed")
			}
			var err error
			publicKey, err = core.PrepareFixedSSHKey(&cfg, req.RequestedLeaseID, core.FixedKeyPolicy{RequireExisting: exists && claim.FixedCreateIntent.Attempt != nil, UseStored: true})
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			fingerprint, err := core.FixedIntentFingerprint("", struct {
				Labels                                                    map[string]string
				Provider                                                  core.ScalewayConfig
				Type, Architecture, Bootstrap, Slug, User, Port, WorkRoot string
				Keep                                                      bool
				TTL, Idle                                                 time.Duration
			}{core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, providerName, "", req.Keep, time.Unix(0, 0)), cfg.Scaleway, cfg.ServerType, cfg.Architecture, core.CloudInitUserData(cfg, publicKey), core.NormalizeLeaseSlug(req.RequestedSlug), cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, req.Keep, cfg.TTL, cfg.IdleTimeout})
			binding := core.FixedLeaseBinding{ProviderScope: client.ProjectID(), Fingerprint: fingerprint}
			if err != nil || exists {
				return binding, err
			}
			items, err := b.listScalewayServers(ctx, client)
			for _, item := range items {
				if b.ownedServer(item) {
					binding.Inventory = append(binding.Inventory, b.serverFromScaleway(item))
				}
			}
			binding.AllocateSlug, binding.RejectExistingLease, binding.RequestedSlug = true, true, req.RequestedSlug
			return binding, err
		},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[*instance.Server], error) {
			claim := tx.Claim
			if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
				cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, claim.LeaseID, claim.Slug, cfg.Provider)
			}
			if claim.FixedCreateIntent.Attempt == nil || claim.FixedCreateIntent.Attempt["server"] == "pending" {
				return core.FixedObservation[*instance.Server]{CanSubmit: true}, nil
			}
			item, err := b.loadFixedServer(ctx, client, *claim)
			if err == nil && item == nil {
				return core.FixedObservation[*instance.Server]{CanSubmit: true}, nil
			}
			return core.FixedObservation[*instance.Server]{Candidates: []*instance.Server{item}}, err
		},
		Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			image, err := b.resolveImage(ctx, client, cfg)
			labels := labelsFromTags(leaseTags(cfg, claim.LeaseID, claim.Slug, "provisioning", req.Keep, core.FixedCreateTime(claim)))
			labels["scaleway_project"], labels["scaleway_zone"] = client.ProjectID(), client.Zone()
			labels["scaleway_organization"], labels["scaleway_region"] = client.OrganizationID(), client.Region()
			labels["scaleway_ssh_key_name"] = providerKeyName(claim.LeaseID)
			labels[volumeContractLabel] = rootVolumeContract
			return core.FixedAttemptPlan{NonceKey: "nonce", FingerprintLabel: "fixed_intent_sha256", NonceLabel: "fixed_attempt",
				Labels: labels, PrivateLabels: map[string]string{"recovery": "ambiguous-create"}, Values: map[string]string{"image": image, "server": "pending"}}, err
		},
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (*instance.Server, error) {
			return b.submitFixedServer(ctx, client, cfg, publicKey, tx)
		},
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item *instance.Server) (core.LeaseTarget, error) {
			return b.prepareFixedServer(ctx, client, cfg, publicKey, tx, item)
		},
	})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func (b *Backend) submitFixedServer(ctx context.Context, client Client, cfg core.Config, publicKey string, tx *core.FixedTransaction) (*instance.Server, error) {
	claim := tx.Claim
	if _, err := b.fixedKey(ctx, client, tx, publicKey, true); err != nil {
		return nil, err
	}
	if err := prepareFixedRoot(ctx, client, tx, true); err != nil {
		return nil, err
	}
	labels := maps.Clone(claim.Labels)
	if err := tx.Admit(); err != nil {
		return nil, err
	}
	request := &instance.CreateServerRequest{Zone: scw.Zone(client.Zone()), Name: core.LeaseProviderName(claim.LeaseID, claim.Slug),
		DynamicIPRequired: scw.BoolPtr(true), CommercialType: cfg.ServerType, Image: scw.StringPtr(claim.FixedCreateIntent.Attempt["image"]),
		Project: scw.StringPtr(client.ProjectID()), Tags: tagsFromLabels(labels),
		Volumes: map[string]*instance.VolumeServerTemplate{"0": {ID: scw.StringPtr(labels[rootVolumeLabel]), Boot: scw.BoolPtr(true)}}}
	if sg := strings.TrimSpace(cfg.Scaleway.SecurityGroup); sg != "" {
		request.SecurityGroup = scw.StringPtr(sg)
	}
	response, err := client.Instance().CreateServer(request, scw.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if response == nil || response.Server == nil || response.Server.ID == "" {
		return nil, core.FixedUncertainCustody(claim.LeaseID)
	}
	item := response.Server
	if err := tx.Bind(core.FixedResourceBinding{CloudID: item.ID, ImmutableID: item.ID, Labels: labels}); err != nil {
		return nil, err
	}
	return item, rootVolumeFromLabels(labels).validate()
}

func (b *Backend) prepareFixedServer(ctx context.Context, client Client, cfg core.Config, publicKey string, tx *core.FixedTransaction, item *instance.Server) (core.LeaseTarget, error) {
	claim := tx.Claim
	if err := b.validateFixedServer(client, *claim, item); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := tx.Bind(core.FixedResourceBinding{CloudID: item.ID, ImmutableID: item.ID}); err != nil {
		return core.LeaseTarget{}, err
	}
	labels := maps.Clone(claim.Labels)
	if claim.FixedCreateIntent.Attempt["userdata"] == "" {
		if err := client.Instance().SetServerUserData(&instance.SetServerUserDataRequest{Zone: scw.Zone(client.Zone()), ServerID: item.ID,
			Key: "cloud-init", Content: strings.NewReader(core.CloudInitUserData(cfg, publicKey))}, scw.WithContext(ctx)); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := tx.Observe(core.FixedResourceBinding{AttemptValues: map[string]string{"userdata": "written"}}); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if item.State == instance.ServerStateStopped || item.State == instance.ServerStateStoppedInPlace {
		if _, err := client.Instance().ServerAction(&instance.ServerActionRequest{Zone: scw.Zone(client.Zone()), ServerID: item.ID, Action: instance.ServerActionPoweron}, scw.WithContext(ctx)); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	item, err := b.waitForPublicIPv4(ctx, client, item.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.validateFixedServer(client, *claim, item); err != nil {
		return core.LeaseTarget{}, err
	}
	server := b.serverFromScaleway(item)
	ssh := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	wait := b.waitSSH
	if wait == nil {
		wait = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
			return core.WaitForSSHReady(ctx, target, b.rt.Stderr, phase, timeout)
		}
	}
	if err := wait(ctx, &ssh, "scaleway bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	labels["state"] = "ready"
	delete(labels, "recovery")
	if _, err := client.Instance().UpdateServer(&instance.UpdateServerRequest{Zone: scw.Zone(client.Zone()), ServerID: item.ID,
		Tags: ptrTags(shared.ReplaceCrabboxTags(item.Tags, tagsFromLabels(labels)))}, scw.WithContext(ctx)); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Labels, server.ImmutableID = labels, item.ID
	return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: claim.LeaseID}, nil
}

func (b *Backend) validateFixedServer(client Client, claim core.LeaseClaim, item *instance.Server) error {
	intent := claim.FixedCreateIntent
	if !fixedLeaseKind.IsFixedClaim(claim) || intent.State == "released" || item == nil || !b.ownedServer(item) {
		return core.Exit(4, "lease_id_conflict: Scaleway server has no fixed ownership evidence")
	}
	labels := labelsFromTags(item.Tags)
	if item.ID == "" || item.Name != core.LeaseProviderName(claim.LeaseID, claim.Slug) || item.Project != client.ProjectID() || string(item.Zone) != client.Zone() ||
		labels["lease"] != claim.LeaseID || labels["slug"] != claim.Slug || labels["provider_key"] != claim.Labels["provider_key"] ||
		intent.Attempt["nonce"] == "" || labels["fixed_attempt"] != intent.Attempt["nonce"] || labels["fixed_intent_sha256"] != intent.Fingerprint ||
		labels["scaleway_ssh_key_id"] != claim.Labels["scaleway_ssh_key_id"] || claim.CloudID != "" && claim.CloudID != item.ID {
		return core.Exit(4, "lease_id_conflict: Scaleway server does not match fixed create intent")
	}
	if labels[rootVolumeLabel] != claim.Labels[rootVolumeLabel] || labels[volumeContractLabel] != rootVolumeContract {
		return core.Exit(4, "lease_id_conflict: Scaleway root-volume identity changed")
	}
	return rootVolumeFromLabels(claim.Labels).validate()
}

// A nil server without error means complete inventory confirmed an unbound
// attempt has no server; callers must still attest its children before mutation.
func (b *Backend) loadFixedServer(ctx context.Context, client Client, claim core.LeaseClaim) (*instance.Server, error) {
	return core.LookupFixedResource(ctx, fixedLeaseKind, claim, func(ctx context.Context, claim core.LeaseClaim) (*instance.Server, error) {
		if claim.CloudID != "" {
			response, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: claim.CloudID}, scw.WithContext(ctx))
			if err != nil {
				return nil, err
			}
			if response == nil {
				return nil, core.FixedUncertainCustody(claim.LeaseID)
			}
			return response.Server, b.validateFixedServer(client, claim, response.Server)
		}
		// Read the complete project inventory so a related server with changed
		// ownership tags cannot be mistaken for an absent submission.
		response, err := client.Instance().ListServers(&instance.ListServersRequest{Zone: scw.Zone(client.Zone()), Project: scw.StringPtr(client.ProjectID())}, scw.WithContext(ctx), scw.WithAllPages())
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, core.FixedUncertainCustody(claim.LeaseID)
		}
		for _, item := range response.Servers {
			if item == nil {
				return nil, core.FixedUncertainCustody(claim.LeaseID)
			}
			labels := labelsFromTags(item.Tags)
			if item.Name == core.LeaseProviderName(claim.LeaseID, claim.Slug) || labels["lease"] == claim.LeaseID ||
				labels["fixed_attempt"] != "" && labels["fixed_attempt"] == claim.FixedCreateIntent.Attempt["nonce"] {
				if err := b.validateFixedServer(client, claim, item); err != nil {
					return nil, err
				}
			}
		}
		item, found, err := core.SelectFixedCandidate(fixedLeaseKind, claim.LeaseID, response.Servers, func(item *instance.Server) bool {
			return b.validateFixedServer(client, claim, item) == nil
		})
		if err == nil && !found {
			// Admit is durable before the API call. Absence permits recovery only
			// with this attempt's already-journaled children, revalidated by Submit
			// or release before any creation or deletion.
			attempt := claim.FixedCreateIntent.Attempt
			if attempt["server"] != "submitted" || attempt["key_submitted"] == "" || attempt["volume_submitted"] == "" ||
				claim.Labels["scaleway_ssh_key_id"] == "" || claim.Labels[rootVolumeLabel] == "" {
				err = core.FixedUncertainCustody(claim.LeaseID)
			}
		}
		return item, err
	})
}

func (b *Backend) fixedKey(ctx context.Context, client Client, tx *core.FixedTransaction, publicKey string, create bool) (*iam.SSHKey, error) {
	claim := tx.Claim
	submitted := claim.FixedCreateIntent.Attempt["key_submitted"] != ""
	if !create && !submitted {
		return nil, nil
	}
	labels := maps.Clone(claim.Labels)
	id, name := labels["scaleway_ssh_key_id"], labels["scaleway_ssh_key_name"]
	var key *iam.SSHKey
	var err error
	if id != "" {
		key, err = client.IAM().GetSSHKey(&iam.GetSSHKeyRequest{SSHKeyID: id}, scw.WithContext(ctx))
		if !create && isScalewayNotFound(err) {
			return nil, nil
		}
	} else {
		if publicKey == "" {
			cfg := b.cfgForRun()
			publicKey, err = core.PrepareFixedSSHKey(&cfg, claim.LeaseID, core.FixedKeyPolicy{RequireExisting: true, UseStored: true})
			if err != nil {
				return nil, err
			}
		}
		key, err = b.reconcileSSHKey(ctx, client, name, publicKey)
		if err == nil && key != nil && !submitted {
			return nil, core.Exit(4, "lease_id_conflict: Scaleway SSH key exists without allocation evidence")
		}
		if err == nil && key == nil && create && !submitted {
			if err := tx.Observe(core.FixedResourceBinding{AttemptValues: map[string]string{"key_submitted": "true"}}); err != nil {
				return nil, err
			}
			key, err = client.IAM().CreateSSHKey(&iam.CreateSSHKeyRequest{Name: name, PublicKey: publicKey, ProjectID: client.ProjectID()}, scw.WithContext(ctx))
		}
	}
	if err != nil {
		return nil, err
	}
	if key == nil || key.ID == "" {
		return nil, core.FixedUncertainCustody(claim.LeaseID)
	}
	if key.Name != name || key.ProjectID != client.ProjectID() || id != "" && id != key.ID || publicKey != "" && strings.TrimSpace(key.PublicKey) != strings.TrimSpace(publicKey) {
		return nil, core.Exit(4, "lease_id_conflict: Scaleway SSH key identity changed")
	}
	labels["scaleway_ssh_key_id"] = key.ID
	return key, tx.Observe(core.FixedResourceBinding{Labels: labels})
}
