package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func NewCloudflareBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	if cfg.ServerType == "" {
		cfg.ServerType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	if normalized, ok := normalizeCloudflareContainerInstanceType(cfg.ServerType); ok {
		cfg.ServerType = normalized
	}
	return &cloudflareBackend{spec: spec, cfg: cfg, rt: rt}
}

type cloudflareBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var cloudflareDoctorTimeout = 10 * time.Second

func (b *cloudflareBackend) Spec() ProviderSpec { return b.spec }

func (b *cloudflareBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	if cloudflareDoctorTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cloudflareDoctorTimeout)
		defer cancel()
	}
	if err := client.checkAuth(ctx); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Message:  fmt.Sprintf("auth=ready control_plane=ready api=readiness mutation=false runner=%s type=%s runtime=ready", client.baseURL, client.instanceType),
	}, nil
}

func (b *cloudflareBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, sandbox, err := b.createSandbox(ctx, client, req.Repo, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", claim.LeaseID, claim.Slug, providerName, sandbox.ID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: %s warmup keeps the container until explicit stop\n", providerName)
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  claim.LeaseID,
			Slug:     claim.Slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *cloudflareBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	workdir, err := cloudflareWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	var client *cloudflareClient
	var claim LeaseClaim
	var command string
	bound := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: blank(claim.Slug, newLeaseSlug(claim.LeaseID)), CleanupCommand: cloudflareCleanupCommand(claim.LeaseID)}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL, CleanupTimeout: cloudflareCleanupTimeout,
		Preflight: func(context.Context) error {
			if err := rejectCloudflareSyncOptions(req); err != nil {
				return err
			}
			if !req.SyncOnly {
				command, err = buildCloudflareCommand(req.Command, req.ShellMode)
				if err != nil {
					return err
				}
			}
			client, err = newCloudflareClient(b.cfg, b.rt)
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) { return b.prepareArchive(ctx, req) },
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			claim, _, err = b.createSandbox(ctx, client, req.Repo, req.RequestedSlug)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", claim.LeaseID, claim.Slug, providerName, claim.LeaseID)
			return bound(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			claim, err = resolveCloudflareClaim(req.ID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			claim, err = admitRunClaim(ctx, claim, req.Repo.Root, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			client.useInstanceType(cloudflareClaimInstanceType(claim))
			return bound(), nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, claim.LeaseID, req, workdir, prepared)
		},
		NoSync: func(ctx context.Context) error { return b.prepareWorkspace(ctx, client, claim.LeaseID, workdir) },
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			if req.EnvSummary {
				printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{Text: command, Run: func(ctx context.Context) (int, error) {
				return client.execStream(ctx, claim.LeaseID, execStreamRequest{Command: command, Cwd: workdir, Env: req.Env, TimeoutMS: durationMillisecondsCeil(b.cfg.TTL)}, b.rt.Stdout, b.rt.Stderr)
			}}, nil
		},
		Cleanup: func(ctx context.Context) error { _, err := destroyClaimedSandbox(ctx, client, claim); return err },
	})
}

func (b *cloudflareBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	claims, err := localCloudflareClaims()
	if err != nil {
		return nil, err
	}
	if req.Refresh {
		if len(claims) == 0 {
			return []LeaseView{}, nil
		}
		return b.listRefreshed(ctx, claims)
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		servers = append(servers, claimToServer(claim, "unknown"))
	}
	return servers, nil
}

func (b *cloudflareBackend) listRefreshed(ctx context.Context, claims []LeaseClaim) ([]LeaseView, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	defaultInstanceType := client.instanceType
	for _, claim := range claims {
		client.instanceType = defaultInstanceType
		client.useInstanceType(cloudflareClaimInstanceType(claim))
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			if cloudflareNotFoundError(err) {
				servers = append(servers, claimToServer(claim, "missing"))
				continue
			}
			fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
			servers = append(servers, claimToServer(claim, "unknown"))
			continue
		}
		servers = append(servers, sandboxToServer(claim.LeaseID, claim.Slug, sandbox))
	}
	return servers, nil
}

func (b *cloudflareBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return StatusView{}, err
	}
	claim, err := resolveCloudflareClaim(req.ID)
	if err != nil {
		return StatusView{}, err
	}
	client.useInstanceType(cloudflareClaimInstanceType(claim))
	deadline := b.now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = b.now().Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			return StatusView{}, err
		}
		view := sandboxStatusView(claim.LeaseID, claim.Slug, sandbox)
		if cloudflareTerminalState(view.State) {
			return view, nil
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for %s container %s to become ready", providerName, claim.LeaseID)
		}
		select {
		case <-ctx.Done():
			return StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *cloudflareBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claim, err := resolveCloudflareClaim(req.ID)
	if err != nil {
		return err
	}
	missing, err := destroyClaimedSandbox(ctx, client, claim)
	if err != nil {
		return err
	}
	if missing {
		fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s reason=not-found\n", providerName, claim.LeaseID)
	} else {
		fmt.Fprintf(b.rt.Stdout, "stopped %s provider=%s sandbox=%s\n", claim.LeaseID, providerName, claim.LeaseID)
	}
	return nil
}

// Keep the captured local authority fenced through the native effect. The
// runner's instance type is a routing preference, not a remote generation CAS.
func destroyClaimedSandbox(ctx context.Context, client *cloudflareClient, claim LeaseClaim) (bool, error) {
	client.useInstanceType(cloudflareClaimInstanceType(claim))
	missing := false
	err := core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, claim.LeaseID, claim, true, func() error {
		err := client.destroySandbox(ctx, claim.LeaseID)
		if cloudflareNotFoundError(err) {
			missing = true
			return nil
		}
		return err
	})
	return missing, err
}

func (b *cloudflareBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	claims, err := localCloudflareClaims()
	if err != nil {
		return err
	}
	removed := 0
	defaultInstanceType := client.instanceType
	for _, claim := range claims {
		client.instanceType = defaultInstanceType
		client.useInstanceType(cloudflareClaimInstanceType(claim))
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			if cloudflareNotFoundError(err) {
				if req.DryRun {
					fmt.Fprintf(b.rt.Stdout, "would remove stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
					continue
				}
				if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				removed++
				fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, blank(claim.Slug, "-"))
				continue
			}
			fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
			continue
		}
		if !cloudflareTerminalState(sandbox.State) {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would confirm cleanup of %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), sandbox.State)
			continue
		}
		if _, err := destroyClaimedSandbox(ctx, client, claim); err != nil {
			return err
		}
		removed++
		fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, blank(claim.Slug, "-"), sandbox.State)
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, len(claims))
	}
	return nil
}

func cloudflareNotFoundError(err error) bool {
	var responseErr *cloudflareResponseError
	return errors.As(err, &responseErr) && responseErr.statusCode == http.StatusNotFound
}

func cloudflareCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, shellQuote(leaseID))
}

func (b *cloudflareBackend) createSandbox(ctx context.Context, client *cloudflareClient, repo Repo, requestedSlug string) (LeaseClaim, cloudflareContainer, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return LeaseClaim{}, cloudflareContainer{}, exit(2, "cloudflare creation requires a repository root for the recovery claim")
	}
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return LeaseClaim{}, cloudflareContainer{}, err
	}
	workdir, err := cloudflareWorkdir(b.cfg)
	if err != nil {
		return LeaseClaim{}, cloudflareContainer{}, err
	}
	labels := map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "repo": repo.Name, "instance_type": client.instanceType}
	sandbox, err := client.createSandbox(ctx, createSandboxRequest{
		ID: leaseID, LeaseID: leaseID, Slug: slug, Repo: repo.Name, Workdir: workdir,
		InstanceType: client.instanceType, TTLSeconds: durationSecondsCeil(b.cfg.TTL), IdleTimeoutSeconds: durationSecondsCeil(b.cfg.IdleTimeout), Labels: labels,
	})
	if err != nil {
		return LeaseClaim{}, cloudflareContainer{}, err
	}
	if sandbox.ID != leaseID {
		return LeaseClaim{}, cloudflareContainer{}, fmt.Errorf("cloudflare creation returned unexpected sandbox %q for requested %q; inspect the runner before recovery", sandbox.ID, leaseID)
	}
	claim, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, "", b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, labels)
	if err != nil {
		cleanupCtx, cancel := cloudflareCleanupContext()
		defer cancel()
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfterContext(cleanupCtx, leaseID, LeaseClaim{}, false, func() error { return client.destroySandbox(cleanupCtx, leaseID) })
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup failed for cloudflare sandbox %s; inspect its claim and runner before recovery: %w", leaseID, cleanupErr))
		}
		return LeaseClaim{}, cloudflareContainer{}, err
	}
	return claim, sandbox, nil
}

func resolveCloudflareClaim(identifier string) (LeaseClaim, error) {
	claim, ok, err := resolveLeaseClaimForProvider(identifier, providerName)
	if err != nil {
		return LeaseClaim{}, err
	}
	if ok {
		return claim, nil
	}
	value := strings.TrimSpace(identifier)
	if value == "" {
		return LeaseClaim{}, exit(2, "%s id is required", providerName)
	}
	return LeaseClaim{}, exit(2, "refusing to use %s sandbox %q without a local Crabbox claim", providerName, value)
}

func admitRunClaim(ctx context.Context, captured LeaseClaim, repoRoot string, reclaim bool) (LeaseClaim, error) {
	if err := ctx.Err(); err != nil {
		return LeaseClaim{}, err
	}
	if repoRoot == "" {
		err := core.WithLeaseClaimUnchangedShared(ctx, captured.LeaseID, captured, func() error { return nil })
		return captured, err
	}
	// Repository admission retains the existing non-cancelable local lock wait.
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(captured.LeaseID, captured.Slug, providerName, captured.ProviderScope, captured.Pond, repoRoot, time.Duration(captured.IdleTimeoutSeconds)*time.Second, reclaim, captured, true)
}

func buildCloudflareCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command), nil
	}
	return strings.Join(shellWords(command), " "), nil
}

func rejectCloudflareSyncOptions(req RunRequest) error {
	if req.ChecksumSync {
		return exit(2, "%s uses archive sync; --checksum is not supported", providerName)
	}
	return nil
}

func cloudflareWorkdir(cfg Config) (string, error) {
	workdir := blank(strings.TrimSpace(cfg.Cloudflare.Workdir), "/workspace/crabbox")
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "%s workdir %q must resolve to an absolute path", providerName, workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "%s workdir %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	return clean, nil
}

func sandboxStatusView(leaseID, slug string, sandbox cloudflareContainer) StatusView {
	server := sandboxToServer(leaseID, slug, sandbox)
	return StatusView{
		ID:         leaseID,
		Slug:       blank(slug, newLeaseSlug(leaseID)),
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      server.Status,
		ServerID:   sandbox.ID,
		ServerType: server.ServerType.Name,
		Network:    networkPublic,
		Ready:      cloudflareReady(server.Status),
		Labels:     server.Labels,
	}
}

func sandboxToServer(leaseID, slug string, sandbox cloudflareContainer) Server {
	labels := map[string]string{}
	for k, v := range sandbox.Labels {
		labels[k] = v
	}
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = blank(slug, newLeaseSlug(leaseID))
	labels["target"] = targetLinux
	state := blank(sandbox.State, "running")
	instanceType := blank(sandbox.InstanceType, providerName)
	labels["state"] = state
	labels["instance_type"] = instanceType
	server := Server{
		Provider: providerName,
		CloudID:  sandbox.ID,
		Name:     sandbox.ID,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = instanceType
	return server
}

func claimToServer(claim LeaseClaim, state string) Server {
	labels := map[string]string{
		"provider": providerName,
		"lease":    claim.LeaseID,
		"slug":     blank(claim.Slug, newLeaseSlug(claim.LeaseID)),
		"target":   targetLinux,
		"state":    state,
	}
	server := Server{
		Provider: providerName,
		CloudID:  claim.LeaseID,
		Name:     claim.LeaseID,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = providerName
	return server
}

func cloudflareReady(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "started", "active", "healthy":
		return true
	default:
		return false
	}
}

func cloudflareTerminalState(status string) bool {
	state := strings.ToLower(strings.TrimSpace(status))
	return state == "expired" || state == "stopped" || state == "stopped_with_code"
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func durationMillisecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}

func (b *cloudflareBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func cloudflareClaimInstanceType(claim LeaseClaim) string {
	return claim.Labels["instance_type"]
}

func localCloudflareClaims() ([]LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	var filtered []LeaseClaim
	for _, claim := range claims {
		if claim.Provider == providerName {
			filtered = append(filtered, claim)
		}
	}
	return filtered, nil
}
