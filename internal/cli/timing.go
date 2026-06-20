package cli

import (
	"encoding/json"
	"io"
	"time"
)

type TimingReport struct {
	Provider      string        `json:"provider"`
	LeaseID       string        `json:"leaseId,omitempty"`
	Slug          string        `json:"slug,omitempty"`
	SyncMs        int64         `json:"syncMs"`
	SyncPhases    []TimingPhase `json:"syncPhases,omitempty"`
	SyncSkipped   bool          `json:"syncSkipped"`
	SyncDelegated bool          `json:"syncDelegated,omitempty"`
	HydrateMs     int64         `json:"hydrateMs,omitempty"`
	ProbeMs       int64         `json:"probeMs,omitempty"`
	CommandMs     int64         `json:"commandMs"`
	CommandPhases []TimingPhase `json:"commandPhases,omitempty"`
	TotalMs       int64         `json:"totalMs"`
	ExitCode      int           `json:"exitCode"`
	ActionsRunURL string        `json:"actionsRunUrl,omitempty"`
	RunID         string        `json:"runId,omitempty"`
	Label         string        `json:"label,omitempty"`
	MachineType   string        `json:"machineType,omitempty"`
	RepoPath      string        `json:"repoPath,omitempty"`
	Workdir       string        `json:"workdir,omitempty"`
	StopCommand   string        `json:"stopCommand,omitempty"`
	IdleTimeout   string        `json:"idleTimeout,omitempty"`
	BlockedStage  string        `json:"blockedStage,omitempty"`
	RetryLikely   string        `json:"retryLikely,omitempty"`
	Artifacts     []runArtifact `json:"artifacts,omitempty"`
	LeaseStopped  *bool         `json:"leaseStopped,omitempty"`
	LeaseStopErr  string        `json:"leaseStopError,omitempty"`
}

type TimingPhase struct {
	Name    string `json:"name"`
	Ms      int64  `json:"ms,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type timingReport = TimingReport
type timingPhase = TimingPhase

type timingReportWriter interface {
	WriteTimingReport(TimingReport) error
}

func writeTimingJSON(w io.Writer, report TimingReport) error {
	if writer, ok := w.(timingReportWriter); ok {
		return writer.WriteTimingReport(report)
	}
	return encodeTimingJSON(w, report)
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
	return timingReport{
		Provider:      provider,
		LeaseID:       leaseID,
		Slug:          slug,
		SyncMs:        timings.sync.Milliseconds(),
		SyncPhases:    syncTimingPhases(timings.syncSteps),
		SyncSkipped:   timings.syncSkipped,
		CommandMs:     timings.command.Milliseconds(),
		CommandPhases: timings.commandPhases,
		TotalMs:       total.Milliseconds(),
		ExitCode:      exitCode,
		BlockedStage:  timings.blockedStage,
		RetryLikely:   timings.retryLikely,
	}
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
