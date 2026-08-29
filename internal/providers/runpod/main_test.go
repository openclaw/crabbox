package runpod

import (
	"os"
	"testing"

	"github.com/openclaw/crabbox/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithIsolatedUserDirs(m))
}
