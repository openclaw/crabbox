package runpod

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// Polling configuration for waiting on a freshly deployed pod to expose its
// public SSH port. RunPod typically reports publicIp/portMappings within tens
// of seconds; cap at 10 minutes to stay under bootstrap timeouts while leaving
// slack for cold scheduler regions.
const (
	runpodSSHPollInitial = 3 * time.Second
	runpodSSHPollMax     = 15 * time.Second
	runpodSSHPollTimeout = 10 * time.Minute
	runpodCleanupTimeout = 15 * time.Second
	runpodPollJitter     = 0.2
)

var (
	runpodPollRandOnce sync.Once
	runpodPollRand     *rand.Rand
	runpodPollRandMu   sync.Mutex
)

func runpodJitter(d time.Duration) time.Duration {
	runpodPollRandOnce.Do(func() {
		runpodPollRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	})
	runpodPollRandMu.Lock()
	defer runpodPollRandMu.Unlock()
	delta := (runpodPollRand.Float64()*2 - 1) * runpodPollJitter
	jittered := time.Duration(float64(d) * (1 + delta))
	if jittered <= 0 {
		return d
	}
	return jittered
}

type runpodLeaseBackend struct {
	spec   core.ProviderSpec
	cfg    core.Config
	rt     core.Runtime
	client runpodAPI

	pollInitialOverride    time.Duration
	pollTimeoutOverride    time.Duration
	cleanupTimeoutOverride time.Duration
}

type runpodSSHEndpoint struct {
	Host   string
	Port   int
	User   string
	Kind   string
	Public bool
}

func NewRunpodLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	applyRunpodDefaults(&cfg)
	return &runpodLeaseBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *runpodLeaseBackend) Spec() core.ProviderSpec { return b.spec }

func (b *runpodLeaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	cfg := b.configForRun()
	servers, err := b.listServersFromClient(ctx, client, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s image=%s instance=%s disk=%dGB keep=%v\n",
		providerName, leaseID, slug, name, cfg.Runpod.Image, cfg.Runpod.InstanceID, cfg.Runpod.DiskGB, req.Keep)

	publicKey, err := core.PublicKeyFor(cfg.SSHKey)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	pod, err := client.DeployPod(ctx, runpodDeployInput{
		Name:              name,
		ImageName:         cfg.Runpod.Image,
		InstanceID:        cfg.Runpod.InstanceID,
		CloudType:         cfg.Runpod.CloudType,
		TemplateID:        cfg.Runpod.TemplateID,
		ContainerDiskInGb: cfg.Runpod.DiskGB,
		Ports:             "22/tcp",
		PublicKey:         publicKey,
	})
	if err != nil {
		return core.LeaseTarget{}, core.Exit(1, "runpod create pod failed: %v", err)
	}

	ready, err := b.waitForPodSSH(ctx, client, pod.ID)
	if err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return core.LeaseTarget{}, err
	}
	if err := validateCreatedRunpodPod(ready, pod.ID, name); err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return core.LeaseTarget{}, err
	}

	createdAt := core.ClockNow(b.rt.Clock).UTC()
	lease, err := b.prepareLease(ctx, cfg, ready, leaseID, slug, true)
	if err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return core.LeaseTarget{}, err
	}
	lease.Server = initializeRunpodLifecycle(lease.Server, cfg, req.Keep, createdAt)
	claim, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, core.LeaseClaim{}, false)
	if err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return core.LeaseTarget{}, err
	}
	if claim.LeaseID != "" {
		core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s pod=%s state=ready\n", leaseID, pod.ID)
	return lease, nil
}

func (b *runpodLeaseBackend) cleanupFailedAcquire(client runpodAPI, podID string, cause error) error {
	timeout := runpodCleanupTimeout
	if b.cleanupTimeoutOverride > 0 {
		timeout = b.cleanupTimeoutOverride
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := client.TerminatePod(cleanupCtx, podID); err != nil {
		return errors.Join(cause, fmt.Errorf("runpod cleanup failed for pod %s: %w", podID, err))
	}
	return cause
}

func (b *runpodLeaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		claim, ok, err := resolveRunpodClaim(req.ID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if !ok {
			return core.LeaseTarget{}, unclaimedRunpodError(req.ID)
		}
		pod, err := b.resolveClaimedPod(ctx, client, claim, true)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{Server: projectRunpodClaim(runpodServer(pod, claim.LeaseID, claim.Slug, cfg), claim), LeaseID: claim.LeaseID}, nil
	}
	claim, claimed, err := resolveRunpodClaim(req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var (
		pod     runpodPod
		leaseID string
		slug    string
	)
	if claimed {
		pod, err = b.resolveClaimedPod(ctx, client, claim, false)
		leaseID = claim.LeaseID
		slug = core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID))
	} else {
		pod, leaseID, slug, err = b.resolveUnclaimedPod(ctx, client, req.ID)
		if err == nil {
			var legacy bool
			claim, legacy, err = findRunpodClaimForPodName(pod)
			if err == nil && legacy {
				claimed = true
				leaseID = claim.LeaseID
				slug = claim.Slug
			}
		}
		if err == nil {
			err = ensureRunpodAdoptionDoesNotRetargetClaim(leaseID, pod)
		}
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	admit := req.Repo.Root != "" && !req.NoLocalStateMutations && !req.StatusOnly
	if admit && (!claimed || !runpodClaimIsBound(claim)) && !req.Reclaim {
		return core.LeaseTarget{}, core.Exit(2, "runpod pod %s is not bound to an exact local claim; retry with --reclaim to adopt it", pod.ID)
	}
	lease, err := b.prepareLease(ctx, cfg, pod, leaseID, slug, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if admit && (!claimed || !runpodClaimIsBound(claim)) {
		lease.Server = initializeRunpodLifecycle(lease.Server, cfg, true, core.ClockNow(b.rt.Clock).UTC())
	}
	if claimed {
		lease.Server = projectRunpodClaim(lease.Server, claim)
	} else {
		core.SetServerLeaseClaimSnapshot(&lease.Server, core.LeaseClaim{}, false)
	}
	if admit {
		if claimed {
			if err := shared.AuthorizeClaimActivity(claim); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		updated, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, claim, claimed)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		lease.Server = projectRunpodClaim(lease.Server, updated)
	}
	return lease, nil
}

func (b *runpodLeaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	return b.listServersFromClient(ctx, client, req.All)
}

func (b *runpodLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	// Doctor performs a read-only auth verification that never creates a pod.
	// Surface clearer messaging than the generic newRunpodClient error so the
	// missing-key case is obvious without staring at a stack trace.
	if strings.TrimSpace(b.cfg.Runpod.APIKey) == "" {
		return core.DoctorResult{}, core.Exit(2, "provider=%s requires RUNPOD_API_KEY (CRABBOX_RUNPOD_API_KEY also accepted)", providerName)
	}
	client, err := b.api()
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.Whoami(ctx); err != nil {
		return core.DoctorResult{}, core.Exit(1, "runpod auth check failed: %v", err)
	}
	pods, err := client.ListPods(ctx)
	if err != nil {
		return core.DoctorResult{}, core.Exit(1, "runpod list pods failed: %v", err)
	}
	return core.InventoryDoctorResult(providerName, len(pods)), nil
}

func (b *runpodLeaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	claim, exists, set := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return core.Exit(4, "runpod lease=%s has no exact observed claim snapshot; refusing release", req.Lease.LeaseID)
	}
	if err := b.validateLeaseClaim(req.Lease, claim); err != nil {
		return err
	}
	err := shared.RemoveExactClaimAfterContext(ctx, claim, b.claimBinding(req.Lease), func() error {
		client, err := b.api()
		if err != nil {
			return err
		}
		pod, err := b.resolveClaimedPod(ctx, client, claim, true)
		if err != nil {
			return err
		}
		if err := client.TerminatePod(ctx, pod.ID); err != nil {
			return core.Exit(1, "runpod terminate pod %s failed: %v", pod.ID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(claim.LeaseID)
	return nil
}

func (b *runpodLeaseBackend) AuthorizeStatusTouchClaim(ctx context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validateLeaseClaim(lease, claim); err != nil {
		return err
	}
	return shared.AuthorizeClaimActivity(claim)
}

func (b *runpodLeaseBackend) claimBinding(lease core.LeaseTarget) shared.ClaimBinding {
	return shared.ClaimBinding{Provider: providerName, ProviderScope: core.ProviderClaimScope(providerName, b.cfg), LeaseID: lease.LeaseID, Slug: lease.Server.Labels["slug"], CloudID: lease.Server.CloudID, RequiredLabels: map[string]string{"name": lease.Server.Name}}
}

func (b *runpodLeaseBackend) validateLeaseClaim(lease core.LeaseTarget, claim core.LeaseClaim) error {
	server := lease.Server
	if !runpodClaimIsBound(claim) || server.Provider != providerName || lease.LeaseID == "" || server.CloudID == "" || server.Name == "" || server.Labels["lease"] != lease.LeaseID || server.Labels["name"] != server.Name {
		return core.Exit(4, "runpod lease=%s target does not match the exact pod claim", lease.LeaseID)
	}
	return shared.ValidateClaimBinding(claim, b.claimBinding(lease))
}

func (b *runpodLeaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	var live core.Server
	updated, err := shared.CommitClaimTouch(ctx, req, shared.ClaimTouchPolicy{
		Provider: providerName,
		Authorize: func(ctx context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
			if err := b.AuthorizeStatusTouchClaim(ctx, lease, claim); err != nil {
				return err
			}
			client, err := b.api()
			if err != nil {
				return err
			}
			pod, err := b.resolveClaimedPod(ctx, client, claim, true)
			if err != nil {
				return err
			}
			live = runpodServer(pod, claim.LeaseID, claim.Slug, b.configForRun())
			return nil
		},
		Prepare: func(claim core.LeaseClaim) (map[string]string, time.Time) {
			cfg := b.configForRun()
			if req.IdleTimeout > 0 {
				cfg.IdleTimeout = req.IdleTimeout
			}
			now := core.ClockNow(b.rt.Clock).UTC()
			return core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(shared.ClaimLifecycleLabels(claim), cfg, req.State, now, req.IdleTimeoutOverride), now
		},
	})
	if err != nil {
		return core.Server{}, err
	}
	return projectRunpodClaim(live, updated), nil
}

func (b *runpodLeaseBackend) api() (runpodAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newRunpodClient(b.cfg, b.rt)
}

func (b *runpodLeaseBackend) configForRun() core.Config {
	cfg := b.cfg
	applyRunpodDefaults(&cfg)
	return cfg
}

func applyRunpodDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.Runpod.APIURL == "" {
		cfg.Runpod.APIURL = core.RunpodConfigDefaultAPIURL
	}
	if cfg.Runpod.CloudType == "" {
		cfg.Runpod.CloudType = core.RunpodConfigDefaultCloudType
	}
	if cfg.Runpod.InstanceID == "" {
		cfg.Runpod.InstanceID = core.RunpodConfigDefaultInstanceID
	}
	if cfg.Runpod.Image == "" {
		cfg.Runpod.Image = core.RunpodConfigDefaultImage
	}
	if cfg.Runpod.DiskGB <= 0 {
		cfg.Runpod.DiskGB = core.RunpodConfigDefaultDiskGB
	}
	cfg.Runpod.WorkRoot = core.ResolveInheritedWorkRoot(cfg.Runpod.WorkRoot, cfg.WorkRoot, core.RunpodWorkRootFallback)
	if cfg.Runpod.User != "" {
		cfg.SSHUser = cfg.Runpod.User
	} else if cfg.SSHUser == "" || cfg.SSHUser == "crabbox" {
		// RunPod pods always boot with root as the SSH user. The local USER
		// environment variable is unrelated to the remote account.
		cfg.SSHUser = core.RunpodSSHUserFallback
	}
	if cfg.Runpod.WorkRoot != "" {
		cfg.WorkRoot = cfg.Runpod.WorkRoot
	}
	cfg.SSHPort = ""
	cfg.SSHFallbackPorts = nil
	cfg.ServerType = cfg.Runpod.InstanceID
}

func (b *runpodLeaseBackend) waitForPodSSH(ctx context.Context, client runpodAPI, podID string) (runpodPod, error) {
	overall := runpodSSHPollTimeout
	if b.pollTimeoutOverride > 0 {
		overall = b.pollTimeoutOverride
	}
	initial := runpodSSHPollInitial
	if b.pollInitialOverride > 0 {
		initial = b.pollInitialOverride
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	interval := initial
	for {
		pod, err := client.GetPod(deadlineCtx, podID)
		if err != nil {
			if deadlineCtx.Err() != nil {
				return runpodPod{}, fmt.Errorf("runpod pod %s ssh wait cancelled: %w", podID, deadlineCtx.Err())
			}
			return runpodPod{}, err
		}
		endpoint := pod.SSHEndpoint()
		if endpoint.Host != "" && endpoint.Port != 0 && (endpoint.Public || strings.EqualFold(pod.DesiredStatus, "RUNNING")) {
			return pod, nil
		}
		sleepFor := runpodJitter(interval)
		select {
		case <-deadlineCtx.Done():
			return runpodPod{}, fmt.Errorf("runpod pod %s ssh endpoint not exposed within %s", podID, overall)
		case <-time.After(sleepFor):
		}
		if interval < runpodSSHPollMax {
			interval *= 2
			if interval > runpodSSHPollMax {
				interval = runpodSSHPollMax
			}
		}
	}
}

func (b *runpodLeaseBackend) prepareLease(ctx context.Context, cfg core.Config, pod runpodPod, leaseID, slug string, wait bool) (core.LeaseTarget, error) {
	server := runpodServer(pod, leaseID, slug, cfg)
	target := runpodSSHTarget(cfg, pod)
	if wait {
		bootstrapTarget := target
		bootstrapTarget.ReadyCheck = "true"
		if err := core.WaitForSSHReady(ctx, &bootstrapTarget, b.rt.Stderr, "runpod pod ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		target.Port = bootstrapTarget.Port
		if err := b.bootstrapRunpodTools(ctx, target); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "runpod pod ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Status = "ready"
		if server.Labels != nil {
			server.Labels["state"] = "ready"
		}
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *runpodLeaseBackend) bootstrapRunpodTools(ctx context.Context, target core.SSHTarget) error {
	if b.rt.Stderr != nil {
		fmt.Fprintln(b.rt.Stderr, "bootstrapping runpod pod tools")
	}
	if err := core.RunSSHQuiet(ctx, target, runpodBootstrapToolsCommand()); err != nil {
		return core.Exit(1, "runpod pod tool bootstrap failed: %v", err)
	}
	return nil
}

func runpodBootstrapToolsCommand() string {
	return strings.Join([]string{
		"set -e",
		"if command -v git >/dev/null 2>&1 && command -v rsync >/dev/null 2>&1 && command -v tar >/dev/null 2>&1; then exit 0; fi",
		"SUDO=; if [ \"$(id -u)\" != 0 ]; then SUDO=sudo; fi",
		"if command -v apt-get >/dev/null 2>&1; then",
		"  $SUDO apt-get update >/tmp/crabbox-runpod-apt-update.log 2>&1",
		"  $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends git rsync tar >/tmp/crabbox-runpod-apt-install.log 2>&1",
		"elif command -v dnf >/dev/null 2>&1; then",
		"  $SUDO dnf install -y git rsync tar >/tmp/crabbox-runpod-dnf-install.log 2>&1",
		"elif command -v yum >/dev/null 2>&1; then",
		"  $SUDO yum install -y git rsync tar >/tmp/crabbox-runpod-yum-install.log 2>&1",
		"elif command -v apk >/dev/null 2>&1; then",
		"  $SUDO apk add --no-cache git rsync tar >/tmp/crabbox-runpod-apk-install.log 2>&1",
		"fi",
		"command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
	}, "\n")
}

func resolveRunpodClaim(identifier string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, core.Exit(2, "provider=%s requires --id <pod-id-or-lease>", providerName)
	}
	if claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(identifier, providerName); err != nil {
		return core.LeaseClaim{}, false, err
	} else if exact && !ok {
		return core.LeaseClaim{}, false, core.Exit(2, "local claim %s belongs to provider=%s, not provider=%s", identifier, core.Blank(claim.Provider, "<unknown>"), providerName)
	} else if exact {
		return claim, true, nil
	} else if ok {
		// A local lease alias is stronger evidence of operator intent than a
		// different claim whose provider ID or pod name happens to collide.
		return claim, true, nil
	}
	if claim, ok, err := core.ResolveLeaseClaimForProviderCloudID(identifier, providerName); err != nil {
		return core.LeaseClaim{}, false, err
	} else if ok {
		return claim, true, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	var match core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.Labels["name"]) != identifier {
			continue
		}
		if match.LeaseID != "" {
			return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s claims match pod name %s", providerName, identifier)
		}
		match = claim
	}
	if match.LeaseID != "" {
		return match, true, nil
	}
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func findRunpodClaimForPodName(pod runpodPod) (core.LeaseClaim, bool, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	_, podSlug := runpodLeaseIdentity(pod.Name)
	var match core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || core.NormalizeLeaseSlug(claim.Slug) != core.NormalizeLeaseSlug(podSlug) || core.LeaseProviderName(claim.LeaseID, claim.Slug) != pod.Name {
			continue
		}
		if match.LeaseID != "" {
			return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s claims match pod name %s", providerName, pod.Name)
		}
		match = claim
	}
	return match, match.LeaseID != "", nil
}

func ensureRunpodAdoptionDoesNotRetargetClaim(leaseID string, pod runpodPod) error {
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, providerName)
	if err != nil {
		return err
	}
	if !exact {
		return nil
	}
	if !ok {
		return core.Exit(2, "cannot adopt RunPod pod %s as lease %s because that local claim belongs to provider=%s", pod.ID, leaseID, core.Blank(claim.Provider, "<unknown>"))
	}
	if !runpodClaimIsBound(claim) {
		return nil
	}
	if claim.CloudID != pod.ID || strings.TrimSpace(claim.Labels["name"]) != pod.Name {
		return core.Exit(2, "cannot retarget RunPod claim %s from pod %s (%s) to pod %s (%s)", claim.LeaseID, claim.CloudID, claim.Labels["name"], pod.ID, pod.Name)
	}
	return nil
}

func runpodClaimIsBound(claim core.LeaseClaim) bool {
	return claim.Provider == providerName &&
		strings.TrimSpace(claim.LeaseID) != "" &&
		strings.TrimSpace(claim.CloudID) != "" &&
		strings.TrimSpace(claim.Labels["name"]) != ""
}

func (b *runpodLeaseBackend) resolveClaimedPod(ctx context.Context, client runpodAPI, claim core.LeaseClaim, requireBound bool) (runpodPod, error) {
	bound := runpodClaimIsBound(claim)
	if requireBound && !bound {
		return runpodPod{}, unclaimedRunpodError(claim.LeaseID)
	}
	var (
		pod runpodPod
		err error
	)
	if strings.TrimSpace(claim.CloudID) != "" {
		pod, err = client.GetPod(ctx, claim.CloudID)
	} else {
		slug := core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID))
		pod, err = b.findPodByName(ctx, client, core.LeaseProviderName(claim.LeaseID, slug))
	}
	if err != nil {
		return runpodPod{}, err
	}
	if !bound {
		return pod, nil
	}
	if pod.ID != claim.CloudID {
		return runpodPod{}, core.Exit(2, "runpod claim %s expects pod %s but provider returned %s", claim.LeaseID, claim.CloudID, core.Blank(pod.ID, "<empty>"))
	}
	if name := strings.TrimSpace(claim.Labels["name"]); pod.Name != name {
		return runpodPod{}, core.Exit(2, "runpod claim %s expects pod name %s but provider returned %s", claim.LeaseID, name, core.Blank(pod.Name, "<empty>"))
	}
	return pod, nil
}

func (b *runpodLeaseBackend) resolveUnclaimedPod(ctx context.Context, client runpodAPI, identifier string) (runpodPod, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return runpodPod{}, "", "", core.Exit(2, "provider=%s requires --id <pod-id-or-lease>", providerName)
	}
	if strings.HasPrefix(identifier, "cbx_") {
		pod, ok, err := b.findPodByLeaseName(ctx, client, identifier)
		if err != nil {
			return runpodPod{}, "", "", err
		}
		if ok {
			leaseID, slug := runpodLeaseIdentity(pod.Name)
			return pod, leaseID, slug, nil
		}
		slug := core.NewLeaseSlug(identifier)
		pod, err = b.findPodByName(ctx, client, core.LeaseProviderName(identifier, slug))
		return pod, identifier, slug, err
	}
	// Identifier is treated as a raw RunPod pod ID first, then as a pod name
	// fallback (pod ids are opaque short strings without dashes; pod names we
	// own start with "crabbox-").
	if pod, err := client.GetPod(ctx, identifier); err == nil {
		leaseID, slug := runpodLeaseIdentity(pod.Name)
		return pod, leaseID, slug, nil
	}
	pod, err := b.findPodByName(ctx, client, identifier)
	if err != nil {
		return runpodPod{}, "", "", err
	}
	leaseID, slug := runpodLeaseIdentity(pod.Name)
	return pod, leaseID, slug, nil
}

func unclaimedRunpodError(identifier string) error {
	return core.Exit(2, "runpod pod %s has no exact resource-bound local claim; adopt it from a reuse command with --reclaim before stopping it", core.Blank(strings.TrimSpace(identifier), "<unknown>"))
}

func validateCreatedRunpodPod(pod runpodPod, expectedID, expectedName string) error {
	expected := shared.NamedResourceIdentity{ID: expectedID, Name: expectedName}
	mismatch := expected.Validate(shared.NamedResourceIdentity{ID: pod.ID, Name: pod.Name})
	if mismatch == nil {
		return nil
	}
	if mismatch.Field == "ID" {
		return core.Exit(1, "runpod create returned pod %s but readiness resolved %s", core.Blank(expectedID, "<empty>"), core.Blank(pod.ID, "<empty>"))
	}
	return core.Exit(1, "runpod create expected pod name %s but readiness returned %s", core.Blank(expectedName, "<empty>"), core.Blank(pod.Name, "<empty>"))
}

func (b *runpodLeaseBackend) findPodByName(ctx context.Context, client runpodAPI, name string) (runpodPod, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return runpodPod{}, err
	}
	exact := make([]runpodPod, 0, 1)
	for _, pod := range pods {
		if pod.Name == name || pod.ID == name {
			exact = append(exact, pod)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return runpodPod{}, core.Exit(2, "multiple RunPod pods match exact identifier %s", name)
	}
	normalized := core.NormalizeLeaseSlug(name)
	matches := make([]runpodPod, 0, 1)
	for _, pod := range pods {
		if core.NormalizeLeaseSlug(pod.Name) == normalized {
			matches = append(matches, pod)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return runpodPod{}, core.Exit(2, "multiple RunPod pods match normalized name %s", name)
	}
	return runpodPod{}, core.Exit(4, "runpod pod not found: %s", name)
}

func (b *runpodLeaseBackend) findPodByLeaseName(ctx context.Context, client runpodAPI, leaseID string) (runpodPod, bool, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return runpodPod{}, false, err
	}
	matches := make([]runpodPod, 0, 1)
	for _, pod := range pods {
		if id, _ := runpodLeaseIdentity(pod.Name); id == leaseID {
			matches = append(matches, pod)
		}
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	if len(matches) > 1 {
		return runpodPod{}, false, core.Exit(2, "multiple RunPod pods match lease %s", leaseID)
	}
	return runpodPod{}, false, nil
}

func (b *runpodLeaseBackend) listServersFromClient(ctx context.Context, client runpodAPI, all bool) ([]core.LeaseView, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return nil, err
	}
	cfg := b.configForRun()
	servers := make([]core.Server, 0, len(pods))
	for _, pod := range pods {
		if !all && !strings.HasPrefix(pod.Name, "crabbox-") {
			continue
		}
		leaseID, slug := runpodLeaseIdentity(pod.Name)
		server := runpodServer(pod, leaseID, slug, cfg)
		claim, exists, err := core.ResolveLeaseClaimForProviderCloudID(pod.ID, providerName)
		if err != nil {
			return nil, err
		}
		if exists && runpodClaimIsBound(claim) && claim.Labels["name"] == pod.Name {
			server = projectRunpodClaim(runpodServer(pod, claim.LeaseID, claim.Slug, cfg), claim)
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func runpodServer(pod runpodPod, leaseID, slug string, cfg core.Config) core.Server {
	labels := map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": providerName, "lease": leaseID, "slug": core.NormalizeLeaseSlug(slug)}
	labels["name"] = pod.Name
	state := pod.DesiredStatus
	if state == "" {
		state = "unknown"
	}
	labels["state"] = strings.ToLower(state)
	labels["work_root"] = cfg.WorkRoot
	labels["pod_id"] = pod.ID
	if pod.MachineID != "" {
		labels["machine_id"] = pod.MachineID
	}
	if pod.Machine.PodHostID != "" {
		labels["pod_host_id"] = pod.Machine.PodHostID
	}
	endpoint := pod.SSHEndpoint()
	if endpoint.Host != "" {
		labels["ssh_host"] = endpoint.Host
	}
	if endpoint.Port != 0 {
		labels["ssh_port"] = strconv.Itoa(endpoint.Port)
	}
	if endpoint.User != "" {
		labels["ssh_user"] = endpoint.User
	}
	if endpoint.Kind != "" {
		labels["ssh_kind"] = endpoint.Kind
	}
	server := core.Server{
		CloudID:  pod.ID,
		Provider: providerName,
		Name:     pod.Name,
		Status:   labels["state"],
		Labels:   labels,
	}
	if endpoint.Public {
		server.PublicNet.IPv4.IP = endpoint.Host
	}
	server.ServerType.Name = cfg.Runpod.InstanceID
	return server
}

func initializeRunpodLifecycle(server core.Server, cfg core.Config, keep bool, now time.Time) core.Server {
	labels := core.DirectLeaseLabels(cfg, server.Labels["lease"], server.Labels["slug"], providerName, "", keep, now)
	for key, value := range server.Labels {
		labels[key] = value
	}
	server.Labels = labels
	return server
}

func projectRunpodClaim(server core.Server, claim core.LeaseClaim) core.Server {
	labels := shared.CloneLabels(server.Labels)
	for key, value := range shared.ClaimLifecycleLabels(claim) {
		switch key {
		case "provider", "lease", "slug", "name", "pod_id", "machine_id", "pod_host_id", "ssh_host", "ssh_port", "ssh_user", "ssh_kind", "work_root":
			continue // Identity and routes come from the validated native observation.
		}
		labels[key] = value
	}
	recordedObsolete := false
	switch strings.ToLower(labels["state"]) {
	case "provisioning", "stopped", "failed", "exited", "dead", "terminated", "stopped_with_code":
		recordedObsolete = true
	}
	observedRunning := server.Status == "running" || server.Status == "ready"
	if state := shared.ObservedClaimActivityState(claim, labels["state"], server.Status, observedRunning, recordedObsolete); state != labels["state"] {
		labels["state"] = state
	}
	server.Labels = labels
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return server
}

func runpodSSHTarget(cfg core.Config, pod runpodPod) core.SSHTarget {
	endpoint := pod.SSHEndpoint()
	target := core.SSHTargetFromConfig(cfg, endpoint.Host)
	if endpoint.Port != 0 {
		target.Port = strconv.Itoa(endpoint.Port)
	}
	if endpoint.User != "" {
		target.User = endpoint.User
	}
	target.TargetOS = targetLinux
	target.NetworkKind = networkPublic
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
	return target
}

// runpodLeaseIdentity infers the (leaseID, slug) pair from a pod name that
// follows the leaseProviderName layout (crabbox-<slug>-<leaseSuffix>). For
// pods we did not name we synthesize an identity so List/Resolve still work.
func runpodLeaseIdentity(name string) (string, string) {
	name = strings.TrimSpace(name)
	const prefix = "crabbox-"
	if !strings.HasPrefix(name, prefix) {
		slug := core.NormalizeLeaseSlug(core.Blank(name, "manual"))
		return "rpod_" + slug, slug
	}
	rest := strings.TrimPrefix(name, prefix)
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 || idx == len(rest)-1 {
		slug := core.NormalizeLeaseSlug(rest)
		return "rpod_" + slug, slug
	}
	hash := rest[idx+1:]
	if len(hash) != 8 || !isLowerHex(hash) {
		slug := core.NormalizeLeaseSlug(rest)
		return "rpod_" + slug, slug
	}
	slug := core.NormalizeLeaseSlug(rest[:idx])
	return "rpod_" + hash, slug
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return value != ""
}
