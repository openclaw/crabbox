package morph

import (
	"context"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"golang.org/x/crypto/ssh"
)

const morphReadyCheck = "command -v bash >/dev/null && command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && (command -v python3 >/dev/null || command -v python >/dev/null || command -v perl >/dev/null)"
const morphAcquireRollbackTimeout = 30 * time.Second

var waitForMorphSSHReady = core.WaitForSSHReady

type morphLeaseBackend struct {
	spec              core.ProviderSpec
	cfg               core.Config
	rt                core.Runtime
	client            morphAPI
	now               func() time.Time
	readyPollInterval time.Duration
	readyTimeout      time.Duration
	rollbackTimeout   time.Duration
}

func RegisterMorphProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterMorphConfigFlags(fs, defaults.Morph)
}

func ApplyMorphProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if isMorphProviderName(cfg.Provider) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", "use --morph-snapshot"); err != nil {
			return err
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return core.Exit(2, "provider=morph supports target=linux only")
		}
	}
	v, ok := values.(core.MorphConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Morph, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.DeleteOnRelease {
		markDeleteOnReleaseExplicit(cfg)
	}
	if err != nil {
		return err
	}
	if isMorphProviderName(cfg.Provider) {
		applyMorphDefaults(cfg)
		return validateMorphConfig(*cfg)
	}
	return nil
}

func NewMorphBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) (core.Backend, error) {
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

func (b *morphLeaseBackend) Spec() core.ProviderSpec { return b.spec }

func (b *morphLeaseBackend) RebindResolvedLeaseTarget(target *core.LeaseTarget, leaseID string) error {
	return core.UseStoredTestboxKey(&target.SSH, leaseID)
}

func (b *morphLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	if err := validateMorphCreateConfig(cfg); err != nil {
		return core.DoctorResult{}, err
	}
	client, err := b.api()
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.GetSnapshot(ctx, cfg.Morph.Snapshot); err != nil {
		if isMorphNotFound(err) {
			return core.DoctorResult{}, core.Exit(2, "provider=morph snapshot %q not found", cfg.Morph.Snapshot)
		}
		return core.DoctorResult{}, core.Exit(1, "provider=morph snapshot lookup failed: %v", err)
	}
	instances, err := b.listInstances(ctx, client, false)
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.DoctorResult{
		Provider: providerName,
		Checks: []core.DoctorCheck{{
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

func (b *morphLeaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := validateMorphCreateConfig(cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if _, err := client.GetSnapshot(ctx, cfg.Morph.Snapshot); err != nil {
		if isMorphNotFound(err) {
			return core.LeaseTarget{}, core.Exit(2, "provider=morph snapshot %q not found", cfg.Morph.Snapshot)
		}
		return core.LeaseTarget{}, core.Exit(1, "morph snapshot lookup failed: %v", err)
	}
	instances, err := b.listInstances(ctx, client, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	if _, err := core.PrepareStoredTestboxKeyPath(leaseID); err != nil {
		return core.LeaseTarget{}, err
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, serversFromLeaseViews(instances, cfg))
	if err != nil {
		return core.LeaseTarget{}, err
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
		return core.LeaseTarget{}, core.Exit(1, "morph boot snapshot %q failed: %v", cfg.Morph.Snapshot, err)
	}
	createdLease := core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: instance.ID}}
	cleanupCreated := func() {
		core.RemoveStoredTestboxKey(leaseID)
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
		return core.LeaseTarget{}, core.Exit(1, "morph set metadata for %s failed: %v", instance.ID, err)
	}
	if ttlSeconds := morphTTLSecondsFromLabels(labels, b.now().UTC()); ttlSeconds > 0 {
		if err := client.UpdateInstanceTTL(ctx, instance.ID, ttlSeconds, morphTTLAction(cfg)); err != nil {
			if !req.Keep {
				cleanupCreated()
			}
			return core.LeaseTarget{}, core.Exit(1, "morph update ttl for %s failed: %v", instance.ID, err)
		}
	}
	if err := client.UpdateInstanceWakeOn(ctx, instance.ID, boolPtr(cfg.Morph.WakeOnSSH), nil); err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return core.LeaseTarget{}, core.Exit(1, "morph update wake-on for %s failed: %v", instance.ID, err)
	}
	instance, err = b.waitForInstanceReady(ctx, client, instance.ID, false)
	if err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return core.LeaseTarget{}, err
	}
	instance.Metadata = labels
	target, err := b.resolveSSHTarget(ctx, cfg, client, leaseID, instance, true)
	if err != nil {
		if !req.Keep {
			cleanupCreated()
		}
		return core.LeaseTarget{}, err
	}
	server := morphServer(instance, cfg, leaseID, slug)
	createdLease.Server = server
	createdLease.SSH = target
	var claimErr error
	if req.Repo.Root == "" {
		claimErr = core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, target, cfg.IdleTimeout)
	} else {
		claimErr = core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, req.Repo.Root, cfg.IdleTimeout, req.Reclaim)
	}
	if claimErr != nil {
		cleanupCreated()
		return core.LeaseTarget{}, fmt.Errorf("persist exact morph instance ownership claim: %w", claimErr)
	}
	return createdLease, nil
}

func (b *morphLeaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (lease core.LeaseTarget, err error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	instance, leaseID, slug, err := b.resolveInstance(ctx, client, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := morphServer(instance, cfg, leaseID, slug)
	if req.ReleaseOnly || (req.StatusOnly && !req.ReadyProbe) {
		return core.LeaseTarget{LeaseID: leaseID, Server: server}, nil
	}
	if _, err := core.PrepareStoredTestboxKeyPath(leaseID); err != nil {
		return core.LeaseTarget{}, err
	}
	var previousClaim, preflightClaim core.LeaseClaim
	var previousClaimExists, rollbackClaim bool
	defer func() {
		if err == nil || !rollbackClaim {
			return
		}
		if restoreErr := core.RestoreLeaseClaimIfUnchanged(leaseID, preflightClaim, previousClaim, previousClaimExists); restoreErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: restore Morph lease claim %s after resolve failure: %v\n", leaseID, restoreErr)
		}
	}()
	if req.Repo.Root != "" {
		previousClaim, previousClaimExists, err = core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		preflightClaim, err = claimLeaseForRepoProviderIfUnchanged(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, previousClaim, previousClaimExists)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		rollbackClaim = true
	}
	needsReady := !req.StatusOnly || req.ReadyProbe
	if needsReady {
		for !morphInstanceReady(instance) {
			if morphInstancePaused(instance) {
				if cfg.Morph.WakeOnSSH {
					if !instance.WakeOn.WakeOnSSH {
						if err := client.UpdateInstanceWakeOn(ctx, instance.ID, boolPtr(true), nil); err != nil {
							return core.LeaseTarget{}, core.Exit(1, "morph enable wake-on-ssh for %s failed: %v", instance.ID, err)
						}
						instance.WakeOn.WakeOnSSH = true
					}
					break
				}
				if err := client.ResumeInstance(ctx, instance.ID); err != nil && !isMorphNotFound(err) {
					return core.LeaseTarget{}, core.Exit(1, "morph resume instance %s failed: %v", instance.ID, err)
				}
				instance, err = b.waitForInstanceReady(ctx, client, instance.ID, false)
			} else {
				instance, err = b.waitForInstanceReady(ctx, client, instance.ID, true)
			}
			if err != nil {
				return core.LeaseTarget{}, err
			}
		}
	}
	server = morphServer(instance, cfg, leaseID, slug)
	target, err := b.resolveSSHTarget(ctx, cfg, client, leaseID, instance, needsReady)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if needsReady && morphInstancePaused(instance) && cfg.Morph.WakeOnSSH {
		if refreshed, refreshErr := client.GetInstance(ctx, instance.ID); refreshErr == nil {
			instance = refreshed
			server = morphServer(instance, cfg, leaseID, slug)
		}
	}
	if req.Repo.Root != "" {
		if _, err = core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, preflightClaim, server, target); err != nil {
			return core.LeaseTarget{}, err
		}
		rollbackClaim = false
	}
	return core.LeaseTarget{LeaseID: leaseID, Server: server, SSH: target}, nil
}

func (b *morphLeaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return nil, err
	}
	instances, err := b.listInstances(ctx, client, req.All)
	if err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0, len(instances))
	for _, instance := range instances {
		leaseID := strings.TrimSpace(instance.Metadata["lease"])
		slug := strings.TrimSpace(instance.Metadata["slug"])
		views = append(views, morphServer(instance, cfg, leaseID, slug))
	}
	sort.Slice(views, func(i, j int) bool {
		left := core.Blank(views[i].Labels["lease"], views[i].CloudID)
		right := core.Blank(views[j].Labels["lease"], views[j].CloudID)
		return left < right
	})
	return views, nil
}

func (b *morphLeaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *morphLeaseBackend) ReleaseLeaseWithOutcome(ctx context.Context, req core.ReleaseLeaseRequest) (core.ReleaseLeaseOutcome, error) {
	var outcome core.ReleaseLeaseOutcome
	err := b.releaseLease(ctx, req, &outcome)
	return outcome, err
}

func (b *morphLeaseBackend) releaseLease(ctx context.Context, req core.ReleaseLeaseRequest, outcome *core.ReleaseLeaseOutcome) error {
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
	removeMissingInstance := func() error {
		claim, ok, claimErr := core.ResolveLeaseClaimForProvider(core.Blank(leaseID, instanceID), providerName)
		if claimErr != nil {
			return claimErr
		}
		if !ok {
			return core.Exit(2, "morph instance %s has no exact local ownership claim", instanceID)
		}
		binding := shared.ClaimBinding{
			Provider: providerName, ProviderScope: Provider{}.ClaimScope(cfg), ExactProviderScope: true,
			LeaseID: claim.LeaseID, CloudID: core.Blank(instanceID, claim.CloudID),
		}
		exact, claimErr := shared.RequireExactClaim(binding)
		if claimErr != nil {
			return claimErr
		}
		if claimErr = shared.RemoveExactClaimAfter(exact, binding, func() error {
			outcome.Terminal = true
			return nil
		}); claimErr != nil {
			return claimErr
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
		return nil
	}
	var instance morphInstance
	if instanceID != "" {
		instance, err = client.GetInstance(ctx, instanceID)
		if err != nil && !isMorphNotFound(err) {
			return core.Exit(1, "morph get instance %s failed: %v", instanceID, err)
		}
		if instance.ID != "" && !morphIsManaged(instance) {
			return core.Exit(4, "morph instance %s is not managed by Crabbox", instance.ID)
		}
	}
	if instance.ID == "" {
		resolveID := core.Blank(leaseID, instanceID)
		if resolveID != "" {
			resolved, resolvedLeaseID, _, resolveErr := b.resolveInstance(ctx, client, resolveID)
			if resolveErr != nil {
				var exitErr core.ExitError
				if core.AsExitError(resolveErr, &exitErr) && exitErr.Code == 4 {
					return removeMissingInstance()
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
		return removeMissingInstance()
	}
	slug := strings.TrimSpace(instance.Metadata["slug"])
	binding := shared.ClaimBinding{
		Provider:           providerName,
		ProviderScope:      Provider{}.ClaimScope(cfg),
		ExactProviderScope: true,
		LeaseID:            leaseID,
		Slug:               slug,
		CloudID:            instance.ID,
		RequiredLabels:     map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "instance_id": instance.ID},
	}
	verifyLiveOwnership := func() error {
		live, err := client.GetInstance(ctx, instance.ID)
		if err != nil {
			return fmt.Errorf("verify live morph instance %s ownership: %w", instance.ID, err)
		}
		if live.ID != instance.ID || !morphIsManaged(live) || live.Metadata["lease"] != leaseID || live.Metadata["slug"] != slug || live.Metadata["instance_id"] != instance.ID {
			return core.Exit(2, "morph instance %s ownership changed before release", instance.ID)
		}
		return nil
	}
	claim, err := shared.RequireExactClaim(binding)
	if err != nil {
		legacy, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
		if readErr != nil {
			return readErr
		}
		if !exists || legacy.ProviderScope != "" {
			return err
		}
		legacyBinding := binding
		legacyBinding.ProviderScope = ""
		legacyBinding.RequiredLabels = map[string]string{"provider": providerName, "lease": leaseID, "slug": slug, "instance_id": instance.ID}
		if shared.ValidateClaimBinding(legacy, legacyBinding) != nil {
			return err
		}
		if verifyErr := verifyLiveOwnership(); verifyErr != nil {
			return verifyErr
		}
		replacement := legacy
		replacement.ProviderScope = binding.ProviderScope
		replacement.Labels = make(map[string]string, len(legacy.Labels)+len(binding.RequiredLabels))
		for key, value := range legacy.Labels {
			replacement.Labels[key] = value
		}
		for key, value := range binding.RequiredLabels {
			replacement.Labels[key] = value
		}
		if replaceErr := core.ReplaceLeaseClaimIfUnchanged(leaseID, legacy, replacement); replaceErr != nil {
			return replaceErr
		}
		claim, err = shared.RequireExactClaim(binding)
		if err != nil {
			return err
		}
	}
	deleteInstance := morphDeleteOnRelease(req.Lease, cfg)
	if deleteInstance {
		if err := shared.RemoveExactClaimAfter(claim, binding, func() error {
			if err := verifyLiveOwnership(); err != nil {
				return err
			}
			if err := client.DeleteInstance(ctx, instance.ID); err != nil && !isMorphNotFound(err) {
				return core.Exit(1, "morph delete instance %s failed: %v", instance.ID, err)
			}
			outcome.Terminal = true
			return nil
		}); err != nil {
			return err
		}
		core.RemoveStoredTestboxKey(core.Blank(leaseID, instance.ID))
		return nil
	}
	wasPaused := morphInstancePaused(instance)
	instance.Status = "paused"
	labels := morphLeaseMetadata(cfg, instance, leaseID, req.Lease.Server.Labels["slug"], "paused", true, b.now().UTC(), true)
	labels["release"] = "pause"
	pauseAndPersist := func() error {
		if err := verifyLiveOwnership(); err != nil {
			return err
		}
		if !wasPaused {
			if err := client.PauseInstance(ctx, instance.ID); err != nil && !isMorphNotFound(err) {
				return core.Exit(1, "morph pause instance %s failed: %v", instance.ID, err)
			}
		}
		if err := client.SetInstanceMetadata(ctx, instance.ID, labels); err != nil {
			return core.Exit(1, "morph persist paused metadata for %s failed: %v", instance.ID, err)
		}
		return nil
	}
	instance.Metadata = labels
	server := morphServer(instance, cfg, leaseID, labels["slug"])
	_, err = core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, core.SSHTarget{}, pauseAndPersist)
	return err
}

func (b *morphLeaseBackend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	instance := core.Blank(core.Blank(lease.Server.CloudID, lease.Server.Labels["instance_id"]), "-")
	if morphDeleteOnRelease(lease, b.configForRun()) {
		return fmt.Sprintf("deleted lease=%s instance=%s", lease.LeaseID, instance)
	}
	return fmt.Sprintf("paused lease=%s instance=%s retained=true", lease.LeaseID, instance)
}

func (b *morphLeaseBackend) RetainLeaseClaimAfterRelease(lease core.LeaseTarget) bool {
	return !morphDeleteOnRelease(lease, b.configForRun())
}

func (b *morphLeaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	cfg := b.configForRun()
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	client, err := b.api()
	if err != nil {
		return core.Server{}, err
	}
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	slug := strings.TrimSpace(req.Lease.Server.Labels["slug"])
	instanceID := strings.TrimSpace(req.Lease.Server.CloudID)
	var instance morphInstance
	if instanceID != "" {
		instance, err = client.GetInstance(ctx, instanceID)
		if err != nil {
			if isMorphNotFound(err) {
				return core.Server{}, core.Exit(4, "lease %s not found for provider=%s", core.Blank(leaseID, instanceID), providerName)
			}
			return core.Server{}, core.Exit(1, "morph get instance %s failed: %v", instanceID, err)
		}
		if !morphIsManaged(instance) {
			return core.Server{}, core.Exit(4, "morph instance %s is not managed by Crabbox", instance.ID)
		}
	} else {
		instance, leaseID, slug, err = b.resolveInstance(ctx, client, core.Blank(leaseID, slug))
		if err != nil {
			return core.Server{}, err
		}
	}
	if instance.Metadata == nil {
		instance.Metadata = morphMetadata{}
	}
	if req.IdleTimeout > 0 {
		instance.Metadata["idle_timeout"] = strconv.Itoa(int(req.IdleTimeout.Seconds()))
		instance.Metadata["idle_timeout_secs"] = strconv.Itoa(int(req.IdleTimeout.Seconds()))
	}
	labels := morphLeaseMetadata(cfg, instance, core.Blank(leaseID, instance.ID), slug, req.State, false, b.now().UTC(), true)
	if err := client.SetInstanceMetadata(ctx, instance.ID, labels); err != nil {
		return core.Server{}, core.Exit(1, "morph set metadata for %s failed: %v", instance.ID, err)
	}
	if ttlSeconds := morphTTLSecondsFromLabels(labels, b.now().UTC()); ttlSeconds > 0 {
		if err := client.UpdateInstanceTTL(ctx, instance.ID, ttlSeconds, morphTTLAction(cfg)); err != nil {
			return core.Server{}, core.Exit(1, "morph update ttl for %s failed: %v", instance.ID, err)
		}
	}
	if err := client.UpdateInstanceWakeOn(ctx, instance.ID, boolPtr(cfg.Morph.WakeOnSSH), nil); err != nil {
		return core.Server{}, core.Exit(1, "morph update wake-on for %s failed: %v", instance.ID, err)
	}
	instance.Metadata = labels
	return morphServer(instance, cfg, core.Blank(leaseID, instance.ID), slug), nil
}

func (b *morphLeaseBackend) api() (morphAPI, error) {
	if b.client != nil {
		return b.client, nil
	}
	return newMorphClient(b.configForRun(), b.rt)
}

func (b *morphLeaseBackend) configForRun() core.Config {
	cfg := b.cfg
	applyMorphDefaults(&cfg)
	return cfg
}

func (b *morphLeaseBackend) resolveInstance(ctx context.Context, client morphAPI, identifier string) (morphInstance, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return morphInstance{}, "", "", core.Exit(2, "missing lease identifier for provider=%s", providerName)
	}
	claim, claimed, err := core.ResolveLeaseClaimForProvider(identifier, providerName)
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
				return morphInstance{}, "", "", core.Exit(4, "morph instance %s is not managed by Crabbox", instance.ID)
			}
			instance, leaseID, slug := finalizeMorphResolution(instance, identifier, claim, claimed)
			return instance, leaseID, slug, nil
		}
		if !isMorphNotFound(err) {
			return morphInstance{}, "", "", core.Exit(1, "morph get instance %s failed: %v", candidate, err)
		}
	}
	instances, err := b.listInstances(ctx, client, false)
	if err != nil {
		return morphInstance{}, "", "", err
	}
	wantSlug := core.NormalizeLeaseSlug(identifier)
	var matched *morphInstance
	for i := range instances {
		labels := instances[i].Metadata
		if labels["lease"] == identifier ||
			(claimed && labels["lease"] == claim.LeaseID) ||
			labels["slug"] == wantSlug ||
			labels["instance_id"] == identifier ||
			instances[i].ID == identifier {
			if matched != nil {
				return morphInstance{}, "", "", core.Exit(5, "lease %s matched multiple morph instances", identifier)
			}
			matched = &instances[i]
		}
	}
	if matched == nil {
		return morphInstance{}, "", "", core.Exit(4, "lease %s not found for provider=%s", identifier, providerName)
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
		return nil, core.Exit(1, "morph list instances failed: %v", err)
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
	waitCtx, cancel := ctx, func() {}
	if b.readyTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, b.readyTimeout)
	}
	defer cancel()
	result, err := shared.Poll(waitCtx, 0, b.readyPollInterval, shared.SleepContext,
		func(ctx context.Context) (morphInstance, error) { return client.GetInstance(ctx, instanceID) },
		func(_ context.Context, instance morphInstance, fetchErr error) (bool, error) {
			switch {
			case isMorphNotFound(fetchErr):
				return false, core.Exit(4, "morph instance %s disappeared while waiting for readiness", instanceID)
			case fetchErr != nil:
				return false, core.Exit(1, "morph get instance %s failed: %v", instanceID, fetchErr)
			case morphInstanceReady(instance) || allowPaused && morphInstancePaused(instance):
				return true, nil
			case morphInstanceTerminal(instance):
				return false, core.Exit(1, "morph instance %s entered terminal state %q", instanceID, core.Blank(instance.Status, "unknown"))
			}
			return false, nil
		}, nil)
	if waitErr := waitCtx.Err(); waitErr != nil && errors.Is(err, context.Cause(waitCtx)) {
		if waitErr == context.DeadlineExceeded {
			return morphInstance{}, core.Exit(5, "timed out waiting for morph instance %s to become ready", instanceID)
		}
		return morphInstance{}, waitErr
	}
	if err == nil {
		return result.Value, nil
	}
	return morphInstance{}, err
}

func (b *morphLeaseBackend) resolveSSHTarget(ctx context.Context, cfg core.Config, client morphAPI, leaseID string, instance morphInstance, waitReady bool) (core.SSHTarget, error) {
	sshKey, err := client.GetSSHKey(ctx, instance.ID)
	if err != nil {
		return core.SSHTarget{}, core.Exit(1, "morph get ssh key for %s failed: %v", instance.ID, err)
	}
	keyPath, err := storeMorphSSHKey(leaseID, sshKey)
	if err != nil {
		return core.SSHTarget{}, err
	}
	knownHostsPath, err := ensureMorphKnownHostsPath()
	if err != nil {
		return core.SSHTarget{}, err
	}
	target := morphSSHTarget(cfg, instance, keyPath, knownHostsPath)
	if waitReady {
		if err := waitForMorphSSHReady(ctx, &target, b.rt.Stderr, "morph ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.SSHTarget{}, err
		}
	}
	return target, nil
}

func applyMorphDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if strings.TrimSpace(cfg.TargetOS) == "" {
		cfg.TargetOS = targetLinux
	}
	if strings.TrimSpace(cfg.Morph.APIURL) == "" {
		cfg.Morph.APIURL = core.MorphConfigDefaultAPIURL
	}
	if strings.TrimSpace(cfg.Morph.SSHGatewayHost) == "" {
		cfg.Morph.SSHGatewayHost = core.MorphConfigDefaultSSHGatewayHost
	}
	if strings.TrimSpace(cfg.Morph.WorkRoot) == "" {
		if core.IsDefaultWorkRoot(cfg.WorkRoot) || strings.TrimSpace(cfg.WorkRoot) == "" {
			cfg.Morph.WorkRoot = core.MorphConfigDefaultWorkRoot
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
	cfg.ServerType = core.Blank(strings.TrimSpace(cfg.Morph.Snapshot), "snapshot")
}

func validateMorphConfig(cfg core.Config) error {
	if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
		return core.Exit(2, "provider=morph supports target=linux only")
	}
	if cfg.Network == networkTailscale {
		return core.Exit(2, "--network=tailscale is not supported for provider=morph; Morph exposes SSH through the public gateway")
	}
	if cfg.Tailscale.Enabled {
		return core.Exit(2, "--tailscale is not supported for provider=morph; Morph exposes SSH through the public gateway")
	}
	return nil
}

func validateMorphCreateConfig(cfg core.Config) error {
	if err := validateMorphConfig(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Morph.Snapshot) == "" {
		return core.Exit(2, "provider=morph requires CRABBOX_MORPH_SNAPSHOT or morph.snapshot")
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

func morphLeaseMetadata(cfg core.Config, instance morphInstance, leaseID, slug, state string, keep bool, now time.Time, touch bool) map[string]string {
	existingWorkRoot := strings.TrimSpace(instance.Metadata["work_root"])
	var labels map[string]string
	if touch {
		labels = core.TouchDirectLeaseLabels(instance.Metadata.Clone(), cfg, state, now)
		if strings.EqualFold(strings.TrimSpace(state), "running") {
			if expiresAt := morphLeaseTTLCap(labels); expiresAt != "" {
				labels["expires_at"] = expiresAt
			}
		}
	} else {
		labels = core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	}
	labels["crabbox"] = "true"
	labels["provider"] = providerName
	if leaseID != "" {
		labels["lease"] = leaseID
	}
	if slug != "" {
		labels["slug"] = core.NormalizeLeaseSlug(slug)
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
	if name := core.LeaseProviderName(core.Blank(leaseID, instance.ID), slug); name != "" {
		labels["lease_name"] = name
	}
	if strings.TrimSpace(labels["release"]) == "" {
		labels["release"] = morphReleaseAction(cfg)
	}
	return labels
}

func morphServer(instance morphInstance, cfg core.Config, leaseID, slug string) core.Server {
	labels := instance.Metadata.Clone()
	if leaseID == "" {
		leaseID = core.Blank(strings.TrimSpace(labels["lease"]), instance.ID)
	}
	if slug == "" {
		slug = strings.TrimSpace(labels["slug"])
	}
	if leaseID != "" {
		labels["lease"] = leaseID
	}
	if slug != "" {
		labels["slug"] = core.NormalizeLeaseSlug(slug)
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
	server := core.Server{
		CloudID:  instance.ID,
		Provider: providerName,
		Name:     core.Blank(labels["lease_name"], instance.ID),
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = core.Blank(labels["server_type"], core.Blank(labels["snapshot_id"], core.Blank(cfg.ServerType, "snapshot")))
	server.PublicNet.IPv4.IP = core.Blank(strings.TrimSpace(cfg.Morph.SSHGatewayHost), core.MorphConfigDefaultSSHGatewayHost)
	return server
}

func morphSSHTarget(cfg core.Config, instance morphInstance, keyPath, knownHostsPath string) core.SSHTarget {
	target := core.SSHTargetFromConfig(cfg, core.Blank(strings.TrimSpace(cfg.Morph.SSHGatewayHost), core.MorphConfigDefaultSSHGatewayHost))
	target.Host = core.Blank(strings.TrimSpace(cfg.Morph.SSHGatewayHost), core.MorphConfigDefaultSSHGatewayHost)
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
		return "", core.Exit(1, "morph ssh key response did not include private_key")
	}
	keyData := []byte(privateKey + "\n")
	if sshKey.Password != "" {
		if _, err := ssh.ParseRawPrivateKey(keyData); err != nil {
			password := []byte(sshKey.Password)
			decryptedKey, decryptErr := ssh.ParseRawPrivateKeyWithPassphrase(keyData, password)
			clear(password)
			if decryptErr != nil {
				return "", core.Exit(1, "morph ssh key response could not be decrypted")
			}
			block, marshalErr := ssh.MarshalPrivateKey(decryptedKey, "")
			if marshalErr != nil {
				return "", core.Exit(1, "morph ssh key response could not be stored: %v", marshalErr)
			}
			keyData = pem.EncodeToMemory(block)
		}
	}
	path, err := core.PrepareStoredTestboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := core.WritePreparedLeaseSSHKeyFile(path, keyData); err != nil {
		return "", err
	}
	return path, nil
}

func ensureMorphKnownHostsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", core.Exit(2, "user config directory is unavailable")
	}
	path := filepath.Join(configDir, "crabbox", providerName, "known_hosts")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func serversFromLeaseViews(instances []morphInstance, cfg core.Config) []core.Server {
	servers := make([]core.Server, 0, len(instances))
	for _, instance := range instances {
		servers = append(servers, morphServer(instance, cfg, strings.TrimSpace(instance.Metadata["lease"]), strings.TrimSpace(instance.Metadata["slug"])))
	}
	return servers
}

func finalizeMorphResolution(instance morphInstance, requested string, claim core.LeaseClaim, claimed bool) (morphInstance, string, string) {
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
		if core.IsCanonicalLeaseID(requested) {
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

func morphTTLAction(cfg core.Config) string {
	if cfg.Morph.DeleteOnRelease {
		return "stop"
	}
	return "pause"
}

func morphReleaseAction(cfg core.Config) string {
	if cfg.Morph.DeleteOnRelease {
		return "delete"
	}
	return "pause"
}

func morphDeleteOnRelease(lease core.LeaseTarget, cfg core.Config) bool {
	if deleteOnReleaseExplicit(cfg) {
		return cfg.Morph.DeleteOnRelease
	}
	if lease.Server.Labels != nil {
		switch strings.ToLower(strings.TrimSpace(lease.Server.Labels["release"])) {
		case "delete":
			return true
		case "pause":
			return false
		}
	}
	return cfg.Morph.DeleteOnRelease
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
	return value == "" || value == core.MorphConfigDefaultWorkRoot || core.IsDefaultWorkRoot(value)
}
