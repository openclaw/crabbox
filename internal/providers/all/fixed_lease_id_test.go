package all

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestFixedLeaseIDCatalogMatchesBackends(t *testing.T) {
	testutil.IsolateUserDirs(t)
	var out bytes.Buffer
	if err := (core.App{Stdout: &out, Stderr: io.Discard}).Run(t.Context(), []string{"providers", "--json"}); err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Provider string          `json:"provider"`
		Features core.FeatureSet `json:"features"`
	}
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	catalog := make(map[string]bool, len(entries))
	for _, entry := range entries {
		catalog[entry.Provider] = entry.Features.Has(core.FeatureFixedLeaseID)
	}
	names := core.RegisteredProviderNames()
	if len(names) == 0 {
		t.Fatal("no providers registered")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			provider := mustProvider(t, name)
			cfg, ok := offlineConformanceConfig(name)
			if !ok {
				t.Fatal("missing offline conformance config")
			}
			backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			configured, implements := backend.(core.IdempotentLeaseIDBackend)
			var declared core.IdempotentLeaseIDBackend
			if source, ok := provider.(core.ProviderBackendCapabilitySource); ok {
				declared, _ = source.BackendCapabilities().(core.IdempotentLeaseIDBackend)
			}
			if implements != (declared != nil) {
				t.Errorf("configured backend %T implements IdempotentLeaseIDBackend=%t, capability declaration=%t", backend, implements, declared != nil)
			}
			supported := implements && configured.SupportsRequestedLeaseID()
			if advertised := declared != nil && declared.SupportsRequestedLeaseID(); advertised != supported {
				t.Errorf("backend capability declaration=%t, configured support=%t", advertised, supported)
			}
			spec := provider.Spec()
			if spec.Features.Has(core.FeatureFixedLeaseID) {
				t.Error("fixed-lease-id must be derived from backend capabilities, not Spec().Features")
			}
			// Brokered SSH providers advertise the coordinator wrapper's support.
			want := supported || (spec.Kind == core.ProviderKindSSHLease && spec.Coordinator == core.CoordinatorSupported)
			got, exists := catalog[name]
			if !exists {
				t.Fatal("provider missing from catalog")
			}
			if got != want {
				t.Errorf("catalog fixed-lease-id=%t, want %t", got, want)
			}
		})
	}
}
