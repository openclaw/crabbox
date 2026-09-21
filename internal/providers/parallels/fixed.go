package parallels

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
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
var parallelsFixedLeaseKind = core.FixedLeaseKind{ClaimProvider: core.FixedParallelsClaimProvider, IntentVersion: 1, Label: "Parallels"}

const parallelsProviderName = "parallels"

// waitForSSHReady is an indirection seam so lifecycle tests can exercise the
// full acquisition path without a live guest.
var waitForSSHReady = core.WaitForSSHReady

func (*leaseBackend) SupportsRequestedLeaseID() bool { return true }

// parallelsFixedIdentity is the create identity a fixed lease ID is bound to.
// Any change here is intent drift, not a replay.
type parallelsFixedIdentity struct {
	Source         string        `json:"source"`
	SourceSnapshot string        `json:"sourceSnapshot"`
	CloneMode      string        `json:"cloneMode"`
	TargetOS       string        `json:"targetOS"`
	WindowsMode    string        `json:"windowsMode"`
	GuestUser      string        `json:"guestUser"`
	WorkRoot       string        `json:"workRoot"`
	VMRoot         string        `json:"vmRoot"`
	Slug           string        `json:"slug"`
	PublicKey      string        `json:"publicKey"`
	SSHPort        string        `json:"sshPort"`
	FallbackPorts  []string      `json:"fallbackPorts"`
	Pond           string        `json:"pond"`
	Keep           bool          `json:"keep"`
	Desktop        bool          `json:"desktop"`
	AccountDesktop bool          `json:"accountDesktop"`
	TTL            time.Duration `json:"ttl"`
	IdleTimeout    time.Duration `json:"idleTimeout"`
}

func parallelsFixedFingerprint(cfg core.Config, req core.AcquireRequest, source, snapshotID, publicKey string) (string, error) {
	data, err := json.Marshal(parallelsFixedIdentity{
		Source:         source,
		SourceSnapshot: snapshotID,
		CloneMode:      strings.ToLower(strings.TrimSpace(core.Blank(cfg.Parallels.CloneMode, "linked"))),
		TargetOS:       cfg.TargetOS,
		WindowsMode:    cfg.WindowsMode,
		GuestUser:      strings.TrimSpace(cfg.SSHUser),
		WorkRoot:       strings.TrimSpace(cfg.WorkRoot),
		VMRoot:         strings.TrimSpace(cfg.Parallels.VMRoot),
		Slug:           core.NormalizeLeaseSlug(req.RequestedSlug),
		PublicKey:      strings.TrimSpace(publicKey),
		SSHPort:        cfg.SSHPort,
		FallbackPorts:  cfg.SSHFallbackPorts,
		Pond:           core.NormalizePondName(cfg.Pond),
		Keep:           req.Keep,
		Desktop:        cfg.Desktop,
		AccountDesktop: cfg.Desktop && cfg.TargetOS == core.TargetMacOS && cfg.Parallels.Password != "",
		TTL:            cfg.TTL,
		IdleTimeout:    cfg.IdleTimeout,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
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

// parallelsCreatedVM finds this attempt's own clone by its creation directory
// rather than by its name. More than one match is not evidence, so it fails
// closed rather than picking one.
func parallelsCreatedVM(vms []core.ParallelsVM, dir string) (core.ParallelsVM, bool, error) {
	var found core.ParallelsVM
	matches := 0
	for _, vm := range vms {
		if parallelsVMInCreationDir(vm, dir) {
			found, matches = vm, matches+1
		}
	}
	if matches > 1 {
		return core.ParallelsVM{}, false, core.Exit(4, "lease_id_conflict: Parallels creation directory %q holds %d VMs, so it attests no single incarnation", dir, matches)
	}
	return found, matches == 1, nil
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

// bindParallelsFixedIncarnation records the UUID a successful clone returned.
// It runs before any further reconciliation so a lost or rejected follow-up
// read can never leave the attempt unbound and adoptable by name alone.
func bindParallelsFixedIncarnation(claim *core.LeaseClaim, intent *core.FixedCreateIntent, uuid, name, leaseID string) error {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return fmt.Errorf("Parallels clone reported no UUID for lease=%s vm=%s; claim and key retained, stop the lease and inspect the host", leaseID, name)
	}
	if recorded := parallelsFixedIncarnation(intent); recorded != "" && recorded != uuid {
		return core.Exit(4, "lease_id_conflict: Parallels lease %s is bound to VM UUID %q, not %q", leaseID, recorded, uuid)
	}
	intent.Attempt["vm_uuid"] = uuid
	claim.CloudID, claim.CloudImmutableID = uuid, uuid
	return nil
}

// parallelsUncertainCustody reports that an attempt reached the provider but
// its VM incarnation was never recorded, so the VM now occupying the recorded
// name cannot be attested as this lease's. Custody is retained: the claim, the
// attempt and the per-lease key survive, no credential is installed, and
// nothing is deleted. Only an operator can adjudicate the name.
func parallelsUncertainCustody(leaseID, name string) error {
	return core.Exit(4, "lease_id_conflict: Parallels lease %s has an unattested creation attempt: VM %q occupies its recorded name but no VM UUID was ever recorded for this attempt, so it cannot be proven to be this lease's VM. The claim, attempt and key are retained and nothing was changed or deleted; inspect that VM on the recorded host and remove it explicitly if it is this lease's orphan, then replay the same lease ID", leaseID, name)
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
		return parallelsUncertainCustody(claim.LeaseID, name)
	}
	if incarnation != vm.ID {
		return conflict("Parallels lease %s is bound to VM UUID %q, not %q", claim.LeaseID, incarnation, vm.ID)
	}
	// The UUID alone was learned by reading the host; the creation directory is
	// what says this attempt produced that VM in the first place.
	if dir := parallelsFixedCreationDir(intent); dir == "" {
		return parallelsUncertainCustody(claim.LeaseID, name)
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

	lease, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: parallelsFixedLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: b.Cfg.TargetOS, WindowsMode: b.Cfg.WindowsMode,
		TTL: b.Cfg.TTL, IdleTimeout: b.Cfg.IdleTimeout,
	}, func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
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
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (core.LeaseTarget, error) {
		name := core.ParallelsLeaseVMName(leaseID, intent.Slug)
		if intent.Attempt == nil {
			if intent.State != "prepared" || claim.CloudID != "" {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels create intent for lease %s has no attempt", leaseID)
			}
			// A per-attempt directory, named with a secret nonce, is what makes
			// the resulting bundle attributable to this attempt. It is recorded
			// before the directory is created and before the clone, so a lost
			// reply still leaves the evidence behind.
			intent.Attempt = map[string]string{
				"name": name, "host": intent.ProviderScope, "source_id": sourceID,
				"dst":        strings.TrimRight(vmBase, "/") + "/" + name + "-" + strings.ToLower(rand.Text()),
				"submission": "pending",
			}
			labels := core.DirectLeaseLabels(cfg, leaseID, intent.Slug, parallelsProviderName, "", req.Keep, time.Now().UTC())
			labels["source"], labels["host"] = source, hostLabel
			labels["source_id"] = sourceID
			labels["ssh_user"], labels["work_root"] = cfg.SSHUser, cfg.WorkRoot
			labels["fixed_intent_sha256"] = intent.Fingerprint
			if snapshotID != "" {
				labels["source_snapshot"] = snapshotID
			}
			claim.Labels = labels
			// Persist the host-unique VM name before prlctl clone. A lost reply
			// can still have created the VM, and only this record lets a replay
			// find it instead of cloning a second one.
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		if intent.Attempt["name"] != name || intent.Attempt["host"] != intent.ProviderScope ||
			(intent.Attempt["source_id"] != "" && intent.Attempt["source_id"] != sourceID) {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels attempt identity changed for lease %s", leaseID)
		}
		createDir := parallelsFixedCreationDir(intent)
		if createDir == "" {
			return core.LeaseTarget{}, parallelsUncertainCustody(leaseID, name)
		}

		// A complete inventory read is the only unambiguous absence proof, and
		// it must carry per-VM detail: the plain listing omits the bundle path
		// this attempt identifies its VM by. A failed listing keeps custody
		// instead of provisioning a second VM.
		vms, err := client.ListVMsDetailed(ctx)
		if err != nil {
			return core.LeaseTarget{}, fmt.Errorf("reconcile Parallels lease=%s vm=%s (claim and key retained): %w", leaseID, name, err)
		}
		// The attempt's own creation directory, not the mutable name, decides
		// which VM this attempt produced.
		vm, found, err := parallelsCreatedVM(vms, createDir)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if !found {
			if intent.State != "prepared" || claim.CloudID != "" {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels lease %s no longer has VM %q on host %s; stop the lease instead of replaying it", leaseID, name, shortParallelsScope(intent.ProviderScope))
			}
			if occupant, taken := parallelsVMByName(vms, name); taken {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels lease %s cannot create VM %q: VM %q already occupies that name and was not created by this lease; custody is retained and nothing was changed or deleted", leaseID, name, occupant.ID)
			}
			// A submitted clone may have vanished before its UUID was recorded.
			// Missing submission evidence is equally inconclusive; never recreate it.
			if intent.Attempt["submission"] != "pending" {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels lease %s has no recoverable VM for its submitted or uncertain attempt; claim and key retained, no replacement created", leaseID)
			}
			// Capacity is enforced by counting the host's VMs and then cloning
			// into it, so both must happen under one reservation: the per-lease
			// claim locks do not serialize different lease IDs, and a prepared
			// retry can reach this point long after its host filled up. The
			// reservation is taken only for an actual submission, so replaying
			// or releasing an existing VM still works at capacity.
			releaseCapacity, err := core.ReserveParallelsHostCapacity(ctx, cfg, b.RT.Exec, sourceID)
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
			if err := client.EnsureHostDir(ctx, createDir); err != nil {
				return core.LeaseTarget{}, err
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
			cloneErr := core.NewParallelsClient(cloneCfg, b.RT.Exec).SubmitClone(ctx, sourceID, snapshotID, leaseID, intent.Slug, req.Keep, func() error {
				intent.Attempt["submission"] = "submitted"
				return persist()
			})
			// Whatever the reply said, a submitted clone may already count
			// against maxVMs, and the reservation has done its job either way:
			// the rest of bring-up need not keep other forks waiting.
			releaseCapacityOnce()
			if cloneErr != nil {
				return core.LeaseTarget{}, fmt.Errorf("Parallels clone outcome uncertain for lease=%s vm=%s; claim and key retained, retry the same lease ID or stop it: %w", leaseID, name, cloneErr)
			}
			// Read the created VM back by its creation directory. `prlctl clone`
			// reports no UUID and the name it was given is mutable, so a name
			// lookup here could bind whatever has taken that name since.
			if vms, err = client.ListVMsDetailed(ctx); err != nil {
				return core.LeaseTarget{}, fmt.Errorf("reconcile Parallels lease=%s vm=%s after clone (claim and key retained): %w", leaseID, name, err)
			}
			if vm, found, err = parallelsCreatedVM(vms, createDir); err != nil {
				return core.LeaseTarget{}, err
			} else if !found {
				return core.LeaseTarget{}, fmt.Errorf("Parallels clone reported success but no VM is present in the creation directory for lease=%s; claim and key retained", leaseID)
			}
		}
		// Pin the incarnation this attempt created before anything else touches
		// it. Parallels never reuses a VM UUID, so from here on adoption, guest
		// preparation and deletion all act on this VM and no other.
		if err := bindParallelsFixedIncarnation(claim, intent, vm.ID, name, leaseID); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := persist(); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateParallelsFixedVM(*claim, intent, vm, name); err != nil {
			return core.LeaseTarget{}, err
		}
		claim.CloudID, claim.CloudImmutableID = vm.ID, vm.ID
		if err := persist(); err != nil {
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
		ready, err := client.WaitForIP(ctx, vm.ID, cfg.Parallels.StartupTimeout)
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
	}, ctx)
	if err != nil {
		// A fixed ID keeps its durable attempt and per-lease key on every
		// failure. Rolling the VM back here would make a lost reply
		// indistinguishable from a caller-visible failure.
		return core.LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *leaseBackend) releaseFixed(ctx context.Context, claim core.LeaseClaim, outcome *core.ReleaseLeaseOutcome) error {
	if claim.FixedCreateIntent.State == "released" {
		outcome.Terminal = true
		return nil
	}
	name := strings.TrimSpace(claim.FixedCreateIntent.Attempt["name"])
	if name == "" {
		return core.Exit(4, "Parallels lease %s has no recorded creation attempt", claim.LeaseID)
	}
	_, client, err := parallelsFixedHostConfig(ctx, b.RT.Exec, b.Cfg, claim.LeaseID, claim.ProviderScope)
	if err != nil {
		return err
	}
	incarnation := parallelsFixedIncarnation(claim.FixedCreateIntent)
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
	if claim.FixedCreateIntent.State != "deleting" {
		// Commit the validated deletion phase before the remote effect, so a
		// later absence is safe to finalize rather than ambiguous.
		deleting := claim
		intent := *claim.FixedCreateIntent
		intent.State = "deleting"
		deleting.FixedCreateIntent = &intent
		updated, err := core.ReplaceLeaseClaimIfUnchangedDurableAfter(claim.LeaseID, claim, deleting, func() error {
			_, err := lookup()
			return err
		})
		if err != nil {
			return err
		}
		claim = updated
	}
	err = parallelsFixedLeaseKind.FinalizeAfterCleanup(claim, func() error {
		vm, err := lookup()
		if err != nil || vm == nil {
			outcome.Terminal = err == nil
			return err
		}
		err = client.Delete(ctx, vm.ID)
		outcome.Terminal = err == nil
		if err == nil {
			// The attempt's directory is spent. Remove it only while empty, so
			// anything unexpected still living there is left untouched.
			client.RemoveHostDirIfEmpty(ctx, parallelsFixedCreationDir(claim.FixedCreateIntent))
		}
		return err
	})
	if err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(claim.LeaseID)
	return nil
}

func (b *leaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return parallelsFixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_intent_sha256"] != "", nil, nil)
}
