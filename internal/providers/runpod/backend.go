package runpod

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Polling configuration for waiting on a freshly deployed pod to expose its
// public SSH port. RunPod typically reports runtime.ports within tens of
// seconds for CPU pods; cap at 10 minutes to stay well under bootstrap
// timeouts while leaving slack for cold scheduler regions.
const (
	runpodSSHPollInitial = 3 * time.Second
	runpodSSHPollMax     = 15 * time.Second
	runpodSSHPollTimeout = 10 * time.Minute
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

	pollInitialOverride time.Duration
	pollTimeoutOverride time.Duration
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

	pod, err := client.DeployCpuPod(ctx, runpodDeployInput{
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
		return LeaseTarget{}, exit(1, "runpod deployCpuPod failed: %v", err)
	}

	ready, err := b.waitForPodSSH(ctx, client, pod.ID)
	if err != nil {
		if !req.Keep {
			_ = client.TerminatePod(context.Background(), pod.ID)
		}
		return LeaseTarget{}, err
	}

	lease, err := b.prepareLease(ctx, cfg, ready, leaseID, slug, req.Keep, true)
	if err != nil {
		if !req.Keep {
			_ = client.TerminatePod(context.Background(), pod.ID)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = client.TerminatePod(context.Background(), pod.ID)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s pod=%s state=ready\n", leaseID, pod.ID)
	return lease, nil
}

func (b *runpodLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	pod, leaseID, slug, err := b.resolvePod(ctx, client, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return LeaseTarget{Server: runpodServer(pod, leaseID, slug, cfg, true), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, pod, leaseID, slug, true, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
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
	podID := strings.TrimSpace(req.Lease.Server.CloudID)
	if podID == "" {
		// Fall back to a fresh resolve so release works against legacy lease
		// records that only carry the lease id.
		pod, _, _, err := b.resolvePod(ctx, client, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		podID = pod.ID
	}
	if podID == "" {
		return exit(2, "provider=%s release requires a pod id", providerName)
	}
	if err := client.TerminatePod(ctx, podID); err != nil {
		return exit(1, "runpod terminate pod %s failed: %v", podID, err)
	}
	removeLeaseClaim(req.Lease.LeaseID)
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
		cfg.Runpod.APIURL = "https://api.runpod.io/graphql"
	}
	if cfg.Runpod.CloudType == "" {
		cfg.Runpod.CloudType = "ALL"
	}
	if cfg.Runpod.InstanceID == "" {
		// Cheapest documented CPU flavor at the time of writing: cpu3c
		// (compute-optimized) with 2 vCPU and 4 GB RAM. Documented in
		// docs/providers/runpod.md so callers can override knowingly.
		cfg.Runpod.InstanceID = "cpu3c-2-4"
	}
	if cfg.Runpod.Image == "" {
		cfg.Runpod.Image = "runpod/base:0.6.2"
	}
	if cfg.Runpod.DiskGB <= 0 {
		cfg.Runpod.DiskGB = 10
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
		cfg.SSHUser = blank(os.Getenv("USER"), "root")
		// runpod pods always boot with root as the SSH user; if a generic env
		// override hasn't kicked in, fall through to root.
		if cfg.SSHUser == "" {
			cfg.SSHUser = "root"
		}
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
		host, port := pod.SSHEndpoint()
		if host != "" && port != 0 {
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

func (b *runpodLeaseBackend) resolvePod(ctx context.Context, client runpodAPI, identifier string) (runpodPod, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return runpodPod{}, "", "", exit(2, "provider=%s requires --id <pod-id-or-lease>", providerName)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return runpodPod{}, "", "", err
	} else if ok {
		slug := blank(claim.Slug, newLeaseSlug(claim.LeaseID))
		name := leaseProviderName(claim.LeaseID, slug)
		pod, err := b.findPodByName(ctx, client, name)
		return pod, claim.LeaseID, slug, err
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

func (b *runpodLeaseBackend) findPodByName(ctx context.Context, client runpodAPI, name string) (runpodPod, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return runpodPod{}, err
	}
	normalized := normalizeLeaseSlug(name)
	for _, pod := range pods {
		if pod.Name == name || normalizeLeaseSlug(pod.Name) == normalized || pod.ID == name {
			return pod, nil
		}
	}
	return runpodPod{}, exit(4, "runpod pod not found: %s", name)
}

func (b *runpodLeaseBackend) findPodByLeaseName(ctx context.Context, client runpodAPI, leaseID string) (runpodPod, bool, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return runpodPod{}, false, err
	}
	for _, pod := range pods {
		if id, _ := runpodLeaseIdentity(pod.Name); id == leaseID {
			return pod, true, nil
		}
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
	host, port := pod.SSHEndpoint()
	if host != "" {
		labels["ssh_host"] = host
	}
	if port != 0 {
		labels["ssh_port"] = strconv.Itoa(port)
	}
	server := Server{
		CloudID:  pod.ID,
		Provider: providerName,
		Name:     pod.Name,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = host
	server.ServerType.Name = cfg.Runpod.InstanceID
	return server
}

func runpodSSHTarget(cfg Config, pod runpodPod) SSHTarget {
	host, port := pod.SSHEndpoint()
	target := sshTargetFromConfig(cfg, host)
	if port != 0 {
		target.Port = strconv.Itoa(port)
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
