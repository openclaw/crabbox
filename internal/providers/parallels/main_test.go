package parallels

import (
	"os"
	"testing"
)

// TestMain lets this binary stand in for prlctl and prlsrvctl when a PATH shim
// re-executes it, so CLI-entrypoint tests can run the real command runner.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__fake-parallels" {
		os.Exit(runFakeParallelsBinary(fakeParallelsStatePath(), os.Args[1:]))
	}
	os.Exit(m.Run())
}
