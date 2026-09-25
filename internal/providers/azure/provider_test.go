package azure

import (
	"flag"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAzureRejectsRemovedDiskMode(t *testing.T) {
	for _, source := range []string{"config", "flag"} {
		t.Run(source, func(t *testing.T) {
			cfg := core.Config{Provider: "azure", Azure: core.AzureConfig{Backend: "vm"}}
			if source == "config" {
				cfg.Azure.OSDisk = "ephemeral-preview"
			}
			fs := flag.NewFlagSet("migration", flag.ContinueOnError)
			values := (Provider{}).RegisterFlags(fs, cfg)
			if source == "flag" {
				if err := fs.Parse([]string{"--azure-os-disk=ephemeral-preview"}); err != nil {
					t.Fatal(err)
				}
			}
			err := (Provider{}).ApplyFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "has been removed; use ephemeral") || !strings.Contains(err.Error(), "--azure-os-disk ephemeral") {
				t.Fatalf("expected actionable migration error, got %v", err)
			}
		})
	}
}

func TestAzureFlatInputTracking(t *testing.T) {
	for _, tc := range []struct {
		name, raw     string
		accepted, bad bool
	}{{"azure-backend", "vm", true, false}, {"azure-backend", "dynamic-sessions", true, false}, {"azure-backend", "invalid", false, true}, {"azure-os-disk", "managed", true, false}, {"azure-os-disk", "invalid", false, true}, {"azure-snapshot-sku", "Standard_LRS", true, false}, {"azure-snapshot-sku", "invalid", true, true}, {"azure-os-disk-sku", "Standard_LRS", true, false}, {"azure-os-disk-sku", "invalid", true, true}} {
		t.Run(tc.name+"/"+tc.raw, func(t *testing.T) {
			cfg := core.Config{Provider: "azure", Azure: core.AzureConfig{Backend: "vm"}}
			fs := flag.NewFlagSet("metadata", flag.ContinueOnError)
			v := (Provider{}).RegisterFlags(fs, cfg)
			if err := fs.Parse([]string{"--" + tc.name + "=" + tc.raw}); err != nil {
				t.Fatal(err)
			}
			err := (Provider{}).ApplyFlags(&cfg, fs, v)
			if (err != nil) != tc.bad {
				t.Fatalf("unexpected validation outcome: %v", err)
			}
			want := core.Config{Provider: "azure", Azure: core.AzureConfig{Backend: "vm"}}
			if tc.accepted {
				core.RecordProviderFlagInputs(&want, true, "azure")
				switch tc.name {
				case "azure-backend":
					want.Azure.Backend = tc.raw
					if tc.raw == "dynamic-sessions" {
						want.Provider = "azure-dynamic-sessions"
					}
				case "azure-os-disk":
					want.Azure.OSDisk = tc.raw
					want.Azure.OSDiskExplicit = true
				case "azure-snapshot-sku":
					want.Azure.SnapshotSKU = tc.raw
				case "azure-os-disk-sku":
					want.Azure.OSDiskSKU = tc.raw
				}
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Fatal("accepted flat flags or partial state attributed incorrectly")
			}
		})
	}
	cfg := core.Config{Provider: "azure", Azure: core.AzureConfig{Backend: "vm"}}
	fs := flag.NewFlagSet("wrong", flag.ContinueOnError)
	(Provider{}).RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--azure-backend=vm"}); err != nil {
		t.Fatal(err)
	}
	before := cfg
	if err := (Provider{}).ApplyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
		t.Fatal("wrong values object counted as input")
	}
}

func TestPrepareLeaseClaimEndpointPreservesExactAzureIdentity(t *testing.T) {
	provider := Provider{}
	existing := core.LeaseClaim{
		LeaseID:          "cbx_123456abcdef",
		Slug:             "owned",
		CloudID:          "crabbox-owned",
		CloudImmutableID: "vmid-owned",
		Labels: map[string]string{
			"provider_key": core.ProviderKeyForLease("cbx_123456abcdef"),
		},
	}
	server := core.Server{
		CloudID:     "crabbox-owned",
		ImmutableID: "vmid-owned",
		Labels: map[string]string{
			"lease":        existing.LeaseID,
			"slug":         existing.Slug,
			"provider_key": existing.Labels["provider_key"],
		},
	}
	got, err := provider.PrepareLeaseClaimEndpoint(existing, "azure", existing.Slug, server, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloudID != existing.CloudID || got.ImmutableID != existing.CloudImmutableID {
		t.Fatalf("server=%+v, want exact existing Azure identity", got)
	}
}

func TestPrepareLeaseClaimEndpointRejectsIdentityRetargeting(t *testing.T) {
	provider := Provider{}
	existing := core.LeaseClaim{
		LeaseID:          "cbx_123456abcdef",
		Slug:             "owned",
		CloudID:          "crabbox-owned",
		CloudImmutableID: "vmid-owned",
		Labels: map[string]string{
			"provider_key": core.ProviderKeyForLease("cbx_123456abcdef"),
		},
	}
	base := core.Server{
		CloudID:     existing.CloudID,
		ImmutableID: existing.CloudImmutableID,
		Labels: map[string]string{
			"lease":        existing.LeaseID,
			"slug":         existing.Slug,
			"provider_key": existing.Labels["provider_key"],
		},
	}
	for _, test := range []struct {
		name   string
		mutate func(*core.Server)
	}{
		{name: "name", mutate: func(server *core.Server) { server.CloudID = "replacement" }},
		{name: "immutable id", mutate: func(server *core.Server) { server.ImmutableID = "vmid-replacement" }},
		{name: "provider key", mutate: func(server *core.Server) { server.Labels["provider_key"] = "wrong" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := base
			server.Labels = map[string]string{}
			for key, value := range base.Labels {
				server.Labels[key] = value
			}
			test.mutate(&server)
			_, err := provider.PrepareLeaseClaimEndpoint(existing, "azure", existing.Slug, server, true)
			if err == nil || !strings.Contains(err.Error(), "refusing to rewrite Azure") {
				t.Fatalf("err=%v, want identity retarget rejection", err)
			}
		})
	}
}

func TestPrepareLeaseClaimEndpointDoesNotPromoteLegacyClaim(t *testing.T) {
	existing := core.LeaseClaim{LeaseID: "cbx_123456abcdef", Slug: "legacy", CloudID: "crabbox-legacy"}
	server := core.Server{
		CloudID:     existing.CloudID,
		ImmutableID: "vmid-later",
		Labels: map[string]string{
			"lease":        existing.LeaseID,
			"slug":         existing.Slug,
			"provider_key": core.ProviderKeyForLease(existing.LeaseID),
		},
	}
	got, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "azure", existing.Slug, server, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImmutableID != "" || got.Labels["provider_key"] != "" {
		t.Fatalf("server=%+v, want legacy claim metadata left unpromoted", got)
	}
}

func TestIsCrabboxAzureLeaseRequiresCanonicalTags(t *testing.T) {
	t.Parallel()
	canonical := map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"provider":   "azure",
		"lease":      "cbx_123456abcdef",
		"slug":       "owned",
	}
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{name: "nil labels", labels: nil, want: false},
		{name: "no crabbox tag", labels: map[string]string{"managed_by": "crabbox"}, want: false},
		{name: "different provider", labels: map[string]string{"crabbox": "true", "provider": "aws"}, want: false},
		{name: "weak azure tags", labels: map[string]string{"crabbox": "true", "provider": "azure"}, want: false},
		{name: "missing provider", labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "lease": "cbx_123456abcdef", "slug": "owned"}, want: false},
		{name: "canonical", labels: canonical, want: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := core.Server{Labels: tc.labels}
			if got := isCrabboxAzureLease(s); got != tc.want {
				t.Fatalf("labels=%+v got %v want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestProviderAppliesAzureOSDiskFlag(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{})
	if err := fs.Parse([]string{"--azure-os-disk", "managed"}); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "azure"}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Azure.OSDisk != core.AzureOSDiskManaged {
		t.Fatalf("AzureOSDisk=%q want %q", cfg.Azure.OSDisk, core.AzureOSDiskManaged)
	}
	if !cfg.Azure.OSDiskExplicit {
		t.Fatal("AzureOSDiskExplicit=false, want true")
	}
}

func TestProviderAppliesAzureSnapshotStorageFlags(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{})
	if err := fs.Parse([]string{
		"--azure-snapshot-sku", "premium_lrs",
		"--azure-os-disk-sku", "Premium_LRS",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "azure"}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Azure.SnapshotSKU != "Premium_LRS" || cfg.Azure.OSDiskSKU != "Premium_LRS" {
		t.Fatalf("snapshot SKU=%q OS disk SKU=%q", cfg.Azure.SnapshotSKU, cfg.Azure.OSDiskSKU)
	}
}

func TestProviderExplicitBackendRoutesToDynamicSessions(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{Azure: core.AzureConfig{Backend: core.AzureBackendVM}})
	if err := fs.Parse([]string{"--azure-backend", "dynamic-sessions"}); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "azure", Azure: core.AzureConfig{Backend: core.AzureBackendVM}}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.Azure.Backend != core.AzureBackendDynamicSessions {
		t.Fatalf("provider=%q backend=%q", cfg.Provider, cfg.Azure.Backend)
	}
}

func TestProviderValidatesConfiguredAzureOSDisk(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{})
	cfg := core.Config{Provider: "azure", Azure: core.AzureConfig{OSDisk: "premium"}}
	if err := provider.ApplyFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected invalid configured Azure OS disk mode to fail")
	}
}

func TestProviderSupportsDirectWindowsOSDiskCheckpoints(t *testing.T) {
	t.Parallel()
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config: core.Config{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Server: core.Server{CloudID: "crabbox-source"},
		Target: core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
	})
	if !ok {
		t.Fatal("expected Windows Azure checkpoint capability")
	}
	if capability.Kind != core.CheckpointKindAzureOS || !capability.Direct {
		t.Fatalf("capability=%+v, want direct Azure OS disk snapshot", capability)
	}
}

func TestProviderRejectsWindowsImageCheckpoints(t *testing.T) {
	t.Parallel()
	_, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config:   core.Config{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Server:   core.Server{CloudID: "crabbox-source"},
		Target:   core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Strategy: core.CheckpointStrategyImage,
	})
	if ok {
		t.Fatal("Azure Windows leases must not advertise managed image checkpoints")
	}
}

func TestProviderRejectsDirectWSL2OSDiskCheckpoints(t *testing.T) {
	t.Parallel()
	_, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
		Config: core.Config{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeWSL2},
		Server: core.Server{CloudID: "crabbox-source"},
		Target: core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeWSL2},
	})
	if ok {
		t.Fatal("WSL2 Azure leases must not advertise native Windows snapshot forks")
	}
}

func TestProviderAppliesWindowsSnapshotForkAzureScope(t *testing.T) {
	t.Parallel()
	cfg := core.Config{}
	err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind:     core.CheckpointKindAzureOS,
			Resource: "/subscriptions/sub/resourceGroups/snapshot-rg/providers/Microsoft.Compute/snapshots/checkpoint",
			Region:   "westus2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Azure.Snapshot == "" || cfg.Azure.Location != "westus2" || cfg.Azure.ResourceGroup != "snapshot-rg" || cfg.Azure.Subscription != "sub" {
		t.Fatalf("fork config=%+v", cfg)
	}
}

func TestProviderReappliesOSDiskSKUAfterCheckpointProviderRewrite(t *testing.T) {
	t.Parallel()
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, core.Config{})
	if err := fs.Parse([]string{"--azure-os-disk-sku", "premium_lrs"}); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: "azure"}
	if err := provider.ApplyNativeCheckpointForkFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Azure.OSDiskSKU != "Premium_LRS" {
		t.Fatalf("AzureOSDiskSKU=%q", cfg.Azure.OSDiskSKU)
	}
}

func TestProviderRegistered(t *testing.T) {
	provider, err := core.ProviderFor("azure")
	if err != nil {
		t.Fatalf("expected azure provider to be registered: %v", err)
	}
	if got := provider.Spec().Name; got != "azure" {
		t.Fatalf("provider name = %q, want %q", got, "azure")
	}
}

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != "azure" {
		t.Fatalf("spec.Name = %q, want azure", spec.Name)
	}
	if spec.Kind != core.ProviderKindSSHLease {
		t.Fatalf("spec.Kind = %q, want %q", spec.Kind, core.ProviderKindSSHLease)
	}
	if spec.Coordinator != core.CoordinatorSupported {
		t.Fatalf("spec.Coordinator = %q, want %q", spec.Coordinator, core.CoordinatorSupported)
	}
	wantTargets := []core.TargetSpec{
		{OS: core.TargetLinux},
		{OS: core.TargetWindows, WindowsMode: "normal"},
		{OS: core.TargetWindows, WindowsMode: "wsl2"},
	}
	if len(spec.Targets) != len(wantTargets) {
		t.Fatalf("spec.Targets = %+v, want %+v", spec.Targets, wantTargets)
	}
	for i, want := range wantTargets {
		if spec.Targets[i] != want {
			t.Fatalf("spec.Targets[%d] = %+v, want %+v", i, spec.Targets[i], want)
		}
	}
	wantFeatures := []core.Feature{
		core.FeatureSSH,
		core.FeatureCrabboxSync,
		core.FeatureCleanup,
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCode,
		core.FeatureTailscale,
	}
	if len(spec.Features) != len(wantFeatures) {
		t.Fatalf("spec.Features = %+v, want %+v", spec.Features, wantFeatures)
	}
	for i, f := range wantFeatures {
		if spec.Features[i] != f {
			t.Fatalf("spec.Features[%d] = %q, want %q", i, spec.Features[i], f)
		}
	}
}
