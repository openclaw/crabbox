package all

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestExecCheckAdmitsOnlySupportedClaimLifecycles(t *testing.T) {
	for _, scenario := range []struct {
		provider      string
		target        string
		brokered      bool
		supported     bool
		checkRejected bool
	}{
		{provider: "daytona", supported: true},
		{provider: "daytona", brokered: true},
		{provider: "daytona", target: "macos", checkRejected: true},
		{provider: "aws"},
		{provider: "machine0"},
		{provider: "local-container"},
	} {
		target := scenario.target
		if target == "" {
			target = "linux"
		}
		t.Run(fmt.Sprintf("%s/%s/brokered=%t", scenario.provider, target, scenario.brokered), func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Chdir(t.TempDir())
			for _, name := range []string{"CRABBOX_PROVIDER", "CRABBOX_COORDINATOR", "CRABBOX_BROKER_MODE", "CRABBOX_PROFILE", "DAYTONA_API_KEY", "CRABBOX_DAYTONA_API_KEY"} {
				t.Setenv(name, "")
			}
			config := "target: linux\n"
			if scenario.brokered {
				config += "coordinator: https://broker.example.invalid\n"
			}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			var out, diagnostic bytes.Buffer
			app := core.App{Stdout: &out, Stderr: &diagnostic, Stdin: strings.NewReader("")}
			err := app.Run(t.Context(), []string{"exec", "--check", "--provider", scenario.provider, "--target", target})
			if (err != nil) != scenario.checkRejected {
				t.Fatalf("offline check error=%v", err)
			}
			var result struct {
				Provider, Target string
				Execution        bool
				CurrentRepoStop  bool `json:"currentRepoStop"`
			}
			if !scenario.checkRejected {
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Provider != scenario.provider || result.Target != target || result.Execution != scenario.supported || result.CurrentRepoStop != scenario.supported {
					t.Fatalf("capabilities=%+v", result)
				}
			}
			if !scenario.supported {
				err := app.Run(t.Context(), []string{"exec", "--provider", scenario.provider, "--target", target, "--id", "cbx_123456789abc", "--", "true"})
				if err == nil || !strings.Contains(err.Error(), "does not support claim-fenced SSH execution") {
					t.Fatalf("unsupported execution reached setup: %v", err)
				}
			}
		})
	}
}
