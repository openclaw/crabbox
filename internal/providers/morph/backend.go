package morph

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const morphReadyCheck = "command -v bash >/dev/null && command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && (command -v python3 >/dev/null || command -v python >/dev/null || command -v perl >/dev/null)"
const defaultMorphWorkRoot = "/tmp/crabbox"
const morphAcquireRollbackTimeout = 30 * time.Second

var waitForMorphSSHReady = waitForSSHReady

type morphFlagValues struct {
	APIURL          *string
	Snapshot        *string
	SSHGatewayHost  *string
	WorkRoot        *string
	DeleteOnRelease *bool
	WakeOnSSH       *bool
}

type morphLeaseBackend struct {
	spec              ProviderSpec
	cfg               Config
	rt                Runtime
	client            morphAPI
	now               func() time.Time
	readyPollInterval time.Duration
	readyTimeout      time.Duration
	rollbackTimeout   time.Duration
}

func RegisterMorphProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return morphFlagValues{
		APIURL:          fs.String("morph-api-url", defaults.Morph.APIURL, "Morph API URL"),
		Snapshot:        fs.String("morph-snapshot", defaults.Morph.Snapshot, "Morph snapshot ID"),
		SSHGatewayHost:  fs.String("morph-ssh-gateway-host", defaults.Morph.SSHGatewayHost, "Morph SSH gateway host"),
		WorkRoot:        fs.String("morph-work-root", defaults.Morph.WorkRoot, "Morph remote Crabbox work root"),
		DeleteOnRelease: fs.Bool("morph-delete-on-release", defaults.Morph.DeleteOnRelease, "Delete Morph instances instead of pausing them on release"),
		WakeOnSSH:       fs.Bool("morph-wake-on-ssh", defaults.Morph.WakeOnSSH, "Enable Morph wake-on-ssh for paused instances"),
	}
}

func ApplyMorphProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isMorphProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=morph")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=morph; use --morph-snapshot")
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=morph supports target=linux only")
		}
	}
	v, ok := values.(morphFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "morph-api-url") {
		cfg.Morph.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "morph-snapshot") {
		cfg.Morph.Snapshot = *v.Snapshot
	}
	if flagWasSet(fs, "morph-ssh-gateway-host") {
		cfg.Morph.SSHGatewayHost = *v.SSHGatewayHost
	}
	if flagWasSet(fs, "morph-work-root") {
		cfg.Morph.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "morph-delete-on-release") {
		cfg.Morph.DeleteOnRelease = *v.DeleteOnRelease
	}
	if flagWasSet(fs, "morph-wake-on-ssh") {
		cfg.Morph.WakeOnSSH = *v.WakeOnSSH
	}
	if isMorphProviderName(cfg.Provider) {
		applyMorphDefaults(cfg)
		return validateMorphConfig(*cfg)
	}
	return nil
}

func NewMorphBackend(spec ProviderSpec, cfg Config, rt Runtime) (Backend, error) {
	applyMorphDefaults(&cfg)
	if err := validateMorphConfig(cfg); err != nil {
		return nil, err
	}
	return &morphLeaseBackend{
		spec:              spec,
		cfg:               cfg,
		rt:                rt,
		now:               time.Now,
		readyPollInterval: 2 * time.Second,
		readyTimeout:      10 * time.Minute,
		rollbackTimeout:   morphAcquireRollbackTimeout,
	}, nil
}

func (b *morphLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *morphLeaseBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	cfg := b.configForRun()
	if err := validateMorphCreateConfig(cfg); err != nil {
		return DoctorResult{}, err
	}
	client, err := b.api()
	if err != nil {
		return DoctorResult{}, err
	}
	if _, err := client.GetSnapshot(ctx, cfg.Morph.Snapshot); err != nil {
		if isMorphNotFound(err) {
			return DoctorResult{}, exit(2, "provider=morph snapshot %q not found", cfg.Morph.Snapshot)
		}
		return DoctorResult{}, exit(1, "provider=morph snapshot lookup failed: %v", err)
	}
	instances, err := b.listInstances(ctx, client, false)
	if err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Checks: []DoctorCheck{{
			Status:  "ok",
			Check:   "provider",
			Message: fmt.Sprintf("provider=%s auth=ready control_plane=ready inventory=ready snapshot=ready api=list,get_snapshot mutation=false leases=%d runtime=unchecked", providerName, len(instances)),
			Details: map[string]string{
				"provider":      providerName,
				"auth":          "ready",
				"control_plane": "ready",
				"inventory":     "ready",
				"snapshot":      "ready",
				"api":           "list,get_snapshot",
				"mutation":      "false",
				"leases":        strconv.Itoa(len(instances)),
				"runtime":       "unchecked",
			},
		}},
	}, nil
}

func (b *morphLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	if err := validateMorphCreateConfig(cfg); err != nil {
		return LeaseTarget{}, err
	}
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	if _, err := client.GetSnapshot(ctx, cfg.Morph.Snapshot); err != nil {
		if isMorphNotFound(err) {
			return LeaseTarget{}, exit(2, "provider=morph snapshot %q not found", cfg.Morph.Snapshot)
		}
		return LeaseTarget{}, exit(1, "morph snapshot lookup failed: %v", err)
	}
	instances, err := b.listInstances(ctx, client, false)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	slug, err := allocateDirectLeaseSlug(leaseID, req.RequestedSlug, serversFromLeaseViews(instances, cfg))
	if err != nil {
		return LeaseTarget{}, err
	}
	now := b.now().UTC()
	labels := morphLeaseMetadata(cfg, morphInstance{
		Refs: morphInstanceRefs{SnapshotID: cfg.Morph.Snapshot},
	}, leaseID, slug, "", req.Keep, now, false)
	bootReq := morphBootSnapshotRequest{
		Metadata:  labels,
		TTLAction: morphTTLAction(cfg),
	}
	if ttlSeconds := morphTTLSecondsFromLabels(labels, now); ttlSeconds > 0 {
		bootReq.TTLSeconds = &ttlSeconds
	}
	instance, err := client.BootSnapshot(ctx, cfg.Morph.Snapshot, bootReq)
	if err != nil {
		return LeaseTarget{}, exit(1, "morph boot snapshot %q failed: %v", cfg.Morph.Snapshot, err)
	}
	createdLease := LeaseTarget{LeaseID: leaseID, Server: Server{CloudID: instance.ID}}
	cleanupCreated := func() {
		removeStoredTestboxKey(leaseID)
		timeout := b.rollbackTimeout
		if timeout <= 0 {
			timeout = morphAcquireRollbackTimeout
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if cleanupErr := client.DeleteInstance(cleanupCtx, instance.ID); cleanupErr != nil && !isMorphNotFound(cleanupErr) && b.rt.Stderr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: failed to delete morph instance %s after acquire error: %v\n", instance.ID, cleanupErr)
		}
	}
	labels = morphLeaseMetadata(cfg, instance, leaseID, slug, "", req.Keep, now, false)
	if err := client.SetInstanceMetadata(ctx, instance.ID, labels); err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return LeaseTarget{}, exit(1, "morph set metadata for %s failed: %v", instance.ID, err)
	}
	if ttlSeconds := morphTTLSecondsFromLabels(labels, b.now().UTC()); ttlSeconds > 0 {
		if err := client.UpdateInstanceTTL(ctx, instance.ID, ttlSeconds, morphTTLAction(cfg)); err != nil {
			if !req.Keep {
				cleanupCreated()
			}
			return LeaseTarget{}, exit(1, "morph update ttl for %s failed: %v", instance.ID, err)
		}
	}
	if err := client.UpdateInstanceWakeOn(ctx, instance.ID, boolPtr(cfg.Morph.WakeOnSSH), nil); err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return LeaseTarget{}, exit(1, "morph update wake-on for %s failed: %v", instance.ID, err)
	}
	instance, err = b.waitForInstanceReady(ctx, client, instance.ID, false)
	if err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return LeaseTarget{}, err
	}
	instance.Metadata = labels
	target, err := b.resolveSSHTarget(ctx, cfg, client, leaseID, instance, true)
	if err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return LeaseTarget{}, err
	}
	server := morphServer(instance, cfg, leaseID, slug)
	createdLease.Server = server
	createdLease.SSH = target
	return createdLease, nil
}

func (b *morphLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return LeaseTarget{}, err
	}
	instance, leaseID, slug, err := b.resolveInstance(ctx, client, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	server := morphServer(instance, cfg, leaseID, slug)
	if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
		return LeaseTarget{LeaseID: leaseID, Server: server}, nil
	}
	needsReady := !req.StatusOnly || req.ReadyProbe
	if needsReady {
		for !morphInstanceReady(instance) {
			if morphInstancePaused(instance) {
				if cfg.Morph.WakeOnSSH {
					if !instance.WakeOn.WakeOnSSH {
						if err := client.UpdateInstanceWakeOn(ctx, instance.ID, boolPtr(true), nil); err != nil {
							return LeaseTarget{}, exit(1, "morph enable wake-on-ssh for %s failed: %v", instance.ID, err)
						}
						instance.WakeOn.WakeOnSSH = true
					}
					break
				}
				if err := client.ResumeInstance(ctx, instance.ID); err != nil && !isMorphNotFound(err) {
					return LeaseTarget{}, exit(1, "morph resume instance %s failed: %v", instance.ID, err)
				}
				instance, err = b.waitForInstanceReady(ctx, client, instance.ID, false)
			} else {
				instance, err = b.waitForInstanceReady(ctx, client, instance.ID, true)
			}
			if err != nil {
				return LeaseTarget{}, err
			}
		}
	}
	server = morphServer(instance, cfg, leaseID, slug)
	target, err := b.resolveSSHTarget(ctx, cfg, client, leaseID, instance, needsReady)
	if err != nil {
		return LeaseTarget{}, err
	}
	if needsReady && morphInstancePaused(instance) && cfg.Morph.WakeOnSSH {
		if refreshed, refreshErr := client.GetInstance(ctx, instance.ID); refreshErr == nil {
			instance = refreshed
			server = morphServer(instance, cfg, leaseID, slug)
		}
	}
	return LeaseTarget{LeaseID: leaseID, Server: server, SSH: target}, nil
}

func (b *morphLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	instances, err := b.listInstances(ctx, client, req.All)
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(instances))
	for _, instance := range instances {
		leaseID := strings.TrimSpace(instance.Metadata["lease"])
		slug := strings.TrimSpace(instance.Metadata["slug"])
		views = append(views, morphServer(instance, cfg, leaseID, slug))
	}
	sort.Slice(views, func(i, j int) bool {
		left := blank(views[i].Labels["lease"], views[i].CloudID)
		right := blank(views[j].Labels["lease"], views[j].CloudID)
		return left < right
	})
	return views, nil
}

func (b *morphLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return err
	}
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	instanceID := strings.TrimSpace(req.Lease.Server.CloudID)
	if instanceID == "" {
		instanceID = strings.TrimSpace(req.Lease.Server.Labels["instance_id"])
	}
	var instance morphInstance
	if instanceID != "" {
		instance, err = client.GetInstance(ctx, instanceID)
		if err != nil && !isMorphNotFound(err) {
			return exit(1, "morph get instance %s failed: %v", instanceID, err)
		}
		if instance.ID != "" && !morphIsManaged(instance) {
			return exit(4, "morph instance %s is not managed by Crabbox", instance.ID)
		}
	}
	if instance.ID == "" {
		resolveID := blank(leaseID, instanceID)
		if resolveID != "" {
			resolved, resolvedLeaseID, _, resolveErr := b.resolveInstance(ctx, client, resolveID)
			if resolveErr != nil {
				var exitErr ExitError
				if asExitError(resolveErr, &exitErr) && exitErr.Code == 4 {
					removeLeaseClaim(leaseID)
					removeStoredTestboxKey(blank(leaseID, resolveID))
					return nil
				}
				return resolveErr
			}
			instance = resolved
			if leaseID == "" {
				leaseID = resolvedLeaseID
			}
		}
	}
	if instance.ID == "" {
		removeLeaseClaim(leaseID)
		removeStoredTestboxKey(blank(leaseID, instanceID))
		return nil
	}
	if cfg.Morph.DeleteOnRelease {
		if err := client.DeleteInstance(ctx, instance.ID); err != nil && !isMorphNotFound(err) {
			return exit(1, "morph delete instance %s failed: %v", instance.ID, err)
		}
	} else if !morphInstancePaused(instance) {
		if err := client.PauseInstance(ctx, instance.ID); err != nil && !isMorphNotFound(err) {
			return exit(1, "morph pause instance %s failed: %v", instance.ID, err)
		}
	}
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(blank(leaseID, instance.ID))
	return nil
}

func (b *morphLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	instance := blank(blank(lease.Server.CloudID, lease.Server.Labels["instance_id"]), "-")
	if b.configForRun().Morph.DeleteOnRelease {
		return fmt.Sprintf("deleted lease=%s instance=%s", lease.LeaseID, instance)
	}
	return fmt.Sprintf("paused lease=%s instance=%s retained=true", lease.LeaseID, instance)
}

func (b *morphLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	cfg := b.configForRun()
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	client, err := b.api()
	if err != nil {
		return Server{}, err
	}
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	slug := strings.TrimSpace(req.Lease.Server.Labels["slug"])
	instanceID := strings.TrimSpace(req.Lease.Server.CloudID)
	var instance morphInstance
	if instanceID != "" {
		instance, err = client.GetInstance(ctx, instanceID)
		if err != nil {
			if isMorphNotFound(err) {
				return Server{}, exit(4, "lease %s not found for provider=%s", blank(leaseID, instanceID), providerName)
			}
			return Server{}, exit(1, "morph get instance %s failed: %v", instanceID, err)
		}
		if !morphIsManaged(instance) {
			return Server{}, exit(4, "morph instance %s is not managed by Crabbox", instance.ID)
		}
	} else {
		instance, leaseID, slug, err = b.resolveInstance(ctx, client, blank(leaseID, slug))
		if err != nil {
			return Server{}, err
		}
	}
	if instance.Metadata == nil {
		instance.Metadata = morphMetadata{}
	}
	if req.IdleTimeout > 0 {
		instance.Metadata["idle_timeout"] = strconv.Itoa(int(req.IdleTimeout.Seconds()))
		instance.Metadata["idle_timeout_secs"] = strconv.Itoa(int(req.IdleTimeout.Seconds()))
	}
	labels := morphLeaseMetadata(cfg, instance, blank(leaseID, instance.ID), slug, req.State, false, b.now().UTC(), true)
	if err := client.SetInstanceMetadata(ctx, instance.ID, labels); err != nil {
		return Server{}, exit(1, "morph set metadata for %s failed: %v", instance.ID, err)
	}
	if ttlSeconds := morphTTLSecondsFromLabels(labels, b.now().UTC()); ttlSeconds > 0 {
		if err := client.UpdateInstanceTTL(ctx, instance.ID, ttlSeconds, morphTTLAction(cfg)); err != nil {
			return Server{}, exit(1, "morph update ttl for %s failed: %v", instance.ID, err)
		}
	}
	if err := client.UpdateInstanceWakeOn(ctx, instance.ID, boolPtr(cfg.Morph.WakeOnSSH), nil); err != nil {
		return Server{}, exit(1, "morph update wake-on for %s failed: %v", instance.ID, err)
	}
	instance.Metadata = labels
	return morphServer(instance, cfg, blank(leaseID, instance.ID), slug), nil
}

func (b *morphLeaseBackend) api() (morphAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newMorphClient(b.configForRun(), b.rt)
}

func (b *morphLeaseBackend) configForRun() Config {
	cfg := b.cfg
	applyMorphDefaults(&cfg)
	return cfg
}

func (b *morphLeaseBackend) resolveInstance(ctx context.Context, client morphAPI, identifier string) (morphInstance, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return morphInstance{}, "", "", exit(2, "missing lease identifier for provider=%s", providerName)
	}
	claim, claimed, err := resolveLeaseClaimForProvider(identifier, providerName)
	if err != nil {
		return morphInstance{}, "", "", err
	}
	for _, candidate := range []string{strings.TrimSpace(claim.Labels["instance_id"]), identifier} {
		if candidate == "" {
			continue
		}
		instance, err := client.GetInstance(ctx, candidate)
		if err == nil {
			if !morphIsManaged(instance) {
				return morphInstance{}, "", "", exit(4, "morph instance %s is not managed by Crabbox", instance.ID)
			}
			instance, leaseID, slug := finalizeMorphResolution(instance, identifier, claim, claimed)
			return instance, leaseID, slug, nil
		}
		if !isMorphNotFound(err) {
			return morphInstance{}, "", "", exit(1, "morph get instance %s failed: %v", candidate, err)
		}
	}
	instances, err := b.listInstances(ctx, client, false)
	if err != nil {
		return morphInstance{}, "", "", err
	}
	wantSlug := normalizeLeaseSlug(identifier)
	var matched *morphInstance
	for i := range instances {
		labels := instances[i].Metadata
		if labels["lease"] == identifier ||
			(claimed && labels["lease"] == claim.LeaseID) ||
			labels["slug"] == wantSlug ||
			labels["instance_id"] == identifier ||
			instances[i].ID == identifier {
			if matched != nil {
				return morphInstance{}, "", "", exit(5, "lease %s matched multiple morph instances", identifier)
			}
			matched = &instances[i]
		}
	}
	if matched == nil {
		return morphInstance{}, "", "", exit(4, "lease %s not found for provider=%s", identifier, providerName)
	}
	instance, leaseID, slug := finalizeMorphResolution(*matched, identifier, claim, claimed)
	return instance, leaseID, slug, nil
}

func (b *morphLeaseBackend) listInstances(ctx context.Context, client morphAPI, all bool) ([]morphInstance, error) {
	filter := map[string]string(nil)
	if !all {
		filter = morphManagedFilter()
	}
	instances, err := client.ListInstances(ctx, filter)
	if err != nil {
		return nil, exit(1, "morph list instances failed: %v", err)
	}
	if all {
		return instances, nil
	}
	filtered := make([]morphInstance, 0, len(instances))
	for _, instance := range instances {
		if morphIsManaged(instance) {
			filtered = append(filtered, instance)
		}
	}
	return filtered, nil
}

func (b *morphLeaseBackend) waitForInstanceReady(ctx context.Context, client morphAPI, instanceID string, allowPaused bool) (morphInstance, error) {
	waitCtx := ctx
	cancel := func() {}
	if b.readyTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, b.readyTimeout)
	}
	defer cancel()
	for {
		instance, err := client.GetInstance(waitCtx, instanceID)
		if err != nil {
			if isMorphNotFound(err) {
				return morphInstance{}, exit(4, "morph instance %s disappeared while waiting for readiness", instanceID)
			}
			return morphInstance{}, exit(1, "morph get instance %s failed: %v", instanceID, err)
		}
		if morphInstanceReady(instance) {
			return instance, nil
		}
		if allowPaused && morphInstancePaused(instance) {
			return instance, nil
		}
		if morphInstanceTerminal(instance) {
			return morphInstance{}, exit(1, "morph instance %s entered terminal state %q", instanceID, blank(instance.Status, "unknown"))
		}
		select {
		case <-waitCtx.Done():
			if waitCtx.Err() == context.DeadlineExceeded {
				return morphInstance{}, exit(5, "timed out waiting for morph instance %s to become ready", instanceID)
			}
			return morphInstance{}, waitCtx.Err()
		case <-time.After(b.readyPollInterval):
		}
	}
}

func (b *morphLeaseBackend) resolveSSHTarget(ctx context.Context, cfg Config, client morphAPI, leaseID string, instance morphInstance, waitReady bool) (SSHTarget, error) {
	sshKey, err := client.GetSSHKey(ctx, instance.ID)
	if err != nil {
		return SSHTarget{}, exit(1, "morph get ssh key for %s failed: %v", instance.ID, err)
	}
	keyPath, err := storeMorphSSHKey(leaseID, sshKey)
	if err != nil {
		return SSHTarget{}, err
	}
	knownHostsPath, err := ensureMorphKnownHostsPath()
	if err != nil {
		return SSHTarget{}, err
	}
	target := morphSSHTarget(cfg, instance, keyPath, knownHostsPath)
	if waitReady {
		if err := waitForMorphSSHReady(ctx, &target, b.rt.Stderr, "morph ssh", bootstrapWaitTimeout(cfg)); err != nil {
			return SSHTarget{}, err
		}
	}
	return target, nil
}

func applyMorphDefaults(cfg *Config) {
	cfg.Provider = providerName
	if strings.TrimSpace(cfg.TargetOS) == "" {
		cfg.TargetOS = targetLinux
	}
	if strings.TrimSpace(cfg.Morph.APIURL) == "" {
		cfg.Morph.APIURL = "https://cloud.morph.so"
	}
	if strings.TrimSpace(cfg.Morph.SSHGatewayHost) == "" {
		cfg.Morph.SSHGatewayHost = "ssh.cloud.morph.so"
	}
	if strings.TrimSpace(cfg.Morph.WorkRoot) == "" {
		if isDefaultWorkRoot(cfg.WorkRoot) || strings.TrimSpace(cfg.WorkRoot) == "" {
			cfg.Morph.WorkRoot = defaultMorphWorkRoot
		} else {
			cfg.Morph.WorkRoot = cfg.WorkRoot
		}
	}
	cfg.WorkRoot = cfg.Morph.WorkRoot
	if cfg.Network == "" || cfg.Network == networkAuto || cfg.Network == networkPublic {
		cfg.Network = networkPublic
	}
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.ServerType = blank(strings.TrimSpace(cfg.Morph.Snapshot), "snapshot")
}

func validateMorphConfig(cfg Config) error {
	if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
		return exit(2, "provider=morph supports target=linux only")
	}
	if cfg.Network == networkTailscale {
		return exit(2, "--network=tailscale is not supported for provider=morph; Morph exposes SSH through the public gateway")
	}
	if cfg.Tailscale.Enabled {
		return exit(2, "--tailscale is not supported for provider=morph; Morph exposes SSH through the public gateway")
	}
	return nil
}

func validateMorphCreateConfig(cfg Config) error {
	if err := validateMorphConfig(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Morph.Snapshot) == "" {
		return exit(2, "provider=morph requires CRABBOX_MORPH_SNAPSHOT or morph.snapshot")
	}
	return nil
}

func morphManagedFilter() map[string]string {
	return map[string]string{
		"crabbox":  "true",
		"provider": providerName,
	}
}

func morphIsManaged(instance morphInstance) bool {
	return strings.EqualFold(strings.TrimSpace(instance.Metadata["crabbox"]), "true") &&
		strings.EqualFold(strings.TrimSpace(instance.Metadata["provider"]), providerName)
}

func morphLeaseMetadata(cfg Config, instance morphInstance, leaseID, slug, state string, keep bool, now time.Time, touch bool) map[string]string {
	existingWorkRoot := strings.TrimSpace(instance.Metadata["work_root"])
	var labels map[string]string
	if touch {
		labels = touchDirectLeaseLabels(instance.Metadata.Clone(), cfg, state, now)
		if strings.EqualFold(strings.TrimSpace(state), "running") {
			if expiresAt := morphLeaseTTLCap(labels); expiresAt != "" {
				labels["expires_at"] = expiresAt
			}
		}
	} else {
		labels = directLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	}
	labels["crabbox"] = "true"
	labels["provider"] = providerName
	if leaseID != "" {
		labels["lease"] = leaseID
	}
	if slug != "" {
		labels["slug"] = normalizeLeaseSlug(slug)
	}
	if root := strings.TrimSpace(cfg.WorkRoot); root != "" {
		switch {
		case !touch, existingWorkRoot == "", existingWorkRoot == root, !isDefaultMorphWorkRoot(root):
			labels["work_root"] = root
		default:
			labels["work_root"] = existingWorkRoot
		}
	}
	if instance.ID != "" {
		labels["instance_id"] = instance.ID
		labels["ssh_user"] = instance.ID
	}
	labels["ssh_port"] = "22"
	snapshotID := strings.TrimSpace(instance.Refs.SnapshotID)
	if snapshotID == "" {
		snapshotID = strings.TrimSpace(labels["snapshot_id"])
	}
	if snapshotID == "" && !touch {
		snapshotID = strings.TrimSpace(cfg.Morph.Snapshot)
	}
	if snapshotID != "" {
		labels["snapshot_id"] = snapshotID
	}
	if touch && snapshotID != "" {
		labels["server_type"] = snapshotID
	} else if !touch && cfg.ServerType != "" {
		labels["server_type"] = cfg.ServerType
	} else if strings.TrimSpace(labels["server_type"]) == "" && snapshotID != "" {
		labels["server_type"] = snapshotID
	}
	if name := leaseProviderName(blank(leaseID, instance.ID), slug); name != "" {
		labels["lease_name"] = name
	}
	return labels
}

func morphServer(instance morphInstance, cfg Config, leaseID, slug string) Server {
	labels := instance.Metadata.Clone()
	if leaseID == "" {
		leaseID = blank(strings.TrimSpace(labels["lease"]), instance.ID)
	}
	if slug == "" {
		slug = strings.TrimSpace(labels["slug"])
	}
	if leaseID != "" {
		labels["lease"] = leaseID
	}
	if slug != "" {
		labels["slug"] = normalizeLeaseSlug(slug)
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["instance_id"] == "" && instance.ID != "" {
		labels["instance_id"] = instance.ID
	}
	if labels["work_root"] == "" && cfg.WorkRoot != "" {
		labels["work_root"] = cfg.WorkRoot
	}
	if labels["snapshot_id"] == "" && instance.Refs.SnapshotID != "" {
		labels["snapshot_id"] = instance.Refs.SnapshotID
	}
	if instance.Networking.Hostname != "" {
		labels["morph_hostname"] = instance.Networking.Hostname
	}
	if instance.Networking.ExternalIP != "" {
		labels["morph_external_ip"] = instance.Networking.ExternalIP
	}
	if instance.Networking.InternalIP != "" {
		labels["morph_internal_ip"] = instance.Networking.InternalIP
	}
	state := morphLeaseState(instance)
	if morphInstanceReady(instance) {
		switch runtimeState := strings.ToLower(strings.TrimSpace(labels["state"])); runtimeState {
		case "running", "active":
			state = runtimeState
		}
	}
	if state != "" {
		labels["state"] = state
	}
	server := Server{
		CloudID:  instance.ID,
		Provider: providerName,
		Name:     blank(labels["lease_name"], instance.ID),
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = blank(labels["server_type"], blank(labels["snapshot_id"], blank(cfg.ServerType, "snapshot")))
	server.PublicNet.IPv4.IP = blank(strings.TrimSpace(cfg.Morph.SSHGatewayHost), "ssh.cloud.morph.so")
	return server
}

func morphSSHTarget(cfg Config, instance morphInstance, keyPath, knownHostsPath string) SSHTarget {
	target := sshTargetFromConfig(cfg, blank(strings.TrimSpace(cfg.Morph.SSHGatewayHost), "ssh.cloud.morph.so"))
	target.Host = blank(strings.TrimSpace(cfg.Morph.SSHGatewayHost), "ssh.cloud.morph.so")
	target.Port = "22"
	target.User = instance.ID
	target.Key = keyPath
	target.KnownHostsFile = knownHostsPath
	target.TargetOS = targetLinux
	target.NetworkKind = networkPublic
	target.ReadyCheck = morphReadyCheck
	target.FallbackPorts = nil
	return target
}

func storeMorphSSHKey(leaseID string, sshKey morphSSHKey) (string, error) {
	privateKey := strings.TrimSpace(sshKey.PrivateKey)
	if privateKey == "" {
		return "", exit(1, "morph ssh key response did not include private_key")
	}
	keyData := []byte(privateKey + "\n")
	if sshKey.Password != "" {
		if _, err := ssh.ParseRawPrivateKey(keyData); err != nil {
			password := []byte(sshKey.Password)
			decryptedKey, decryptErr := ssh.ParseRawPrivateKeyWithPassphrase(keyData, password)
			clear(password)
			if decryptErr != nil {
				return "", exit(1, "morph ssh key response could not be decrypted")
			}
			block, marshalErr := ssh.MarshalPrivateKey(decryptedKey, "")
			if marshalErr != nil {
				return "", exit(1, "morph ssh key response could not be stored: %v", marshalErr)
			}
			keyData = pem.EncodeToMemory(block)
		}
	}
	path, err := testboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, keyData, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func ensureMorphKnownHostsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user config directory is unavailable")
	}
	path := filepath.Join(configDir, "crabbox", providerName, "known_hosts")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func serversFromLeaseViews(instances []morphInstance, cfg Config) []Server {
	servers := make([]Server, 0, len(instances))
	for _, instance := range instances {
		servers = append(servers, morphServer(instance, cfg, strings.TrimSpace(instance.Metadata["lease"]), strings.TrimSpace(instance.Metadata["slug"])))
	}
	return servers
}

func finalizeMorphResolution(instance morphInstance, requested string, claim LeaseClaim, claimed bool) (morphInstance, string, string) {
	leaseID := strings.TrimSpace(instance.Metadata["lease"])
	slug := strings.TrimSpace(instance.Metadata["slug"])
	if claimed {
		if leaseID == "" {
			leaseID = claim.LeaseID
		}
		if slug == "" {
			slug = claim.Slug
		}
	}
	if leaseID == "" {
		if isCanonicalLeaseID(requested) {
			leaseID = requested
		} else {
			leaseID = instance.ID
		}
	}
	return instance, leaseID, slug
}

func morphLeaseState(instance morphInstance) string {
	switch strings.ToLower(strings.TrimSpace(instance.Status)) {
	case "", "unknown":
		return "provisioning"
	case "ready", "running":
		return "ready"
	default:
		return strings.ToLower(strings.TrimSpace(instance.Status))
	}
}

func morphInstanceReady(instance morphInstance) bool {
	switch strings.ToLower(strings.TrimSpace(instance.Status)) {
	case "ready", "running":
		return true
	default:
		return false
	}
}

func morphInstancePaused(instance morphInstance) bool {
	return strings.EqualFold(strings.TrimSpace(instance.Status), "paused")
}

func morphInstanceTerminal(instance morphInstance) bool {
	switch strings.ToLower(strings.TrimSpace(instance.Status)) {
	case "deleted", "failed", "stopped", "terminated", "error":
		return true
	default:
		return false
	}
}

func morphTTLAction(cfg Config) string {
	if cfg.Morph.DeleteOnRelease {
		return "stop"
	}
	return "pause"
}

func morphTTLSecondsFromLabels(labels map[string]string, now time.Time) int {
	expiresAt := strings.TrimSpace(labels["expires_at"])
	if expiresAt == "" {
		return 0
	}
	unixSeconds, err := strconv.ParseInt(expiresAt, 10, 64)
	if err != nil {
		return 0
	}
	remaining := unixSeconds - now.Unix()
	if remaining <= 0 {
		return 1
	}
	return int(remaining)
}

func morphLeaseTTLCap(labels map[string]string) string {
	createdAt, err := strconv.ParseInt(strings.TrimSpace(labels["created_at"]), 10, 64)
	if err != nil {
		return ""
	}
	ttlSeconds, err := strconv.ParseInt(strings.TrimSpace(labels["ttl_secs"]), 10, 64)
	if err != nil || ttlSeconds <= 0 {
		return ""
	}
	return strconv.FormatInt(createdAt+ttlSeconds, 10)
}

func boolPtr(value bool) *bool {
	return &value
}

func isDefaultMorphWorkRoot(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == defaultMorphWorkRoot || isDefaultWorkRoot(value)
}
