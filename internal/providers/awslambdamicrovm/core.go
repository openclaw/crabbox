package awslambdamicrovm

import (
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "aws-lambda-microvm"
	targetLinux  = core.TargetLinux
	runnerPort   = 8080
)

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func claimLease(leaseID, slug, scope, pond, repoRoot string, idle time.Duration, reclaim bool, server core.Server) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, scope, pond, repoRoot, idle, reclaim, server, core.SSHTarget{})
}
func directLeaseLabels(cfg core.Config, leaseID, slug string, keep bool, at time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "serverless", keep, at)
}

func handleDelegatedRunFailure(w io.Writer, req core.RunRequest, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, providerName, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}
func printEnvForwardingSummary(w io.Writer, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, providerName, "forwarded", allow, env)
}
