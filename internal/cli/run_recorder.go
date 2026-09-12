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
)

type runRecorder struct {
	coord              *CoordinatorClient
	command            []string
	label              string
	runID              string
	requestedRunID     string
	createLeaseID      string
	createConfig       *Config
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
	terminalConfirmed  bool
	warned             bool
	warnMu             sync.Mutex
	publisher          *runEventPublisher
	telemetryStart     *LeaseTelemetry
	telemetryMu        sync.Mutex
	telemetrySamples   []*LeaseTelemetry
	telemetryCancel    func()
	telemetryDone      chan struct{}
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
		return exit(7, "run admission %s belongs to a different coordinator; restore the original route", r.requestedRunID)
	}
	r.coord = coord
	return nil
}

func (r *runRecorder) createRun(ctx context.Context, leaseID string, cfg Config) error {
	// Replay the first admission payload; lease.created owns later lease attribution.
	if r.createConfig == nil {
		r.createLeaseID = leaseID
		r.createConfig = &Config{Provider: cfg.Provider, TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode, Class: cfg.Class, ServerType: cfg.ServerType}
		r.createBaseURL = strings.TrimRight(r.coord.BaseURL, "/")
		fmt.Fprintf(r.stderr, "run admission attempt %s\n", r.requestedRunID)
	}
	run, err := r.coord.CreateRun(ctx, r.requestedRunID, r.createLeaseID, *r.createConfig, r.command, r.label)
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
	if r == nil || r.coord == nil || r.runID != "" {
		return nil
	}
	if r.createErr != nil {
		return exit(7, "run admission %s unavailable before command: %v", r.requestedRunID, r.createErr)
	}
	return exit(7, "run history unavailable before command; refusing execution without a coordinator run handle")
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
		if err := postRunEvent(context.Background(), r.coord, r.runID, input); err != nil {
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
			return exit(7, "run admission %s unavailable before command: %v", r.requestedRunID, err)
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
			return exit(7, "run history lease attribution failed for %s: %v", r.runID, err)
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
	r.telemetryStart = collectLeaseTelemetryBestEffort(contextWithoutWorkspaceOwner(ctx), leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(r.telemetryStart)
}

func (r *runRecorder) StartTelemetrySampler(ctx context.Context, target SSHTarget) {
	if r == nil || r.coord == nil || r.runID == "" {
		return
	}
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
	r.leaseID, r.leaseSlug, r.leaseProvider = run.LeaseID, run.Slug, run.Provider
	r.startedAt, _ = time.Parse(time.RFC3339Nano, run.StartedAt)
	r.attachedAt = time.Now()
	r.createPending = false
	r.historyUnavailable = false
	r.publisher = newRunEventPublisher(r.handleRunEventAppendError)
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
	r.waitForEvents(runEventOutputPostWait)
	r.stopTelemetrySampler()
	telemetryEnd := collectLeaseTelemetryBestEffort(contextWithoutWorkspaceOwner(ctx), leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(telemetryEnd)
	telemetry := runTelemetrySummary(r.telemetryStart, telemetryEnd, r.telemetrySnapshot())
	ctx, cancel := context.WithTimeout(context.Background(), runRecorderFinishTimeout)
	defer cancel()
	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= runRecorderFinishAttempts && ctx.Err() == nil; attempt++ {
		attempts = attempt
		_, finishErr := r.coord.FinishRun(ctx, r.runID, exitCode, sync, command, log, truncated, results, telemetry, classification, receipt)
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
			committed, receiptErr := r.coord.RunReceipt(ctx, r.runID)
			if receiptErr == nil {
				if committed == *receipt {
					r.finished = true
					r.terminalConfirmed = true
					return nil
				}
				lastErr = fmt.Errorf("stored terminal receipt differs from the signed finish payload")
				break
			}
			lastErr = errors.Join(lastErr, fmt.Errorf("verify persisted terminal receipt: %w", receiptErr))
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
	return exit(7, "run history terminal commit failed for %s after %d attempts: %v; recover with `crabbox receipt %s`", r.runID, attempts, lastErr, r.runID)
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
	r.waitForEvents(runEventOutputPostWait)
	r.stopTelemetrySampler()
	if r.runID == "" || r.finished || err == nil {
		return
	}
	r.finished = true
	r.appendEvent("run.failed", CoordinatorRunEventInput{
		Type:    "run.failed",
		Phase:   "failed",
		Message: err.Error(),
	})
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
	fmt.Fprintf(r.stderr, "warning: "+format+"\n", args...)
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

func (r *runRecorder) resetTelemetryForLeaseReplacement() {
	if r == nil {
		return
	}
	r.stopTelemetrySampler()
	r.telemetryMu.Lock()
	r.telemetryStart = nil
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
