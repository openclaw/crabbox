package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"strconv"
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
	if normalized, ok := normalizeContainerInstanceType(cfg.ServerType); ok {
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
	claim, sandbox, err := b.createSandbox(ctx, client, req.Repo, req.RequestedSlug, sandboxSource{})
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
	cfg := b.runConfig(req.ID)
	workdir, err := cloudflareWorkdir(cfg)
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
				intent, err := core.ParseCommandIntent(req.Command, req.ShellMode, req.CommandLiteralArgs)
				if err != nil {
					return err
				}
				command = intent.ShellScript()
			}
			client, err = newCloudflareClient(cfg, b.rt)
			return err
		},
		Workspace: func() shared.SandboxWorkspace {
			return shared.WorkspaceOperations{
				PrepareArchiveFunc: func(ctx context.Context) (*core.PreparedArchive, error) { return b.prepareArchive(ctx, req) },
				SyncFunc: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
					return b.syncWorkspace(ctx, client, claim.LeaseID, req, workdir, prepared)
				},
				EnsureFunc: func(ctx context.Context) error { return b.prepareWorkspace(ctx, client, claim.LeaseID, workdir) },
			}
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			claim, _, err = b.createSandbox(ctx, client, req.Repo, req.RequestedSlug, sandboxSource{})
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
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			if req.EnvSummary {
				core.PrintEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
			}
			return shared.DelegatedSandboxCommand{Text: command, Run: func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
				return client.execStream(ctx, claim.LeaseID, shared.CommandStreamRequest{Command: command, Cwd: workdir, Env: req.Env, TimeoutMS: durationMillisecondsCeil(b.cfg.TTL)}, stdout, stderr)
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
	servers := make([]core.Server, 0, len(claims))
	for _, claim := range claims {
		client, err := b.leaseClient(claim)
		if err != nil {
			return nil, err
		}
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
	claim, err := resolveCloudflareClaim(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	client, err := b.leaseClient(claim)
	if err != nil {
		return core.StatusView{}, err
	}
	return shared.PollStatus(ctx, req, func() time.Time { return core.ClockNow(b.rt.Clock) }, func(ctx context.Context) (core.StatusView, bool, error) {
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			return core.StatusView{}, false, err
		}
		view := sandboxStatusView(claim.LeaseID, claim.Slug, sandbox)
		return view, cloudflareTerminalState(view.State), nil
	}, func() error {
		return core.Exit(5, "timed out waiting for %s container %s to become ready", providerName, claim.LeaseID)
	})
}

func (b *cloudflareBackend) Stop(ctx context.Context, req core.StopRequest) error {
	claim, err := resolveCloudflareClaim(req.ID)
	if err != nil {
		return err
	}
	client, err := b.leaseClient(claim)
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
	claims, err := localCloudflareClaims()
	if err != nil {
		return err
	}
	removed := 0
	for _, claim := range claims {
		client, err := b.leaseClient(claim)
		if err != nil {
			return err
		}
		sandbox, err := client.getSandbox(ctx, claim.LeaseID)
		if err != nil {
			if cloudflareNotFoundError(err) {
				if createPending(claim, core.ClockNow(b.rt.Clock)) {
					continue
				}
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

// sandboxSource starts a lease from a container snapshot instead of an image.
type sandboxSource struct {
	snapshotID string
	workdir    string
}

func (b *cloudflareBackend) createSandbox(ctx context.Context, client *cloudflareClient, repo core.Repo, requestedSlug string, source sandboxSource) (core.LeaseClaim, cloudflareContainer, error) {
	if strings.TrimSpace(repo.Root) == "" {
		return core.LeaseClaim{}, cloudflareContainer{}, core.Exit(2, "cloudflare creation requires a repository root for the recovery claim")
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	workdirCfg := b.cfg
	if source.workdir != "" {
		workdirCfg.Cloudflare.Workdir = source.workdir
	}
	workdir, err := cloudflareWorkdir(workdirCfg)
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	labels := map[string]string{"crabbox": "true", "provider": providerName, "lease": leaseID, "slug": slug, "repo": repo.Name, "instance_type": client.instanceType, "workdir": workdir, runnerURLLabel: client.baseURL}
	// The runner may allocate the sandbox even when its response is lost, so
	// the claim exists before the request and outlives an ambiguous failure.
	pendingLabels := maps.Clone(labels)
	pendingLabels[createPendingUntilLabel] = core.LeaseLabelTime(core.ClockNow(b.rt.Clock).Add(createPendingWindow))
	claim, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, "", b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, pendingLabels)
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, err
	}
	sandbox, err := client.createSandbox(ctx, createSandboxRequest{
		ID: leaseID, LeaseID: leaseID, Slug: slug, Repo: repo.Name, Workdir: workdir, SnapshotID: source.snapshotID,
		InstanceType: client.instanceType, Image: strings.TrimSpace(b.cfg.Cloudflare.Image), TTLSeconds: durationSecondsCeil(b.cfg.TTL), IdleTimeoutSeconds: durationSecondsCeil(b.cfg.IdleTimeout), Labels: labels,
	})
	if err == nil && sandbox.ID != leaseID {
		err = fmt.Errorf("cloudflare creation returned unexpected sandbox %q for requested %q", sandbox.ID, leaseID)
	}
	if err != nil {
		if cloudflareRejectedError(err) {
			if releaseErr := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("release %s claim %s: %w", providerName, leaseID, releaseErr))
			}
			return core.LeaseClaim{}, cloudflareContainer{}, err
		}
		return core.LeaseClaim{}, cloudflareContainer{}, fmt.Errorf("%s create of %s failed and may have allocated a sandbox; its claim is kept: check it with `crabbox status --provider %s --id %s` or destroy it with `%s`: %w", providerName, leaseID, providerName, leaseID, cloudflareCleanupCommand(leaseID), err)
	}
	claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels)
	if err != nil {
		return core.LeaseClaim{}, cloudflareContainer{}, fmt.Errorf("%s sandbox %s was created but its claim changed; inspect it with `crabbox status --provider %s --id %s`: %w", providerName, leaseID, providerName, leaseID, err)
	}
	return claim, sandbox, nil
}

// createPendingUntilLabel marks a claim whose create request may still be in
// flight, so cleanup does not drop it before the runner records the sandbox.
const createPendingUntilLabel = "create_pending_until"

const createPendingWindow = cloudflareDefaultResponseHeaderTimeout + time.Minute

func createPending(claim core.LeaseClaim, now time.Time) bool {
	seconds, err := strconv.ParseInt(strings.TrimSpace(claim.Labels[createPendingUntilLabel]), 10, 64)
	return err == nil && now.Before(time.Unix(seconds, 0))
}

// A 4xx create response means the runner rejected the request before it
// allocated anything.
func cloudflareRejectedError(err error) bool {
	var responseErr *cloudflareResponseError
	return errors.As(err, &responseErr) && responseErr.statusCode >= 400 && responseErr.statusCode < 500
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

func rejectCloudflareSyncOptions(req core.RunRequest) error {
	if req.ChecksumSync {
		return core.Exit(2, "%s uses archive sync; --checksum is not supported", providerName)
	}
	return nil
}

// runnerURLLabel records the runner that created a lease. Sandboxes exist only
// on that runner, so lease commands keep using it even when the current
// configuration, such as a fork command's nested run, selects another one.
const runnerURLLabel = "runner_url"

// leaseConfig keeps a claimed lease on the runner and workdir it was created
// with, such as a checkpoint fork's captured workdir.
func leaseConfig(cfg core.Config, claim core.LeaseClaim) core.Config {
	if runner := strings.TrimSpace(claim.Labels[runnerURLLabel]); runner != "" {
		cfg.Cloudflare.APIURL = runner
	}
	if workdir := strings.TrimSpace(claim.Labels["workdir"]); workdir != "" {
		cfg.Cloudflare.Workdir = workdir
	}
	return cfg
}

func (b *cloudflareBackend) leaseClient(claim core.LeaseClaim) (*cloudflareClient, error) {
	client, err := newCloudflareClient(leaseConfig(b.cfg, claim), b.rt)
	if err != nil {
		return nil, err
	}
	client.useInstanceType(cloudflareClaimInstanceType(claim))
	return client, nil
}

// runConfig applies a claimed lease's runner and workdir; new leases use the
// configured ones. Resolve reports claim lookup failures for the run.
func (b *cloudflareBackend) runConfig(id string) core.Config {
	if strings.TrimSpace(id) != "" {
		if claim, ok, err := core.ResolveLeaseClaimForProvider(id, providerName); err == nil && ok {
			return leaseConfig(b.cfg, claim)
		}
	}
	return b.cfg
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
	case "ready", "started", "active", "healthy", "running":
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
