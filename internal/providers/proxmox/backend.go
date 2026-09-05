package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Config = core.Config
type Runtime = core.Runtime
type ProviderSpec = core.ProviderSpec
type Backend = core.Backend
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget

type leaseBackend struct{ shared.DirectSSHBackend }

const proxmoxReleaseAbsentMarker = "proxmox-release-absent"

type proxmoxClient interface {
	DoctorReadiness(context.Context, Config) ([]core.ProxmoxReadinessCheck, error)
	ListCrabboxServers(context.Context) ([]Server, error)
	ListCrabboxServersCluster(context.Context) ([]Server, error)
	CreateServer(context.Context, Config, string, string, string, bool) (Server, error)
	NextVMID(context.Context) (int, error)
	CreateServerWithVMID(context.Context, Config, string, string, string, bool, int, map[string]string) (Server, error)
	GetServer(context.Context, string) (Server, error)
	GetServerOnNode(context.Context, string, string) (Server, error)
	VMExistsInCluster(context.Context, string) (bool, error)
	DeleteServer(context.Context, string) error
	DeleteServerOnNode(context.Context, string, string) error
	DeleteServerOnNodeChecked(context.Context, string, string, func(Server) error) error
	SetLabels(context.Context, string, map[string]string) error
	SetLabelsOnNode(context.Context, string, string, map[string]string) error
}

func NewLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "proxmox"
	if cfg.Proxmox.User != "" {
		cfg.SSHUser = cfg.Proxmox.User
	}
	if cfg.Proxmox.WorkRoot != "" {
		cfg.WorkRoot = cfg.Proxmox.WorkRoot
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true}}
}

func (b *leaseBackend) SupportsRequestedLeaseID() bool { return true }

func (b *leaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if strings.TrimSpace(req.RequestedLeaseID) != "" {
		return b.acquireFixed(ctx, req)
	}
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

const fixedProxmoxCreateIntentVersion = 1

var fixedProxmoxLeaseKind = core.FixedLeaseKind{
	ClaimProvider: core.FixedProxmoxClaimProvider,
	IntentVersion: fixedProxmoxCreateIntentVersion,
	Label:         "Proxmox",
	TerminalIdentityLabels: []string{
		"crabbox", "provider", "lease", "slug", "provider_key",
		"fixed_intent_sha256", "node", "template_id",
	},
}

type fixedProxmoxCreateIntent struct {
	ProviderScope  string `json:"providerScope"`
	Node           string `json:"node"`
	TemplateID     int    `json:"templateId"`
	Storage        string `json:"storage,omitempty"`
	Pool           string `json:"pool,omitempty"`
	Bridge         string `json:"bridge,omitempty"`
	User           string `json:"user"`
	WorkRoot       string `json:"workRoot"`
	FullClone      bool   `json:"fullClone"`
	ServerType     string `json:"serverType"`
	TargetOS       string `json:"targetOS"`
	RequestedSlug  string `json:"requestedSlug,omitempty"`
	Keep           bool   `json:"keep"`
	TTLNanoseconds int64  `json:"ttlNanoseconds"`
	IdleNanos      int64  `json:"idleNanoseconds"`
	SSHPublicKey   string `json:"sshPublicKey"`
}

func fixedProxmoxFingerprint(cfg Config, req AcquireRequest, providerScope, publicKey string) (string, error) {
	data, err := json.Marshal(fixedProxmoxCreateIntent{
		ProviderScope: providerScope, Node: strings.TrimSpace(cfg.Proxmox.Node),
		TemplateID: cfg.Proxmox.TemplateID, Storage: strings.TrimSpace(cfg.Proxmox.Storage),
		Pool: strings.TrimSpace(cfg.Proxmox.Pool), Bridge: strings.TrimSpace(cfg.Proxmox.Bridge),
		User: strings.TrimSpace(cfg.SSHUser), WorkRoot: strings.TrimSpace(cfg.WorkRoot),
		FullClone: cfg.Proxmox.FullClone, ServerType: strings.TrimSpace(cfg.ServerType),
		TargetOS: strings.TrimSpace(cfg.TargetOS), RequestedSlug: core.NormalizeLeaseSlug(req.RequestedSlug),
		Keep: req.Keep, TTLNanoseconds: cfg.TTL.Nanoseconds(), IdleNanos: cfg.IdleTimeout.Nanoseconds(),
		SSHPublicKey: strings.TrimSpace(publicKey),
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint fixed Proxmox create intent: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (b *leaseBackend) acquireFixed(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if b.Cfg.Proxmox.TemplateID <= 0 {
		return LeaseTarget{}, exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	}
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	cfg := b.Cfg
	cfg.ServerType = proxmoxServerTypeForConfig(cfg)
	providerScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", cfg))
	if providerScope == "" {
		return LeaseTarget{}, exit(2, "Proxmox cluster scope is unavailable; refusing fixed lease creation")
	}
	client, err := newClient(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	var publicKey, fingerprint string
	freshClaim := false
	acquired, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: fixedProxmoxLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: cfg.TargetOS,
		WindowsMode: cfg.WindowsMode, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, func(ctx context.Context, _ *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		freshClaim = !exists
		keyPath, key, err := ensureTestboxKeyForConfig(cfg, leaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.SSHKey, publicKey = keyPath, key
		cfg.ProviderKey = providerKeyForLease(leaseID)
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
		binding.Slug, err = allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
		return binding, err
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (LeaseTarget, error) {
		if claim.ProviderScope != providerScope || intent.ProviderScope != providerScope {
			return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Proxmox lease %s provider scope changed", leaseID)
		}
		server, found, err := b.findFixedProxmoxServer(ctx, client, *claim)
		if err != nil {
			return LeaseTarget{}, err
		}
		attemptVMID, attemptNode, attemptErr := fixedProxmoxAttempt(*claim)
		if attemptErr != nil {
			return LeaseTarget{}, attemptErr
		}
		if !found {
			if attemptVMID != 0 {
				exists, err := client.VMExistsInCluster(ctx, strconv.Itoa(attemptVMID))
				if err != nil {
					return LeaseTarget{}, fmt.Errorf("reconcile fixed Proxmox VMID %d: %w", attemptVMID, err)
				}
				if exists {
					return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Proxmox VMID %d exists without matching lease identity", attemptVMID)
				}
				return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Proxmox lease %s has an unresolved clone attempt; retain its claim for recovery", leaseID)
			}
			if !freshClaim {
				return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Proxmox lease %s has no provably unsubmitted attempt; retain its claim", leaseID)
			}
			attemptVMID, err = client.NextVMID(ctx)
			if err != nil {
				return LeaseTarget{}, err
			}
			if attemptVMID <= 0 {
				return LeaseTarget{}, exit(4, "lease_id_conflict: Proxmox selected invalid VMID %d", attemptVMID)
			}
			attemptNode = strings.TrimSpace(cfg.Proxmox.Node)
			intent.Attempt = map[string]string{
				"vmid": strconv.Itoa(attemptVMID), "node": attemptNode,
			}
			labels := fixedProxmoxIdentityLabels(cfg, leaseID, intent.Slug, fingerprint, attemptNode)
			claim.CloudID = strconv.Itoa(attemptVMID)
			claim.CloudNumericID = int64(attemptVMID)
			claim.Labels = maps.Clone(labels)
			if err := persist(); err != nil {
				return LeaseTarget{}, err
			}
			fmt.Fprintf(b.RT.Stderr, "provisioning provider=proxmox lease=%s slug=%s node=%s template=%d vmid=%d keep=%v fixed=true\n",
				leaseID, intent.Slug, cfg.Proxmox.Node, cfg.Proxmox.TemplateID, attemptVMID, req.Keep)
			server, err = client.CreateServerWithVMID(ctx, cfg, publicKey, leaseID, intent.Slug, req.Keep, attemptVMID, labels)
			if err != nil {
				recovered, recoveredFound, reconcileErr := b.findFixedProxmoxServer(ctx, client, *claim)
				if reconcileErr != nil {
					return LeaseTarget{}, errors.Join(err, reconcileErr)
				}
				if !recoveredFound {
					return LeaseTarget{}, err
				}
				server = recovered
			}
		}
		if err := validateFixedProxmoxServer(server, *claim, attemptVMID, attemptNode); err != nil {
			return LeaseTarget{}, err
		}
		if claim.CloudImmutableID == "" {
			claim.CloudNumericID = server.ID
			claim.CloudImmutableID = server.ImmutableID
			claim.Labels = maps.Clone(server.Labels)
			if err := persist(); err != nil {
				return LeaseTarget{}, err
			}
		} else if claim.CloudID != server.CloudID || claim.CloudImmutableID != server.ImmutableID {
			return LeaseTarget{}, exit(4, "lease_id_conflict: fixed Proxmox lease %s resource identity changed", leaseID)
		}
		target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
		if err := waitForSSHReadyFunc(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Labels = maps.Clone(server.Labels)
		server.Labels["state"] = "ready"
		if err := client.SetLabelsOnNode(ctx, server.HostID, server.CloudID, server.Labels); err != nil {
			return LeaseTarget{}, fmt.Errorf("persist Proxmox fixed lease labels: %w", err)
		}
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}, ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(acquired); err != nil {
			return LeaseTarget{}, fmt.Errorf("acknowledge fixed Proxmox acquisition: %w", err)
		}
	}
	return acquired, nil
}

func fixedProxmoxIdentityLabels(cfg Config, leaseID, slug, fingerprint, node string) map[string]string {
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
		(intent.State != "prepared" && intent.State != "acquired") || len(intent.FailedAttempts) != 0 {
		return 0, "", exit(4, "lease_id_conflict: invalid fixed Proxmox create intent for lease %s", claim.LeaseID)
	}
	if len(intent.Attempt) == 0 {
		if claim.CloudID != "" || claim.CloudImmutableID != "" || len(claim.Labels) != 0 {
			return 0, "", exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable clone attempt", claim.LeaseID)
		}
		return 0, "", nil
	}
	vmid, err := strconv.Atoi(intent.Attempt["vmid"])
	node := strings.TrimSpace(intent.Attempt["node"])
	if err != nil || vmid <= 0 || strconv.Itoa(vmid) != intent.Attempt["vmid"] || node == "" || len(intent.Attempt) != 2 {
		return 0, "", exit(4, "lease_id_conflict: invalid fixed Proxmox clone attempt for lease %s", claim.LeaseID)
	}
	if claim.CloudID != strconv.Itoa(vmid) || claim.CloudNumericID != int64(vmid) ||
		claim.ProviderScope != intent.ProviderScope ||
		claim.Labels["crabbox"] != "true" || claim.Labels["provider"] != "proxmox" ||
		claim.Labels["lease"] != claim.LeaseID || claim.Labels["slug"] != claim.Slug ||
		claim.Labels["provider_key"] != core.ProviderKeyForLease(claim.LeaseID) ||
		claim.Labels["fixed_intent_sha256"] != intent.Fingerprint ||
		claim.Labels["node"] != node || claim.Labels["template_id"] == "" ||
		intent.State == "acquired" && claim.CloudImmutableID == "" {
		return 0, "", exit(4, "lease_id_conflict: fixed Proxmox lease %s durable VM identity is inconsistent", claim.LeaseID)
	}
	return vmid, node, nil
}

func (b *leaseBackend) findFixedProxmoxServer(ctx context.Context, client proxmoxClient, claim core.LeaseClaim) (Server, bool, error) {
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return Server{}, false, err
	}
	var found []Server
	for _, server := range servers {
		if strings.TrimSpace(server.Labels["lease"]) == claim.LeaseID {
			found = append(found, server)
		}
	}
	if len(found) > 1 {
		return Server{}, false, exit(4, "lease_id_conflict: multiple Proxmox VMs match fixed lease %s", claim.LeaseID)
	}
	if len(found) == 1 {
		return found[0], true, nil
	}
	return Server{}, false, nil
}

func validateFixedProxmoxServer(server Server, claim core.LeaseClaim, vmid int, attemptNode string) error {
	intent := claim.FixedCreateIntent
	if vmid <= 0 || server.CloudID != strconv.Itoa(vmid) || server.ID != 0 && server.ID != int64(vmid) ||
		server.Provider != "proxmox" || server.HostID == "" || server.ImmutableID == "" ||
		server.Labels["crabbox"] != "true" || server.Labels["provider"] != "proxmox" ||
		server.Labels["lease"] != claim.LeaseID || core.NormalizeLeaseSlug(server.Labels["slug"]) != intent.Slug ||
		server.Labels["provider_key"] != core.ProviderKeyForLease(claim.LeaseID) ||
		server.Labels["fixed_intent_sha256"] != intent.Fingerprint ||
		server.Labels["node"] != attemptNode ||
		server.Labels["template_id"] != claim.Labels["template_id"] {
		return exit(4, "lease_id_conflict: Proxmox VM for lease %s does not match its durable fixed identity", claim.LeaseID)
	}
	if claim.CloudID != "" && server.CloudID != claim.CloudID ||
		claim.CloudImmutableID != "" && server.ImmutableID != claim.CloudImmutableID {
		return exit(4, "lease_id_conflict: Proxmox VM for lease %s does not match its bound VMID and vmgenid", claim.LeaseID)
	}
	if attemptNode == "" {
		return exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable source node", claim.LeaseID)
	}
	return nil
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	if b.Cfg.Proxmox.TemplateID <= 0 {
		return LeaseTarget{}, exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.Cfg
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	cfg.ServerType = proxmoxServerTypeForConfig(cfg)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=proxmox lease=%s slug=%s node=%s template=%d keep=%v\n",
		leaseID, slug, cfg.Proxmox.Node, cfg.Proxmox.TemplateID, keep)
	server, err := client.CreateServer(ctx, cfg, publicKey, leaseID, slug, keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server.PublicNet.IPv4.IP == "" {
		cloudID := server.CloudID
		hostID := server.HostID
		server, err = b.waitForServerIP(ctx, client, cloudID, bootstrapWaitTimeout(cfg))
		if err != nil {
			b.cleanupFailedAcquire(client, Server{CloudID: cloudID, HostID: hostID}, leaseID)
			return LeaseTarget{}, err
		}
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReadyFunc(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		b.cleanupFailedAcquire(client, server, leaseID)
		return LeaseTarget{}, err
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set proxmox labels: %v\n", err)
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s node=%s ip=%s\n", leaseID, server.DisplayID(), cfg.Proxmox.Node, server.PublicNet.IPv4.IP)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) cleanupFailedAcquire(client proxmoxClient, server Server, leaseID string) {
	node := core.Blank(server.HostID, b.Cfg.Proxmox.Node)
	if err := client.DeleteServerOnNode(context.Background(), node, server.CloudID); err != nil && !core.IsProxmoxNotFound(err) {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve failed Proxmox acquire residue lease=%s reason=delete_failed error=%v\n", leaseID, err)
		return
	}
	exists, err := client.VMExistsInCluster(context.Background(), server.CloudID)
	if err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve failed Proxmox acquire residue lease=%s reason=cluster_verification_failed error=%v\n", leaseID, err)
		return
	}
	if exists {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve failed Proxmox acquire residue lease=%s reason=vm_still_exists\n", leaseID)
		return
	}
	removeLocalLeaseResidue(leaseID)
}

func (b *leaseBackend) waitForServerIP(ctx context.Context, client proxmoxClient, cloudID string, timeout time.Duration) (Server, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(proxmoxIPPollInterval)
	defer ticker.Stop()
	result, err := shared.Poll(deadlineCtx, 0, proxmoxIPPollInterval,
		func(ctx context.Context, _ time.Duration) error {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-ticker.C:
				return nil
			}
		},
		func(ctx context.Context) (Server, error) { return client.GetServer(ctx, cloudID) },
		func(_ context.Context, server Server, fetchErr error) (bool, error) {
			return server.PublicNet.IPv4.IP != "", fetchErr
		}, nil)
	if err != nil {
		if result.Err == nil && context.Cause(deadlineCtx) != nil && errors.Is(err, context.Cause(deadlineCtx)) {
			return Server{}, deadlineCtx.Err()
		}
		return Server{}, err
	}
	return result.Value, nil
}

func (b *leaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ID != "" {
		if _, err := strconv.Atoi(req.ID); err == nil || strings.HasPrefix(req.ID, "crabbox-") {
			server, err := client.GetServer(ctx, req.ID)
			if err != nil {
				if !core.IsProxmoxNotFound(err) && !req.ReleaseOnly {
					return LeaseTarget{}, err
				}
			} else {
				if !isCrabboxLease(server) {
					return LeaseTarget{}, exit(4, "lease/server not found: %s (VM exists but is not Crabbox-managed)", req.ID)
				}
				return b.targetForServer(server), nil
			}
		}
	}
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		target := b.targetForServer(server)
		target.LeaseID = leaseID
		return target, nil
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID)
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *leaseBackend) releaseTargetFromClaim(ctx context.Context, client proxmoxClient, id string) (LeaseTarget, error) {
	var (
		claim core.LeaseClaim
		ok    bool
		err   error
	)
	if _, numeric := strconv.ParseInt(strings.TrimSpace(id), 10, 64); numeric == nil {
		claim, ok, err = b.resolveNumericClaim(id)
	} else {
		var exact bool
		claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, "proxmox")
		if err == nil && exact && (!ok || claim.LeaseID != id) {
			return LeaseTarget{}, exit(2, "proxmox exact lease identifier %q does not match a valid Proxmox claim", id)
		}
	}
	if err != nil {
		return LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" || !core.LeaseClaimMatchesIdentifier(claim, id) {
		return LeaseTarget{}, exit(4, "lease/server not found: %s", id)
	}
	cloudID := strings.TrimSpace(claim.CloudID)
	vmid, err := strconv.ParseInt(cloudID, 10, 64)
	if err != nil || vmid <= 0 {
		return LeaseTarget{}, exit(2, "proxmox lease claim has invalid VM identity for lease=%s", claim.LeaseID)
	}
	if server, err := client.GetServer(ctx, cloudID); err == nil {
		if !isCrabboxLease(server) || strings.TrimSpace(server.Labels["lease"]) != claim.LeaseID {
			return LeaseTarget{}, exit(2, "refusing to release Proxmox VM %s from stale local claim lease=%s", cloudID, claim.LeaseID)
		}
		return LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	}
	claimScope := strings.TrimSpace(claim.ProviderScope)
	currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	if claimScope == "" || currentScope == "" || claimScope != currentScope {
		return LeaseTarget{}, exit(2, "refusing to accept missing Proxmox VM %s from lease=%s with unverified cluster scope", cloudID, claim.LeaseID)
	}
	clusterServers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return LeaseTarget{}, fmt.Errorf("locate Proxmox VM %s across cluster: %w", cloudID, err)
	}
	for _, server := range clusterServers {
		if server.CloudID != cloudID {
			continue
		}
		if !isCrabboxLease(server) || strings.TrimSpace(server.Labels["lease"]) != claim.LeaseID {
			return LeaseTarget{}, exit(2, "refusing to release Proxmox VM %s from stale local claim lease=%s", cloudID, claim.LeaseID)
		}
		return LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	}
	exists, err := client.VMExistsInCluster(ctx, cloudID)
	if err != nil {
		return LeaseTarget{}, fmt.Errorf("verify Proxmox VM %s cluster absence: %w", cloudID, err)
	}
	if exists {
		return LeaseTarget{}, exit(2, "refusing to accept missing Proxmox VM %s from lease=%s because it still exists in the cluster", cloudID, claim.LeaseID)
	}
	labels := shared.CloneLabels(claim.Labels)
	if leaseLabel := strings.TrimSpace(labels["lease"]); leaseLabel != "" && leaseLabel != claim.LeaseID {
		return LeaseTarget{}, exit(2, "proxmox lease claim label mismatch for lease=%s", claim.LeaseID)
	}
	if providerLabel := strings.TrimSpace(labels["provider"]); providerLabel != "" && providerLabel != "proxmox" {
		return LeaseTarget{}, exit(2, "proxmox lease claim provider label mismatch for lease=%s", claim.LeaseID)
	}
	labels["lease"] = claim.LeaseID
	labels["provider"] = "proxmox"
	return LeaseTarget{
		LeaseID: claim.LeaseID,
		Server: Server{
			CloudID:  cloudID,
			Provider: "proxmox",
			HostID:   proxmoxReleaseAbsentMarker,
			ID:       vmid,
			Name:     claim.Slug,
			Labels:   labels,
		},
	}, nil
}

func (b *leaseBackend) resolveNumericClaim(cloudID string) (core.LeaseClaim, bool, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	var scoped, legacy []core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != "proxmox" || strings.TrimSpace(claim.CloudID) != cloudID {
			continue
		}
		scope := strings.TrimSpace(claim.ProviderScope)
		switch {
		case scope != "" && scope == currentScope:
			scoped = append(scoped, claim)
		case scope == "":
			legacy = append(legacy, claim)
		}
	}
	if len(scoped) > 1 {
		return core.LeaseClaim{}, false, exit(2, "multiple provider=proxmox claims in the current scope match cloud id %s", cloudID)
	}
	if len(scoped) == 1 {
		return scoped[0], true, nil
	}
	if len(legacy) > 1 {
		return core.LeaseClaim{}, false, exit(2, "multiple unscoped provider=proxmox claims match cloud id %s", cloudID)
	}
	if len(legacy) == 1 {
		return legacy[0], true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func (b *leaseBackend) targetForServer(server Server) LeaseTarget {
	cfg := b.Cfg
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	leaseID := core.Blank(server.Labels["lease"], server.CloudID)
	useStoredTestboxKey(&target, leaseID)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
}

func (b *leaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newClient(b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServersCluster(ctx)
}

func (b *leaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	checks, err := client.DoctorReadiness(ctx, b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.DoctorResult{Provider: "proxmox", Checks: make([]core.DoctorCheck, 0, len(checks))}
	for _, check := range checks {
		result.Checks = append(result.Checks, core.DoctorCheck{
			Status:  check.Status,
			Check:   check.Check,
			Message: check.Message,
			Details: check.Details,
		})
	}
	return result, nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *leaseBackend) ReleaseLeaseWithOutcome(ctx context.Context, req ReleaseLeaseRequest) (core.ReleaseLeaseOutcome, error) {
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		leaseID = proxmoxClaimLabelLeaseID(req.Lease.Server)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.ReleaseLeaseOutcome{}, err
	}
	if exists && fixedProxmoxLeaseKind.IsFixedClaim(claim) {
		if label := proxmoxClaimLabelLeaseID(req.Lease.Server); label != "" && label != leaseID {
			return core.ReleaseLeaseOutcome{}, exit(4, "lease_id_conflict: fixed Proxmox release lease label %s does not match %s", label, leaseID)
		}
		err := b.releaseFixed(ctx, req, false)
		return core.ReleaseLeaseOutcome{Terminal: err == nil}, err
	}
	err = b.releaseOrdinary(ctx, req)
	return core.ReleaseLeaseOutcome{Terminal: err == nil}, err
}

func (b *leaseBackend) releaseOrdinary(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	id := req.Lease.Server.CloudID
	if id == "" {
		id = req.Lease.LeaseID
	}
	leaseID := proxmoxClaimLeaseID(req.Lease.Server, req.Lease.LeaseID)
	if req.Lease.Server.HostID != proxmoxReleaseAbsentMarker {
		if err := b.backfillReleaseClaimScope(leaseID, id, req.Lease.Server); err != nil {
			return err
		}
		node := core.Blank(req.Lease.Server.HostID, b.Cfg.Proxmox.Node)
		if err := client.DeleteServerOnNode(ctx, node, id); err != nil && !core.IsProxmoxNotFound(err) {
			return err
		}
	}
	remaining, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve local lease residue lease=%s reason=inventory_refresh_failed error=%v\n", leaseID, err)
		return fmt.Errorf("reconcile Proxmox lease after release: %w", err)
	}
	deleted := req.Lease.Server
	deleted.CloudID = id
	if deleted.Labels == nil {
		deleted.Labels = map[string]string{}
	}
	if deleted.Labels["lease"] == "" {
		deleted.Labels["lease"] = leaseID
	}
	return removeCleanupLeaseResidue(ctx, client, deleted, remaining, b.Cfg, b.RT.Stderr)
}

func (b *leaseBackend) releaseFixed(ctx context.Context, req ReleaseLeaseRequest, requireCleanupEligible bool) error {
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		leaseID = proxmoxClaimLabelLeaseID(req.Lease.Server)
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	return core.WithDurableLeaseClaimLock(leaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists || !fixedProxmoxLeaseKind.IsFixedClaim(*claim) {
			return exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable ownership claim", leaseID)
		}
		if claim.FixedCreateIntent.State == "released" {
			return fixedProxmoxLeaseKind.ValidateTerminalClaim(*claim, core.LeaseClaim{}, leaseID, validateFixedProxmoxTerminalClaim)
		}
		if strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg)) != claim.FixedCreateIntent.ProviderScope ||
			claim.ProviderScope != claim.FixedCreateIntent.ProviderScope {
			return exit(4, "lease_id_conflict: fixed Proxmox lease %s provider scope changed before release", leaseID)
		}
		vmid, node, err := fixedProxmoxAttempt(*claim)
		if err != nil {
			return err
		}
		if vmid == 0 {
			return exit(4, "lease_id_conflict: fixed Proxmox lease %s has no durable clone attempt", leaseID)
		}
		server, found, err := b.findFixedProxmoxServer(ctx, client, *claim)
		if err != nil {
			return err
		}
		if found {
			if err := validateFixedProxmoxServer(server, *claim, vmid, node); err != nil {
				return err
			}
			if claim.CloudImmutableID == "" {
				claim.CloudNumericID, claim.CloudImmutableID = server.ID, server.ImmutableID
				claim.Labels = maps.Clone(server.Labels)
				if err := persist(); err != nil {
					return err
				}
			}
			check := func(live Server) error {
				if err := validateFixedProxmoxServer(live, *claim, vmid, node); err != nil {
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
		}
		remaining, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return fmt.Errorf("verify fixed Proxmox release inventory: %w", err)
		}
		for _, candidate := range remaining {
			if candidate.CloudID == strconv.Itoa(vmid) || candidate.Labels["lease"] == leaseID {
				return exit(4, "lease_id_conflict: fixed Proxmox lease %s still has a surviving VM", leaseID)
			}
		}
		present, err := client.VMExistsInCluster(ctx, strconv.Itoa(vmid))
		if err != nil {
			return fmt.Errorf("verify fixed Proxmox VMID %d absence: %w", vmid, err)
		}
		if present {
			return exit(4, "lease_id_conflict: fixed Proxmox VMID %d still exists after release", vmid)
		}
		if len(claim.Labels) == 0 {
			claim.Labels = fixedProxmoxIdentityLabels(b.Cfg, claim.LeaseID, claim.Slug, claim.FixedCreateIntent.Fingerprint, node)
		}
		*claim = fixedProxmoxLeaseKind.TerminalClaim(*claim, time.Now().UTC())
		return persist()
	})
}

func validateFixedProxmoxTerminalClaim(claim core.LeaseClaim) error {
	if claim.CloudID == "" || claim.CloudID != strconv.FormatInt(claim.CloudNumericID, 10) ||
		claim.Labels["lease"] != claim.LeaseID || claim.Labels["provider"] != "proxmox" ||
		claim.Labels["fixed_intent_sha256"] != claim.FixedCreateIntent.Fingerprint {
		return exit(4, "lease_id_conflict: fixed Proxmox lease %s has an invalid terminal VM identity", claim.LeaseID)
	}
	return nil
}

func (b *leaseBackend) RetainLeaseClaimAfterRelease(lease LeaseTarget) bool {
	retained, err := b.retainLeaseClaimAfterRelease(lease, core.LeaseClaim{})
	return retained || err != nil
}

func (b *leaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return b.retainLeaseClaimAfterRelease(lease, previous)
}

func (b *leaseBackend) retainLeaseClaimAfterRelease(lease LeaseTarget, previous core.LeaseClaim) (bool, error) {
	fixedEvidence := strings.TrimSpace(lease.Server.Labels["fixed_intent_sha256"]) != ""
	return fixedProxmoxLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, fixedEvidence, validateFixedProxmoxTerminalClaim, nil)
}

func (b *leaseBackend) backfillReleaseClaimScope(leaseID, cloudID string, server Server) error {
	if leaseID == "" || proxmoxClaimLabelLeaseID(server) != leaseID {
		return nil
	}
	claim, found, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !found || strings.TrimSpace(claim.ProviderScope) != "" {
		return nil
	}
	if claim.Provider != "proxmox" || (claim.CloudID != "" && claim.CloudID != cloudID) {
		return nil
	}
	scope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	if scope == "" {
		return exit(2, "cannot safely release legacy Proxmox claim lease=%s without configured cluster scope", leaseID)
	}
	replacement := claim
	replacement.ProviderScope = scope
	if replacement.CloudID == "" {
		replacement.CloudID = cloudID
	}
	return core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, replacement)
}

func (b *leaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	node := core.Blank(server.HostID, b.Cfg.Proxmox.Node)
	if err := client.SetLabelsOnNode(ctx, node, server.CloudID, server.Labels); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return err
	}
	for _, server := range servers {
		var fixedClaim core.LeaseClaim
		for _, claim := range claims {
			if claim.LeaseID == proxmoxClaimLabelLeaseID(server) && fixedProxmoxLeaseKind.IsFixedClaim(claim) {
				fixedClaim = claim
				break
			}
		}
		if fixedClaim.LeaseID != "" {
			if eligible, reason := core.ShouldCleanupServer(server, time.Now().UTC()); !eligible {
				fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
				continue
			}
			if req.DryRun {
				fmt.Fprintf(b.RT.Stderr, "would delete server id=%s name=%s\n", server.DisplayID(), server.Name)
				continue
			}
			if err := b.releaseFixed(ctx, ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: fixedClaim.LeaseID, Server: server}}, true); err != nil {
				return err
			}
			fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s fixed=true key_retained=true\n", server.DisplayID(), server.Name)
			continue
		}
		claim, binding, err := b.cleanupClaim(server, servers, claims)
		if err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", server.DisplayID(), server.Name, err)
			continue
		}
		if eligible, reason := core.ShouldCleanupServer(server, time.Now().UTC()); !eligible {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.RT.Stderr, "would delete server id=%s name=%s\n", server.DisplayID(), server.Name)
			continue
		}
		if err := b.cleanupClaimedServer(ctx, client, server, claim, binding); err != nil {
			return err
		}
		// Acquisition creates keys before publishing claims; this claim lock
		// cannot fence a concurrent key creator, so cleanup keeps local keys.
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s key_retained=true\n", server.DisplayID(), server.Name)
	}
	return nil
}

func (b *leaseBackend) cleanupClaimedServer(ctx context.Context, client proxmoxClient, server Server, claim core.LeaseClaim, binding shared.ClaimBinding) error {
	var deleteErr error
	err := shared.RemoveExactClaimAfter(claim, binding, func() error {
		// Inventory is discovery only. Revalidate its node, lifecycle and native
		// generation while the same claim revision is fenced through removal.
		currentClaims, err := core.ListLeaseClaims()
		if err != nil {
			return err
		}
		current, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return err
		}
		var fresh Server
		for _, candidate := range current {
			if candidate.CloudID == server.CloudID {
				fresh = candidate
				break
			}
		}
		if fresh.CloudID == "" {
			return fmt.Errorf("Proxmox VM %s disappeared during cleanup; claim retained", server.CloudID)
		}
		if _, _, err := b.cleanupClaim(fresh, current, currentClaims); err != nil {
			return err
		}
		check := func(live Server) error {
			if live.HostID != fresh.HostID {
				return fmt.Errorf("Proxmox VM %s changed node during cleanup", server.CloudID)
			}
			if err := validateCleanupServer(live, claim, binding); err != nil {
				return err
			}
			if eligible, reason := core.ShouldCleanupServer(live, time.Now().UTC()); !eligible {
				return fmt.Errorf("Proxmox VM %s no longer eligible: %s", server.CloudID, reason)
			}
			return nil
		}
		if err := check(fresh); err != nil {
			return err
		}
		deleteErr = client.DeleteServerOnNodeChecked(ctx, fresh.HostID, fresh.CloudID, check)
		if deleteErr != nil {
			// Only an accepted/ambiguous purge may be reconciled as success;
			// failed authorization or stop must preserve the original claim.
			if !core.IsProxmoxDeleteTaskError(deleteErr) && !core.IsProxmoxDeleteRequestError(deleteErr) {
				return deleteErr
			}
			if err := waitForProxmoxCleanupAbsence(ctx, client, fresh.CloudID); err != nil {
				return err
			}
		}
		remaining, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return fmt.Errorf("verify Proxmox cleanup inventory: %w", err)
		}
		for _, survivor := range remaining {
			if survivor.CloudID == fresh.CloudID || proxmoxClaimLabelLeaseID(survivor) == claim.LeaseID {
				return fmt.Errorf("Proxmox lease %s still has a surviving VM; claim retained", claim.LeaseID)
			}
		}
		// The filtered Crabbox inventory cannot prove a VMID is absent.
		exists, err := client.VMExistsInCluster(ctx, fresh.CloudID)
		if err != nil {
			return fmt.Errorf("verify Proxmox cleanup absence: %w", err)
		}
		if exists {
			return fmt.Errorf("Proxmox VM %s still exists in cluster; claim retained", fresh.CloudID)
		}
		return nil
	})
	return errors.Join(deleteErr, err)
}

func waitForProxmoxCleanupAbsence(ctx context.Context, client proxmoxClient, cloudID string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, proxmoxDeleteVerifyTimeout)
	defer cancel()
	_, err := shared.Poll(verifyCtx, 0, proxmoxDeleteVerifyPollInterval, shared.SleepContext,
		func(ctx context.Context) (bool, error) { return client.VMExistsInCluster(ctx, cloudID) },
		func(_ context.Context, exists bool, err error) (bool, error) { return !exists, err }, nil)
	return err
}

func removeCleanupLeaseResidue(ctx context.Context, client proxmoxClient, deleted Server, inventory []Server, cfg Config, stderr io.Writer) error {
	leaseID := proxmoxClaimLabelLeaseID(deleted)
	if leaseID == "" {
		return nil
	}
	missingCloudIDs := map[string]bool{deleted.CloudID: true}
	var survivors []Server
	for _, server := range inventory {
		if proxmoxClaimLabelLeaseID(server) == leaseID {
			survivors = append(survivors, server)
		}
	}
	claim, found, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_read_failed error=%v\n", leaseID, err)
		return nil
	}
	if found && claim.Provider == "proxmox" {
		claimScope := strings.TrimSpace(claim.ProviderScope)
		currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", cfg))
		if claimScope != currentScope && (claimScope != "" || currentScope != "") {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_scope_mismatch\n", leaseID)
			return nil
		}
	}
	if len(survivors) == 1 {
		node := core.Blank(survivors[0].HostID, cfg.Proxmox.Node)
		verified, err := client.GetServerOnNode(ctx, node, survivors[0].CloudID)
		if err == nil {
			if proxmoxClaimLabelLeaseID(verified) != leaseID {
				fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=survivor_ownership_unverified\n", leaseID)
				return fmt.Errorf("Proxmox lease %s has an unverified surviving VM", leaseID)
			}
			survivors[0] = verified
		} else if core.IsProxmoxNotFound(err) {
			missingCloudIDs[survivors[0].CloudID] = true
			survivors = nil
		} else {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=survivor_verification_failed error=%v\n", leaseID, err)
			return fmt.Errorf("verify surviving Proxmox VM for lease %s: %w", leaseID, err)
		}
	}
	if len(survivors) > 0 {
		if found && len(survivors) == 1 && claim.Provider == "proxmox" {
			canRetarget := claim.CloudID == "" || claim.CloudID == deleted.CloudID || claim.CloudID == survivors[0].CloudID
			if !canRetarget {
				if exists, err := client.VMExistsInCluster(ctx, claim.CloudID); err == nil && !exists {
					canRetarget = true
				} else if err != nil {
					fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_verification_failed error=%v\n", leaseID, err)
					return fmt.Errorf("verify claimed Proxmox VM for lease %s: %w", leaseID, err)
				}
			}
			if canRetarget {
				target := sshTargetFromConfig(cfg, survivors[0].PublicNet.IPv4.IP)
				if target.Port == "" && claim.SSHPort > 0 {
					target.Port = strconv.Itoa(claim.SSHPort)
				}
				if _, err := core.ReplaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, claim, survivors[0], target); err != nil {
					fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_retarget_failed error=%v\n", leaseID, err)
					return fmt.Errorf("retarget Proxmox lease %s to surviving VM: %w", leaseID, err)
				}
			}
		}
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=duplicate_remote_lease_label\n", leaseID)
		return fmt.Errorf("Proxmox lease %s still has %d surviving VM(s)", leaseID, len(survivors))
	}
	if !found {
		removeStoredTestboxKey(leaseID)
		return nil
	}
	if claim.Provider != "proxmox" {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_mismatch\n", leaseID)
		return nil
	}
	if claim.CloudID != "" && !missingCloudIDs[claim.CloudID] {
		if exists, err := client.VMExistsInCluster(ctx, claim.CloudID); err == nil && exists {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_still_exists\n", leaseID)
			return nil
		} else if err == nil {
			missingCloudIDs[claim.CloudID] = true
		} else {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_verification_failed error=%v\n", leaseID, err)
			return nil
		}
	}
	if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_changed error=%v\n", leaseID, err)
		return nil
	}
	removeStoredTestboxKey(leaseID)
	return nil
}

func proxmoxClaimLeaseID(server Server, fallback string) string {
	if leaseID := proxmoxClaimLabelLeaseID(server); leaseID != "" {
		return leaseID
	}
	return strings.TrimSpace(fallback)
}

func proxmoxClaimLabelLeaseID(server Server) string {
	if server.Labels != nil {
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			return leaseID
		}
	}
	return ""
}

var newClient = func(cfg Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }

func newLeaseID() string { return core.NewLeaseID() }
func allocateDirectLeaseSlug(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
func providerKeyForLease(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
func proxmoxServerTypeForConfig(cfg Config) string {
	return core.ProxmoxServerTypeForConfig(cfg)
}
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

var waitForSSHReadyFunc = waitForSSHReady

var proxmoxIPPollInterval = 2 * time.Second
var proxmoxDeleteVerifyPollInterval = time.Second
var proxmoxDeleteVerifyTimeout = 30 * time.Second

func bootstrapWaitTimeout(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}
func isCrabboxLease(server Server) bool { return core.IsCrabboxProxmoxLease(server) }
func removeLeaseClaim(leaseID string)   { core.RemoveLeaseClaim(leaseID) }
func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
}

func removeLocalLeaseResidue(leaseID string) {
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
}
func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	shared.UseStoredTestboxKey(target, leaseID)
}
