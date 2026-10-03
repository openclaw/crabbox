package azure

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestFixedAzureHeartbeatAndStatusWait(t *testing.T) {
	for _, command := range []string{"heartbeat", "status"} {
		for _, change := range []string{"owned", "scope", "resource", "generation", "account"} {
			t.Run(command+"/"+change, func(t *testing.T) {
				client := &fakeAzureClient{}
				b := fixedAzureTestBackend(t, client)
				repo := t.TempDir()
				t.Chdir(repo)
				lease, err := b.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed-heartbeat", Repo: core.Repo{Root: repo}, Keep: true})
				if err != nil {
					t.Fatal(err)
				}
				claim, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil || claim.CloudID == "" || claim.CloudID != lease.Server.CloudID || claim.CloudImmutableID != lease.Server.ImmutableID || claim.ProviderScope != azureTestClaimScope {
					t.Fatalf("fixed acquisition lost ownership: %+v, %v", claim, err)
				}
				if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
					switch change {
					case "scope":
						c.ProviderScope = "subscription:foreign|resource-group:rg"
					case "resource":
						c.CloudID = "foreign-vm"
					case "generation":
						c.CloudImmutableID = "replacement"
					}
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				if change == "account" {
					client.claimScope = "subscription:foreign|resource-group:rg"
				}
				// Leave the configured subscription empty: the client discovers it from az login.
				config := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(config, []byte("provider: azure\nazure:\n  resourceGroup: rg\n  location: eastus\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				for _, name := range []string{"AZURE_SUBSCRIPTION_ID", "CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN"} {
					t.Setenv(name, "")
				}
				client.tagged = nil
				args := []string{command, "--provider", "azure", "--id", lease.LeaseID, "--json"}
				if command == "heartbeat" {
					args = append(args, "--idle-timeout", "45m")
				} else {
					args = append(args, "--wait", "--wait-timeout", "100ms")
				}
				var stdout, stderr bytes.Buffer
				err = (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				if change == "owned" {
					if len(client.tagged) != 1 || (command == "heartbeat" && err != nil) {
						t.Fatalf("owned lease not touched: tags=%v err=%v stderr=%s", client.tagged, err, &stderr)
					}
				} else if len(client.tagged) != 0 || (command == "heartbeat" && err == nil) {
					t.Fatalf("foreign claim touched: tags=%v err=%v", client.tagged, err)
				}
			})
		}
	}
}
