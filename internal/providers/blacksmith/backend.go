package blacksmith

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type BlacksmithConfig = core.BlacksmithConfig
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type Server = core.Server
type Repo = core.Repo
type ExitError = core.ExitError
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type CommandRunner = core.CommandRunner
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const targetLinux = core.TargetLinux

func RegisterBlacksmithProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return registerBlacksmithFlags(fs, defaults)
}

func ApplyBlacksmithProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if v, ok := values.(blacksmithFlagValues); ok {
		applyBlacksmithFlagOverrides(cfg, fs, v)
	}
	return nil
}

func NewBlacksmithBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = blacksmithTestboxProvider
	return &blacksmithBackend{spec: spec, cfg: cfg, rt: rt}
}

type blacksmithBackend struct {
	spec  ProviderSpec
	cfg   Config
	rt    Runtime
	route *blacksmithRoute
	claim *core.LeaseClaim
}

var _ core.RunOptionsValidator = (*blacksmithBackend)(nil)

func (b *blacksmithBackend) Spec() ProviderSpec { return b.spec }

func (b *blacksmithBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s; Blacksmith owns runner hydration", b.cfg.Provider)
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
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: blacksmithTestboxProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *blacksmithBackend) ValidateRunOptions(req RunRequest) error {
	return validateBlacksmithRunOptions(b.spec, req)
}

func validateBlacksmithRunOptions(spec ProviderSpec, req RunRequest) error {
	if req.NoSync {
		return core.Exit(2, "%s delegates sync; --no-sync is not supported", blacksmithTestboxProvider)
	}
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}

func (b *blacksmithBackend) Run(ctx context.Context, req RunRequest) (runResult RunResult, runErr error) {
	if err := b.ValidateRunOptions(req); err != nil {
		return RunResult{}, err
	}
	if err := core.ValidateRunArtifactGlobs(req.ArtifactGlobs); err != nil {
		return RunResult{}, err
	}
	if err := core.ValidateRequiredRunArtifactGlobs(req.RequiredArtifactGlobs); err != nil {
		return RunResult{}, err
	}
	if blacksmithEnvForwardingRequested(req) {
		core.PrintEnvForwardingSummary(b.rt.Stderr, blacksmithTestboxProvider, "unsupported", req.Options.EnvAllow, req.Env)
		fmt.Fprintf(b.rt.Stderr, "env forwarding note=blacksmith-testbox delegates execution to the Blacksmith CLI; configure secrets in the Testbox workflow instead\n")
		return RunResult{}, core.Exit(2, "env forwarding is unsupported for provider=%s; configure secrets in the provider workflow or use an SSH-backed provider", blacksmithTestboxProvider)
	}
	hiddenOmissions, sparseErr := core.GitCheckoutHasHiddenOmissions(req.Repo.Root)
	if sparseErr != nil {
		return RunResult{}, core.Exit(
			2,
			"provider=%s cannot verify sparse-checkout safety: %v; run from a materialized full checkout",
			blacksmithTestboxProvider,
			sparseErr,
		)
	}
	if hiddenOmissions {
		return RunResult{}, core.Exit(
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
			return RunResult{}, err
		}
		route, _, _ := blacksmithClaimBinding(claim)
		bound := *b
		bound.route, bound.claim = &route, &claim
		b = &bound
		acquired = true
	} else {
		b, claim, err = b.ownedTestbox(ctx, req.ID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
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
		cleanupCtx, cancel := context.WithTimeout(context.Background(), blacksmithCleanupTimeout)
		defer cancel()
		if err := b.stopClaimedTestbox(cleanupCtx, leaseID, claim); err != nil {
			shouldStop = false
			fmt.Fprintf(b.rt.Stderr, "warning: blacksmith cleanup failed stage=cleanup lease=%s retry_likely=true: %v\n", leaseID, err)
			return err
		}
		return nil
	}
	defer func() {
		if err := cleanup(); err != nil {
			if runResult.Session == nil {
				runResult.Provider, runResult.LeaseID, runResult.Slug = blacksmithTestboxProvider, leaseID, slug
				runResult.Session = &core.RunSessionHandle{Provider: blacksmithTestboxProvider, LeaseID: leaseID, Slug: slug, CleanupCommand: fmt.Sprintf("crabbox stop --provider %s %s", blacksmithTestboxProvider, leaseID)}
			}
			runResult.Session.Kept = true
			if runErr == nil {
				runResult.ExitCode = 1
				runResult.ErrorKind = core.RunErrorProvider
				runErr = exit(1, "Blacksmith cleanup unconfirmed; retained lease %s: %v", leaseID, err)
			}
		}
	}()
	fmt.Fprintf(b.rt.Stderr, "provider=blacksmith-testbox id=%s sync=delegated auth=blacksmith\n", leaseID)
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		core.PrintEnvForwardingSummary(b.rt.Stderr, blacksmithTestboxProvider, "unsupported", req.Options.EnvAllow, req.Env)
		fmt.Fprintf(b.rt.Stderr, "env forwarding note=blacksmith-testbox delegates execution to the Blacksmith CLI; configure secrets in the Testbox workflow instead\n")
	}
	stdoutCapture, stdoutCapturePath, stdoutCleanup, err := b.openFailureStreamCapture("stdout")
	if err != nil {
		return RunResult{}, err
	}
	defer stdoutCleanup()
	stderrCapture, stderrCapturePath, stderrCleanup, err := b.openFailureStreamCapture("stderr")
	if err != nil {
		return RunResult{}, err
	}
	defer stderrCleanup()
	stdoutProof := newBlacksmithProofTailBuffer()
	stderrProof := newBlacksmithProofTailBuffer()
	commandStart := b.rt.Clock.Now()
	phaseTracker := core.NewCommandPhaseTracker(commandStart)
	code := 0
	var commandEnd time.Time
	var collected []core.RunArtifact
	var artifactErr error
	if err := b.withOwnedTestbox(ctx, claim, func() error {
		if len(req.ArtifactGlobs) > 0 || len(req.RequiredArtifactGlobs) > 0 {
			code, commandEnd, collected, artifactErr = b.runArtifactTestbox(ctx, req, leaseID, phaseTracker,
				mergeWriters(stdoutCapture, stdoutProof), mergeWriters(stderrCapture, stderrProof), blacksmithCollectionTimeout)
			return nil
		}
		code = b.runTestbox(
			ctx,
			leaseID,
			req.Command,
			req.DebugSync,
			req.ShellMode,
			phaseTracker,
			mergeWriters(stdoutCapture, stdoutProof),
			mergeWriters(stderrCapture, stderrProof),
		)
		return nil
	}); err != nil {
		return RunResult{}, err
	}
	// Artifact diagnostics must not reclassify an earlier workload failure.
	artifactFailedSuccess := artifactErr != nil && code == 0
	if artifactErr != nil {
		fmt.Fprintf(b.rt.Stderr, "blacksmith artifact retrieval failed: %v\n", artifactErr)
		if code == 0 {
			code = blacksmithArtifactFailureExitCode(artifactErr)
		}
	}
	if closeErr := stdoutCapture.Close(); closeErr != nil && code == 0 {
		return RunResult{}, core.Exit(2, "blacksmith failure bundle stdout close: %v", closeErr)
	}
	if closeErr := stderrCapture.Close(); closeErr != nil && code == 0 {
		return RunResult{}, core.Exit(2, "blacksmith failure bundle stderr close: %v", closeErr)
	}
	finished := b.rt.Clock.Now()
	if commandEnd.IsZero() {
		commandEnd = finished
	}
	commandDuration := commandEnd.Sub(commandStart)
	commandPhases := core.FinishCommandPhaseTracker(phaseTracker, commandEnd)
	total := finished.Sub(started)
	actionsURL := firstNonBlank(stdoutProof.ActionsURL(), stderrProof.ActionsURL())
	result := RunResult{
		Provider:      blacksmithTestboxProvider,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   blacksmithCommandString(req.Command, req.ShellMode),
		LogExcerpt:    core.SelectProofLogExcerpt(strings.TrimSpace(string(stdoutProof.Bytes()) + "\n" + string(stderrProof.Bytes()))),
		ActionsURL:    firstNonBlank(actionsURL, firstBlacksmithActionsURL(string(stdoutProof.Bytes())+"\n"+string(stderrProof.Bytes()))),
		ExitCode:      code,
		Command:       commandDuration,
		Total:         total,
		SyncDelegated: true,
	}
	for _, artifact := range collected {
		fmt.Fprintf(b.rt.Stderr, "artifact kind=%s path=%s bytes=%d\n", artifact.Kind, artifact.Path, artifact.Bytes)
	}
	result.Artifacts = append(result.Artifacts, collected...)
	if code != 0 && req.KeepOnFailure {
		shouldStop = false
	}
	cleanupErr := cleanup()
	if cleanupErr != nil && code == 0 {
		code = 1
		result.ExitCode = code
		result.ErrorKind = core.RunErrorProvider
	}
	if cleanupErr == nil && cleanupAttempted && code == 0 {
		printBlacksmithOneShotActionsWarning(b.rt.Stderr, result.ActionsURL)
	}
	total = b.rt.Clock.Now().Sub(started)
	result.Total = total
	report := delegatedTimingReport(blacksmithTestboxProvider, leaseID, slug, "blacksmith-testbox owns sync", commandDuration, commandPhases, total, code)
	report = core.TimingReportWithRunResult(report, result, cleanupErr)
	if code != 0 {
		classificationInput := string(stdoutProof.Bytes()) + "\n" + string(stderrProof.Bytes())
		failurePhases := commandPhases
		if artifactFailedSuccess {
			classificationInput = artifactErr.Error()
			failurePhases = nil
		}
		classification := core.ClassifyRunFailure(code, classificationInput, failurePhases)
		core.ApplyFailureClassification(&report, classification)
	}
	if cleanupErr != nil && result.ErrorKind == core.RunErrorProvider {
		report.BlockedStage, report.RetryLikely = "cleanup", "true"
	}
	fmt.Fprintf(b.rt.Stderr, "blacksmith run summary sync=delegated command=%s total=%s exit=%d%s\n", commandDuration.Round(time.Millisecond), total.Round(time.Millisecond), code, core.FormatFailureClassificationFields(core.FailureClassification{BlockedStage: report.BlockedStage, RetryLikely: report.RetryLikely}))
	report.Label = strings.TrimSpace(req.Label)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
			return RunResult{}, err
		}
	}
	proof, proofErr := b.blacksmithProofResult(req, leaseID, slug, code, commandDuration, total, report, stdoutProof.Bytes(), stderrProof.Bytes(), actionsURL)
	if proofErr != nil && code == 0 {
		return RunResult{}, proofErr
	}
	if proofErr == nil {
		result.Provider = proof.Provider
		result.LeaseID = proof.LeaseID
		result.Slug = proof.Slug
		result.CommandText = proof.CommandText
		result.LogExcerpt = proof.LogExcerpt
		result.ActionsURL = proof.ActionsURL
		result.Artifacts = append(result.Artifacts, proof.Artifacts...)
	}
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
	if code != 0 {
		local, bytes, bundleErr := core.CaptureLocalFailureBundle(leaseID, core.FailureCaptureMetadata{
			Provider:   blacksmithTestboxProvider,
			LeaseID:    leaseID,
			Slug:       slug,
			Workdir:    "blacksmith-testbox",
			ExitCode:   code,
			Timing:     report,
			EnvAllow:   req.Options.EnvAllow,
			Env:        req.Env,
			Config:     b.cfg,
			StdoutPath: stdoutCapturePath,
			StderrPath: stderrCapturePath,
		})
		if bundleErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: failure bundle failed: %v\n", bundleErr)
		} else {
			fmt.Fprintf(b.rt.Stderr, "failure-bundle local=%s bytes=%d secret_risk=caller-redacts-before-sharing\n", local, bytes)
		}
		core.HandleDelegatedRunFailure(b.rt.Stderr, req, blacksmithTestboxProvider, leaseID, slug, blacksmithIdleTimeout(b.cfg), b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, ExitError{Code: code, Message: fmt.Sprintf("blacksmith testbox run exited %d", code)}
	}
	return result, nil
}

func printBlacksmithOneShotActionsWarning(w io.Writer, actionsURL string) {
	if w == nil {
		return
	}
	fmt.Fprint(w, "blacksmith proof note: stopped one-shot Testbox after success; the backing GitHub Actions run may show a cancelled Testbox step because Blacksmith owns the delegated session lifecycle")
	if strings.TrimSpace(actionsURL) != "" {
		fmt.Fprintf(w, " actions=%s", strings.TrimSpace(actionsURL))
	}
	fmt.Fprintln(w)
}

func blacksmithArtifactFailureExitCode(err error) int {
	var exitErr ExitError
	if core.AsExitError(err, &exitErr) && exitErr.Code != 0 {
		return exitErr.Code
	}
	return 7
}

func blacksmithExtractArtifactArchive(output string, maxBytes int64) ([]byte, string, error) {
	begin := blacksmithArtifactMarkerLineIndex(output, core.DelegatedRunArtifactBeginMarker, 0)
	end := -1
	if begin >= 0 {
		end = blacksmithArtifactMarkerLineIndex(output, core.DelegatedRunArtifactEndMarker, begin+len(core.DelegatedRunArtifactBeginMarker))
	}
	if begin < 0 || end < 0 {
		return nil, output, exit(7, "blacksmith artifact retrieval did not return a bounded artifact archive")
	}
	before := strings.TrimSpace(output[:begin])
	encodedStart := begin + len(core.DelegatedRunArtifactBeginMarker)
	encoded := output[encodedStart:end]
	after := strings.TrimSpace(output[end+len(core.DelegatedRunArtifactEndMarker):])
	compact := strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "").Replace(encoded)
	if maxBytes <= 0 {
		maxBytes = core.DelegatedRunArtifactDefaultMaxBytes
	}
	decodedLen := int64(base64.StdEncoding.DecodedLen(len(compact)))
	if strings.HasSuffix(compact, "==") {
		decodedLen -= 2
	} else if strings.HasSuffix(compact, "=") {
		decodedLen--
	}
	if decodedLen > maxBytes {
		return nil, "", exit(7, "blacksmith artifact archive too large: decoded output exceeds %d bytes", maxBytes)
	}
	archive, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, "", exit(7, "blacksmith artifact archive decode failed: %v", err)
	}
	if int64(len(archive)) > maxBytes {
		return nil, "", exit(7, "blacksmith artifact archive too large: %d > %d bytes", len(archive), maxBytes)
	}
	return archive, strings.TrimSpace(strings.TrimSpace(before) + "\n" + strings.TrimSpace(after)), nil
}

func blacksmithArtifactMarkerLineIndex(output, marker string, start int) int {
	if start < 0 {
		start = 0
	}
	for offset := start; offset < len(output); {
		idx := strings.Index(output[offset:], marker)
		if idx < 0 {
			return -1
		}
		pos := offset + idx
		beforeLine := pos == 0 || output[pos-1] == '\n'
		after := pos + len(marker)
		afterLine := after == len(output) || output[after] == '\n' || output[after] == '\r'
		if beforeLine && afterLine {
			return pos
		}
		offset = after
	}
	return -1
}

var githubActionsRunURLPattern = regexp.MustCompile(`https://github\.com/[^\s"'<>]+/actions/runs/[0-9]+[^\s"'<>]*`)

func blacksmithEnvForwardingRequested(req RunRequest) bool {
	return req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != ""
}

const (
	blacksmithProofStreamCaptureBytes              = 1024 * 1024
	blacksmithCommandCaptureBytes                  = 16 * 1024 * 1024
	blacksmithCleanupTimeout                       = 30 * time.Second
	blacksmithArtifactDiagnosticCaptureBytes int64 = 64 * 1024
)

func blacksmithArtifactOutputCaptureLimit(maxBytes int64) int64 {
	if maxBytes <= 0 {
		maxBytes = core.DelegatedRunArtifactDefaultMaxBytes
	}
	if maxBytes > (math.MaxInt64-blacksmithArtifactDiagnosticCaptureBytes-4096)/2 {
		return math.MaxInt64
	}
	return maxBytes*2 + blacksmithArtifactDiagnosticCaptureBytes + 4096
}

type blacksmithProofTailBuffer struct {
	mu         sync.Mutex
	data       []byte
	scanTail   string
	actionsURL string
	truncated  bool
}

func firstNonBlank(values ...string) string {
	return shared.FirstNonBlank(values...)
}

func newBlacksmithProofTailBuffer() *blacksmithProofTailBuffer {
	return &blacksmithProofTailBuffer{data: make([]byte, 0, 32*1024)}
}

func (b *blacksmithProofTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.actionsURL == "" {
		probe := b.scanTail + string(p)
		if match := firstBlacksmithActionsURL(probe); match != "" {
			b.actionsURL = match
		}
		if len(probe) > 2048 {
			b.scanTail = probe[len(probe)-2048:]
		} else {
			b.scanTail = probe
		}
	}
	if len(p) >= blacksmithProofStreamCaptureBytes {
		b.data = append(b.data[:0], p[len(p)-blacksmithProofStreamCaptureBytes:]...)
		b.truncated = true
		return len(p), nil
	}
	overflow := len(b.data) + len(p) - blacksmithProofStreamCaptureBytes
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *blacksmithProofTailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := append([]byte(nil), b.data...)
	if !b.truncated {
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

func (b *blacksmithBackend) blacksmithProofResult(req RunRequest, leaseID, slug string, exitCode int, commandDuration, total time.Duration, report timingReport, stdoutData, stderrData []byte, actionsURL string) (RunResult, error) {
	combined := strings.TrimSpace(string(stdoutData) + "\n" + string(stderrData))
	result := RunResult{
		Provider:    blacksmithTestboxProvider,
		LeaseID:     leaseID,
		Slug:        slug,
		CommandText: blacksmithCommandString(req.Command, req.ShellMode),
		LogExcerpt:  core.SelectProofLogExcerpt(combined),
		ActionsURL:  firstNonBlank(actionsURL, firstBlacksmithActionsURL(combined)),
	}
	if strings.TrimSpace(req.EmitProof) == "" {
		return result, nil
	}
	artifacts, err := persistBlacksmithRunArtifacts(req.Repo.Root, leaseID, exitCode, commandDuration, total, report, stdoutData, stderrData, result)
	if err != nil {
		return RunResult{}, err
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

func persistBlacksmithRunArtifacts(repoRoot, leaseID string, exitCode int, commandDuration, total time.Duration, report timingReport, stdoutData, stderrData []byte, result RunResult) ([]core.RunArtifact, error) {
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

func (b *blacksmithBackend) List(ctx context.Context, req ListRequest) ([]Server, error) {
	out, err := b.commandOutput(ctx, b.listArgs(req))
	if err != nil {
		return nil, err
	}
	items := parseBlacksmithList(out)
	servers := make([]Server, 0, len(items))
	for _, item := range items {
		servers = append(servers, blacksmithItemToServer(item))
	}
	return servers, nil
}

func (b *blacksmithBackend) ListJSON(ctx context.Context, req ListRequest) (any, error) {
	out, err := b.commandOutput(ctx, b.listArgs(req))
	if err != nil {
		return nil, err
	}
	return parseBlacksmithList(out), nil
}

func (b *blacksmithBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{All: true})
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

func delegatedTimingReport(provider, leaseID, slug, syncReason string, commandDuration time.Duration, commandPhases []timingPhase, total time.Duration, exitCode int) timingReport {
	return timingReport{
		Provider:      provider,
		LeaseID:       leaseID,
		Slug:          slug,
		SyncPhases:    []timingPhase{{Name: "delegated", Skipped: true, Reason: syncReason}},
		SyncDelegated: true,
		CommandMs:     commandDuration.Milliseconds(),
		CommandPhases: commandPhases,
		TotalMs:       total.Milliseconds(),
		ExitCode:      exitCode,
	}
}

func (b *blacksmithBackend) listArgs(req ListRequest) []string {
	if req.All {
		return blacksmithListAllArgs(b.cfg)
	}
	return blacksmithListArgs(b.cfg)
}

func (b *blacksmithBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	leaseID, err := resolveBlacksmithDiscoveryID(req.ID)
	if err != nil {
		return statusView{}, err
	}
	deadline := b.rt.Clock.Now().Add(req.WaitTimeout)
	var lastState statusView
	for {
		state, err := b.blacksmithStatusView(ctx, leaseID)
		if err != nil {
			return statusView{}, err
		}
		lastState = state
		if !req.Wait || state.Ready {
			return state, nil
		}
		if b.rt.Clock.Now().After(deadline) {
			return statusView{}, exit(5, "%s", blacksmithWaitTimeoutMessage(req.ID, lastState.State))
		}
		delay := blacksmithStatusPollDelay
		if remaining := deadline.Sub(b.rt.Clock.Now()); remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			return statusView{}, exit(5, "%s", blacksmithWaitTimeoutMessage(req.ID, lastState.State))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return statusView{}, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func (b *blacksmithBackend) Stop(ctx context.Context, req StopRequest) error {
	claim, err := resolveOwnedBlacksmithClaim(req.ID)
	if err != nil {
		return err
	}
	return b.stopClaimedTestbox(ctx, claim.LeaseID, claim)
}

func (b *blacksmithBackend) stopClaimedTestbox(ctx context.Context, leaseID string, claim core.LeaseClaim) (stopErr error) {
	defer func() { stopErr = blacksmithExitDiagnostics(stopErr) }()
	_, _, err := blacksmithClaimBinding(claim)
	if err != nil {
		return err
	}
	if claim.LeaseID != leaseID {
		return exit(2, "Blacksmith exact claim identifier mismatch")
	}
	ctx, cancel := context.WithTimeout(ctx, blacksmithCleanupTimeout)
	defer cancel()
	var bound *blacksmithBackend
	var reconciled *blacksmithReconciledStop
	if err := core.WithLeaseClaimUnchangedShared(ctx, leaseID, claim, func() error {
		var err error
		bound, err = b.withRoute(ctx)
		if err != nil {
			return err
		}
		reconciled, err = bound.terminateTestbox(ctx, claim)
		return err
	}); err != nil {
		return err
	}
	// Release the shared fence before taking the exclusive one. A replacement
	// published in between must survive: only the original snapshot authorizes
	// finalization, and a stuck command must not make this wait unbounded.
	err = core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, leaseID, claim, true, func() error {
		identity, err := bound.verifyTestbox(ctx, claim)
		if err != nil {
			return err
		}
		if !identity.terminal() {
			return exit(2, "Blacksmith termination is not confirmed; retaining claim and key")
		}
		if err := core.RemoveStoredTestboxConnectionArtifacts(leaseID); err != nil {
			return fmt.Errorf("Blacksmith local connection artifacts cleanup failed; retaining claim: %w", err)
		}
		return nil
	})
	if err != nil && reconciled != nil {
		bound.printStopOutput(reconciled.result)
		return errors.Join(reconciled.err, err)
	}
	if reconciled != nil {
		fmt.Fprintf(b.rt.Stderr, "blacksmith cleanup reconciled lease=%s state=completed: stop failed; native status confirmed completion\n", leaseID)
	}
	return err
}

func (b *blacksmithBackend) warmupLease(ctx context.Context, repo Repo, reclaim bool, requestedSlug string) (core.LeaseClaim, error) {
	if repo.Root == "" {
		return core.LeaseClaim{}, exit(2, "Blacksmith acquisition requires a repository owner")
	}
	bound, err := b.withRoute(ctx)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	b = bound
	pendingID := "tbx_pending_" + strings.TrimPrefix(newLeaseID(), "cbx_")
	_, publicKey, err := ensureTestboxKey(pendingID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	args, err := blacksmithWarmupArgs(b.cfg, publicKey)
	if err != nil {
		removeStoredTestboxKey(pendingID)
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
		return core.LeaseClaim{}, exit(failureCode, "Blacksmith warmup returned no unambiguous creation receipt; retained pending_key=%s for native recovery; inspect Blacksmith inventory; no resource was selected for rollback: %v", pendingID, warmupErr)
	}
	if warmupErr != nil {
		b.rollbackTestbox(leaseID, pendingID, repo.Root)
		return core.LeaseClaim{}, exit(failureCode, "blacksmith testbox warmup failed: %v; inspect the exact receipt %s or use another provider", warmupErr, leaseID)
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
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
			return exit(2, "Blacksmith acquisition conflicts with existing claim; retaining resource")
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

func (b *blacksmithBackend) runTestbox(ctx context.Context, leaseID string, command []string, debug, shellMode bool, phaseTracker *core.CommandPhaseTracker, stdoutExtra, stderrExtra io.Writer) int {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "blacksmith key path failed: %v\n", err)
		return 2
	}
	args := blacksmithRunArgs(b.cfg, leaseID, keyPath, command, debug || b.cfg.Blacksmith.Debug, shellMode)
	stdout, stdoutPhaseWriter := commandPhaseWriter(mergeWriters(b.rt.Stdout, stdoutExtra), phaseTracker)
	stderr, stderrPhaseWriter := commandPhaseWriter(mergeWriters(b.rt.Stderr, stderrExtra), phaseTracker)
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
		return 124
	}
	if err != nil {
		return result.ExitCode
	}
	return 0
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
		return "", ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v: %s", err, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return result.Stdout + result.Stderr, nil
}

func (b *blacksmithBackend) runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return b.runCommandCapture(ctx, args, stdout, stderr, false)
}

func (b *blacksmithBackend) runCommandCapture(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool) (LocalCommandResult, error) {
	return b.runCommandCaptureInDir(ctx, args, stdout, stderr, disableOutputCapture, "")
}

func (b *blacksmithBackend) runCommandCaptureInDir(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool, dir string) (LocalCommandResult, error) {
	if b.route != nil {
		args = append(append([]string(nil), args...), "--api-url", b.route.API, "--org", b.route.Org)
	}
	request := LocalCommandRequest{Name: "blacksmith", Args: args, Dir: dir, Stdout: stdout, Stderr: stderr, DisableOutputCapture: disableOutputCapture}
	if dir != "" {
		// Artifact supervision must also bound local pipe draining on cancel.
		request.CancelGracePeriod = time.Second
	}
	if !disableOutputCapture {
		request.MaxCapturedOutputBytes = blacksmithCommandCaptureBytes
	}
	result, err := b.rt.Exec.Run(ctx, request)
	if err != nil {
		return result, blacksmithCommandError{ExitError: ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v", err)}, cause: err}
	}
	return result, nil
}

func (b *blacksmithBackend) runCommandWithSyncGuard(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, bool, error) {
	return b.runCommandWithSyncGuardCapture(ctx, args, stdout, stderr, true)
}

func (b *blacksmithBackend) runCommandWithSyncGuardCapture(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool) (LocalCommandResult, bool, error) {
	return b.runCommandWithSyncGuardFiltered(ctx, args, stdout, stderr, disableOutputCapture, "", nil)
}

// Filter before sync observation as well as console, proof, and failure capture.
func (b *blacksmithBackend) runCommandWithSyncGuardFiltered(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool, dir string, filter func(io.Writer, io.Writer) (io.Writer, io.Writer)) (LocalCommandResult, bool, error) {
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
		result LocalCommandResult
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
			result LocalCommandResult
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

func (b *blacksmithBackend) blacksmithStatusView(ctx context.Context, leaseID string) (statusView, error) {
	out, err := b.commandOutput(ctx, blacksmithListAllArgs(b.cfg))
	if err != nil {
		return statusView{}, err
	}
	for _, item := range parseBlacksmithList(out) {
		if item.ID != leaseID {
			continue
		}
		server := blacksmithItemToServer(item)
		return statusView{
			ID:          item.ID,
			Provider:    blacksmithTestboxProvider,
			TargetOS:    targetLinux,
			State:       item.Status,
			ServerID:    item.ID,
			ServerType:  "testbox",
			Labels:      server.Labels,
			HasHost:     false,
			Ready:       strings.EqualFold(item.Status, "ready") || strings.EqualFold(item.Status, "running"),
			IdleTimeout: blacksmithIdleTimeout(b.cfg).String(),
		}, nil
	}
	return statusView{}, exit(4, "blacksmith testbox not found: %s", leaseID)
}

func blacksmithItemToServer(item blacksmithListItem) Server {
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
	server := Server{
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

type statusView = core.StatusView

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func newLeaseID() string {
	return core.NewLeaseID()
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func ensureTestboxKey(leaseID string) (string, string, error) {
	return core.EnsureTestboxKey(leaseID)
}

func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
}

func testboxKeyPath(leaseID string) (string, error) {
	return core.TestboxKeyPath(leaseID)
}

func baseConfig() Config {
	return core.BaseConfig()
}

func readLeaseClaim(leaseID string) (core.LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}
