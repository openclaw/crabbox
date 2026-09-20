package parallels

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// DesktopLeaseWithoutLabel admits already-configured Screen Sharing only on an
// exactly owned clone. It never changes the lease's creation-time capabilities.
func (Provider) DesktopLeaseWithoutLabel(cfg core.Config, server core.Server, leaseID string) (bool, error) {
	if cfg.TargetOS != core.TargetMacOS && !strings.EqualFold(server.Labels["target"], core.TargetMacOS) {
		return false, nil
	}
	provider := strings.TrimSpace(server.Provider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.Provider)
	}
	if provider != "parallels" {
		return false, nil
	}
	leaseID = strings.TrimSpace(leaseID)
	nameLeaseID, _ := parallelsLeaseFromVMName(server.Name)
	if leaseID == "" || nameLeaseID == "" || nameLeaseID != leaseID {
		return false, nil
	}
	if label := strings.TrimSpace(server.Labels["lease"]); label != "" && label != leaseID {
		return false, nil
	}
	cloudID := strings.TrimSpace(server.CloudID)
	host := strings.TrimSpace(server.Labels["host"])
	if cloudID == "" || host == "" {
		return false, nil
	}
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, "parallels")
	if err != nil {
		return false, err
	}
	if !ok || !exact || claim.LeaseID != leaseID || strings.TrimSpace(claim.CloudID) != cloudID || strings.TrimSpace(claim.Labels["host"]) != host {
		return false, nil
	}
	return true, nil
}
