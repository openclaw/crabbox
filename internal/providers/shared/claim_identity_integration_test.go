package shared_test

import (
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/aws"
	_ "github.com/openclaw/crabbox/internal/providers/azure"
)

func TestClaimIdentityProjectionPersistsThroughProviderAdapters(t *testing.T) {
	for _, provider := range []struct {
		name string
		keys []string
	}{
		{"aws", []string{"aws_key_pair_id", "aws_account_id"}},
		{"azure", []string{"provider_key"}},
	} {
		for _, scenario := range append([]string{"omitted", "confirmed", "legacy"}, provider.keys...) {
			t.Run(provider.name+"/"+scenario, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				const leaseID, slug = "cbx_abcdef123456", "identity-proof"
				cfg := core.Config{Provider: provider.name}
				server := core.Server{Provider: provider.name, CloudID: "resource-original", Labels: map[string]string{"lease": leaseID, "slug": slug, "state": "ready"}}
				for _, key := range provider.keys {
					if scenario != "legacy" {
						server.Labels[key] = "recorded-" + key
					}
				}
				if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
					t.Fatal(err)
				}
				before, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Fatal(err)
				}
				observed := server
				observed.Labels = maps.Clone(server.Labels)
				observed.Labels["state"] = "running"
				for _, key := range provider.keys {
					switch scenario {
					case "omitted":
						delete(observed.Labels, key)
					case "legacy":
						observed.Labels[key] = "unrecorded-observation"
					case key:
						observed.Labels[key] = "replacement"
					}
				}
				updated, updateErr := core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, before, observed, core.SSHTarget{Host: "192.0.2.25", Port: "22"})
				after, readErr := core.ReadLeaseClaim(leaseID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				conflict := scenario != "omitted" && scenario != "confirmed" && scenario != "legacy"
				if conflict {
					var exit core.ExitError
					if !errors.As(updateErr, &exit) || exit.Code != 2 || !reflect.DeepEqual(after, before) || !reflect.DeepEqual(updated, core.LeaseClaim{}) {
						t.Fatalf("conflicting identity changed durable claim or lost exit 2: %v", updateErr)
					}
					t.Log("actual claim transaction: conflict rejected with exit 2; durable claim unchanged")
					return
				}
				if updateErr != nil || !reflect.DeepEqual(updated, after) || after.Revision == before.Revision || after.SSHHost != "192.0.2.25" {
					t.Fatalf("endpoint update did not return the committed on-disk claim: %v", updateErr)
				}
				for _, key := range provider.keys {
					if after.Labels[key] != before.Labels[key] {
						t.Fatalf("persisted cleanup identity %s changed", key)
					}
				}
				t.Logf("actual claim transaction: endpoint committed; recorded cleanup identity preserved; legacy=%t", scenario == "legacy")
			})
		}
	}
}
