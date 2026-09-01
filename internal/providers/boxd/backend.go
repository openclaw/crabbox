package boxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	providerName        = "boxd"
	defaultBoxdWorkRoot = "/home/boxd/crabbox"
	boxdReadyCheck      = "command -v bash >/dev/null && command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
)

type backend struct {
	spec       core.ProviderSpec
	cfg        core.Config
	rt         core.Runtime
	clientOnce sync.Once
	api        *consoleClient
	clientErr  error
	// Package-private seams keep tests off real SSH and provider infrastructure.
	readyTimeout, pollInterval, absenceGrace, rollbackTimeout time.Duration
	ensureKey                                                 func(string) (string, string, error)
	waitSSH                                                   func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) *backend {
	if rt.Stdout == nil {
		rt.Stdout = io.Discard
	}
	if rt.Stderr == nil {
		rt.Stderr = io.Discard
	}
	return &backend{spec: spec, cfg: cfg, rt: rt, readyTimeout: 5 * time.Minute, pollInterval: 2 * time.Second, absenceGrace: 30 * time.Second, rollbackTimeout: time.Minute, ensureKey: core.EnsureTestboxKey, waitSSH: core.WaitForSSHReady}
}
func (b *backend) Spec() core.ProviderSpec { return b.spec }
func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}
func (b *backend) client() (*consoleClient, error) {
	b.clientOnce.Do(func() {
		token := os.Getenv("CRABBOX_BOXD_TOKEN")
		if token == "" {
			token = os.Getenv("BOXD_TOKEN")
		}
		b.api, b.clientErr = newConsoleClient(b.cfg, b.rt, token)
	})
	return b.api, b.clientErr
}
func (b *backend) scope() string { return Provider{}.ClaimScope(b.cfg) }

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	c, err := b.client()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	user, err := c.whoami(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	id := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(id, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	labels := core.DirectLeaseLabels(b.cfg, id, slug, providerName, "", req.Keep, b.now())
	labels["machine"], labels["user_id"], labels["recovery"] = "crabbox-"+strings.TrimPrefix(id, "cbx_"), user, "provisioning"
	labels["state"] = "provisioning"
	labels["work_root"] = b.cfg.WorkRoot
	labels["release"] = "delete"
	if !b.cfg.Boxd.DeleteOnRelease {
		labels["release"] = "stop"
	}
	server := core.Server{Provider: providerName, Name: slug, Status: "provisioning", Labels: labels}
	// Publish the intent before the asynchronous create. A lost response must
	// leave an ownership record, never trigger adoption by reusable name.
	var claim core.LeaseClaim
	if req.Repo.Root == "" {
		if err = core.EnsureCrabboxClaimNamespaceDurable(); err == nil {
			claim, err = core.ClaimLeaseTargetForConfigScopeIfUnchanged(id, slug, b.cfg, b.scope(), server, core.SSHTarget{}, b.cfg.IdleTimeout, core.LeaseClaim{}, false)
		}
	} else {
		claim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(id, slug, b.cfg, b.scope(), server, core.SSHTarget{}, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, core.LeaseClaim{}, false)
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	createdClaim, _, _, err := core.UpdateLeaseClaimEndpointIfUnchangedAction(id, claim, func() (core.Server, core.SSHTarget, bool, error) {
		vm, err := c.create(ctx, labels["machine"])
		if err != nil {
			return core.Server{}, core.SSHTarget{}, false, err
		}
		if _, err := machineRoute(vm.ID, ""); err != nil {
			return core.Server{}, core.SSHTarget{}, false, core.Exit(5, "boxd create response has no valid immutable machine ID")
		}
		server.CloudID = vm.ID
		server.Labels = shared.CloneLabels(labels)
		server.Labels["vm_id"] = vm.ID
		return server, core.SSHTarget{}, true, nil
	})
	if err != nil {
		var rejection *consoleHTTPError
		if errors.As(err, &rejection) {
			switch rejection.Code {
			case 400, 401, 403, 422:
				// A definite rejection did not allocate a resource. Remove only
				// our unchanged pending intent; never retain it as ambiguous.
				return core.LeaseTarget{}, errors.Join(err, core.RemoveLeaseClaimIfUnchanged(id, claim))
			}
		}
		// Transport/server errors and malformed success can follow a committed
		// create. Do not retry the write or guess ownership from inventory names.
		return core.LeaseTarget{}, errors.Join(err, core.Exit(5, "boxd creation is ambiguous; retained claim %s (machine %s). Inspect the HTTPS console before manual recovery; Crabbox will not adopt a machine by name", id, labels["machine"]))
	}
	claim = createdClaim
	rollback := func(cause error) (core.LeaseTarget, error) {
		if !req.Keep {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), b.rollbackTimeout)
			defer cleanupCancel()
			if err := b.deleteClaim(cleanupCtx, c, claim); err == nil {
				return core.LeaseTarget{}, cause
			} else {
				cause = errors.Join(cause, fmt.Errorf("boxd rollback failed; retained claim %s: %w", id, err))
			}
		}
		failed := shared.CloneLabels(claim.Labels)
		failed["recovery"] = "failed"
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(id, claim, failed); err != nil {
			cause = errors.Join(cause, err)
		}
		return core.LeaseTarget{}, cause
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(leaseFromClaim(claim, consoleMachine{})); err != nil {
			return rollback(err)
		}
	}
	updated, server, target, err := core.UpdateLeaseClaimEndpointIfUnchangedAction(id, claim, func() (core.Server, core.SSHTarget, bool, error) {
		lease, err := b.prepare(ctx, c, claim)
		return lease.Server, lease.SSH, err == nil, err
	})
	if err != nil {
		return rollback(err)
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	fmt.Fprintf(b.rt.Stderr, "provisioned provider=boxd lease=%s slug=%s state=ready\n", id, slug)
	return core.LeaseTarget{LeaseID: id, Server: server, SSH: target}, nil
}

func (b *backend) authenticateClaim(ctx context.Context, c *consoleClient, claim core.LeaseClaim) error {
	user, err := c.whoami(ctx)
	if err != nil {
		return err
	}
	if claim.Provider != providerName || claim.ProviderScope != b.scope() || claim.Labels["user_id"] != user || claim.Labels["lease"] != claim.LeaseID {
		return core.Exit(4, "boxd claim origin, organization, or authenticated account does not match")
	}
	if claim.CloudID == "" || claim.Labels["vm_id"] != claim.CloudID {
		return core.Exit(4, "boxd claim has no verified immutable machine ID; inspect the HTTPS console for manual recovery")
	}
	_, err = machineRoute(claim.CloudID, "")
	return err
}

func findMachine(rows []consoleMachine, id string) (consoleMachine, bool) {
	for _, vm := range rows {
		if vm.ID == id {
			return vm, true
		}
	}
	return consoleMachine{}, false
}
func (b *backend) observe(ctx context.Context, c *consoleClient, claim core.LeaseClaim) (consoleMachine, bool, error) {
	if err := b.authenticateClaim(ctx, c, claim); err != nil {
		return consoleMachine{}, false, err
	}
	rows, err := c.machines(ctx)
	if err != nil {
		return consoleMachine{}, false, err
	}
	vm, found := findMachine(rows, claim.CloudID)
	if found && vm.OwnerID != claim.Labels["user_id"] {
		return consoleMachine{}, false, core.Exit(4, "boxd machine owner does not match its authenticated ownership claim")
	}
	return vm, found, nil
}

func (b *backend) prepare(ctx context.Context, c *consoleClient, claim core.LeaseClaim) (core.LeaseTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	vm, found, err := b.observe(ctx, c, claim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if found && !vm.Isolated {
		return core.LeaseTarget{}, core.Exit(5, "boxd machine is not isolated; refusing guest access")
	}
	if found && machineState(vm.Status) == "stopped" {
		if err := c.action(ctx, claim.CloudID, "start"); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	result, err := shared.Poll(ctx, 0, b.pollInterval, shared.SleepContext, func(ctx context.Context) (consoleMachine, error) {
		vm, _, err := b.observe(ctx, c, claim)
		return vm, err
	}, func(_ context.Context, vm consoleMachine, err error) (bool, error) {
		if err != nil {
			return false, err
		}
		if vm.ID == "" {
			return false, nil
		}
		if !vm.Isolated {
			return false, core.Exit(5, "boxd machine is not isolated; refusing guest access")
		}
		switch machineState(vm.Status) {
		case "ready":
			return true, nil
		case "failed", "deleted":
			return false, core.Exit(5, "boxd machine failed during provisioning")
		}
		return false, nil
	}, nil)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	vm = result.Value
	if net.ParseIP(vm.PublicIP) == nil {
		return core.LeaseTarget{}, core.Exit(5, "boxd machine has no valid authenticated public IP")
	}
	privateKey, publicKey, err := b.ensureKey(claim.LeaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	hostKey, err := c.bootstrap(ctx, claim.CloudID, publicKey)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	forward, err := c.exposeSSH(ctx, claim.CloudID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	vm, found, err = b.observe(ctx, c, claim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !found || !vm.Isolated || net.ParseIP(vm.PublicIP) == nil {
		return core.LeaseTarget{}, core.Exit(5, "boxd machine lost its isolated SSH endpoint during bootstrap")
	}
	target := core.SSHTargetFromConfig(b.cfg, vm.PublicIP)
	target.User, target.Port, target.Key, target.SSHHostKey = "boxd", strconv.Itoa(forward.PublicPort), privateKey, hostKey
	target.TargetOS, target.NetworkKind, target.ReadyCheck = core.TargetLinux, core.NetworkPublic, boxdReadyCheck
	target.FallbackPorts = nil
	target.DisableHostKeyChecking = false
	if err := core.UseLeaseKnownHosts(&target, claim.LeaseID); err != nil {
		return core.LeaseTarget{}, err
	}
	core.UseStoredTestboxKey(&target, claim.LeaseID)
	if err := pinHostKey(target); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.waitSSH(ctx, &target, b.rt.Stderr, "boxd guest ssh", core.BootstrapWaitTimeout(b.cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	lease := leaseFromClaim(claim, vm)
	lease.SSH = target
	lease.Server.Labels["recovery"] = ""
	lease.Server.Labels["ssh_host_key"] = hostKey
	lease.Server.Labels["state"] = "ready"
	lease.Server.Status = "ready"
	return lease, nil
}

func leaseFromClaim(claim core.LeaseClaim, vm consoleMachine) core.LeaseTarget {
	labels := shared.CloneLabels(claim.Labels)
	state := labels["state"]
	if vm.ID != "" {
		state = machineState(vm.Status)
	}
	labels["state"] = state
	server := core.Server{Provider: providerName, CloudID: claim.CloudID, Name: claim.Slug, Status: state, Labels: labels}
	server.ServerType.Name = "machine"
	server.PublicNet.IPv4.IP = vm.PublicIP
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}
}
func machineState(status string) string {
	switch strings.ToLower(status) {
	case "running", "ready":
		return "ready"
	case "stopped", "suspended":
		return "stopped"
	case "destroyed", "deleted":
		return "deleted"
	case "failed", "error":
		return "failed"
	default:
		return "provisioning"
	}
}

func (b *backend) resolveClaim(identifier string) (core.LeaseClaim, error) {
	claim, found, err := shared.ResolveProviderClaimStrict(identifier, providerName, b.scope())
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !found {
		return core.LeaseClaim{}, core.Exit(4, "boxd lease has no matching local ownership claim; machines are never adopted by name")
	}
	return claim, nil
}
func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	claim, err := b.resolveClaim(req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	c, err := b.client()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	vm, found, err := b.observe(ctx, c, claim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly || req.StatusOnly && !req.ReadyProbe {
		lease := leaseFromClaim(claim, vm)
		if !found {
			lease.Server.Status = "missing"
			lease.Server.Labels["state"] = "missing"
		}
		return lease, nil
	}
	if !found {
		return core.LeaseTarget{}, core.Exit(4, "claimed boxd machine is missing; refusing to resolve a replacement")
	}
	if req.StatusOnly && req.ReadyProbe {
		lease := leaseFromClaim(claim, vm)
		if !vm.Isolated || machineState(vm.Status) != "ready" || vm.PublicIP != claim.SSHHost || claim.SSHPort < 1 || claim.Labels["ssh_host_key"] == "" {
			return core.LeaseTarget{}, core.Exit(5, "boxd lease has no ready, verified SSH endpoint; run with --id to prepare it")
		}
		target := core.SSHTargetFromConfig(b.cfg, vm.PublicIP)
		target.User, target.Port, target.SSHHostKey = "boxd", strconv.Itoa(claim.SSHPort), claim.Labels["ssh_host_key"]
		target.Key = ""
		core.UseStoredTestboxKey(&target, claim.LeaseID)
		if target.Key == "" {
			return core.LeaseTarget{}, core.Exit(5, "boxd stored lease SSH key is missing")
		}
		target.KnownHostsFile = filepath.Join(filepath.Dir(target.Key), "known_hosts")
		target.ReadyCheck = boxdReadyCheck
		target.FallbackPorts = nil
		target.DisableHostKeyChecking = false
		if err := core.WithLeaseClaimUnchanged(claim.LeaseID, claim, func() error {
			return b.waitSSH(ctx, &target, b.rt.Stderr, "boxd guest ssh", core.BootstrapWaitTimeout(b.cfg))
		}); err != nil {
			return core.LeaseTarget{}, err
		}
		lease.SSH = target
		return lease, nil
	}
	if req.NoLocalStateMutations {
		return core.LeaseTarget{}, core.Exit(2, "boxd SSH preparation requires a fenced local claim update")
	}
	updated, server, target, err := core.UpdateLeaseClaimEndpointIfUnchangedAction(claim.LeaseID, claim, func() (core.Server, core.SSHTarget, bool, error) {
		lease, err := b.prepare(ctx, c, claim)
		return lease.Server, lease.SSH, err == nil, err
	})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		updated, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(claim.LeaseID, claim.Slug, b.cfg, server, target, req.Repo.Root, idle, req.Reclaim, updated, true)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server, SSH: target}, nil
}

func (b *backend) exactClaim(lease core.LeaseTarget) (core.LeaseClaim, error) {
	claim, err := shared.RequireExactClaim(shared.ClaimBinding{Provider: providerName, ProviderScope: b.scope(), ExactProviderScope: true, LeaseID: lease.LeaseID, CloudID: lease.Server.CloudID})
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server); set {
		if !exists {
			return core.LeaseClaim{}, core.Exit(4, "boxd ownership snapshot is absent")
		}
		if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, snapshot); err != nil {
			return core.LeaseClaim{}, err
		}
	}
	return claim, nil
}

func (b *backend) deleteClaim(ctx context.Context, c *consoleClient, claim core.LeaseClaim) error {
	return b.deleteClaimWithOutcome(ctx, c, claim, &core.ReleaseLeaseOutcome{})
}

func (b *backend) deleteClaimWithOutcome(ctx context.Context, c *consoleClient, claim core.LeaseClaim, outcome *core.ReleaseLeaseOutcome) error {
	ctx, cancel := context.WithTimeout(ctx, b.rollbackTimeout)
	defer cancel()
	return core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		vm, found, err := b.observe(ctx, c, claim)
		if err != nil {
			return err
		}
		if found && machineState(vm.Status) != "deleted" {
			if err := c.action(ctx, claim.CloudID, "destroy"); err != nil {
				return err
			}
		}
		// An acknowledged write is not deletion proof. Require consistent inventory
		// absence across a grace period, checking the authenticated account each time.
		var absentSince time.Time
		_, err = shared.Poll(ctx, 0, b.pollInterval, shared.SleepContext, func(ctx context.Context) (bool, error) {
			_, found, err := b.observe(ctx, c, claim)
			return !found, err
		}, func(_ context.Context, absent bool, err error) (bool, error) {
			if err != nil {
				return false, err
			}
			if !absent {
				absentSince = time.Time{}
				return false, nil
			}
			if absentSince.IsZero() {
				absentSince = time.Now()
				return false, nil
			}
			return time.Since(absentSince) >= b.absenceGrace, nil
		}, nil)
		outcome.Terminal = err == nil
		return err
	})
}
func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *backend) ReleaseLeaseWithOutcome(ctx context.Context, req core.ReleaseLeaseRequest) (core.ReleaseLeaseOutcome, error) {
	var outcome core.ReleaseLeaseOutcome
	err := b.releaseLease(ctx, req, &outcome)
	return outcome, err
}

func (b *backend) releaseLease(ctx context.Context, req core.ReleaseLeaseRequest, outcome *core.ReleaseLeaseOutcome) error {
	claim, err := b.exactClaim(req.Lease)
	if err != nil {
		return err
	}
	c, err := b.client()
	if err != nil {
		return err
	}
	if deleteOnRelease(leaseFromClaim(claim, consoleMachine{}), b.cfg) {
		return b.deleteClaimWithOutcome(ctx, c, claim, outcome)
	}
	labels := shared.CloneLabels(claim.Labels)
	labels["state"], labels["release"] = "stopped", "stop"
	_, err = core.UpdateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, labels, func() error {
		vm, found, err := b.observe(ctx, c, claim)
		if err != nil {
			return err
		}
		if !found {
			return core.Exit(4, "boxd machine is missing; retaining its ownership claim")
		}
		if machineState(vm.Status) == "stopped" {
			return nil
		}
		return c.action(ctx, claim.CloudID, "stop")
	})
	return err
}
func deleteOnRelease(lease core.LeaseTarget, cfg core.Config) bool {
	if core.DeleteOnReleaseExplicit(cfg, providerName) {
		return cfg.Boxd.DeleteOnRelease
	}
	if action := lease.Server.Labels["release"]; action != "" {
		return action == "delete"
	}
	return cfg.Boxd.DeleteOnRelease
}
func (b *backend) RetainLeaseClaimAfterRelease(lease core.LeaseTarget) bool {
	return !deleteOnRelease(lease, b.cfg)
}
func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	if b.RetainLeaseClaimAfterRelease(lease) {
		return fmt.Sprintf("stopped lease=%s retained=true", lease.LeaseID)
	}
	return fmt.Sprintf("verified absent lease=%s; removed ownership claim", lease.LeaseID)
}
func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	claim, err := b.exactClaim(req.Lease)
	if err != nil {
		return core.Server{}, err
	}
	c, err := b.client()
	if err != nil {
		return core.Server{}, err
	}
	vm, found, err := b.observe(ctx, c, claim)
	if err != nil {
		return core.Server{}, err
	}
	if !found {
		return core.Server{}, core.Exit(4, "boxd machine is missing; refusing to touch its claim")
	}
	server := leaseFromClaim(claim, vm).Server
	server.Labels = core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(server.Labels, b.cfg, req.State, b.now(), req.IdleTimeoutOverride)
	updated, err := core.UpdateLeaseClaimTouchIfUnchanged(claim.LeaseID, claim, server.Labels, b.now(), req.IdleTimeoutOverride)
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, err
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	c, err := b.client()
	if err != nil {
		return nil, err
	}
	user, err := c.whoami(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.machines(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != b.scope() || claim.Labels["user_id"] != user {
			continue
		}
		vm, found := findMachine(rows, claim.CloudID)
		if found && vm.OwnerID != user {
			return nil, core.Exit(4, "boxd machine owner does not match its claim")
		}
		lease := leaseFromClaim(claim, vm)
		if !found && claim.CloudID != "" {
			lease.Server.Status = "missing"
			lease.Server.Labels["state"] = "missing"
		}
		out = append(out, lease.Server)
	}
	return out, nil
}
func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	leases, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(providerName, len(leases)), nil
}
func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	views, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return err
	}
	c, err := b.client()
	if err != nil {
		return err
	}
	for _, server := range views {
		claim, _, _ := core.ServerLeaseClaimSnapshot(server)
		if claim.CloudID == "" {
			fmt.Fprintf(b.rt.Stderr, "skip lease=%s: ambiguous creation needs manual HTTPS console recovery\n", claim.LeaseID)
			continue
		}
		eligible, reason := core.ShouldCleanupServer(server, b.now())
		if claim.Labels["recovery"] == "provisioning" {
			started, parseErr := time.Parse(time.RFC3339, claim.ClaimedAt)
			eligible = parseErr == nil && b.now().After(started.Add(2*b.readyTimeout+b.rollbackTimeout)) && claim.Labels["keep"] != "true"
			reason = "abandoned provisioning"
		} else if claim.Labels["recovery"] == "failed" && claim.Labels["keep"] != "true" {
			eligible = true
			reason = "failed provisioning"
		}
		if !eligible {
			continue
		}
		fmt.Fprintf(b.rt.Stderr, "cleanup lease=%s dry-run=%t reason=%s\n", claim.LeaseID, req.DryRun, reason)
		if !req.DryRun {
			if err := b.deleteClaim(ctx, c, claim); err != nil {
				return err
			}
		}
	}
	return nil
}
