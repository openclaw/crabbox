package localcontainer

import (
	"os"
	"testing"

	"github.com/openclaw/crabbox/internal/testutil"
)

func TestMain(m *testing.M) {
	if address := os.Getenv(endpointProbeTestAddress); address != "" {
		os.Exit(runEndpointProbeTestProcess(address))
	}
	os.Exit(testutil.RunWithIsolatedUserDirs(m))
}
