package replicate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

const replicateCleanupTimeout = 15 * time.Second

func NewReplicateBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	applyReplicateDefaults(&cfg)
	return &replicateBackend{spec: spec, cfg: cfg, rt: rt}
}

type replicateBackend struct {
	spec   ProviderSpec
	cfg    Config
	rt     Runtime
	client replicateAPI
}

func (b replicateBackend) Spec() ProviderSpec { return b.spec }

func (b replicateBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	_ = ctx
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	if _, _, err := b.validateReadyConfig(); err != nil {
		return err
	}
	fmt.Fprintf(b.stdout(), "provider=%s ready deployment=%t version=%t mutation=false\n",
		providerName,
		strings.TrimSpace(b.cfg.Replicate.Deployment) != "",
		strings.TrimSpace(b.cfg.Replicate.Version) != "")
	if req.TimingJSON {
		return writeTimingJSON(b.stderr(), timingReport{
			Provider: providerName,
			TotalMs:  b.now().Sub(started).Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b replicateBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	_ = ctx
	if _, _, err := b.validateReadyConfig(); err != nil {
		return DoctorResult{}, err
	}
	claims, err := listReplicateLeaseClaims()
	if err != nil {
		return DoctorResult{}, err
	}
	result := inventoryDoctorResult(providerName, countReplicateClaimsForScope(claims, b.claimScope()))
	result.Message = strings.TrimSpace(result.Message + " mutation=false billing=none")
	return result, nil
}

func (b replicateBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := b.rejectRunOptions(req); err != nil {
		return RunResult{}, err
	}
	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	baseURL, _, err := b.validateReadyConfig()
	if err != nil {
		return RunResult{}, err
	}
	workdir, err := replicateWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	api, _, err := b.api()
	if err != nil {
		return RunResult{}, err
	}

	started := b.now()
	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	archiveURL := ""
	if !req.NoSync {
		archive, err := buildReplicateArchiveDataURL(ctx, b.cfg, b.rt, req.Repo, req.ForceSyncLarge)
		if err != nil {
			return RunResult{Provider: providerName, Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		archiveURL = archive.DataURL
		syncDuration = archive.Duration
		syncPhases = archive.Phases
		fmt.Fprintf(b.stderr(), "sync archive complete size=%d duration=%s\n", archive.Size, syncDuration.Round(time.Millisecond))
	}
	runnerEnv, strippedAuthEnv := replicateRunnerEnv(req.Env)
	if len(strippedAuthEnv) > 0 {
		fmt.Fprintf(b.stderr(), "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(strippedAuthEnv, ","))
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.stderr(), providerName, "forwarded", req.Options.EnvAllow, runnerEnv)
	}

	input := b.runnerInput(req, command, workdir, archiveURL, runnerEnv)
	createReq := replicateCreatePredictionRequest{
		Deployment:      strings.TrimSpace(b.cfg.Replicate.Deployment),
		Version:         strings.TrimSpace(b.cfg.Replicate.Version),
		Input:           runnerInputMap(input),
		WaitSecs:        b.cfg.Replicate.WaitSecs,
		CancelAfterSecs: b.cfg.Replicate.CancelAfterSecs,
	}
	pred, err := api.CreatePrediction(ctx, createReq)
	if err != nil {
		return RunResult{Provider: providerName, Total: b.now().Sub(started), SyncDelegated: true}, err
	}
	if strings.TrimSpace(pred.ID) == "" {
		return RunResult{Provider: providerName, Total: b.now().Sub(started), SyncDelegated: true}, exit(5, "replicate prediction create returned empty id")
	}
	leaseID := leaseIDForPrediction(pred.ID)
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Total: b.now().Sub(started), SyncDelegated: true}, b.cleanupCreateFailure(ctx, api, pred.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, replicateEndpointScope(baseURL), b.cfg.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, b.cleanupCreateFailure(ctx, api, pred.ID, err)
	}
	fmt.Fprintf(b.stderr(), "provider=%s prediction=%s lease=%s slug=%s workdir=%s\n", providerName, pred.ID, leaseID, slug, workdir)

	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Kept:           true,
		RunID:          pred.ID,
		CleanupCommand: replicateCleanupCommand(leaseID),
	}
	logOffset := 0
	b.writeNewLogs(pred, &logOffset)
	commandStarted := b.now()
	pred, err = b.pollPrediction(ctx, api, pred, &logOffset)
	commandDuration := b.now().Sub(commandStarted)
	result, runErr := b.resultFromPrediction(pred, started, commandDuration, syncDuration, session, req.Command)
	result.SyncDelegated = true
	if req.TimingJSON {
		report := timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, errors.Join(runErr, err))
		if timingErr := writeTimingJSON(b.stderr(), report); timingErr != nil {
			return result, timingErr
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			cancelCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if _, cancelErr := api.CancelPrediction(cancelCtx, pred.ID); cancelErr != nil {
				fmt.Fprintf(b.stderr(), "warning: replicate cancel failed for prediction=%s: %v\n", pred.ID, cancelErr)
			}
		}
		shouldStop := false
		handleDelegatedRunFailure(b.stderr(), req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, true, &shouldStop)
		return result, err
	}
	if runErr != nil {
		shouldStop := false
		handleDelegatedRunFailure(b.stderr(), req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, true, &shouldStop)
		return result, runErr
	}
	return result, nil
}

func (b replicateBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = ctx
	_ = req
	if _, _, err := b.validateAPIConfig(); err != nil {
		return nil, err
	}
	claims, err := listReplicateLeaseClaims()
	if err != nil {
		return nil, err
	}
	scope := b.claimScope()
	views := make([]LeaseView, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) || claim.ProviderScope != scope {
			continue
		}
		predictionID := predictionIDFromLeaseID(claim.LeaseID)
		if predictionID == "" {
			continue
		}
		slug := claim.Slug
		if strings.TrimSpace(slug) == "" {
			slug = newLeaseSlug(claim.LeaseID)
		}
		views = append(views, Server{
			Provider: providerName,
			CloudID:  predictionID,
			Name:     predictionID,
			Status:   "claimed",
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     slug,
				"pond":     claim.Pond,
				"target":   targetLinux,
				"state":    "claimed",
			},
		})
	}
	return views, nil
}

func (b replicateBackend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, baseURL, err := b.api()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, predictionID, slug, _, err := b.resolvePredictionIdentifier(req.ID, baseURL, false, "", false)
	if err != nil {
		return StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		pred, err := api.GetPrediction(pollCtx, predictionID)
		if err != nil {
			if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for replicate prediction %s to finish", predictionID)
			}
			return StatusView{}, err
		}
		view := replicateStatusView(leaseID, predictionID, slug, pred, b.claimPond(leaseID))
		if !req.Wait || predictionTerminal(pred.Status) {
			return view, nil
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for replicate prediction %s to finish", predictionID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(b.pollInterval()):
		}
	}
}

func (b replicateBackend) Stop(ctx context.Context, req StopRequest) error {
	api, baseURL, err := b.api()
	if err != nil {
		return err
	}
	leaseID, predictionID, _, claimed, err := b.resolvePredictionIdentifier(req.ID, baseURL, false, "", false)
	if err != nil {
		return err
	}
	pred, err := api.CancelPrediction(ctx, predictionID)
	if err != nil {
		return err
	}
	if claimed || predictionTerminal(pred.Status) {
		removeLeaseClaim(leaseID)
	}
	fmt.Fprintf(b.stderr(), "canceled provider=%s prediction=%s lease=%s status=%s\n", providerName, predictionID, leaseID, strings.TrimSpace(pred.Status))
	return nil
}

func (b replicateBackend) rejectRunOptions(req RunRequest) error {
	if err := rejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
		return err
	}
	if strings.TrimSpace(req.ID) != "" {
		return exit(2, "provider=replicate creates a new prediction for each run; use status/stop with --id for existing predictions")
	}
	if req.SyncOnly {
		return exit(2, "provider=replicate cannot --sync-only because predictions are one-shot runs")
	}
	if req.Preflight {
		return exit(2, "provider=replicate does not support --preflight without creating a prediction")
	}
	return nil
}

func (b replicateBackend) validateReadyConfig() (string, string, error) {
	cfg := b.cfg
	cfg.Provider = providerName
	if err := ValidateConfig(cfg); err != nil {
		return "", "", err
	}
	if err := validateRunnerTargetConfig(cfg); err != nil {
		return "", "", err
	}
	baseURL, source, err := b.validateAPIConfig()
	if err != nil {
		return "", "", err
	}
	if _, err := replicateWorkdir(cfg); err != nil {
		return "", "", err
	}
	return baseURL, source, nil
}

func (b replicateBackend) validateAPIConfig() (string, string, error) {
	cfg := b.cfg
	baseURL, err := validateReplicateAPIURL(blank(strings.TrimSpace(cfg.Replicate.APIURL), defaultAPIURL))
	if err != nil {
		return "", "", err
	}
	_, source, ok := ResolveAPIToken()
	if !ok {
		return "", "", exit(2, "provider=replicate needs an API token; load CRABBOX_REPLICATE_API_TOKEN from a secret manager or set REPLICATE_API_TOKEN")
	}
	return baseURL, source, nil
}

func (b replicateBackend) api() (replicateAPI, string, error) {
	baseURL, _, err := b.validateAPIConfig()
	if err != nil {
		return nil, "", err
	}
	if b.client != nil {
		return b.client, baseURL, nil
	}
	client, err := newReplicateClient(b.cfg, b.rt)
	if err != nil {
		return nil, "", err
	}
	return client, baseURL, nil
}

func (b replicateBackend) runnerInput(req RunRequest, command []string, workdir, archiveURL string, env map[string]string) RunnerInput {
	return RunnerInput{
		Command:      append([]string(nil), command...),
		Workdir:      workdir,
		ArchiveURL:   archiveURL,
		Env:          cloneStringMap(env),
		TimeoutSecs:  b.cfg.Replicate.ExecTimeoutSecs,
		CancelAfter:  b.cfg.Replicate.CancelAfterSecs,
		Metadata:     b.runnerMetadata(req),
		OutputSchema: "crabbox.runner.v1",
	}
}

func replicateRunnerEnv(env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var stripped []string
	for name, value := range env {
		switch name {
		case envCrabboxReplicateToken, envReplicateToken:
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	slices.Sort(stripped)
	return out, stripped
}

func (b replicateBackend) runnerMetadata(req RunRequest) map[string]string {
	metadata := map[string]string{
		"provider": "crabbox",
	}
	if strings.TrimSpace(req.Repo.Name) != "" {
		metadata["repo"] = strings.TrimSpace(req.Repo.Name)
	}
	if strings.TrimSpace(req.Label) != "" {
		metadata["label"] = strings.TrimSpace(req.Label)
	}
	return metadata
}

func runnerInputMap(input RunnerInput) map[string]any {
	payload, _ := json.Marshal(input)
	var out map[string]any
	_ = json.Unmarshal(payload, &out)
	return out
}

func (b replicateBackend) pollPrediction(ctx context.Context, api replicateAPI, pred replicatePrediction, logOffset *int) (replicatePrediction, error) {
	for !predictionTerminal(pred.Status) {
		select {
		case <-ctx.Done():
			return pred, ctx.Err()
		case <-time.After(b.pollInterval()):
		}
		next, err := api.GetPrediction(ctx, pred.ID)
		if err != nil {
			return pred, err
		}
		pred = next
		b.writeNewLogs(pred, logOffset)
	}
	return pred, nil
}

func (b replicateBackend) resultFromPrediction(pred replicatePrediction, started time.Time, commandDuration, syncDuration time.Duration, session *RunSessionHandle, command []string) (RunResult, error) {
	result := RunResult{
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       session.LeaseID,
		Slug:          session.Slug,
		CommandText:   strings.Join(command, " "),
		Session:       session,
	}
	status := strings.ToLower(strings.TrimSpace(pred.Status))
	switch status {
	case "succeeded":
		output, err := parsePredictionOutput(pred)
		if err != nil {
			result.ExitCode = 1
			return result, exit(5, "%v", err)
		}
		if output.Stdout != "" {
			_, _ = io.WriteString(b.stdout(), output.Stdout)
		}
		if output.Stderr != "" {
			_, _ = io.WriteString(b.stderr(), output.Stderr)
		}
		result.ExitCode = output.ExitCode
		if output.ExitCode != 0 {
			return result, ExitError{Code: output.ExitCode, Message: fmt.Sprintf("replicate run exited %d", output.ExitCode)}
		}
		return result, nil
	case "failed":
		result.ExitCode = 1
		return result, exit(5, "replicate prediction %s failed: %s", pred.ID, predictionErrorText(pred))
	case "canceled":
		result.ExitCode = 1
		return result, ExitError{Code: 1, Message: fmt.Sprintf("replicate prediction %s canceled", pred.ID)}
	default:
		result.ExitCode = 1
		return result, exit(5, "replicate prediction %s ended with unknown status %q after sync=%s", pred.ID, pred.Status, syncDuration.Round(time.Millisecond))
	}
}

func predictionErrorText(pred replicatePrediction) string {
	if len(pred.Error) == 0 || string(pred.Error) == "null" {
		return "no error detail"
	}
	var text string
	if err := json.Unmarshal(pred.Error, &text); err == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(string(pred.Error))
}

func (b replicateBackend) writeNewLogs(pred replicatePrediction, offset *int) {
	if offset == nil {
		return
	}
	logs := pred.Logs
	if logs == "" {
		return
	}
	if *offset > len(logs) {
		*offset = 0
	}
	if *offset < len(logs) {
		_, _ = io.WriteString(b.stderr(), logs[*offset:])
		*offset = len(logs)
	}
}

func (b replicateBackend) resolvePredictionIdentifier(identifier, baseURL string, reclaim bool, repoRoot string, updateClaim bool) (string, string, string, bool, error) {
	id := strings.TrimSpace(identifier)
	if id == "" {
		return "", "", "", false, exit(2, "provider=replicate requires a prediction id or Crabbox claim slug")
	}
	scope := replicateEndpointScope(baseURL)
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leaseIDForPrediction(id)
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", false, err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		leaseID, predictionID, slug, err := b.finishResolvedClaim(claim, scope, reclaim, repoRoot, updateClaim)
		return leaseID, predictionID, slug, true, err
	}
	claims, err := listReplicateLeaseClaims()
	if err != nil {
		return "", "", "", false, err
	}
	slugKey := normalizeLeaseSlug(id)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if claim.LeaseID == id || (slugKey != "" && normalizeLeaseSlug(claim.Slug) == slugKey) {
			leaseID, predictionID, slug, err := b.finishResolvedClaim(claim, scope, reclaim, repoRoot, updateClaim)
			return leaseID, predictionID, slug, true, err
		}
	}
	if strings.HasPrefix(id, leasePrefix) {
		return "", "", "", false, exit(4, "replicate prediction %q is not claimed by Crabbox for this API endpoint", id)
	}
	return leaseIDForPrediction(id), id, newLeaseSlug(leaseIDForPrediction(id)), false, nil
}

func (b replicateBackend) finishResolvedClaim(claim LeaseClaim, scope string, reclaim bool, repoRoot string, updateClaim bool) (string, string, string, error) {
	if claim.ProviderScope != scope {
		return "", "", "", exit(4, "replicate lease %q belongs to a different API endpoint; restore the endpoint used to create it", claim.LeaseID)
	}
	if updateClaim && repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot, b.cfg.IdleTimeout, reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, predictionIDFromLeaseID(claim.LeaseID), slug, nil
}

func (b replicateBackend) claimPond(leaseID string) string {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return ""
	}
	return claim.Pond
}

func (b replicateBackend) claimScope() string {
	baseURL, err := validateReplicateAPIURL(blank(strings.TrimSpace(b.cfg.Replicate.APIURL), defaultAPIURL))
	if err != nil {
		return ""
	}
	return replicateEndpointScope(baseURL)
}

func countReplicateClaimsForScope(claims []LeaseClaim, scope string) int {
	count := 0
	for _, claim := range claims {
		if claim.Provider == providerName && strings.HasPrefix(claim.LeaseID, leasePrefix) && claim.ProviderScope == scope {
			count++
		}
	}
	return count
}

func replicateStatusView(leaseID, predictionID, slug string, pred replicatePrediction, pond string) StatusView {
	state := strings.ToLower(strings.TrimSpace(pred.Status))
	return StatusView{
		ID:       leaseID,
		Slug:     slug,
		Provider: providerName,
		TargetOS: targetLinux,
		State:    state,
		ServerID: predictionID,
		Pond:     pond,
		Network:  networkPublic,
		Ready:    state == statusViewReady,
		Labels: map[string]string{
			"provider":   providerName,
			"lease":      leaseID,
			"prediction": predictionID,
			"state":      state,
		},
	}
}

func applyReplicateDefaults(cfg *Config) {
	defaults := DefaultConfig()
	if strings.TrimSpace(cfg.Replicate.APIURL) == "" {
		cfg.Replicate.APIURL = defaults.APIURL
	}
	if strings.TrimSpace(cfg.Replicate.Workdir) == "" {
		cfg.Replicate.Workdir = defaults.Workdir
	}
}

func replicateWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.Replicate.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "replicate workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "replicate workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func leaseIDForPrediction(predictionID string) string {
	id := strings.TrimSpace(predictionID)
	if strings.HasPrefix(id, leasePrefix) {
		return id
	}
	return leasePrefix + id
}

func predictionIDFromLeaseID(leaseID string) string {
	return strings.TrimPrefix(strings.TrimSpace(leaseID), leasePrefix)
}

func replicateCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
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

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (b replicateBackend) pollInterval() time.Duration {
	if b.cfg.Replicate.PollIntervalSecs <= 0 {
		return time.Second
	}
	return time.Duration(b.cfg.Replicate.PollIntervalSecs) * time.Second
}

func (b replicateBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b replicateBackend) stdout() io.Writer {
	if b.rt.Stdout != nil {
		return b.rt.Stdout
	}
	return io.Discard
}

func (b replicateBackend) stderr() io.Writer {
	if b.rt.Stderr != nil {
		return b.rt.Stderr
	}
	return io.Discard
}

func (b replicateBackend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), replicateCleanupTimeout)
}

func (b replicateBackend) cleanupCreateFailure(ctx context.Context, api replicateAPI, predictionID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if _, err := api.CancelPrediction(cleanupCtx, predictionID); err != nil {
		return errors.Join(cause, fmt.Errorf("replicate cleanup failed for prediction %s; cancel it in Replicate: %w", predictionID, err))
	}
	return cause
}
