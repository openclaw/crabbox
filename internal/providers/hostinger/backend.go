package hostinger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"
)

type leaseBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client hostingerAPI

	skipSSHWait bool
}

var (
	hostingerRunSSHQuiet             = runSSHQuiet
	hostingerWaitForSSHReady         = waitForSSHReady
	hostingerLookPath                = exec.LookPath
	hostingerSleep                   = time.Sleep
	hostingerPurchaseRecoveryTimeout = time.Minute
	hostingerStopWaitTimeout         = 2 * time.Minute
)

const (
	hostingerRecoveryLabel         = "recovery"
	hostingerRecoveryHostnameLabel = "hostinger_hostname"
	hostingerRecoveryAmbiguous     = "ambiguous-purchase"
	hostingerAdoptionPendingLabel  = "hostinger_adoption_pending"
)

func NewLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &leaseBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *leaseBackend) Spec() ProviderSpec { return b.spec }

func (b *leaseBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	return useStoredTestboxKey(&target.SSH, leaseID, sshKeyExplicit(&b.cfg))
}

func (b *leaseBackend) Acquire(ctx context.Context, req AcquireRequest) (lease LeaseTarget, err error) {
	cfg := b.configForRun()
	if err := validateHostingerWorkRoot(cfg); err != nil {
		return LeaseTarget{}, err
	}
	if err := validateHostingerReleaseAction(cfg); err != nil {
		return LeaseTarget{}, err
	}
	if !cfg.Hostinger.AllowPurchase {
		return LeaseTarget{}, exit(2, "provider=%s requires --hostinger-allow-purchase, CRABBOX_HOSTINGER_ALLOW_PURCHASE=true, or hostinger.allowPurchase=true in private user config before billable VPS purchase/setup", providerName)
	}
	if strings.TrimSpace(cfg.Hostinger.ItemID) == "" {
		return LeaseTarget{}, exit(2, "provider=%s requires hostinger item id", providerName)
	}
	if strings.TrimSpace(cfg.Hostinger.TemplateID) == "" {
		return LeaseTarget{}, exit(2, "provider=%s requires hostinger template id", providerName)
	}
	if strings.TrimSpace(cfg.Hostinger.DataCenterID) == "" {
		return LeaseTarget{}, exit(2, "provider=%s requires hostinger data center id", providerName)
	}
	templateID, err := hostingerIntegerID("template id", cfg.Hostinger.TemplateID)
	if err != nil {
		return LeaseTarget{}, err
	}
	dataCenterID, err := hostingerIntegerID("data center id", cfg.Hostinger.DataCenterID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := validateHostingerLocalTools(); err != nil {
		return LeaseTarget{}, err
	}
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	options, err := loadHostingerPurchaseOptions(ctx, client)
	if err != nil {
		return LeaseTarget{}, err
	}
	paymentMethodID, err := validateHostingerPurchaseOptions(cfg, options)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	servers, err := b.listServers(ctx, client, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	hostname := hostingerHostname(cfg, leaseID, slug)
	if err := validateHostingerHostname(hostname); err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	recovery := hostingerRecoveryRecord{
		LeaseID:  leaseID,
		Slug:     slug,
		Hostname: hostname,
	}
	if err := writeHostingerRecoveryRecord(recovery); err != nil {
		removeStoredTestboxKey(leaseID)
		return LeaseTarget{}, exit(1, "persist hostinger purchase recovery record: %v", err)
	}
	purchasedVMID := ""
	claimPersisted := false
	var rollbackClaim LeaseClaim
	retainRecoveryKey := false
	defer func() {
		if err == nil {
			return
		}
		if purchasedVMID == "" {
			if !claimPersisted && !retainRecoveryKey {
				removeHostingerRecoveryRecord(leaseID)
				removeStoredTestboxKey(leaseID)
			}
			return
		}
		rollback := "retained"
		if !req.Keep {
			if !claimPersisted {
				stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				stopErr := b.stopVMAndWait(stopCtx, client, purchasedVMID)
				cancel()
				if stopErr == nil {
					rollback = "stopped"
				} else {
					rollback = "stop-failed: " + stopErr.Error()
				}
			} else if rollbackClaim.LeaseID == "" {
				rollback = "retained; claim-snapshot-unavailable"
			} else {
				stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				server := Server{
					CloudID:  purchasedVMID,
					Provider: providerName,
					Name:     hostname,
					Status:   "stopped",
					Labels:   rollbackClaim.Labels,
				}
				_, stopErr := b.stopClaimedVM(stopCtx, client, rollbackClaim, purchasedVMID, server)
				cancel()
				if stopErr == nil {
					rollback = "stopped"
				} else {
					rollback = "retained; stop-skipped: " + stopErr.Error()
				}
			}
		}
		err = exit(1, "hostinger VPS provisioning failed after purchase: lease=%s vm=%s rollback=%s billing=still-owned: %v", leaseID, purchasedVMID, rollback, err)
	}()
	cfg.SSHKey = keyPath
	keyName := fmt.Sprintf("crabbox-%s", leaseID)
	persistPendingClaim := func() error {
		if claimPersisted {
			return nil
		}
		server := hostingerServer(hostingerVM{Hostname: hostname, State: "provisioning"}, leaseID, slug, cfg, req.Keep)
		server.Labels[hostingerRecoveryLabel] = hostingerRecoveryAmbiguous
		server.Labels[hostingerRecoveryHostnameLabel] = hostname
		claim, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, LeaseClaim{}, false)
		if err != nil {
			return exit(1, "persist hostinger ambiguous purchase recovery claim: %v", err)
		}
		claimPersisted = true
		rollbackClaim = claim
		return nil
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s hostname=%s item=%s template=%s data_center=%s keep=%v\n",
		providerName, leaseID, slug, hostname, cfg.Hostinger.ItemID, cfg.Hostinger.TemplateID, cfg.Hostinger.DataCenterID, req.Keep)
	vm, err := client.PurchaseVM(ctx, hostingerPurchaseInput{
		ItemID:          cfg.Hostinger.ItemID,
		PaymentMethodID: paymentMethodID,
		Setup: hostingerSetupInput{
			TemplateID:    templateID,
			DataCenterID:  dataCenterID,
			Hostname:      hostname,
			EnableBackups: false,
			PublicKey: &hostingerSetupPublicKey{
				Name: keyName,
				Key:  publicKey,
			},
		},
	})
	if err != nil {
		if !hostingerPurchaseMayHaveSucceeded(err) {
			return LeaseTarget{}, exit(1, "hostinger purchase vps failed: %v", err)
		}
		retainRecoveryKey = true
		purchaseErr := err
		pendingClaimErr := persistPendingClaim()
		recoveryCtx, cancel := context.WithTimeout(context.Background(), hostingerPurchaseRecoveryTimeout)
		recovered, found, recoveryErr := b.recoverVMByHostname(recoveryCtx, client, hostname)
		cancel()
		if recoveryErr != nil {
			if pendingClaimErr != nil {
				return LeaseTarget{}, exit(1, "hostinger purchase outcome is unknown; recovery claim failed and key retained for lease=%s hostname=%s key=%s: purchase_error=%v claim_error=%v recovery_error=%v", leaseID, hostname, keyPath, purchaseErr, pendingClaimErr, recoveryErr)
			}
			return LeaseTarget{}, exit(1, "hostinger purchase outcome is unknown; recovery claim retained for lease=%s hostname=%s: purchase_error=%v recovery_error=%v", leaseID, hostname, purchaseErr, recoveryErr)
		}
		if !found {
			if pendingClaimErr != nil {
				return LeaseTarget{}, exit(1, "hostinger purchase outcome is unknown; recovery claim failed and key retained for lease=%s hostname=%s key=%s: purchase_error=%v claim_error=%v", leaseID, hostname, keyPath, purchaseErr, pendingClaimErr)
			}
			return LeaseTarget{}, exit(1, "hostinger purchase outcome is unknown; recovery claim retained for lease=%s hostname=%s: %v", leaseID, hostname, purchaseErr)
		}
		vm = recovered
		fmt.Fprintf(b.rt.Stderr, "recovered ambiguous hostinger purchase lease=%s vm=%s hostname=%s\n", leaseID, vm.IDString(), hostname)
	}
	if vm.IDString() == "" {
		retainRecoveryKey = true
		pendingClaimErr := persistPendingClaim()
		recoveryCtx, cancel := context.WithTimeout(context.Background(), hostingerPurchaseRecoveryTimeout)
		recovered, found, recoveryErr := b.recoverVMByHostname(recoveryCtx, client, hostname)
		cancel()
		if recoveryErr != nil {
			if pendingClaimErr != nil {
				return LeaseTarget{}, exit(1, "hostinger purchase returned no vm id; recovery claim failed and key retained for lease=%s hostname=%s key=%s: claim_error=%v recovery_error=%v", leaseID, hostname, keyPath, pendingClaimErr, recoveryErr)
			}
			return LeaseTarget{}, exit(1, "hostinger purchase returned no vm id; recovery claim retained for lease=%s hostname=%s: %v", leaseID, hostname, recoveryErr)
		}
		if !found {
			if pendingClaimErr != nil {
				return LeaseTarget{}, exit(1, "hostinger purchase returned no vm id; recovery claim failed and key retained for lease=%s hostname=%s key=%s: claim_error=%v", leaseID, hostname, keyPath, pendingClaimErr)
			}
			return LeaseTarget{}, exit(1, "hostinger purchase returned no vm id; recovery claim retained for lease=%s hostname=%s", leaseID, hostname)
		}
		vm = recovered
	}
	purchasedVMID = vm.IDString()
	recovery.VMID = purchasedVMID
	if err := writeHostingerRecoveryRecord(recovery); err != nil {
		return LeaseTarget{}, exit(1, "bind hostinger purchase recovery record lease=%s vm=%s key=%s: %v", leaseID, purchasedVMID, keyPath, err)
	}
	server := hostingerServer(vm, leaseID, slug, cfg, req.Keep)
	if claimPersisted {
		updated, updateErr := updateLeaseClaimEndpointIfUnchanged(leaseID, rollbackClaim, server, SSHTarget{})
		if updateErr != nil {
			return LeaseTarget{}, exit(1, "bind hostinger recovered VPS claim: %v", updateErr)
		}
		rollbackClaim = updated
	} else {
		claim, claimErr := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, LeaseClaim{}, false)
		if claimErr != nil {
			return LeaseTarget{}, exit(1, "persist hostinger paid VPS claim: %v", claimErr)
		}
		claimPersisted = true
		rollbackClaim = claim
	}
	removeHostingerRecoveryRecord(leaseID)
	ready, waitErr := b.waitForVM(ctx, client, vm.IDString())
	if waitErr != nil {
		return LeaseTarget{}, waitErr
	}
	vm = ready
	lease, err = b.leaseFromVM(cfg, vm, leaseID, slug, req.Keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	updated, updateErr := updateLeaseClaimEndpointIfUnchanged(leaseID, rollbackClaim, lease.Server, lease.SSH)
	if updateErr != nil {
		return LeaseTarget{}, exit(1, "persist hostinger VPS endpoint: %v", updateErr)
	}
	rollbackClaim = updated
	if !b.skipSSHWait {
		if err := b.ensureBootstrap(ctx, cfg, lease, "bootstrap"); err != nil {
			return LeaseTarget{}, err
		}
		if err := hostingerWaitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s vm=%s state=ready\n", leaseID, vm.IDString())
	return lease, nil
}

func (b *leaseBackend) Resolve(ctx context.Context, req ResolveRequest) (lease LeaseTarget, err error) {
	cfg := b.configForRun()
	if err := validateHostingerWorkRoot(cfg); err != nil {
		return LeaseTarget{}, err
	}
	if err := validateHostingerReleaseAction(cfg); err != nil {
		return LeaseTarget{}, err
	}
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	vm, leaseID, slug, err := b.resolveVM(ctx, client, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg, err = b.configForLeaseClaim(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		server, err := b.serverFromVMWithClaim(vm, leaseID, slug, cfg, true)
		if err != nil {
			return LeaseTarget{}, err
		}
		owned, err := hostingerReleaseOwned(vm)
		if err != nil {
			return LeaseTarget{}, err
		}
		if !owned {
			return LeaseTarget{}, exit(2, "refusing to stop unowned hostinger vps %s; a matching local Crabbox lease claim is required", vm.IDString())
		}
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	if req.StatusOnly && (!req.ReadyProbe || !vm.Ready() || vm.Host() == "") {
		server, err := b.serverFromVMWithClaim(vm, leaseID, slug, cfg, true)
		if err != nil {
			return LeaseTarget{}, err
		}
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	if vm.Stopped() && req.Repo.Root == "" && !req.Prepare {
		server, err := b.serverFromVMWithClaim(vm, leaseID, slug, cfg, true)
		if err != nil {
			return LeaseTarget{}, err
		}
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	var previousClaim, repoClaim, rollbackExpectedClaim LeaseClaim
	var rollbackRepoClaim bool
	adoptionPending := false
	defer func() {
		if err == nil || !rollbackRepoClaim {
			return
		}
		restored := hostingerOwnershipRollbackClaim(rollbackExpectedClaim, previousClaim)
		if restoreErr := replaceLeaseClaimIfUnchanged(leaseID, rollbackExpectedClaim, restored); restoreErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: restore Hostinger lease claim %s after resolve failure: %v\n", leaseID, restoreErr)
		}
	}()
	if req.Repo.Root != "" {
		claimServer, err := b.serverFromVMWithClaim(vm, leaseID, slug, cfg, true)
		if err != nil {
			return LeaseTarget{}, err
		}
		expected, expectedExists, err := resolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil {
			return LeaseTarget{}, err
		}
		previousClaim = expected
		adoptionPending = !hostingerClaimOwned(expected, expectedExists, vm.IDString())
		if adoptionPending {
			claimServer.Labels[hostingerAdoptionPendingLabel] = "true"
		}
		repoClaim, err = claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, claimServer, SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, expected, expectedExists)
		if err != nil {
			return LeaseTarget{}, err
		}
		rollbackExpectedClaim = repoClaim
		rollbackRepoClaim = !adoptionPending
		removeHostingerRecoveryRecord(leaseID)
		vm, err = client.GetVM(ctx, vm.IDString())
		if err != nil {
			return LeaseTarget{}, exit(1, "hostinger refresh claimed vps %s failed: %v", claimServer.CloudID, err)
		}
	}
	started := false
	var restartClaim LeaseClaim
	rollbackStarted := func(id string, cause error) error {
		stoppedClaim, rollbackErr := b.rollbackStartedVM(client, id, restartClaim, cfg, cause)
		if rollbackRepoClaim {
			if stoppedClaim.LeaseID == "" {
				rollbackRepoClaim = false
			} else {
				rollbackExpectedClaim = stoppedClaim
			}
		}
		return rollbackErr
	}
	if vm.Stopped() {
		vmID := vm.IDString()
		restartClaim, err = b.updateClaimState(leaseID, cfg, "provisioning", true)
		if err != nil {
			return LeaseTarget{}, fmt.Errorf("prepare hostinger restart lease=%s: %w", leaseID, err)
		}
		if rollbackRepoClaim {
			rollbackExpectedClaim = restartClaim
		}
		if err := client.StartVM(ctx, vmID); err != nil {
			stoppedClaim, claimErr := b.updateClaimStateIfUnchanged(restartClaim, cfg, "stopped", false)
			if claimErr != nil {
				return LeaseTarget{}, exit(1, "hostinger start vps %s failed: %v; claim update failed: %v", vmID, err, claimErr)
			}
			if rollbackRepoClaim {
				rollbackExpectedClaim = stoppedClaim
			}
			return LeaseTarget{}, exit(1, "hostinger start vps %s failed: %v", vmID, err)
		}
		started = true
		vm, err = b.waitForVM(ctx, client, vmID)
		if err != nil {
			return LeaseTarget{}, rollbackStarted(vmID, err)
		}
	} else if !vm.Ready() {
		return LeaseTarget{}, exit(5, "hostinger vps %s is not runnable; state=%s", vm.IDString(), firstNonBlank(vm.State, vm.Status, "unknown"))
	}
	lease, err = b.leaseFromVM(cfg, vm, leaseID, slug, true)
	if err != nil {
		if started {
			return LeaseTarget{}, rollbackStarted(vm.IDString(), err)
		}
		return LeaseTarget{}, err
	}
	sshValidated := b.skipSSHWait
	if started && !b.skipSSHWait {
		transport := lease.SSH
		transport.ReadyCheck = "true"
		if err := hostingerWaitForSSHReady(ctx, &transport, b.rt.Stderr, "restart", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, rollbackStarted(vm.IDString(), err)
		}
		sshValidated = true
	}
	if req.Prepare && !b.skipSSHWait {
		if err := b.ensureBootstrap(ctx, cfg, lease, "resolve"); err != nil {
			if started {
				return LeaseTarget{}, rollbackStarted(vm.IDString(), err)
			}
			return LeaseTarget{}, err
		}
		sshValidated = true
	}
	if adoptionPending && !sshValidated {
		transport := lease.SSH
		transport.ReadyCheck = "true"
		if err := hostingerWaitForSSHReady(ctx, &transport, b.rt.Stderr, "adoption", bootstrapWaitTimeout(cfg)); err != nil {
			if started {
				return LeaseTarget{}, rollbackStarted(vm.IDString(), err)
			}
			return LeaseTarget{}, err
		}
		sshValidated = true
	}
	if started || req.Repo.Root != "" {
		expected := repoClaim
		if started {
			expected = restartClaim
		}
		server := lease.Server
		server.Labels = touchDirectLeaseLabels(expected.Labels, cfg, "running", time.Now().UTC())
		if adoptionPending {
			delete(server.Labels, hostingerAdoptionPendingLabel)
		}
		if _, err := updateLeaseClaimEndpointIfUnchanged(expected.LeaseID, expected, server, lease.SSH); err != nil {
			if started {
				return LeaseTarget{}, rollbackStarted(vm.IDString(), err)
			}
			return LeaseTarget{}, err
		}
		rollbackRepoClaim = false
	}
	return lease, nil
}

func hostingerOwnershipRollbackClaim(current, previous LeaseClaim) LeaseClaim {
	restored := previous
	restored.Labels = make(map[string]string, len(previous.Labels))
	for key, value := range previous.Labels {
		restored.Labels[key] = value
	}
	restored.TailscaleTags = append([]string(nil), previous.TailscaleTags...)
	restored.CacheVolumes = append([]string(nil), previous.CacheVolumes...)
	restored.CloudID = current.CloudID
	restored.TailscaleIPv4 = current.TailscaleIPv4
	restored.TailscaleFQDN = current.TailscaleFQDN
	restored.SSHHost = current.SSHHost
	restored.SSHPort = current.SSHPort
	restored.BridgeURL = current.BridgeURL
	if current.Labels["state"] != previous.Labels["state"] {
		for _, key := range []string{"state", "created_at", "last_touched_at", "expires_at"} {
			if value, ok := current.Labels[key]; ok {
				restored.Labels[key] = value
			} else {
				delete(restored.Labels, key)
			}
		}
	}
	return restored
}

func (b *leaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	return b.listServers(ctx, client, req.All)
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := validateHostingerReleaseAction(b.configForRun()); err != nil {
		return err
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	vmID := strings.TrimSpace(req.Lease.Server.CloudID)
	var vm hostingerVM
	if vmID == "" {
		vm, _, _, err = b.resolveVM(ctx, client, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		vmID = vm.IDString()
	} else {
		vm, err = client.GetVM(ctx, vmID)
		if err != nil {
			return exit(1, "hostinger get vps %s before release failed: %v", vmID, err)
		}
	}
	if vmID == "" {
		return exit(2, "provider=%s release requires a vm id", providerName)
	}
	owned, err := hostingerReleaseOwned(vm)
	if err != nil {
		return err
	}
	if !owned {
		return exit(2, "refusing to stop unowned hostinger vps %s; a matching local Crabbox lease claim is required", vmID)
	}
	claim, claimOK, err := resolveLeaseClaimForProviderCloudID(vmID, providerName)
	if err != nil {
		return err
	}
	if !claimOK && req.Lease.LeaseID != "" {
		candidate, ok, resolveErr := resolveLeaseClaimForProvider(req.Lease.LeaseID, providerName)
		if resolveErr != nil {
			return resolveErr
		}
		if ok && (candidate.CloudID == "" || candidate.CloudID == vmID) {
			claim, claimOK = candidate, true
		}
	}
	if claimOK {
		server := req.Lease.Server
		server.CloudID = vmID
		server.Provider = providerName
		server.Status = "stopped"
		server.Labels = hostingerStoppedClaimLabels(claim.Labels)
		_, err := b.stopClaimedVM(ctx, client, claim, vmID, server)
		if err != nil {
			return fmt.Errorf("finalize hostinger release lease=%s: %w", claim.LeaseID, err)
		}
		return nil
	}
	return exit(2, "refusing to stop unowned hostinger vps %s; a matching local Crabbox lease claim is required", vmID)
}

func (b *leaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("stopped lease=%s vm=%s name=%s billing=still-owned", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *leaseBackend) RetainLeaseClaimAfterRelease(LeaseTarget) bool {
	return true
}

func validateHostingerReleaseAction(cfg Config) error {
	if strings.ToLower(strings.TrimSpace(cfg.Hostinger.ReleaseAction)) != "stop" {
		return exit(2, "provider=%s release action must be stop", providerName)
	}
	return nil
}

func validateHostingerLocalTools() error {
	for _, tool := range []string{"ssh", "ssh-keygen", "rsync"} {
		if _, err := hostingerLookPath(tool); err != nil {
			return exit(2, "provider=%s requires local %s before billable VPS purchase/setup", providerName, tool)
		}
	}
	return nil
}

var hostingerSSHUserPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)

func validateHostingerWorkRoot(cfg Config) error {
	workRoot := strings.TrimSpace(cfg.WorkRoot)
	if workRoot != cfg.WorkRoot || workRoot == "" || !strings.HasPrefix(workRoot, "/") || path.Clean(workRoot) != workRoot {
		return exit(2, "provider=%s work root must be a canonical absolute Linux path, got %q", providerName, cfg.WorkRoot)
	}
	roots := []string{"/work/crabbox", "/workspaces/crabbox", "/var/lib/crabbox/work", "/opt/crabbox"}
	user := strings.TrimSpace(cfg.SSHUser)
	if user != cfg.SSHUser || !hostingerSSHUserPattern.MatchString(user) {
		return exit(2, "provider=%s SSH user must be a valid Linux login name, got %q", providerName, cfg.SSHUser)
	}
	roots = append(roots, "/home/"+user+"/crabbox")
	for _, root := range roots {
		if workRoot == root || strings.HasPrefix(workRoot, root+"/") {
			return nil
		}
	}
	return exit(2, "provider=%s work root %q is outside approved Crabbox roots", providerName, workRoot)
}

func hostingerStoppedClaimLabels(labels map[string]string) map[string]string {
	stopped := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		stopped[key] = value
	}
	stopped["state"] = "stopped"
	return stopped
}

func (b *leaseBackend) stopClaimedVM(ctx context.Context, client hostingerAPI, claim LeaseClaim, vmID string, server Server) (LeaseClaim, error) {
	server.CloudID = vmID
	server.Provider = providerName
	server.Status = "stopped"
	server.Labels = hostingerStoppedClaimLabels(server.Labels)
	return updateLeaseClaimEndpointIfUnchangedAfter(claim.LeaseID, claim, server, SSHTarget{}, func() error {
		return b.stopVMAndWait(ctx, client, vmID)
	})
}

func (b *leaseBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.configForRun(), req.State, time.Now().UTC())
	if err := updateLeaseClaimEndpoint(req.Lease.LeaseID, server, req.Lease.SSH); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := b.api()
	if err != nil {
		return err
	}
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return err
	}
	cfg := b.configForRun()
	now := time.Now().UTC()
	for _, vm := range vms {
		leaseID, slug, claimed, err := hostingerLeaseIdentityWithClaim(vm, cfg)
		if err != nil {
			return err
		}
		server := hostingerServer(vm, leaseID, slug, cfg, true)
		if !claimed {
			reason := "no-local-cleanup-claim"
			if !hostingerOwnedServer(server, cfg) {
				reason = "not-crabbox-owned"
			}
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil {
			return err
		}
		if !ok || len(claim.Labels) == 0 {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=no-local-cleanup-claim\n", server.DisplayID(), server.Name)
			continue
		}
		if hostingerAdoptionPending(claim) {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=adoption-pending\n", server.DisplayID(), server.Name)
			continue
		}
		if claim.CloudID != server.CloudID {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=claim-cloud-id-mismatch\n", server.DisplayID(), server.Name)
			continue
		}
		if vm.Stopped() {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=state=stopped\n", server.DisplayID(), server.Name)
			continue
		}
		server.Labels = claim.Labels
		ok, reason := shouldCleanupServer(server, now)
		if !ok {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stderr, "stop server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
		if !req.DryRun {
			server.Status = "stopped"
			server.Labels = hostingerStoppedClaimLabels(claim.Labels)
			if _, err := b.stopClaimedVM(ctx, client, claim, server.CloudID, server); err != nil {
				return fmt.Errorf("finalize hostinger cleanup lease=%s: %w", leaseID, err)
			}
		}
	}
	return nil
}

func (b *leaseBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if strings.TrimSpace(b.cfg.Hostinger.APIToken) == "" {
		return DoctorResult{}, exit(2, "provider=%s requires HOSTINGER_API_TOKEN (CRABBOX_HOSTINGER_API_TOKEN also accepted)", providerName)
	}
	client, err := b.api()
	if err != nil {
		return DoctorResult{}, err
	}
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return DoctorResult{}, exit(1, "hostinger list vms failed: %v", err)
	}
	options, err := loadHostingerPurchaseOptions(ctx, client)
	if err != nil {
		return DoctorResult{}, err
	}
	result := inventoryDoctorResult(providerName, len(vms))
	result.Message += " purchase=explicit release=stop"
	purchaseStatus := "ok"
	purchaseMessage := fmt.Sprintf("priced_items=%d payment_methods=%d templates=%d data_centers=%d", hostingerCatalogPriceCount(options.catalog), len(options.paymentMethods), len(options.templates), len(options.dataCenters))
	cfg := b.configForRun()
	missing := make([]string, 0, 3)
	if strings.TrimSpace(cfg.Hostinger.ItemID) == "" {
		missing = append(missing, "item_id")
	}
	if strings.TrimSpace(cfg.Hostinger.TemplateID) == "" {
		missing = append(missing, "template_id")
	}
	if strings.TrimSpace(cfg.Hostinger.DataCenterID) == "" {
		missing = append(missing, "data_center_id")
	}
	if err := validateHostingerConfiguredPurchaseOptions(cfg, options); err != nil {
		purchaseStatus = "failed"
		purchaseMessage = err.Error()
	} else if len(missing) > 0 {
		purchaseStatus = "warning"
		purchaseMessage += " configuration=incomplete missing=" + strings.Join(missing, ",")
	} else if _, err := validateHostingerPurchaseOptions(cfg, options); err != nil {
		purchaseStatus = "failed"
		purchaseMessage = err.Error()
	}
	result.Checks = append(result.Checks, DoctorCheck{
		Status:  "ok",
		Check:   "provider",
		Message: result.Message,
	}, DoctorCheck{
		Status:  purchaseStatus,
		Check:   "purchase-options",
		Message: purchaseMessage,
		Details: map[string]string{
			"configured_item_id":           blank(strings.TrimSpace(b.cfg.Hostinger.ItemID), "missing"),
			"configured_payment_method_id": blank(strings.TrimSpace(b.cfg.Hostinger.PaymentMethodID), "auto"),
			"configured_template_id":       blank(strings.TrimSpace(b.cfg.Hostinger.TemplateID), "missing"),
			"configured_data_center_id":    blank(strings.TrimSpace(b.cfg.Hostinger.DataCenterID), "missing"),
			"priced_items":                 summarizeHostingerCatalog(options.catalog),
			"payment_methods":              summarizeHostingerPaymentMethods(options.paymentMethods),
			"templates":                    summarizeHostingerTemplates(options.templates),
			"data_centers":                 summarizeHostingerDataCenters(options.dataCenters),
		},
	})
	return result, nil
}

func (b *leaseBackend) api() (hostingerAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newClient(b.cfg, b.rt)
}

func (b *leaseBackend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.Hostinger.APIURL == "" {
		cfg.Hostinger.APIURL = "https://developers.hostinger.com"
	}
	if cfg.Hostinger.HostnamePrefix == "" {
		cfg.Hostinger.HostnamePrefix = "crabbox"
	}
	if cfg.Hostinger.User == "" {
		cfg.Hostinger.User = "root"
	}
	if cfg.Hostinger.WorkRoot == "" {
		cfg.Hostinger.WorkRoot = effectiveHostingerWorkRoot(*cfg)
	}
	if cfg.Hostinger.ReleaseAction == "" {
		cfg.Hostinger.ReleaseAction = "stop"
	}
	cfg.SSHUser = cfg.Hostinger.User
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.WorkRoot = cfg.Hostinger.WorkRoot
}

func hostingerHostname(cfg Config, leaseID, slug string) string {
	prefix := strings.Trim(strings.ToLower(cfg.Hostinger.HostnamePrefix), "- ")
	if prefix == "" {
		prefix = "crabbox"
	}
	return fmt.Sprintf("%s-%s-%s", prefix, slug, strings.TrimPrefix(leaseID, "cbx_"))
}

func validateHostingerHostname(hostname string) error {
	if len(hostname) == 0 || len(hostname) > 63 {
		return exit(2, "provider=%s generated hostname must contain 1-63 characters, got %q", providerName, hostname)
	}
	for i, r := range hostname {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
		if !valid {
			return exit(2, "provider=%s generated hostname contains invalid character %q in %q", providerName, r, hostname)
		}
		if (i == 0 || i == len(hostname)-1) && r == '-' {
			return exit(2, "provider=%s generated hostname must start and end with a letter or number, got %q", providerName, hostname)
		}
	}
	return nil
}

func (b *leaseBackend) listServers(ctx context.Context, client hostingerAPI, all bool) ([]Server, error) {
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	cfg := b.configForRun()
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		leaseID, slug, claimed, err := hostingerLeaseIdentityWithClaim(vm, cfg)
		if err != nil {
			return nil, err
		}
		server, err := b.serverFromVMWithClaim(vm, leaseID, slug, cfg, true)
		if err != nil {
			return nil, err
		}
		if all || claimed {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func (b *leaseBackend) findVMByHostname(ctx context.Context, client hostingerAPI, hostname string) (hostingerVM, bool, error) {
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return hostingerVM{}, false, err
	}
	var match hostingerVM
	for _, vm := range vms {
		if vm.NameValue() != hostname {
			continue
		}
		if match.IDString() != "" {
			return hostingerVM{}, false, fmt.Errorf("multiple Hostinger VPSs match hostname %s", hostname)
		}
		match = vm
	}
	if match.IDString() == "" {
		return hostingerVM{}, false, nil
	}
	return match, true, nil
}

type hostingerPurchaseOptions struct {
	catalog        []hostingerCatalogItem
	paymentMethods []hostingerPaymentMethod
	templates      []hostingerTemplate
	dataCenters    []hostingerDataCenter
}

func loadHostingerPurchaseOptions(ctx context.Context, client hostingerAPI) (hostingerPurchaseOptions, error) {
	var options hostingerPurchaseOptions
	var err error
	options.catalog, err = client.ListCatalog(ctx)
	if err != nil {
		return hostingerPurchaseOptions{}, exit(1, "hostinger list catalog failed: %v", err)
	}
	options.paymentMethods, err = client.ListPaymentMethods(ctx)
	if err != nil {
		return hostingerPurchaseOptions{}, exit(1, "hostinger list payment methods failed: %v", err)
	}
	options.templates, err = client.ListTemplates(ctx)
	if err != nil {
		return hostingerPurchaseOptions{}, exit(1, "hostinger list templates failed: %v", err)
	}
	options.dataCenters, err = client.ListDataCenters(ctx)
	if err != nil {
		return hostingerPurchaseOptions{}, exit(1, "hostinger list data centers failed: %v", err)
	}
	return options, nil
}

func validateHostingerPurchaseOptions(cfg Config, options hostingerPurchaseOptions) (int64, error) {
	if err := validateHostingerConfiguredPurchaseOptions(cfg, options); err != nil {
		return 0, err
	}
	itemID := strings.TrimSpace(cfg.Hostinger.ItemID)
	if itemID == "" {
		return 0, exit(2, "provider=%s configured item id %q is not a current priced VPS item; available=%s", providerName, blank(itemID, "missing"), blank(summarizeHostingerCatalog(options.catalog), "none"))
	}

	templateID := strings.TrimSpace(cfg.Hostinger.TemplateID)
	if templateID == "" {
		return 0, exit(2, "provider=%s configured template id %q is unavailable; available=%s", providerName, blank(templateID, "missing"), blank(summarizeHostingerTemplates(options.templates), "none"))
	}

	dataCenterID := strings.TrimSpace(cfg.Hostinger.DataCenterID)
	if dataCenterID == "" {
		return 0, exit(2, "provider=%s configured data center id %q is unavailable; available=%s", providerName, blank(dataCenterID, "missing"), blank(summarizeHostingerDataCenters(options.dataCenters), "none"))
	}

	configuredPaymentID := strings.TrimSpace(cfg.Hostinger.PaymentMethodID)
	if configuredPaymentID != "" {
		return hostingerIntegerID("payment method id", configuredPaymentID)
	}
	var selected string
	for _, method := range options.paymentMethods {
		if !method.IsDefault || method.IsExpired || method.IsSuspended {
			continue
		}
		id := hostingerIDString(method.ID)
		if id == "" {
			continue
		}
		if selected != "" {
			return 0, exit(2, "provider=%s has multiple active default payment methods; set --hostinger-payment-method-id explicitly", providerName)
		}
		selected = id
	}
	if selected == "" {
		return 0, exit(2, "provider=%s requires an active default Hostinger payment method or --hostinger-payment-method-id; available=%s", providerName, blank(summarizeHostingerPaymentMethods(options.paymentMethods), "none"))
	}
	return hostingerIntegerID("payment method id", selected)
}

func validateHostingerConfiguredPurchaseOptions(cfg Config, options hostingerPurchaseOptions) error {
	itemID := strings.TrimSpace(cfg.Hostinger.ItemID)
	if itemID != "" {
		found := false
		for _, item := range options.catalog {
			for _, price := range item.Prices {
				if price.ID == itemID {
					found = true
					break
				}
			}
		}
		if !found {
			return exit(2, "provider=%s configured item id %q is not a current priced VPS item; available=%s", providerName, itemID, blank(summarizeHostingerCatalog(options.catalog), "none"))
		}
	}

	templateID := strings.TrimSpace(cfg.Hostinger.TemplateID)
	if templateID != "" {
		var selected hostingerTemplate
		for _, template := range options.templates {
			if hostingerIDString(template.ID) == templateID {
				selected = template
				break
			}
		}
		if hostingerIDString(selected.ID) == "" {
			return exit(2, "provider=%s configured template id %q is unavailable; available=%s", providerName, templateID, blank(summarizeHostingerTemplates(options.templates), "none"))
		}
		if !hostingerTemplateSupported(selected) {
			return exit(2, "provider=%s template %s=%s is unsupported; choose an Ubuntu or Debian template so Crabbox can install required SSH tools before readiness", providerName, templateID, firstNonBlank(selected.Name, selected.OS))
		}
	}

	dataCenterID := strings.TrimSpace(cfg.Hostinger.DataCenterID)
	if dataCenterID != "" {
		found := false
		for _, dataCenter := range options.dataCenters {
			if hostingerIDString(dataCenter.ID) == dataCenterID {
				found = true
				break
			}
		}
		if !found {
			return exit(2, "provider=%s configured data center id %q is unavailable; available=%s", providerName, dataCenterID, blank(summarizeHostingerDataCenters(options.dataCenters), "none"))
		}
	}

	paymentID := strings.TrimSpace(cfg.Hostinger.PaymentMethodID)
	if paymentID == "" {
		return nil
	}
	if _, err := hostingerIntegerID("payment method id", paymentID); err != nil {
		return err
	}
	for _, method := range options.paymentMethods {
		if hostingerIDString(method.ID) != paymentID {
			continue
		}
		if method.IsExpired || method.IsSuspended {
			return exit(2, "provider=%s configured payment method id %q is not active; available=%s", providerName, paymentID, blank(summarizeHostingerPaymentMethods(options.paymentMethods), "none"))
		}
		return nil
	}
	return exit(2, "provider=%s configured payment method id %q is unavailable; available=%s", providerName, paymentID, blank(summarizeHostingerPaymentMethods(options.paymentMethods), "none"))
}

func hostingerTemplateSupported(template hostingerTemplate) bool {
	name := strings.ToLower(strings.Join([]string{template.Name, template.OS}, " "))
	return strings.Contains(name, "ubuntu") || strings.Contains(name, "debian")
}

func (b *leaseBackend) recoverVMByHostname(ctx context.Context, client hostingerAPI, hostname string) (hostingerVM, bool, error) {
	for {
		vm, found, err := b.findVMByHostname(ctx, client, hostname)
		if err != nil || found {
			return vm, found, err
		}
		if ctx.Err() != nil {
			if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				return hostingerVM{}, false, nil
			}
			return hostingerVM{}, false, context.Cause(ctx)
		}
		hostingerSleep(3 * time.Second)
	}
}

func hostingerPurchaseMayHaveSucceeded(err error) bool {
	var apiErr *hostingerAPIError
	if !errors.As(err, &apiErr) {
		return true
	}
	if apiErr.StatusCode == http.StatusRequestTimeout || apiErr.StatusCode == http.StatusConflict {
		return true
	}
	return apiErr.StatusCode < 400 || apiErr.StatusCode >= 500
}

func hostingerCatalogPriceCount(items []hostingerCatalogItem) int {
	count := 0
	for _, item := range items {
		count += len(item.Prices)
	}
	return count
}

func summarizeHostingerCatalog(items []hostingerCatalogItem) string {
	const limit = 12
	values := make([]string, 0, limit)
	for _, item := range items {
		for _, price := range item.Prices {
			if len(values) == limit {
				return strings.Join(values, ",")
			}
			amount := price.FirstPeriodPrice
			if amount <= 0 {
				amount = price.Price
			}
			values = append(values, fmt.Sprintf("%s=%d%s/%d%s", price.ID, amount, strings.ToUpper(price.Currency), price.Period, price.PeriodUnit))
		}
	}
	return strings.Join(values, ",")
}

func summarizeHostingerPaymentMethods(methods []hostingerPaymentMethod) string {
	const limit = 20
	values := make([]string, 0, min(len(methods), limit))
	for _, method := range methods {
		if len(values) == limit {
			break
		}
		state := "active"
		if method.IsExpired {
			state = "expired"
		} else if method.IsSuspended {
			state = "suspended"
		}
		if method.IsDefault {
			state += "+default"
		}
		values = append(values, fmt.Sprintf("%s=%s(%s)", hostingerIDString(method.ID), firstNonBlank(method.Name, method.PaymentMethod, "payment-method"), state))
	}
	return strings.Join(values, ",")
}

func summarizeHostingerTemplates(templates []hostingerTemplate) string {
	const limit = 20
	values := make([]string, 0, min(len(templates), limit))
	for _, template := range templates {
		if len(values) == limit {
			break
		}
		values = append(values, fmt.Sprintf("%s=%s", hostingerIDString(template.ID), firstNonBlank(template.Name, template.OS)))
	}
	return strings.Join(values, ",")
}

func summarizeHostingerDataCenters(dataCenters []hostingerDataCenter) string {
	const limit = 20
	values := make([]string, 0, min(len(dataCenters), limit))
	for _, dataCenter := range dataCenters {
		if len(values) == limit {
			break
		}
		values = append(values, fmt.Sprintf("%s=%s", hostingerIDString(dataCenter.ID), firstNonBlank(dataCenter.Name, dataCenter.Location)))
	}
	return strings.Join(values, ",")
}

func (b *leaseBackend) resolveVM(ctx context.Context, client hostingerAPI, id string) (hostingerVM, string, string, error) {
	id = strings.TrimSpace(id)
	claim, claimOK, err := resolveLeaseClaimForProvider(id, providerName)
	if err != nil {
		return hostingerVM{}, "", "", err
	}
	if claimOK && claim.LeaseID != "" {
		if strings.TrimSpace(claim.CloudID) != "" {
			vm, getErr := client.GetVM(ctx, claim.CloudID)
			if getErr != nil {
				return hostingerVM{}, "", "", exit(1, "hostinger get claimed vps %s failed: %v", claim.CloudID, getErr)
			}
			return vm, claim.LeaseID, firstNonBlank(claim.Slug, hostingerLeaseIdentitySlug(vm, b.configForRun())), nil
		}
		id = firstNonBlank(claim.LeaseID, claim.Slug, id)
	}
	if id != "" && !strings.HasPrefix(id, "cbx_") {
		vm, err := client.GetVM(ctx, id)
		if err == nil && vm.IDString() != "" {
			leaseID, slug, _, claimErr := hostingerLeaseIdentityWithClaim(vm, b.configForRun())
			if claimErr != nil {
				return hostingerVM{}, "", "", claimErr
			}
			return vm, leaseID, slug, nil
		}
	}
	vms, err := client.ListVMs(ctx)
	if err != nil {
		return hostingerVM{}, "", "", err
	}
	if claimOK && claim.Labels[hostingerRecoveryLabel] == hostingerRecoveryAmbiguous {
		hostname := claim.Labels[hostingerRecoveryHostnameLabel]
		matches := make([]hostingerVM, 0, 2)
		for _, vm := range vms {
			if vm.NameValue() == hostname {
				matches = append(matches, vm)
			}
		}
		if len(matches) > 1 {
			return hostingerVM{}, "", "", exit(4, "multiple Hostinger VPSs match pending recovery hostname %s; refusing to bind lease %s", hostname, claim.LeaseID)
		}
		if len(matches) == 0 {
			return hostingerVM{}, "", "", exit(4, "pending hostinger purchase not found: lease=%s hostname=%s", claim.LeaseID, hostname)
		}
		vm := matches[0]
		leaseID := claim.LeaseID
		slug := firstNonBlank(claim.Slug, hostingerLeaseIdentitySlug(vm, b.configForRun()))
		server, serverErr := b.serverFromVMWithClaim(vm, leaseID, slug, b.configForRun(), true)
		if serverErr != nil {
			return hostingerVM{}, "", "", serverErr
		}
		delete(server.Labels, hostingerRecoveryLabel)
		delete(server.Labels, hostingerRecoveryHostnameLabel)
		if updateErr := updateLeaseClaimEndpoint(leaseID, server, SSHTarget{}); updateErr != nil {
			return hostingerVM{}, "", "", exit(1, "persist recovered hostinger VPS %s: %v", vm.IDString(), updateErr)
		}
		removeHostingerRecoveryRecord(leaseID)
		return vm, leaseID, slug, nil
	}
	servers := make([]Server, 0, len(vms))
	vmsByID := make(map[string]hostingerVM, len(vms))
	for _, vm := range vms {
		leaseID, slug, _, identityErr := hostingerLeaseIdentityWithClaim(vm, b.configForRun())
		if identityErr != nil {
			return hostingerVM{}, "", "", identityErr
		}
		servers = append(servers, hostingerServer(vm, leaseID, slug, b.configForRun(), true))
		vmsByID[vm.IDString()] = vm
	}
	server, leaseID, err := findServerByAlias(servers, id)
	if err != nil {
		return hostingerVM{}, "", "", err
	}
	if server.CloudID != "" {
		return vmsByID[server.CloudID], leaseID, server.Labels["slug"], nil
	}
	return hostingerVM{}, "", "", exit(4, "lease/vm not found: %s", id)
}

func hostingerReleaseOwned(vm hostingerVM) (bool, error) {
	claim, claimed, err := resolveLeaseClaimForProviderCloudID(vm.IDString(), providerName)
	if err != nil {
		return false, err
	}
	return claimed && !hostingerAdoptionPending(claim), nil
}

func hostingerClaimOwned(claim LeaseClaim, exists bool, vmID string) bool {
	return exists && claim.CloudID == vmID && !hostingerAdoptionPending(claim)
}

func hostingerAdoptionPending(claim LeaseClaim) bool {
	return strings.EqualFold(strings.TrimSpace(claim.Labels[hostingerAdoptionPendingLabel]), "true")
}

func (b *leaseBackend) waitForVM(ctx context.Context, client hostingerAPI, id string) (hostingerVM, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if ctx.Err() != nil {
			return hostingerVM{}, context.Cause(ctx)
		}
		vm, err := client.GetVM(ctx, id)
		if err != nil {
			return hostingerVM{}, exit(1, "hostinger get vps %s failed: %v", id, err)
		}
		if vm.Host() != "" && vm.Ready() {
			return vm, nil
		}
		if vm.Terminal() {
			return hostingerVM{}, exit(5, "hostinger vps %s entered terminal state=%s", id, firstNonBlank(vm.State, vm.Status, "unknown"))
		}
		if time.Now().After(deadline) {
			return hostingerVM{}, exit(5, "timed out waiting for hostinger vps %s to expose a public IP; last_state=%s", id, firstNonBlank(vm.State, vm.Status))
		}
		hostingerSleep(5 * time.Second)
	}
}

func (b *leaseBackend) stopVMAndWait(ctx context.Context, client hostingerAPI, id string) error {
	stopCtx, cancel := context.WithTimeout(ctx, hostingerStopWaitTimeout)
	defer cancel()
	if err := client.StopVM(stopCtx, id); err != nil {
		return exit(1, "hostinger stop vps %s failed: %v", id, err)
	}
	lastState := "unknown"
	for {
		vm, err := client.GetVM(stopCtx, id)
		if err != nil {
			if errors.Is(stopCtx.Err(), context.DeadlineExceeded) {
				return exit(5, "timed out waiting for hostinger vps %s to stop; last_state=%s", id, lastState)
			}
			return exit(1, "hostinger confirm stopped vps %s failed: %v", id, err)
		}
		lastState = firstNonBlank(vm.State, vm.Status, "unknown")
		if vm.Stopped() {
			return nil
		}
		if stopCtx.Err() != nil {
			return exit(5, "timed out waiting for hostinger vps %s to stop; last_state=%s", id, lastState)
		}
		hostingerSleep(2 * time.Second)
	}
}

func (b *leaseBackend) updateClaimState(leaseID string, cfg Config, state string, resetLifetime bool) (LeaseClaim, error) {
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return LeaseClaim{}, err
	}
	if !ok {
		return LeaseClaim{}, exit(2, "hostinger lease %s has no local claim", leaseID)
	}
	return b.updateClaimStateIfUnchanged(claim, cfg, state, resetLifetime)
}

func (b *leaseBackend) updateClaimStateIfUnchanged(claim LeaseClaim, cfg Config, state string, resetLifetime bool) (LeaseClaim, error) {
	labels := claim.Labels
	if resetLifetime {
		labels = make(map[string]string, len(claim.Labels))
		for key, value := range claim.Labels {
			labels[key] = value
		}
		delete(labels, "created_at")
		delete(labels, "expires_at")
	}
	labels = touchDirectLeaseLabels(labels, cfg, state, time.Now().UTC())
	return updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
}

func (b *leaseBackend) configForLeaseClaim(cfg Config, leaseID string) (Config, error) {
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		return cfg, err
	}
	userExplicit := hostingerUserExplicit(&cfg)
	storedUser := strings.TrimSpace(claim.Labels["ssh_user"])
	if storedUser != "" && !userExplicit {
		cfg.Hostinger.User = storedUser
		cfg.SSHUser = storedUser
	}
	userChanged := userExplicit && storedUser != strings.TrimSpace(cfg.SSHUser)
	if workRoot := claim.Labels["work_root"]; workRoot != "" && !userChanged && !hostingerWorkRootExplicit(&cfg) {
		cfg.Hostinger.WorkRoot = workRoot
		cfg.WorkRoot = workRoot
	}
	if err := validateHostingerWorkRoot(cfg); err != nil {
		return Config{}, exit(2, "hostinger lease %s has invalid stored SSH configuration: %v", leaseID, err)
	}
	return cfg, nil
}

func (b *leaseBackend) rollbackStartedVM(client hostingerAPI, id string, claim LeaseClaim, cfg Config, cause error) (LeaseClaim, error) {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	labels := touchDirectLeaseLabels(claim.Labels, cfg, "stopped", time.Now().UTC())
	server := Server{CloudID: id, Provider: providerName, Status: "stopped", Labels: labels}
	updated, err := b.stopClaimedVM(rollbackCtx, client, claim, id, server)
	if err != nil {
		return LeaseClaim{}, fmt.Errorf("%w; restart rollback skipped: %v", cause, err)
	}
	return updated, fmt.Errorf("%w; restart rollback=stopped", cause)
}

func (b *leaseBackend) ensureBootstrap(ctx context.Context, cfg Config, lease LeaseTarget, phase string) error {
	deadline := time.Now().Add(bootstrapWaitTimeout(cfg))
	remote := "bash -lc " + shellQuote(hostingerBootstrapScript(cfg))
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err := hostingerRunSSHQuiet(ctx, lease.SSH, remote); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return exit(5, "timed out bootstrapping hostinger vps %s during %s: %v", lease.Server.DisplayID(), phase, err)
		}
		fmt.Fprintf(b.rt.Stderr, "waiting for hostinger bootstrap lease=%s vm=%s phase=%s\n", lease.LeaseID, lease.Server.DisplayID(), phase)
		hostingerSleep(5 * time.Second)
	}
}

func hostingerBootstrapScript(cfg Config) string {
	workRoot := shellQuote(cfg.WorkRoot)
	user := shellQuote(cfg.SSHUser)
	return fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
work_root=%s
user=%s
group=$(id -gn "$user" 2>/dev/null || printf '%%s' "$user")
can_privileged=0
privileged_prefix=
if [ "$(id -u)" -eq 0 ]; then
  can_privileged=1
elif command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
  can_privileged=1
  privileged_prefix=sudo
fi
run_privileged() {
  if [ "$privileged_prefix" = sudo ]; then
    sudo "$@"
  else
    "$@"
  fi
}
safe_work_root_chown=0
case "$work_root" in
  /work/crabbox|/work/crabbox/*|/workspaces/crabbox|/workspaces/crabbox/*|/var/lib/crabbox/work|/var/lib/crabbox/work/*|/opt/crabbox|/opt/crabbox/*|/home/*/crabbox|/home/*/crabbox/*) safe_work_root_chown=1 ;;
esac
canonical_work_root=$(readlink -m -- "$work_root")
[ "$canonical_work_root" = "$work_root" ] || {
  printf 'unsafe work root: %%s resolves to %%s\n' "$work_root" "$canonical_work_root" >&2
  exit 1
}
created_work_root=0
if [ ! -e "$work_root" ]; then
  created_work_root=1
fi
mkdir -p "$work_root" 2>/dev/null || {
  if [ "$can_privileged" -eq 1 ]; then
    run_privileged mkdir -p "$work_root"
  else
    exit 1
  fi
}
if [ "$can_privileged" -eq 1 ]; then
  run_privileged mkdir -p /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/lib/crabbox
  run_privileged chown -R "$user:$group" /var/cache/crabbox 2>/dev/null || true
  if [ "$safe_work_root_chown" -eq 1 ] && { [ "$created_work_root" -eq 1 ] || [ ! -w "$work_root" ]; }; then
    [ "$(readlink -m -- "$work_root")" = "$work_root" ] || exit 1
    run_privileged chown -h -- "$user:$group" "$work_root" 2>/dev/null || true
  fi
else
  mkdir -p "$HOME/.cache/crabbox/pnpm" "$HOME/.cache/crabbox/npm"
fi
chmod 755 "$work_root" 2>/dev/null || true
case "$(dirname "$work_root")" in
  /work|/workspaces|/var/lib/crabbox|/opt/crabbox) if [ "$can_privileged" -eq 1 ]; then run_privileged chmod 755 "$(dirname "$work_root")" 2>/dev/null || true; fi ;;
esac
have_crabbox_tools() {
  command -v git >/dev/null 2>&1 &&
    command -v rsync >/dev/null 2>&1 &&
    command -v curl >/dev/null 2>&1 &&
    command -v jq >/dev/null 2>&1
}
if ! have_crabbox_tools && [ "$can_privileged" -eq 1 ] && command -v apt-get >/dev/null 2>&1; then
  run_privileged mkdir -p /etc/apt/apt.conf.d
  run_privileged tee /etc/apt/apt.conf.d/80-crabbox-retries >/dev/null <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT
  retry() {
    n=1
    until "$@"; do
      if [ "$n" -ge 8 ]; then
        return 1
      fi
      sleep $((n * 5))
      n=$((n + 1))
    done
  }
  retry run_privileged apt-get update
  retry run_privileged apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq
  run_privileged systemctl enable ssh >/dev/null 2>&1 || true
  run_privileged systemctl restart ssh >/dev/null 2>&1 || run_privileged systemctl restart ssh.socket >/dev/null 2>&1 || true
fi
if [ "$can_privileged" -eq 1 ]; then
run_privileged tee /usr/local/bin/crabbox-ready >/dev/null <<'READY'
#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
test -w %s
READY
  run_privileged chmod 0755 /usr/local/bin/crabbox-ready
  run_privileged touch /var/lib/crabbox/bootstrapped
fi
have_crabbox_tools
test -w "$work_root"
`, workRoot, user, workRoot)
}

func hostingerReadyCheck(cfg Config) string {
	return strings.Join([]string{
		"git --version >/dev/null 2>&1",
		"rsync --version >/dev/null 2>&1",
		"curl --version >/dev/null 2>&1",
		"jq --version >/dev/null 2>&1",
		"test -w " + shellQuote(cfg.WorkRoot),
	}, " && ")
}

func (b *leaseBackend) leaseFromVM(cfg Config, vm hostingerVM, leaseID, slug string, keep bool) (LeaseTarget, error) {
	host := vm.Host()
	if host == "" {
		return LeaseTarget{}, exit(5, "hostinger vps %s has no public ip", vm.IDString())
	}
	server, err := b.serverFromVMWithClaim(vm, leaseID, slug, cfg, keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	target := sshTargetFromConfig(cfg, host)
	if err := useStoredTestboxKey(&target, leaseID, sshKeyExplicit(&cfg)); err != nil {
		return LeaseTarget{}, err
	}
	target.NetworkKind = networkPublic
	target.ReadyCheck = hostingerReadyCheck(cfg)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func hostingerServer(vm hostingerVM, leaseID, slug string, cfg Config, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["release"] = "stop"
	labels["ssh_user"] = cfg.SSHUser
	labels["work_root"] = cfg.WorkRoot
	if state := strings.ToLower(firstNonBlank(vm.State, vm.Status)); state != "" {
		labels["state"] = state
	}
	server := Server{
		CloudID:  vm.IDString(),
		Provider: providerName,
		Name:     vm.NameValue(),
		Status:   firstNonBlank(vm.State, vm.Status),
		Labels:   labels,
	}
	if server.Name == "" {
		server.Name = hostingerHostname(cfg, leaseID, slug)
	}
	server.PublicNet.IPv4.IP = vm.Host()
	return server
}

func (b *leaseBackend) serverFromVMWithClaim(vm hostingerVM, leaseID, slug string, cfg Config, keep bool) (Server, error) {
	server := hostingerServer(vm, leaseID, slug, cfg, keep)
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return Server{}, err
	}
	if !ok {
		return server, nil
	}
	if claim.CloudID != "" && claim.CloudID != vm.IDString() {
		return Server{}, exit(2, "hostinger lease %s is bound to vps %s, not %s", claim.LeaseID, claim.CloudID, vm.IDString())
	}
	if len(claim.Labels) == 0 {
		return server, nil
	}
	labels := make(map[string]string, len(claim.Labels)+4)
	for key, value := range claim.Labels {
		labels[key] = value
	}
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = slug
	labels["release"] = "stop"
	labels["ssh_user"] = cfg.SSHUser
	labels["work_root"] = cfg.WorkRoot
	if state := strings.ToLower(firstNonBlank(vm.State, vm.Status)); state != "" {
		labels["state"] = state
	}
	server.Labels = labels
	return server, nil
}

func hostingerOwnedServer(server Server, cfg Config) bool {
	return server.Provider == providerName && hostingerOwnedName(server.Name, cfg)
}

func hostingerLeaseIdentity(vm hostingerVM, cfg Config) (string, string) {
	name := vm.NameValue()
	if hostingerOwnedName(name, cfg) {
		rest := strings.TrimPrefix(name, hostingerHostnamePrefix(cfg))
		parts := strings.Split(rest, "-")
		if len(parts) >= 2 {
			return "cbx_" + parts[len(parts)-1], strings.Join(parts[:len(parts)-1], "-")
		}
	}
	id := vm.IDString()
	if id == "" {
		id = "manual"
	}
	return "cbx_hostinger_" + id, firstNonBlank(name, "manual")
}

func hostingerLeaseIdentitySlug(vm hostingerVM, cfg Config) string {
	_, slug := hostingerLeaseIdentity(vm, cfg)
	return slug
}

func hostingerLeaseIdentityWithClaim(vm hostingerVM, cfg Config) (string, string, bool, error) {
	claim, ok, err := resolveLeaseClaimForProviderCloudID(vm.IDString(), providerName)
	if err != nil {
		return "", "", false, err
	}
	if ok && claim.LeaseID != "" {
		return claim.LeaseID, firstNonBlank(claim.Slug, hostingerLeaseIdentitySlug(vm, cfg)), !hostingerAdoptionPending(claim), nil
	}
	recovery, recovered, err := findHostingerRecoveryRecord(vm)
	if err != nil {
		return "", "", false, err
	}
	if recovered {
		return recovery.LeaseID, firstNonBlank(recovery.Slug, hostingerLeaseIdentitySlug(vm, cfg)), false, nil
	}
	id := vm.IDString()
	if id == "" {
		id = "manual"
	}
	return "cbx_hostinger_" + id, firstNonBlank(hostingerLeaseIdentitySlug(vm, cfg), vm.NameValue(), "manual"), false, nil
}

func hostingerOwnedName(name string, cfg Config) bool {
	if !strings.HasPrefix(name, hostingerHostnamePrefix(cfg)) {
		return false
	}
	rest := strings.TrimPrefix(name, hostingerHostnamePrefix(cfg))
	parts := strings.Split(rest, "-")
	return len(parts) >= 2 && validHostingerLeaseSuffix(parts[len(parts)-1])
}

func hostingerHostnamePrefix(cfg Config) string {
	prefix := strings.Trim(strings.ToLower(cfg.Hostinger.HostnamePrefix), "- ")
	if prefix == "" {
		prefix = "crabbox"
	}
	return prefix + "-"
}

func validHostingerLeaseSuffix(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'f') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (vm hostingerVM) IDString() string {
	return hostingerIDString(vm.ID)
}

func (vm hostingerVM) NameValue() string {
	return firstNonBlank(vm.Hostname, vm.Name)
}

func (vm hostingerVM) Host() string {
	if ip := vm.IPv4.First(); ip != "" {
		return ip
	}
	if vm.IP != "" {
		return vm.IP
	}
	if vm.ExternalIP != "" {
		return vm.ExternalIP
	}
	if len(vm.IPV4) > 0 {
		return vm.IPV4[0]
	}
	return ""
}

func (vm hostingerVM) Ready() bool {
	state := strings.ToLower(firstNonBlank(vm.State, vm.Status))
	return state == "" || strings.Contains(state, "running") || strings.Contains(state, "active") || strings.Contains(state, "ready")
}

func (vm hostingerVM) Stopped() bool {
	state := strings.ToLower(firstNonBlank(vm.State, vm.Status))
	return state == "stopped" || state == "off" || state == "powered_off"
}

func (vm hostingerVM) Terminal() bool {
	switch strings.ToLower(firstNonBlank(vm.State, vm.Status)) {
	case "error", "suspended", "destroyed":
		return true
	default:
		return false
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
