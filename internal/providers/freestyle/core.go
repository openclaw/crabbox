package freestyle

import (
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

type statusView = core.StatusView

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shellWords(words []string) []string {
	return core.ShellWords(words)
}

func leadingEnvAssignment(command []string) bool {
	return core.LeadingEnvAssignment(command)
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

var claimLeaseForRepoProviderPond = core.ClaimLeaseForRepoProviderPond

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func delegatedSyncOptionsError(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}
