package parallels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type leaseBackend struct {
	shared.DirectSSHBackend
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = "parallels"
	if cfg.Parallels.User != "" {
		cfg.SSHUser = cfg.Parallels.User
	}
	if cfg.Parallels.WorkRoot != "" {
		cfg.WorkRoot = cfg.Parallels.WorkRoot
	}
	if cfg.TargetOS == core.TargetMacOS && cfg.SSHUser == core.BaseConfig().SSHUser {
		cfg.SSHUser = os.Getenv("USER")
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true}}
}

func (b *leaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	// A caller-supplied lease ID is a single create identity. Retrying it with a
	// fresh lease would defeat the replay contract, so it never enters the
	// generated-ID bootstrap retry loop.
	if strings.TrimSpace(req.RequestedLeaseID) != "" {
		return b.acquireFixed(ctx, req)
	}
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (core.LeaseTarget, error) {
	cfg := b.Cfg
	source := strings.TrimSpace(shared.FirstNonBlankTrimmed(cfg.Parallels.SourceID, cfg.Parallels.Source))
	if source == "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=parallels requires --parallels-source, --parallels-template, or parallels.source")
	}
	// Hold the host's capacity reservation from the maxVMs count through the clone,
	// so concurrent forks cannot all pass the gate on the same pre-clone count.
	selected, releaseCapacity, err := core.ReserveParallelsFleetCapacity(ctx, cfg, b.RT.Exec, source)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	capacityHeld := true
	releaseCapacityOnce := func() {
		if capacityHeld {
			capacityHeld = false
			releaseCapacity()
		}
	}
	defer releaseCapacityOnce()
	cfg = selected
	client := core.NewParallelsClient(cfg, b.RT.Exec)
	if err := client.ValidateMacOSBootstrapKey(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keepKey := false
	defer func() {
		if !keepKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cleanupVM := func(id string) {
		if err := client.Delete(context.Background(), id); err != nil {
			keepKey = true
		}
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	snapshotID := strings.TrimSpace(shared.FirstNonBlankTrimmed(cfg.Parallels.SourceSnapshotID, cfg.Parallels.SourceSnapshot))
	if snapshotID != "" && cfg.Parallels.SourceSnapshotID == "" {
		resolved, err := client.SnapshotID(ctx, source, snapshotID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		snapshotID = resolved
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=parallels lease=%s slug=%s host=%s source=%s snapshot=%s clone_mode=%s keep=%v\n",
		leaseID, slug, parallelsHostName(cfg), source, blank(snapshotID, "-"), blank(cfg.Parallels.CloneMode, "linked"), keep)
	server, err := client.Clone(ctx, source, snapshotID, leaseID, slug, keep)
	// The clone now counts against maxVMs for every later reservation, so the rest of
	// bootstrap does not need to keep other forks waiting.
	releaseCapacityOnce()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := client.Start(ctx, server.CloudID); err != nil {
		cleanupVM(server.CloudID)
		return core.LeaseTarget{}, err
	}
	vm, err := client.WaitForIP(ctx, server.CloudID, cfg.Parallels.StartupTimeout)
	if err != nil {
		cleanupVM(server.CloudID)
		return core.LeaseTarget{}, err
	}
	if err := b.prepareGuest(ctx, client, server.CloudID, vm, cfg, publicKey); err != nil {
		cleanupVM(server.CloudID)
		return core.LeaseTarget{}, err
	}
	server.PublicNet.IPv4.IP = vm.IP
	if vm.IPSource != "" {
		server.Labels["ip_source"] = vm.IPSource
	}
	target := parallelsLeaseSSHTarget(cfg, vm.IP)
	if cfg.TargetOS == core.TargetWindows && cfg.WindowsMode == core.WindowsModeNormal {
		target.ReadyCheck = core.PowershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
	}
	if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		cleanupVM(server.CloudID)
		return core.LeaseTarget{}, err
	}
	server.Status = "ready"
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Now().UTC())
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, target, cfg.IdleTimeout); err != nil {
		cleanupVM(server.CloudID)
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s vm=%s ip=%s\n", leaseID, server.DisplayID(), vm.IP)
	keepKey = true
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) prepareGuest(ctx context.Context, client *core.ParallelsClient, vmID string, vm core.ParallelsVM, cfg core.Config, publicKey string) error {
	if vm.IPSource == "dhcp-mac" {
		fmt.Fprintf(b.RT.Stderr, "parallels macOS fallback vm=%s ip=%s discovery=dhcp-mac bootstrap=ssh\n", vmID, vm.IP)
		return client.BootstrapMacOSOverSSH(ctx, vm.IP, cfg, publicKey)
	}
	if err := client.WaitForGuestExec(ctx, vmID, cfg, cfg.Parallels.StartupTimeout); err != nil {
		if parallelsMacOSBootstrapFallbackAllowed(cfg, err) {
			fmt.Fprintf(b.RT.Stderr, "parallels macOS fallback vm=%s ip=%s discovery=tools bootstrap=ssh\n", vmID, vm.IP)
			return client.BootstrapMacOSOverSSH(ctx, vm.IP, cfg, publicKey)
		}
		return err
	}
	if err := client.InstallSSHKey(ctx, vmID, cfg, publicKey); err != nil {
		if parallelsMacOSBootstrapFallbackAllowed(cfg, err) {
			fmt.Fprintf(b.RT.Stderr, "parallels macOS fallback vm=%s ip=%s discovery=tools bootstrap=ssh\n", vmID, vm.IP)
			return client.BootstrapMacOSOverSSH(ctx, vm.IP, cfg, publicKey)
		}
		return err
	}
	if err := client.EnsureGuestReady(ctx, vmID, cfg); err != nil {
		if parallelsMacOSBootstrapFallbackAllowed(cfg, err) {
			fmt.Fprintf(b.RT.Stderr, "parallels macOS fallback vm=%s ip=%s discovery=tools bootstrap=ssh\n", vmID, vm.IP)
			return client.BootstrapMacOSOverSSH(ctx, vm.IP, cfg, publicKey)
		}
		return err
	}
	return nil
}

func parallelsMacOSBootstrapFallbackAllowed(cfg core.Config, err error) bool {
	return cfg.TargetOS == core.TargetMacOS && strings.TrimSpace(cfg.Parallels.BootstrapKey) != "" && core.ParallelsGuestToolsUnavailable(err)
}

func (b *leaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return core.LeaseTarget{}, core.Exit(2, "parallels resolve requires lease id or slug")
	}
	fixedClaim, fixedClaimFound, err := core.ResolveLeaseClaimForProvider(id, "parallels")
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if fixedClaimFound {
		id = fixedClaim.LeaseID
	}
	fixedClaimFound = fixedClaimFound && parallelsFixedLeaseKind.IsFixedClaim(fixedClaim)
	// A terminal fixed lease has no resource and needs no host. Answering it
	// before the inventory sweep is what makes `crabbox stop` on an already
	// released fixed ID an idempotent no-op rather than an error, even when the
	// fleet is unreachable.
	if req.ReleaseOnly && fixedClaimFound && fixedClaim.FixedCreateIntent.State == "released" {
		return parallelsFixedReleaseTarget(fixedClaim), nil
	}
	var hostErrs []error
	for _, candidate := range core.ParallelsCandidateConfigs(b.Cfg) {
		client := core.NewParallelsClient(candidate, b.RT.Exec)
		vms, err := client.ListVMs(ctx)
		if err != nil {
			hostErrs = append(hostErrs, parallelsHostError(candidate, "list vms", err))
			continue
		}
		for _, vm := range vms {
			leaseID, slug := parallelsLeaseFromVMName(vm.Name)
			labels := core.ParallelsLabelsFromName(vm.Name)
			if labels["lease"] == "" {
				labels = core.DirectLeaseLabels(candidate, shared.FirstNonBlankTrimmed(leaseID, vm.ID), slug, "parallels", "", true, time.Now().UTC())
			}
			labels["host"] = parallelsHostName(candidate)
			server := core.Server{CloudID: vm.ID, Provider: "parallels", Name: vm.Name, Status: strings.ToLower(vm.State), Labels: labels}
			server.ServerType.Name = core.ServerTypeForProviderClass("parallels", candidate.Class)
			normalizedID := strings.ReplaceAll(id, "_", "-")
			if vm.ID == id || vm.Name == id || leaseID == id || strings.ReplaceAll(leaseID, "_", "-") == normalizedID || core.NormalizeLeaseSlug(slug) == core.NormalizeLeaseSlug(id) {
				leaseID = shared.FirstNonBlankTrimmed(leaseID, vm.ID)
				adopt, err := authorizeParallelsResolve(req, leaseID, vm.ID, parallelsHostName(candidate))
				if err != nil {
					return core.LeaseTarget{}, err
				}
				claim, claimOK, claimExact, claimErr := core.ResolveLeaseClaimForProviderWithExact(leaseID, "parallels")
				if claimErr != nil {
					return core.LeaseTarget{}, claimErr
				}
				if claimOK && claimExact && applyParallelsClaimSSHPortToConfig(&candidate, claim, leaseID, vm.ID, parallelsHostName(candidate)) {
					client = core.NewParallelsClient(candidate, b.RT.Exec)
				}
				if vm.IP == "" && strings.EqualFold(vm.State, "running") {
					discovered, err := client.WaitForIP(ctx, vm.ID, 30*time.Second)
					if err != nil {
						if !req.ReleaseOnly && !req.StatusOnly {
							return core.LeaseTarget{}, err
						}
					} else {
						vm = discovered
					}
				}
				server.PublicNet.IPv4.IP = vm.IP
				if vm.IPSource != "" {
					server.Labels["ip_source"] = vm.IPSource
				}
				if strings.TrimSpace(candidate.SSHUser) == core.BaseConfig().SSHUser {
					var user string
					var err error
					if candidate.TargetOS == core.TargetWindows && candidate.WindowsMode == core.WindowsModeNormal {
						user, err = client.WindowsGuestText(ctx, vm.ID, `C:\ProgramData\crabbox\windows.username`)
					} else {
						user, err = client.POSIXGuestText(ctx, vm.ID, `/var/lib/crabbox/ssh.username`)
					}
					if err == nil && strings.TrimSpace(user) != "" {
						candidate.SSHUser = strings.TrimSpace(user)
					}
				}
				target := parallelsLeaseSSHTarget(candidate, vm.IP)
				if !req.ReleaseOnly {
					if err := core.UseStoredTestboxKey(&target, leaseID); err != nil {
						return core.LeaseTarget{}, err
					}
				}
				if claimOK && claimExact {
					applyParallelsClaimSSHPort(&target, claim, leaseID, vm.ID, parallelsHostName(candidate))
				}
				if adopt {
					if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, candidate, server, target, req.Repo.Root, candidate.IdleTimeout, true); err != nil {
						return core.LeaseTarget{}, err
					}
				}
				return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
			}
		}
	}
	if len(hostErrs) > 0 {
		return core.LeaseTarget{}, fmt.Errorf("parallels fleet inventory incomplete while resolving %s: %w", req.ID, errors.Join(hostErrs...))
	}
	// A fixed lease's durable claim is its own recovery route. Stop resolves
	// before releasing, and an acquired VM that is already gone, or an
	// unfinished attempt, has no live inventory match. Hand the claim to the
	// release path instead of failing here: releaseFixed re-attests the host
	// connection and the recorded VM incarnation, finalizes only a proven
	// absence, and retains custody otherwise. A complete sweep reached this
	// point, so absence is at least well-founded.
	if req.ReleaseOnly && fixedClaimFound {
		return parallelsFixedReleaseTarget(fixedClaim), nil
	}
	return core.LeaseTarget{}, core.Exit(4, "parallels lease not found: %s", req.ID)
}

// parallelsFixedReleaseTarget carries only the identity the durable release
// path needs. It grants no SSH access and asserts no live resource: releaseFixed
// reads the claim itself and performs every ownership and attestation check.
func parallelsFixedReleaseTarget(claim core.LeaseClaim) core.LeaseTarget {
	server := core.Server{
		CloudID:  claim.CloudID,
		Provider: parallelsProviderName,
		Name:     strings.TrimSpace(claim.FixedCreateIntent.Attempt["name"]),
		Status:   "unknown",
		Labels:   maps.Clone(claim.Labels),
	}
	server.ImmutableID = claim.CloudImmutableID
	return core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}
}

func parallelsSSHTarget(cfg core.Config, host string) core.SSHTarget {
	target := core.SSHTargetFromConfig(cfg, host)
	// The account credential is consumed only by Crabbox's local ARD client.
	// Never inherit it into ssh, scp, rsync, ProxyCommand, or viewer children.
	target.ChildEnvDenylist = append(target.ChildEnvDenylist, "CRABBOX_PARALLELS_PASSWORD")
	return target
}

func parallelsLeaseSSHTarget(cfg core.Config, host string) core.SSHTarget {
	target := parallelsSSHTarget(cfg, host)
	if cfg.Parallels.Host != "" {
		target.ProxyCommand = parallelsProxyCommand(cfg, host)
		target.SSHConfigProxy = true
	}
	return target
}

func parallelsClaimSSHPort(claim core.LeaseClaim, leaseID, vmID, host string) (string, bool) {
	if claim.LeaseID != strings.TrimSpace(leaseID) ||
		claim.CloudID != strings.TrimSpace(vmID) ||
		strings.TrimSpace(claim.Labels["host"]) != strings.TrimSpace(host) ||
		claim.SSHPort <= 0 || claim.SSHPort > 65535 {
		return "", false
	}
	return strconv.Itoa(claim.SSHPort), true
}

func applyParallelsClaimSSHPortToConfig(cfg *core.Config, claim core.LeaseClaim, leaseID, vmID, host string) bool {
	port, ok := parallelsClaimSSHPort(claim, leaseID, vmID, host)
	if !ok || cfg == nil {
		return false
	}
	cfg.SSHPort = port
	cfg.SSHFallbackPorts = []string{}
	return true
}

func applyParallelsClaimSSHPort(target *core.SSHTarget, claim core.LeaseClaim, leaseID, vmID, host string) bool {
	// Resolve already ownership-fences the exact lease/VM/host binding before
	// this point. Reuse the transport endpoint that acquisition actually proved
	// instead of replacing it with the command's generic SSH default. The same
	// port is applied to the client config before DHCP/IP discovery.
	port, ok := parallelsClaimSSHPort(claim, leaseID, vmID, host)
	if !ok || target == nil {
		return false
	}
	target.Port = port
	target.FallbackPorts = []string{}
	return true
}

func authorizeParallelsResolve(req core.ResolveRequest, leaseID, vmID, host string) (bool, error) {
	owned, err := exactParallelsClaimOwned(leaseID, vmID, host)
	if err != nil {
		return false, err
	}
	readOnlyStatus := req.StatusOnly && !req.ReleaseOnly && !req.Reclaim
	if owned || readOnlyStatus {
		return false, nil
	}
	if req.ReleaseOnly {
		// A fixed claim that has not yet bound a VM fails this exact-ownership
		// check, which would end stop before the durable release path runs. Let
		// releaseFixed answer instead: it re-attests the host connection and the
		// recorded VM incarnation, so it is strictly stricter than this gate and
		// reports uncertain custody in terms the operator can act on.
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return false, err
		}
		if exists && parallelsFixedLeaseKind.IsFixedClaim(claim) {
			return false, nil
		}
	}
	if req.ReleaseOnly || !req.Reclaim {
		return false, parallelsOwnershipError(leaseID, vmID, host)
	}
	// A fixed lease ID is a single create identity. Adoption would rewrite its
	// claim and discard the durable create intent, so the replay and tombstone
	// guarantees must outrank an explicit --reclaim.
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return false, err
	}
	if exists && parallelsFixedLeaseKind.IsFixedClaim(claim) {
		return false, core.Exit(4, "lease_id_conflict: Parallels lease %q has a durable fixed create intent; --reclaim cannot re-bind it to VM %q on host %q", strings.TrimSpace(leaseID), strings.TrimSpace(vmID), strings.TrimSpace(host))
	}
	if strings.TrimSpace(req.Repo.Root) == "" {
		return false, core.Exit(2, "parallels --reclaim requires repository context before binding VM %q on host %q", strings.TrimSpace(vmID), strings.TrimSpace(host))
	}
	return true, nil
}

func (b *leaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	var out []core.LeaseView
	var hostErrs []error
	for _, cfg := range core.ParallelsCandidateConfigs(b.Cfg) {
		leases, err := core.NewParallelsClient(cfg, b.RT.Exec).ListCrabboxServers(ctx)
		if err != nil {
			hostErrs = append(hostErrs, parallelsHostError(cfg, "list leases", err))
			continue
		}
		for i := range leases {
			if leases[i].Labels == nil {
				leases[i].Labels = map[string]string{}
			}
			leases[i].Labels["host"] = parallelsHostName(cfg)
		}
		out = append(out, leases...)
	}
	if len(hostErrs) > 0 {
		return nil, fmt.Errorf("parallels fleet inventory incomplete: %w", errors.Join(hostErrs...))
	}
	return out, nil
}

func parallelsHostError(cfg core.Config, action string, err error) error {
	return fmt.Errorf("host %s %s: %w", parallelsHostName(cfg), action, err)
}

func (b *leaseBackend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	runtime := "unchecked"
	cfg := b.Cfg
	if req.ProbeSSH && cfg.Parallels.Source != "" {
		selected, err := core.SelectParallelsFleetConfig(ctx, cfg, b.RT.Exec, cfg.Parallels.Source)
		if err != nil {
			return core.DoctorResult{}, err
		}
		client := core.NewParallelsClient(selected, b.RT.Exec)
		vm, err := client.GetVM(ctx, selected.Parallels.Source)
		if err != nil {
			return core.DoctorResult{}, err
		}
		if vm.IP != "" {
			target := parallelsLeaseSSHTarget(selected, vm.IP)
			if err := core.WaitForSSHReady(ctx, &target, io.Discard, "doctor", 10*time.Second); err != nil {
				return core.DoctorResult{}, err
			}
			runtime = "ssh_reachable"
		}
	}
	return core.DoctorResult{
		Provider: "parallels",
		Message:  fmt.Sprintf("cli=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=%s hosts=%d template=%s", len(servers), runtime, len(b.Cfg.Parallels.Hosts), blank(b.Cfg.Parallels.Template, "-")),
	}, nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *leaseBackend) ReleaseLeaseWithOutcome(ctx context.Context, req core.ReleaseLeaseRequest) (core.ReleaseLeaseOutcome, error) {
	var outcome core.ReleaseLeaseOutcome
	return outcome, b.releaseLease(ctx, req, &outcome)
}

func (b *leaseBackend) releaseLease(ctx context.Context, req core.ReleaseLeaseRequest, outcome *core.ReleaseLeaseOutcome) error {
	if leaseID := strings.TrimSpace(req.Lease.LeaseID); leaseID != "" {
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return err
		}
		if exists && parallelsFixedLeaseKind.IsFixedClaim(claim) {
			return b.releaseFixed(ctx, claim, outcome)
		}
	}
	if req.Lease.Server.Name != "" && !strings.HasPrefix(req.Lease.Server.Name, "crabbox-") {
		return core.Exit(2, "refusing to release non-Crabbox Parallels VM %q", req.Lease.Server.Name)
	}
	id := shared.FirstNonBlankTrimmed(req.Lease.Server.CloudID, req.Lease.LeaseID)
	cfg := b.configForLease(ctx, req.Lease)
	if err := requireExactParallelsClaim(req.Lease.LeaseID, id, parallelsHostName(cfg)); err != nil {
		return err
	}
	if err := core.NewParallelsClient(cfg, b.RT.Exec).Delete(ctx, id); err != nil {
		return err
	}
	outcome.Terminal = true
	core.RemoveLeaseClaim(req.Lease.LeaseID)
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *leaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	core.NewParallelsClient(b.configForLease(ctx, req.Lease), b.RT.Exec).SetLeaseLabels(shared.FirstNonBlankTrimmed(req.Lease.LeaseID, server.Labels["lease"]), server.Labels)
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip vm id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		leaseID := server.Labels["lease"]
		cfg := b.configForLease(ctx, core.LeaseTarget{
			Server:  server,
			LeaseID: leaseID,
		})
		owned, err := exactParallelsClaimOwned(leaseID, server.CloudID, parallelsHostName(cfg))
		if err != nil {
			return err
		}
		if !owned {
			fmt.Fprintf(b.RT.Stderr, "skip vm id=%s name=%s reason=ownership: missing exact local claim\n", server.DisplayID(), server.Name)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete vm id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		claim, claimExists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return err
		}
		if claimExists && parallelsFixedLeaseKind.IsFixedClaim(claim) {
			// A fixed ID is a single-use operation identity: sweep the VM
			// through the durable release path so its terminal tombstone
			// survives and the ID can never be replayed into a second VM.
			if err := b.releaseFixed(ctx, claim, &core.ReleaseLeaseOutcome{}); err != nil {
				return err
			}
			continue
		}
		client := core.NewParallelsClient(cfg, b.RT.Exec)
		if err := client.Delete(ctx, server.CloudID); err != nil {
			return err
		}
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}
	return nil
}

func requireExactParallelsClaim(leaseID, vmID, host string) error {
	owned, err := exactParallelsClaimOwned(leaseID, vmID, host)
	if err != nil {
		return err
	}
	if !owned {
		return parallelsOwnershipError(leaseID, vmID, host)
	}
	return nil
}

func parallelsOwnershipError(leaseID, vmID, host string) error {
	return core.Exit(4, "parallels lease %q has no exact local claim bound to VM %q on host %q; adopt it with an explicit --reclaim reuse before stop", strings.TrimSpace(leaseID), strings.TrimSpace(vmID), strings.TrimSpace(host))
}

func exactParallelsClaimOwned(leaseID, vmID, host string) (bool, error) {
	leaseID = strings.TrimSpace(leaseID)
	vmID = strings.TrimSpace(vmID)
	host = strings.TrimSpace(host)
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, "parallels")
	if err != nil {
		return false, err
	}
	return ok && exact && claim.LeaseID == leaseID && claim.CloudID == vmID && strings.TrimSpace(claim.Labels["host"]) == host, nil
}

func parallelsProxyCommand(cfg core.Config, guestIP string) string {
	host := cfg.Parallels.Host
	if cfg.Parallels.HostUser != "" {
		host = cfg.Parallels.HostUser + "@" + host
	}
	args := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-W", guestIP + ":%p"}
	if cfg.Parallels.HostKey != "" {
		args = append([]string{"ssh", "-i", cfg.Parallels.HostKey, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-W", guestIP + ":%p"}, host)
	} else {
		args = append(args, host)
	}
	return strings.Join(core.ShellWords(args), " ")
}

func (b *leaseBackend) configForLease(ctx context.Context, lease core.LeaseTarget) core.Config {
	host := strings.TrimSpace(lease.Server.Labels["host"])
	if host != "" {
		for _, candidate := range core.ParallelsCandidateConfigs(b.Cfg) {
			if parallelsHostName(candidate) == host {
				return candidate
			}
		}
	}
	id := shared.FirstNonBlankTrimmed(lease.Server.CloudID, lease.LeaseID)
	if id != "" {
		for _, candidate := range core.ParallelsCandidateConfigs(b.Cfg) {
			client := core.NewParallelsClient(candidate, b.RT.Exec)
			if _, err := client.GetVM(ctx, id); err == nil {
				return candidate
			}
		}
	}
	return b.Cfg
}

func parallelsHostName(cfg core.Config) string {
	return core.ParallelsHostRefForConfig(cfg)
}

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func parallelsLeaseFromVMName(name string) (string, string) {
	rest := strings.TrimPrefix(name, "crabbox-")
	if rest == name {
		return "", ""
	}
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) < 2 || parts[0] != "cbx" {
		return "", core.NormalizeLeaseSlug(rest)
	}
	leaseID := "cbx_" + parts[1]
	slug := ""
	if len(parts) == 3 {
		slug = core.NormalizeLeaseSlug(parts[2])
	}
	return leaseID, slug
}
