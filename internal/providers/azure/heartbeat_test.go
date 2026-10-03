package azure

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
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
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var stdout, stderr bytes.Buffer
				var output io.Writer = &stdout
				args := []string{command, "--provider", "azure", "--id", lease.LeaseID}
				if command == "heartbeat" {
					args = append(args, "--idle-timeout", "45m", "--json")
				} else {
					args = append(args, "--wait", "--wait-timeout", "30s")
					output = testutil.CancelOnWrite(&stdout, cancel)
				}
				err = (core.App{Stdout: output, Stderr: &stderr}).Run(ctx, args)
				if change == "owned" {
					if len(client.tagged) != 1 || (command == "heartbeat" && err != nil) {
						t.Fatalf("owned lease not touched: tags=%v err=%v stderr=%s", client.tagged, err, &stderr)
					}
					if command == "status" && !errors.Is(err, context.Canceled) {
						t.Fatalf("status did not finish its first poll: %v", err)
					}
				} else if len(client.tagged) != 0 || (command == "heartbeat" && err == nil) {
					t.Fatalf("foreign claim touched: tags=%v err=%v", client.tagged, err)
				}
			})
		}
	}
}
