package external

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, request core.CommandRoutingRequest) core.CommandRouting {
	return core.CommandRouting{Args: externalCommandRoutingArgs(cfg, request.LeaseID, request.Purpose)}
}

// externalCommandRoutingArgs keeps generated follow-up commands bound to
// the resolved External lease. Private routing state is the complete contract;
// the flag fallback is deliberately limited to fields that cannot contain
// provider arguments or config secrets.
func externalCommandRoutingArgs(cfg core.Config, leaseID string, purpose core.CommandRoutingPurpose) []string {
	routingPath := strings.TrimSpace(cfg.External.RoutingFile)
	if routingPath != "" {
		return externalPersistedRoutingArgs(routingPath, cfg)
	}
	canonicalPath, pathErr := core.ExternalRoutingPath(leaseID)
	if purpose != core.CommandRoutingRescue && pathErr == nil {
		return externalPersistedRoutingArgs(canonicalPath, cfg)
	}
	if pathErr == nil {
		_, statErr := os.Stat(core.ExpandUserPath(canonicalPath))
		if statErr == nil || !os.IsNotExist(statErr) {
			return externalPersistedRoutingArgs(canonicalPath, cfg)
		}
	}

	if !externalRoutingHasSafeFlagFallback(cfg.External) {
		// Keep complex External state fail-closed. Re-emitting adapter arguments,
		// config, lifecycle templates, or connection data could put secrets on
		// argv or silently address a different resource.
		if pathErr == nil {
			return externalPersistedRoutingArgs(canonicalPath, cfg)
		}
		return core.AppendExternalDesktopRoutingArgs(nil, cfg)
	}

	args := []string{"--external-command", strings.TrimSpace(cfg.External.Command)}
	if workRoot := strings.TrimSpace(cfg.External.WorkRoot); workRoot != "" {
		args = append(args, "--external-work-root", workRoot)
	}
	args = append(args, fmt.Sprintf("--external-idempotent-lease-id=%t", cfg.External.Capabilities.IdempotentLeaseID))
	return core.AppendExternalDesktopRoutingArgs(args, cfg)
}

func externalPersistedRoutingArgs(path string, overrides core.Config) []string {
	routing := overrides.External
	if core.ExternalRoutingDigest(routing) == "" {
		loaded, err := core.LoadExternalRouting(path)
		if err != nil {
			return []string{"--external-routing-file", path, "--external-routing-digest", ""}
		}
		routing = loaded
	}
	routed := core.Config{External: routing}
	if core.IsExternalDesktopUsernameExplicit(&overrides) {
		routed.External.Connection.Desktop.Username = overrides.External.Connection.Desktop.Username
		core.MarkExternalDesktopUsernameExplicit(&routed)
	}
	if core.IsExternalDesktopPasswordEnvExplicit(&overrides) {
		routed.External.Connection.Desktop.PasswordEnv = overrides.External.Connection.Desktop.PasswordEnv
		core.MarkExternalDesktopPasswordEnvExplicit(&routed)
	}
	return core.AppendExternalDesktopRoutingArgs(externalRoutingFileArgs(path, routing), routed)
}

func externalRoutingFileArgs(path string, cfg core.ExternalConfig) []string {
	digest := core.ExternalRoutingDigest(cfg)
	if digest == "" {
		if routing, err := core.LoadExternalRouting(path); err == nil {
			digest = core.ExternalRoutingDigest(routing)
		}
	}
	// Always emit the binding flag. An unreadable/missing route therefore
	// produces an invalid empty digest and the generated child fails closed;
	// it can never accept a later path replacement as an unbound route.
	return []string{"--external-routing-file", path, "--external-routing-digest", digest}
}

func externalRoutingHasSafeFlagFallback(cfg core.ExternalConfig) bool {
	connection := cfg.Connection
	connection.Desktop = core.ExternalDesktopConfig{}
	return strings.TrimSpace(cfg.Command) != "" &&
		len(cfg.Args) == 0 &&
		len(cfg.Config) == 0 &&
		reflect.DeepEqual(cfg.Lifecycle, core.ExternalLifecycleConfig{}) &&
		reflect.DeepEqual(connection, core.ExternalConnectionConfig{})
}
