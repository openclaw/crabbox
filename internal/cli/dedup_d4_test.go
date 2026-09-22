package cli

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestDedupD4TypedFlagApplication(t *testing.T) {
	for _, synthesized := range []bool{false, true} {
		cfg := Config{Nomad: defaultNomadConfig()}
		cfg.synthesizedFlagInputs = synthesized
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		values := RegisterNomadConfigFlags(fs, cfg.Nomad)
		for _, wrong := range []any{nil, &values, ModalConfigFlagValues{}} {
			matched, err := ApplyProviderConfigFlags[NomadConfigFlagValues](&cfg, fs, wrong, &cfg.Nomad, "nomad")
			if matched || err != nil || cfg.inputProvenance != nil {
				t.Fatalf("wrong type: matched=%t, err=%v", matched, err)
			}
		}
		matched, err := ApplyProviderConfigFlags[NomadConfigFlagValues](&cfg, fs, values, &cfg.Nomad, "nomad")
		if !matched || err != nil || cfg.inputProvenance != nil {
			t.Fatalf("unvisited: matched=%t, err=%v", matched, err)
		}
		if err := fs.Parse([]string{"--nomad-region=earlier", "--nomad-alloc-ready-timeout=invalid"}); err != nil {
			t.Fatal(err)
		}
		matched, err = ApplyProviderConfigFlags[NomadConfigFlagValues](&cfg, fs, values, &cfg.Nomad, "nomad")
		if !matched || err == nil || cfg.Nomad.Region != "earlier" {
			t.Fatalf("partial error: matched=%t, err=%v", matched, err)
		}
		if accepted := cfg.inputProvenance["nomad"].values != 0; accepted == synthesized {
			t.Fatalf("accepted=%t, synthesized=%t", accepted, synthesized)
		}
	}
}

func dedupD4StringField(root any, path string) reflect.Value {
	value := reflect.ValueOf(root).Elem()
	for _, name := range strings.Split(path, ".") {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
		value = value.FieldByName(name)
	}
	return value
}

func TestDedupD4FileStringAssignments(t *testing.T) {
	for _, tc := range []struct {
		file, config string
		owner        configInputOwner
	}{
		{"Profile", "Profile", configInputGeneric},
		{"DesktopEnv", "DesktopEnv", configInputGeneric},
		{"HostID", "HostID", configInputGeneric},
		{"Hetzner.SSHKey", "ProviderKey", "hetzner"},
		{"AWS.AMI", "AWSAMI", "aws"},
		{"AWS.SecurityGroupID", "AWSSGID", "aws"},
		{"AWS.SubnetID", "AWSSubnetID", "aws"},
		{"AWS.InstanceProfile", "AWSProfile", "aws"},
		{"Azure.Backend", "Azure.Backend", "azure"},
		{"Azure.ClientID", "Azure.ClientID", "azure"},
		{"Azure.Location", "Azure.Location", "azure"},
		{"Azure.ResourceGroup", "Azure.ResourceGroup", "azure"},
		{"Azure.SnapshotSKU", "Azure.SnapshotSKU", "azure"},
		{"Azure.OSDiskSKU", "Azure.OSDiskSKU", "azure"},
		{"Azure.VNet", "Azure.VNet", "azure"},
		{"Azure.Subnet", "Azure.Subnet", "azure"},
		{"Azure.NSG", "Azure.NSG", "azure"},
		{"Azure.Network", "Azure.Network", "azure"},
		{"GCP.Subnet", "GCPSubnet", "gcp"},
		{"GCP.ServiceAccount", "GCPServiceAccount", "gcp"},
		{"Parallels.Template", "Parallels.Template", "parallels"},
		{"Parallels.Source", "Parallels.Source", "parallels"},
		{"Parallels.SourceID", "Parallels.SourceID", "parallels"},
		{"Parallels.SourceSnapshot", "Parallels.SourceSnapshot", "parallels"},
		{"Parallels.SourceSnapshotID", "Parallels.SourceSnapshotID", "parallels"},
		{"Parallels.CloneMode", "Parallels.CloneMode", "parallels"},
		{"Parallels.HostUser", "Parallels.HostUser", "parallels"},
		{"Parallels.User", "Parallels.User", "parallels"},
		{"Parallels.WorkRoot", "Parallels.WorkRoot", "parallels"},
		{"Sync.Source", "Sync.Source", configInputGeneric},
		{"Sync.GitSeedSource", "Sync.GitSeedSource", configInputGeneric},
		{"Sync.BaseRef", "Sync.BaseRef", configInputGeneric},
		{"Capacity.Strategy", "Capacity.Strategy", configInputGeneric},
		{"Capacity.Fallback", "Capacity.Fallback", configInputGeneric},
		{"External.Command", "External.Command", "external"},
		{"External.WorkRoot", "External.WorkRoot", "external"},
		{"CubeSandbox.Template", "CubeSandbox.Template", "cubesandbox"},
		{"CubeSandbox.Workdir", "CubeSandbox.Workdir", "cubesandbox"},
		{"CubeSandbox.User", "CubeSandbox.User", "cubesandbox"},
		{"AsciiBox.CLIPath", "AsciiBox.CLIPath", "ascii-box"},
		{"AsciiBox.Workdir", "AsciiBox.Workdir", "ascii-box"},
		{"Tailscale.HostnameTemplate", "Tailscale.HostnameTemplate", configInputGeneric},
		{"Tailscale.AuthKeyEnv", "Tailscale.AuthKeyEnv", configInputGeneric},
	} {
		for _, trusted := range []bool{false, true} {
			for _, input := range []string{"", "  ", "replacement"} {
				t.Run(tc.file+"/"+input+"/"+map[bool]string{false: "repo", true: "user"}[trusted], func(t *testing.T) {
					var cfg Config
					var file fileConfig
					dedupD4StringField(&cfg, tc.config).SetString("prior")
					dedupD4StringField(&file, tc.file).SetString(input)
					source, wantSource := providerSelectionRepoConfig, configInputRepo
					if trusted {
						source, wantSource = providerSelectionUserConfig, configInputUser
					}
					if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, trusted, source); err != nil {
						t.Fatal(err)
					}
					want := input
					if want == "" {
						want = "prior"
					}
					if got := dedupD4StringField(&cfg, tc.config).String(); got != want {
						t.Fatalf("value=%q, want %q", got, want)
					}
					var wantLedger configInputLedger
					if input != "" {
						wantLedger = wantLedger.withInput(tc.owner, wantSource, configInputValue)
					}
					if !reflect.DeepEqual(cfg.inputProvenance, wantLedger) {
						t.Fatalf("ledger=%#v, want %#v", cfg.inputProvenance, wantLedger)
					}
				})
			}
		}
	}
}

func TestDedupD4FileBoundariesAndAliases(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		var file fileConfig
		for field, value := range map[string]string{
			"Azure.SubscriptionID": "subscription", "Azure.TenantID": "tenant",
			"Parallels.Template": "template", "Parallels.Host": "host.example.test",
			"Parallels.HostKey": "key", "Parallels.Password": "fixture",
		} {
			dedupD4StringField(&file, field).SetString(value)
		}
		primary, alias := "https://primary.example.test", "https://alias.example.test"
		file.CloudflareSandbox = &fileCloudflareSandboxConfig{BridgeURL: &primary, BridgeURLConfigAlias: &alias}
		cfg := Config{}
		source, wantSource := providerSelectionRepoConfig, configInputRepo
		if trusted {
			source, wantSource = providerSelectionUserConfig, configInputUser
		}
		if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, trusted, source); err != nil {
			t.Fatal(err)
		}
		if cfg.Azure.Subscription != "subscription" || cfg.Azure.Tenant != "tenant" {
			t.Fatal("Azure family values changed")
		}
		for _, owner := range []configInputOwner{"azure", "azure-dynamic-sessions", "parallels"} {
			if cfg.inputProvenance[owner].values != 1<<(wantSource-1) {
				t.Fatalf("lost %s source", owner)
			}
		}
		if cfg.credentialProvenance.parallelsHost != credentialSourceForFile(trusted) || cfg.credentialProvenance.parallelsHostKey != credentialSourceForFile(trusted) {
			t.Fatal("lost Parallels credential provenance")
		}
		wantPassword, wantURL := "", ""
		if trusted {
			wantPassword, wantURL = "fixture", alias
		}
		if cfg.Parallels.Password != wantPassword || cfg.CloudflareSandbox.BridgeURL != wantURL {
			t.Fatal("trusted-only input or alias ordering changed")
		}
	}
}

func TestDedupD4FilePartialErrorOrder(t *testing.T) {
	cfg := Config{Profile: "prior"}
	cfg.Tailscale.HostnameTemplate = "prior-template"
	var file fileConfig
	file.Profile = "earlier"
	dedupD4StringField(&file, "Tailscale.HostnameTemplate").SetString("later")
	invalid := -1
	file.Blaxel = &fileBlaxelConfig{MemoryMB: &invalid}
	if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, false, providerSelectionRepoConfig); err == nil {
		t.Fatal("expected config error")
	}
	if cfg.Profile != "earlier" || cfg.Tailscale.HostnameTemplate != "prior-template" {
		t.Fatal("file declaration-order application changed")
	}
	if cfg.inputProvenance[configInputGeneric].values != 1<<(configInputRepo-1) || cfg.inputProvenance["aws"].values != 0 {
		t.Fatal("file partial-error provenance changed")
	}
}
