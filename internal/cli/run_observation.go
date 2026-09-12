package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type RunPhase string

const (
	RunPhaseAcquire RunPhase = "acquire"
	RunPhaseResolve RunPhase = "resolve"
	RunPhaseSetup   RunPhase = "setup"
	RunPhaseSync    RunPhase = "sync"
	RunPhaseCommand RunPhase = "command"
	RunPhaseCleanup RunPhase = "cleanup"
	RunPhaseOpaque  RunPhase = "opaque-provider-run"
)

type RunOutputScope string

const (
	RunOutputWorkload RunOutputScope = "workload"
	RunOutputProvider RunOutputScope = "provider-run"
)

// RunObservation retains local evidence only; it grants no provider or receipt authority.
// A nil observation preserves the original terminal and capture writers.
type RunObservation struct {
	mu        sync.Mutex
	record    localHistoryRecord
	writer    *localHistoryWriter
	log       runLogBuffer
	directLog *runLogBuffer
	results   *TestResultSummary
	warnings  io.Writer
	warned    bool
	secrets   []string
}

func beginRunObservation(cfg Config, id string, command []string, warnings io.Writer) (*RunObservation, error) {
	if !cfg.RecordLocal {
		return nil, nil
	}
	if warnings == nil {
		warnings = io.Discard
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := localHistoryRecord{Version: 1, ID: id, RecordingState: "active", Source: "local", CoordinatorState: "none", Provider: cfg.Provider, Target: cfg.TargetOS, CommandDisplay: strings.Join(command, " "), Phase: "admitted", StartedAt: now, UpdatedAt: now, CaptureScope: "unavailable", Stdout: localHistoryStream{Availability: "unavailable"}, Stderr: localHistoryStream{Availability: "unavailable"}, ResultsState: "no-results"}
	secrets := configuredDiagnosticSecrets(cfg)
	record.CommandDisplay = RedactDiagnosticSecrets(record.CommandDisplay, secrets...)
	writer, err := beginLocalHistory(record)
	if err != nil {
		return nil, fmt.Errorf("local history preflight: %w", err)
	}
	return &RunObservation{record: record, writer: writer, warnings: warnings, secrets: secrets}, nil
}

func (o *RunObservation) warn(err error) {
	if err != nil && !o.warned {
		o.warned = true
		fmt.Fprintf(o.warnings, "warning: local history %s may be incomplete: %v\n", o.record.ID, err)
	}
}

func (o *RunObservation) Phase(phase RunPhase) {
	if o == nil {
		return
	}
	switch phase {
	case RunPhaseAcquire, RunPhaseResolve, RunPhaseSetup, RunPhaseSync, RunPhaseCommand, RunPhaseCleanup, RunPhaseOpaque:
	default:
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.record.Phase == string(phase) {
		return
	}
	o.record.Phase = string(phase)
	o.record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	o.warn(o.writer.Checkpoint(o.record))
}

func (o *RunObservation) BindLease(leaseID, slug string) {
	if o == nil || leaseID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.LeaseID, o.record.Slug = leaseID, slug
	if err := o.writer.Checkpoint(o.record); err != nil {
		o.warn(err)
	}
}

func (o *RunObservation) CommandWriters(stdout, stderr io.Writer, scope RunOutputScope) (io.Writer, io.Writer) {
	if o == nil {
		return stdout, stderr
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.CaptureScope = string(scope)
	wrap := func(w io.Writer, stream *localHistoryStream) io.Writer {
		if w == nil || stream.Availability == "omitted" {
			return w
		}
		stream.Availability = "retained"
		return io.MultiWriter(w, &o.log)
	}
	return wrap(stdout, &o.record.Stdout), wrap(stderr, &o.record.Stderr)
}

func (o *RunObservation) OmitStream(stream, reason string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	value := localHistoryStream{Availability: "omitted", Reason: reason}
	if stream == "stdout" {
		o.record.Stdout = value
	}
	if stream == "stderr" {
		o.record.Stderr = value
	}
}

func (o *RunObservation) Results(results *TestResultSummary, availability string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = results
	o.record.ResultsState = availability
}

func (o *RunObservation) adoptDirectLog(log *runLogBuffer, stdoutOmitted, stderrOmitted bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.directLog = log
	// The direct buffer also receives diagnostics from the SSH transport.
	o.record.CaptureScope = string(RunOutputProvider)
	o.record.Stdout = localHistoryStream{Availability: "retained"}
	o.record.Stderr = localHistoryStream{Availability: "retained"}
	if stdoutOmitted {
		o.record.Stdout = localHistoryStream{Availability: "omitted", Reason: "local-capture"}
	}
	if stderrOmitted {
		o.record.Stderr = localHistoryStream{Availability: "omitted", Reason: "local-capture"}
	}
}

func (o *RunObservation) finish(report timingReport, hasReport bool, err error, recorder *runRecorder) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() { o.warn(o.writer.Close()) }()
	o.record.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	o.record.UpdatedAt = o.record.EndedAt
	if hasReport {
		if report.Provider != "" {
			o.record.Provider = report.Provider
		}
		if report.LeaseID != "" {
			o.record.LeaseID = report.LeaseID
		}
		if report.Slug != "" {
			o.record.Slug = report.Slug
		}
		o.record.ExitCode = &report.ExitCode
		o.record.RunStatus, o.record.ErrorKind = report.RunStatus, report.ErrorKind
		o.record.SyncMs, o.record.CommandMs, o.record.TotalMs = report.SyncMs, report.CommandMs, report.TotalMs
	} else {
		result := FinalizeRunResult(RunResult{}, err)
		o.record.ExitCode = &result.ExitCode
		o.record.RunStatus, o.record.ErrorKind = result.Status, result.ErrorKind
	}
	if err != nil {
		o.record.Diagnostic = RedactDiagnosticSecrets(err.Error(), o.secrets...)
	}
	if recorder != nil && recorder.coord != nil {
		o.record.CoordinatorState = "unknown"
		reference := coordinatorHistoryReference(recorder.coord.BaseURL)
		if recorder.runID != "" && reference != "" {
			o.record.CoordinatorRunID = recorder.runID
			o.record.CoordinatorReference = reference
		}
		if recorder.runID != "" && recorder.terminalConfirmed && reference != "" {
			o.record.Source, o.record.CoordinatorState = "both", "recorded"
		}
	}
	log := o.log.Snapshot()
	if o.directLog != nil {
		log = o.directLog.Snapshot()
	}
	o.record.RecordingState = "terminal"
	o.warn(o.writer.Commit(o.record, log, o.results))
}
