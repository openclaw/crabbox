package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func NewCloudflareBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	if cfg.ServerType == "" {
		cfg.ServerType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	if normalized, ok := core.NormalizeCloudflareContainerInstanceType(cfg.ServerType); ok {
		cfg.ServerType = normalized
	}
	return &cloudflareBackend{spec: spec, cfg: cfg, rt: rt}
}

type cloudflareBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

var cloudflareDoctorTimeout = 10 * time.Second

func (b *cloudflareBackend) Spec() core.ProviderSpec { return b.spec }

func (b *cloudflareBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cloudflareDoctorTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cloudflareDoctorTimeout)
		defer cancel()
	}
	if err := client.checkAuth(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	return core.DoctorResult{
		Provider: providerName,
		Message:  fmt.Sprintf("auth=ready control_plane=ready api=readiness mutation=false runner=%s type=%s runtime=ready", client.baseURL, client.instanceType),
	}, nil
}

func (b *cloudflareBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := core.ClockNow(b.rt.Clock)
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
	total := core.ClockNow(b.rt.Clock).Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: providerName,
		LeaseID:  claim.LeaseID,
		Slug:     claim.Slug,
		Total:    total,
	})
}

func (b *cloudflareBackend) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	workdir, err := cloudflareWorkdir(b.cfg)
	if err != nil {
		return core.RunResult{}, err
	}
	var client *cloudflareClient
	var claim core.LeaseClaim
	var command string
	bound := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{LeaseID: claim.LeaseID, Slug: core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID)), CleanupCommand: cloudflareCleanupCommand(claim.LeaseID)}
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
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{Text: command, Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				return client.execStream(ctx, claim.LeaseID, execStreamRequest{Command: command, Cwd: workdir, Env: req.Env, TimeoutMS: durationMillisecondsCeil(b.cfg.TTL)}, stdout, stderr)
			}}, nil
		},
		Cleanup: func(ctx context.Context) error { _, err := destroyClaimedSandbox(ctx, client, claim); return err },
	})
}

func (b *cloudflareBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	claims, err := localCloudflareClaims()
	if err != nil {
		return nil, err
	}
	if req.Refresh {
		if len(claims) == 0 {
			return []core.LeaseView{}, nil
		}
		return b.listRefreshed(ctx, claims)
	}
	servers := make([]core.Server, 0, len(claims))
	for _, claim := range claims {
		servers = append(servers, claimToServer(claim, "unknown"))
	}
	return servers, nil
}

func (b *cloudflareBackend) listRefreshed(ctx context.Context, claims []core.LeaseClaim) ([]core.LeaseView, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(claims))
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

func (b *cloudflareBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return core.StatusView{}, err
	}
	claim, err := resolveCloudflareClaim(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	client.useInstanceType(cloudflareClaimInstanceType(claim))
	deadline := core.ClockNow(b.rt.Clock).Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = core.ClockNow(b.rt.Clock).Add(5 * time.Minute)
	}
	for {
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			return core.StatusView{}, err
		}
		view := sandboxStatusView(claim.LeaseID, claim.Slug, sandbox)
		if cloudflareTerminalState(view.State) {
			return view, nil
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for %s container %s to become ready", providerName, claim.LeaseID)
		}
		select {
		case <-ctx.Done():
			return core.StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *cloudflareBackend) Stop(ctx context.Context, req core.StopRequest) error {
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
func destroyClaimedSandbox(ctx context.Context, client *cloudflareClient, claim core.LeaseClaim) (bool, error) {
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

func (b *cloudflareBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
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
					fmt.Fprintf(b.rt.Stdout, "would remove stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, core.Blank(claim.Slug, "-"))
					continue
				}
				if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				removed++
				fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s reason=not-found\n", providerName, claim.LeaseID, core.Blank(claim.Slug, "-"))
				continue
			}
			fmt.Fprintf(b.rt.Stderr, "warning: %s status failed for %s: %v\n", providerName, claim.LeaseID, err)
			continue
		}
		if !cloudflareTerminalState(sandbox.State) {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would confirm cleanup of %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, core.Blank(claim.Slug, "-"), sandbox.State)
			continue
		}
		if _, err := destroyClaimedSandbox(ctx, client, claim); err != nil {
			return err
		}
		removed++
		fmt.Fprintf(b.rt.Stdout, "removed stale %s claim %s slug=%s state=%s\n", providerName, claim.LeaseID, core.Blank(claim.Slug, "-"), sandbox.State)
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
	return fmt.Sprintf("crabbox stop --provider %s --id %s", providerName, core.ShellQuote(leaseID))
}

func (b *cloudflareBackend) createSandbox(ctx context.Context, client *cloudflareClient, repo core.Repo, requestedSlug string) (core.LeaseClaim, cloudflareContainer, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, cloudflareContainer{}, core.Exit(2, "cloudflare creation requires a repository root for the recovery claim")
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	workdir, err := cloudflareWorkdir(b.cfg)
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	labels := map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "repo": repo.Name, "instance_type": client.instanceType}
	sandbox, err := client.createSandbox(ctx, createSandboxRequest{
		ID: leaseID, LeaseID: leaseID, Slug: slug, Repo: repo.Name, Workdir: workdir,
		InstanceType: client.instanceType, TTLSeconds: durationSecondsCeil(b.cfg.TTL), IdleTimeoutSeconds: durationSecondsCeil(b.cfg.IdleTimeout), Labels: labels,
	})
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	if sandbox.ID != leaseID {
		return core.LeaseClaim{}, cloudflareContainer{}, fmt.Errorf("cloudflare creation returned unexpected sandbox %q for requested %q; inspect the runner before recovery", sandbox.ID, leaseID)
	}
	claim, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, "", b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, labels)
	if err != nil {
		cleanupCtx, cancel := cloudflareCleanupContext()
		defer cancel()
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfterContext(cleanupCtx, leaseID, core.LeaseClaim{}, false, func() error { return client.destroySandbox(cleanupCtx, leaseID) })
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup failed for cloudflare sandbox %s; inspect its claim and runner before recovery: %w", leaseID, cleanupErr))
		}
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	return claim, sandbox, nil
}

func resolveCloudflareClaim(identifier string) (core.LeaseClaim, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if ok {
		return claim, nil
	}
	value := strings.TrimSpace(identifier)
	if value == "" {
		return core.LeaseClaim{}, core.Exit(2, "%s id is required", providerName)
	}
	return core.LeaseClaim{}, core.Exit(2, "refusing to use %s sandbox %q without a local Crabbox claim", providerName, value)
}

func admitRunClaim(ctx context.Context, captured core.LeaseClaim, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	if err := ctx.Err(); err != nil {
		return core.LeaseClaim{}, err
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
	if core.ShouldUseShell(command) || core.LeadingEnvAssignment(command) {
		return core.ShellScriptFromArgv(command), nil
	}
	return strings.Join(core.ShellWords(command), " "), nil
}

func rejectCloudflareSyncOptions(req core.RunRequest) error {
	if req.ChecksumSync {
		return core.Exit(2, "%s uses archive sync; --checksum is not supported", providerName)
	}
	return nil
}

func cloudflareWorkdir(cfg core.Config) (string, error) {
	workdir := core.Blank(strings.TrimSpace(cfg.Cloudflare.Workdir), core.CloudflareConfigDefaultWorkdir)
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", core.Exit(2, "%s workdir %q must resolve to an absolute path", providerName, workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", core.Exit(2, "%s workdir %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	return clean, nil
}

func sandboxStatusView(leaseID, slug string, sandbox cloudflareContainer) core.StatusView {
	server := sandboxToServer(leaseID, slug, sandbox)
	return core.StatusView{
		ID:         leaseID,
		Slug:       core.Blank(slug, core.NewLeaseSlug(leaseID)),
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

func sandboxToServer(leaseID, slug string, sandbox cloudflareContainer) core.Server {
	labels := map[string]string{}
	for k, v := range sandbox.Labels {
		labels[k] = v
	}
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = core.Blank(slug, core.NewLeaseSlug(leaseID))
	labels["target"] = targetLinux
	state := core.Blank(sandbox.State, "running")
	instanceType := core.Blank(sandbox.InstanceType, providerName)
	labels["state"] = state
	labels["instance_type"] = instanceType
	server := core.Server{
		Provider: providerName,
		CloudID:  sandbox.ID,
		Name:     sandbox.ID,
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = instanceType
	return server
}

func claimToServer(claim core.LeaseClaim, state string) core.Server {
	labels := map[string]string{
		"provider": providerName,
		"lease":    claim.LeaseID,
		"slug":     core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID)),
		"target":   targetLinux,
		"state":    state,
	}
	server := core.Server{
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

func cloudflareClaimInstanceType(claim core.LeaseClaim) string {
	return claim.Labels["instance_type"]
}

func localCloudflareClaims() ([]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	var filtered []core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider == providerName {
			filtered = append(filtered, claim)
		}
	}
	return filtered, nil
}
