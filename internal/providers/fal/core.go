package fal

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type FalConfig = core.FalConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult

const (
	providerName = "fal"
	targetLinux  = core.TargetLinux
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

func isFalProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "fal-ai":
		return true
	default:
		return false
	}
}
