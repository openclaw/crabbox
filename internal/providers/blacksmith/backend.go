package blacksmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/tailbuffer"
)

const targetLinux = core.TargetLinux

func RegisterBlacksmithProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterBlacksmithConfigFlags(fs, defaults.Blacksmith)
}

func ApplyBlacksmithProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if v, ok := values.(core.BlacksmithConfigFlagValues); ok {
		applied, err := v.Apply(&cfg.Blacksmith, fs)
		core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "blacksmith-testbox")
		return err
	}
	return nil
}

func NewBlacksmithBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = blacksmithTestboxProvider
	return &blacksmithBackend{spec: spec, cfg: cfg, rt: rt}
}

type blacksmithBackend struct {
	spec  core.ProviderSpec
	cfg   core.Config
	rt    core.Runtime
	route *blacksmithRoute
	claim *core.LeaseClaim
}

var _ core.RunOptionsValidator = (*blacksmithBackend)(nil)

func (b *blacksmithBackend) Spec() core.ProviderSpec { return b.spec }

func (b *blacksmithBackend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	if req.ActionsRunner {
		return core.Exit(2, "--actions-runner is not supported for provider=%s; Blacksmith owns runner hydration", b.cfg.Provider)
	}
	started := b.rt.Clock.Now()
	claim, err := b.warmupLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	leaseID, slug := claim.LeaseID, claim.Slug
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s idle_timeout=%s\n", leaseID, slug, blacksmithTestboxProvider, blacksmithIdleTimeout(b.cfg))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: blacksmith warmup keeps the testbox until idle timeout or explicit stop\n")
	}
	if req.BeforeComplete != nil {
		req.BeforeComplete()
	}
	total := b.rt.Clock.Now().Sub(started)
	return shared.CompleteWarmup(b.rt, req.TimingJSON, shared.WarmupCompletion{
		Provider: blacksmithTestboxProvider,
		LeaseID:  leaseID,
		Slug:     slug,
		Total:    total,
	})
}

func (b *blacksmithBackend) ValidateRunOptions(req core.RunRequest) error {
	return validateBlacksmithRunOptions(b.spec, req)
}

func validateBlacksmithRunOptions(spec core.ProviderSpec, req core.RunRequest) error {
	if req.NoSync {
		return core.Exit(2, "%s delegates sync; --no-sync is not supported", blacksmithTestboxProvider)
	}
	if err := core.ValidateLocalCommandProcessGroupJoin(context.Background()); err != nil {
		return core.Exit(2, "Blacksmith command ownership: %v", err)
	}
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}

func (b *blacksmithBackend) Run(ctx context.Context, req core.RunRequest) (runResult core.RunResult, runErr error) {
	if err := b.ValidateRunOptions(req); err != nil {
		return core.RunResult{}, err
	}
	if err := validateBlacksmithNativeSyncScope(req.Repo.Root); err != nil {
		return core.RunResult{}, err
	}
	if len(req.ArtifactGlobs) == 0 && len(req.RequiredArtifactGlobs) == 0 {
		if err := validateBlacksmithNativeSyncScope(""); err != nil {
			return core.RunResult{}, err
		}
	}
	if err := core.ValidateRunArtifactGlobs(req.ArtifactGlobs); err != nil {
		return core.RunResult{}, err
	}
	if err := core.ValidateRequiredRunArtifactGlobs(req.RequiredArtifactGlobs); err != nil {
		return core.RunResult{}, err
	}
	if blacksmithEnvForwardingRequested(req) {
		core.PrintEnvForwardingSummary(b.rt.Stderr, blacksmithTestboxProvider, "unsupported", req.Options.EnvAllow, req.Env)
		fmt.Fprintf(b.rt.Stderr, "env forwarding note=blacksmith-testbox delegates execution to the Blacksmith CLI; configure secrets in the Testbox workflow instead\n")
		return core.RunResult{}, core.Exit(2, "env forwarding is unsupported for provider=%s; configure secrets in the provider workflow or use an SSH-backed provider", blacksmithTestboxProvider)
	}
	hiddenOmissions, sparseErr := core.GitCheckoutHasHiddenOmissions(req.Repo.Root)
	if sparseErr != nil {
		return core.RunResult{}, core.Exit(
			2,
			"provider=%s cannot verify sparse-checkout safety: %v; run from a materialized full checkout",
			blacksmithTestboxProvider,
			sparseErr,
		)
	}
	if hiddenOmissions {
		return core.RunResult{}, core.Exit(
			2,
			"provider=%s cannot safely delegate sync while Git omits tracked paths from the working tree; run from a materialized full checkout",
			blacksmithTestboxProvider,
		)
	}
	started := b.rt.Clock.Now()
	var claim core.LeaseClaim
	acquired := false
	var err error
	if req.ID == "" {
		claim, err = b.warmupLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return core.RunResult{}, err
		}
		route, _, _ := blacksmithClaimBinding(claim)
		bound := *b
		bound.route, bound.claim = &route, &claim
		b = &bound
		acquired = true
	} else {
		b, claim, err = b.ownedTestbox(ctx, req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return core.RunResult{}, err
		}
	}
	leaseID, slug := claim.LeaseID, claim.Slug
	shouldStop := acquired && !req.Keep
	cleanupAttempted := false
	cleanup := func() error {
		if !shouldStop || cleanupAttempted {
			return nil
		}
		cleanupAttempted = true
		if err := b.stopClaimedTestbox(context.Background(), leaseID, claim); err != nil {
			shouldStop = false
			fmt.Fprintf(b.rt.Stderr, "warning: blacksmith cleanup failed stage=cleanup lease=%s retry_likely=true: %v\n", leaseID, err)
			return err
		}
		return nil
	}
	defer func() {
		runResult, runErr = shared.PinDelegatedRunFailure(runResult, runErr)
		if err := cleanup(); err != nil {
			if runResult.Session == nil {
				runResult.Provider, runResult.LeaseID, runResult.Slug = blacksmithTestboxProvider, leaseID, slug
				runResult.Session = &core.RunSessionHandle{Provider: blacksmithTestboxProvider, LeaseID: leaseID, Slug: slug, CleanupCommand: fmt.Sprintf("crabbox stop --provider %s %s", blacksmithTestboxProvider, leaseID)}
			}
			runResult.Session.Kept = true
			runResult, runErr = shared.AppendDelegatedRunFailure(runResult, runErr, blacksmithRunDiagnostic(1, err), 1)
		}
	}()
	fmt.Fprintf(b.rt.Stderr, "provider=blacksmith-testbox id=%s sync=delegated auth=blacksmith\n", leaseID)
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		core.PrintEnvForwardingSummary(b.rt.Stderr, blacksmithTestboxProvider, "unsupported", req.Options.EnvAllow, req.Env)
		fmt.Fprintf(b.rt.Stderr, "env forwarding note=blacksmith-testbox delegates execution to the Blacksmith CLI; configure secrets in the Testbox workflow instead\n")
	}
	stdoutCapture, stdoutCapturePath, stdoutCleanup, err := b.openFailureStreamCapture("stdout")
	if err != nil {
		return core.RunResult{}, err
	}
	defer stdoutCleanup()
	stderrCapture, stderrCapturePath, stderrCleanup, err := b.openFailureStreamCapture("stderr")
	if err != nil {
		return core.RunResult{}, err
	}
	defer stderrCleanup()
	stdoutProof := newBlacksmithProofTailBuffer()
	stderrProof := newBlacksmithProofTailBuffer()
	commandStart := b.rt.Clock.Now()
	req.Observation.Phase(core.RunPhaseCommand)
	phaseTracker := core.NewCommandPhaseTracker(commandStart)
	var outcome blacksmithRunOutcome
	var commandEnd time.Time
	var collected []core.RunArtifact
	var artifactErr error
	if err := b.withOwnedTestbox(ctx, claim, func() error {
		if len(req.ArtifactGlobs) > 0 || len(req.RequiredArtifactGlobs) > 0 {
			outcome, commandEnd, collected, artifactErr = b.runArtifactTestbox(ctx, req, leaseID, phaseTracker,
				mergeWriters(stdoutCapture, stdoutProof), mergeWriters(stderrCapture, stderrProof), blacksmithCollectionTimeout)
			return nil
		}
		outcome = b.runTestbox(
			ctx,
			leaseID,
			req.Command,
			req.DebugSync,
			req.ShellMode,
			phaseTracker,
			mergeWriters(stdoutCapture, stdoutProof),
			mergeWriters(stderrCapture, stderrProof),
			req.Observation,
		)
		return nil
	}); err != nil {
		return core.RunResult{}, err
	}
	// Artifact diagnostics must not reclassify an earlier workload failure.
	code := outcome.code
	result := core.RunResult{ExitCode: code}
	primaryErr := blacksmithRunDiagnostic(code, outcome.err)
	if outcome.syncTimedOut {
		result.Status, result.ErrorKind = core.RunStatusTimedOut, core.RunErrorTimeout
	} else if outcome.workloadObserved && code != 0 {
		result = core.FinalizeRunResult(result, nil)
	}
	if primaryErr == nil && code != 0 && (outcome.workloadObserved || outcome.syncTimedOut || artifactErr == nil) {
		primaryErr = core.Exit(code, "blacksmith testbox run exited %d", code)
		result = core.FinalizeRunResult(result, nil)
	}
	result, primaryErr = shared.PinDelegatedRunFailure(result, primaryErr)
	artifactFailedSuccess := artifactErr != nil && code == 0
	if artifactErr != nil {
		artifactCode := core.ExitCodeForError(artifactErr, 7)
		if primaryErr == nil && code != 0 {
			artifactCode = code
		}
		diagnostic := blacksmithRunDiagnostic(artifactCode, fmt.Errorf("blacksmith artifact retrieval failed: %w", artifactErr))
		if primaryErr == nil {
			result, primaryErr = shared.PinDelegatedRunFailure(result, diagnostic)
		} else {
			result, primaryErr = shared.AppendDelegatedRunFailure(result, primaryErr, diagnostic, 7)
		}
	}
	code = result.ExitCode
	// Only command/artifact failures activate keep-on-failure. Local capture
	// and reporting errors must not retain a successful one-shot lease.
	retainForFailure := code != 0
	if primaryErr != nil {
		if code == 255 && !outcome.workloadObserved {
			primaryErr = shared.ExitErrorWithCause(code, "native exit 255; workload exit unconfirmed: "+primaryErr.Error(), primaryErr)
		}
		// A broken console must not prevent either owned capture from retaining
		// the safe primary diagnostic before the streams close.
		var writeErr error
		for _, writer := range []io.Writer{stderrCapture, stderrProof, b.rt.Stderr} {
			_, err := fmt.Fprintln(writer, primaryErr)
			writeErr = errors.Join(writeErr, err)
		}
		result, primaryErr = shared.AppendDelegatedRunFailure(result, primaryErr, blacksmithRunDiagnostic(1, writeErr), 1)
	}
	appendFailure := func(stage string, err error, firstCode int) {
		if err != nil {
			diagnostic := blacksmithRunDiagnostic(firstCode, fmt.Errorf("blacksmith %s: %w", stage, err))
			result, primaryErr = shared.AppendDelegatedRunFailure(result, primaryErr, diagnostic, firstCode)
		}
	}
	// Close both streams before publishing evidence, even if one close fails.
	appendFailure("failure capture close", errors.Join(stdoutCapture.Close(), stderrCapture.Close()), 2)
	code = result.ExitCode
	finished := b.rt.Clock.Now()
	if commandEnd.IsZero() {
		commandEnd = finished
	}
	commandDuration := commandEnd.Sub(commandStart)
	commandPhases := core.FinishCommandPhaseTracker(phaseTracker, commandEnd)
	total := finished.Sub(started)
	actionsURL := shared.FirstNonBlank(stdoutProof.ActionsURL(), stderrProof.ActionsURL())
	result = core.RunResult{
		Provider:      blacksmithTestboxProvider,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   blacksmithCommandString(req.Command, req.ShellMode),
		LogExcerpt:    core.SelectProofLogExcerpt(strings.TrimSpace(string(stdoutProof.Bytes()) + "\n" + string(stderrProof.Bytes()))),
		ActionsURL:    shared.FirstNonBlank(actionsURL, firstBlacksmithActionsURL(string(stdoutProof.Bytes())+"\n"+string(stderrProof.Bytes()))),
		ExitCode:      code,
		Status:        result.Status,
		ErrorKind:     result.ErrorKind,
		Command:       commandDuration,
		Total:         total,
		SyncDelegated: true,
	}
	for _, artifact := range collected {
		_, err := fmt.Fprintf(b.rt.Stderr, "artifact kind=%s path=%s bytes=%d\n", artifact.Kind, artifact.Path, artifact.Bytes)
		appendFailure("artifact report", err, 1)
	}
	result.Artifacts = append(result.Artifacts, collected...)
	if retainForFailure {
		core.HandleDelegatedRunFailure(b.rt.Stderr, req, blacksmithTestboxProvider, leaseID, slug, blacksmithIdleTimeout(b.cfg), b.cfg.TTL, acquired, &shouldStop)
	}
	cleanupErr := cleanup()
	cleanupFailedSuccess := cleanupErr != nil && result.ExitCode == 0
	appendFailure("cleanup", cleanupErr, 1)
	if cleanupErr == nil && cleanupAttempted && result.ExitCode == 0 {
		appendFailure("one-shot report", printBlacksmithOneShotActionsWarning(b.rt.Stderr, result.ActionsURL), 1)
	}
	// Custody is settled before reporting; an output failure must still return
	// the retained/reused lease's cleanup handle.
	result.Session = &core.RunSessionHandle{
		Provider:       blacksmithTestboxProvider,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		ActionsURL:     result.ActionsURL,
		RunID:          blacksmithActionsRunID(result.ActionsURL),
		CleanupCommand: fmt.Sprintf("crabbox stop --provider %s %s", blacksmithTestboxProvider, leaseID),
	}
	if cleanupErr != nil {
		// Cleanup recovery must not replay the command. Its observed exit remains
		// separate from workload confirmation and the final failed run outcome.
		_, err := fmt.Fprintf(b.rt.Stderr, "blacksmith cleanup recovery observed_run_exit=%d workload_exit_confirmed=%t; retry cleanup only: %s\n", outcome.code, outcome.workloadObserved, result.Session.CleanupCommand)
		appendFailure("cleanup recovery report", err, 1)
	}
	total = b.rt.Clock.Now().Sub(started)
	result.Total = total
	code = result.ExitCode
	report := delegatedTimingReport(blacksmithTestboxProvider, leaseID, slug, "blacksmith-testbox owns sync", commandDuration, commandPhases, total, code)
	if outcome.syncTimedOut {
		report.BlockedStage, report.RetryLikely = "sync", "true"
	} else if code != 0 {
		classificationInput := string(stdoutProof.Bytes()) + "\n" + string(stderrProof.Bytes())
		failurePhases := commandPhases
		if artifactFailedSuccess {
			classificationInput = artifactErr.Error()
			failurePhases = nil
		}
		classification := core.ClassifyRunFailure(code, classificationInput, failurePhases)
		core.ApplyFailureClassification(&report, classification)
	}
	if cleanupFailedSuccess {
		report.BlockedStage, report.RetryLikely = "cleanup", "true"
	}
	_, summaryErr := fmt.Fprintf(b.rt.Stderr, "blacksmith run summary sync=delegated command=%s total=%s exit=%d%s\n", commandDuration.Round(time.Millisecond), total.Round(time.Millisecond), code, core.FormatFailureClassificationFields(core.FailureClassification{BlockedStage: report.BlockedStage, RetryLikely: report.RetryLikely}))
	appendFailure("summary", summaryErr, 1)
	report.Label = strings.TrimSpace(req.Label)
	// Each independent destination gets the outcome known at its write. A
	// failed destination cannot skip later evidence or rewrite settled custody.
	currentReport := func() core.TimingReport {
		snapshot := report
		snapshot.ExitCode = result.ExitCode
		snapshot.RunStatus, snapshot.ErrorKind = result.Status, result.ErrorKind
		return core.TimingReportWithRunResult(snapshot, result, primaryErr)
	}
	if req.TimingJSON {
		appendFailure("timing report", core.WriteTimingJSON(b.rt.Stderr, currentReport()), 1)
	}
	proof, proofErr := b.blacksmithProofResult(req, leaseID, slug, result.ExitCode, commandDuration, total, currentReport(), stdoutProof.Bytes(), stderrProof.Bytes(), actionsURL)
	appendFailure("proof", proofErr, core.ExitCodeForError(proofErr, 1))
	if proofErr == nil {
		result.Provider = proof.Provider
		result.LeaseID = proof.LeaseID
		result.Slug = proof.Slug
		result.CommandText = proof.CommandText
		result.LogExcerpt = proof.LogExcerpt
		result.ActionsURL = proof.ActionsURL
		result.Artifacts = append(result.Artifacts, proof.Artifacts...)
	}
	if result.ExitCode != 0 {
		local, bytes, bundleErr := core.CaptureLocalFailureBundle(leaseID, core.FailureCaptureMetadata{
			Provider:   blacksmithTestboxProvider,
			LeaseID:    leaseID,
			Slug:       slug,
			Workdir:    "blacksmith-testbox",
			ExitCode:   result.ExitCode,
			Timing:     currentReport(),
			EnvAllow:   req.Options.EnvAllow,
			Env:        req.Env,
			Config:     b.cfg,
			StdoutPath: stdoutCapturePath,
			StderrPath: stderrCapturePath,
		})
		appendFailure("failure bundle", bundleErr, core.ExitCodeForError(bundleErr, 1))
		if bundleErr != nil {
			_, err := fmt.Fprintf(b.rt.Stderr, "warning: failure bundle failed: %v\n", blacksmithRunDiagnostic(1, bundleErr))
			appendFailure("failure bundle diagnostic", err, 1)
		} else {
			_, err := fmt.Fprintf(b.rt.Stderr, "failure-bundle local=%s bytes=%d secret_risk=caller-redacts-before-sharing\n", local, bytes)
			appendFailure("failure bundle report", err, 1)
		}
	}
	return result, primaryErr
}

func printBlacksmithOneShotActionsWarning(w io.Writer, actionsURL string) error {
	if w == nil {
		return nil
	}
	message := "blacksmith proof note: stopped one-shot Testbox after success; the backing GitHub Actions run may show a cancelled Testbox step because Blacksmith owns the delegated session lifecycle"
	if strings.TrimSpace(actionsURL) != "" {
		message += " actions=" + strings.TrimSpace(actionsURL)
	}
	_, err := fmt.Fprintln(w, message)
	return err
}

var githubActionsRunURLPattern = regexp.MustCompile(`https://github\.com/[^\s"'<>]+/actions/runs/[0-9]+[^\s"'<>]*`)

func blacksmithEnvForwardingRequested(req core.RunRequest) bool {
	return req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != ""
}

const (
	blacksmithProofStreamCaptureBytes              = 1024 * 1024
	blacksmithCommandCaptureBytes                  = 16 * 1024 * 1024
	blacksmithCleanupTimeout                       = 30 * time.Second
	blacksmithArtifactDiagnosticCaptureBytes int64 = 64 * 1024
)

type blacksmithProofTailBuffer struct {
	mu         sync.Mutex
	data       tailbuffer.Buffer
	scanTail   tailbuffer.Buffer
	actionsURL string
	fullChunk  bool
}

func newBlacksmithProofTailBuffer() *blacksmithProofTailBuffer {
	return &blacksmithProofTailBuffer{
		data:     tailbuffer.NewLimited(blacksmithProofStreamCaptureBytes),
		scanTail: tailbuffer.NewLimited(2048),
	}
}

func (b *blacksmithProofTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.actionsURL == "" {
		probe := b.scanTail.String() + string(p)
		if match := firstBlacksmithActionsURL(probe); match != "" {
			b.actionsURL = match
		}
		_, _ = b.scanTail.Write(p)
	}
	// Proof artifacts historically mark a full-sized incoming chunk, even
	// when it exactly fills an empty tail without discarding earlier bytes.
	b.fullChunk = b.fullChunk || len(p) >= blacksmithProofStreamCaptureBytes
	return b.data.Write(p)
}

func (b *blacksmithProofTailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := bytes.Clone(b.data.Bytes())
	if !b.fullChunk && !b.data.Exceeded() {
		return data
	}
	prefix := fmt.Appendf(nil, "[crabbox: proof stream kept last %d bytes]\n", blacksmithProofStreamCaptureBytes)
	return append(prefix, data...)
}

func (b *blacksmithProofTailBuffer) ActionsURL() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.actionsURL
}

func (b *blacksmithBackend) blacksmithProofResult(req core.RunRequest, leaseID, slug string, exitCode int, commandDuration, total time.Duration, report core.TimingReport, stdoutData, stderrData []byte, actionsURL string) (core.RunResult, error) {
	combined := strings.TrimSpace(string(stdoutData) + "\n" + string(stderrData))
	result := core.RunResult{
		Provider:    blacksmithTestboxProvider,
		LeaseID:     leaseID,
		Slug:        slug,
		CommandText: blacksmithCommandString(req.Command, req.ShellMode),
		LogExcerpt:  core.SelectProofLogExcerpt(combined),
		ActionsURL:  shared.FirstNonBlank(actionsURL, firstBlacksmithActionsURL(combined)),
	}
	if strings.TrimSpace(req.EmitProof) == "" {
		return result, nil
	}
	artifacts, err := persistBlacksmithRunArtifacts(req.Repo.Root, leaseID, exitCode, commandDuration, total, report, stdoutData, stderrData, result)
	if err != nil {
		return core.RunResult{}, err
	}
	result.Artifacts = artifacts
	return result, nil
}

func firstBlacksmithActionsURL(text string) string {
	for _, candidate := range githubActionsRunURLPattern.FindAllString(text, -1) {
		normalized := normalizeBlacksmithActionsURL(candidate)
		if blacksmithActionsRunID(normalized) != "" {
			return normalized
		}
	}
	return ""
}

func normalizeBlacksmithActionsURL(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	for candidate != "" {
		trimmed := strings.TrimRight(candidate, ".,;:)]}")
		if trimmed == candidate {
			return candidate
		}
		candidate = trimmed
		if blacksmithActionsRunID(candidate) != "" {
			return candidate
		}
	}
	return ""
}

func blacksmithActionsRunID(actionsURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(actionsURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+2 < len(segments); i++ {
		if segments[i] != "actions" || segments[i+1] != "runs" {
			continue
		}
		runID := segments[i+2]
		if runID == "" {
			return ""
		}
		for _, r := range runID {
			if r < '0' || r > '9' {
				return ""
			}
		}
		return runID
	}
	return ""
}

func persistBlacksmithRunArtifacts(repoRoot, leaseID string, exitCode int, commandDuration, total time.Duration, report core.TimingReport, stdoutData, stderrData []byte, result core.RunResult) ([]core.RunArtifact, error) {
	metadata := map[string]any{
		"provider":      blacksmithTestboxProvider,
		"leaseId":       leaseID,
		"slug":          result.Slug,
		"command":       result.CommandText,
		"exitCode":      exitCode,
		"commandMs":     commandDuration.Milliseconds(),
		"totalMs":       total.Milliseconds(),
		"actionsRunUrl": result.ActionsURL,
	}
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, err
	}
	timingJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	files := []struct {
		kind string
		name string
		data []byte
	}{
		{kind: "stdout", name: "blacksmith.stdout.log", data: stdoutData},
		{kind: "stderr", name: "blacksmith.stderr.log", data: stderrData},
		{kind: "timing", name: "timing.json", data: append(timingJSON, '\n')},
		{kind: "metadata", name: "metadata.json", data: append(metadataJSON, '\n')},
	}
	artifacts := make([]core.RunArtifact, 0, len(files))
	for _, file := range files {
		path := core.LocalRunArtifactPath(repoRoot, "", leaseID, file.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, core.Exit(2, "blacksmith proof artifact create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, file.data, 0o600); err != nil {
			return nil, core.Exit(2, "blacksmith proof artifact write %s: %v", path, err)
		}
		artifacts = append(artifacts, core.RunArtifact{Kind: file.kind, Path: path, Bytes: len(file.data)})
	}
	return artifacts, nil
}

func (b *blacksmithBackend) List(ctx context.Context, req core.ListRequest) ([]core.Server, error) {
	out, err := b.commandOutput(ctx, b.listArgs(req))
	if err != nil {
		return nil, err
	}
	items := parseBlacksmithList(out)
	servers := make([]core.Server, 0, len(items))
	for _, item := range items {
		servers = append(servers, blacksmithItemToServer(item))
	}
	return servers, nil
}

func (b *blacksmithBackend) ListJSON(ctx context.Context, req core.ListRequest) (any, error) {
	out, err := b.commandOutput(ctx, b.listArgs(req))
	if err != nil {
		return nil, err
	}
	return parseBlacksmithList(out), nil
}

func (b *blacksmithBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{All: true})
	if err != nil {
		return core.DoctorResult{}, err
	}
	activeLeases := 0
	for _, server := range servers {
		// Blacksmith's all-org inventory may include terminal rows. Unknown states
		// count as active so admission fails conservatively when the CLI evolves.
		switch strings.ToLower(strings.TrimSpace(server.Status)) {
		case "completed", "cancelled", "canceled", "failed", "released", "stopped":
		default:
			activeLeases++
		}
	}
	return core.DoctorResult{
		Provider: blacksmithTestboxProvider,
		Message: fmt.Sprintf(
			"cli=ready control_plane=ready inventory=ready inventory_scope=all api=list mutation=false leases=%d active_leases=%d inventory_rows=%d runtime=ci_hydrated_by_provider",
			activeLeases,
			activeLeases,
			len(servers),
		),
	}, nil
}

func delegatedTimingReport(provider, leaseID, slug, syncReason string, commandDuration time.Duration, commandPhases []core.TimingPhase, total time.Duration, exitCode int) core.TimingReport {
	return core.TimingReport{
		Provider:      provider,
		LeaseID:       leaseID,
		Slug:          slug,
		SyncPhases:    []core.TimingPhase{{Name: "delegated", Skipped: true, Reason: syncReason}},
		SyncDelegated: true,
		CommandMs:     commandDuration.Milliseconds(),
		CommandPhases: commandPhases,
		TotalMs:       total.Milliseconds(),
		ExitCode:      exitCode,
	}
}

func (b *blacksmithBackend) listArgs(req core.ListRequest) []string {
	if req.All {
		return blacksmithListAllArgs(b.cfg)
	}
	return blacksmithListArgs(b.cfg)
}

func (b *blacksmithBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	// Bound route discovery as well as each snapshot. Waiting retains its
	// existing readiness semantics; it never waits for remote settlement.
	if !req.Wait {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, blacksmithStatusReadTimeout)
		defer cancel()
	}
	leaseID, err := resolveBlacksmithDiscoveryID(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	routeCtx, cancel := context.WithTimeout(ctx, blacksmithStatusReadTimeout)
	bound, err := b.withRoute(routeCtx)
	err = blacksmithContextError(routeCtx, err)
	cancel()
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := b.rt.Clock.Now().Add(req.WaitTimeout)
	var lastState core.StatusView
	for {
		state, err := bound.blacksmithStatusView(ctx, leaseID, req.Wait)
		if err != nil {
			return core.StatusView{}, err
		}
		lastState = state
		if !req.Wait || state.Ready {
			return state, nil
		}
		if b.rt.Clock.Now().After(deadline) {
			return core.StatusView{}, core.Exit(5, "%s", blacksmithWaitTimeoutMessage(req.ID, lastState.State))
		}
		delay := blacksmithStatusPollDelay
		if remaining := deadline.Sub(b.rt.Clock.Now()); remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			return core.StatusView{}, core.Exit(5, "%s", blacksmithWaitTimeoutMessage(req.ID, lastState.State))
		}
		if err := shared.SleepContext(ctx, delay); err != nil {
			return core.StatusView{}, err
		}
	}
}

func (b *blacksmithBackend) Stop(ctx context.Context, req core.StopRequest) error {
	claim, err := resolveOwnedBlacksmithClaim(ctx, req.ID)
	if err != nil {
		return err
	}
	return b.stopClaimedTestbox(ctx, claim.LeaseID, claim)
}

var errBlacksmithReconciliationDeadline = errors.New("Blacksmith cleanup reconciliation deadline")

func (b *blacksmithBackend) stopClaimedTestbox(ctx context.Context, leaseID string, claim core.LeaseClaim) (stopErr error) {
	defer func() { stopErr = blacksmithExitDiagnostics(stopErr) }()
	_, _, err := blacksmithClaimBinding(claim)
	if err != nil {
		return err
	}
	if claim.LeaseID != leaseID {
		return core.Exit(2, "Blacksmith exact claim identifier mismatch")
	}
	reconciliationCtx, cancel := context.WithTimeoutCause(ctx, blacksmithCleanupTimeout, errBlacksmithReconciliationDeadline)
	defer cancel()
	var bound *blacksmithBackend
	var reconciled *blacksmithReconciledStop
	phase := "reconciliation-admission"
	reconciliationErr := core.WithLeaseClaimUnchangedShared(reconciliationCtx, leaseID, claim, func() error {
		phase = "reconciliation"
		var err error
		bound, err = b.withRoute(reconciliationCtx)
		if err != nil {
			return fmt.Errorf("Blacksmith cleanup phase=%s: %w", phase, err)
		}
		reconciled, err = bound.terminateTestbox(reconciliationCtx, claim, true)
		return err
	})
	if reconciliationErr != nil {
		if phase == "reconciliation-admission" {
			return fmt.Errorf("Blacksmith cleanup phase=%s: %w", phase, reconciliationErr)
		}
		// Only an authenticated native completion whose GitHub read was actually
		// interrupted by our phase budget may reach fresh final verification.
		if reconciled == nil || !reconciled.nativeCompleted || !errors.Is(reconciliationErr, errBlacksmithGitHubDeadline) ||
			context.Cause(reconciliationCtx) != errBlacksmithReconciliationDeadline || ctx.Err() != nil {
			return reconciliationErr
		}
	}
	// Release the shared fence before taking the exclusive one. A replacement
	// published in between must survive: only the original snapshot authorizes
	// finalization, and a stuck command must not make this wait unbounded.
	// Callback entry distinguishes admission from verification. Failure before
	// entry does not by itself establish lock contention.
	// Reserve a full status-read budget after reconciliation's 30 seconds, still
	// subordinate to the original caller. Only this fresh positive pair releases custody.
	ctx, cancelFinalization := context.WithTimeout(ctx, blacksmithStatusReadTimeout)
	defer cancelFinalization()
	phase = "finalization-admission"
	err = core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, leaseID, claim, true, func() error {
		phase = "final-verification"
		conclusion, err := bound.verifySettlement(ctx, claim, reconciled.runURL)
		if err != nil {
			return err
		}
		reconciled.conclusion = conclusion
		phase = "connection-removal"
		if err := core.RemoveStoredTestboxConnectionArtifacts(leaseID); err != nil {
			return fmt.Errorf("Blacksmith local connection artifacts cleanup failed; retaining claim: %w", err)
		}
		phase = "claim-commit"
		return nil
	})
	if err != nil && reconciled != nil {
		cause := reconciliationErr
		if reconciliationErr == nil {
			bound.printStopOutput(reconciled.result)
			cause = reconciled.err
		}
		return errors.Join(cause, fmt.Errorf("Blacksmith cleanup phase=%s: %w", phase, err))
	}
	if reconciled != nil && reconciled.err != nil {
		fmt.Fprintf(b.rt.Stderr, "blacksmith cleanup reconciled lease=%s state=completed: stop failed; native status and exact GitHub run confirmed completion conclusion=%s\n", leaseID, reconciled.conclusion)
	} else if reconciled != nil && reconciliationErr == nil {
		bound.printStopOutput(reconciled.result)
	}
	return err
}

func (b *blacksmithBackend) warmupLease(ctx context.Context, repo core.Repo, reclaim bool, requestedSlug string) (core.LeaseClaim, error) {
	if repo.Root == "" {
		return core.LeaseClaim{}, core.Exit(2, "Blacksmith acquisition requires a repository owner")
	}
	bound, err := b.withRoute(ctx)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	b = bound
	pendingID := "tbx_pending_" + strings.TrimPrefix(core.NewLeaseID(), "cbx_")
	_, publicKey, err := core.EnsureTestboxKey(pendingID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	args, err := blacksmithWarmupArgs(b.cfg, publicKey)
	if err != nil {
		core.RemoveStoredTestboxKey(pendingID)
		return core.LeaseClaim{}, err
	}
	result, warmupErr := b.runCommand(ctx, args, b.rt.Stdout, b.rt.Stderr)
	leaseID := blacksmithCreationReceipt(result.Stdout)
	failureCode := 5
	if warmupErr != nil && result.ExitCode > 0 {
		failureCode = result.ExitCode
	}
	if leaseID == "" {
		// Allocation may have succeeded even when its receipt cannot be trusted.
		// Keep the invocation's recovery key without granting resource authority.
		return core.LeaseClaim{}, core.Exit(failureCode, "Blacksmith warmup returned no unambiguous creation receipt; retained pending_key=%s for native recovery; inspect Blacksmith inventory; no resource was selected for rollback: %v", pendingID, warmupErr)
	}
	if warmupErr != nil {
		b.rollbackTestbox(leaseID, pendingID, repo.Root)
		return core.LeaseClaim{}, core.Exit(failureCode, "blacksmith testbox warmup failed: %v; inspect the exact receipt %s or use another provider", warmupErr, leaseID)
	}
	slug, err := core.AllocateClaimLeaseSlugContext(ctx, leaseID, requestedSlug)
	if err != nil {
		b.rollbackTestbox(leaseID, pendingID, repo.Root)
		return core.LeaseClaim{}, err
	}
	scope, _ := b.route.canonical()
	keyID := pendingID
	var identity blacksmithIdentity
	// Publication owns the absent-claim fence before moving this invocation's
	// key. A partial publication is retained rather than reinterpreted as absent.
	var published core.LeaseClaim
	err = core.WithDurableLeaseClaimLockContext(ctx, leaseID, func(current *core.LeaseClaim, exists bool, checkpoint func() error) error {
		if exists {
			return core.Exit(2, "Blacksmith acquisition conflicts with existing claim; retaining resource")
		}
		identity, err = b.inspectTestbox(ctx, leaseID)
		if err != nil {
			return err
		}
		if err := identity.usable(); err != nil {
			return err
		}
		if err := moveFreshBlacksmithKey(pendingID, leaseID); err != nil {
			return err
		}
		keyID = leaseID
		*current = blacksmithIdentityClaim(leaseID, slug, repo.Root, *b.route, identity)
		current.ClaimedAt = time.Now().UTC().Format(time.RFC3339)
		current.LastUsedAt = current.ClaimedAt
		current.TargetOS = targetLinux
		current.IdleTimeoutSeconds = int(blacksmithIdleTimeout(b.cfg).Seconds())
		current.CacheVolumes = append([]string(nil), core.CacheVolumeStickyDiskSpecs(b.cfg.Cache.Volumes)...)
		current.ProviderScope = scope
		if err := checkpoint(); err != nil {
			return err
		}
		published = *current
		return nil
	})
	if err != nil {
		b.rollbackTestbox(leaseID, keyID, repo.Root)
		return core.LeaseClaim{}, err
	}
	return published, nil
}

func (b *blacksmithBackend) openFailureStreamCapture(label string) (io.WriteCloser, string, func(), error) {
	file, err := os.CreateTemp("", "crabbox-blacksmith-failure-*."+label+".log")
	if err != nil {
		return nil, "", func() {}, core.Exit(2, "blacksmith failure bundle %s temp: %v", label, err)
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	return core.NewCappedFailureBundleStream(file), path, cleanup, nil
}

// Exit 124 alone cannot distinguish the sync guard from a workload failure.
type blacksmithRunOutcome struct {
	code             int
	err              error
	syncTimedOut     bool
	workloadObserved bool
}

func blacksmithRunDiagnostic(code int, err error) error {
	if err == nil {
		return nil
	}
	if code == 0 {
		code = 1
	}
	message := shared.RedactErrorSecrets(err.Error())
	// Redact before bounding; truncating a credential first can leak its prefix.
	if len(message) > 4096 {
		message = message[:4096] + " [truncated]"
	}
	return shared.ExitErrorWithCause(code, message, err)
}

func (b *blacksmithBackend) runTestbox(ctx context.Context, leaseID string, command []string, debug, shellMode bool, phaseTracker *core.CommandPhaseTracker, stdoutExtra, stderrExtra io.Writer, observation *core.RunObservation) blacksmithRunOutcome {
	keyPath, err := core.StoredTestboxKeyPath(leaseID)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "blacksmith key path failed: %v\n", err)
		return blacksmithRunOutcome{code: 2, err: err}
	}
	args := blacksmithRunArgs(b.cfg, leaseID, keyPath, command, debug || b.cfg.Blacksmith.Debug, shellMode)
	stdoutTarget, stderrTarget := observation.CommandWriters(mergeWriters(b.rt.Stdout, stdoutExtra), mergeWriters(b.rt.Stderr, stderrExtra), core.RunOutputProvider)
	stdout, stdoutPhaseWriter := commandPhaseWriter(stdoutTarget, phaseTracker)
	stderr, stderrPhaseWriter := commandPhaseWriter(stderrTarget, phaseTracker)
	result, timedOut, err := b.runCommandWithSyncGuard(ctx, args, stdout, stderr)
	stdoutPhaseWriter.Flush()
	stderrPhaseWriter.Flush()
	if timedOut {
		fmt.Fprintf(
			b.rt.Stderr,
			"Blacksmith Testbox sync did not print a completion marker for %s; terminating local runner. "+
				"Rerun with CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS=0 to disable this guard.\n",
			blacksmithSyncTimeout(os.Getenv),
		)
		return blacksmithRunOutcome{code: 124, err: err, syncTimedOut: true}
	}
	return blacksmithRunOutcome{code: result.ExitCode, err: err}
}

func commandPhaseWriter(w io.Writer, tracker *core.CommandPhaseTracker) (io.Writer, *core.PhaseMarkerWriter) {
	phaseWriter := core.NewPhaseMarkerWriter(tracker)
	if w == nil {
		return phaseWriter, phaseWriter
	}
	return io.MultiWriter(w, phaseWriter), phaseWriter
}

func mergeWriters(writers ...io.Writer) io.Writer {
	nonNil := make([]io.Writer, 0, len(writers))
	for _, writer := range writers {
		if writer != nil {
			nonNil = append(nonNil, writer)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}
	return io.MultiWriter(nonNil...)
}

func (b *blacksmithBackend) commandOutput(ctx context.Context, args []string) (string, error) {
	result, err := b.runCommand(ctx, args, nil, nil)
	if err != nil {
		return "", core.ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v: %s", err, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return result.Stdout + result.Stderr, nil
}

func (b *blacksmithBackend) runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	return b.runCommandCapture(ctx, args, stdout, stderr, false)
}

func (b *blacksmithBackend) runCommandCapture(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool) (core.LocalCommandResult, error) {
	return b.runCommandCaptureInDir(ctx, args, stdout, stderr, disableOutputCapture, "")
}

func (b *blacksmithBackend) runCommandCaptureInDir(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool, dir string) (result core.LocalCommandResult, err error) {
	operationArgs := args
	if len(operationArgs) > 2 && operationArgs[0] == "--org" {
		operationArgs = operationArgs[2:]
	}
	operation := ""
	if len(operationArgs) > 1 && operationArgs[0] == "testbox" && (operationArgs[1] == "status" || operationArgs[1] == "stop") {
		operation = "blacksmith testbox " + operationArgs[1]
	}
	if b.route != nil {
		args = append(append([]string(nil), args...), "--api-url", b.route.API, "--org", b.route.Org)
	}
	request := core.LocalCommandRequest{Name: "blacksmith", Args: args, Dir: dir, Stdout: stdout, Stderr: stderr, DisableOutputCapture: disableOutputCapture}
	if dir != "" {
		env, cleanup, prepareErr := blacksmithSSHCommandEnvironment()
		if prepareErr != nil {
			return core.LocalCommandResult{ExitCode: 2}, prepareErr
		}
		defer func() {
			if cleanupErr := cleanup(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove native Blacksmith SSH launcher: %w", cleanupErr))
				if result.ExitCode == 0 {
					result.ExitCode = 1
				}
			}
		}()
		// Artifact supervision must also bound local pipe draining on cancel.
		request.CancelGracePeriod = time.Second
		request.Env = env
		request.RequireProcessGroupJoin = true
		output := stderr
		if output == nil {
			output = b.rt.Stderr
		}
		if output == nil {
			output = io.Discard
		}
		progress := &blacksmithArtifactProgressWriter{output: output}
		request.Stderr = progress
		request.OnCleanupPending = func(err error) {
			fmt.Fprintf(progress, "blacksmith command cleanup pending: %v\n", err)
		}
	}
	if !disableOutputCapture {
		request.MaxCapturedOutputBytes = blacksmithCommandCaptureBytes
	}
	var issued time.Time
	if operation != "" {
		issued = time.Now()
	}
	result, err = b.rt.Exec.Run(ctx, request)
	if operation != "" && (err != nil || result.ExitCode != 0) {
		diagnostic := blacksmithCommandDiagnostic(ctx, operation, issued, time.Since(issued), ctx.Err(), result, err)
		return result, blacksmithCommandError{ExitError: core.ExitError{Code: result.ExitCode, Message: diagnostic}, cause: err}
	}
	if err != nil {
		return result, blacksmithCommandError{ExitError: core.ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v", err)}, cause: err}
	}
	return result, nil
}

// Cleanup notification may arrive while native stderr is still draining.
type blacksmithArtifactProgressWriter struct {
	mu     sync.Mutex
	output io.Writer
}

func (w *blacksmithArtifactProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.Write(p)
}

func (b *blacksmithBackend) runCommandWithSyncGuard(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, bool, error) {
	return b.runCommandWithSyncGuardCapture(ctx, args, stdout, stderr, true)
}

func (b *blacksmithBackend) runCommandWithSyncGuardCapture(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool) (core.LocalCommandResult, bool, error) {
	// Ordinary runs need the same joined command owner as artifact runs. Pin
	// the inherited directory so cancellation cannot leave sync children behind.
	dir, err := os.Getwd()
	if err != nil {
		return core.LocalCommandResult{ExitCode: 2}, false, err
	}
	return b.runCommandWithSyncGuardFiltered(ctx, args, stdout, stderr, disableOutputCapture, dir, nil)
}

// Filter before sync observation as well as console, proof, and failure capture.
func (b *blacksmithBackend) runCommandWithSyncGuardFiltered(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool, dir string, filter func(io.Writer, io.Writer) (io.Writer, io.Writer)) (core.LocalCommandResult, bool, error) {
	if err := validateBlacksmithNativeSyncScope(dir); err != nil {
		return core.LocalCommandResult{ExitCode: 2}, false, err
	}
	timeout := blacksmithSyncTimeout(os.Getenv)
	if timeout <= 0 {
		if filter != nil {
			stdout, stderr = filter(stdout, stderr)
		}
		result, err := b.runCommandCaptureInDir(ctx, args, stdout, stderr, disableOutputCapture, dir)
		return result, false, err
	}
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tracker := &blacksmithSyncTracker{}
	stdout = blacksmithSyncGuardWriter{w: stdout, tracker: tracker}
	stderr = blacksmithSyncGuardWriter{w: stderr, tracker: tracker}
	if filter != nil {
		stdout, stderr = filter(stdout, stderr)
	}
	resultCh := make(chan struct {
		result core.LocalCommandResult
		err    error
	}, 1)
	go func() {
		result, err := b.runCommandCaptureInDir(
			guardCtx,
			args,
			stdout,
			stderr,
			disableOutputCapture,
			dir,
		)
		resultCh <- struct {
			result core.LocalCommandResult
			err    error
		}{result: result, err: err}
	}()
	ticker := time.NewTicker(minBlacksmithDuration(timeout, time.Second))
	defer ticker.Stop()
	timedOut := false
	for {
		select {
		case result := <-resultCh:
			return result.result, timedOut, result.err
		case <-ticker.C:
			if !tracker.syncStalled(timeout, b.rt.Clock.Now()) {
				continue
			}
			timedOut = true
			cancel()
		}
	}
}

func validateBlacksmithNativeSyncScope(dir string) error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return nil
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	return core.ValidateManagedStateTransferScope("blacksmith native sync", root)
}

type blacksmithSyncTracker struct {
	mu           sync.Mutex
	syncingSince time.Time
	pending      string
}

func (t *blacksmithSyncTracker) observe(text string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending += text
	if len(t.pending) > 4096 {
		t.pending = t.pending[len(t.pending)-4096:]
	}
	for {
		i := strings.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		t.observeLineLocked(t.pending[:i+1], now)
		t.pending = t.pending[i+1:]
	}
	if t.pending != "" {
		t.observeLineLocked(t.pending, now)
	}
}

func (t *blacksmithSyncTracker) observeLineLocked(line string, now time.Time) {
	if blacksmithSyncStartPattern.MatchString(line) {
		if t.syncingSince.IsZero() {
			t.syncingSince = now
		}
		return
	}
	if !t.syncingSince.IsZero() && blacksmithSyncDonePattern.MatchString(line) {
		t.syncingSince = time.Time{}
	}
}

func (t *blacksmithSyncTracker) syncStalled(timeout time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.syncingSince.IsZero() && now.Sub(t.syncingSince) >= timeout
}

type blacksmithSyncGuardWriter struct {
	w       io.Writer
	tracker *blacksmithSyncTracker
}

func (w blacksmithSyncGuardWriter) Write(chunk []byte) (int, error) {
	if w.tracker != nil {
		w.tracker.observe(string(chunk), time.Now())
	}
	if w.w == nil {
		return len(chunk), nil
	}
	return w.w.Write(chunk)
}

func minBlacksmithDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func (b *blacksmithBackend) blacksmithStatusView(ctx context.Context, leaseID string, wait bool) (core.StatusView, error) {
	ctx, cancel := context.WithTimeout(ctx, blacksmithStatusReadTimeout)
	defer cancel()
	identity, err := b.inspectTestbox(ctx, leaseID)
	if err != nil {
		return core.StatusView{}, err
	}
	if wait && (identity.State == "completed" || identity.State == "hydration_failed") {
		return core.StatusView{}, core.Exit(5, "Blacksmith Testbox %s reached state %s before ready", leaseID, identity.State)
	}
	item := blacksmithListItem{ID: identity.ID, Status: identity.State, Workflow: identity.Workflow, Job: identity.Job, Ref: identity.Ref, Created: identity.Created}
	metadata := b.statusSettlement(ctx, identity)
	if err := ctx.Err(); err != nil {
		return core.StatusView{}, err
	}
	return core.StatusView{
		ID:               identity.ID,
		Provider:         blacksmithTestboxProvider,
		TargetOS:         targetLinux,
		State:            identity.State,
		ServerID:         identity.ID,
		ServerType:       "testbox",
		Labels:           blacksmithItemToServer(item).Labels,
		HasHost:          false,
		Ready:            identity.State == "ready" || identity.State == "running" || identity.State == "in_progress",
		IdleTimeout:      blacksmithIdleTimeout(b.cfg).String(),
		ProviderMetadata: metadata,
	}, nil
}

func blacksmithItemToServer(item blacksmithListItem) core.Server {
	labels := map[string]string{
		"lease":    item.ID,
		"provider": blacksmithTestboxProvider,
		"state":    item.Status,
		"repo":     item.Repo,
		"workflow": item.Workflow,
		"job":      item.Job,
		"ref":      item.Ref,
		"created":  item.Created,
	}
	server := core.Server{
		CloudID:  item.ID,
		Provider: blacksmithTestboxProvider,
		Name:     item.ID,
		Status:   item.Status,
		Labels:   labels,
	}
	server.ServerType.Name = "testbox"
	return server
}

func blacksmithWaitTimeoutMessage(identifier, state string) string {
	state = strings.TrimSpace(state)
	if strings.EqualFold(state, "queued") {
		return fmt.Sprintf("timed out waiting for %s to become ready (last state queued; Blacksmith queue may be stalled, so stop queued ids you created or use another provider)", identifier)
	}
	if state != "" {
		return fmt.Sprintf("timed out waiting for %s to become ready (last state %s)", identifier, state)
	}
	return fmt.Sprintf("timed out waiting for %s to become ready", identifier)
}
