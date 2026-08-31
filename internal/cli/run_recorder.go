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
	startedAt          time.Time
	attachedAt         time.Time
	stderr             io.Writer
	diagnosticConfig   Config
	diagnosticSecrets  []string
	createPending      bool
	historyUnavailable bool
	eventsMu           sync.Mutex
	eventsDisabled     bool
	finished           bool
	warned             bool
	warnMu             sync.Mutex
	output             *runOutputEventQueue
	telemetryStart     *LeaseTelemetry
	telemetryMu        sync.Mutex
	telemetrySamples   []*LeaseTelemetry
	telemetryCancel    func()
	telemetryDone      chan struct{}
}

func newRunRecorder(ctx context.Context, coord *CoordinatorClient, cfg Config, command []string, label string, stderr io.Writer, createAfterLease bool) *runRecorder {
	rec := &runRecorder{
		coord:             coord,
		command:           command,
		label:             strings.TrimSpace(label),
		stderr:            stderr,
		diagnosticConfig:  cfg,
		diagnosticSecrets: configuredDiagnosticSecrets(cfg),
	}
	if coord == nil {
		return rec
	}
	if createAfterLease {
		rec.createPending = true
		return rec
	}
	run, err := coord.CreateRun(ctx, "", cfg, command, rec.label)
	if err != nil {
		rec.createPending = true
		if isInvalidLeaseIDCoordinatorError(err) {
			return rec
		}
		rec.warnRunHistory("run history create failed before lease; will retry after lease is available: %v", err)
		return rec
	}
	rec.attachRun(run)
	return rec
}

func (r *runRecorder) UseCoordinator(coord *CoordinatorClient) {
	if r == nil || coord == nil {
		return
	}
	r.coord = coord
}

func (r *runRecorder) historyIsUnavailable() bool {
	return r == nil || r.runID == "" || r.historyUnavailable
}

func (r *runRecorder) requireHandle() error {
	if r == nil || r.coord == nil || r.runID != "" {
		return nil
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
	if r == nil || r.coord == nil || r.runID == "" || !r.runEventsEnabled() {
		return
	}
	if input.Message != "" {
		secrets := append([]string(nil), r.diagnosticSecrets...)
		secrets = append(secrets, configuredDiagnosticSecrets(r.diagnosticConfig)...)
		input.Message = RedactDiagnosticSecrets(input.Message, secrets...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runRecorderRequestTimeout)
	defer cancel()
	_, err := r.coord.AppendRunEvent(ctx, r.runID, input)
	if err != nil {
		r.handleRunEventAppendError(kind, err)
	}
}

func (r *runRecorder) AttachLease(leaseID, slug string, cfg Config) {
	if r == nil || r.finished {
		return
	}
	if r.runID == "" && r.createPending && r.coord != nil && leaseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), runRecorderRequestTimeout)
		defer cancel()
		run, err := r.coord.CreateRun(ctx, leaseID, cfg, r.command, r.label)
		if err != nil {
			r.historyUnavailable = true
			r.warnRunHistory("run history create failed after lease; run history unavailable, use lease-based recovery commands: %v", err)
			return
		}
		r.attachRun(run)
	}
	if r.runID == "" {
		return
	}
	r.appendEvent("lease.created", CoordinatorRunEventInput{
		Type:        "lease.created",
		Phase:       "leased",
		LeaseID:     leaseID,
		Slug:        slug,
		Provider:    cfg.Provider,
		TargetOS:    cfg.TargetOS,
		WindowsMode: cfg.WindowsMode,
		Class:       cfg.Class,
		ServerType:  cfg.ServerType,
	})
}

func (r *runRecorder) CaptureTelemetryStart(ctx context.Context, target SSHTarget) {
	if r == nil || r.coord == nil || r.runID == "" || r.telemetryStart != nil {
		return
	}
	r.telemetryStart = collectLeaseTelemetryBestEffort(contextWithoutWorkspaceOwner(ctx), leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(r.telemetryStart)
	r.appendTelemetryBestEffort(r.telemetryStart)
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
	go func() {
		defer close(done)
		ticker := time.NewTicker(runTelemetrySampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample := collectLeaseTelemetryBestEffort(sampleCtx, collector)
				r.recordTelemetrySample(sample)
				r.appendTelemetryBestEffort(sample)
			case <-sampleCtx.Done():
				return
			}
		}
	}()
}

func (r *runRecorder) attachRun(run CoordinatorRun) {
	r.runID = run.ID
	r.startedAt, _ = time.Parse(time.RFC3339Nano, run.StartedAt)
	r.attachedAt = time.Now()
	r.createPending = false
	r.historyUnavailable = false
	r.output = newRunOutputEventQueue(r.coord, run.ID, r.handleRunEventAppendError)
	fmt.Fprintf(r.stderr, "recording run %s\n", run.ID)
}

func (r *runRecorder) StreamWriter(stream string) *runEventStreamWriter {
	if r != nil && r.output == nil && r.coord != nil && r.runID != "" {
		r.output = newRunOutputEventQueue(r.coord, r.runID, r.handleRunEventAppendError)
	}
	return &runEventStreamWriter{recorder: r, stream: stream}
}

func (r *runRecorder) Finish(ctx context.Context, target SSHTarget, exitCode int, sync, command time.Duration, log string, truncated bool, results *TestResultSummary, classification FailureClassification, receipt *terminalRunReceipt) error {
	if r == nil || r.runID == "" || r.finished {
		return nil
	}
	r.waitForOutputEvents(runEventOutputPostWait)
	r.stopTelemetrySampler()
	telemetryEnd := collectLeaseTelemetryBestEffort(contextWithoutWorkspaceOwner(ctx), leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(telemetryEnd)
	telemetry := runTelemetrySummary(r.telemetryStart, telemetryEnd, r.telemetrySnapshot())
	ctx, cancel := context.WithTimeout(context.Background(), runRecorderFinishTimeout)
	defer cancel()
	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= runRecorderFinishAttempts; attempt++ {
		attempts = attempt
		_, finishErr := r.coord.FinishRun(ctx, r.runID, exitCode, sync, command, log, truncated, results, telemetry, classification, receipt)
		if finishErr == nil && receipt == nil {
			r.finished = true
			return nil
		}
		if receipt != nil {
			committed, receiptErr := r.coord.RunReceipt(ctx, r.runID)
			if receiptErr == nil {
				if committed == *receipt {
					r.finished = true
					return nil
				}
				lastErr = fmt.Errorf("stored terminal receipt differs from the signed finish payload")
				break
			}
			if finishErr == nil {
				lastErr = fmt.Errorf("verify persisted terminal receipt: %w", receiptErr)
				if attempt == runRecorderFinishAttempts || !runRecorderFinishRetryable(receiptErr) {
					break
				}
			} else {
				lastErr = finishErr
				if attempt == runRecorderFinishAttempts || !runRecorderFinishRetryable(finishErr) {
					break
				}
			}
		} else {
			lastErr = finishErr
			if attempt == runRecorderFinishAttempts || !runRecorderFinishRetryable(finishErr) {
				break
			}
		}
		timer := time.NewTimer(runRecorderFinishRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return exit(7, "run history terminal commit failed for %s: %v", r.runID, ctx.Err())
		case <-timer.C:
		}
	}
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
	if r == nil || r.runID == "" || r.finished || err == nil {
		return
	}
	r.waitForOutputEvents(runEventOutputPostWait)
	r.finished = true
	r.stopTelemetrySampler()
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

func (r *runRecorder) appendTelemetryBestEffort(sample *LeaseTelemetry) {
	if r == nil || r.coord == nil || r.runID == "" || sample == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.coord.AppendRunTelemetry(ctx, r.runID, sample); err != nil && !isCoordinatorNotFoundError(err) {
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
	select {
	case <-done:
	case <-time.After(time.Second):
	}
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

func (r *runRecorder) waitForOutputEvents(timeout time.Duration) {
	if r == nil || r.output == nil {
		return
	}
	r.output.CloseAndWait(timeout)
}

func (r *runRecorder) runEventsEnabled() bool {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	return !r.eventsDisabled
}

func (r *runRecorder) disableRunEvents() {
	r.eventsMu.Lock()
	r.eventsDisabled = true
	r.eventsMu.Unlock()
	if r.output != nil {
		r.output.Disable()
	}
}

func (r *runRecorder) handleRunEventAppendError(kind string, err error) bool {
	if isCoordinatorNotFoundError(err) {
		r.disableRunEvents()
		return false
	}
	r.warn("run event append failed for %s: %v", kind, err)
	return true
}

func isInvalidLeaseIDCoordinatorError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid_lease_id")
}
