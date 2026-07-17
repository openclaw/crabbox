package lume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec                  ProviderSpec
	cfg                   Config
	rt                    Runtime
	startupObserveTimeout time.Duration
	stopObserveTimeout    time.Duration
	stopPollInterval      time.Duration
}

type lumeRunOwner struct {
	PID           int
	StartedAt     time.Time
	StartIdentity string
	BootIdentity  string
	LogPath       string
}

type lumeVM struct {
	Name           string `json:"name"`
	OS             string `json:"os"`
	Status         string `json:"status"`
	IPAddress      string `json:"ipAddress"`
	SSHAvailable   *bool  `json:"sshAvailable"`
	LocationName   string `json:"locationName"`
	NetworkMode    string `json:"networkMode"`
	ProvisioningOp string `json:"provisioningOperation"`
}

var validPOSIXUser = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9._-]*$`)
var invalidLogName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{
		spec:                  spec,
		cfg:                   cfg,
		rt:                    rt,
		startupObserveTimeout: defaultStartupObserveTimeout,
		stopObserveTimeout:    defaultStopObserveTimeout,
		stopPollInterval:      defaultStopPollInterval,
	}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	cfg.TargetOS = targetMacOS
	cfg.WindowsMode = ""
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.Lume.CLIPath) == "" {
		cfg.Lume.CLIPath = "lume"
	}
	if strings.TrimSpace(cfg.Lume.Base) == "" {
		cfg.Lume.Base = "crabbox-macos-golden"
	}
	if strings.TrimSpace(cfg.Lume.User) == "" {
		cfg.Lume.User = "lume"
	}
	if strings.TrimSpace(cfg.Lume.WorkRoot) == "" || (cfg.Lume.User != "lume" && cfg.Lume.WorkRoot == "/Users/lume/crabbox") {
		if !core.IsDefaultWorkRoot(cfg.WorkRoot) {
			cfg.Lume.WorkRoot = cfg.WorkRoot
		} else {
			cfg.Lume.WorkRoot = "/Users/" + cfg.Lume.User + "/crabbox"
		}
	}
	cfg.SSHUser = cfg.Lume.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.Lume.WorkRoot
	cfg.ServerType = cfg.Lume.Base
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return core.UseLeaseKnownHosts(&target.SSH, leaseID)
}

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func configForClaim(cfg Config, claim core.LeaseClaim) Config {
	if value, ok := claim.Labels["base"]; ok {
		cfg.Lume.Base = value
	}
	if value, ok := claim.Labels["storage"]; ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "home", "unknown":
			cfg.Lume.Storage = ""
		default:
			cfg.Lume.Storage = value
		}
	}
	if value := strings.TrimSpace(claim.Labels["ssh_user"]); value != "" {
		cfg.Lume.User = value
	}
	if value := strings.TrimSpace(claim.Labels["work_root"]); value != "" {
		cfg.Lume.WorkRoot = value
	}
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	unlockCapacity, err := lockLumeCapacity(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	capacityLocked := true
	releaseCapacity := func() {
		if capacityLocked {
			unlockCapacity()
			capacityLocked = false
		}
	}
	defer releaseCapacity()
	activeGuests, err := b.activeMacOSGuestCount(ctx, cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if activeGuests >= 2 {
		return LeaseTarget{}, exit(5, "Lume macOS guest capacity exhausted: %d of 2 guests are running or starting", activeGuests)
	}
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	if leaseID == "" {
		leaseID = newLeaseID()
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return LeaseTarget{}, err
	}
	servers := make([]Server, 0, len(instances))
	for _, inst := range instances {
		if inst.Name != cfg.Lume.Base && strings.HasPrefix(inst.Name, "crabbox-") {
			servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
		}
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := leaseProviderName(leaseID, slug)
	for _, inst := range instances {
		if inst.Name == name {
			return LeaseTarget{}, exit(4, "refusing to overwrite existing Lume VM %q", name)
		}
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			removeStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s base=%s storage=%s keep=%v\n", providerName, leaseID, slug, cfg.Lume.Base, blank(cfg.Lume.Storage, "home"), req.Keep)

	if err := b.cloneVM(ctx, cfg, name); err != nil {
		return LeaseTarget{}, err
	}
	owner := lumeRunOwner{}
	cleanupUnclaimedVM := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if stopErr := b.stopVM(cleanupCtx, cfg, name, owner); stopErr != nil {
			return errors.Join(cause, fmt.Errorf("Lume rollback could not stop VM %s: %w", name, stopErr))
		}
		if deleteErr := b.deleteVM(cleanupCtx, cfg, name, owner); deleteErr != nil {
			return errors.Join(cause, fmt.Errorf("Lume rollback could not delete VM %s: %w", name, deleteErr))
		}
		return cause
	}
	labels := directLeaseLabels(cfg, leaseID, slug, req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["base"] = cfg.Lume.Base
	labels["storage"] = blank(cfg.Lume.Storage, "home")
	labels["ssh_user"] = cfg.Lume.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.Lume.WorkRoot
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	if req.OnAcquired != nil {
		acquired := LeaseTarget{Server: b.serverFromInstance(lumeVM{Name: name, Status: "stopped"}, claim, cfg), LeaseID: leaseID}
		if err := req.OnAcquired(acquired); err != nil {
			return LeaseTarget{}, cleanupUnclaimedVM(err)
		}
	}
	runOwner, err := b.startVM(ctx, cfg, name)
	owner = runOwner
	if err != nil {
		return LeaseTarget{}, cleanupUnclaimedVM(err)
	}
	labels["run_owner_pid"] = strconv.Itoa(runOwner.PID)
	labels["run_owner_started_at"] = runOwner.StartedAt.UTC().Format(time.RFC3339Nano)
	labels["run_owner_start_identity"] = runOwner.StartIdentity
	labels["run_owner_boot_identity"] = runOwner.BootIdentity
	labels["run_log"] = runOwner.LogPath
	inst, err := b.waitForRunningVM(ctx, cfg, name, runOwner)
	if err != nil {
		return LeaseTarget{}, cleanupUnclaimedVM(err)
	}
	if err := b.waitForGuestIdentity(ctx, cfg, name, inst.IPAddress); err != nil {
		return LeaseTarget{}, cleanupUnclaimedVM(err)
	}
	if err := b.injectSSHKey(ctx, cfg, name, inst.IPAddress, leaseID, publicKey); err != nil {
		return LeaseTarget{}, cleanupUnclaimedVM(err)
	}
	lease, err := b.prepareLease(ctx, cfg, inst, claim, true)
	if err != nil {
		return LeaseTarget{}, cleanupUnclaimedVM(err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH); err != nil {
		return LeaseTarget{}, cleanupUnclaimedVM(err)
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	inst, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if claim.LeaseID == "" {
		return LeaseTarget{}, exit(4, "Lume VM %q has no Crabbox lease claim", inst.Name)
	}
	cfg = configForClaim(cfg, claim)
	server := b.serverFromInstance(inst, claim, cfg)
	lease := LeaseTarget{Server: server, LeaseID: claim.LeaseID}
	if req.ReleaseOnly {
		if err := core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity); err != nil {
			return LeaseTarget{}, err
		}
		return lease, nil
	}
	if !instanceRunning(inst.Status) {
		if req.StatusOnly {
			return lease, nil
		}
		return LeaseTarget{}, exit(5, "Lume VM %s is %s; start a new lease or clean it up", inst.Name, blank(inst.Status, "not running"))
	}
	lease, err = b.prepareLease(ctx, cfg, inst, claim, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" && !req.NoLocalStateMutations {
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(claim.LeaseID, claim.Slug, providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := providerClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(instances))
	for _, inst := range instances {
		if inst.Name == cfg.Lume.Base {
			continue
		}
		claim := claims[inst.Name]
		if claim.LeaseID == "" && !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		views = append(views, b.serverFromInstance(inst, claim, configForClaim(cfg, claim)))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	cfg := b.configForRun()
	version, err := b.lume(ctx, cfg, []string{"--version"}, nil, nil)
	if err != nil {
		return DoctorResult{}, commandError("lume --version", version, err)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	baseState := "missing"
	claims, err := providerClaims()
	if err != nil {
		return DoctorResult{}, err
	}
	leases := 0
	for _, inst := range instances {
		if inst.Name == cfg.Lume.Base {
			baseState = normalizedState(inst.Status)
		}
		if inst.Name != cfg.Lume.Base && claims[inst.Name].LeaseID != "" {
			leases++
		}
	}
	if baseState == "missing" {
		return DoctorResult{}, exit(2, "Lume base VM %q was not found", cfg.Lume.Base)
	}
	if baseState != "stopped" {
		return DoctorResult{}, exit(2, "Lume base VM %q must be stopped, found %s", cfg.Lume.Base, baseState)
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	msg := fmt.Sprintf("cli=ready control_plane=local inventory=ready mutation=false leases=%d runtime=%s base=%s base_state=%s ssh_probe=%s", leases, firstLine(version.Stdout+version.Stderr), cfg.Lume.Base, baseState, probe)
	return DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	lease := req.Lease
	if lease.LeaseID == "" {
		lease.LeaseID = strings.TrimSpace(lease.Server.Labels["lease"])
	}
	if err := core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	name := strings.TrimSpace(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]))
	if name == "" && lease.LeaseID != "" {
		inst, claim, err := b.resolveInstance(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		name = inst.Name
		if claim.LeaseID != "" {
			lease.LeaseID = claim.LeaseID
		}
	}
	if name == "" {
		return exit(2, "provider=%s release requires a Lume VM name", providerName)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID)
	if err != nil {
		return err
	}
	if !ok || instanceNameFromClaim(claim) != name {
		return exit(4, "refusing to delete unclaimed Lume VM %q", name)
	}
	cfg := configForClaim(b.configForRun(), claim)
	instances, err := b.listInstancesForConfig(ctx, cfg)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if inst.Name != name {
			continue
		}
		if instanceRunning(inst.Status) && req.GuardedRemoteCleanup != nil {
			cleanupLease, prepareErr := b.prepareLease(ctx, cfg, inst, claim, false)
			if prepareErr != nil {
				return prepareErr
			}
			req.GuardedRemoteCleanup(ctx, cleanupLease)
			current, currentOK, claimErr := resolveLeaseClaimForProvider(lease.LeaseID)
			if claimErr != nil {
				return claimErr
			}
			if !currentOK || instanceNameFromClaim(current) != name {
				return exit(4, "refusing to stop Lume VM %q after its lease claim changed during remote cleanup", name)
			}
			claim = current
			cfg = configForClaim(b.configForRun(), claim)
		}
		break
	}
	if err := core.RemoveLeaseClaimIfUnchangedAfter(lease.LeaseID, claim, func() error {
		return b.removeClaimedVM(ctx, cfg, name, ownerFromClaim(claim))
	}); err != nil {
		return err
	}
	removeStoredTestboxKey(lease.LeaseID)
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	claims, err := providerClaims()
	if err != nil {
		return err
	}
	removed := 0
	checked := 0
	for _, claim := range claims {
		if claim.LeaseID == "" {
			continue
		}
		checked++
		name := instanceNameFromClaim(claim)
		if name == "" {
			continue
		}
		claimCfg := configForClaim(cfg, claim)
		inst, _, resolveErr := b.resolveClaimedInstance(ctx, claim)
		if resolveErr != nil {
			return resolveErr
		}
		missing := normalizedState(inst.Status) == "missing"
		server := b.serverFromInstance(inst, claim, claimCfg)
		shouldDelete, reason := shouldCleanup(server, claim, time.Now().UTC())
		if missing {
			shouldDelete, reason = true, "instance missing"
		}
		if !shouldDelete {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove instance name=%s lease=%s reason=%s\n", name, claim.LeaseID, reason)
			continue
		}
		action := func() error {
			if missing {
				state, stillMissing, observeErr := b.observeVMState(ctx, claimCfg, name)
				if observeErr != nil {
					return observeErr
				}
				if !stillMissing {
					return exit(4, "refusing to remove Lume claim %s after VM %q reappeared in state %s", claim.LeaseID, name, blank(state, "unknown"))
				}
				if owner := ownerFromClaim(claim); ownerProcessMatches(owner) {
					return exit(5, "refusing to remove missing Lume claim %s while owner pid %d is still running", claim.LeaseID, owner.PID)
				}
				return b.deleteVM(ctx, claimCfg, name, ownerFromClaim(claim))
			}
			live, _, resolveErr := b.resolveClaimedInstance(ctx, claim)
			if resolveErr != nil {
				return resolveErr
			}
			if normalizedState(live.Status) != "missing" {
				refreshed := b.serverFromInstance(live, claim, claimCfg)
				if stillEligible, _ := shouldCleanup(refreshed, claim, time.Now().UTC()); !stillEligible {
					return exit(4, "Lume lease %s is no longer eligible for cleanup", claim.LeaseID)
				}
			}
			return b.removeClaimedVM(ctx, claimCfg, name, ownerFromClaim(claim))
		}
		if err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, action); err != nil {
			return err
		}
		removeStoredTestboxKey(claim.LeaseID)
		removed++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, checked)
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = touchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	for _, key := range []string{"base", "storage", "instance", "ssh_user", "ssh_port", "work_root", "run_owner_pid", "run_owner_started_at", "run_owner_start_identity", "run_owner_boot_identity", "run_log"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

func (b *backend) cloneVM(ctx context.Context, cfg Config, name string) error {
	args := []string{"clone", cfg.Lume.Base, name}
	if storage := strings.TrimSpace(cfg.Lume.Storage); storage != "" {
		args = append(args, "--source-storage", storage, "--dest-storage", storage)
	}
	result, err := b.lume(ctx, cfg, args, nil, b.rt.Stderr)
	if err != nil {
		return commandError("lume clone", result, err)
	}
	return nil
}

func (b *backend) startVM(ctx context.Context, cfg Config, name string) (lumeRunOwner, error) {
	args := []string{"run", name, "--no-display"}
	if storage := strings.TrimSpace(cfg.Lume.Storage); storage != "" {
		args = append(args, "--storage", storage)
	}
	if err := ctx.Err(); err != nil {
		return lumeRunOwner{}, exit(2, "lume run %s: context already cancelled", name)
	}
	logPath, err := lumeRunLogPath(name)
	if err != nil {
		return lumeRunOwner{}, err
	}
	detachedStderr, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return lumeRunOwner{}, exit(2, "lume run %s: create startup log: %v", name, err)
	}
	defer detachedStderr.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return lumeRunOwner{}, errors.Join(exit(2, "lume run %s: open null device: %v", name, err), detachedStderr.Close())
	}
	cmd := exec.Command(cfg.Lume.CLIPath, args...)
	detachCommand(cmd)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = detachedStderr
	var stderrBuf bytes.Buffer
	if err := cmd.Start(); err != nil {
		return lumeRunOwner{}, errors.Join(exit(2, "lume run %s: %v", name, err), devNull.Close())
	}
	if err := devNull.Close(); err != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		return lumeRunOwner{}, exit(2, "lume run %s: close null device: %v", name, err)
	}
	startIdentity, startIdentityErr := core.LocalProcessStartIdentity(cmd.Process.Pid)
	if startIdentityErr != nil || strings.TrimSpace(startIdentity) == "" {
		_ = cmd.Process.Signal(os.Interrupt)
		return lumeRunOwner{}, exit(2, "lume run %s: capture owner process identity: %v", name, startIdentityErr)
	}
	bootIdentity, bootIdentityErr := core.LocalProcessBootIdentity()
	if core.LocalProcessBootIdentityRequired() && (bootIdentityErr != nil || strings.TrimSpace(bootIdentity) == "") {
		_ = cmd.Process.Signal(os.Interrupt)
		return lumeRunOwner{}, exit(2, "lume run %s: capture owner boot identity: %v", name, bootIdentityErr)
	}
	owner := lumeRunOwner{
		PID:           cmd.Process.Pid,
		StartedAt:     time.Now().UTC(),
		StartIdentity: startIdentity,
		BootIdentity:  bootIdentity,
		LogPath:       logPath,
	}
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(os.Interrupt)
		return owner, exit(2, "lume run %s: context cancelled during startup", name)
	case err := <-exitCh:
		_ = detachedStderr.Sync()
		if _, seekErr := detachedStderr.Seek(0, io.SeekStart); seekErr == nil {
			_, _ = io.Copy(&stderrBuf, io.LimitReader(detachedStderr, 64<<10))
		}
		detail := strings.TrimSpace(stderrBuf.String())
		if detail != "" {
			return owner, exit(2, "lume run %s failed during startup: %s", name, detail)
		}
		if err != nil {
			return owner, exit(2, "lume run %s failed during startup: %v", name, err)
		}
		return owner, exit(2, "lume run %s exited unexpectedly during startup", name)
	case <-time.After(b.startupObserveTimeout):
		return owner, nil
	}
}

func lumeRunLogPath(name string) (string, error) {
	dir, err := core.CrabboxStateDir()
	if err != nil {
		return "", exit(2, "resolve Crabbox state directory for Lume: %v", err)
	}
	dir = filepath.Join(dir, "lume", "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", exit(2, "create Lume run log directory: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", exit(2, "secure Lume run log directory: %v", err)
	}
	safeName := strings.Trim(invalidLogName.ReplaceAllString(name, "_"), "._")
	if safeName == "" {
		safeName = "vm"
	}
	return filepath.Join(dir, safeName+".log"), nil
}

func (b *backend) waitForRunningVM(ctx context.Context, cfg Config, name string, owner lumeRunOwner) (lumeVM, error) {
	deadline := time.NewTimer(bootstrapWaitTimeout(cfg))
	ticker := time.NewTicker(2 * time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if owner.PID > 0 && !ownerProcessMatches(owner) {
			detail := ""
			if file, err := os.Open(owner.LogPath); err == nil {
				data, _ := io.ReadAll(io.LimitReader(file, 64<<10))
				_ = file.Close()
				detail = strings.TrimSpace(string(data))
			}
			if detail != "" {
				return lumeVM{}, exit(2, "Lume VM %s owner exited during startup: %s", name, detail)
			}
			return lumeVM{}, exit(2, "Lume VM %s owner exited during startup", name)
		}
		inst, err := b.getInstance(ctx, cfg, name)
		// Lume 0.3.16 computes sshAvailable with `nc -z`, which reports a
		// false negative on some macOS hosts even when TCP/22 is accepting
		// connections. The first-boot identity probe below is the authoritative
		// guest SSH readiness check.
		if err == nil && instanceRunning(inst.Status) && inst.IPAddress != "" {
			return inst, nil
		}
		select {
		case <-ctx.Done():
			return lumeVM{}, exit(2, "wait for Lume VM %s: context cancelled", name)
		case <-deadline.C:
			return lumeVM{}, exit(5, "timed out waiting for Lume VM %s running state and IP address", name)
		case <-ticker.C:
		}
	}
}

func (b *backend) injectSSHKey(ctx context.Context, cfg Config, name, host, leaseID, publicKey string) error {
	key := shellSingleQuote(strings.TrimSpace(publicKey))
	script := "umask 077; mkdir -p \"$HOME/.ssh\"; touch \"$HOME/.ssh/authorized_keys\"; grep -qxF " + key + " \"$HOME/.ssh/authorized_keys\" || printf '%s\\n' " + key + " >> \"$HOME/.ssh/authorized_keys\"; chmod 700 \"$HOME/.ssh\"; chmod 600 \"$HOME/.ssh/authorized_keys\""
	target := SSHTarget{}
	if err := core.UseLeaseKnownHosts(&target, leaseID); err != nil {
		return err
	}
	result, err := b.bootstrapSSH(ctx, cfg, host, script, target.KnownHostsFile)
	if err != nil {
		return commandError("Lume guest SSH key injection for "+name, result, err)
	}
	return nil
}

func (b *backend) waitForGuestIdentity(ctx context.Context, cfg Config, name, host string) error {
	script := `current="$(ioreg -rd1 -c IOPlatformExpertDevice | awk -F'"' '/IOPlatformUUID/ { print $(NF-1); exit }')"; marker="$(cat /var/db/crabbox-lume-machine-id 2>/dev/null || true)"; test -n "$current" && test "$marker" = "$current"`
	deadline := time.NewTimer(defaultGuestIdentityTimeout)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	var last LocalCommandResult
	var lastErr error
	for {
		last, lastErr = b.bootstrapSSH(ctx, cfg, host, script, "")
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return exit(2, "wait for Lume VM %s first-boot identity: context cancelled", name)
		case <-deadline.C:
			return commandError("wait for Lume first-boot identity", last, lastErr)
		case <-ticker.C:
		}
	}
}

func (b *backend) bootstrapSSH(ctx context.Context, cfg Config, host, command, knownHostsFile string) (LocalCommandResult, error) {
	askpass, err := os.CreateTemp("", "crabbox-lume-askpass-*.sh")
	if err != nil {
		return LocalCommandResult{}, exit(2, "create Lume bootstrap askpass helper: %v", err)
	}
	askpassPath := askpass.Name()
	defer os.Remove(askpassPath)
	if _, err := askpass.WriteString("#!/bin/sh\nprintf '%s\\n' '" + bootstrapPassword + "'\n"); err != nil {
		_ = askpass.Close()
		return LocalCommandResult{}, exit(2, "write Lume bootstrap askpass helper: %v", err)
	}
	if err := askpass.Chmod(0o700); err != nil {
		_ = askpass.Close()
		return LocalCommandResult{}, exit(2, "secure Lume bootstrap askpass helper: %v", err)
	}
	if err := askpass.Close(); err != nil {
		return LocalCommandResult{}, exit(2, "close Lume bootstrap askpass helper: %v", err)
	}
	args := []string{}
	if knownHostsFile == "" {
		args = append(args,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
		)
	} else {
		args = append(args,
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "UserKnownHostsFile="+knownHostsFile,
		)
	}
	args = append(args,
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", "PubkeyAuthentication=no",
		"-o", "PreferredAuthentications=password,keyboard-interactive",
		"-p", sshPort,
		cfg.Lume.User+"@"+host,
		command,
	)
	env := append(os.Environ(),
		"SSH_ASKPASS="+askpassPath,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=:0",
	)
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   "/usr/bin/ssh",
		Args:                   args,
		Env:                    env,
		Stdin:                  strings.NewReader(""),
		MaxCapturedOutputBytes: 64 << 10,
	})
}

func (b *backend) stopVM(ctx context.Context, cfg Config, name string, owner lumeRunOwner) error {
	args := []string{"stop", name}
	if storage := strings.TrimSpace(cfg.Lume.Storage); storage != "" {
		args = append(args, "--storage", storage)
	}
	var stopErr error
	var signalErr error
	var state string
	for attempt := 0; attempt < 2; attempt++ {
		result, err := b.lume(ctx, cfg, args, nil, b.rt.Stderr)
		if err != nil {
			stopErr = commandError("lume stop", result, err)
		} else {
			stopErr = nil
		}
		// Lume 0.3.16 can return success (or exit 130) without terminating the
		// long-running `lume run` owner. Signal only the exact process identity
		// captured at acquisition; a recycled PID is never eligible.
		if ownerSafeToSignal(owner) {
			if err := signalProcessInterrupt(owner.PID); err != nil {
				signalErr = fmt.Errorf("interrupt Lume owner pid %d: %w", owner.PID, err)
			}
		}
		stopped, observedState, observeErr := b.waitForStoppedOrMissingVM(ctx, cfg, name, owner)
		state = observedState
		if stopped {
			return nil
		}
		if observeErr != nil {
			return errors.Join(stopErr, observeErr)
		}
	}
	if stopErr != nil {
		return errors.Join(stopErr, signalErr, exit(5, "Lume VM %s remained %s after two stop attempts", name, blank(state, "unknown")))
	}
	return errors.Join(signalErr, exit(5, "Lume VM %s remained %s after two stop attempts", name, blank(state, "unknown")))
}

func (b *backend) removeClaimedVM(ctx context.Context, cfg Config, name string, owner lumeRunOwner) error {
	state, missing, err := b.observeVMState(ctx, cfg, name)
	if err != nil {
		return err
	}
	if (!missing && state != "stopped") || ownerProcessMatches(owner) {
		if err := b.stopVM(ctx, cfg, name, owner); err != nil {
			return err
		}
	}
	return b.deleteVM(ctx, cfg, name, owner)
}

func (b *backend) deleteVM(ctx context.Context, cfg Config, name string, owner lumeRunOwner) error {
	stopped, state, err := b.observeStoppedOrMissingVM(ctx, cfg, name, owner)
	if err != nil {
		return err
	}
	if !stopped {
		return exit(5, "refusing to delete Lume VM %s while state=%s", name, blank(state, "unknown"))
	}
	if ownerProcessMatches(owner) {
		return exit(5, "refusing to delete Lume VM %s while owner pid %d is still running", name, owner.PID)
	}
	if state == "missing" {
		if logPath, pathErr := lumeRunLogPath(name); pathErr == nil {
			_ = os.Remove(logPath)
		}
		return nil
	}
	args := []string{"delete", name, "--force"}
	if storage := strings.TrimSpace(cfg.Lume.Storage); storage != "" {
		args = append(args, "--storage", storage)
	}
	result, err := b.lume(ctx, cfg, args, nil, b.rt.Stderr)
	if err != nil {
		return commandError("lume delete", result, err)
	}
	if err := b.waitForMissingVM(ctx, cfg, name); err != nil {
		return err
	}
	if logPath, pathErr := lumeRunLogPath(name); pathErr == nil {
		_ = os.Remove(logPath)
	}
	return nil
}

func (b *backend) listInstances(ctx context.Context) ([]lumeVM, error) {
	cfg := b.configForRun()
	return b.listInstancesForConfig(ctx, cfg)
}

func (b *backend) listInstancesForConfig(ctx context.Context, cfg Config) ([]lumeVM, error) {
	args := []string{"ls", "--format", "json"}
	if storage := strings.TrimSpace(cfg.Lume.Storage); storage != "" {
		args = append(args, "--storage", storage)
	}
	result, err := b.lume(ctx, cfg, args, nil, nil)
	if err != nil {
		return nil, commandError("lume ls", result, err)
	}
	instances, err := parseLumeVMs(result.Stdout)
	if err != nil {
		return nil, exit(2, "parse lume ls: %v", err)
	}
	return instances, nil
}

func (b *backend) activeMacOSGuestCount(ctx context.Context, cfg Config) (int, error) {
	// An unfiltered Lume inventory spans configured storage locations; capacity
	// is host-wide rather than scoped to the destination storage for this lease.
	cfg.Lume.Storage = ""
	instances, err := b.listInstancesForConfig(ctx, cfg)
	if err != nil {
		return 0, err
	}
	active := 0
	for _, inst := range instances {
		if !strings.EqualFold(strings.TrimSpace(inst.OS), targetMacOS) {
			continue
		}
		state := normalizedState(inst.Status)
		if state != "stopped" && state != "missing" {
			active++
		}
	}
	return active, nil
}

func (b *backend) waitForStoppedOrMissingVM(ctx context.Context, cfg Config, name string, owner lumeRunOwner) (bool, string, error) {
	timeout := b.stopObserveTimeout
	if timeout <= 0 {
		timeout = defaultStopObserveTimeout
	}
	interval := b.stopPollInterval
	if interval <= 0 {
		interval = defaultStopPollInterval
	}
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(interval)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		stopped, state, err := b.observeStoppedOrMissingVM(ctx, cfg, name, owner)
		if err != nil || stopped {
			return stopped, state, err
		}
		select {
		case <-ctx.Done():
			return false, state, exit(2, "wait for Lume VM %s to stop: context cancelled", name)
		case <-deadline.C:
			return false, state, nil
		case <-ticker.C:
		}
	}
}

func (b *backend) observeStoppedOrMissingVM(ctx context.Context, cfg Config, name string, owner lumeRunOwner) (bool, string, error) {
	state, missing, err := b.observeVMState(ctx, cfg, name)
	if err != nil {
		return false, "", err
	}
	if ownerProcessMatches(owner) {
		return false, state + " (owner running)", nil
	}
	return missing || state == "stopped", state, nil
}

func (b *backend) observeVMState(ctx context.Context, cfg Config, name string) (string, bool, error) {
	inst, getErr := b.getInstance(ctx, cfg, name)
	if getErr == nil {
		return normalizedState(inst.Status), false, nil
	}
	instances, listErr := b.listInstancesForConfig(ctx, cfg)
	if listErr != nil {
		return "", false, errors.Join(getErr, listErr)
	}
	for _, candidate := range instances {
		if candidate.Name == name {
			return normalizedState(candidate.Status), false, nil
		}
	}
	return "missing", true, nil
}

func (b *backend) waitForMissingVM(ctx context.Context, cfg Config, name string) error {
	timeout := b.stopObserveTimeout
	if timeout <= 0 {
		timeout = defaultStopObserveTimeout
	}
	interval := b.stopPollInterval
	if interval <= 0 {
		interval = defaultStopPollInterval
	}
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(interval)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		state, missing, err := b.observeVMState(ctx, cfg, name)
		if err != nil {
			return err
		}
		if missing {
			return nil
		}
		select {
		case <-ctx.Done():
			return exit(2, "wait for deleted Lume VM %s: context cancelled", name)
		case <-deadline.C:
			return exit(5, "Lume VM %s remained %s after delete", name, blank(state, "unknown"))
		case <-ticker.C:
		}
	}
}

func (b *backend) getInstance(ctx context.Context, cfg Config, name string) (lumeVM, error) {
	args := []string{"get", name, "--format", "json"}
	if storage := strings.TrimSpace(cfg.Lume.Storage); storage != "" {
		args = append(args, "--storage", storage)
	}
	result, err := b.lume(ctx, cfg, args, nil, nil)
	if err != nil {
		return lumeVM{}, commandError("lume get", result, err)
	}
	instances, err := parseLumeVMs(result.Stdout)
	if err != nil {
		return lumeVM{}, exit(2, "parse lume get: %v", err)
	}
	if len(instances) != 1 || instances[0].Name != name {
		return lumeVM{}, exit(4, "Lume VM not found: %s", name)
	}
	return instances[0], nil
}

func parseLumeVMs(output string) ([]lumeVM, error) {
	// Lume can print timestamped informational lines to stdout before JSON,
	// especially while cleaning a stale session file. Those lines also begin
	// with `[`, so try each possible array boundary and accept only an array
	// that decodes as VM objects.
	for offset := 0; offset < len(output); {
		next := strings.IndexByte(output[offset:], '[')
		if next < 0 {
			break
		}
		offset += next
		var instances []lumeVM
		decoder := json.NewDecoder(strings.NewReader(output[offset:]))
		if err := decoder.Decode(&instances); err == nil {
			return instances, nil
		}
		offset++
	}
	return nil, fmt.Errorf("no VM JSON array found")
}

func (b *backend) resolveInstance(ctx context.Context, identifier string) (lumeVM, core.LeaseClaim, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return lumeVM{}, core.LeaseClaim{}, exit(2, "provider=%s requires --id <lease-id-or-slug-or-instance>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier); err != nil {
		return lumeVM{}, core.LeaseClaim{}, err
	} else if ok {
		return b.resolveClaimedInstance(ctx, claim)
	}
	claims, err := providerClaims()
	if err != nil {
		return lumeVM{}, core.LeaseClaim{}, err
	}
	normalized := normalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if instanceNameFromClaim(claim) == identifier || claim.LeaseID == identifier || (normalized != "" && normalizeLeaseSlug(claim.Slug) == normalized) {
			return b.resolveClaimedInstance(ctx, claim)
		}
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return lumeVM{}, core.LeaseClaim{}, err
	}
	for _, inst := range instances {
		claim := claims[inst.Name]
		if inst.Name == identifier || claim.LeaseID == identifier || (normalized != "" && normalizeLeaseSlug(claim.Slug) == normalized) {
			return inst, claim, nil
		}
	}
	return lumeVM{}, core.LeaseClaim{}, exit(4, "Lume lease not found: %s", identifier)
}

func (b *backend) resolveClaimedInstance(ctx context.Context, claim core.LeaseClaim) (lumeVM, core.LeaseClaim, error) {
	name := instanceNameFromClaim(claim)
	if name == "" {
		return lumeVM{}, core.LeaseClaim{}, exit(4, "Lume lease %s has no instance name in its claim", claim.LeaseID)
	}
	cfg := configForClaim(b.configForRun(), claim)
	inst, getErr := b.getInstance(ctx, cfg, name)
	if getErr == nil {
		return inst, claim, nil
	}
	instances, listErr := b.listInstancesForConfig(ctx, cfg)
	if listErr != nil {
		return lumeVM{}, core.LeaseClaim{}, errors.Join(getErr, listErr)
	}
	for _, candidate := range instances {
		if candidate.Name == name {
			return candidate, claim, nil
		}
	}
	return lumeVM{Name: name, Status: "missing"}, claim, nil
}

func (b *backend) prepareLease(ctx context.Context, cfg Config, inst lumeVM, claim core.LeaseClaim, wait bool) (LeaseTarget, error) {
	server := b.serverFromInstance(inst, claim, cfg)
	if inst.IPAddress == "" {
		return LeaseTarget{}, exit(5, "Lume VM %s has no IP address", inst.Name)
	}
	server.PublicNet.IPv4.IP = inst.IPAddress
	if claim.LeaseID != "" {
		if keyPath, err := testboxKeyPath(claim.LeaseID); err == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				cfg.SSHKey = keyPath
			}
		}
	}
	target := sshTargetFromConfig(cfg, inst.IPAddress)
	target.Port = sshPort
	target.FallbackPorts = nil
	target.TargetOS = targetMacOS
	target.ReadyCheck = "uname -s | grep -qx Darwin && test -d \"$HOME\""
	// Local macOS sandboxing can deny Go's direct TCP preflight even when
	// OpenSSH can reach the VM. Tart uses the same switch for local guests:
	// probe readiness through OpenSSH without adding an actual proxy command.
	target.SSHConfigProxy = true
	if claim.LeaseID != "" {
		if err := core.UseLeaseKnownHosts(&target, claim.LeaseID); err != nil {
			return LeaseTarget{}, err
		}
	}
	if wait {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "lume ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *backend) serverFromInstance(inst lumeVM, claim core.LeaseClaim, cfg Config) Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	labels["crabbox"] = "true"
	labels["provider"] = providerName
	labels["instance"] = inst.Name
	if labels["lease"] == "" {
		labels["lease"] = claim.LeaseID
	}
	if labels["slug"] == "" {
		labels["slug"] = claim.Slug
	}
	state := normalizedState(inst.Status)
	if labels["state"] == "" || labels["state"] == "running" {
		labels["state"] = state
	}
	if labels["server_type"] == "" {
		labels["server_type"] = cfg.Lume.Base
	}
	if labels["base"] == "" {
		labels["base"] = cfg.Lume.Base
	}
	if labels["storage"] == "" {
		labels["storage"] = blank(cfg.Lume.Storage, "home")
	}
	if labels["ssh_user"] == "" {
		labels["ssh_user"] = cfg.Lume.User
	}
	if labels["ssh_port"] == "" {
		labels["ssh_port"] = sshPort
	}
	if labels["work_root"] == "" {
		labels["work_root"] = cfg.Lume.WorkRoot
	}
	status := state
	if instanceRunning(inst.Status) && labels["state"] == "ready" {
		status = "ready"
	}
	server := Server{CloudID: inst.Name, Provider: providerName, Name: inst.Name, Status: status, Labels: labels}
	server.ServerType.Name = cfg.Lume.Base
	return server
}

func providerClaims() (map[string]core.LeaseClaim, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if name := instanceNameFromClaim(claim); name != "" {
			out[name] = claim
		}
	}
	return out, nil
}

func instanceScope(name string) string { return "instance:" + strings.TrimSpace(name) }

func instanceNameFromClaim(claim core.LeaseClaim) string {
	if name := strings.TrimSpace(claim.Labels["instance"]); name != "" {
		return name
	}
	return strings.TrimPrefix(strings.TrimSpace(claim.ProviderScope), "instance:")
}

func ownerFromClaim(claim core.LeaseClaim) lumeRunOwner {
	pid, err := strconv.Atoi(strings.TrimSpace(claim.Labels["run_owner_pid"]))
	if err != nil || pid <= 0 {
		return lumeRunOwner{}
	}
	return lumeRunOwner{
		PID:           pid,
		StartIdentity: strings.TrimSpace(claim.Labels["run_owner_start_identity"]),
		BootIdentity:  strings.TrimSpace(claim.Labels["run_owner_boot_identity"]),
		LogPath:       strings.TrimSpace(claim.Labels["run_log"]),
	}
}

func ownerProcessMatches(owner lumeRunOwner) bool {
	if owner.PID <= 0 {
		return false
	}
	if core.LocalProcessBootIdentityRequired() {
		if owner.BootIdentity == "" {
			return processAlive(owner.PID)
		}
		currentBoot, err := core.LocalProcessBootIdentity()
		if err != nil {
			return processAlive(owner.PID)
		}
		if currentBoot != owner.BootIdentity {
			return false
		}
	}
	if owner.StartIdentity == "" {
		return processAlive(owner.PID)
	}
	currentStart, err := core.LocalProcessStartIdentity(owner.PID)
	if err != nil {
		return processAlive(owner.PID)
	}
	return currentStart == owner.StartIdentity
}

func ownerSafeToSignal(owner lumeRunOwner) bool {
	if strings.TrimSpace(owner.StartIdentity) == "" {
		return false
	}
	if core.LocalProcessBootIdentityRequired() {
		if strings.TrimSpace(owner.BootIdentity) == "" {
			return false
		}
		currentBoot, err := core.LocalProcessBootIdentity()
		if err != nil || currentBoot != owner.BootIdentity {
			return false
		}
	}
	currentStart, err := core.LocalProcessStartIdentity(owner.PID)
	return err == nil && currentStart == owner.StartIdentity
}

func shouldCleanup(server Server, claim core.LeaseClaim, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !instanceRunning(server.Status) {
		return true, "instance stopped"
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil || lastUsed.IsZero() {
		return false, "claim active"
	}
	idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
	if idle <= 0 || !now.After(lastUsed.Add(idle).Add(12*time.Hour)) {
		return false, "claim active"
	}
	return true, "claim expired"
}

func (b *backend) lume(ctx context.Context, cfg Config, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{Name: cfg.Lume.CLIPath, Args: args, Stdout: stdout, Stderr: stderr})
}

func instanceRunning(state string) bool {
	switch normalizedState(state) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func normalizedState(state string) string { return strings.ToLower(strings.TrimSpace(state)) }

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func commandError(action string, result LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return exit(code, "%s failed: %v", action, err)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	return blank(strings.TrimSpace(value), "unknown")
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
