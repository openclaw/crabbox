package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	runTelemetrySampleInterval = 15 * time.Second
	runRecorderRequestTimeout  = 10 * time.Second
	runRecorderFinishTimeout   = 60 * time.Second
)

type runRecorder struct {
	coord            *CoordinatorClient
	command          []string
	runID            string
	stderr           io.Writer
	deferUntilLease  bool
	eventsMu         sync.Mutex
	eventsDisabled   bool
	finished         bool
	warned           bool
	warnMu           sync.Mutex
	output           *runOutputEventQueue
	telemetryStart   *LeaseTelemetry
	telemetryMu      sync.Mutex
	telemetrySamples []*LeaseTelemetry
	telemetryCancel  func()
	telemetryDone    chan struct{}
}

func newRunRecorder(ctx context.Context, coord *CoordinatorClient, cfg Config, command []string, stderr io.Writer) *runRecorder {
	rec := &runRecorder{coord: coord, command: command, stderr: stderr}
	if coord == nil {
		return rec
	}
	run, err := coord.CreateRun(ctx, "", cfg, command)
	if err != nil {
		if isInvalidLeaseIDCoordinatorError(err) {
			rec.deferUntilLease = true
			return rec
		}
		rec.warn("run history create failed: %v", err)
		return rec
	}
	rec.attachRun(run)
	return rec
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
	if r.runID == "" && r.deferUntilLease && r.coord != nil && leaseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), runRecorderRequestTimeout)
		defer cancel()
		run, err := r.coord.CreateRun(ctx, leaseID, cfg, r.command)
		if err != nil {
			r.warn("run history create failed: %v", err)
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
	if r == nil || r.telemetryStart != nil {
		return
	}
	r.telemetryStart = collectLeaseTelemetryBestEffort(ctx, leaseTelemetryCollectorForTarget(target))
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
	sampleCtx, cancel := context.WithCancel(ctx)
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
	r.output = newRunOutputEventQueue(r.coord, run.ID, r.handleRunEventAppendError)
	fmt.Fprintf(r.stderr, "recording run %s\n", run.ID)
}

func (r *runRecorder) StreamWriter(stream string) *runEventStreamWriter {
	if r != nil && r.output == nil && r.coord != nil && r.runID != "" {
		r.output = newRunOutputEventQueue(r.coord, r.runID, r.handleRunEventAppendError)
	}
	return &runEventStreamWriter{recorder: r, stream: stream}
}

func (r *runRecorder) Finish(ctx context.Context, target SSHTarget, exitCode int, sync, command time.Duration, log string, truncated bool, results *TestResultSummary) {
	if r == nil || r.runID == "" || r.finished {
		return
	}
	r.waitForOutputEvents(runEventOutputPostWait)
	r.finished = true
	r.stopTelemetrySampler()
	telemetryEnd := collectLeaseTelemetryBestEffort(ctx, leaseTelemetryCollectorForTarget(target))
	r.recordTelemetrySample(telemetryEnd)
	ctx, cancel := context.WithTimeout(context.Background(), runRecorderFinishTimeout)
	defer cancel()
	if _, err := r.coord.FinishRun(ctx, r.runID, exitCode, sync, command, log, truncated, results, runTelemetrySummary(r.telemetryStart, telemetryEnd, r.telemetrySnapshot())); err != nil {
		r.warn("run history finish failed for %s: %v", r.runID, err)
	}
}

func (r *runRecorder) Failed(err error) {
	if r == nil || r.runID == "" || r.finished || err == nil {
		return
	}
	r.waitForOutputEvents(runEventOutputPostWait)
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
