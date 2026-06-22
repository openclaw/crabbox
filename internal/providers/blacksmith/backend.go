package blacksmith

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var _ core.DelegatedRunArtifactBackend = (*blacksmithBackend)(nil)

func (b *blacksmithBackend) Spec() ProviderSpec { return b.spec }

func (b *blacksmithBackend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s; Blacksmith owns runner hydration", b.cfg.Provider)
	}
	started := b.rt.Clock.Now()
	leaseID, slug, err := b.warmupLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s idle_timeout=%s\n", leaseID, slug, blacksmithTestboxProvider, blacksmithIdleTimeout(b.cfg))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: blacksmith warmup keeps the testbox until idle timeout or explicit stop\n")
	}
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", b.rt.Clock.Now().Sub(started).Round(time.Millisecond))
	if req.TimingJSON {
		total := b.rt.Clock.Now().Sub(started)
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

func (b *blacksmithBackend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := core.RejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if blacksmithEnvForwardingRequested(req) {
		core.PrintEnvForwardingSummary(b.rt.Stderr, blacksmithTestboxProvider, "unsupported", req.Options.EnvAllow, req.Env)
		fmt.Fprintf(b.rt.Stderr, "env forwarding note=blacksmith-testbox delegates execution to the Blacksmith CLI; configure secrets in the Testbox workflow instead\n")
		return RunResult{}, core.Exit(2, "env forwarding is unsupported for provider=%s; configure secrets in the provider workflow or use an SSH-backed provider", blacksmithTestboxProvider)
	}
	started := b.rt.Clock.Now()
	leaseID := req.ID
	slug := ""
	acquired := false
	var err error
	if leaseID == "" {
		leaseID, slug, err = b.warmupLease(ctx, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		acquired = true
	} else {
		leaseID, err = resolveBlacksmithLeaseID(leaseID, req.Repo.Root, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		slug, err = blacksmithClaimSlug(req.ID, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		if err := claimLeaseForRepoProvider(leaseID, slug, blacksmithTestboxProvider, req.Repo.Root, blacksmithIdleTimeout(b.cfg), req.Reclaim); err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	finalExitCode := -1
	finalActionsURL := ""
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			if err := b.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: blacksmith cleanup failed stage=cleanup lease=%s retry_likely=true: %v\n", leaseID, err)
				return
			}
			removeLeaseClaim(leaseID)
			removeStoredTestboxKey(leaseID)
			if finalExitCode == 0 {
				printBlacksmithOneShotActionsWarning(b.rt.Stderr, finalActionsURL)
			}
		}()
	}
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
	code := b.runTestbox(
		ctx,
		leaseID,
		req.Command,
		req.DebugSync,
		req.ShellMode,
		phaseTracker,
		mergeWriters(stdoutCapture, stdoutProof),
		mergeWriters(stderrCapture, stderrProof),
	)
	if closeErr := stdoutCapture.Close(); closeErr != nil && code == 0 {
		return RunResult{}, core.Exit(2, "blacksmith failure bundle stdout close: %v", closeErr)
	}
	if closeErr := stderrCapture.Close(); closeErr != nil && code == 0 {
		return RunResult{}, core.Exit(2, "blacksmith failure bundle stderr close: %v", closeErr)
	}
	finished := b.rt.Clock.Now()
	commandDuration := finished.Sub(commandStart)
	commandPhases := core.FinishCommandPhaseTracker(phaseTracker, finished)
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
	var artifactErr error
	if code == 0 && (len(req.ArtifactGlobs) > 0 || len(req.RequiredArtifactGlobs) > 0) {
		collected, err := b.CollectRunArtifacts(ctx, core.DelegatedRunArtifactRequest{
			RunReq:   req,
			Result:   result,
			MaxFiles: core.DelegatedRunArtifactDefaultMaxFiles,
			MaxBytes: core.DelegatedRunArtifactDefaultMaxBytes,
		})
		if err != nil {
			artifactErr = err
			fmt.Fprintf(b.rt.Stderr, "blacksmith artifact retrieval failed: %v\n", err)
			code = blacksmithArtifactFailureExitCode(err)
			result.ExitCode = code
		} else {
			if strings.TrimSpace(collected.Output) != "" {
				fmt.Fprintln(b.rt.Stderr, strings.TrimSpace(collected.Output))
			}
			for _, artifact := range collected.Artifacts {
				fmt.Fprintf(b.rt.Stderr, "artifact kind=%s path=%s bytes=%d\n", artifact.Kind, artifact.Path, artifact.Bytes)
			}
			result.Artifacts = append(result.Artifacts, collected.Artifacts...)
		}
	}
	report := delegatedTimingReport(blacksmithTestboxProvider, leaseID, slug, "blacksmith-testbox owns sync", commandDuration, commandPhases, total, code)
	report = core.TimingReportWithRunResult(report, result, nil)
	if code != 0 {
		classificationInput := string(stdoutProof.Bytes()) + "\n" + string(stderrProof.Bytes())
		if artifactErr != nil {
			classificationInput += "\n" + artifactErr.Error()
		}
		classification := core.ClassifyRunFailure(code, classificationInput, commandPhases)
		core.ApplyFailureClassification(&report, classification)
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
	finalExitCode = code
	finalActionsURL = result.ActionsURL
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

func (b *blacksmithBackend) CollectRunArtifacts(ctx context.Context, req core.DelegatedRunArtifactRequest) (core.DelegatedRunArtifactResult, error) {
	leaseID := strings.TrimSpace(firstNonBlank(req.Result.LeaseID, req.RunReq.ID))
	if leaseID == "" {
		return core.DelegatedRunArtifactResult{}, exit(2, "blacksmith artifact retrieval requires a testbox id")
	}
	if err := core.ValidateRunArtifactGlobs(req.RunReq.ArtifactGlobs); err != nil {
		return core.DelegatedRunArtifactResult{}, err
	}
	if err := core.ValidateRequiredRunArtifactGlobs(req.RunReq.RequiredArtifactGlobs); err != nil {
		return core.DelegatedRunArtifactResult{}, err
	}
	collectGlobs := append([]string{}, req.RunReq.ArtifactGlobs...)
	collectGlobs = append(collectGlobs, req.RunReq.RequiredArtifactGlobs...)
	script := core.DelegatedRunArtifactScript(req.RunReq.RequiredArtifactGlobs, collectGlobs, req.MaxFiles, req.MaxBytes)
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return core.DelegatedRunArtifactResult{}, err
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = core.DelegatedRunArtifactDefaultMaxBytes
	}
	captureLimit := blacksmithArtifactOutputCaptureLimit(maxBytes)
	stdout := newBlacksmithLimitedBuffer(captureLimit)
	stderr := newBlacksmithLimitedBuffer(captureLimit)
	args := blacksmithRunArgs(b.cfg, leaseID, keyPath, []string{script}, b.cfg.Blacksmith.Debug, true)
	_, timedOut, err := b.runCommandWithSyncGuardCapture(ctx, args, stdout, stderr, true)
	output := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if stdout.exceeded || stderr.exceeded {
		return core.DelegatedRunArtifactResult{}, exit(7, "blacksmith artifact output too large before archive validation: captured more than %d bytes", captureLimit)
	}
	if timedOut {
		fmt.Fprintf(
			b.rt.Stderr,
			"Blacksmith Testbox sync did not print a completion marker for %s during artifact retrieval; terminating local runner. "+
				"Rerun with CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS=0 to disable this guard.\n",
			blacksmithSyncTimeout(os.Getenv),
		)
		return core.DelegatedRunArtifactResult{}, exit(124, "blacksmith artifact retrieval sync timed out: %s", output)
	}
	if err != nil {
		return core.DelegatedRunArtifactResult{}, exit(7, "blacksmith artifact retrieval failed: %v: %s", err, output)
	}
	if len(collectGlobs) == 0 {
		return core.DelegatedRunArtifactResult{Output: output}, nil
	}
	archive, cleanOutput, err := blacksmithExtractArtifactArchive(output, maxBytes)
	if err != nil {
		return core.DelegatedRunArtifactResult{}, err
	}
	path := core.LocalRunArtifactPath(req.RunReq.Repo.Root, "", leaseID, "blacksmith-artifacts.tgz")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return core.DelegatedRunArtifactResult{}, exit(2, "blacksmith artifact create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		return core.DelegatedRunArtifactResult{}, exit(2, "blacksmith artifact write %s: %v", path, err)
	}
	return core.DelegatedRunArtifactResult{
		Output: strings.TrimSpace(cleanOutput),
		Artifacts: []core.RunArtifact{{
			Kind:  "artifact-glob",
			Path:  path,
			Bytes: len(archive),
		}},
	}, nil
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

const blacksmithProofStreamCaptureBytes = 1024 * 1024
const blacksmithArtifactDiagnosticCaptureBytes int64 = 64 * 1024

type blacksmithLimitedBuffer struct {
	bytes.Buffer
	limit    int64
	exceeded bool
}

func newBlacksmithLimitedBuffer(limit int64) *blacksmithLimitedBuffer {
	return &blacksmithLimitedBuffer{limit: limit}
}

func (b *blacksmithLimitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 || b.exceeded {
		b.exceeded = b.exceeded || b.limit > 0
		return len(p), nil
	}
	remaining := b.limit - int64(b.Buffer.Len())
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = b.Buffer.Write(p[:int(remaining)])
		b.exceeded = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

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
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.DoctorResult{
		Provider: blacksmithTestboxProvider,
		Message:  fmt.Sprintf("cli=ready control_plane=ready inventory=ready api=list mutation=false leases=%d runtime=ci_hydrated_by_provider", len(servers)),
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
	leaseID, err := resolveBlacksmithLeaseID(req.ID, "", false)
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
	leaseID, err := resolveBlacksmithLeaseID(req.ID, "", false)
	if err != nil {
		return err
	}
	if _, err := b.runCommand(ctx, blacksmithStopArgs(b.cfg, leaseID), b.rt.Stdout, b.rt.Stderr); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
	return nil
}

func (b *blacksmithBackend) warmupLease(ctx context.Context, repo Repo, reclaim bool, requestedSlug string) (string, string, error) {
	pendingID := "tbx_pending_" + strings.TrimPrefix(newLeaseID(), "cbx_")
	cleanupKeyID := pendingID
	defer func() {
		if cleanupKeyID != "" {
			removeStoredTestboxKey(cleanupKeyID)
		}
	}()
	_, publicKey, err := ensureTestboxKey(pendingID)
	if err != nil {
		return "", "", err
	}
	args, err := blacksmithWarmupArgs(b.cfg, publicKey)
	if err != nil {
		return "", "", err
	}
	beforeWarmup := b.listIDsBestEffort(ctx)
	result, err := b.runCommand(ctx, args, b.rt.Stdout, b.rt.Stderr)
	output := result.Stdout + result.Stderr
	if err != nil {
		b.cleanupFailedWarmup(ctx, beforeWarmup, output)
		return "", "", exit(result.ExitCode, "blacksmith testbox warmup failed: %v; if the delegated queue is unavailable, rerun with a coordinator-backed provider such as --provider aws", err)
	}
	leaseID := parseBlacksmithID(output)
	if leaseID == "" {
		return "", "", exit(5, "blacksmith testbox warmup did not print a tbx_ id")
	}
	if err := moveStoredTestboxKey(pendingID, leaseID); err != nil {
		_ = b.Stop(ctx, StopRequest{ID: leaseID})
		return "", "", exit(2, "store blacksmith key for %s: %v", leaseID, err)
	}
	cleanupKeyID = leaseID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		_ = b.Stop(ctx, StopRequest{ID: leaseID})
		return "", "", err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, blacksmithTestboxProvider, repo.Root, blacksmithIdleTimeout(b.cfg), reclaim); err != nil {
		_ = b.Stop(ctx, StopRequest{ID: leaseID})
		return "", "", err
	}
	if err := core.UpdateLeaseClaimCacheVolumes(leaseID, core.CacheVolumeStickyDiskSpecs(b.cfg.Cache.Volumes)); err != nil {
		_ = b.Stop(ctx, StopRequest{ID: leaseID})
		return "", "", err
	}
	cleanupKeyID = ""
	return leaseID, slug, nil
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
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{Name: "blacksmith", Args: args, Stdout: stdout, Stderr: stderr, DisableOutputCapture: disableOutputCapture})
	if err != nil {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("blacksmith failed: %v", err)}
	}
	return result, nil
}

func (b *blacksmithBackend) runCommandWithSyncGuard(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, bool, error) {
	return b.runCommandWithSyncGuardCapture(ctx, args, stdout, stderr, false)
}

func (b *blacksmithBackend) runCommandWithSyncGuardCapture(ctx context.Context, args []string, stdout, stderr io.Writer, disableOutputCapture bool) (LocalCommandResult, bool, error) {
	timeout := blacksmithSyncTimeout(os.Getenv)
	if timeout <= 0 {
		result, err := b.runCommandCapture(ctx, args, stdout, stderr, disableOutputCapture)
		return result, false, err
	}
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tracker := &blacksmithSyncTracker{}
	resultCh := make(chan struct {
		result LocalCommandResult
		err    error
	}, 1)
	go func() {
		result, err := b.runCommandCapture(
			guardCtx,
			args,
			blacksmithSyncGuardWriter{w: stdout, tracker: tracker},
			blacksmithSyncGuardWriter{w: stderr, tracker: tracker},
			disableOutputCapture,
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

func (b *blacksmithBackend) listIDsBestEffort(ctx context.Context) map[string]bool {
	out, err := b.commandOutput(ctx, blacksmithListAllArgs(b.cfg))
	if err != nil {
		return map[string]bool{}
	}
	ids := map[string]bool{}
	for _, item := range parseBlacksmithList(out) {
		ids[item.ID] = true
	}
	return ids
}

func (b *blacksmithBackend) cleanupFailedWarmup(ctx context.Context, before map[string]bool, output string) {
	if leaseID := parseBlacksmithID(output); leaseID != "" {
		if err := b.Stop(ctx, StopRequest{ID: leaseID}); err == nil {
			before[leaseID] = true
		}
	}
	stoppedAny := false
	quietAttempts := 0
	for attempt := 0; attempt < blacksmithCleanupAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(blacksmithCleanupDelay):
			}
		}
		list, err := b.commandOutput(ctx, blacksmithListAllArgs(b.cfg))
		if err != nil {
			return
		}
		stopped := false
		for _, item := range parseBlacksmithList(list) {
			if before[item.ID] || !blacksmithMatchesConfig(item, b.cfg) {
				continue
			}
			_ = b.Stop(ctx, StopRequest{ID: item.ID})
			before[item.ID] = true
			stopped = true
		}
		if stopped {
			stoppedAny = true
			quietAttempts = 0
			continue
		}
		if stoppedAny {
			quietAttempts++
			if quietAttempts >= blacksmithCleanupQuiet {
				return
			}
		}
	}
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

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func ensureTestboxKey(leaseID string) (string, string, error) {
	return core.EnsureTestboxKey(leaseID)
}

func moveStoredTestboxKey(oldLeaseID, newLeaseID string) error {
	return core.MoveStoredTestboxKey(oldLeaseID, newLeaseID)
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
