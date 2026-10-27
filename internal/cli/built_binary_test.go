package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/openclaw/crabbox/internal/testutil"
)

var cliTestBuild struct {
	once   sync.Once
	dir    string
	binary string
	err    error
}

// Own the immutable CLI across roots and -count iterations, without building
// anything unless a binary contract requests it during m.Run.
func builtCLITestBinary() (string, error) {
	cliTestBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "crabbox-cli-test-build-")
		if err != nil {
			cliTestBuild.err = fmt.Errorf("create CLI test build directory: %w", err)
			return
		}
		cliTestBuild.dir = dir
		binary := filepath.Join(dir, "crabbox")
		build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crabbox")
		build.Dir = filepath.Join("..", "..")
		if output, err := build.CombinedOutput(); err != nil {
			cliTestBuild.err = fmt.Errorf("go build ./cmd/crabbox: %w\n%s", err, output)
			return
		}
		cliTestBuild.binary = binary
	})
	return cliTestBuild.binary, cliTestBuild.err
}

func runCLITests(m *testing.M) (code int) {
	if os.Getenv("CRABBOX_TEST_SERVE_RELAY_PROXY") == "1" && len(os.Args) > 1 && os.Args[1] == "__serve-relay-proxy" {
		if err := Run(context.Background(), os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	defer func() {
		if cliTestBuild.dir != "" {
			// Capture roots rebuild the adjacent .provider fixture after their
			// owned children finish; retain t.TempDir's cleanup failure reporting.
			if err := os.RemoveAll(cliTestBuild.dir); err != nil {
				fmt.Fprintf(os.Stderr, "remove CLI test build directory: %v\n", err)
				if code == 0 {
					code = 1
				}
			}
		}
	}()
	return testutil.RunWithIsolatedUserDirs(m)
}
