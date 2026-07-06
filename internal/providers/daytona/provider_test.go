package daytona

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSupportsCoordinator(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != "daytona" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorSupported {
		t.Fatalf("spec=%#v", spec)
	}
}
