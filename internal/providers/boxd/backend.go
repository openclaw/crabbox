package boxd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	boxdRollbackTimeout = 60 * time.Second
	// A boxd machine boots in seconds, but the control plane is eventually
	// consistent: the create can return before reads see the machine, and the
	// per-machine SSH port is allocated asynchronously. The poll tolerates
	// not-found within this grace before treating the machine as lost.
	boxdNotFoundGrace    = 60 * time.Second
	boxdReadyTimeout     = 5 * time.Minute
	boxdReadyPollDefault = 2 * time.Second
)

var waitForBoxdSSHReady = core.WaitForSSHReady

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime

	// Test seams.
	sshConfigPath        string
	knownHostsPath       string
	readyPollInterval    time.Duration
	readyTimeout         time.Duration
	rollbackTimeout      time.Duration
	releaseNotFoundGrace time.Duration

	// replacedReleases marks leases whose release reaped a stale claim
	// without touching a live replacement, keyed by lease id.
	replacedMu       sync.Mutex
	replacedReleases map[string]bool
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) *backend {
	return &backend{
		spec:                 spec,
		cfg:                  cfg,
		rt:                   rt,
		sshConfigPath:        defaultSSHConfigPath(),
		knownHostsPath:       defaultKnownHostsPath(),
		readyPollInterval:    boxdReadyPollDefault,
		readyTimeout:         boxdReadyTimeout,
		rollbackTimeout:      boxdRollbackTimeout,
		releaseNotFoundGrace: boxdNotFoundGrace,
	}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) configForRun() core.Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	leaseID := core.NewLeaseID()
	rows, err := b.listMachines(ctx, cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claims, err := boxdClaims(cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(rows))
	for _, row := range rows {
		if leaseIDFromMachineName(row.Name) == "" {
			continue
		}
		servers = append(servers, b.server(row.Name, row.Status, row.URL, cfg, claims))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	name := machineNameForLease(leaseID)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, b.now())

	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s machine=%s keep=%v\n", providerName, leaseID, slug, name, req.Keep)
	created, err := b.createMachine(ctx, cfg, name)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	rollback := func(cause error) error {
		if req.Keep {
			// The user asked to keep the machine on failure: persist a claim so
			// List/Cleanup can still manage it (the local claim is the sole
			// ownership anchor — boxd has no server-side labels).
			if claimErr := b.persistRecoveryClaim(cfg, leaseID, slug, name, created.ID, labels, req); claimErr != nil {
				return errors.Join(cause, fmt.Errorf("persist kept boxd machine claim: %w", claimErr))
			}
			return cause
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.rollbackTimeout)
		defer cancel()
		// Never remove by reusable name alone, even here: verify the live
		// machine is the one THIS acquire created before the rollback destroy.
		// When identity cannot be verified (id-less create response, blank
		// read, transport failure), retain a recovery claim for id-fenced
		// cleanup instead of destroying.
		if createdID := strings.TrimSpace(created.ID); createdID != "" {
			summary, found, getErr := b.getMachine(cleanupCtx, cfg, name)
			if getErr == nil && !found {
				return cause // already gone; nothing to roll back
			}
			if getErr == nil && found {
				currentID := strings.TrimSpace(summary.ID)
				if currentID == createdID {
					if destroyErr := b.destroyMachine(cleanupCtx, cfg, name); destroyErr != nil {
						claimErr := b.persistRecoveryClaim(cfg, leaseID, slug, name, created.ID, labels, req)
						return errors.Join(cause, fmt.Errorf("destroy leaked boxd machine %s: %w", name, destroyErr), claimErr)
					}
					return cause
				}
				if currentID != "" {
					// The name now belongs to a replacement; ours is gone.
					fmt.Fprintf(b.rt.Stderr, "boxd machine %s was replaced during rollback (created id=%s, current=%s); leaving the current machine untouched\n", name, createdID, currentID)
					return cause
				}
			}
		}
		if claimErr := b.persistRecoveryClaim(cfg, leaseID, slug, name, created.ID, labels, req); claimErr != nil {
			return errors.Join(cause, fmt.Errorf("persist boxd rollback recovery claim: %w", claimErr))
		}
		return errors.Join(cause, fmt.Errorf("boxd machine %s identity could not be verified during rollback; retained a recovery claim — inspect and remove the machine via the boxd CLI, then `crabbox cleanup` reaps the claim", name))
	}

	// The machine id is the anchor of the replacement fence (boxd retains
	// destroyed names for reuse): a claim persisted without a verified id
	// would leave that lease with no protection against later name reuse.
	// Fail closed — the rollback destroys the machine we just created.
	if strings.TrimSpace(created.ID) == "" {
		return core.LeaseTarget{}, rollback(core.Exit(5, "boxd machine new %s response did not include a machine id; refusing to lease without a replacement fence", name))
	}

	zone := firstNonBlank(zoneFromHost(created.URL, name), defaultBoxdZone)
	labels["machine"] = name
	labels["vm_id"] = created.ID
	labels["zone"] = zone
	// Persist the release intent with the claim (the morph/coder convention):
	// a lease acquired with --boxd-delete-on-release=false keeps its
	// stop-on-release semantics in a later process that did not pass the flag.
	labels["release"] = releaseAction(cfg)

	if req.OnAcquired != nil {
		observed := core.Server{CloudID: created.ID, Provider: providerName, Name: name, Labels: labels}
		if err := req.OnAcquired(core.LeaseTarget{LeaseID: leaseID, Server: observed}); err != nil {
			return core.LeaseTarget{}, rollback(err)
		}
	}

	ready, err := b.waitMachineReady(ctx, cfg, name)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	// Verify the id before it anchors the claim: the ready machine must be
	// the one the create returned, not a same-named machine that raced in.
	// Do NOT roll back here — rollback destroys by name, which would hit the
	// replacement; our machine is necessarily gone if the name resolves to a
	// different id, and the replacement is not ours to touch.
	if readyID := strings.TrimSpace(ready.ID); readyID == "" {
		// A blank id cannot prove the ready machine is ours: fail closed. The
		// rollback verifies identity itself before any destroy.
		return core.LeaseTarget{}, rollback(core.Exit(5, "boxd machine get %s response did not include a machine id; cannot verify the created machine's identity", name))
	} else if readyID != strings.TrimSpace(created.ID) {
		return core.LeaseTarget{}, core.Exit(4, "boxd machine %s changed identity during provisioning (created id=%s, current=%s); leaving the current machine untouched", name, created.ID, ready.ID)
	}
	target, err := b.resolveSSHTarget(ctx, cfg, name, zone)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	if err := waitForBoxdSSHReady(ctx, &target, b.rt.Stderr, "boxd machine ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}

	labels["state"] = "ready"
	server := b.serverFromLabels(name, created.ID, "ready", cfg, labels)
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	if err := claimAcquiredLease(leaseID, slug, cfg, lease, req.Repo.Root, req.Reclaim); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s machine=%s state=ready\n", leaseID, name)
	return lease, nil
}

// claimAcquiredLease persists the ownership claim for a freshly acquired
// machine. boxd exposes no server-side labels, so this LOCAL claim is the sole
// anchor List/Release/Cleanup use to prove a crabbox-named machine is ours —
// it must be written on every successful acquire, repo or not.
func claimAcquiredLease(leaseID, slug string, cfg core.Config, lease core.LeaseTarget, repoRoot string, reclaim bool) error {
	if strings.TrimSpace(repoRoot) != "" {
		return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, repoRoot, cfg.IdleTimeout, reclaim)
	}
	return core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, lease.Server, lease.SSH, cfg.IdleTimeout)
}

func (b *backend) persistRecoveryClaim(cfg core.Config, leaseID, slug, name, vmID string, labels map[string]string, req core.AcquireRequest) error {
	recovery := make(map[string]string, len(labels)+2)
	for key, value := range labels {
		recovery[key] = value
	}
	recovery["machine"] = name
	if vmID != "" {
		recovery["vm_id"] = vmID
	}
	recovery["recovery"] = "rollback-cleanup"
	if req.Keep {
		recovery["recovery"] = "kept-after-failure"
	}
	recovery["state"] = "provisioning"
	server := core.Server{CloudID: vmID, Provider: providerName, Name: firstNonBlank(slug, name), Labels: recovery}
	lease := core.LeaseTarget{Server: server, LeaseID: leaseID}
	return claimAcquiredLease(leaseID, slug, cfg, lease, req.Repo.Root, req.Reclaim)
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	name, leaseID, claim, claimed, err := b.resolveMachineIdentity(req.ID, cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	// Machines are never ADOPTED by name: resolving an unclaimed identity and
	// then writing a repo claim for it would launder a same-account machine
	// that merely follows the naming convention into "locally owned", making
	// it a later destructive-release target. Every lease this install created
	// has a local claim (acquire writes one on success and on every recovery
	// path), so an unclaimed identity is simply not ours.
	if !claimed {
		return core.LeaseTarget{}, core.Exit(4, "lease %s not found for provider=%s on this crabbox install (no local claim; machines are never adopted by name)", req.ID, providerName)
	}
	summary, found, err := b.getMachine(ctx, cfg, name)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !found {
		return core.LeaseTarget{}, core.Exit(4, "lease %s not found for provider=%s (machine %s does not exist)", req.ID, providerName, name)
	}
	// boxd retains destroyed machine names for reuse: bind on the immutable
	// machine id so a stale claim can never hand this lease a replacement
	// machine of the same name. An UNBOUND claim (an id-less create-failure
	// recovery) or a blank live read proves nothing, so resolving through it
	// could start a replacement, hand out its SSH target, and refresh the
	// claim with the replacement's identity — fail closed instead. The
	// read-only release/status view below stays reachable so release and
	// cleanup can still surface the claim.
	boundID := claimBoundMachineID(claim, name)
	currentID := strings.TrimSpace(summary.ID)
	identityErr := func() error {
		if boundID == "" || currentID == "" {
			return core.Exit(4, "lease %s machine %s identity cannot be verified (claimed vm_id=%q, current=%q); act on it via the boxd CLI if intended", req.ID, name, boundID, currentID)
		}
		if currentID != boundID {
			return core.Exit(4, "lease %s machine %s was replaced (claimed vm_id=%s, current=%s); refusing to touch the replacement", req.ID, name, boundID, currentID)
		}
		return nil
	}()
	labels := b.labelsFor(name, summary.ID, summary.Status, cfg, claim, claimed)
	server := b.serverFromLabels(name, summary.ID, boxdState(summary.Status), cfg, labels)
	if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
		return core.LeaseTarget{LeaseID: leaseID, Server: server}, nil
	}
	if identityErr != nil {
		return core.LeaseTarget{}, identityErr
	}
	if leaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "boxd machine %s has no crabbox lease id", name)
	}
	if boxdState(summary.Status) == "stopped" {
		fmt.Fprintf(b.rt.Stderr, "starting stopped boxd machine %s\n", name)
		if err := b.startMachine(ctx, cfg, name); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if _, err := b.waitMachineReady(ctx, cfg, name); err != nil {
		return core.LeaseTarget{}, err
	}
	zone := firstNonBlank(labels["zone"], zoneFromHost(summary.URL, name), defaultBoxdZone)
	target, err := b.resolveSSHTarget(ctx, cfg, name, zone)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !req.StatusOnly {
		if err := waitForBoxdSSHReady(ctx, &target, b.rt.Stderr, "boxd machine ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	server.Status = "ready"
	server.Labels["state"] = "ready"
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	if req.Repo.Root != "" {
		slug := firstNonBlank(server.Labels["slug"], claim.Slug)
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	rows, err := b.listMachines(ctx, cfg)
	if err != nil {
		return nil, err
	}
	claims, err := boxdClaims(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(rows))
	for _, row := range rows {
		leaseID := leaseIDFromMachineName(row.Name)
		if leaseID == "" {
			continue
		}
		// boxd has no server-side ownership labels, so the crabbox- name prefix
		// alone only proves the naming convention. Require the matching local
		// claim before surfacing a machine as one of our leases.
		if _, ok := claims[leaseID]; !ok {
			continue
		}
		out = append(out, b.server(row.Name, row.Status, row.URL, cfg, claims))
	}
	return out, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	if result, err := b.boxd(ctx, cfg, []string{"version"}, nil); err != nil {
		return core.DoctorResult{}, commandError("boxd version", result, err)
	}
	views, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.CLIDoctorResult(providerName, len(views), "boxd"), nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	cfg := b.configForRun()
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	name := strings.TrimSpace(req.Lease.Server.Labels["machine"])
	if name == "" {
		var err error
		name, leaseID, _, _, err = b.resolveMachineIdentity(firstNonBlank(leaseID, req.Lease.Server.CloudID, req.Lease.Server.Name), cfg)
		if err != nil {
			return err
		}
	}
	if leaseID == "" {
		if leaseID = leaseIDFromMachineName(name); leaseID == "" {
			return core.Exit(4, "refusing to release boxd machine %s without a crabbox lease id", name)
		}
	}
	// The local claim is the ownership fence for every destructive boxd
	// operation (boxd has no server-side labels). A canonical machine name or
	// lease id alone must never authorize a stop or destroy: a machine in the
	// same boxd account that merely follows the naming convention — for
	// example one managed by a different crabbox install — is not ours to
	// touch. Cleanup enforces this; direct release must too.
	claim, claimed, err := resolveBoxdClaim(leaseID, cfg)
	if err != nil {
		return err
	}
	if !claimed {
		return core.Exit(4, "refusing to release boxd machine %s: no local claim for lease %s on this crabbox install", name, leaseID)
	}
	if claimedMachine := strings.TrimSpace(claim.Labels["machine"]); claimedMachine != "" && claimedMachine != name {
		return core.Exit(4, "refusing to release boxd machine %s: local claim for lease %s names machine %s", name, leaseID, claimedMachine)
	}
	// boxd retains destroyed machine names for reuse, so the name alone cannot
	// prove the live machine is the claimed one: bind on the immutable machine
	// id the claim recorded. On mismatch the CLAIMED machine is provably gone
	// — reap the stale claim and leave the replacement untouched.
	boundID := claimBoundMachineID(claim, name)
	releaseReplaced := func(currentID string) error {
		fmt.Fprintf(b.rt.Stderr, "boxd machine %s was replaced (claimed vm_id=%s, current=%s); leaving the current machine untouched\n", name, boundID, currentID)
		core.RemoveLeaseClaim(leaseID)
		b.recordReplacedRelease(leaseID)
		return nil
	}
	if !deleteOnRelease(req.Lease, cfg) {
		// Keep mode: stop the machine (disk persists; a later resolve restarts
		// it) and retain the claim as the ownership anchor.
		summary, found, err := b.getMachine(ctx, cfg, name)
		if err != nil {
			return err
		}
		if found {
			if boundID == "" || strings.TrimSpace(summary.ID) == "" {
				return core.Exit(4, "refusing to stop boxd machine %s: cannot verify identity (claimed vm_id=%q, current=%q); act on it via the boxd CLI if intended", name, boundID, summary.ID)
			}
			if strings.TrimSpace(summary.ID) != boundID {
				return releaseReplaced(summary.ID)
			}
			if boxdState(summary.Status) != "stopped" {
				if err := b.stopMachine(ctx, cfg, name); err != nil {
					return err
				}
			}
		}
		labels := make(map[string]string, len(claim.Labels)+2)
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels["state"] = "stopped"
		labels["release"] = "stop"
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
			return err
		}
		return nil
	}
	// Delete mode. Distinguish "provably gone" from "not visible yet": the
	// control plane is eventually consistent, and reaping the claim on a
	// transient inventory gap would orphan a LIVE machine (the claim is the
	// sole anchor, so List and Cleanup would never see it again). The machine
	// must stay absent through the same visibility grace acquire uses before
	// it counts as provably gone; an interrupted wait keeps the claim so a
	// retry can finish the release.
	start := b.now()
	for {
		summary, found, err := b.getMachine(ctx, cfg, name)
		if err != nil {
			return err
		}
		if found {
			if boundID == "" || strings.TrimSpace(summary.ID) == "" {
				return core.Exit(4, "refusing to destroy boxd machine %s: cannot verify identity (claimed vm_id=%q, current=%q); remove it via the boxd CLI if intended", name, boundID, summary.ID)
			}
			if strings.TrimSpace(summary.ID) != boundID {
				return releaseReplaced(summary.ID)
			}
			if err := b.destroyMachine(ctx, cfg, name); err != nil {
				return err
			}
			break
		}
		if b.now().Sub(start) >= b.releaseNotFoundGrace {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.readyPollInterval):
		}
	}
	core.RemoveLeaseClaim(leaseID)
	return nil
}

// claimBoundMachineID returns the immutable boxd machine id (vm uuid) the
// claim recorded at acquire time, or "" when the claim carries no id binding.
// The CloudID fallback can be the machine name itself, which carries no id
// authority.
func claimBoundMachineID(claim core.LeaseClaim, name string) string {
	id := strings.TrimSpace(firstNonBlank(claim.Labels["vm_id"], claim.CloudID))
	if id == "" || id == name {
		return ""
	}
	return id
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	name := firstNonBlank(lease.Server.Labels["machine"], lease.Server.Name)
	if b.wasReplacedRelease(lease.LeaseID) {
		return fmt.Sprintf("reaped stale claim lease=%s machine=%s (machine was replaced; left untouched)", lease.LeaseID, name)
	}
	if deleteOnRelease(lease, b.configForRun()) {
		return fmt.Sprintf("destroyed lease=%s machine=%s", lease.LeaseID, name)
	}
	return fmt.Sprintf("stopped lease=%s machine=%s retained=true", lease.LeaseID, name)
}

// recordReplacedRelease remembers, for this backend instance, that a release
// reaped a stale claim without touching the live replacement machine, so the
// user-facing release message never claims a destroy that did not happen.
func (b *backend) recordReplacedRelease(leaseID string) {
	b.replacedMu.Lock()
	defer b.replacedMu.Unlock()
	if b.replacedReleases == nil {
		b.replacedReleases = map[string]bool{}
	}
	b.replacedReleases[leaseID] = true
}

func (b *backend) wasReplacedRelease(leaseID string) bool {
	b.replacedMu.Lock()
	defer b.replacedMu.Unlock()
	return b.replacedReleases[leaseID]
}

func (b *backend) RetainLeaseClaimAfterRelease(lease core.LeaseTarget) bool {
	return !deleteOnRelease(lease, b.configForRun())
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	cfg := b.configForRun()
	now := b.now()
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, now)
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID != "" {
		claim, ok, err := resolveBoxdClaim(leaseID, cfg)
		if err != nil {
			return core.Server{}, err
		}
		idleTimeout := req.IdleTimeout
		if idleTimeout <= 0 {
			idleTimeout = cfg.IdleTimeout
		}
		// Never blank a stored slug: a lease target whose labels lost the slug
		// would otherwise wipe it from the claim on every idle keepalive.
		slug := firstNonBlank(server.Labels["slug"], claim.Slug)
		if ok {
			if claim.RepoRoot != "" {
				_, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, req.Lease.SSH, claim.RepoRoot, idleTimeout, false, claim, true)
			} else {
				_, err = core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, slug, cfg, server, req.Lease.SSH, idleTimeout, claim, true)
			}
		} else {
			err = core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, req.Lease.SSH, idleTimeout)
		}
		if err != nil {
			return core.Server{}, err
		}
	}
	return server, nil
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	rows, err := b.listMachines(ctx, cfg)
	if err != nil {
		return err
	}
	claims, err := boxdClaims(cfg)
	if err != nil {
		return err
	}
	live := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		leaseID := leaseIDFromMachineName(row.Name)
		if leaseID == "" {
			continue
		}
		claim, ok := claims[leaseID]
		// boxd has no server-side ownership labels: a crabbox-named machine
		// with no matching local claim cannot be proven ours. Never delete it.
		if !ok || claim.LeaseID == "" {
			fmt.Fprintf(b.rt.Stderr, "skip machine=%s reason=no local claim for lease %s\n", row.Name, leaseID)
			continue
		}
		// boxd retains destroyed machine names for reuse, and `machine list`
		// carries no ids: corroborate the claim's immutable machine id via
		// `machine get` before any destructive decision. On mismatch the
		// CLAIMED machine is gone and the live one is a foreign replacement —
		// leave it untouched and let the orphan pass reap the stale claim.
		summary, found, err := b.getMachine(ctx, cfg, row.Name)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		boundID := claimBoundMachineID(claim, row.Name)
		if boundID == "" || strings.TrimSpace(summary.ID) == "" {
			// Destructive cleanup requires a verified identity. An unbound
			// claim (id-less create recovery) or a blank read proves nothing;
			// keep both the claim and the machine and leave the decision to
			// the operator via the boxd CLI.
			fmt.Fprintf(b.rt.Stderr, "skip machine=%s reason=cannot verify identity (claimed vm_id=%q, current=%q); act on it via the boxd CLI if intended\n", row.Name, boundID, summary.ID)
			live[leaseID] = struct{}{}
			continue
		}
		if strings.TrimSpace(summary.ID) != boundID {
			fmt.Fprintf(b.rt.Stderr, "skip machine=%s reason=claimed machine was replaced (claimed vm_id=%s, current=%s)\n", row.Name, boundID, summary.ID)
			continue
		}
		live[leaseID] = struct{}{}
		server := b.server(row.Name, row.Status, row.URL, cfg, claims)
		remove, reason := core.ShouldCleanupServer(server, b.now())
		if recoveryRemove, recoveryReason, handled := boxdRecoveryCleanup(claim); handled {
			remove, reason = recoveryRemove, recoveryReason
		}
		if !remove {
			fmt.Fprintf(b.rt.Stderr, "skip machine=%s reason=%s\n", row.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would destroy machine=%s lease=%s reason=%s\n", row.Name, leaseID, reason)
			continue
		}
		labels := make(map[string]string, len(claim.Labels)+1)
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels["state"] = "releasing"
		claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
		if err != nil {
			return fmt.Errorf("claim boxd machine %s for cleanup: %w", row.Name, err)
		}
		fmt.Fprintf(b.rt.Stdout, "destroy machine=%s lease=%s reason=%s\n", row.Name, leaseID, reason)
		if err := b.destroyMachine(ctx, cfg, row.Name); err != nil {
			return err
		}
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("finalize boxd machine cleanup claim: %w", err)
		}
	}
	// Reap claims whose machine is PROVABLY gone. A claim is the sole
	// ownership record of its machine, so a transient inventory omission
	// (a list or get miss) must never reap the claim of a still-live VM:
	// absence has to be corroborated by a direct read — and, for a real
	// removal, survive the same bounded visibility grace release uses. A
	// name present with a DIFFERENT immutable id is definitive (the claimed
	// machine is gone); a matching or unverifiable id retains the claim.
	for leaseID, claim := range claims {
		if _, ok := live[leaseID]; ok || claim.LeaseID == "" {
			continue
		}
		machineName := firstNonBlank(strings.TrimSpace(claim.Labels["machine"]), machineNameForLease(leaseID))
		boundID := claimBoundMachineID(claim, machineName)
		summary, found, err := b.getMachine(ctx, cfg, machineName)
		if err != nil {
			return err
		}
		if found {
			currentID := strings.TrimSpace(summary.ID)
			if boundID == "" || currentID == "" || currentID == boundID {
				fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=machine %s still present (claimed vm_id=%q, current=%q)\n", claim.LeaseID, machineName, boundID, currentID)
				continue
			}
			// Name reused by a replacement: the claimed machine is gone.
		} else if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=missing boxd machine (if absence persists through the visibility grace)\n", claim.LeaseID)
			continue
		} else {
			gone, err := b.machineAbsentThroughGrace(ctx, cfg, machineName)
			if err != nil {
				return err
			}
			if !gone {
				fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=machine %s reappeared within the visibility grace\n", claim.LeaseID, machineName)
				continue
			}
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=machine %s was replaced\n", claim.LeaseID, machineName)
			continue
		}
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("remove missing boxd machine claim: %w", err)
		}
	}
	return nil
}

// machineAbsentThroughGrace reports whether the machine stays absent through
// the same bounded visibility grace release uses, so a transient inventory
// gap never counts as proof of absence.
func (b *backend) machineAbsentThroughGrace(ctx context.Context, cfg core.Config, name string) (bool, error) {
	start := b.now()
	for {
		_, found, err := b.getMachine(ctx, cfg, name)
		if err != nil {
			return false, err
		}
		if found {
			return false, nil
		}
		if b.now().Sub(start) >= b.releaseNotFoundGrace {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(b.readyPollInterval):
		}
	}
}

// waitMachineReady polls `machine get` until the machine is SSH-reachable
// (running, or idle-suspended/hibernated — those wake on SSH ingress).
// Not-found within the grace window is eventual consistency, not loss.
func (b *backend) waitMachineReady(ctx context.Context, cfg core.Config, name string) (machineSummary, error) {
	waitCtx := ctx
	cancel := func() {}
	if b.readyTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, b.readyTimeout)
	}
	defer cancel()
	start := b.now()
	for {
		summary, found, err := b.getMachine(waitCtx, cfg, name)
		if err != nil {
			return machineSummary{}, err
		}
		if found {
			if machineReachable(summary.Status) {
				return summary, nil
			}
			if machineTerminal(summary.Status) {
				return machineSummary{}, core.Exit(1, "boxd machine %s entered terminal state %q", name, summary.Status)
			}
		} else if b.now().Sub(start) > boxdNotFoundGrace {
			return machineSummary{}, core.Exit(4, "boxd machine %s not visible after %s; giving up", name, boxdNotFoundGrace)
		}
		select {
		case <-waitCtx.Done():
			if waitCtx.Err() == context.DeadlineExceeded {
				return machineSummary{}, core.Exit(5, "timed out waiting for boxd machine %s to become ready", name)
			}
			return machineSummary{}, waitCtx.Err()
		case <-time.After(b.readyPollInterval):
		}
	}
}

// resolveSSHTarget reads the boxd-CLI-managed ssh-config entry for the machine
// into an explicit SSHTarget. `machine list` re-syncs the entry (including the
// asynchronously allocated per-machine SSH port and the known_hosts pin), so
// missing/incomplete entries are retried behind a re-sync.
func (b *backend) resolveSSHTarget(ctx context.Context, cfg core.Config, name, zone string) (core.SSHTarget, error) {
	host := name + "." + zone
	waitCtx := ctx
	cancel := func() {}
	if b.readyTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, b.readyTimeout)
	}
	defer cancel()
	for {
		// The list command's side effect is the point: it refreshes the managed
		// ssh-config block and known_hosts for every machine.
		if _, err := b.listMachines(waitCtx, cfg); err != nil {
			return core.SSHTarget{}, err
		}
		data, err := os.ReadFile(b.sshConfigPath)
		if err != nil && !os.IsNotExist(err) {
			return core.SSHTarget{}, core.Exit(1, "read ssh config %s: %v", b.sshConfigPath, err)
		}
		entry, found, selErr := selectBoxdSSHEntry(string(data), host)
		if selErr != nil {
			return core.SSHTarget{}, selErr
		}
		if found && strings.TrimSpace(entry.Port) != "" {
			return sshTargetFromEntry(entry, host, b.knownHostsPath)
		}
		select {
		case <-waitCtx.Done():
			if !found {
				return core.SSHTarget{}, core.Exit(5, "timed out waiting for the boxd CLI to write an ssh-config entry for %s", host)
			}
			return core.SSHTarget{}, core.Exit(5, "timed out waiting for boxd to allocate an SSH port for %s", host)
		case <-time.After(b.readyPollInterval):
		}
	}
}

// resolveMachineIdentity maps a crabbox identifier (lease id, slug, machine
// name, or vm id recorded in a claim) to the machine name and lease id.
func (b *backend) resolveMachineIdentity(identifier string, cfg core.Config) (name, leaseID string, claim core.LeaseClaim, claimed bool, err error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", core.LeaseClaim{}, false, core.Exit(2, "missing lease identifier for provider=%s", providerName)
	}
	claim, claimed, err = resolveBoxdClaim(identifier, cfg)
	if err != nil {
		return "", "", core.LeaseClaim{}, false, err
	}
	if claimed {
		leaseID = claim.LeaseID
		name = firstNonBlank(claim.Labels["machine"], machineNameForLease(leaseID))
		return name, leaseID, claim, true, nil
	}
	if id := leaseIDFromMachineName(identifier); id != "" {
		return identifier, id, core.LeaseClaim{}, false, nil
	}
	if core.IsCanonicalLeaseID(identifier) {
		return machineNameForLease(identifier), identifier, core.LeaseClaim{}, false, nil
	}
	return "", "", core.LeaseClaim{}, false, core.Exit(4, "lease %s not found for provider=%s", identifier, providerName)
}

func (b *backend) labelsFor(name, vmID, status string, cfg core.Config, claim core.LeaseClaim, claimed bool) map[string]string {
	labels := map[string]string{}
	if claimed {
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels["lease"] = claim.LeaseID
		if claim.Slug != "" {
			labels["slug"] = claim.Slug
		}
	} else if leaseID := leaseIDFromMachineName(name); leaseID != "" {
		labels["lease"] = leaseID
	}
	labels["provider"] = providerName
	labels["machine"] = name
	if vmID != "" {
		labels["vm_id"] = vmID
	}
	labels["work_root"] = cfg.WorkRoot
	if labels["state"] == "" || status != "" {
		labels["state"] = boxdState(status)
	}
	return labels
}

func (b *backend) server(name, status, url string, cfg core.Config, claims map[string]core.LeaseClaim) core.Server {
	leaseID := leaseIDFromMachineName(name)
	claim, claimed := claims[leaseID]
	labels := b.labelsFor(name, "", status, cfg, claim, claimed)
	if zone := zoneFromHost(url, name); zone != "" {
		labels["zone"] = zone
	}
	return b.serverFromLabels(name, labels["vm_id"], boxdState(status), cfg, labels)
}

func (b *backend) serverFromLabels(name, vmID, state string, cfg core.Config, labels map[string]string) core.Server {
	if labels == nil {
		labels = map[string]string{}
	}
	if labels["machine"] == "" {
		labels["machine"] = name
	}
	if vmID != "" && labels["vm_id"] == "" {
		labels["vm_id"] = vmID
	}
	if state != "" {
		labels["state"] = state
	}
	if labels["server_type"] == "" {
		labels["server_type"] = firstNonBlank(cfg.ServerType, "machine")
	}
	server := core.Server{
		CloudID:  firstNonBlank(labels["vm_id"], name),
		Provider: providerName,
		Name:     firstNonBlank(labels["slug"], name),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = labels["server_type"]
	if zone := strings.TrimSpace(labels["zone"]); zone != "" && name != "" {
		server.PublicNet.IPv4.IP = name + "." + zone
	}
	return server
}

func boxdClaims(cfg core.Config) (map[string]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	scope := core.ProviderClaimScope(providerName, cfg)
	out := make(map[string]core.LeaseClaim)
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == scope && claim.LeaseID != "" {
			out[claim.LeaseID] = claim
		}
	}
	return out, nil
}

func resolveBoxdClaim(identifier string, cfg core.Config) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	scope := core.ProviderClaimScope(providerName, cfg)
	var exact core.LeaseClaim
	var slugMatch core.LeaseClaim
	normalized := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if claim.LeaseID == identifier || claim.CloudID == identifier || claim.Labels["machine"] == identifier {
			if exact.LeaseID != "" && exact.LeaseID != claim.LeaseID {
				return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s scope=%s claims exactly match %s", providerName, scope, identifier)
			}
			exact = claim
			continue
		}
		if normalized != "" && core.NormalizeLeaseSlug(claim.Slug) == normalized {
			if slugMatch.LeaseID != "" && slugMatch.LeaseID != claim.LeaseID {
				return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s scope=%s claims match slug %s", providerName, scope, identifier)
			}
			slugMatch = claim
		}
	}
	if exact.LeaseID != "" {
		return exact, true, nil
	}
	return slugMatch, slugMatch.LeaseID != "", nil
}

func boxdRecoveryCleanup(claim core.LeaseClaim) (bool, string, bool) {
	switch claim.Labels["recovery"] {
	case "rollback-cleanup":
		return true, "failed acquisition rollback", true
	case "kept-after-failure":
		return false, "kept after failed acquisition", true
	default:
		return false, "", false
	}
}

func releaseAction(cfg core.Config) string {
	if cfg.Boxd.DeleteOnRelease {
		return "delete"
	}
	return "stop"
}

// deleteOnRelease decides destroy-vs-stop for a release: an explicit
// --boxd-delete-on-release wins, then the lease's recorded release label,
// then the config default (destroy).
func deleteOnRelease(lease core.LeaseTarget, cfg core.Config) bool {
	if core.DeleteOnReleaseExplicit(cfg, providerName) {
		return cfg.Boxd.DeleteOnRelease
	}
	if lease.Server.Labels != nil {
		switch strings.ToLower(strings.TrimSpace(lease.Server.Labels["release"])) {
		case "delete":
			return true
		case "stop":
			return false
		}
	}
	return cfg.Boxd.DeleteOnRelease
}
