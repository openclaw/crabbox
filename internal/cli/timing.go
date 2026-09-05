package cli

import (
	"encoding/json"
	"io"
	"math"
	"strings"
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
	FailureEvidence    *RunFailureEvidence      `json:"failureEvidence,omitempty"`
	RetryLikely        string                   `json:"retryLikely,omitempty"`
	Artifacts          []runArtifact            `json:"artifacts,omitempty"`

	SchemaValidations []SchemaValidationResult `json:"schemaValidations,omitempty"`
	ArtifactChanges   []ArtifactChangeResult   `json:"artifactChanges,omitempty"`

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
	TransferCount int    `json:"transferCount,omitempty"`
	TransferBytes int64  `json:"transferBytes,omitempty"`
}

type runnerProviderTiming struct {
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
	if report.FailureEvidence != nil {
		report.FailureEvidence = runFailureEvidenceSnapshot(*report.FailureEvidence)
	}
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
		FailureEvidence:    runFailureEvidenceSnapshot(timings.failureEvidence),
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
		if strings.TrimSpace(phase.Name) != "" && phase.Ms > 0 {
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

	appendIdentity("provider.borrow", timings.borrow)
	leaseMs := timings.lease.Milliseconds()
	if providerTiming := timings.providerTiming; providerTiming != nil && len(providerTiming.Phases) > 0 {
		remaining := leaseMs
		for _, providerPhase := range providerTiming.Phases {
			if providerPhase.Ms <= 0 || providerPhase.Ms > remaining {
				continue
			}
			providerPhase.Provider = report.Provider
			providerPhase.LeaseID = report.LeaseID
			providerPhase.Slug = report.Slug
			providerPhase.MachineType = report.MachineType
			appendPhase(providerPhase)
			remaining -= providerPhase.Ms
		}
		if remaining > 0 {
			phase := identity
			phase.Name = firstNonBlank(timings.leasePhase, "provider.acquire")
			phase.Ms = remaining
			phase.Reason = "client-side acquisition overhead"
			appendPhase(phase)
		}
	} else {
		appendIdentity(firstNonBlank(timings.leasePhase, "provider.acquire"), timings.lease)
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
	total := report.RunnerTotalMs
	if total <= 0 {
		total = report.EndToEndMs
	}
	if total <= 0 {
		total = report.TotalMs
	}
	if total <= 0 {
		report.RunnerTotalMs = 0
		report.RunnerPhases = nil
		return report
	}
	if len(report.RunnerPhases) == 0 {
		report.RunnerPhases = runnerPhasesFromLegacyReport(report, total)
	}

	filtered := make([]RunnerPhase, 0, len(report.RunnerPhases)+1)
	remaining := total
	for _, phase := range report.RunnerPhases {
		if strings.TrimSpace(phase.Name) == "" || phase.Ms <= 0 || phase.Ms > remaining {
			continue
		}
		phase = populateRunnerPhaseMetadata(report, phase)
		filtered = append(filtered, phase)
		remaining -= phase.Ms
	}
	if remaining > 0 {
		filtered = append(filtered, RunnerPhase{Name: "unattributed", Ms: remaining})
	}
	report.RunnerTotalMs = total
	report.RunnerPhases = filtered
	return report
}

func runnerPhasesFromLegacyReport(report TimingReport, total int64) []RunnerPhase {
	if !report.SyncDelegated || total <= 0 {
		return nil
	}
	remaining := total
	phases := make([]RunnerPhase, 0, 3)
	appendBounded := func(phase RunnerPhase) {
		if phase.Ms <= 0 || phase.Ms > remaining {
			return
		}
		phases = append(phases, phase)
		remaining -= phase.Ms
	}
	appendBounded(RunnerPhase{Name: "workspace.sync", Ms: report.SyncMs})
	appendBounded(RunnerPhase{Name: "command", Ms: report.CommandMs})
	if remaining > 0 {
		phases = append(phases, RunnerPhase{
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
	var total int64
	for _, artifact := range artifacts {
		if artifact.Bytes > 0 && int64(artifact.Bytes) <= math.MaxInt64-total {
			total += int64(artifact.Bytes)
		}
	}
	return total
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

func durationMillisecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	milliseconds := duration.Milliseconds()
	if duration%time.Millisecond != 0 {
		milliseconds++
	}
	return milliseconds
}

func resetRunnerTimingsForReplacement(timings *runTimings, oldReport TimingReport, oldLeaseCleanupDuration, replacementLeaseDuration time.Duration, replacementTiming *runnerProviderTiming) {
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
	timings.providerTiming = replacementTiming
	timings.connect = 0
	timings.syncConnect = 0
	timings.bootstrap = 0
	timings.sync = 0
	timings.syncSteps = syncStepTimings{}
	timings.syncSkipped = false
}

func recordFailedReplacementLeaseDuration(timings *runTimings, duration time.Duration) {
	if timings != nil && duration > 0 {
		timings.legacyLease += duration
	}
}

func legacyLeaseDuration(timings runTimings) time.Duration {
	return timings.legacyLease + timings.lease
}

func legacyBootstrapDuration(timings runTimings) time.Duration {
	return timings.legacyBootstrap + timings.bootstrap
}
