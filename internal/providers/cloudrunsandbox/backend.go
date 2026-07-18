package cloudrunsandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return exit(2, "provider=%s does not support desktop, browser, or code-server options", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	started := b.now()
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, err := b.createSandbox(ctx, transport, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s mode=%s\n", leaseID, slug, providerName, sandboxID, transport.Mode())
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: cloud-run-sandbox warmup keeps the sandbox until explicit stop\n")
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

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if req.Options.Desktop || req.Options.Browser || req.Options.Code {
		return RunResult{}, exit(2, "provider=%s does not support desktop, browser, or code-server options", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	workdir, err := cloudRunSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		leaseID, sandboxID, slug, err = b.createSandbox(ctx, transport, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s mode=%s\n", leaseID, slug, providerName, sandboxID, transport.Mode())
		acquired = true
	} else {
		leaseID, sandboxID, slug, err = b.resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: cleanupCommand(leaseID),
	}
	if shouldStop {
		defer func() {
			if !shouldStop {
				session.Kept = true
				return
			}
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if killErr := transport.Destroy(cleanupCtx, sandboxID); killErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: cloud-run-sandbox destroy failed for %s: %v\n", sandboxID, killErr)
				session.Kept = true
				return
			}
			removeLeaseClaim(leaseID)
			session.Kept = false
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s mode=%s\n", providerName, leaseID, sandboxID, workdir, transport.Mode())

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, transport, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, transport, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
	}

	if req.SyncOnly {
		result := RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			return result, writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
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
			}, result, nil))
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true, Session: session}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStart := b.now()
	exitCode, runErr := b.execCommand(ctx, transport, sandboxID, workdir, command, req.Env, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   strings.Join(req.Command, " "),
		Session:       session,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "cloud-run-sandbox run summary sync_skipped=true command=%s total=%s exit=%d\n",
			result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "cloud-run-sandbox run summary sync=%s command=%s total=%s exit=%d\n",
			syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
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
		}, result, runErr)); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: 1, Message: fmt.Sprintf("cloud-run-sandbox run failed: %v", runErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("cloud-run-sandbox run exited %d", exitCode)}
	}
	return result, nil
}

func cleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	_ = ctx
	scope, err := b.claimScope()
	if err != nil {
		return nil, err
	}
	claims, err := listCloudRunSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			continue
		}
		if claim.ProviderScope != "" && claim.ProviderScope != scope {
			continue
		}
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" {
			continue
		}
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  sandboxID,
			Name:     sandboxID,
			Status:   statusViewReady,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"pond":     claim.Pond,
				"target":   targetLinux,
				"state":    statusViewReady,
			},
		})
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{Provider: providerName}
	healthErr := transport.Health(ctx)
	details := map[string]string{
		"mode":    transport.Mode(),
		"cli":     blank(strings.TrimSpace(b.cfg.CloudRunSandbox.CLIPath), defaultCLIPath),
		"workdir": blank(strings.TrimSpace(b.cfg.CloudRunSandbox.Workdir), defaultWorkdir),
	}
	if transport.Mode() == "remote" {
		details["gateway"] = strings.TrimSpace(b.cfg.CloudRunSandbox.GatewayURL)
	}
	result.Checks = append(result.Checks, doctorCheck("control_plane", healthErr, details))
	servers, listErr := b.List(ctx, ListRequest{})
	if listErr != nil {
		result.Checks = append(result.Checks, doctorCheck("local_claims", listErr, nil))
	} else {
		result.Checks = append(result.Checks, DoctorCheck{
			Status:  "ok",
			Check:   "local_claims",
			Message: fmt.Sprintf("%d claimed sandboxes", len(servers)),
			Details: map[string]string{"count": fmt.Sprint(len(servers))},
		})
	}
	if healthErr != nil {
		result.Status = "error"
		result.Message = fmt.Sprintf("mode=%s control_plane=blocked mutation=false", transport.Mode())
		return result, healthErr
	}
	result.Status = "ok"
	result.Message = fmt.Sprintf("mode=%s control_plane=ready mutation=false claims=%d", transport.Mode(), len(servers))
	return result, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	_ = ctx
	leaseID, sandboxID, slug, err := b.resolveLeaseID(req.ID, "", false)
	if err != nil {
		return StatusView{}, err
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return StatusView{}, err
	}
	return StatusView{
		ID:       leaseID,
		Slug:     slug,
		Provider: providerName,
		TargetOS: targetLinux,
		State:    statusViewReady,
		ServerID: sandboxID,
		Pond:     claim.Pond,
		Network:  NetworkPublic,
		Ready:    true,
		Labels: map[string]string{
			"provider": providerName,
			"lease":    leaseID,
			"pond":     claim.Pond,
			"state":    statusViewReady,
		},
	}, nil
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := b.resolveLeaseID(req.ID, "", false)
	if err != nil {
		return err
	}
	if err := transport.Destroy(ctx, sandboxID); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	transport, err := newTransport(b.cfg, b.rt)
	if err != nil {
		return err
	}
	scope, err := b.claimScope()
	if err != nil {
		return err
	}
	claims, err := listCloudRunSandboxLeaseClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked, removed, claimsRemoved := 0, 0, 0
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if claim.ProviderScope != "" && claim.ProviderScope != scope {
			continue
		}
		checked++
		sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
		if sandboxID == "" || sandboxID == claim.LeaseID {
			continue
		}
		due, reason := claimCleanupDue(claim, now)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := transport.Destroy(ctx, sandboxID); err != nil && !isNotFoundDetail(err.Error()) {
			fmt.Fprintf(b.rt.Stderr, "warning: destroy sandbox=%s failed: %v\n", sandboxID, err)
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		removed++
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return nil
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "no-idle-timeout"
	}
	lastUsed := strings.TrimSpace(claim.LastUsedAt)
	if lastUsed == "" {
		lastUsed = strings.TrimSpace(claim.ClaimedAt)
	}
	if lastUsed == "" {
		return true, "missing-timestamps"
	}
	parsed, err := time.Parse(time.RFC3339, lastUsed)
	if err != nil {
		// Accept common claim timestamp formats.
		parsed, err = time.Parse(time.RFC3339Nano, lastUsed)
		if err != nil {
			return true, "unparseable-timestamp"
		}
	}
	deadline := parsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle-timeout-remaining"
	}
	return true, "idle-timeout-expired"
}

func (b *backend) createSandbox(ctx context.Context, transport sandboxTransport, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	sandboxID := newSandboxName(repo)
	leaseID := leasePrefix + sandboxID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", err
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", "", err
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, scope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return "", "", "", err
	}
	claimed := true
	defer func() {
		if claimed {
			removeLeaseClaim(leaseID)
		}
	}()
	workdir, err := cloudRunSandboxWorkdir(b.cfg)
	if err != nil {
		return "", "", "", err
	}
	if err := transport.Create(ctx, sandboxID, runOptions{
		AllowEgress: b.cfg.CloudRunSandbox.AllowEgress,
		Write:       b.cfg.CloudRunSandbox.Write,
		Rootfs:      b.cfg.CloudRunSandbox.Rootfs,
		Workdir:     workdir,
	}); err != nil {
		return "", "", "", err
	}
	claimed = false
	return leaseID, sandboxID, slug, nil
}

func (b *backend) resolveLeaseID(id, repoRoot string, reclaim bool) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "missing lease id")
	}
	scope, err := b.claimScope()
	if err != nil {
		return "", "", "", err
	}
	if claim, ok, err := resolveLeaseClaim(id); err != nil {
		return "", "", "", err
	} else if ok && claim.Provider == providerName {
		if claim.ProviderScope != "" && claim.ProviderScope != scope {
			return "", "", "", exit(4, "cloud-run-sandbox lease %q belongs to a different gateway/cli scope", id)
		}
		if repoRoot != "" {
			if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, scope, claim.Pond, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
				return "", "", "", err
			}
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), claim.Slug, nil
	}
	// Accept raw sandbox id when an exact claim exists.
	leaseID := id
	if !strings.HasPrefix(leaseID, leasePrefix) {
		leaseID = leasePrefix + id
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return "", "", "", exit(4, "cloud-run-sandbox sandbox %q is not claimed by Crabbox", id)
	}
	if claim.Provider != providerName {
		return "", "", "", exit(4, "cloud-run-sandbox sandbox %q is not claimed by Crabbox", id)
	}
	if claim.ProviderScope != "" && claim.ProviderScope != scope {
		return "", "", "", exit(4, "cloud-run-sandbox lease %q belongs to a different gateway/cli scope", id)
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, scope, claim.Pond, repoRoot, time.Duration(claim.IdleTimeoutSeconds)*time.Second, reclaim); err != nil {
			return "", "", "", err
		}
	}
	return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), claim.Slug, nil
}

func (b *backend) claimScope() (string, error) {
	if gateway := strings.TrimSpace(b.cfg.CloudRunSandbox.GatewayURL); gateway != "" {
		validated, err := validateGatewayURL(gateway)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(validated))
		return "gateway:" + hex.EncodeToString(sum[:8]), nil
	}
	cli := blank(strings.TrimSpace(b.cfg.CloudRunSandbox.CLIPath), defaultCLIPath)
	sum := sha256.Sum256([]byte("direct:" + cli))
	return "direct:" + hex.EncodeToString(sum[:8]), nil
}

func (b *backend) now() time.Time {
	return time.Now()
}

func (b *backend) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
}

func cloudRunSandboxWorkdir(cfg Config) (string, error) {
	workdir := blank(strings.TrimSpace(cfg.CloudRunSandbox.Workdir), defaultWorkdir)
	if !path.IsAbs(workdir) {
		return "", exit(2, "cloudRunSandbox.workdir must be an absolute path")
	}
	return workdir, nil
}

func newSandboxName(repo Repo) string {
	base := namePrefix
	if name := sanitizeName(path.Base(repo.Root)); name != "" {
		base = namePrefix + name + "-"
	}
	suffix := randomSuffix()
	maxBase := maxSandboxNameLen - len(suffix)
	if maxBase < len(namePrefix) {
		maxBase = len(namePrefix)
	}
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	if !strings.HasSuffix(base, "-") {
		base += "-"
	}
	return base + suffix
}

func sanitizeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	re := regexp.MustCompile(`[^a-z0-9-]+`)
	value = re.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func randomSuffix() string {
	var buf [sandboxNameSuffix]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	}
	return hex.EncodeToString(buf[:])
}

func doctorCheck(name string, err error, details map[string]string) DoctorCheck {
	if err != nil {
		return DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}
