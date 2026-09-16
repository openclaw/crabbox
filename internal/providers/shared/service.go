package shared

import core "github.com/openclaw/crabbox/internal/cli"

// RejectServiceRunOptions rejects run options that service-control backends
// cannot honor. Command and service identity validation remain with the caller.
func RejectServiceRunOptions(req core.RunRequest, provider, lifecycleReason, shellReason string) error {
	if req.Keep {
		return core.Exit(2, "provider=%s %s; --keep is not supported", provider, lifecycleReason)
	}
	if req.Reclaim {
		return core.Exit(2, "provider=%s %s; --reclaim is not supported", provider, lifecycleReason)
	}
	if !req.NoSync {
		// Services expose no workspace-sync surface. Require --no-sync explicitly:
		// a deployment runs what the service is already configured to run.
		return core.Exit(2, "provider=%s does not support workspace sync; pass --no-sync", provider)
	}
	if req.SyncOnly {
		return core.Exit(2, "provider=%s does not support sync; --sync-only is rejected", provider)
	}
	if req.ChecksumSync {
		return core.Exit(2, "provider=%s does not support sync; --checksum is rejected", provider)
	}
	if req.ForceSyncLarge {
		return core.Exit(2, "provider=%s does not support sync; --force-sync-large is rejected", provider)
	}
	if req.FullResync {
		return core.Exit(2, "provider=%s does not support sync; --full-resync is rejected", provider)
	}
	if req.ShellMode {
		return core.Exit(2, "provider=%s %s; --shell is not supported", provider, shellReason)
	}
	if req.EnvSummary {
		return core.Exit(2, "provider=%s cannot forward per-run environment variables", provider)
	}
	return nil
}
