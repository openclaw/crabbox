package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	spec                  ProviderSpec
	cfg                   Config
	rt                    Runtime
	startupObserveTimeout time.Duration
}

type tartInstance struct {
	Name    string      `json:"Name"`
	State   string      `json:"State"`
	Running bool        `json:"Running"`
	Disk    int         `json:"Disk"`
	Size    json.Number `json:"Size"`
	Source  string      `json:"Source"`
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{
		spec:                  spec,
		cfg:                   cfg,
		rt:                    rt,
		startupObserveTimeout: defaultStartupObserveTimeout,
	}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetMacOS
	}
	cfg.WindowsMode = ""
	cfg.SSHFallbackPorts = []string{}
	if cfg.Tart.Image == "" {
		cfg.Tart.Image = core.DefaultTartImage
	}
	if cfg.Tart.User == "" {
		if cfg.SSHUser != "" && cfg.SSHUser != "crabbox" {
			cfg.Tart.User = cfg.SSHUser
		} else {
			cfg.Tart.User = "admin"
		}
	}
	if cfg.Tart.Password == "" {
		cfg.Tart.Password = "admin" // cirruslabs base-image default; WebVNC viewer credential only
	}
	if cfg.Tart.WorkRoot == "" {
		if !core.IsDefaultWorkRoot(cfg.WorkRoot) {
			cfg.Tart.WorkRoot = cfg.WorkRoot
		} else {
			cfg.Tart.WorkRoot = "/Users/admin/crabbox"
		}
	}
	if cfg.Tart.CPUs <= 0 {
		cfg.Tart.CPUs = 4
	}
	if cfg.Tart.Memory <= 0 {
		cfg.Tart.Memory = 8192
	}
	cfg.SSHUser = cfg.Tart.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.Tart.WorkRoot
	cfg.ServerType = cfg.Tart.Image
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req AcquireRequest) (target LeaseTarget, acquireErr error) {
	cfg := b.configForRun()
	leaseID := newLeaseID()
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
		if !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
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
	name := leaseProviderName(leaseID, slug)
	diskLabel := "clone-default"
	if core.IsTartDiskExplicit(&cfg) {
		diskLabel = fmt.Sprintf("%dGB", cfg.Tart.Disk)
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s cpus=%d memory=%dMB disk=%s keep=%v\n", providerName, leaseID, slug, cfg.Tart.Image, cfg.Tart.CPUs, cfg.Tart.Memory, diskLabel, req.Keep)

	if err := b.cloneVM(ctx, cfg, name); err != nil {
		return LeaseTarget{}, err
	}
	storage, identity, err := createTartVMIdentity(name)
	if err != nil {
		return LeaseTarget{}, fmt.Errorf("bind new Tart instance %s ownership (VM retained): %w", name, err)
	}
	cleanupUnclaimedVM := func() error {
		if err := verifyTartVMIdentity(name, storage, identity); err != nil {
			return err
		}
		_ = b.stopVM(context.Background(), name)
		if err := verifyTartVMIdentity(name, storage, identity); err != nil {
			return err
		}
		return b.deleteVM(context.Background(), name)
	}
	if cfg.Tart.Image == core.DefaultTartImage {
		fmt.Fprintln(b.rt.Stderr, "verifying built-in Tart image contents before boot (full disk read)")
	}
	imageDigest, err := verifyDefaultTartImage(ctx, cfg.Tart.Image, storage, name)
	if err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupUnclaimedVM())
	}
	if err := verifyTartVMIdentity(name, storage, identity); err != nil {
		return LeaseTarget{}, fmt.Errorf("Tart clone ownership changed before configuration (VM retained): %w", err)
	}
	if err := b.configureVM(ctx, cfg, name); err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupUnclaimedVM())
	}
	startup, err := b.startVM(ctx, name, req.Keep)
	if err != nil {
		return LeaseTarget{}, errors.Join(err, cleanupUnclaimedVM())
	}
	var publishedClaim core.LeaseClaim
	defer func() {
		if acquireErr == nil {
			return
		}
		// Reap our exact child and preserve its failure before name-based cleanup.
		acquireErr = startup.abort(acquireErr)
		cleanup := cleanupUnclaimedVM
		if publishedClaim.LeaseID != "" {
			cleanup = func() error {
				// A failed durable write can return a candidate before or after
				// rename. Fence cleanup against either absence or that revision.
				_, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
				if err != nil {
					return err
				}
				return core.CleanupLeaseClaimIfUnchangedAfter(leaseID, publishedClaim, exists, cleanupUnclaimedVM)
			}
		}
		acquireErr = errors.Join(acquireErr, cleanup())
	}()
	ctx = startup.ctx
	ip, err := b.waitForIP(ctx, name)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := b.injectSSHKey(ctx, name, cfg.Tart.User, publicKey); err != nil {
		return LeaseTarget{}, err
	}
	if cfg.Desktop {
		if err := b.enableScreenSharing(ctx, name); err != nil {
			return LeaseTarget{}, err
		}
	}

	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = cfg.Tart.Image
	if imageDigest != "" {
		labels["image_digest"] = imageDigest
	}
	labels["ssh_user"] = cfg.Tart.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.Tart.WorkRoot
	labels["tart_storage"] = storage
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), CloudImmutableID: identity, Labels: labels}

	inst := tartInstance{Name: name, State: "running", Running: true, Source: cfg.Tart.Image}
	lease, err := b.prepareLease(ctx, cfg, inst, ip, claim, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	// Cancel lock acquisition on startup exit, and retain the exact published
	// revision for rollback if startup fails at the final handoff.
	publishedClaim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, leaseID, slug, cfg, instanceScope(name), lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, core.LeaseClaim{}, false, nil)
	if err != nil {
		return LeaseTarget{}, err
	}
	if err := startup.handoff(); err != nil {
		return LeaseTarget{}, err
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	inst, ip, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if claim.LeaseID == "" {
		return LeaseTarget{}, exit(4, "tart instance %q has no Crabbox lease claim; remove it with `tart stop %s && tart delete %s` or warm a new lease with `crabbox run`", inst.Name, inst.Name, inst.Name)
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: claim.LeaseID}, nil
	}
	if !inst.Running && !instanceRunning(inst.State) && !req.StatusOnly {
		return LeaseTarget{}, exit(5, "tart instance %s is stopped; start a new lease with `crabbox run` or clean up with `crabbox cleanup --provider tart`", inst.Name)
	}
	lease, err := b.prepareLease(ctx, cfg, inst, ip, claim, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
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
		claim := claims[inst.Name]
		if claim.LeaseID == "" && !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		views = append(views, b.serverFromInstance(inst, claim, cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	cfg := b.configForRun()
	version, err := b.tart(ctx, []string{"--version"}, nil, nil)
	if err != nil {
		return DoctorResult{}, commandError("tart --version", version, err)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	leases := 0
	for _, inst := range instances {
		if strings.HasPrefix(inst.Name, "crabbox-") {
			leases++
		}
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	msg := fmt.Sprintf("cli=ready control_plane=local inventory=ready api=list mutation=false leases=%d runtime=%s image=%s ssh_probe=%s", leases, firstLine(version.Stdout+version.Stderr), cfg.Tart.Image, probe)
	return DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	lease := req.Lease
	if lease.LeaseID == "" {
		lease.LeaseID = strings.TrimSpace(lease.Server.Labels["lease"])
	}
	if lease.LeaseID != "" && tartState(lease.Server.Status) == "missing" {
		pruneLeaseState(lease.LeaseID)
		return nil
	}
	name := strings.TrimSpace(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]))
	if name == "" && lease.LeaseID != "" {
		inst, _, claim, err := b.resolveInstance(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		name = inst.Name
		if claim.LeaseID != "" {
			lease.LeaseID = claim.LeaseID
		}
		if tartState(inst.State) == "missing" {
			pruneLeaseState(lease.LeaseID)
			return nil
		}
	}
	if name == "" {
		return exit(2, "provider=%s release requires a tart instance name", providerName)
	}
	_ = b.stopVM(ctx, name)
	if err := b.deleteVM(ctx, name); err != nil {
		return err
	}
	if lease.LeaseID != "" {
		pruneLeaseState(lease.LeaseID)
	}
	return nil
}

func pruneLeaseState(leaseID string) {
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
}

func (b *backend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	// Snapshot candidates before instances so newer claims cannot be compared
	// against an older instance view and misclassified as orphans.
	orphanCandidates, err := listLeaseClaims()
	if err != nil {
		return err
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return err
	}
	storage, err := tartStorageRoot()
	if err != nil {
		return fmt.Errorf("inspect Tart cleanup storage: %w", err)
	}
	byName := map[string][]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName {
			name := instanceNameFromClaim(claim)
			byName[name] = append(byName[name], claim)
		}
	}
	live := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, inst := range instances {
		live[inst.Name] = struct{}{}
		if !strings.HasPrefix(inst.Name, "crabbox-") {
			continue
		}
		matches := byName[inst.Name]
		if len(matches) != 1 {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=expected one exact claim, found %d\n", inst.Name, len(matches))
			continue
		}
		claim := matches[0]
		_, err := tartCleanupBinding(claim, inst.Name, storage)
		if err == nil {
			err = verifyTartVMIdentity(inst.Name, storage, claim.CloudImmutableID)
		}
		if err != nil {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%v\n", inst.Name, err)
			continue
		}
		server := b.serverFromInstance(inst, claim, cfg)
		shouldDelete, reason := shouldCleanup(server, claim, claim.LeaseID != "", now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%s\n", inst.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove instance name=%s lease=%s reason=%s\n", inst.Name, blank(claim.LeaseID, "-"), reason)
			continue
		}
		if err := b.cleanupInstance(ctx, cfg, inst, claim, storage); err != nil {
			return err
		}
		// Key creation precedes claim publication, so the claim fence alone
		// cannot safely authorize deleting a possibly reused local key.
		fmt.Fprintf(b.rt.Stdout, "remove instance name=%s lease=%s reason=%s key_retained=true\n", inst.Name, claim.LeaseID, reason)
		removed++
	}
	claimsRemoved := 0
	for _, claim := range orphanCandidates {
		if claim.Provider != providerName || claim.LeaseID == "" {
			continue
		}
		name := instanceNameFromClaim(claim)
		if _, ok := live[name]; ok {
			continue
		}
		if _, err := tartCleanupBinding(claim, name, storage); err != nil {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=%v\n", claim.LeaseID, err)
			continue
		}
		reason := "missing instance"
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=%s\n", claim.LeaseID, blank(claim.Slug, "-"), reason)
			continue
		}
		// Acquisition creates or reuses the key before publishing its claim, so
		// missing-instance cleanup cannot safely delete that key without a wider fence.
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s slug=%s reason=changed-during-cleanup err=%v\n", claim.LeaseID, blank(claim.Slug, "-"), err)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=%s\n", claim.LeaseID, blank(claim.Slug, "-"), reason)
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(instances))
	}
	return nil
}

func (b *backend) cleanupInstance(ctx context.Context, cfg Config, inst tartInstance, claim core.LeaseClaim, storage string) error {
	binding, err := tartCleanupBinding(claim, inst.Name, storage)
	if err != nil {
		return err
	}
	return shared.RemoveExactClaimAfter(claim, binding, func() error {
		// Re-read lifecycle state and the incarnation witness under the same
		// claim fence that covers deletion and durable claim removal.
		current, err := b.listInstances(ctx)
		if err != nil {
			return err
		}
		for _, fresh := range current {
			if fresh.Name != inst.Name {
				continue
			}
			server := b.serverFromInstance(fresh, claim, cfg)
			if fresh.Running {
				server.Status = "running"
			}
			if eligible, why := shouldCleanup(server, claim, true, time.Now().UTC()); !eligible {
				return fmt.Errorf("Tart instance %s changed during cleanup: %s", inst.Name, why)
			}
			if err := verifyTartVMIdentity(inst.Name, storage, claim.CloudImmutableID); err != nil {
				return err
			}
			if fresh.Running || instanceRunning(fresh.State) {
				if err := b.stopVM(ctx, inst.Name); err != nil {
					return err
				}
			}
			if err := verifyTartVMIdentity(inst.Name, storage, claim.CloudImmutableID); err != nil {
				return err
			}
			return b.deleteVM(ctx, inst.Name)
		}
		return fmt.Errorf("Tart instance %s disappeared during cleanup; claim retained", inst.Name)
	})
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = touchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	for _, key := range []string{"image", "image_digest", "instance", "ssh_user", "ssh_port", "work_root"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	// Storage identity is an exact path, not a sanitized provider-label value.
	if storage, ok := original["tart_storage"]; ok {
		server.Labels["tart_storage"] = storage
	}
	return server, nil
}

// cloneVM clones the base image to create a new VM.
func (b *backend) cloneVM(ctx context.Context, cfg Config, name string) error {
	args := []string{"clone", cfg.Tart.Image, name}
	result, err := b.tart(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return commandError("tart clone", result, err)
	}
	return nil
}

// configureVM applies CPU, memory, and disk settings to the cloned VM before boot.
func (b *backend) configureVM(ctx context.Context, cfg Config, name string) error {
	if cfg.Tart.CPUs > 0 {
		if _, err := b.tart(ctx, []string{"set", name, "--cpu", strconv.Itoa(cfg.Tart.CPUs)}, nil, b.rt.Stderr); err != nil {
			return fmt.Errorf("tart set --cpu: %w", err)
		}
	}
	if cfg.Tart.Memory > 0 {
		if _, err := b.tart(ctx, []string{"set", name, "--memory", strconv.Itoa(cfg.Tart.Memory)}, nil, b.rt.Stderr); err != nil {
			return fmt.Errorf("tart set --memory: %w", err)
		}
	}
	if cfg.Tart.Disk > 0 && core.IsTartDiskExplicit(&cfg) {
		if _, err := b.tart(ctx, []string{"set", name, "--disk-size", strconv.Itoa(cfg.Tart.Disk)}, nil, b.rt.Stderr); err != nil {
			return fmt.Errorf("tart set --disk-size: %w", err)
		}
	}
	return nil
}

// waitForIP polls `tart ip` until the VM has an IP address.
func (b *backend) waitForIP(ctx context.Context, name string) (string, error) {
	deadline := time.After(5 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	type observation struct {
		result LocalCommandResult
		err    error
	}
	initial := true
	result, err := shared.Poll(context.WithoutCancel(ctx), 0, 3*time.Second,
		func(context.Context, time.Duration) error {
			select {
			case <-ctx.Done():
				return exit(2, "tart ip %s: context cancelled", name)
			case <-deadline:
				return exit(5, "tart ip %s: timed out waiting for IP address", name)
			case <-ticker.C:
				return nil
			}
		},
		func(context.Context) (observation, error) {
			if initial {
				initial = false
				return observation{}, nil
			}
			commandResult, commandErr := b.tart(ctx, []string{"ip", name}, nil, nil)
			return observation{result: commandResult, err: commandErr}, nil
		},
		func(_ context.Context, current observation, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, fetchErr
			}
			if current.err != nil {
				stderr := strings.ToLower(strings.TrimSpace(current.result.Stderr))
				if strings.Contains(stderr, "is your vm running") || strings.Contains(stderr, "not running") {
					return false, exit(2, "tart ip %s: %s", name, strings.TrimSpace(current.result.Stderr))
				}
				return false, nil
			}
			ip := strings.TrimSpace(current.result.Stdout)
			return ip != "" && ip != "--", nil
		}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Value.result.Stdout), nil
}

var validPOSIXUser = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9._-]*$`)

func (b *backend) injectSSHKey(ctx context.Context, name string, user string, publicKey string) error {
	if !validPOSIXUser.MatchString(user) {
		return exit(2, "tart.user %q is not a valid POSIX account name", user)
	}
	if err := b.waitForGuestAgent(ctx, name); err != nil {
		return fmt.Errorf("ssh key injection: %w", err)
	}
	sshDir := fmt.Sprintf("~%s/.ssh", user)
	safeKey := strings.ReplaceAll(strings.TrimSpace(publicKey), "'", "'\\''")
	injectScript := fmt.Sprintf(
		`mkdir -p %s && chmod 700 %s && echo '%s' >> %s/authorized_keys && chmod 600 %s/authorized_keys`,
		sshDir, sshDir, safeKey, sshDir, sshDir,
	)
	injectResult, err := b.tart(ctx, []string{"exec", name, "bash", "-c", injectScript}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("ssh key injection", injectResult, err)
	}
	return nil
}

func (b *backend) waitForGuestAgent(ctx context.Context, name string) error {
	// An IP can come from a previous DHCP lease before the new guest agent is
	// ready. Probe with a read-only command; never retry the key-writing script.
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	type observation struct {
		result LocalCommandResult
		err    error
	}
	_, err := shared.Poll(waitCtx, 0, 500*time.Millisecond, shared.SleepContext,
		func(ctx context.Context) (observation, error) {
			result, err := b.tart(ctx, []string{"exec", name, "/usr/bin/true"}, nil, nil)
			return observation{result: result, err: err}, nil
		},
		func(ctx context.Context, current observation, _ error) (bool, error) {
			if err := context.Cause(ctx); err != nil {
				return false, err
			}
			if current.err == nil {
				return true, nil
			}
			detail := current.result.Stderr + " " + current.err.Error()
			if strings.Contains(detail, "GRPCConnectionPoolError") || strings.Contains(detail, "is the Tart Guest Agent running?") {
				return false, nil
			}
			return false, commandError("Tart Guest Agent readiness", current.result, current.err)
		},
		func(result shared.PollResult[observation]) {
			if result.Attempt == 1 {
				fmt.Fprintln(b.rt.Stderr, "waiting for Tart Guest Agent before SSH key injection")
			}
		})
	if err != nil {
		return fmt.Errorf("wait for Tart Guest Agent: %w", err)
	}
	return nil
}

// enableScreenSharing turns on the guest's built-in macOS Screen Sharing for a
// --desktop lease (port 5900). Authentication uses the guest account's own
// credentials; crabbox provisions no VNC password and passes no secret to the
// guest. macOS Screen Sharing binds all guest interfaces, so the service is
// reachable at the guest's address on the host-local tart network (gated by
// account auth); an SSH tunnel can keep the viewer on 127.0.0.1. Only invoked
// for --desktop leases.
func (b *backend) enableScreenSharing(ctx context.Context, name string) error {
	script := `set -eu
sudo launchctl enable system/com.apple.screensharing || true
sudo launchctl load -w /System/Library/LaunchDaemons/com.apple.screensharing.plist 2>/dev/null || true
sudo launchctl kickstart -k system/com.apple.screensharing || true
for i in 1 2 3 4 5 6 7 8 9 10; do
  if nc -z 127.0.0.1 5900; then exit 0; fi
  sleep 1
done
echo 'macOS Screen Sharing did not start (no VNC listener on 127.0.0.1:5900)' >&2
exit 1`
	result, err := b.tart(ctx, []string{"exec", name, "bash", "-c", script}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("enable screen sharing", result, err)
	}
	return nil
}

// stopVM stops a running VM.
func (b *backend) stopVM(ctx context.Context, name string) error {
	result, err := b.tart(ctx, []string{"stop", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("tart stop", result, err)
	}
	return nil
}

// deleteVM deletes a VM.
func (b *backend) deleteVM(ctx context.Context, name string) error {
	result, err := b.tart(ctx, []string{"delete", name}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("tart delete", result, err)
	}
	return nil
}

func (b *backend) listInstances(ctx context.Context) ([]tartInstance, error) {
	result, err := b.tart(ctx, []string{"list", "--source", "local", "--format", "json"}, nil, nil)
	if err != nil {
		return nil, commandError("tart list", result, err)
	}
	var instances []tartInstance
	if err := json.Unmarshal([]byte(result.Stdout), &instances); err != nil {
		return nil, exit(2, "parse tart list: %v", err)
	}
	return instances, nil
}

func (b *backend) resolveInstance(ctx context.Context, identifier string) (tartInstance, string, core.LeaseClaim, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return tartInstance{}, "", core.LeaseClaim{}, exit(2, "provider=%s requires --id <lease-id-or-slug-or-instance>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return tartInstance{}, "", core.LeaseClaim{}, err
	} else if ok {
		name := instanceNameFromClaim(claim)
		if name == "" {
			return tartInstance{}, "", core.LeaseClaim{}, exit(4, "tart lease %s has no instance name in its claim", claim.LeaseID)
		}
		instances, listErr := b.listInstances(ctx)
		if listErr != nil {
			return tartInstance{}, "", core.LeaseClaim{}, listErr
		}
		for _, inst := range instances {
			if inst.Name == name {
				ip := b.getIP(ctx, name)
				return inst, ip, claim, nil
			}
		}
		return tartInstance{Name: name, State: "missing"}, "", claim, nil
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return tartInstance{}, "", core.LeaseClaim{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return tartInstance{}, "", core.LeaseClaim{}, err
	}
	normalized := normalizeLeaseSlug(identifier)
	for _, inst := range instances {
		claim := claims[inst.Name]
		if inst.Name == identifier || claim.LeaseID == identifier || (normalized != "" && normalizeLeaseSlug(claim.Slug) == normalized) {
			ip := b.getIP(ctx, inst.Name)
			return inst, ip, claim, nil
		}
	}
	return tartInstance{}, "", core.LeaseClaim{}, exit(4, "tart lease not found: %s", identifier)
}

func (b *backend) getIP(ctx context.Context, name string) string {
	result, err := b.tart(ctx, []string{"ip", name}, nil, nil)
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(result.Stdout)
	if ip == "--" {
		return ""
	}
	return ip
}

func (b *backend) prepareLease(ctx context.Context, cfg Config, inst tartInstance, ip string, claim core.LeaseClaim, wait bool) (LeaseTarget, error) {
	server := b.serverFromInstance(inst, claim, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" && validPOSIXUser.MatchString(user) {
		cfg.Tart.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.Tart.WorkRoot = root
		cfg.WorkRoot = root
	}
	if ip == "" || ip == "--" {
		if !instanceRunning(inst.State) {
			server.Status = inst.State
			server.Labels["state"] = tartState(inst.State)
			return LeaseTarget{Server: server, LeaseID: claim.LeaseID}, nil
		}
		return LeaseTarget{}, exit(5, "tart instance %s has no IP address", inst.Name)
	}
	server.PublicNet.IPv4.IP = ip
	if claim.LeaseID != "" {
		keyPath, err := testboxKeyPath(claim.LeaseID)
		if err == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				cfg.SSHKey = keyPath
			}
		}
	}
	target := sshTargetFromConfig(cfg, ip)
	target.Port = sshPort
	target.FallbackPorts = []string{}
	target.ReadyCheck = "uname -s && test -d ~"
	target.SSHConfigProxy = true
	if wait {
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "tart ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
}

func (b *backend) serverFromInstance(inst tartInstance, claim core.LeaseClaim, cfg Config) Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	if labels["crabbox"] == "" {
		labels["crabbox"] = "true"
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["instance"] == "" {
		labels["instance"] = inst.Name
	}
	if labels["lease"] == "" {
		labels["lease"] = claim.LeaseID
	}
	if labels["slug"] == "" {
		labels["slug"] = claim.Slug
	}
	if labels["state"] == "" {
		labels["state"] = tartState(inst.State)
	}
	if labels["server_type"] == "" {
		labels["server_type"] = firstNonBlank(inst.Source, cfg.Tart.Image)
	}
	// Native inventory's Source is a storage kind, not an image identity.
	// Only acquisition records image provenance in the claim.
	if labels["ssh_user"] == "" {
		labels["ssh_user"] = cfg.Tart.User
	}
	if labels["ssh_port"] == "" {
		labels["ssh_port"] = sshPort
	}
	if labels["work_root"] == "" {
		labels["work_root"] = cfg.Tart.WorkRoot
	}
	status := tartState(inst.State)
	if instanceRunning(inst.State) && labels["state"] == "ready" {
		status = "ready"
	}
	server := Server{
		CloudID:     inst.Name,
		ImmutableID: claim.CloudImmutableID,
		Provider:    providerName,
		Name:        inst.Name,
		Status:      status,
		Labels:      labels,
	}
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.Tart.Image)
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
		name := instanceNameFromClaim(claim)
		if name == "" {
			continue
		}
		out[name] = claim
	}
	return out, nil
}

func instanceScope(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return "instance:" + name
}

func instanceNameFromClaim(claim core.LeaseClaim) string {
	if name := strings.TrimSpace(claim.Labels["instance"]); name != "" {
		return name
	}
	return instanceNameFromScope(claim.ProviderScope)
}

func instanceNameFromScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if !strings.HasPrefix(scope, "instance:") {
		return ""
	}
	return strings.TrimPrefix(scope, "instance:")
}

func shouldCleanup(server Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	if strings.EqualFold(server.Labels["keep"], "true") {
		return false, "keep=true"
	}
	if !hasClaim {
		return false, "missing claim"
	}
	if !instanceRunning(server.Status) && server.Status != "ready" {
		return true, "instance state=" + blank(server.Status, "unknown")
	}
	if hasClaim {
		lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
		if err != nil || lastUsed.IsZero() {
			return false, "claim active"
		}
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		if idle <= 0 {
			return false, "claim active"
		}
		if now.After(lastUsed.Add(idle).Add(12 * time.Hour)) {
			return true, "claim expired"
		}
		return false, "claim active"
	}
	return false, "missing claim"
}

func (b *backend) tart(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	env, err := tartEnvironment()
	if err != nil {
		return LocalCommandResult{}, err
	}
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   "tart",
		Args:   args,
		Env:    env,
		Stdout: stdout,
		Stderr: stderr,
	})
}

func instanceRunning(state string) bool {
	switch tartState(state) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func tartState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
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
	if value == "" {
		return "unknown"
	}
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func firstNonBlank(values ...string) string {
	return shared.FirstNonBlank(values...)
}
