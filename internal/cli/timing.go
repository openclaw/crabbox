package cli

import (
	"encoding/json"
	"io"
	"time"
)

type TimingReport struct {
	Provider           string                   `json:"provider"`
	LeaseID            string                   `json:"leaseId,omitempty"`
	Slug               string                   `json:"slug,omitempty"`
	RunnerTotalMs      int64                    `json:"runnerTotalMs,omitempty"`
	RunnerPhases       []RunnerPhase            `json:"runnerPhases,omitempty"`
	LeaseMs            int64                    `json:"leaseMs,omitempty"`
	BootstrapMs        int64                    `json:"bootstrapMs,omitempty"`
	SyncMs             int64                    `json:"syncMs"`
	SyncPhases         []TimingPhase            `json:"syncPhases,omitempty"`
	SyncSkipped        bool                     `json:"syncSkipped"`
	SyncDelegated      bool                     `json:"syncDelegated,omitempty"`
	SyncMode           string                   `json:"syncMode,omitempty"`
	SyncTransferFiles  int                      `json:"syncTransferFiles,omitempty"`
	SyncTransferBytes  int64                    `json:"syncTransferBytes,omitempty"`
	SyncFallbackReason string                   `json:"syncFallbackReason,omitempty"`
	HydrateMs          int64                    `json:"hydrateMs,omitempty"`
	ProbeMs            int64                    `json:"probeMs,omitempty"`
	CommandMs          int64                    `json:"commandMs"`
	CommandPhases      []TimingPhase            `json:"commandPhases,omitempty"`
	TotalMs            int64                    `json:"totalMs"`
	EndToEndMs         int64                    `json:"endToEndMs"`
	ExitCode           int                      `json:"exitCode"`
	RunStatus          RunStatus                `json:"runStatus,omitempty"`
	ErrorKind          RunErrorKind             `json:"errorKind,omitempty"`
	ActionsRunURL      string                   `json:"actionsRunUrl,omitempty"`
	RunID              string                   `json:"runId,omitempty"`
	Label              string                   `json:"label,omitempty"`
	MachineType        string                   `json:"machineType,omitempty"`
	RepoPath           string                   `json:"repoPath,omitempty"`
	Workdir            string                   `json:"workdir,omitempty"`
	StopCommand        string                   `json:"stopCommand,omitempty"`
	IdleTimeout        string                   `json:"idleTimeout,omitempty"`
	BlockedStage       string                   `json:"blockedStage,omitempty"`
	ResourceExhaustion ResourceExhaustionReason `json:"resourceExhaustion,omitempty"`
	RetryLikely        string                   `json:"retryLikely,omitempty"`
	Artifacts          []runArtifact            `json:"artifacts,omitempty"`

	SchemaValidations []SchemaValidationResult `json:"schemaValidations,omitempty"`

	LeaseStopped *bool  `json:"leaseStopped,omitempty"`
	LeaseStopErr string `json:"leaseStopError,omitempty"`
}

type TimingPhase struct {
	Name    string `json:"name"`
	Ms      int64  `json:"ms,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type RunnerPhase struct {
	Name          string `json:"name"`
	Ms            int64  `json:"ms"`
	Opaque        bool   `json:"opaque,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Provider      string `json:"provider,omitempty"`
	LeaseID       string `json:"leaseId,omitempty"`
	Slug          string `json:"slug,omitempty"`
	RunID         string `json:"runId,omitempty"`
	MachineType   string `json:"machineType,omitempty"`
	ImageID       string `json:"imageId,omitempty"`
	TransferCount int    `json:"transferCount,omitempty"`
	TransferBytes int64  `json:"transferBytes,omitempty"`
}

type runnerProviderEvidence struct {
	ImageID string
	TotalMs int64
	Phases  []RunnerPhase
}

type timingReport = TimingReport
type timingPhase = TimingPhase

type timingReportWriter interface {
	WriteTimingReport(TimingReport) error
}

func writeTimingJSON(w io.Writer, report TimingReport) error {
	report = finalizeTimingReport(report)
	if writer, ok := w.(timingReportWriter); ok {
		return writer.WriteTimingReport(report)
	}
	return encodeTimingJSON(w, report)
}

func finalizeTimingReport(report TimingReport) TimingReport {
	if report.RunStatus == "" {
		report.RunStatus = RunStatusForResult(RunResult{ExitCode: report.ExitCode}, nil)
	}
	if report.ErrorKind == "" {
		report.ErrorKind = RunErrorKindForResult(RunResult{ExitCode: report.ExitCode}, nil)
	}
	report = finalizeRunnerPhases(report)
	return report
}

// TimingReportWithRunResult copies normalized run outcome fields onto a timing report.
func TimingReportWithRunResult(report TimingReport, result RunResult, err error) TimingReport {
	result = FinalizeRunResult(result, err)
	if report.ExitCode == 0 && result.ExitCode != 0 {
		report.ExitCode = result.ExitCode
	}
	if report.RunStatus == "" {
		report.RunStatus = result.Status
	}
	if report.ErrorKind == "" {
		report.ErrorKind = result.ErrorKind
	}
	return report
}

func encodeTimingJSON(w io.Writer, report TimingReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(report)
}

func WriteTimingJSON(w io.Writer, report TimingReport) error {
	return writeTimingJSON(w, report)
}

func DurationMinutesCeil(duration time.Duration) int {
	if duration <= 0 {
		return 1
	}
	minutes := int(duration / time.Minute)
	if duration%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		return 1
	}
	return minutes
}

func timingReportFromRun(provider, leaseID, slug string, timings runTimings, total time.Duration, exitCode int) timingReport {
	report := timingReport{
		Provider:           provider,
		LeaseID:            leaseID,
		Slug:               slug,
		RunnerTotalMs:      runEndToEndDuration(timings, total).Milliseconds() + timings.borrow.Milliseconds(),
		LeaseMs:            legacyLeaseDuration(timings).Milliseconds(),
		BootstrapMs:        legacyBootstrapDuration(timings).Milliseconds(),
		SyncMs:             timings.sync.Milliseconds(),
		SyncPhases:         syncTimingPhases(timings.syncSteps),
		SyncSkipped:        timings.syncSkipped,
		SyncMode:           timings.syncMode,
		SyncTransferFiles:  timings.syncTransferFiles,
		SyncTransferBytes:  timings.syncTransferBytes,
		SyncFallbackReason: timings.syncFallbackReason,
		CommandMs:          timings.command.Milliseconds(),
		CommandPhases:      timings.commandPhases,
		TotalMs:            total.Milliseconds(),
		EndToEndMs:         runEndToEndDuration(timings, total).Milliseconds(),
		ExitCode:           exitCode,
		BlockedStage:       timings.blockedStage,
		ResourceExhaustion: timings.resourceExhaustion,
		RetryLikely:        timings.retryLikely,
	}
	report.RunnerPhases = runnerPhasesFromRun(report, timings)
	return report
}

func timingReportFromRunWithActionsURL(provider, leaseID, slug string, timings runTimings, total time.Duration, exitCode int, actionsRunURL string) timingReport {
	report := timingReportFromRun(provider, leaseID, slug, timings, total, exitCode)
	report.ActionsRunURL = actionsRunURL
	return report
}

func syncTimingPhases(steps syncStepTimings) []timingPhase {
	phases := make([]timingPhase, 0, 15)
	appendDuration := func(name string, duration time.Duration) {
		if duration > 0 {
			phases = append(phases, timingPhase{Name: name, Ms: duration.Milliseconds()})
		}
	}
	appendDuration("ssh", steps.sshReady)
	appendDuration("mkdir", steps.mkdir)
	appendDuration("manifest", steps.manifest)
	appendDuration("preflight", steps.preflight)
	appendDuration("reset", steps.reset)
	appendDuration("fingerprint", steps.fingerprintLocal)
	appendDuration("fingerprint_remote", steps.fingerprintRemote)
	appendDuration("git_seed", steps.gitSeed)
	appendDuration("manifest_write", steps.manifestWrite)
	appendDuration("prune", steps.prune)
	appendDuration("rsync", steps.rsync)
	appendDuration("manifest_apply", steps.manifestApply)
	appendDuration("sanity", steps.sanity)
	appendDuration("git_hydrate", steps.gitHydrate)
	if steps.gitHydrateSkipped {
		phases = append(phases, timingPhase{Name: "git_hydrate", Skipped: true, Reason: steps.gitHydrateSkipReason})
	}
	appendDuration("finalize", steps.finalize)
	appendDuration("fingerprint_write", steps.fingerprintWrite)
	return phases
}

func runnerPhasesFromRun(report TimingReport, timings runTimings) []RunnerPhase {
	phases := append([]RunnerPhase(nil), timings.priorRunnerPhases...)
	appendPhase := func(phase RunnerPhase) {
		if phase.Ms > 0 {
			phases = append(phases, phase)
		}
	}
	identity := RunnerPhase{
		Provider:    report.Provider,
		LeaseID:     report.LeaseID,
		Slug:        report.Slug,
		MachineType: report.MachineType,
	}
	appendIdentity := func(name string, duration time.Duration) {
		phase := identity
		phase.Name = name
		phase.Ms = duration.Milliseconds()
		appendPhase(phase)
	}

	if timings.borrow > 0 {
		appendIdentity("provider.borrow", timings.borrow)
	}
	leaseMs := timings.lease.Milliseconds()
	if evidence := timings.providerEvidence; evidence != nil && len(evidence.Phases) > 0 {
		remainingLeaseMs := leaseMs
		for _, providerPhase := range evidence.Phases {
			if providerPhase.Ms > remainingLeaseMs {
				providerPhase.Ms = remainingLeaseMs
			}
			providerPhase.Provider = report.Provider
			providerPhase.LeaseID = report.LeaseID
			providerPhase.Slug = report.Slug
			providerPhase.MachineType = report.MachineType
			providerPhase.ImageID = evidence.ImageID
			appendPhase(providerPhase)
			remainingLeaseMs -= providerPhase.Ms
			if remainingLeaseMs <= 0 {
				break
			}
		}
		if remainingLeaseMs > 0 {
			phase := identity
			phase.Name = "provider.acquire"
			phase.Ms = remainingLeaseMs
			phase.ImageID = evidence.ImageID
			phase.Reason = "client-side acquisition overhead"
			appendPhase(phase)
		}
	} else {
		phase := identity
		phase.Name = firstNonBlank(timings.leasePhase, "provider.acquire")
		phase.Ms = leaseMs
		appendPhase(phase)
	}

	appendIdentity("connect.ssh", timings.connect)
	workspaceMs := timings.sync.Milliseconds() - timings.syncConnect.Milliseconds()
	if workspaceMs < 0 {
		workspaceMs = 0
	}
	seedMs := minPositiveMilliseconds(timings.syncSteps.gitSeed.Milliseconds(), workspaceMs)
	if seedMs > 0 {
		phase := identity
		phase.Name = "workspace.seed"
		phase.Ms = seedMs
		appendPhase(phase)
		workspaceMs -= seedMs
	}
	if workspaceMs > 0 {
		phase := identity
		if timings.syncMode == "git-overlay" {
			phase.Name = "workspace.overlay"
		} else {
			phase.Name = "workspace.sync"
		}
		phase.Ms = workspaceMs
		appendPhase(phase)
	}
	if timings.command > 0 {
		phase := identity
		phase.Name = "command"
		phase.Ms = timings.command.Milliseconds()
		phase.RunID = report.RunID
		appendPhase(phase)
	}
	if timings.artifacts > 0 {
		phase := identity
		phase.Name = "artifacts"
		phase.Ms = timings.artifacts.Milliseconds()
		phase.RunID = report.RunID
		phase.TransferCount = timings.artifactTransferCount
		phase.TransferBytes = timings.artifactTransferBytes
		appendPhase(phase)
	}
	return phases
}

func finalizeRunnerPhases(report TimingReport) TimingReport {
	if len(report.RunnerPhases) == 0 {
		report.RunnerPhases = runnerPhasesFromLegacyReport(report)
	}
	total := report.RunnerTotalMs
	if total <= 0 {
		total = report.EndToEndMs
	}
	if total <= 0 {
		total = report.TotalMs
	}

	filtered := make([]RunnerPhase, 0, len(report.RunnerPhases)+1)
	var accounted int64
	for _, phase := range report.RunnerPhases {
		if phase.Ms <= 0 || phase.Name == "unattributed" {
			continue
		}
		phase = populateRunnerPhaseMetadata(report, phase)
		filtered = append(filtered, phase)
		accounted += phase.Ms
	}
	if total < accounted {
		total = accounted
	}
	if remainder := total - accounted; remainder > 0 {
		if report.SyncDelegated {
			filtered = appendDelegatedOpaqueRemainder(report, filtered, remainder)
		} else {
			filtered = append(filtered, RunnerPhase{Name: "unattributed", Ms: remainder})
		}
	}
	report.RunnerTotalMs = total
	report.RunnerPhases = filtered
	return report
}

func appendDelegatedOpaqueRemainder(report TimingReport, phases []RunnerPhase, remainder int64) []RunnerPhase {
	const reason = "provider owns lifecycle work outside measured sync and command"
	for index := range phases {
		if phases[index].Name != "delegated.opaque" {
			continue
		}
		phases[index].Ms += remainder
		phases[index].Opaque = true
		phases[index].Reason = reason
		return phases
	}
	return append(phases, populateRunnerPhaseMetadata(report, RunnerPhase{
		Name:   "delegated.opaque",
		Ms:     remainder,
		Opaque: true,
		Reason: reason,
	}))
}

func runnerPhasesFromLegacyReport(report TimingReport) []RunnerPhase {
	total := report.EndToEndMs
	if total <= 0 {
		total = report.TotalMs
	}
	if !report.SyncDelegated {
		return nil
	}
	remaining := total
	phases := make([]RunnerPhase, 0, 3)
	appendBounded := func(phase RunnerPhase) {
		if phase.Ms <= 0 || remaining <= 0 {
			return
		}
		if phase.Ms > remaining {
			phase.Ms = remaining
		}
		phases = append(phases, phase)
		remaining -= phase.Ms
	}

	if report.SyncMs > 0 {
		appendBounded(RunnerPhase{Name: "workspace.sync", Ms: report.SyncMs})
	}
	if report.CommandMs > 0 {
		appendBounded(RunnerPhase{Name: "command", Ms: report.CommandMs})
	}
	if remaining > 0 {
		appendBounded(RunnerPhase{
			Name:   "delegated.opaque",
			Ms:     remaining,
			Opaque: true,
			Reason: "provider owns lifecycle work outside measured sync and command",
		})
	}
	return phases
}

func populateRunnerPhaseMetadata(report TimingReport, phase RunnerPhase) RunnerPhase {
	if phase.Name == "unattributed" {
		return phase
	}
	if phase.Provider == "" {
		phase.Provider = report.Provider
	}
	if phase.LeaseID == "" {
		phase.LeaseID = report.LeaseID
	}
	if phase.Slug == "" {
		phase.Slug = report.Slug
	}
	if phase.MachineType == "" {
		phase.MachineType = report.MachineType
	}
	if (phase.Name == "command" || phase.Name == "artifacts") && phase.RunID == "" {
		phase.RunID = report.RunID
	}
	return phase
}

func runArtifactBytes(artifacts []runArtifact) int64 {
	var bytes int64
	for _, artifact := range artifacts {
		if artifact.Bytes > 0 {
			bytes += int64(artifact.Bytes)
		}
	}
	return bytes
}

func minPositiveMilliseconds(value, limit int64) int64 {
	if value <= 0 || limit <= 0 {
		return 0
	}
	if value > limit {
		return limit
	}
	return value
}

func resetRunnerTimingsForReplacement(timings *runTimings, oldReport TimingReport, oldLeaseCleanupDuration, replacementLeaseDuration time.Duration, replacementEvidence *runnerProviderEvidence) {
	if timings == nil {
		return
	}
	oldAttempt := *timings
	oldAttempt.priorRunnerPhases = nil
	oldAttempt.command = 0
	oldAttempt.commandPhases = nil
	oldAttempt.artifacts = 0
	oldAttempt.artifactTransferCount = 0
	oldAttempt.artifactTransferBytes = 0

	prior := append([]RunnerPhase(nil), timings.priorRunnerPhases...)
	prior = append(prior, runnerPhasesFromRun(oldReport, oldAttempt)...)
	if cleanupMs := oldLeaseCleanupDuration.Milliseconds(); cleanupMs > 0 {
		prior = append(prior, populateRunnerPhaseMetadata(oldReport, RunnerPhase{
			Name: "cleanup",
			Ms:   cleanupMs,
		}))
	}
	timings.priorRunnerPhases = prior
	timings.legacyLease += timings.lease
	timings.legacyBootstrap += timings.bootstrap
	timings.borrow = 0
	timings.lease = replacementLeaseDuration
	timings.leasePhase = "provider.acquire"
	timings.providerEvidence = replacementEvidence
	timings.connect = 0
	timings.syncConnect = 0
	timings.bootstrap = 0
	// Replacement retries have historically reset syncMs and syncPhases to the
	// final attempt; only the new runner phases retain both attempts.
	timings.sync = 0
	timings.syncSteps = syncStepTimings{}
	timings.syncSkipped = false
}

func recordFailedReplacementLeaseDuration(timings *runTimings, duration time.Duration) {
	if timings == nil || duration <= 0 {
		return
	}
	timings.legacyLease += duration
}

func legacyLeaseDuration(timings runTimings) time.Duration {
	return timings.legacyLease + timings.lease
}

func legacyBootstrapDuration(timings runTimings) time.Duration {
	return timings.legacyBootstrap + timings.bootstrap
}

func includeObservedRunnerTail(report *TimingReport, startedAt, endedAt time.Time) {
	if report == nil || startedAt.IsZero() || endedAt.Before(startedAt) {
		return
	}
	report.RunnerTotalMs = max(report.RunnerTotalMs, endedAt.Sub(startedAt).Milliseconds())
}
