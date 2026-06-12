package opensandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

type openSandboxBackend struct {
	spec                   ProviderSpec
	cfg                    Config
	rt                     Runtime
	newClient              func(Config, Runtime) (openSandboxClient, error)
	cleanupTimeoutOverride time.Duration
	statusPollOverride     time.Duration
	statusProbeOverride    time.Duration
}

func (b *openSandboxBackend) Spec() ProviderSpec { return b.spec }

func (b *openSandboxBackend) client() (openSandboxClient, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newOpenSandboxClient(b.cfg, b.rt)
}

func (b *openSandboxBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if err := validateOpenSandboxRunConfig(b.cfg); err != nil {
		return err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, sb, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
		sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
		if err != nil {
			return b.cleanupClaimedSandboxFailure(ctx, api, leaseID, sandboxID, err)
		}
	}
	deadline, err := openSandboxExpiration(sb)
	if err != nil {
		return b.cleanupClaimedSandboxFailure(ctx, api, leaseID, sandboxID, err)
	}
	required := openSandboxRunBudgetForConfig(b.cfg, false, false)
	if remaining := deadline.Sub(b.now()); remaining < required {
		return b.cleanupClaimedSandboxFailure(ctx, api, leaseID, sandboxID,
			exit(5, "opensandbox sandbox %s has %s remaining after warmup, less than the %s default run budget", sandboxID, remaining.Round(time.Second), required))
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: opensandbox warmup keeps the sandbox until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *openSandboxBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if req.ID == "" {
		if err := validateOpenSandboxRequestConfig(b.cfg, req); err != nil {
			return RunResult{}, err
		}
	}
	workdir, err := openSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	sb := sandboxInfo{}
	acquired := false
	if req.ID == "" {
		leaseID, sandboxID, slug, sb, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
		acquired = true
	} else {
		leaseID, sandboxID, slug, err = resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
		sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
		if err != nil {
			return RunResult{}, err
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return RunResult{}, err
		}
		if err := authorizeOpenSandboxRepoClaim(claim, req.Repo.Root, req.Reclaim); err != nil {
			return RunResult{}, err
		}
		if err := b.ensureReusableSandbox(ctx, api, sandboxID, sb); err != nil {
			return RunResult{}, err
		}
		_, _, slug, err = finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, api.BaseURL())
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if killErr := api.DeleteSandbox(cleanupCtx, sandboxID); killErr != nil && !isOpenSandboxNotFound(killErr) {
				fmt.Fprintf(b.rt.Stderr, "warning: opensandbox delete failed for %s: %v\n", sandboxID, killErr)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
		sb, err = verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID)
		if err != nil {
			return RunResult{}, err
		}
	}
	deadline, err := openSandboxExpiration(sb)
	if err != nil {
		return RunResult{}, err
	}
	if !deadline.After(b.now()) {
		return RunResult{}, exit(5, "opensandbox sandbox %s exceeded its absolute Crabbox TTL", sandboxID)
	}
	if remaining, required := deadline.Sub(b.now()), b.runLifetimeBudget(req); remaining < required {
		runErr := exit(5, "opensandbox sandbox %s has %s remaining before its absolute TTL, less than the %s sync/command budget; create a new sandbox", sandboxID, remaining.Round(time.Second), required)
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, runErr
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, api, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, api, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{}, err
	}

	if req.SyncOnly {
		result := RunResult{Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			return result, writeTimingJSON(b.rt.Stderr, timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      0,
				Label:         strings.TrimSpace(req.Label),
			})
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	if remaining := deadline.Sub(b.now()); remaining < b.commandLifetime() {
		runErr := exit(5, "opensandbox sandbox %s has %s remaining before its absolute TTL, less than the %s command budget; create a new sandbox", sandboxID, remaining.Round(time.Second), b.commandLifetime())
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, runErr
	}
	commandStart := b.now()
	exitCode, runErr := api.RunCommand(ctx, sandboxID, runCommandRequest{
		Command:     commandScript(command),
		Workdir:     workdir,
		Env:         req.Env,
		TimeoutSecs: b.execTimeoutSecs(),
	})
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	fmt.Fprintf(b.rt.Stderr, "opensandbox run summary sync=%s command=%s total=%s exit=%d\n",
		syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("opensandbox run failed: %v", runErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("opensandbox run exited %d", exitCode)}
	}
	return result, nil
}

func (b *openSandboxBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	claims, err := listOpenSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if validateOpenSandboxClaimScope(claim, api.BaseURL()) != nil {
			continue
		}
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" {
			continue
		}
		sb, getErr := api.GetSandbox(ctx, sandboxID)
		state := ""
		if getErr != nil {
			if isOpenSandboxNotFound(getErr) {
				state = "missing-or-inaccessible"
			} else {
				return nil, getErr
			}
		} else {
			if err := validateOpenSandboxOwnership(claim, sb); err != nil {
				return nil, err
			}
			state = blank(strings.ToLower(sb.State), statusViewReady)
		}
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  sandboxID,
			Name:     sandboxID,
			Status:   state,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"pond":     claim.Pond,
				"target":   targetLinux,
				"state":    state,
			},
		})
	}
	return servers, nil
}

func (b *openSandboxBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	if err := api.Probe(ctx); err != nil {
		return DoctorResult{}, err
	}
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(servers)), nil
}

func (b *openSandboxBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	claim, ok, err := resolveOpenSandboxLeaseClaim(leaseID, api.BaseURL())
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "opensandbox sandbox %q is not claimed by Crabbox", req.ID)
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	deadline := b.now().Add(waitTimeout)
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		sb, getErr := api.GetSandbox(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateOpenSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := strings.ToLower(strings.TrimSpace(sb.State))
		ready := false
		if isReadyState(state) {
			probeCtx, probeCancel := context.WithTimeout(pollCtx, b.statusProbeTimeout())
			pingErr := api.PingSandbox(probeCtx, sandboxID)
			probeCancel()
			ready = pingErr == nil
			if pingErr != nil && pollCtx.Err() != nil {
				if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
				}
				return StatusView{}, pollCtx.Err()
			}
			if pingErr != nil && !isOpenSandboxReadinessPending(pingErr) {
				return StatusView{}, fmt.Errorf("opensandbox status execd health: %w", pingErr)
			}
		}
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Pond:     claim.Pond,
			Network:  NetworkPublic,
			Ready:    ready,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "opensandbox sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for opensandbox sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(b.statusPollInterval()):
		}
	}
}

func (b *openSandboxBackend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := resolveLeaseID(req.ID, "", false, 0, api.BaseURL())
	if err != nil {
		return err
	}
	if _, err := verifyOpenSandboxClaim(ctx, api, leaseID, sandboxID); err != nil {
		if !isOpenSandboxNotFound(err) || !b.cfg.OpenSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing opensandbox sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isOpenSandboxNotFound(err) || !b.cfg.OpenSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing opensandbox sandbox=%s after explicit request\n", sandboxID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *openSandboxBackend) createSandbox(ctx context.Context, api openSandboxClient, repo Repo, reclaim bool, requestedSlug string) (string, string, string, sandboxInfo, error) {
	providerScope, err := newOpenSandboxClaimScope(api.BaseURL())
	if err != nil {
		return "", "", "", sandboxInfo{}, err
	}
	image := strings.TrimSpace(b.cfg.OpenSandbox.Image)
	if image == "" {
		image = defaultImage
	}
	platformOS, err := openSandboxPlatformOS(b.cfg.OpenSandbox.PlatformOS)
	if err != nil {
		return "", "", "", sandboxInfo{}, err
	}
	sb, err := api.CreateSandbox(ctx, createSandboxOptions{
		Image:          image,
		TimeoutSecs:    durationSecondsCeil(b.sandboxLifetime()),
		CPU:            b.cfg.OpenSandbox.CPU,
		Memory:         b.cfg.OpenSandbox.Memory,
		SecureAccess:   b.cfg.OpenSandbox.SecureAccess,
		UseServerProxy: b.cfg.OpenSandbox.UseServerProxy,
		PlatformOS:     platformOS,
		PlatformArch:   b.cfg.OpenSandbox.PlatformArch,
		Metadata: map[string]string{
			openSandboxClaimKey: providerScope,
			openSandboxNameKey:  newSandboxName(repo),
			"crabbox":           "true",
		},
	})
	if err != nil {
		return "", "", "", sandboxInfo{}, err
	}
	leaseID := leasePrefix + sb.ID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb.ID, "", sandboxInfo{}, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, sandboxInfo{}, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	return leaseID, sb.ID, slug, sb, nil
}

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=opensandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
	}
	claim, ok, err := resolveOpenSandboxLeaseClaim(id, baseURL)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL)
	}
	return "", "", "", exit(4, "opensandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func resolveOpenSandboxLeaseClaim(identifier, baseURL string) (LeaseClaim, bool, error) {
	claims, err := listOpenSandboxLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := validateOpenSandboxClaimScope(claim, baseURL); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == providerName && normalizeLeaseSlug(claim.Slug) == slug {
				if err := validateOpenSandboxClaimScope(claim, baseURL); err != nil {
					return LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return LeaseClaim{}, false, nil
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL string) (string, string, string, error) {
	if err := validateOpenSandboxClaimScope(claim, baseURL); err != nil {
		return "", "", "", err
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot,
			timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), slug, nil
}

func authorizeOpenSandboxRepoClaim(claim LeaseClaim, repoRoot string, reclaim bool) error {
	if repoRoot == "" || claim.RepoRoot == "" || claim.RepoRoot == repoRoot || reclaim {
		return nil
	}
	return exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, repoRoot)
}

func validateOpenSandboxClaimScope(claim LeaseClaim, baseURL string) error {
	if !strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), openSandboxEndpointScope(baseURL)+"-own-") {
		return exit(4, "opensandbox lease %q belongs to a different API endpoint; restore the endpoint used to create it", claim.LeaseID)
	}
	return nil
}

func openSandboxPlatformOS(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !strings.EqualFold(value, "linux") {
		return "", exit(2, "provider=opensandbox only supports Linux sandboxes; set openSandbox.platformOS to linux or leave it empty")
	}
	return "linux", nil
}

func newOpenSandboxClaimScope(baseURL string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate opensandbox ownership token: %v", err)
	}
	return openSandboxEndpointScope(baseURL) + "-own-" + hex.EncodeToString(token[:]), nil
}

func openSandboxEndpointScope(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return "ep-" + hex.EncodeToString(digest[:8])
}

func verifyOpenSandboxClaim(ctx context.Context, api openSandboxClient, leaseID, sandboxID string) (sandboxInfo, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return sandboxInfo{}, err
	}
	if err := validateOpenSandboxClaimScope(claim, api.BaseURL()); err != nil {
		return sandboxInfo{}, err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return sandboxInfo{}, err
	}
	if err := validateOpenSandboxOwnership(claim, sb); err != nil {
		return sandboxInfo{}, err
	}
	return sb, nil
}

func validateOpenSandboxOwnership(claim LeaseClaim, sb sandboxInfo) error {
	if sb.Metadata[openSandboxClaimKey] != claim.ProviderScope {
		return exit(4, "opensandbox sandbox %q ownership metadata does not match its local claim", sb.ID)
	}
	return nil
}

func (b *openSandboxBackend) ensureReusableSandbox(ctx context.Context, api openSandboxClient, sandboxID string, sb sandboxInfo) error {
	switch strings.ToLower(strings.TrimSpace(sb.State)) {
	case "", "running":
		return nil
	case "paused":
		fmt.Fprintf(b.rt.Stderr, "resuming opensandbox sandbox=%s\n", sandboxID)
		return api.ResumeSandbox(ctx, sandboxID)
	default:
		return exit(4, "opensandbox sandbox %q is %s and cannot be reused until it is running", sandboxID, sb.State)
	}
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		if len(command) == 1 {
			return []string{"bash", "-lc", command[0]}, nil
		}
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

func commandScript(command []string) string {
	return shellScriptFromArgv(command)
}

func openSandboxWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.OpenSandbox.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "opensandbox workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "opensandbox workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "terminated", "stopped", "failed", "error", "killed", "deleted":
		return true
	default:
		return false
	}
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	maxBase := 63 - len(namePrefix) - 1 - 6
	if maxBase < 1 {
		maxBase = 1
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "crabbox"
	}
	return namePrefix + base + "-" + randomSuffix()
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func (b *openSandboxBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *openSandboxBackend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := openSandboxCleanupTimeout
	if b.cleanupTimeoutOverride > 0 {
		timeout = b.cleanupTimeoutOverride
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (b *openSandboxBackend) cleanupCreateFailure(ctx context.Context, api openSandboxClient, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		if isOpenSandboxNotFound(err) {
			return cause
		}
		return fmt.Errorf("%w; cleanup opensandbox sandbox %s failed: %v", cause, sandboxID, err)
	}
	return cause
}

func (b *openSandboxBackend) cleanupClaimedSandboxFailure(ctx context.Context, api openSandboxClient, leaseID, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isOpenSandboxNotFound(err) {
		return fmt.Errorf("%w; cleanup opensandbox sandbox %s failed: %v", cause, sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return cause
}

func (b *openSandboxBackend) execTimeoutSecs() int {
	if b.cfg.OpenSandbox.ExecTimeoutSecs > 0 {
		return b.cfg.OpenSandbox.ExecTimeoutSecs
	}
	return openSandboxExecTimeoutSecs
}

func (b *openSandboxBackend) sandboxLifetime() time.Duration {
	return openSandboxLifetimeForConfig(b.cfg)
}

func (b *openSandboxBackend) commandLifetime() time.Duration {
	return openSandboxCommandBudgetForConfig(b.cfg)
}

func (b *openSandboxBackend) runLifetimeBudget(req RunRequest) time.Duration {
	return openSandboxRunBudgetForConfig(b.cfg, req.NoSync, req.SyncOnly)
}

func openSandboxExpiration(sb sandboxInfo) (time.Time, error) {
	if sb.ExpiresAt == nil || sb.ExpiresAt.IsZero() {
		return time.Time{}, exit(5, "opensandbox sandbox %s did not report an expiration", sb.ID)
	}
	return sb.ExpiresAt.UTC(), nil
}

func isOpenSandboxReadinessPending(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound ||
			apiErr.StatusCode == http.StatusConflict ||
			apiErr.StatusCode == http.StatusTooEarly ||
			apiErr.StatusCode == http.StatusTooManyRequests ||
			apiErr.StatusCode >= http.StatusInternalServerError
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

func (b *openSandboxBackend) statusPollInterval() time.Duration {
	if b.statusPollOverride > 0 {
		return b.statusPollOverride
	}
	return openSandboxStatusPoll
}

func (b *openSandboxBackend) statusProbeTimeout() time.Duration {
	if b.statusProbeOverride > 0 {
		return b.statusProbeOverride
	}
	return openSandboxStatusProbe
}

func durationSecondsCeil(value time.Duration) int {
	if value <= 0 {
		return 0
	}
	return int((value + time.Second - 1) / time.Second)
}
