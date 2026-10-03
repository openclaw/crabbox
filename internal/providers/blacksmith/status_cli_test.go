//go:build !windows

package blacksmith

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Exercise both public JSON paths with native command fixtures and no claim,
// as after a completed stop whose acknowledgement was lost.
func TestBlacksmithStatusJSONWithoutClaim(t *testing.T) {
	for _, command := range []string{"status", "inspect"} {
		for _, state := range []string{"ready", "completed"} {
			t.Run(command+"/"+state, func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				repo := t.TempDir()
				t.Chdir(repo)
				config := filepath.Join(repo, "crabbox.yaml")
				if err := os.WriteFile(config, []byte("provider: blacksmith-testbox\nblacksmith:\n  org: example-org\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				t.Setenv("BLACKSMITH_API_URL", "")
				t.Setenv("BLACKSMITH_ORG", "")
				bin := t.TempDir()
				calls := filepath.Join(bin, "calls")
				t.Setenv("CRABBOX_TEST_CALLS", calls)
				t.Setenv("CRABBOX_TEST_STATUS", filepath.Join(bin, "status"))
				if err := os.WriteFile(filepath.Join(bin, "status"), []byte(testBlacksmithStatus("tbx_status", state)), 0600); err != nil {
					t.Fatal(err)
				}
				for name, script := range map[string]string{
					"blacksmith": `#!/bin/sh
set -eu
[ "$*" = 'testbox status --id tbx_status --api-url https://backend.blacksmith.sh --org example-org' ] || exit 91
printf 'native\n' >> "$CRABBOX_TEST_CALLS"
cat "$CRABBOX_TEST_STATUS"
`,
					"gh": `#!/bin/sh
set -eu
[ "$*" = 'api --hostname github.com --method GET repos/example-org/my-app/actions/runs/123 --jq {id,html_url,status,conclusion}' ] || exit 92
printf 'github\n' >> "$CRABBOX_TEST_CALLS"
printf '%s\n' '{"id":123,"html_url":"https://github.com/example-org/my-app/actions/runs/123","status":"completed","conclusion":"cancelled"}'
`,
				} {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				var stdout, stderr bytes.Buffer
				err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{command, "--provider", "blacksmith-testbox", "--id", "tbx_status", "--json"})
				if err != nil {
					t.Fatalf("CLI failed: %v %s", err, stderr.String())
				}
				var view core.StatusView
				if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
					t.Fatalf("invalid JSON: %s %v", stdout.String(), err)
				}
				want, wantCalls := "pending", "native\n"
				if state == "completed" {
					want, wantCalls = "complete", "native\ngithub\nnative\n"
				}
				if view.State != state || view.Ready != (state == "ready") || view.ProviderMetadata["remoteSettlement"] != want || view.ProviderMetadata["runURL"] != "https://github.com/example-org/my-app/actions/runs/123" || view.CleanupStatus != "" {
					t.Fatalf("wrong public contract: %s", stdout.String())
				}
				recorded, err := os.ReadFile(calls)
				if err != nil || string(recorded) != wantCalls {
					t.Fatalf("unexpected reads/mutations: %q %v", recorded, err)
				}
				claim, err := core.ReadLeaseClaim("tbx_status")
				if err != nil || claim.LeaseID != "" {
					t.Fatalf("invented claim: %+v %v", claim, err)
				}
				key, err := core.TestboxKeyPath("tbx_status")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Dir(key)); !os.IsNotExist(err) {
					t.Fatalf("invented key directory: %v", err)
				}
				if strings.Contains(stderr.String(), "retaining claim") {
					t.Fatal("read-only observation claimed custody")
				}
			})
		}
	}
}
