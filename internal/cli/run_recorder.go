package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	runTelemetrySampleInterval = 15 * time.Second
	runRecorderRequestTimeout  = 10 * time.Second
	runRecorderFinishTimeout   = 60 * time.Second
	runRecorderFinishAttempts  = 3
	runRecorderFinishRetry     = 250 * time.Millisecond
	// Leave recovery time after a saturated coordinator's per-request timeout.
	runAdmissionBudget   = 30 * time.Second
	runAdmissionAttempts = 4
)

// Production backoff honors Retry-After; sequential tests may stub it to avoid real sleeps.
var runAdmissionRetryDelay = coordinatorReadBackoff

type runRecorder struct {
	coord              *CoordinatorClient
	command            []string
	label              string
	runID              string
	runOwner           string
	runOrg             string
	requestedRunID     string
	createLeaseID      string
	createConfig       *Config
	createCoordinator  *CoordinatorClient
	createBaseURL      string
	createErr          error
	startedAt          time.Time
	attachedAt         time.Time
	stderr             io.Writer
	diagnosticConfig   Config
	diagnosticSecrets  []string
	createPending      bool
	historyUnavailable bool
	leaseID            string
	leaseSlug          string
	leaseProvider      string
	finished           bool
	terminalAttempted  bool
	terminalConfirmed  bool
	warned             bool
	warnMu             sync.Mutex
	publisher          *runEventPublisher
	telemetryStart     *LeaseTelemetry
	telemetryRequested bool
	telemetryEnd       *LeaseTelemetry
	telemetryEndFrozen bool
	telemetryMu        sync.Mutex
	telemetrySamples   []*LeaseTelemetry
	telemetryCancel    func()
	telemetryDone      chan struct{}

	pendingFinishCleanupErr error
}

func newRunRecorder(ctx context.Context, coord *CoordinatorClient, cfg Config, command []string, label string, stderr io.Writer, createAfterLease bool, requestedRunID string) *runRecorder {
	rec := &runRecorder{
		coord:             coord,
		command:           append([]string(nil), command...),
		requestedRunID:    requestedRunID,
		createPending:     true,
		label:             strings.TrimSpace(label),
		stderr:            stderr,
		diagnosticConfig:  cfg,
		diagnosticSecrets: configuredDiagnosticSecrets(cfg),
	}
	if coord != nil && !createAfterLease {
		if err := rec.createRun(ctx, "", cfg); err != nil {
			rec.warnRunHistory("run admission %s failed before lease: %v", requestedRunID, err)
		}
	}

	return rec
}

func (r *runRecorder) UseCoordinator(coord *CoordinatorClient) error {
	if r == nil || coord == nil {
		return nil
	}
	if r.createBaseURL != "" && strings.TrimRight(coord.BaseURL, "/") != r.createBaseURL {
		return Exit(7, "run admission %s belongs to a different coordinator; restore the original route", r.requestedRunID)
	}
	r.coord = coord
	return nil
}

func (r *runRecorder) createRun(ctx context.Context, leaseID string, cfg Config) error {
	// Replay the first admission payload; lease.created owns later lease attribution.
	if r.createConfig == nil {
		r.createLeaseID = leaseID
		r.createConfig = &Config{Provider: cfg.Provider, TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode, Class: cfg.Class, ServerType: cfg.ServerType}
		// Keep admission replay and terminal bookkeeping on the initiating route
		// and credential source. Renewable credentials are refreshed at finish.
		r.createCoordinator = &CoordinatorClient{
			BaseURL: r.coord.BaseURL, Token: r.coord.Token, Access: r.coord.Access,
			TokenCommand:     append([]string(nil), r.coord.TokenCommand...),
			ChildEnvDenylist: append([]string(nil), r.coord.ChildEnvDenylist...),
			Client:           r.coord.Client,
			readRetryWriter:  r.stderr,
			admissionAuth:    &coordinatorAdmissionAuth{},
		}
		r.createBaseURL = strings.TrimRight(r.coord.BaseURL, "/")
		fmt.Fprintf(r.stderr, "run admission attempt %s\n", r.requestedRunID)
	}
	run, err := r.createCoordinator.CreateRun(ctx, r.requestedRunID, r.createLeaseID, *r.createConfig, r.command, r.label)
	if binding := r.createCoordinator.admissionAuth; binding != nil {
		r.diagnosticSecrets = append(r.diagnosticSecrets, strings.TrimPrefix(binding.headers.Get("Authorization"), "Bearer "), binding.headers.Get("CF-Access-Client-Secret"), binding.headers.Get("cf-access-token"))
	}
	if err != nil {
		r.historyUnavailable = true
		r.createErr = err
		var refusal ExitError
		r.createPending = !AsExitError(err, &refusal) && runRecorderFinishRetryable(err)
		return err
	}
	r.createErr = nil
	r.attachRun(run)
	return nil
}

func (r *runRecorder) historyIsUnavailable() bool {
	return r == nil || r.runID == "" || r.historyUnavailable
}

func (r *runRecorder) requireHandle() error {
	if r != nil && r.finished {
		return Exit(7, "run admission %s is already finalized; refusing execution", r.requestedRunID)
	}
	if r == nil || r.coord == nil || r.runID != "" {
		return nil
	}
	if r.createErr != nil {
		return r.admissionError(r.createErr)
	}
	return Exit(7, "run history unavailable before command; refusing execution without a coordinator run handle")
}

func (r *runRecorder) Event(kind, phase, message string) {
	if r == nil || r.runID == "" || (r.finished && kind != "lease.released") {
		return
	}
	r.appendEvent(kind, CoordinatorRunEventInput{
		Type:    kind,
		Phase:   phase,
		Message: message,
	})
}

func (r *runRecorder) appendEvent(kind string, input CoordinatorRunEventInput) {
	if r == nil || r.coord == nil || r.runID == "" {
		return
	}
	if input.Message != "" {
		secrets := append([]string(nil), r.diagnosticSecrets...)
		secrets = append(secrets, configuredDiagnosticSecrets(r.diagnosticConfig)...)
		input.Message = RedactDiagnosticSecrets(input.Message, secrets...)
	}
	if r.publisher == nil {
		r.publisher = newRunEventPublisher(r.handleRunEventAppendError)
	}
	if r.finished {
		if err := r.publisher.postEvent(context.Background(), r.coord, r.runID, input); err != nil {
			r.handleRunEventAppendError(kind, err)
		}
		return
	}
	r.publisher.append(r.coord, r.runID, input)
}

func (r *runRecorder) AttachLease(ctx context.Context, leaseID, slug string, cfg Config) error {
	if r == nil || r.finished {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.runID == "" && r.createPending && r.coord != nil && leaseID != "" {
		if err := r.createRun(ctx, leaseID, cfg); err != nil {
			return r.admissionError(err)
		}
	}

	if r.runID == "" {
		return r.requireHandle()
	}
	input := CoordinatorRunEventInput{
		Type:        "lease.created",
		Phase:       "leased",
		LeaseID:     leaseID,
		Slug:        slug,
		Provider:    cfg.Provider,
		TargetOS:    cfg.TargetOS,
		WindowsMode: cfg.WindowsMode,
		Class:       cfg.Class,
		ServerType:  cfg.ServerType,
	}
	if r.publisher == nil {
		r.publisher = newRunEventPublisher(r.handleRunEventAppendError)
	}
	// CreateRun already binds an existing lease. Only initial attribution and
	// replacement need acknowledgement before the command's receipt can bind it.
	needsBinding := r.leaseID != leaseID || r.leaseSlug != slug || r.leaseProvider != cfg.Provider
	if needsBinding {
		if err := r.publisher.Bind(ctx, r.coord, r.runID, input); err != nil {
			return Exit(7, "run history lease attribution failed for %s: %v", r.runID, err)
		}
	} else {
		r.publisher.append(r.coord, r.runID, input)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.leaseID, r.leaseSlug, r.leaseProvider = leaseID, slug, cfg.Provider
	return nil
}

func (r *runRecorder) CaptureTelemetryStart(ctx context.Context, target SSHTarget) {
	if r == nil || r.coord == nil || r.runID == "" || r.telemetryStart != nil {
		return
	}
	r.telemetryRequested = true
	r.telemetryStart = collectLeaseTelemetryBestEffort(contextWithoutWorkspaceOwner(ctx), leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(r.telemetryStart)
}

func (r *runRecorder) StartTelemetrySampler(ctx context.Context, target SSHTarget) {
	if r == nil || r.coord == nil || r.runID == "" {
		return
	}
	r.telemetryRequested = true
	r.telemetryMu.Lock()
	if r.telemetryCancel != nil {
		r.telemetryMu.Unlock()
		return
	}
	sampleCtx, cancel := context.WithCancel(contextWithoutWorkspaceOwner(ctx))
	done := make(chan struct{})
	r.telemetryCancel = cancel
	r.telemetryDone = done
	r.telemetryMu.Unlock()

	collector := leaseTelemetryCollectorForTarget(target)
	initial := r.telemetryStart
	go func() {
		defer close(done)
		ticker := time.NewTicker(runTelemetrySampleInterval)
		defer ticker.Stop()
		// Preserve the pre-command baseline without making workload admission wait
		// for its best-effort publication. This owner also joins it before finish.
		r.appendTelemetryBestEffort(sampleCtx, initial)
		for {
			select {
			case <-ticker.C:
				sample := collectLeaseTelemetryBestEffort(sampleCtx, collector)
				r.recordTelemetrySample(sample)
				r.appendTelemetryBestEffort(sampleCtx, sample)
			case <-sampleCtx.Done():
				return
			}
		}
	}()
}

func (r *runRecorder) attachRun(run CoordinatorRun) {
	r.runID = run.ID
	r.runOwner, r.runOrg = run.Owner, run.Org
	r.leaseID, r.leaseSlug, r.leaseProvider = run.LeaseID, run.Slug, run.Provider
	r.startedAt, _ = time.Parse(time.RFC3339Nano, run.StartedAt)
	r.attachedAt = time.Now()
	r.createPending = false
	r.historyUnavailable = false
	r.publisher = newRunEventPublisher(r.handleRunEventAppendError)
	r.publisher.idempotent = run.EventAppendIdempotent
	fmt.Fprintf(r.stderr, "recording run %s\n", run.ID)
}

func (r *runRecorder) StreamWriter(stream string) *runEventStreamWriter {
	if r != nil && r.publisher == nil && r.coord != nil && r.runID != "" {
		r.publisher = newRunEventPublisher(r.handleRunEventAppendError)
	}
	return &runEventStreamWriter{recorder: r, stream: stream}
}

func (r *runRecorder) Finish(ctx context.Context, target SSHTarget, exitCode int, sync, command time.Duration, log string, truncated bool, results *TestResultSummary, classification FailureClassification, receipt *terminalRunReceipt) error {
	if r == nil || r.runID == "" || r.finished {
		return nil
	}
	r.terminalAttempted = true
	eventsDrained := make(chan struct{})
	go func() {
		r.waitForEvents(r.publisher.drainTimeout())
		close(eventsDrained)
	}()
	r.CaptureTelemetryEnd(ctx, target)
	<-eventsDrained
	telemetry := runTelemetrySummary(r.telemetryStart, r.telemetryEnd, r.telemetrySnapshot())
	ctx, cancel := context.WithTimeout(context.Background(), runRecorderFinishTimeout)
	defer cancel()
	data, encodeErr := encodeRunFinish(exitCode, sync, command, log, truncated, results, telemetry, classification, receipt)
	if encodeErr != nil {
		return Exit(7, "encode terminal finish for %s: %v", r.runID, encodeErr)
	}
	coord := r.coord
	if r.createCoordinator != nil {
		coord = r.createCoordinator
	}
	var pendingPath string
	if receipt != nil {
		pendingPath, encodeErr = retainPendingRunFinish(pendingRunFinish{
			SchemaVersion: 1, Coordinator: strings.TrimRight(coord.BaseURL, "/"),
			RunID: r.runID, Owner: r.runOwner, Org: r.runOrg,
			Body: data, BodySHA256: sha256Digest(data),
		})
		if encodeErr != nil {
			return Exit(7, "retain original terminal finish for %s before submission: %v", r.runID, encodeErr)
		}
	}
	// Retain the signed original before refreshing: an unavailable helper or
	// changed identity must not discard the only recoverable finish bytes.
	coord, authErr := r.finishCoordinator(ctx, coord)
	lastErr := authErr
	attempts := 0
	for attempt := 1; authErr == nil && attempt <= runRecorderFinishAttempts && ctx.Err() == nil; attempt++ {
		attempts = attempt
		_, finishErr := coord.finishRunBytes(ctx, r.runID, data)
		if finishErr == nil && receipt == nil {
			r.finished = true
			r.terminalConfirmed = true
			return nil
		}
		lastErr = nil
		if finishErr != nil {
			lastErr = fmt.Errorf("submit terminal result: %w", finishErr)
		}
		retryErr := finishErr
		if receipt != nil {
			committed, receiptErr := coord.RunReceipt(ctx, r.runID)
			if receiptErr == nil {
				if committed == *receipt && finishErr == nil {
					// The caller owns terminal output ordering. Cleanup is diagnostic:
					// it cannot change the confirmed original task result.
					r.pendingFinishCleanupErr = removePendingRunFinish(pendingPath)
					r.finished = true
					r.terminalConfirmed = true
					return nil
				}
				if committed != *receipt {
					lastErr = errors.Join(lastErr, fmt.Errorf("stored terminal receipt differs from the signed finish payload"))
					break
				}
				// The receipt does not bind results/telemetry. Require a successful
				// idempotent POST of the full original before accepting a lost response.
			} else {
				lastErr = errors.Join(lastErr, fmt.Errorf("verify persisted terminal receipt: %w", receiptErr))
			}
			// Preserve both diagnostics without letting a secondary read error
			// change whether the original finish request can be retried.
			if retryErr == nil {
				retryErr = receiptErr
			}
		}
		if attempt == runRecorderFinishAttempts || !runRecorderFinishRetryable(retryErr) {
			break
		}
		timer := time.NewTimer(runRecorderFinishRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	lastErr = errors.Join(lastErr, ctx.Err())
	if pendingPath != "" {
		// Keep recovery pins with the failure instead of appending progress text
		// after the caller has emitted its final machine-readable timing report.
		return Exit(7, "run history terminal commit failed for %s after %d attempts: %v; recover with `crabbox receipt %s`; pending signed finish: run=%q file=%q signer=%s finish-sha256=%s; use receipt --recover with these original pins", r.runID, attempts, lastErr, r.runID, r.runID, pendingPath, receipt.Signer, sha256Digest(data))
	}
	return Exit(7, "run history terminal commit failed for %s after %d attempts: %v; recover with `crabbox receipt %s`", r.runID, attempts, lastErr, r.runID)
}

// finishCoordinator renews only renewable credentials. Static admission headers
// have no refresh source and retain their original binding. Never select a later
// r.coord or mutate createCoordinator: admission retries must still use the
// original credentials, while all finish requests share one fresh authority.
func (r *runRecorder) finishCoordinator(ctx context.Context, original *CoordinatorClient) (*CoordinatorClient, error) {
	if r.createCoordinator == nil || len(original.TokenCommand) == 0 {
		return original, nil
	}
	if r.runOwner == "" || r.runOrg == "" {
		return nil, errors.New("cannot refresh terminal credentials without the admitted run owner and organization")
	}
	headers := make(http.Header)
	// resolveRequestHeaders intentionally bypasses the admission snapshot once.
	if err := original.resolveRequestHeaders(ctx, headers); err != nil {
		return nil, fmt.Errorf("refresh terminal credentials: %w", err)
	}
	r.diagnosticSecrets = append(r.diagnosticSecrets, strings.TrimPrefix(headers.Get("Authorization"), "Bearer "), headers.Get("CF-Access-Client-Secret"), headers.Get("cf-access-token"))
	bound := &CoordinatorClient{
		BaseURL: original.BaseURL, Client: original.Client,
		readRetryWriter: original.readRetryWriter,
		admissionAuth:   &coordinatorAdmissionAuth{captured: true, headers: headers},
	}
	who, err := bound.Whoami(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify refreshed terminal identity: %w", err)
	}
	if who.Owner != r.runOwner || who.Org != r.runOrg {
		return nil, errors.New("refreshed terminal owner or organization mismatch")
	}
	run, err := bound.Run(ctx, r.runID)
	if err != nil {
		return nil, fmt.Errorf("verify admitted terminal run: %w", err)
	}
	if run.ID != r.runID || run.Owner != r.runOwner || run.Org != r.runOrg {
		return nil, errors.New("admitted run owner or organization mismatch")
	}
	return bound, nil
}

func runRecorderFinishRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var httpErr CoordinatorHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}
	return true
}

func (r *runRecorder) Failed(err error) {
	if r == nil {
		return
	}
	r.waitForEvents(r.publisher.drainTimeout())
	r.stopTelemetrySampler()
	if r.finished || err == nil || r.terminalAttempted {
		return
	}
	if r.runID == "" {
		r.failUnacknowledgedAdmission(err)
		return
	}
	r.finished = true
	r.appendEvent("run.failed", CoordinatorRunEventInput{
		Type:    "run.failed",
		Phase:   "failed",
		Message: err.Error(),
	})
}

func (r *runRecorder) failUnacknowledgedAdmission(primary error) {
	if r.createCoordinator == nil || r.createConfig == nil || r.requestedRunID == "" {
		return
	}
	r.terminalAttempted = true
	ctx, cancel := context.WithTimeout(context.Background(), runRecorderFinishTimeout)
	defer cancel()
	message := r.redactDiagnostic(primary.Error())
	code := ExitCodeForError(primary, 7)
	var lastErr error
	for attempt := 1; attempt <= runRecorderFinishAttempts && ctx.Err() == nil; attempt++ {
		_, lastErr = r.createCoordinator.FailRunAdmission(ctx, r.requestedRunID, r.createLeaseID, *r.createConfig, r.command, r.label, code, message)
		if lastErr == nil {
			r.runID = r.requestedRunID
			r.finished, r.terminalConfirmed = true, true
			r.historyUnavailable = false
			return
		}
		if attempt == runRecorderFinishAttempts || !runRecorderFinishRetryable(lastErr) {
			break
		}
		if err := sleepContext(ctx, runRecorderFinishRetry); err != nil {
			break
		}
	}
	r.historyUnavailable = true
	r.warnRunHistory("run history finalization unconfirmed for %s: %v; no workload was started; inspect the original run history without replaying the workload", r.requestedRunID, errors.Join(lastErr, ctx.Err()))
}

func (r *runRecorder) warn(format string, args ...any) {
	if r == nil {
		return
	}
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warned {
		return
	}
	r.warned = true
	fmt.Fprintf(r.stderr, "warning: "+format+"\n", args...)
}

func (r *runRecorder) warnRunHistory(format string, args ...any) {
	if r == nil {
		return
	}
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	fmt.Fprintf(r.stderr, "warning: %s\n", r.redactDiagnostic(fmt.Sprintf(format, args...)))
}

func (r *runRecorder) admissionError(err error) error {
	return Exit(7, "%s", r.redactDiagnostic(fmt.Sprintf("run admission %s unavailable before command: %v", r.requestedRunID, err)))
}

func (r *runRecorder) redactDiagnostic(message string) string {
	secrets := append([]string(nil), r.diagnosticSecrets...)
	secrets = append(secrets, configuredDiagnosticSecrets(r.diagnosticConfig)...)
	return RedactDiagnosticSecrets(message, secrets...)
}

func (r *runRecorder) recordTelemetrySample(sample *LeaseTelemetry) {
	if r == nil || sample == nil || sample.CapturedAt == "" {
		return
	}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	for index, existing := range r.telemetrySamples {
		if existing != nil && existing.CapturedAt == sample.CapturedAt {
			r.telemetrySamples[index] = sample
			return
		}
	}
	r.telemetrySamples = append(r.telemetrySamples, sample)
	if len(r.telemetrySamples) > 60 {
		r.telemetrySamples = r.telemetrySamples[len(r.telemetrySamples)-60:]
	}
}

func (r *runRecorder) telemetrySnapshot() []*LeaseTelemetry {
	if r == nil {
		return nil
	}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	if len(r.telemetrySamples) == 0 {
		return nil
	}
	samples := make([]*LeaseTelemetry, len(r.telemetrySamples))
	copy(samples, r.telemetrySamples)
	return samples
}

func (r *runRecorder) appendTelemetryBestEffort(ctx context.Context, sample *LeaseTelemetry) {
	if r == nil || r.coord == nil || r.runID == "" || sample == nil {
		return
	}
	postCtx, cancel := context.WithTimeout(ctx, runRecorderRequestTimeout)
	defer cancel()
	if _, err := r.coord.AppendRunTelemetry(postCtx, r.runID, sample); err != nil && ctx.Err() == nil && !isCoordinatorNotFoundError(err) {
		r.warn("run telemetry append failed for %s: %v", r.runID, err)
	}
}

func (r *runRecorder) stopTelemetrySampler() {
	if r == nil {
		return
	}
	r.telemetryMu.Lock()
	cancel := r.telemetryCancel
	done := r.telemetryDone
	r.telemetryCancel = nil
	r.telemetryDone = nil
	r.telemetryMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// CaptureTelemetryEnd is called by the run owner before lease cleanup can
// revoke guest access. Finish may subsequently publish without another SSH call.
func (r *runRecorder) CaptureTelemetryEnd(ctx context.Context, target SSHTarget) {
	if r == nil || r.runID == "" || r.telemetryEndFrozen {
		return
	}
	r.stopTelemetrySampler()
	r.telemetryEnd = collectLeaseTelemetryBestEffort(contextWithoutWorkspaceOwner(ctx), leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(r.telemetryEnd)
	r.telemetryEndFrozen = true
}

func (r *runRecorder) resetTelemetryForLeaseReplacement() {
	if r == nil {
		return
	}
	r.stopTelemetrySampler()
	r.telemetryMu.Lock()
	r.telemetryStart = nil
	r.telemetryRequested = false
	r.telemetryEnd = nil
	r.telemetryEndFrozen = false
	r.telemetrySamples = nil
	r.telemetryMu.Unlock()
}

func (r *runRecorder) waitForEvents(timeout time.Duration) {
	if r == nil || r.publisher == nil {
		return
	}
	r.publisher.CloseAndWait(timeout)
}

func (r *runRecorder) handleRunEventAppendError(kind string, err error) bool {
	if isCoordinatorNotFoundError(err) {
		return false
	}
	r.warn("run event append failed for %s: %v", kind, err)
	return true
}
