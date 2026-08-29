package machine0

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"golang.org/x/crypto/ssh"
)

type backend struct {
	spec                     ProviderSpec
	cfg                      Config
	rt                       Runtime
	api                      machine0API
	sleep                    func(context.Context, time.Duration) error
	stat                     func(string) (os.FileInfo, error)
	knownHostsFile           func(string, string) (string, error)
	waitSSH                  func(context.Context, *SSHTarget, time.Duration) error
	prepareNativeImageSource func(context.Context, SSHTarget) error
}

type machine0PrepareOptions struct {
	Check          bool
	ResetHostTrust bool
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	b := &backend{spec: spec, cfg: cfg, rt: rt}
	b.api = &client{cfg: cfg.Machine0, rt: rt, sleep: sleepContext}
	b.sleep = sleepContext
	b.stat = os.Stat
	b.knownHostsFile = machine0KnownHostsFile
	b.waitSSH = func(ctx context.Context, target *SSHTarget, timeout time.Duration) error {
		return waitForSSHReady(ctx, target, rt.Stderr, "machine0", timeout)
	}
	b.prepareNativeImageSource = core.PrepareNativeImageSource
	return b
}

func applyDefaults(cfg *Config) {
	base := core.BaseConfig().Machine0
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	cfg.WindowsMode = ""
	cfg.SSHPort = sshPort
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.Machine0.CLIPath) == "" {
		cfg.Machine0.CLIPath = base.CLIPath
	}
	if strings.TrimSpace(cfg.Machine0.Image) == "" {
		cfg.Machine0.Image = base.Image
	}
	if strings.TrimSpace(cfg.Machine0.Size) == "" {
		cfg.Machine0.Size = base.Size
	}
	cfg.Machine0.Size = (Provider{}).ServerTypeForConfig(*cfg)
	if strings.TrimSpace(cfg.Machine0.Region) == "" {
		cfg.Machine0.Region = base.Region
	}
	if strings.TrimSpace(cfg.Machine0.WorkRoot) == "" {
		cfg.Machine0.WorkRoot = base.WorkRoot
	}
	if strings.TrimSpace(cfg.Machine0.ReleasePolicy) == "" {
		cfg.Machine0.ReleasePolicy = base.ReleasePolicy
	}
	if cfg.Machine0.CreateTimeout <= 0 {
		cfg.Machine0.CreateTimeout = base.CreateTimeout
	}
	if cfg.Machine0.PollInterval <= 0 {
		cfg.Machine0.PollInterval = base.PollInterval
	}
	cfg.WorkRoot = cfg.Machine0.WorkRoot
	cfg.ServerType = cfg.Machine0.Size
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) SupportsRequestedLeaseID() bool { return true }

func (b *backend) SupportsRequestedCheckpointID() bool { return true }

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func effectiveMachine0Config(cfg Config, item machine) Config {
	user := machine0SSHUser(item)
	workRoot := cfg.Machine0.WorkRoot
	if strings.TrimSpace(workRoot) == "" {
		workRoot = "/home/" + user + "/crabbox"
	}
	cfg.SSHUser = user
	cfg.WorkRoot = workRoot
	cfg.Machine0.WorkRoot = workRoot
	return cfg
}

func machine0SSHUser(item machine) string {
	if user := strings.TrimSpace(item.DefaultSSHUsername); user != "" {
		return user
	}
	if strings.EqualFold(strings.TrimSpace(item.Distribution), "nixos") {
		return "nix"
	}
	return "ubuntu"
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if strings.TrimSpace(req.RequestedLeaseID) != "" {
		return b.acquireFixed(ctx, req)
	}
	cfg := b.configForRun()
	if err := b.preflightSSHKey(ctx, cfg.Machine0.Key); err != nil {
		return LeaseTarget{}, err
	}
	if err := b.validateCatalogSelection(ctx, cfg.Machine0.Size, cfg.Machine0.Region); err != nil {
		return LeaseTarget{}, err
	}
	machines, err := b.api.List(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := machine0Claims()
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := make([]Server, 0, len(machines))
	for _, item := range machines {
		servers = append(servers, b.serverFromMachine(item, claims[item.ID], cfg))
	}
	leaseID := newLeaseID()
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := machine0MachineName(leaseID, slug)
	image := cfg.Machine0.Image
	if req.Options.Desktop && strings.TrimSpace(cfg.Machine0.DesktopImage) != "" {
		image = cfg.Machine0.DesktopImage
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s size=%s region=%s image=%s keep=%v\n", providerName, leaseID, slug, name, cfg.Machine0.Size, cfg.Machine0.Region, image, req.Keep)
	if err := b.api.Create(ctx, createMachineRequest{Name: name, Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: image, ImageVersion: cfg.Machine0.ImageVersion, Key: cfg.Machine0.Key}); err != nil {
		return LeaseTarget{}, err
	}
	pendingMachine := machine{Name: name, Status: "CREATING", Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: image, ImageVersion: cfg.Machine0.ImageVersion}
	pendingServer := Server{Provider: providerName, Name: name, Status: "provisioning", Labels: machineLabels(cfg, pendingMachine, leaseID, slug, req.Keep, b.now())}
	pendingServer.Labels["recovery"] = "create-pending"
	recoveryClaim, claimErr := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		leaseID, slug, cfg, machine0NameScope(name), pendingServer, SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, LeaseClaim{}, false,
	)
	if claimErr != nil || recoveryClaim.LeaseID == "" {
		if claimErr == nil {
			claimErr = errors.New("claim store did not publish the created machine")
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if cleanupErr := b.api.Remove(cleanupCtx, name); cleanupErr != nil {
			return LeaseTarget{}, errors.Join(fmt.Errorf("persist machine0 recovery claim for %s: %w", name, claimErr), fmt.Errorf("machine0 rollback failed for %s; remove the machine manually: %w", name, cleanupErr))
		}
		return LeaseTarget{}, fmt.Errorf("persist machine0 recovery claim for %s: %w", name, claimErr)
	}
	rollback := func(cause error) error {
		if req.Keep {
			return cause
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if cleanupErr := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, recoveryClaim, func() error {
			return b.api.Remove(cleanupCtx, name)
		}); cleanupErr != nil {
			return errors.Join(cause, fmt.Errorf("machine0 rollback failed for %s; recovery claim %s remains for cleanup: %w", name, leaseID, cleanupErr))
		}
		return cause
	}
	item, err := b.waitForRunning(ctx, name, cfg.Machine0.CreateTimeout)
	if err != nil {
		return LeaseTarget{}, rollback(err)
	}
	if item.Name != name {
		return LeaseTarget{}, rollback(exit(5, "machine0 create returned mismatched machine name: expected %s, found %s", name, item.Name))
	}
	cfg = effectiveMachine0Config(cfg, item)
	boundClaim, err := b.bindRecoveryClaim(recoveryClaim, item, cfg)
	if err != nil {
		return LeaseTarget{}, rollback(err)
	}
	recoveryClaim = boundClaim
	claim := LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: machineScope(item.ID)}
	server := b.serverFromMachine(item, claim, cfg)
	server.Labels = machineLabels(cfg, item, leaseID, slug, req.Keep, b.now())
	lease, err := b.prepareLease(ctx, item, server, leaseID, true)
	if err != nil {
		return LeaseTarget{}, rollback(err)
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			return LeaseTarget{}, rollback(err)
		}
	}
	if err := claimLease(leaseID, slug, cfg, req.Repo.Root, req.Reclaim, lease.Server, lease.SSH); err != nil {
		return LeaseTarget{}, rollback(err)
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s machine=%s resource=%s state=ready\n", leaseID, item.Name, item.ID)
	return lease, nil
}

func (b *backend) preflightSSHKey(ctx context.Context, name string) error {
	key, err := b.api.SelectedKey(ctx, name)
	if err != nil {
		return err
	}
	if key == nil || !strings.EqualFold(strings.TrimSpace(key.Type), "PUBLIC") {
		return nil
	}
	fileName := strings.TrimSpace(key.FileName)
	if fileName == "" || filepath.IsAbs(fileName) || fileName == "." || fileName == ".." || strings.ContainsAny(fileName, `/\`) || filepath.Base(fileName) != fileName {
		return exit(2, "Machine0 PUBLIC SSH key %q has no usable local private-key filename; select a managed key with --machine0-key <managed-key-name>", blank(key.Name, "<default>"))
	}
	keyRoot := strings.TrimSpace(os.Getenv("SSH_KEY_PATH"))
	if keyRoot == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || strings.TrimSpace(home) == "" {
			return exit(2, "resolve private key for Machine0 PUBLIC SSH key %q: set SSH_KEY_PATH or select --machine0-key <managed-key-name>", blank(key.Name, "<default>"))
		}
		keyRoot = filepath.Join(home, ".ssh")
	}
	keyPath := filepath.Join(keyRoot, fileName)
	info, err := b.stat(keyPath)
	if err == nil && !info.IsDir() {
		// Special files are unverified: extraction can block opening a FIFO/device.
		if !info.Mode().IsRegular() {
			return nil
		}
		registered := machine0BarePublicKey(key.PublicKey)
		if registered == nil {
			return nil
		}
		// Like native Machine0 SSH, compare the private file, not its .pub sidecar.
		// Failed noninteractive extraction (including encrypted keys) is unknown.
		result, extractErr := b.rt.Exec.Run(ctx, LocalCommandRequest{
			Name: "ssh-keygen", Args: []string{"-y", "-P", "", "-f", keyPath},
			MaxCapturedOutputBytes: 64 << 10,
		})
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if extractErr == nil && result.ExitCode == 0 {
			if local := machine0BarePublicKey(result.Stdout); local != nil && !bytes.Equal(local.Marshal(), registered.Marshal()) {
				return exit(2, "Machine0 PUBLIC SSH key does not match the local private key; provide the matching local key or select the intended key with --machine0-key")
			}
		}
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return exit(2, "inspect private key for Machine0 PUBLIC SSH key %q at %q: %v", blank(key.Name, "<default>"), keyPath, err)
	}
	return exit(2, "Machine0 PUBLIC SSH key %q has no local private key at %q; select a managed key with --machine0-key <managed-key-name>", blank(key.Name, "<default>"), keyPath)
}

// Certificates, options and multiple records need more than a bare-key comparison.
func machine0BarePublicKey(value string) ssh.PublicKey {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\r\n") {
		return nil
	}
	key, _, options, _, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil || len(options) != 0 {
		return nil
	}
	// ParseAuthorizedKey can discard an empty option prefix such as ", ".
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != key.Type() {
		return nil
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return nil
	}
	return key
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	baseCfg := b.configForRun()
	claim, claimed, err := resolveClaim(req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	lookup := strings.TrimSpace(req.ID)
	if claimed {
		lookup = firstNonBlank(claim.Labels["machine0_name"], claim.CloudID, lookup)
	}
	var item machine
	if !claimed && core.IsCanonicalLeaseID(lookup) {
		machines, err := b.api.List(ctx)
		if err != nil {
			return LeaseTarget{}, err
		}
		suffix := machine0LeaseSuffix(lookup)
		for _, candidate := range machines {
			if !strings.HasPrefix(candidate.Name, "crabbox-") || !strings.HasSuffix(candidate.Name, suffix) {
				continue
			}
			if item.Name != "" {
				return LeaseTarget{}, exit(4, "multiple Machine0 machines match lease %s", lookup)
			}
			item = candidate
		}
		if item.Name == "" {
			return LeaseTarget{}, exit(4, "lease/server not found: %s", lookup)
		}
		return LeaseTarget{}, exit(4, "machine0 lease %s has no local claim; candidate %q matches only a short name hash: inspect the machine and use its explicit name with --reclaim to adopt it", lookup, item.Name)
	} else {
		item, err = b.api.Get(ctx, lookup)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	cfg := effectiveMachine0Config(baseCfg, item)
	if claimed && claim.CloudID != "" && claim.CloudID != item.ID {
		return LeaseTarget{}, exit(4, "machine0 lease=%s resource identity changed: expected %s, found %s", claim.LeaseID, claim.CloudID, item.ID)
	}
	if !claimed && !req.Reclaim && !req.ReleaseOnly && !req.StatusOnly {
		return LeaseTarget{}, exit(4, "machine0 machine %q has no Crabbox lease claim; reuse it with explicit --reclaim", item.Name)
	}
	leaseID := claim.LeaseID
	slug := claim.Slug
	if !claimed && req.Reclaim {
		if req.Repo.Root == "" {
			return LeaseTarget{}, exit(2, "machine0 --reclaim requires repository context")
		}
		leaseID = newLeaseID()
		slug = core.NormalizeLeaseSlug(item.Name)
		if slug == "" {
			slug = core.NewLeaseSlug(leaseID)
		}
		claim = LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: machineScope(item.ID)}
	}
	server := b.serverFromMachine(item, claim, cfg)
	if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	resetHostTrust := !machineRunning(item.Status)
	if resetHostTrust {
		item, err = b.waitForResolveRunning(ctx, item, cfg.Machine0.CreateTimeout)
		if err != nil {
			return LeaseTarget{}, err
		}
		cfg = effectiveMachine0Config(baseCfg, item)
		server = b.serverFromMachine(item, claim, cfg)
	}
	lease, err := b.prepareLeaseWithOptions(ctx, item, server, leaseID, machine0PrepareOptions{Check: req.Prepare || req.ReadyProbe, ResetHostTrust: resetHostTrust})
	if err != nil {
		return LeaseTarget{}, err
	}
	if !claimed && req.Reclaim {
		lease.Server.Labels = machineLabels(cfg, item, leaseID, slug, true, b.now())
		if err := claimLease(leaseID, slug, cfg, req.Repo.Root, true, lease.Server, lease.SSH); err != nil {
			return LeaseTarget{}, err
		}
	} else if claimed {
		if _, err := updateClaim(leaseID, claim, lease.Server, lease.SSH); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	machines, err := b.api.List(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := machine0Claims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(machines))
	for _, item := range machines {
		claim := claims[item.ID]
		if claim.LeaseID == "" {
			claim = claims[machine0NameScope(item.Name)]
		}
		if claim.LeaseID == "" && !req.All {
			continue
		}
		views = append(views, b.serverFromMachine(item, claim, effectiveMachine0Config(cfg, item)))
	}
	return views, nil
}

func (b *backend) AuthorizeStatusTouchClaim(_ context.Context, lease LeaseTarget, claim LeaseClaim) error {
	resourceID := strings.TrimSpace(lease.Server.CloudID)
	if !core.IsCanonicalLeaseID(lease.LeaseID) || claim.LeaseID != lease.LeaseID ||
		lease.Server.Provider != providerName || !isMachine0ClaimProvider(claim.Provider) ||
		resourceID == "" || lease.Server.ImmutableID != resourceID || claim.CloudImmutableID != resourceID {
		return exit(4, "refusing machine0 lifecycle touch for lease=%s resource=%s: claim does not match the canonical lease, provider, or immutable Machine0 identity", lease.LeaseID, blank(resourceID, "<empty>"))
	}
	if err := validateMachineClaimOwnership(claim, machine{ID: resourceID}); err != nil {
		return exit(4, "refusing machine0 lifecycle touch for lease=%s resource=%s: %v", lease.LeaseID, resourceID, err)
	}
	return nil
}

func (b *backend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	expected, exists, set := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return Server{}, exit(4, "machine0 lease %s has no exact claim snapshot; refusing touch", req.Lease.LeaseID)
	}
	if err := b.AuthorizeStatusTouchClaim(ctx, req.Lease, expected); err != nil {
		return Server{}, err
	}
	if req.IdleTimeoutOverride != nil && *req.IdleTimeoutOverride <= 0 {
		return Server{}, exit(2, "machine0 lease %s idle timeout override must be positive", req.Lease.LeaseID)
	}

	cfg := b.configForRun()
	if expected.IdleTimeoutSeconds > 0 {
		cfg.IdleTimeout = time.Duration(expected.IdleTimeoutSeconds) * time.Second
	}
	labels := shared.CloneLabels(expected.Labels)
	for _, key := range machineLabelKeys {
		if value := req.Lease.Server.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	now := b.now()
	labels = core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(labels, cfg, req.State, now, req.IdleTimeoutOverride)
	updated, err := core.UpdateLeaseClaimTouchIfUnchanged(req.Lease.LeaseID, expected, labels, now, req.IdleTimeoutOverride)
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	server.Labels = updated.Labels
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	identifier := firstNonBlank(req.Lease.LeaseID, req.Lease.Server.Labels["lease"], req.Lease.Server.CloudID, req.Lease.Server.Name)
	claim, claimed, err := resolveClaim(identifier)
	if err != nil {
		return err
	}
	if claimed && claim.CloudID == "" && claim.Labels["recovery"] == "create-pending" {
		name := machine0MachineName(claim.LeaseID, claim.Slug)
		if claim.ProviderScope != machine0NameScope(name) || claim.Labels["machine0_name"] != name {
			return exit(2, "refusing machine0 recovery release for lease=%s: pending machine name does not match its durable claim", claim.LeaseID)
		}
		if normalizeReleasePolicy(b.configForRun().Machine0.ReleasePolicy) != "destroy" {
			return exit(2, "pending machine0 lease=%s cannot be suspended before its resource identity is known; use --machine0-release-policy destroy", claim.LeaseID)
		}
		// Pending claims retain only the exact-name deletion already authorized by create rollback.
		return core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error { return b.api.Remove(ctx, name) })
	}
	claim, item, err := b.releaseTarget(ctx, req.Lease)
	if err != nil {
		return err
	}
	if normalizeReleasePolicy(b.configForRun().Machine0.ReleasePolicy) == "suspend" {
		return b.suspendClaimedMachine(ctx, claim, item)
	}
	return fixedMachine0LeaseKind.FinalizeAfterCleanup(claim, func() error { return b.api.Remove(ctx, item.Name) })
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	cfg := b.configForRun()
	machines, err := b.api.List(ctx)
	if err != nil {
		return err
	}
	claims, err := machine0Claims()
	if err != nil {
		return err
	}
	liveClaims := map[string]bool{}
	removed := 0
	for _, item := range machines {
		claim := claims[item.ID]
		if claim.LeaseID != "" {
			liveClaims[claim.LeaseID] = true
		}
		if err := validateMachineClaimOwnership(claim, item); err != nil {
			fmt.Fprintf(b.rt.Stderr, "skip machine name=%s reason=ownership: %v\n", item.Name, err)
			continue
		}
		server := b.serverFromMachine(item, claim, cfg)
		should, reason := shouldCleanupMachine0(server, claim, claim.LeaseID != "", b.now())
		if !should {
			fmt.Fprintf(b.rt.Stderr, "skip machine name=%s reason=%s\n", item.Name, reason)
			continue
		}
		actionName := "remove"
		if normalizeReleasePolicy(cfg.Machine0.ReleasePolicy) == "suspend" {
			actionName = "suspend"
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would %s machine name=%s lease=%s reason=%s\n", actionName, item.Name, claim.LeaseID, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "%s machine name=%s lease=%s reason=%s\n", actionName, item.Name, claim.LeaseID, reason)
		if normalizeReleasePolicy(cfg.Machine0.ReleasePolicy) == "suspend" {
			if err := b.suspendClaimedMachine(ctx, claim, item); err != nil {
				return err
			}
		} else if err := fixedMachine0LeaseKind.FinalizeAfterCleanup(claim, func() error { return b.api.Remove(ctx, item.Name) }); err != nil {
			return err
		}
		removed++
	}
	claimsRemoved := 0
	for _, claim := range claims {
		if claim.LeaseID == "" || liveClaims[claim.LeaseID] {
			continue
		}
		if fixedMachine0LeaseKind.IsFixedClaim(claim) && claim.FixedCreateIntent.State == fixedMachine0IntentReleased {
			continue
		}
		if claim.CloudID == "" || claim.ProviderScope != machineScope(claim.CloudID) {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=ownership: incomplete Machine0 identity\n", claim.LeaseID)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=missing machine\n", claim.LeaseID)
			continue
		}
		if err := fixedMachine0LeaseKind.FinalizeAfterCleanup(claim, nil); err != nil {
			return err
		}
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(machines))
	}
	return nil
}

func (b *backend) RetainLeaseClaimAfterRelease(lease LeaseTarget) bool {
	retained, err := b.retainLeaseClaimAfterRelease(lease, LeaseClaim{})
	return retained || err != nil
}

func (b *backend) RetainLeaseClaimAfterReleaseWithClaim(lease LeaseTarget, previous LeaseClaim) (bool, error) {
	return b.retainLeaseClaimAfterRelease(lease, previous)
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	action := normalizeReleasePolicy(b.configForRun().Machine0.ReleasePolicy)
	return fmt.Sprintf("released lease=%s machine=%s action=%s", lease.LeaseID, blank(lease.Server.Name, "-"), action)
}

func (b *backend) Pause(ctx context.Context, req PauseRequest) error {
	claim, item, err := b.releaseTarget(ctx, LeaseTarget{LeaseID: req.ID})
	if err != nil {
		return err
	}
	return b.suspendClaimedMachine(ctx, claim, item)
}

func (b *backend) Resume(ctx context.Context, req ResumeRequest) error {
	claim, item, err := b.releaseTarget(ctx, LeaseTarget{LeaseID: req.ID})
	if err != nil {
		return err
	}
	resetHostTrust := !machineRunning(item.Status)
	if resetHostTrust {
		if err := withClaimUnchanged(claim.LeaseID, claim, func() error { return b.api.Start(ctx, item.Name) }); err != nil {
			return err
		}
		item, err = b.waitForRunningAfterStart(ctx, item.Name, b.configForRun().Machine0.CreateTimeout)
		if err != nil {
			return err
		}
	}
	server := b.serverFromMachine(item, claim, b.configForRun())
	lease, err := b.prepareLeaseWithOptions(ctx, item, server, claim.LeaseID, machine0PrepareOptions{Check: true, ResetHostTrust: resetHostTrust})
	if err != nil {
		return err
	}
	_, err = updateClaim(claim.LeaseID, claim, lease.Server, lease.SSH)
	return err
}

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	type probeResult struct {
		version  string
		sizes    []machineSize
		machines []machine
		err      error
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan probeResult, 3)
	go func() {
		version, err := b.api.Version(probeCtx)
		results <- probeResult{version: version, err: err}
	}()
	go func() {
		sizes, err := b.api.Sizes(probeCtx)
		results <- probeResult{sizes: sizes, err: err}
	}()
	go func() {
		machines, err := b.api.List(probeCtx)
		results <- probeResult{machines: machines, err: err}
	}()
	var version string
	var sizes []machineSize
	var machines []machine
	var probeErr error
	for range 3 {
		result := <-results
		if result.err != nil {
			if probeErr == nil {
				probeErr = result.err
				cancel()
			}
			continue
		}
		if result.version != "" {
			version = result.version
		}
		if result.sizes != nil {
			sizes = result.sizes
		}
		if result.machines != nil {
			machines = result.machines
		}
	}
	if probeErr != nil {
		return DoctorResult{}, probeErr
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	return DoctorResult{Provider: providerName, Message: fmt.Sprintf("cli=ready auth=ready control_plane=ready inventory=ready mutation=false leases=%d sizes=%d runtime=%s version=%s", len(machines), len(sizes), probe, firstLine(version))}, nil
}

func (b *backend) SizeCatalog(ctx context.Context, _ bool) ([]core.ProviderSize, error) {
	sizes, err := b.api.Sizes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.ProviderSize, 0, len(sizes))
	for _, size := range sizes {
		entry := core.ProviderSize{Name: size.Size, VCPU: size.VCPU, RAMGB: size.RAMGB, DiskGB: size.DiskGB, Regions: append([]string(nil), size.Regions...), PricePerHourMicro: size.PricePerHourMicro, TransferGiBPerMonth: size.TransferGiBPerMonth, EstimatedSnapshotGB: size.EstimatedSnapshotGB, DefaultImage: size.DefaultImage}
		if size.GPU != nil {
			entry.GPU = &core.ProviderSizeGPU{Label: size.GPU.Label, VRAMGB: size.GPU.VRAMGB, ScratchDiskGB: size.GPU.ScratchDiskGB}
		}
		if len(size.ProviderMetadata) > 0 {
			entry.ProviderMetadata = map[string]string{}
			for key, value := range size.ProviderMetadata {
				entry.ProviderMetadata[key] = fmt.Sprint(value)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func (b *backend) validateCatalogSelection(ctx context.Context, sizeName, region string) error {
	sizes, err := b.api.Sizes(ctx)
	if err != nil {
		return err
	}
	for _, size := range sizes {
		if size.Size != sizeName {
			continue
		}
		for _, available := range size.Regions {
			if available == region {
				return nil
			}
		}
		return exit(2, "machine0 size %q is not currently available in region %q; available regions: %s", sizeName, region, blank(strings.Join(size.Regions, ","), "none"))
	}
	available := make([]string, 0, len(sizes))
	for _, size := range sizes {
		available = append(available, size.Size)
	}
	return exit(2, "machine0 size %q is not in the live catalog; available: %s", sizeName, strings.Join(available, ","))
}

func (b *backend) waitForRunning(ctx context.Context, name string, timeout time.Duration) (machine, error) {
	return b.waitForRunningState(ctx, name, timeout, "")
}

func (b *backend) waitForRunningAfterStart(ctx context.Context, name string, timeout time.Duration) (machine, error) {
	return b.waitForRunningState(ctx, name, timeout, "RUNNING after start")
}

func (b *backend) waitForRunningState(ctx context.Context, name string, timeout time.Duration, target string) (machine, error) {
	return b.pollMachine(ctx, name, timeout, target, nil, func(_ context.Context, item machine) (bool, error) {
		if done, err := runningMachineResult(item); done || err != nil {
			return done, err
		}
		if target == "" {
			switch {
			case machineStopped(item.Status):
				return false, exit(5, "machine0 machine %s reached stable state %s while waiting for creation", item.Name, item.Status)
			case !machineAcquirePending(item.Status):
				return false, exit(5, "machine0 machine %s entered unexpected state %s", item.Name, item.Status)
			}
		} else if !machinePending(item.Status) && !machineStopped(item.Status) {
			return false, exit(5, "machine0 machine %s entered unexpected state %s after start", item.Name, item.Status)
		}
		return false, nil
	})
}

func (b *backend) waitForResolveRunning(ctx context.Context, initial machine, timeout time.Duration) (machine, error) {
	started := false
	return b.pollMachine(ctx, initial.Name, timeout, "RUNNING during resolve", &initial, func(observeCtx context.Context, item machine) (bool, error) {
		if done, err := runningMachineResult(item); done || err != nil {
			return done, err
		}
		switch {
		case machineStopped(item.Status) && !started:
			if err := b.api.Start(observeCtx, item.Name); err != nil {
				return false, err
			}
			started = true
		case !machinePending(item.Status) && !machineStopped(item.Status):
			return false, exit(5, "machine0 machine %s entered unexpected state %s while resolving", item.Name, item.Status)
		}
		return false, nil
	})
}

func (b *backend) waitForSuspended(ctx context.Context, name string, timeout time.Duration) (machine, error) {
	item, err := b.waitForExactMachine(ctx, name, timeout, "SUSPENDED", "suspending", func(item machine) bool {
		return machineRunning(item.Status) || machineStopped(item.Status) || machineSuspending(item.Status)
	})
	if err == nil {
		item.IP = ""
	}
	return item, err
}

func (b *backend) waitForStopped(ctx context.Context, name string, timeout time.Duration) (machine, error) {
	return b.waitForExactMachine(ctx, name, timeout, "STOPPED", "stopping", func(item machine) bool {
		return machineRunning(item.Status) || strings.EqualFold(strings.TrimSpace(item.Status), "STOPPING")
	})
}

func (b *backend) waitForExactMachine(ctx context.Context, name string, timeout time.Duration, target, phase string, pending func(machine) bool) (machine, error) {
	return b.pollMachine(ctx, name, timeout, target, nil, func(_ context.Context, item machine) (bool, error) {
		if strings.EqualFold(strings.TrimSpace(item.Status), target) {
			return true, nil
		}
		if machineTerminal(item.Status) {
			return false, terminalMachineError(item)
		}
		if !pending(item) {
			return false, exit(5, "machine0 machine %s entered unexpected state %s while %s", item.Name, item.Status, phase)
		}
		return false, nil
	})
}

func (b *backend) pollMachine(
	ctx context.Context,
	name string,
	timeout time.Duration,
	target string,
	initial *machine,
	check func(context.Context, machine) (bool, error),
) (machine, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := shared.Poll(waitCtx, 0, b.configForRun().Machine0.PollInterval, b.sleep, func(observeCtx context.Context) (machine, error) {
		if initial != nil {
			item := *initial
			initial = nil
			return item, nil
		}
		return b.api.Get(observeCtx, name)
	}, func(checkCtx context.Context, item machine, fetchErr error) (bool, error) {
		if fetchErr != nil {
			return false, fetchErr
		}
		return check(checkCtx, item)
	}, nil)
	if err != nil && context.Cause(ctx) == nil && errors.Is(context.Cause(waitCtx), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		if target == "" {
			err = exit(5, "timed out waiting for machine0 machine %s; last state=%s", name, blank(result.Value.Status, "unknown"))
		} else {
			err = exit(5, "timed out waiting for machine0 machine %s to reach %s; last state=%s", name, target, blank(result.Value.Status, "unknown"))
		}
	}
	return result.Value, err
}

func runningMachineResult(item machine) (bool, error) {
	if machineTerminal(item.Status) {
		return false, terminalMachineError(item)
	}
	if !machineRunning(item.Status) {
		return false, nil
	}
	if strings.TrimSpace(item.IP) == "" {
		return false, exit(5, "machine0 machine %s is RUNNING without an IP", item.Name)
	}
	return true, nil
}

func (b *backend) suspendClaimedMachine(ctx context.Context, claim LeaseClaim, item machine) error {
	_, _, _, err := updateClaimAction(claim.LeaseID, claim, func() (Server, SSHTarget, bool, error) {
		suspended := item
		var err error
		if !strings.EqualFold(strings.TrimSpace(item.Status), "SUSPENDED") {
			if err := b.api.Suspend(ctx, item.Name); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			suspended, err = b.waitForSuspended(ctx, item.Name, b.configForRun().Machine0.CreateTimeout)
			if err != nil {
				return Server{}, SSHTarget{}, false, err
			}
		}
		suspended.IP = ""
		server := b.serverFromMachine(suspended, claim, b.configForRun())
		return server, SSHTarget{}, true, nil
	})
	return err
}

func (b *backend) prepareLease(ctx context.Context, item machine, server Server, leaseID string, check bool) (LeaseTarget, error) {
	return b.prepareLeaseWithOptions(ctx, item, server, leaseID, machine0PrepareOptions{Check: check})
}

func (b *backend) prepareLeaseWithOptions(ctx context.Context, item machine, server Server, leaseID string, opts machine0PrepareOptions) (LeaseTarget, error) {
	if !machineRunning(item.Status) || strings.TrimSpace(item.IP) == "" {
		return LeaseTarget{}, exit(4, "machine0 machine %s is not ready for SSH; state=%s", item.Name, item.Status)
	}
	cfg := b.configForRun()
	user := machine0SSHUser(item)
	keyPath := cfg.SSHKey
	keyRoot := strings.TrimSpace(os.Getenv("SSH_KEY_PATH"))
	var keyFileName string
	if item.Key != nil {
		keyFileName = strings.TrimSpace(item.Key.FileName)
		if keyName := strings.TrimSpace(item.Key.Name); keyFileName == "" && keyName != "" && !strings.EqualFold(strings.TrimSpace(item.Key.Type), "PUBLIC") {
			keyFileName = "machine0__" + keyName
		}
	}
	if keyFileName != "" {
		if filepath.IsAbs(keyFileName) || keyFileName == "." || keyFileName == ".." || strings.ContainsAny(keyFileName, `/\`) || filepath.Base(keyFileName) != keyFileName {
			return LeaseTarget{}, exit(2, "Machine0 returned invalid SSH key filename %q; key filename must be a single basename", keyFileName)
		}
		if keyRoot == "" {
			home, err := os.UserHomeDir()
			if err != nil || strings.TrimSpace(home) == "" {
				return LeaseTarget{}, exit(2, "resolve Machine0 SSH key directory for %s: set SSH_KEY_PATH or configure a user home", item.Name)
			}
			keyRoot = filepath.Join(home, ".ssh")
		}
		keyPath = filepath.Join(keyRoot, keyFileName)
		if opts.Check {
			info, err := b.stat(keyPath)
			switch {
			case err == nil && info.IsDir():
				return LeaseTarget{}, exit(2, "Machine0 SSH key path %q is a directory; set SSH_KEY_PATH to the directory containing private key %s", keyPath, keyFileName)
			case err == nil:
			case !errors.Is(err, os.ErrNotExist):
				return LeaseTarget{}, exit(2, "inspect Machine0 SSH key %q: %v", keyPath, err)
			default:
				primeErr := b.api.PrimeSSH(ctx, item.Name)
				info, statErr := b.stat(keyPath)
				if statErr != nil || info.IsDir() {
					missingErr := exit(2, "Machine0 SSH key %q is missing after `machine0 ssh %s true`; set SSH_KEY_PATH to the directory containing private key %s (resolved directory: %q), or ensure Machine0 can materialize it", keyPath, item.Name, keyFileName, keyRoot)
					if primeErr != nil {
						return LeaseTarget{}, errors.Join(primeErr, missingErr)
					}
					return LeaseTarget{}, missingErr
				}
				if primeErr != nil {
					return LeaseTarget{}, primeErr
				}
			}
		}
	}
	if keyRoot == "" {
		if strings.TrimSpace(keyPath) != "" {
			keyRoot = filepath.Dir(keyPath)
		} else {
			home, err := os.UserHomeDir()
			if err != nil || strings.TrimSpace(home) == "" {
				return LeaseTarget{}, exit(2, "resolve Machine0 SSH trust directory for %s: set SSH_KEY_PATH or configure a user home", item.Name)
			}
			keyRoot = filepath.Join(home, ".ssh")
		}
	}
	knownHostsFile, err := b.knownHostsFile(keyRoot, item.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if opts.ResetHostTrust {
		if err := resetMachine0HostTrust(knownHostsFile); err != nil {
			return LeaseTarget{}, err
		}
	}
	target := SSHTarget{User: user, Host: item.IP, Key: keyPath, KnownHostsFile: knownHostsFile, Port: sshPort, TargetOS: targetLinux, NetworkKind: core.NetworkPublic, ReadyCheck: "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"}
	if opts.Check {
		if err := b.waitSSH(ctx, &target, bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func machine0KnownHostsFile(keyRoot, machineID string) (string, error) {
	machineID = strings.TrimSpace(machineID)
	if machineID == "" {
		return "", exit(2, "resolve Machine0 SSH host trust: missing immutable machine id")
	}
	dir := filepath.Join(keyRoot, "crabbox", providerName, "known_hosts.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", exit(2, "create Machine0 SSH host trust directory %q: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", exit(2, "secure Machine0 SSH host trust directory %q: %v", dir, err)
	}
	sum := sha256.Sum256([]byte(machineID))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])), nil
}

func resetMachine0HostTrust(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return exit(2, "inspect Machine0 SSH host trust %q before reset: %v", path, err)
	}
	if info.IsDir() {
		return exit(2, "refusing to reset Machine0 SSH host trust %q because it is a directory", path)
	}
	if err := os.Remove(path); err != nil {
		return exit(2, "reset Machine0 SSH host trust %q: %v", path, err)
	}
	return nil
}

func (b *backend) releaseTarget(ctx context.Context, lease LeaseTarget) (LeaseClaim, machine, error) {
	identifier := firstNonBlank(lease.LeaseID, lease.Server.Labels["lease"], lease.Server.CloudID, lease.Server.Name)
	claim, claimed, err := resolveClaim(identifier)
	if err != nil {
		return LeaseClaim{}, machine{}, err
	}
	if !claimed {
		return LeaseClaim{}, machine{}, exit(2, "refusing to release machine0 machine without an exact local Crabbox claim")
	}
	lookup := firstNonBlank(claim.Labels["machine0_name"], claim.CloudID)
	item, err := b.api.Get(ctx, lookup)
	if err != nil {
		return LeaseClaim{}, machine{}, err
	}
	if err := validateMachineClaimOwnership(claim, item); err != nil {
		return LeaseClaim{}, machine{}, exit(2, "refusing machine0 release for lease=%s: %v", claim.LeaseID, err)
	}
	return claim, item, nil
}

func (b *backend) bindRecoveryClaim(claim LeaseClaim, item machine, cfg Config) (LeaseClaim, error) {
	expectedName := machine0MachineName(claim.LeaseID, claim.Slug)
	if claim.Labels["recovery"] != "create-pending" || claim.CloudID != "" ||
		claim.ProviderScope != machine0NameScope(expectedName) || claim.Labels["machine0_name"] != expectedName ||
		item.Name != expectedName || strings.TrimSpace(item.ID) == "" {
		return LeaseClaim{}, exit(2, "Machine0 recovery claim does not match its durable machine name and resource identity")
	}
	server := b.serverFromMachine(item, claim, cfg)
	server.Labels["recovery"] = "create-pending"
	bound, err := core.ClaimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged(
		claim.LeaseID, claim.Slug, cfg, machineScope(item.ID), server, SSHTarget{}, claim.RepoRoot, cfg.IdleTimeout, false, claim, true,
	)
	if err != nil {
		return LeaseClaim{}, err
	}
	if bound.LeaseID != claim.LeaseID || bound.CloudID != item.ID || bound.ProviderScope != machineScope(item.ID) {
		return LeaseClaim{}, exit(2, "Machine0 recovery claim did not publish its immutable resource identity")
	}
	return bound, nil
}

func validateMachineClaimOwnership(claim LeaseClaim, item machine) error {
	if strings.TrimSpace(claim.LeaseID) == "" {
		return errors.New("missing lease identity")
	}
	if strings.TrimSpace(claim.CloudID) == "" {
		return errors.New("missing immutable Machine0 id")
	}
	if claim.CloudID != item.ID {
		return fmt.Errorf("Machine0 id mismatch: claim=%s machine=%s", claim.CloudID, item.ID)
	}
	if claim.ProviderScope != machineScope(item.ID) {
		return fmt.Errorf("provider scope mismatch: claim=%s machine=%s", blank(claim.ProviderScope, "missing"), machineScope(item.ID))
	}
	return nil
}

func machine0Claims() (map[string]LeaseClaim, error) {
	claims, err := listClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]LeaseClaim{}
	for _, claim := range claims {
		if !isMachine0ClaimProvider(claim.Provider) {
			continue
		}
		id := firstNonBlank(claim.CloudID, claim.Labels["machine0_id"])
		if id != "" {
			out[id] = claim
		} else if (fixedMachine0LeaseKind.IsFixedClaim(claim) || claim.Labels["recovery"] == "create-pending") && claim.ProviderScope != "" {
			out[claim.ProviderScope] = claim
		}
	}
	return out, nil
}

func (b *backend) serverFromMachine(item machine, claim LeaseClaim, cfg Config) Server {
	cfg = effectiveMachine0Config(cfg, item)
	labels := shared.CloneLabels(claim.Labels)
	leaseID, slug := claim.LeaseID, claim.Slug
	if len(labels) == 0 {
		labels = machineLabels(cfg, item, leaseID, slug, false, b.now())
	}
	for key, value := range machineDynamicLabels(item) {
		labels[key] = value
	}
	labels["work_root"] = cfg.WorkRoot
	server := Server{CloudID: item.ID, ImmutableID: item.ID, Provider: providerName, Name: item.Name, Status: normalizeMachineState(item.Status), Labels: labels, ProviderMetadata: machineProviderMetadata(item)}
	server.PublicNet.IPv4.IP = item.IP
	server.ServerType.Name = item.Size
	return server
}

var machineLabelKeys = []string{"machine0_id", "machine0_name", "machine0_status", "machine0_url", "image", "image_version", "size", "region", "ssh_user", "ssh_key", "release_policy", "price_per_hour_micro", "work_root"}

func machineLabels(cfg Config, item machine, leaseID, slug string, keep bool, now time.Time) map[string]string {
	cfg = effectiveMachine0Config(cfg, item)
	labels := directLeaseLabels(cfg, leaseID, slug, keep, now)
	for key, value := range machineDynamicLabels(item) {
		labels[key] = value
	}
	labels["release_policy"] = normalizeReleasePolicy(cfg.Machine0.ReleasePolicy)
	labels["work_root"] = cfg.WorkRoot
	return labels
}

func machineDynamicLabels(item machine) map[string]string {
	labels := map[string]string{"machine0_id": item.ID, "machine0_name": item.Name, "machine0_status": item.Status, "machine0_url": item.URL, "image": item.Image, "image_version": strconv.Itoa(item.ImageVersion), "size": item.Size, "region": item.Region, "ssh_user": machine0SSHUser(item), "price_per_hour_micro": strconv.FormatInt(item.PricePerHour, 10), "state": normalizeMachineState(item.Status)}
	if item.Key != nil {
		labels["ssh_key"] = item.Key.Name
	}
	return labels
}

func machineProviderMetadata(item machine) map[string]any {
	return map[string]any{"url": item.URL, "status": item.Status, "ip": item.IP, "size": item.Size, "vcpu": item.VCPU, "ram": item.RAM, "disk": item.Disk, "region": item.Region, "provider": item.Provider, "image": item.Image, "imageVersion": item.ImageVersion, "defaultSSHUsername": item.DefaultSSHUsername, "distribution": item.Distribution, "ageMinutes": item.AgeMinutes, "totalCostMicro": item.TotalCost, "pricePerHourMicro": item.PricePerHour, "mountPaths": item.MountPaths}
}

func machineScope(id string) string { return providerName + ":" + strings.TrimSpace(id) }

const machine0MachineNameMaxLength = 31

func machine0MachineName(leaseID, slug string) string {
	base := core.NormalizeLeaseSlug(slug)
	if base == "" {
		base = core.NormalizeLeaseSlug(strings.ReplaceAll(leaseID, "_", "-"))
	}
	suffix := machine0LeaseSuffix(leaseID)
	maxBase := machine0MachineNameMaxLength - len("crabbox-") - len(suffix)
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "vm"
	}
	return "crabbox-" + base + suffix
}

func machine0LeaseSuffix(leaseID string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(leaseID))
	return fmt.Sprintf("-%08x", hash.Sum32())
}

func shouldCleanupMachine0(server Server, claim LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !hasClaim {
		return false, "missing claim"
	}
	if machineStopped(server.Labels["machine0_status"]) || machineTerminal(server.Labels["machine0_status"]) {
		return true, "machine state=" + blank(server.Labels["machine0_status"], "unknown")
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil || lastUsed.IsZero() || claim.IdleTimeoutSeconds <= 0 {
		return false, "claim active"
	}
	if now.After(lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second).Add(12 * time.Hour)) {
		return true, "claim expired"
	}
	return false, "claim active"
}

func normalizeReleasePolicy(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "suspend") {
		return "suspend"
	}
	return "destroy"
}
func normalizeMachineState(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "RUNNING":
		return "ready"
	case "CREATING", "STARTING":
		return "initializing"
	case "STOPPED":
		return "stopped"
	case "SUSPENDED":
		return "suspended"
	case "STOPPING", "SUSPENDING":
		return "stopping"
	case "DELETING", "DELETED":
		return "deleted"
	case "ERRORED", "UNAVAILABLE":
		return "failed"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func machineRunning(value string) bool { return strings.EqualFold(strings.TrimSpace(value), "RUNNING") }
func machineStopped(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value == "STOPPED" || value == "SUSPENDED"
}
func machinePending(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value == "CREATING" || value == "STARTING" || value == "STOPPING" || value == "SUSPENDING"
}
func machineAcquirePending(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value == "CREATING" || value == "STARTING"
}
func machineSuspending(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value == "SUSPENDING" || value == "STOPPING"
}
func machineTerminal(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value == "ERRORED" || value == "UNAVAILABLE"
}
func terminalMachineError(item machine) error {
	return exit(5, "machine0 machine %s entered terminal state %s: %s", item.Name, item.Status, blank(item.LastErrorMessage, "run `machine0 get "+item.Name+" --json` for diagnostics"))
}
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func firstNonBlank(values ...string) string {
	return shared.FirstNonBlankTrimmed(values...)
}
func firstLine(value string) string {
	if line, _, ok := strings.Cut(strings.TrimSpace(value), "\n"); ok {
		return line
	}
	return strings.TrimSpace(value)
}
