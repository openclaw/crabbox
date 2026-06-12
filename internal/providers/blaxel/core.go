package blaxel

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type BlaxelConfig = core.BlaxelConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest
type Server = core.Server
type Repo = core.Repo
type SyncManifest = core.SyncManifest
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName      = "blaxel"
	defaultAPIURL     = "https://api.blaxel.ai"
	defaultAPIVersion = "2026-04-28"
	defaultImage      = "ubuntu:24.04"
	defaultRegion     = ""
	defaultWorkdir    = "/workspace/crabbox"
	targetLinux       = core.TargetLinux
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func now(rt Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}
