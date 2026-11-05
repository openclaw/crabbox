package cli

import (
	"os"
	"testing"
)

func isolatedArtifactTestUmask(t *testing.T) (os.FileMode, bool) {
	t.Helper()
	return 0, true
}
