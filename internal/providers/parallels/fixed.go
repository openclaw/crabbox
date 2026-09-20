package parallels

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// parallelsFixedLeaseKind marks durable fixed-ID claims. The marker stays the
// ordinary provider name so existing exact-ownership checks, resolution, and
// cleanup keep recognizing the claim; a fixed claim is distinguished by its
// durable create intent, not by a separate provider discriminator.
var parallelsFixedLeaseKind = core.FixedLeaseKind{ClaimProvider: parallelsProviderName, IntentVersion: 1, Label: "Parallels"}

const parallelsProviderName = "parallels"

// waitForSSHReady is an indirection seam so lifecycle tests can exercise the
// full acquisition path without a live guest.
var waitForSSHReady = core.WaitForSSHReady

func (*leaseBackend) SupportsRequestedLeaseID() bool { return true }

// parallelsFixedIdentity is the create identity a fixed lease ID is bound to.
// Any change here is intent drift, not a replay.
type parallelsFixedIdentity struct {
	Source         string `json:"source"`
	SourceSnapshot string `json:"sourceSnapshot"`
	CloneMode      string `json:"cloneMode"`
	TargetOS       string `json:"targetOS"`
	WindowsMode    string `json:"windowsMode"`
	GuestUser      string `json:"guestUser"`
	WorkRoot       string `json:"workRoot"`
	VMRoot         string `json:"vmRoot"`
}

func parallelsFixedFingerprint(cfg core.Config, source, snapshotID string) (string, error) {
	data, err := json.Marshal(parallelsFixedIdentity{
		Source:         source,
		SourceSnapshot: snapshotID,
		CloneMode:      strings.ToLower(strings.TrimSpace(core.Blank(cfg.Parallels.CloneMode, "linked"))),
		TargetOS:       cfg.TargetOS,
		WindowsMode:    cfg.WindowsMode,
		GuestUser:      strings.TrimSpace(cfg.SSHUser),
		WorkRoot:       strings.TrimSpace(cfg.WorkRoot),
		VMRoot:         strings.TrimSpace(cfg.Parallels.VMRoot),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// parallelsFixedHostConfig pins a replay to the Parallels host recorded in the
// durable intent. A fixed lease never re-runs fleet selection: its VM lives on
// exactly one host, and silently choosing another one would clone a second VM.
func parallelsFixedHostConfig(base core.Config, leaseID, scope string) (core.Config, error) {
	scope = strings.TrimSpace(scope)
	for _, candidate := range core.ParallelsCandidateConfigs(base) {
		if parallelsHostName(candidate) == scope {
			return candidate, nil
		}
	}
	return core.Config{}, core.Exit(4, "lease_id_conflict: Parallels lease %s is bound to host %q, which is not in the configured fleet", leaseID, core.Blank(scope, "<none>"))
}

func parallelsVMByName(vms []core.ParallelsVM, name string) (core.ParallelsVM, bool) {
	for _, vm := range vms {
		if vm.Name == name {
			return vm, true
		}
	}
	return core.ParallelsVM{}, false
}

// validateParallelsFixedVM re-attests the exact resource identity a fixed lease
// is bound to. Only the recorded attempt name is an adoption key; a slug or a
// lookalike name never is.
func validateParallelsFixedVM(claim core.LeaseClaim, vm core.ParallelsVM, name, scope string) error {
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
	if claim.CloudImmutableID != "" && claim.CloudImmutableID != vm.ID {
		return conflict("Parallels lease %s is bound to VM UUID %q, not %q", claim.LeaseID, claim.CloudImmutableID, vm.ID)
	}
	if claim.CloudID != "" && claim.CloudID != vm.ID {
		return conflict("Parallels lease %s is bound to VM %q, not %q", claim.LeaseID, claim.CloudID, vm.ID)
	}
	if host := strings.TrimSpace(claim.Labels["host"]); host != "" && host != scope {
		return conflict("Parallels lease %s carries host label %q on host %q", claim.LeaseID, host, scope)
	}
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
	var publicKey, snapshotID string

	lease, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind: parallelsFixedLeaseKind, LeaseID: leaseID, CheckpointID: req.RequestedCheckpointID,
		RepoRoot: req.Repo.Root, Reclaim: req.Reclaim, TargetOS: b.Cfg.TargetOS, WindowsMode: b.Cfg.WindowsMode,
		TTL: b.Cfg.TTL, IdleTimeout: b.Cfg.IdleTimeout,
	}, func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		var err error
		if exists {
			if claim.Provider != parallelsProviderName || claim.FixedCreateIntent == nil {
				return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: lease %s already has another owner", leaseID)
			}
			cfg, err = parallelsFixedHostConfig(b.Cfg, leaseID, claim.ProviderScope)
		} else {
			cfg, err = core.SelectParallelsFleetConfig(ctx, b.Cfg, b.RT.Exec, source)
		}
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		client = core.NewParallelsClient(cfg, b.RT.Exec)
		if err := client.ValidateMacOSBootstrapKey(ctx); err != nil {
			return core.FixedLeaseBinding{}, err
		}
		scope := parallelsHostName(cfg)
		if exists && claim.ProviderScope != scope {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Parallels host identity changed for lease %s", leaseID)
		}
		keyPath, key, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.SSHKey, publicKey = keyPath, key
		cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
		snapshotID = shared.FirstNonBlankTrimmed(cfg.Parallels.SourceSnapshotID, cfg.Parallels.SourceSnapshot)
		if snapshotID != "" && cfg.Parallels.SourceSnapshotID == "" {
			// Resolve the name to its stable snapshot ID so a renamed or
			// replaced snapshot is drift rather than a silent different fork.
			if snapshotID, err = client.SnapshotID(ctx, source, snapshotID); err != nil {
				return core.FixedLeaseBinding{}, err
			}
		}
		fingerprint, err := parallelsFixedFingerprint(cfg, source, snapshotID)
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
			intent.Attempt = map[string]string{"name": name, "host": intent.ProviderScope}
			labels := core.DirectLeaseLabels(cfg, leaseID, intent.Slug, parallelsProviderName, "", req.Keep, time.Now().UTC())
			labels["source"], labels["host"] = source, intent.ProviderScope
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
		if intent.Attempt["name"] != name || intent.Attempt["host"] != intent.ProviderScope {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels attempt identity changed for lease %s", leaseID)
		}

		// A complete inventory read is the only unambiguous absence proof. A
		// failed listing keeps custody instead of provisioning a second VM.
		vms, err := client.ListVMs(ctx)
		if err != nil {
			return core.LeaseTarget{}, fmt.Errorf("reconcile Parallels lease=%s vm=%s (claim and key retained): %w", leaseID, name, err)
		}
		vm, found := parallelsVMByName(vms, name)
		if !found {
			if intent.State != "prepared" || claim.CloudID != "" {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels lease %s no longer has VM %q on host %q; stop the lease instead of replaying it", leaseID, name, intent.ProviderScope)
			}
			fmt.Fprintf(b.RT.Stderr, "provisioning provider=parallels lease=%s slug=%s host=%s source=%s snapshot=%s clone_mode=%s keep=%v\n",
				leaseID, intent.Slug, intent.ProviderScope, source, blank(snapshotID, "-"), blank(cfg.Parallels.CloneMode, "linked"), req.Keep)
			// The VM name is host-unique: prlctl refuses a concurrent duplicate
			// create of this exact lease-derived name even after a lost reply.
			created, err := client.Clone(ctx, source, snapshotID, leaseID, intent.Slug, req.Keep)
			if err != nil {
				return core.LeaseTarget{}, fmt.Errorf("Parallels clone outcome uncertain for lease=%s vm=%s; claim and key retained, retry the same lease ID or stop it: %w", leaseID, name, err)
			}
			if created.Name != name {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: Parallels clone produced VM %q, not %q", created.Name, name)
			}
			if vms, err = client.ListVMs(ctx); err != nil {
				return core.LeaseTarget{}, fmt.Errorf("reconcile Parallels lease=%s vm=%s after clone (claim and key retained): %w", leaseID, name, err)
			}
			if vm, found = parallelsVMByName(vms, name); !found {
				return core.LeaseTarget{}, fmt.Errorf("Parallels clone reported success but VM %q is absent for lease=%s; claim and key retained", name, leaseID)
			}
		}
		if err := validateParallelsFixedVM(*claim, vm, name, intent.ProviderScope); err != nil {
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
		if err := b.prepareGuest(ctx, client, vm.ID, ready, cfg, publicKey); err != nil {
			return core.LeaseTarget{}, err
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
	cfg, err := parallelsFixedHostConfig(b.Cfg, claim.LeaseID, claim.ProviderScope)
	if err != nil {
		return err
	}
	scope := parallelsHostName(cfg)
	client := core.NewParallelsClient(cfg, b.RT.Exec)
	// A prepared create's absence is inconclusive: the clone may still be in
	// flight. Only an attempt that reached the provider may finalize on absence.
	lookup := func() (*core.ParallelsVM, error) {
		vms, err := client.ListVMs(ctx)
		if err != nil {
			return nil, err
		}
		vm, found := parallelsVMByName(vms, name)
		if !found {
			if state := claim.FixedCreateIntent.State; state == "acquired" || state == "deleting" {
				return nil, nil
			}
			return nil, core.Exit(4, "lease_id_conflict: prepared Parallels lease %s has no VM %q on host %q; replay the same lease ID before stopping it", claim.LeaseID, name, scope)
		}
		if err := validateParallelsFixedVM(claim, vm, name, scope); err != nil {
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
