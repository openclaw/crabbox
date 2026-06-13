package superserve

import (
	"context"
	"flag"
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest

const (
	providerName   = "superserve"
	leasePrefix    = "ssbx_"
	namePrefix     = "crabbox-"
	defaultBaseURL = "https://api.superserve.ai"
	defaultWorkdir = "/workspace/crabbox"
	targetLinux    = core.TargetLinux
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func writeTimingJSON(w io.Writer, report core.TimingReport) error {
	return core.WriteTimingJSON(w, report)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func notImplemented(ctx context.Context, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return exit(2, "provider=superserve %s is not implemented yet; lifecycle support lands in the Superserve client/lifecycle plan", action)
}
