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
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
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

func NewRunpodLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	applyRunpodDefaults(&cfg)
	return &runpodLeaseBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *runpodLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *runpodLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	cfg := b.configForRun()
	servers, err := b.listServersFromClient(ctx, client, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	name := leaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s image=%s instance=%s disk=%dGB keep=%v\n",
		providerName, leaseID, slug, name, cfg.Runpod.Image, cfg.Runpod.InstanceID, cfg.Runpod.DiskGB, req.Keep)

	pod, err := client.DeployPod(ctx, runpodDeployInput{
		Name:              name,
		ImageName:         cfg.Runpod.Image,
		InstanceID:        cfg.Runpod.InstanceID,
		CloudType:         cfg.Runpod.CloudType,
		TemplateID:        cfg.Runpod.TemplateID,
		ContainerDiskInGb: cfg.Runpod.DiskGB,
		Ports:             "22/tcp",
		StartSSH:          true,
	})
	if err != nil {
		return LeaseTarget{}, exit(1, "runpod create pod failed: %v", err)
	}

	ready, err := b.waitForPodSSH(ctx, client, pod.ID)
	if err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return LeaseTarget{}, err
	}
	if err := validateCreatedRunpodPod(ready, pod.ID, name); err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return LeaseTarget{}, err
	}

	lease, err := b.prepareLease(ctx, cfg, ready, leaseID, slug, req.Keep, true)
	if err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(client, pod.ID, err)
		}
		return LeaseTarget{}, err
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

func (b *runpodLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		claim, ok, err := resolveRunpodClaim(req.ID)
		if err != nil {
			return LeaseTarget{}, err
		}
		if !ok {
			return LeaseTarget{}, unclaimedRunpodError(req.ID)
		}
		pod, err := b.resolveClaimedPod(ctx, client, claim, true)
		if err != nil {
			return LeaseTarget{}, err
		}
		return LeaseTarget{Server: runpodServer(pod, claim.LeaseID, claim.Slug, cfg, true), LeaseID: claim.LeaseID}, nil
	}
	claim, claimed, err := resolveRunpodClaim(req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	var (
		pod     runpodPod
		leaseID string
		slug    string
	)
	if claimed {
		pod, err = b.resolveClaimedPod(ctx, client, claim, false)
		leaseID = claim.LeaseID
		slug = blank(claim.Slug, newLeaseSlug(claim.LeaseID))
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
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" && (!claimed || !runpodClaimIsBound(claim)) && !req.Reclaim {
		return LeaseTarget{}, exit(2, "runpod pod %s is not bound to an exact local claim; retry with --reclaim to adopt it", pod.ID)
	}
	lease, err := b.prepareLease(ctx, cfg, pod, leaseID, slug, true, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *runpodLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	return b.listServersFromClient(ctx, client, req.All)
}

func (b *runpodLeaseBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	// Doctor performs a read-only auth verification that never creates a pod.
	// Surface clearer messaging than the generic newRunpodClient error so the
	// missing-key case is obvious without staring at a stack trace.
	if strings.TrimSpace(b.cfg.Runpod.APIKey) == "" {
		return DoctorResult{}, exit(2, "provider=%s requires RUNPOD_API_KEY (CRABBOX_RUNPOD_API_KEY also accepted)", providerName)
	}
	client, err := b.api()
	if err != nil {
		return DoctorResult{}, err
	}
	if _, err := client.Whoami(ctx); err != nil {
		return DoctorResult{}, exit(1, "runpod auth check failed: %v", err)
	}
	pods, err := client.ListPods(ctx)
	if err != nil {
		return DoctorResult{}, exit(1, "runpod list pods failed: %v", err)
	}
	return inventoryDoctorResult(providerName, len(pods)), nil
}

func (b *runpodLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := b.api()
	if err != nil {
		return err
	}
	identifier := strings.TrimSpace(req.Lease.LeaseID)
	if identifier == "" {
		identifier = strings.TrimSpace(req.Lease.Server.CloudID)
	}
	if identifier == "" {
		identifier = strings.TrimSpace(req.Lease.Server.Name)
	}
	claim, ok, err := resolveRunpodClaim(identifier)
	if err != nil {
		return err
	}
	if !ok || !runpodClaimIsBound(claim) {
		return unclaimedRunpodError(identifier)
	}
	if leaseID := strings.TrimSpace(req.Lease.LeaseID); leaseID != "" && leaseID != claim.LeaseID {
		return exit(2, "runpod release lease %s does not match bound claim %s", leaseID, claim.LeaseID)
	}
	if podID := strings.TrimSpace(req.Lease.Server.CloudID); podID != "" && podID != claim.CloudID {
		return exit(2, "runpod release pod %s does not match bound claim pod %s", podID, claim.CloudID)
	}
	err = removeLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		pod, err := b.resolveClaimedPod(ctx, client, claim, true)
		if err != nil {
			return err
		}
		if err := client.TerminatePod(ctx, pod.ID); err != nil {
			return exit(1, "runpod terminate pod %s failed: %v", pod.ID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	removeStoredTestboxKey(claim.LeaseID)
	return nil
}

func (b *runpodLeaseBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.configForRun(), req.State, time.Now().UTC())
	return server, nil
}

func (b *runpodLeaseBackend) api() (runpodAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newRunpodClient(b.cfg, b.rt)
}

func (b *runpodLeaseBackend) configForRun() Config {
	cfg := b.cfg
	applyRunpodDefaults(&cfg)
	return cfg
}

func applyRunpodDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.Runpod.APIURL == "" {
		cfg.Runpod.APIURL = "https://rest.runpod.io/v1"
	}
	if cfg.Runpod.CloudType == "" {
		cfg.Runpod.CloudType = "SECURE"
	}
	if cfg.Runpod.InstanceID == "" {
		cfg.Runpod.InstanceID = "NVIDIA L4,NVIDIA RTX 4000 Ada Generation,NVIDIA RTX A4000,NVIDIA GeForce RTX 3090,NVIDIA GeForce RTX 4090,NVIDIA RTX A5000,NVIDIA RTX A4500"
	}
	if cfg.Runpod.Image == "" {
		cfg.Runpod.Image = "runpod/pytorch:2.8.0-py3.11-cuda12.8.1-cudnn-devel-ubuntu22.04"
	}
	if cfg.Runpod.DiskGB <= 0 {
		cfg.Runpod.DiskGB = 20
	}
	if cfg.Runpod.WorkRoot == "" {
		if !isDefaultWorkRoot(cfg.WorkRoot) {
			cfg.Runpod.WorkRoot = cfg.WorkRoot
		} else {
			cfg.Runpod.WorkRoot = "/tmp/crabbox"
		}
	}
	if cfg.Runpod.User != "" {
		cfg.SSHUser = cfg.Runpod.User
	} else if cfg.SSHUser == "" || cfg.SSHUser == "crabbox" {
		// RunPod pods always boot with root as the SSH user. The local USER
		// environment variable is unrelated to the remote account.
		cfg.SSHUser = "root"
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

func (b *runpodLeaseBackend) prepareLease(ctx context.Context, cfg Config, pod runpodPod, leaseID, slug string, keep, wait bool) (LeaseTarget, error) {
	server := runpodServer(pod, leaseID, slug, cfg, keep)
	target := runpodSSHTarget(cfg, pod)
	if wait {
		bootstrapTarget := target
		bootstrapTarget.ReadyCheck = "true"
		if err := waitForSSHReady(ctx, &bootstrapTarget, b.rt.Stderr, "runpod pod ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		target.Port = bootstrapTarget.Port
		if err := b.bootstrapRunpodTools(ctx, target); err != nil {
			return LeaseTarget{}, err
		}
		if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "runpod pod ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return LeaseTarget{}, err
		}
		server.Status = "ready"
		if server.Labels != nil {
			server.Labels["state"] = "ready"
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *runpodLeaseBackend) bootstrapRunpodTools(ctx context.Context, target SSHTarget) error {
	if b.rt.Stderr != nil {
		fmt.Fprintln(b.rt.Stderr, "bootstrapping runpod pod tools")
	}
	if err := runSSHQuiet(ctx, target, runpodBootstrapToolsCommand()); err != nil {
		return exit(1, "runpod pod tool bootstrap failed: %v", err)
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

func resolveRunpodClaim(identifier string) (LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return LeaseClaim{}, false, exit(2, "provider=%s requires --id <pod-id-or-lease>", providerName)
	}
	if claim, ok, exact, err := resolveLeaseClaimForProviderWithExact(identifier, providerName); err != nil {
		return LeaseClaim{}, false, err
	} else if exact && !ok {
		return LeaseClaim{}, false, exit(2, "local claim %s belongs to provider=%s, not provider=%s", identifier, blank(claim.Provider, "<unknown>"), providerName)
	} else if exact {
		return claim, true, nil
	} else if ok {
		// A local lease alias is stronger evidence of operator intent than a
		// different claim whose provider ID or pod name happens to collide.
		return claim, true, nil
	}
	if claim, ok, err := resolveLeaseClaimForProviderCloudID(identifier, providerName); err != nil {
		return LeaseClaim{}, false, err
	} else if ok {
		return claim, true, nil
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	var match LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.Labels["name"]) != identifier {
			continue
		}
		if match.LeaseID != "" {
			return LeaseClaim{}, false, exit(2, "multiple provider=%s claims match pod name %s", providerName, identifier)
		}
		match = claim
	}
	if match.LeaseID != "" {
		return match, true, nil
	}
	return resolveLeaseClaimForProvider(identifier, providerName)
}

func findRunpodClaimForPodName(pod runpodPod) (LeaseClaim, bool, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	_, podSlug := runpodLeaseIdentity(pod.Name)
	var match LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName ||
			normalizeLeaseSlug(claim.Slug) != normalizeLeaseSlug(podSlug) ||
			leaseProviderName(claim.LeaseID, claim.Slug) != pod.Name {
			continue
		}
		if match.LeaseID != "" {
			return LeaseClaim{}, false, exit(2, "multiple provider=%s claims match pod name %s", providerName, pod.Name)
		}
		match = claim
	}
	return match, match.LeaseID != "", nil
}

func ensureRunpodAdoptionDoesNotRetargetClaim(leaseID string, pod runpodPod) error {
	claim, ok, exact, err := resolveLeaseClaimForProviderWithExact(leaseID, providerName)
	if err != nil {
		return err
	}
	if !exact {
		return nil
	}
	if !ok {
		return exit(2, "cannot adopt RunPod pod %s as lease %s because that local claim belongs to provider=%s", pod.ID, leaseID, blank(claim.Provider, "<unknown>"))
	}
	if !runpodClaimIsBound(claim) {
		return nil
	}
	if claim.CloudID != pod.ID || strings.TrimSpace(claim.Labels["name"]) != pod.Name {
		return exit(2, "cannot retarget RunPod claim %s from pod %s (%s) to pod %s (%s)", claim.LeaseID, claim.CloudID, claim.Labels["name"], pod.ID, pod.Name)
	}
	return nil
}

func runpodClaimIsBound(claim LeaseClaim) bool {
	return claim.Provider == providerName &&
		strings.TrimSpace(claim.LeaseID) != "" &&
		strings.TrimSpace(claim.CloudID) != "" &&
		strings.TrimSpace(claim.Labels["name"]) != ""
}

func (b *runpodLeaseBackend) resolveClaimedPod(ctx context.Context, client runpodAPI, claim LeaseClaim, requireBound bool) (runpodPod, error) {
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
		slug := blank(claim.Slug, newLeaseSlug(claim.LeaseID))
		pod, err = b.findPodByName(ctx, client, leaseProviderName(claim.LeaseID, slug))
	}
	if err != nil {
		return runpodPod{}, err
	}
	if !bound {
		return pod, nil
	}
	if pod.ID != claim.CloudID {
		return runpodPod{}, exit(2, "runpod claim %s expects pod %s but provider returned %s", claim.LeaseID, claim.CloudID, blank(pod.ID, "<empty>"))
	}
	if name := strings.TrimSpace(claim.Labels["name"]); pod.Name != name {
		return runpodPod{}, exit(2, "runpod claim %s expects pod name %s but provider returned %s", claim.LeaseID, name, blank(pod.Name, "<empty>"))
	}
	return pod, nil
}

func (b *runpodLeaseBackend) resolveUnclaimedPod(ctx context.Context, client runpodAPI, identifier string) (runpodPod, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return runpodPod{}, "", "", exit(2, "provider=%s requires --id <pod-id-or-lease>", providerName)
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
		slug := newLeaseSlug(identifier)
		pod, err = b.findPodByName(ctx, client, leaseProviderName(identifier, slug))
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
	return exit(2, "runpod pod %s has no exact resource-bound local claim; adopt it from a reuse command with --reclaim before stopping it", blank(strings.TrimSpace(identifier), "<unknown>"))
}

func validateCreatedRunpodPod(pod runpodPod, expectedID, expectedName string) error {
	if strings.TrimSpace(expectedID) == "" || pod.ID != expectedID {
		return exit(1, "runpod create returned pod %s but readiness resolved %s", blank(expectedID, "<empty>"), blank(pod.ID, "<empty>"))
	}
	if strings.TrimSpace(expectedName) == "" || pod.Name != expectedName {
		return exit(1, "runpod create expected pod name %s but readiness returned %s", blank(expectedName, "<empty>"), blank(pod.Name, "<empty>"))
	}
	return nil
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
		return runpodPod{}, exit(2, "multiple RunPod pods match exact identifier %s", name)
	}
	normalized := normalizeLeaseSlug(name)
	matches := make([]runpodPod, 0, 1)
	for _, pod := range pods {
		if normalizeLeaseSlug(pod.Name) == normalized {
			matches = append(matches, pod)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return runpodPod{}, exit(2, "multiple RunPod pods match normalized name %s", name)
	}
	return runpodPod{}, exit(4, "runpod pod not found: %s", name)
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
		return runpodPod{}, false, exit(2, "multiple RunPod pods match lease %s", leaseID)
	}
	return runpodPod{}, false, nil
}

func (b *runpodLeaseBackend) listServersFromClient(ctx context.Context, client runpodAPI, all bool) ([]LeaseView, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return nil, err
	}
	cfg := b.configForRun()
	servers := make([]Server, 0, len(pods))
	for _, pod := range pods {
		if !all && !strings.HasPrefix(pod.Name, "crabbox-") {
			continue
		}
		leaseID, slug := runpodLeaseIdentity(pod.Name)
		servers = append(servers, runpodServer(pod, leaseID, slug, cfg, true))
	}
	return servers, nil
}

func runpodServer(pod runpodPod, leaseID, slug string, cfg Config, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
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
	server := Server{
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

func runpodSSHTarget(cfg Config, pod runpodPod) SSHTarget {
	endpoint := pod.SSHEndpoint()
	target := sshTargetFromConfig(cfg, endpoint.Host)
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
		slug := normalizeLeaseSlug(blank(name, "manual"))
		return "rpod_" + slug, slug
	}
	rest := strings.TrimPrefix(name, prefix)
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 || idx == len(rest)-1 {
		slug := normalizeLeaseSlug(rest)
		return "rpod_" + slug, slug
	}
	hash := rest[idx+1:]
	if len(hash) != 8 || !isLowerHex(hash) {
		slug := normalizeLeaseSlug(rest)
		return "rpod_" + slug, slug
	}
	slug := normalizeLeaseSlug(rest[:idx])
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
