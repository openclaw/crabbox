package parallels

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// parallelsFixedLeaseKind marks durable fixed-ID claims with a downgrade-safe
// discriminator. A released client canonicalizes only the markers it shipped
// with, so it cannot mistake a fixed Parallels claim for an ordinary lease:
// its release and cleanup paths delete the VM and call RemoveLeaseClaim
// unconditionally, which would erase the terminal tombstone and let the same
// fixed ID create a second VM. Current clients map the marker back to the
// runtime provider through canonicalClaimProvider, so exact-ownership checks,
// resolution, and cleanup keep routing it.
var parallelsFixedLeaseKind = core.FixedLeaseKind{ClaimProvider: core.FixedParallelsClaimProvider, IntentVersion: 1, Label: "Parallels", DeletionState: "deleting"}

const parallelsProviderName = "parallels"

// waitForSSHReady is an indirection seam so lifecycle tests can exercise the
// full acquisition path without a live guest.
var waitForSSHReady = core.WaitForSSHReady

func (*leaseBackend) SupportsRequestedLeaseID() bool { return true }

func parallelsFixedFingerprint(cfg core.Config, req core.AcquireRequest, source, snapshotID, publicKey string) (string, error) {
	return core.FixedIntentFingerprint("", core.FixedIntentFields{
		{Name: "source", Value: source}, {Name: "sourceSnapshot", Value: snapshotID},
		{Name: "cloneMode", Value: strings.ToLower(strings.TrimSpace(core.Blank(cfg.Parallels.CloneMode, "linked")))},
		{Name: "targetOS", Value: cfg.TargetOS}, {Name: "windowsMode", Value: cfg.WindowsMode},
		{Name: "guestUser", Value: strings.TrimSpace(cfg.SSHUser)}, {Name: "workRoot", Value: strings.TrimSpace(cfg.WorkRoot)},
		{Name: "vmRoot", Value: strings.TrimSpace(cfg.Parallels.VMRoot)}, {Name: "slug", Value: core.NormalizeLeaseSlug(req.RequestedSlug)},
		{Name: "publicKey", Value: strings.TrimSpace(publicKey)}, {Name: "sshPort", Value: cfg.SSHPort},
		{Name: "fallbackPorts", Value: cfg.SSHFallbackPorts}, {Name: "pond", Value: core.NormalizePondName(cfg.Pond)},
		{Name: "keep", Value: req.Keep}, {Name: "desktop", Value: cfg.Desktop},
		{Name: "accountDesktop", Value: cfg.Desktop && cfg.TargetOS == core.TargetMacOS && cfg.Parallels.Password != ""},
		{Name: "ttl", Value: cfg.TTL}, {Name: "idleTimeout", Value: cfg.IdleTimeout},
	})
}

// parallelsVMByID searches the complete inventory for a bound UUID. Parallels
// VM names are mutable, so only the UUID can prove that a resource is gone.
func parallelsVMByID(vms []core.ParallelsVM, id string) (core.ParallelsVM, bool) {
	if strings.TrimSpace(id) == "" {
		return core.ParallelsVM{}, false
	}
	for _, vm := range vms {
		if vm.ID == id {
			return vm, true
		}
	}
	return core.ParallelsVM{}, false
}

func parallelsVMByName(vms []core.ParallelsVM, name string) (core.ParallelsVM, bool) {
	for _, vm := range vms {
		if vm.Name == name {
			return vm, true
		}
	}
	return core.ParallelsVM{}, false
}

// parallelsFixedCreationDir is the directory this attempt told `prlctl clone`
// to place its VM bundle in. `prlctl clone` reports no UUID, and the name it
// was given is mutable, so --dst is the only create-time attribute Crabbox
// controls: a bundle inside this per-attempt directory can only have come from
// this attempt's own clone.
func parallelsFixedCreationDir(intent *core.FixedCreateIntent) string {
	if intent == nil {
		return ""
	}
	return strings.TrimSpace(intent.Attempt["dst"])
}

func parallelsVMInCreationDir(vm core.ParallelsVM, dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" || strings.TrimSpace(vm.Home) == "" {
		return false
	}
	return strings.HasPrefix(vm.Home, strings.TrimRight(dir, "/")+"/")
}

// parallelsFixedIncarnation is the provider-issued UUID this attempt's clone
// produced. Parallels never reuses a VM UUID, so it is the only evidence that
// separates this attempt's VM from a later VM occupying the same name.
func parallelsFixedIncarnation(intent *core.FixedCreateIntent) string {
	if intent == nil {
		return ""
	}
	return strings.TrimSpace(intent.Attempt["vm_uuid"])
}

// validateParallelsFixedVM re-attests the exact resource identity a fixed lease
// is bound to. Only the recorded attempt name is an adoption key, and only
// together with the provider-issued UUID this attempt's clone produced; a slug,
// a lookalike name, or an unattested name match never is.
func validateParallelsFixedVM(claim core.LeaseClaim, intent *core.FixedCreateIntent, vm core.ParallelsVM, name string) error {
	conflict := func(format string, args ...any) error {
		return core.Exit(4, "lease_id_conflict: "+format, args...)
	}
	if vm.Name != name {
		return conflict("Parallels lease %s expected VM %q but observed %q", claim.LeaseID, name, vm.Name)
	}
	if !strings.HasPrefix(vm.Name, "crabbox-") {
		return conflict("Parallels lease %s is bound to non-Crabbox VM %q", claim.LeaseID, vm.Name)
	}
	if leaseID, _ := parallelsLeaseFromVMName(vm.Name); leaseID != claim.LeaseID {
		return conflict("Parallels VM %q does not carry lease %s", vm.Name, claim.LeaseID)
	}
	if strings.TrimSpace(vm.ID) == "" {
		return conflict("Parallels VM %q reported no UUID for lease %s", vm.Name, claim.LeaseID)
	}
	// The VM name is host-unique but reusable over time, so it attests nothing
	// on its own. Requiring the recorded incarnation unconditionally is what
	// stops a replacement at that name from inheriting this attempt's authority.
	incarnation := parallelsFixedIncarnation(intent)
	if incarnation == "" {
		return core.FixedUncertainCustody(claim.LeaseID)
	}
	if incarnation != vm.ID {
		return conflict("Parallels lease %s is bound to VM UUID %q, not %q", claim.LeaseID, incarnation, vm.ID)
	}
	// The UUID alone was learned by reading the host; the creation directory is
	// what says this attempt produced that VM in the first place.
	if dir := parallelsFixedCreationDir(intent); dir == "" {
		return core.FixedUncertainCustody(claim.LeaseID)
	} else if !parallelsVMInCreationDir(vm, dir) {
		return conflict("Parallels lease %s created its VM in %q, but VM %q reports home %q", claim.LeaseID, dir, vm.ID, core.Blank(vm.Home, "<none>"))
	}
	if claim.CloudImmutableID != "" && claim.CloudImmutableID != vm.ID {
		return conflict("Parallels lease %s is bound to VM UUID %q, not %q", claim.LeaseID, claim.CloudImmutableID, vm.ID)
	}
	if claim.CloudID != "" && claim.CloudID != vm.ID {
		return conflict("Parallels lease %s is bound to VM %q, not %q", claim.LeaseID, claim.CloudID, vm.ID)
	}
	// The `host` label is a fleet entry's display name, recorded for operators.
	// It is local claim data that attests nothing about this VM, and comparing
	// it here would reject a lease whose host was merely re-addressed. The
	// durable provider scope, re-attested from the Parallels service itself
	// before this point, is what binds the lease to a machine.
	if lease := strings.TrimSpace(claim.Labels["lease"]); lease != "" && lease != claim.LeaseID {
		return conflict("Parallels lease %s carries lease label %q", claim.LeaseID, lease)
	}
	return nil
}

func (b *leaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	leaseID := req.RequestedLeaseID
	source := shared.FirstNonBlankTrimmed(b.Cfg.Parallels.SourceID, b.Cfg.Parallels.Source)
	if source == "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=parallels requires --parallels-source, --parallels-template, or parallels.source")
	}

	var cfg core.Config
	var client *core.ParallelsClient
	var publicKey, snapshotID, sourceID, hostLabel, vmBase string

	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: parallelsFixedLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: b.Cfg.TargetOS, WindowsMode: b.Cfg.WindowsMode,
		TTL: b.Cfg.TTL, IdleTimeout: b.Cfg.IdleTimeout,
	}, core.FixedLeaseOperations[core.ParallelsVM]{
		Admission:         &core.FixedAdmission{PendingKey: "submission", PendingValue: "pending", SubmittedValue: "submitted"},
		DeferredAdmission: true,
		Identity: func(vm core.ParallelsVM) core.FixedResourceBinding {
			return core.FixedResourceBinding{CloudID: vm.ID, ImmutableID: vm.ID, AttemptIdentityKey: "vm_uuid"}
		},
		DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
			var err error
			var scope string
			if exists {
				if !parallelsFixedLeaseKind.IsFixedClaim(*claim) {
					return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: lease %s already has another owner", leaseID)
				}
				// Replay re-attests the recorded connection before doing anything
				// else. A fleet entry that kept its display name while its host or
				// account moved is a different connection, not this lease's host.
				cfg, client, err = parallelsFixedHostConfig(ctx, b.RT.Exec, b.Cfg, leaseID, claim.ProviderScope)
				scope = claim.ProviderScope
			} else {
				if cfg, err = core.SelectParallelsFleetConfig(ctx, b.Cfg, b.RT.Exec, source); err == nil {
					client = core.NewParallelsClient(cfg, b.RT.Exec)
				}
			}
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			identity, err := client.ServerIdentity(ctx)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			if !exists {
				scope = parallelsScopeFromIdentity(identity)
			}
			// Where a clone's bundle lands is the only create-time attribute this
			// provider controls, so the base directory must be known before any
			// attempt is recorded.
			if vmBase = strings.TrimSpace(core.Blank(cfg.Parallels.VMRoot, identity.VMHome)); vmBase == "" {
				return core.FixedLeaseBinding{}, core.Exit(4, "Parallels host reported no VM directory and none is configured; a fixed lease cannot record where its VM was created")
			}
			if err := client.ValidateMacOSBootstrapKey(ctx); err != nil {
				return core.FixedLeaseBinding{}, err
			}
			hostLabel = parallelsHostName(cfg)
			keyPath, key, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			cfg.SSHKey, publicKey = keyPath, key
			cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
			// The configured source may be a mutable VM name. Resolve it to the
			// immutable UUID the host reports before it enters the fingerprint or a
			// clone submission, so replacing the source VM under the same name is
			// intent drift rather than a silent provision from a different template.
			sourceVM, err := client.GetVM(ctx, source)
			if err != nil {
				return core.FixedLeaseBinding{}, fmt.Errorf("resolve Parallels source %q for lease=%s (claim retained): %w", source, leaseID, err)
			}
			if sourceID = strings.TrimSpace(sourceVM.ID); sourceID == "" {
				return core.FixedLeaseBinding{}, core.Exit(4, "Parallels source %q reported no UUID for lease %s", source, leaseID)
			}
			snapshotID = shared.FirstNonBlankTrimmed(cfg.Parallels.SourceSnapshotID, cfg.Parallels.SourceSnapshot)
			if snapshotID != "" && cfg.Parallels.SourceSnapshotID == "" {
				// Resolve the name to its stable snapshot ID so a renamed or
				// replaced snapshot is drift rather than a silent different fork.
				if snapshotID, err = client.SnapshotID(ctx, sourceID, snapshotID); err != nil {
					return core.FixedLeaseBinding{}, err
				}
			}
			fingerprint, err := parallelsFixedFingerprint(cfg, req, sourceID, snapshotID, publicKey)
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			binding := core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint}
			if exists {
				return binding, nil
			}
			servers, err := client.ListCrabboxServers(ctx)
			if err != nil {
				return binding, err
			}
			binding.Slug, err = core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
			return binding, err

		}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			name := core.ParallelsLeaseVMName(leaseID, claim.Slug)
			labels := map[string]string{"source": source, "host": hostLabel, "source_id": sourceID, "ssh_user": cfg.SSHUser, "work_root": cfg.WorkRoot}
			if snapshotID != "" {
				labels["source_snapshot"] = snapshotID
			}
			return core.FixedAttemptPlan{
				Values:   map[string]string{"name": name, "host": claim.ProviderScope, "source_id": sourceID, "submission": "pending"},
				NonceKey: "dst", LowerNonce: true, NoncePrefix: strings.TrimRight(vmBase, "/") + "/" + name + "-",
				DirectLabels: &core.FixedDirectLabels{Config: cfg, Provider: parallelsProviderName, Keep: req.Keep, Now: time.Now().UTC()},
				Labels:       labels, FingerprintLabel: "fixed_intent_sha256",
			}, nil
		}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.ParallelsVM], error) {
			claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
			name := core.ParallelsLeaseVMName(leaseID, intent.Slug)
			var result core.FixedObservation[core.ParallelsVM]
			if _, err := core.ReadFixedAttempt[map[string]string](intent, core.FixedAttemptFormat{
				Equal:         map[string]string{"name": name, "host": intent.ProviderScope},
				OptionalEqual: map[string]string{"source_id": sourceID},
			}); err != nil {
				return result, err
			}
			createDir := parallelsFixedCreationDir(intent)
			if intent.Attempt != nil && createDir == "" {
				return result, core.FixedUncertainCustody(leaseID)
			}
			vms, err := client.ListVMsDetailed(ctx)
			if err != nil {
				return result, fmt.Errorf("reconcile Parallels lease=%s vm=%s (claim and key retained): %w", leaseID, name, err)
			}
			if createDir != "" {
				for _, vm := range vms {
					if parallelsVMInCreationDir(vm, createDir) {
						result.Candidates = append(result.Candidates, vm)
					}
				}
			}
			if len(result.Candidates) != 0 {
				return result, nil
			}
			if intent.State != "prepared" || claim.CloudID != "" {
				return result, core.Exit(4, "lease_id_conflict: Parallels lease %s no longer has VM %q on host %s; stop the lease instead of replaying it", leaseID, name, shortParallelsScope(intent.ProviderScope))
			}
			if occupant, taken := parallelsVMByName(vms, name); taken {
				return result, core.Exit(4, "lease_id_conflict: Parallels lease %s cannot create VM %q: VM %q already occupies that name and was not created by this lease; custody is retained and nothing was changed or deleted", leaseID, name, occupant.ID)
			}
			result.CanSubmit = true
			return result, nil
		}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (core.ParallelsVM, error) {
			intent := tx.Claim.FixedCreateIntent
			name, createDir := intent.Attempt["name"], parallelsFixedCreationDir(intent)
			// Capacity is enforced by counting the host's VMs and then cloning
			// into it, so both must happen under one reservation: the per-lease
			// claim locks do not serialize different lease IDs, and a prepared
			// retry can reach this point long after its host filled up. The
			// reservation is taken only for an actual submission, so replaying
			// or releasing an existing VM still works at capacity.
			releaseCapacity, err := core.ReserveParallelsHostCapacity(ctx, cfg, b.RT.Exec, sourceID)
			if err != nil {
				return core.ParallelsVM{}, err
			}
			capacityHeld := true
			releaseCapacityOnce := func() {
				if capacityHeld {
					capacityHeld = false
					releaseCapacity()
				}
			}
			defer releaseCapacityOnce()
			if err := client.EnsureHostDir(ctx, createDir); err != nil {
				return core.ParallelsVM{}, err
			}
			fmt.Fprintf(b.RT.Stderr, "provisioning provider=parallels lease=%s slug=%s host=%s source=%s snapshot=%s clone_mode=%s keep=%v\n",
				leaseID, intent.Slug, hostLabel, sourceID, blank(snapshotID, "-"), blank(cfg.Parallels.CloneMode, "linked"), req.Keep)
			// The VM name is host-unique: prlctl refuses a concurrent duplicate
			// create of this exact lease-derived name even after a lost reply.
			// Submitting the resolved source UUID keeps the clone bound to the
			// template the fingerprint was taken over, and --dst keeps the
			// resulting bundle inside this attempt's directory.
			cloneCfg := cfg
			cloneCfg.Parallels.VMRoot = createDir
			cloneErr := core.NewParallelsClient(cloneCfg, b.RT.Exec).SubmitClone(ctx, sourceID, snapshotID, leaseID, intent.Slug, req.Keep, tx.Admit)
			// Whatever the reply said, a submitted clone may already count
			// against maxVMs, and the reservation has done its job either way:
			// the rest of bring-up need not keep other forks waiting.
			releaseCapacityOnce()
			if cloneErr != nil {
				return core.ParallelsVM{}, fmt.Errorf("Parallels clone outcome uncertain for lease=%s vm=%s; claim and key retained, retry the same lease ID or stop it: %w", leaseID, name, cloneErr)
			}
			// Read the created VM back by its creation directory. `prlctl clone`
			// reports no UUID and the name it was given is mutable, so a name
			// lookup here could bind whatever has taken that name since.
			vms, err := client.ListVMsDetailed(ctx)
			if err != nil {
				return core.ParallelsVM{}, fmt.Errorf("reconcile Parallels lease=%s vm=%s after clone (claim and key retained): %w", leaseID, name, err)
			}
			vm, found, err := core.SelectFixedCandidate(parallelsFixedLeaseKind, leaseID, vms, func(vm core.ParallelsVM) bool { return parallelsVMInCreationDir(vm, createDir) })
			if err != nil {
				return core.ParallelsVM{}, err
			} else if !found {
				return core.ParallelsVM{}, fmt.Errorf("Parallels clone reported success but no VM is present in the creation directory for lease=%s; claim and key retained", leaseID)
			}
			return vm, nil
		}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, vm core.ParallelsVM) (core.LeaseTarget, error) {
			claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
			name := intent.Attempt["name"]
			// Core has durably recorded creation-directory provenance before a
			// name check can refuse use of the discovered incarnation.
			if err := validateParallelsFixedVM(*claim, intent, vm, name); err != nil {
				return core.LeaseTarget{}, err
			}

			// Replay must be idempotent about power state. prlctl refuses `start`
			// on a VM that is not stopped, so an adopted running VM is left alone
			// rather than restarted.
			if !strings.EqualFold(strings.TrimSpace(vm.State), "running") {
				if err := client.Start(ctx, vm.ID); err != nil {
					return core.LeaseTarget{}, err
				}
			}
			// Fixed acquisitions retain failed VMs, so use existing-lease guidance.
			ready, err := client.WaitForIP(ctx, vm.ID, cfg.Parallels.StartupTimeout, core.ParallelsIPWaitExisting)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			// Guest preparation runs once. An intent only reaches `acquired` after
			// the key is installed and the guest is ready, so replaying one must not
			// write to the guest again: the work is redundant, it is the only part
			// of replay that mutates the VM, and it depends on a guest-tools channel
			// that need not be available just because the lease is still valid.
			// SSH readiness below still re-proves the lease is usable.
			if intent.State != "acquired" {
				if err := b.prepareGuest(ctx, client, vm.ID, ready, cfg, publicKey); err != nil {
					return core.LeaseTarget{}, err
				}
			}
			server := core.Server{CloudID: vm.ID, Provider: parallelsProviderName, Name: vm.Name, Status: "ready", Labels: maps.Clone(claim.Labels)}
			server.ImmutableID = vm.ID
			server.ServerType.Name = core.ServerTypeForProviderClass(parallelsProviderName, cfg.Class)
			server.PublicNet.IPv4.IP = ready.IP
			if ready.IPSource != "" {
				server.Labels["ip_source"] = ready.IPSource
			}
			target := parallelsLeaseSSHTarget(cfg, ready.IP)
			if cfg.TargetOS == core.TargetWindows && cfg.WindowsMode == core.WindowsModeNormal {
				target.ReadyCheck = core.PowershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
			}
			if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
				return core.LeaseTarget{}, err
			}
			server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Now().UTC())
			client.SetLeaseLabels(leaseID, server.Labels)
			fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s vm=%s ip=%s\n", leaseID, server.DisplayID(), ready.IP)
			return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
		}})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func (b *leaseBackend) releaseFixed(ctx context.Context, claim core.LeaseClaim, outcome *core.ReleaseLeaseOutcome) error {
	if claim.FixedCreateIntent.State == "released" {
		if err := parallelsFixedLeaseKind.ValidateTerminalClaim(claim, claim, claim.LeaseID, nil); err != nil {
			return err
		}
		outcome.Terminal = true
		return nil
	}
	name := strings.TrimSpace(claim.FixedCreateIntent.Attempt["name"])
	if name == "" {
		return core.Exit(4, "Parallels lease %s has no recorded creation attempt", claim.LeaseID)
	}
	intent := claim.FixedCreateIntent
	incarnation := parallelsFixedIncarnation(intent)
	// Absence cannot repair disagreement between the recorded host or VM identities.
	if claim.ProviderScope != intent.ProviderScope || intent.Attempt["host"] != intent.ProviderScope ||
		(claim.CloudID != "" && claim.CloudID != incarnation) ||
		(claim.CloudImmutableID != "" && claim.CloudImmutableID != incarnation) ||
		(incarnation == "" && (intent.State == "acquired" || intent.State == "deleting")) {
		return core.Exit(4, "lease_id_conflict: Parallels lease %s has inconsistent recorded host or VM identity; claim retained", claim.LeaseID)
	}
	_, client, err := parallelsFixedHostConfig(ctx, b.RT.Exec, b.Cfg, claim.LeaseID, claim.ProviderScope)
	if err != nil {
		return err
	}
	// A prepared create's absence is inconclusive: the clone may still be in
	// flight. Only an attempt that reached the provider may finalize on absence.
	lookup := func() (*core.ParallelsVM, error) {
		vms, err := client.ListVMsDetailed(ctx)
		if err != nil {
			return nil, err
		}
		vm, found := parallelsVMByName(vms, name)
		if !found {
			// The recorded name is not an absence proof on its own: an acquired
			// VM renamed to another crabbox-<same-lease-id>-<slug> still exists
			// and is still this lease's. Search the complete inventory for the
			// bound UUID before concluding the resource is gone, and fail
			// closed rather than tombstoning a VM that is still running.
			if renamed, ok := parallelsVMByID(vms, incarnation); ok {
				return nil, core.Exit(4, "lease_id_conflict: Parallels lease %s is bound to VM UUID %q, which is present as %q rather than its recorded name %q; custody is retained and nothing was deleted. Restore that VM's name or remove it explicitly before stopping the lease",
					claim.LeaseID, incarnation, renamed.Name, name)
			}
			if state := claim.FixedCreateIntent.State; state == "acquired" || state == "deleting" {
				return nil, nil
			}
			if incarnation != "" {
				// The attempt bound a VM that is now absent under any name.
				return nil, nil
			}
			return nil, core.Exit(4, "lease_id_conflict: prepared Parallels lease %s has no VM %q on host %s; replay the same lease ID before stopping it", claim.LeaseID, name, shortParallelsScope(claim.ProviderScope))
		}
		if err := validateParallelsFixedVM(claim, claim.FixedCreateIntent, vm, name); err != nil {
			return nil, err
		}
		return &vm, nil
	}
	err = core.DeleteFixedResource(ctx, parallelsFixedLeaseKind, claim, core.FixedLeaseOperations[core.ParallelsVM]{
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.ParallelsVM], error) {
			claim = *tx.Claim
			vm, err := lookup()
			result := core.FixedObservation[core.ParallelsVM]{AbsenceProven: err == nil && vm == nil}
			if vm != nil {
				result.Candidates = []core.ParallelsVM{*vm}
			}
			return result, err
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, vm core.ParallelsVM) error {
			claim = *tx.Claim
			current, err := lookup()
			if err != nil || current == nil {
				outcome.Terminal = err == nil
				return err
			}
			err = client.Delete(ctx, current.ID)
			outcome.Terminal = err == nil
			if err == nil {
				client.RemoveHostDirIfEmpty(ctx, parallelsFixedCreationDir(claim.FixedCreateIntent))
			}
			return err
		},
	})
	if err == nil {
		outcome.Terminal = true
	}
	if err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(claim.LeaseID)
	return nil
}

func (b *leaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return parallelsFixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_intent_sha256"] != "", nil, nil)
}
