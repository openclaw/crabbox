package proxmox

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedProxmoxCreateIntentVersion = 1

var fixedProxmoxLeaseKind = core.FixedLeaseKind{
	ClaimProvider:  core.FixedProxmoxClaimProvider,
	IntentVersion:  fixedProxmoxCreateIntentVersion,
	Label:          "Proxmox",
	DeletionState:  "deleting",
	ResourcePlural: "Proxmox VMs",
	TerminalIdentityLabels: []string{
		"crabbox", "provider", "lease", "slug", "provider_key",
		"fixed_intent_sha256", "node", "template_id",
	},
}

type fixedProxmoxCreateIntent struct {
	Labels         map[string]string `json:"labels"`
	SSHPort        string            `json:"sshPort"`
	ProviderScope  string            `json:"providerScope"`
	Node           string            `json:"node"`
	TemplateID     int               `json:"templateId"`
	Storage        string            `json:"storage,omitempty"`
	Pool           string            `json:"pool,omitempty"`
	Bridge         string            `json:"bridge,omitempty"`
	User           string            `json:"user"`
	WorkRoot       string            `json:"workRoot"`
	FullClone      bool              `json:"fullClone"`
	ServerType     string            `json:"serverType"`
	TargetOS       string            `json:"targetOS"`
	RequestedSlug  string            `json:"requestedSlug,omitempty"`
	Keep           bool              `json:"keep"`
	TTLNanoseconds int64             `json:"ttlNanoseconds"`
	IdleNanos      int64             `json:"idleNanoseconds"`
	SSHPublicKey   string            `json:"sshPublicKey"`
}

func fixedProxmoxFingerprint(cfg core.Config, req core.AcquireRequest, providerScope, publicKey string) (string, error) {
	return core.FixedIntentFingerprint("", fixedProxmoxCreateIntent{
		Labels:        core.DirectLeaseLabels(cfg, req.RequestedLeaseID, core.NormalizeLeaseSlug(req.RequestedSlug), "proxmox", "", req.Keep, time.Unix(0, 0)),
		SSHPort:       cfg.SSHPort,
		ProviderScope: providerScope, Node: strings.TrimSpace(cfg.Proxmox.Node),
		TemplateID: cfg.Proxmox.TemplateID, Storage: strings.TrimSpace(cfg.Proxmox.Storage),
		Pool: strings.TrimSpace(cfg.Proxmox.Pool), Bridge: strings.TrimSpace(cfg.Proxmox.Bridge),
		User: strings.TrimSpace(cfg.SSHUser), WorkRoot: strings.TrimSpace(cfg.WorkRoot),
		FullClone: cfg.Proxmox.FullClone, ServerType: strings.TrimSpace(cfg.ServerType),
		TargetOS: strings.TrimSpace(cfg.TargetOS), RequestedSlug: core.NormalizeLeaseSlug(req.RequestedSlug),
		Keep: req.Keep, TTLNanoseconds: cfg.TTL.Nanoseconds(), IdleNanos: cfg.IdleTimeout.Nanoseconds(),
		SSHPublicKey: strings.TrimSpace(publicKey),
	})
}

func (b *leaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if !core.IsCanonicalLeaseID(req.RequestedLeaseID) || req.RequestedCheckpointID != "" || b.Cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "fixed Proxmox creation requires a canonical lease ID and a Linux template")
	}
	if b.Cfg.Proxmox.TemplateID <= 0 {
		return core.LeaseTarget{}, core.Exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	}
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	cfg := b.Cfg
	cfg.ServerType = (Provider{}).ServerTypeForConfig(cfg)
	providerScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", cfg))
	if providerScope == "" {
		return core.LeaseTarget{}, core.Exit(2, "Proxmox cluster scope is unavailable; refusing fixed lease creation")
	}
	client, err := newClient(cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var publicKey, fingerprint string
	acquired, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedProxmoxLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: cfg.TargetOS,
		WindowsMode: cfg.WindowsMode, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, core.FixedLeaseOperations[core.Server]{DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists {
			if !fixedProxmoxLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != providerScope {
				return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: fixed Proxmox claim scope changed")
			}
			target := core.SSHTarget{}
			if err := core.UseStoredTestboxKey(&target, leaseID); err != nil {
				return core.FixedLeaseBinding{}, err
			}
		}
		keyPath, key, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.SSHKey, publicKey = keyPath, key
		cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
		fingerprint, err = fixedProxmoxFingerprint(cfg, req, providerScope, publicKey)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: providerScope, Fingerprint: fingerprint}
		if exists {
			return binding, nil
		}
		servers, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding.Slug, err = core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
		return binding, err
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.Server], error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		var result core.FixedObservation[core.Server]
		if claim.ProviderScope != providerScope || intent.ProviderScope != providerScope {
			return result, core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s provider scope changed", leaseID)
		}
		server, found, err := b.findFixedProxmoxServer(ctx, client, *claim)
		if err != nil {
			return result, err
		}
		vmid, node, err := fixedProxmoxAttempt(*claim)
		if err != nil {
			return result, err
		}
		if found {
			if err := validateFixedProxmoxLocalBinding(*claim); err != nil {
				return result, err
			}
			if claim.CloudImmutableID == "" || server.Labels["state"] != "ready" {
				return result, core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has an unresolved generation or readiness binding; inspect and release the attempt", leaseID)
			}
			if err := validateFixedProxmoxServer(server, *claim, vmid, node); err != nil {
				return result, err
			}
			result.Candidates = []core.Server{server}
		} else if vmid != 0 {
			present, err := client.VMExistsInCluster(ctx, strconv.Itoa(vmid))
			if err != nil {
				return result, fmt.Errorf("reconcile fixed Proxmox VMID %d: %w", vmid, err)
			}
			if present {
				return result, core.Exit(4, "lease_id_conflict: fixed Proxmox VMID %d exists without matching lease identity", vmid)
			}
			return result, core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has an unresolved clone attempt; retain its claim for recovery", leaseID)
		} else {
			result.CanSubmit = true
		}
		return result, nil
	}, PlanAttempt: func(ctx context.Context, tx *core.FixedTransaction) error {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		vmid, err := client.NextVMID(ctx)
		if err != nil {
			return err
		}
		if vmid <= 0 {
			return core.Exit(4, "lease_id_conflict: Proxmox selected invalid VMID %d", vmid)
		}
		node := strings.TrimSpace(cfg.Proxmox.Node)
		intent.Attempt = map[string]string{"vmid": strconv.Itoa(vmid), "node": node}
		claim.CloudID, claim.CloudNumericID = strconv.Itoa(vmid), int64(vmid)
		claim.Labels = fixedProxmoxIdentityLabels(cfg, leaseID, intent.Slug, fingerprint, node)
		return validateFixedProxmoxLocalBinding(*claim)
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (core.Server, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		vmid, node, err := fixedProxmoxAttempt(*claim)
		if err != nil {
			return core.Server{}, err
		}
		if err := tx.Record("submitting"); err != nil {
			return core.Server{}, err
		}
		fmt.Fprintf(b.RT.Stderr, "provisioning provider=proxmox lease=%s slug=%s node=%s template=%d vmid=%d keep=%v fixed=true\n", leaseID, intent.Slug, cfg.Proxmox.Node, cfg.Proxmox.TemplateID, vmid, req.Keep)
		return client.CreateServerWithVMID(ctx, cfg, publicKey, leaseID, intent.Slug, req.Keep, vmid, maps.Clone(claim.Labels), func(created core.Server) error {
			if err := validateFixedProxmoxServer(created, *claim, vmid, node); err != nil {
				return err
			}
			claim.CloudImmutableID = created.ImmutableID
			claim.Labels = maps.Clone(created.Labels)
			return tx.Record("bound")
		})
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, server core.Server) (core.LeaseTarget, error) {
		claim := tx.Claim
		attemptVMID, attemptNode, err := fixedProxmoxAttempt(*claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if claim.CloudImmutableID == "" {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed Proxmox clone has no durable generation")
		}
		if err := validateFixedProxmoxServer(server, *claim, attemptVMID, attemptNode); err != nil {
			return core.LeaseTarget{}, err
		}
		target := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
		if err := waitForSSHReadyFunc(ctx, &target, b.RT.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Labels = maps.Clone(server.Labels)
		server.Labels["state"] = "ready"
		if err := client.SetLabelsOnNode(ctx, server.HostID, server.CloudID, server.Labels); err != nil {
			return core.LeaseTarget{}, fmt.Errorf("persist Proxmox fixed lease labels: %w", err)
		}
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(acquired); err != nil {
			return core.LeaseTarget{}, fmt.Errorf("acknowledge fixed Proxmox acquisition: %w", err)
		}
	}
	return acquired, nil
}

func fixedProxmoxIdentityLabels(cfg core.Config, leaseID, slug, fingerprint, node string) map[string]string {
	return map[string]string{
		"crabbox":             "true",
		"provider":            "proxmox",
		"lease":               leaseID,
		"slug":                slug,
		"provider_key":        core.ProviderKeyForLease(leaseID),
		"fixed_intent_sha256": fingerprint,
		"node":                node,
		"template_id":         strconv.Itoa(cfg.Proxmox.TemplateID),
	}
}

func fixedProxmoxAttempt(claim core.LeaseClaim) (int, string, error) {
	intent := claim.FixedCreateIntent
	if !fixedProxmoxLeaseKind.IsFixedClaim(claim) || intent.Version != fixedProxmoxCreateIntentVersion ||
		intent.Fingerprint == "" || intent.Slug != claim.Slug || intent.ProviderScope == "" ||
		(intent.State != "prepared" && intent.State != "acquired" && intent.State != "deleting") || len(intent.FailedAttempts) != 0 {
		return 0, "", core.Exit(4, "lease_id_conflict: invalid fixed Proxmox create intent for lease %s", claim.LeaseID)
	}
	if len(intent.Attempt) == 0 {
		if intent.State != "prepared" || claim.CloudID != "" || claim.CloudNumericID != 0 || claim.CloudImmutableID != "" || len(claim.Labels) != 0 {
			return 0, "", core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable clone attempt", claim.LeaseID)
		}
		return 0, "", nil
	}
	vmid, err := strconv.Atoi(intent.Attempt["vmid"])
	node := strings.TrimSpace(intent.Attempt["node"])
	if err != nil || vmid <= 0 || strconv.Itoa(vmid) != intent.Attempt["vmid"] || node == "" || len(intent.Attempt) != 2 {
		return 0, "", core.Exit(4, "lease_id_conflict: invalid fixed Proxmox clone attempt for lease %s", claim.LeaseID)
	}
	if claim.CloudID != strconv.Itoa(vmid) || claim.CloudNumericID != int64(vmid) ||
		claim.ProviderScope != intent.ProviderScope ||
		claim.Labels["crabbox"] != "true" || claim.Labels["provider"] != "proxmox" ||
		claim.Labels["lease"] != claim.LeaseID || claim.Labels["slug"] != claim.Slug ||
		claim.Labels["provider_key"] != core.ProviderKeyForLease(claim.LeaseID) ||
		claim.Labels["fixed_intent_sha256"] != intent.Fingerprint ||
		claim.Labels["node"] != node || claim.Labels["template_id"] == "" ||
		intent.State != "prepared" && claim.CloudImmutableID == "" {
		return 0, "", core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s durable VM identity is inconsistent", claim.LeaseID)
	}
	return vmid, node, nil
}

func (b *leaseBackend) findFixedProxmoxServer(ctx context.Context, client proxmoxClient, claim core.LeaseClaim) (core.Server, bool, error) {
	observed, err := core.InspectFixedResource(ctx, fixedProxmoxLeaseKind, claim, core.FixedLeaseOperations[core.Server]{
		ObserveExact: func(ctx context.Context, _ *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.Server], error) {
			servers, err := client.ListCrabboxServersCluster(ctx)
			if err != nil {
				return core.FixedObservation[core.Server]{}, err
			}
			var found []core.Server
			for _, server := range servers {
				if strings.TrimSpace(server.Labels["lease"]) == claim.LeaseID || server.CloudID == claim.CloudID || server.Labels["provider_key"] == core.ProviderKeyForLease(claim.LeaseID) {
					found = append(found, server)
				}
			}
			return core.FixedObservation[core.Server]{Candidates: found}, nil
		},
	})
	if err != nil {
		return core.Server{}, false, err
	}
	if len(observed.Candidates) == 0 {
		return core.Server{}, false, nil
	}
	return observed.Candidates[0], true, nil
}

func validateFixedProxmoxServer(server core.Server, claim core.LeaseClaim, vmid int, attemptNode string) error {
	intent := claim.FixedCreateIntent
	if vmid <= 0 || server.CloudID != strconv.Itoa(vmid) || server.ID != 0 && server.ID != int64(vmid) ||
		server.Provider != "proxmox" || server.HostID == "" || server.ImmutableID == "" ||
		server.Labels["crabbox"] != "true" || server.Labels["provider"] != "proxmox" ||
		server.Labels["lease"] != claim.LeaseID || server.Labels["slug"] != intent.Slug ||
		server.Labels["provider_key"] != core.ProviderKeyForLease(claim.LeaseID) ||
		server.Labels["fixed_intent_sha256"] != intent.Fingerprint ||
		server.Labels["node"] != attemptNode ||
		server.Labels["template_id"] != claim.Labels["template_id"] {
		return core.Exit(4, "lease_id_conflict: Proxmox VM for lease %s does not match its durable fixed identity", claim.LeaseID)
	}
	if claim.CloudID != "" && server.CloudID != claim.CloudID ||
		claim.CloudImmutableID != "" && server.ImmutableID != claim.CloudImmutableID {
		return core.Exit(4, "lease_id_conflict: Proxmox VM for lease %s does not match its bound VMID and vmgenid", claim.LeaseID)
	}
	if attemptNode == "" {
		return core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable source node", claim.LeaseID)
	}
	return nil
}

func (b *leaseBackend) releaseFixed(ctx context.Context, req core.ReleaseLeaseRequest, requireCleanupEligible bool) error {
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		leaseID = proxmoxClaimLabelLeaseID(req.Lease.Server)
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	expected, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !exists || !fixedProxmoxLeaseKind.IsFixedClaim(expected) {
		return core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable ownership claim", leaseID)
	}
	verifyAbsent := func(vmid int) error {
		remaining, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return fmt.Errorf("verify fixed Proxmox release inventory: %w", err)
		}
		for _, candidate := range remaining {
			if candidate.CloudID == strconv.Itoa(vmid) || candidate.Labels["lease"] == leaseID {
				return core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s still has a surviving VM", leaseID)
			}
		}
		present, err := client.VMExistsInCluster(ctx, strconv.Itoa(vmid))
		if err != nil {
			return fmt.Errorf("verify fixed Proxmox VMID %d absence: %w", vmid, err)
		}
		if present {
			return core.Exit(4, "lease_id_conflict: fixed Proxmox VMID %d still exists after release", vmid)
		}
		return nil
	}
	return core.DeleteFixedResource(ctx, fixedProxmoxLeaseKind, expected, core.FixedLeaseOperations[core.Server]{
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.Server], error) {
			claim := tx.Claim
			var result core.FixedObservation[core.Server]
			if strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg)) != claim.ProviderScope || claim.ProviderScope != claim.FixedCreateIntent.ProviderScope {
				return result, core.Exit(4, "lease_id_conflict: fixed Proxmox provider scope changed before release")
			}
			if claim.FixedCreateIntent.State == "released" {
				err := fixedProxmoxLeaseKind.ValidateTerminalClaim(*claim, core.LeaseClaim{}, leaseID, validateFixedProxmoxTerminalClaim)
				return core.FixedObservation[core.Server]{AbsenceProven: err == nil}, err
			}
			vmid, node, err := fixedProxmoxAttempt(*claim)
			if err != nil {
				return result, err
			}
			if vmid == 0 {
				return result, core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable clone attempt", leaseID)
			}
			server, found, err := b.findFixedProxmoxServer(ctx, client, *claim)
			if err != nil {
				return result, err
			}
			if found {
				if err := validateFixedProxmoxLocalBinding(*claim); err != nil {
					return result, err
				}
				if err := validateFixedProxmoxServer(server, *claim, vmid, node); err != nil {
					return result, err
				}
				if claim.CloudImmutableID == "" {
					return result, core.Exit(4, "lease_id_conflict: fixed Proxmox attempt has no bound vmgenid; retain for inspection")
				}
				if requireCleanupEligible {
					if eligible, reason := core.ShouldCleanupServer(server, time.Now().UTC()); !eligible {
						return result, fmt.Errorf("Proxmox VM no longer eligible: %s", reason)
					}
				}
				result.Candidates = []core.Server{server}
				return result, nil
			}
			if claim.FixedCreateIntent.State == "prepared" {
				return result, core.Exit(4, "lease_id_conflict: absence cannot settle an unresolved Proxmox clone attempt")
			}
			err = verifyAbsent(vmid)
			result.AbsenceProven = err == nil
			return result, err
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, server core.Server) error {
			vmid, node, err := fixedProxmoxAttempt(*tx.Claim)
			if err != nil {
				return err
			}
			check := func(live core.Server) error {
				if err := validateFixedProxmoxServer(live, *tx.Claim, vmid, node); err != nil {
					return err
				}
				if requireCleanupEligible {
					if eligible, reason := core.ShouldCleanupServer(live, time.Now().UTC()); !eligible {
						return fmt.Errorf("Proxmox VM %s no longer eligible: %s", live.CloudID, reason)
					}
				}
				return nil
			}
			if err := client.DeleteServerOnNodeChecked(ctx, server.HostID, server.CloudID, check); err != nil {
				return err
			}
			return verifyAbsent(vmid)
		},
	})
}

func validateFixedProxmoxTerminalClaim(claim core.LeaseClaim) error {
	if claim.CloudID == "" || claim.CloudID != strconv.FormatInt(claim.CloudNumericID, 10) ||
		claim.Labels["lease"] != claim.LeaseID || claim.Labels["provider"] != "proxmox" ||
		claim.Labels["fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint {
		return core.Exit(4, "lease_id_conflict: fixed Proxmox lease %s has an invalid terminal VM identity", claim.LeaseID)
	}
	return nil
}

func (b *leaseBackend) RetainLeaseClaimAfterRelease(lease core.LeaseTarget) bool {
	retained, err := b.retainLeaseClaimAfterRelease(lease, core.LeaseClaim{})
	return retained || err != nil
}

func (b *leaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return b.retainLeaseClaimAfterRelease(lease, previous)
}

func (b *leaseBackend) retainLeaseClaimAfterRelease(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	fixedEvidence := strings.TrimSpace(lease.Server.Labels["fixed_intent_sha256"]) != ""
	return fixedProxmoxLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, fixedEvidence, validateFixedProxmoxTerminalClaim, nil)
}

// Another local owner of this VMID makes lifecycle mutations ambiguous, even
// when the remote labels still point at this fixed operation.
func validateFixedProxmoxLocalBinding(claim core.LeaseClaim) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	for _, other := range claims {
		if other.LeaseID == claim.LeaseID || other.CloudID != claim.CloudID {
			continue
		}
		if other.Provider != "proxmox" && other.Provider != core.FixedProxmoxClaimProvider {
			continue
		}
		if other.ProviderScope != "" && other.ProviderScope != claim.ProviderScope {
			continue
		}
		if fixedProxmoxLeaseKind.IsFixedClaim(other) && other.FixedCreateIntent.State == "released" {
			continue
		}
		return core.Exit(4, "lease_id_conflict: multiple local Proxmox claims bind VMID %s", claim.CloudID)
	}
	return nil
}

func (b *leaseBackend) validateFixedCleanupCandidate(claim core.LeaseClaim, server core.Server, inventory []core.Server) error {
	vmid, node, err := fixedProxmoxAttempt(claim)
	if err != nil {
		return err
	}
	if claim.ProviderScope != core.ProviderClaimScope("proxmox", b.Cfg) || claim.CloudImmutableID == "" {
		return core.Exit(4, "lease_id_conflict: fixed Proxmox cleanup has no exact scope/generation binding")
	}
	count := 0
	for _, candidate := range inventory {
		if candidate.CloudID == claim.CloudID || candidate.Labels["lease"] == claim.LeaseID || candidate.Labels["provider_key"] == core.ProviderKeyForLease(claim.LeaseID) {
			count++
		}
	}
	if count != 1 {
		return core.Exit(4, "lease_id_conflict: ambiguous fixed Proxmox cleanup inventory")
	}
	if err := validateFixedProxmoxLocalBinding(claim); err != nil {
		return err
	}
	return validateFixedProxmoxServer(server, claim, vmid, node)
}
