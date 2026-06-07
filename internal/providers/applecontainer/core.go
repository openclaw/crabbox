package applecontainer

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

// Type aliases keep the provider body terse while leaning on the shared core
// package, mirroring the asciibox/localcontainer providers.
type Config = core.Config
type AppleContainerConfig = core.AppleContainerConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type CommandRunner = core.CommandRunner
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type Backend = core.Backend
type Server = core.Server
type SSHTarget = core.SSHTarget

const (
	providerName = "apple-container"
	targetLinux  = core.TargetLinux
	// sshPort is the in-guest SSH port. Apple's container runtime gives each
	// container a routable IP on the host vmnet bridge, so unlike Docker we
	// connect straight to the container IP on the standard SSH port rather
	// than to a published host port.
	sshPort            = "22"
	workRootMarkerName = ".crabbox-apple-container-work-root"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}
