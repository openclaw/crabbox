package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestHyperVPublicDiscoveryTargets(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Setenv("CRABBOX_HYPERV_IMAGE", "")
	t.Setenv("CRABBOX_HYPERV_GUEST_PASSWORD", "")
	var stdout bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.Run(context.Background(), []string{"providers", "describe", "hyperv", "--json"}); err != nil {
		t.Fatal(err)
	}
	var description struct {
		SchemaVersion int `json:"schemaVersion"`
		Provider      struct {
			Canonical string `json:"canonical"`
		} `json:"provider"`
		Targets       []string `json:"targets"`
		ProviderFlags []struct {
			Name string `json:"name"`
		} `json:"providerFlags"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &description); err != nil {
		t.Fatal(err)
	}
	if description.SchemaVersion != 2 || description.Provider.Canonical != providerName || len(description.Targets) != 1 {
		t.Fatal("wrong public provider description")
	}
	if description.Targets[0] != "windows/normal" {
		t.Fatalf("unsupported target string %q", description.Targets[0])
	}
	flags := map[string]bool{}
	for _, item := range description.ProviderFlags {
		flags[item.Name] = true
	}
	for _, name := range []string{"hyperv-image", "hyperv-user", "hyperv-work-root", "hyperv-cpu", "hyperv-memory", "hyperv-switch"} {
		if !flags[name] {
			t.Fatalf("public flag %q missing", name)
		}
	}
}
