package daytona

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	apidaytona "github.com/daytonaio/daytona/libs/api-client-go"
	sdkdaytona "github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	sdkoptions "github.com/daytonaio/daytona/libs/sdk-go/pkg/options"
	sdktypes "github.com/daytonaio/daytona/libs/sdk-go/pkg/types"
)

var daytonaCleanupTimeout = 30 * time.Second

func daytonaCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), daytonaCleanupTimeout)
}

func (b *daytonaLeaseBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=daytona SDK warmup")
	}
	started := time.Now()
	sandbox, leaseID, slug, err := b.createDaytonaToolboxSandbox(ctx, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=daytona sandbox=%s\n", leaseID, slug, sandbox.ID)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", time.Since(started).Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: daytonaProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  time.Since(started).Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *daytonaLeaseBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	started := time.Now()
	client, err := newDaytonaToolboxClient(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	var sandbox *sdkdaytona.Sandbox
	leaseID, slug := "", ""
	acquired := false
	if req.ID == "" {
		sandbox, leaseID, slug, err = b.createDaytonaToolboxSandboxWithClient(ctx, client, req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=daytona sandbox=%s\n", leaseID, slug, sandbox.ID)
		acquired = true
	} else {
		sandbox, leaseID, err = b.resolveDaytonaToolboxSandbox(ctx, client, req.ID, req.Repo, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		slug = newLeaseSlug(leaseID)
		if claim, ok, claimErr := resolveLeaseClaimForProvider(leaseID, daytonaProvider); claimErr != nil {
			return RunResult{}, claimErr
		} else if ok {
			slug = claim.Slug
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if shouldStop {
				cleanupCtx, cancel := daytonaCleanupContext()
				defer cancel()
				b.deleteDaytonaToolboxSandbox(cleanupCtx, sandbox.ID, leaseID)
			}
		}()
	}
	cfg := b.cfg
	cfg.Provider = daytonaProvider
	cfg.WorkRoot = daytonaWorkRoot(cfg)
	workdir := remoteJoin(cfg, leaseID, req.Repo.Name)
	var syncDuration time.Duration
	var syncPhases []timingPhase
	if !req.NoSync {
		syncStarted := time.Now()
		syncPhases, err = b.syncDaytonaToolbox(ctx, sandbox, req, workdir)
		syncDuration = time.Since(syncStarted)
		if err != nil {
			return RunResult{Total: time.Since(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else {
		if _, err := sandbox.Process.ExecuteCommand(ctx, "mkdir -p "+shellQuote(workdir)); err != nil {
			return RunResult{}, fmt.Errorf("daytona create workdir: %w", err)
		}
	}
	if req.SyncOnly {
		result := RunResult{Total: time.Since(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
				Provider:    daytonaProvider,
				LeaseID:     leaseID,
				Slug:        slug,
				SyncMs:      syncDuration.Milliseconds(),
				SyncPhases:  syncPhases,
				SyncSkipped: req.NoSync,
				TotalMs:     result.Total.Milliseconds(),
				ExitCode:    0,
				Label:       strings.TrimSpace(req.Label),
			}, result, nil))
			return result, err
		}
		return result, nil
	}
	command := daytonaCommandString(req.Command, req.ShellMode)
	if command == "" {
		return RunResult{}, exit(2, "missing command")
	}
	commandStarted := time.Now()
	fmt.Fprintf(b.rt.Stderr, "running on daytona %s\n", strings.Join(req.Command, " "))
	execOpts := []func(*sdkoptions.ExecuteCommand){sdkoptions.WithCwd(workdir)}
	if env := req.Env; len(env) > 0 {
		execOpts = append(execOpts, sdkoptions.WithCommandEnv(env))
	}
	response, err := sandbox.Process.ExecuteCommand(ctx, command, execOpts...)
	commandDuration := time.Since(commandStarted)
	result := RunResult{
		ExitCode:      responseExitCode(response),
		Command:       commandDuration,
		Total:         time.Since(started),
		SyncDelegated: true,
	}
	if response != nil && response.Result != "" {
		fmt.Fprint(b.rt.Stdout, response.Result)
		if !strings.HasSuffix(response.Result, "\n") {
			fmt.Fprintln(b.rt.Stdout)
		}
	}
	fmt.Fprintf(b.rt.Stderr, "daytona run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	if req.TimingJSON {
		if timingErr := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:    daytonaProvider,
			LeaseID:     leaseID,
			Slug:        slug,
			SyncMs:      syncDuration.Milliseconds(),
			SyncPhases:  syncPhases,
			SyncSkipped: req.NoSync,
			CommandMs:   commandDuration.Milliseconds(),
			TotalMs:     result.Total.Milliseconds(),
			ExitCode:    result.ExitCode,
			Label:       strings.TrimSpace(req.Label),
		}, result, err)); timingErr != nil {
			return result, timingErr
		}
	}
	if err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, daytonaProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("daytona run failed: %v", err)}
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, daytonaProvider, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("daytona run exited %d", result.ExitCode)}
	}
	return result, nil
}

func (b *daytonaLeaseBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return statusView{}, err
	}
	deadline := time.Now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = time.Now().Add(5 * time.Minute)
	}
	for {
		sandbox, leaseID, err := resolveDaytonaSandbox(ctx, client, b.cfg, req.ID)
		if err != nil {
			return statusView{}, err
		}
		view := daytonaStatusView(leaseID, sandbox, b.cfg)
		if !req.Wait || view.Ready {
			return view, nil
		}
		if time.Now().After(deadline) {
			return statusView{}, exit(5, "timed out waiting for sandbox %s to become ready", req.ID)
		}
		select {
		case <-ctx.Done():
			return statusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *daytonaLeaseBackend) Stop(ctx context.Context, req StopRequest) error {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	sandbox, leaseID, err := resolveDaytonaSandbox(ctx, client, b.cfg, req.ID)
	if err != nil {
		return err
	}
	if err := requireExactDaytonaClaim(leaseID, sandbox); err != nil {
		return err
	}
	if err := client.DeleteSandbox(ctx, sandbox.GetId()); err != nil {
		return daytonaError("delete sandbox", err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandbox.GetId())
	return nil
}

func (b *daytonaLeaseBackend) createDaytonaToolboxSandbox(ctx context.Context, repo Repo, keep, reclaim bool, requestedSlug string) (*sdkdaytona.Sandbox, string, string, error) {
	client, err := newDaytonaToolboxClient(b.cfg)
	if err != nil {
		return nil, "", "", err
	}
	return b.createDaytonaToolboxSandboxWithClient(ctx, client, repo, keep, reclaim, requestedSlug)
}

func (b *daytonaLeaseBackend) createDaytonaToolboxSandboxWithClient(ctx context.Context, client *sdkdaytona.Client, repo Repo, keep, reclaim bool, requestedSlug string) (*sdkdaytona.Sandbox, string, string, error) {
	if strings.TrimSpace(b.cfg.Daytona.Snapshot) == "" {
		return nil, "", "", exit(2, "provider=daytona requires --daytona-snapshot or daytona.snapshot")
	}
	apiClient, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return nil, "", "", err
	}
	leaseID := newLeaseID()
	existing, err := apiClient.ListCrabboxSandboxes(ctx)
	if err != nil {
		return nil, "", "", daytonaError("list sandboxes", err)
	}
	slug, err := allocateDirectLeaseSlug(leaseID, requestedSlug, daytonaSandboxesToServers(existing, b.cfg))
	if err != nil {
		return nil, "", "", err
	}
	cfg := b.cfg
	cfg.Provider = daytonaProvider
	cfg.ServerType = "snapshot"
	cfg.WorkRoot = daytonaWorkRoot(cfg)
	cfg.SSHUser = daytonaUser(cfg)
	cfg.SSHPort = "22"
	labels := directLeaseLabels(cfg, leaseID, slug, daytonaProvider, "", keep, time.Now().UTC())
	labels["lease_name"] = leaseProviderName(leaseID, slug)
	labels["work_root"] = cfg.WorkRoot
	autoStop := durationMinutesCeil(cfg.IdleTimeout)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=daytona lease=%s slug=%s snapshot=%s target=%s keep=%v mode=sdk\n", leaseID, slug, cfg.Daytona.Snapshot, blank(cfg.Daytona.Target, "-"), keep)
	sandbox, err := client.Create(ctx, sdktypes.SnapshotParams{
		Snapshot: strings.TrimSpace(cfg.Daytona.Snapshot),
		SandboxBaseParams: sdktypes.SandboxBaseParams{
			Name:             labels["lease_name"],
			User:             daytonaUser(cfg),
			Labels:           labels,
			Public:           true,
			AutoStopInterval: &autoStop,
		},
	}, sdkoptions.WithTimeout(5*time.Minute))
	if err != nil {
		return nil, "", "", daytonaError("create sandbox", err)
	}
	labels["state"] = "ready"
	labels["last_touched_at"] = leaseLabelTime(time.Now().UTC())
	if _, err := establishDaytonaSandboxOwnership(ctx, apiClient, sandbox.ID, leaseID, labels); err != nil {
		_ = sandbox.Delete(context.Background())
		return nil, "", "", err
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, Server{Provider: daytonaProvider, CloudID: sandbox.ID, Labels: labels}, SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		_ = sandbox.Delete(context.Background())
		return nil, "", "", err
	}
	return sandbox, leaseID, slug, nil
}

func (b *daytonaLeaseBackend) resolveDaytonaToolboxSandbox(ctx context.Context, sdkClient *sdkdaytona.Client, id string, repo Repo, reclaim bool) (*sdkdaytona.Sandbox, string, error) {
	apiClient, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return nil, "", err
	}
	apiSandbox, leaseID, err := resolveDaytonaSandbox(ctx, apiClient, b.cfg, id)
	if err != nil {
		return nil, "", err
	}
	server := daytonaSandboxToServer(apiSandbox, b.cfg)
	if reclaim {
		if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), b.cfg, server, SSHTarget{}, repo.Root, b.cfg.IdleTimeout, true); err != nil {
			return nil, "", err
		}
	}
	if err := requireExactDaytonaClaim(leaseID, apiSandbox); err != nil {
		return nil, "", err
	}
	if !reclaim {
		if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), b.cfg, server, SSHTarget{}, repo.Root, b.cfg.IdleTimeout, false); err != nil {
			return nil, "", err
		}
	}
	if !daytonaStateReady(daytonaSandboxState(apiSandbox)) {
		if _, err := apiClient.StartSandbox(ctx, apiSandbox.GetId()); err != nil {
			return nil, "", daytonaError("start sandbox", err)
		}
		if _, err := waitForDaytonaReady(ctx, apiClient, apiSandbox.GetId(), 5*time.Minute); err != nil {
			return nil, "", err
		}
	}
	sandbox, err := sdkClient.Get(ctx, apiSandbox.GetId())
	if err != nil {
		return nil, "", daytonaError("get sandbox", err)
	}
	return sandbox, leaseID, nil
}

func (b *daytonaLeaseBackend) deleteDaytonaToolboxSandbox(ctx context.Context, sandboxID, leaseID string) {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: daytona stop failed for %s: %v\n", sandboxID, err)
		return
	}
	if err := client.DeleteSandbox(ctx, sandboxID); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: daytona stop failed for %s: %v\n", sandboxID, daytonaError("delete sandbox", err))
		return
	}
	removeLeaseClaim(leaseID)
}

func (b *daytonaLeaseBackend) syncDaytonaToolbox(ctx context.Context, sandbox *sdkdaytona.Sandbox, req RunRequest, workdir string) ([]timingPhase, error) {
	start := time.Now()
	excludes, err := syncExcludes(req.Repo.Root, b.cfg)
	if err != nil {
		return nil, err
	}
	manifestStarted := time.Now()
	manifest, err := syncManifest(req.Repo.Root, excludes, b.cfg.Sync.Includes)
	if err != nil {
		return nil, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := time.Since(manifestStarted)
	preflightStarted := time.Now()
	if err := checkSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, err
	}
	preflightDuration := time.Since(preflightStarted)
	archiveStarted := time.Now()
	archive, err := createDaytonaSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := time.Since(archiveStarted)
	uploadStarted := time.Now()
	archivePath := path.Join("/tmp", "crabbox-"+newLeaseID()+".tgz")
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("daytona rewind archive: %w", err)
	}
	if err := b.uploadDaytonaArchive(ctx, sandbox.ID, archivePath, archive); err != nil {
		return nil, err
	}
	uploadDuration := time.Since(uploadStarted)
	extractStarted := time.Now()
	deletePrefix := ""
	if b.cfg.Sync.Delete {
		deletePrefix = "rm -rf " + shellQuote(workdir) + " && "
	}
	extractCommand := daytonaExtractArchiveCommand(workdir, archivePath, deletePrefix)
	if response, err := sandbox.Process.ExecuteCommand(ctx, extractCommand); err != nil {
		return nil, fmt.Errorf("daytona extract archive: %w", err)
	} else if responseExitCode(response) != 0 {
		return nil, exit(responseExitCode(response), "daytona extract archive exited %d: %s", responseExitCode(response), response.Result)
	}
	extractDuration := time.Since(extractStarted)
	manifestWriteStarted := time.Now()
	metaDir := path.Join(workdir, ".crabbox")
	if err := sandbox.FileSystem.CreateFolder(ctx, metaDir); err != nil {
		return nil, fmt.Errorf("daytona create metadata dir: %w", err)
	}
	if err := sandbox.FileSystem.UploadFile(ctx, manifest.NUL(), path.Join(metaDir, "sync-manifest")); err != nil {
		return nil, fmt.Errorf("daytona upload sync manifest: %w", err)
	}
	manifestWriteDuration := time.Since(manifestWriteStarted)
	phases := []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "extract", Ms: extractDuration.Milliseconds()},
		{Name: "manifest_write", Ms: manifestWriteDuration.Milliseconds()},
		{Name: "toolbox_sync", Ms: time.Since(start).Milliseconds()},
	}
	return phases, nil
}

func daytonaExtractArchiveCommand(workdir, archivePath, deletePrefix string) string {
	return deletePrefix +
		"mkdir -p " + shellQuote(workdir) +
		" && tar -xzf " + shellQuote(archivePath) + " -C " + shellQuote(workdir) +
		"; crabbox_status=$?; rm -f " + shellQuote(archivePath) + "; exit $crabbox_status"
}

func createDaytonaSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-daytona-sync-*.tgz")
}

func daytonaCommandString(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode {
		return strings.Join(command, " ")
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command)
	}
	return strings.Join(shellWords(command), " ")
}

func daytonaStatusView(leaseID string, sandbox *apidaytona.Sandbox, cfg Config) statusView {
	server := daytonaSandboxToServer(sandbox, cfg)
	state := server.Status
	if !daytonaStateReady(state) {
		state = blank(server.Labels["state"], state)
	}
	return statusView{
		ID:         leaseID,
		Slug:       serverSlug(server),
		Provider:   daytonaProvider,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   server.DisplayID(),
		ServerType: server.ServerType.Name,
		Network:    NetworkPublic,
		Ready:      daytonaStateReady(state) || daytonaStateReady(server.Status),
		HasHost:    true,
		LastTouchedAt: blank(leaseLabelTimeDisplay(server.Labels["last_touched_at"]),
			server.Labels["last_touched_at"]),
		IdleFor:     idleForString(server.Labels["last_touched_at"], time.Now()),
		IdleTimeout: leaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:   blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
		Labels:      server.Labels,
	}
}

func responseExitCode(response *sdktypes.ExecuteResponse) int {
	if response == nil {
		return 1
	}
	return response.ExitCode
}
