package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestStaticFlagInputFacts(t *testing.T) {
	for _, name := range []string{"static-host", "static-user", "static-port", "static-work-root"} {
		for _, value := range []string{"", "sample"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Static = StaticConfig{Host: "old-host", User: "old-user", Port: "22", WorkRoot: "/old"}
				fs := newFlagSet("test", io.Discard)
				values := registerTargetFlags(fs, cfg)
				if err := fs.Parse([]string{"--" + name + "=" + value}); err != nil {
					t.Fatal(err)
				}
				if err := applyTargetFlagOverrides(&cfg, fs, values); err != nil {
					t.Fatal(err)
				}
				got := map[string]string{"static-host": cfg.Static.Host, "static-user": cfg.Static.User, "static-port": cfg.Static.Port, "static-work-root": cfg.Static.WorkRoot}[name]
				if got != value {
					t.Fatalf("assigned value=%q, want=%q", got, value)
				}
				if cfg.inputProvenance.summary("ssh").state != "present" {
					t.Fatal("accepted static flag source missing")
				}
			})
		}
	}
	t.Run("unvisited-defaults", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Static = StaticConfig{Host: "old-host", User: "old-user", Port: "22", WorkRoot: "/old"}
		before := cfg.Static
		fs := newFlagSet("test", io.Discard)
		values := registerTargetFlags(fs, cfg)
		if err := applyTargetFlagOverrides(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg.Static, before) || cfg.inputProvenance.summary("ssh").state != "unknown" {
			t.Fatal("unvisited static defaults changed value or acquired an input fact")
		}
	})
}

func TestAzureDynamicSessionsSharedInputFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   fileAzureConfig
		shared bool
	}{
		{"tenant", fileAzureConfig{TenantID: "sample-tenant"}, true},
		{"subscription", fileAzureConfig{SubscriptionID: "sample-subscription"}, true},
		{"client", fileAzureConfig{ClientID: "sample-client"}, false},
	} {
		t.Run("file/"+tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			if err := applyFileConfig(&cfg, fileConfig{Azure: &tc.file}); err != nil {
				t.Fatal(err)
			}
			if got := cfg.inputProvenance.summary("azure-dynamic-sessions").state == "present"; got != tc.shared {
				t.Fatalf("shared source=%t, want=%t", got, tc.shared)
			}
			if cfg.inputProvenance.summary("azure").state != "present" {
				t.Fatal("Azure source missing")
			}
		})
	}
	for _, name := range []string{"CRABBOX_AZURE_TENANT_ID", "AZURE_TENANT_ID", "CRABBOX_AZURE_SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID", "CRABBOX_AZURE_CLIENT_ID"} {
		t.Run("env/"+name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(name, "sample-identity")
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			want := name != "CRABBOX_AZURE_CLIENT_ID"
			if got := cfg.inputProvenance.summary("azure-dynamic-sessions").state == "present"; got != want {
				t.Fatalf("shared source=%t, want=%t", got, want)
			}
		})
	}
}

func TestExternalInputFactsSurviveSerializationError(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	file := fileConfig{External: &fileExternalConfig{
		Config:     map[string]any{"measurement": math.NaN()},
		Connection: &ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{TrustProviderOutput: true}},
	}}
	err := applyFileConfig(&cfg, file)
	if err == nil || !strings.Contains(err.Error(), "JSON encodable") {
		t.Fatal("expected ordinary non-finite JSON serialization rejection")
	}
	if cfg.External.Config == nil || !cfg.External.Connection.SSH.TrustProviderOutput {
		t.Fatal("source assignments unexpectedly changed")
	}
	if cfg.inputProvenance.summary("external").state != "present" {
		t.Fatal("earlier applied configuration was lost at the later serialization error")
	}
}

func TestManualBatchBFileInputFacts(t *testing.T) {
	clearConfigEnv(t)
	owners := map[string]configInputOwner{
		"Freestyle": "freestyle", "GitHubCodespaces": "github-codespaces",
		"Hostinger": "hostinger", "HyperV": "hyperv", "Incus": "incus",
		"Islo": "islo", "MXC": "mxc", "Nebius": "nebius", "Nomad": "nomad", "NvidiaBrev": "nvidia-brev",
	}
	for field, owner := range owners {
		fileType, ok := reflect.TypeFor[fileConfig]().FieldByName(field)
		if !ok {
			t.Fatalf("missing file owner %s", field)
		}
		t.Run(field+"/empty-section", func(t *testing.T) {
			var file fileConfig
			reflect.ValueOf(&file).Elem().FieldByName(field).Set(reflect.New(fileType.Type.Elem()))
			cfg := baseConfig()
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal("empty owner section failed")
			}
			if cfg.inputProvenance.summary(owner).state != "unknown" {
				t.Fatal("section presence alone acquired an input fact")
			}
		})
		for i := 0; i < fileType.Type.Elem().NumField(); i++ {
			member := fileType.Type.Elem().Field(i)
			t.Run(field+"/"+member.Name, func(t *testing.T) {
				var file fileConfig
				input := reflect.New(fileType.Type.Elem())
				value := input.Elem().Field(i)
				if value.Kind() == reflect.Pointer {
					value.Set(reflect.New(value.Type().Elem()))
					value = value.Elem()
				}
				switch value.Kind() {
				case reflect.String:
					raw := "sample"
					if strings.Contains(member.Name, "Timeout") || member.Name == "RetentionPeriod" {
						raw = "5m"
					}
					value.SetString(raw)
				case reflect.Int, reflect.Int32, reflect.Int64:
					value.SetInt(1)
				case reflect.Bool:
					value.SetBool(false)
				case reflect.Slice:
					value.Set(reflect.ValueOf([]string{"sample"}))
				default:
					t.Fatalf("add an ordinary fixture for %s", member.Name)
				}
				reflect.ValueOf(&file).Elem().FieldByName(field).Set(input)
				cfg := baseConfig()
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatalf("ordinary file input failed for %s", member.Name)
				}
				summary := cfg.inputProvenance.summary(owner)
				if summary.state != "present" || !reflect.DeepEqual(summary.sources, []string{"user_config"}) || summary.complete {
					t.Fatalf("input fact missing or incorrectly complete: %+v", summary)
				}
			})
		}
	}
}

func TestManualBatchBInputNoOpsAndPartialErrors(t *testing.T) {
	clearConfigEnv(t)
	for _, owner := range []configInputOwner{"freestyle", "github-codespaces", "hostinger", "hyperv", "incus", "islo", "mxc", "nebius", "nomad", "nvidia-brev"} {
		cfg := baseConfig()
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.inputProvenance.summary(owner).state != "unknown" {
			t.Fatalf("absent environment acquired an input fact for %s", owner)
		}
	}
	for _, name := range []string{"CRABBOX_FREESTYLE_VCPUS", "CRABBOX_NEBIUS_DISK_SIZE_GIB", "CRABBOX_HYPERV_CPUS", "CRABBOX_ISLO_VCPUS"} {
		t.Setenv(name, "invalid")
	}
	cfg := baseConfig()
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []configInputOwner{"freestyle", "nebius", "hyperv", "islo"} {
		if cfg.inputProvenance.summary(owner).state != "unknown" {
			t.Fatalf("ignored malformed integer acquired an input fact for %s", owner)
		}
	}
	negative := -1
	cfg = baseConfig()
	err := applyFileConfig(&cfg, fileConfig{Nomad: &fileNomadConfig{Region: "sample", MemoryMB: &negative}})
	if ExitCodeForError(err, 0) != 2 || cfg.Nomad.Region != "sample" || cfg.inputProvenance.summary("nomad").state != "present" {
		t.Fatal("earlier Nomad input did not survive its later file error")
	}
	t.Setenv("CRABBOX_NOMAD_REGION", "sample")
	t.Setenv("CRABBOX_NOMAD_EXEC_TIMEOUT_SECS", "-1")
	cfg = baseConfig()
	if err := applyEnv(&cfg); ExitCodeForError(err, 0) != 2 || cfg.inputProvenance.summary("nomad").state != "present" {
		t.Fatal("earlier Nomad input did not survive its later environment error")
	}
}

func TestManualBatchBEnvironmentInputFacts(t *testing.T) {
	groups := []struct {
		owner         configInputOwner
		prefix, names string
	}{
		{"freestyle", "CRABBOX_FREESTYLE_", "API_KEY API_URL WORKDIR VCPUS MEMORY_GB"},
		{"github-codespaces", "CRABBOX_GITHUB_CODESPACES_", "API_URL GH_PATH REPO REF MACHINE DEVCONTAINER_PATH WORKING_DIRECTORY GEO IDLE_TIMEOUT RETENTION_PERIOD DELETE_ON_RELEASE WORK_ROOT"},
		{"hostinger", "CRABBOX_HOSTINGER_", "API_TOKEN API_URL ITEM_ID PAYMENT_METHOD_ID TEMPLATE_ID DATA_CENTER_ID HOSTNAME_PREFIX USER WORK_ROOT ALLOW_PURCHASE RELEASE_ACTION"},
		{"hyperv", "CRABBOX_HYPERV_", "IMAGE USER WORK_ROOT CPUS MEMORY SWITCH GUEST_PASSWORD INIT_PASSWORD"},
		{"incus", "CRABBOX_INCUS_", "REMOTE PROJECT ADDRESS SOCKET INSTANCE_TYPE IMAGE PROFILE USER WORK_ROOT DELETE_ON_RELEASE START_TIMEOUT LAUNCH_PORT PROXY_LISTEN_HOST PROXY_LISTEN_PORT PROXY_DEVICE TLS_SERVER_CERT INSECURE_TLS REMOTE_IMAGE_SERVER"},
		{"islo", "CRABBOX_ISLO_", "API_KEY BASE_URL IMAGE WORKDIR GATEWAY_PROFILE SNAPSHOT_NAME VCPUS MEMORY_MB DISK_GB"},
		{"mxc", "CRABBOX_MXC_", "CLI VERSION CONTAINMENT NETWORK READONLY_PATHS READWRITE_PATHS ALLOWED_HOSTS BLOCKED_HOSTS ALLOW_DACL_MUTATION ALLOW_WINDOWS_UI EXPERIMENTAL"},
		{"nebius", "CRABBOX_NEBIUS_", "CLI PROFILE PARENT_ID SUBNET_ID PLATFORM PRESET IMAGE_FAMILY DISK_TYPE DISK_SIZE_GIB USER PUBLIC_IP SECURITY_GROUP_IDS SERVICE_ACCOUNT_ID RECOVERY_POLICY"},
		{"nomad", "CRABBOX_NOMAD_", "ADDR REGION NAMESPACE TOKEN_ENV CA_CERT CA_PATH CLIENT_CERT CLIENT_KEY TLS_SERVER_NAME SKIP_VERIFY TASK DRIVER IMAGE WORKDIR JOBSPEC_TEMPLATE NODE_POOL DATACENTERS CPU MEMORY_MB DISK_MB ALLOC_READY_TIMEOUT EVAL_TIMEOUT EXEC_TIMEOUT_SECS"},
		{"nvidia-brev", "CRABBOX_NVIDIA_BREV_", "CLI ORG TYPE GPU_NAME PROVIDER MODE LAUNCHABLE STARTUP_SCRIPT RELEASE_ACTION TARGET USER WORK_ROOT"},
	}
	for _, group := range groups {
		for _, suffix := range strings.Fields(group.names) {
			t.Run(string(group.owner)+"/"+suffix, func(t *testing.T) {
				clearConfigEnv(t)
				raw := "sample"
				switch suffix {
				case "VCPUS", "MEMORY_GB", "CPUS", "MEMORY", "MEMORY_MB", "DISK_GB", "DISK_SIZE_GIB", "CPU", "DISK_MB", "EXEC_TIMEOUT_SECS":
					raw = "1"
				case "IDLE_TIMEOUT", "RETENTION_PERIOD", "START_TIMEOUT", "ALLOC_READY_TIMEOUT", "EVAL_TIMEOUT":
					raw = "5m"
				case "DELETE_ON_RELEASE", "ALLOW_PURCHASE", "INIT_PASSWORD", "INSECURE_TLS", "ALLOW_DACL_MUTATION", "ALLOW_WINDOWS_UI", "EXPERIMENTAL", "SKIP_VERIFY":
					raw = "false"
				}
				t.Setenv(group.prefix+suffix, raw)
				cfg := baseConfig()
				if err := applyEnv(&cfg); err != nil {
					t.Fatalf("ordinary environment input failed for %s", suffix)
				}
				summary := cfg.inputProvenance.summary(group.owner)
				if summary.state != "present" || !reflect.DeepEqual(summary.sources, []string{"environment"}) || summary.complete {
					t.Fatalf("missing or incorrectly complete environment fact: %+v", summary)
				}
			})
		}
	}
}

func TestFlatProviderFileInputTracking(t *testing.T) {
	fields := map[string][]string{
		"hetzner": {"location", "image", "sshKey"},
		"aws":     {"region", "ami", "securityGroupId", "subnetId", "instanceProfile", "rootGB", "sshCIDRs", "macHostId"},
		"azure":   {"backend", "subscriptionId", "tenantId", "clientId", "location", "resourceGroup", "image", "osDisk", "snapshotSKU", "osDiskSKU", "vnet", "subnet", "nsg", "sshCIDRs", "network"},
		"gcp":     {"project", "zone", "image", "network", "subnet", "tags", "sshCIDRs", "rootGB", "serviceAccount"},
	}
	for owner, keys := range fields {
		for _, key := range keys {
			t.Run(owner+"/"+key, func(t *testing.T) {
				clearConfigEnv(t)
				for _, trusted := range []bool{true, false} {
					for _, accepted := range []bool{false, true} {
						value := "''"
						if accepted {
							value = "fixture"
						}
						if key == "rootGB" {
							value = "0"
							if accepted {
								value = "1"
							}
						}
						if key == "tags" || key == "sshCIDRs" {
							value = "[]"
							if accepted {
								value = "[fixture]"
							}
						}
						var file fileConfig
						if err := yaml.Unmarshal([]byte(owner+": {"+key+": "+value+"}"), &file); err != nil {
							t.Fatal(err)
						}
						cfg := baseConfig()
						source := configInputUser
						if !trusted {
							source = configInputRepo
						}
						for repeat := 0; repeat < 2; repeat++ {
							cfg.inputProvenance = nil
							if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
								t.Fatal(err)
							}
							var want configInputLedger
							if accepted {
								want = want.withInput(configInputOwner(owner), source, configInputValue)
								if owner == "aws" && key == "region" {
									want = want.withInput("aws-lambda-microvm", source, configInputValue)
								}
								if owner == "azure" && (key == "tenantId" || key == "subscriptionId") {
									want = want.withInput("azure-dynamic-sessions", source, configInputValue)
								}
							}
							if !reflect.DeepEqual(cfg.inputProvenance, want) {
								t.Fatalf("wrong accepted owner/source for %s.%s", owner, key)
							}
						}
					}
				}
			})
		}
	}
}

func TestFlatProviderEnvironmentInputTracking(t *testing.T) {
	t.Run("empty environment", func(t *testing.T) {
		clearConfigEnv(t)
		cfg := baseConfig()
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		for _, owner := range []configInputOwner{"aws", "aws-lambda-microvm", "azure", "gcp", "hetzner"} {
			if cfg.inputProvenance.summary(owner).state != "unknown" {
				t.Fatal("fallback recorded as input")
			}
		}
	})

	fields := map[string][]string{
		"hetzner": {"CRABBOX_HETZNER_LOCATION", "CRABBOX_HETZNER_IMAGE", "CRABBOX_HETZNER_SSH_KEY"},
		"aws":     {"CRABBOX_AWS_REGION", "AWS_REGION", "CRABBOX_AWS_AMI", "CRABBOX_AWS_SECURITY_GROUP_ID", "CRABBOX_AWS_SUBNET_ID", "CRABBOX_AWS_INSTANCE_PROFILE", "CRABBOX_AWS_ROOT_GB", "CRABBOX_AWS_MAC_HOST_ID", "CRABBOX_AWS_SSH_CIDRS"},
		"azure":   {"CRABBOX_AZURE_BACKEND", "CRABBOX_AZURE_SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID", "CRABBOX_AZURE_TENANT_ID", "AZURE_TENANT_ID", "CRABBOX_AZURE_CLIENT_ID", "AZURE_CLIENT_ID", "CRABBOX_AZURE_LOCATION", "CRABBOX_AZURE_RESOURCE_GROUP", "CRABBOX_AZURE_IMAGE", "CRABBOX_AZURE_OS_DISK", "CRABBOX_AZURE_SNAPSHOT_SKU", "CRABBOX_AZURE_OS_DISK_SKU", "CRABBOX_AZURE_VNET", "CRABBOX_AZURE_SUBNET", "CRABBOX_AZURE_NSG", "CRABBOX_AZURE_SSH_CIDRS", "CRABBOX_AZURE_NETWORK"},
		"gcp":     {"CRABBOX_GCP_PROJECT", "GOOGLE_CLOUD_PROJECT", "GCP_PROJECT_ID", "CRABBOX_GCP_ZONE", "CRABBOX_GCP_IMAGE", "CRABBOX_GCP_NETWORK", "CRABBOX_GCP_SUBNET", "CRABBOX_GCP_ROOT_GB", "CRABBOX_GCP_SERVICE_ACCOUNT", "CRABBOX_GCP_TAGS", "CRABBOX_GCP_SSH_CIDRS"},
	}
	for owner, keys := range fields {
		for _, key := range keys {
			t.Run(owner+"/"+key, func(t *testing.T) {
				clearConfigEnv(t)
				value := "fixture"
				if strings.HasSuffix(key, "ROOT_GB") {
					value = "0"
				}
				t.Setenv(key, value)
				cfg := baseConfig()
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				want := configInputLedger(nil).withInput(configInputOwner(owner), configInputEnvironment, configInputValue)
				if key == "CRABBOX_AWS_REGION" || key == "AWS_REGION" {
					want = want.withInput("aws-lambda-microvm", configInputEnvironment, configInputValue)
				}
				if key == "CRABBOX_AZURE_TENANT_ID" || key == "AZURE_TENANT_ID" || key == "CRABBOX_AZURE_SUBSCRIPTION_ID" || key == "AZURE_SUBSCRIPTION_ID" {
					want = want.withInput("azure-dynamic-sessions", configInputEnvironment, configInputValue)
				}
				if key == "CRABBOX_GCP_ROOT_GB" {
					want = want.withInput("gcp", configInputEnvironment, configInputIntent)
				}
				if !reflect.DeepEqual(cfg.inputProvenance, want) {
					t.Fatalf("wrong accepted environment owner for %s", key)
				}
			})
		}
	}
	for _, tc := range []struct {
		key, raw string
		effect   configInputEffect
	}{{"CRABBOX_AWS_ROOT_GB", "invalid", 0}, {"CRABBOX_AWS_ROOT_GB", "2147483648", 0}, {"CRABBOX_AWS_ROOT_GB", "5", configInputValue}, {"CRABBOX_GCP_ROOT_GB", "invalid", configInputIntent}, {"CRABBOX_GCP_ROOT_GB", "5", configInputValue | configInputIntent}} {
		t.Run(tc.key+"/"+tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.raw)
			cfg := baseConfig()
			cfg.AWSRootGB = 5
			cfg.GCPRootGB = 5
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.AWSRootGB != 5 || cfg.GCPRootGB != 5 {
				t.Fatal("fallback/equal value changed")
			}
			owner := configInputOwner("aws")
			if strings.Contains(tc.key, "GCP") {
				owner = "gcp"
				if !cfg.gcpRootGBExplicit {
					t.Fatal("existing intent lost")
				}
			}
			if cfg.inputProvenance.summary(owner).effects != tc.effect {
				t.Fatal("parse acceptance confused with fallback intent")
			}
		})
	}
	t.Run("ignored project alias", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("GOOGLE_CLOUD_PROJECT", "fixture")
		cfg := baseConfig()
		cfg.GCPProject = "prior"
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.GCPProject != "prior" || cfg.inputProvenance.summary("gcp").state != "unknown" {
			t.Fatal("ignored alias recorded as accepted")
		}
	})
	t.Run("region primary and copy isolation", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("CRABBOX_AWS_REGION", "same")
		t.Setenv("AWS_REGION", "alias")
		cfg := baseConfig()
		cfg.AWSRegion = "same"
		original := cfg
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.AWSRegion != "same" || cfg.inputProvenance.summary("aws").state != "present" || cfg.inputProvenance.summary("aws-lambda-microvm").state != "present" || original.inputProvenance != nil {
			t.Fatal("shared equal region/copy handling")
		}
	})
}

func TestConfigInputProvenance(t *testing.T) {
	const provider configInputOwner = "machine0"
	for _, tc := range []struct {
		name     string
		ledger   configInputLedger
		state    string
		sources  []string
		effects  configInputEffect
		complete bool
	}{
		{"untracked", nil, "unknown", []string{}, 0, false},
		{"complete defaults", configInputLedger(nil).withCoverage(provider, true), "none", []string{}, 0, true},
		{"ignored input", configInputLedger(nil).withInput(provider, configInputEnvironment, 0), "unknown", []string{}, 0, false},
		{"accepted value", configInputLedger(nil).withInput(provider, configInputUser, configInputValue), "present", []string{"user_config"}, configInputValue, false},
		{"intent without parsed value", configInputLedger(nil).withInput(provider, configInputEnvironment, configInputIntent), "present", []string{"environment"}, configInputIntent, false},
		{"both effects one source", configInputLedger(nil).withInput(provider, configInputFlag, configInputValue|configInputIntent), "present", []string{"flag"}, configInputValue | configInputIntent, false},
		{"covered accepted value", configInputLedger(nil).withInput(provider, configInputRepo, configInputValue).withCoverage(provider, true), "present", []string{"repo_config"}, configInputValue, true},
		{"invalidated coverage", configInputLedger(nil).withCoverage(provider, true).withCoverage(provider, false), "unknown", []string{}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ledger.summary(provider)
			want := configInputSummary{tc.state, tc.sources, tc.effects, tc.complete}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("summary=%#v, want %#v", got, want)
			}
		})
	}
}

func TestConfigInputProvenanceCopyIsolation(t *testing.T) {
	const provider configInputOwner = "machine0"
	original := Config{inputProvenance: configInputLedger(nil).withInput(provider, configInputUser, configInputValue).withCoverage(provider, true)}
	copy := original
	copy.inputProvenance = copy.inputProvenance.withInput(provider, configInputEnvironment, configInputIntent).withCoverage(provider, false)
	copy.inputProvenance = copy.inputProvenance.withInput(configInputGeneric, configInputRepo, configInputValue)
	if got := original.inputProvenance.summary(provider); !reflect.DeepEqual(got, configInputSummary{"present", []string{"user_config"}, configInputValue, true}) {
		t.Fatalf("copy changed original: %#v", got)
	}
	if got := original.inputProvenance.summary(configInputGeneric); got.state != "unknown" || len(got.sources) != 0 {
		t.Fatalf("copy added generic input to original: %#v", got)
	}
	got := copy.inputProvenance.summary(provider)
	if !reflect.DeepEqual(got, configInputSummary{"present", []string{"user_config", "environment"}, configInputValue | configInputIntent, false}) {
		t.Fatalf("copy summary=%#v", got)
	}
	got.sources[0] = "changed"
	if copy.inputProvenance.summary(provider).sources[0] != "user_config" {
		t.Fatal("projection shares mutable source storage")
	}
}

func TestConfigInputProvenanceSourceSets(t *testing.T) {
	const provider configInputOwner = "machine0"
	var ledger configInputLedger
	for _, source := range []configInputSource{configInputFlag, configInputEnvironment, configInputUser, configInputRepo, configInputUser} {
		ledger = ledger.withInput(provider, source, configInputValue)
	}
	if got := ledger.summary(provider).sources; !reflect.DeepEqual(got, []string{"user_config", "repo_config", "environment", "flag"}) {
		t.Fatalf("ordered unique sources=%v", got)
	}
	before := ledger.summary(provider)
	for _, source := range []configInputSource{0, 5, 255} {
		ledger = ledger.withInput(provider, source, configInputIntent)
	}
	ledger = ledger.withInput("", configInputUser, configInputIntent).withCoverage("", true)
	if !reflect.DeepEqual(ledger.summary(provider), before) || len(ledger) != 1 {
		t.Fatalf("unknown source or owner changed ledger: %#v", ledger)
	}
	if got := ledger.summary("tart"); got.state != "unknown" || len(got.sources) != 0 {
		t.Fatalf("untracked provider inherited another input owner: %#v", got)
	}
	ledger = ledger.withInput(provider, configInputUser, configInputIntent).withInput(provider, configInputEnvironment, configInputIntent)
	if facts := ledger[provider]; facts.values != 0b1111 || facts.intents != 0b0101 {
		t.Fatalf("lost per-source effect attribution: %#v", facts)
	}
}

func TestConfigInputProvenanceCoverageCopyIsolation(t *testing.T) {
	const provider configInputOwner = "machine0"
	original := Config{inputProvenance: configInputLedger(nil).withInput(provider, configInputUser, configInputValue)}
	covered := original
	covered.inputProvenance = covered.inputProvenance.withCoverage(provider, true)
	if original.inputProvenance.summary(provider).complete || !covered.inputProvenance.summary(provider).complete {
		t.Fatal("coverage change mutated original or did not apply to copy")
	}
	untracked := covered
	untracked.inputProvenance = untracked.inputProvenance.withCoverage(provider, false)
	if !covered.inputProvenance.summary(provider).complete || untracked.inputProvenance.summary(provider).complete {
		t.Fatal("coverage invalidation mutated original or did not apply to copy")
	}
	for _, cfg := range []Config{original, covered, untracked} {
		if got := cfg.inputProvenance.summary(provider); got.state != "present" || !reflect.DeepEqual(got.sources, []string{"user_config"}) {
			t.Fatalf("coverage update changed accepted input: %#v", got)
		}
	}
}

func TestConfigInputApplicationSources(t *testing.T) {
	var cfg Config
	recordConfigInput(&cfg, "machine0", configInputSourceForFile(providerSelectionUserConfig), true)
	recordConfigInput(&cfg, "machine0", configInputSourceForFile(providerSelectionRepoConfig), true)
	recordConfigInput(&cfg, "machine0", configInputSourceForFile(providerSelectionCompiledDefault), true)
	recordConfigInput(&cfg, configInputGeneric, configInputEnvironment, true)
	RecordProviderFlagInputs(&cfg, false, "machine0")
	RecordProviderFlagInputs(&cfg, true, "apple-container", "apple-machine")
	for _, tc := range []struct {
		owner   configInputOwner
		sources []string
	}{
		{"machine0", []string{"user_config", "repo_config"}},
		{configInputGeneric, []string{"environment"}},
		{"apple-container", []string{"flag"}},
		{"apple-machine", []string{"flag"}},
	} {
		got := cfg.inputProvenance.summary(tc.owner)
		if got.state != "present" || got.complete || !reflect.DeepEqual(got.sources, tc.sources) {
			t.Fatalf("%s: %#v", tc.owner, got)
		}
	}
	if got := cfg.inputProvenance.summary("tart"); got.state != "unknown" {
		t.Fatalf("unobserved owner: %#v", got)
	}
}

func TestLocalContainerBaseConfig(t *testing.T) {
	cfg := baseConfig()
	_, _, _, _, _, image, _ := osImageDefaultProviderImages(cfg.OSImage)
	want := LocalContainerConfig{Runtime: "docker", Image: image, User: "crabbox", Network: "bridge"}
	if image == "" || !reflect.DeepEqual(cfg.LocalContainer, want) {
		t.Fatalf("compiled local-container defaults=%#v, want %#v", cfg.LocalContainer, want)
	}
	if cfg.localContainerImageExplicit || LocalContainerRuntimeExplicit(cfg) || LocalContainerWorkRootExplicit(cfg) {
		t.Fatal("compiled defaults marked explicit")
	}
}

func TestLocalContainerOrdinarySourceLayering(t *testing.T) {
	settings := func(text string, cpus int, enabled bool) LocalContainerConfig {
		return LocalContainerConfig{
			Runtime: text, Image: text, User: text, WorkRoot: text,
			CPUs: cpus, Memory: text, Network: text, DockerSocket: enabled, NoHostname: enabled,
		}
	}
	for _, source := range []string{"file", "env"} {
		for _, tc := range []struct {
			name, text, cpu, boolean, wantText string
			fileCPU, envCPU                    int
			startBool, wantBool                bool
		}{
			{"empty preserves", "", "", "", "prior", 5, 5, true, true},
			{"equal accepts", "prior", "5", "true", "prior", 5, 5, true, true},
			{"raw paths and false", "~/literal ", "7", "false", "~/literal ", 7, 7, true, false},
			{"whitespace and zero", " \t ", "0", "", " \t ", 5, 0, true, true},
			{"negative and true", "", "-2", "true", "prior", 5, -2, false, true},
			{"malformed preserves", "", "bad", "invalid", "prior", 5, 5, true, true},
			{"boolean on", "", "", " ON ", "prior", 5, 5, false, true},
			{"boolean off", "", "", " Off ", "prior", 5, 5, true, false},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				clearConfigEnv(t)
				for _, key := range []string{"CRABBOX_PROVIDER", "CRABBOX_OS", "CRABBOX_WORK_ROOT", "CRABBOX_USER", "CRABBOX_SSH_USER"} {
					t.Setenv(key, "")
				}
				cfg := baseConfig()
				cfg.Provider = "unselected-config-test"
				cfg.SSHUser, cfg.WorkRoot = "generic-user", "generic-root"
				cfg.LocalContainer = settings("prior", 5, tc.startBool)
				list := []string{"inert-list-value"}
				metadata := map[string]string{"inert-key": "inert-value"}
				cfg.LocalContainer.Volumes, cfg.LocalContainer.CheckpointMetadata = list, metadata
				wantCPU := tc.fileCPU
				if source == "file" {
					cpu, _ := strconv.Atoi(tc.cpu)
					var boolean *bool
					if tc.boolean != "" && tc.boolean != "invalid" {
						value := tc.wantBool
						boolean = &value
					}
					if err := applyFileConfig(&cfg, fileConfig{LocalContainer: &fileLocalContainerConfig{
						Runtime: tc.text, Image: tc.text, User: tc.text, WorkRoot: tc.text,
						CPUs: cpu, Memory: tc.text, Network: tc.text, DockerSocket: boolean, NoHostname: boolean,
					}}); err != nil {
						t.Fatal(err)
					}
				} else {
					wantCPU = tc.envCPU
					for _, key := range []string{"RUNTIME", "IMAGE", "USER", "WORK_ROOT", "MEMORY", "NETWORK"} {
						t.Setenv("CRABBOX_LOCAL_CONTAINER_"+key, tc.text)
					}
					t.Setenv("CRABBOX_LOCAL_CONTAINER_CPUS", tc.cpu)
					t.Setenv("CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET", tc.boolean)
					t.Setenv("CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME", tc.boolean)
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				}
				want := settings(tc.wantText, wantCPU, tc.wantBool)
				want.Volumes = []string{"inert-list-value"}
				want.CheckpointMetadata = map[string]string{"inert-key": "inert-value"}
				if !reflect.DeepEqual(cfg.LocalContainer, want) {
					t.Fatalf("source settings=%#v, want %#v", cfg.LocalContainer, want)
				}
				accepted := tc.text != ""
				if cfg.localContainerImageExplicit != accepted || LocalContainerRuntimeExplicit(cfg) != accepted || LocalContainerWorkRootExplicit(cfg) != accepted {
					t.Fatal("source markers did not track acceptance")
				}
				if cfg.SSHUser != "generic-user" || cfg.WorkRoot != "generic-root" || IsWorkRootExplicit(&cfg) {
					t.Fatal("provider source changed generic user/root or its marker")
				}
				if &cfg.LocalContainer.Volumes[0] != &list[0] {
					t.Fatal("source replaced the runtime list")
				}
				metadata["inert-key"] = "updated-inert-value"
				if cfg.LocalContainer.CheckpointMetadata["inert-key"] != "updated-inert-value" {
					t.Fatal("source replaced the runtime map")
				}
			})
		}
	}
}

func isolateTestUserDirs(t *testing.T) testutil.UserDirs {
	t.Helper()
	return testutil.IsolateUserDirs(t)
}

func TestClearConfigEnvPreservesAbsence(t *testing.T) {
	const key = "CRABBOX_NOMAD_DATACENTERS"
	t.Setenv(key, "prior")
	t.Run("absent is not an explicit clear", func(t *testing.T) {
		clearConfigEnv(t)
		if _, present := os.LookupEnv(key); present {
			t.Fatal("environment reset must unset presence-aware inputs")
		}
		t.Setenv(key, "")
		if value, present := os.LookupEnv(key); !present || value != "" {
			t.Fatal("an explicit empty input must remain distinguishable")
		}
	})
	if os.Getenv(key) != "prior" {
		t.Fatal("test cleanup did not restore the previous environment")
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	isolateTestUserDirs(t)
	for _, key := range []string{
		"CRABBOX_ENV_ALLOW",
		"CRABBOX_RESULTS_JUNIT",
		"CRABBOX_RESULTS_AUTO",
		"CRABBOX_RESULTS_FAIL_ON_FAILURES",
		"CRABBOX_PREFLIGHT_TOOLS",
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_MODE",
		"CRABBOX_COORDINATOR_AUTO_WEBVNC",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_TOKEN_COMMAND",
		"CRABBOX_WEBVNC_AGENT_BASE_URL",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
		"CRABBOX_ADMIN_TOKEN",
		"CRABBOX_HOST_ID",
		"CRABBOX_AWS_MAC_HOST_ID",
		"CRABBOX_AWS_LAMBDA_MICROVM_IMAGE",
		"CRABBOX_AWS_LAMBDA_MICROVM_IMAGE_VERSION",
		"CRABBOX_AWS_LAMBDA_MICROVM_EXECUTION_ROLE_ARN",
		"CRABBOX_AWS_LAMBDA_MICROVM_WORKDIR",
		"CRABBOX_AWS_LAMBDA_MICROVM_INGRESS_CONNECTORS",
		"CRABBOX_AWS_LAMBDA_MICROVM_EGRESS_CONNECTORS",
		"CRABBOX_AWS_LAMBDA_MICROVM_FORGET_MISSING",
		"CRABBOX_NETWORK",
		"CRABBOX_TAILSCALE",
		"CRABBOX_TAILSCALE_TAGS",
		"CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE",
		"CRABBOX_TAILSCALE_AUTH_KEY_ENV",
		"CRABBOX_TAILSCALE_AUTH_KEY",
		"CRABBOX_TAILSCALE_EXIT_NODE",
		"CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS",
		"CRABBOX_ACCESS_CLIENT_ID",
		"CRABBOX_ACCESS_CLIENT_SECRET",
		"CRABBOX_ACCESS_TOKEN",
		"CF_ACCESS_CLIENT_ID",
		"CF_ACCESS_CLIENT_SECRET",
		"CF_ACCESS_TOKEN",
		"CRABBOX_AZURE_BACKEND",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS",
		"CRABBOX_GCP_PROJECT",
		"GOOGLE_CLOUD_PROJECT",
		"GCP_PROJECT_ID",
		"CRABBOX_GCP_ZONE",
		"CRABBOX_GCP_IMAGE",
		"CRABBOX_GCP_NETWORK",
		"CRABBOX_GCP_SUBNET",
		"CRABBOX_GCP_TAGS",
		"CRABBOX_GCP_SSH_CIDRS",
		"CRABBOX_GCP_ROOT_GB",
		"CRABBOX_GCP_SERVICE_ACCOUNT",
		"CRABBOX_DIGITALOCEAN_REGION",
		"CRABBOX_DIGITALOCEAN_IMAGE",
		"CRABBOX_DIGITALOCEAN_VPC",
		"CRABBOX_DIGITALOCEAN_SSH_CIDRS",
		"CRABBOX_VULTR_REGION",
		"CRABBOX_VULTR_OS",
		"CRABBOX_VULTR_IMAGE",
		"CRABBOX_VULTR_SNAPSHOT",
		"CRABBOX_VULTR_FIREWALL_GROUP",
		"CRABBOX_VULTR_VPC_IDS",
		"CRABBOX_VULTR_SSH_CIDRS",
		"CRABBOX_VULTR_USER_SCHEME",
		"CRABBOX_LINODE_REGION",
		"CRABBOX_LINODE_IMAGE",
		"CRABBOX_LINODE_TYPE",
		"CRABBOX_LINODE_FIREWALL",
		"CRABBOX_LINODE_SSH_CIDRS",
		"CRABBOX_LAMBDA_REGION",
		"CRABBOX_LAMBDA_TYPE",
		"CRABBOX_LAMBDA_IMAGE",
		"CRABBOX_LAMBDA_IMAGE_FAMILY",
		"CRABBOX_LAMBDA_FIREWALL_RULESET",
		"CRABBOX_LAMBDA_SSH_CIDRS",
		"CRABBOX_LAMBDA_FILESYSTEM_NAMES",
		"CRABBOX_LAMBDA_FILESYSTEM_MOUNTS",
		"LAMBDA_API_KEY",
		"CRABBOX_NEBIUS_CLI",
		"CRABBOX_NEBIUS_PROFILE",
		"CRABBOX_NEBIUS_PARENT_ID",
		"CRABBOX_NEBIUS_SUBNET_ID",
		"CRABBOX_NEBIUS_PLATFORM",
		"CRABBOX_NEBIUS_PRESET",
		"CRABBOX_NEBIUS_IMAGE_FAMILY",
		"CRABBOX_NEBIUS_DISK_TYPE",
		"CRABBOX_NEBIUS_DISK_SIZE_GIB",
		"CRABBOX_NEBIUS_USER",
		"CRABBOX_NEBIUS_PUBLIC_IP",
		"CRABBOX_NEBIUS_SECURITY_GROUP_IDS",
		"CRABBOX_NEBIUS_SERVICE_ACCOUNT_ID",
		"CRABBOX_NEBIUS_RECOVERY_POLICY",
		"OVH_ENDPOINT",
		"OVH_APPLICATION_KEY",
		"OVH_APPLICATION_SECRET",
		"OVH_CONSUMER_KEY",
		"CRABBOX_OVH_PROJECT_ID",
		"CRABBOX_OVH_REGION",
		"CRABBOX_OVH_IMAGE",
		"CRABBOX_OVH_FLAVOR",
		"CRABBOX_SCALEWAY_REGION",
		"CRABBOX_SCALEWAY_ZONE",
		"CRABBOX_SCALEWAY_IMAGE",
		"CRABBOX_SCALEWAY_TYPE",
		"CRABBOX_SCALEWAY_PROJECT_ID",
		"CRABBOX_SCALEWAY_ORGANIZATION_ID",
		"CRABBOX_SCALEWAY_SECURITY_GROUP",
		"CRABBOX_SCALEWAY_SSH_CIDRS",
		"SCW_ACCESS_KEY",
		"SCW_SECRET_KEY",
		"SCW_DEFAULT_ORGANIZATION_ID",
		"SCW_DEFAULT_PROJECT_ID",
		"SCW_DEFAULT_REGION",
		"SCW_DEFAULT_ZONE",
		"SCW_PROFILE",
		"SCW_CONFIG_PATH",
		"CRABBOX_DAYTONA_API_KEY",
		"DAYTONA_API_KEY",
		"CRABBOX_DAYTONA_JWT_TOKEN",
		"DAYTONA_JWT_TOKEN",
		"CRABBOX_DAYTONA_ORGANIZATION_ID",
		"DAYTONA_ORGANIZATION_ID",
		"CRABBOX_DAYTONA_API_URL",
		"DAYTONA_API_URL",
		"CRABBOX_DAYTONA_SNAPSHOT",
		"DAYTONA_SNAPSHOT",
		"CRABBOX_DAYTONA_TARGET",
		"DAYTONA_TARGET",
		"CRABBOX_DAYTONA_USER",
		"CRABBOX_DAYTONA_WORK_ROOT",
		"CRABBOX_DAYTONA_SSH_GATEWAY_HOST",
		"CRABBOX_DAYTONA_SSH_ACCESS_MINUTES",
		"CRABBOX_E2B_API_KEY",
		"E2B_API_KEY",
		"CRABBOX_E2B_API_URL",
		"E2B_API_URL",
		"CRABBOX_E2B_DOMAIN",
		"E2B_DOMAIN",
		"CRABBOX_E2B_TEMPLATE",
		"CRABBOX_E2B_WORKDIR",
		"CRABBOX_E2B_USER",
		"CRABBOX_CODESANDBOX_API_KEY",
		"CSB_API_KEY",
		"CRABBOX_CODESANDBOX_TEMPLATE_ID",
		"CRABBOX_CODESANDBOX_WORKDIR",
		"CRABBOX_CODESANDBOX_VM_TIER",
		"CRABBOX_CODESANDBOX_PRIVACY",
		"CRABBOX_CODESANDBOX_HIBERNATION_TIMEOUT_SECS",
		"CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_HTTP",
		"CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_WEBSOCKET",
		"CRABBOX_CODESANDBOX_BRIDGE_COMMAND",
		"CRABBOX_CODESANDBOX_SDK_PACKAGE",
		"CRABBOX_CODESANDBOX_DOCTOR_LIST_LIMIT",
		"CRABBOX_CODESANDBOX_OPERATION_TIMEOUT_SECS",
		"CRABBOX_OPENSANDBOX_API_URL",
		"OPEN_SANDBOX_API_URL",
		"CRABBOX_OPENSANDBOX_API_KEY",
		"OPEN_SANDBOX_API_KEY",
		"CRABBOX_OPENSANDBOX_IMAGE",
		"CRABBOX_OPENSANDBOX_WORKDIR",
		"CRABBOX_OPENSANDBOX_CPU",
		"CRABBOX_OPENSANDBOX_MEMORY",
		"CRABBOX_OPENSANDBOX_TIMEOUT_SECS",
		"CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS",
		"CRABBOX_OPENSANDBOX_PLATFORM_OS",
		"CRABBOX_OPENSANDBOX_PLATFORM_ARCH",
		"CRABBOX_OPENSANDBOX_SECURE_ACCESS",
		"CRABBOX_OPENSANDBOX_USE_SERVER_PROXY",
		"CRABBOX_NOMAD_ADDR",
		"NOMAD_ADDR",
		"CRABBOX_NOMAD_REGION",
		"NOMAD_REGION",
		"CRABBOX_NOMAD_NAMESPACE",
		"NOMAD_NAMESPACE",
		"NOMAD_TOKEN",
		"CRABBOX_NOMAD_TOKEN_ENV",
		"CRABBOX_NOMAD_CA_CERT",
		"NOMAD_CACERT",
		"CRABBOX_NOMAD_CA_PATH",
		"NOMAD_CAPATH",
		"CRABBOX_NOMAD_CLIENT_CERT",
		"NOMAD_CLIENT_CERT",
		"CRABBOX_NOMAD_CLIENT_KEY",
		"NOMAD_CLIENT_KEY",
		"CRABBOX_NOMAD_TLS_SERVER_NAME",
		"NOMAD_TLS_SERVER_NAME",
		"CRABBOX_NOMAD_SKIP_VERIFY",
		"NOMAD_SKIP_VERIFY",
		"CRABBOX_NOMAD_TASK",
		"CRABBOX_NOMAD_DRIVER",
		"CRABBOX_NOMAD_IMAGE",
		"CRABBOX_NOMAD_WORKDIR",
		"CRABBOX_NOMAD_JOBSPEC_TEMPLATE",
		"CRABBOX_NOMAD_NODE_POOL",
		"CRABBOX_NOMAD_DATACENTERS",
		"CRABBOX_NOMAD_CPU",
		"CRABBOX_NOMAD_MEMORY_MB",
		"CRABBOX_NOMAD_DISK_MB",
		"CRABBOX_NOMAD_ALLOC_READY_TIMEOUT",
		"CRABBOX_NOMAD_EVAL_TIMEOUT",
		"CRABBOX_NOMAD_EXEC_TIMEOUT_SECS",
		"CRABBOX_BLAXEL_API_KEY",
		"BL_API_KEY",
		"CRABBOX_BLAXEL_API_URL",
		"CRABBOX_BLAXEL_WORKSPACE",
		"BL_WORKSPACE",
		"CRABBOX_BLAXEL_REGION",
		"BL_REGION",
		"CRABBOX_BLAXEL_IMAGE",
		"CRABBOX_BLAXEL_MEMORY_MB",
		"CRABBOX_BLAXEL_TTL",
		"CRABBOX_BLAXEL_IDLE_TTL",
		"CRABBOX_BLAXEL_WORKDIR",
		"CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS",
		"CRABBOX_BLAXEL_FORGET_MISSING",
		"CRABBOX_SEALOS_DEVBOX_KUBECTL",
		"CRABBOX_SEALOS_DEVBOX_KUBECONFIG",
		"CRABBOX_SEALOS_DEVBOX_CONTEXT",
		"CRABBOX_SEALOS_DEVBOX_NAMESPACE",
		"CRABBOX_SEALOS_DEVBOX_IMAGE",
		"CRABBOX_SEALOS_DEVBOX_TEMPLATE_ID",
		"CRABBOX_SEALOS_DEVBOX_CPU",
		"CRABBOX_SEALOS_DEVBOX_MEMORY",
		"CRABBOX_SEALOS_DEVBOX_STORAGE_LIMIT",
		"CRABBOX_SEALOS_DEVBOX_NETWORK",
		"CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_HOST",
		"CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_PORT",
		"CRABBOX_SEALOS_DEVBOX_SSH_USER",
		"CRABBOX_SEALOS_DEVBOX_WORK_ROOT",
		"CRABBOX_SEALOS_DEVBOX_NODE_HOST",
		"CRABBOX_SEALOS_DEVBOX_DELETE_ON_RELEASE",
		"CRABBOX_AGENT_SANDBOX_KUBECONFIG",
		"CRABBOX_AGENT_SANDBOX_KUBECTL",
		"CRABBOX_AGENT_SANDBOX_CONTEXT",
		"CRABBOX_AGENT_SANDBOX_NAMESPACE",
		"CRABBOX_AGENT_SANDBOX_WARM_POOL",
		"CRABBOX_AGENT_SANDBOX_CONTAINER",
		"CRABBOX_AGENT_SANDBOX_WORKDIR",
		"CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT",
		"CRABBOX_AGENT_SANDBOX_POD_READY_TIMEOUT",
		"CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS",
		"CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE",
		"CRABBOX_AGENT_SANDBOX_FORGET_MISSING",
		"CRABBOX_VERCEL_SANDBOX_RUNTIME",
		"CRABBOX_VERCEL_SANDBOX_WORKDIR",
		"CRABBOX_VERCEL_SANDBOX_PROJECT_ID",
		"CRABBOX_VERCEL_SANDBOX_TEAM_ID",
		"CRABBOX_VERCEL_SANDBOX_SCOPE",
		"CRABBOX_VERCEL_SANDBOX_VCPUS",
		"CRABBOX_VERCEL_SANDBOX_TIMEOUT_SECS",
		"CRABBOX_VERCEL_SANDBOX_EXEC_TIMEOUT_SECS",
		"CRABBOX_VERCEL_SANDBOX_PERSISTENT",
		"CRABBOX_VERCEL_SANDBOX_SNAPSHOT",
		"CRABBOX_VERCEL_SANDBOX_SNAPSHOT_MODE",
		"CRABBOX_VERCEL_SANDBOX_NETWORK_POLICY",
		"CRABBOX_VERCEL_SANDBOX_NETWORK_ALLOW",
		"CRABBOX_VERCEL_SANDBOX_NETWORK_DENY",
		"CRABBOX_VERCEL_SANDBOX_PORTS",
		"CRABBOX_VERCEL_SANDBOX_FORGET_MISSING",
		"CRABBOX_SUPERSERVE_API_KEY",
		"SUPERSERVE_API_KEY",
		"CRABBOX_SUPERSERVE_BASE_URL",
		"SUPERSERVE_BASE_URL",
		"CRABBOX_SUPERSERVE_TEMPLATE",
		"CRABBOX_SUPERSERVE_SNAPSHOT",
		"CRABBOX_SUPERSERVE_WORKDIR",
		"CRABBOX_SUPERSERVE_TIMEOUT_SECS",
		"CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS",
		"CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT",
		"CRABBOX_SUPERSERVE_NETWORK_DENY_OUT",
		"CRABBOX_SUPERSERVE_FORGET_MISSING",
		"CRABBOX_ISLO_API_KEY",
		"ISLO_API_KEY",
		"CRABBOX_ISLO_BASE_URL",
		"ISLO_BASE_URL",
		"CRABBOX_ISLO_IMAGE",
		"CRABBOX_ISLO_WORKDIR",
		"CRABBOX_ISLO_GATEWAY_PROFILE",
		"CRABBOX_ISLO_SNAPSHOT_NAME",
		"CRABBOX_ISLO_VCPUS",
		"CRABBOX_ISLO_MEMORY_MB",
		"CRABBOX_ISLO_DISK_GB",
		"CRABBOX_FREESTYLE_API_KEY",
		"FREESTYLE_API_KEY",
		"CRABBOX_FREESTYLE_API_URL",
		"FREESTYLE_API_URL",
		"CRABBOX_FREESTYLE_WORKDIR",
		"CRABBOX_FREESTYLE_VCPUS",
		"CRABBOX_FREESTYLE_MEMORY_GB",
		"CRABBOX_TENKI_CLI",
		"TENKI_CLI",
		"CRABBOX_TENKI_ENDPOINT",
		"TENKI_ENDPOINT",
		"CRABBOX_TENKI_GATEWAY",
		"TENKI_GATEWAY",
		"CRABBOX_TENKI_WORKSPACE",
		"CRABBOX_TENKI_PROJECT",
		"CRABBOX_TENKI_IMAGE",
		"CRABBOX_TENKI_SNAPSHOT",
		"CRABBOX_TENKI_WORK_ROOT",
		"CRABBOX_TENKI_CPUS",
		"CRABBOX_TENKI_MEMORY_MB",
		"CRABBOX_TENKI_DISK_GB",
		"CRABBOX_TENSORLAKE_API_KEY",
		"TENSORLAKE_API_KEY",
		"CRABBOX_TENSORLAKE_API_URL",
		"TENSORLAKE_API_URL",
		"CRABBOX_TENSORLAKE_CLI",
		"CRABBOX_TENSORLAKE_IMAGE",
		"CRABBOX_TENSORLAKE_SNAPSHOT",
		"CRABBOX_TENSORLAKE_ORGANIZATION_ID",
		"TENSORLAKE_ORGANIZATION_ID",
		"CRABBOX_TENSORLAKE_PROJECT_ID",
		"TENSORLAKE_PROJECT_ID",
		"CRABBOX_TENSORLAKE_NAMESPACE",
		"INDEXIFY_NAMESPACE",
		"CRABBOX_TENSORLAKE_WORKDIR",
		"CRABBOX_TENSORLAKE_CPUS",
		"CRABBOX_TENSORLAKE_MEMORY_MB",
		"CRABBOX_TENSORLAKE_DISK_MB",
		"CRABBOX_TENSORLAKE_TIMEOUT_SECS",
		"CRABBOX_TENSORLAKE_NO_INTERNET",
		"CRABBOX_CUA_API_URL",
		"CUA_BASE_URL",
		"CRABBOX_CUA_IMAGE",
		"CRABBOX_CUA_KIND",
		"CRABBOX_CUA_REGION",
		"CRABBOX_CUA_WORKDIR",
		"CRABBOX_CUA_VCPUS",
		"CRABBOX_CUA_MEMORY_MB",
		"CRABBOX_CUA_DISK_GB",
		"CRABBOX_CUA_STARTUP_TIMEOUT_SECS",
		"CRABBOX_CUA_EXEC_TIMEOUT_SECS",
		"CRABBOX_CUA_BRIDGE_COMMAND",
		"CRABBOX_CUA_SDK_PACKAGE",
		"CRABBOX_CUA_SDK_IMPORT",
		"CRABBOX_CUA_SDK_FALLBACK_IMPORT",
		"CRABBOX_DOCKER_SANDBOX_CLI",
		"CRABBOX_DOCKER_SANDBOX_AGENT",
		"CRABBOX_DOCKER_SANDBOX_TEMPLATE",
		"CRABBOX_DOCKER_SANDBOX_CPUS",
		"CRABBOX_DOCKER_SANDBOX_MEMORY",
		"CRABBOX_DOCKER_SANDBOX_CLONE",
		"CRABBOX_DOCKER_SANDBOX_WORKDIR",
		"CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES",
		"CRABBOX_DOCKER_SANDBOX_MCP",
		"CRABBOX_DOCKER_SANDBOX_KIT",
		"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI",
		"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS",
		"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG",
		"CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL",
		"CLOUD_RUN_SANDBOX_URL",
		"CRABBOX_CLOUD_RUN_SANDBOX_CLI",
		"CLOUD_RUN_SANDBOX_BINARY",
		"CRABBOX_CLOUD_RUN_SANDBOX_WORKDIR",
		"CRABBOX_CLOUD_RUN_SANDBOX_ALLOW_EGRESS",
		"CRABBOX_CLOUD_RUN_SANDBOX_WRITE",
		"CRABBOX_CLOUD_RUN_SANDBOX_ROOTFS",
		"CRABBOX_SMOLVM_API_KEY",
		"SMOLMACHINES_API_KEY",
		"SMK_API_KEY",
		"CRABBOX_SMOLVM_BASE_URL",
		"CRABBOX_SMOLVM_IMAGE",
		"CRABBOX_SMOLVM_WORKDIR",
		"CRABBOX_SMOLVM_CPUS",
		"CRABBOX_SMOLVM_MEMORY_MB",
		"CRABBOX_SMOLVM_NETWORK",
		"CRABBOX_SMOLVM_KEEP",
		"CRABBOX_ASCII_BOX_API_KEY",
		"ASCII_BOX_API_KEY",
		"CRABBOX_ASCII_BOX_BASE_URL",
		"ASCII_BOX_BASE_URL",
		"CRABBOX_ASCII_BOX_CLI",
		"BOX_CLI",
		"CRABBOX_ASCII_BOX_WORKDIR",
		"CRABBOX_APPLE_CONTAINER_CLI",
		"CRABBOX_APPLE_CONTAINER_IMAGE",
		"CRABBOX_APPLE_CONTAINER_USER",
		"CRABBOX_APPLE_CONTAINER_WORK_ROOT",
		"CRABBOX_APPLE_CONTAINER_CPUS",
		"CRABBOX_APPLE_CONTAINER_MEMORY",
		"CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS",
		"CRABBOX_APPLE_VM_HELPER",
		"CRABBOX_APPLE_VM_IMAGE",
		"CRABBOX_APPLE_VM_IMAGE_SHA256",
		"CRABBOX_APPLE_VM_USER",
		"CRABBOX_APPLE_VM_WORK_ROOT",
		"CRABBOX_APPLE_VM_CPUS",
		"CRABBOX_APPLE_VM_MEMORY",
		"CRABBOX_APPLE_VM_DISK",
		"CRABBOX_APPLE_VZ_HELPER",
		"CRABBOX_APPLE_VZ_IMAGE",
		"CRABBOX_APPLE_VZ_IMAGE_SHA256",
		"CRABBOX_APPLE_VZ_USER",
		"CRABBOX_APPLE_VZ_WORK_ROOT",
		"CRABBOX_APPLE_VZ_CPUS",
		"CRABBOX_APPLE_VZ_MEMORY",
		"CRABBOX_APPLE_VZ_DISK",
		"CRABBOX_MULTIPASS_CLI",
		"CRABBOX_MULTIPASS_IMAGE",
		"CRABBOX_MULTIPASS_USER",
		"CRABBOX_MULTIPASS_WORK_ROOT",
		"CRABBOX_MULTIPASS_CPUS",
		"CRABBOX_MULTIPASS_MEMORY",
		"CRABBOX_MULTIPASS_DISK",
		"CRABBOX_MULTIPASS_LAUNCH_TIMEOUT",
		"CRABBOX_WINDOWS_SANDBOX_WORKDIR",
		"CRABBOX_WINDOWS_SANDBOX_TEMP_ROOT",
		"CRABBOX_WINDOWS_SANDBOX_NETWORKING",
		"CRABBOX_WINDOWS_SANDBOX_VGPU",
		"CRABBOX_WINDOWS_SANDBOX_CLIPBOARD",
		"CRABBOX_WINDOWS_SANDBOX_PROTECTED_CLIENT",
		"CRABBOX_WINDOWS_SANDBOX_AUDIO_INPUT",
		"CRABBOX_WINDOWS_SANDBOX_VIDEO_INPUT",
		"CRABBOX_WINDOWS_SANDBOX_PRINTER_REDIRECTION",
		"CRABBOX_WINDOWS_SANDBOX_MEMORY_MB",
		"CRABBOX_WANDB_API_KEY",
		"WANDB_API_KEY",
		"CRABBOX_WANDB_DEFAULT_IMAGE",
		"WANDB_DEFAULT_IMAGE",
		"CRABBOX_WANDB_MAX_LIFETIME_SECONDS",
		"WANDB_MAX_LIFETIME_SECONDS",
		"CRABBOX_CLOUDFLARE_RUNNER_URL",
		"CRABBOX_CLOUDFLARE_RUNNER_TOKEN",
		"CRABBOX_CLOUDFLARE_WORKDIR",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_LOADER_URL",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_DATE",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CACHE_MODE",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_EGRESS",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_SUBREQUESTS",
		"CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TIMEOUT_SECS",
		"CRABBOX_SEMAPHORE_HOST",
		"SEMAPHORE_HOST",
		"CRABBOX_SEMAPHORE_TOKEN",
		"SEMAPHORE_API_TOKEN",
		"CRABBOX_SEMAPHORE_PROJECT",
		"SEMAPHORE_PROJECT",
		"CRABBOX_SEMAPHORE_MACHINE",
		"CRABBOX_SEMAPHORE_OS_IMAGE",
		"CRABBOX_SEMAPHORE_IDLE_TIMEOUT",
		"CRABBOX_SPRITES_TOKEN",
		"SPRITES_TOKEN",
		"SPRITE_TOKEN",
		"SETUP_SPRITE_TOKEN",
		"CRABBOX_SPRITES_API_URL",
		"SPRITES_API_URL",
		"CRABBOX_SPRITES_WORK_ROOT",
		"CRABBOX_LOCAL_CONTAINER_RUNTIME",
		"CRABBOX_LOCAL_CONTAINER_IMAGE",
		"CRABBOX_LOCAL_CONTAINER_USER",
		"CRABBOX_LOCAL_CONTAINER_WORK_ROOT",
		"CRABBOX_LOCAL_CONTAINER_CPUS",
		"CRABBOX_LOCAL_CONTAINER_MEMORY",
		"CRABBOX_LOCAL_CONTAINER_NETWORK",
		"CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET",
		"CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME",
		"CRABBOX_NAMESPACE_IMAGE",
		"CRABBOX_NAMESPACE_SIZE",
		"CRABBOX_NAMESPACE_REPOSITORY",
		"CRABBOX_NAMESPACE_SITE",
		"CRABBOX_NAMESPACE_VOLUME_SIZE_GB",
		"CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT",
		"CRABBOX_NAMESPACE_WORK_ROOT",
		"CRABBOX_NAMESPACE_DELETE_ON_RELEASE",
		"CRABBOX_CODER_CLI",
		"CRABBOX_CODER_TEMPLATE",
		"CRABBOX_CODER_PRESET",
		"CRABBOX_CODER_WORKSPACE_PREFIX",
		"CRABBOX_CODER_WORK_ROOT",
		"CRABBOX_CODER_DELETE_ON_RELEASE",
		"CRABBOX_CODER_WAIT",
		"CRABBOX_CODER_USE_PARAMETER_DEFAULTS",
		"CRABBOX_CODER_PARAMETERS",
		"CRABBOX_CODER_RICH_PARAMETER_FILE",
		"CRABBOX_MORPH_API_KEY",
		"MORPH_API_KEY",
		"CRABBOX_MORPH_API_URL",
		"CRABBOX_MORPH_SNAPSHOT",
		"CRABBOX_MORPH_SSH_GATEWAY_HOST",
		"CRABBOX_MORPH_WORK_ROOT",
		"CRABBOX_MORPH_DELETE_ON_RELEASE",
		"CRABBOX_MORPH_WAKE_ON_SSH",
		"CRABBOX_EXE_DEV_CONTROL_HOST",
		"EXE_DEV_CONTROL_HOST",
		"CRABBOX_EXE_DEV_IMAGE",
		"EXE_DEV_IMAGE",
		"CRABBOX_EXE_DEV_CPUS",
		"CRABBOX_EXE_DEV_MEMORY",
		"EXE_DEV_MEMORY",
		"CRABBOX_EXE_DEV_DISK",
		"EXE_DEV_DISK",
		"CRABBOX_EXE_DEV_COMMAND",
		"CRABBOX_EXE_DEV_USER",
		"CRABBOX_EXE_DEV_WORK_ROOT",
		"CRABBOX_EXE_DEV_NO_EMAIL",
		"CRABBOX_RAILWAY_API_TOKEN",
		"RAILWAY_API_TOKEN",
		"CRABBOX_RAILWAY_API_URL",
		"RAILWAY_API_URL",
		"CRABBOX_RAILWAY_PROJECT_ID",
		"RAILWAY_PROJECT_ID",
		"CRABBOX_RAILWAY_ENVIRONMENT_ID",
		"RAILWAY_ENVIRONMENT_ID",
		"CRABBOX_FASTAPI_CLOUD_TOKEN",
		"FASTAPI_CLOUD_TOKEN",
		"CRABBOX_FASTAPI_CLOUD_API_URL",
		"FASTAPI_CLOUD_API_URL",
		"CRABBOX_FASTAPI_CLOUD_APP_ID",
		"FASTAPI_CLOUD_APP_ID",
		"CRABBOX_FASTAPI_CLOUD_TEAM_ID",
		"FASTAPI_CLOUD_TEAM_ID",
		"RUNPOD_API_KEY",
		"CRABBOX_RUNPOD_API_KEY",
		"RUNPOD_API_URL",
		"CRABBOX_RUNPOD_API_URL",
		"RUNPOD_CLOUD_TYPE",
		"CRABBOX_RUNPOD_CLOUD_TYPE",
		"RUNPOD_INSTANCE_ID",
		"CRABBOX_RUNPOD_INSTANCE_ID",
		"RUNPOD_IMAGE",
		"CRABBOX_RUNPOD_IMAGE",
		"RUNPOD_TEMPLATE_ID",
		"CRABBOX_RUNPOD_TEMPLATE_ID",
		"CRABBOX_RUNPOD_DISK_GB",
		"CRABBOX_RUNPOD_USER",
		"CRABBOX_RUNPOD_WORK_ROOT",
		"CRABBOX_VAST_API_KEY",
		"VAST_API_KEY",
		"CRABBOX_VAST_API_URL",
		"VAST_API_URL",
		"CRABBOX_VAST_INSTANCE_TYPE",
		"CRABBOX_VAST_GPU_NAME",
		"CRABBOX_VAST_GPU_COUNT",
		"CRABBOX_VAST_IMAGE",
		"CRABBOX_VAST_TEMPLATE_ID",
		"CRABBOX_VAST_RUNTYPE",
		"CRABBOX_VAST_DISK_GB",
		"CRABBOX_VAST_MAX_DPH_TOTAL",
		"CRABBOX_VAST_MIN_RELIABILITY",
		"CRABBOX_VAST_ORDER",
		"CRABBOX_VAST_USER",
		"CRABBOX_VAST_WORK_ROOT",
		"CRABBOX_VAST_RELEASE_ACTION",
		"CRABBOX_NVIDIA_BREV_CLI",
		"CRABBOX_NVIDIA_BREV_ORG",
		"CRABBOX_NVIDIA_BREV_TYPE",
		"CRABBOX_NVIDIA_BREV_GPU_NAME",
		"CRABBOX_NVIDIA_BREV_PROVIDER",
		"CRABBOX_NVIDIA_BREV_MODE",
		"CRABBOX_NVIDIA_BREV_LAUNCHABLE",
		"CRABBOX_NVIDIA_BREV_STARTUP_SCRIPT",
		"CRABBOX_NVIDIA_BREV_RELEASE_ACTION",
		"CRABBOX_NVIDIA_BREV_TARGET",
		"CRABBOX_NVIDIA_BREV_USER",
		"CRABBOX_NVIDIA_BREV_WORK_ROOT",
		"CRABBOX_GITHUB_CODESPACES_API_URL",
		"CRABBOX_GITHUB_CODESPACES_GH_PATH",
		"CRABBOX_GITHUB_CODESPACES_REPO",
		"CRABBOX_GITHUB_CODESPACES_REF",
		"CRABBOX_GITHUB_CODESPACES_MACHINE",
		"CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH",
		"CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY",
		"CRABBOX_GITHUB_CODESPACES_GEO",
		"CRABBOX_GITHUB_CODESPACES_IDLE_TIMEOUT",
		"CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD",
		"CRABBOX_GITHUB_CODESPACES_DELETE_ON_RELEASE",
		"CRABBOX_GITHUB_CODESPACES_WORK_ROOT",
		"HOSTINGER_API_TOKEN",
		"CRABBOX_HOSTINGER_API_TOKEN",
		"HOSTINGER_API_URL",
		"CRABBOX_HOSTINGER_API_URL",
		"CRABBOX_HOSTINGER_ITEM_ID",
		"CRABBOX_HOSTINGER_PAYMENT_METHOD_ID",
		"CRABBOX_HOSTINGER_TEMPLATE_ID",
		"CRABBOX_HOSTINGER_DATA_CENTER_ID",
		"CRABBOX_HOSTINGER_HOSTNAME_PREFIX",
		"CRABBOX_HOSTINGER_USER",
		"CRABBOX_HOSTINGER_WORK_ROOT",
		"CRABBOX_HOSTINGER_ALLOW_PURCHASE",
		"CRABBOX_HOSTINGER_RELEASE_ACTION",
		"CRABBOX_EXTERNAL_IDEMPOTENT_LEASE_ID",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsloCreateDefaultsTrackExplicitConfigAndEnvironment(t *testing.T) {
	base := baseConfig()
	fromFile := base
	if err := applyFileConfig(&fromFile, fileConfig{Islo: &fileIsloConfig{
		Image:    base.Islo.Image,
		VCPUs:    base.Islo.VCPUs,
		MemoryMB: base.Islo.MemoryMB,
		DiskGB:   base.Islo.DiskGB,
	}}); err != nil {
		t.Fatal(err)
	}
	if !IsloImageExplicit(fromFile) || !IsloVCPUsExplicit(fromFile) || !IsloMemoryMBExplicit(fromFile) || !IsloDiskGBExplicit(fromFile) {
		t.Fatalf("file explicit markers missing: %#v", fromFile)
	}

	clearConfigEnv(t)
	t.Setenv("CRABBOX_ISLO_IMAGE", base.Islo.Image)
	t.Setenv("CRABBOX_ISLO_VCPUS", strconv.Itoa(base.Islo.VCPUs))
	t.Setenv("CRABBOX_ISLO_MEMORY_MB", strconv.Itoa(base.Islo.MemoryMB))
	t.Setenv("CRABBOX_ISLO_DISK_GB", strconv.Itoa(base.Islo.DiskGB))
	fromEnv := base
	if err := applyEnv(&fromEnv); err != nil {
		t.Fatal(err)
	}
	if !IsloImageExplicit(fromEnv) || !IsloVCPUsExplicit(fromEnv) || !IsloMemoryMBExplicit(fromEnv) || !IsloDiskGBExplicit(fromEnv) {
		t.Fatalf("environment explicit markers missing: %#v", fromEnv)
	}
}

func TestProviderExplicitMarkerHelpers(t *testing.T) {
	cfg := Config{
		SSHUser:  "alice",
		SSHKey:   "/keys/id_ed25519",
		SSHPort:  "2222",
		WorkRoot: "/workspace",
	}

	MarkIsloImageExplicit(&cfg)
	MarkIsloVCPUsExplicit(&cfg)
	MarkIsloMemoryMBExplicit(&cfg)
	MarkIsloDiskGBExplicit(&cfg)
	MarkAppleVMImageSHA256Explicit(&cfg)
	MarkAppleVMCPUsExplicit(&cfg)
	MarkAppleVMMemoryExplicit(&cfg)
	MarkAppleVMDiskExplicit(&cfg)
	MarkMultipassImageExplicit(&cfg)
	MarkTartImageExplicit(&cfg)
	MarkTartDiskExplicit(&cfg)
	MarkTartCPUsExplicit(&cfg)
	MarkTartMemoryExplicit(&cfg)
	MarkTargetExplicit(&cfg)
	MarkSSHUserExplicit(&cfg)
	MarkSSHKeyExplicit(&cfg)
	MarkSSHPortExplicit(&cfg)
	MarkWorkRootExplicit(&cfg)

	if !IsloImageExplicit(cfg) || !IsloVCPUsExplicit(cfg) || !IsloMemoryMBExplicit(cfg) || !IsloDiskGBExplicit(cfg) ||
		!cfg.appleVMImageSHA256Explicit || !AppleVMCPUsExplicit(cfg) || !AppleVMMemoryExplicit(cfg) || !AppleVMDiskExplicit(cfg) ||
		!cfg.multipassImageExplicit || !cfg.tartImageExplicit || !IsTartDiskExplicit(&cfg) ||
		!IsTartCPUsExplicit(&cfg) || !IsTartMemoryExplicit(&cfg) || !IsTargetExplicit(&cfg) ||
		!IsSSHUserExplicit(&cfg) || !IsSSHKeyExplicit(&cfg) || !IsSSHPortExplicit(&cfg) || !IsWorkRootExplicit(&cfg) {
		t.Fatalf("explicit marker API did not preserve every setting: %#v", cfg)
	}
}

func TestNvidiaBrevConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.NvidiaBrev.CLI != "brev" ||
		cfg.NvidiaBrev.GPUName != "A100" ||
		cfg.NvidiaBrev.Mode != "vm" ||
		cfg.NvidiaBrev.ReleaseAction != "delete" ||
		cfg.NvidiaBrev.Target != "container" ||
		cfg.NvidiaBrev.WorkRoot != "/tmp/crabbox" {
		t.Fatalf("nvidiaBrev defaults not applied: %#v", cfg.NvidiaBrev)
	}

	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "nvidia-brev",
		NvidiaBrev: &fileNvidiaBrevConfig{
			CLI:           "/opt/brev",
			Org:           "example-org",
			Type:          "gpu",
			GPUName:       "L40S",
			Provider:      "aws",
			Mode:          "vm",
			Launchable:    "pytorch",
			StartupScript: "setup.sh",
			ReleaseAction: "stop",
			Target:        "host",
			User:          "ubuntu",
			WorkRoot:      "/work/brev",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "nvidia-brev" ||
		cfg.NvidiaBrev.CLI != "/opt/brev" ||
		cfg.NvidiaBrev.Org != "example-org" ||
		cfg.NvidiaBrev.Type != "gpu" ||
		cfg.NvidiaBrev.GPUName != "L40S" ||
		cfg.NvidiaBrev.Provider != "aws" ||
		cfg.NvidiaBrev.Mode != "vm" ||
		cfg.NvidiaBrev.Launchable != "pytorch" ||
		cfg.NvidiaBrev.StartupScript != "setup.sh" ||
		cfg.NvidiaBrev.ReleaseAction != "stop" ||
		cfg.NvidiaBrev.Target != "host" ||
		cfg.NvidiaBrev.User != "ubuntu" ||
		cfg.NvidiaBrev.WorkRoot != "/work/brev" {
		t.Fatalf("file nvidiaBrev config not applied: %#v", cfg.NvidiaBrev)
	}
	if !DeleteOnReleaseExplicit(cfg, "nvidia-brev") {
		t.Fatal("file nvidiaBrev release action not marked explicit")
	}
	if !IsNvidiaBrevWorkRootExplicit(&cfg) {
		t.Fatal("file nvidiaBrev work root not marked explicit")
	}

	t.Setenv("CRABBOX_NVIDIA_BREV_CLI", "/usr/local/bin/brev")
	t.Setenv("CRABBOX_NVIDIA_BREV_ORG", "env-example-org")
	t.Setenv("CRABBOX_NVIDIA_BREV_TYPE", "cpu")
	t.Setenv("CRABBOX_NVIDIA_BREV_GPU_NAME", "A100")
	t.Setenv("CRABBOX_NVIDIA_BREV_PROVIDER", "gcp")
	t.Setenv("CRABBOX_NVIDIA_BREV_MODE", "vm")
	t.Setenv("CRABBOX_NVIDIA_BREV_LAUNCHABLE", "base")
	t.Setenv("CRABBOX_NVIDIA_BREV_STARTUP_SCRIPT", "env-setup.sh")
	t.Setenv("CRABBOX_NVIDIA_BREV_RELEASE_ACTION", "delete")
	t.Setenv("CRABBOX_NVIDIA_BREV_TARGET", "container")
	t.Setenv("CRABBOX_NVIDIA_BREV_USER", "runner")
	t.Setenv("CRABBOX_NVIDIA_BREV_WORK_ROOT", "/workspace/brev")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.NvidiaBrev.CLI != "/usr/local/bin/brev" ||
		cfg.NvidiaBrev.Org != "env-example-org" ||
		cfg.NvidiaBrev.Type != "cpu" ||
		cfg.NvidiaBrev.GPUName != "A100" ||
		cfg.NvidiaBrev.Provider != "gcp" ||
		cfg.NvidiaBrev.Mode != "vm" ||
		cfg.NvidiaBrev.Launchable != "base" ||
		cfg.NvidiaBrev.StartupScript != "env-setup.sh" ||
		cfg.NvidiaBrev.ReleaseAction != "delete" ||
		cfg.NvidiaBrev.Target != "container" ||
		cfg.NvidiaBrev.User != "runner" ||
		cfg.NvidiaBrev.WorkRoot != "/workspace/brev" {
		t.Fatalf("env nvidiaBrev config not applied: %#v", cfg.NvidiaBrev)
	}
	if !DeleteOnReleaseExplicit(cfg, "nvidia-brev") {
		t.Fatal("env nvidiaBrev release action not marked explicit")
	}
	if !IsNvidiaBrevWorkRootExplicit(&cfg) {
		t.Fatal("env nvidiaBrev work root not marked explicit")
	}
}

func TestGitHubCodespacesConfigDefaultsFileEnvAndShow(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.GitHubCodespaces.APIURL != "https://api.github.com" ||
		cfg.GitHubCodespaces.GHPath != "gh" ||
		cfg.GitHubCodespaces.Machine != "basicLinux32gb" ||
		cfg.GitHubCodespaces.IdleTimeout != 30*time.Minute ||
		cfg.GitHubCodespaces.RetentionPeriod != 7*24*time.Hour ||
		!cfg.GitHubCodespaces.DeleteOnRelease ||
		cfg.GitHubCodespaces.WorkRoot != "/workspaces/crabbox" {
		t.Fatalf("githubCodespaces defaults not applied: %#v", cfg.GitHubCodespaces)
	}

	deleteOnRelease := true
	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "github-codespaces",
		GitHubCodespaces: &fileGitHubCodespacesConfig{
			APIURL:           "https://api.github.example",
			GHPath:           "/opt/gh",
			Repo:             "example-org/my-app",
			Ref:              "main",
			Machine:          "standardLinux32gb",
			DevcontainerPath: ".devcontainer/devcontainer.json",
			WorkingDirectory: "/workspaces/my-app",
			Geo:              "UsWest",
			IdleTimeout:      "45m",
			RetentionPeriod:  "48h",
			DeleteOnRelease:  &deleteOnRelease,
			WorkRoot:         "/workspaces/my-app",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "github-codespaces" ||
		cfg.GitHubCodespaces.APIURL != "https://api.github.example" ||
		cfg.GitHubCodespaces.GHPath != "/opt/gh" ||
		cfg.GitHubCodespaces.Repo != "example-org/my-app" ||
		cfg.GitHubCodespaces.Ref != "main" ||
		cfg.GitHubCodespaces.Machine != "standardLinux32gb" ||
		cfg.GitHubCodespaces.DevcontainerPath != ".devcontainer/devcontainer.json" ||
		cfg.GitHubCodespaces.WorkingDirectory != "/workspaces/my-app" ||
		cfg.GitHubCodespaces.Geo != "UsWest" ||
		cfg.GitHubCodespaces.IdleTimeout != 45*time.Minute ||
		cfg.GitHubCodespaces.RetentionPeriod != 48*time.Hour ||
		!cfg.GitHubCodespaces.DeleteOnRelease ||
		cfg.GitHubCodespaces.WorkRoot != "/workspaces/my-app" {
		t.Fatalf("file githubCodespaces config not applied: %#v", cfg.GitHubCodespaces)
	}
	if !DeleteOnReleaseExplicit(cfg, "github-codespaces") {
		t.Fatal("file githubCodespaces delete-on-release not marked explicit")
	}
	if !GitHubCodespacesRetentionExplicit(cfg) {
		t.Fatal("file githubCodespaces retention period not marked explicit")
	}

	t.Setenv("CRABBOX_GITHUB_CODESPACES_API_URL", "https://api.env.example")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_GH_PATH", "/usr/local/bin/gh")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_REPO", "example-org/env-app")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_REF", "env-main")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_MACHINE", "premiumLinux")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH", ".devcontainer/env.json")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY", "/workspaces/env-app")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_GEO", "EuropeWest")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_IDLE_TIMEOUT", "1h")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD", "72h")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_DELETE_ON_RELEASE", "false")
	t.Setenv("CRABBOX_GITHUB_CODESPACES_WORK_ROOT", "/workspaces/env-app")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.APIURL != "https://api.env.example" ||
		cfg.GitHubCodespaces.GHPath != "/usr/local/bin/gh" ||
		cfg.GitHubCodespaces.Repo != "example-org/env-app" ||
		cfg.GitHubCodespaces.Ref != "env-main" ||
		cfg.GitHubCodespaces.Machine != "premiumLinux" ||
		cfg.GitHubCodespaces.DevcontainerPath != ".devcontainer/env.json" ||
		cfg.GitHubCodespaces.WorkingDirectory != "/workspaces/env-app" ||
		cfg.GitHubCodespaces.Geo != "EuropeWest" ||
		cfg.GitHubCodespaces.IdleTimeout != time.Hour ||
		cfg.GitHubCodespaces.RetentionPeriod != 72*time.Hour ||
		cfg.GitHubCodespaces.DeleteOnRelease ||
		cfg.GitHubCodespaces.WorkRoot != "/workspaces/env-app" {
		t.Fatalf("env githubCodespaces config not applied: %#v", cfg.GitHubCodespaces)
	}
	if !DeleteOnReleaseExplicit(cfg, "github-codespaces") {
		t.Fatal("env githubCodespaces delete-on-release not marked explicit")
	}
	if !GitHubCodespacesRetentionExplicit(cfg) {
		t.Fatal("env githubCodespaces retention period not marked explicit")
	}

	view := configShowView(cfg)["githubCodespaces"].(map[string]any)
	if view["auth"] != "gh" {
		t.Fatalf("auth=%#v", view["auth"])
	}
	if _, ok := view["token"]; ok {
		t.Fatalf("config show exposed token key: %#v", view)
	}
}

func TestGitHubCodespacesConfigAcceptsExplicitZeroRetention(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "github-codespaces",
		GitHubCodespaces: &fileGitHubCodespacesConfig{
			RetentionPeriod: "0s",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.RetentionPeriod != 0 || !GitHubCodespacesRetentionExplicit(cfg) {
		t.Fatalf("file retention=%s explicit=%t", cfg.GitHubCodespaces.RetentionPeriod, GitHubCodespacesRetentionExplicit(cfg))
	}

	cfg = baseConfig()
	t.Setenv("CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD", "0s")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.RetentionPeriod != 0 || !GitHubCodespacesRetentionExplicit(cfg) {
		t.Fatalf("env retention=%s explicit=%t", cfg.GitHubCodespaces.RetentionPeriod, GitHubCodespacesRetentionExplicit(cfg))
	}
}

func TestGitHubCodespacesUntrustedConfigCannotRedirectOrChangeReleasePolicy(t *testing.T) {
	cfg := baseConfig()
	cfg.GitHubCodespaces.APIURL = "https://api.trusted.example"
	cfg.GitHubCodespaces.GHPath = "/trusted/gh"
	cfg.GitHubCodespaces.Repo = "trusted-org/trusted-app"
	cfg.GitHubCodespaces.IdleTimeout = 45 * time.Minute
	cfg.GitHubCodespaces.RetentionPeriod = 24 * time.Hour
	untrustedRetain := false
	if err := applyFileConfigWithTrust(&cfg, fileConfig{GitHubCodespaces: &fileGitHubCodespacesConfig{
		APIURL:          "https://api.untrusted.example",
		GHPath:          "./payload",
		Repo:            "attacker-org/payload",
		IdleTimeout:     "24h",
		RetentionPeriod: "720h",
		DeleteOnRelease: &untrustedRetain,
	}}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.APIURL != "https://api.trusted.example" || cfg.GitHubCodespaces.GHPath != "/trusted/gh" {
		t.Fatalf("untrusted redirect applied: %#v", cfg.GitHubCodespaces)
	}
	if cfg.GitHubCodespaces.Repo != "trusted-org/trusted-app" {
		t.Fatalf("untrusted repo redirect applied: %#v", cfg.GitHubCodespaces)
	}
	if cfg.GitHubCodespaces.IdleTimeout != 45*time.Minute || cfg.GitHubCodespaces.RetentionPeriod != 24*time.Hour {
		t.Fatalf("untrusted retention periods applied: %#v", cfg.GitHubCodespaces)
	}
	if !cfg.GitHubCodespaces.DeleteOnRelease || DeleteOnReleaseExplicit(cfg, "github-codespaces") {
		t.Fatalf("untrusted retention policy applied: %#v", cfg.GitHubCodespaces)
	}

	trustedRetain := false
	if err := applyFileConfigWithTrust(&cfg, fileConfig{GitHubCodespaces: &fileGitHubCodespacesConfig{
		IdleTimeout:     "1h",
		RetentionPeriod: "48h",
		DeleteOnRelease: &trustedRetain,
	}}, true); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.DeleteOnRelease || !DeleteOnReleaseExplicit(cfg, "github-codespaces") {
		t.Fatalf("trusted retention policy not applied: %#v", cfg.GitHubCodespaces)
	}
	if cfg.GitHubCodespaces.IdleTimeout != time.Hour || cfg.GitHubCodespaces.RetentionPeriod != 48*time.Hour {
		t.Fatalf("trusted retention periods not applied: %#v", cfg.GitHubCodespaces)
	}

	untrustedDelete := true
	if err := applyFileConfigWithTrust(&cfg, fileConfig{GitHubCodespaces: &fileGitHubCodespacesConfig{
		DeleteOnRelease: &untrustedDelete,
	}}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.DeleteOnRelease || !DeleteOnReleaseExplicit(cfg, "github-codespaces") {
		t.Fatalf("untrusted deletion policy replaced trusted retention: %#v", cfg.GitHubCodespaces)
	}
}

func TestNvidiaBrevUntrustedConfigCannotRedirectCLI(t *testing.T) {
	cfg := baseConfig()
	cfg.NvidiaBrev.CLI = "/trusted/brev"
	cfg.NvidiaBrev.StartupScript = "echo trusted"
	file := fileConfig{NvidiaBrev: &fileNvidiaBrevConfig{
		CLI:           "./payload",
		GPUName:       "L40S",
		StartupScript: "@/etc/passwd",
	}}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.NvidiaBrev.CLI != "/trusted/brev" {
		t.Fatalf("untrusted CLI override applied: %q", cfg.NvidiaBrev.CLI)
	}
	if cfg.NvidiaBrev.GPUName != "L40S" {
		t.Fatalf("safe untrusted setting not applied: %#v", cfg.NvidiaBrev)
	}
	if cfg.NvidiaBrev.StartupScript != "echo trusted" {
		t.Fatalf("untrusted startup script file applied: %q", cfg.NvidiaBrev.StartupScript)
	}

	file.NvidiaBrev.StartupScript = "pip install torch"
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.NvidiaBrev.StartupScript != "pip install torch" {
		t.Fatalf("inline untrusted startup script not applied: %q", cfg.NvidiaBrev.StartupScript)
	}
}

func TestLocalContainerWorkRootExplicitSources(t *testing.T) {
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, fileConfig{
		LocalContainer: &fileLocalContainerConfig{
			WorkRoot: "/workspace/file",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.LocalContainer.WorkRoot != "/workspace/file" {
		t.Fatalf("file local-container work root=%q", cfg.LocalContainer.WorkRoot)
	}
	if !LocalContainerWorkRootExplicit(cfg) {
		t.Fatal("file local-container work root not marked explicit")
	}

	cfg = baseConfig()
	t.Setenv("CRABBOX_LOCAL_CONTAINER_WORK_ROOT", "/workspace/env")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.LocalContainer.WorkRoot != "/workspace/env" {
		t.Fatalf("env local-container work root=%q", cfg.LocalContainer.WorkRoot)
	}
	if !LocalContainerWorkRootExplicit(cfg) {
		t.Fatal("env local-container work root not marked explicit")
	}
}

func TestContainerExplicitMarkerHelpers(t *testing.T) {
	var cfg Config
	MarkLocalContainerImageExplicit(&cfg)
	MarkLocalContainerRuntimeExplicit(&cfg)
	MarkLocalContainerWorkRootExplicit(&cfg)
	MarkAppleContainerImageExplicit(&cfg)
	if !cfg.localContainerImageExplicit || !LocalContainerRuntimeExplicit(cfg) ||
		!LocalContainerWorkRootExplicit(cfg) || !AppleContainerImageExplicit(cfg) {
		t.Fatalf("container explicit markers were not retained: %+v", cfg)
	}

	cfg.appleVMImageSHA256Explicit = true
	MarkAppleVMImageExplicit(&cfg)
	if !AppleVMImageExplicit(cfg) || cfg.appleVMImageSHA256Explicit {
		t.Fatalf("Apple VZ image marker did not invalidate the prior digest marker: %+v", cfg)
	}
}

func TestEffectiveNvidiaBrevWorkRootDoesNotInheritAnotherProviderDefault(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "xcp-ng"
	cfg.WorkRoot = "/home/vm/crabbox"
	if got := EffectiveNvidiaBrevWorkRoot(cfg); got != "/tmp/crabbox" {
		t.Fatalf("effective nvidia-brev work root=%q", got)
	}
}

func TestExplicitProviderWorkRootResolution(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		savedGeneric, currentGeneric string
		vastRoot, brevRoot           string
		vastMarked, brevMarked       bool
		wantVast, wantBrev           string
	}{
		{"no saved generic", "", "/generic/current", "", "/tmp/crabbox", false, false, "/work/crabbox", "/tmp/crabbox"},
		{"no saved generic reversed roots", "", "", "/work/crabbox", "", false, false, "/work/crabbox", "/tmp/crabbox"},
		{"saved generic snapshot", "/generic/saved", "/generic/current", "", "/tmp/crabbox", false, false, "/generic/saved", "/generic/saved"},
		{"saved generic after current cleared", "/generic/saved", "", "/work/crabbox", "", false, false, "/generic/saved", "/generic/saved"},
		{"explicit empty vast", "/generic/saved", "/generic/current", "", "/tmp/crabbox", true, false, "/work/crabbox", "/generic/saved"},
		{"explicit empty brev", "/generic/saved", "/generic/current", "/work/crabbox", "", false, true, "/generic/saved", "/tmp/crabbox"},
		{"explicit default vast", "/generic/saved", "/generic/current", "/work/crabbox", "", true, false, "/work/crabbox", "/generic/saved"},
		{"explicit default brev", "/generic/saved", "/generic/current", "", "/tmp/crabbox", false, true, "/generic/saved", "/tmp/crabbox"},
		{"unmarked custom roots", "/generic/saved", "/generic/current", "/vast/custom", "/brev/custom", false, false, "/vast/custom", "/brev/custom"},
		{"marked custom roots", "/generic/saved", "/generic/current", "/vast/custom", "/brev/custom", true, true, "/vast/custom", "/brev/custom"},
		{"padded provider defaults", "/generic/saved", "/generic/current", " /work/crabbox\t", "\t/tmp/crabbox ", false, false, " /work/crabbox\t", "\t/tmp/crabbox "},
		{"whitespace provider roots", "/generic/saved", "/generic/current", " \t ", "\t ", true, false, " \t ", "\t "},
		{"saved generic looks like vast default", "/work/crabbox", "/generic/current", "", "/tmp/crabbox", false, false, "/work/crabbox", "/work/crabbox"},
		{"saved generic looks like brev default", "/tmp/crabbox", "/generic/current", "/work/crabbox", "", false, false, "/tmp/crabbox", "/tmp/crabbox"},
		{"padded saved generic", " \t/generic/saved \t", "/generic/current", "", "/tmp/crabbox", false, false, " \t/generic/saved \t", " \t/generic/saved \t"},
		{"whitespace saved generic", " \t ", "/generic/current", "/work/crabbox", "", false, false, " \t ", " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Provider: "unselected-config-test", WorkRoot: tc.savedGeneric}
			MarkWorkRootExplicit(&cfg)
			cfg.WorkRoot = tc.currentGeneric
			cfg.Vast.WorkRoot, cfg.NvidiaBrev.WorkRoot = tc.vastRoot, tc.brevRoot
			if tc.vastMarked {
				MarkVastWorkRootExplicit(&cfg)
			}
			if tc.brevMarked {
				MarkNvidiaBrevWorkRootExplicit(&cfg)
			}
			before := cfg
			if got := EffectiveVastWorkRoot(cfg); got != tc.wantVast {
				t.Errorf("effective vast work root=%q, want %q", got, tc.wantVast)
			}
			if got := EffectiveNvidiaBrevWorkRoot(cfg); got != tc.wantBrev {
				t.Errorf("effective nvidia-brev work root=%q, want %q", got, tc.wantBrev)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Error("effective work-root getters modified Config")
			}
			if cfg.explicitWorkRoot != tc.savedGeneric || IsVastWorkRootExplicit(&cfg) != tc.vastMarked || IsNvidiaBrevWorkRootExplicit(&cfg) != tc.brevMarked {
				t.Error("work-root markers changed")
			}
		})
	}
}

func TestCoordinatorTokenCommandEnv(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", `["token-helper","--scope","example"]`)
	cfg := baseConfig()
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.CoordTokenCommand, "\x00"); got != "token-helper\x00--scope\x00example" {
		t.Fatalf("token command=%q", cfg.CoordTokenCommand)
	}
}

func TestCoordinatorTokenCommandEnvRejectsInvalidArgv(t *testing.T) {
	for name, value := range map[string]string{
		"not-json":  "token-helper",
		"empty":     `[]`,
		"empty-arg": `["token-helper",""]`,
		"newline":   `["token-helper","bad\narg"]`,
	} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", value)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err == nil {
				t.Fatal("invalid token command was accepted")
			}
		})
	}
}

func TestHostingerConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Hostinger.APIURL != "https://developers.hostinger.com" ||
		cfg.Hostinger.HostnamePrefix != "crabbox" ||
		cfg.Hostinger.User != "root" ||
		cfg.Hostinger.AllowPurchase {
		t.Fatalf("hostinger defaults not applied: %#v", cfg.Hostinger)
	}

	allowPurchase := true
	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "hostinger",
		Hostinger: &fileHostingerConfig{
			APIToken:        "file-token",
			APIURL:          "https://hostinger-file.example",
			ItemID:          "item-file",
			PaymentMethodID: "42",
			TemplateID:      "123",
			DataCenterID:    "456",
			HostnamePrefix:  "cbx-file",
			User:            "ubuntu",
			WorkRoot:        "/home/ubuntu/crabbox",
			AllowPurchase:   &allowPurchase,
			ReleaseAction:   "stop",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "hostinger" ||
		cfg.Hostinger.APIToken != "file-token" ||
		cfg.Hostinger.ItemID != "item-file" ||
		cfg.Hostinger.PaymentMethodID != "42" ||
		cfg.Hostinger.TemplateID != "123" ||
		cfg.Hostinger.DataCenterID != "456" ||
		!cfg.Hostinger.AllowPurchase {
		t.Fatalf("file hostinger config not applied: %#v", cfg.Hostinger)
	}
	if !IsHostingerUserExplicit(&cfg) || !IsHostingerWorkRootExplicit(&cfg) {
		t.Fatal("file hostinger SSH settings not marked explicit")
	}

	t.Setenv("HOSTINGER_API_TOKEN", "fallback-token")
	t.Setenv("CRABBOX_HOSTINGER_API_TOKEN", "env-token")
	t.Setenv("CRABBOX_HOSTINGER_API_URL", "https://hostinger-env.example")
	t.Setenv("CRABBOX_HOSTINGER_ITEM_ID", "item-env")
	t.Setenv("CRABBOX_HOSTINGER_PAYMENT_METHOD_ID", "84")
	t.Setenv("CRABBOX_HOSTINGER_TEMPLATE_ID", "789")
	t.Setenv("CRABBOX_HOSTINGER_DATA_CENTER_ID", "321")
	t.Setenv("CRABBOX_HOSTINGER_USER", "admin")
	t.Setenv("CRABBOX_HOSTINGER_WORK_ROOT", "/srv/hostinger")
	t.Setenv("CRABBOX_HOSTINGER_ALLOW_PURCHASE", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Hostinger.APIToken != "env-token" ||
		cfg.Hostinger.APIURL != "https://hostinger-env.example" ||
		cfg.Hostinger.ItemID != "item-env" ||
		cfg.Hostinger.PaymentMethodID != "84" ||
		cfg.Hostinger.TemplateID != "789" ||
		cfg.Hostinger.DataCenterID != "321" ||
		cfg.Hostinger.User != "admin" ||
		cfg.Hostinger.WorkRoot != "/srv/hostinger" ||
		cfg.Hostinger.AllowPurchase {
		t.Fatalf("env hostinger config not applied: %#v", cfg.Hostinger)
	}
	if !IsHostingerUserExplicit(&cfg) {
		t.Fatal("env hostinger user not marked explicit")
	}
}

func TestKubeVirtConfigSources(t *testing.T) {
	clearConfigEnv(t)
	want := KubeVirtConfig{Kubectl: "kubectl", Virtctl: "virtctl", Namespace: "default", SSHUser: "crabbox", SSHPort: "22", WorkRoot: "/home/crabbox/crabbox", DeleteOnRelease: true}
	if got := baseConfig().KubeVirt; got != want {
		t.Fatalf("defaults=%#v want %#v", got, want)
	}
	fields := []struct{ field, key, env string }{
		{"Kubectl", "kubectl", "KUBECTL"}, {"Virtctl", "virtctl", "VIRTCTL"}, {"Kubeconfig", "kubeconfig", "KUBECONFIG"}, {"Context", "context", "CONTEXT"}, {"Namespace", "namespace", "NAMESPACE"}, {"Template", "template", "TEMPLATE"}, {"SSHUser", "sshUser", "SSH_USER"}, {"SSHKey", "sshKey", "SSH_KEY"}, {"SSHPublicKey", "sshPublicKey", "SSH_PUBLIC_KEY"}, {"SSHPort", "sshPort", "SSH_PORT"}, {"WorkRoot", "workRoot", "WORK_ROOT"},
	}
	for _, f := range fields {
		t.Run(f.field, func(t *testing.T) {
			for _, input := range []string{"null", "''", "'  '", "'same'", "'next'"} {
				cfg := baseConfig()
				reflect.ValueOf(&cfg.KubeVirt).Elem().FieldByName(f.field).SetString("same")
				generic := cfg.WorkRoot
				var file fileConfig
				if err := yaml.Unmarshal([]byte("kubevirt: {"+f.key+": "+input+"}"), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
				want := "same"
				if input == "'  '" {
					want = "  "
				}
				if input == "'next'" {
					want = "next"
				}
				if got := reflect.ValueOf(cfg.KubeVirt).FieldByName(f.field).String(); got != want {
					t.Fatalf("file %s=%q want %q", input, got, want)
				}
				if cfg.WorkRoot != generic || IsWorkRootExplicit(&cfg) {
					t.Fatal("file introduced generic workroot state")
				}
			}
			for _, input := range []string{"", "  ", "same", "next"} {
				t.Setenv("CRABBOX_KUBEVIRT_"+f.env, input)
				cfg := baseConfig()
				reflect.ValueOf(&cfg.KubeVirt).Elem().FieldByName(f.field).SetString("same")
				generic := cfg.WorkRoot
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				want := input
				if input == "" {
					want = "same"
				}
				if got := reflect.ValueOf(cfg.KubeVirt).FieldByName(f.field).String(); got != want {
					t.Fatalf("env %q=%q want %q", input, got, want)
				}
				if cfg.WorkRoot != generic || IsWorkRootExplicit(&cfg) {
					t.Fatal("env introduced generic workroot state")
				}
			}
		})
	}
	for _, input := range []string{"", "invalid", " true ", "OFF"} {
		t.Run("bool-"+input, func(t *testing.T) {
			t.Setenv("CRABBOX_KUBEVIRT_DELETE_ON_RELEASE", input)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.KubeVirt.DeleteOnRelease != (input != "OFF") || DeleteOnReleaseExplicit(cfg, "kubevirt") != (input == " true " || input == "OFF") {
				t.Fatal("bool value/accepted marker")
			}
		})
	}
}

func TestKubeVirtConfigPathAndInput(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	fields := []struct{ field, key, env string }{{"Kubectl", "kubectl", "KUBECTL"}, {"Virtctl", "virtctl", "VIRTCTL"}, {"Kubeconfig", "kubeconfig", "KUBECONFIG"}, {"Template", "template", "TEMPLATE"}, {"SSHKey", "sshKey", "SSH_KEY"}, {"SSHPublicKey", "sshPublicKey", "SSH_PUBLIC_KEY"}}
	for _, f := range fields {
		t.Run(f.field, func(t *testing.T) {
			for _, input := range []string{"null", "''", "'~/fixture'"} {
				cfg := baseConfig()
				reflect.ValueOf(&cfg.KubeVirt).Elem().FieldByName(f.field).SetString("~/fixture")
				var file fileConfig
				if err := yaml.Unmarshal([]byte("kubevirt: {"+f.key+": "+input+", deleteOnRelease: false}"), &file); err != nil {
					t.Fatal(err)
				}
				original := *file.KubeVirt
				ptr := file.KubeVirt.DeleteOnRelease
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(*file.KubeVirt, original) || file.KubeVirt.DeleteOnRelease != ptr || *ptr {
					t.Fatal("file input or bool pointer mutated")
				}
				want := "~/fixture"
				if input == "'~/fixture'" {
					want = filepath.Join(home, "fixture")
				}
				if got := reflect.ValueOf(cfg.KubeVirt).FieldByName(f.field).String(); got != want {
					t.Fatalf("file expansion=%q want %q", got, want)
				}
				if cfg.KubeVirt.DeleteOnRelease || !DeleteOnReleaseExplicit(cfg, "kubevirt") {
					t.Fatal("false presence lost")
				}
				for _, env := range []string{"", "~/fixture"} {
					t.Setenv("CRABBOX_KUBEVIRT_"+f.env, env)
					reflect.ValueOf(&cfg.KubeVirt).Elem().FieldByName(f.field).SetString("~/fixture")
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
					if reflect.ValueOf(cfg.KubeVirt).FieldByName(f.field).String() != filepath.Join(home, "fixture") {
						t.Fatal("env fallback did not expand")
					}
				}
			}
		})
	}
	cfg := baseConfig()
	cfg.KubeVirt.Kubectl = "~/fixture"
	before := cfg.KubeVirt
	if err := applyFileConfig(&cfg, fileConfig{}); err != nil {
		t.Fatal(err)
	}
	if cfg.KubeVirt != before {
		t.Fatal("nil file changed provider")
	}
}

func TestDeleteOnReleaseExplicitTracksProviderAndSource(t *testing.T) {
	value := true
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, fileConfig{
		Incus:        &fileIncusConfig{DeleteOnRelease: &value},
		KubeVirt:     &fileKubeVirtConfig{DeleteOnRelease: &value},
		SealosDevbox: &fileSealosDevboxConfig{DeleteOnRelease: &value},
		AgentSandbox: &fileAgentSandboxConfig{DeleteOnRelease: &value},
		Namespace:    &fileNamespaceConfig{DeleteOnRelease: &value},
		Morph:        &fileMorphConfig{DeleteOnRelease: &value},
		NvidiaBrev: &fileNvidiaBrevConfig{
			ReleaseAction: "stop",
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"incus", "kubevirt", "sealos-devbox", "agent-sandbox", "namespace-devbox", "morph", "nvidia-brev"} {
		if !DeleteOnReleaseExplicit(cfg, provider) {
			t.Fatalf("file release policy not explicit for %s", provider)
		}
	}
	if DeleteOnReleaseExplicit(cfg, "hostinger") {
		t.Fatal("release policy leaked across providers")
	}

	clearConfigEnv(t)
	envCfg := baseConfig()
	for _, key := range []string{
		"CRABBOX_INCUS_DELETE_ON_RELEASE",
		"CRABBOX_KUBEVIRT_DELETE_ON_RELEASE",
		"CRABBOX_SEALOS_DEVBOX_DELETE_ON_RELEASE",
		"CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE",
		"CRABBOX_NAMESPACE_DELETE_ON_RELEASE",
		"CRABBOX_MORPH_DELETE_ON_RELEASE",
	} {
		t.Setenv(key, "false")
	}
	t.Setenv("CRABBOX_NVIDIA_BREV_RELEASE_ACTION", "stop")
	if err := applyEnv(&envCfg); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"incus", "kubevirt", "sealos-devbox", "agent-sandbox", "namespace-devbox", "morph", "nvidia-brev"} {
		if !DeleteOnReleaseExplicit(envCfg, provider) {
			t.Fatalf("environment release policy not explicit for %s", provider)
		}
	}
}

func TestSealosConfigBindingPresence(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	wantDefaults := SealosDevboxConfig{Kubectl: "kubectl", Namespace: "default", CPU: "2", Memory: "4Gi", StorageLimit: "20Gi", Network: "SSHGate", SSHGatewayPort: "2233", SSHUser: "devbox", WorkRoot: "/home/devbox/project"}
	if got := baseConfig().SealosDevbox; got != wantDefaults {
		t.Fatalf("defaults=%#v, want %#v", got, wantDefaults)
	}
	fields := []struct{ field, key, env string }{
		{"Kubectl", "kubectl", "KUBECTL"}, {"Kubeconfig", "kubeconfig", "KUBECONFIG"}, {"Context", "context", "CONTEXT"}, {"Namespace", "namespace", "NAMESPACE"}, {"Image", "image", "IMAGE"}, {"TemplateID", "templateID", "TEMPLATE_ID"}, {"CPU", "cpu", "CPU"}, {"Memory", "memory", "MEMORY"}, {"StorageLimit", "storageLimit", "STORAGE_LIMIT"}, {"Network", "network", "NETWORK"}, {"SSHGatewayHost", "sshGatewayHost", "SSH_GATEWAY_HOST"}, {"SSHGatewayPort", "sshGatewayPort", "SSH_GATEWAY_PORT"}, {"SSHUser", "sshUser", "SSH_USER"}, {"WorkRoot", "workRoot", "WORK_ROOT"}, {"NodeHost", "nodeHost", "NODE_HOST"},
	}
	for _, field := range fields {
		t.Run(field.field, func(t *testing.T) {
			for _, input := range []string{"null", "''", "'  '", "'same'"} {
				cfg := baseConfig()
				reflect.ValueOf(&cfg.SealosDevbox).Elem().FieldByName(field.field).SetString("same")
				var file fileConfig
				if err := yaml.Unmarshal([]byte("sealosDevbox: {"+field.key+": "+input+"}"), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
				want := "same"
				if input == "'  '" {
					want = "  "
				}
				if got := reflect.ValueOf(cfg.SealosDevbox).FieldByName(field.field).String(); got != want {
					t.Fatalf("file %s: got %q want %q", input, got, want)
				}
				if field.field == "WorkRoot" && IsSealosDevboxWorkRootExplicit(&cfg) != (input == "'  '" || input == "'same'") {
					t.Fatal("file work-root acceptance marker")
				}
			}
			for _, input := range []string{"", "  ", "same"} {
				t.Setenv("CRABBOX_SEALOS_DEVBOX_"+field.env, input)
				cfg := baseConfig()
				reflect.ValueOf(&cfg.SealosDevbox).Elem().FieldByName(field.field).SetString("same")
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				want := input
				if input == "" {
					want = "same"
				}
				if got := reflect.ValueOf(cfg.SealosDevbox).FieldByName(field.field).String(); got != want {
					t.Fatalf("env %q: got %q want %q", input, got, want)
				}
				if field.field == "WorkRoot" && IsSealosDevboxWorkRootExplicit(&cfg) != (input != "") {
					t.Fatal("env work-root acceptance marker")
				}
			}
		})
	}
}

func TestSealosConfigPathAndBoolTiming(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, input := range []string{"{}", "{kubectl: '', kubeconfig: ''}", "{kubectl: '~/bin/tool', kubeconfig: '~/config'}"} {
		cfg := baseConfig()
		cfg.SealosDevbox.Kubectl, cfg.SealosDevbox.Kubeconfig = "~/bin/tool", "~/config"
		var file fileConfig
		if err := yaml.Unmarshal([]byte("sealosDevbox: "+input), &file); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		wantTool, wantConfig := "~/bin/tool", "~/config"
		if strings.Contains(input, "~/") {
			wantTool, wantConfig = filepath.Join(home, "bin/tool"), filepath.Join(home, "config")
		}
		if cfg.SealosDevbox.Kubectl != wantTool || cfg.SealosDevbox.Kubeconfig != wantConfig {
			t.Fatal("file path expansion must follow acceptance")
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.SealosDevbox.Kubectl != filepath.Join(home, "bin/tool") || cfg.SealosDevbox.Kubeconfig != filepath.Join(home, "config") {
			t.Fatal("environment fallback paths must expand")
		}
	}
	for _, value := range []string{"", "invalid", " false ", "OFF", "yes"} {
		t.Run("bool-"+value, func(t *testing.T) {
			t.Setenv("CRABBOX_SEALOS_DEVBOX_DELETE_ON_RELEASE", value)
			cfg := baseConfig()
			accepted := value != "" && value != "invalid"
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.SealosDevbox.DeleteOnRelease != (value == "yes") || DeleteOnReleaseExplicit(cfg, "sealos-devbox") != accepted {
				t.Fatal("bool acceptance/value")
			}
		})
	}
	for _, value := range []string{"null", "false", "true"} {
		var file fileConfig
		if err := yaml.Unmarshal([]byte("sealosDevbox: {deleteOnRelease: "+value+"}"), &file); err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		if cfg.SealosDevbox.DeleteOnRelease != (value == "true") || DeleteOnReleaseExplicit(cfg, "sealos-devbox") != (value != "null") {
			t.Fatal("file bool presence")
		}
	}
}

func TestSealosDevboxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := baseConfig()
	if cfg.SealosDevbox.Kubectl != "kubectl" ||
		cfg.SealosDevbox.Namespace != "default" ||
		cfg.SealosDevbox.Network != "SSHGate" ||
		cfg.SealosDevbox.SSHGatewayPort != "2233" ||
		cfg.SealosDevbox.SSHUser != "devbox" ||
		cfg.SealosDevbox.WorkRoot != "/home/devbox/project" ||
		cfg.SealosDevbox.DeleteOnRelease {
		t.Fatalf("defaults=%#v", cfg.SealosDevbox)
	}
	deleteOnRelease := true
	if err := applyFileConfig(&cfg, fileConfig{SealosDevbox: &fileSealosDevboxConfig{
		Kubectl:         "~/bin/kubectl",
		Kubeconfig:      "~/.kube/sealos.yaml",
		Context:         "sealos-file",
		Namespace:       "team-a",
		Image:           "ubuntu:24.04",
		TemplateID:      "tpl-file",
		CPU:             "4",
		Memory:          "8Gi",
		StorageLimit:    "40Gi",
		Network:         "NodePort",
		SSHGatewayHost:  "ssh.file.example",
		SSHGatewayPort:  "2200",
		SSHUser:         "devbox-file",
		WorkRoot:        "/workspace/file",
		NodeHost:        "node.file.example",
		DeleteOnRelease: &deleteOnRelease,
	}}); err != nil {
		t.Fatal(err)
	}
	if cfg.SealosDevbox.Kubectl != filepath.Join(home, "bin/kubectl") ||
		cfg.SealosDevbox.Kubeconfig != filepath.Join(home, ".kube/sealos.yaml") ||
		cfg.SealosDevbox.Context != "sealos-file" ||
		cfg.SealosDevbox.Namespace != "team-a" ||
		cfg.SealosDevbox.Image != "ubuntu:24.04" ||
		cfg.SealosDevbox.TemplateID != "tpl-file" ||
		cfg.SealosDevbox.CPU != "4" ||
		cfg.SealosDevbox.Memory != "8Gi" ||
		cfg.SealosDevbox.StorageLimit != "40Gi" ||
		cfg.SealosDevbox.Network != "NodePort" ||
		cfg.SealosDevbox.SSHGatewayHost != "ssh.file.example" ||
		cfg.SealosDevbox.SSHGatewayPort != "2200" ||
		cfg.SealosDevbox.SSHUser != "devbox-file" ||
		cfg.SealosDevbox.WorkRoot != "/workspace/file" ||
		cfg.SealosDevbox.NodeHost != "node.file.example" ||
		!cfg.SealosDevbox.DeleteOnRelease {
		t.Fatalf("file sealos config not applied: %#v", cfg.SealosDevbox)
	}
	if !DeleteOnReleaseExplicit(cfg, "sealos-devbox") {
		t.Fatal("file deleteOnRelease was not marked explicit")
	}
	if !IsSealosDevboxWorkRootExplicit(&cfg) {
		t.Fatal("file workRoot was not marked explicit")
	}

	t.Setenv("CRABBOX_SEALOS_DEVBOX_KUBECTL", "~/env/kubectl")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_KUBECONFIG", "~/.kube/env.yaml")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_CONTEXT", "sealos-env")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_NAMESPACE", "team-env")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_IMAGE", "python:3.12")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_TEMPLATE_ID", "tpl-env")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_CPU", "6")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_MEMORY", "12Gi")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_STORAGE_LIMIT", "60Gi")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_NETWORK", "SSHGate")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_HOST", "ssh.env.example")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_PORT", "2223")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_SSH_USER", "devbox-env")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_WORK_ROOT", "/workspace/env")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_NODE_HOST", "node.env.example")
	t.Setenv("CRABBOX_SEALOS_DEVBOX_DELETE_ON_RELEASE", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SealosDevbox.Kubectl != filepath.Join(home, "env/kubectl") ||
		cfg.SealosDevbox.Kubeconfig != filepath.Join(home, ".kube/env.yaml") ||
		cfg.SealosDevbox.Context != "sealos-env" ||
		cfg.SealosDevbox.Namespace != "team-env" ||
		cfg.SealosDevbox.Image != "python:3.12" ||
		cfg.SealosDevbox.TemplateID != "tpl-env" ||
		cfg.SealosDevbox.CPU != "6" ||
		cfg.SealosDevbox.Memory != "12Gi" ||
		cfg.SealosDevbox.StorageLimit != "60Gi" ||
		cfg.SealosDevbox.Network != "SSHGate" ||
		cfg.SealosDevbox.SSHGatewayHost != "ssh.env.example" ||
		cfg.SealosDevbox.SSHGatewayPort != "2223" ||
		cfg.SealosDevbox.SSHUser != "devbox-env" ||
		cfg.SealosDevbox.WorkRoot != "/workspace/env" ||
		cfg.SealosDevbox.NodeHost != "node.env.example" ||
		cfg.SealosDevbox.DeleteOnRelease {
		t.Fatalf("env sealos config not applied: %#v", cfg.SealosDevbox)
	}
	if !DeleteOnReleaseExplicit(cfg, "sealos-devbox") {
		t.Fatal("env deleteOnRelease was not marked explicit")
	}
	if !IsSealosDevboxWorkRootExplicit(&cfg) {
		t.Fatal("env workRoot was not marked explicit")
	}
}

func TestAgentSandboxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.AgentSandbox.Kubectl != "kubectl" ||
		cfg.AgentSandbox.Namespace != "default" ||
		cfg.AgentSandbox.Workdir != "/workspace/crabbox" ||
		cfg.AgentSandbox.SandboxReadyTimeout != 180*time.Second ||
		cfg.AgentSandbox.PodReadyTimeout != 180*time.Second ||
		cfg.AgentSandbox.ExecTimeoutSecs != 600 ||
		!cfg.AgentSandbox.DeleteOnRelease {
		t.Fatalf("agentSandbox defaults not applied: %#v", cfg.AgentSandbox)
	}

	execTimeout := 42
	deleteOnRelease := false
	forgetMissing := true
	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "agent-sandbox",
		AgentSandbox: &fileAgentSandboxConfig{
			Kubectl:             "/opt/bin/kubectl",
			Kubeconfig:          "~/.kube/agent-sandbox",
			Context:             "agent-context",
			Namespace:           "sandboxes",
			WarmPool:            "linux-pool",
			Container:           "worker",
			Workdir:             "/workspace/my-app",
			SandboxReadyTimeout: "2m",
			PodReadyTimeout:     "45s",
			ExecTimeoutSecs:     &execTimeout,
			DeleteOnRelease:     &deleteOnRelease,
			ForgetMissing:       &forgetMissing,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "agent-sandbox" ||
		cfg.AgentSandbox.Kubectl != "/opt/bin/kubectl" ||
		!strings.HasSuffix(cfg.AgentSandbox.Kubeconfig, "/.kube/agent-sandbox") ||
		cfg.AgentSandbox.Context != "agent-context" ||
		cfg.AgentSandbox.Namespace != "sandboxes" ||
		cfg.AgentSandbox.WarmPool != "linux-pool" ||
		cfg.AgentSandbox.Container != "worker" ||
		cfg.AgentSandbox.Workdir != "/workspace/my-app" ||
		cfg.AgentSandbox.SandboxReadyTimeout != 2*time.Minute ||
		cfg.AgentSandbox.PodReadyTimeout != 45*time.Second ||
		cfg.AgentSandbox.ExecTimeoutSecs != 42 ||
		cfg.AgentSandbox.DeleteOnRelease ||
		!cfg.AgentSandbox.ForgetMissing {
		t.Fatalf("file agentSandbox config not applied: %#v", cfg.AgentSandbox)
	}
	if !DeleteOnReleaseExplicit(cfg, "agent-sandbox") {
		t.Fatal("file agentSandbox deleteOnRelease was not marked explicit")
	}

	t.Setenv("CRABBOX_AGENT_SANDBOX_KUBECTL", "/usr/local/bin/kubectl")
	t.Setenv("CRABBOX_AGENT_SANDBOX_KUBECONFIG", "/tmp/kubeconfig")
	t.Setenv("CRABBOX_AGENT_SANDBOX_CONTEXT", "env-context")
	t.Setenv("CRABBOX_AGENT_SANDBOX_NAMESPACE", "env-ns")
	t.Setenv("CRABBOX_AGENT_SANDBOX_WARM_POOL", "env-pool")
	t.Setenv("CRABBOX_AGENT_SANDBOX_CONTAINER", "env-container")
	t.Setenv("CRABBOX_AGENT_SANDBOX_WORKDIR", "/workspace/env")
	t.Setenv("CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT", "3m")
	t.Setenv("CRABBOX_AGENT_SANDBOX_POD_READY_TIMEOUT", "90s")
	t.Setenv("CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS", "99")
	t.Setenv("CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_AGENT_SANDBOX_FORGET_MISSING", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentSandbox.Kubectl != "/usr/local/bin/kubectl" ||
		cfg.AgentSandbox.Kubeconfig != "/tmp/kubeconfig" ||
		cfg.AgentSandbox.Context != "env-context" ||
		cfg.AgentSandbox.Namespace != "env-ns" ||
		cfg.AgentSandbox.WarmPool != "env-pool" ||
		cfg.AgentSandbox.Container != "env-container" ||
		cfg.AgentSandbox.Workdir != "/workspace/env" ||
		cfg.AgentSandbox.SandboxReadyTimeout != 3*time.Minute ||
		cfg.AgentSandbox.PodReadyTimeout != 90*time.Second ||
		cfg.AgentSandbox.ExecTimeoutSecs != 99 ||
		!cfg.AgentSandbox.DeleteOnRelease ||
		cfg.AgentSandbox.ForgetMissing {
		t.Fatalf("env agentSandbox config not applied: %#v", cfg.AgentSandbox)
	}
}

func TestNamespaceConfigBindingSources(t *testing.T) {
	clearConfigEnv(t)
	defaults := baseConfig().Namespace
	if defaults.Image != "builtin:base" || defaults.Size != "" || defaults.Repository != "" || defaults.Site != "" || defaults.VolumeSizeGB != 0 || defaults.AutoStopIdleTimeout != 30*time.Minute || defaults.WorkRoot != "/workspaces/crabbox" || defaults.DeleteOnRelease {
		t.Fatalf("Namespace defaults=%#v", defaults)
	}
	for _, tc := range []struct {
		name, duration, volumeEnv     string
		volumeFile, fileWant, envWant int
		wantDuration                  time.Duration
	}{
		{"positive", "45m", "9", 9, 9, 9, 45 * time.Minute},
		{"zero", "0s", "0", 0, 7, 0, 17 * time.Minute},
		{"negative", "-1m", "-2", -2, 7, -2, 17 * time.Minute},
		{"malformed", "invalid", "invalid", 0, 7, 7, 17 * time.Minute},
		{"padded", " 45m ", "7", 0, 7, 7, 17 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			newConfig := func() Config {
				cfg := baseConfig()
				cfg.Namespace.VolumeSizeGB = 7
				cfg.Namespace.AutoStopIdleTimeout = 17 * time.Minute
				cfg.Namespace.DeleteOnRelease = true
				return cfg
			}
			value := false
			file := fileConfig{Namespace: &fileNamespaceConfig{Image: " raw-image ", Size: " l ", Repository: " raw-repo ", Site: " raw-site ", VolumeSizeGB: tc.volumeFile, AutoStopIdleTimeout: tc.duration, WorkRoot: " /workspaces/raw ", DeleteOnRelease: &value}}
			cfg := newConfig()
			priorRoot, priorType := cfg.WorkRoot, cfg.ServerType
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal(err)
			}
			assertRaw := func(cfg Config, volume int) {
				t.Helper()
				got := cfg.Namespace
				if got.Image != " raw-image " || got.Size != " l " || got.Repository != " raw-repo " || got.Site != " raw-site " || got.WorkRoot != " /workspaces/raw " || got.VolumeSizeGB != volume || got.AutoStopIdleTimeout != tc.wantDuration || got.DeleteOnRelease || !DeleteOnReleaseExplicit(cfg, "namespace-devbox") {
					t.Fatalf("raw source binding=%#v", got)
				}
				if cfg.WorkRoot != priorRoot || cfg.ServerType != priorType {
					t.Fatal("file/environment input acquired flag-only generic effects")
				}
			}
			assertRaw(cfg, tc.fileWant)
			encoded, err := yaml.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			var decoded fileConfig
			if err := yaml.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(file.Namespace, decoded.Namespace) {
				t.Fatalf("raw Namespace writer representation changed: %s (%v)", encoded, err)
			}
			for name, raw := range map[string]string{"IMAGE": " raw-image ", "SIZE": " l ", "REPOSITORY": " raw-repo ", "SITE": " raw-site ", "WORK_ROOT": " /workspaces/raw ", "VOLUME_SIZE_GB": tc.volumeEnv, "AUTO_STOP_IDLE_TIMEOUT": tc.duration, "DELETE_ON_RELEASE": "false"} {
				t.Setenv("CRABBOX_NAMESPACE_"+name, raw)
			}
			cfg = newConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			assertRaw(cfg, tc.envWant)
		})
	}
}

func TestAgentSandboxDurationOverlays(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 13 * time.Second}, {"0", 13 * time.Second}, {"0s", 13 * time.Second},
		{"-1s", 13 * time.Second}, {"invalid", 13 * time.Second}, {" 2m ", 13 * time.Second},
		{"999999999999999999999h", 13 * time.Second}, {"2m", 2 * time.Minute}, {"1500ms", 1500 * time.Millisecond},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.AgentSandbox.SandboxReadyTimeout = 13 * time.Second
			cfg.AgentSandbox.PodReadyTimeout = 13 * time.Second
			file := fileConfig{AgentSandbox: &fileAgentSandboxConfig{SandboxReadyTimeout: tc.raw, PodReadyTimeout: tc.raw}}
			if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
				t.Fatal(err)
			}
			if cfg.AgentSandbox.SandboxReadyTimeout != tc.want || cfg.AgentSandbox.PodReadyTimeout != tc.want {
				t.Fatalf("file durations=%v/%v, want %v", cfg.AgentSandbox.SandboxReadyTimeout, cfg.AgentSandbox.PodReadyTimeout, tc.want)
			}
			encoded, err := yaml.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			var decoded fileConfig
			if err := yaml.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.AgentSandbox == nil || decoded.AgentSandbox.SandboxReadyTimeout != tc.raw || decoded.AgentSandbox.PodReadyTimeout != tc.raw {
				t.Fatalf("duration storage changed raw input %q", tc.raw)
			}
			cfg.AgentSandbox.SandboxReadyTimeout = 13 * time.Second
			cfg.AgentSandbox.PodReadyTimeout = 13 * time.Second
			t.Setenv("CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT", tc.raw)
			t.Setenv("CRABBOX_AGENT_SANDBOX_POD_READY_TIMEOUT", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.AgentSandbox.SandboxReadyTimeout != tc.want || cfg.AgentSandbox.PodReadyTimeout != tc.want {
				t.Fatalf("env durations=%v/%v, want %v", cfg.AgentSandbox.SandboxReadyTimeout, cfg.AgentSandbox.PodReadyTimeout, tc.want)
			}
		})
	}
}

func TestAgentSandboxPartialInputAndPathEvents(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	negative, disabled, forgotten := -1, false, true
	file := fileConfig{AgentSandbox: &fileAgentSandboxConfig{
		Kubectl: "custom-kubectl", Kubeconfig: "~/accepted", Namespace: "changed",
		SandboxReadyTimeout: "2m", PodReadyTimeout: "invalid", ExecTimeoutSecs: &negative,
		DeleteOnRelease: &disabled, ForgetMissing: &forgotten,
	}}
	cfg := baseConfig()
	err := applyFileConfig(&cfg, file)
	if err == nil || err.Error() != "agentSandbox execTimeoutSecs must be non-negative" {
		t.Fatalf("file error=%v", err)
	}
	if cfg.AgentSandbox.Kubeconfig != filepath.Join(home, "accepted") || cfg.AgentSandbox.Namespace != "changed" ||
		cfg.AgentSandbox.SandboxReadyTimeout != 2*time.Minute || cfg.AgentSandbox.PodReadyTimeout != 180*time.Second || cfg.AgentSandbox.ExecTimeoutSecs != 600 {
		t.Fatalf("file partial values=%#v", cfg.AgentSandbox)
	}
	if !cfg.AgentSandbox.DeleteOnRelease || cfg.AgentSandbox.ForgetMissing || DeleteOnReleaseExplicit(cfg, "agent-sandbox") {
		t.Fatal("later booleans applied after file error")
	}
	if file.AgentSandbox.Kubeconfig != "~/accepted" {
		t.Fatal("file input was normalized in place")
	}

	cfg = baseConfig()
	cfg.AgentSandbox.Kubeconfig = "~/inherited"
	file.AgentSandbox.ExecTimeoutSecs = nil
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentSandbox.Kubeconfig != "~/inherited" || cfg.AgentSandbox.Kubectl != "kubectl" || cfg.AgentSandbox.Namespace != "default" {
		t.Fatal("unaccepted strings changed")
	}
	if cfg.AgentSandbox.DeleteOnRelease || !cfg.AgentSandbox.ForgetMissing || !DeleteOnReleaseExplicit(cfg, "agent-sandbox") {
		t.Fatal("admitted explicit booleans were lost")
	}

	cfg = baseConfig()
	cfg.AgentSandbox.Kubeconfig = "~/inherited"
	t.Setenv("CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT", "90s")
	t.Setenv("CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS", "invalid")
	t.Setenv("CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE", "false")
	t.Setenv("CRABBOX_AGENT_SANDBOX_FORGET_MISSING", "true")
	if err := applyEnv(&cfg); err == nil {
		t.Fatal("missing integer environment error")
	}
	if cfg.AgentSandbox.Kubeconfig != filepath.Join(home, "inherited") || cfg.AgentSandbox.SandboxReadyTimeout != 90*time.Second || cfg.AgentSandbox.ExecTimeoutSecs != 0 {
		t.Fatalf("env partial values=%#v", cfg.AgentSandbox)
	}
	if !cfg.AgentSandbox.DeleteOnRelease || cfg.AgentSandbox.ForgetMissing || DeleteOnReleaseExplicit(cfg, "agent-sandbox") {
		t.Fatal("later booleans applied after environment error")
	}
}

func TestSealosDevboxUntrustedConfigCannotRedirectClusterWorkload(t *testing.T) {
	cfg := baseConfig()
	cfg.SealosDevbox.Kubectl = "/trusted/kubectl"
	cfg.SealosDevbox.Kubeconfig = "/trusted/kubeconfig"
	cfg.SealosDevbox.Context = "trusted-context"
	cfg.SealosDevbox.Namespace = "trusted-namespace"
	cfg.SealosDevbox.Image = "trusted-image"
	cfg.SealosDevbox.TemplateID = "trusted-template"
	cfg.SealosDevbox.CPU = "2"
	cfg.SealosDevbox.Memory = "4Gi"
	cfg.SealosDevbox.StorageLimit = "20Gi"
	cfg.SealosDevbox.Network = "SSHGate"
	cfg.SealosDevbox.SSHGatewayHost = "trusted-ssh.example.test"
	cfg.SealosDevbox.SSHGatewayPort = "2222"
	cfg.SealosDevbox.SSHUser = "trusted-user"
	cfg.SealosDevbox.WorkRoot = "/trusted/work"
	cfg.SealosDevbox.NodeHost = "trusted-node.example.test"
	deleteOnRelease := true
	if err := applyFileConfigWithTrust(&cfg, fileConfig{
		SealosDevbox: &fileSealosDevboxConfig{
			Kubectl:         "./payload",
			Kubeconfig:      "./exec-plugin-kubeconfig",
			Context:         "attacker-context",
			Namespace:       "attacker-namespace",
			Image:           "attacker-image",
			TemplateID:      "attacker-template",
			CPU:             "64",
			Memory:          "1Ti",
			StorageLimit:    "10Ti",
			Network:         "NodePort",
			SSHGatewayHost:  "attacker-ssh.example.test",
			SSHGatewayPort:  "2022",
			SSHUser:         "attacker-user",
			WorkRoot:        "/attacker/work",
			NodeHost:        "attacker-node.example.test",
			DeleteOnRelease: &deleteOnRelease,
		},
	}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.SealosDevbox.Kubectl != "/trusted/kubectl" ||
		cfg.SealosDevbox.Kubeconfig != "/trusted/kubeconfig" ||
		cfg.SealosDevbox.Context != "trusted-context" ||
		cfg.SealosDevbox.Namespace != "trusted-namespace" ||
		cfg.SealosDevbox.Image != "trusted-image" ||
		cfg.SealosDevbox.TemplateID != "trusted-template" ||
		cfg.SealosDevbox.CPU != "2" ||
		cfg.SealosDevbox.Memory != "4Gi" ||
		cfg.SealosDevbox.StorageLimit != "20Gi" ||
		cfg.SealosDevbox.Network != "SSHGate" ||
		cfg.SealosDevbox.SSHGatewayHost != "trusted-ssh.example.test" ||
		cfg.SealosDevbox.SSHGatewayPort != "2222" ||
		cfg.SealosDevbox.SSHUser != "trusted-user" ||
		cfg.SealosDevbox.WorkRoot != "/trusted/work" ||
		cfg.SealosDevbox.NodeHost != "trusted-node.example.test" {
		t.Fatalf("untrusted sealos cluster workload override applied: %#v", cfg.SealosDevbox)
	}
	if !cfg.SealosDevbox.DeleteOnRelease || !DeleteOnReleaseExplicit(cfg, "sealos-devbox") {
		t.Fatalf("untrusted deleteOnRelease should still apply explicitly: %#v", cfg.SealosDevbox)
	}
}

func TestAgentSandboxUntrustedConfigCannotRedirectClusterWorkload(t *testing.T) {
	cfg := baseConfig()
	cfg.AgentSandbox.Kubectl = "/trusted/kubectl"
	cfg.AgentSandbox.Kubeconfig = "/trusted/kubeconfig"
	cfg.AgentSandbox.Context = "trusted-context"
	cfg.AgentSandbox.Namespace = "trusted-namespace"
	cfg.AgentSandbox.WarmPool = "trusted-pool"
	cfg.AgentSandbox.Container = "trusted-container"
	cfg.AgentSandbox.Workdir = "/trusted/workspace"
	if err := applyFileConfigWithTrust(&cfg, fileConfig{
		AgentSandbox: &fileAgentSandboxConfig{
			Kubectl:    "./payload",
			Kubeconfig: "./exec-plugin-kubeconfig",
			Context:    "attacker-context",
			Namespace:  "repo-sandboxes",
			WarmPool:   "repo-pool",
			Container:  "repo-container",
			Workdir:    "/workspace/repo",
		},
	}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentSandbox.Kubectl != "/trusted/kubectl" ||
		cfg.AgentSandbox.Kubeconfig != "/trusted/kubeconfig" ||
		cfg.AgentSandbox.Context != "trusted-context" ||
		cfg.AgentSandbox.Namespace != "trusted-namespace" ||
		cfg.AgentSandbox.WarmPool != "trusted-pool" ||
		cfg.AgentSandbox.Container != "trusted-container" ||
		cfg.AgentSandbox.Workdir != "/trusted/workspace" {
		t.Fatalf("untrusted cluster workload override applied: %#v", cfg.AgentSandbox)
	}
}

func TestAgentSandboxConfigRejectsNegativeExecTimeout(t *testing.T) {
	timeout := -1
	cfg := baseConfig()
	err := applyFileConfig(&cfg, fileConfig{
		AgentSandbox: &fileAgentSandboxConfig{ExecTimeoutSecs: &timeout},
	})
	if err == nil {
		t.Fatal("negative agentSandbox exec timeout was accepted")
	}

	clearConfigEnv(t)
	cfg = baseConfig()
	t.Setenv("CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS", "-1")
	if err := applyEnv(&cfg); err == nil {
		t.Fatal("negative env agentSandbox exec timeout was accepted")
	}
}

func TestDockerSandboxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.DockerSandbox.CLIPath != "sbx" || cfg.DockerSandbox.Agent != "shell" || cfg.DockerSandbox.Workdir != "" {
		t.Fatalf("dockerSandbox defaults not applied: %#v", cfg.DockerSandbox)
	}
	clone := true
	template := "ubuntu"
	cpus := 2.5
	memory := "6g"
	workdir := "/workspace/my-app"
	extraWorkspaces := []string{"/tmp/extra"}
	mcp := []string{"context7", "all"}
	kit := []string{"example-org/base"}
	applyFileConfig(&cfg, fileConfig{
		Provider: "docker-sandbox",
		DockerSandbox: &fileDockerSandboxConfig{
			CLIPath:         "/opt/sbx",
			Agent:           "shell",
			Template:        &template,
			CPUs:            &cpus,
			Memory:          &memory,
			Clone:           &clone,
			Workdir:         &workdir,
			ExtraWorkspaces: &extraWorkspaces,
			MCP:             &mcp,
			Kit:             &kit,
		},
	})
	if cfg.Provider != "docker-sandbox" || cfg.DockerSandbox.CLIPath != "/opt/sbx" || cfg.DockerSandbox.Template != "ubuntu" || cfg.DockerSandbox.CPUs != 2.5 || cfg.DockerSandbox.Memory != "6g" || !cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/workspace/my-app" {
		t.Fatalf("file dockerSandbox config not applied: %#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(cfg.DockerSandbox.MCP, ",") != "context7,all" || strings.Join(cfg.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("file dockerSandbox list config not applied: %#v", cfg.DockerSandbox)
	}

	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLI", "/usr/local/bin/sbx")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_AGENT", "shell")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_TEMPLATE", "debian")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "4")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MEMORY", "8g")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLONE", "false")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_WORKDIR", "/workspace/env-app")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "/tmp/a,/tmp/b")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MCP", "context7,all")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_KIT", "kit-a,kit-b")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.DockerSandbox.CLIPath != "/usr/local/bin/sbx" || cfg.DockerSandbox.Template != "debian" || cfg.DockerSandbox.CPUs != 4 || cfg.DockerSandbox.Memory != "8g" || cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/workspace/env-app" {
		t.Fatalf("env dockerSandbox config not applied: %#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/a,/tmp/b" || strings.Join(cfg.DockerSandbox.MCP, ",") != "context7,all" || strings.Join(cfg.DockerSandbox.Kit, ",") != "kit-a,kit-b" {
		t.Fatalf("env dockerSandbox list config not applied: %#v", cfg.DockerSandbox)
	}
}

func TestE2BFileAcceptanceAndSource(t *testing.T) {
	if _, ok := reflect.TypeOf(fileE2BConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("API key YAML field introduced")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace"} {
			cfg := baseConfig()
			cfg.Provider = "e2b"
			cfg.E2B = E2BConfig{APIKey: "inert", APIURL: "https://example.invalid/api", Domain: "example.invalid", Template: "template", Workdir: "work", User: "alice"}
			cfg.credentialProvenance.e2bAPIURL, cfg.credentialProvenance.e2bDomain, cfg.credentialProvenance.e2bAPIKey = credentialSourceEnvironment, credentialSourceEnvironment, credentialSourceEnvironment
			want := cfg.E2B
			source := credentialSourceEnvironment
			fields := map[string]any{"apiKey": "ignored-inert"}
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiUrl", &want.APIURL}, {"domain", &want.Domain}, {"template", &want.Template}, {"workdir", &want.Workdir}, {"user", &want.User}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
					*f.v = "  "
				}
				fields[f.key] = raw
			}
			if mode == "equal" || mode == "whitespace" {
				source = credentialSourceForFile(trusted)
			}
			data, err := yaml.Marshal(map[string]any{"e2b": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.E2B != want || cfg.credentialProvenance.e2bAPIURL != source || cfg.credentialProvenance.e2bDomain != source || cfg.credentialProvenance.e2bAPIKey != credentialSourceEnvironment {
				t.Fatalf("file acceptance changed mode=%s trusted=%t", mode, trusted)
			}
		}
	}
}

func TestE2BEnvironmentAcceptanceAndSource(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "equal", "whitespace", "API_KEY", "API_URL", "DOMAIN"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.E2B = E2BConfig{APIKey: "inert", APIURL: "https://example.invalid/api", Domain: "example.invalid", Template: "template", Workdir: "work", User: "alice"}
		cfg.credentialProvenance.e2bAPIURL, cfg.credentialProvenance.e2bDomain, cfg.credentialProvenance.e2bAPIKey = credentialSourceTrustedFile, credentialSourceTrustedFile, credentialSourceTrustedFile
		want := cfg.E2B
		accepted := map[string]bool{}
		for _, f := range []struct {
			suffix, alias, value string
			v                    *string
		}{{"API_KEY", "E2B_API_KEY", "inert-new", &want.APIKey}, {"API_URL", "E2B_API_URL", "https://example.invalid/new", &want.APIURL}, {"DOMAIN", "E2B_DOMAIN", "new.example.invalid", &want.Domain}, {"TEMPLATE", "", "new-template", &want.Template}, {"WORKDIR", "", "new-work", &want.Workdir}, {"USER", "", "bob", &want.User}} {
			primary, alias := f.value, f.value+"-alias"
			if mode == "equal" {
				primary = *f.v
			}
			if mode == "whitespace" {
				primary = "  "
			}
			allow := mode != "empty" && (!(mode == "API_KEY" || mode == "API_URL" || mode == "DOMAIN") || mode == f.suffix)
			if mode == "alias" {
				primary = ""
				allow = f.alias != ""
			}
			if !allow {
				primary, alias = "", ""
			} else if primary != "" {
				*f.v = primary
			} else {
				*f.v = alias
			}
			accepted[f.suffix] = allow
			t.Setenv("CRABBOX_E2B_"+f.suffix, primary)
			if f.alias != "" {
				t.Setenv(f.alias, alias)
			}
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.E2B != want {
			t.Fatalf("environment values changed mode=%s", mode)
		}
		for suffix, source := range map[string]credentialValueSource{"API_KEY": cfg.credentialProvenance.e2bAPIKey, "API_URL": cfg.credentialProvenance.e2bAPIURL, "DOMAIN": cfg.credentialProvenance.e2bDomain} {
			want := credentialSourceTrustedFile
			if accepted[suffix] {
				want = credentialSourceEnvironment
			}
			if source != want {
				t.Fatalf("source=%s mode=%s", suffix, mode)
			}
		}
	}
}

func TestE2BCoreTemplateDefaultKeepsRawWhitespace(t *testing.T) {
	for _, raw := range []string{"", "  ", "custom"} {
		cfg := Config{Provider: "e2b", E2B: E2BConfig{Template: raw}}
		want := raw
		if want == "" {
			want = "base"
		}
		if got := serverTypeForConfig(cfg); got != want {
			t.Fatalf("template=%q got=%q want=%q", raw, got, want)
		}
	}
}

func TestCloudflareConfigAcceptanceAndSource(t *testing.T) {
	for _, source := range []string{"user", "repository", "environment"} {
		for _, mode := range []string{"omitted", "empty", "null", "equal", "whitespace", "token only", "URL only"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				cfg.Provider = "cloudflare"
				cfg.Cloudflare = CloudflareConfig{APIURL: "https://example.invalid/api", Token: "inert", Workdir: "/workspace/app"}
				cfg.credentialProvenance.cloudflareAPIURL, cfg.credentialProvenance.cloudflareToken = credentialSourceFlag, credentialSourceFlag
				want := cfg.Cloudflare
				urlSource, tokenSource := credentialSourceFlag, credentialSourceFlag
				acceptedSource := credentialSourceTrustedFile
				if source == "repository" {
					acceptedSource = credentialSourceRepository
				}
				if source == "environment" {
					acceptedSource = credentialSourceEnvironment
				}
				fields := map[string]any{}
				for _, field := range []struct {
					key, env string
					value    *string
				}{{"apiUrl", "CRABBOX_CLOUDFLARE_RUNNER_URL", &want.APIURL}, {"token", "CRABBOX_CLOUDFLARE_RUNNER_TOKEN", &want.Token}, {"workdir", "CRABBOX_CLOUDFLARE_WORKDIR", &want.Workdir}} {
					if mode == "omitted" || (mode == "token only" && field.key != "token") || (mode == "URL only" && field.key != "apiUrl") {
						continue
					}
					var raw any = *field.value
					if mode == "empty" {
						raw = ""
					}
					if mode == "null" {
						raw = nil
					}
					if mode == "whitespace" {
						raw = "  "
					}
					fields[field.key] = raw
					if v, ok := raw.(string); ok && v != "" {
						*field.value = v
						if field.key == "apiUrl" {
							urlSource = acceptedSource
						}
						if field.key == "token" {
							tokenSource = acceptedSource
						}
					}
					if source == "environment" {
						v, _ := raw.(string)
						t.Setenv(field.env, v)
					}
				}
				if source == "environment" {
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					data, err := yaml.Marshal(map[string]any{"cloudflare": fields})
					if err != nil {
						t.Fatal(err)
					}
					var file fileConfig
					if err := yaml.Unmarshal(data, &file); err != nil {
						t.Fatal(err)
					}
					if err := applyFileConfigWithTrust(&cfg, file, source == "user"); err != nil {
						t.Fatal(err)
					}
				}
				if cfg.Cloudflare != want || cfg.credentialProvenance.cloudflareAPIURL != urlSource || cfg.credentialProvenance.cloudflareToken != tokenSource {
					t.Fatal("Cloudflare value/source acceptance changed")
				}
				if source == "repository" && mode == "equal" {
					if err := validateProviderCredentialDestination(cfg); err != nil {
						t.Fatalf("actual same-source repository token contract changed: %v", err)
					}
				}
			})
		}
	}
}

func TestUpstashBoxFileAcceptanceAndSource(t *testing.T) {
	if _, ok := reflect.TypeOf(fileUpstashBoxConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("APIKey must not be a YAML field")
	}
	for _, trusted := range []bool{false, true} {
		for _, raw := range []string{"null", "''", "'  '", "equal"} {
			cfg := baseConfig()
			cfg.Provider = "upstash-box"
			cfg.UpstashBox = UpstashBoxConfig{APIKey: "inert", BaseURL: "https://example.invalid/api", Runtime: "python", Size: "large", Workdir: "/workspace/home/app", KeepAlive: true}
			cfg.credentialProvenance.upstashBoxBaseURL, cfg.credentialProvenance.upstashBoxAPIKey = credentialSourceEnvironment, credentialSourceEnvironment
			want := cfg.UpstashBox
			wantSource := credentialSourceEnvironment
			base, runtime, size, workdir := raw, raw, raw, raw
			if raw == "equal" {
				base, runtime, size, workdir = want.BaseURL, want.Runtime, want.Size, want.Workdir
			}
			if raw == "'  '" {
				want.BaseURL, want.Runtime, want.Size, want.Workdir = "  ", "  ", "  ", "  "
			}
			if raw == "equal" || raw == "'  '" {
				wantSource = credentialSourceForFile(trusted)
			}
			keep := "null"
			if raw != "null" {
				keep = "false"
				want.KeepAlive = false
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte("upstashBox:\n  apiKey: ignored-inert\n  baseUrl: "+base+"\n  runtime: "+runtime+"\n  size: "+size+"\n  workdir: "+workdir+"\n  keepAlive: "+keep+"\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.UpstashBox != want || cfg.credentialProvenance.upstashBoxBaseURL != wantSource || cfg.credentialProvenance.upstashBoxAPIKey != credentialSourceEnvironment {
				t.Fatalf("file contract trusted=%t raw=%s", trusted, raw)
			}
			if raw == "equal" {
				err := validateProviderCredentialDestination(cfg)
				if (err != nil) != !trusted {
					t.Fatalf("later policy=%v", err)
				}
			}
			before := cfg
			if err := applyFileConfigWithTrust(&cfg, fileConfig{UpstashBox: &fileUpstashBoxConfig{}}, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.UpstashBox != before.UpstashBox || !reflect.DeepEqual(cfg.credentialProvenance, before.credentialProvenance) {
				t.Fatal("omission changed config/source")
			}
		}
	}
}

func TestUpstashBoxEnvironmentAcceptanceAndSource(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "whitespace", "equal", "key only", "URL only"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.UpstashBox = UpstashBoxConfig{APIKey: "inert", BaseURL: "https://example.invalid/prior", Runtime: "node", Size: "small", Workdir: "/workspace/home/app"}
		cfg.credentialProvenance.upstashBoxBaseURL, cfg.credentialProvenance.upstashBoxAPIKey = credentialSourceTrustedFile, credentialSourceTrustedFile
		want := cfg.UpstashBox
		for _, item := range []struct {
			suffix, alias, value string
			target               *string
		}{
			{"API_KEY", "UPSTASH_BOX_API_KEY", "inert-new", &want.APIKey}, {"BASE_URL", "UPSTASH_BOX_BASE_URL", "https://example.invalid/new", &want.BaseURL},
			{"RUNTIME", "", "python", &want.Runtime}, {"SIZE", "", "large", &want.Size}, {"WORKDIR", "", "/workspace/home/new", &want.Workdir},
		} {
			primary, alias := item.value, item.value+"-alias"
			if mode == "equal" {
				primary = *item.target
			} else if mode == "whitespace" {
				primary = "  "
			}
			accept := mode != "empty" && !(mode == "key only" && item.suffix != "API_KEY") && !(mode == "URL only" && item.suffix != "BASE_URL")
			if mode == "alias" {
				primary = ""
				accept = item.alias != ""
			}
			if !accept {
				primary, alias = "", ""
			} else if primary != "" {
				*item.target = primary
			} else {
				*item.target = alias
			}
			t.Setenv("CRABBOX_UPSTASH_BOX_"+item.suffix, primary)
			if item.alias != "" {
				t.Setenv(item.alias, alias)
			}
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		urlSource, keySource := credentialSourceEnvironment, credentialSourceEnvironment
		if mode == "empty" || mode == "key only" {
			urlSource = credentialSourceTrustedFile
		}
		if mode == "empty" || mode == "URL only" {
			keySource = credentialSourceTrustedFile
		}
		if cfg.UpstashBox != want || cfg.credentialProvenance.upstashBoxBaseURL != urlSource || cfg.credentialProvenance.upstashBoxAPIKey != keySource {
			t.Fatalf("env contract changed mode=%s", mode)
		}
	}
	for _, prior := range []bool{false, true} {
		for _, raw := range []string{"", "invalid", "no", "yes"} {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_UPSTASH_BOX_KEEP_ALIVE", raw)
			cfg := baseConfig()
			cfg.UpstashBox.KeepAlive = prior
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			want := prior
			if raw == "no" {
				want = false
			}
			if raw == "yes" {
				want = true
			}
			if cfg.UpstashBox.KeepAlive != want {
				t.Fatalf("bool raw=%q prior=%t", raw, prior)
			}
		}
	}
}

func TestUpstashBoxCoreSizePresentationUsesExactAliases(t *testing.T) {
	for _, name := range []string{"upstash-box", "upstash", "box", "upstashbox", " Upstash "} {
		for _, size := range []string{"", "  ", "medium"} {
			cfg := Config{Provider: name, UpstashBox: UpstashBoxConfig{Size: size}}
			want := ""
			if name == "upstash-box" || name == "upstash" {
				want = size
				if want == "" {
					want = "small"
				}
			}
			if got := serverTypeForConfig(cfg); got != want {
				t.Fatalf("name=%q raw=%q type=%q want=%q", name, size, got, want)
			}
		}
	}
}

func TestRailwayFileAcceptanceAndSource(t *testing.T) {
	if _, ok := reflect.TypeOf(fileRailwayConfig{}).FieldByName("APIToken"); ok {
		t.Fatal("APIToken must not be a YAML field")
	}
	for _, trusted := range []bool{false, true} {
		for _, raw := range []string{"null", "''", "'  '", "equal"} {
			cfg := baseConfig()
			cfg.Provider = "railway"
			cfg.Railway = RailwayConfig{APIToken: "inert", APIURL: "https://example.invalid/api", ProjectID: "project", EnvironmentID: "environment"}
			cfg.credentialProvenance.railwayAPIURL, cfg.credentialProvenance.railwayAPIToken = credentialSourceEnvironment, credentialSourceEnvironment
			want := cfg.Railway
			wantSource := credentialSourceEnvironment
			url, project, environment := raw, raw, raw
			if raw == "equal" {
				url, project, environment = want.APIURL, want.ProjectID, want.EnvironmentID
			}
			if raw == "'  '" {
				want.APIURL, want.ProjectID, want.EnvironmentID = "  ", "  ", "  "
			}
			if raw == "equal" || raw == "'  '" {
				wantSource = credentialSourceForFile(trusted)
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte("railway:\n  apiToken: ignored-inert\n  apiUrl: "+url+"\n  projectId: "+project+"\n  environmentId: "+environment+"\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Railway != want || cfg.credentialProvenance.railwayAPIURL != wantSource || cfg.credentialProvenance.railwayAPIToken != credentialSourceEnvironment {
				t.Fatalf("file contract changed trusted=%t raw=%q", trusted, raw)
			}
			if raw == "equal" {
				err := validateProviderCredentialDestination(cfg)
				if (err != nil) != !trusted {
					t.Fatalf("later policy trusted=%t err=%v", trusted, err)
				}
			}
			before := cfg
			if err := applyFileConfigWithTrust(&cfg, fileConfig{Railway: &fileRailwayConfig{}}, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Railway != before.Railway || !reflect.DeepEqual(cfg.credentialProvenance, before.credentialProvenance) {
				t.Fatal("omitted fields changed value/source")
			}
		}
	}
}

func TestRailwayEnvironmentAcceptanceAndSource(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "whitespace", "equal", "token only", "URL only"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Railway = RailwayConfig{APIToken: "inert", APIURL: "https://example.invalid/prior", ProjectID: "project", EnvironmentID: "environment"}
			cfg.credentialProvenance.railwayAPIURL, cfg.credentialProvenance.railwayAPIToken = credentialSourceTrustedFile, credentialSourceTrustedFile
			want := cfg.Railway
			for _, item := range []struct {
				suffix, primaryValue, aliasValue string
				target                           *string
			}{
				{"API_TOKEN", "inert-primary", "inert-alias", &want.APIToken},
				{"API_URL", "https://example.invalid/primary", "https://example.invalid/alias", &want.APIURL},
				{"PROJECT_ID", "primary-project", "alias-project", &want.ProjectID},
				{"ENVIRONMENT_ID", "primary-environment", "alias-environment", &want.EnvironmentID},
			} {
				primary, alias := item.primaryValue, item.aliasValue
				switch mode {
				case "empty":
					primary, alias = "", ""
				case "equal":
					primary = *item.target
				case "whitespace":
					primary = "  "
					*item.target = primary
				case "alias":
					primary = ""
					*item.target = alias
				case "token only", "URL only":
					if (mode == "token only" && item.suffix == "API_TOKEN") || (mode == "URL only" && item.suffix == "API_URL") {
						*item.target = primary
					} else {
						primary, alias = "", ""
					}
				default:
					*item.target = primary
				}
				t.Setenv("CRABBOX_RAILWAY_"+item.suffix, primary)
				t.Setenv("RAILWAY_"+item.suffix, alias)
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			urlSource, tokenSource := credentialSourceEnvironment, credentialSourceEnvironment
			if mode == "empty" || mode == "token only" {
				urlSource = credentialSourceTrustedFile
			}
			if mode == "empty" || mode == "URL only" {
				tokenSource = credentialSourceTrustedFile
			}
			if cfg.Railway != want || cfg.credentialProvenance.railwayAPIURL != urlSource || cfg.credentialProvenance.railwayAPIToken != tokenSource {
				t.Fatal("environment value/acceptance changed")
			}
		})
	}
}

func TestFastAPICloudFileAcceptanceAndProvenance(t *testing.T) {
	if _, ok := reflect.TypeOf(fileFastAPICloudConfig{}).FieldByName("Token"); ok {
		t.Fatal("token must not have a YAML source")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "whitespace", "equal"} {
			t.Run(fmt.Sprintf("trusted=%t/%s", trusted, mode), func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = "fastapi-cloud"
				cfg.FastAPICloud = FastAPICloudConfig{Token: "inert", APIURL: "https://example.invalid/api", AppID: "example-app", TeamID: "example-team"}
				cfg.credentialProvenance.fastAPICloudAPIURL = credentialSourceEnvironment
				cfg.credentialProvenance.fastAPICloudToken = credentialSourceEnvironment
				want := cfg.FastAPICloud
				wantSource := credentialSourceEnvironment
				body := "fastapiCloud:\n  token: ignored-inert-value\n"
				if mode != "omitted" {
					url, app, team := "null", "null", "null"
					if mode == "empty" {
						url, app, team = "''", "''", "''"
					}
					if mode == "whitespace" {
						url, app, team = "'  '", "'  '", "'  '"
						want.APIURL, want.AppID, want.TeamID = "  ", "  ", "  "
					}
					if mode == "equal" {
						url, app, team = want.APIURL, want.AppID, want.TeamID
					}
					if mode == "whitespace" || mode == "equal" {
						wantSource = credentialSourceForFile(trusted)
					}
					body += "  apiUrl: " + url + "\n  appId: " + app + "\n  teamId: " + team + "\n"
				}
				var file fileConfig
				if err := yaml.Unmarshal([]byte(body), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if cfg.FastAPICloud != want || cfg.credentialProvenance.fastAPICloudAPIURL != wantSource || cfg.credentialProvenance.fastAPICloudToken != credentialSourceEnvironment {
					t.Fatal("file acceptance/value/provenance mismatch")
				}
				if mode == "equal" {
					err := validateProviderCredentialDestination(cfg)
					if (err != nil) != !trusted {
						t.Fatalf("later destination policy trusted=%t error=%v", trusted, err)
					}
				}
			})
		}
	}
}

func TestFastAPICloudEnvironmentAcceptanceAndProvenance(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "whitespace", "equal", "token only", "URL only"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.FastAPICloud = FastAPICloudConfig{Token: "inert-prior", APIURL: "https://example.invalid/prior", AppID: "prior-app", TeamID: "prior-team"}
			cfg.credentialProvenance.fastAPICloudAPIURL, cfg.credentialProvenance.fastAPICloudToken = credentialSourceTrustedFile, credentialSourceTrustedFile
			want := cfg.FastAPICloud
			for _, item := range []struct {
				primary, alias, primaryValue, aliasValue string
				target                                   *string
			}{
				{"CRABBOX_FASTAPI_CLOUD_TOKEN", "FASTAPI_CLOUD_TOKEN", "inert-primary", "inert-alias", &want.Token},
				{"CRABBOX_FASTAPI_CLOUD_API_URL", "FASTAPI_CLOUD_API_URL", "https://example.invalid/primary", "https://example.invalid/alias", &want.APIURL},
				{"CRABBOX_FASTAPI_CLOUD_APP_ID", "FASTAPI_CLOUD_APP_ID", "primary-app", "alias-app", &want.AppID},
				{"CRABBOX_FASTAPI_CLOUD_TEAM_ID", "FASTAPI_CLOUD_TEAM_ID", "primary-team", "alias-team", &want.TeamID},
			} {
				primary, alias := item.primaryValue, item.aliasValue
				switch mode {
				case "alias":
					primary = ""
					*item.target = alias
				case "empty":
					primary, alias = "", ""
				case "whitespace":
					primary = "  "
					*item.target = primary
				case "equal":
					primary = *item.target
				default:
					*item.target = primary
				}
				if mode == "token only" || mode == "URL only" {
					accept := (mode == "token only" && item.primary == "CRABBOX_FASTAPI_CLOUD_TOKEN") || (mode == "URL only" && item.primary == "CRABBOX_FASTAPI_CLOUD_API_URL")
					if !accept {
						primary, alias = "", ""
						switch item.primary {
						case "CRABBOX_FASTAPI_CLOUD_TOKEN":
							*item.target = cfg.FastAPICloud.Token
						case "CRABBOX_FASTAPI_CLOUD_API_URL":
							*item.target = cfg.FastAPICloud.APIURL
						case "CRABBOX_FASTAPI_CLOUD_APP_ID":
							*item.target = cfg.FastAPICloud.AppID
						case "CRABBOX_FASTAPI_CLOUD_TEAM_ID":
							*item.target = cfg.FastAPICloud.TeamID
						}
					}
				}
				t.Setenv(item.primary, primary)
				t.Setenv(item.alias, alias)
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			wantSource := credentialSourceEnvironment
			if mode == "empty" {
				wantSource = credentialSourceTrustedFile
			}
			wantURLSource, wantTokenSource := wantSource, wantSource
			if mode == "token only" {
				wantURLSource = credentialSourceTrustedFile
			}
			if mode == "URL only" {
				wantTokenSource = credentialSourceTrustedFile
			}
			if cfg.FastAPICloud != want || cfg.credentialProvenance.fastAPICloudAPIURL != wantURLSource || cfg.credentialProvenance.fastAPICloudToken != wantTokenSource {
				t.Fatal("environment acceptance/value/provenance mismatch")
			}
		})
	}
}

func TestCloudRunSandboxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.CloudRunSandbox.CLIPath != "/usr/local/gcp/bin/sandbox" || cfg.CloudRunSandbox.Workdir != "/tmp/crabbox" || !cfg.CloudRunSandbox.Write || cfg.CloudRunSandbox.Rootfs != "/" {
		t.Fatalf("cloudRunSandbox defaults not applied: %#v", cfg.CloudRunSandbox)
	}
	var file fileConfig
	if err := yaml.Unmarshal([]byte("provider: cloud-run-sandbox\ncloudRunSandbox:\n  cliPath: /opt/sandbox\n  workdir: /workspace/app\n  allowEgress: true\n  write: false\n  rootfs: /var/rootfs\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "cloud-run-sandbox" || cfg.CloudRunSandbox.CLIPath != "/opt/sandbox" || cfg.CloudRunSandbox.Workdir != "/workspace/app" || !cfg.CloudRunSandbox.AllowEgress || cfg.CloudRunSandbox.Write || cfg.CloudRunSandbox.Rootfs != "/var/rootfs" {
		t.Fatalf("file cloudRunSandbox config not applied: %#v", cfg.CloudRunSandbox)
	}

	// Empty file section must not clear previously applied values.
	applyFileConfig(&cfg, fileConfig{CloudRunSandbox: &fileCloudRunSandboxConfig{}})
	if cfg.CloudRunSandbox.CLIPath != "/opt/sandbox" || cfg.CloudRunSandbox.Workdir != "/workspace/app" || !cfg.CloudRunSandbox.AllowEgress || cfg.CloudRunSandbox.Write || cfg.CloudRunSandbox.Rootfs != "/var/rootfs" {
		t.Fatalf("empty file cloudRunSandbox config cleared existing values: %#v", cfg.CloudRunSandbox)
	}

	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL", "https://gateway.example.run.app")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_CLI", "/usr/local/bin/sandbox")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_WORKDIR", "/tmp/env-work")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_ALLOW_EGRESS", "false")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_WRITE", "true")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_ROOTFS", "/")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.CloudRunSandbox.GatewayURL != "https://gateway.example.run.app" || cfg.CloudRunSandbox.CLIPath != "/usr/local/bin/sandbox" || cfg.CloudRunSandbox.Workdir != "/tmp/env-work" || cfg.CloudRunSandbox.AllowEgress || !cfg.CloudRunSandbox.Write || cfg.CloudRunSandbox.Rootfs != "/" {
		t.Fatalf("env cloudRunSandbox config not applied: %#v", cfg.CloudRunSandbox)
	}

	// Alternate env names should also apply gateway URL and CLI binary.
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL", "")
	t.Setenv("CLOUD_RUN_SANDBOX_URL", "https://alt.example.run.app")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_CLI", "")
	t.Setenv("CLOUD_RUN_SANDBOX_BINARY", "/bin/sandbox-alt")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv alternate err=%v", err)
	}
	if cfg.CloudRunSandbox.GatewayURL != "https://alt.example.run.app" || cfg.CloudRunSandbox.CLIPath != "/bin/sandbox-alt" {
		t.Fatalf("alternate env cloudRunSandbox config not applied: %#v", cfg.CloudRunSandbox)
	}
}

func TestCloudRunSandboxFilePresenceAndAuthority(t *testing.T) {
	for _, name := range []string{"GatewayURL", "Secret", "AuthToken"} {
		if _, ok := reflect.TypeOf(fileCloudRunSandboxConfig{}).FieldByName(name); ok {
			t.Fatalf("unexpected YAML field %s", name)
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "whitespace"} {
			t.Run(fmt.Sprintf("trusted=%t/%s", trusted, mode), func(t *testing.T) {
				cfg := baseConfig()
				cfg.CloudRunSandbox = CloudRunSandboxConfig{GatewayURL: "https://example.invalid/prior", CLIPath: "/opt/prior", Workdir: "/tmp/prior", Rootfs: "/prior", AllowEgress: true, Write: true}
				want := cfg.CloudRunSandbox
				body := "cloudRunSandbox:\n  gatewayUrl: https://example.invalid/file\n  secret: ignored\n  authToken: ignored\n"
				if mode != "omitted" {
					value := "null"
					if mode == "empty" {
						value = "''"
					}
					if mode == "whitespace" {
						value = "'  '"
						want.CLIPath, want.Workdir, want.Rootfs = "  ", "  ", "  "
					}
					body += "  cliPath: " + value + "\n  workdir: " + value + "\n  rootfs: " + value + "\n"
					boolValue := "null"
					if mode != "null" {
						boolValue = "false"
						want.AllowEgress, want.Write = false, false
					}
					body += "  allowEgress: " + boolValue + "\n  write: " + boolValue + "\n"
				}
				var file fileConfig
				if err := yaml.Unmarshal([]byte(body), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if cfg.CloudRunSandbox != want {
					t.Fatalf("got=%#v want=%#v", cfg.CloudRunSandbox, want)
				}
			})
		}
	}
}

func TestCloudRunSandboxEnvironmentAliasesAndBooleanFallback(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "whitespace"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.CloudRunSandbox.GatewayURL, cfg.CloudRunSandbox.CLIPath = "https://example.invalid/prior", "/opt/prior"
			primaryURL, primaryCLI := "https://example.invalid/primary", "/opt/primary"
			aliasURL, aliasCLI := "https://example.invalid/alias", "/opt/alias"
			wantURL, wantCLI := primaryURL, primaryCLI
			if mode == "alias" {
				primaryURL, primaryCLI = "", ""
				wantURL, wantCLI = aliasURL, aliasCLI
			}
			if mode == "empty" {
				primaryURL, primaryCLI, aliasURL, aliasCLI = "", "", "", ""
				wantURL, wantCLI = cfg.CloudRunSandbox.GatewayURL, cfg.CloudRunSandbox.CLIPath
			}
			if mode == "whitespace" {
				primaryURL, primaryCLI, wantURL, wantCLI = "  ", "  ", "  ", "  "
			}
			t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL", primaryURL)
			t.Setenv("CLOUD_RUN_SANDBOX_URL", aliasURL)
			t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_CLI", primaryCLI)
			t.Setenv("CLOUD_RUN_SANDBOX_BINARY", aliasCLI)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.CloudRunSandbox.GatewayURL != wantURL || cfg.CloudRunSandbox.CLIPath != wantCLI {
				t.Fatalf("alias precedence=%#v", cfg.CloudRunSandbox)
			}
		})
	}
	for _, prior := range []bool{false, true} {
		for _, raw := range []string{"", "invalid", "no", "yes"} {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_ALLOW_EGRESS", raw)
			t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_WRITE", raw)
			cfg := baseConfig()
			cfg.CloudRunSandbox.AllowEgress, cfg.CloudRunSandbox.Write = prior, prior
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			want := prior
			if raw == "no" {
				want = false
			}
			if raw == "yes" {
				want = true
			}
			if cfg.CloudRunSandbox.AllowEgress != want || cfg.CloudRunSandbox.Write != want {
				t.Fatalf("bool %q prior=%t got=%#v", raw, prior, cfg.CloudRunSandbox)
			}
		}
	}
}

func TestDigitalOceanConfigFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var digitalOceanFile fileDigitalOceanConfig
	if err := yaml.Unmarshal([]byte(`region: sfo3
image: ubuntu-24-04-x64
vpc: vpc-file
sshCIDRs: [203.0.113.0/24]`), &digitalOceanFile); err != nil {
		t.Fatal(err)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider:     "digitalocean",
		DigitalOcean: &digitalOceanFile,
	})
	if cfg.Provider != "digitalocean" || cfg.DigitalOcean.Region != "sfo3" || cfg.Location == "sfo3" || cfg.DigitalOcean.Image != "ubuntu-24-04-x64" || cfg.Image == "ubuntu-24-04-x64" || cfg.DigitalOcean.VPCUUID != "vpc-file" {
		t.Fatalf("file digitalocean config not applied: cfg=%#v do=%#v", cfg, cfg.DigitalOcean)
	}
	if strings.Join(cfg.DigitalOcean.SSHCIDRs, ",") != "203.0.113.0/24" {
		t.Fatalf("file digitalocean ssh cidrs=%v", cfg.DigitalOcean.SSHCIDRs)
	}

	t.Setenv("CRABBOX_DIGITALOCEAN_REGION", "nyc3")
	t.Setenv("CRABBOX_DIGITALOCEAN_IMAGE", "ubuntu-22-04-x64")
	t.Setenv("CRABBOX_DIGITALOCEAN_VPC", "vpc-env")
	t.Setenv("CRABBOX_DIGITALOCEAN_SSH_CIDRS", "198.51.100.0/24,2001:db8::/64")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.DigitalOcean.Region != "nyc3" || cfg.Location == "nyc3" || cfg.DigitalOcean.Image != "ubuntu-22-04-x64" || cfg.Image == "ubuntu-22-04-x64" || cfg.DigitalOcean.VPCUUID != "vpc-env" {
		t.Fatalf("env digitalocean config not applied: cfg=%#v do=%#v", cfg, cfg.DigitalOcean)
	}
	if strings.Join(cfg.DigitalOcean.SSHCIDRs, ",") != "198.51.100.0/24,2001:db8::/64" {
		t.Fatalf("env digitalocean ssh cidrs=%v", cfg.DigitalOcean.SSHCIDRs)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	base := baseConfig()
	if cfg.Location != base.Location || cfg.Image != base.Image {
		t.Fatalf("digitalocean defaults leaked into generic fields: cfg=%#v", cfg)
	}
}

func TestVultrConfigFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var vultrFile fileVultrConfig
	if err := yaml.Unmarshal([]byte(`region: ewr
os: "2284"
image: image-file
snapshot: snapshot-file
firewallGroup: fw-file
vpcIds: [vpc-file-a, vpc-file-b]
sshCIDRs: [203.0.113.0/24]
userScheme: limited`), &vultrFile); err != nil {
		t.Fatal(err)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "vultr",
		Vultr:    &vultrFile,
	})
	if cfg.Provider != "vultr" ||
		cfg.Vultr.Region != "ewr" ||
		cfg.Vultr.OS != "2284" ||
		cfg.Vultr.Image != "image-file" ||
		cfg.Vultr.Snapshot != "snapshot-file" ||
		cfg.Vultr.FirewallGroup != "fw-file" ||
		cfg.Vultr.UserScheme != "limited" ||
		cfg.Location == "ewr" ||
		cfg.Image == "image-file" {
		t.Fatalf("file vultr config not applied or leaked: cfg=%#v vultr=%#v", cfg, cfg.Vultr)
	}
	if strings.Join(cfg.Vultr.VPCIDs, ",") != "vpc-file-a,vpc-file-b" {
		t.Fatalf("file vultr vpc ids=%v", cfg.Vultr.VPCIDs)
	}
	if strings.Join(cfg.Vultr.SSHCIDRs, ",") != "203.0.113.0/24" {
		t.Fatalf("file vultr ssh cidrs=%v", cfg.Vultr.SSHCIDRs)
	}

	t.Setenv("CRABBOX_VULTR_REGION", "lax")
	t.Setenv("CRABBOX_VULTR_OS", "1743")
	t.Setenv("CRABBOX_VULTR_IMAGE", "image-env")
	t.Setenv("CRABBOX_VULTR_SNAPSHOT", "snapshot-env")
	t.Setenv("CRABBOX_VULTR_FIREWALL_GROUP", "fw-env")
	t.Setenv("CRABBOX_VULTR_VPC_IDS", "vpc-env-a, vpc-env-b")
	t.Setenv("CRABBOX_VULTR_SSH_CIDRS", "198.51.100.0/24,2001:db8::/64")
	t.Setenv("CRABBOX_VULTR_USER_SCHEME", "root")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Vultr.Region != "lax" ||
		cfg.Vultr.OS != "1743" ||
		cfg.Vultr.Image != "image-env" ||
		cfg.Vultr.Snapshot != "snapshot-env" ||
		cfg.Vultr.FirewallGroup != "fw-env" ||
		cfg.Vultr.UserScheme != "root" ||
		cfg.Location == "lax" ||
		cfg.Image == "image-env" {
		t.Fatalf("env vultr config not applied or leaked: cfg=%#v vultr=%#v", cfg, cfg.Vultr)
	}
	if strings.Join(cfg.Vultr.VPCIDs, ",") != "vpc-env-a,vpc-env-b" {
		t.Fatalf("env vultr vpc ids=%v", cfg.Vultr.VPCIDs)
	}
	if strings.Join(cfg.Vultr.SSHCIDRs, ",") != "198.51.100.0/24,2001:db8::/64" {
		t.Fatalf("env vultr ssh cidrs=%v", cfg.Vultr.SSHCIDRs)
	}
}

func TestVultrDefaultsAndIsolation(t *testing.T) {
	clearConfigEnv(t)
	base := baseConfig()
	cfg := baseConfig()
	cfg.Provider = "vultr"
	cfg.OSImage = "ubuntu:26.04"
	cfg.osImageExplicit = true

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	if cfg.Vultr.Region != "ewr" || cfg.Vultr.UserScheme != "root" {
		t.Fatalf("vultr defaults=%#v", cfg.Vultr)
	}
	if cfg.TargetOS != targetLinux || cfg.WorkRoot != defaultPOSIXWorkRoot || cfg.SSHUser != "root" || cfg.SSHPort != "22" || len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("vultr direct defaults not applied: %#v", cfg)
	}
	if cfg.Location != base.Location || cfg.Image != base.Image {
		t.Fatalf("vultr defaults leaked into generic fields: location=%q image=%q", cfg.Location, cfg.Image)
	}
	if cfg.Vultr.OS != "" || cfg.Vultr.Image != "" {
		t.Fatalf("portable OS must not silently map to unverified Vultr boot source: %#v", cfg.Vultr)
	}
}

func TestLinuxProviderConnectionDefaultsPreserveExplicitValues(t *testing.T) {
	base := baseConfig()
	for _, provider := range []struct {
		name, user, port string
		clearFallbacks   bool
	}{
		{"digitalocean", base.SSHUser, base.SSHPort, false},
		{"vultr", "root", "22", true},
		{"linode", base.SSHUser, base.SSHPort, false},
		{"lambda", "ubuntu", "22", true},
		{"nebius", "builder", base.SSHPort, false},
		{"ovh", base.SSHUser, base.SSHPort, false},
		{"scaleway", "root", "22", false},
		{"tencentcloud", "ubuntu", "22", true},
	} {
		for _, explicit := range []string{"none", "base", "custom"} {
			t.Run(provider.name+"/"+explicit, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = provider.name
				cfg.TargetOS, cfg.WindowsMode = targetMacOS, windowsModeWSL2
				cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort = "/previous", "previous", "2999"
				cfg.Nebius.User = "builder"
				cfg.SSHFallbackPorts = []string{"2022"}
				cfg.sshFallbackPortsExplicit = true
				cfg.explicitSSHFallbackPorts = []string{"2022"}
				wantUser, wantPort, wantRoot := provider.user, provider.port, defaultPOSIXWorkRoot
				if explicit != "none" {
					wantUser, wantPort, wantRoot = base.SSHUser, base.SSHPort, base.WorkRoot
					if explicit == "custom" {
						wantUser, wantPort, wantRoot = "alice", "2200", "/srv/proof"
					}
					if err := applyFileConfig(&cfg, fileConfig{
						WorkRoot: wantRoot,
						SSH:      &fileSSHConfig{User: wantUser, Port: wantPort},
						Windows:  &fileWindowsConfig{Mode: windowsModeNormal},
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := applyProviderConfigDefaults(&cfg); err != nil {
					t.Fatal(err)
				}
				got := []string{cfg.TargetOS, cfg.WindowsMode, cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort}
				want := []string{targetLinux, windowsModeNormal, wantRoot, wantUser, wantPort}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("connection = %q, want %q", got, want)
				}
				wantFallbacks := "2022"
				if provider.clearFallbacks {
					wantFallbacks = ""
				}
				if got := strings.Join(cfg.SSHFallbackPorts, ","); got != wantFallbacks {
					t.Fatalf("fallback ports = %q, want %q", got, wantFallbacks)
				}
			})
		}
	}
}

func TestVultrDefaultsPreserveExplicitGenericValues(t *testing.T) {
	cfg := baseConfig()
	var vultrFile fileVultrConfig
	if err := yaml.Unmarshal([]byte(`region: sjc`), &vultrFile); err != nil {
		t.Fatal(err)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "vultr",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Windows:  &fileWindowsConfig{Mode: windowsModeNormal},
		Vultr:    &vultrFile,
	})

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/srv/crabbox" || cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("vultr explicit generic values changed: %#v", cfg)
	}
	if cfg.Vultr.Region != "sjc" {
		t.Fatalf("Vultr.Region=%q", cfg.Vultr.Region)
	}
}

func TestOVHConfigFileEnvAndDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("provider: ovh\novh:\n  endpoint: https://ca.api.ovhcloud.com/1.0\n  projectId: project-file\n  region: BHS5\n  image: Ubuntu 22.04\n  flavor: b3-16\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "ovh" || cfg.OVH.Endpoint != "https://ca.api.ovhcloud.com/1.0" || cfg.OVH.ProjectID != "project-file" || cfg.OVH.Region != "BHS5" || cfg.OVH.Image != "Ubuntu 22.04" || cfg.OVH.Flavor != "b3-16" {
		t.Fatalf("file ovh config not applied: %#v", cfg.OVH)
	}
	if !cfg.ovhImageExplicit {
		t.Fatal("file ovh image should mark ovh image explicit")
	}

	t.Setenv("OVH_ENDPOINT", "https://api.us.ovhcloud.com/1.0")
	t.Setenv("CRABBOX_OVH_PROJECT_ID", "project-env")
	t.Setenv("CRABBOX_OVH_REGION", "GRA11")
	t.Setenv("CRABBOX_OVH_IMAGE", "Ubuntu 24.04")
	t.Setenv("CRABBOX_OVH_FLAVOR", "b3-8")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.OVH.Endpoint != "https://api.us.ovhcloud.com/1.0" || cfg.OVH.ProjectID != "project-env" || cfg.OVH.Region != "GRA11" || cfg.OVH.Image != "Ubuntu 24.04" || cfg.OVH.Flavor != "b3-8" {
		t.Fatalf("env ovh config not applied: %#v", cfg.OVH)
	}
	if !cfg.ovhImageExplicit {
		t.Fatal("env ovh image should mark ovh image explicit")
	}
	cfg.OVH.Image = ""
	cfg.OVH.Flavor = ""
	cfg.ovhImageExplicit = false
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	if cfg.TargetOS != targetLinux || cfg.OVH.Image != "Ubuntu 24.04" || cfg.OVH.Flavor != "b3-8" {
		t.Fatalf("ovh defaults not applied: cfg=%#v ovh=%#v", cfg, cfg.OVH)
	}
	if cfg.ovhImageExplicit {
		t.Fatal("default ovh image should not mark ovh image explicit")
	}
}

func TestOVHConfigShowRedactsEnvCredentials(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("OVH_APPLICATION_KEY", "app-key-secret")
	t.Setenv("OVH_APPLICATION_SECRET", "application-secret-value")
	t.Setenv("OVH_CONSUMER_KEY", "consumer-key-value")
	cfg := Config{
		Provider: "ovh",
		OVH: OVHConfig{
			Endpoint:  "https://user:pass@api.us.ovhcloud.com/1.0",
			ProjectID: "project-test",
			Region:    "GRA11",
			Image:     "Ubuntu 24.04",
			Flavor:    "b3-8",
		},
	}
	view := configShowView(cfg)["ovh"].(map[string]any)
	rendered := fmt.Sprint(view)
	for _, secret := range []string{"app-key-secret", "application-secret-value", "consumer-key-value", "user:pass"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("config show leaked %q in %s", secret, rendered)
		}
	}
	if view["auth"] != "configured" {
		t.Fatalf("auth=%v, want configured", view["auth"])
	}
	t.Setenv("OVH_APPLICATION_SECRET", "")
	t.Setenv("OVH_CONSUMER_KEY", "")
	if got := configShowView(cfg)["ovh"].(map[string]any)["auth"]; got != "partial" {
		t.Fatalf("partial auth=%v, want partial", got)
	}
}

func TestScalewayConfigFileEnvAndDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("provider: scaleway\nscaleway:\n  region: nl-ams\n  zone: nl-ams-1\n  image: ubuntu_jammy\n  type: DEV1-M\n  projectId: project-file\n  organizationId: org-file\n  securityGroup: sg-file\n  sshCIDRs: [203.0.113.0/24]\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "scaleway" || cfg.Scaleway.Region != "nl-ams" || cfg.Scaleway.Zone != "nl-ams-1" || cfg.Scaleway.Image != "ubuntu_jammy" || cfg.Scaleway.Type != "DEV1-M" || cfg.Scaleway.ProjectID != "project-file" || cfg.Scaleway.OrganizationID != "org-file" || cfg.Scaleway.SecurityGroup != "sg-file" {
		t.Fatalf("file scaleway config not applied: %#v", cfg.Scaleway)
	}
	if strings.Join(cfg.Scaleway.SSHCIDRs, ",") != "203.0.113.0/24" {
		t.Fatalf("file scaleway ssh cidrs=%v", cfg.Scaleway.SSHCIDRs)
	}
	if !cfg.scalewayRegionExplicit || !cfg.scalewayZoneExplicit || !cfg.scalewayImageExplicit || !cfg.scalewayTypeExplicit {
		t.Fatalf("file scaleway location/image/type should be explicit: region=%t zone=%t image=%t type=%t", cfg.scalewayRegionExplicit, cfg.scalewayZoneExplicit, cfg.scalewayImageExplicit, cfg.scalewayTypeExplicit)
	}

	t.Setenv("CRABBOX_SCALEWAY_REGION", "fr-par")
	t.Setenv("CRABBOX_SCALEWAY_ZONE", "fr-par-2")
	t.Setenv("CRABBOX_SCALEWAY_IMAGE", "ubuntu_noble")
	t.Setenv("CRABBOX_SCALEWAY_TYPE", "DEV1-S")
	t.Setenv("CRABBOX_SCALEWAY_PROJECT_ID", "project-env")
	t.Setenv("CRABBOX_SCALEWAY_ORGANIZATION_ID", "org-env")
	t.Setenv("CRABBOX_SCALEWAY_SECURITY_GROUP", "sg-env")
	t.Setenv("CRABBOX_SCALEWAY_SSH_CIDRS", "198.51.100.0/24,2001:db8::/64")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Scaleway.Region != "fr-par" || cfg.Scaleway.Zone != "fr-par-2" || cfg.Scaleway.Image != "ubuntu_noble" || cfg.Scaleway.Type != "DEV1-S" || cfg.Scaleway.ProjectID != "project-env" || cfg.Scaleway.OrganizationID != "org-env" || cfg.Scaleway.SecurityGroup != "sg-env" {
		t.Fatalf("env scaleway config not applied: %#v", cfg.Scaleway)
	}
	if strings.Join(cfg.Scaleway.SSHCIDRs, ",") != "198.51.100.0/24,2001:db8::/64" {
		t.Fatalf("env scaleway ssh cidrs=%v", cfg.Scaleway.SSHCIDRs)
	}
	if !cfg.scalewayRegionExplicit || !cfg.scalewayZoneExplicit {
		t.Fatalf("env scaleway location should be explicit: region=%t zone=%t", cfg.scalewayRegionExplicit, cfg.scalewayZoneExplicit)
	}

	cfg.Scaleway = ScalewayConfig{}
	cfg.scalewayRegionExplicit = false
	cfg.scalewayZoneExplicit = false
	cfg.scalewayImageExplicit = false
	cfg.scalewayTypeExplicit = false
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	if cfg.TargetOS != targetLinux || cfg.Scaleway.Region != "fr-par" || cfg.Scaleway.Zone != "fr-par-1" || cfg.Scaleway.Image != "ubuntu_noble" || cfg.Scaleway.Type != "DEV1-S" || cfg.SSHUser != "root" || cfg.SSHPort != "22" || cfg.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("scaleway defaults not applied: cfg=%#v scaleway=%#v", cfg, cfg.Scaleway)
	}
	if cfg.scalewayRegionExplicit || cfg.scalewayZoneExplicit || cfg.scalewayImageExplicit || cfg.scalewayTypeExplicit {
		t.Fatalf("default scaleway values should not be explicit: region=%t zone=%t image=%t type=%t", cfg.scalewayRegionExplicit, cfg.scalewayZoneExplicit, cfg.scalewayImageExplicit, cfg.scalewayTypeExplicit)
	}
}

func TestScalewayPortableOSSelection(t *testing.T) {
	t.Run("supported selector maps to provider image", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "scaleway"
		cfg.OSImage = "ubuntu:24.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Scaleway.Image != "ubuntu_noble" {
			t.Fatalf("Scaleway.Image=%q", cfg.Scaleway.Image)
		}
	})

	t.Run("unsupported selector is deferred to acquisition", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "scaleway"
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Scaleway.Image != "" {
			t.Fatalf("Scaleway.Image=%q, want unresolved provider image", cfg.Scaleway.Image)
		}
	})

	t.Run("provider image overrides portable selector", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "scaleway"
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		cfg.Scaleway.Image = "custom-image"
		cfg.scalewayImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Scaleway.Image != "custom-image" {
			t.Fatalf("Scaleway.Image=%q", cfg.Scaleway.Image)
		}
	})
}

func TestScalewayDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "scaleway"
	cfg.explicitWorkRoot = "/work/custom"
	cfg.explicitSSHUser = "ubuntu"
	cfg.explicitSSHPort = "2222"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/work/custom" || cfg.SSHUser != "ubuntu" || cfg.SSHPort != "2222" {
		t.Fatalf("explicit generic values not preserved: work_root=%q ssh=%s:%s", cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort)
	}
}

func TestScalewayEnvDoesNotMutateGenericFieldsForOtherProviders(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "hetzner"
	originalLocation := cfg.Location
	originalImage := cfg.Image
	t.Setenv("CRABBOX_SCALEWAY_REGION", "fr-par")
	t.Setenv("CRABBOX_SCALEWAY_IMAGE", "ubuntu_noble")
	t.Setenv("CRABBOX_SCALEWAY_TYPE", "DEV1-S")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Scaleway.Region != "fr-par" || cfg.Scaleway.Image != "ubuntu_noble" || cfg.Scaleway.Type != "DEV1-S" {
		t.Fatalf("scaleway env not stored: %#v", cfg.Scaleway)
	}
	if cfg.Location != originalLocation || cfg.Image != originalImage {
		t.Fatalf("scaleway env leaked into generic fields: location=%q image=%q", cfg.Location, cfg.Image)
	}
}

func TestRepoConfigCannotRedirectInheritedOVHCredentials(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.OVH.Endpoint = "https://api.us.ovhcloud.com/1.0"
	var file fileConfig
	if err := yaml.Unmarshal([]byte("ovh:\n  endpoint: https://attacker.example.test/1.0\n  projectId: project-from-repo\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.OVH.Endpoint != "https://api.us.ovhcloud.com/1.0" {
		t.Fatalf("repo config redirected OVH endpoint: %#v", cfg.OVH)
	}
	if cfg.OVH.ProjectID != "project-from-repo" {
		t.Fatalf("non-secret OVH project setting was not applied: %#v", cfg.OVH)
	}
}

func TestDigitalOceanPortableOSSelection(t *testing.T) {
	t.Run("supported selector maps to provider image", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		cfg.OSImage = "ubuntu:24.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
			t.Fatalf("DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
	})

	t.Run("unsupported selector is deferred to acquisition", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
			t.Fatalf("default DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "" {
			t.Fatalf("DigitalOcean.Image=%q, want unresolved provider image", cfg.DigitalOcean.Image)
		}
	})

	t.Run("provider image overrides portable selector", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		cfg.DigitalOcean.Image = "custom-image"
		cfg.digitalOceanImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "custom-image" {
			t.Fatalf("DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
	})
}

func TestDigitalOceanUnsupportedPortableOSDoesNotBlockCLIOverrides(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("provider: digitalocean\nos: ubuntu:26.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("portable os override", func(t *testing.T) {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		fs := newFlagSet("test", io.Discard)
		values := registerLeaseCreateFlags(fs, cfg)
		if err := parseFlags(fs, []string{"--os", "ubuntu:24.04"}); err != nil {
			t.Fatal(err)
		}
		if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
			t.Fatalf("DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
	})

	t.Run("provider override", func(t *testing.T) {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		fs := newFlagSet("test", io.Discard)
		values := registerLeaseCreateFlags(fs, cfg)
		if err := parseFlags(fs, []string{"--provider", "aws"}); err != nil {
			t.Fatal(err)
		}
		if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "aws" {
			t.Fatalf("Provider=%q", cfg.Provider)
		}
	})
}

func TestDigitalOceanEnvDoesNotMutateGenericFieldsForOtherProviders(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "hetzner"
	originalLocation := cfg.Location
	originalImage := cfg.Image
	t.Setenv("CRABBOX_DIGITALOCEAN_REGION", "nyc3")
	t.Setenv("CRABBOX_DIGITALOCEAN_IMAGE", "ubuntu-22-04-x64")

	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.DigitalOcean.Region != "nyc3" || cfg.DigitalOcean.Image != "ubuntu-22-04-x64" {
		t.Fatalf("digitalocean env not stored: do=%#v", cfg.DigitalOcean)
	}
	if cfg.Location != originalLocation || cfg.Image != originalImage {
		t.Fatalf("digitalocean env leaked into generic fields: location=%q image=%q", cfg.Location, cfg.Image)
	}
}

func TestDigitalOceanDefaultsPreserveExplicitGenericBaseValues(t *testing.T) {
	clearConfigEnv(t)
	base := baseConfig()
	cfg := baseConfig()
	var digitalOceanFile fileDigitalOceanConfig
	if err := yaml.Unmarshal([]byte(`region: sfo3
image: ubuntu-24-04-x64`), &digitalOceanFile); err != nil {
		t.Fatal(err)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "digitalocean",
		SSH: &fileSSHConfig{
			User: base.SSHUser,
			Port: base.SSHPort,
		},
		Hetzner: &fileHetznerConfig{
			Location: base.Location,
			Image:    base.Image,
		},
		DigitalOcean: &digitalOceanFile,
	})

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	if cfg.Location != base.Location {
		t.Fatalf("Location=%q want explicit %q", cfg.Location, base.Location)
	}
	if cfg.Image != base.Image {
		t.Fatalf("Image=%q want explicit %q", cfg.Image, base.Image)
	}
	if cfg.SSHUser != base.SSHUser || cfg.SSHPort != base.SSHPort {
		t.Fatalf("SSH=%s@:%s want explicit %s@:%s", cfg.SSHUser, cfg.SSHPort, base.SSHUser, base.SSHPort)
	}
	if cfg.DigitalOcean.Region != "sfo3" || cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
		t.Fatalf("DigitalOcean=%#v", cfg.DigitalOcean)
	}
}

func TestDigitalOceanDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "tart",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Windows:  &fileWindowsConfig{Mode: windowsModeNormal},
	})

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != cfg.Tart.WorkRoot {
		t.Fatalf("Tart WorkRoot=%q want provider root %q before override", cfg.WorkRoot, cfg.Tart.WorkRoot)
	}
	cfg.WindowsMode = windowsModeWSL2
	cfg.Provider = "digitalocean"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/srv/crabbox" {
		t.Fatalf("DigitalOcean WorkRoot=%q want explicit generic root", cfg.WorkRoot)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("DigitalOcean SSHUser=%q want explicit generic user", cfg.SSHUser)
	}
	if cfg.SSHPort != "2200" {
		t.Fatalf("DigitalOcean SSHPort=%q want explicit generic port", cfg.SSHPort)
	}
	if cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("DigitalOcean WindowsMode=%q want explicit generic mode", cfg.WindowsMode)
	}
}

func TestDigitalOceanDefaultsIgnoreStaticProviderOverlays(t *testing.T) {
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "ssh",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Static: &fileStaticConfig{
			User:     "builder",
			Port:     "2202",
			WorkRoot: "/srv/static",
		},
	})
	normalizeTargetConfig(&cfg)
	if cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WorkRoot != "/srv/static" {
		t.Fatalf("static source settings user=%q port=%q root=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot)
	}

	cfg.Provider = "digitalocean"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WorkRoot != "/srv/crabbox" {
		t.Fatalf("DigitalOcean settings user=%q port=%q root=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot)
	}
}

func TestDigitalOceanDefaultsDoNotLeakAcrossProviderOverride(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "digitalocean"
	wantLocation := cfg.Location
	wantImage := cfg.Image
	wantSSHUser := cfg.SSHUser
	wantSSHPort := cfg.SSHPort
	wantFallbackPorts := append([]string(nil), cfg.SSHFallbackPorts...)

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("digitalocean defaults: %v", err)
	}
	cfg.Provider = "hetzner"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("hetzner defaults: %v", err)
	}

	if cfg.Location != wantLocation || cfg.Image != wantImage ||
		cfg.SSHUser != wantSSHUser || cfg.SSHPort != wantSSHPort ||
		strings.Join(cfg.SSHFallbackPorts, ",") != strings.Join(wantFallbackPorts, ",") {
		t.Fatalf("digitalocean defaults leaked after provider override: %#v", cfg)
	}
}

func TestProviderOverrideRecomputesInferredTarget(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "cloudflare-dynamic-workers"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetWorkerRuntime {
		t.Fatalf("dynamic workers target=%q", cfg.TargetOS)
	}

	cfg.Provider = "hetzner"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux {
		t.Fatalf("hetzner target after override=%q, want linux", cfg.TargetOS)
	}
	base := baseConfig()
	if cfg.SSHUser != base.SSHUser || cfg.SSHPort != base.SSHPort ||
		strings.Join(cfg.SSHFallbackPorts, ",") != strings.Join(base.SSHFallbackPorts, ",") ||
		cfg.WorkRoot != base.WorkRoot || cfg.ServerType == cfg.Tart.Image {
		t.Fatalf("tart defaults leaked after provider override: %#v", cfg)
	}

	cfg.Provider = "tart"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetMacOS {
		t.Fatalf("tart target=%q", cfg.TargetOS)
	}

	cfg.Provider = "cloudflare-dynamic-workers"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetWorkerRuntime {
		t.Fatalf("dynamic workers target after tart override=%q", cfg.TargetOS)
	}
}

func TestProviderFlagOverrideRecomputesTargetBeforeAWSDefaults(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetMacOS {
		t.Fatalf("tart target=%q", cfg.TargetOS)
	}

	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--provider", "aws"}); err != nil {
		t.Fatal(err)
	}
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "cbx_existing"); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux {
		t.Fatalf("aws target=%q, want linux", cfg.TargetOS)
	}
	if cfg.ServerType == "mac2.metal" {
		t.Fatalf("aws server type retained macOS default: %q", cfg.ServerType)
	}
	if cfg.Capacity.Market != "spot" {
		t.Fatalf("aws market=%q, want spot", cfg.Capacity.Market)
	}
}

func TestProviderOverridePreservesExplicitGenericFields(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	cfg.SSHUser = "alice"
	cfg.explicitSSHUser = "alice"
	cfg.SSHPort = "2200"
	cfg.explicitSSHPort = "2200"
	cfg.SSHFallbackPorts = []string{"2222"}
	cfg.sshFallbackPortsExplicit = true
	cfg.explicitSSHFallbackPorts = []string{"2222"}
	cfg.WorkRoot = "/srv/work"
	cfg.explicitWorkRoot = "/srv/work"
	cfg.ServerType = "custom-type"
	cfg.ServerTypeExplicit = true
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}

	cfg.Provider = "hetzner"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "alice" || cfg.SSHPort != "2200" ||
		strings.Join(cfg.SSHFallbackPorts, ",") != "2222" ||
		cfg.WorkRoot != "/srv/work" || cfg.ServerType != "custom-type" {
		t.Fatalf("explicit generic fields changed after provider override: %#v", cfg)
	}
}

func TestProviderOverrideWithExplicitTargetResetsPreviousProviderDefaults(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = "hetzner"
	cfg.TargetOS = targetLinux
	cfg.targetExplicit = true
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	base := baseConfig()
	if cfg.SSHUser != base.SSHUser || cfg.SSHPort != base.SSHPort ||
		strings.Join(cfg.SSHFallbackPorts, ",") != strings.Join(base.SSHFallbackPorts, ",") ||
		cfg.WorkRoot != base.WorkRoot || cfg.ServerType == cfg.Tart.Image {
		t.Fatalf("tart defaults leaked through explicit target override: %#v", cfg)
	}
}

func TestProviderOverrideReappliesExplicitOSImageDefaults(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = "local-container"
	cfg.OSImage = "ubuntu:24.04"
	cfg.osImageExplicit = true
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.LocalContainer.Image != osImageSpecs["ubuntu:24.04"].ContainerName {
		t.Fatalf("provider override target=%q image=%q", cfg.TargetOS, cfg.LocalContainer.Image)
	}
}

func TestProviderOverrideResetsParallelsTemplateTarget(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = parallelsProvider
	cfg.Parallels.Template = "windows"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"windows": {
			TargetOS:    targetWindows,
			WindowsMode: windowsModeWSL2,
		},
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetWindows || !cfg.parallelsTemplateApplied {
		t.Fatalf("parallels target=%q applied=%t", cfg.TargetOS, cfg.parallelsTemplateApplied)
	}

	cfg.Provider = "hetzner"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.parallelsTemplateApplied {
		t.Fatalf("hetzner target=%q template_applied=%t", cfg.TargetOS, cfg.parallelsTemplateApplied)
	}
}

func TestProviderOverrideRestoresExplicitWindowsMode(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = parallelsProvider
	cfg.WindowsMode = windowsModeNormal
	cfg.explicitWindowsMode = windowsModeNormal
	cfg.Parallels.Template = "windows"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"windows": {
			TargetOS:    targetWindows,
			WindowsMode: windowsModeWSL2,
		},
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WindowsMode != windowsModeWSL2 {
		t.Fatalf("parallels windows mode=%q", cfg.WindowsMode)
	}

	cfg.Provider = "hetzner"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("hetzner target=%q windows_mode=%q", cfg.TargetOS, cfg.WindowsMode)
	}
}

func TestProviderSelectionDefersDefaultsUntilAfterFlagOverrides(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "tart"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Parallels.Template = "stale"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"valid": {TargetOS: targetLinux},
	}

	if err := prepareProviderSelection(&cfg, parallelsProvider); err != nil {
		t.Fatalf("provider selection validated stale defaults before flags: %v", err)
	}
	cfg.Parallels.Template = "valid"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("provider defaults after flag override: %v", err)
	}
}

func TestLinodeConfigFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var linodeFile fileLinodeConfig
	if err := yaml.Unmarshal([]byte(`region: us-sea
image: linode/ubuntu24.04
type: g6-standard-2
firewall: "12345"
sshCIDRs: [203.0.113.0/24]`), &linodeFile); err != nil {
		t.Fatal(err)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "linode",
		Linode:   &linodeFile,
	})
	if cfg.Provider != "linode" || cfg.Linode.Region != "us-sea" || cfg.Location == "us-sea" || cfg.Linode.Image != "linode/ubuntu24.04" || cfg.Image == "linode/ubuntu24.04" || cfg.Linode.Type != "g6-standard-2" || cfg.Linode.FirewallID != "12345" {
		t.Fatalf("file linode config not applied: cfg=%#v linode=%#v", cfg, cfg.Linode)
	}
	if strings.Join(cfg.Linode.SSHCIDRs, ",") != "203.0.113.0/24" {
		t.Fatalf("file linode ssh cidrs=%v", cfg.Linode.SSHCIDRs)
	}

	t.Setenv("CRABBOX_LINODE_REGION", "us-ord")
	t.Setenv("CRABBOX_LINODE_IMAGE", "private/999")
	t.Setenv("CRABBOX_LINODE_TYPE", "g6-nanode-1")
	t.Setenv("CRABBOX_LINODE_FIREWALL", "67890")
	t.Setenv("CRABBOX_LINODE_SSH_CIDRS", "198.51.100.0/24,2001:db8::/64")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Linode.Region != "us-ord" || cfg.Location == "us-ord" || cfg.Linode.Image != "private/999" || cfg.Image == "private/999" || cfg.Linode.Type != "g6-nanode-1" || cfg.Linode.FirewallID != "67890" {
		t.Fatalf("env linode config not applied: cfg=%#v linode=%#v", cfg, cfg.Linode)
	}
	if strings.Join(cfg.Linode.SSHCIDRs, ",") != "198.51.100.0/24,2001:db8::/64" {
		t.Fatalf("env linode ssh cidrs=%v", cfg.Linode.SSHCIDRs)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	base := baseConfig()
	if cfg.Location != base.Location || cfg.Image != base.Image {
		t.Fatalf("linode defaults leaked into generic fields: cfg=%#v", cfg)
	}
	if got := serverTypeForConfig(cfg); got != "g6-nanode-1" {
		t.Fatalf("serverTypeForConfig=%q", got)
	}
}

func TestLambdaProviderConfigDefaultsAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "lambda"
	if err := applyFileConfig(&cfg, fileConfig{Lambda: &fileLambdaConfig{
		Region:          "us-east-1",
		Type:            "gpu_1x_h100_sxm5",
		ImageFamily:     "lambda-stack-24-04",
		FirewallRuleset: "crabbox",
		SSHCIDRs:        []string{"203.0.113.0/24"},
		FilesystemNames: []string{"cache"},
		FilesystemMounts: []LambdaFilesystemMount{{
			Name:      "dataset",
			MountPath: "/mnt/dataset",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if cfg.Lambda.Region != "us-east-1" || cfg.Location == "us-east-1" || cfg.Lambda.Type != "gpu_1x_h100_sxm5" || cfg.Lambda.ImageFamily != "lambda-stack-24-04" || cfg.Lambda.FirewallRuleset != "crabbox" {
		t.Fatalf("file lambda config not applied: cfg=%#v lambda=%#v", cfg, cfg.Lambda)
	}
	if strings.Join(cfg.Lambda.SSHCIDRs, ",") != "203.0.113.0/24" || strings.Join(cfg.Lambda.FilesystemNames, ",") != "cache" || len(cfg.Lambda.FilesystemMounts) != 1 {
		t.Fatalf("file lambda lists not applied: %#v", cfg.Lambda)
	}

	t.Setenv("CRABBOX_LAMBDA_REGION", "us-south-1")
	t.Setenv("CRABBOX_LAMBDA_TYPE", "gpu_1x_a10")
	t.Setenv("CRABBOX_LAMBDA_IMAGE", "img-123")
	t.Setenv("CRABBOX_LAMBDA_IMAGE_FAMILY", "")
	t.Setenv("CRABBOX_LAMBDA_FIREWALL_RULESET", "agents")
	t.Setenv("CRABBOX_LAMBDA_SSH_CIDRS", "198.51.100.0/24,2001:db8::/64")
	t.Setenv("CRABBOX_LAMBDA_FILESYSTEM_NAMES", "cache,models")
	t.Setenv("CRABBOX_LAMBDA_FILESYSTEM_MOUNTS", "cache:/mnt/cache,models:/mnt/models")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Lambda.Region != "us-south-1" || cfg.Location == "us-south-1" || cfg.Lambda.Type != "gpu_1x_a10" || cfg.Lambda.Image != "img-123" || cfg.Lambda.ImageFamily != "" || cfg.Image == "img-123" || cfg.Lambda.FirewallRuleset != "agents" {
		t.Fatalf("env lambda config not applied: cfg=%#v lambda=%#v", cfg, cfg.Lambda)
	}
	if strings.Join(cfg.Lambda.SSHCIDRs, ",") != "198.51.100.0/24,2001:db8::/64" || strings.Join(cfg.Lambda.FilesystemNames, ",") != "cache,models" || len(cfg.Lambda.FilesystemMounts) != 2 {
		t.Fatalf("env lambda lists not applied: %#v", cfg.Lambda)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	base := baseConfig()
	if cfg.Location != base.Location || cfg.Image != base.Image {
		t.Fatalf("lambda defaults leaked into generic fields: cfg=%#v", cfg)
	}
	if cfg.SSHUser != "ubuntu" || cfg.SSHPort != "22" || len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("lambda ssh defaults not applied: user=%q port=%q fallback=%v", cfg.SSHUser, cfg.SSHPort, cfg.SSHFallbackPorts)
	}
	if got := serverTypeForConfig(cfg); got != "gpu_1x_a10" {
		t.Fatalf("serverTypeForConfig=%q", got)
	}
}

func TestLambdaImageFamilyEnvClearsFileImage(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "lambda"
	if err := applyFileConfig(&cfg, fileConfig{Lambda: &fileLambdaConfig{
		Image: "img-file",
	}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_LAMBDA_IMAGE_FAMILY", "lambda-stack-24-04")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Lambda.Image != "" || cfg.Lambda.ImageFamily != "lambda-stack-24-04" {
		t.Fatalf("image family env did not clear file image: %#v", cfg.Lambda)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
}

func TestNebiusConfigFileEnvAndDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "nebius",
		Nebius: &fileNebiusConfig{
			CLI:              "/opt/nebius/bin/nebius",
			Profile:          "file-profile",
			ParentID:         "project-file",
			SubnetID:         "subnet-file",
			Platform:         "cpu-e2",
			Preset:           "8vcpu-32gb",
			ImageFamily:      "ubuntu24.04-driverless",
			DiskType:         "network_ssd_nonreplicated",
			DiskSizeGiB:      80,
			User:             "builder",
			PublicIP:         "none",
			SecurityGroupIDs: []string{"sg-file"},
			ServiceAccountID: "sa-file",
			RecoveryPolicy:   "fail",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "nebius" || cfg.Nebius.CLI != "/opt/nebius/bin/nebius" || cfg.Nebius.Profile != "file-profile" || cfg.Nebius.ParentID != "project-file" || cfg.Nebius.SubnetID != "subnet-file" {
		t.Fatalf("file nebius config not applied: %#v", cfg.Nebius)
	}
	if cfg.Nebius.Platform != "cpu-e2" || cfg.Nebius.Preset != "8vcpu-32gb" || cfg.Nebius.DiskSizeGiB != 80 || strings.Join(cfg.Nebius.SecurityGroupIDs, ",") != "sg-file" {
		t.Fatalf("file nebius sizing/network config not applied: %#v", cfg.Nebius)
	}

	t.Setenv("CRABBOX_NEBIUS_CLI", "/usr/local/bin/nebius")
	t.Setenv("CRABBOX_NEBIUS_PROFILE", "env-profile")
	t.Setenv("CRABBOX_NEBIUS_PARENT_ID", "project-env")
	t.Setenv("CRABBOX_NEBIUS_SUBNET_ID", "subnet-env")
	t.Setenv("CRABBOX_NEBIUS_PLATFORM", "gpu-h100")
	t.Setenv("CRABBOX_NEBIUS_PRESET", "1gpu-16vcpu-200gb")
	t.Setenv("CRABBOX_NEBIUS_IMAGE_FAMILY", "ubuntu22.04-cuda")
	t.Setenv("CRABBOX_NEBIUS_DISK_TYPE", "network_ssd")
	t.Setenv("CRABBOX_NEBIUS_DISK_SIZE_GIB", "120")
	t.Setenv("CRABBOX_NEBIUS_USER", "alice")
	t.Setenv("CRABBOX_NEBIUS_PUBLIC_IP", "dynamic")
	t.Setenv("CRABBOX_NEBIUS_SECURITY_GROUP_IDS", "sg-a, sg-b")
	t.Setenv("CRABBOX_NEBIUS_SERVICE_ACCOUNT_ID", "sa-env")
	t.Setenv("CRABBOX_NEBIUS_RECOVERY_POLICY", "fail")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Nebius.CLI != "/usr/local/bin/nebius" || cfg.Nebius.Profile != "env-profile" || cfg.Nebius.ParentID != "project-env" || cfg.Nebius.SubnetID != "subnet-env" {
		t.Fatalf("env nebius identity config not applied: %#v", cfg.Nebius)
	}
	if cfg.Nebius.Platform != "gpu-h100" || cfg.Nebius.Preset != "1gpu-16vcpu-200gb" || cfg.Nebius.ImageFamily != "ubuntu22.04-cuda" || cfg.Nebius.DiskSizeGiB != 120 || strings.Join(cfg.Nebius.SecurityGroupIDs, ",") != "sg-a,sg-b" {
		t.Fatalf("env nebius sizing/network config not applied: %#v", cfg.Nebius)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	base := baseConfig()
	if cfg.TargetOS != targetLinux || cfg.WorkRoot != defaultPOSIXWorkRoot || cfg.SSHUser != "alice" || cfg.SSHPort != base.SSHPort {
		t.Fatalf("nebius target defaults not applied: target=%q root=%q user=%q port=%q", cfg.TargetOS, cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort)
	}

	defaulted := baseConfig()
	defaulted.Provider = "nebius"
	defaulted.Nebius = NebiusConfig{}
	if err := applyProviderConfigDefaults(&defaulted); err != nil {
		t.Fatal(err)
	}
	if defaulted.Nebius.CLI != "nebius" || defaulted.Nebius.Platform != "cpu-d3" || defaulted.Nebius.Preset != "4vcpu-16gb" || defaulted.Nebius.ImageFamily != "ubuntu24.04-driverless" || defaulted.Nebius.DiskSizeGiB != 50 || defaulted.Nebius.PublicIP != "dynamic" {
		t.Fatalf("nebius conservative defaults not applied: %#v", defaulted.Nebius)
	}
}

func TestNebiusUntrustedConfigCannotRedirectCLI(t *testing.T) {
	cfg := baseConfig()
	cfg.Nebius.CLI = "/trusted/nebius"
	cfg.Nebius.Profile = "trusted-profile"
	cfg.Nebius.ServiceAccountID = "trusted-service-account"
	file := fileConfig{
		Nebius: &fileNebiusConfig{
			CLI:              "/tmp/untrusted-nebius",
			Profile:          "untrusted-profile",
			ParentID:         "project-repo",
			SubnetID:         "subnet-repo",
			ServiceAccountID: "untrusted-service-account",
		},
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Nebius.CLI != "/trusted/nebius" {
		t.Fatalf("untrusted CLI override applied: %q", cfg.Nebius.CLI)
	}
	if cfg.Nebius.Profile != "trusted-profile" {
		t.Fatalf("untrusted profile override applied: %q", cfg.Nebius.Profile)
	}
	if cfg.Nebius.ServiceAccountID != "trusted-service-account" {
		t.Fatalf("untrusted service account override applied: %q", cfg.Nebius.ServiceAccountID)
	}
	if cfg.Nebius.ParentID != "project-repo" || cfg.Nebius.SubnetID != "subnet-repo" {
		t.Fatalf("safe untrusted nebius settings not applied: %#v", cfg.Nebius)
	}
}

func TestNebiusEnvDoesNotMutateGenericFieldsForOtherProviders(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "ssh"
	t.Setenv("CRABBOX_NEBIUS_PARENT_ID", "project-env")
	t.Setenv("CRABBOX_NEBIUS_USER", "nebius-user")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Nebius.ParentID != "project-env" || cfg.Nebius.User != "nebius-user" {
		t.Fatalf("nebius env not stored: %#v", cfg.Nebius)
	}
	base := baseConfig()
	if cfg.SSHUser != base.SSHUser || cfg.WorkRoot != base.WorkRoot || cfg.TargetOS != base.TargetOS {
		t.Fatalf("nebius env leaked into generic config: %#v", cfg)
	}
}

func TestNebiusDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, fileConfig{
		Provider: "nebius",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Nebius:   &fileNebiusConfig{ParentID: "project", SubnetID: "subnet"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/srv/crabbox" || cfg.SSHUser != "alice" || cfg.SSHPort != "2200" {
		t.Fatalf("explicit generic SSH settings not preserved: root=%q user=%q port=%q", cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort)
	}
}

func TestLinodePortableOSSelection(t *testing.T) {
	t.Run("supported selector maps to provider image", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "linode"
		cfg.OSImage = "ubuntu:24.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Linode.Image != "linode/ubuntu24.04" {
			t.Fatalf("Linode.Image=%q", cfg.Linode.Image)
		}
	})

	t.Run("unsupported selector is deferred to acquisition", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "linode"
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Linode.Image != "linode/ubuntu24.04" {
			t.Fatalf("default Linode.Image=%q", cfg.Linode.Image)
		}
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Linode.Image != "" {
			t.Fatalf("Linode.Image=%q, want unresolved provider image", cfg.Linode.Image)
		}
	})

	t.Run("provider image overrides portable selector", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "linode"
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		cfg.Linode.Image = "private/123"
		cfg.linodeImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Linode.Image != "private/123" {
			t.Fatalf("Linode.Image=%q", cfg.Linode.Image)
		}
	})
}

func TestLinodeDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "ssh",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Static: &fileStaticConfig{
			User:     "builder",
			Port:     "2202",
			WorkRoot: "/srv/static",
		},
	})
	normalizeTargetConfig(&cfg)

	cfg.Provider = "linode"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/srv/crabbox" {
		t.Fatalf("Linode WorkRoot=%q want explicit generic root", cfg.WorkRoot)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("Linode SSHUser=%q want explicit generic user", cfg.SSHUser)
	}
	if cfg.SSHPort != "2200" {
		t.Fatalf("Linode SSHPort=%q want explicit generic port", cfg.SSHPort)
	}
}

func TestDockerSandboxEmptyFileConfigDoesNotClearExistingValues(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.DockerSandbox = DockerSandboxConfig{
		CLIPath:         "/opt/sbx",
		Agent:           "shell",
		Template:        "ubuntu",
		CPUs:            2,
		Memory:          "4g",
		Clone:           true,
		Workdir:         "/workspace/my-app",
		ExtraWorkspaces: []string{"/tmp/extra"},
		MCP:             []string{"context7"},
		Kit:             []string{"example-org/base"},
	}
	applyFileConfig(&cfg, fileConfig{DockerSandbox: &fileDockerSandboxConfig{}})
	if cfg.DockerSandbox.CLIPath != "/opt/sbx" || cfg.DockerSandbox.Agent != "shell" || cfg.DockerSandbox.Template != "ubuntu" || cfg.DockerSandbox.CPUs != 2 || cfg.DockerSandbox.Memory != "4g" || !cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/workspace/my-app" {
		t.Fatalf("empty file dockerSandbox config cleared existing scalar values: %#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(cfg.DockerSandbox.MCP, ",") != "context7" || strings.Join(cfg.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("empty file dockerSandbox config cleared existing list values: %#v", cfg.DockerSandbox)
	}
}

func TestDockerSandboxFileConfigCanClearInheritedLists(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.DockerSandbox.ExtraWorkspaces = []string{"/tmp/inherited"}
	cfg.DockerSandbox.MCP = []string{"context7"}
	cfg.DockerSandbox.Kit = []string{"example-org/base"}

	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
dockerSandbox:
  extraWorkspaces: []
  mcp: []
  kit: []
`), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatalf("applyFileConfig err=%v", err)
	}
	if len(cfg.DockerSandbox.ExtraWorkspaces) != 0 || len(cfg.DockerSandbox.MCP) != 0 || len(cfg.DockerSandbox.Kit) != 0 {
		t.Fatalf("repo dockerSandbox empty lists did not clear inherited values: %#v", cfg.DockerSandbox)
	}
}

func TestDockerSandboxFileConfigCanClearInheritedRuntimeDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.DockerSandbox.Template = "ubuntu"
	cfg.DockerSandbox.CPUs = 4
	cfg.DockerSandbox.Memory = "8g"
	cfg.DockerSandbox.Workdir = "/workspace/inherited"

	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
dockerSandbox:
  template: ""
  cpus: 0
  memory: ""
  workdir: ""
`), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatalf("applyFileConfig err=%v", err)
	}
	if cfg.DockerSandbox.Template != "" || cfg.DockerSandbox.CPUs != 0 || cfg.DockerSandbox.Memory != "" || cfg.DockerSandbox.Workdir != "" {
		t.Fatalf("repo dockerSandbox runtime defaults did not clear inherited values: %#v", cfg.DockerSandbox)
	}
}

func TestDockerSandboxFileConfigRejectsNegativeCPUs(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
provider: docker-sandbox
dockerSandbox:
  cpus: -1
`), &file); err != nil {
		t.Fatal(err)
	}
	err := applyFileConfig(&cfg, file)
	if err == nil {
		t.Fatal("applyConfigFile err=<nil>, want negative dockerSandbox cpus rejection")
	}
	if !strings.Contains(err.Error(), "docker-sandbox cpus must be non-negative") {
		t.Fatalf("applyConfigFile err=%v, want negative dockerSandbox cpus rejection", err)
	}
}

func TestDockerSandboxConfigAcceptsMCPFromFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
provider: docker-sandbox
dockerSandbox:
  mcp:
    - context7
    - all
`), &file); err != nil {
		t.Fatal(err)
	}
	err := applyFileConfig(&cfg, file)
	if err != nil {
		t.Fatalf("applyFileConfig mcp err=%v", err)
	}
	if strings.Join(cfg.DockerSandbox.MCP, ",") != "context7,all" {
		t.Fatalf("applyFileConfig mcp cfg=%#v", cfg.DockerSandbox)
	}

	t.Setenv("CRABBOX_DOCKER_SANDBOX_MCP", "one,two")
	err = applyEnv(&cfg)
	if err != nil {
		t.Fatalf("applyEnv mcp err=%v", err)
	}
	if strings.Join(cfg.DockerSandbox.MCP, ",") != "one,two" {
		t.Fatalf("applyEnv mcp cfg=%#v", cfg.DockerSandbox)
	}
}

func TestAnthropicSandboxRuntimeConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.AnthropicSRT.CLIPath != "srt" || cfg.AnthropicSRT.Settings != "" || cfg.AnthropicSRT.Debug {
		t.Fatalf("anthropicSandboxRuntime defaults not applied: %#v", cfg.AnthropicSRT)
	}
	settings := ".crabbox/srt-settings.json"
	var file fileConfig
	if err := yaml.Unmarshal([]byte("provider: anthropic-sandbox-runtime\nanthropicSandboxRuntime:\n  cliPath: /opt/srt\n  settings: "+settings+"\n  debug: true\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "anthropic-sandbox-runtime" || cfg.AnthropicSRT.CLIPath != "/opt/srt" || cfg.AnthropicSRT.Settings != settings || !cfg.AnthropicSRT.Debug {
		t.Fatalf("file anthropicSandboxRuntime config not applied: %#v", cfg.AnthropicSRT)
	}

	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI", "/usr/local/bin/srt")
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS", ".crabbox/env-srt-settings.json")
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.AnthropicSRT.CLIPath != "/usr/local/bin/srt" || cfg.AnthropicSRT.Settings != ".crabbox/env-srt-settings.json" || cfg.AnthropicSRT.Debug {
		t.Fatalf("env anthropicSandboxRuntime config not applied: %#v", cfg.AnthropicSRT)
	}
}

func TestAnthropicSandboxRuntimeFilePresenceAndTrust(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			name, yaml, cli, settings string
			debug                     bool
		}{
			{"omitted", "{}", "/opt/prior-srt", "prior.json", true},
			{"null", "{cliPath: null, settings: null, debug: null}", "/opt/prior-srt", "prior.json", true},
			{"empty", "{cliPath: '', settings: '', debug: false}", "/opt/prior-srt", "", false},
			{"whitespace", "{cliPath: '  ', settings: '  ', debug: false}", "  ", "  ", false},
		} {
			t.Run(fmt.Sprintf("trusted=%t/%s", trusted, tc.name), func(t *testing.T) {
				cfg := baseConfig()
				cfg.AnthropicSRT = AnthropicSRTConfig{CLIPath: "/opt/prior-srt", Settings: "prior.json", Debug: true}
				var file fileConfig
				if err := yaml.Unmarshal([]byte("anthropicSandboxRuntime: "+tc.yaml), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				want := AnthropicSRTConfig{CLIPath: tc.cli, Settings: tc.settings, Debug: tc.debug}
				if cfg.AnthropicSRT != want {
					t.Fatalf("got=%#v want=%#v", cfg.AnthropicSRT, want)
				}
			})
		}
	}
}

func TestAnthropicSandboxRuntimeLayerAndEnvironmentSemantics(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	for _, layer := range []struct {
		cli, settings string
		trusted       bool
	}{{"/opt/user-srt", "user.json", true}, {"/opt/repo-srt", "repo.json", false}} {
		var file fileConfig
		if err := yaml.Unmarshal([]byte("anthropicSandboxRuntime:\n  cliPath: "+layer.cli+"\n  settings: "+layer.settings+"\n  debug: true\n"), &file); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfigWithTrust(&cfg, file, layer.trusted); err != nil {
			t.Fatal(err)
		}
		if cfg.AnthropicSRT.CLIPath != layer.cli || cfg.AnthropicSRT.Settings != layer.settings || !cfg.AnthropicSRT.Debug {
			t.Fatalf("layer=%#v got=%#v", layer, cfg.AnthropicSRT)
		}
	}
	for _, raw := range []string{"", "invalid", "no", "invalid", "yes"} {
		t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI", "")
		t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS", "")
		t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG", raw)
		before := cfg.AnthropicSRT
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		want := before
		if raw == "no" {
			want.Debug = false
		}
		if raw == "yes" {
			want.Debug = true
		}
		if cfg.AnthropicSRT != want {
			t.Fatalf("env %q got=%#v want=%#v", raw, cfg.AnthropicSRT, want)
		}
	}
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI", "/opt/env-srt")
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS", "env.json")
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AnthropicSRT != (AnthropicSRTConfig{CLIPath: "/opt/env-srt", Settings: "env.json"}) {
		t.Fatalf("environment did not override repository: %#v", cfg.AnthropicSRT)
	}
}

func TestAnthropicSandboxRuntimeFileConfigCanClearSettings(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.AnthropicSRT.Settings = ".crabbox/inherited-srt-settings.json"

	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
anthropicSandboxRuntime:
  settings: ""
`), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatalf("applyFileConfig err=%v", err)
	}
	if cfg.AnthropicSRT.Settings != "" {
		t.Fatalf("settings=%q want cleared", cfg.AnthropicSRT.Settings)
	}
}

func TestAsciiBoxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "ascii-box",
		AsciiBox: &fileAsciiBoxConfig{
			BaseURL: "https://box.example.test",
			CLIPath: "/tmp/box",
			Workdir: "/home/user/project",
		},
	})
	if cfg.Provider != "ascii-box" || cfg.AsciiBox.BaseURL != "https://box.example.test" || cfg.AsciiBox.CLIPath != "/tmp/box" || cfg.AsciiBox.Workdir != "/home/user/project" {
		t.Fatalf("file asciiBox config not applied: %#v", cfg.AsciiBox)
	}

	t.Setenv("ASCII_BOX_API_KEY", "fallback-key")
	t.Setenv("ASCII_BOX_BASE_URL", "https://fallback.example.test")
	t.Setenv("CRABBOX_ASCII_BOX_API_KEY", "override-key")
	t.Setenv("CRABBOX_ASCII_BOX_BASE_URL", "https://override.example.test")
	t.Setenv("CRABBOX_ASCII_BOX_CLI", "/opt/box")
	t.Setenv("CRABBOX_ASCII_BOX_WORKDIR", "/home/user/env-project")
	applyEnv(&cfg)
	if cfg.AsciiBox.APIKey != "override-key" || cfg.AsciiBox.BaseURL != "https://override.example.test" || cfg.AsciiBox.CLIPath != "/opt/box" || cfg.AsciiBox.Workdir != "/home/user/env-project" {
		t.Fatalf("env asciiBox config not applied: %#v", cfg.AsciiBox)
	}
}

func TestCloudflareDynamicWorkersConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.CloudflareDynamicWorkers.CacheMode != "stable" || cfg.CloudflareDynamicWorkers.Egress != "blocked" || cfg.CloudflareDynamicWorkers.TimeoutSecs != 60 {
		t.Fatalf("dynamic workers defaults not applied: %#v", cfg.CloudflareDynamicWorkers)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "cloudflare-dynamic-workers",
		CloudflareDynamicWorkers: &fileCloudflareDynamicWorkersConfig{
			LoaderURL:          "https://file-loader.example.test",
			Token:              "file-token",
			CompatibilityDate:  "2026-06-01",
			CompatibilityFlags: []string{"nodejs_compat"},
			CacheMode:          "explicit",
			Egress:             "intercept",
			CPUMs:              25,
			Subrequests:        7,
			TimeoutSecs:        45,
			Metadata:           map[string]string{"team": "file"},
		},
	})
	if cfg.Provider != "cloudflare-dynamic-workers" || cfg.CloudflareDynamicWorkers.LoaderURL != "https://file-loader.example.test" || cfg.CloudflareDynamicWorkers.Token != "file-token" {
		t.Fatalf("file dynamic workers config not applied: %#v", cfg.CloudflareDynamicWorkers)
	}
	if cfg.CloudflareDynamicWorkers.Metadata["team"] != "file" || cfg.CloudflareDynamicWorkers.CacheMode != "explicit" || cfg.CloudflareDynamicWorkers.Egress != "intercept" {
		t.Fatalf("file dynamic workers metadata/cache not applied: %#v", cfg.CloudflareDynamicWorkers)
	}

	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL", "https://env-loader.example.test")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN", "env-token")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_DATE", "2026-06-02")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS", "nodejs_compat,streams_enable_constructors")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CACHE_MODE", "stable")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_EGRESS", "blocked")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS", "50")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_SUBREQUESTS", "12")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TIMEOUT_SECS", "30")
	applyEnv(&cfg)
	if cfg.CloudflareDynamicWorkers.LoaderURL != "https://env-loader.example.test" || cfg.CloudflareDynamicWorkers.Token != "env-token" || cfg.CloudflareDynamicWorkers.CompatibilityDate != "2026-06-02" {
		t.Fatalf("env dynamic workers config not applied: %#v", cfg.CloudflareDynamicWorkers)
	}
	if strings.Join(cfg.CloudflareDynamicWorkers.CompatibilityFlags, ",") != "nodejs_compat,streams_enable_constructors" {
		t.Fatalf("compatibility flags=%v", cfg.CloudflareDynamicWorkers.CompatibilityFlags)
	}
	if cfg.CloudflareDynamicWorkers.CacheMode != "stable" || cfg.CloudflareDynamicWorkers.Egress != "blocked" || cfg.CloudflareDynamicWorkers.CPUMs != 50 || cfg.CloudflareDynamicWorkers.Subrequests != 12 || cfg.CloudflareDynamicWorkers.TimeoutSecs != 30 {
		t.Fatalf("env dynamic workers limits/cache not applied: %#v", cfg.CloudflareDynamicWorkers)
	}
}

func TestCloudflareDynamicWorkersUntrustedConfigCannotReplaceConnectionOrEnableEgress(t *testing.T) {
	cfg := baseConfig()
	cfg.CloudflareDynamicWorkers.LoaderURL = "https://trusted-loader.example.test"
	cfg.CloudflareDynamicWorkers.Token = "trusted-token"
	cfg.CloudflareDynamicWorkers.Egress = "blocked"
	cfg.CloudflareDynamicWorkers.CPUMs = 25
	cfg.CloudflareDynamicWorkers.Subrequests = 7
	cfg.CloudflareDynamicWorkers.TimeoutSecs = 30

	err := applyFileConfigWithTrust(&cfg, fileConfig{
		CloudflareDynamicWorkers: &fileCloudflareDynamicWorkersConfig{
			LoaderURL:   "https://untrusted-loader.example.test",
			Token:       "untrusted-token",
			Egress:      "intercept",
			CacheMode:   "one-shot",
			CPUMs:       100,
			Subrequests: 20,
			TimeoutSecs: 90,
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareDynamicWorkers.LoaderURL != "https://trusted-loader.example.test" ||
		cfg.CloudflareDynamicWorkers.Token != "trusted-token" ||
		cfg.CloudflareDynamicWorkers.Egress != "blocked" {
		t.Fatalf("untrusted config changed connection or egress: %#v", cfg.CloudflareDynamicWorkers)
	}
	if cfg.CloudflareDynamicWorkers.CacheMode != "one-shot" ||
		cfg.CloudflareDynamicWorkers.CPUMs != 25 ||
		cfg.CloudflareDynamicWorkers.Subrequests != 7 ||
		cfg.CloudflareDynamicWorkers.TimeoutSecs != 30 {
		t.Fatalf("untrusted config loosened runtime limits: %#v", cfg.CloudflareDynamicWorkers)
	}

	err = applyFileConfigWithTrust(&cfg, fileConfig{
		CloudflareDynamicWorkers: &fileCloudflareDynamicWorkersConfig{
			CPUMs:       10,
			Subrequests: 3,
			TimeoutSecs: 15,
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareDynamicWorkers.CPUMs != 10 ||
		cfg.CloudflareDynamicWorkers.Subrequests != 3 ||
		cfg.CloudflareDynamicWorkers.TimeoutSecs != 15 {
		t.Fatalf("untrusted config did not tighten runtime limits: %#v", cfg.CloudflareDynamicWorkers)
	}
}

func TestCloudflareDynamicWorkersUntrustedConfigCannotSetUnsetResourceLimits(t *testing.T) {
	cfg := baseConfig()
	cfg.CloudflareDynamicWorkers.CPUMs = 0
	cfg.CloudflareDynamicWorkers.Subrequests = 0

	err := applyFileConfigWithTrust(&cfg, fileConfig{
		CloudflareDynamicWorkers: &fileCloudflareDynamicWorkersConfig{
			CPUMs:       300_000,
			Subrequests: 10_000,
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareDynamicWorkers.CPUMs != 0 || cfg.CloudflareDynamicWorkers.Subrequests != 0 {
		t.Fatalf("untrusted config set unset resource limits: %#v", cfg.CloudflareDynamicWorkers)
	}
}

func TestCloudflareDynamicWorkersRepositoryCapsApplyAfterEnvironment(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS", "50")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_SUBREQUESTS", "12")
	t.Setenv("CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TIMEOUT_SECS", "30")

	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.WriteFile(
		".crabbox.yaml",
		[]byte("provider: cloudflare-dynamic-workers\ncloudflareDynamicWorkers:\n  cpuMs: 10\n  subrequests: 3\n  timeoutSecs: 15\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareDynamicWorkers.CPUMs != 10 ||
		cfg.CloudflareDynamicWorkers.Subrequests != 3 ||
		cfg.CloudflareDynamicWorkers.TimeoutSecs != 15 {
		t.Fatalf("repository caps did not constrain environment limits: %#v", cfg.CloudflareDynamicWorkers)
	}

	fs := newFlagSet("test", io.Discard)
	values := registerProviderFlags(fs, cfg)
	if err := parseFlags(fs, []string{
		"--cloudflare-dynamic-workers-cpu-ms", "100",
		"--cloudflare-dynamic-workers-subrequests", "20",
		"--cloudflare-dynamic-workers-timeout-secs", "90",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareDynamicWorkers.CPUMs != 10 ||
		cfg.CloudflareDynamicWorkers.Subrequests != 3 ||
		cfg.CloudflareDynamicWorkers.TimeoutSecs != 15 {
		t.Fatalf("repository caps did not constrain provider flags: %#v", cfg.CloudflareDynamicWorkers)
	}

	fs = newFlagSet("test-zero", io.Discard)
	values = registerProviderFlags(fs, cfg)
	if err := parseFlags(fs, []string{
		"--cloudflare-dynamic-workers-cpu-ms", "0",
		"--cloudflare-dynamic-workers-subrequests", "0",
		"--cloudflare-dynamic-workers-timeout-secs", "0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareDynamicWorkers.CPUMs != 10 ||
		cfg.CloudflareDynamicWorkers.Subrequests != 3 ||
		cfg.CloudflareDynamicWorkers.TimeoutSecs != 15 {
		t.Fatalf("zero provider flags disabled repository caps: %#v", cfg.CloudflareDynamicWorkers)
	}
}

func TestAppleContainerConfigSources(t *testing.T) {
	clearConfigEnv(t)
	base := baseConfig()
	want := AppleContainerConfig{CLIPath: "container", Image: base.LocalContainer.Image, User: "crabbox", WorkRoot: "/work/crabbox"}
	if !reflect.DeepEqual(base.AppleContainer, want) || base.AppleContainer.Image == "" {
		t.Fatalf("defaults=%#v want %#v", base.AppleContainer, want)
	}
	for _, f := range []struct{ field, key, env string }{{"CLIPath", "cliPath", "CLI"}, {"Image", "image", "IMAGE"}, {"User", "user", "USER"}, {"WorkRoot", "workRoot", "WORK_ROOT"}, {"Memory", "memory", "MEMORY"}} {
		t.Run(f.field, func(t *testing.T) {
			for _, input := range []string{"null", "''", "'  '", "'same'", "'~/literal'"} {
				cfg := baseConfig()
				reflect.ValueOf(&cfg.AppleContainer).Elem().FieldByName(f.field).SetString("same")
				var file fileConfig
				if err := yaml.Unmarshal([]byte("appleContainer: {"+f.key+": "+input+"}"), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
				want := "same"
				if input == "'  '" {
					want = "  "
				}
				if input == "'~/literal'" {
					want = "~/literal"
				}
				if got := reflect.ValueOf(cfg.AppleContainer).FieldByName(f.field).String(); got != want {
					t.Fatalf("file %s=%q want %q", input, got, want)
				}
				if AppleContainerImageExplicit(cfg) != (f.field == "Image" && input != "null" && input != "''") {
					t.Fatal("file image acceptance marker")
				}
			}
			for _, input := range []string{"", "  ", "same", "~/literal"} {
				t.Setenv("CRABBOX_APPLE_CONTAINER_"+f.env, input)
				cfg := baseConfig()
				reflect.ValueOf(&cfg.AppleContainer).Elem().FieldByName(f.field).SetString("same")
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				want := input
				if input == "" {
					want = "same"
				}
				if got := reflect.ValueOf(cfg.AppleContainer).FieldByName(f.field).String(); got != want {
					t.Fatalf("env %q=%q", input, got)
				}
				if AppleContainerImageExplicit(cfg) != (f.field == "Image" && input != "") {
					t.Fatal("env image acceptance marker")
				}
			}
		})
	}
	for _, input := range []int{-2, 0, 3} {
		cfg := baseConfig()
		cfg.AppleContainer.CPUs = 7
		if err := applyFileConfig(&cfg, fileConfig{AppleContainer: &fileAppleContainerConfig{CPUs: input}}); err != nil {
			t.Fatal(err)
		}
		want := 7
		if input > 0 {
			want = input
		}
		if cfg.AppleContainer.CPUs != want {
			t.Fatal("file CPU positive predicate")
		}
	}
	for _, tc := range []struct {
		input string
		want  int
	}{{"", 7}, {"invalid", 7}, {" 3 ", 7}, {"0", 0}, {"-2", -2}, {"3", 3}} {
		t.Run("cpu-"+tc.input, func(t *testing.T) {
			t.Setenv("CRABBOX_APPLE_CONTAINER_CPUS", tc.input)
			cfg := baseConfig()
			cfg.AppleContainer.CPUs = 7
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.AppleContainer.CPUs != tc.want {
				t.Fatalf("CPU=%d want %d", cfg.AppleContainer.CPUs, tc.want)
			}
		})
	}
}

func TestAppleContainerConfigLists(t *testing.T) {
	clearConfigEnv(t)
	for _, source := range [][]string{nil, {}, {" alpha ", "alpha", "alpha"}} {
		cfg := baseConfig()
		cfg.AppleContainer.ExtraRunArgs = []string{"prior"}
		file := fileConfig{AppleContainer: &fileAppleContainerConfig{ExtraRunArgs: source}}
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		want := []string{"prior"}
		if len(source) > 0 {
			want = []string{" alpha ", "alpha", "alpha"}
		}
		if !reflect.DeepEqual(cfg.AppleContainer.ExtraRunArgs, want) {
			t.Fatal("file raw list/nonempty rule")
		}
		if len(source) > 0 {
			source[0] = "source-change"
			if cfg.AppleContainer.ExtraRunArgs[0] != " alpha " {
				t.Fatal("accepted file list not cloned")
			}
			cfg.AppleContainer.ExtraRunArgs[1] = "runtime-change"
			if source[1] != "alpha" {
				t.Fatal("runtime list aliases file input")
			}
		}
	}
	for _, tc := range []struct {
		input string
		want  []string
	}{{"", []string{"prior"}}, {" \t\n", []string{"prior"}}, {"alpha\tbeta alpha", []string{"alpha", "beta", "alpha"}}, {"\"alpha beta\" a,b", []string{"\"alpha", "beta\"", "a,b"}}} {
		t.Run(tc.input, func(t *testing.T) {
			t.Setenv("CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS", tc.input)
			cfg := baseConfig()
			cfg.AppleContainer.ExtraRunArgs = []string{"prior"}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.AppleContainer.ExtraRunArgs, tc.want) {
				t.Fatalf("env list=%q want %q", cfg.AppleContainer.ExtraRunArgs, tc.want)
			}
		})
	}
}

func TestAppleContainerConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.AppleContainer.CLIPath != "container" || cfg.AppleContainer.User != "crabbox" {
		t.Fatalf("apple container defaults not applied: %#v", cfg.AppleContainer)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "apple-container",
		AppleContainer: &fileAppleContainerConfig{
			CLIPath:      "/opt/bin/container",
			Image:        "example-org/my-app:test",
			User:         "runner",
			WorkRoot:     "/work/example",
			CPUs:         4,
			Memory:       "8g",
			ExtraRunArgs: []string{"--mount", "type=virtiofs,source=/tmp,target=/tmp"},
		},
	})
	if cfg.Provider != "apple-container" || cfg.AppleContainer.CLIPath != "/opt/bin/container" || cfg.AppleContainer.Image != "example-org/my-app:test" || cfg.AppleContainer.User != "runner" || cfg.AppleContainer.WorkRoot != "/work/example" || cfg.AppleContainer.CPUs != 4 || cfg.AppleContainer.Memory != "8g" || len(cfg.AppleContainer.ExtraRunArgs) != 2 {
		t.Fatalf("file appleContainer config not applied: %#v", cfg.AppleContainer)
	}

	t.Setenv("CRABBOX_APPLE_CONTAINER_CLI", "/usr/local/bin/container")
	t.Setenv("CRABBOX_APPLE_CONTAINER_IMAGE", "example-org/other:live")
	t.Setenv("CRABBOX_APPLE_CONTAINER_USER", "env-user")
	t.Setenv("CRABBOX_APPLE_CONTAINER_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_APPLE_CONTAINER_CPUS", "6")
	t.Setenv("CRABBOX_APPLE_CONTAINER_MEMORY", "12g")
	t.Setenv("CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS", "--dns 1.1.1.1")
	applyEnv(&cfg)
	if cfg.AppleContainer.CLIPath != "/usr/local/bin/container" || cfg.AppleContainer.Image != "example-org/other:live" || cfg.AppleContainer.User != "env-user" || cfg.AppleContainer.WorkRoot != "/work/env" || cfg.AppleContainer.CPUs != 6 || cfg.AppleContainer.Memory != "12g" || len(cfg.AppleContainer.ExtraRunArgs) != 2 {
		t.Fatalf("env appleContainer config not applied: %#v", cfg.AppleContainer)
	}
}

func TestAppleVMOrdinaryFileSections(t *testing.T) {
	t.Run("initializer", func(t *testing.T) {
		cfg := baseConfig()
		image, err := osImageDefaultAppleVMImage(cfg.OSImage)
		if err != nil {
			t.Fatal(err)
		}
		checksum, err := osImageDefaultAppleVMSHA256(cfg.OSImage)
		if err != nil {
			t.Fatal(err)
		}
		want := AppleVMConfig{Image: image, ImageSHA256: checksum, User: "crabbox", WorkRoot: "/work/crabbox", CPUs: 4, MemoryMiB: 8192, DiskGiB: 30}
		if cfg.AppleVM != want {
			t.Fatalf("initial AppleVM=%+v, want %+v", cfg.AppleVM, want)
		}
		if AppleVMImageExplicit(cfg) || cfg.appleVMImageSHA256Explicit || AppleVMCPUsExplicit(cfg) || AppleVMMemoryExplicit(cfg) || AppleVMDiskExplicit(cfg) {
			t.Fatal("initializer marked source values explicit")
		}
	})
	initial := AppleVMConfig{HelperPath: "before-helper", Image: "before-image", ImageSHA256: "before-checksum", User: "before-user", WorkRoot: "/before", CPUs: 4, MemoryMiB: 8192, DiskGiB: 30}
	current := AppleVMConfig{HelperPath: " ~/current/helper ", Image: " ~/current/image ", ImageSHA256: " current-checksum ", User: " current-user ", WorkRoot: " ~/current/work ", CPUs: 0, MemoryMiB: -2, DiskGiB: -3}
	legacy := AppleVMConfig{HelperPath: " ~/legacy/helper ", Image: " ~/legacy/image ", ImageSHA256: " legacy-checksum ", User: " legacy-user ", WorkRoot: " ~/legacy/work ", CPUs: -1, MemoryMiB: 0, DiskGiB: 0}
	currentYAML := "appleVM:\n  helperPath: ' ~/current/helper '\n  image: ' ~/current/image '\n  imageSHA256: ' current-checksum '\n  user: ' current-user '\n  workRoot: ' ~/current/work '\n  cpus: 0\n  memoryMiB: -2\n  diskGiB: -3\n"
	legacyYAML := "appleVZ:\n  helperPath: ' ~/legacy/helper '\n  image: ' ~/legacy/image '\n  imageSHA256: ' legacy-checksum '\n  user: ' legacy-user '\n  workRoot: ' ~/legacy/work '\n  cpus: -1\n  memoryMiB: 0\n  diskGiB: 0\n"
	for _, tc := range []struct {
		name, document string
		want           AppleVMConfig
		marked         bool
	}{
		{"current", currentYAML, current, true},
		{"legacy", legacyYAML, legacy, true},
		{"current-whole-section-wins", currentYAML + legacyYAML, current, true},
		{"current-whole-section-wins-reversed", legacyYAML + currentYAML, current, true},
		{"empty-current-wins", "appleVM: {}\n" + legacyYAML, initial, false},
		{"null-current-falls-back", "appleVM: null\n" + legacyYAML, legacy, true},
		{"both-null", "appleVM: null\nappleVZ: null\n", initial, false},
		{"equal-values-still-explicit", "appleVM:\n  helperPath: before-helper\n  image: before-image\n  imageSHA256: before-checksum\n  user: before-user\n  workRoot: /before\n  cpus: 4\n  memoryMiB: 8192\n  diskGiB: 30\n", initial, true},
		{"empty-values-and-null-numbers", "appleVM:\n  helperPath: ''\n  image: ''\n  imageSHA256: ''\n  user: ''\n  workRoot: ''\n  cpus: null\n  memoryMiB: null\n  diskGiB: null\n" + legacyYAML, initial, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var file, original fileConfig
			if err := yaml.Unmarshal([]byte(tc.document), &file); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(tc.document), &original); err != nil {
				t.Fatal(err)
			}
			for _, alreadyMarked := range []bool{false, true} {
				cfg := Config{AppleVM: initial, SSHUser: "generic-user", WorkRoot: "/generic"}
				if alreadyMarked {
					MarkAppleVMImageExplicit(&cfg)
					MarkAppleVMImageSHA256Explicit(&cfg)
					MarkAppleVMCPUsExplicit(&cfg)
					MarkAppleVMMemoryExplicit(&cfg)
					MarkAppleVMDiskExplicit(&cfg)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
				if cfg.AppleVM != tc.want {
					t.Fatalf("AppleVM=%+v, want %+v", cfg.AppleVM, tc.want)
				}
				wantMarker := alreadyMarked || tc.marked
				if got := [5]bool{AppleVMImageExplicit(cfg), cfg.appleVMImageSHA256Explicit, AppleVMCPUsExplicit(cfg), AppleVMMemoryExplicit(cfg), AppleVMDiskExplicit(cfg)}; got != [5]bool{wantMarker, wantMarker, wantMarker, wantMarker, wantMarker} {
					t.Fatalf("markers=%v, want all %v", got, wantMarker)
				}
				if cfg.SSHUser != "generic-user" || cfg.WorkRoot != "/generic" || IsWorkRootExplicit(&cfg) {
					t.Fatal("file overlay changed generic user/root state")
				}
				if !reflect.DeepEqual(file, original) {
					t.Fatal("file input was mutated")
				}
			}
		})
	}
	t.Run("sparse-current-does-not-merge-legacy", func(t *testing.T) {
		var file fileConfig
		if err := yaml.Unmarshal([]byte("appleVM:\n  user: current-user\n"+legacyYAML), &file); err != nil {
			t.Fatal(err)
		}
		cfg := Config{AppleVM: initial}
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		want := initial
		want.User = "current-user"
		if cfg.AppleVM != want || AppleVMImageExplicit(cfg) || cfg.appleVMImageSHA256Explicit || AppleVMCPUsExplicit(cfg) || AppleVMMemoryExplicit(cfg) || AppleVMDiskExplicit(cfg) {
			t.Fatalf("sparse current merged legacy values or markers: %+v", cfg.AppleVM)
		}
	})
	// Successive source events must clear an earlier explicit checksum, even
	// when the image value itself is unchanged.
	for _, section := range []string{"appleVM", "appleVZ"} {
		t.Run(section+"-image-sequence", func(t *testing.T) {
			cfg := Config{AppleVM: initial}
			for _, step := range []struct {
				body, image, checksum       string
				imageMarked, checksumMarked bool
			}{
				{"imageSHA256: before-checksum", initial.Image, initial.ImageSHA256, false, true},
				{"image: before-image", initial.Image, "", true, false},
				{"imageSHA256: next-checksum", initial.Image, "next-checksum", true, true},
				{"image: next-image\n  imageSHA256: paired-checksum", "next-image", "paired-checksum", true, true},
				{"image: ''\n  imageSHA256: ''", "next-image", "paired-checksum", true, true},
			} {
				var file fileConfig
				if err := yaml.Unmarshal([]byte(section+":\n  "+step.body+"\n"), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
				if cfg.AppleVM.Image != step.image || cfg.AppleVM.ImageSHA256 != step.checksum || AppleVMImageExplicit(cfg) != step.imageMarked || cfg.appleVMImageSHA256Explicit != step.checksumMarked {
					t.Fatalf("step %q: image/checksum=%q/%q markers=%v/%v", step.body, cfg.AppleVM.Image, cfg.AppleVM.ImageSHA256, AppleVMImageExplicit(cfg), cfg.appleVMImageSHA256Explicit)
				}
			}
		})
	}
}

func TestAppleVMOrdinaryEnvironmentAliases(t *testing.T) {
	suffixes := []string{"HELPER", "IMAGE", "IMAGE_SHA256", "USER", "WORK_ROOT", "CPUS", "MEMORY", "DISK"}
	initial := AppleVMConfig{HelperPath: "before-helper", Image: "before-image", ImageSHA256: "before-checksum", User: "before-user", WorkRoot: "/before", CPUs: 4, MemoryMiB: 8192, DiskGiB: 30}
	current := []string{" ~/current/helper ", " ~/current/image ", " current-checksum ", " current-user ", " ~/current/work ", " +6 ", " -2 ", " 0 "}
	legacy := []string{" ~/legacy/helper ", " ~/legacy/image ", " legacy-checksum ", " legacy-user ", " ~/legacy/work ", " -3 ", " 0 ", " +9 "}
	equal := []string{initial.HelperPath, initial.Image, initial.ImageSHA256, initial.User, initial.WorkRoot, "4", "8192", "30"}
	empty := make([]string, len(suffixes))
	for _, tc := range []struct {
		name            string
		current, legacy []string
		want            AppleVMConfig
		marked          bool
	}{
		{"current-only", current, empty, AppleVMConfig{current[0], current[1], current[2], current[3], current[4], 6, -2, 0}, true},
		{"legacy-only-empty-current", empty, legacy, AppleVMConfig{legacy[0], legacy[1], legacy[2], legacy[3], legacy[4], -3, 0, 9}, true},
		{"current-outranks-legacy", current, legacy, AppleVMConfig{current[0], current[1], current[2], current[3], current[4], 6, -2, 0}, true},
		{"equal-current-still-explicit", equal, legacy, initial, true},
		{"equal-legacy-still-explicit", empty, equal, initial, true},
		{"signed-numerics-all-fields", []string{equal[0], equal[1], equal[2], equal[3], equal[4], " -4 ", " +8192 ", " -30 "}, legacy, AppleVMConfig{initial.HelperPath, initial.Image, initial.ImageSHA256, initial.User, initial.WorkRoot, -4, 8192, -30}, true},
		{"empty-preserves", empty, empty, initial, false},
		{"whitespace-strings-win", []string{" ", " ", " ", " ", " ", "4", "8192", "30"}, legacy, AppleVMConfig{" ", " ", " ", " ", " ", 4, 8192, 30}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			for i, suffix := range suffixes {
				t.Setenv("CRABBOX_APPLE_VM_"+suffix, tc.current[i])
				t.Setenv("CRABBOX_APPLE_VZ_"+suffix, tc.legacy[i])
			}
			cfg := Config{AppleVM: initial, SSHUser: "generic-user", WorkRoot: "/generic"}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.AppleVM != tc.want {
				t.Fatalf("AppleVM=%+v, want %+v", cfg.AppleVM, tc.want)
			}
			if got := [5]bool{AppleVMImageExplicit(cfg), cfg.appleVMImageSHA256Explicit, AppleVMCPUsExplicit(cfg), AppleVMMemoryExplicit(cfg), AppleVMDiskExplicit(cfg)}; got != [5]bool{tc.marked, tc.marked, tc.marked, tc.marked, tc.marked} {
				t.Fatalf("markers=%v, want all %v", got, tc.marked)
			}
			if cfg.SSHUser != "generic-user" || cfg.WorkRoot != "/generic" || IsWorkRootExplicit(&cfg) {
				t.Fatal("environment changed generic user/root state")
			}
			for i, suffix := range suffixes {
				if os.Getenv("CRABBOX_APPLE_VM_"+suffix) != tc.current[i] || os.Getenv("CRABBOX_APPLE_VZ_"+suffix) != tc.legacy[i] {
					t.Fatalf("environment input %s mutated", suffix)
				}
			}
			for _, suffix := range suffixes {
				t.Setenv("CRABBOX_APPLE_VM_"+suffix, "")
				t.Setenv("CRABBOX_APPLE_VZ_"+suffix, "")
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.AppleVM != tc.want || [5]bool{AppleVMImageExplicit(cfg), cfg.appleVMImageSHA256Explicit, AppleVMCPUsExplicit(cfg), AppleVMMemoryExplicit(cfg), AppleVMDiskExplicit(cfg)} != [5]bool{tc.marked, tc.marked, tc.marked, tc.marked, tc.marked} {
				t.Fatal("empty environment changed existing values or markers")
			}
		})
	}
	for _, prefix := range []string{"CRABBOX_APPLE_VM_", "CRABBOX_APPLE_VZ_"} {
		t.Run(prefix+"image-sequence", func(t *testing.T) {
			clearConfigEnv(t)
			cfg := Config{AppleVM: initial}
			for _, step := range []struct {
				image, checksum, wantImage, wantChecksum string
				imageMarked, checksumMarked              bool
			}{
				{"", initial.ImageSHA256, initial.Image, initial.ImageSHA256, false, true},
				{initial.Image, "", initial.Image, "", true, false},
				{"", " next-checksum ", initial.Image, " next-checksum ", true, true},
				{" next-image ", " paired-checksum ", " next-image ", " paired-checksum ", true, true},
				{"", "", " next-image ", " paired-checksum ", true, true},
			} {
				t.Setenv(prefix+"IMAGE", step.image)
				t.Setenv(prefix+"IMAGE_SHA256", step.checksum)
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				if cfg.AppleVM.Image != step.wantImage || cfg.AppleVM.ImageSHA256 != step.wantChecksum || AppleVMImageExplicit(cfg) != step.imageMarked || cfg.appleVMImageSHA256Explicit != step.checksumMarked {
					t.Fatalf("step %+v: image/checksum=%q/%q markers=%v/%v", step, cfg.AppleVM.Image, cfg.AppleVM.ImageSHA256, AppleVMImageExplicit(cfg), cfg.appleVMImageSHA256Explicit)
				}
			}
		})
	}
}

func TestAppleVMOrdinaryEnvironmentNumericErrors(t *testing.T) {
	for _, prefix := range []string{"CRABBOX_APPLE_VM_", "CRABBOX_APPLE_VZ_"} {
		for failed, suffix := range []string{"CPUS", "MEMORY", "DISK"} {
			for _, raw := range []string{"garbage", " \t ", "1.5", "1_024", "9999999999999999999999999999999999999999"} {
				for _, alreadyMarked := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s%s/%q/marked=%v", prefix, suffix, raw, alreadyMarked), func(t *testing.T) {
						clearConfigEnv(t)
						cfg := Config{AppleVM: AppleVMConfig{Image: "before-image", ImageSHA256: "before-checksum", CPUs: 4, MemoryMiB: 8192, DiskGiB: 30}}
						if alreadyMarked {
							MarkAppleVMCPUsExplicit(&cfg)
							MarkAppleVMMemoryExplicit(&cfg)
							MarkAppleVMDiskExplicit(&cfg)
						}
						MarkAppleVMImageSHA256Explicit(&cfg)
						for key, value := range map[string]string{"HELPER": " ~/helper ", "IMAGE": " ~/image ", "USER": " user ", "WORK_ROOT": " ~/work "} {
							t.Setenv(prefix+key, value)
						}
						for i, numeric := range []string{"CPUS", "MEMORY", "DISK"} {
							value := " +6 "
							if i == failed {
								value = raw
							} else if i > failed {
								value = "later-invalid"
							}
							t.Setenv(prefix+numeric, value)
							if prefix == "CRABBOX_APPLE_VM_" {
								t.Setenv("CRABBOX_APPLE_VZ_"+numeric, "10")
							}
						}
						// A later ordinary provider assignment must not be reached.
						cfg.MXC.CLIPath = "before-cli"
						t.Setenv("CRABBOX_MXC_CLI", "after-cli")
						err := applyEnv(&cfg)
						_, parseErr := strconv.Atoi(strings.TrimSpace(raw))
						wantError := fmt.Sprintf("CRABBOX_APPLE_VM_%s must be an integer: %v", suffix, parseErr)
						if err == nil || err.Error() != wantError {
							t.Fatalf("error=%v, want %s", err, wantError)
						}
						var numericErr *strconv.NumError
						if !errors.As(err, &numericErr) || *numericErr != *parseErr.(*strconv.NumError) {
							t.Fatalf("error did not retain trimmed numeric parse cause: %v", err)
						}
						want := AppleVMConfig{HelperPath: " ~/helper ", Image: " ~/image ", User: " user ", WorkRoot: " ~/work ", CPUs: 4, MemoryMiB: 8192, DiskGiB: 30}
						if failed > 0 {
							want.CPUs = 6
						}
						if failed > 1 {
							want.MemoryMiB = 6
						}
						if cfg.AppleVM != want {
							t.Fatalf("partial AppleVM=%+v, want %+v", cfg.AppleVM, want)
						}
						if got := [3]bool{AppleVMCPUsExplicit(cfg), AppleVMMemoryExplicit(cfg), AppleVMDiskExplicit(cfg)}; got != [3]bool{alreadyMarked || failed > 0, alreadyMarked || failed > 1, alreadyMarked} {
							t.Fatalf("partial numeric markers=%v", got)
						}
						if !AppleVMImageExplicit(cfg) || cfg.appleVMImageSHA256Explicit || cfg.MXC.CLIPath != "before-cli" {
							t.Fatal("wrong image markers or later provider mutation")
						}
					})
				}
			}
		}
	}
}

func TestAppleVMConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "apple-vm"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVM.User != "crabbox" || cfg.AppleVM.WorkRoot != "/work/crabbox" || cfg.AppleVM.CPUs != 4 || cfg.AppleVM.MemoryMiB != 8192 || cfg.AppleVM.DiskGiB != 30 {
		t.Fatalf("apple-vm defaults not applied: %#v", cfg.AppleVM)
	}
	wantImage, err := osImageDefaultAppleVMImage("ubuntu:26.04")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA256, err := osImageDefaultAppleVMSHA256("ubuntu:26.04")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVM.Image != wantImage || cfg.AppleVM.ImageSHA256 != wantSHA256 {
		t.Fatalf("apple-vm default source=(%q, %q), want portable OS image source", cfg.AppleVM.Image, cfg.AppleVM.ImageSHA256)
	}
	if cfg.SSHUser != "crabbox" || cfg.SSHPort != "22" || cfg.WorkRoot != "/work/crabbox" || cfg.TargetOS != targetLinux {
		t.Fatalf("apple-vm derived defaults not applied: sshUser=%q sshPort=%q workRoot=%q target=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, cfg.TargetOS)
	}
	fileCPUs := 6
	fileMemoryMiB := 12288
	fileDiskGiB := 64
	applyFileConfig(&cfg, fileConfig{
		Provider: "apple-vm",
		AppleVM: &fileAppleVMConfig{
			HelperPath:  "/opt/bin/crabbox-apple-vm-helper",
			Image:       "https://example.test/custom.img",
			ImageSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			User:        "runner",
			WorkRoot:    "/work/example",
			CPUs:        &fileCPUs,
			MemoryMiB:   &fileMemoryMiB,
			DiskGiB:     &fileDiskGiB,
		},
	})
	if cfg.Provider != "apple-vm" || cfg.AppleVM.HelperPath != "/opt/bin/crabbox-apple-vm-helper" || cfg.AppleVM.Image != "https://example.test/custom.img" || cfg.AppleVM.ImageSHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || cfg.AppleVM.User != "runner" || cfg.AppleVM.WorkRoot != "/work/example" || cfg.AppleVM.CPUs != 6 || cfg.AppleVM.MemoryMiB != 12288 || cfg.AppleVM.DiskGiB != 64 {
		t.Fatalf("file appleVM config not applied: %#v", cfg.AppleVM)
	}
	if !AppleVMCPUsExplicit(cfg) || !AppleVMMemoryExplicit(cfg) || !AppleVMDiskExplicit(cfg) {
		t.Fatal("file appleVM numeric settings should be marked explicit")
	}

	t.Setenv("CRABBOX_APPLE_VM_HELPER", "/usr/local/bin/crabbox-apple-vm-helper")
	t.Setenv("CRABBOX_APPLE_VM_IMAGE", "https://example.test/env.img")
	t.Setenv("CRABBOX_APPLE_VM_IMAGE_SHA256", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	t.Setenv("CRABBOX_APPLE_VM_USER", "env-user")
	t.Setenv("CRABBOX_APPLE_VM_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_APPLE_VM_CPUS", "8")
	t.Setenv("CRABBOX_APPLE_VM_MEMORY", "16384")
	t.Setenv("CRABBOX_APPLE_VM_DISK", "80")
	applyEnv(&cfg)
	if cfg.AppleVM.HelperPath != "/usr/local/bin/crabbox-apple-vm-helper" || cfg.AppleVM.Image != "https://example.test/env.img" || cfg.AppleVM.ImageSHA256 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || cfg.AppleVM.User != "env-user" || cfg.AppleVM.WorkRoot != "/work/env" || cfg.AppleVM.CPUs != 8 || cfg.AppleVM.MemoryMiB != 16384 || cfg.AppleVM.DiskGiB != 80 {
		t.Fatalf("env appleVM config not applied: %#v", cfg.AppleVM)
	}
	if !AppleVMCPUsExplicit(cfg) || !AppleVMMemoryExplicit(cfg) || !AppleVMDiskExplicit(cfg) {
		t.Fatal("env appleVM numeric settings should be marked explicit")
	}
}

func TestAppleVMNumericSettingsPreserveExplicitZero(t *testing.T) {
	clearConfigEnv(t)
	fileZeroCPUs := 0
	fileZero := 0
	fileZeroDisk := 0
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{AppleVM: &fileAppleVMConfig{
		CPUs:      &fileZeroCPUs,
		MemoryMiB: &fileZero,
		DiskGiB:   &fileZeroDisk,
	}})
	if cfg.AppleVM.CPUs != 0 || cfg.AppleVM.MemoryMiB != 0 || cfg.AppleVM.DiskGiB != 0 ||
		!AppleVMCPUsExplicit(cfg) || !AppleVMMemoryExplicit(cfg) || !AppleVMDiskExplicit(cfg) {
		t.Fatalf("file appleVM=%+v explicit=%v/%v/%v", cfg.AppleVM, AppleVMCPUsExplicit(cfg), AppleVMMemoryExplicit(cfg), AppleVMDiskExplicit(cfg))
	}

	cfg = baseConfig()
	t.Setenv("CRABBOX_APPLE_VM_CPUS", "0")
	t.Setenv("CRABBOX_APPLE_VM_MEMORY", "0")
	t.Setenv("CRABBOX_APPLE_VM_DISK", "0")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVM.CPUs != 0 || cfg.AppleVM.MemoryMiB != 0 || cfg.AppleVM.DiskGiB != 0 ||
		!AppleVMCPUsExplicit(cfg) || !AppleVMMemoryExplicit(cfg) || !AppleVMDiskExplicit(cfg) {
		t.Fatalf("env appleVM=%+v explicit=%v/%v/%v", cfg.AppleVM, AppleVMCPUsExplicit(cfg), AppleVMMemoryExplicit(cfg), AppleVMDiskExplicit(cfg))
	}
}

func TestAppleVMNumericSettingsRejectInvalidEnvironmentValues(t *testing.T) {
	for _, name := range []string{"CRABBOX_APPLE_VM_CPUS", "CRABBOX_APPLE_VM_MEMORY", "CRABBOX_APPLE_VM_DISK"} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			t.Setenv(name, "garbage")
			if err := applyEnv(&cfg); err == nil || !strings.Contains(err.Error(), name+" must be an integer") {
				t.Fatalf("applyEnv error=%v", err)
			}
		})
	}
}

func TestAppleVMConfigDefaultsRedactSignedImageServerType(t *testing.T) {
	for _, image := range []string{
		"https://alice:secret@example.test/images/ubuntu.img?token=private#fragment",
		"HTTPS://alice:secret@example.test/images/ubuntu.img?token=private#fragment",
	} {
		cfg := baseConfig()
		cfg.Provider = "apple-vm"
		cfg.AppleVM.Image = image
		cfg.AppleVM.ImageSHA256 = strings.Repeat("a", 64)

		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.ServerType != "<remote-image>" {
			t.Fatalf("ServerType=%q", cfg.ServerType)
		}
		if !strings.Contains(cfg.AppleVM.Image, "token=private") {
			t.Fatalf("AppleVM.Image should retain the request URL in memory: %q", cfg.AppleVM.Image)
		}
	}
}

func TestMultipassOrdinarySourceMetadata(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	image, err := osImageDefaultMultipassImage(cfg.OSImage)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := MultipassConfig{CLIPath: "multipass", Image: image, User: "crabbox", WorkRoot: "/work/crabbox", CPUs: 4, Memory: "8G", Disk: "30G", LaunchTimeout: 20 * time.Minute}
	if cfg.Multipass != wantDefault || cfg.multipassImageExplicit {
		t.Fatalf("defaults=%#v want %#v", cfg.Multipass, wantDefault)
	}
	for _, source := range []string{"file", "env"} {
		for _, tc := range []struct {
			text, cpu, duration string
			fileCPU, envCPU     int
			wantDuration        time.Duration
		}{{"", "0", "", 5, 0, time.Minute}, {"~/literal", "-2", "0s", 5, -2, time.Minute}, {" ", "3", " 2m ", 3, 3, time.Minute}, {"same", "bad", "invalid", 5, 5, time.Minute}, {"same", "6", "2m", 6, 6, 2 * time.Minute}} {
			t.Run(source+"/"+tc.text+"/"+tc.duration, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				cfg.Multipass = MultipassConfig{CLIPath: "same", Image: "same", User: "same", WorkRoot: "same", CPUs: 5, Memory: "same", Disk: "same", LaunchTimeout: time.Minute}
				genericRoot, genericUser := cfg.WorkRoot, cfg.SSHUser
				wantCPU := tc.fileCPU
				if source == "file" {
					cpu, _ := strconv.Atoi(tc.cpu)
					if err := applyFileConfig(&cfg, fileConfig{Multipass: &fileMultipassConfig{CLIPath: tc.text, Image: tc.text, User: tc.text, WorkRoot: tc.text, CPUs: cpu, Memory: tc.text, Disk: tc.text, LaunchTimeout: tc.duration}}); err != nil {
						t.Fatal(err)
					}
				} else {
					wantCPU = tc.envCPU
					for key, value := range map[string]string{"CLI": tc.text, "IMAGE": tc.text, "USER": tc.text, "WORK_ROOT": tc.text, "CPUS": tc.cpu, "MEMORY": tc.text, "DISK": tc.text, "LAUNCH_TIMEOUT": tc.duration} {
						t.Setenv("CRABBOX_MULTIPASS_"+key, value)
					}
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				}
				text := tc.text
				if text == "" {
					text = "same"
				}
				want := MultipassConfig{CLIPath: text, Image: text, User: text, WorkRoot: text, CPUs: wantCPU, Memory: text, Disk: text, LaunchTimeout: tc.wantDuration}
				if cfg.Multipass != want || cfg.multipassImageExplicit != (tc.text != "") || cfg.WorkRoot != genericRoot || cfg.SSHUser != genericUser {
					t.Fatalf("source=%#v want %#v", cfg.Multipass, want)
				}
			})
		}
	}
}

func TestMultipassOrdinaryWriterMetadata(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"multipass: null", "{}"}, {"multipass: {}", "multipass: {}"}, {"multipass: {cliPath: '', image: '', user: '', workRoot: '', cpus: 0, memory: '', disk: '', launchTimeout: ''}", "multipass: {}"}, {"multipass: {cliPath: '~/literal', image: custom-image, user: example, workRoot: '~/guest', cpus: -2, memory: 4G, disk: 20G, launchTimeout: 0s}", "multipass: {cliPath: '~/literal', image: custom-image, user: example, workRoot: '~/guest', cpus: -2, memory: 4G, disk: 20G, launchTimeout: 0s}"}} {
		path := isolatedConfigPath(t)
		if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		before, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		after, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("source overlay changed file DTO")
		}
		if _, err := writeUserFileConfig(file); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got, want map[string]any
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(tc.want), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("writer=%#v want %#v", got, want)
		}
	}
	if reflect.TypeOf(fileMultipassConfig{}).Name() != "fileMultipassConfig" {
		t.Fatal("file DTO identity")
	}
}

func TestMultipassConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Multipass.CLIPath != "multipass" || cfg.Multipass.Image != "26.04" || cfg.Multipass.User != "crabbox" {
		t.Fatalf("multipass defaults not applied: %#v", cfg.Multipass)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "multipass",
		Multipass: &fileMultipassConfig{
			CLIPath:       "/opt/bin/multipass",
			Image:         "24.04",
			User:          "runner",
			WorkRoot:      "/work/example",
			CPUs:          4,
			Memory:        "8G",
			Disk:          "40G",
			LaunchTimeout: "7m",
		},
	})
	if cfg.Provider != "multipass" || cfg.Multipass.CLIPath != "/opt/bin/multipass" || cfg.Multipass.Image != "24.04" || cfg.Multipass.User != "runner" || cfg.Multipass.WorkRoot != "/work/example" || cfg.Multipass.CPUs != 4 || cfg.Multipass.Memory != "8G" || cfg.Multipass.Disk != "40G" || cfg.Multipass.LaunchTimeout != 7*time.Minute {
		t.Fatalf("file multipass config not applied: %#v", cfg.Multipass)
	}

	t.Setenv("CRABBOX_MULTIPASS_CLI", "/usr/local/bin/multipass")
	t.Setenv("CRABBOX_MULTIPASS_IMAGE", "26.04")
	t.Setenv("CRABBOX_MULTIPASS_USER", "env-user")
	t.Setenv("CRABBOX_MULTIPASS_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_MULTIPASS_CPUS", "6")
	t.Setenv("CRABBOX_MULTIPASS_MEMORY", "12G")
	t.Setenv("CRABBOX_MULTIPASS_DISK", "80G")
	t.Setenv("CRABBOX_MULTIPASS_LAUNCH_TIMEOUT", "11m")
	applyEnv(&cfg)
	if cfg.Multipass.CLIPath != "/usr/local/bin/multipass" || cfg.Multipass.Image != "26.04" || cfg.Multipass.User != "env-user" || cfg.Multipass.WorkRoot != "/work/env" || cfg.Multipass.CPUs != 6 || cfg.Multipass.Memory != "12G" || cfg.Multipass.Disk != "80G" || cfg.Multipass.LaunchTimeout != 11*time.Minute {
		t.Fatalf("env multipass config not applied: %#v", cfg.Multipass)
	}
}

func TestMachine0OrdinarySourceMetadata(t *testing.T) {
	clearConfigEnv(t)
	wantDefault := Machine0Config{CLIPath: "machine0", Image: "ubuntu-24-04-loaded", Size: "large", Region: "eu", ReleasePolicy: "destroy", CreateTimeout: 15 * time.Minute, PollInterval: time.Minute}
	if got := baseConfig().Machine0; got != wantDefault {
		t.Fatalf("defaults=%#v want %#v", got, wantDefault)
	}
	for _, source := range []string{"file", "env"} {
		for _, marked := range []bool{false, true} {
			for _, tc := range []struct {
				text, version, duration string
				wantVersion             int
				wantDuration            time.Duration
			}{{"", "", "", 5, time.Minute}, {"large", "0", "0s", 0, time.Minute}, {"~/literal", "-2", " 2m ", -2, time.Minute}, {" ", "bad", "invalid", 5, time.Minute}, {"large", "3", "2m", 3, 2 * time.Minute}} {
				t.Run(source+"/"+tc.text+"/"+tc.version+"/"+strconv.FormatBool(marked), func(t *testing.T) {
					clearConfigEnv(t)
					cfg := baseConfig()
					cfg.Machine0 = Machine0Config{CLIPath: "prior", Image: "prior", ImageVersion: 5, DesktopImage: "prior", Size: "large", SizeExplicit: marked, Region: "prior", Key: "prior", WorkRoot: "prior", ReleasePolicy: "prior", CreateTimeout: time.Minute, PollInterval: time.Minute}
					genericRoot, genericSize := cfg.WorkRoot, cfg.ServerType
					if source == "file" {
						var version *int
						if tc.version != "" && tc.version != "bad" {
							value, err := strconv.Atoi(tc.version)
							if err != nil {
								t.Fatal(err)
							}
							version = &value
						}
						if err := applyFileConfig(&cfg, fileConfig{Machine0: &fileMachine0Config{CLIPath: tc.text, Image: tc.text, ImageVersion: version, DesktopImage: tc.text, Size: tc.text, Region: tc.text, Key: tc.text, WorkRoot: tc.text, ReleasePolicy: tc.text, CreateTimeout: tc.duration, PollInterval: tc.duration}}); err != nil {
							t.Fatal(err)
						}
					} else {
						for key, value := range map[string]string{"CLI": tc.text, "IMAGE": tc.text, "IMAGE_VERSION": tc.version, "DESKTOP_IMAGE": tc.text, "SIZE": tc.text, "REGION": tc.text, "KEY": tc.text, "WORK_ROOT": tc.text, "RELEASE_POLICY": tc.text, "CREATE_TIMEOUT": tc.duration, "POLL_INTERVAL": tc.duration} {
							t.Setenv("CRABBOX_MACHINE0_"+key, value)
						}
						if err := applyEnv(&cfg); err != nil {
							t.Fatal(err)
						}
					}
					text := tc.text
					if text == "" {
						text = "prior"
					}
					size := tc.text
					if size == "" {
						size = "large"
					}
					want := Machine0Config{CLIPath: text, Image: text, ImageVersion: tc.wantVersion, DesktopImage: text, Size: size, SizeExplicit: marked || tc.text != "", Region: text, Key: text, WorkRoot: text, ReleasePolicy: text, CreateTimeout: tc.wantDuration, PollInterval: tc.wantDuration}
					if cfg.Machine0 != want || cfg.WorkRoot != genericRoot || cfg.ServerType != genericSize || cfg.ServerTypeExplicit {
						t.Fatalf("source=%#v want %#v", cfg.Machine0, want)
					}
				})
			}
		}
	}
}

func TestMachine0OrdinaryWriterMetadata(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"machine0: null", "{}"}, {"machine0: {}", "machine0: {}"}, {"machine0: {cliPath: '', image: '', imageVersion: null, desktopImage: '', size: '', region: '', key: '', workRoot: '', releasePolicy: '', createTimeout: '', pollInterval: ''}", "machine0: {}"}, {"machine0: {imageVersion: 0, createTimeout: 0s, pollInterval: 0s}", "machine0: {imageVersion: 0, createTimeout: 0s, pollInterval: 0s}"}, {"machine0: {cliPath: '~/literal', image: image-example, imageVersion: -2, desktopImage: desktop-example, size: large, region: eu, key: name-example, workRoot: '~/guest', releasePolicy: suspend, createTimeout: 2m, pollInterval: 3s}", "machine0: {cliPath: '~/literal', image: image-example, imageVersion: -2, desktopImage: desktop-example, size: large, region: eu, key: name-example, workRoot: '~/guest', releasePolicy: suspend, createTimeout: 2m, pollInterval: 3s}"}} {
		path := isolatedConfigPath(t)
		if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		before, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		after, err := yaml.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("overlay changed DTO input")
		}
		if _, err := writeUserFileConfig(file); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got, want map[string]any
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(tc.want), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("writer=%#v want %#v", got, want)
		}
	}
}

func TestMachine0ConfigDefaultsFileAndEnvPrecedence(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Machine0.CLIPath != "machine0" || cfg.Machine0.Image != "ubuntu-24-04-loaded" || cfg.Machine0.Size != "large" || cfg.Machine0.SizeExplicit || cfg.Machine0.Region != "eu" || cfg.Machine0.WorkRoot != "" || cfg.Machine0.ReleasePolicy != "destroy" || cfg.Machine0.PollInterval != 60*time.Second {
		t.Fatalf("machine0 defaults not applied: poll interval=%ds, want=60s; config=%#v", cfg.Machine0.PollInterval/time.Second, cfg.Machine0)
	}
	imageVersion := 2
	applyFileConfig(&cfg, fileConfig{Provider: "machine0", Machine0: &fileMachine0Config{
		CLIPath: "/opt/bin/machine0", Image: "ubuntu-ci", ImageVersion: &imageVersion, DesktopImage: "ubuntu-desktop", Size: "xl-nvme", Region: "us-west", Key: "ci-key", WorkRoot: "/work/example", ReleasePolicy: "suspend", CreateTimeout: "9m", PollInterval: "3s",
	}})
	if cfg.Machine0.CLIPath != "/opt/bin/machine0" || cfg.Machine0.Image != "ubuntu-ci" || cfg.Machine0.ImageVersion != 2 || cfg.Machine0.DesktopImage != "ubuntu-desktop" || cfg.Machine0.Size != "xl-nvme" || !cfg.Machine0.SizeExplicit || cfg.Machine0.Region != "us-west" || cfg.Machine0.Key != "ci-key" || cfg.Machine0.WorkRoot != "/work/example" || cfg.Machine0.ReleasePolicy != "suspend" || cfg.Machine0.CreateTimeout != 9*time.Minute || cfg.Machine0.PollInterval != 3*time.Second {
		t.Fatalf("file machine0 config not applied: %#v", cfg.Machine0)
	}
	activeVersion := 0
	applyFileConfig(&cfg, fileConfig{Machine0: &fileMachine0Config{ImageVersion: &activeVersion}})
	if cfg.Machine0.ImageVersion != 0 {
		t.Fatalf("higher-precedence YAML could not reset imageVersion to active: %#v", cfg.Machine0)
	}
	t.Setenv("CRABBOX_MACHINE0_CLI", "/usr/local/bin/machine0")
	t.Setenv("CRABBOX_MACHINE0_IMAGE", "ubuntu-env")
	t.Setenv("CRABBOX_MACHINE0_IMAGE_VERSION", "4")
	t.Setenv("CRABBOX_MACHINE0_DESKTOP_IMAGE", "desktop-env")
	t.Setenv("CRABBOX_MACHINE0_SIZE", "gpu-h100-1")
	t.Setenv("CRABBOX_MACHINE0_REGION", "us-east")
	t.Setenv("CRABBOX_MACHINE0_KEY", "env-key")
	t.Setenv("CRABBOX_MACHINE0_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_MACHINE0_RELEASE_POLICY", "destroy")
	t.Setenv("CRABBOX_MACHINE0_CREATE_TIMEOUT", "12m")
	t.Setenv("CRABBOX_MACHINE0_POLL_INTERVAL", "5s")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Machine0.CLIPath != "/usr/local/bin/machine0" || cfg.Machine0.Image != "ubuntu-env" || cfg.Machine0.ImageVersion != 4 || cfg.Machine0.DesktopImage != "desktop-env" || cfg.Machine0.Size != "gpu-h100-1" || !cfg.Machine0.SizeExplicit || cfg.Machine0.Region != "us-east" || cfg.Machine0.Key != "env-key" || cfg.Machine0.WorkRoot != "/work/env" || cfg.Machine0.ReleasePolicy != "destroy" || cfg.Machine0.CreateTimeout != 12*time.Minute || cfg.Machine0.PollInterval != 5*time.Second {
		t.Fatalf("environment did not override machine0 file config: %#v", cfg.Machine0)
	}
}

func TestMachine0NativeSizeConfigProvenanceAndPrecedence(t *testing.T) {
	clearConfigEnv(t)
	tests := []struct {
		name         string
		yaml         string
		envSize      string
		wantSize     string
		wantExplicit bool
	}{
		{name: "legacy config without native size keeps implicit default", yaml: "class: fast\nmachine0:\n  image: ubuntu-ci\n", wantSize: "large"},
		{name: "yaml default-valued size remains explicit", yaml: "class: fast\nmachine0:\n  size: large\n", wantSize: "large", wantExplicit: true},
		{name: "yaml arbitrary native size remains explicit", yaml: "class: fast\nmachine0:\n  size: gpu-h100-1\n", wantSize: "gpu-h100-1", wantExplicit: true},
		{name: "environment default-valued size remains explicit", yaml: "class: fast\n", envSize: "large", wantSize: "large", wantExplicit: true},
		{name: "environment arbitrary native size remains explicit", yaml: "class: fast\n", envSize: "gpu-h100-1", wantSize: "gpu-h100-1", wantExplicit: true},
		{name: "environment native size overrides yaml native size", yaml: "class: fast\nmachine0:\n  size: gpu-h100-1\n", envSize: "large", wantSize: "large", wantExplicit: true},
		{name: "empty environment preserves explicit yaml size", yaml: "class: fast\nmachine0:\n  size: large\n", wantSize: "large", wantExplicit: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRABBOX_MACHINE0_SIZE", tc.envSize)
			cfg := baseConfig()
			var file fileConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &file); err != nil {
				t.Fatalf("parse YAML: %v", err)
			}
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatalf("apply YAML: %v", err)
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatalf("apply environment: %v", err)
			}
			if cfg.Machine0.Size != tc.wantSize || cfg.Machine0.SizeExplicit != tc.wantExplicit {
				t.Fatalf("native size=%q explicit=%t, want size=%q explicit=%t", cfg.Machine0.Size, cfg.Machine0.SizeExplicit, tc.wantSize, tc.wantExplicit)
			}
			if !ClassWasExplicit(cfg) {
				t.Fatal("portable class provenance was lost")
			}
		})
	}
}

func TestTartConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "tart"
	cfg.Tart.Image = "ghcr.io/test:latest"
	cfg.Tart.User = "admin"
	cfg.Tart.WorkRoot = "/Users/admin/work"
	cfg.Tart.CPUs = 4
	cfg.Tart.Memory = 8192
	cfg.Tart.Disk = 50
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "admin" {
		t.Fatalf("SSHUser=%q, want admin", cfg.SSHUser)
	}
	if cfg.SSHPort != "22" {
		t.Fatalf("SSHPort=%q, want 22", cfg.SSHPort)
	}
	if cfg.SSHFallbackPorts != nil {
		t.Fatalf("SSHFallbackPorts=%v, want nil", cfg.SSHFallbackPorts)
	}
	if cfg.WorkRoot != "/Users/admin/work" {
		t.Fatalf("WorkRoot=%q, want /Users/admin/work", cfg.WorkRoot)
	}
	if cfg.TargetOS != "macos" {
		t.Fatalf("TargetOS=%q, want macos", cfg.TargetOS)
	}
	if cfg.ServerType != "ghcr.io/test:latest" {
		t.Fatalf("ServerType=%q, want ghcr.io/test:latest", cfg.ServerType)
	}

	// env overrides
	t.Setenv("CRABBOX_TART_IMAGE", "ghcr.io/env:latest")
	t.Setenv("CRABBOX_TART_USER", "env-user")
	t.Setenv("CRABBOX_TART_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_TART_CPUS", "8")
	t.Setenv("CRABBOX_TART_MEMORY", "16384")
	t.Setenv("CRABBOX_TART_DISK", "100")
	applyEnv(&cfg)
	if cfg.Tart.Image != "ghcr.io/env:latest" || cfg.Tart.User != "env-user" || cfg.Tart.WorkRoot != "/work/env" || cfg.Tart.CPUs != 8 || cfg.Tart.Memory != 16384 || cfg.Tart.Disk != 100 {
		t.Fatalf("env tart config not applied: %+v", cfg.Tart)
	}
	if !cfg.tartDiskExplicit {
		t.Fatal("positive CRABBOX_TART_DISK should mark tart disk explicit")
	}
	t.Setenv("CRABBOX_TART_DISK", "0")
	applyEnv(&cfg)
	if cfg.Tart.Disk != 0 {
		t.Fatalf("zero CRABBOX_TART_DISK disk=%d, want clone default 0", cfg.Tart.Disk)
	}
	if cfg.tartDiskExplicit {
		t.Fatal("zero CRABBOX_TART_DISK should not mark tart disk explicit")
	}
}

func TestCoderOrdinaryFileMetadata(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	wantDefault := CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}
	if got := baseConfig().Coder; !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("defaults=%#v want %#v", got, wantDefault)
	}
	for _, tc := range []struct {
		name        string
		input, want []string
	}{{"nil", nil, []string{"prior"}}, {"empty", []string{}, []string{"prior"}}, {"all blank", []string{" ", ""}, []string{}}, {"normalized clone", []string{" a=1 ", " ", "b=2", "a=1"}, []string{"a=1", "b=2", "a=1"}}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Coder.Parameters = []string{"prior"}
			generic := cfg.WorkRoot
			no := false
			file := fileConfig{Coder: &fileCoderConfig{CLIPath: "~/coder", Template: " template ", Preset: " preset ", WorkspacePrefix: " prefix ", WorkRoot: "~/guest", DeleteOnRelease: &no, Wait: " auto ", UseParameterDefaults: &no, Parameters: tc.input, RichParameterFile: "~/params"}}
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal(err)
			}
			want := CoderConfig{CLIPath: filepath.Join(home, "coder"), Template: " template ", Preset: " preset ", WorkspacePrefix: " prefix ", WorkRoot: "~/guest", Wait: " auto ", Parameters: tc.want, RichParameterFile: filepath.Join(home, "params")}
			if !reflect.DeepEqual(cfg.Coder, want) || cfg.WorkRoot != generic {
				t.Fatalf("file=%#v want %#v", cfg.Coder, want)
			}
			if len(tc.input) > 0 && len(tc.want) > 0 {
				tc.input[0] = "changed"
				if cfg.Coder.Parameters[0] != "a=1" {
					t.Fatal("file list not cloned")
				}
			}
		})
	}
	for _, input := range []string{"{}", "{cliPath: '', richParameterFile: '', deleteOnRelease: null, useParameterDefaults: null}", "{cliPath: '~/coder', richParameterFile: '~/params', deleteOnRelease: false, useParameterDefaults: false}"} {
		var file fileConfig
		if err := yaml.Unmarshal([]byte("coder: "+input), &file); err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		cfg.Coder.CLIPath = "~/coder"
		cfg.Coder.RichParameterFile = "~/params"
		cfg.Coder.DeleteOnRelease = true
		cfg.Coder.UseParameterDefaults = true
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		accepted := strings.Contains(input, "~/")
		wantCLI, wantRich := "~/coder", "~/params"
		if accepted {
			wantCLI, wantRich = filepath.Join(home, "coder"), filepath.Join(home, "params")
		}
		if cfg.Coder.CLIPath != wantCLI || cfg.Coder.RichParameterFile != wantRich || cfg.Coder.DeleteOnRelease == accepted || cfg.Coder.UseParameterDefaults == accepted {
			t.Fatal("file accepted path/bool presence")
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Coder.CLIPath != filepath.Join(home, "coder") || cfg.Coder.RichParameterFile != filepath.Join(home, "params") {
			t.Fatal("env fallback path expansion")
		}
	}
}

func TestCoderOrdinaryEnvMetadata(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{{"", []string{"prior"}}, {" \t ", []string{"prior"}}, {" NoNe ", []string{}}, {", ,", []string{}}, {" a=1, ,b=2,a=1 ", []string{"a=1", "b=2", "a=1"}}} {
		t.Run(tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			for key, value := range map[string]string{"CLI": "~/coder", "TEMPLATE": " template ", "PRESET": " preset ", "WORKSPACE_PREFIX": " prefix ", "WORK_ROOT": "~/guest", "DELETE_ON_RELEASE": "false", "WAIT": " auto ", "USE_PARAMETER_DEFAULTS": "false", "PARAMETERS": tc.raw, "RICH_PARAMETER_FILE": "~/params"} {
				t.Setenv("CRABBOX_CODER_"+key, value)
			}
			cfg := baseConfig()
			cfg.Coder.Parameters = []string{"prior"}
			cfg.Coder.DeleteOnRelease = true
			cfg.Coder.UseParameterDefaults = true
			generic := cfg.WorkRoot
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			want := CoderConfig{CLIPath: filepath.Join(home, "coder"), Template: " template ", Preset: " preset ", WorkspacePrefix: " prefix ", WorkRoot: "~/guest", Wait: " auto ", Parameters: tc.want, RichParameterFile: filepath.Join(home, "params")}
			if !reflect.DeepEqual(cfg.Coder, want) || cfg.WorkRoot != generic {
				t.Fatalf("env=%#v want %#v", cfg.Coder, want)
			}
		})
	}
}

func TestCoderOrdinaryCodecAndWriter(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"coder: {}\n", "coder: {}\n"}, {"coder: {parameters: [], deleteOnRelease: false, useParameterDefaults: false}\n", "coder: {deleteOnRelease: false, useParameterDefaults: false}\n"}, {"coder: {parameters: [' a=1 ', '', 'a=1']}\n", "coder: {parameters: ['a=1', 'a=1']}\n"}, {"coder: {parameters: [' ', '']}\n", "coder: {}\n"}} {
		path := isolatedConfigPath(t)
		if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeUserFileConfig(file); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got, want map[string]any
		if err := yaml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(tc.want), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("writer=%#v want %#v", got, want)
		}
	}
	for _, tc := range []struct{ input, kind string }{{"parameters: a=1", "!!str `a=1`"}, {"parameters: {a: b}", "!!map"}} {
		file := fileCoderConfig{Template: "prior"}
		err := yaml.Unmarshal([]byte(tc.input), &file)
		want := "yaml: unmarshal errors:\n  line 1: cannot unmarshal " + tc.kind + " into []string"
		if err == nil || err.Error() != want || file.Template != "prior" {
			t.Fatalf("codec error=%v, want %q; template=%q", err, want, file.Template)
		}
	}
}

func TestCoderConfigDefaultsSetWorkRoot(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "coder"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/home/coder/crabbox" {
		t.Fatalf("WorkRoot=%q want /home/coder/crabbox", cfg.WorkRoot)
	}
	cfg = baseConfig()
	cfg.Provider = "coder"
	cfg.Coder.WorkRoot = "/home/coder/custom"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/home/coder/custom" {
		t.Fatalf("custom WorkRoot=%q want /home/coder/custom", cfg.WorkRoot)
	}
	cfg = baseConfig()
	cfg.Provider = "coder"
	cfg.WorkRoot = "/tmp/explicit"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Coder.WorkRoot != "/tmp/explicit" || cfg.WorkRoot != "/tmp/explicit" {
		t.Fatalf("explicit top-level work root not propagated: coder=%q work=%q", cfg.Coder.WorkRoot, cfg.WorkRoot)
	}
}

func TestIncusConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Incus.Remote != "local" || cfg.Incus.Project != "" || cfg.Incus.InstanceType != "container" || cfg.Incus.Image != "images:ubuntu/24.04/cloud" {
		t.Fatalf("incus defaults not applied: %#v", cfg.Incus)
	}
	deleteOnRelease := false
	insecureTLS := true
	applyFileConfig(&cfg, fileConfig{
		Provider: "incus",
		Incus: &fileIncusConfig{
			Remote:            "lab",
			Project:           "crabbox",
			Address:           "https://incus.example.test:8443",
			Socket:            "~/incus.sock",
			InstanceType:      "vm",
			Image:             "images:ubuntu/26.04/cloud",
			Profile:           "crabbox",
			User:              "ubuntu",
			WorkRoot:          "/workspace/incus",
			DeleteOnRelease:   &deleteOnRelease,
			StartTimeout:      "12m",
			LaunchPort:        "22",
			ProxyListenHost:   "127.0.0.1",
			ProxyListenPort:   "2201",
			ProxyDevice:       "ssh-proxy",
			TLSServerCert:     "~/certs/incus.crt",
			InsecureTLS:       &insecureTLS,
			RemoteImageServer: "https://images.example.test",
		},
	})
	if cfg.Incus.Remote != "lab" || cfg.Incus.Project != "crabbox" || cfg.Incus.Address != "https://incus.example.test:8443" || !strings.HasSuffix(cfg.Incus.Socket, "/incus.sock") {
		t.Fatalf("file incus config not applied: %#v", cfg.Incus)
	}
	if cfg.Incus.InstanceType != "vm" || cfg.Incus.Image != "images:ubuntu/26.04/cloud" || cfg.Incus.Profile != "crabbox" || cfg.Incus.User != "ubuntu" || cfg.Incus.WorkRoot != "/workspace/incus" {
		t.Fatalf("file incus identity config not applied: %#v", cfg.Incus)
	}
	if cfg.Incus.DeleteOnRelease || cfg.Incus.StartTimeout != 12*time.Minute || cfg.Incus.ProxyListenPort != "2201" || cfg.Incus.ProxyDevice != "ssh-proxy" || !strings.HasSuffix(cfg.Incus.TLSServerCert, "/certs/incus.crt") || !cfg.Incus.InsecureTLS || cfg.Incus.RemoteImageServer != "https://images.example.test" {
		t.Fatalf("file incus runtime config not applied: %#v", cfg.Incus)
	}

	t.Setenv("CRABBOX_INCUS_REMOTE", "env-remote")
	t.Setenv("CRABBOX_INCUS_PROJECT", "env-project")
	t.Setenv("CRABBOX_INCUS_ADDRESS", "https://env-incus.example.test:8443")
	t.Setenv("CRABBOX_INCUS_SOCKET", "~/env-incus.sock")
	t.Setenv("CRABBOX_INCUS_INSTANCE_TYPE", "container")
	t.Setenv("CRABBOX_INCUS_IMAGE", "images:debian/12/cloud")
	t.Setenv("CRABBOX_INCUS_PROFILE", "env-profile")
	t.Setenv("CRABBOX_INCUS_USER", "crabuser")
	t.Setenv("CRABBOX_INCUS_WORK_ROOT", "/env/work")
	t.Setenv("CRABBOX_INCUS_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_INCUS_START_TIMEOUT", "5m")
	t.Setenv("CRABBOX_INCUS_LAUNCH_PORT", "2222")
	t.Setenv("CRABBOX_INCUS_PROXY_LISTEN_HOST", "0.0.0.0")
	t.Setenv("CRABBOX_INCUS_PROXY_LISTEN_PORT", "2223")
	t.Setenv("CRABBOX_INCUS_PROXY_DEVICE", "env-proxy")
	t.Setenv("CRABBOX_INCUS_TLS_SERVER_CERT", "~/env-incus.crt")
	t.Setenv("CRABBOX_INCUS_INSECURE_TLS", "false")
	t.Setenv("CRABBOX_INCUS_REMOTE_IMAGE_SERVER", "https://env-images.example.test")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Incus.Remote != "env-remote" || cfg.Incus.Project != "env-project" || cfg.Incus.Address != "https://env-incus.example.test:8443" || !strings.HasSuffix(cfg.Incus.Socket, "/env-incus.sock") {
		t.Fatalf("env incus config not applied: %#v", cfg.Incus)
	}
	if cfg.Incus.InstanceType != "container" || cfg.Incus.Image != "images:debian/12/cloud" || cfg.Incus.Profile != "env-profile" || cfg.Incus.User != "crabuser" || cfg.Incus.WorkRoot != "/env/work" {
		t.Fatalf("env incus identity config not applied: %#v", cfg.Incus)
	}
	if !cfg.Incus.DeleteOnRelease || cfg.Incus.StartTimeout != 5*time.Minute || cfg.Incus.LaunchPort != "2222" || cfg.Incus.ProxyListenPort != "2223" || cfg.Incus.ProxyDevice != "env-proxy" || !strings.HasSuffix(cfg.Incus.TLSServerCert, "/env-incus.crt") || cfg.Incus.InsecureTLS || cfg.Incus.RemoteImageServer != "https://env-images.example.test" {
		t.Fatalf("env incus runtime config not applied: %#v", cfg.Incus)
	}
}

func TestLoadConfigIncusPreservesExplicitTopLevelSSHUserAndWorkRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	body := "provider: incus\nssh:\n  user: alice\nworkRoot: /tmp/custom\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestLoadConfigIncusPreservesExplicitTopLevelSSHUserAndWorkRootFromEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	t.Setenv("CRABBOX_SSH_USER", "alice")
	t.Setenv("CRABBOX_WORK_ROOT", "/tmp/custom")
	if err := os.WriteFile(cfgPath, []byte("provider: incus\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestLoadConfigIncusSpecificUserAndWorkRootOverrideTopLevel(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	body := "provider: incus\nssh:\n  user: alice\nworkRoot: /tmp/custom\nincus:\n  user: ubuntu\n  workRoot: /workspace/incus\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "ubuntu" {
		t.Fatalf("SSHUser=%q want ubuntu", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/workspace/incus" {
		t.Fatalf("WorkRoot=%q want /workspace/incus", cfg.WorkRoot)
	}
}

func TestFirecrackerConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Firecracker.Binary != "firecracker" || cfg.Firecracker.Kernel != "/var/lib/crabbox/firecracker/vmlinux" || cfg.Firecracker.RootFS != "/var/lib/crabbox/firecracker/rootfs.ext4" {
		t.Fatalf("firecracker path defaults not applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.User != "crabbox" || cfg.Firecracker.WorkRoot != "/work/crabbox" || cfg.Firecracker.CPUs != 4 || cfg.Firecracker.MemoryMiB != 4096 || cfg.Firecracker.DiskMiB != 16384 {
		t.Fatalf("firecracker sizing defaults not applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.Network != "cni" || cfg.Firecracker.CNINetwork != "crabbox-firecracker" || cfg.Firecracker.CNIConfDir != "/etc/cni/conf.d" || cfg.Firecracker.CNIBinDir != "/opt/cni/bin" || cfg.Firecracker.LaunchTimeout != 2*time.Minute || !cfg.Firecracker.DeleteOnRelease {
		t.Fatalf("firecracker runtime defaults not applied: %#v", cfg.Firecracker)
	}

	deleteOnRelease := false
	cpus := 6
	memoryMiB := 8192
	diskMiB := 24576
	applyFileConfig(&cfg, fileConfig{
		Provider: "firecracker",
		Firecracker: &fileFirecrackerConfig{
			Binary:          "/opt/bin/firecracker",
			Jailer:          "/opt/bin/jailer",
			Kernel:          "/var/lib/firecracker/vmlinux.custom",
			RootFS:          "/var/lib/firecracker/rootfs.custom.ext4",
			User:            "runner",
			WorkRoot:        "/workspace/firecracker",
			CPUs:            &cpus,
			MemoryMiB:       &memoryMiB,
			DiskMiB:         &diskMiB,
			Network:         "cni",
			CNINetwork:      "lab-firecracker",
			CNIConfDir:      "/etc/cni/lab",
			CNIBinDir:       "/opt/cni/lab",
			LaunchTimeout:   "7m",
			DeleteOnRelease: &deleteOnRelease,
		},
	})
	if cfg.Provider != "firecracker" || cfg.Firecracker.Binary != "/opt/bin/firecracker" || cfg.Firecracker.Jailer != "/opt/bin/jailer" || cfg.Firecracker.Kernel != "/var/lib/firecracker/vmlinux.custom" || cfg.Firecracker.RootFS != "/var/lib/firecracker/rootfs.custom.ext4" {
		t.Fatalf("file firecracker paths not applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.User != "runner" || cfg.Firecracker.WorkRoot != "/workspace/firecracker" || cfg.Firecracker.CPUs != 6 || cfg.Firecracker.MemoryMiB != 8192 || cfg.Firecracker.DiskMiB != 24576 {
		t.Fatalf("file firecracker identity/sizing not applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.CNINetwork != "lab-firecracker" || cfg.Firecracker.CNIConfDir != "/etc/cni/lab" || cfg.Firecracker.CNIBinDir != "/opt/cni/lab" || cfg.Firecracker.LaunchTimeout != 7*time.Minute || cfg.Firecracker.DeleteOnRelease {
		t.Fatalf("file firecracker runtime not applied: %#v", cfg.Firecracker)
	}
	if !DeleteOnReleaseExplicit(cfg, "firecracker") {
		t.Fatal("file firecracker deleteOnRelease should mark explicit state")
	}

	t.Setenv("CRABBOX_FIRECRACKER_BINARY", "/usr/local/bin/firecracker")
	t.Setenv("CRABBOX_FIRECRACKER_JAILER", "/usr/local/bin/jailer")
	t.Setenv("CRABBOX_FIRECRACKER_KERNEL", "/srv/firecracker/vmlinux")
	t.Setenv("CRABBOX_FIRECRACKER_ROOTFS", "/srv/firecracker/rootfs.ext4")
	t.Setenv("CRABBOX_FIRECRACKER_USER", "env-user")
	t.Setenv("CRABBOX_FIRECRACKER_WORK_ROOT", "/env/firecracker")
	t.Setenv("CRABBOX_FIRECRACKER_CPUS", "8")
	t.Setenv("CRABBOX_FIRECRACKER_MEMORY_MIB", "12288")
	t.Setenv("CRABBOX_FIRECRACKER_DISK_MIB", "32768")
	t.Setenv("CRABBOX_FIRECRACKER_NETWORK", "cni")
	t.Setenv("CRABBOX_FIRECRACKER_CNI_NETWORK", "env-firecracker")
	t.Setenv("CRABBOX_FIRECRACKER_CNI_CONF_DIR", "/env/etc/cni/conf.d")
	t.Setenv("CRABBOX_FIRECRACKER_CNI_BIN_DIR", "/env/opt/cni/bin")
	t.Setenv("CRABBOX_FIRECRACKER_LAUNCH_TIMEOUT", "11m")
	t.Setenv("CRABBOX_FIRECRACKER_DELETE_ON_RELEASE", "true")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Firecracker.Binary != "/usr/local/bin/firecracker" || cfg.Firecracker.Jailer != "/usr/local/bin/jailer" || cfg.Firecracker.Kernel != "/srv/firecracker/vmlinux" || cfg.Firecracker.RootFS != "/srv/firecracker/rootfs.ext4" {
		t.Fatalf("env firecracker paths not applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.User != "env-user" || cfg.Firecracker.WorkRoot != "/env/firecracker" || cfg.Firecracker.CPUs != 8 || cfg.Firecracker.MemoryMiB != 12288 || cfg.Firecracker.DiskMiB != 32768 {
		t.Fatalf("env firecracker identity/sizing not applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.CNINetwork != "env-firecracker" || cfg.Firecracker.CNIConfDir != "/env/etc/cni/conf.d" || cfg.Firecracker.CNIBinDir != "/env/opt/cni/bin" || cfg.Firecracker.LaunchTimeout != 11*time.Minute || !cfg.Firecracker.DeleteOnRelease {
		t.Fatalf("env firecracker runtime not applied: %#v", cfg.Firecracker)
	}
	if !DeleteOnReleaseExplicit(cfg, "firecracker") {
		t.Fatal("env firecracker deleteOnRelease should remain explicit")
	}
}

func TestFirecrackerUntrustedConfigCannotRedirectHostExecutionPaths(t *testing.T) {
	cfg := baseConfig()
	cfg.Firecracker.Binary = "/trusted/firecracker"
	cfg.Firecracker.Jailer = "/trusted/jailer"
	cfg.Firecracker.Kernel = "/trusted/vmlinux"
	cfg.Firecracker.RootFS = "/trusted/rootfs.ext4"
	cfg.Firecracker.Network = "cni"
	cfg.Firecracker.CNINetwork = "trusted-firecracker"
	cfg.Firecracker.CNIConfDir = "/trusted/cni/conf.d"
	cfg.Firecracker.CNIBinDir = "/trusted/cni/bin"

	cpus := 8
	memoryMiB := 12288
	diskMiB := 32768
	deleteOnRelease := false
	file := fileConfig{
		Provider: "firecracker",
		Firecracker: &fileFirecrackerConfig{
			Binary:          "./repo/firecracker",
			Jailer:          "./repo/jailer",
			Kernel:          "./repo/vmlinux",
			RootFS:          "./repo/rootfs.ext4",
			User:            "runner",
			WorkRoot:        "/workspace/firecracker",
			CPUs:            &cpus,
			MemoryMiB:       &memoryMiB,
			DiskMiB:         &diskMiB,
			Network:         "repo-cni-mode",
			CNINetwork:      "repo-firecracker",
			CNIConfDir:      "./repo/cni/conf.d",
			CNIBinDir:       "./repo/cni/bin",
			LaunchTimeout:   "9m",
			DeleteOnRelease: &deleteOnRelease,
		},
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "firecracker" {
		t.Fatalf("provider=%q want firecracker", cfg.Provider)
	}
	if cfg.Firecracker.Binary != "/trusted/firecracker" ||
		cfg.Firecracker.Jailer != "/trusted/jailer" ||
		cfg.Firecracker.Kernel != "/trusted/vmlinux" ||
		cfg.Firecracker.RootFS != "/trusted/rootfs.ext4" ||
		cfg.Firecracker.Network != "cni" ||
		cfg.Firecracker.CNINetwork != "trusted-firecracker" ||
		cfg.Firecracker.CNIConfDir != "/trusted/cni/conf.d" ||
		cfg.Firecracker.CNIBinDir != "/trusted/cni/bin" {
		t.Fatalf("untrusted firecracker host execution path override applied: %#v", cfg.Firecracker)
	}
	if cfg.Firecracker.User != "runner" ||
		cfg.Firecracker.WorkRoot != "/workspace/firecracker" ||
		cfg.Firecracker.CPUs != cpus ||
		cfg.Firecracker.MemoryMiB != memoryMiB ||
		cfg.Firecracker.DiskMiB != diskMiB ||
		cfg.Firecracker.LaunchTimeout != 9*time.Minute ||
		cfg.Firecracker.DeleteOnRelease {
		t.Fatalf("safe untrusted firecracker settings not applied: %#v", cfg.Firecracker)
	}
	if !DeleteOnReleaseExplicit(cfg, "firecracker") {
		t.Fatal("untrusted firecracker deleteOnRelease should mark explicit state")
	}
}

func TestLoadConfigFirecrackerPreservesExplicitTopLevelSSHUserAndWorkRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	body := "provider: firecracker\nssh:\n  user: alice\nworkRoot: /tmp/custom\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestLoadConfigFirecrackerSpecificUserAndWorkRootOverrideTopLevel(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	body := "provider: firecracker\nssh:\n  user: alice\nworkRoot: /tmp/custom\nfirecracker:\n  user: runner\n  workRoot: /workspace/firecracker\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "runner" {
		t.Fatalf("SSHUser=%q want runner", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/workspace/firecracker" {
		t.Fatalf("WorkRoot=%q want /workspace/firecracker", cfg.WorkRoot)
	}
}

func TestTartConfigYAMLExplicitZeroPreserved(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	file := fileConfig{}
	zero := 0
	negative := -1
	file.Tart = &fileTartConfig{
		CPUs:   &zero,
		Memory: &zero,
		Disk:   &negative,
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Tart.CPUs != 0 {
		t.Fatalf("Tart.CPUs=%d, want 0 (explicit YAML zero must be preserved)", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 0 {
		t.Fatalf("Tart.Memory=%d, want 0 (explicit YAML zero must be preserved)", cfg.Tart.Memory)
	}
	if cfg.Tart.Disk != -1 {
		t.Fatalf("Tart.Disk=%d, want -1 (explicit YAML negative must be preserved)", cfg.Tart.Disk)
	}
	if !IsTartCPUsExplicit(&cfg) {
		t.Fatal("tartCPUsExplicit must be true after YAML sets cpus")
	}
	if !IsTartMemoryExplicit(&cfg) {
		t.Fatal("tartMemoryExplicit must be true after YAML sets memory")
	}
}

func TestExternalFixedLeaseIDCapabilityConfigAndEnv(t *testing.T) {
	var file fileConfig
	if err := yaml.Unmarshal([]byte("external:\n  capabilities:\n    idempotentLeaseId: true\n"), &file); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if !cfg.External.Capabilities.IdempotentLeaseID {
		t.Fatal("external fixed lease ID capability was not loaded from YAML")
	}
	cfg.External.Capabilities.IdempotentLeaseID = false
	t.Setenv("CRABBOX_EXTERNAL_IDEMPOTENT_LEASE_ID", "true")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.External.Capabilities.IdempotentLeaseID {
		t.Fatal("external fixed lease ID capability was not loaded from the environment")
	}
}

func TestExternalDesktopCredentialsEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME", "screen-user")
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV", "EXTERNAL_TEST_DESKTOP_PASSWORD")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.External.Connection.Desktop.Username != "screen-user" {
		t.Fatalf("desktop username=%q", cfg.External.Connection.Desktop.Username)
	}
	if cfg.External.Connection.Desktop.PasswordEnv != "EXTERNAL_TEST_DESKTOP_PASSWORD" {
		t.Fatalf("desktop password env=%q", cfg.External.Connection.Desktop.PasswordEnv)
	}
}

func TestExternalDesktopReservedEnvironmentCannotSelfRedirect(t *testing.T) {
	const secret = "unique-operator-screen-sharing-secret"
	for _, reserved := range []string{
		"CRABBOX_EXTERNAL_DESKTOP_USERNAME",
		"CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV",
		"GH_TOKEN",
		"GITHUB_TOKEN",
		"LD_PRELOAD",
		"LD_AUDIT",
		"DYLD_INSERT_LIBRARIES",
		"dyld_library_path",
		"MallocDebugReport",
		"malloc_check_",
		"MALLOC_STACK_LOGGING",
		"GODEBUG",
		"GOGC",
		"GOMEMLIMIT",
		"GOMAXPROCS",
		"GOTRACEBACK",
		"USERNAME",
		"USERDOMAIN",
	} {
		t.Run(reserved, func(t *testing.T) {
			cfg := Config{Provider: "external", TargetOS: targetMacOS}
			cfg.External.Connection.Desktop.PasswordEnv = reserved
			t.Setenv(reserved, secret)
			ApplyExternalDesktopEnvironmentOverrides(&cfg)
			if cfg.External.Connection.Desktop.Username == secret || cfg.External.Connection.Desktop.PasswordEnv == secret {
				t.Fatalf("reserved environment copied secret into config: %#v", cfg.External.Connection.Desktop)
			}
			err := ValidateExternalDesktopPasswordEnvironmentName(cfg.External.Connection.Desktop.PasswordEnv)
			if err == nil || !strings.Contains(err.Error(), "reserved") || strings.Contains(err.Error(), secret) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestExternalDesktopReservedProviderEnvironmentCannotBypassValidation(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	// The password value doubles as provider selection before provider-specific
	// validation runs. Global validation must reject the name first.
	t.Setenv("CRABBOX_PROVIDER", "aws")
	if err := os.WriteFile(configPath, []byte(`
provider: external
external:
  command: provider-command
  connection:
    desktop:
      passwordEnv: CRABBOX_PROVIDER
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "CRABBOX_PROVIDER") || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("error=%v, want reserved CRABBOX_PROVIDER rejection before dispatch", err)
	}
}

func TestLegacyExternalDesktopPasswordEnvironmentRemainsSupported(t *testing.T) {
	for _, name := range []string{legacyExternalDesktopPasswordEnvironment, strings.ToLower(legacyExternalDesktopPasswordEnvironment)} {
		if err := ValidateExternalDesktopPasswordEnvironmentName(name); err != nil {
			t.Fatalf("legacy environment %q rejected: %v", name, err)
		}
	}
}

func TestTartConfigYAMLMissingFieldsNotOverwritten(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 16384
	file := fileConfig{}
	file.Tart = &fileTartConfig{
		Image: "ghcr.io/test:latest",
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Tart.CPUs != 8 {
		t.Fatalf("Tart.CPUs=%d, want 8 (missing YAML field must not overwrite)", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 16384 {
		t.Fatalf("Tart.Memory=%d, want 16384 (missing YAML field must not overwrite)", cfg.Tart.Memory)
	}
}

func TestOpenComputerConfigYAMLExplicitZeroPreserved(t *testing.T) {
	cfg := baseConfig()
	cfg.OpenComputer.CPU = 8
	cfg.OpenComputer.MemoryMB = 16384
	cfg.OpenComputer.TimeoutSecs = 600
	cfg.OpenComputer.ExecTimeoutSecs = 7200
	zero := 0
	file := fileConfig{OpenComputer: &fileOpenComputerConfig{
		CPU:             &zero,
		MemoryMB:        &zero,
		TimeoutSecs:     &zero,
		ExecTimeoutSecs: &zero,
	}}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.OpenComputer.CPU != 0 || cfg.OpenComputer.MemoryMB != 0 || cfg.OpenComputer.TimeoutSecs != 0 || cfg.OpenComputer.ExecTimeoutSecs != 0 {
		t.Fatalf("explicit zero values not preserved: %#v", cfg.OpenComputer)
	}
}

func TestOpenComputerConfigYAMLCannotSetAPIURL(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("openComputer:\n  apiUrl: https://attacker.example\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.OpenComputer.APIURL != "" {
		t.Fatalf("repository config set OpenComputer API URL to %q", cfg.OpenComputer.APIURL)
	}
}

func TestCodeSandboxConfigDefaultsFileEnvAndNoPersistentSecretSurface(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.CodeSandbox.Workdir != "/project/workspace" ||
		cfg.CodeSandbox.Privacy != "private" ||
		!cfg.CodeSandbox.AutomaticWakeupHTTP ||
		cfg.CodeSandbox.AutomaticWakeupWebSocket ||
		cfg.CodeSandbox.BridgeCommand != "node" ||
		cfg.CodeSandbox.SDKPackage != "@codesandbox/sdk@2.4.2" ||
		cfg.CodeSandbox.DoctorListLimit != 1 ||
		cfg.CodeSandbox.OperationTimeoutSecs != 30 {
		t.Fatalf("unexpected codesandbox defaults: %#v", cfg.CodeSandbox)
	}
	if _, ok := reflect.TypeOf(CodeSandboxConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("provider config must not persist CodeSandbox API keys")
	}
	if _, ok := reflect.TypeOf(fileCodeSandboxConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("file config must not accept CodeSandbox API keys")
	}
	var file fileConfig
	if err := yaml.Unmarshal([]byte(strings.Join([]string{
		"codeSandbox:",
		"  apiKey: should-not-load",
		"  templateId: tmpl_file",
		"  workdir: /project/workspace/file",
		"  vmTier: micro",
		"  privacy: public-hosts",
		"  hibernationTimeoutSecs: 600",
		"  automaticWakeupHTTP: false",
		"  automaticWakeupWebSocket: true",
		"  bridgeCommand: /opt/node",
		"  sdkPackage: '@codesandbox/sdk@2.4.2'",
		"  doctorListLimit: 2",
		"  operationTimeoutSecs: 45",
	}, "\n")), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.CodeSandbox.TemplateID != "tmpl_file" ||
		cfg.CodeSandbox.Workdir != "/project/workspace/file" ||
		cfg.CodeSandbox.VMTier != "micro" ||
		cfg.CodeSandbox.Privacy != "public-hosts" ||
		cfg.CodeSandbox.HibernationTimeoutSecs != 600 ||
		cfg.CodeSandbox.AutomaticWakeupHTTP ||
		!cfg.CodeSandbox.AutomaticWakeupWebSocket ||
		cfg.CodeSandbox.BridgeCommand != "/opt/node" ||
		cfg.CodeSandbox.SDKPackage != "@codesandbox/sdk@2.4.2" ||
		cfg.CodeSandbox.DoctorListLimit != 2 ||
		cfg.CodeSandbox.OperationTimeoutSecs != 45 {
		t.Fatalf("file codesandbox config not applied: %#v", cfg.CodeSandbox)
	}

	t.Setenv("CRABBOX_CODESANDBOX_API_KEY", "env-primary-secret")
	t.Setenv("CSB_API_KEY", "env-fallback-secret")
	t.Setenv("CRABBOX_CODESANDBOX_TEMPLATE_ID", "tmpl_env")
	t.Setenv("CRABBOX_CODESANDBOX_WORKDIR", "/project/workspace/env")
	t.Setenv("CRABBOX_CODESANDBOX_VM_TIER", "small")
	t.Setenv("CRABBOX_CODESANDBOX_PRIVACY", "private")
	t.Setenv("CRABBOX_CODESANDBOX_HIBERNATION_TIMEOUT_SECS", "900")
	t.Setenv("CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_HTTP", "true")
	t.Setenv("CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_WEBSOCKET", "false")
	t.Setenv("CRABBOX_CODESANDBOX_BRIDGE_COMMAND", "/usr/local/bin/node")
	t.Setenv("CRABBOX_CODESANDBOX_SDK_PACKAGE", "@codesandbox/sdk@latest")
	t.Setenv("CRABBOX_CODESANDBOX_DOCTOR_LIST_LIMIT", "3")
	t.Setenv("CRABBOX_CODESANDBOX_OPERATION_TIMEOUT_SECS", "60")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CodeSandbox.TemplateID != "tmpl_env" ||
		cfg.CodeSandbox.Workdir != "/project/workspace/env" ||
		cfg.CodeSandbox.VMTier != "small" ||
		cfg.CodeSandbox.Privacy != "private" ||
		cfg.CodeSandbox.HibernationTimeoutSecs != 900 ||
		!cfg.CodeSandbox.AutomaticWakeupHTTP ||
		cfg.CodeSandbox.AutomaticWakeupWebSocket ||
		cfg.CodeSandbox.BridgeCommand != "/usr/local/bin/node" ||
		cfg.CodeSandbox.SDKPackage != "@codesandbox/sdk@latest" ||
		cfg.CodeSandbox.DoctorListLimit != 3 ||
		cfg.CodeSandbox.OperationTimeoutSecs != 60 {
		t.Fatalf("env codesandbox config not applied: %#v", cfg.CodeSandbox)
	}
	if value := reflect.ValueOf(cfg.CodeSandbox); strings.Contains(fmt.Sprint(value.Interface()), "env-primary-secret") || strings.Contains(fmt.Sprint(value.Interface()), "env-fallback-secret") {
		t.Fatalf("CodeSandbox env API key leaked into config: %#v", cfg.CodeSandbox)
	}
}

func TestCodeSandboxRepoConfigCannotReplaceBridgeExecutable(t *testing.T) {
	cfg := baseConfig()
	cfg.CodeSandbox.BridgeCommand = "trusted-node"
	cfg.CodeSandbox.SDKPackage = "@codesandbox/sdk"
	var file fileConfig
	if err := yaml.Unmarshal([]byte(strings.Join([]string{
		"codeSandbox:",
		"  workdir: /project/workspace/repo",
		"  bridgeCommand: ./steal-token",
		"  sdkPackage: ./fake-sdk",
	}, "\n")), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.CodeSandbox.BridgeCommand != "trusted-node" || cfg.CodeSandbox.SDKPackage != "@codesandbox/sdk" {
		t.Fatalf("repository config replaced trusted bridge settings: %#v", cfg.CodeSandbox)
	}
	if cfg.CodeSandbox.Workdir != "/project/workspace/repo" {
		t.Fatalf("safe repository workdir setting not applied: %#v", cfg.CodeSandbox)
	}
}

func TestCodeSandboxConfigRejectsNegativeYAMLValues(t *testing.T) {
	tests := []string{
		"codeSandbox:\n  hibernationTimeoutSecs: -1\n",
		"codeSandbox:\n  doctorListLimit: -1\n",
		"codeSandbox:\n  operationTimeoutSecs: -1\n",
	}
	for _, yamlText := range tests {
		t.Run(yamlText, func(t *testing.T) {
			cfg := baseConfig()
			var file fileConfig
			if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfig(&cfg, file); err == nil {
				t.Fatalf("negative CodeSandbox config accepted for %q", yamlText)
			}
		})
	}
}

func TestSuperserveConfigDefaultsAndNoPersistentSecretSurface(t *testing.T) {
	cfg := baseConfig()
	if cfg.Superserve.BaseURL != "https://api.superserve.ai" || cfg.Superserve.Template != "superserve/base" || cfg.Superserve.Workdir != "/workspace/crabbox" || cfg.Superserve.ExecTimeoutSecs != 600 {
		t.Fatalf("unexpected superserve defaults: %#v", cfg.Superserve)
	}
	fieldName := "API" + "Key"
	if _, ok := reflect.TypeOf(SuperserveConfig{}).FieldByName(fieldName); ok {
		t.Fatal("provider config must not persist API keys")
	}
	if _, ok := reflect.TypeOf(fileSuperserveConfig{}).FieldByName(fieldName); ok {
		t.Fatal("file config must not accept API keys")
	}
}

func TestCrownestConfigDefaultsAndNoPersistentSecretSurface(t *testing.T) {
	cfg := baseConfig()
	if cfg.Crownest.APIURL != "https://api.crownest.dev" || cfg.Crownest.Template != "python-node" || cfg.Crownest.TimeoutSecs != 600 {
		t.Fatalf("unexpected crownest defaults: %#v", cfg.Crownest)
	}
	fieldName := "API" + "Key"
	if _, ok := reflect.TypeOf(CrownestConfig{}).FieldByName(fieldName); ok {
		t.Fatal("provider config must not persist API keys")
	}
	if _, ok := reflect.TypeOf(fileCrownestConfig{}).FieldByName(fieldName); ok {
		t.Fatal("file config must not accept API keys")
	}
}

func TestCrownestUnprefixedEnvAliases(t *testing.T) {
	cfg := baseConfig()
	t.Setenv("CROWNEST_API_URL", "https://crownest.example.test")
	t.Setenv("CROWNEST_PROJECT_ID", "proj_alias")
	t.Setenv("CROWNEST_TEMPLATE", "go-node")
	t.Setenv("CROWNEST_TIMEOUT_SECS", "321")
	t.Setenv("CROWNEST_FORGET_MISSING", "true")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if cfg.Crownest.APIURL != "https://crownest.example.test" || cfg.Crownest.ProjectID != "proj_alias" || cfg.Crownest.Template != "go-node" || cfg.Crownest.TimeoutSecs != 321 || !cfg.Crownest.ForgetMissing {
		t.Fatalf("crownest env aliases not applied: %#v", cfg.Crownest)
	}
}

func TestCrownestPrefixedEnvOverridesUnprefixedAliases(t *testing.T) {
	cfg := baseConfig()
	t.Setenv("CROWNEST_API_URL", "https://alias.example.test")
	t.Setenv("CRABBOX_CROWNEST_API_URL", "https://prefixed.example.test")
	t.Setenv("CROWNEST_PROJECT_ID", "proj_alias")
	t.Setenv("CRABBOX_CROWNEST_PROJECT_ID", "proj_prefixed")
	t.Setenv("CROWNEST_TEMPLATE", "alias-template")
	t.Setenv("CRABBOX_CROWNEST_TEMPLATE", "prefixed-template")
	t.Setenv("CROWNEST_TIMEOUT_SECS", "321")
	t.Setenv("CRABBOX_CROWNEST_TIMEOUT_SECS", "654")
	t.Setenv("CROWNEST_FORGET_MISSING", "true")
	t.Setenv("CRABBOX_CROWNEST_FORGET_MISSING", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if cfg.Crownest.APIURL != "https://prefixed.example.test" || cfg.Crownest.ProjectID != "proj_prefixed" || cfg.Crownest.Template != "prefixed-template" || cfg.Crownest.TimeoutSecs != 654 || cfg.Crownest.ForgetMissing {
		t.Fatalf("crownest env precedence not applied: %#v", cfg.Crownest)
	}
}

func TestCrownestTimeoutEnvValidation(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "CRABBOX_CROWNEST_TIMEOUT_SECS", value: "later", want: "must be an integer"},
		{name: "CROWNEST_TIMEOUT_SECS", value: "-1", want: "must be non-negative"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := baseConfig()
			t.Setenv("CRABBOX_CROWNEST_TIMEOUT_SECS", "")
			t.Setenv("CROWNEST_TIMEOUT_SECS", "")
			t.Setenv(testCase.name, testCase.value)
			err := applyEnv(&cfg)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("applyEnv error=%v, want %q", err, testCase.want)
			}
		})
	}
}

func TestCrownestRepoConfigKeepsTrustedAPIURL(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("crownest:\n  apiUrl: https://attacker.example\n  projectId: proj_repo\n  template: repo-template\n  timeoutSecs: 45\n  forgetMissing: true\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Crownest.APIURL != "https://api.crownest.dev" {
		t.Fatalf("repository config set Crownest API URL to %q", cfg.Crownest.APIURL)
	}
	if cfg.Crownest.ProjectID != "proj_repo" || cfg.Crownest.Template != "repo-template" || cfg.Crownest.TimeoutSecs != 45 || !cfg.Crownest.ForgetMissing {
		t.Fatalf("repository config not applied: %#v", cfg.Crownest)
	}
}

func TestCrownestTrustedConfigCanSetAPIURL(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("crownest:\n  apiUrl: https://trusted.example\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Crownest.APIURL != "https://trusted.example" {
		t.Fatalf("trusted config API URL=%q", cfg.Crownest.APIURL)
	}
}

func TestCrownestConfigRejectsNegativeTimeout(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("crownest:\n  timeoutSecs: -1\n"), &file); err != nil {
		t.Fatal(err)
	}
	err := applyFileConfigWithTrust(&cfg, file, true)
	if err == nil || !strings.Contains(err.Error(), "crownest timeoutSecs must be non-negative") {
		t.Fatalf("config error=%v", err)
	}
}

func TestSuperserveRepoConfigCannotSetBaseURL(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("superserve:\n  baseUrl: https://attacker.example\n  template: superserve/repo\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Superserve.BaseURL != "https://api.superserve.ai" {
		t.Fatalf("repository config set Superserve base URL to %q", cfg.Superserve.BaseURL)
	}
	if cfg.Superserve.Template != "superserve/repo" {
		t.Fatalf("repository config did not set non-secret template: %#v", cfg.Superserve)
	}
}

func TestSuperserveTrustedConfigCanSetBaseURL(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("superserve:\n  baseUrl: https://trusted.example\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Superserve.BaseURL != "https://trusted.example" {
		t.Fatalf("trusted config base URL=%q", cfg.Superserve.BaseURL)
	}
}

func TestVercelSandboxConfigDefaultsAndNoPersistentSecretSurface(t *testing.T) {
	cfg := baseConfig()
	if cfg.VercelSandbox.Runtime != "node24" || cfg.VercelSandbox.Workdir != "/vercel/sandbox/crabbox" || cfg.VercelSandbox.ExecTimeoutSecs != 600 || cfg.VercelSandbox.NetworkPolicy != "default" {
		t.Fatalf("unexpected vercel-sandbox defaults: %#v", cfg.VercelSandbox)
	}
	for _, fieldName := range []string{"Token", "AuthToken", "APIToken", "APIKey", "Secret"} {
		if _, ok := reflect.TypeOf(VercelSandboxConfig{}).FieldByName(fieldName); ok {
			t.Fatalf("provider config must not persist %s", fieldName)
		}
		if _, ok := reflect.TypeOf(fileVercelSandboxConfig{}).FieldByName(fieldName); ok {
			t.Fatalf("file config must not accept %s", fieldName)
		}
	}
}

func TestVercelSandboxConfigYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	yamlText := strings.Join([]string{
		"vercelSandbox:",
		"  runtime: node22",
		"  workdir: /work/app",
		"  projectId: prj_123",
		"  teamId: team_123",
		"  scope: example-org",
		"  vcpus: 2",
		"  timeoutSecs: 120",
		"  execTimeoutSecs: 60",
		"  persistent: true",
		"  snapshot: snap_123",
		"  snapshotMode: restore",
		"  networkPolicy: restricted",
		"  networkAllow: [api.example.com, 10.0.0.0/8]",
		"  networkDeny: [169.254.169.254/32]",
		"  ports: [3000, 8080-8090]",
		"  forgetMissing: true",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.VercelSandbox.Runtime != "node22" || cfg.VercelSandbox.Workdir != "/work/app" || cfg.VercelSandbox.ProjectID != "prj_123" || cfg.VercelSandbox.TeamID != "team_123" || cfg.VercelSandbox.Scope != "example-org" {
		t.Fatalf("vercel-sandbox YAML scalar config not applied: %#v", cfg.VercelSandbox)
	}
	if cfg.VercelSandbox.VCPUs != 2 || cfg.VercelSandbox.TimeoutSecs != 120 || cfg.VercelSandbox.ExecTimeoutSecs != 60 || !cfg.VercelSandbox.Persistent || cfg.VercelSandbox.Snapshot != "snap_123" || cfg.VercelSandbox.SnapshotMode != "restore" || cfg.VercelSandbox.NetworkPolicy != "restricted" || !cfg.VercelSandbox.ForgetMissing {
		t.Fatalf("vercel-sandbox YAML config not applied: %#v", cfg.VercelSandbox)
	}
	if !reflect.DeepEqual(cfg.VercelSandbox.NetworkAllow, []string{"api.example.com", "10.0.0.0/8"}) || !reflect.DeepEqual(cfg.VercelSandbox.NetworkDeny, []string{"169.254.169.254/32"}) || !reflect.DeepEqual(cfg.VercelSandbox.Ports, []string{"3000", "8080-8090"}) {
		t.Fatalf("vercel-sandbox YAML lists not applied: %#v", cfg.VercelSandbox)
	}

	t.Setenv("CRABBOX_VERCEL_SANDBOX_RUNTIME", "node24")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_WORKDIR", "/work/env")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_PROJECT_ID", "prj_env")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_TEAM_ID", "team_env")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_SCOPE", "env-org")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_VCPUS", "4")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_TIMEOUT_SECS", "240")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_EXEC_TIMEOUT_SECS", "90")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_PERSISTENT", "false")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_SNAPSHOT", "snap_env")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_SNAPSHOT_MODE", "checkpoint")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_NETWORK_POLICY", "public")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_NETWORK_ALLOW", "example.com,192.168.0.0/16")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_NETWORK_DENY", "10.0.0.5")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_PORTS", "443,9000-9001")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_FORGET_MISSING", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.VercelSandbox.Runtime != "node24" || cfg.VercelSandbox.Workdir != "/work/env" || cfg.VercelSandbox.ProjectID != "prj_env" || cfg.VercelSandbox.TeamID != "team_env" || cfg.VercelSandbox.Scope != "env-org" {
		t.Fatalf("vercel-sandbox env scalar config not applied: %#v", cfg.VercelSandbox)
	}
	if cfg.VercelSandbox.VCPUs != 4 || cfg.VercelSandbox.TimeoutSecs != 240 || cfg.VercelSandbox.ExecTimeoutSecs != 90 || cfg.VercelSandbox.Persistent || cfg.VercelSandbox.Snapshot != "snap_env" || cfg.VercelSandbox.SnapshotMode != "checkpoint" || cfg.VercelSandbox.NetworkPolicy != "public" || cfg.VercelSandbox.ForgetMissing {
		t.Fatalf("vercel-sandbox env config not applied: %#v", cfg.VercelSandbox)
	}
	if !reflect.DeepEqual(cfg.VercelSandbox.NetworkAllow, []string{"example.com", "192.168.0.0/16"}) || !reflect.DeepEqual(cfg.VercelSandbox.NetworkDeny, []string{"10.0.0.5"}) || !reflect.DeepEqual(cfg.VercelSandbox.Ports, []string{"443", "9000-9001"}) {
		t.Fatalf("vercel-sandbox env lists not applied: %#v", cfg.VercelSandbox)
	}
}

func TestAzureDynamicSessionsFilePositiveTimeoutAndSources(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace"} {
			for _, timeout := range []any{nil, 0, -1, 17} {
				cfg := baseConfig()
				cfg.AzureDynamicSessions = AzureDynamicSessionsConfig{Endpoint: "https://example.invalid/pool", Pool: "legacy", APIVersion: "version", Workdir: "/workspace/prior", TimeoutSecs: 12}
				cfg.credentialProvenance.azSessionsEndpoint = credentialSourceFlag
				want := cfg.AzureDynamicSessions
				wantSource := credentialSourceFlag
				fields := map[string]any{}
				for _, f := range []struct {
					key string
					v   *string
				}{{"endpoint", &want.Endpoint}, {"pool", &want.Pool}, {"apiVersion", &want.APIVersion}, {"workdir", &want.Workdir}} {
					if mode == "omitted" {
						continue
					}
					var value any = *f.v
					if mode == "null" {
						value = nil
					}
					if mode == "empty" {
						value = ""
					}
					if mode == "whitespace" {
						value = "  "
						*f.v = "  "
					}
					fields[f.key] = value
				}
				if mode == "equal" || mode == "whitespace" {
					wantSource = credentialSourceForFile(trusted)
				}
				fields["timeoutSecs"] = timeout
				if v, ok := timeout.(int); ok && v > 0 {
					want.TimeoutSecs = v
				}
				data, err := yaml.Marshal(map[string]any{"azureDynamicSessions": fields})
				if err != nil {
					t.Fatal(err)
				}
				var file fileConfig
				if err := yaml.Unmarshal(data, &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if cfg.AzureDynamicSessions != want || cfg.credentialProvenance.azSessionsEndpoint != wantSource {
					t.Fatalf("file mode=%s timeout=%v trusted=%t", mode, timeout, trusted)
				}
			}
		}
	}
}

func TestAzureDynamicSessionsEnvironmentRawTimeoutAndSources(t *testing.T) {
	for _, mode := range []string{"empty", "equal", "whitespace", "changed"} {
		for _, tc := range []struct {
			raw  string
			want int
		}{{"", 12}, {"invalid", 12}, {" 17 ", 12}, {"9999999999999999999999999", 12}, {"0", 0}, {"-1", -1}, {"17", 17}} {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.AzureDynamicSessions = AzureDynamicSessionsConfig{Endpoint: "https://example.invalid/pool", Pool: "legacy", APIVersion: "version", Workdir: "/workspace/prior", TimeoutSecs: 12}
			cfg.credentialProvenance.azSessionsEndpoint = credentialSourceFlag
			want := cfg.AzureDynamicSessions
			source := credentialSourceEnvironment
			if mode == "empty" {
				source = credentialSourceFlag
			}
			for _, f := range []struct {
				suffix string
				v      *string
			}{{"ENDPOINT", &want.Endpoint}, {"POOL", &want.Pool}, {"API_VERSION", &want.APIVersion}, {"WORKDIR", &want.Workdir}} {
				raw := *f.v
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
					*f.v = raw
				}
				if mode == "changed" {
					raw += "-new"
					*f.v = raw
				}
				t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_"+f.suffix, raw)
			}
			t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS", tc.raw)
			want.TimeoutSecs = tc.want
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.AzureDynamicSessions != want || cfg.credentialProvenance.azSessionsEndpoint != source {
				t.Fatalf("env mode=%s raw=%q", mode, tc.raw)
			}
		}
	}
}

func TestBlaxelFilePresenceTrustAndPartialErrors(t *testing.T) {
	if _, ok := reflect.TypeOf(fileBlaxelConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("API key YAML source introduced")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "zero", "whitespace"} {
			cfg := baseConfig()
			cfg.Blaxel = BlaxelConfig{APIKey: "inert", APIURL: "https://example.invalid/prior", Workspace: "prior", Region: "prior", Image: "prior", MemoryMB: 10, TTL: "prior", IdleTTL: "prior", Workdir: "/workspace/prior", ExecTimeoutSecs: 20, ForgetMissing: true}
			want := cfg.Blaxel
			fields := map[string]any{"apiKey": "ignored-inert"}
			for _, f := range []struct {
				key                      string
				v                        *string
				ignoreEmpty, trustedOnly bool
			}{{"apiUrl", &want.APIURL, true, true}, {"workspace", &want.Workspace, true, true}, {"region", &want.Region, true, false}, {"image", &want.Image, false, false}, {"ttl", &want.TTL, true, false}, {"idleTTL", &want.IdleTTL, true, false}, {"workdir", &want.Workdir, false, false}} {
				if mode == "omitted" {
					continue
				}
				var value any = nil
				if mode == "zero" {
					value = ""
					if !f.ignoreEmpty && (!f.trustedOnly || trusted) {
						*f.v = ""
					}
				}
				if mode == "whitespace" {
					value = "  "
					if !f.trustedOnly || trusted {
						*f.v = "  "
					}
				}
				fields[f.key] = value
			}
			if mode != "omitted" {
				fields["memoryMB"], fields["execTimeoutSecs"], fields["forgetMissing"] = nil, nil, nil
				if mode != "null" {
					fields["memoryMB"], fields["execTimeoutSecs"], fields["forgetMissing"] = 0, 0, false
					want.MemoryMB, want.ExecTimeoutSecs, want.ForgetMissing = 0, 0, false
				}
			}
			data, err := yaml.Marshal(map[string]any{"blaxel": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Blaxel != want {
				t.Fatalf("file trust/presence mode=%s trusted=%t", mode, trusted)
			}
		}
	}
	for _, memoryFails := range []bool{false, true} {
		cfg := baseConfig()
		cfg.Blaxel.MemoryMB, cfg.Blaxel.ExecTimeoutSecs = 10, 20
		cfg.Blaxel.ForgetMissing = true
		before := cfg.Blaxel
		memory, timeout := 7, -1
		if memoryFails {
			memory = -1
		}
		var file fileConfig
		data := fmt.Sprintf("blaxel:\n  apiUrl: https://example.invalid/after\n  workspace: after\n  region: after\n  image: after\n  memoryMB: %d\n  ttl: after\n  idleTTL: after\n  workdir: /workspace/after\n  execTimeoutSecs: %d\n  forgetMissing: false\n", memory, timeout)
		if err := yaml.Unmarshal([]byte(data), &file); err != nil {
			t.Fatal(err)
		}
		err := applyFileConfigWithTrust(&cfg, file, true)
		wantError := "blaxel execTimeoutSecs must be non-negative"
		if memoryFails {
			wantError = "blaxel memoryMB must be non-negative"
		}
		if err == nil || err.Error() != wantError {
			t.Fatalf("file error=%v", err)
		}
		want := before
		want.APIURL, want.Workspace, want.Region, want.Image = "https://example.invalid/after", "after", "after", "after"
		if !memoryFails {
			want.MemoryMB, want.TTL, want.IdleTTL, want.Workdir = 7, "after", "after", "/workspace/after"
		}
		if cfg.Blaxel != want {
			t.Fatal("file partial mutation order changed")
		}
	}
}

func TestBlaxelMemoryEnvironmentParsingAndOrder(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 10}, {"invalid", 10}, {" 17 ", 10}, {" ", 10}, {"999999999999999999999999999999", 10}, {"-2", -2}, {"0", 0}, {"17", 17}} {
		for _, timeout := range []string{"19", "0", "-1", "invalid"} {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Blaxel.MemoryMB, cfg.Blaxel.ExecTimeoutSecs = 10, 20
			cfg.Blaxel.ForgetMissing = true
			t.Setenv("CRABBOX_BLAXEL_MEMORY_MB", tc.raw)
			t.Setenv("CRABBOX_BLAXEL_TTL", "after")
			t.Setenv("CRABBOX_BLAXEL_IDLE_TTL", "after")
			t.Setenv("CRABBOX_BLAXEL_WORKDIR", "/workspace/after")
			t.Setenv("CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS", timeout)
			t.Setenv("CRABBOX_BLAXEL_FORGET_MISSING", "false")
			err := applyEnv(&cfg)
			wantTimeout := 19
			wantForget := false
			if timeout == "0" {
				wantTimeout = 0
			}
			if timeout == "-1" || timeout == "invalid" {
				wantTimeout = 0
				wantForget = true
				message := "CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS must be non-negative"
				if timeout == "invalid" {
					message = "CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS must be an integer"
				}
				if err == nil || err.Error() != message {
					t.Fatalf("strict timeout error=%v", err)
				}
			} else if err != nil {
				t.Fatalf("tolerant memory=%q error=%v", tc.raw, err)
			}
			if cfg.Blaxel.MemoryMB != tc.want || cfg.Blaxel.TTL != "after" || cfg.Blaxel.IdleTTL != "after" || cfg.Blaxel.Workdir != "/workspace/after" || cfg.Blaxel.ExecTimeoutSecs != wantTimeout || cfg.Blaxel.ForgetMissing != wantForget {
				t.Fatalf("env order memory=%q timeout=%q", tc.raw, timeout)
			}
		}
	}
}

func TestBlaxelRawEnvironmentAliases(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "whitespace"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Blaxel.APIKey, cfg.Blaxel.Workspace, cfg.Blaxel.Region = "prior", "prior", "prior"
		want := "primary"
		primary, alias := "primary", "alias"
		if mode == "alias" {
			primary = ""
			want = "alias"
		}
		if mode == "empty" {
			primary, alias = "", ""
			want = "prior"
		}
		if mode == "whitespace" {
			primary = "  "
			want = "  "
		}
		for _, names := range [][2]string{{"CRABBOX_BLAXEL_API_KEY", "BL_API_KEY"}, {"CRABBOX_BLAXEL_WORKSPACE", "BL_WORKSPACE"}, {"CRABBOX_BLAXEL_REGION", "BL_REGION"}} {
			t.Setenv(names[0], primary)
			t.Setenv(names[1], alias)
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Blaxel.APIKey != want || cfg.Blaxel.Workspace != want || cfg.Blaxel.Region != want {
			t.Fatalf("alias raw=%s", mode)
		}
	}
}

func TestBlaxelConfigYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	defaultAPIURL := cfg.Blaxel.APIURL
	var file fileConfig
	yamlText := strings.Join([]string{
		"blaxel:",
		"  apiUrl: https://repo-ignored.example.test",
		"  workspace: workspace-file",
		"  region: us-pdx-1",
		"  image: ubuntu:24.04",
		"  memoryMB: 4096",
		"  ttl: 30m",
		"  idleTTL: 5m",
		"  workdir: /workspace/file",
		"  execTimeoutSecs: 120",
		"  forgetMissing: true",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Blaxel.APIURL != defaultAPIURL {
		t.Fatalf("untrusted repo config redirected Blaxel API URL to %q", cfg.Blaxel.APIURL)
	}
	if cfg.Blaxel.APIKey != "" ||
		cfg.Blaxel.Workspace != "" ||
		cfg.Blaxel.Region != "us-pdx-1" ||
		cfg.Blaxel.Image != "ubuntu:24.04" ||
		cfg.Blaxel.MemoryMB != 4096 ||
		cfg.Blaxel.TTL != "30m" ||
		cfg.Blaxel.IdleTTL != "5m" ||
		cfg.Blaxel.Workdir != "/workspace/file" ||
		cfg.Blaxel.ExecTimeoutSecs != 120 ||
		!cfg.Blaxel.ForgetMissing {
		t.Fatalf("file cfg.Blaxel=%#v", cfg.Blaxel)
	}

	t.Setenv("CRABBOX_BLAXEL_API_KEY", "blaxel-env-key")
	t.Setenv("CRABBOX_BLAXEL_API_URL", "https://api-env.example.test")
	t.Setenv("CRABBOX_BLAXEL_WORKSPACE", "workspace-env")
	t.Setenv("CRABBOX_BLAXEL_REGION", "us-was-1")
	t.Setenv("CRABBOX_BLAXEL_IMAGE", "python:3.12")
	t.Setenv("CRABBOX_BLAXEL_MEMORY_MB", "8192")
	t.Setenv("CRABBOX_BLAXEL_TTL", "1h")
	t.Setenv("CRABBOX_BLAXEL_IDLE_TTL", "10m")
	t.Setenv("CRABBOX_BLAXEL_WORKDIR", "/workspace/env")
	t.Setenv("CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS", "240")
	t.Setenv("CRABBOX_BLAXEL_FORGET_MISSING", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	want := BlaxelConfig{
		APIKey:          "blaxel-env-key",
		APIURL:          "https://api-env.example.test",
		Workspace:       "workspace-env",
		Region:          "us-was-1",
		Image:           "python:3.12",
		MemoryMB:        8192,
		TTL:             "1h",
		IdleTTL:         "10m",
		Workdir:         "/workspace/env",
		ExecTimeoutSecs: 240,
		ForgetMissing:   false,
	}
	if cfg.Blaxel != want {
		t.Fatalf("env cfg.Blaxel=%#v, want %#v", cfg.Blaxel, want)
	}
}

func TestNomadConfigYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	yamlText := strings.Join([]string{
		"nomad:",
		"  address: https://nomad-file.example.test:4646",
		"  region: file-region",
		"  namespace: file-namespace",
		"  tokenEnv: FILE_NOMAD_TOKEN",
		"  caCert: ~/nomad/ca.pem",
		"  caPath: ~/nomad/certs",
		"  clientCert: ~/nomad/client.pem",
		"  clientKey: ~/nomad/client.key",
		"  tlsServerName: nomad-file.example.test",
		"  skipVerify: true",
		"  task: file-task",
		"  driver: raw_exec",
		"  image: file-image:latest",
		"  workdir: /workspace/file",
		"  jobspecTemplate: ~/nomad/job.hcl",
		"  nodePool: file-pool",
		"  datacenters: [dc1, dc2]",
		"  cpu: 500",
		"  memoryMB: 1024",
		"  diskMB: 2048",
		"  allocReadyTimeout: 2m",
		"  evalTimeout: 3m",
		"  execTimeoutSecs: 45",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if cfg.Nomad.Address != "https://nomad-file.example.test:4646" ||
		cfg.Nomad.Region != "file-region" ||
		cfg.Nomad.Namespace != "file-namespace" ||
		cfg.Nomad.TokenEnv != "FILE_NOMAD_TOKEN" ||
		cfg.Nomad.CACert != filepath.Join(home, "nomad/ca.pem") ||
		cfg.Nomad.CAPath != filepath.Join(home, "nomad/certs") ||
		cfg.Nomad.ClientCert != filepath.Join(home, "nomad/client.pem") ||
		cfg.Nomad.ClientKey != filepath.Join(home, "nomad/client.key") ||
		cfg.Nomad.TLSServerName != "nomad-file.example.test" ||
		!cfg.Nomad.SkipVerify ||
		cfg.Nomad.Task != "file-task" ||
		cfg.Nomad.Driver != "raw_exec" ||
		cfg.Nomad.Image != "file-image:latest" ||
		cfg.Nomad.Workdir != "/workspace/file" ||
		cfg.Nomad.JobSpecTemplate != filepath.Join(home, "nomad/job.hcl") ||
		cfg.Nomad.NodePool != "file-pool" ||
		!reflect.DeepEqual(cfg.Nomad.Datacenters, []string{"dc1", "dc2"}) ||
		cfg.Nomad.CPU != 500 ||
		cfg.Nomad.MemoryMB != 1024 ||
		cfg.Nomad.DiskMB != 2048 ||
		cfg.Nomad.AllocReadyTimeout != 2*time.Minute ||
		cfg.Nomad.EvalTimeout != 3*time.Minute ||
		cfg.Nomad.ExecTimeoutSecs != 45 {
		t.Fatalf("file cfg.Nomad=%#v", cfg.Nomad)
	}

	t.Setenv("NOMAD_ADDR", "https://nomad-vendor.example.test:4646")
	t.Setenv("NOMAD_REGION", "vendor-region")
	t.Setenv("NOMAD_NAMESPACE", "vendor-namespace")
	t.Setenv("NOMAD_CACERT", "/vendor/ca.pem")
	t.Setenv("NOMAD_CAPATH", "/vendor/certs")
	t.Setenv("NOMAD_CLIENT_CERT", "/vendor/client.pem")
	t.Setenv("NOMAD_CLIENT_KEY", "/vendor/client.key")
	t.Setenv("NOMAD_TLS_SERVER_NAME", "nomad-vendor.example.test")
	t.Setenv("NOMAD_SKIP_VERIFY", "false")
	t.Setenv("CRABBOX_NOMAD_ADDR", "https://nomad-env.example.test:4646")
	t.Setenv("CRABBOX_NOMAD_REGION", "env-region")
	t.Setenv("CRABBOX_NOMAD_NAMESPACE", "env-namespace")
	t.Setenv("CRABBOX_NOMAD_TOKEN_ENV", "ENV_NOMAD_TOKEN")
	t.Setenv("CRABBOX_NOMAD_CA_CERT", "/env/ca.pem")
	t.Setenv("CRABBOX_NOMAD_CA_PATH", "/env/certs")
	t.Setenv("CRABBOX_NOMAD_CLIENT_CERT", "/env/client.pem")
	t.Setenv("CRABBOX_NOMAD_CLIENT_KEY", "/env/client.key")
	t.Setenv("CRABBOX_NOMAD_TLS_SERVER_NAME", "nomad-env.example.test")
	t.Setenv("CRABBOX_NOMAD_SKIP_VERIFY", "true")
	t.Setenv("CRABBOX_NOMAD_TASK", "env-task")
	t.Setenv("CRABBOX_NOMAD_DRIVER", "docker")
	t.Setenv("CRABBOX_NOMAD_IMAGE", "env-image:latest")
	t.Setenv("CRABBOX_NOMAD_WORKDIR", "/workspace/env")
	t.Setenv("CRABBOX_NOMAD_JOBSPEC_TEMPLATE", "/env/job.hcl")
	t.Setenv("CRABBOX_NOMAD_NODE_POOL", "env-pool")
	t.Setenv("CRABBOX_NOMAD_DATACENTERS", "dc3,dc4")
	t.Setenv("CRABBOX_NOMAD_CPU", "750")
	t.Setenv("CRABBOX_NOMAD_MEMORY_MB", "1536")
	t.Setenv("CRABBOX_NOMAD_DISK_MB", "3072")
	t.Setenv("CRABBOX_NOMAD_ALLOC_READY_TIMEOUT", "4m")
	t.Setenv("CRABBOX_NOMAD_EVAL_TIMEOUT", "5m")
	t.Setenv("CRABBOX_NOMAD_EXEC_TIMEOUT_SECS", "60")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Nomad.Address != "https://nomad-env.example.test:4646" ||
		cfg.Nomad.Region != "env-region" ||
		cfg.Nomad.Namespace != "env-namespace" ||
		cfg.Nomad.TokenEnv != "ENV_NOMAD_TOKEN" ||
		cfg.Nomad.CACert != "/env/ca.pem" ||
		cfg.Nomad.CAPath != "/env/certs" ||
		cfg.Nomad.ClientCert != "/env/client.pem" ||
		cfg.Nomad.ClientKey != "/env/client.key" ||
		cfg.Nomad.TLSServerName != "nomad-env.example.test" ||
		!cfg.Nomad.SkipVerify ||
		cfg.Nomad.Task != "env-task" ||
		cfg.Nomad.Driver != "docker" ||
		cfg.Nomad.Image != "env-image:latest" ||
		cfg.Nomad.Workdir != "/workspace/env" ||
		cfg.Nomad.JobSpecTemplate != "/env/job.hcl" ||
		cfg.Nomad.NodePool != "env-pool" ||
		!reflect.DeepEqual(cfg.Nomad.Datacenters, []string{"dc3", "dc4"}) ||
		cfg.Nomad.CPU != 750 ||
		cfg.Nomad.MemoryMB != 1536 ||
		cfg.Nomad.DiskMB != 3072 ||
		cfg.Nomad.AllocReadyTimeout != 4*time.Minute ||
		cfg.Nomad.EvalTimeout != 5*time.Minute ||
		cfg.Nomad.ExecTimeoutSecs != 60 {
		t.Fatalf("env cfg.Nomad=%#v", cfg.Nomad)
	}
}

func TestNomadUntrustedConfigCannotRedirectCredentialedJobs(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Nomad.Address = "https://trusted-nomad.example.test:4646"
	cfg.Nomad.Region = "trusted-region"
	cfg.Nomad.Namespace = "trusted-namespace"
	cfg.Nomad.TokenEnv = "TRUSTED_NOMAD_TOKEN"
	cfg.Nomad.CACert = "/trusted/ca.pem"
	cfg.Nomad.CAPath = "/trusted/certs"
	cfg.Nomad.ClientCert = "/trusted/client.pem"
	cfg.Nomad.ClientKey = "/trusted/client.key"
	cfg.Nomad.TLSServerName = "trusted-nomad.example.test"
	cfg.Nomad.SkipVerify = false
	cfg.Nomad.Task = "trusted-task"
	cfg.Nomad.Driver = "docker"
	cfg.Nomad.Image = "trusted-image:latest"
	cfg.Nomad.Workdir = "/workspace/trusted"
	cfg.Nomad.JobSpecTemplate = "/trusted/job.hcl"
	cfg.Nomad.NodePool = "trusted-pool"
	cfg.Nomad.Datacenters = []string{"trusted-dc"}
	cfg.Nomad.CPU = 1000
	cfg.Nomad.MemoryMB = 2048
	cfg.Nomad.DiskMB = 1024
	cfg.Nomad.AllocReadyTimeout = 5 * time.Minute
	cfg.Nomad.EvalTimeout = 6 * time.Minute
	cfg.Nomad.ExecTimeoutSecs = 600
	var file fileConfig
	yamlText := strings.Join([]string{
		"nomad:",
		"  address: https://attacker.example.test:4646",
		"  region: repo-region",
		"  namespace: repo-namespace",
		"  tokenEnv: ATTACKER_CHOSEN_TOKEN",
		"  caCert: /repo/ca.pem",
		"  caPath: /repo/certs",
		"  clientCert: /repo/client.pem",
		"  clientKey: /repo/client.key",
		"  tlsServerName: attacker.example.test",
		"  skipVerify: true",
		"  task: repo-task",
		"  driver: raw_exec",
		"  image: repo-image:latest",
		"  workdir: /workspace/repo",
		"  jobspecTemplate: /repo/job.hcl",
		"  nodePool: repo-pool",
		"  datacenters: [dc1, dc2]",
		"  cpu: 500",
		"  memoryMB: 1024",
		"  diskMB: 2048",
		"  allocReadyTimeout: 2m",
		"  evalTimeout: 3m",
		"  execTimeoutSecs: 45",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Nomad.Address != "https://trusted-nomad.example.test:4646" ||
		cfg.Nomad.Region != "trusted-region" ||
		cfg.Nomad.Namespace != "trusted-namespace" ||
		cfg.Nomad.TokenEnv != "TRUSTED_NOMAD_TOKEN" ||
		cfg.Nomad.CACert != "/trusted/ca.pem" ||
		cfg.Nomad.CAPath != "/trusted/certs" ||
		cfg.Nomad.ClientCert != "/trusted/client.pem" ||
		cfg.Nomad.ClientKey != "/trusted/client.key" ||
		cfg.Nomad.TLSServerName != "trusted-nomad.example.test" ||
		cfg.Nomad.SkipVerify ||
		cfg.Nomad.Task != "trusted-task" ||
		cfg.Nomad.Driver != "docker" ||
		cfg.Nomad.Image != "trusted-image:latest" ||
		cfg.Nomad.Workdir != "/workspace/trusted" ||
		cfg.Nomad.JobSpecTemplate != "/trusted/job.hcl" ||
		cfg.Nomad.NodePool != "trusted-pool" ||
		!reflect.DeepEqual(cfg.Nomad.Datacenters, []string{"trusted-dc"}) ||
		cfg.Nomad.CPU != 1000 ||
		cfg.Nomad.MemoryMB != 2048 ||
		cfg.Nomad.DiskMB != 1024 ||
		cfg.Nomad.AllocReadyTimeout != 5*time.Minute ||
		cfg.Nomad.EvalTimeout != 6*time.Minute ||
		cfg.Nomad.ExecTimeoutSecs != 600 {
		t.Fatalf("untrusted nomad config changed credentialed job settings: %#v", cfg.Nomad)
	}
}

func TestBlaxelVendorEnvFallbacks(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("BL_API_KEY", "vendor-key")
	t.Setenv("BL_WORKSPACE", "vendor-workspace")
	t.Setenv("BL_REGION", "vendor-region")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Blaxel.APIKey != "vendor-key" || cfg.Blaxel.Workspace != "vendor-workspace" || cfg.Blaxel.Region != "vendor-region" {
		t.Fatalf("cfg.Blaxel=%#v", cfg.Blaxel)
	}
}

func TestBlaxelConfigRejectsInvalidValues(t *testing.T) {
	clearConfigEnv(t)
	for _, tc := range []struct {
		name  string
		yaml  string
		env   map[string]string
		error string
	}{
		{
			name:  "negative memory in YAML",
			yaml:  "blaxel:\n  memoryMB: -1\n",
			error: "blaxel memoryMB must be non-negative",
		},
		{
			name:  "negative exec timeout in YAML",
			yaml:  "blaxel:\n  execTimeoutSecs: -1\n",
			error: "blaxel execTimeoutSecs must be non-negative",
		},
		{
			name:  "negative exec timeout in env",
			env:   map[string]string{"CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS": "-1"},
			error: "CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS must be non-negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			if tc.yaml != "" {
				var file fileConfig
				if err := yaml.Unmarshal([]byte(tc.yaml), &file); err != nil {
					t.Fatal(err)
				}
				err := applyFileConfig(&cfg, file)
				if err == nil || !strings.Contains(err.Error(), tc.error) {
					t.Fatalf("applyFileConfig err=%v, want %q", err, tc.error)
				}
				return
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			err := applyEnv(&cfg)
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Fatalf("applyEnv err=%v, want %q", err, tc.error)
			}
		})
	}
}

func TestCloudflareSandboxOrderedFileAliasAndPresence(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct{ name, body, wantURL string }{
			{"omitted", "", "https://example.invalid/prior"},
			{"primary", "  bridgeUrl: https://example.invalid/primary\n", "https://example.invalid/primary"},
			{"alias", "  url: https://example.invalid/alias\n", "https://example.invalid/alias"},
			{"both", "  bridgeUrl: https://example.invalid/primary\n  url: https://example.invalid/alias\n", "https://example.invalid/alias"},
			{"reversed", "  url: https://example.invalid/alias\n  bridgeUrl: https://example.invalid/primary\n", "https://example.invalid/alias"},
			{"alias empty", "  bridgeUrl: https://example.invalid/primary\n  url: ''\n", ""},
			{"alias null", "  bridgeUrl: https://example.invalid/primary\n  url: null\n", "https://example.invalid/primary"},
			{"whitespace", "  bridgeUrl: https://example.invalid/primary\n  url: '  '\n", "  "},
		} {
			t.Run(fmt.Sprintf("trusted=%t/%s", trusted, tc.name), func(t *testing.T) {
				cfg := baseConfig()
				cfg.CloudflareSandbox = CloudflareSandboxConfig{BridgeURL: "https://example.invalid/prior", Token: "inert", Workdir: "/workspace/prior", ExecTimeoutSecs: 12, ForgetMissing: true}
				var file fileConfig
				if err := yaml.Unmarshal([]byte("cloudflareSandbox:\n"+tc.body+"  token: ''\n  workdir: ''\n  execTimeoutSecs: 0\n  forgetMissing: false\n"), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				want := CloudflareSandboxConfig{BridgeURL: tc.wantURL}
				if !trusted {
					want.BridgeURL, want.Token = "https://example.invalid/prior", "inert"
				}
				if cfg.CloudflareSandbox != want {
					t.Fatalf("file aliases/presence changed trusted=%t case=%s", trusted, tc.name)
				}
			})
		}
	}
	cfg := baseConfig()
	before := cfg.CloudflareSandbox
	var file fileConfig
	if err := yaml.Unmarshal([]byte("cloudflareSandbox: {bridgeUrl: null, url: null, token: null, workdir: null, execTimeoutSecs: null, forgetMissing: null}"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareSandbox != before {
		t.Fatal("null fields changed config")
	}
}

func TestCloudflareSandboxOverlayPartialErrorOrder(t *testing.T) {
	for _, source := range []string{"file", "env"} {
		for _, raw := range []string{"-1", "invalid", "0"} {
			if source == "file" && raw == "invalid" {
				continue
			}
			t.Run(source+"/"+raw, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				cfg.CloudflareSandbox = CloudflareSandboxConfig{BridgeURL: "https://example.invalid/prior", Token: "inert", Workdir: "/workspace/prior", ExecTimeoutSecs: 12, ForgetMissing: true}
				var err error
				if source == "file" {
					var file fileConfig
					if err := yaml.Unmarshal([]byte("cloudflareSandbox:\n  bridgeUrl: https://example.invalid/primary\n  url: https://example.invalid/after\n  token: inert-after\n  workdir: /workspace/after\n  execTimeoutSecs: "+raw+"\n  forgetMissing: false\n"), &file); err != nil {
						t.Fatal(err)
					}
					err = applyFileConfigWithTrust(&cfg, file, true)
				} else {
					t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_URL", "https://example.invalid/after")
					t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_TOKEN", "inert-after")
					t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_WORKDIR", "/workspace/after")
					t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS", raw)
					t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_FORGET_MISSING", "false")
					err = applyEnv(&cfg)
				}
				want := CloudflareSandboxConfig{BridgeURL: "https://example.invalid/after", Token: "inert-after", Workdir: "/workspace/after"}
				if raw != "0" {
					want.ForgetMissing = true
					if source == "file" {
						want.ExecTimeoutSecs = 12
					}
					message := "cloudflare-sandbox execTimeoutSecs must be non-negative"
					if source == "env" {
						message = "CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS must be non-negative"
						if raw == "invalid" {
							message = "CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS must be an integer"
						}
					}
					if err == nil || err.Error() != message {
						t.Fatalf("error=%v want=%s", err, message)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if cfg.CloudflareSandbox != want {
					t.Fatal("overlay partial mutation order changed")
				}
			})
		}
	}
}

func TestCloudflareSandboxEmptyEnvironmentAndBooleanFallback(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, raw := range []string{"", "invalid", "no", "yes"} {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_URL", "")
			t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_TOKEN", "")
			t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_WORKDIR", "")
			t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS", "")
			t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_FORGET_MISSING", raw)
			cfg := baseConfig()
			cfg.CloudflareSandbox = CloudflareSandboxConfig{BridgeURL: "https://example.invalid/prior", Token: "inert", Workdir: "/workspace/prior", ExecTimeoutSecs: 12, ForgetMissing: prior}
			want := cfg.CloudflareSandbox
			if raw == "no" {
				want.ForgetMissing = false
			}
			if raw == "yes" {
				want.ForgetMissing = true
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.CloudflareSandbox != want {
				t.Fatalf("empty environment or boolean fallback changed raw=%q prior=%t", raw, prior)
			}
		}
	}
}

func TestCloudflareSandboxConfigDefaultsYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.CloudflareSandbox.Workdir != "/workspace/crabbox" || cfg.CloudflareSandbox.ExecTimeoutSecs != 600 {
		t.Fatalf("unexpected cloudflare-sandbox defaults: %#v", cfg.CloudflareSandbox)
	}

	var file fileConfig
	yamlText := strings.Join([]string{
		"cloudflareSandbox:",
		"  url: https://bridge.example.test",
		"  token: trusted-token",
		"  workdir: /workspace/app",
		"  execTimeoutSecs: 60",
		"  forgetMissing: true",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareSandbox.BridgeURL != "https://bridge.example.test" || cfg.CloudflareSandbox.Token != "trusted-token" || cfg.CloudflareSandbox.Workdir != "/workspace/app" || cfg.CloudflareSandbox.ExecTimeoutSecs != 60 || !cfg.CloudflareSandbox.ForgetMissing {
		t.Fatalf("trusted cloudflare-sandbox YAML config not applied: %#v", cfg.CloudflareSandbox)
	}

	var untrusted fileConfig
	if err := yaml.Unmarshal([]byte("cloudflareSandbox:\n  bridgeUrl: https://repo.example.test\n  token: repo-token\n  workdir: /workspace/repo\n  execTimeoutSecs: 30\n"), &untrusted); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, untrusted, false); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareSandbox.BridgeURL != "https://bridge.example.test" || cfg.CloudflareSandbox.Token != "trusted-token" {
		t.Fatalf("untrusted config overrode trusted URL/token: %#v", cfg.CloudflareSandbox)
	}
	if cfg.CloudflareSandbox.Workdir != "/workspace/repo" || cfg.CloudflareSandbox.ExecTimeoutSecs != 30 {
		t.Fatalf("untrusted non-secret config not applied: %#v", cfg.CloudflareSandbox)
	}

	t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_URL", "http://localhost:8787")
	t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_TOKEN", "env-token")
	t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_WORKDIR", "/workspace/env")
	t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS", "90")
	t.Setenv("CRABBOX_CLOUDFLARE_SANDBOX_FORGET_MISSING", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareSandbox.BridgeURL != "http://localhost:8787" || cfg.CloudflareSandbox.Token != "env-token" || cfg.CloudflareSandbox.Workdir != "/workspace/env" || cfg.CloudflareSandbox.ExecTimeoutSecs != 90 || cfg.CloudflareSandbox.ForgetMissing {
		t.Fatalf("cloudflare-sandbox env config not applied: %#v", cfg.CloudflareSandbox)
	}
}

func TestOpenComputerBurstConfigYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("openComputer:\n  burst: true\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenComputer.Burst {
		t.Fatal("openComputer.burst YAML was not applied")
	}

	cfg.OpenComputer.Burst = false
	t.Setenv("CRABBOX_OPENCOMPUTER_BURST", "true")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenComputer.Burst {
		t.Fatal("CRABBOX_OPENCOMPUTER_BURST was not applied")
	}
}

func TestSemaphoreRawDefaultsAndFileAcceptance(t *testing.T) {
	if got := baseConfig().Semaphore; got != (SemaphoreConfig{}) {
		t.Fatal("raw Semaphore defaults must remain empty")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace"} {
			cfg := baseConfig()
			cfg.Semaphore = SemaphoreConfig{Host: "example.semaphoreci.com", Token: "inert", Project: "project", Machine: "machine", OSImage: "image", IdleTimeout: "10m"}
			cfg.credentialProvenance.semaphoreHost, cfg.credentialProvenance.semaphoreToken = credentialSourceFlag, credentialSourceFlag
			want := cfg.Semaphore
			source := credentialSourceFlag
			fields := map[string]any{}
			for _, f := range []struct {
				key string
				v   *string
			}{{"host", &want.Host}, {"token", &want.Token}, {"project", &want.Project}, {"machine", &want.Machine}, {"osImage", &want.OSImage}, {"idleTimeout", &want.IdleTimeout}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
					*f.v = "  "
				}
				fields[f.key] = raw
			}
			if mode == "equal" || mode == "whitespace" {
				source = credentialSourceForFile(trusted)
			}
			data, err := yaml.Marshal(map[string]any{"semaphore": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Semaphore != want || cfg.credentialProvenance.semaphoreHost != source || cfg.credentialProvenance.semaphoreToken != source {
				t.Fatalf("file mode=%s trusted=%t", mode, trusted)
			}
		}
	}
}

func TestSemaphoreRawEnvironmentAliasAndSource(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "equal", "whitespace", "HOST", "TOKEN"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Semaphore = SemaphoreConfig{Host: "prior.semaphoreci.com", Token: "inert", Project: "project", Machine: "machine", OSImage: "image", IdleTimeout: "10m"}
		cfg.credentialProvenance.semaphoreHost, cfg.credentialProvenance.semaphoreToken = credentialSourceFlag, credentialSourceFlag
		want := cfg.Semaphore
		accepted := map[string]bool{}
		for _, f := range []struct {
			suffix, alias string
			v             *string
		}{{"HOST", "SEMAPHORE_HOST", &want.Host}, {"TOKEN", "SEMAPHORE_API_TOKEN", &want.Token}, {"PROJECT", "SEMAPHORE_PROJECT", &want.Project}, {"MACHINE", "", &want.Machine}, {"OS_IMAGE", "", &want.OSImage}, {"IDLE_TIMEOUT", "", &want.IdleTimeout}} {
			primary, alias := *f.v+"-primary", *f.v+"-alias"
			if mode == "equal" {
				primary = *f.v
			}
			if mode == "whitespace" {
				primary = "  "
			}
			allow := mode != "empty" && (!(mode == "HOST" || mode == "TOKEN") || mode == f.suffix)
			if mode == "alias" {
				primary = ""
				allow = f.alias != ""
			}
			if !allow {
				primary, alias = "", ""
			} else if primary != "" {
				*f.v = primary
			} else {
				*f.v = alias
			}
			accepted[f.suffix] = allow
			t.Setenv("CRABBOX_SEMAPHORE_"+f.suffix, primary)
			if f.alias != "" {
				t.Setenv(f.alias, alias)
			}
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		host, token := credentialSourceFlag, credentialSourceFlag
		if accepted["HOST"] {
			host = credentialSourceEnvironment
		}
		if accepted["TOKEN"] {
			token = credentialSourceEnvironment
		}
		if cfg.Semaphore != want || cfg.credentialProvenance.semaphoreHost != host || cfg.credentialProvenance.semaphoreToken != token {
			t.Fatalf("env mode=%s", mode)
		}
	}
}

func TestSmolvmFilePresenceAndPositiveIntegers(t *testing.T) {
	if _, ok := reflect.TypeOf(fileSmolvmConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("API key YAML source introduced")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace"} {
			for _, integer := range []any{nil, 0, -1, 7} {
				cfg := baseConfig()
				cfg.Smolvm = SmolvmConfig{APIKey: "inert", BaseURL: "https://example.invalid/api", Image: "image", Workdir: "/workspace/app", CPUs: 3, MemoryMB: 100, Network: "open", Keep: true}
				cfg.credentialProvenance.smolvmBaseURL = credentialSourceFlag
				cfg.credentialProvenance.smolvmAPIKey = credentialSourceFlag
				want := cfg.Smolvm
				source := credentialSourceFlag
				fields := map[string]any{"apiKey": "ignored-inert"}
				for _, f := range []struct {
					key string
					v   *string
				}{{"baseUrl", &want.BaseURL}, {"image", &want.Image}, {"workdir", &want.Workdir}, {"network", &want.Network}} {
					if mode == "omitted" {
						continue
					}
					var value any = *f.v
					if mode == "null" {
						value = nil
					}
					if mode == "empty" {
						value = ""
					}
					if mode == "whitespace" {
						value = "  "
						*f.v = "  "
					}
					fields[f.key] = value
				}
				if mode == "equal" || mode == "whitespace" {
					source = credentialSourceForFile(trusted)
				}
				fields["cpus"], fields["memoryMB"] = integer, integer
				if v, ok := integer.(int); ok && v > 0 {
					want.CPUs, want.MemoryMB = v, v
				}
				if mode == "null" {
					fields["keep"] = nil
				} else if mode != "omitted" {
					fields["keep"] = false
					want.Keep = false
				}
				data, err := yaml.Marshal(map[string]any{"smolvm": fields})
				if err != nil {
					t.Fatal(err)
				}
				var file fileConfig
				if err := yaml.Unmarshal(data, &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if cfg.Smolvm != want || cfg.credentialProvenance.smolvmBaseURL != source || cfg.credentialProvenance.smolvmAPIKey != credentialSourceFlag {
					t.Fatalf("file mode=%s integer=%v trusted=%t", mode, integer, trusted)
				}
			}
		}
	}
}

func TestSmolvmThreeNameKeyAndAcceptance(t *testing.T) {
	for _, tc := range []struct {
		primary, alias, alias2, want string
		accepted                     bool
	}{{"primary", "alias", "third", "primary", true}, {"", "alias", "third", "alias", true}, {"", "", "third", "third", true}, {"", "", "", "prior", false}, {"  ", "alias", "third", "  ", true}, {"", "  ", "third", "  ", true}, {"prior", "alias", "third", "prior", true}, {"", "", "  ", "  ", true}} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Smolvm.APIKey = "prior"
		cfg.credentialProvenance.smolvmAPIKey = credentialSourceTrustedFile
		cfg.credentialProvenance.smolvmBaseURL = credentialSourceTrustedFile
		t.Setenv("CRABBOX_SMOLVM_API_KEY", tc.primary)
		t.Setenv("SMOLMACHINES_API_KEY", tc.alias)
		t.Setenv("SMK_API_KEY", tc.alias2)
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		source := credentialSourceTrustedFile
		if tc.accepted {
			source = credentialSourceEnvironment
		}
		if cfg.Smolvm.APIKey != tc.want || cfg.credentialProvenance.smolvmAPIKey != source || cfg.credentialProvenance.smolvmBaseURL != credentialSourceTrustedFile {
			t.Fatal("three-name raw key acceptance changed")
		}
	}
}

func TestSmolvmEnvironmentIntegerAndStringSemantics(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 12}, {"invalid", 12}, {" 17 ", 12}, {"9999999999999999999999999", 12}, {"0", 0}, {"-1", -1}, {"17", 17}} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Smolvm.CPUs, cfg.Smolvm.MemoryMB = 12, 12
		cfg.Smolvm.Keep = true
		t.Setenv("CRABBOX_SMOLVM_CPUS", tc.raw)
		t.Setenv("CRABBOX_SMOLVM_MEMORY_MB", tc.raw)
		t.Setenv("CRABBOX_SMOLVM_NETWORK", "blocked")
		t.Setenv("CRABBOX_SMOLVM_KEEP", "false")
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Smolvm.CPUs != tc.want || cfg.Smolvm.MemoryMB != tc.want || cfg.Smolvm.Network != "blocked" || cfg.Smolvm.Keep {
			t.Fatalf("tolerant integer/continuation=%q", tc.raw)
		}
	}
	for _, mode := range []string{"empty", "equal", "whitespace", "changed"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Smolvm = SmolvmConfig{BaseURL: "https://example.invalid/api", Image: "image", Workdir: "/workspace/app", Network: "open", Keep: true}
		cfg.credentialProvenance.smolvmBaseURL = credentialSourceFlag
		cfg.credentialProvenance.smolvmAPIKey = credentialSourceTrustedFile
		want := cfg.Smolvm
		for _, f := range []struct {
			suffix string
			v      *string
		}{{"BASE_URL", &want.BaseURL}, {"IMAGE", &want.Image}, {"WORKDIR", &want.Workdir}, {"NETWORK", &want.Network}} {
			raw := *f.v
			if mode == "empty" {
				raw = ""
			}
			if mode == "whitespace" {
				raw = "  "
				*f.v = raw
			}
			if mode == "changed" {
				raw += "-new"
				*f.v = raw
			}
			t.Setenv("CRABBOX_SMOLVM_"+f.suffix, raw)
		}
		t.Setenv("CRABBOX_SMOLVM_KEEP", "invalid")
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		source := credentialSourceEnvironment
		if mode == "empty" {
			source = credentialSourceFlag
		}
		if cfg.Smolvm != want || cfg.credentialProvenance.smolvmBaseURL != source || cfg.credentialProvenance.smolvmAPIKey != credentialSourceTrustedFile {
			t.Fatalf("string/boolean fallback mode=%s", mode)
		}
	}
	for _, prior := range []bool{false, true} {
		clearConfigEnv(t)
		t.Setenv("CRABBOX_SMOLVM_KEEP", "invalid")
		cfg := baseConfig()
		cfg.Smolvm.Keep = prior
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Smolvm.Keep != prior {
			t.Fatal("malformed boolean changed preceding value")
		}
	}
}

func TestSmolvmConfigYAMLOverrides(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	yamlText := strings.Join([]string{
		"smolvm:",
		"  baseUrl: https://smol.example",
		"  image: ubuntu",
		"  workdir: /srv/app",
		"  cpus: 4",
		"  memoryMB: 4096",
		"  network: closed",
		"  keep: true",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlText), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	want := SmolvmConfig{
		BaseURL:  "https://smol.example",
		Image:    "ubuntu",
		Workdir:  "/srv/app",
		CPUs:     4,
		MemoryMB: 4096,
		Network:  "closed",
		Keep:     true,
	}
	if cfg.Smolvm != want {
		t.Fatalf("smolvm YAML config = %#v, want %#v", cfg.Smolvm, want)
	}
}

func TestSmolvmConfigYAMLEmptyKeepsDefaults(t *testing.T) {
	cfg := baseConfig()
	defaults := cfg.Smolvm
	file := fileConfig{Smolvm: &fileSmolvmConfig{}}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Smolvm != defaults {
		t.Fatalf("empty smolvm YAML changed defaults: %#v, want %#v", cfg.Smolvm, defaults)
	}
}

func TestSmolvmConfigYAMLCannotSetAPIKey(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("smolvm:\n  apiKey: smk-attacker\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Smolvm.APIKey != "" {
		t.Fatalf("repository config set smolvm API key to %q", cfg.Smolvm.APIKey)
	}
}

func TestSmolvmConfigEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("CRABBOX_SMOLVM_API_KEY", "smk-env")
	t.Setenv("CRABBOX_SMOLVM_BASE_URL", "https://env.smol.example")
	t.Setenv("CRABBOX_SMOLVM_IMAGE", "debian")
	t.Setenv("CRABBOX_SMOLVM_WORKDIR", "/env/workdir")
	t.Setenv("CRABBOX_SMOLVM_CPUS", "8")
	t.Setenv("CRABBOX_SMOLVM_MEMORY_MB", "8192")
	t.Setenv("CRABBOX_SMOLVM_NETWORK", "closed")
	t.Setenv("CRABBOX_SMOLVM_KEEP", "true")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	want := SmolvmConfig{
		APIKey:   "smk-env",
		BaseURL:  "https://env.smol.example",
		Image:    "debian",
		Workdir:  "/env/workdir",
		CPUs:     8,
		MemoryMB: 8192,
		Network:  "closed",
		Keep:     true,
	}
	if cfg.Smolvm != want {
		t.Fatalf("smolvm env config = %#v, want %#v", cfg.Smolvm, want)
	}
}

func TestSmolvmAPIKeyEnvFallbacks(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("SMOLMACHINES_API_KEY", "smk-vendor")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Smolvm.APIKey != "smk-vendor" {
		t.Fatalf("SMOLMACHINES_API_KEY fallback = %q, want smk-vendor", cfg.Smolvm.APIKey)
	}

	cfg = baseConfig()
	t.Setenv("SMOLMACHINES_API_KEY", "")
	t.Setenv("SMK_API_KEY", "smk-short")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Smolvm.APIKey != "smk-short" {
		t.Fatalf("SMK_API_KEY fallback = %q, want smk-short", cfg.Smolvm.APIKey)
	}
}

func TestHostingerPurchaseOptInRequiresTrustedConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")

	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	if err := os.WriteFile(".crabbox.yaml", []byte("hostinger:\n  apiToken: attacker-token\n  apiUrl: https://attacker.example\n  itemId: attacker-item\n  paymentMethodId: attacker-payment\n  templateId: attacker-template\n  dataCenterId: attacker-dc\n  allowPurchase: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostinger.AllowPurchase {
		t.Fatal("repo-local config authorized a Hostinger purchase")
	}
	if cfg.Hostinger.APIURL != "https://developers.hostinger.com" {
		t.Fatalf("repo-local config redirected Hostinger API URL: %q", cfg.Hostinger.APIURL)
	}
	if cfg.Hostinger.APIToken != "" || cfg.Hostinger.ItemID != "" || cfg.Hostinger.PaymentMethodID != "" ||
		cfg.Hostinger.TemplateID != "" || cfg.Hostinger.DataCenterID != "" {
		t.Fatalf("repo-local config selected Hostinger account or purchase inputs: %#v", cfg.Hostinger)
	}

	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("hostinger:\n  apiToken: trusted-user-token\n  apiUrl: https://trusted-user.example\n  itemId: trusted-user-item\n  paymentMethodId: trusted-user-payment\n  templateId: trusted-user-template\n  dataCenterId: trusted-user-dc\n  allowPurchase: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Hostinger.AllowPurchase {
		t.Fatal("private user config did not authorize a Hostinger purchase")
	}
	if cfg.Hostinger.APIURL != "https://trusted-user.example" {
		t.Fatalf("private user config API URL=%q", cfg.Hostinger.APIURL)
	}
	if cfg.Hostinger.APIToken != "trusted-user-token" || cfg.Hostinger.ItemID != "trusted-user-item" ||
		cfg.Hostinger.PaymentMethodID != "trusted-user-payment" || cfg.Hostinger.TemplateID != "trusted-user-template" ||
		cfg.Hostinger.DataCenterID != "trusted-user-dc" {
		t.Fatalf("private user config purchase inputs=%#v", cfg.Hostinger)
	}

	explicitPath := filepath.Join(t.TempDir(), "explicit.yaml")
	if err := os.WriteFile(explicitPath, []byte("hostinger:\n  apiUrl: https://trusted-explicit.example\n  allowPurchase: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", explicitPath)
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Hostinger.AllowPurchase {
		t.Fatal("explicit config did not authorize a Hostinger purchase")
	}
	if cfg.Hostinger.APIURL != "https://trusted-explicit.example" {
		t.Fatalf("explicit config API URL=%q", cfg.Hostinger.APIURL)
	}
}

func TestGenericScalarInputSources(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, fileConfig{Provider: "machine0"}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.inputProvenance) != 0 {
		t.Fatal("provider selection is not a configuration setting")
	}
	value := false
	if err := applyFileConfig(&cfg, fileConfig{Desktop: &value}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.inputProvenance.summary(configInputGeneric); got.state != "present" || !reflect.DeepEqual(got.sources, []string{"user_config"}) {
		t.Fatalf("explicit false was not recorded: %#v", got)
	}
	for _, source := range []providerSelectionSource{providerSelectionUserConfig, providerSelectionRepoConfig} {
		if err := applyFileConfigWithTrustAndProviderSource(&cfg, fileConfig{Profile: "same"}, source == providerSelectionUserConfig, source); err != nil {
			t.Fatal(err)
		}
	}
	original := cfg
	t.Setenv("CRABBOX_PROFILE", "same")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	got := cfg.inputProvenance.summary(configInputGeneric)
	if cfg.Profile != "same" || got.complete || !reflect.DeepEqual(got.sources, []string{"user_config", "repo_config", "environment"}) {
		t.Fatalf("equal-value layers: %#v", got)
	}
	if len(original.inputProvenance.summary(configInputGeneric).sources) != 2 {
		t.Fatal("environment application changed a copied ledger")
	}
	t.Setenv("CRABBOX_PROFILE", "")
	for _, raw := range []string{"", "invalid", "false"} {
		t.Run("desktop/"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_DESKTOP", raw)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if (cfg.inputProvenance.summary(configInputGeneric).state == "present") != (raw == "false") {
				t.Fatal("invalid/omitted boolean confused with accepted false")
			}
		})
	}
}

func TestTartInputValueAndIntent(t *testing.T) {
	for _, field := range []struct {
		name string
		read func(Config) (int, bool)
	}{
		{"CPUS", func(c Config) (int, bool) { return c.Tart.CPUs, c.tartCPUsExplicit }},
		{"MEMORY", func(c Config) (int, bool) { return c.Tart.Memory, c.tartMemoryExplicit }},
		{"DISK", func(c Config) (int, bool) { return c.Tart.Disk, c.tartDiskExplicit }},
	} {
		for _, raw := range []string{"", "invalid", " 4 ", "0", "-1", "7"} {
			t.Run(field.name+"/"+raw, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := Config{Tart: TartConfig{CPUs: 7, Memory: 7, Disk: 7}}
				t.Setenv("CRABBOX_TART_"+field.name, raw)
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				value, explicit := field.read(cfg)
				want, parseErr := strconv.Atoi(raw)
				accepted := parseErr == nil
				if !accepted {
					want = 7
				}
				wantExplicit := raw != "" && (field.name != "DISK" || want > 0)
				if value != want || explicit != wantExplicit {
					t.Fatalf("value=%d explicit=%v, want %d/%v", value, explicit, want, wantExplicit)
				}
				facts := cfg.inputProvenance["tart"]
				if (facts.values != 0) != accepted || (facts.intents != 0) != (raw != "") || facts.complete {
					t.Fatalf("incorrect value/intent facts: %#v", facts)
				}
				if got := cfg.inputProvenance.summary("tart"); raw != "" && !reflect.DeepEqual(got.sources, []string{"environment"}) {
					t.Fatalf("sources=%v", got.sources)
				}
			})
		}
	}
	// Invalid disk input can clear intent even though it preserves the value.
	t.Run("disk marker clear", func(t *testing.T) {
		clearConfigEnv(t)
		cfg := Config{tartDiskExplicit: true}
		t.Setenv("CRABBOX_TART_DISK", "invalid")
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Tart.Disk != 0 || cfg.tartDiskExplicit || cfg.inputProvenance["tart"].intents == 0 || cfg.inputProvenance["tart"].values != 0 {
			t.Fatal("disk fallback must preserve zero, clear marker, and record only intent")
		}
	})
}

func TestConfigInputEnvFallbacks(t *testing.T) {
	const primary, alias = "CRABBOX_TEST_INPUT_PRIMARY", "CRABBOX_TEST_INPUT_ALIAS"
	for _, tc := range []struct {
		primary, alias string
		want           int
		accepted       bool
	}{
		{"", "", 7, false}, {"invalid", "", 7, false},
		{" 4 ", "", 7, false}, {"7", "", 7, true},
		{"0", "9", 0, true}, {"invalid", "-2", -2, true},
		{"3", "9", 3, true}, {"", "7", 7, true},
	} {
		t.Run(tc.primary+"/"+tc.alias, func(t *testing.T) {
			t.Setenv(primary, tc.primary)
			t.Setenv(alias, tc.alias)
			var cfg Config
			got := configInputEnvInt(&cfg, "example", 7, primary, alias)
			if got != tc.want || (cfg.inputProvenance["example"].values != 0) != tc.accepted {
				t.Fatalf("value=%d facts=%#v", got, cfg.inputProvenance["example"])
			}
		})
	}
	for _, raw := range []string{"", "kept", " "} {
		t.Run("string/"+raw, func(t *testing.T) {
			t.Setenv(primary, raw)
			var cfg Config
			got := configInputEnvString(&cfg, "example", "kept", primary)
			want := raw
			if raw == "" {
				want = "kept"
			}
			if got != want || (cfg.inputProvenance["example"].values != 0) != (raw != "") {
				t.Fatalf("value=%q facts=%#v", got, cfg.inputProvenance["example"])
			}
		})
	}
}

func TestTartEnvExplicitFlags(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("CRABBOX_TART_CPUS", "8")
	t.Setenv("CRABBOX_TART_MEMORY", "16384")
	applyEnv(&cfg)
	if !IsTartCPUsExplicit(&cfg) {
		t.Fatal("tartCPUsExplicit must be true after env sets CRABBOX_TART_CPUS")
	}
	if !IsTartMemoryExplicit(&cfg) {
		t.Fatal("tartMemoryExplicit must be true after env sets CRABBOX_TART_MEMORY")
	}
}

func TestWindowsSandboxConfigDefaultsFileEnvAndProviderTarget(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.WindowsSandbox.Workdir != `C:\crabbox-work` || cfg.WindowsSandbox.Networking != "Enable" || cfg.WindowsSandbox.VGPU != "Disable" {
		t.Fatalf("windows sandbox defaults not applied: %#v", cfg.WindowsSandbox)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "windows-sandbox",
		WindowsSandbox: &fileWindowsSandboxConfig{
			Workdir:            `C:\repo`,
			TempRoot:           `C:\tmp\crabbox`,
			Networking:         "disable",
			VGPU:               "default",
			Clipboard:          "disable",
			ProtectedClient:    "enable",
			AudioInput:         "disable",
			VideoInput:         "disable",
			PrinterRedirection: "disable",
			MemoryMB:           8192,
		},
	})
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "windows-sandbox" || cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeNormal || cfg.WorkRoot != `C:\repo` {
		t.Fatalf("provider target/workroot not applied: provider=%s target=%s mode=%s workroot=%s", cfg.Provider, cfg.TargetOS, cfg.WindowsMode, cfg.WorkRoot)
	}
	if cfg.WindowsSandbox.Workdir != `C:\repo` || cfg.WindowsSandbox.TempRoot != `C:\tmp\crabbox` || cfg.WindowsSandbox.MemoryMB != 8192 {
		t.Fatalf("file windowsSandbox config not applied: %#v", cfg.WindowsSandbox)
	}

	t.Setenv("CRABBOX_WINDOWS_SANDBOX_WORKDIR", `C:\env\repo`)
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_TEMP_ROOT", `C:\env\tmp`)
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_NETWORKING", "enable")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_VGPU", "disable")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_CLIPBOARD", "default")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_PROTECTED_CLIENT", "default")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_AUDIO_INPUT", "disable")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_VIDEO_INPUT", "disable")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_PRINTER_REDIRECTION", "disable")
	t.Setenv("CRABBOX_WINDOWS_SANDBOX_MEMORY_MB", "4096")
	applyEnv(&cfg)
	if cfg.WindowsSandbox.Workdir != `C:\env\repo` || cfg.WindowsSandbox.TempRoot != `C:\env\tmp` || cfg.WindowsSandbox.Networking != "enable" || cfg.WindowsSandbox.Clipboard != "default" || cfg.WindowsSandbox.MemoryMB != 4096 {
		t.Fatalf("env windowsSandbox config not applied: %#v", cfg.WindowsSandbox)
	}

	badTarget := baseConfig()
	badTarget.Provider = "windows-sandbox"
	badTarget.TargetOS = targetLinux
	badTarget.targetExplicit = true
	if err := applyProviderConfigDefaults(&badTarget); err == nil || !strings.Contains(err.Error(), "target=windows") {
		t.Fatalf("target linux err=%v, want windows-sandbox target rejection", err)
	}

	badMode := baseConfig()
	badMode.Provider = "windows-sandbox"
	badMode.TargetOS = targetWindows
	badMode.WindowsMode = windowsModeWSL2
	badMode.explicitWindowsMode = windowsModeWSL2
	if err := applyProviderConfigDefaults(&badMode); err == nil || !strings.Contains(err.Error(), "windows.mode=normal") {
		t.Fatalf("wsl2 err=%v, want windows-sandbox windows mode rejection", err)
	}
}

func TestRepositoryConfigCannotLoosenWindowsSandboxHostPolicy(t *testing.T) {
	cfg := baseConfig()
	cfg.WindowsSandbox.TempRoot = `C:\trusted-temp`
	cfg.WindowsSandbox.Networking = "Disable"
	cfg.WindowsSandbox.VGPU = "Disable"
	cfg.WindowsSandbox.Clipboard = "Disable"
	cfg.WindowsSandbox.ProtectedClient = "Enable"
	cfg.WindowsSandbox.AudioInput = "Disable"
	cfg.WindowsSandbox.VideoInput = "Disable"
	cfg.WindowsSandbox.PrinterRedirection = "Disable"
	cfg.WindowsSandbox.MemoryMB = 4096

	if err := applyFileConfigWithTrust(&cfg, fileConfig{
		WindowsSandbox: &fileWindowsSandboxConfig{
			Workdir:            `C:\repo-work`,
			TempRoot:           `\\server\share`,
			Networking:         "enable",
			VGPU:               "enable",
			Clipboard:          "enable",
			ProtectedClient:    "disable",
			AudioInput:         "enable",
			VideoInput:         "enable",
			PrinterRedirection: "enable",
			MemoryMB:           65536,
		},
	}, false); err != nil {
		t.Fatal(err)
	}

	if cfg.WindowsSandbox.Workdir != `C:\repo-work` {
		t.Fatalf("repository workdir=%q, want sandbox-local override", cfg.WindowsSandbox.Workdir)
	}
	if cfg.WindowsSandbox.TempRoot != `C:\trusted-temp` || cfg.WindowsSandbox.MemoryMB != 4096 {
		t.Fatalf("repository config changed host resources: %#v", cfg.WindowsSandbox)
	}
	if cfg.WindowsSandbox.Networking != "Disable" ||
		cfg.WindowsSandbox.VGPU != "Disable" ||
		cfg.WindowsSandbox.Clipboard != "Disable" ||
		cfg.WindowsSandbox.ProtectedClient != "Enable" ||
		cfg.WindowsSandbox.AudioInput != "Disable" ||
		cfg.WindowsSandbox.VideoInput != "Disable" ||
		cfg.WindowsSandbox.PrinterRedirection != "Disable" {
		t.Fatalf("repository config loosened sandbox policy: %#v", cfg.WindowsSandbox)
	}
}

func TestRepoConfigBareEnvWildcardDoesNotForwardEveryLocalVariable(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	t.Setenv("CRABBOX_PROOF_API_TOKEN", "critical-secret-value")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("env:\n  allow:\n    - '*'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := allowedEnv(cfg.EnvAllow); got["CRABBOX_PROOF_API_TOKEN"] != "" {
		t.Fatalf("bare wildcard forwarded proof secret: %q", got["CRABBOX_PROOF_API_TOKEN"])
	}
}

func writeReplacementListConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigReplacementListsAcrossYAMLLayers(t *testing.T) {
	for _, layers := range []struct{ lower, upper string }{
		{"defaults", "user"},
		{"defaults", "crabbox.yaml"},
		{"defaults", ".crabbox.yaml"},
		{"user", "crabbox.yaml"},
		{"user", ".crabbox.yaml"},
		{"crabbox.yaml", ".crabbox.yaml"},
	} {
		for _, value := range []struct {
			name, yaml string
		}{
			{"absent", "{}\n"},
			{"omitted", "env: {}\nresults: {}\nrun: {}\n"},
			{"empty", "env:\n  allow: []\nresults:\n  junit: []\nrun:\n  preflightTools: []\n"},
			{"replacement", "env:\n  allow: [BUILD_FLAVOR, BUILD_FLAVOR]\nresults:\n  junit: [new-report.xml, new-report.xml]\nrun:\n  preflightTools: [go, GO]\n"},
		} {
			t.Run(layers.lower+"/"+layers.upper+"/"+value.name, func(t *testing.T) {
				clearConfigEnv(t)
				t.Setenv("CRABBOX_CONFIG", "")
				t.Chdir(t.TempDir())
				path := func(layer string) string {
					if layer == "user" {
						return userConfigPath()
					}
					return layer
				}
				wantAllow, wantJUnit, wantTools := "CI,NODE_OPTIONS", "", ""
				if layers.lower != "defaults" {
					writeReplacementListConfig(t, path(layers.lower), "env:\n  allow: [CI, NODE_OPTIONS, BUILD_FLAVOR]\nresults:\n  junit: [old-report.xml]\n  auto: true\nrun:\n  preflightTools: [cmake]\n")
					wantAllow, wantJUnit, wantTools = "CI,NODE_OPTIONS,BUILD_FLAVOR", "old-report.xml", "cmake"
				}
				writeReplacementListConfig(t, path(layers.upper), value.yaml)
				if value.name == "empty" {
					wantAllow, wantJUnit, wantTools = "", "", ""
				} else if value.name == "replacement" {
					wantAllow, wantJUnit, wantTools = "BUILD_FLAVOR", "new-report.xml", "go"
				}
				cfg, err := loadConfig()
				if err != nil {
					t.Fatal(err)
				}
				for _, list := range []struct {
					name string
					got  []string
					want string
				}{
					{"env.allow", cfg.EnvAllow, wantAllow},
					{"results.junit", cfg.Results.JUnit, wantJUnit},
					{"run.preflightTools", cfg.Run.PreflightTools, wantTools},
				} {
					if got := strings.Join(list.got, ","); got != list.want {
						t.Errorf("%s=%q, want %q", list.name, got, list.want)
					}
				}
				if cfg.Results.Auto != (layers.lower != "defaults") {
					t.Error("replacing results.junit changed independent results.auto")
				}
				if value.name == "empty" {
					for _, name := range []string{"CI", "NODE_OPTIONS", "BUILD_FLAVOR"} {
						t.Setenv(name, "synthetic-local-proof")
					}
					if got := allowedEnv(cfg.EnvAllow); len(got) != 0 {
						t.Error("cleared allowlist still forwards local environment")
					}
					if got := preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, cfg.Run.PreflightTools); len(got) != 0 {
						t.Errorf("cleared preflight tools restored probes: %v", got)
					}
				}
			})
		}
	}
}

func TestConfigReplacementListsStayClearedAndRespectOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", "")
	t.Chdir(t.TempDir())
	writeReplacementListConfig(t, userConfigPath(), "env:\n  allow: [BUILD_FLAVOR]\nresults:\n  junit: [old-report.xml]\nrun:\n  preflightTools: [cmake]\n")
	writeReplacementListConfig(t, "crabbox.yaml", "env:\n  allow: []\nresults:\n  junit: []\nrun:\n  preflightTools: []\n")
	writeReplacementListConfig(t, ".crabbox.yaml", "env: {}\nresults: {}\nrun: {}\n")
	for _, name := range []string{"CRABBOX_ENV_ALLOW", "CRABBOX_RESULTS_JUNIT", "CRABBOX_PREFLIGHT_TOOLS"} {
		t.Setenv(name, "")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.EnvAllow)+len(cfg.Results.JUnit)+len(cfg.Run.PreflightTools) != 0 {
		t.Fatal("omitted fields or empty environment overrides restored cleared lists")
	}
	applyRunEnvAllowFlags(&cfg, []string{"BUILD_FLAVOR,BUILD_FLAVOR"})
	if got := strings.Join(cfg.EnvAllow, ","); got != "BUILD_FLAVOR" {
		t.Fatalf("CLI append after clear=%q", got)
	}
	t.Setenv("CRABBOX_ENV_ALLOW", "CI")
	t.Setenv("CRABBOX_RESULTS_JUNIT", "env-report.xml")
	t.Setenv("CRABBOX_PREFLIGHT_TOOLS", "go")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	applyRunEnvAllowFlags(&cfg, []string{"BUILD_FLAVOR"})
	if strings.Join(cfg.EnvAllow, ",") != "CI,BUILD_FLAVOR" || strings.Join(cfg.Results.JUnit, ",") != "env-report.xml" || strings.Join(cfg.Run.PreflightTools, ",") != "go" {
		t.Fatalf("higher precedence overrides not applied: allow=%v junit=%v tools=%v", cfg.EnvAllow, cfg.Results.JUnit, cfg.Run.PreflightTools)
	}
}

func TestProfileEnvConfigYAMLShape(t *testing.T) {
	var env fileProfileEnvConfig
	if err := yaml.Unmarshal([]byte("CI: 1\nNODE_OPTIONS: --max-old-space-size=4096\nallow:\n  - CUSTOM_*\n"), &env); err != nil {
		t.Fatal(err)
	}
	if env.Values["CI"] != "1" || env.Values["NODE_OPTIONS"] != "--max-old-space-size=4096" {
		t.Fatalf("profile env values not decoded: %#v", env.Values)
	}
	if len(env.Allow) != 1 || env.Allow[0] != "CUSTOM_*" {
		t.Fatalf("profile env allow not decoded: %#v", env.Allow)
	}
	data, err := yaml.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{"CI: \"1\"", "NODE_OPTIONS: --max-old-space-size=4096", "allow:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("marshaled profile env missing %q:\n%s", want, out)
		}
	}
}

func TestProfileEnvConfigYAMLRejectsNonMapping(t *testing.T) {
	var env fileProfileEnvConfig
	err := yaml.Unmarshal([]byte("- CI=1\n"), &env)
	if err == nil || !strings.Contains(err.Error(), "profile env must be a mapping") {
		t.Fatalf("error=%v want profile env mapping error", err)
	}
}

func TestRepoConfigClearsInheritedCacheVolumes(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("cache:\n  volumes:\n    - key: user-cache\n      path: /var/cache/crabbox/user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("cache:\n  volumes: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cache.Volumes) != 0 {
		t.Fatalf("repo config did not clear inherited cache volumes: %#v", cfg.Cache.Volumes)
	}
}

func TestRepoConfigCannotRedirectInheritedXCPNgCredentials(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := "provider: xcp-ng\nxcpNg:\n  apiUrl: https://trusted.example.test\n  username: root\n  password: user-secret\n"
	if err := os.WriteFile(userPath, []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	projectConfig := "xcpNg:\n  apiUrl: https://attacker.example.test\n  username: attacker\n  password: attacker-secret\n  insecureTls: true\n  template: project-template\n"
	if err := os.WriteFile(".crabbox.yaml", []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.XCPNg.APIURL != "https://trusted.example.test" || cfg.XCPNg.InsecureTLS {
		t.Fatalf("project config changed trusted connection: %#v", cfg.XCPNg)
	}
	if cfg.XCPNg.Username != "root" || cfg.XCPNg.Password != "user-secret" || cfg.XCPNg.Template != "project-template" {
		t.Fatalf("unexpected merged xcp-ng config: %#v", cfg.XCPNg)
	}
}

func TestXCPNgHigherPrecedenceNamesClearInheritedUUIDs(t *testing.T) {
	// Values are ordered as Template, TemplateUUID, SR, SRUUID, Network, NetworkUUID.
	type selectors [6]string
	prior := selectors{"old-template", "old-template-uuid", "old-sr", "old-sr-uuid", "old-network", "old-network-uuid"}
	tests := []struct {
		name     string
		absent   bool
		in, want selectors
	}{
		{name: "absent", absent: true, want: prior},
		{name: "both empty", want: prior},
		{
			name: "name only",
			in:   selectors{"new-template", "", "new-sr", "", "new-network", ""},
			want: selectors{"new-template", "", "new-sr", "", "new-network", ""},
		},
		{
			name: "uuid only",
			in:   selectors{"", "new-template-uuid", "", "new-sr-uuid", "", "new-network-uuid"},
			want: selectors{"", "new-template-uuid", "", "new-sr-uuid", "", "new-network-uuid"},
		},
		{
			name: "both populated",
			in:   selectors{"new-template", "new-template-uuid", "new-sr", "new-sr-uuid", "new-network", "new-network-uuid"},
			want: selectors{"new-template", "new-template-uuid", "new-sr", "new-sr-uuid", "new-network", "new-network-uuid"},
		},
		{
			name: "template empty with other updates",
			in:   selectors{"", "", "new-sr", "", "", "new-network-uuid"},
			want: selectors{"old-template", "old-template-uuid", "new-sr", "", "", "new-network-uuid"},
		},
		{
			name: "sr empty with other updates",
			in:   selectors{"", "new-template-uuid", "", "", "new-network", ""},
			want: selectors{"", "new-template-uuid", "old-sr", "old-sr-uuid", "new-network", ""},
		},
		{
			name: "network empty with other updates",
			in:   selectors{"new-template", "", "", "new-sr-uuid", "", ""},
			want: selectors{"new-template", "", "", "new-sr-uuid", "old-network", "old-network-uuid"},
		},
		{
			name: "equal name only clears uuid",
			in:   selectors{"old-template", "", "old-sr", "", "old-network", ""},
			want: selectors{"old-template", "", "old-sr", "", "old-network", ""},
		},
		{
			name: "equal uuid only clears name",
			in:   selectors{"", "old-template-uuid", "", "old-sr-uuid", "", "old-network-uuid"},
			want: selectors{"", "old-template-uuid", "", "old-sr-uuid", "", "old-network-uuid"},
		},
		{
			name: "whitespace name only",
			in:   selectors{" ", "", "\t", "", " \t ", ""},
			want: selectors{" ", "", "\t", "", " \t ", ""},
		},
		{
			name: "whitespace uuid only",
			in:   selectors{"", "\t ", "", " \t", "", "\t\t"},
			want: selectors{"", "\t ", "", " \t", "", "\t\t"},
		},
		{
			name: "both raw padded values",
			in:   selectors{" template ", " template-uuid\t", " sr ", " sr-uuid\t", " network ", " network-uuid\t"},
			want: selectors{" template ", " template-uuid\t", " sr ", " sr-uuid\t", " network ", " network-uuid\t"},
		},
	}
	for _, source := range []string{"file", "env"} {
		for _, tt := range tests {
			t.Run(source+"/"+tt.name, func(t *testing.T) {
				clearConfigEnv(t)
				for _, key := range []string{"CRABBOX_PROVIDER", "CRABBOX_OS", "CRABBOX_WORK_ROOT", "CRABBOX_USER", "CRABBOX_SSH_USER", "CRABBOX_XCP_NG_HOST", "CRABBOX_XCP_NG_USER", "CRABBOX_XCP_NG_WORK_ROOT"} {
					t.Setenv(key, "")
				}
				cfg := baseConfig()
				cfg.Provider = "unselected-config-test"
				cfg.XCPNg = XCPNgConfig{
					Template: prior[0], TemplateUUID: prior[1],
					SR: prior[2], SRUUID: prior[3],
					Network: prior[4], NetworkUUID: prior[5],
					Host: "prior-host", User: "prior-user", WorkRoot: t.TempDir(),
				}
				want := cfg.XCPNg
				want.Template, want.TemplateUUID = tt.want[0], tt.want[1]
				want.SR, want.SRUUID = tt.want[2], tt.want[3]
				want.Network, want.NetworkUUID = tt.want[4], tt.want[5]
				if source == "file" {
					file := fileConfig{}
					wantFile := fileConfig{}
					if !tt.absent {
						input := fileXCPNgConfig{
							Template: tt.in[0], TemplateUUID: tt.in[1],
							SR: tt.in[2], SRUUID: tt.in[3],
							Network: tt.in[4], NetworkUUID: tt.in[5],
						}
						inputCopy := input
						file.XCPNg, wantFile.XCPNg = &input, &inputCopy
					}
					if err := applyFileConfig(&cfg, file); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(file, wantFile) {
						t.Fatal("file input changed")
					}
				} else {
					for i, key := range []string{"TEMPLATE", "TEMPLATE_UUID", "SR", "SR_UUID", "NETWORK", "NETWORK_UUID"} {
						key = "CRABBOX_XCP_NG_" + key
						t.Setenv(key, tt.in[i])
						if tt.absent {
							if err := os.Unsetenv(key); err != nil {
								t.Fatal(err)
							}
						}
					}
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(cfg.XCPNg, want) {
					t.Fatalf("XCPNg=%#v, want %#v", cfg.XCPNg, want)
				}
			})
		}
	}
}

func TestRepoConfigCannotOverrideFreestyleAPIURL(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("freestyle:\n  apiUrl: https://trusted.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("freestyle:\n  apiUrl: https://untrusted.example.test\n  workdir: repo-workdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Freestyle.APIURL != "https://trusted.example.test" {
		t.Fatalf("Freestyle.APIURL=%q, want trusted user endpoint", cfg.Freestyle.APIURL)
	}
	if cfg.Freestyle.Workdir != "repo-workdir" {
		t.Fatalf("Freestyle.Workdir=%q, want repository config applied", cfg.Freestyle.Workdir)
	}
	if got := cfg.inputProvenance.summary("freestyle").sources; !reflect.DeepEqual(got, []string{"user_config", "repo_config"}) {
		t.Fatalf("accepted workdir source missing: %v", got)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("freestyle:\n  apiUrl: https://untrusted.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.inputProvenance.summary("freestyle").sources; !reflect.DeepEqual(got, []string{"user_config"}) {
		t.Fatalf("restored repository URL acquired a source: %v", got)
	}
	if err := os.Remove(userPath); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.inputProvenance.summary("freestyle").state != "unknown" {
		t.Fatal("URL-only repository input acquired a finalized source")
	}
}

func TestExplicitRepoConfigCannotRedirectInheritedMorphCredential(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	runGit(t, repo, "init")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_MORPH_API_KEY", "inherited-test-key")

	configPath := filepath.Join(repo, ".crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("provider: morph\nmorph:\n  apiUrl: https://attacker.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(repo, "nested")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	t.Setenv("CRABBOX_CONFIG", configPath)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.credentialProvenance.morphAPIURL != credentialSourceRepository {
		t.Fatalf("explicit repository endpoint source=%v, want repository", cfg.credentialProvenance.morphAPIURL)
	}
	err = validateProviderCredentialDestination(cfg)
	if err == nil || !strings.Contains(err.Error(), "morph.apiUrl") {
		t.Fatalf("destination validation err=%v, want morph.apiUrl rejection", err)
	}
}

func TestExplicitConfigSymlinkIntoRepoRemainsUntrusted(t *testing.T) {
	clearConfigEnv(t)
	repo := t.TempDir()
	runGit(t, repo, "init")
	configPath := filepath.Join(repo, ".crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("provider: morph\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(t.TempDir(), "explicit.yaml")
	if err := os.Symlink(configPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(repo)
	t.Setenv("CRABBOX_CONFIG", linkPath)

	trust := classifyConfigPath(linkPath)
	if trust.trusted {
		t.Fatal("explicit symlink into repository was trusted")
	}
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if trust.repositoryRoot != wantRoot {
		t.Fatalf("repository root=%q, want %q", trust.repositoryRoot, wantRoot)
	}
}

func TestConfigDiscoveryDoesNotReadRepositoryMetadata(t *testing.T) {
	clearConfigEnv(t)
	repo := t.TempDir()
	runGit(t, repo, "init")
	t.Chdir(repo)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, configPath, "provider: aws\nprofile: root-discovery\n")
	t.Setenv("CRABBOX_CONFIG", configPath)
	tracePath := filepath.Join(t.TempDir(), "git-trace.jsonl")
	// Git's global Trace2 setting observes real commands without changing the
	// credential-filtered environment used by repository discovery.
	runGit(t, repo, "config", "--file", filepath.Join(os.Getenv("HOME"), ".gitconfig"), "trace2.eventTarget", tracePath)
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.Run(context.Background(), []string{"config", "show", "--json"}); err != nil {
		t.Fatal(err)
	}
	var config struct{ Profile string }
	if err := json.Unmarshal(stdout.Bytes(), &config); err != nil || config.Profile != "root-discovery" {
		t.Fatalf("config show profile=%q decode=%v", config.Profile, err)
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	discoveredRoot := false
	decoder := json.NewDecoder(bytes.NewReader(trace))
	for {
		var event struct {
			Event string
			Argv  []string
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event.Event != "start" {
			continue
		}
		command := strings.Join(event.Argv[1:], " ")
		if strings.Contains(command, "--show-toplevel") {
			discoveredRoot = true
		}
		if strings.HasPrefix(command, "remote ") || strings.HasPrefix(command, "symbolic-ref ") || strings.HasPrefix(command, "branch ") || command == "rev-parse HEAD" {
			t.Errorf("config discovery read unused repository metadata: git %s", command)
		}
	}
	if !discoveredRoot {
		t.Fatal("config discovery did not resolve the active repository root")
	}
}

func TestUserConfigRemainsTrustedForInheritedMorphCredential(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	runGit(t, repo, "init")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_MORPH_API_KEY", "inherited-test-key")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("provider: morph\nmorph:\n  apiUrl: https://trusted.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.credentialProvenance.morphAPIURL != credentialSourceTrustedFile {
		t.Fatalf("user endpoint source=%v, want trusted file", cfg.credentialProvenance.morphAPIURL)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("trusted user config rejected: %v", err)
	}
}

func TestCacheVolumesOmittedKeepsInheritedConfig(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Cache.Volumes = []CacheVolumeConfig{{Key: "user-cache", Path: "/var/cache/crabbox/user"}}
	pnpm := false
	file := fileConfig{Cache: &fileCacheConfig{Pnpm: &pnpm}}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cache.Volumes) != 1 || cfg.Cache.Volumes[0].Key != "user-cache" {
		t.Fatalf("omitted cache volumes should keep inherited value: %#v", cfg.Cache.Volumes)
	}
}

func TestApplyFileParallelsTemplateConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := ParallelsTemplateConfig{
		Source:   "macOS Tahoe",
		TargetOS: targetMacOS,
	}
	got := applyFileParallelsTemplateConfig(existing, fileParallelsTemplateConfig{
		Source:           "Windows 11",
		SourceID:         "{source-id}",
		SourceSnapshot:   "Known Good",
		SourceSnapshotID: "{snapshot-id}",
		Target:           targetLinux,
		TargetOS:         targetWindows,
		WindowsMode:      windowsModeNormal,
		CloneMode:        "linked",
		Host:             "mac.example.test",
		HostUser:         "build",
		HostKey:          "~/keys/parallels",
		VMRoot:           "~/Parallels",
		User:             "runner",
		WorkRoot:         "C:\\crabbox",
	})
	if got.Source != "Windows 11" || got.SourceID != "{source-id}" || got.SourceSnapshot != "Known Good" || got.SourceSnapshotID != "{snapshot-id}" {
		t.Fatalf("source fields=%#v", got)
	}
	if got.TargetOS != targetWindows || got.WindowsMode != windowsModeNormal || got.CloneMode != "linked" {
		t.Fatalf("target fields=%#v", got)
	}
	if got.Host != "mac.example.test" || got.HostUser != "build" || got.User != "runner" || got.WorkRoot != "C:\\crabbox" {
		t.Fatalf("host/user fields=%#v", got)
	}
	if got.HostKey != filepath.Join(home, "keys/parallels") || got.VMRoot != filepath.Join(home, "Parallels") {
		t.Fatalf("expanded paths hostKey=%q vmRoot=%q", got.HostKey, got.VMRoot)
	}
}

func TestApplyFileParallelsHostConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	targets := []string{targetMacOS, targetLinux}
	got := applyFileParallelsHostConfig(fileParallelsHostConfig{
		Name:    " mac-mini ",
		Host:    " mac.example.test ",
		User:    " build ",
		Key:     " ~/keys/fleet ",
		VMRoot:  " ~/Parallels ",
		Targets: targets,
		MaxVMs:  3,
	})
	targets[0] = targetWindows
	if got.Name != "mac-mini" || got.Host != "mac.example.test" || got.User != "build" {
		t.Fatalf("trimmed fields=%#v", got)
	}
	if got.Key != filepath.Join(home, "keys/fleet") || got.VMRoot != filepath.Join(home, "Parallels") {
		t.Fatalf("expanded paths key=%q vmRoot=%q", got.Key, got.VMRoot)
	}
	if got.MaxVMs != 3 || len(got.Targets) != 2 || got.Targets[0] != targetMacOS || got.Targets[1] != targetLinux {
		t.Fatalf("targets/max=%#v", got)
	}
}

func TestParallelsServerTypeForConfig(t *testing.T) {
	if got := parallelsServerTypeForConfig(Config{}); got != "template" {
		t.Fatalf("empty=%q", got)
	}
	if got := parallelsServerTypeForConfig(Config{Parallels: ParallelsConfig{Template: "macOS Tahoe Latest"}}); got != "template-macos-tahoe-latest" {
		t.Fatalf("template=%q", got)
	}
	if got := parallelsServerTypeForConfig(Config{Parallels: ParallelsConfig{SourceID: "{VM-ID}"}}); got != "template-vm-id" {
		t.Fatalf("source id=%q", got)
	}
}

func TestApplyParallelsTemplateConfigSourceIDsAndEmptyName(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.Source = "old-source"
	cfg.Parallels.SourceSnapshot = "old-snapshot"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"ids": {
			SourceID:         "{source-id}",
			SourceSnapshotID: "{snapshot-id}",
			TargetOS:         "windows",
			WindowsMode:      windowsModeNormal,
			CloneMode:        "linked",
			HostKey:          "/keys/fleet",
			VMRoot:           "/vms",
			WorkRoot:         "C:\\work",
		},
	}
	if err := ApplyParallelsTemplateConfig(&cfg, " "); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Template != "" || cfg.parallelsTemplateApplied {
		t.Fatalf("empty name should be no-op: %#v", cfg.Parallels)
	}
	if err := ApplyParallelsTemplateConfig(&cfg, "ids"); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Source != "old-source" || cfg.Parallels.SourceID != "{source-id}" || cfg.Parallels.SourceSnapshot != "old-snapshot" || cfg.Parallels.SourceSnapshotID != "{snapshot-id}" {
		t.Fatalf("source ids=%#v", cfg.Parallels)
	}
	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeNormal || cfg.WorkRoot != "C:\\work" {
		t.Fatalf("defaults target=%s windows=%s work=%s", cfg.TargetOS, cfg.WindowsMode, cfg.WorkRoot)
	}
	if cfg.Parallels.CloneMode != "linked" || cfg.Parallels.HostKey != "/keys/fleet" || cfg.Parallels.VMRoot != "/vms" || !cfg.parallelsTemplateApplied {
		t.Fatalf("template fields=%#v applied=%v", cfg.Parallels, cfg.parallelsTemplateApplied)
	}
}

func TestLoadConfigFromUserFile(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`broker:
  url: https://crabbox.example.test
  mode: registered
  autoWebVNC: false
  token: secret
  adminToken: admin-secret
  provider: aws
  access:
    clientId: access-client
    clientSecret: access-secret
    token: access-jwt
class: standard
target: windows
hostId: h-neutral-file
windows:
  mode: wsl2
lease:
  ttl: 2h
  idleTimeout: 45m
aws:
  region: eu-west-1
  rootGB: 800
  sshCIDRs:
    - 198.51.100.7/32
sync:
  checksum: true
  gitSeed: false
  baseRef: trunk
  timeout: 30m
  warnFiles: 100
  warnBytes: 200
  failFiles: 300
  failBytes: 400
  allowLarge: true
  exclude:
    - .artifacts
    - tmp
    - '!tmp'
    - tmp
env:
  allow:
    - CI
    - NODE_OPTIONS
    - CUSTOM_*
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
  hints: false
  regions:
    - eu-west-1
actions:
  repo: openclaw/crabbox
  workflow: .github/workflows/crabbox.yml
  job: hydrate
  ref: main
  fields:
    - crabbox_docker_cache=true
    - crabbox_prepare_images=1
  runnerLabels:
    - crabbox
    - linux-large
  runnerVersion: latest
  ephemeral: false
blacksmith:
  org: openclaw
  workflow: .github/workflows/blacksmith-testbox.yml
  job: hydrate
  ref: main
  idleTimeout: 90m
  debug: true
namespace:
  image: crabbox-ready
  size: L
  repository: github.com/openclaw/crabbox
  site: fra1
  volumeSizeGB: 120
  autoStopIdleTimeout: 1h
  workRoot: /workspaces/test
  deleteOnRelease: true
morph:
  apiKey: morph-file-key
  apiUrl: https://morph.example.test
  snapshot: snapshot-file
  sshGatewayHost: ssh.morph.example.test
  workRoot: /tmp/morph-test
  deleteOnRelease: true
  wakeOnSSH: false
daytona:
  apiUrl: https://daytona.example.test/api
  snapshot: crabbox-ready
  target: us
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.daytona.example.test
  sshAccessMinutes: 12
azureDynamicSessions:
  endpoint: https://pool.env.eastus.azurecontainerapps.io
  pool: pool
  apiVersion: 2025-02-02-preview
  workdir: /workspace/file
  timeoutSecs: 120
e2b:
  apiUrl: https://api.e2b.example.test
  domain: e2b.example.test
  template: crabbox-ready
  workdir: work/repo
  user: sandbox
railway:
  apiUrl: https://railway.example.test/graphql/v2
  projectId: project-file
  environmentId: environment-file
runpod:
  apiUrl: https://runpod.example.test/v1
  cloudType: SECURE
  instanceId: NVIDIA L4
  image: runpod/pytorch:custom
  templateId: tpl-file
  diskGB: 25
  user: runpod-user
  workRoot: /workspaces/runpod-test
vast:
  apiUrl: https://vast.example.test/api/v0
  instanceType: on-demand
  gpuName: RTX 4090
  gpuCount: 2
  image: nvidia/cuda:vast-file
  templateId: vast-tpl-file
  runtype: ssh_direct
  diskGB: 60
  maxDphTotal: 3.5
  minReliability: 0.9
  order: reliability desc
  user: root
  workRoot: /workspaces/vast-test
  releaseAction: stop
islo:
  baseUrl: https://islo.example.test
  image: docker.io/library/ubuntu:24.04
  workdir: crabbox
  gatewayProfile: default
  snapshotName: snap-ready
  vcpus: 4
  memoryMB: 8192
  diskGB: 40
freestyle:
  apiUrl: https://freestyle.example.test
  workdir: team/repo
  vcpus: 4
  memoryGB: 8
tenki:
  cliPath: /usr/local/bin/tenki
  endpoint: https://api.tenki.example.test
  gateway: wss://gateway.tenki.example.test
  workspace: ws_file
  project: proj_file
  image: ubuntu:tenki
  workRoot: /home/tenki/test
  cpus: 4
  memoryMB: 8192
  diskGB: 40
coder:
  cliPath: /usr/local/bin/coder
  template: go-dev
  preset: large
  workspacePrefix: cbx-
  workRoot: /home/coder/test
  deleteOnRelease: true
  wait: auto
  useParameterDefaults: true
  parameters:
    - region=iad
    - size=large
  richParameterFile: ~/.config/coder/params.yaml
tensorlake:
  apiUrl: https://api.tensorlake.example.test
  cliPath: /usr/local/bin/tl
  image: ubuntu-22.04
  snapshot: snap-tl
  organizationId: org-tl
  projectId: proj-tl
  namespace: ns-tl
  workdir: /workspace/crabbox-test
  cpus: 4
  memoryMB: 8192
  diskMB: 30000
  timeoutSecs: 1800
  noInternet: true
cua:
  apiUrl: https://ignored.example.test
  apiKey: ignored
  image: ubuntu:cua-file
  kind: vm
  region: us-west
  workdir: /workspace/cua-test
  vcpus: 4
  memoryMB: 8192
  diskGB: 40
  startupTimeoutSecs: 300
  execTimeoutSecs: 900
  bridgeCommand: python3.12
  sdkPackage: cua
  sdkImport: cua
  sdkFallbackImport: cua_sandbox
openComputer:
  apiUrl: https://opencomputer.example.test
  workdir: /workspace/oc-test
  cpu: 8
  memoryMB: 16384
  timeoutSecs: 600
  execTimeoutSecs: 7200
openSandbox:
  apiUrl: https://opensandbox-file-ignored.example.test
  image: docker.io/library/python:3.12
  workdir: /workspace/osb-test
  cpu: "2"
  memory: 4Gi
  timeoutSecs: 900
  execTimeoutSecs: 1800
  platformOS: linux
  platformArch: arm64
  secureAccess: true
  useServerProxy: true
superserve:
  baseUrl: https://superserve-file-ignored.example.test
  template: superserve/custom
  snapshot: snap-file
  workdir: /workspace/ss-test
  timeoutSecs: 777
  execTimeoutSecs: 888
  networkAllowOut:
    - api.example.test
    - ""
    - pkg.example.test
  networkDenyOut:
    - 169.254.169.254/32
  forgetMissing: true
cloudflare:
  apiUrl: https://cloudflare.example.test
  token: cloudflare-token
  workdir: /workspace/cf-test
proxmox:
  apiUrl: https://pve.example.test:8006
  tokenId: crabbox@pve!test
  tokenSecret: proxmox-secret
  node: pve1
  templateId: 9000
  storage: local-lvm
  pool: crabbox
  bridge: vmbr1
  user: runner
  workRoot: /work/proxmox
  fullClone: false
  insecureTLS: true
xcpNg:
  apiUrl: https://xcp-ng.example.test
  username: root
  password: xcp-ng-secret
  template: ubuntu-template
  templateUuid: tpl-0001
  sr: default-sr
  srUuid: sr-0001
  network: pool-network
  networkUuid: net-0001
  host: host-0001
  user: runner
  workRoot: /work/xcp-ng
  insecureTLS: true
semaphore:
  host: semaphore.example.test
  token: semaphore-token
  project: crabbox
  machine: f1-standard-4
  osImage: ubuntu2404
  idleTimeout: 15m
sprites:
  apiUrl: https://api.sprites.example.test
  workRoot: /home/sprite/test
static:
  id: win-dev
  name: windows-dev
  host: win-dev.local
  user: peter
  port: "22"
  workRoot: /home/peter/crabbox
results:
  auto: true
  failOnFailures: true
  junit:
    - junit.xml
shard:
  maxCount: 12
run:
  preflightTools:
    - node
    - bun
cache:
  pnpm: true
  npm: false
  docker: true
  git: true
  maxGB: 120
  purgeOnRelease: true
  volumes:
    - name: pnpm-store
      key: my-app-linux-amd64-node24-pnpm10-lock
      path: /var/cache/crabbox/pnpm
      sizeGB: 80
      required: true
ssh:
  key: ~/.ssh/crabbox
  fallbackPorts:
    - "22"
    - "2022"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" {
		t.Fatalf("Provider=%q want aws", cfg.Provider)
	}
	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeWSL2 {
		t.Fatalf("target config not loaded: target=%s windowsMode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
	if cfg.ServerType != "m8i.large" {
		t.Fatalf("ServerType=%q want m8i.large", cfg.ServerType)
	}
	if cfg.SSHUser != "Administrator" {
		t.Fatalf("SSHUser=%q want Administrator", cfg.SSHUser)
	}
	if cfg.Coordinator != "https://crabbox.example.test" || cfg.CoordToken != "secret" || cfg.CoordAdminToken != "admin-secret" {
		t.Fatalf("broker config not loaded: %#v", cfg)
	}
	if cfg.BrokerMode != BrokerModeRegistered || cfg.BrokerAutoWebVNC {
		t.Fatalf("broker registration config not loaded: mode=%q autoWebVNC=%t", cfg.BrokerMode, cfg.BrokerAutoWebVNC)
	}
	if cfg.HostID != "h-neutral-file" {
		t.Fatalf("host id not loaded: %q", cfg.HostID)
	}
	if cfg.Access.ClientID != "access-client" || cfg.Access.ClientSecret != "access-secret" || cfg.Access.Token != "access-jwt" {
		t.Fatalf("access config not loaded: %#v", cfg.Access)
	}
	if cfg.TTL.String() != "2h0m0s" || cfg.IdleTimeout.String() != "45m0s" {
		t.Fatalf("lease config not loaded: ttl=%s idle=%s", cfg.TTL, cfg.IdleTimeout)
	}
	if cfg.AWSRootGB != 800 {
		t.Fatalf("AWSRootGB=%d want 800", cfg.AWSRootGB)
	}
	if len(cfg.AWSSSHCIDRs) != 1 || cfg.AWSSSHCIDRs[0] != "198.51.100.7/32" {
		t.Fatalf("AWSSSHCIDRs=%v", cfg.AWSSSHCIDRs)
	}
	if cfg.SSHKey != filepath.Join(home, ".ssh", "crabbox") {
		t.Fatalf("SSHKey=%q", cfg.SSHKey)
	}
	if len(cfg.SSHFallbackPorts) != 2 || cfg.SSHFallbackPorts[0] != "22" || cfg.SSHFallbackPorts[1] != "2022" {
		t.Fatalf("SSHFallbackPorts=%v", cfg.SSHFallbackPorts)
	}
	if !cfg.Sync.Checksum || cfg.Sync.GitSeed || cfg.Sync.BaseRef != "trunk" {
		t.Fatalf("sync config not loaded: %#v", cfg.Sync)
	}
	if cfg.Sync.Timeout.String() != "30m0s" || cfg.Sync.WarnFiles != 100 || cfg.Sync.WarnBytes != 200 || cfg.Sync.FailFiles != 300 || cfg.Sync.FailBytes != 400 || !cfg.Sync.AllowLarge {
		t.Fatalf("sync guardrails not loaded: %#v", cfg.Sync)
	}
	if got, want := cfg.Sync.Excludes, []string{".artifacts", "tmp", "!tmp", "tmp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sync excludes not loaded: %#v", cfg.Sync.Excludes)
	}
	if len(cfg.EnvAllow) != 3 || cfg.EnvAllow[2] != "CUSTOM_*" {
		t.Fatalf("env allow not loaded: %#v", cfg.EnvAllow)
	}
	if cfg.Capacity.Strategy != "most-available" || cfg.Capacity.Hints || len(cfg.Capacity.Regions) != 1 || cfg.Capacity.Regions[0] != "eu-west-1" {
		t.Fatalf("capacity config not loaded: %#v", cfg.Capacity)
	}
	if cfg.Actions.Repo != "openclaw/crabbox" || cfg.Actions.Workflow != ".github/workflows/crabbox.yml" || cfg.Actions.Job != "hydrate" || cfg.Actions.Ref != "main" {
		t.Fatalf("actions config not loaded: %#v", cfg.Actions)
	}
	if len(cfg.Actions.Fields) != 2 || cfg.Actions.Fields[0] != "crabbox_docker_cache=true" || cfg.Actions.Fields[1] != "crabbox_prepare_images=1" {
		t.Fatalf("actions fields config not loaded: %#v", cfg.Actions.Fields)
	}
	if cfg.Actions.Ephemeral || len(cfg.Actions.RunnerLabels) != 2 || cfg.Actions.RunnerLabels[1] != "linux-large" {
		t.Fatalf("actions runner config not loaded: %#v", cfg.Actions)
	}
	if cfg.Blacksmith.Org != "openclaw" || cfg.Blacksmith.Workflow != ".github/workflows/blacksmith-testbox.yml" || cfg.Blacksmith.Job != "hydrate" || cfg.Blacksmith.Ref != "main" || cfg.Blacksmith.IdleTimeout != 90*time.Minute || !cfg.Blacksmith.Debug {
		t.Fatalf("blacksmith config not loaded: %#v", cfg.Blacksmith)
	}
	if cfg.Namespace.Image != "crabbox-ready" || cfg.Namespace.Size != "L" || cfg.Namespace.Repository != "github.com/openclaw/crabbox" || cfg.Namespace.Site != "fra1" || cfg.Namespace.VolumeSizeGB != 120 || cfg.Namespace.AutoStopIdleTimeout != time.Hour || cfg.Namespace.WorkRoot != "/workspaces/test" || !cfg.Namespace.DeleteOnRelease {
		t.Fatalf("namespace config not loaded: %#v", cfg.Namespace)
	}
	if cfg.Morph.APIKey != "morph-file-key" || cfg.Morph.APIURL != "https://morph.example.test" || cfg.Morph.Snapshot != "snapshot-file" || cfg.Morph.SSHGatewayHost != "ssh.morph.example.test" || cfg.Morph.WorkRoot != "/tmp/morph-test" || !cfg.Morph.DeleteOnRelease || cfg.Morph.WakeOnSSH {
		t.Fatalf("morph config not loaded: %#v", cfg.Morph)
	}
	if cfg.Daytona.APIURL != "https://daytona.example.test/api" || cfg.Daytona.Snapshot != "crabbox-ready" || cfg.Daytona.Target != "us" || cfg.Daytona.User != "daytona" || cfg.Daytona.WorkRoot != "/home/daytona/crabbox" || cfg.Daytona.SSHGatewayHost != "ssh.daytona.example.test" || cfg.Daytona.SSHAccessMinutes != 12 {
		t.Fatalf("daytona config not loaded: %#v", cfg.Daytona)
	}
	if cfg.AzureDynamicSessions.Endpoint != "https://pool.env.eastus.azurecontainerapps.io" || cfg.AzureDynamicSessions.Pool != "pool" || cfg.AzureDynamicSessions.Workdir != "/workspace/file" || cfg.AzureDynamicSessions.TimeoutSecs != 120 {
		t.Fatalf("azure dynamic sessions config not loaded: %#v", cfg.AzureDynamicSessions)
	}
	if cfg.E2B.APIURL != "https://api.e2b.example.test" || cfg.E2B.Domain != "e2b.example.test" || cfg.E2B.Template != "crabbox-ready" || cfg.E2B.Workdir != "work/repo" || cfg.E2B.User != "sandbox" {
		t.Fatalf("e2b config not loaded: %#v", cfg.E2B)
	}
	if cfg.Railway.APIURL != "https://railway.example.test/graphql/v2" || cfg.Railway.ProjectID != "project-file" || cfg.Railway.EnvironmentID != "environment-file" {
		t.Fatalf("railway config not loaded: %#v", cfg.Railway)
	}
	if cfg.Runpod.APIURL != "https://runpod.example.test/v1" || cfg.Runpod.CloudType != "SECURE" || cfg.Runpod.InstanceID != "NVIDIA L4" || cfg.Runpod.Image != "runpod/pytorch:custom" || cfg.Runpod.TemplateID != "tpl-file" || cfg.Runpod.DiskGB != 25 || cfg.Runpod.User != "runpod-user" || cfg.Runpod.WorkRoot != "/workspaces/runpod-test" {
		t.Fatalf("runpod config not loaded: %#v", cfg.Runpod)
	}
	if cfg.Vast.APIURL != "https://vast.example.test/api/v0" || cfg.Vast.InstanceType != "on-demand" || cfg.Vast.GPUName != "RTX 4090" || cfg.Vast.GPUCount != 2 || cfg.Vast.Image != "nvidia/cuda:vast-file" || cfg.Vast.TemplateID != "vast-tpl-file" || cfg.Vast.Runtype != "ssh_direct" || cfg.Vast.DiskGB != 60 || cfg.Vast.MaxDphTotal != 3.5 || cfg.Vast.MinReliability != 0.9 || cfg.Vast.Order != "reliability desc" || cfg.Vast.User != "root" || cfg.Vast.WorkRoot != "/workspaces/vast-test" || cfg.Vast.ReleaseAction != "stop" {
		t.Fatalf("vast config not loaded: %#v", cfg.Vast)
	}
	if cfg.Islo.BaseURL != "https://islo.example.test" || cfg.Islo.Image != "docker.io/library/ubuntu:24.04" || cfg.Islo.Workdir != "crabbox" || cfg.Islo.GatewayProfile != "default" || cfg.Islo.SnapshotName != "snap-ready" || cfg.Islo.VCPUs != 4 || cfg.Islo.MemoryMB != 8192 || cfg.Islo.DiskGB != 40 {
		t.Fatalf("islo config not loaded: %#v", cfg.Islo)
	}
	if cfg.Freestyle.APIURL != "https://freestyle.example.test" || cfg.Freestyle.Workdir != "team/repo" || cfg.Freestyle.VCPUs != 4 || cfg.Freestyle.MemoryGB != 8 {
		t.Fatalf("freestyle config not loaded: %#v", cfg.Freestyle)
	}
	if cfg.Tenki.CLIPath != "/usr/local/bin/tenki" || cfg.Tenki.Endpoint != "https://api.tenki.example.test" || cfg.Tenki.Gateway != "wss://gateway.tenki.example.test" || cfg.Tenki.Workspace != "ws_file" || cfg.Tenki.Project != "proj_file" || cfg.Tenki.Image != "ubuntu:tenki" || cfg.Tenki.WorkRoot != "/home/tenki/test" || cfg.Tenki.CPUs != 4 || cfg.Tenki.MemoryMB != 8192 || cfg.Tenki.DiskGB != 40 {
		t.Fatalf("tenki config not loaded: %#v", cfg.Tenki)
	}
	if cfg.Coder.CLIPath != "/usr/local/bin/coder" || cfg.Coder.Template != "go-dev" || cfg.Coder.Preset != "large" || cfg.Coder.WorkspacePrefix != "cbx-" || cfg.Coder.WorkRoot != "/home/coder/test" || !cfg.Coder.DeleteOnRelease || cfg.Coder.Wait != "auto" || !cfg.Coder.UseParameterDefaults || len(cfg.Coder.Parameters) != 2 || cfg.Coder.Parameters[1] != "size=large" || cfg.Coder.RichParameterFile != filepath.Join(home, ".config", "coder", "params.yaml") {
		t.Fatalf("coder config not loaded: %#v", cfg.Coder)
	}
	if cfg.Tensorlake.APIURL != "https://api.tensorlake.example.test" || cfg.Tensorlake.CLIPath != "/usr/local/bin/tl" || cfg.Tensorlake.Image != "ubuntu-22.04" || cfg.Tensorlake.Snapshot != "snap-tl" || cfg.Tensorlake.OrganizationID != "org-tl" || cfg.Tensorlake.ProjectID != "proj-tl" || cfg.Tensorlake.Namespace != "ns-tl" || cfg.Tensorlake.Workdir != "/workspace/crabbox-test" || cfg.Tensorlake.CPUs != 4 || cfg.Tensorlake.MemoryMB != 8192 || cfg.Tensorlake.DiskMB != 30000 || cfg.Tensorlake.TimeoutSecs != 1800 || !cfg.Tensorlake.NoInternet {
		t.Fatalf("tensorlake config not loaded: %#v", cfg.Tensorlake)
	}
	if cfg.Cua.APIURL != "" || cfg.Cua.Image != "ubuntu:cua-file" || cfg.Cua.Kind != "vm" || cfg.Cua.Region != "us-west" || cfg.Cua.Workdir != "/workspace/cua-test" || cfg.Cua.VCPUs != 4 || cfg.Cua.MemoryMB != 8192 || cfg.Cua.DiskGB != 40 || cfg.Cua.StartupTimeoutSecs != 300 || cfg.Cua.ExecTimeoutSecs != 900 || cfg.Cua.BridgeCommand != "python3.12" || cfg.Cua.SDKPackage != "cua" || cfg.Cua.SDKImport != "cua" || cfg.Cua.SDKFallbackImport != "cua_sandbox" {
		t.Fatalf("cua config not loaded safely: %#v", cfg.Cua)
	}
	if cfg.OpenComputer.APIURL != "" || cfg.OpenComputer.Workdir != "/workspace/oc-test" || cfg.OpenComputer.CPU != 8 || cfg.OpenComputer.MemoryMB != 16384 || cfg.OpenComputer.TimeoutSecs != 600 || cfg.OpenComputer.ExecTimeoutSecs != 7200 {
		t.Fatalf("opencomputer config not loaded: %#v", cfg.OpenComputer)
	}
	if cfg.OpenSandbox.APIURL != "" || cfg.OpenSandbox.Image != "docker.io/library/python:3.12" || cfg.OpenSandbox.Workdir != "/workspace/osb-test" || cfg.OpenSandbox.CPU != "2" || cfg.OpenSandbox.Memory != "4Gi" || cfg.OpenSandbox.TimeoutSecs != 900 || cfg.OpenSandbox.ExecTimeoutSecs != 1800 || cfg.OpenSandbox.PlatformOS != "linux" || cfg.OpenSandbox.PlatformArch != "arm64" || !cfg.OpenSandbox.SecureAccess || !cfg.OpenSandbox.UseServerProxy {
		t.Fatalf("opensandbox config not loaded safely: %#v", cfg.OpenSandbox)
	}
	if cfg.Superserve.BaseURL != "https://superserve-file-ignored.example.test" || cfg.Superserve.Template != "superserve/custom" || cfg.Superserve.Snapshot != "snap-file" || cfg.Superserve.Workdir != "/workspace/ss-test" || cfg.Superserve.TimeoutSecs != 777 || cfg.Superserve.ExecTimeoutSecs != 888 || !cfg.Superserve.ForgetMissing {
		t.Fatalf("superserve config not loaded safely: %#v", cfg.Superserve)
	}
	if len(cfg.Superserve.NetworkAllowOut) != 2 || cfg.Superserve.NetworkAllowOut[0] != "api.example.test" || cfg.Superserve.NetworkAllowOut[1] != "pkg.example.test" || len(cfg.Superserve.NetworkDenyOut) != 1 || cfg.Superserve.NetworkDenyOut[0] != "169.254.169.254/32" {
		t.Fatalf("superserve network config not normalized: %#v", cfg.Superserve)
	}
	if cfg.Cloudflare.APIURL != "https://cloudflare.example.test" || cfg.Cloudflare.Token != "cloudflare-token" || cfg.Cloudflare.Workdir != "/workspace/cf-test" {
		t.Fatalf("cloudflare config not loaded: %#v", cfg.Cloudflare)
	}
	if cfg.Proxmox.APIURL != "https://pve.example.test:8006" || cfg.Proxmox.TokenID != "crabbox@pve!test" || cfg.Proxmox.TokenSecret != "proxmox-secret" || cfg.Proxmox.Node != "pve1" || cfg.Proxmox.TemplateID != 9000 || cfg.Proxmox.Storage != "local-lvm" || cfg.Proxmox.Pool != "crabbox" || cfg.Proxmox.Bridge != "vmbr1" || cfg.Proxmox.User != "runner" || cfg.Proxmox.WorkRoot != "/work/proxmox" || cfg.Proxmox.FullClone || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("proxmox config not loaded: %#v", cfg.Proxmox)
	}
	if cfg.XCPNg.APIURL != "https://xcp-ng.example.test" || cfg.XCPNg.Username != "root" || cfg.XCPNg.Password != "xcp-ng-secret" || cfg.XCPNg.Template != "ubuntu-template" || cfg.XCPNg.TemplateUUID != "tpl-0001" || cfg.XCPNg.SR != "default-sr" || cfg.XCPNg.SRUUID != "sr-0001" || cfg.XCPNg.Network != "pool-network" || cfg.XCPNg.NetworkUUID != "net-0001" || cfg.XCPNg.Host != "host-0001" || cfg.XCPNg.User != "runner" || cfg.XCPNg.WorkRoot != "/work/xcp-ng" || !cfg.XCPNg.InsecureTLS {
		t.Fatalf("xcpNg config not loaded: %#v", cfg.XCPNg)
	}
	if cfg.Semaphore.Host != "semaphore.example.test" || cfg.Semaphore.Token != "semaphore-token" || cfg.Semaphore.Project != "crabbox" || cfg.Semaphore.Machine != "f1-standard-4" || cfg.Semaphore.OSImage != "ubuntu2404" || cfg.Semaphore.IdleTimeout != "15m" {
		t.Fatalf("semaphore config not loaded: %#v", cfg.Semaphore)
	}
	if cfg.Sprites.APIURL != "https://api.sprites.example.test" || cfg.Sprites.WorkRoot != "/home/sprite/test" {
		t.Fatalf("sprites config not loaded: %#v", cfg.Sprites)
	}
	if cfg.Static.Host != "win-dev.local" || cfg.Static.User != "peter" || cfg.Static.Port != "22" || cfg.Static.WorkRoot != "/home/peter/crabbox" {
		t.Fatalf("static config not loaded: static=%#v", cfg.Static)
	}
	if cfg.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("static work root leaked into active provider: workRoot=%s", cfg.WorkRoot)
	}
	if len(cfg.Results.JUnit) != 1 || cfg.Results.JUnit[0] != "junit.xml" || !cfg.Results.Auto || !cfg.Results.FailOnFailures {
		t.Fatalf("results config not loaded: %#v", cfg.Results)
	}
	if cfg.Shard.MaxCount != 12 {
		t.Fatalf("shard config not loaded: %#v", cfg.Shard)
	}
	if len(cfg.Run.PreflightTools) != 2 || cfg.Run.PreflightTools[0] != "node" || cfg.Run.PreflightTools[1] != "bun" {
		t.Fatalf("run config not loaded: %#v", cfg.Run)
	}
	if !cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || !cfg.Cache.Git || cfg.Cache.MaxGB != 120 || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("cache config not loaded: %#v", cfg.Cache)
	}
	if len(cfg.Cache.Volumes) != 1 || cfg.Cache.Volumes[0].Name != "pnpm-store" || cfg.Cache.Volumes[0].Key != "my-app-linux-amd64-node24-pnpm10-lock" || cfg.Cache.Volumes[0].Path != "/var/cache/crabbox/pnpm" || cfg.Cache.Volumes[0].SizeGB != 80 || !cfg.Cache.Volumes[0].Required {
		t.Fatalf("cache volumes config not loaded: %#v", cfg.Cache.Volumes)
	}
}

func TestNormalizeBrokerConfig(t *testing.T) {
	t.Run("defaults to managed", func(t *testing.T) {
		cfg := Config{}
		if err := normalizeBrokerConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.BrokerMode != BrokerModeManaged {
			t.Fatalf("mode=%q", cfg.BrokerMode)
		}
	})
	t.Run("registered requires coordinator", func(t *testing.T) {
		cfg := Config{BrokerMode: BrokerModeRegistered}
		if err := normalizeBrokerConfig(&cfg); err == nil || !strings.Contains(err.Error(), "requires broker.url") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("rejects unknown mode", func(t *testing.T) {
		cfg := Config{BrokerMode: "mirror"}
		if err := normalizeBrokerConfig(&cfg); err == nil || !strings.Contains(err.Error(), "managed or registered") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestLoadConfigExeDevWorkRootDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		want    string
		wantExe string
	}{
		"default": {
			body:    "provider: exe-dev\n",
			want:    "/tmp/crabbox",
			wantExe: "/tmp/crabbox",
		},
		"top-level": {
			body:    "provider: exe-dev\nworkRoot: /custom/crabbox\n",
			want:    "/custom/crabbox",
			wantExe: "/custom/crabbox",
		},
		"provider-specific": {
			body:    "provider: exe-dev\nworkRoot: /custom/crabbox\nexeDev:\n  workRoot: /exe/crabbox\n",
			want:    "/exe/crabbox",
			wantExe: "/exe/crabbox",
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			home := t.TempDir()
			path := filepath.Join(home, "config.yaml")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("CRABBOX_CONFIG", path)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkRoot != tc.want || cfg.ExeDev.WorkRoot != tc.wantExe {
				t.Fatalf("workRoot=%q exeDev.workRoot=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot)
			}
		})
	}
}

func TestLoadConfigRoutesAzureBackendToDynamicSessions(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
provider: azure
azure:
  backend: dynamic-sessions
azureDynamicSessions:
  endpoint: http://127.0.0.1:8787/
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.AzureBackend != AzureBackendDynamicSessions || cfg.ServerType != "" {
		t.Fatalf("provider=%q azureBackend=%q serverType=%q", cfg.Provider, cfg.AzureBackend, cfg.ServerType)
	}
}

func TestLoadConfigRoutesAzureBackendFromEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
	t.Setenv("CRABBOX_PROVIDER", "azure")
	t.Setenv("CRABBOX_AZURE_BACKEND", "azds")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.AzureBackend != AzureBackendDynamicSessions {
		t.Fatalf("provider=%q azureBackend=%q", cfg.Provider, cfg.AzureBackend)
	}
}

func TestLoadConfigMXCCapabilityEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
	t.Setenv("CRABBOX_MXC_ALLOW_DACL_MUTATION", "true")
	t.Setenv("CRABBOX_MXC_ALLOW_WINDOWS_UI", "true")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MXC.AllowDACLMutation || !cfg.MXC.AllowWindowsUI {
		t.Fatalf("mxc=%+v", cfg.MXC)
	}
}

func TestLoadConfigTailscaleBlock(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`provider: aws
network: public
tailscale:
  enabled: true
  network: tailscale
  tags:
    - tag:crabbox
    - tag:ci
  hostnameTemplate: cbx-{slug}
  authKeyEnv: TEST_TS_AUTH_KEY
  exitNode: mac-studio.tailnet.ts.net
  exitNodeAllowLanAccess: true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tailscale.Enabled || cfg.Network != NetworkTailscale || cfg.Tailscale.HostnameTemplate != "cbx-{slug}" || cfg.Tailscale.AuthKeyEnv != "TEST_TS_AUTH_KEY" || cfg.Tailscale.ExitNode != "mac-studio.tailnet.ts.net" || !cfg.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("tailscale config not loaded: network=%s tailscale=%#v", cfg.Network, cfg.Tailscale)
	}
	if len(cfg.Tailscale.Tags) != 2 || cfg.Tailscale.Tags[1] != "tag:ci" {
		t.Fatalf("tailscale tags not loaded: %#v", cfg.Tailscale.Tags)
	}
}

func TestEnvOverridesConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "fast")
	t.Setenv("CRABBOX_SERVER_TYPE", "cx22")
	t.Setenv("CRABBOX_DESKTOP", "true")
	t.Setenv("CRABBOX_BROWSER", "true")
	t.Setenv("CRABBOX_CODE", "true")
	t.Setenv("CRABBOX_TTL", "3h")
	t.Setenv("CRABBOX_IDLE_TIMEOUT", "20m")
	t.Setenv("CRABBOX_AWS_SSH_CIDRS", "198.51.100.7/32,203.0.113.8/32")
	t.Setenv("CRABBOX_AZURE_OS_DISK", "managed")
	t.Setenv("CRABBOX_AZURE_SSH_CIDRS", "198.51.100.9/32,203.0.113.10/32")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT", "https://env-pool.env.westus.azurecontainerapps.io")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL", "env-pool")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION", "2025-02-02-preview")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR", "/workspace/env")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS", "90")
	t.Setenv("CRABBOX_GCP_PROJECT", "crabbox-project")
	t.Setenv("CRABBOX_GCP_ZONE", "europe-west2-b")
	t.Setenv("CRABBOX_GCP_IMAGE", "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64")
	t.Setenv("CRABBOX_GCP_NETWORK", "crabbox-net")
	t.Setenv("CRABBOX_GCP_SUBNET", "crabbox-subnet")
	t.Setenv("CRABBOX_GCP_TAGS", "crabbox-ssh,crabbox-ci")
	t.Setenv("CRABBOX_GCP_SSH_CIDRS", "198.51.100.11/32,203.0.113.12/32")
	t.Setenv("CRABBOX_GCP_ROOT_GB", "900")
	t.Setenv("CRABBOX_GCP_SERVICE_ACCOUNT", "runner@crabbox-project.iam.gserviceaccount.com")
	t.Setenv("CRABBOX_SSH_FALLBACK_PORTS", "none")
	t.Setenv("CRABBOX_ACCESS_CLIENT_ID", "env-access-client")
	t.Setenv("CRABBOX_ACCESS_CLIENT_SECRET", "env-access-secret")
	t.Setenv("CRABBOX_ACCESS_TOKEN", "env-access-jwt")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "env-admin-secret")
	t.Setenv("CRABBOX_HOST_ID", "h-neutral-env")
	t.Setenv("CRABBOX_NETWORK", "public")
	t.Setenv("CRABBOX_CAPACITY_HINTS", "false")
	t.Setenv("CRABBOX_CAPACITY_REGIONS", "eu-west-1,us-east-1")
	t.Setenv("CRABBOX_CAPACITY_AVAILABILITY_ZONES", "eu-west-1a,eu-west-1b")
	t.Setenv("CRABBOX_TAILSCALE_TAGS", "tag:crabbox,tag:ci")
	t.Setenv("CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE", "lease-{id}")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-secret")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE", "mac-studio.tailnet.ts.net")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS", "1")
	t.Setenv("CRABBOX_TARGET", "macos")
	t.Setenv("CRABBOX_STATIC_HOST", "mac.local")
	t.Setenv("MORPH_API_KEY", "morph-api-file")
	t.Setenv("CRABBOX_MORPH_API_KEY", "morph-api-env")
	t.Setenv("CRABBOX_MORPH_API_URL", "https://morph-env.example")
	t.Setenv("CRABBOX_MORPH_SNAPSHOT", "snapshot-env")
	t.Setenv("CRABBOX_MORPH_SSH_GATEWAY_HOST", "ssh.morph-env.example")
	t.Setenv("CRABBOX_MORPH_WORK_ROOT", "/tmp/morph-env")
	t.Setenv("CRABBOX_MORPH_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_MORPH_WAKE_ON_SSH", "false")
	t.Setenv("DAYTONA_API_KEY", "daytona-api-file")
	t.Setenv("CRABBOX_DAYTONA_API_KEY", "daytona-api-env")
	t.Setenv("DAYTONA_API_URL", "https://daytona-file.example/api")
	t.Setenv("CRABBOX_DAYTONA_API_URL", "https://daytona-env.example/api")
	t.Setenv("DAYTONA_SNAPSHOT", "snapshot-file")
	t.Setenv("CRABBOX_DAYTONA_SNAPSHOT", "snapshot-env")
	t.Setenv("DAYTONA_TARGET", "target-file")
	t.Setenv("CRABBOX_DAYTONA_TARGET", "target-env")
	t.Setenv("CRABBOX_DAYTONA_USER", "daytona-env-user")
	t.Setenv("CRABBOX_DAYTONA_WORK_ROOT", "/home/daytona/env")
	t.Setenv("CRABBOX_DAYTONA_SSH_GATEWAY_HOST", "ssh.env.example")
	t.Setenv("CRABBOX_DAYTONA_SSH_ACCESS_MINUTES", "44")
	t.Setenv("E2B_API_KEY", "e2b-api-file")
	t.Setenv("CRABBOX_E2B_API_KEY", "e2b-api-env")
	t.Setenv("E2B_API_URL", "https://api.e2b-file.example")
	t.Setenv("CRABBOX_E2B_API_URL", "https://api.e2b-env.example")
	t.Setenv("E2B_DOMAIN", "e2b-file.example")
	t.Setenv("CRABBOX_E2B_DOMAIN", "e2b-env.example")
	t.Setenv("CRABBOX_E2B_TEMPLATE", "template-env")
	t.Setenv("CRABBOX_E2B_WORKDIR", "env-workdir")
	t.Setenv("CRABBOX_E2B_USER", "sandbox-env")
	t.Setenv("RAILWAY_API_TOKEN", "railway-token-file")
	t.Setenv("CRABBOX_RAILWAY_API_TOKEN", "railway-token-env")
	t.Setenv("RAILWAY_API_URL", "https://railway-file.example/graphql/v2")
	t.Setenv("CRABBOX_RAILWAY_API_URL", "https://railway-env.example/graphql/v2")
	t.Setenv("RAILWAY_PROJECT_ID", "railway-project-file")
	t.Setenv("CRABBOX_RAILWAY_PROJECT_ID", "railway-project-env")
	t.Setenv("RAILWAY_ENVIRONMENT_ID", "railway-environment-file")
	t.Setenv("CRABBOX_RAILWAY_ENVIRONMENT_ID", "railway-environment-env")
	t.Setenv("FASTAPI_CLOUD_TOKEN", "fastapi-token-file")
	t.Setenv("CRABBOX_FASTAPI_CLOUD_TOKEN", "fastapi-token-env")
	t.Setenv("FASTAPI_CLOUD_API_URL", "https://fastapi-file.example/api/v1")
	t.Setenv("CRABBOX_FASTAPI_CLOUD_API_URL", "https://fastapi-env.example/api/v1")
	t.Setenv("FASTAPI_CLOUD_APP_ID", "fastapi-app-file")
	t.Setenv("CRABBOX_FASTAPI_CLOUD_APP_ID", "fastapi-app-env")
	t.Setenv("FASTAPI_CLOUD_TEAM_ID", "fastapi-team-file")
	t.Setenv("CRABBOX_FASTAPI_CLOUD_TEAM_ID", "fastapi-team-env")
	t.Setenv("RUNPOD_API_KEY", "runpod-key-file")
	t.Setenv("CRABBOX_RUNPOD_API_KEY", "runpod-key-env")
	t.Setenv("RUNPOD_API_URL", "https://runpod-file.example/v1")
	t.Setenv("CRABBOX_RUNPOD_API_URL", "https://runpod-env.example/v1")
	t.Setenv("RUNPOD_CLOUD_TYPE", "COMMUNITY")
	t.Setenv("CRABBOX_RUNPOD_CLOUD_TYPE", "SECURE")
	t.Setenv("RUNPOD_INSTANCE_ID", "NVIDIA RTX A4000")
	t.Setenv("CRABBOX_RUNPOD_INSTANCE_ID", "NVIDIA L4")
	t.Setenv("RUNPOD_IMAGE", "runpod/pytorch:file")
	t.Setenv("CRABBOX_RUNPOD_IMAGE", "runpod/pytorch:env")
	t.Setenv("RUNPOD_TEMPLATE_ID", "tpl-file")
	t.Setenv("CRABBOX_RUNPOD_TEMPLATE_ID", "tpl-env")
	t.Setenv("CRABBOX_RUNPOD_DISK_GB", "30")
	t.Setenv("CRABBOX_RUNPOD_USER", "runpod-env-user")
	t.Setenv("CRABBOX_RUNPOD_WORK_ROOT", "/work/runpod-env")
	t.Setenv("VAST_API_KEY", "vast-key-file")
	t.Setenv("CRABBOX_VAST_API_KEY", "vast-key-env")
	t.Setenv("VAST_API_URL", "https://vast-file.example/api/v0")
	t.Setenv("CRABBOX_VAST_API_URL", "https://vast-env.example/api/v0")
	t.Setenv("CRABBOX_VAST_INSTANCE_TYPE", "interruptible")
	t.Setenv("CRABBOX_VAST_GPU_NAME", "H100")
	t.Setenv("CRABBOX_VAST_GPU_COUNT", "4")
	t.Setenv("CRABBOX_VAST_IMAGE", "nvidia/cuda:vast-env")
	t.Setenv("CRABBOX_VAST_TEMPLATE_ID", "vast-tpl-env")
	t.Setenv("CRABBOX_VAST_RUNTYPE", "ssh_direct")
	t.Setenv("CRABBOX_VAST_DISK_GB", "80")
	t.Setenv("CRABBOX_VAST_MAX_DPH_TOTAL", "4.25")
	t.Setenv("CRABBOX_VAST_MIN_RELIABILITY", "0.95")
	t.Setenv("CRABBOX_VAST_ORDER", "dlperf desc")
	t.Setenv("CRABBOX_VAST_USER", "ubuntu")
	t.Setenv("CRABBOX_VAST_WORK_ROOT", "/work/vast-env")
	t.Setenv("CRABBOX_VAST_RELEASE_ACTION", "keep")
	t.Setenv("ISLO_API_KEY", "islo-api-file")
	t.Setenv("CRABBOX_ISLO_API_KEY", "islo-api-env")
	t.Setenv("ISLO_BASE_URL", "https://islo-file.example")
	t.Setenv("CRABBOX_ISLO_BASE_URL", "https://islo-env.example")
	t.Setenv("CRABBOX_ISLO_IMAGE", "ubuntu:env")
	t.Setenv("CRABBOX_ISLO_WORKDIR", "env-workdir")
	t.Setenv("CRABBOX_ISLO_GATEWAY_PROFILE", "env-gateway")
	t.Setenv("CRABBOX_ISLO_SNAPSHOT_NAME", "env-snapshot")
	t.Setenv("CRABBOX_ISLO_VCPUS", "8")
	t.Setenv("CRABBOX_ISLO_MEMORY_MB", "16384")
	t.Setenv("CRABBOX_ISLO_DISK_GB", "80")
	t.Setenv("FREESTYLE_API_KEY", "freestyle-key-file")
	t.Setenv("CRABBOX_FREESTYLE_API_KEY", "freestyle-key-env")
	t.Setenv("FREESTYLE_API_URL", "https://freestyle-file.example")
	t.Setenv("CRABBOX_FREESTYLE_API_URL", "https://freestyle-env.example")
	t.Setenv("CRABBOX_FREESTYLE_WORKDIR", "env/repo")
	t.Setenv("CRABBOX_FREESTYLE_VCPUS", "6")
	t.Setenv("CRABBOX_FREESTYLE_MEMORY_GB", "16")
	t.Setenv("TENKI_CLI", "/usr/bin/tenki-file")
	t.Setenv("CRABBOX_TENKI_CLI", "/opt/tenki/bin/tenki")
	t.Setenv("TENKI_ENDPOINT", "https://api.tenki-file.example")
	t.Setenv("CRABBOX_TENKI_ENDPOINT", "https://api.tenki-env.example")
	t.Setenv("TENKI_GATEWAY", "wss://gateway.tenki-file.example")
	t.Setenv("CRABBOX_TENKI_GATEWAY", "wss://gateway.tenki-env.example")
	t.Setenv("CRABBOX_TENKI_WORKSPACE", "ws_env")
	t.Setenv("CRABBOX_TENKI_PROJECT", "proj_env")
	t.Setenv("CRABBOX_TENKI_IMAGE", "ubuntu:tenki-env")
	t.Setenv("CRABBOX_TENKI_SNAPSHOT", "snap-env")
	t.Setenv("CRABBOX_TENKI_WORK_ROOT", "/home/tenki/env")
	t.Setenv("CRABBOX_TENKI_CPUS", "8")
	t.Setenv("CRABBOX_TENKI_MEMORY_MB", "16384")
	t.Setenv("CRABBOX_TENKI_DISK_GB", "80")
	t.Setenv("TENSORLAKE_API_KEY", "tl-api-file")
	t.Setenv("CRABBOX_TENSORLAKE_API_KEY", "tl-api-env")
	t.Setenv("TENSORLAKE_API_URL", "https://api.tl-file.example")
	t.Setenv("CRABBOX_TENSORLAKE_API_URL", "https://api.tl-env.example")
	t.Setenv("CRABBOX_TENSORLAKE_CLI", "/opt/tl/bin/tensorlake")
	t.Setenv("CRABBOX_TENSORLAKE_IMAGE", "ubuntu:tl-env")
	t.Setenv("CRABBOX_TENSORLAKE_SNAPSHOT", "snap-tl-env")
	t.Setenv("TENSORLAKE_ORGANIZATION_ID", "org-tl-file")
	t.Setenv("CRABBOX_TENSORLAKE_ORGANIZATION_ID", "org-tl-env")
	t.Setenv("TENSORLAKE_PROJECT_ID", "proj-tl-file")
	t.Setenv("CRABBOX_TENSORLAKE_PROJECT_ID", "proj-tl-env")
	t.Setenv("INDEXIFY_NAMESPACE", "ns-tl-file")
	t.Setenv("CRABBOX_TENSORLAKE_NAMESPACE", "ns-tl-env")
	t.Setenv("CRABBOX_TENSORLAKE_WORKDIR", "/workspace/tl-env")
	t.Setenv("CRABBOX_TENSORLAKE_CPUS", "2.5")
	t.Setenv("CRABBOX_TENSORLAKE_MEMORY_MB", "4096")
	t.Setenv("CRABBOX_TENSORLAKE_DISK_MB", "20480")
	t.Setenv("CRABBOX_TENSORLAKE_TIMEOUT_SECS", "900")
	t.Setenv("CRABBOX_TENSORLAKE_NO_INTERNET", "true")
	t.Setenv("CUA_BASE_URL", "https://cua-file.example")
	t.Setenv("CRABBOX_CUA_API_URL", "https://cua-env.example")
	t.Setenv("CRABBOX_CUA_IMAGE", "ubuntu:cua-env")
	t.Setenv("CRABBOX_CUA_KIND", "vm")
	t.Setenv("CRABBOX_CUA_REGION", "us-central")
	t.Setenv("CRABBOX_CUA_WORKDIR", "/workspace/cua-env")
	t.Setenv("CRABBOX_CUA_VCPUS", "6")
	t.Setenv("CRABBOX_CUA_MEMORY_MB", "12288")
	t.Setenv("CRABBOX_CUA_DISK_GB", "80")
	t.Setenv("CRABBOX_CUA_STARTUP_TIMEOUT_SECS", "240")
	t.Setenv("CRABBOX_CUA_EXEC_TIMEOUT_SECS", "1200")
	t.Setenv("CRABBOX_CUA_BRIDGE_COMMAND", "python3.12")
	t.Setenv("CRABBOX_CUA_SDK_PACKAGE", "cua")
	t.Setenv("CRABBOX_CUA_SDK_IMPORT", "cua")
	t.Setenv("CRABBOX_CUA_SDK_FALLBACK_IMPORT", "cua_sandbox")
	t.Setenv("OPENCOMPUTER_API_URL", "https://oc-file.example")
	t.Setenv("CRABBOX_OPENCOMPUTER_API_URL", "https://oc-env.example")
	t.Setenv("CRABBOX_OPENCOMPUTER_WORKDIR", "/workspace/oc-env")
	t.Setenv("CRABBOX_OPENCOMPUTER_CPU", "6")
	t.Setenv("CRABBOX_OPENCOMPUTER_MEMORY_MB", "12288")
	t.Setenv("CRABBOX_OPENCOMPUTER_TIMEOUT_SECS", "1200")
	t.Setenv("CRABBOX_OPENCOMPUTER_EXEC_TIMEOUT_SECS", "2400")
	t.Setenv("OPEN_SANDBOX_API_URL", "https://opensandbox-file.example")
	t.Setenv("CRABBOX_OPENSANDBOX_API_URL", "https://opensandbox-env.example")
	t.Setenv("CRABBOX_OPENSANDBOX_IMAGE", "ubuntu:osb-env")
	t.Setenv("CRABBOX_OPENSANDBOX_WORKDIR", "/workspace/osb-env")
	t.Setenv("CRABBOX_OPENSANDBOX_CPU", "750m")
	t.Setenv("CRABBOX_OPENSANDBOX_MEMORY", "1536Mi")
	t.Setenv("CRABBOX_OPENSANDBOX_TIMEOUT_SECS", "123")
	t.Setenv("CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS", "456")
	t.Setenv("CRABBOX_OPENSANDBOX_PLATFORM_OS", "linux")
	t.Setenv("CRABBOX_OPENSANDBOX_PLATFORM_ARCH", "amd64")
	t.Setenv("CRABBOX_OPENSANDBOX_SECURE_ACCESS", "true")
	t.Setenv("CRABBOX_OPENSANDBOX_USE_SERVER_PROXY", "true")
	t.Setenv("SUPERSERVE_BASE_URL", "https://superserve-file.example")
	t.Setenv("CRABBOX_SUPERSERVE_BASE_URL", "https://superserve-env.example")
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "superserve-key-ignored-by-config")
	t.Setenv("SUPERSERVE_API_KEY", "superserve-vendor-key-ignored-by-config")
	t.Setenv("CRABBOX_SUPERSERVE_TEMPLATE", "superserve/env-template")
	t.Setenv("CRABBOX_SUPERSERVE_SNAPSHOT", "snap-env")
	t.Setenv("CRABBOX_SUPERSERVE_WORKDIR", "/workspace/ss-env")
	t.Setenv("CRABBOX_SUPERSERVE_TIMEOUT_SECS", "321")
	t.Setenv("CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS", "654")
	t.Setenv("CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT", "api.env.example, ,pkg.env.example")
	t.Setenv("CRABBOX_SUPERSERVE_NETWORK_DENY_OUT", "169.254.169.254/32")
	t.Setenv("CRABBOX_SUPERSERVE_FORGET_MISSING", "true")
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_URL", "https://cloudflare-env.example")
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", "cloudflare-env-token")
	t.Setenv("CRABBOX_CLOUDFLARE_WORKDIR", "/workspace/cloudflare-env")
	t.Setenv("CRABBOX_PROXMOX_API_URL", "https://pve-env.example:8006")
	t.Setenv("CRABBOX_PROXMOX_TOKEN_ID", "runner@pve!env")
	t.Setenv("CRABBOX_PROXMOX_TOKEN_SECRET", "proxmox-env-secret")
	t.Setenv("CRABBOX_PROXMOX_NODE", "pve-env")
	t.Setenv("CRABBOX_PROXMOX_TEMPLATE_ID", "9100")
	t.Setenv("CRABBOX_PROXMOX_STORAGE", "ceph-env")
	t.Setenv("CRABBOX_PROXMOX_POOL", "pool-env")
	t.Setenv("CRABBOX_PROXMOX_BRIDGE", "vmbr2")
	t.Setenv("CRABBOX_PROXMOX_USER", "runner-env")
	t.Setenv("CRABBOX_PROXMOX_WORK_ROOT", "/work/proxmox-env")
	t.Setenv("CRABBOX_PROXMOX_FULL_CLONE", "false")
	t.Setenv("CRABBOX_PROXMOX_INSECURE_TLS", "true")
	t.Setenv("CRABBOX_XCP_NG_API_URL", "https://xcp-ng-env.example.test")
	t.Setenv("CRABBOX_XCP_NG_USERNAME", "root-env")
	t.Setenv("CRABBOX_XCP_NG_PASSWORD", "xcp-ng-env-secret")
	t.Setenv("CRABBOX_XCP_NG_TEMPLATE", "template-env")
	t.Setenv("CRABBOX_XCP_NG_TEMPLATE_UUID", "tpl-env")
	t.Setenv("CRABBOX_XCP_NG_SR", "sr-env")
	t.Setenv("CRABBOX_XCP_NG_SR_UUID", "sr-uuid-env")
	t.Setenv("CRABBOX_XCP_NG_NETWORK", "network-env")
	t.Setenv("CRABBOX_XCP_NG_NETWORK_UUID", "network-uuid-env")
	t.Setenv("CRABBOX_XCP_NG_HOST", "host-env")
	t.Setenv("CRABBOX_XCP_NG_USER", "runner-xcp-env")
	t.Setenv("CRABBOX_XCP_NG_WORK_ROOT", "/work/xcp-ng-env")
	t.Setenv("CRABBOX_XCP_NG_INSECURE_TLS", "true")
	t.Setenv("SEMAPHORE_HOST", "semaphore-file.example.test")
	t.Setenv("CRABBOX_SEMAPHORE_HOST", "semaphore-env.example.test")
	t.Setenv("SEMAPHORE_API_TOKEN", "semaphore-token-file")
	t.Setenv("CRABBOX_SEMAPHORE_TOKEN", "semaphore-token-env")
	t.Setenv("SEMAPHORE_PROJECT", "semaphore-project-file")
	t.Setenv("CRABBOX_SEMAPHORE_PROJECT", "semaphore-project-env")
	t.Setenv("CRABBOX_SEMAPHORE_MACHINE", "f1-standard-env")
	t.Setenv("CRABBOX_SEMAPHORE_OS_IMAGE", "ubuntu-env")
	t.Setenv("CRABBOX_SEMAPHORE_IDLE_TIMEOUT", "22m")
	t.Setenv("SPRITE_TOKEN", "sprite-token-file")
	t.Setenv("SETUP_SPRITE_TOKEN", "setup-sprite-token-file")
	t.Setenv("SPRITES_TOKEN", "sprites-token-file")
	t.Setenv("CRABBOX_SPRITES_TOKEN", "sprites-token-env")
	t.Setenv("SPRITES_API_URL", "https://api.sprites-file.example")
	t.Setenv("CRABBOX_SPRITES_API_URL", "https://api.sprites-env.example")
	t.Setenv("CRABBOX_SPRITES_WORK_ROOT", "/home/sprite/env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_RUNTIME", "docker")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_IMAGE", "ubuntu:env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_USER", "runner-env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_WORK_ROOT", "/workspace/env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_CPUS", "6")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_MEMORY", "12g")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_NETWORK", "bridge")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET", "true")
	t.Setenv("CRABBOX_NAMESPACE_IMAGE", "namespace-env-image")
	t.Setenv("CRABBOX_NAMESPACE_SIZE", "XL")
	t.Setenv("CRABBOX_NAMESPACE_REPOSITORY", "github.com/openclaw/env")
	t.Setenv("CRABBOX_NAMESPACE_SITE", "iad1")
	t.Setenv("CRABBOX_NAMESPACE_VOLUME_SIZE_GB", "300")
	t.Setenv("CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT", "4h")
	t.Setenv("CRABBOX_NAMESPACE_WORK_ROOT", "/workspaces/env")
	t.Setenv("CRABBOX_NAMESPACE_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_CODER_CLI", "/opt/coder/bin/coder")
	t.Setenv("CRABBOX_CODER_TEMPLATE", "python-dev")
	t.Setenv("CRABBOX_CODER_PRESET", "gpu")
	t.Setenv("CRABBOX_CODER_WORKSPACE_PREFIX", "env-")
	t.Setenv("CRABBOX_CODER_WORK_ROOT", "/home/coder/env")
	t.Setenv("CRABBOX_CODER_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_CODER_WAIT", "no")
	t.Setenv("CRABBOX_CODER_USE_PARAMETER_DEFAULTS", "true")
	t.Setenv("CRABBOX_CODER_PARAMETERS", "region=sfo,size=xl")
	t.Setenv("CRABBOX_CODER_RICH_PARAMETER_FILE", "~/coder-rich.yaml")
	t.Setenv("CRABBOX_BLACKSMITH_IDLE_TIMEOUT", "2h")
	t.Setenv("CRABBOX_BLACKSMITH_DEBUG", "true")
	t.Setenv("CRABBOX_ACTIONS_RUNNER_LABELS", "crabbox,linux-large")
	t.Setenv("CRABBOX_ACTIONS_EPHEMERAL", "false")
	t.Setenv("CRABBOX_RESULTS_JUNIT", "junit.xml,build/test.xml")
	t.Setenv("CRABBOX_RESULTS_AUTO", "true")
	t.Setenv("CRABBOX_RESULTS_FAIL_ON_FAILURES", "true")
	t.Setenv("CRABBOX_CACHE_PNPM", "false")
	t.Setenv("CRABBOX_CACHE_NPM", "false")
	t.Setenv("CRABBOX_CACHE_DOCKER", "true")
	t.Setenv("CRABBOX_CACHE_GIT", "false")
	t.Setenv("CRABBOX_CACHE_PURGE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_CACHE_VOLUMES", "pnpm=env-pnpm:/var/cache/crabbox/pnpm,npm-cache:/var/cache/crabbox/npm")
	t.Setenv("CRABBOX_SYNC_CHECKSUM", "true")
	t.Setenv("CRABBOX_SYNC_DELETE", "false")
	t.Setenv("CRABBOX_SYNC_GIT_SEED", "false")
	t.Setenv("CRABBOX_SYNC_FINGERPRINT", "false")
	t.Setenv("CRABBOX_SYNC_TIMEOUT", "45m")
	t.Setenv("CRABBOX_SYNC_ALLOW_LARGE", "true")
	t.Setenv("CRABBOX_ENV_ALLOW", "CI,NODE_OPTIONS,CUSTOM_*")
	t.Setenv("CRABBOX_PREFLIGHT_TOOLS", "node,bun,docker")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: aws\nclass: beast\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "hetzner" || cfg.Class != "fast" || cfg.ServerType != "cx22" || !cfg.ServerTypeExplicit || cfg.TTL.String() != "3h0m0s" || cfg.IdleTimeout.String() != "20m0s" {
		t.Fatalf("unexpected config: provider=%s class=%s type=%s ttl=%s idle=%s", cfg.Provider, cfg.Class, cfg.ServerType, cfg.TTL, cfg.IdleTimeout)
	}
	if !cfg.Desktop || !cfg.Browser || !cfg.Code {
		t.Fatalf("capability env not loaded: desktop=%t browser=%t code=%t", cfg.Desktop, cfg.Browser, cfg.Code)
	}
	if len(cfg.AWSSSHCIDRs) != 2 || cfg.AWSSSHCIDRs[0] != "198.51.100.7/32" || cfg.AWSSSHCIDRs[1] != "203.0.113.8/32" {
		t.Fatalf("AWSSSHCIDRs=%v", cfg.AWSSSHCIDRs)
	}
	if len(cfg.AzureSSHCIDRs) != 2 || cfg.AzureSSHCIDRs[0] != "198.51.100.9/32" || cfg.AzureSSHCIDRs[1] != "203.0.113.10/32" {
		t.Fatalf("AzureSSHCIDRs=%v", cfg.AzureSSHCIDRs)
	}
	if cfg.AzureOSDisk != "managed" {
		t.Fatalf("AzureOSDisk=%q", cfg.AzureOSDisk)
	}
	if !cfg.AzureOSDiskExplicit {
		t.Fatal("AzureOSDiskExplicit=false, want true")
	}
	if cfg.AzureDynamicSessions.Endpoint != "https://env-pool.env.westus.azurecontainerapps.io" || cfg.AzureDynamicSessions.Pool != "env-pool" || cfg.AzureDynamicSessions.Workdir != "/workspace/env" || cfg.AzureDynamicSessions.TimeoutSecs != 90 {
		t.Fatalf("unexpected azure dynamic sessions env: %#v", cfg.AzureDynamicSessions)
	}
	if cfg.GCPProject != "crabbox-project" || cfg.GCPZone != "europe-west2-b" || cfg.GCPNetwork != "crabbox-net" || cfg.GCPSubnet != "crabbox-subnet" || cfg.GCPRootGB != 900 || cfg.GCPServiceAccount != "runner@crabbox-project.iam.gserviceaccount.com" {
		t.Fatalf("unexpected gcp env: project=%s zone=%s network=%s subnet=%s root=%d service=%s", cfg.GCPProject, cfg.GCPZone, cfg.GCPNetwork, cfg.GCPSubnet, cfg.GCPRootGB, cfg.GCPServiceAccount)
	}
	if len(cfg.GCPTags) != 2 || cfg.GCPTags[1] != "crabbox-ci" || len(cfg.GCPSSHCIDRs) != 2 || cfg.GCPSSHCIDRs[1] != "203.0.113.12/32" {
		t.Fatalf("unexpected gcp tags/cidrs: tags=%v cidrs=%v", cfg.GCPTags, cfg.GCPSSHCIDRs)
	}
	if len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("SSHFallbackPorts=%v want disabled fallback", cfg.SSHFallbackPorts)
	}
	if cfg.Access.ClientID != "env-access-client" || cfg.Access.ClientSecret != "env-access-secret" || cfg.Access.Token != "env-access-jwt" {
		t.Fatalf("unexpected access config: %#v", cfg.Access)
	}
	if cfg.CoordAdminToken != "env-admin-secret" {
		t.Fatalf("unexpected admin token state: %q", cfg.CoordAdminToken)
	}
	if cfg.HostID != "h-neutral-env" {
		t.Fatalf("unexpected host id: %q", cfg.HostID)
	}
	if cfg.TargetOS != targetMacOS || cfg.Static.Host != "mac.local" {
		t.Fatalf("unexpected target env: target=%s static=%#v", cfg.TargetOS, cfg.Static)
	}
	if cfg.Network != NetworkPublic || cfg.Tailscale.AuthKey != "tskey-secret" || cfg.Tailscale.HostnameTemplate != "lease-{id}" || cfg.Tailscale.ExitNode != "mac-studio.tailnet.ts.net" || !cfg.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("unexpected tailscale env: network=%s tailscale=%#v", cfg.Network, cfg.Tailscale)
	}
	if cfg.Capacity.Hints || len(cfg.Capacity.Regions) != 2 || len(cfg.Capacity.AvailabilityZones) != 2 {
		t.Fatalf("unexpected capacity env: %#v", cfg.Capacity)
	}
	if len(cfg.Tailscale.Tags) != 2 || cfg.Tailscale.Tags[1] != "tag:ci" {
		t.Fatalf("unexpected tailscale tags: %#v", cfg.Tailscale.Tags)
	}
	if cfg.Morph.APIKey != "morph-api-env" || cfg.Morph.APIURL != "https://morph-env.example" || cfg.Morph.Snapshot != "snapshot-env" || cfg.Morph.SSHGatewayHost != "ssh.morph-env.example" || cfg.Morph.WorkRoot != "/tmp/morph-env" || !cfg.Morph.DeleteOnRelease || cfg.Morph.WakeOnSSH {
		t.Fatalf("unexpected morph env: %#v", cfg.Morph)
	}
	if cfg.Daytona.APIKey != "daytona-api-env" || cfg.Daytona.APIURL != "https://daytona-env.example/api" || cfg.Daytona.Snapshot != "snapshot-env" || cfg.Daytona.Target != "target-env" || cfg.Daytona.User != "daytona-env-user" || cfg.Daytona.WorkRoot != "/home/daytona/env" || cfg.Daytona.SSHGatewayHost != "ssh.env.example" || cfg.Daytona.SSHAccessMinutes != 44 {
		t.Fatalf("unexpected daytona env: %#v", cfg.Daytona)
	}
	if cfg.E2B.APIKey != "e2b-api-env" || cfg.E2B.APIURL != "https://api.e2b-env.example" || cfg.E2B.Domain != "e2b-env.example" || cfg.E2B.Template != "template-env" || cfg.E2B.Workdir != "env-workdir" || cfg.E2B.User != "sandbox-env" {
		t.Fatalf("unexpected e2b env: %#v", cfg.E2B)
	}
	if cfg.Railway.APIToken != "railway-token-env" || cfg.Railway.APIURL != "https://railway-env.example/graphql/v2" || cfg.Railway.ProjectID != "railway-project-env" || cfg.Railway.EnvironmentID != "railway-environment-env" {
		t.Fatalf("unexpected railway env: %#v", cfg.Railway)
	}
	if cfg.FastAPICloud.Token != "fastapi-token-env" || cfg.FastAPICloud.APIURL != "https://fastapi-env.example/api/v1" || cfg.FastAPICloud.AppID != "fastapi-app-env" || cfg.FastAPICloud.TeamID != "fastapi-team-env" {
		t.Fatalf("unexpected fastapi-cloud env: %#v", cfg.FastAPICloud)
	}
	if cfg.Runpod.APIKey != "runpod-key-env" || cfg.Runpod.APIURL != "https://runpod-env.example/v1" || cfg.Runpod.CloudType != "SECURE" || cfg.Runpod.InstanceID != "NVIDIA L4" || cfg.Runpod.Image != "runpod/pytorch:env" || cfg.Runpod.TemplateID != "tpl-env" || cfg.Runpod.DiskGB != 30 || cfg.Runpod.User != "runpod-env-user" || cfg.Runpod.WorkRoot != "/work/runpod-env" {
		t.Fatalf("unexpected runpod env: %#v", cfg.Runpod)
	}
	if cfg.Vast.APIKey != "vast-key-env" || cfg.Vast.APIURL != "https://vast-env.example/api/v0" || cfg.Vast.InstanceType != "interruptible" || cfg.Vast.GPUName != "H100" || cfg.Vast.GPUCount != 4 || cfg.Vast.Image != "nvidia/cuda:vast-env" || cfg.Vast.TemplateID != "vast-tpl-env" || cfg.Vast.Runtype != "ssh_direct" || cfg.Vast.DiskGB != 80 || cfg.Vast.MaxDphTotal != 4.25 || cfg.Vast.MinReliability != 0.95 || cfg.Vast.Order != "dlperf desc" || cfg.Vast.User != "ubuntu" || cfg.Vast.WorkRoot != "/work/vast-env" || cfg.Vast.ReleaseAction != "keep" {
		t.Fatalf("unexpected vast env: %#v", cfg.Vast)
	}
	if cfg.Islo.APIKey != "islo-api-env" || cfg.Islo.BaseURL != "https://islo-env.example" || cfg.Islo.Image != "ubuntu:env" || cfg.Islo.Workdir != "env-workdir" || cfg.Islo.GatewayProfile != "env-gateway" || cfg.Islo.SnapshotName != "env-snapshot" || cfg.Islo.VCPUs != 8 || cfg.Islo.MemoryMB != 16384 || cfg.Islo.DiskGB != 80 {
		t.Fatalf("unexpected islo env: %#v", cfg.Islo)
	}
	if cfg.Freestyle.APIKey != "freestyle-key-env" || cfg.Freestyle.APIURL != "https://freestyle-env.example" || cfg.Freestyle.Workdir != "env/repo" || cfg.Freestyle.VCPUs != 6 || cfg.Freestyle.MemoryGB != 16 {
		t.Fatalf("unexpected freestyle env: %#v", cfg.Freestyle)
	}
	if cfg.Tenki.CLIPath != "/opt/tenki/bin/tenki" || cfg.Tenki.Endpoint != "https://api.tenki-env.example" || cfg.Tenki.Gateway != "wss://gateway.tenki-env.example" || cfg.Tenki.Workspace != "ws_env" || cfg.Tenki.Project != "proj_env" || cfg.Tenki.Image != "ubuntu:tenki-env" || cfg.Tenki.Snapshot != "snap-env" || cfg.Tenki.WorkRoot != "/home/tenki/env" || cfg.Tenki.CPUs != 8 || cfg.Tenki.MemoryMB != 16384 || cfg.Tenki.DiskGB != 80 {
		t.Fatalf("unexpected tenki env: %#v", cfg.Tenki)
	}
	if cfg.Coder.CLIPath != "/opt/coder/bin/coder" || cfg.Coder.Template != "python-dev" || cfg.Coder.Preset != "gpu" || cfg.Coder.WorkspacePrefix != "env-" || cfg.Coder.WorkRoot != "/home/coder/env" || !cfg.Coder.DeleteOnRelease || cfg.Coder.Wait != "no" || !cfg.Coder.UseParameterDefaults || len(cfg.Coder.Parameters) != 2 || cfg.Coder.Parameters[0] != "region=sfo" || cfg.Coder.RichParameterFile != filepath.Join(home, "coder-rich.yaml") {
		t.Fatalf("unexpected coder env: %#v", cfg.Coder)
	}
	if cfg.Tensorlake.APIKey != "tl-api-env" || cfg.Tensorlake.APIURL != "https://api.tl-env.example" || cfg.Tensorlake.CLIPath != "/opt/tl/bin/tensorlake" || cfg.Tensorlake.Image != "ubuntu:tl-env" || cfg.Tensorlake.Snapshot != "snap-tl-env" || cfg.Tensorlake.OrganizationID != "org-tl-env" || cfg.Tensorlake.ProjectID != "proj-tl-env" || cfg.Tensorlake.Namespace != "ns-tl-env" || cfg.Tensorlake.Workdir != "/workspace/tl-env" || cfg.Tensorlake.CPUs != 2.5 || cfg.Tensorlake.MemoryMB != 4096 || cfg.Tensorlake.DiskMB != 20480 || cfg.Tensorlake.TimeoutSecs != 900 || !cfg.Tensorlake.NoInternet {
		t.Fatalf("unexpected tensorlake env: %#v", cfg.Tensorlake)
	}
	if cfg.Cua.APIURL != "https://cua-env.example" || cfg.Cua.Image != "ubuntu:cua-env" || cfg.Cua.Kind != "vm" || cfg.Cua.Region != "us-central" || cfg.Cua.Workdir != "/workspace/cua-env" || cfg.Cua.VCPUs != 6 || cfg.Cua.MemoryMB != 12288 || cfg.Cua.DiskGB != 80 || cfg.Cua.StartupTimeoutSecs != 240 || cfg.Cua.ExecTimeoutSecs != 1200 || cfg.Cua.BridgeCommand != "python3.12" || cfg.Cua.SDKPackage != "cua" || cfg.Cua.SDKImport != "cua" || cfg.Cua.SDKFallbackImport != "cua_sandbox" {
		t.Fatalf("unexpected cua env: %#v", cfg.Cua)
	}
	if cfg.OpenComputer.APIURL != "https://oc-env.example" || cfg.OpenComputer.Workdir != "/workspace/oc-env" || cfg.OpenComputer.CPU != 6 || cfg.OpenComputer.MemoryMB != 12288 || cfg.OpenComputer.TimeoutSecs != 1200 || cfg.OpenComputer.ExecTimeoutSecs != 2400 {
		t.Fatalf("unexpected opencomputer env: %#v", cfg.OpenComputer)
	}
	if cfg.OpenSandbox.APIURL != "https://opensandbox-env.example" || cfg.OpenSandbox.Image != "ubuntu:osb-env" || cfg.OpenSandbox.Workdir != "/workspace/osb-env" || cfg.OpenSandbox.CPU != "750m" || cfg.OpenSandbox.Memory != "1536Mi" || cfg.OpenSandbox.TimeoutSecs != 123 || cfg.OpenSandbox.ExecTimeoutSecs != 456 || cfg.OpenSandbox.PlatformOS != "linux" || cfg.OpenSandbox.PlatformArch != "amd64" || !cfg.OpenSandbox.SecureAccess || !cfg.OpenSandbox.UseServerProxy {
		t.Fatalf("unexpected opensandbox env: %#v", cfg.OpenSandbox)
	}
	if cfg.Superserve.BaseURL != "https://superserve-env.example" || cfg.Superserve.Template != "superserve/env-template" || cfg.Superserve.Snapshot != "snap-env" || cfg.Superserve.Workdir != "/workspace/ss-env" || cfg.Superserve.TimeoutSecs != 321 || cfg.Superserve.ExecTimeoutSecs != 654 || !cfg.Superserve.ForgetMissing {
		t.Fatalf("unexpected superserve env: %#v", cfg.Superserve)
	}
	if len(cfg.Superserve.NetworkAllowOut) != 2 || cfg.Superserve.NetworkAllowOut[0] != "api.env.example" || cfg.Superserve.NetworkAllowOut[1] != "pkg.env.example" || len(cfg.Superserve.NetworkDenyOut) != 1 || cfg.Superserve.NetworkDenyOut[0] != "169.254.169.254/32" {
		t.Fatalf("unexpected superserve env network lists: %#v", cfg.Superserve)
	}
	if cfg.Cloudflare.APIURL != "https://cloudflare-env.example" || cfg.Cloudflare.Token != "cloudflare-env-token" || cfg.Cloudflare.Workdir != "/workspace/cloudflare-env" {
		t.Fatalf("unexpected cloudflare env: %#v", cfg.Cloudflare)
	}
	if cfg.Proxmox.APIURL != "https://pve-env.example:8006" || cfg.Proxmox.TokenID != "runner@pve!env" || cfg.Proxmox.TokenSecret != "proxmox-env-secret" || cfg.Proxmox.Node != "pve-env" || cfg.Proxmox.TemplateID != 9100 || cfg.Proxmox.Storage != "ceph-env" || cfg.Proxmox.Pool != "pool-env" || cfg.Proxmox.Bridge != "vmbr2" || cfg.Proxmox.User != "runner-env" || cfg.Proxmox.WorkRoot != "/work/proxmox-env" || cfg.Proxmox.FullClone || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("unexpected proxmox env: %#v", cfg.Proxmox)
	}
	if cfg.XCPNg.APIURL != "https://xcp-ng-env.example.test" || cfg.XCPNg.Username != "root-env" || cfg.XCPNg.Password != "xcp-ng-env-secret" || cfg.XCPNg.Template != "template-env" || cfg.XCPNg.TemplateUUID != "tpl-env" || cfg.XCPNg.SR != "sr-env" || cfg.XCPNg.SRUUID != "sr-uuid-env" || cfg.XCPNg.Network != "network-env" || cfg.XCPNg.NetworkUUID != "network-uuid-env" || cfg.XCPNg.Host != "host-env" || cfg.XCPNg.User != "runner-xcp-env" || cfg.XCPNg.WorkRoot != "/work/xcp-ng-env" || !cfg.XCPNg.InsecureTLS {
		t.Fatalf("unexpected xcp-ng env: %#v", cfg.XCPNg)
	}
	if cfg.Semaphore.Host != "semaphore-env.example.test" || cfg.Semaphore.Token != "semaphore-token-env" || cfg.Semaphore.Project != "semaphore-project-env" || cfg.Semaphore.Machine != "f1-standard-env" || cfg.Semaphore.OSImage != "ubuntu-env" || cfg.Semaphore.IdleTimeout != "22m" {
		t.Fatalf("unexpected semaphore env: %#v", cfg.Semaphore)
	}
	if cfg.Sprites.Token != "sprites-token-env" || cfg.Sprites.APIURL != "https://api.sprites-env.example" || cfg.Sprites.WorkRoot != "/home/sprite/env" {
		t.Fatalf("unexpected sprites env: %#v", cfg.Sprites)
	}
	if cfg.LocalContainer.Runtime != "docker" || cfg.LocalContainer.Image != "ubuntu:env" || cfg.LocalContainer.User != "runner-env" || cfg.LocalContainer.WorkRoot != "/workspace/env" || cfg.LocalContainer.CPUs != 6 || cfg.LocalContainer.Memory != "12g" || cfg.LocalContainer.Network != "bridge" || !cfg.LocalContainer.DockerSocket {
		t.Fatalf("unexpected local-container env: %#v", cfg.LocalContainer)
	}
	if !LocalContainerWorkRootExplicit(cfg) {
		t.Fatal("env local-container work root not marked explicit")
	}
	if cfg.Blacksmith.IdleTimeout != 2*time.Hour || !cfg.Blacksmith.Debug {
		t.Fatalf("unexpected blacksmith env: %#v", cfg.Blacksmith)
	}
	if cfg.Namespace.Image != "namespace-env-image" || cfg.Namespace.Size != "XL" || cfg.Namespace.Repository != "github.com/openclaw/env" || cfg.Namespace.Site != "iad1" || cfg.Namespace.VolumeSizeGB != 300 || cfg.Namespace.AutoStopIdleTimeout != 4*time.Hour || cfg.Namespace.WorkRoot != "/workspaces/env" || !cfg.Namespace.DeleteOnRelease {
		t.Fatalf("unexpected namespace env: %#v", cfg.Namespace)
	}
	if len(cfg.Actions.RunnerLabels) != 2 || cfg.Actions.RunnerLabels[1] != "linux-large" || cfg.Actions.Ephemeral {
		t.Fatalf("unexpected actions env: %#v", cfg.Actions)
	}
	if len(cfg.Results.JUnit) != 2 || cfg.Results.JUnit[1] != "build/test.xml" || !cfg.Results.Auto || !cfg.Results.FailOnFailures {
		t.Fatalf("unexpected results env: %#v", cfg.Results)
	}
	if cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || cfg.Cache.Git || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("unexpected cache env: %#v", cfg.Cache)
	}
	if len(cfg.Cache.Volumes) != 2 || cfg.Cache.Volumes[0].Name != "pnpm" || cfg.Cache.Volumes[0].Key != "env-pnpm" || cfg.Cache.Volumes[1].Key != "npm-cache" {
		t.Fatalf("unexpected cache volume env: %#v", cfg.Cache.Volumes)
	}
	if !cfg.Sync.Checksum || cfg.Sync.Delete || cfg.Sync.GitSeed || cfg.Sync.Fingerprint || cfg.Sync.Timeout != 45*time.Minute || !cfg.Sync.AllowLarge {
		t.Fatalf("unexpected sync env: %#v", cfg.Sync)
	}
	if len(cfg.EnvAllow) != 3 || cfg.EnvAllow[2] != "CUSTOM_*" {
		t.Fatalf("unexpected env allow: %#v", cfg.EnvAllow)
	}
	if len(cfg.Run.PreflightTools) != 3 || cfg.Run.PreflightTools[1] != "bun" {
		t.Fatalf("unexpected preflight tools: %#v", cfg.Run.PreflightTools)
	}
}

func TestOpenSandboxFilePresenceAndExcludedFields(t *testing.T) {
	initial := OpenSandboxConfig{APIURL: "https://example.invalid/prior", Image: "image", Workdir: "/workspace/app", CPU: "2", Memory: "4Gi", TimeoutSecs: 10, ExecTimeoutSecs: 20, PlatformOS: "linux", PlatformArch: "arm64", SecureAccess: true, UseServerProxy: true}
	for _, field := range []string{"APIURL", "ForgetMissing"} {
		if _, ok := reflect.TypeOf(fileOpenSandboxConfig{}).FieldByName(field); ok {
			t.Fatalf("%s must not have a YAML binding", field)
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, priorForget := range []bool{false, true} {
			for _, mode := range []string{"omitted", "null", "zero"} {
				t.Run(fmt.Sprintf("trusted=%t/forget=%t/%s", trusted, priorForget, mode), func(t *testing.T) {
					body := fmt.Sprintf("openSandbox:\n  apiUrl: https://example.invalid/file\n  forgetMissing: %t\n", !priorForget)
					if mode != "omitted" {
						for _, key := range []string{"image", "workdir", "cpu", "memory", "timeoutSecs", "execTimeoutSecs", "platformOS", "platformArch", "secureAccess", "useServerProxy"} {
							value := "null"
							if mode == "zero" {
								value = "''"
								switch key {
								case "timeoutSecs", "execTimeoutSecs":
									value = "0"
								case "secureAccess", "useServerProxy":
									value = "false"
								}
							}
							body += "  " + key + ": " + value + "\n"
						}
					}
					cfg := baseConfig()
					cfg.OpenSandbox = initial
					cfg.OpenSandbox.ForgetMissing = priorForget
					var file fileConfig
					if err := yaml.Unmarshal([]byte(body), &file); err != nil {
						t.Fatal(err)
					}
					if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
						t.Fatal(err)
					}
					want := initial
					want.ForgetMissing = priorForget
					if mode == "zero" {
						want = OpenSandboxConfig{APIURL: initial.APIURL, ForgetMissing: priorForget}
					}
					if cfg.OpenSandbox != want {
						t.Fatalf("got %#v, want %#v", cfg.OpenSandbox, want)
					}
				})
			}
		}
	}
}

func TestOpenSandboxEnvironmentSourceAndBooleanSemantics(t *testing.T) {
	for _, tc := range []struct{ primary, fallback, want string }{
		{"https://example.invalid/primary", "https://example.invalid/fallback", "https://example.invalid/primary"},
		{"", "https://example.invalid/fallback", "https://example.invalid/fallback"},
		{"", "", "https://example.invalid/prior"},
		{" ", "https://example.invalid/fallback", " "},
		{"", " ", " "},
	} {
		t.Run("url/"+tc.want, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_OPENSANDBOX_API_URL", tc.primary)
			t.Setenv("OPEN_SANDBOX_API_URL", tc.fallback)
			cfg := baseConfig()
			cfg.OpenSandbox.APIURL = "https://example.invalid/prior"
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.OpenSandbox.APIURL != tc.want {
				t.Fatalf("URL=%q, want %q", cfg.OpenSandbox.APIURL, tc.want)
			}
		})
	}
	for _, prior := range []bool{false, true} {
		for _, raw := range []string{"", "invalid", "true", "false", "yes", "no", "on", "off", "1", "0"} {
			t.Run(fmt.Sprintf("bool/%t/%s", prior, raw), func(t *testing.T) {
				clearConfigEnv(t)
				t.Setenv("CRABBOX_OPENSANDBOX_SECURE_ACCESS", raw)
				t.Setenv("CRABBOX_OPENSANDBOX_USE_SERVER_PROXY", raw)
				t.Setenv("CRABBOX_OPENSANDBOX_FORGET_MISSING", strconv.FormatBool(!prior))
				t.Setenv("OPEN_SANDBOX_FORGET_MISSING", strconv.FormatBool(!prior))
				cfg := baseConfig()
				cfg.OpenSandbox.SecureAccess, cfg.OpenSandbox.UseServerProxy, cfg.OpenSandbox.ForgetMissing = prior, prior, prior
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				want := prior
				switch raw {
				case "true", "yes", "on", "1":
					want = true
				case "false", "no", "off", "0":
					want = false
				}
				if cfg.OpenSandbox.SecureAccess != want || cfg.OpenSandbox.UseServerProxy != want || cfg.OpenSandbox.ForgetMissing != prior {
					t.Fatalf("boolean overlay=%#v, want bool=%t forget=%t", cfg.OpenSandbox, want, prior)
				}
			})
		}
	}
}

func TestOpenSandboxIntegerOverlayErrorOrder(t *testing.T) {
	for _, source := range []string{"file", "env"} {
		for _, second := range []bool{false, true} {
			for _, invalid := range []string{"-1", "invalid"} {
				if source == "file" && invalid == "invalid" {
					continue
				}
				t.Run(fmt.Sprintf("%s/second=%t/%s", source, second, invalid), func(t *testing.T) {
					clearConfigEnv(t)
					first := invalid
					if second {
						first = "7"
					}
					cfg := baseConfig()
					cfg.OpenSandbox.TimeoutSecs, cfg.OpenSandbox.ExecTimeoutSecs = 10, 20
					cfg.OpenSandbox.PlatformOS = "before"
					var err error
					key, env := "timeoutSecs", "CRABBOX_OPENSANDBOX_TIMEOUT_SECS"
					if second {
						key, env = "execTimeoutSecs", "CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS"
					}
					wantError := "opensandbox " + key + " must be non-negative"
					if source == "file" {
						var file fileConfig
						if err := yaml.Unmarshal([]byte("openSandbox:\n  image: after\n  platformOS: after\n  timeoutSecs: "+first+"\n  execTimeoutSecs: "+invalid+"\n"), &file); err != nil {
							t.Fatal(err)
						}
						err = applyFileConfig(&cfg, file)
					} else {
						t.Setenv("CRABBOX_OPENSANDBOX_IMAGE", "after")
						t.Setenv("CRABBOX_OPENSANDBOX_PLATFORM_OS", "after")
						t.Setenv("CRABBOX_OPENSANDBOX_TIMEOUT_SECS", first)
						t.Setenv("CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS", invalid)
						err = applyEnv(&cfg)
						wantError = env + " must be non-negative"
						if invalid == "invalid" {
							wantError = env + " must be an integer"
						}
					}
					if err == nil || err.Error() != wantError {
						t.Fatalf("error=%v, want %q", err, wantError)
					}
					wantFirst, wantSecond := 10, 20
					if second {
						wantFirst = 7
					}
					if source == "env" {
						if second {
							wantSecond = 0
						} else {
							wantFirst = 0
						}
					}
					if cfg.OpenSandbox.TimeoutSecs != wantFirst || cfg.OpenSandbox.ExecTimeoutSecs != wantSecond || cfg.OpenSandbox.Image != "after" || cfg.OpenSandbox.PlatformOS != "before" {
						t.Fatalf("partial update=%#v, want timeouts=%d,%d", cfg.OpenSandbox, wantFirst, wantSecond)
					}
				})
			}
		}
	}
}

func TestApplyEnvRejectsNegativeOpenSandboxTimeouts(t *testing.T) {
	for _, name := range []string{"CRABBOX_OPENSANDBOX_TIMEOUT_SECS", "CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "-1")
			cfg := baseConfig()
			err := applyEnv(&cfg)
			if err == nil || !strings.Contains(err.Error(), name+" must be non-negative") {
				t.Fatalf("err=%v, want negative timeout rejection", err)
			}
		})
	}
}

func TestApplyEnvRejectsNegativeCUAResources(t *testing.T) {
	for _, name := range []string{
		"CRABBOX_CUA_VCPUS",
		"CRABBOX_CUA_MEMORY_MB",
		"CRABBOX_CUA_DISK_GB",
		"CRABBOX_CUA_STARTUP_TIMEOUT_SECS",
		"CRABBOX_CUA_EXEC_TIMEOUT_SECS",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "-1")
			cfg := baseConfig()
			err := applyEnv(&cfg)
			if err == nil || !strings.Contains(err.Error(), name+" must be non-negative") {
				t.Fatalf("err=%v, want negative CUA value rejection", err)
			}
		})
	}
}

func TestCUAFilePresenceAndSourceAdmission(t *testing.T) {
	initial := CuaConfig{APIURL: "https://example.invalid/initial", Image: "image", Kind: "vm", Region: "region", Workdir: "/workspace/app", VCPUs: 1, MemoryMB: 2, DiskGB: 3, StartupTimeoutSecs: 4, ExecTimeoutSecs: 5, BridgeCommand: "python3", SDKPackage: "cua", SDKImport: "cua", SDKFallbackImport: "cua_sandbox"}
	keys := []string{"image", "kind", "region", "workdir", "vcpus", "memoryMB", "diskGB", "startupTimeoutSecs", "execTimeoutSecs", "bridgeCommand", "sdkPackage", "sdkImport", "sdkFallbackImport"}
	if _, ok := reflect.TypeOf(fileCuaConfig{}).FieldByName("APIURL"); ok {
		t.Fatal("API URL must not have a YAML field, even for trusted input")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "zero"} {
			t.Run(fmt.Sprintf("trusted=%t/%s", trusted, mode), func(t *testing.T) {
				body := "cua:\n  apiURL: https://example.invalid/yaml\n"
				if mode != "omitted" {
					for i, key := range keys {
						value := "null"
						if mode == "zero" {
							value = "''"
							if i >= 4 && i <= 8 {
								value = "0"
							}
						}
						body += "  " + key + ": " + value + "\n"
					}
				}
				var file fileConfig
				if err := yaml.Unmarshal([]byte(body), &file); err != nil {
					t.Fatal(err)
				}
				cfg := baseConfig()
				cfg.Cua = initial
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				want := initial
				if mode == "zero" {
					want = CuaConfig{APIURL: initial.APIURL}
					if !trusted {
						want.BridgeCommand, want.SDKPackage, want.SDKImport, want.SDKFallbackImport = initial.BridgeCommand, initial.SDKPackage, initial.SDKImport, initial.SDKFallbackImport
					}
				}
				if cfg.Cua != want {
					t.Fatalf("got %#v, want %#v", cfg.Cua, want)
				}
			})
		}
	}
}

func TestCUAAPIURLEnvironmentPrecedence(t *testing.T) {
	for _, tc := range []struct{ primary, fallback, want string }{
		{"https://example.invalid/primary", "https://example.invalid/fallback", "https://example.invalid/primary"},
		{"", "https://example.invalid/fallback", "https://example.invalid/fallback"},
		{"", "", "https://example.invalid/initial"},
		{" ", "https://example.invalid/fallback", " "},
	} {
		t.Run(tc.want, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CUA_API_URL", tc.primary)
			t.Setenv("CUA_BASE_URL", tc.fallback)
			cfg := baseConfig()
			cfg.Cua.APIURL = "https://example.invalid/initial"
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Cua.APIURL != tc.want {
				t.Fatalf("URL=%q, want %q", cfg.Cua.APIURL, tc.want)
			}
		})
	}
}

func TestCUAIntegerOverlayErrorOrder(t *testing.T) {
	keys := []string{"vcpus", "memoryMB", "diskGB", "startupTimeoutSecs", "execTimeoutSecs"}
	envs := []string{"CRABBOX_CUA_VCPUS", "CRABBOX_CUA_MEMORY_MB", "CRABBOX_CUA_DISK_GB", "CRABBOX_CUA_STARTUP_TIMEOUT_SECS", "CRABBOX_CUA_EXEC_TIMEOUT_SECS"}
	for _, source := range []string{"file", "env"} {
		for fail := range keys {
			for _, invalid := range []string{"-1", "invalid"} {
				if source == "file" && invalid == "invalid" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%s", source, keys[fail], invalid), func(t *testing.T) {
					clearConfigEnv(t)
					cfg := baseConfig()
					cfg.Cua.VCPUs, cfg.Cua.MemoryMB, cfg.Cua.DiskGB, cfg.Cua.StartupTimeoutSecs, cfg.Cua.ExecTimeoutSecs = 10, 20, 30, 40, 50
					cfg.Cua.BridgeCommand = "before-python"
					body := "cua:\n  image: after\n  bridgeCommand: after-python\n"
					t.Setenv("CRABBOX_CUA_IMAGE", "after")
					t.Setenv("CRABBOX_CUA_BRIDGE_COMMAND", "after-python")
					for i, key := range keys {
						value := "7"
						if i >= fail {
							value = invalid
						}
						body += "  " + key + ": " + value + "\n"
						t.Setenv(envs[i], value)
					}
					var err error
					wantError := "cua " + keys[fail] + " must be non-negative"
					if source == "file" {
						var file fileConfig
						if err := yaml.Unmarshal([]byte(body), &file); err != nil {
							t.Fatal(err)
						}
						err = applyFileConfig(&cfg, file)
					} else {
						err = applyEnv(&cfg)
						wantError = envs[fail] + " must be non-negative"
						if invalid == "invalid" {
							wantError = envs[fail] + " must be an integer"
						}
					}
					if err == nil || err.Error() != wantError {
						t.Fatalf("error=%v, want %q", err, wantError)
					}
					want := []int{10, 20, 30, 40, 50}
					for i := 0; i < fail; i++ {
						want[i] = 7
					}
					if source == "env" {
						want[fail] = 0
					}
					got := []int{cfg.Cua.VCPUs, cfg.Cua.MemoryMB, cfg.Cua.DiskGB, cfg.Cua.StartupTimeoutSecs, cfg.Cua.ExecTimeoutSecs}
					if !reflect.DeepEqual(got, want) || cfg.Cua.Image != "after" || cfg.Cua.BridgeCommand != "before-python" {
						t.Fatalf("partial update=%#v, want integers %v", cfg.Cua, want)
					}
				})
			}
		}
	}
}

func TestCUARepoConfigCannotReplaceBridgeRuntime(t *testing.T) {
	cfg := baseConfig()
	cfg.Cua.BridgeCommand = "trusted-python"
	cfg.Cua.SDKPackage = "trusted-package"
	cfg.Cua.SDKImport = "trusted_import"
	cfg.Cua.SDKFallbackImport = "trusted_fallback"
	var file fileConfig
	if err := yaml.Unmarshal([]byte(strings.Join([]string{
		"cua:",
		"  workdir: /workspace/repo",
		"  bridgeCommand: ./example-python",
		"  sdkPackage: example-package",
		"  sdkImport: example_sdk",
		"  sdkFallbackImport: example_fallback",
	}, "\n")), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Cua.BridgeCommand != "trusted-python" || cfg.Cua.SDKPackage != "trusted-package" || cfg.Cua.SDKImport != "trusted_import" || cfg.Cua.SDKFallbackImport != "trusted_fallback" {
		t.Fatalf("repository config replaced trusted CUA bridge settings: %#v", cfg.Cua)
	}
	if cfg.Cua.Workdir != "/workspace/repo" {
		t.Fatalf("safe repository workdir setting not applied: %#v", cfg.Cua)
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Cua.BridgeCommand != "./example-python" || cfg.Cua.SDKPackage != "example-package" || cfg.Cua.SDKImport != "example_sdk" || cfg.Cua.SDKFallbackImport != "example_fallback" {
		t.Fatalf("trusted file did not apply bridge settings: %#v", cfg.Cua)
	}
}

func TestCUAConfigRejectsNegativeYAMLValues(t *testing.T) {
	for _, field := range []string{"vcpus", "memoryMB", "diskGB", "startupTimeoutSecs", "execTimeoutSecs"} {
		t.Run(field, func(t *testing.T) {
			cfg := baseConfig()
			var file fileConfig
			if err := yaml.Unmarshal([]byte("cua:\n  "+field+": -1\n"), &file); err != nil {
				t.Fatal(err)
			}
			err := applyFileConfig(&cfg, file)
			if err == nil || !strings.Contains(err.Error(), "cua "+field+" must be non-negative") {
				t.Fatalf("err=%v, want negative CUA %s rejection", err, field)
			}
		})
	}
}

func TestExplicitProviderImagesSurvivePortableOSDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
os: ubuntu:24.04
hetzner:
  image: ubuntu-26.04
azure:
  image: Canonical:ubuntu-26_04-lts:server:latest
islo:
  image: docker.io/library/ubuntu:26.04
localContainer:
  image: ubuntu:26.04
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "ubuntu-26.04" || cfg.AzureImage != defaultAzureLinuxImage || cfg.Islo.Image != "docker.io/library/ubuntu:26.04" || cfg.LocalContainer.Image != "ubuntu:26.04" {
		t.Fatalf("explicit images were overwritten: hetzner=%q azure=%q islo=%q local=%q", cfg.Image, cfg.AzureImage, cfg.Islo.Image, cfg.LocalContainer.Image)
	}
}

func TestLocalContainerNoHostnameConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		want            bool
	}{
		{name: "omitted default", yaml: "{}"},
		{name: "YAML enabled", yaml: "{noHostname: true}", want: true},
		{name: "YAML disabled", yaml: "{noHostname: false}"},
		{name: "environment enabled", yaml: "{}", env: "1", want: true},
		{name: "environment overrides true", yaml: "{noHostname: true}", env: "0"},
		{name: "environment overrides false", yaml: "{noHostname: false}", env: "true", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			cfgPath := filepath.Join(home, "crabbox.yaml")
			t.Setenv("CRABBOX_CONFIG", cfgPath)
			t.Setenv("CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME", tc.env)
			if err := os.WriteFile(cfgPath, []byte("provider: local-container\nlocalContainer: "+tc.yaml+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LocalContainer.NoHostname != tc.want {
				t.Fatalf("NoHostname=%t, want %t", cfg.LocalContainer.NoHostname, tc.want)
			}
		})
	}
}

func TestLocalContainerNoHostnameFileLayering(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{name: "omitted preserves enabled", yaml: "{}", want: true},
		{name: "explicit false clears enabled", yaml: "{noHostname: false}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filePath := filepath.Join(t.TempDir(), "crabbox.yaml")
			if err := os.WriteFile(filePath, []byte("localContainer: "+tc.yaml+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := readFileConfig(filePath)
			if err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			cfg.LocalContainer.NoHostname = true
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal(err)
			}
			if cfg.LocalContainer.NoHostname != tc.want {
				t.Fatalf("NoHostname=%t, want %t", cfg.LocalContainer.NoHostname, tc.want)
			}
		})
	}
}

func TestRepoConfigDoesNotApplyLocalContainerVolumes(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
provider: local-container
localContainer:
  volumes:
    - /host/secret:/container/secret:ro
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalContainer.Volumes) != 0 {
		t.Fatalf("repo config applied local-container volumes: %#v", cfg.LocalContainer.Volumes)
	}
}

func TestPortableOSDefaultsRespectTargetAlias(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
target: ubuntu
os: ubuntu:24.04
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.Image != "ubuntu-24.04" || cfg.AzureImage != "Canonical:ubuntu-24_04-lts:server:latest" {
		t.Fatalf("portable os defaults not applied through target alias: target=%q image=%q azure=%q", cfg.TargetOS, cfg.Image, cfg.AzureImage)
	}
}

func TestAppleContainerImageFollowsOSImageDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// apple-container must track the OS image default the same way local-container does.
	if cfg.AppleContainer.Image == "" || cfg.AppleContainer.Image != cfg.LocalContainer.Image {
		t.Fatalf("apple-container image should follow the os default like local-container: apple=%q local=%q", cfg.AppleContainer.Image, cfg.LocalContainer.Image)
	}
	if cfg.AppleContainer.Image == baseConfig().AppleContainer.Image {
		t.Fatalf("--os did not update apple-container image: still base %q", cfg.AppleContainer.Image)
	}
}

func TestAppleContainerExplicitImageSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\nappleContainer:\n  image: my-org/custom:tag\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleContainer.Image != "my-org/custom:tag" {
		t.Fatalf("explicit apple-container image was overwritten by --os: %q", cfg.AppleContainer.Image)
	}
}

func TestAppleVMImageFollowsOSImageDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vm\ntarget: linux\nos: ubuntu:24.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.AppleVM.Image, "ubuntu-24.04-server-cloudimg-arm64.img") {
		t.Fatalf("apple-vm image should follow --os default: %q", cfg.AppleVM.Image)
	}
	if cfg.AppleVM.ImageSHA256 != "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec" {
		t.Fatalf("apple-vm checksum should follow --os default: %q", cfg.AppleVM.ImageSHA256)
	}
}

func TestAppleVMExplicitImageSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vm\ntarget: linux\nos: ubuntu:24.04\nappleVM:\n  image: https://example.test/custom.img\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVM.Image != "https://example.test/custom.img" {
		t.Fatalf("explicit apple-vm image was overwritten by --os: %q", cfg.AppleVM.Image)
	}
	if cfg.AppleVM.ImageSHA256 != "" {
		t.Fatalf("custom apple-vm image should clear default checksum unless explicitly set: %q", cfg.AppleVM.ImageSHA256)
	}
}

func TestAppleVMExplicitChecksumSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	checksum := strings.Repeat("b", 64)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vm\ntarget: linux\nos: ubuntu:24.04\nappleVM:\n  imageSHA256: "+checksum+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVM.ImageSHA256 != checksum {
		t.Fatalf("explicit apple-vm checksum was overwritten by OS defaults: %q", cfg.AppleVM.ImageSHA256)
	}

	t.Setenv("CRABBOX_APPLE_VM_IMAGE_SHA256", strings.Repeat("c", 64))
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVM.ImageSHA256 != strings.Repeat("c", 64) {
		t.Fatalf("environment apple-vm checksum was overwritten by OS defaults: %q", cfg.AppleVM.ImageSHA256)
	}
}

func TestAppleVMPreservesExplicitTopLevelWorkRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vm\nworkRoot: /custom/crabbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/custom/crabbox" {
		t.Fatalf("WorkRoot=%q want /custom/crabbox", cfg.WorkRoot)
	}
}

func TestAppleVMSpecificWorkRootOverridesTopLevel(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vm\nworkRoot: /custom/crabbox\nappleVM:\n  workRoot: /work/apple-vm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/work/apple-vm" {
		t.Fatalf("WorkRoot=%q want /work/apple-vm", cfg.WorkRoot)
	}
}

func TestMultipassImageFollowsOSImageDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Multipass.Image != "24.04" {
		t.Fatalf("multipass image should follow --os default: %q", cfg.Multipass.Image)
	}
}

func TestMultipassExplicitImageSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\nmultipass:\n  image: daily:26.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Multipass.Image != "daily:26.04" {
		t.Fatalf("explicit multipass image was overwritten by --os: %q", cfg.Multipass.Image)
	}
}

func TestPortableOSHigherPrecedenceOverridesEarlierPortableDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	t.Setenv("CRABBOX_OS", "ubuntu:26.04")
	if err := os.WriteFile(cfgPath, []byte(`
os: ubuntu:24.04
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OSImage != "ubuntu:26.04" || cfg.Image != "ubuntu-24.04" || cfg.AzureImage != defaultAzureLinuxImage {
		t.Fatalf("higher precedence os did not override provider defaults: os=%q image=%q azure=%q", cfg.OSImage, cfg.Image, cfg.AzureImage)
	}
}

func TestWandbConfigAPIKeyBeatsGenericWANDBEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("WANDB_API_KEY", "generic-env-key")

	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: wandb\nwandb:\n  apiKey: config-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Wandb.APIKey != "config-key" {
		t.Fatalf("Wandb.APIKey = %q, want config-key", cfg.Wandb.APIKey)
	}
}

func TestCrabboxWandbAPIKeyOverridesConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_WANDB_API_KEY", "crabbox-env-key")
	t.Setenv("WANDB_API_KEY", "generic-env-key")

	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: wandb\nwandb:\n  apiKey: config-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Wandb.APIKey != "crabbox-env-key" {
		t.Fatalf("Wandb.APIKey = %q, want crabbox-env-key", cfg.Wandb.APIKey)
	}
}

func TestTailscaleEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	t.Setenv("CRABBOX_NETWORK", "tailscale")
	t.Setenv("CRABBOX_TAILSCALE", "1")
	t.Setenv("CRABBOX_TAILSCALE_TAGS", "tag:crabbox,tag:ci")
	t.Setenv("CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE", "lease-{slug}")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-secret")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE", "100.100.100.100")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS", "true")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network != NetworkTailscale || !cfg.Tailscale.Enabled || cfg.Tailscale.AuthKey != "tskey-secret" || cfg.Tailscale.HostnameTemplate != "lease-{slug}" || cfg.Tailscale.ExitNode != "100.100.100.100" || !cfg.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("unexpected tailscale env: network=%s tailscale=%#v", cfg.Network, cfg.Tailscale)
	}
	if len(cfg.Tailscale.Tags) != 2 || cfg.Tailscale.Tags[1] != "tag:ci" {
		t.Fatalf("unexpected tailscale tags: %#v", cfg.Tailscale.Tags)
	}
}

func TestProviderAliasCanonicalizedBeforeDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "google")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "crabbox-project")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "gcp" || cfg.ServerType != "c4-standard-192" {
		t.Fatalf("provider=%q type=%q want gcp c4-standard-192", cfg.Provider, cfg.ServerType)
	}
}

func TestConfigFileServerTypeIsExplicit(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: gcp\nserverType: c4-standard-192\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerType != "c4-standard-192" || !cfg.ServerTypeExplicit {
		t.Fatalf("serverType=%q explicit=%t, want explicit c4-standard-192", cfg.ServerType, cfg.ServerTypeExplicit)
	}
	if largeDefaultServerType(cfg) {
		t.Fatalf("explicit config serverType should not warn as a large default")
	}
}

func TestInvalidNetworkConfigFails(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("network: private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid network config to fail")
	}
}

func TestInvalidNetworkEnvFails(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_NETWORK", "tailnet")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid CRABBOX_NETWORK to fail")
	}
}

func TestDockerSandboxCPUEnvCanBeOverriddenByFlags(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "docker-sandbox")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "2.5")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig err=%v, want command flags to get a chance to override provider config", err)
	}
	fs := newFlagSet("test", io.Discard)
	values := registerProviderFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--docker-sandbox-cpus", "2"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatalf("applyProviderFlags err=%v, want valid CLI override to win", err)
	}
	if cfg.DockerSandbox.CPUs != 2 {
		t.Fatalf("cpus=%g, want CLI override 2", cfg.DockerSandbox.CPUs)
	}
}

func TestInvalidDockerSandboxCPUEnvNonNumericFailsDuringLoad(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "docker-sandbox")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "not-a-number")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "CRABBOX_DOCKER_SANDBOX_CPUS") {
		t.Fatalf("loadConfig err=%v, want docker-sandbox CPU env parse rejection", err)
	}
}

func TestAccessAuthState(t *testing.T) {
	for name, tc := range map[string]struct {
		access AccessConfig
		want   string
	}{
		"missing": {
			want: "missing",
		},
		"incomplete": {
			access: AccessConfig{ClientID: "client"},
			want:   "incomplete",
		},
		"service token": {
			access: AccessConfig{ClientID: "client", ClientSecret: "secret"},
			want:   "service-token",
		},
		"token": {
			access: AccessConfig{Token: "jwt"},
			want:   "token",
		},
		"service token plus token": {
			access: AccessConfig{ClientID: "client", ClientSecret: "secret", Token: "jwt"},
			want:   "service-token+token",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := accessAuthState(tc.access); got != tc.want {
				t.Fatalf("accessAuthState()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRepoConfigIsYamlOnly(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	if err := os.WriteFile(".crabbox.json", []byte(`{"profile":"json-profile","provider":"aws"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("profile: yaml-profile\nprovider: aws\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "yaml-profile" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: profile=%s provider=%s", cfg.Profile, cfg.Provider)
	}
}

func TestLeaseDurationErrorPolicies(t *testing.T) {
	const prior = 17 * time.Second
	for _, tc := range []struct {
		raw  string
		want time.Duration
		bad  bool
	}{
		{"", prior, false}, {" ", prior, true}, {"invalid", prior, true},
		{"0", prior, true}, {"0s", prior, true}, {"-1ns", prior, true},
		{" 2m ", prior, true}, {"999999999999999999h", prior, true},
		{"2m", 2 * time.Minute, false}, {"125ms", 125 * time.Millisecond, false},
		{"+1s", time.Second, false}, {"17s", prior, false},
	} {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			strict, tolerant := prior, prior
			err := ApplyLeaseDuration(&strict, tc.raw)
			accepted := applyLeaseDuration(&tolerant, tc.raw)
			if accepted != (tc.raw != "" && !tc.bad) {
				t.Fatalf("accepted=%t raw=%q", accepted, tc.raw)
			}
			if strict != tc.want || tolerant != tc.want {
				t.Fatalf("strict=%s tolerant=%s want=%s", strict, tolerant, tc.want)
			}
			if tc.bad {
				if err == nil || err.Error() != fmt.Sprintf("invalid duration %q", tc.raw) {
					t.Fatalf("error=%v", err)
				}
				var exitErr ExitError
				if AsExitError(err, &exitErr) {
					t.Fatalf("ordinary duration error changed to ExitError: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigHelperBranches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "explicit.yaml"))

	if got := configPaths(); len(got) != 1 || got[0] != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("configPaths=%v", got)
	}
	if got := writableConfigPath(); got != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("writableConfigPath=%q", got)
	}

	cfgPath, err := writeUserFileConfig(fileConfig{Profile: "written", Provider: "aws"})
	if err != nil {
		t.Fatal(err)
	}
	if cfgPath != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("write path=%q", cfgPath)
	}
	file, err := readFileConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Profile != "written" || file.Provider != "aws" {
		t.Fatalf("file config=%#v", file)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode=%04o want 0600", got)
	}

	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeUserFileConfig(fileConfig{Profile: "rewritten"}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("rewritten config mode=%04o want 0600", got)
	}
	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := configFilePermissionProblem(cfgPath); got == "" {
		t.Fatal("expected config permission problem")
	}
	if got := configFilePermissionProblem(""); got != "" {
		t.Fatalf("empty path permission problem=%q", got)
	}
	if got := configFilePermissionProblem(filepath.Join(t.TempDir(), "missing.yaml")); got != "" {
		t.Fatalf("missing path permission problem=%q", got)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := configFilePermissionProblem(cfgPath); got != "" {
		t.Fatalf("secure config permission problem=%q", got)
	}

	empty, err := readFileConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Profile != "" {
		t.Fatalf("missing file config=%#v", empty)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	empty, err = readFileConfig(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Profile != "" {
		t.Fatalf("empty file config=%#v", empty)
	}
	badPath := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(badPath, []byte("profile: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileConfig(badPath); err == nil {
		t.Fatal("expected parse error for bad config")
	}

	if got := expandUserPath("~"); got != home {
		t.Fatalf("expand ~= %q want %q", got, home)
	}
	if got := expandUserPath("~/bin"); got != filepath.Join(home, "bin") {
		t.Fatalf("expand ~/bin=%q", got)
	}
	if got := expandUserPath("/tmp/x"); got != "/tmp/x" {
		t.Fatalf("absolute path changed to %q", got)
	}

	duration := 10 * time.Minute
	applyLeaseDuration(&duration, "")
	applyLeaseDuration(&duration, "bad")
	applyLeaseDuration(&duration, "0s")
	if duration != 10*time.Minute {
		t.Fatalf("invalid durations changed value to %s", duration)
	}
	applyLeaseDuration(&duration, "15m")
	if duration != 15*time.Minute {
		t.Fatalf("duration=%s", duration)
	}
}

func TestWriteUserFileConfigAtomic(t *testing.T) {
	t.Run("successful rewrite keeps mode 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("profile: previous\nprovider: aws\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeUserFileConfigAtomic(path, []byte("profile: rewritten\nprovider: aws\n"), os.Rename, func(string) {}); err != nil {
			t.Fatal(err)
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if file.Profile != "rewritten" || file.Provider != "aws" {
			t.Fatalf("file config=%#v", file)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config mode=%04o want 0600", got)
		}
	})

	t.Run("rename failure preserves previous readable config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("profile: previous\nprovider: aws\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stagedPath string
		renameErr := fmt.Errorf("rename failed")
		err := writeUserFileConfigAtomic(path, []byte("profile: staged\nprovider: aws\n"), func(from, to string) error {
			stagedPath = from
			if to != path {
				t.Fatalf("rename target=%q want %q", to, path)
			}
			if _, err := os.Stat(from); err != nil {
				t.Fatalf("staged config missing before rename: %v", err)
			}
			return renameErr
		}, func(string) {
			t.Fatal("directory sync should not run after failed rename")
		})
		if err == nil || !strings.Contains(err.Error(), renameErr.Error()) {
			t.Fatalf("rename error=%v want %v", err, renameErr)
		}
		if stagedPath == "" {
			t.Fatal("rename was not attempted")
		}
		if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
			t.Fatalf("staged temp remains after failed rename: %v", err)
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if file.Profile != "previous" || file.Provider != "aws" {
			t.Fatalf("previous config not preserved: %#v", file)
		}
	})

	t.Run("symlink rewrite preserves link", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.yaml")
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(target, []byte("profile: previous\nprovider: aws\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target.yaml", path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}

		if err := writeUserFileConfigAtomic(path, []byte("profile: rewritten\nprovider: aws\n"), os.Rename, func(string) {}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("config path mode=%s want symlink", info.Mode())
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if file.Profile != "rewritten" || file.Provider != "aws" {
			t.Fatalf("file config=%#v", file)
		}
		targetInfo, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := targetInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("target config mode=%04o want 0600", got)
		}
	})
}

func TestConfigHelperErrorBranches(t *testing.T) {
	t.Run("unavailable user config dir", func(t *testing.T) {
		t.Setenv("CRABBOX_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "")
		if _, err := writeUserFileConfig(fileConfig{Profile: "missing-home"}); err == nil {
			t.Fatal("expected unavailable user config dir error")
		}
	})

	t.Run("config parent is file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-dir")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CRABBOX_CONFIG", filepath.Join(parent, "config.yaml"))
		if _, err := writeUserFileConfig(fileConfig{Profile: "mkdir-fails"}); err == nil {
			t.Fatal("expected config directory create error")
		}
	})

	t.Run("config path is directory", func(t *testing.T) {
		path := t.TempDir()
		t.Setenv("CRABBOX_CONFIG", path)
		if _, err := writeUserFileConfig(fileConfig{Profile: "write-fails"}); err == nil {
			t.Fatal("expected config write error")
		}
	})
}

func TestWindowsWSLWorkRoot(t *testing.T) {
	if got := windowsWSLWorkRoot(Config{}); got != defaultPOSIXWorkRoot {
		t.Fatalf("windowsWSLWorkRoot default=%q want %q", got, defaultPOSIXWorkRoot)
	}
	if got := windowsWSLWorkRoot(Config{WorkRoot: "/work/custom"}); got != "/work/custom" {
		t.Fatalf("windowsWSLWorkRoot custom=%q", got)
	}
}

func envIntegerCases() []struct {
	name, value    string
	want32, want64 int64
	ok32, ok64     bool
} {
	return []struct {
		name, value    string
		want32, want64 int64
		ok32, ok64     bool
	}{
		{"unset", "", 7, 7, false, false},
		{"empty", "", 7, 7, false, false},
		{"same as fallback", "7", 7, 7, true, true},
		{"zero", "0", 0, 0, true, true},
		{"plus zero", "+0", 0, 0, true, true},
		{"negative zero", "-0", 0, 0, true, true},
		{"negative", "-12", -12, -12, true, true},
		{"positive sign", "+12", 12, 12, true, true},
		{"decimal leading zero", "042", 42, 42, true, true},
		{"padded", " 12 ", 7, 7, false, false},
		{"newline", "12\n", 7, 7, false, false},
		{"fractional", "1.5", 7, 7, false, false},
		{"word", "invalid", 7, 7, false, false},
		{"hex prefix", "0x10", 7, 7, false, false},
		{"underscore", "1_000", 7, 7, false, false},
		{"unicode digit", "１２", 7, 7, false, false},
		{"only sign", "+", 7, 7, false, false},
		{"int32 max", "2147483647", 2147483647, 2147483647, true, true},
		{"int32 min", "-2147483648", -2147483648, -2147483648, true, true},
		{"int32 positive overflow", "2147483648", 7, 2147483648, false, true},
		{"int32 negative overflow", "-2147483649", 7, -2147483649, false, true},
		{"int64 max", "9223372036854775807", 7, 9223372036854775807, false, true},
		{"int64 min", "-9223372036854775808", 7, -9223372036854775808, false, true},
		{"int64 positive overflow", "9223372036854775808", 7, 7, false, false},
		{"int64 negative overflow", "-9223372036854775809", 7, 7, false, false},
	}
}

func TestEnvIntegerFallbacks(t *testing.T) {
	const name = "CRABBOX_TEST_INTEGER"
	for _, helper := range []struct {
		name string
		bits int
		read func(string) int64
	}{
		{"native", strconv.IntSize, func(name string) int64 { return int64(getenvInt(name, 7)) }},
		{"int32", 32, func(name string) int64 { return int64(getenvInt32(name, 7)) }},
		{"int64", 64, func(name string) int64 { return getenvInt64(name, 7) }},
	} {
		t.Run(helper.name, func(t *testing.T) {
			for _, tc := range envIntegerCases() {
				t.Run(tc.name, func(t *testing.T) {
					t.Setenv(name, tc.value)
					if tc.name == "unset" {
						if err := os.Unsetenv(name); err != nil {
							t.Fatal(err)
						}
					}
					want := tc.want64
					if helper.bits == 32 {
						want = tc.want32
					}
					if got := helper.read(name); got != want {
						t.Fatalf("%q at %d bits: got %d, want %d", tc.value, helper.bits, got, want)
					}
				})
			}
		})
	}
}

func TestLookupEnvIntegerAcceptance(t *testing.T) {
	const name = "CRABBOX_TEST_INTEGER"
	for _, bits := range []int{32, 64} {
		t.Run(strconv.Itoa(bits), func(t *testing.T) {
			for _, tc := range envIntegerCases() {
				t.Run(tc.name, func(t *testing.T) {
					t.Setenv(name, tc.value)
					if tc.name == "unset" {
						if err := os.Unsetenv(name); err != nil {
							t.Fatal(err)
						}
					}
					want, accepted := tc.want64, tc.ok64
					if bits == 32 {
						want, accepted = tc.want32, tc.ok32
					}
					got, ok := lookupEnvInteger(name, bits)
					if ok != accepted || (ok && got != want) {
						t.Fatalf("%q: got (%d,%v), want (%d,%v)", tc.value, got, ok, want, accepted)
					}
				})
			}
		})
	}
}

func TestEnvHelperBranches(t *testing.T) {
	t.Setenv("CRABBOX_INT", "42")
	t.Setenv("CRABBOX_BAD_INT", "oops")
	if got := getenvInt("CRABBOX_INT", 7); got != 42 {
		t.Fatalf("int=%d", got)
	}
	if got := getenvInt("CRABBOX_BAD_INT", 7); got != 7 {
		t.Fatalf("bad int fallback=%d", got)
	}
	if got := getenvInt("CRABBOX_MISSING_INT", 7); got != 7 {
		t.Fatalf("missing int fallback=%d", got)
	}
	t.Setenv("CRABBOX_INT32", "2147483647")
	t.Setenv("CRABBOX_INT32_OVERFLOW", "2147483648")
	if got := getenvInt32("CRABBOX_INT32", 7); got != 2147483647 {
		t.Fatalf("int32=%d", got)
	}
	if got := getenvInt32("CRABBOX_INT32_OVERFLOW", 7); got != 7 {
		t.Fatalf("overflow int32 fallback=%d", got)
	}
	t.Setenv("CRABBOX_FLOAT", "1.5")
	t.Setenv("CRABBOX_BAD_FLOAT", "oops")
	if got := getenvFloat("CRABBOX_FLOAT", 7); got != 1.5 {
		t.Fatalf("float=%f", got)
	}
	if got := getenvFloat("CRABBOX_BAD_FLOAT", 7); got != 7 {
		t.Fatalf("bad float fallback=%f", got)
	}
	if got := getenvFloat("CRABBOX_MISSING_FLOAT", 7); got != 7 {
		t.Fatalf("missing float fallback=%f", got)
	}

	for _, tc := range []struct {
		name  string
		value string
		want  bool
		ok    bool
	}{
		{"CRABBOX_BOOL_TRUE", "yes", true, true},
		{"CRABBOX_BOOL_FALSE", "off", false, true},
		{"CRABBOX_BOOL_BAD", "maybe", false, false},
		{"CRABBOX_BOOL_EMPTY", "", false, false},
	} {
		if tc.value != "" {
			t.Setenv(tc.name, tc.value)
		}
		got, ok := getenvBool(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("getenvBool(%s)=%v,%v want %v,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}

	list := splitCommaList(" CI, ,NODE_OPTIONS,CUSTOM_* ")
	if len(list) != 3 || list[0] != "CI" || list[2] != "CUSTOM_*" {
		t.Fatalf("splitCommaList=%v", list)
	}
	t.Setenv("CRABBOX_LIST", "CI,NODE_OPTIONS")
	if list, ok := getenvList("CRABBOX_LIST"); !ok || len(list) != 2 || list[1] != "NODE_OPTIONS" {
		t.Fatalf("getenvList=%v ok=%t", list, ok)
	}
}

func TestFileProfileEnvConfigUnmarshalRejectsNonMapping(t *testing.T) {
	var cfg struct {
		Env fileProfileEnvConfig `yaml:"env"`
	}
	err := yaml.Unmarshal([]byte("env: []\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "profile env must be a mapping") {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteUserFileConfigPreservesProfileEnvShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "explicit.yaml"))

	path, err := writeUserFileConfig(fileConfig{
		Profiles: map[string]fileProfileConfig{
			"qa": {
				Env: fileProfileEnvConfig{
					Values: map[string]string{"CI": "1"},
					Allow:  []string{"QA_*"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "values:") || !strings.Contains(text, "CI: \"1\"") || !strings.Contains(text, "allow:") {
		t.Fatalf("unexpected profile env YAML:\n%s", text)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	env := file.Profiles["qa"].Env
	if env.Values["CI"] != "1" || strings.Join(env.Allow, ",") != "QA_*" {
		t.Fatalf("env=%#v", env)
	}
}

func TestConfigServerTypeHelperBranches(t *testing.T) {
	if got := incusServerTypeForConfig(Config{}); got != "container" {
		t.Fatalf("incus default=%q", got)
	}
	if got := incusServerTypeForConfig(Config{Incus: IncusConfig{InstanceType: "vm", Image: "images:ubuntu/24.04/cloud"}}); got != "vm:images:ubuntu/24.04/cloud" {
		t.Fatalf("incus vm=%q", got)
	}
	if got := proxmoxServerTypeForConfig(Config{}); got != "template" {
		t.Fatalf("proxmox default=%q", got)
	}
	if got := proxmoxServerTypeForConfig(Config{Proxmox: ProxmoxConfig{TemplateID: 9000}}); got != "template-9000" {
		t.Fatalf("proxmox template=%q", got)
	}
	if got := firecrackerServerTypeForConfig(Config{}); got != "microvm" {
		t.Fatalf("firecracker default=%q", got)
	}
	if got := serverTypeForProviderClass("firecracker", "beast"); got != "microvm" {
		t.Fatalf("firecracker provider class helper=%q", got)
	}
}

func TestApplyFileConfigCloudProviderBranches(t *testing.T) {
	var azSessionsFile fileConfig
	if err := yaml.Unmarshal([]byte("azureDynamicSessions:\n  endpoint: https://pool.env.eastus.azurecontainerapps.io\n  pool: pool\n  apiVersion: 2025-02-02-preview\n  workdir: /workspace/file\n  timeoutSecs: 120\n"), &azSessionsFile); err != nil {
		t.Fatal(err)
	}
	enabled := true
	disabled := false
	cfg := Config{}
	applyFileConfig(&cfg, fileConfig{
		TargetOS:         targetLinux,
		Desktop:          &enabled,
		Browser:          &disabled,
		Code:             &enabled,
		ServerType:       "custom-type",
		CoordinatorToken: "coord-token",
		HostID:           "",
		Broker: &fileBrokerConfig{
			Provider: "aws",
			Access:   &fileAccessConfig{ClientID: "access-id", ClientSecret: "access-secret", Token: "access-token"},
		},
		Hetzner: &fileHetznerConfig{Location: "fsn1", Image: "ubuntu-24.04", SSHKey: "hetzner-key"},
		AWS: &fileAWSConfig{
			Region:          "eu-central-1",
			AMI:             "ami-test",
			SecurityGroupID: "sg-test",
			SubnetID:        "subnet-test",
			InstanceProfile: "profile-test",
			RootGB:          123,
			SSHCIDRs:        []string{"198.51.100.1/32"},
			MacHostID:       "h-mac",
		},
		Azure: &fileAzureConfig{
			SubscriptionID: "sub",
			TenantID:       "tenant",
			ClientID:       "client",
			Location:       "westeurope",
			ResourceGroup:  "rg",
			Image:          "ubuntu",
			OSDisk:         "ephemeral",
			VNet:           "vnet",
			Subnet:         "subnet",
			NSG:            "nsg",
			SSHCIDRs:       []string{"198.51.100.2/32"},
			Network:        "public",
		},
		AzureDynamicSessions: azSessionsFile.AzureDynamicSessions,
		GCP: &fileGCPConfig{
			Project:        "project",
			Zone:           "europe-west1-b",
			Image:          "ubuntu",
			Network:        "net",
			Subnet:         "subnet",
			Tags:           []string{"crabbox"},
			SSHCIDRs:       []string{"198.51.100.3/32"},
			RootGB:         456,
			ServiceAccount: "runner@example.iam.gserviceaccount.com",
		},
	})
	if !cfg.Desktop || cfg.Browser || !cfg.Code || cfg.TargetOS != targetLinux || cfg.ServerType != "custom-type" {
		t.Fatalf("top-level config not applied: %#v", cfg)
	}
	if cfg.Provider != "aws" || cfg.Access.ClientID != "access-id" || cfg.CoordToken != "coord-token" {
		t.Fatalf("broker/access config not applied: provider=%s access=%#v token=%s", cfg.Provider, cfg.Access, cfg.CoordToken)
	}
	if cfg.Location != "fsn1" || cfg.ProviderKey != "hetzner-key" || cfg.HostID != "h-mac" || cfg.AWSRootGB != 123 {
		t.Fatalf("hetzner/aws config not applied: location=%s key=%s host=%s root=%d", cfg.Location, cfg.ProviderKey, cfg.HostID, cfg.AWSRootGB)
	}
	if cfg.AzureOSDisk != "ephemeral" || !cfg.AzureOSDiskExplicit || cfg.AzureNetwork != "public" {
		t.Fatalf("azure config not applied: %#v", cfg)
	}
	if cfg.AzureDynamicSessions.Pool != "pool" || cfg.AzureDynamicSessions.Workdir != "/workspace/file" || cfg.AzureDynamicSessions.TimeoutSecs != 120 {
		t.Fatalf("azure dynamic sessions config not applied: %#v", cfg.AzureDynamicSessions)
	}
	if cfg.GCPProject != "project" || !cfg.gcpProjectExplicit || cfg.GCPRootGB != 456 || cfg.GCPServiceAccount == "" {
		t.Fatalf("gcp config not applied: %#v", cfg)
	}
}

func TestApplyFileJobConfigCoversJobOptions(t *testing.T) {
	enabled := true
	disabled := false
	job := applyFileJobConfig(JobConfig{}, fileJobConfig{
		Provider:     "aws",
		TargetOS:     targetLinux,
		Windows:      &fileWindowsConfig{Mode: windowsModeWSL2},
		Profile:      "ci",
		Class:        "large",
		Architecture: "arm64",
		Type:         "m8i.large",
		Capacity:     &fileCapacityConfig{Market: "spot"},
		Market:       "on-demand",
		TTL:          "45m",
		IdleTimeout:  "5m",
		Desktop:      &enabled,
		Browser:      &disabled,
		Code:         &enabled,
		Network:      "tailscale",
		Hydrate: &fileJobHydrateConfig{
			Actions:          &enabled,
			GitHubRunner:     &enabled,
			WaitTimeout:      "12m",
			KeepAliveMinutes: 3,
		},
		Actions: &fileJobActionsConfig{
			Repo:     "openclaw/crabbox",
			Workflow: ".github/workflows/ci.yml",
			Job:      "test",
			Ref:      "main",
			Fields:   []string{"a=1", "a=1", "b=2"},
		},
		Shell:             &enabled,
		Command:           "pnpm test",
		NoSync:            &enabled,
		SyncOnly:          &disabled,
		Checksum:          &enabled,
		ForceSyncLarge:    &enabled,
		JUnit:             []string{"junit.xml", "junit.xml"},
		Label:             "nightly smoke",
		ArtifactGlobs:     []string{"reports/**", "reports/**"},
		RequiredArtifacts: []string{"reports/summary.json", "reports/summary.json"},
		Downloads:         []string{"out=out", "out=out"},
		Stop:              "always",
	})
	if job.Provider != "aws" || job.Target != targetLinux || job.WindowsMode != windowsModeWSL2 || job.Profile != "ci" || job.Class != "large" || job.Architecture != "arm64" || job.ServerType != "m8i.large" || job.Market != "on-demand" {
		t.Fatalf("basic job fields not applied: %#v", job)
	}
	if job.TTL != 45*time.Minute || job.IdleTimeout != 5*time.Minute {
		t.Fatalf("job durations ttl=%s idle=%s", job.TTL, job.IdleTimeout)
	}
	if job.Desktop == nil || !*job.Desktop || job.Browser == nil || *job.Browser || job.Code == nil || !*job.Code || job.Network != "tailscale" {
		t.Fatalf("job UI/network fields not applied: %#v", job)
	}
	if !job.Hydrate.Actions || !job.Hydrate.GitHubRunner || job.Hydrate.WaitTimeout != 12*time.Minute || job.Hydrate.KeepAliveMinutes != 3 {
		t.Fatalf("hydrate not applied: %#v", job.Hydrate)
	}
	if job.Actions.Repo != "openclaw/crabbox" || job.Actions.Workflow != ".github/workflows/ci.yml" || job.Actions.Job != "test" || job.Actions.Ref != "main" || len(job.Actions.Fields) != 2 {
		t.Fatalf("actions not applied: %#v", job.Actions)
	}
	if !job.Shell || job.Command != "pnpm test" || !job.NoSync || job.SyncOnly || job.Checksum == nil || !*job.Checksum || !job.ForceSyncLarge || len(job.JUnit) != 1 || job.Label != "nightly smoke" || len(job.ArtifactGlobs) != 1 || len(job.RequiredArtifacts) != 1 || len(job.Downloads) != 1 || job.Stop != "always" {
		t.Fatalf("command/sync fields not applied: %#v", job)
	}
}

func TestModalSecretConfigRequiresTrustedFile(t *testing.T) {
	cfg := baseConfig()
	cfg.Modal.Environment = "trusted-env"
	cfg.Modal.Secrets = []string{"sample"}
	var file fileConfig
	if err := yaml.Unmarshal([]byte("modal:\n  environment: repo-env\n  secrets: [example]\n"), &file); err != nil {
		t.Fatal(err)
	}

	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Modal.Environment != "trusted-env" || !reflect.DeepEqual(cfg.Modal.Secrets, []string{"sample"}) {
		t.Fatalf("untrusted Modal secret selection applied: %#v", cfg.Modal)
	}

	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Modal.Environment != "repo-env" || !reflect.DeepEqual(cfg.Modal.Secrets, []string{"example"}) {
		t.Fatalf("trusted Modal secret selection not applied: %#v", cfg.Modal)
	}
}

func TestLumeHostLifecycleConfigRequiresTrustedFile(t *testing.T) {
	cfg := baseConfig()
	var trusted fileConfig
	if err := yaml.Unmarshal([]byte("lume:\n  cliPath: /opt/homebrew/bin/lume\n  base: trusted-golden\n  storage: trusted-storage\n  user: trusted-user\n  workRoot: /Users/trusted-user/work\n"), &trusted); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, trusted, true); err != nil {
		t.Fatal(err)
	}
	var untrusted fileConfig
	if err := yaml.Unmarshal([]byte("lume:\n  cliPath: ./run-me\n  base: credentialed-personal-vm\n  storage: other-storage\n  user: repo-user\n  workRoot: /Users/trusted-user/repo-work\n"), &untrusted); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrust(&cfg, untrusted, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Lume.CLIPath != "/opt/homebrew/bin/lume" || cfg.Lume.Base != "trusted-golden" || cfg.Lume.Storage != "trusted-storage" {
		t.Fatalf("untrusted repo config changed host lifecycle selection: %#v", cfg.Lume)
	}
	if cfg.Lume.User != "trusted-user" || cfg.Lume.WorkRoot != "/Users/trusted-user/repo-work" {
		t.Fatalf("bootstrap user trust boundary was not preserved: %#v", cfg.Lume)
	}
}

func TestCodeSandboxFilePresenceAndTrust(t *testing.T) {
	initial := CodeSandboxConfig{TemplateID: "template", Workdir: "/project/workspace/app", VMTier: "micro", Privacy: "private", HibernationTimeoutSecs: 60, AutomaticWakeupHTTP: true, AutomaticWakeupWebSocket: true, BridgeCommand: "node", SDKPackage: "@codesandbox/sdk", DoctorListLimit: 2, OperationTimeoutSecs: 30}
	keys := []string{"templateId", "workdir", "vmTier", "privacy", "hibernationTimeoutSecs", "automaticWakeupHTTP", "automaticWakeupWebSocket", "bridgeCommand", "sdkPackage", "doctorListLimit", "operationTimeoutSecs"}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "zero"} {
			t.Run(fmt.Sprintf("trusted=%t/%s", trusted, mode), func(t *testing.T) {
				body := "codeSandbox: {}\n"
				if mode != "omitted" {
					body = "codeSandbox:\n"
					for i, key := range keys {
						value := "null"
						if mode == "zero" {
							value = "''"
							if i == 4 || i == 9 || i == 10 {
								value = "0"
							}
							if i == 5 || i == 6 {
								value = "false"
							}
						}
						body += "  " + key + ": " + value + "\n"
					}
				}
				var file fileConfig
				if err := yaml.Unmarshal([]byte(body), &file); err != nil {
					t.Fatal(err)
				}
				before, err := yaml.Marshal(file)
				if err != nil {
					t.Fatal(err)
				}
				cfg := baseConfig()
				cfg.CodeSandbox = initial
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				want := initial
				if mode == "zero" {
					want = CodeSandboxConfig{}
					if !trusted {
						want.BridgeCommand, want.SDKPackage = initial.BridgeCommand, initial.SDKPackage
					}
				}
				if cfg.CodeSandbox != want {
					t.Fatalf("got %#v, want %#v", cfg.CodeSandbox, want)
				}
				after, err := yaml.Marshal(file)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatal("file input mutated")
				}
			})
		}
	}
}

func TestCodeSandboxIntegerOverlayErrorOrder(t *testing.T) {
	keys := []string{"hibernationTimeoutSecs", "doctorListLimit", "operationTimeoutSecs"}
	envs := []string{"CRABBOX_CODESANDBOX_HIBERNATION_TIMEOUT_SECS", "CRABBOX_CODESANDBOX_DOCTOR_LIST_LIMIT", "CRABBOX_CODESANDBOX_OPERATION_TIMEOUT_SECS"}
	for _, source := range []string{"file", "env"} {
		for fail := range keys {
			for _, invalid := range []string{"-1", "invalid"} {
				if source == "file" && invalid == "invalid" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%s", source, keys[fail], invalid), func(t *testing.T) {
					clearConfigEnv(t)
					cfg := baseConfig()
					cfg.CodeSandbox.TemplateID = "before"
					cfg.CodeSandbox.HibernationTimeoutSecs, cfg.CodeSandbox.DoctorListLimit, cfg.CodeSandbox.OperationTimeoutSecs = 10, 20, 30
					cfg.CodeSandbox.AutomaticWakeupHTTP = true
					cfg.CodeSandbox.BridgeCommand = "before-node"
					body := "codeSandbox:\n  templateId: after\n  automaticWakeupHTTP: false\n  bridgeCommand: after-node\n"
					t.Setenv("CRABBOX_CODESANDBOX_TEMPLATE_ID", "after")
					t.Setenv("CRABBOX_CODESANDBOX_AUTOMATIC_WAKEUP_HTTP", "false")
					t.Setenv("CRABBOX_CODESANDBOX_BRIDGE_COMMAND", "after-node")
					for i, key := range keys {
						value := "7"
						if i >= fail {
							value = invalid
						}
						body += "  " + key + ": " + value + "\n"
						t.Setenv(envs[i], value)
					}
					var err error
					wantError := "codesandbox " + keys[fail] + " must be non-negative"
					if source == "file" {
						var file fileConfig
						if err := yaml.Unmarshal([]byte(body), &file); err != nil {
							t.Fatal(err)
						}
						err = applyFileConfig(&cfg, file)
					} else {
						err = applyEnv(&cfg)
						wantError = envs[fail] + " must be non-negative"
						if invalid == "invalid" {
							wantError = envs[fail] + " must be an integer"
						}
					}
					if err == nil || err.Error() != wantError {
						t.Fatalf("error=%v, want %q", err, wantError)
					}
					wantInts := []int{10, 20, 30}
					for i := 0; i < fail; i++ {
						wantInts[i] = 7
					}
					// Environment assignment stores the parser's zero result before returning its error.
					if source == "env" {
						wantInts[fail] = 0
					}
					gotInts := []int{cfg.CodeSandbox.HibernationTimeoutSecs, cfg.CodeSandbox.DoctorListLimit, cfg.CodeSandbox.OperationTimeoutSecs}
					if !reflect.DeepEqual(gotInts, wantInts) {
						t.Fatalf("integers=%v, want %v", gotInts, wantInts)
					}
					wantBridge := "before-node"
					if fail > 0 {
						wantBridge = "after-node"
					}
					if cfg.CodeSandbox.TemplateID != "after" || cfg.CodeSandbox.BridgeCommand != wantBridge || cfg.CodeSandbox.AutomaticWakeupHTTP != (fail == 0) {
						t.Fatalf("partial update=%#v", cfg.CodeSandbox)
					}
				})
			}
		}
	}
}

func TestTensorlakeConfigFileContract(t *testing.T) {
	if got, want := baseConfig().Tensorlake, (TensorlakeConfig{APIURL: "https://api.tensorlake.ai", CLIPath: "tensorlake", Workdir: "/workspace/crabbox", CPUs: 1, MemoryMB: 1024, DiskMB: 10240}); got != want {
		t.Fatalf("defaults=%#v want %#v", got, want)
	}
	if _, ok := reflect.TypeOf(fileTensorlakeConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("APIKey must remain env-only")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace"} {
			cfg := baseConfig()
			cfg.Tensorlake.APIKey = "inert"
			cfg.Tensorlake.Image = "prior-image"
			cfg.Tensorlake.Snapshot = "prior-snapshot"
			cfg.Tensorlake.OrganizationID = "prior-org"
			cfg.Tensorlake.ProjectID = "prior-project"
			cfg.Tensorlake.Namespace = "prior-namespace"
			cfg.credentialProvenance.tensorlakeAPIURL = credentialSourceFlag
			cfg.credentialProvenance.tensorlakeAPIKey = credentialSourceFlag
			want := cfg.Tensorlake
			source := credentialSourceFlag
			fields := map[string]any{"apiKey": "ignored"}
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiUrl", &want.APIURL}, {"cliPath", &want.CLIPath}, {"image", &want.Image}, {"snapshot", &want.Snapshot}, {"organizationId", &want.OrganizationID}, {"projectId", &want.ProjectID}, {"namespace", &want.Namespace}, {"workdir", &want.Workdir}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
					*f.v = "  "
				}
				fields[f.key] = raw
			}
			if mode == "equal" || mode == "whitespace" {
				source = credentialSourceForFile(trusted)
			}
			data, err := yaml.Marshal(map[string]any{"tensorlake": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Tensorlake != want || cfg.credentialProvenance.tensorlakeAPIURL != source || cfg.credentialProvenance.tensorlakeAPIKey != credentialSourceFlag {
				t.Fatalf("mode=%s trusted=%t got=%#v", mode, trusted, cfg.Tensorlake)
			}
		}
		for _, raw := range []string{"null", "0", "-2", "2"} {
			cfg := baseConfig()
			cfg.Tensorlake.TimeoutSecs = 45
			cfg.Tensorlake.NoInternet = true
			want := cfg.Tensorlake
			want.NoInternet = false
			if raw == "2" {
				want.CPUs = 2
				want.MemoryMB = 2
				want.DiskMB = 2
				want.TimeoutSecs = 2
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte(fmt.Sprintf("tensorlake:\n  cpus: %s\n  memoryMB: %s\n  diskMB: %s\n  timeoutSecs: %s\n  noInternet: false\n", raw, raw, raw, raw)), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Tensorlake != want {
				t.Fatalf("raw=%s got=%#v want=%#v", raw, cfg.Tensorlake, want)
			}
		}
		for _, raw := range []string{"0.25", "-0.25"} {
			cfg := baseConfig()
			var file fileConfig
			if err := yaml.Unmarshal([]byte("tensorlake:\n  cpus: "+raw+"\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			want := 1.0
			if raw == "0.25" {
				want = 0.25
			}
			if cfg.Tensorlake.CPUs != want {
				t.Fatalf("fraction %s got %v", raw, cfg.Tensorlake.CPUs)
			}
		}
	}
}

func TestTensorlakeConfigEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "equal", "whitespace", "API_KEY", "API_URL"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Tensorlake.APIKey = "inert"
		want := cfg.Tensorlake
		cfg.credentialProvenance.tensorlakeAPIKey = credentialSourceFlag
		cfg.credentialProvenance.tensorlakeAPIURL = credentialSourceFlag
		accepted := map[string]bool{}
		for _, f := range []struct {
			suffix, alias string
			v             *string
		}{{"API_KEY", "TENSORLAKE_API_KEY", &want.APIKey}, {"API_URL", "TENSORLAKE_API_URL", &want.APIURL}, {"CLI", "", &want.CLIPath}, {"IMAGE", "", &want.Image}, {"SNAPSHOT", "", &want.Snapshot}, {"ORGANIZATION_ID", "TENSORLAKE_ORGANIZATION_ID", &want.OrganizationID}, {"PROJECT_ID", "TENSORLAKE_PROJECT_ID", &want.ProjectID}, {"NAMESPACE", "INDEXIFY_NAMESPACE", &want.Namespace}, {"WORKDIR", "", &want.Workdir}} {
			primary, alias := "primary-value", "alias-value"
			if mode == "equal" {
				primary = *f.v
			}
			if mode == "whitespace" {
				primary = "  "
			}
			allow := mode != "empty" && ((mode != "API_KEY" && mode != "API_URL") || mode == f.suffix)
			if mode == "alias" {
				primary = ""
				allow = f.alias != ""
			}
			if !allow {
				primary = ""
				alias = ""
			}
			if primary != "" {
				*f.v = primary
			} else if f.alias != "" && alias != "" {
				*f.v = alias
			}
			accepted[f.suffix] = primary != "" || (f.alias != "" && alias != "")
			t.Setenv("CRABBOX_TENSORLAKE_"+f.suffix, primary)
			if f.alias != "" {
				t.Setenv(f.alias, alias)
			}
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		key, url := credentialSourceFlag, credentialSourceFlag
		if accepted["API_KEY"] {
			key = credentialSourceEnvironment
		}
		if accepted["API_URL"] {
			url = credentialSourceEnvironment
		}
		if cfg.Tensorlake != want || cfg.credentialProvenance.tensorlakeAPIKey != key || cfg.credentialProvenance.tensorlakeAPIURL != url {
			t.Fatalf("mode=%s got=%#v want=%#v", mode, cfg.Tensorlake, want)
		}
	}
	for _, raw := range []string{"", "invalid", "0", "-2", "3"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Tensorlake.TimeoutSecs = 45
		cfg.Tensorlake.NoInternet = true
		want := cfg.Tensorlake
		for _, suffix := range []string{"CPUS", "MEMORY_MB", "DISK_MB", "TIMEOUT_SECS"} {
			t.Setenv("CRABBOX_TENSORLAKE_"+suffix, raw)
		}
		if n, err := strconv.Atoi(raw); err == nil {
			want.CPUs = float64(n)
			want.MemoryMB = n
			want.DiskMB = n
			want.TimeoutSecs = n
		}
		t.Setenv("CRABBOX_TENSORLAKE_NO_INTERNET", raw)
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if raw == "0" {
			want.NoInternet = false
		}
		if cfg.Tensorlake != want {
			t.Fatalf("raw=%s got=%#v want=%#v", raw, cfg.Tensorlake, want)
		}
	}
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("CRABBOX_TENSORLAKE_CPUS", "0.25")
	t.Setenv("CRABBOX_TENSORLAKE_NO_INTERNET", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Tensorlake.CPUs != 0.25 || cfg.Tensorlake.NoInternet {
		t.Fatal("fractional CPU/false env lost")
	}
}

func TestTensorlakeConfigCentralFlagSource(t *testing.T) {
	cfg := baseConfig()
	cfg.credentialProvenance.tensorlakeAPIKey = credentialSourceEnvironment
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.String("tensorlake-api-url", "", "")
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.tensorlakeAPIURL == credentialSourceFlag {
		t.Fatal("unvisited URL marked")
	}
	if err := fs.Parse([]string{"--tensorlake-api-url="}); err != nil {
		t.Fatal(err)
	}
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.tensorlakeAPIURL != credentialSourceFlag || cfg.credentialProvenance.tensorlakeAPIKey != credentialSourceEnvironment {
		t.Fatal("central source phase changed")
	}
}

func TestOrgoConfigFileContract(t *testing.T) {
	wantDefaults := OrgoConfig{APIBase: "https://www.orgo.ai/api", RAMGB: 4, CPUs: 1, DiskGB: 8, Resolution: "1280x720x24"}
	if got := baseConfig().Orgo; got != wantDefaults {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Orgo.APIKey = "inert-prior"
			cfg.Orgo.WorkspaceID = "prior-workspace"
			want := cfg.Orgo
			cfg.credentialProvenance.orgoAPIKey = credentialSourceFlag
			cfg.credentialProvenance.orgoAPIBase = credentialSourceFlag
			fields := map[string]any{}
			key, base := credentialSourceFlag, credentialSourceFlag
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiKey", &want.APIKey}, {"apiBase", &want.APIBase}, {"workspaceID", &want.WorkspaceID}, {"resolution", &want.Resolution}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				fields[f.key] = raw
				if mode == "equal" || mode == "whitespace" || mode == "value" {
					if f.key != "apiKey" || trusted {
						*f.v = raw.(string)
					}
					if f.key == "apiKey" && trusted {
						key = credentialSourceForFile(trusted)
					}
					if f.key == "apiBase" {
						base = credentialSourceForFile(trusted)
					}
				}
			}
			data, err := yaml.Marshal(map[string]any{"orgo": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Orgo != want || cfg.credentialProvenance.orgoAPIKey != key || cfg.credentialProvenance.orgoAPIBase != base {
				t.Fatalf("mode=%s trusted=%t got=%#v want=%#v", mode, trusted, cfg.Orgo, want)
			}
		}
		for _, raw := range []string{"null", "0", "-2", "3"} {
			cfg := baseConfig()
			want := cfg.Orgo
			if raw == "3" {
				want.RAMGB = 3
				want.CPUs = 3
				want.DiskGB = 3
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte(fmt.Sprintf("orgo:\n  ramGB: %s\n  cpus: %s\n  diskGB: %s\n", raw, raw, raw)), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Orgo != want {
				t.Fatalf("raw=%s trusted=%t got=%#v want=%#v", raw, trusted, cfg.Orgo, want)
			}
		}
	}
}

func TestOrgoConfigKeyEnvironmentContract(t *testing.T) {
	for _, tc := range []struct {
		name, primary, configured, alias, want string
		applied                                bool
	}{
		{"primary", "inert-primary", "inert-config", "inert-vendor", "inert-primary", true},
		{"configured", "", "inert-config", "inert-vendor", "inert-config", false},
		{"vendor", "", "", "inert-vendor", "inert-vendor", true},
		{"absent", "", "", "", "", false},
		{"equal-primary", "inert-config", "inert-config", "inert-vendor", "inert-config", true},
		{"equal-vendor-ignored", "", "inert-config", "inert-config", "inert-config", false},
		{"raw-configured", "", "  ", "inert-vendor", "  ", false},
		{"raw-primary", "  ", "inert-config", "inert-vendor", "  ", true},
		{"raw-vendor", "", "", "  ", "  ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Orgo.APIKey = tc.configured
			cfg.credentialProvenance.orgoAPIKey = credentialSourceTrustedFile
			t.Setenv("CRABBOX_ORGO_API_KEY", tc.primary)
			t.Setenv("ORGO_API_KEY", tc.alias)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			source := credentialSourceTrustedFile
			if tc.applied {
				source = credentialSourceEnvironment
			}
			if cfg.Orgo.APIKey != tc.want || cfg.credentialProvenance.orgoAPIKey != source {
				t.Fatalf("key/source mismatch for %s", tc.name)
			}
		})
	}
}

func TestOrgoConfigEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "equal", "whitespace"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		cfg.Orgo.WorkspaceID = "prior-workspace"
		want := cfg.Orgo
		cfg.credentialProvenance.orgoAPIBase = credentialSourceTrustedFile
		source := credentialSourceTrustedFile
		for _, f := range []struct {
			suffix, alias string
			v             *string
		}{{"API_BASE", "ORGO_API_BASE_URL", &want.APIBase}, {"WORKSPACE_ID", "ORGO_WORKSPACE_ID", &want.WorkspaceID}, {"RESOLUTION", "", &want.Resolution}} {
			primary, alias := "primary-value", "alias-value"
			if mode == "alias" {
				primary = ""
			}
			if mode == "empty" {
				primary = ""
				alias = ""
			}
			if mode == "equal" {
				primary = *f.v
			}
			if mode == "whitespace" {
				primary = "  "
			}
			if primary != "" {
				*f.v = primary
			} else if f.alias != "" && alias != "" {
				*f.v = alias
			}
			if f.suffix == "API_BASE" && (primary != "" || alias != "") {
				source = credentialSourceEnvironment
			}
			t.Setenv("CRABBOX_ORGO_"+f.suffix, primary)
			if f.alias != "" {
				t.Setenv(f.alias, alias)
			}
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Orgo != want || cfg.credentialProvenance.orgoAPIBase != source {
			t.Fatalf("mode=%s got=%#v want=%#v", mode, cfg.Orgo, want)
		}
	}
	for _, key := range []string{"CRABBOX_ORGO_API_BASE", "ORGO_API_BASE_URL", "CRABBOX_ORGO_WORKSPACE_ID", "ORGO_WORKSPACE_ID", "CRABBOX_ORGO_RESOLUTION"} {
		t.Setenv(key, "")
	}
	for _, raw := range []string{"", "invalid", " 2 ", "0", "-2", "3"} {
		clearConfigEnv(t)
		cfg := baseConfig()
		want := cfg.Orgo
		for _, s := range []string{"RAM_GB", "CPUS", "DISK_GB"} {
			t.Setenv("CRABBOX_ORGO_"+s, raw)
		}
		if n, err := strconv.Atoi(raw); err == nil {
			want.RAMGB = n
			want.CPUs = n
			want.DiskGB = n
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Orgo != want {
			t.Fatalf("raw=%q got=%#v want=%#v", raw, cfg.Orgo, want)
		}
	}
}

func TestOrgoConfigCentralFlagSource(t *testing.T) {
	cfg := baseConfig()
	cfg.credentialProvenance.orgoAPIKey = credentialSourceTrustedFile
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.String("orgo-api-base", "", "")
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.orgoAPIBase == credentialSourceFlag {
		t.Fatal("unvisited base marked")
	}
	if err := fs.Parse([]string{"--orgo-api-base="}); err != nil {
		t.Fatal(err)
	}
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.orgoAPIBase != credentialSourceFlag || cfg.credentialProvenance.orgoAPIKey != credentialSourceTrustedFile {
		t.Fatal("central source phase changed")
	}
}

func TestOpenComputerConfigFileContract(t *testing.T) {
	wantDefaults := OpenComputerConfig{Workdir: "/workspace/crabbox", ExecTimeoutSecs: 3600}
	if got := baseConfig().OpenComputer; got != wantDefaults {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	if reflect.TypeOf(OpenComputerConfig{}).NumField() != 8 || reflect.TypeOf(fileOpenComputerConfig{}).NumField() != 6 {
		t.Fatal("configuration field count changed")
	}
	for _, name := range []string{"APIKey", "APIURL", "ForgetMissing"} {
		if _, ok := reflect.TypeOf(fileOpenComputerConfig{}).FieldByName(name); ok {
			t.Fatalf("unexpected file field %s", name)
		}
	}
	if _, ok := reflect.TypeOf(OpenComputerConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("API key config field introduced")
	}
	for _, trusted := range []bool{false, true} {
		for _, raw := range []string{"omitted", "null", "0", "-2", "3"} {
			cfg := baseConfig()
			cfg.OpenComputer = OpenComputerConfig{APIURL: "prior-url", Workdir: "/workspace/prior", CPU: 8, MemoryMB: 1024, TimeoutSecs: 45, ExecTimeoutSecs: 90}
			want := cfg.OpenComputer
			fields := map[string]any{"apiUrl": "ignored-file-url", "apiKey": "ignored-inert-key", "forgetMissing": true}
			for _, f := range []struct {
				key string
				v   *int
			}{{"cpu", &want.CPU}, {"memoryMB", &want.MemoryMB}, {"timeoutSecs", &want.TimeoutSecs}, {"execTimeoutSecs", &want.ExecTimeoutSecs}} {
				if raw == "omitted" {
					continue
				}
				if raw == "null" {
					fields[f.key] = nil
					continue
				}
				n, err := strconv.Atoi(raw)
				if err != nil {
					t.Fatal(err)
				}
				fields[f.key] = n
				*f.v = n
			}
			data, err := yaml.Marshal(map[string]any{"openComputer": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.OpenComputer != want {
				t.Fatalf("raw=%s trusted=%t got=%#v want=%#v", raw, trusted, cfg.OpenComputer, want)
			}
		}
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.OpenComputer.Workdir = "/workspace/prior"
			cfg.OpenComputer.Burst = true
			want := cfg.OpenComputer
			fields := map[string]any{}
			if mode != "omitted" {
				var workdir any = want.Workdir
				var burst any = false
				if mode == "null" {
					workdir = nil
					burst = nil
				}
				if mode == "empty" {
					workdir = ""
				}
				if mode == "whitespace" {
					workdir = "  "
				}
				if mode == "value" {
					workdir = "/workspace/value"
				}
				fields["workdir"] = workdir
				fields["burst"] = burst
				if mode != "null" {
					want.Burst = false
					if workdir != "" {
						want.Workdir = workdir.(string)
					}
				}
			}
			data, err := yaml.Marshal(map[string]any{"openComputer": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.OpenComputer != want {
				t.Fatalf("mode=%s trusted=%t got=%#v want=%#v", mode, trusted, cfg.OpenComputer, want)
			}
		}
	}
}

func TestOpenComputerConfigEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"primary", "alias", "empty", "equal", "whitespace"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.OpenComputer.APIURL = "prior-url"
			want := cfg.OpenComputer
			primary, alias, workdir := "primary-url", "alias-url", "/workspace/env"
			if mode == "alias" {
				primary = ""
				workdir = ""
			}
			if mode == "empty" {
				primary = ""
				alias = ""
				workdir = ""
			}
			if mode == "equal" {
				primary = want.APIURL
				workdir = want.Workdir
			}
			if mode == "whitespace" {
				primary = "  "
				workdir = "  "
			}
			t.Setenv("CRABBOX_OPENCOMPUTER_API_URL", primary)
			t.Setenv("OPENCOMPUTER_API_URL", alias)
			t.Setenv("CRABBOX_OPENCOMPUTER_WORKDIR", workdir)
			t.Setenv("CRABBOX_OPENCOMPUTER_FORGET_MISSING", "true")
			if primary != "" {
				want.APIURL = primary
			} else if alias != "" {
				want.APIURL = alias
			}
			if workdir != "" {
				want.Workdir = workdir
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.OpenComputer != want {
				t.Fatalf("mode=%s got=%#v want=%#v", mode, cfg.OpenComputer, want)
			}
		})
	}
	for _, raw := range []string{"", "invalid", " 2 ", "0", "-2", "3"} {
		t.Run("numbers-"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.OpenComputer.CPU = 8
			cfg.OpenComputer.MemoryMB = 1024
			cfg.OpenComputer.TimeoutSecs = 45
			want := cfg.OpenComputer
			for _, f := range []struct {
				suffix string
				v      *int
			}{{"CPU", &want.CPU}, {"MEMORY_MB", &want.MemoryMB}, {"TIMEOUT_SECS", &want.TimeoutSecs}, {"EXEC_TIMEOUT_SECS", &want.ExecTimeoutSecs}} {
				t.Setenv("CRABBOX_OPENCOMPUTER_"+f.suffix, raw)
				if n, err := strconv.Atoi(raw); err == nil {
					*f.v = n
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.OpenComputer != want {
				t.Fatalf("raw=%q got=%#v want=%#v", raw, cfg.OpenComputer, want)
			}
		})
	}
	for _, tc := range []struct {
		raw           string
		initial, want bool
	}{{"", true, true}, {"invalid", true, true}, {"false", true, false}, {"true", false, true}, {"0", true, false}} {
		t.Run("burst-"+tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.OpenComputer.Burst = tc.initial
			t.Setenv("CRABBOX_OPENCOMPUTER_BURST", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.OpenComputer.Burst != tc.want {
				t.Fatalf("burst raw=%q got=%t", tc.raw, cfg.OpenComputer.Burst)
			}
		})
	}
}

func TestModalConfigFileContract(t *testing.T) {
	wantDefaults := ModalConfig{App: "crabbox", Image: "python:3.13-slim", Workdir: "/workspace/crabbox", Python: "python3"}
	if got := baseConfig().Modal; !reflect.DeepEqual(got, wantDefaults) {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	if reflect.TypeOf(ModalConfig{}).NumField() != 6 || reflect.TypeOf(fileModalConfig{}).NumField() != 6 {
		t.Fatal("config field count changed")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Modal.Environment = "prior-environment"
			want := cfg.Modal
			fields := map[string]any{}
			for _, f := range []struct {
				key string
				v   *string
			}{{"app", &want.App}, {"image", &want.Image}, {"workdir", &want.Workdir}, {"python", &want.Python}, {"environment", &want.Environment}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				fields[f.key] = raw
				if (mode == "equal" || mode == "whitespace" || mode == "value") && (trusted || f.key != "environment") {
					*f.v = raw.(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"modal": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal, want) {
				t.Fatalf("mode=%s trusted=%t got=%#v want=%#v", mode, trusted, cfg.Modal, want)
			}
		}
		for _, tc := range []struct {
			name, yaml string
			want       []string
		}{{"omitted", "modal: {}\n", []string{"prior"}}, {"null", "modal:\n  secrets: null\n", []string{"prior"}}, {"empty", "modal:\n  secrets: []\n", nil}, {"raw", "modal:\n  secrets: [' alpha ', '', alpha, ' ', beta, alpha]\n", []string{" alpha ", "", "alpha", " ", "beta", "alpha"}}} {
			cfg := baseConfig()
			cfg.Modal.Secrets = []string{"prior"}
			var file fileConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if !trusted {
				want = []string{"prior"}
			}
			if !reflect.DeepEqual(cfg.Modal.Secrets, want) {
				t.Fatalf("list=%s trusted=%t got=%#v want=%#v", tc.name, trusted, cfg.Modal.Secrets, want)
			}
			if trusted && tc.name == "raw" {
				source := reflect.ValueOf(file.Modal).Elem().FieldByName("Secrets")
				if source.Kind() == reflect.Pointer {
					source = source.Elem()
				}
				source.Index(0).SetString("source-mutated")
				if cfg.Modal.Secrets[0] != " alpha " {
					t.Fatal("file list aliases config")
				}
				cfg.Modal.Secrets[1] = "config-mutated"
				if source.Index(1).String() != "" {
					t.Fatal("config aliases file list")
				}
			}
		}
	}
}

func TestModalConfigEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"empty", "equal", "whitespace", "value"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Modal.Environment = "prior-environment"
			want := cfg.Modal
			for _, f := range []struct {
				suffix string
				v      *string
			}{{"APP", &want.App}, {"IMAGE", &want.Image}, {"WORKDIR", &want.Workdir}, {"PYTHON", &want.Python}, {"ENVIRONMENT", &want.Environment}} {
				raw := *f.v
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				t.Setenv("CRABBOX_MODAL_"+f.suffix, raw)
				if raw != "" {
					*f.v = raw
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal, want) {
				t.Fatalf("mode=%s got=%#v want=%#v", mode, cfg.Modal, want)
			}
		})
	}
	for _, tc := range []struct {
		name, raw string
		present   bool
		want      []string
	}{{"absent", "", false, []string{"prior"}}, {"empty", "", true, []string{}}, {"none", " NoNe ", true, []string{}}, {"blanks", " , , ", true, []string{}}, {"ordered", " alpha, ,beta,alpha ", true, []string{"alpha", "beta", "alpha"}}} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Modal.Secrets = []string{"prior"}
			t.Setenv("CRABBOX_MODAL_SECRETS", tc.raw)
			if !tc.present {
				if err := os.Unsetenv("CRABBOX_MODAL_SECRETS"); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal.Secrets, tc.want) {
				t.Fatalf("list=%s got=%#v want=%#v", tc.name, cfg.Modal.Secrets, tc.want)
			}
		})
	}
}

func TestModalConfigServerTypeFallback(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"", "python:3.13-slim"}, {"  ", "  "}, {" custom-image ", " custom-image "}} {
		cfg := baseConfig()
		cfg.Provider = "modal"
		cfg.Modal.Image = tc.raw
		if got := serverTypeForConfig(cfg); got != tc.want {
			t.Fatalf("serverType=%q want=%q", got, tc.want)
		}
	}
}

func TestMorphConfigFileContract(t *testing.T) {
	wantDefaults := MorphConfig{APIURL: "https://cloud.morph.so", SSHGatewayHost: "ssh.cloud.morph.so", WorkRoot: "/tmp/crabbox", WakeOnSSH: true}
	if got := baseConfig().Morph; got != wantDefaults {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Morph.APIKey = "inert-prior"
			cfg.Morph.Snapshot = "prior-snapshot"
			want := cfg.Morph
			source := credentialSourceFlag
			cfg.credentialProvenance.morphAPIKey = source
			cfg.credentialProvenance.morphAPIURL = source
			cfg.credentialProvenance.morphSSHGatewayHost = source
			fields := map[string]any{}
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiKey", &want.APIKey}, {"apiUrl", &want.APIURL}, {"snapshot", &want.Snapshot}, {"sshGatewayHost", &want.SSHGatewayHost}, {"workRoot", &want.WorkRoot}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				fields[f.key] = raw
				if mode == "equal" || mode == "whitespace" || mode == "value" {
					*f.v = raw.(string)
					source = credentialSourceForFile(trusted)
				}
			}
			data, err := yaml.Marshal(map[string]any{"morph": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Morph != want || cfg.credentialProvenance.morphAPIKey != source || cfg.credentialProvenance.morphAPIURL != source || cfg.credentialProvenance.morphSSHGatewayHost != source {
				t.Fatalf("file mode=%s trusted=%t", mode, trusted)
			}
		}
		for _, raw := range []string{"omitted", "null", "false", "true"} {
			cfg := baseConfig()
			cfg.Morph.DeleteOnRelease = false
			cfg.Morph.WakeOnSSH = true
			want := cfg.Morph
			data := "morph: {}\n"
			if raw != "omitted" {
				data = "morph:\n  deleteOnRelease: " + raw + "\n  wakeOnSSH: " + raw + "\n"
			}
			if raw == "false" {
				want.WakeOnSSH = false
			}
			if raw == "true" {
				want.DeleteOnRelease = true
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte(data), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Morph != want || DeleteOnReleaseExplicit(cfg, "morph") != (raw == "false" || raw == "true") {
				t.Fatalf("bool file raw=%s trusted=%t", raw, trusted)
			}
		}
	}
}

func TestMorphConfigEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"empty", "alias", "equal", "whitespace", "value"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Morph.APIKey = "inert-prior"
			cfg.Morph.Snapshot = "prior-snapshot"
			want := cfg.Morph
			prior := credentialSourceTrustedFile
			cfg.credentialProvenance.morphAPIKey = prior
			cfg.credentialProvenance.morphAPIURL = prior
			cfg.credentialProvenance.morphSSHGatewayHost = prior
			accepted := map[string]bool{}
			for _, f := range []struct {
				suffix string
				v      *string
			}{{"API_KEY", &want.APIKey}, {"API_URL", &want.APIURL}, {"SNAPSHOT", &want.Snapshot}, {"SSH_GATEWAY_HOST", &want.SSHGatewayHost}, {"WORK_ROOT", &want.WorkRoot}} {
				raw := *f.v
				if mode == "empty" || mode == "alias" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				t.Setenv("CRABBOX_MORPH_"+f.suffix, raw)
				if raw != "" {
					*f.v = raw
					accepted[f.suffix] = true
				}
			}
			t.Setenv("MORPH_API_KEY", "")
			if mode == "alias" || mode == "value" || mode == "whitespace" {
				t.Setenv("MORPH_API_KEY", "inert-alias")
				if mode == "alias" {
					want.APIKey = "inert-alias"
					accepted["API_KEY"] = true
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			key, url, host := prior, prior, prior
			if accepted["API_KEY"] {
				key = credentialSourceEnvironment
			}
			if accepted["API_URL"] {
				url = credentialSourceEnvironment
			}
			if accepted["SSH_GATEWAY_HOST"] {
				host = credentialSourceEnvironment
			}
			if cfg.Morph != want || cfg.credentialProvenance.morphAPIKey != key || cfg.credentialProvenance.morphAPIURL != url || cfg.credentialProvenance.morphSSHGatewayHost != host {
				t.Fatalf("env mode=%s", mode)
			}
		})
	}
	for _, raw := range []string{"", "invalid", "false", "true"} {
		t.Run("bool-"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			want := cfg.Morph
			t.Setenv("CRABBOX_MORPH_DELETE_ON_RELEASE", raw)
			t.Setenv("CRABBOX_MORPH_WAKE_ON_SSH", raw)
			if raw == "false" {
				want.WakeOnSSH = false
			}
			if raw == "true" {
				want.DeleteOnRelease = true
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Morph != want || DeleteOnReleaseExplicit(cfg, "morph") != (raw == "false" || raw == "true") {
				t.Fatalf("bool env raw=%s", raw)
			}
		})
	}
}

func TestMorphConfigCentralFlagSources(t *testing.T) {
	cfg := baseConfig()
	cfg.credentialProvenance.morphAPIKey = credentialSourceEnvironment
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.String("morph-api-url", "", "")
	fs.String("morph-ssh-gateway-host", "", "")
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.morphAPIURL == credentialSourceFlag || cfg.credentialProvenance.morphSSHGatewayHost == credentialSourceFlag {
		t.Fatal("unvisited marked")
	}
	if err := fs.Parse([]string{"--morph-api-url=", "--morph-ssh-gateway-host="}); err != nil {
		t.Fatal(err)
	}
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.morphAPIURL != credentialSourceFlag || cfg.credentialProvenance.morphSSHGatewayHost != credentialSourceFlag || cfg.credentialProvenance.morphAPIKey != credentialSourceEnvironment {
		t.Fatal("central sources changed")
	}
}

func TestMorphConfigIndependentSources(t *testing.T) {
	for _, tc := range []struct{ key, env string }{{"apiKey", "API_KEY"}, {"apiUrl", "API_URL"}, {"sshGatewayHost", "SSH_GATEWAY_HOST"}} {
		for _, mode := range []string{"user", "repo", "env"} {
			t.Run(tc.key+"-"+mode, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				cfg.credentialProvenance.morphAPIKey = credentialSourceFlag
				cfg.credentialProvenance.morphAPIURL = credentialSourceFlag
				cfg.credentialProvenance.morphSSHGatewayHost = credentialSourceFlag
				var source credentialValueSource
				if mode == "env" {
					t.Setenv("CRABBOX_MORPH_"+tc.env, "fixture")
					source = credentialSourceEnvironment
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					var file fileConfig
					if err := yaml.Unmarshal([]byte("morph:\n  "+tc.key+": fixture\n"), &file); err != nil {
						t.Fatal(err)
					}
					source = credentialSourceForFile(mode == "user")
					if err := applyFileConfigWithTrust(&cfg, file, mode == "user"); err != nil {
						t.Fatal(err)
					}
				}
				for key, got := range map[string]credentialValueSource{"apiKey": cfg.credentialProvenance.morphAPIKey, "apiUrl": cfg.credentialProvenance.morphAPIURL, "sshGatewayHost": cfg.credentialProvenance.morphSSHGatewayHost} {
					want := credentialSourceFlag
					if key == tc.key {
						want = source
					}
					if got != want {
						t.Fatalf("source %s=%v want=%v", key, got, want)
					}
				}
			})
		}
	}
}

func TestExeDevConfigFileContract(t *testing.T) {
	wantDefaults := ExeDevConfig{ControlHost: "exe.dev", CPUs: 2, Memory: "4GB", Disk: "10GB", NoEmail: true}
	if got := baseConfig().ExeDev; got != wantDefaults {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	if reflect.TypeOf(ExeDevConfig{}).NumField() != 9 || reflect.TypeOf(fileExeDevConfig{}).NumField() != 9 {
		t.Fatal("config field count changed")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.ExeDev.Image = "prior-image"
			cfg.ExeDev.Command = "prior-command"
			cfg.ExeDev.User = "prior-user"
			cfg.ExeDev.WorkRoot = "/prior/root"
			want := cfg.ExeDev
			source := credentialSourceFlag
			cfg.credentialProvenance.exeDevControlHost = source
			fields := map[string]any{}
			for _, f := range []struct {
				key string
				v   *string
			}{{"controlHost", &want.ControlHost}, {"image", &want.Image}, {"memory", &want.Memory}, {"disk", &want.Disk}, {"command", &want.Command}, {"user", &want.User}, {"workRoot", &want.WorkRoot}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				fields[f.key] = raw
				if mode == "equal" || mode == "whitespace" || mode == "value" {
					*f.v = raw.(string)
					source = credentialSourceForFile(trusted)
				}
			}
			data, err := yaml.Marshal(map[string]any{"exeDev": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.ExeDev != want || cfg.credentialProvenance.exeDevControlHost != source {
				t.Fatalf("file mode=%s trusted=%t", mode, trusted)
			}
		}
		for _, raw := range []string{"null", "0", "-2", "3"} {
			cfg := baseConfig()
			cfg.ExeDev.CPUs = 6
			want := cfg.ExeDev
			want.NoEmail = false
			if raw == "3" {
				want.CPUs = 3
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte("exeDev:\n  cpus: "+raw+"\n  noEmail: false\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.ExeDev != want {
				t.Fatalf("CPU=%s trusted=%t got=%#v want=%#v", raw, trusted, cfg.ExeDev, want)
			}
		}
		for _, raw := range []string{"omitted", "null", "true"} {
			cfg := baseConfig()
			cfg.ExeDev.NoEmail = false
			var file fileConfig
			body := "exeDev: {}\n"
			if raw != "omitted" {
				body = "exeDev:\n  noEmail: " + raw + "\n"
			}
			if err := yaml.Unmarshal([]byte(body), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.ExeDev.NoEmail != (raw == "true") {
				t.Fatalf("noEmail raw=%s", raw)
			}
		}
	}
}

func TestExeDevConfigEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"empty", "alias", "equal", "whitespace", "value"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.ExeDev.Image = "prior-image"
			cfg.ExeDev.Command = "prior-command"
			cfg.ExeDev.User = "prior-user"
			cfg.ExeDev.WorkRoot = "/prior/root"
			want := cfg.ExeDev
			source := credentialSourceFlag
			cfg.credentialProvenance.exeDevControlHost = source
			for _, f := range []struct {
				suffix, alias string
				v             *string
			}{{"CONTROL_HOST", "EXE_DEV_CONTROL_HOST", &want.ControlHost}, {"IMAGE", "EXE_DEV_IMAGE", &want.Image}, {"MEMORY", "EXE_DEV_MEMORY", &want.Memory}, {"DISK", "EXE_DEV_DISK", &want.Disk}, {"COMMAND", "", &want.Command}, {"USER", "", &want.User}, {"WORK_ROOT", "", &want.WorkRoot}} {
				raw, alias := *f.v, "alias-value"
				if mode == "empty" {
					raw = ""
					alias = ""
				}
				if mode == "alias" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				t.Setenv("CRABBOX_EXE_DEV_"+f.suffix, raw)
				if f.alias != "" {
					t.Setenv(f.alias, alias)
				}
				accepted := false
				if raw != "" {
					*f.v = raw
					accepted = true
				} else if f.alias != "" && alias != "" {
					*f.v = alias
					accepted = true
				}
				if f.suffix == "CONTROL_HOST" && accepted {
					source = credentialSourceEnvironment
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.ExeDev != want || cfg.credentialProvenance.exeDevControlHost != source {
				t.Fatalf("env mode=%s got=%#v want=%#v", mode, cfg.ExeDev, want)
			}
		})
	}
	for _, raw := range []string{"", "invalid", "0", "-2", "3"} {
		t.Run("CPU-"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.ExeDev.CPUs = 6
			want := 6
			t.Setenv("CRABBOX_EXE_DEV_CPUS", raw)
			if n, err := strconv.Atoi(raw); err == nil {
				want = n
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.ExeDev.CPUs != want {
				t.Fatalf("CPU=%d want=%d", cfg.ExeDev.CPUs, want)
			}
		})
	}
	for _, tc := range []struct {
		raw         string
		prior, want bool
	}{{"", true, true}, {"invalid", true, true}, {"false", true, false}, {"true", false, true}} {
		t.Run("bool-"+tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.ExeDev.NoEmail = tc.prior
			t.Setenv("CRABBOX_EXE_DEV_NO_EMAIL", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.ExeDev.NoEmail != tc.want {
				t.Fatalf("NoEmail=%t want=%t", cfg.ExeDev.NoEmail, tc.want)
			}
		})
	}
}

func TestExeDevConfigCoreFallbackContract(t *testing.T) {
	for _, tc := range []struct{ providerRoot, generic, want string }{{"", "/work/crabbox", "/tmp/crabbox"}, {"", "/custom/root", "/custom/root"}, {"/specific/root", "/custom/root", "/specific/root"}, {"  ", "/custom/root", "  "}} {
		cfg := baseConfig()
		cfg.Provider = "exe-dev"
		cfg.WorkRoot = tc.generic
		cfg.ExeDev.WorkRoot = tc.providerRoot
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.WorkRoot != tc.want || cfg.ExeDev.WorkRoot != tc.want {
			t.Fatalf("roots=%q/%q want=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot, tc.want)
		}
	}
	for _, tc := range []struct{ raw, want string }{{"", "default"}, {"  ", "  "}, {" image ", " image "}} {
		cfg := baseConfig()
		cfg.Provider = "exe-dev"
		cfg.ExeDev.Image = tc.raw
		if got := serverTypeForConfig(cfg); got != tc.want {
			t.Fatalf("display=%q want=%q", got, tc.want)
		}
	}
}

func TestExeDevConfigCentralFlagSource(t *testing.T) {
	cfg := baseConfig()
	cfg.credentialProvenance.exeDevControlHost = credentialSourceTrustedFile
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	fs.String("exe-dev-control-host", "", "")
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.exeDevControlHost != credentialSourceTrustedFile {
		t.Fatal("unvisited source changed")
	}
	if err := fs.Parse([]string{"--exe-dev-control-host="}); err != nil {
		t.Fatal(err)
	}
	markCredentialDestinationFlagSources(&cfg, fs)
	if cfg.credentialProvenance.exeDevControlHost != credentialSourceFlag {
		t.Fatal("explicit empty source missing")
	}
}

func TestInheritedWorkRootCallerContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "fixture-user")
	for _, tc := range []struct{ providerRoot, genericRoot, want string }{
		{"", "", "/tmp/crabbox"}, {"", "/work/crabbox", "/tmp/crabbox"}, {"", "/Users/ec2-user/crabbox", "/tmp/crabbox"}, {"", `C:\crabbox`, "/tmp/crabbox"},
		{"", " /work/crabbox ", " /work/crabbox "}, {"", "/WORK/crabbox", "/WORK/crabbox"}, {"", `c:\crabbox`, `c:\crabbox`},
		{"", "/srv/custom", "/srv/custom"}, {"", "/Users/alice/custom", "/Users/alice/custom"}, {"", `D:\custom`, `D:\custom`}, {"", "  ", "  "},
		{" ", "/srv/custom", " "}, {"/work/crabbox", "/srv/custom", "/work/crabbox"}, {"relative", "/srv/custom", "relative"}, {"/provider/root", "/srv/custom", "/provider/root"},
	} {
		for _, explicit := range []bool{false, true} {
			cfg := baseConfig()
			cfg.Provider = "exe-dev"
			cfg.SSHUser = "fixture-user"
			cfg.SSHPort = "1234"
			cfg.SSHFallbackPorts = []string{"4567"}
			cfg.WorkRoot = "/recorded/root"
			if explicit {
				MarkWorkRootExplicit(&cfg)
			}
			cfg.WorkRoot = tc.genericRoot
			cfg.ExeDev.WorkRoot = tc.providerRoot
			want := cfg
			want.WorkRoot = tc.want
			want.ExeDev.WorkRoot = tc.want
			want.SSHFallbackPorts = nil
			want.providerDefaultsApplied = "exe-dev"
			want.inferredTargetProvider = "exe-dev"
			want.osImageProviderDefaults = want.OSImage
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("whole core config differs for roots=%q/%q explicit=%t: got=%#v want=%#v", tc.providerRoot, tc.genericRoot, explicit, cfg, want)
			}
		}
	}
}

func TestOVHBindingFileContract(t *testing.T) {
	wantDefaults := OVHConfig{Endpoint: "https://api.us.ovhcloud.com/1.0", Image: "Ubuntu 24.04", Flavor: "b3-8"}
	if cfg := baseConfig(); cfg.OVH != wantDefaults || OVHImageWasExplicit(cfg) {
		t.Fatalf("defaults=%#v", cfg.OVH)
	}
	if reflect.TypeOf(OVHConfig{}).NumField() != 5 || reflect.TypeOf(fileOVHConfig{}).NumField() != 5 {
		t.Fatal("five-field config surface changed")
	}
	for _, trusted := range []bool{false, true} {
		for _, priorMarker := range []bool{false, true} {
			for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value", "other-only"} {
				cfg := baseConfig()
				cfg.OVH.ProjectID = "prior-project"
				cfg.OVH.Region = "prior-region"
				cfg.ovhImageExplicit = priorMarker
				want := cfg.OVH
				wantMarker := priorMarker
				fields := map[string]any{}
				for _, f := range []struct {
					key string
					v   *string
				}{{"endpoint", &want.Endpoint}, {"projectId", &want.ProjectID}, {"region", &want.Region}, {"image", &want.Image}, {"flavor", &want.Flavor}} {
					if mode == "omitted" || (mode == "other-only" && f.key == "image") {
						continue
					}
					var raw any = *f.v
					if mode == "null" {
						raw = nil
					}
					if mode == "empty" {
						raw = ""
					}
					if mode == "whitespace" {
						raw = "  "
					}
					if mode == "value" || mode == "other-only" {
						raw = "fixture-value"
					}
					fields[f.key] = raw
					if mode == "equal" || mode == "whitespace" || mode == "value" || mode == "other-only" {
						if trusted || f.key != "endpoint" {
							*f.v = raw.(string)
						}
						if f.key == "image" {
							wantMarker = true
						}
					}
				}
				data, err := yaml.Marshal(map[string]any{"ovh": fields})
				if err != nil {
					t.Fatal(err)
				}
				var file fileConfig
				if err := yaml.Unmarshal(data, &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if cfg.OVH != want || OVHImageWasExplicit(cfg) != wantMarker {
					t.Fatalf("file mode=%s trusted=%t priorMarker=%t got=%#v want=%#v", mode, trusted, priorMarker, cfg.OVH, want)
				}
			}
		}
	}
}

func TestOVHBindingEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "equal", "whitespace", "value", "other-only"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.OVH.ProjectID = "prior-project"
			cfg.OVH.Region = "prior-region"
			want := cfg.OVH
			wantMarker := false
			for _, f := range []struct {
				env string
				v   *string
			}{{"OVH_ENDPOINT", &want.Endpoint}, {"CRABBOX_OVH_PROJECT_ID", &want.ProjectID}, {"CRABBOX_OVH_REGION", &want.Region}, {"CRABBOX_OVH_IMAGE", &want.Image}, {"CRABBOX_OVH_FLAVOR", &want.Flavor}} {
				raw := *f.v
				if mode == "absent" || mode == "empty" || (mode == "other-only" && f.env == "CRABBOX_OVH_IMAGE") {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" || (mode == "other-only" && f.env != "CRABBOX_OVH_IMAGE") {
					raw = "fixture-value"
				}
				t.Setenv(f.env, raw)
				if mode == "absent" {
					if err := os.Unsetenv(f.env); err != nil {
						t.Fatal(err)
					}
				}
				if raw != "" {
					*f.v = raw
					if f.env == "CRABBOX_OVH_IMAGE" {
						wantMarker = true
					}
				}
			}
			t.Setenv("CRABBOX_OVH_ENDPOINT", "ignored-unrecognized-alias")
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.OVH != want || OVHImageWasExplicit(cfg) != wantMarker {
				t.Fatalf("env mode=%s got=%#v want=%#v marker=%t", mode, cfg.OVH, want, OVHImageWasExplicit(cfg))
			}
		})
	}
}

func TestOVHBindingCoreDefaults(t *testing.T) {
	for _, raw := range []string{"", "  ", "fixture-value"} {
		cfg := baseConfig()
		cfg.Provider = "ovh"
		cfg.OVH = OVHConfig{Endpoint: raw, ProjectID: "project", Region: "region", Image: raw, Flavor: raw}
		cfg.ovhImageExplicit = false
		want := cfg.OVH
		if raw == "" {
			want.Endpoint = "https://api.us.ovhcloud.com/1.0"
			want.Image = "Ubuntu 24.04"
			want.Flavor = "b3-8"
		}
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.OVH != want || OVHImageWasExplicit(cfg) || cfg.TargetOS != "linux" {
			t.Fatalf("raw=%q defaults=%#v want=%#v", raw, cfg.OVH, want)
		}
	}
}

func TestLumeBindingFileContract(t *testing.T) {
	wantDefaults := LumeConfig{CLIPath: "lume", Base: "crabbox-macos-golden", User: "lume", WorkRoot: "/Users/lume/crabbox"}
	if got := baseConfig().Lume; got != wantDefaults {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	if reflect.TypeOf(LumeConfig{}).NumField() != 5 || reflect.TypeOf(fileLumeConfig{}).NumField() != 5 {
		t.Fatal("five-field surface changed")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Lume.Storage = "prior-storage"
			want := cfg.Lume
			fields := map[string]any{}
			for _, f := range []struct {
				key, value string
				v          *string
			}{{"cliPath", "/usr/local/bin/lume-fixture", &want.CLIPath}, {"base", "fixture-base", &want.Base}, {"storage", "fixture-storage", &want.Storage}, {"user", "alice", &want.User}, {"workRoot", "/Users/alice/work", &want.WorkRoot}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = f.value
				}
				fields[f.key] = raw
				if (mode == "equal" || mode == "whitespace" || mode == "value") && (trusted || f.key == "workRoot") {
					*f.v = raw.(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"lume": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Lume != want {
				t.Fatalf("file mode=%s trusted=%t got=%#v want=%#v", mode, trusted, cfg.Lume, want)
			}
		}
	}
}

func TestLumeBindingEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"absent", "empty", "equal", "whitespace", "value"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Lume.Storage = "prior-storage"
			want := cfg.Lume
			for _, f := range []struct {
				suffix, value string
				v             *string
			}{{"CLI", "lume-fixture", &want.CLIPath}, {"BASE", "fixture-base", &want.Base}, {"STORAGE", "fixture-storage", &want.Storage}, {"USER", "alice", &want.User}, {"WORK_ROOT", "/Users/alice/work", &want.WorkRoot}} {
				raw := *f.v
				if mode == "absent" || mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = f.value
				}
				name := "CRABBOX_LUME_" + f.suffix
				t.Setenv(name, raw)
				if mode == "absent" {
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
				}
				if raw != "" {
					*f.v = raw
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Lume != want {
				t.Fatalf("env mode=%s got=%#v want=%#v", mode, cfg.Lume, want)
			}
		})
	}
}

func TestRunpodBindingFileContract(t *testing.T) {
	wantDefaults := RunpodConfig{APIURL: "https://rest.runpod.io/v1", CloudType: "SECURE", InstanceID: "NVIDIA L4,NVIDIA RTX 4000 Ada Generation,NVIDIA RTX A4000,NVIDIA GeForce RTX 3090,NVIDIA GeForce RTX 4090,NVIDIA RTX A5000,NVIDIA RTX A4500", Image: "runpod/pytorch:2.8.0-py3.11-cuda12.8.1-cudnn-devel-ubuntu22.04", DiskGB: 20}
	if got := baseConfig().Runpod; got != wantDefaults {
		t.Fatalf("defaults=%#v want=%#v", got, wantDefaults)
	}
	if reflect.TypeOf(RunpodConfig{}).NumField() != 9 || reflect.TypeOf(fileRunpodConfig{}).NumField() != 8 {
		t.Fatal("config source surface changed")
	}
	if _, ok := reflect.TypeOf(fileRunpodConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("key admitted to YAML")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Runpod.APIKey = "inert-prior"
			cfg.Runpod.TemplateID = "prior-template"
			cfg.Runpod.User = "prior-user"
			cfg.Runpod.WorkRoot = "/prior/root"
			want := cfg.Runpod
			source := credentialSourceFlag
			cfg.credentialProvenance.runpodAPIKey = source
			cfg.credentialProvenance.runpodAPIURL = source
			fields := map[string]any{"apiKey": "ignored-inert-key"}
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiUrl", &want.APIURL}, {"cloudType", &want.CloudType}, {"instanceId", &want.InstanceID}, {"image", &want.Image}, {"templateId", &want.TemplateID}, {"user", &want.User}, {"workRoot", &want.WorkRoot}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				fields[f.key] = raw
				if mode == "equal" || mode == "whitespace" || mode == "value" {
					*f.v = raw.(string)
					source = credentialSourceForFile(trusted)
				}
			}
			data, err := yaml.Marshal(map[string]any{"runpod": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Runpod != want || cfg.credentialProvenance.runpodAPIURL != source || cfg.credentialProvenance.runpodAPIKey != credentialSourceFlag {
				t.Fatalf("mode=%s trusted=%t", mode, trusted)
			}
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			yaml string
			want int
		}{{"runpod: null\n", 37}, {"runpod: {}\n", 37}, {"runpod:\n  diskGB: null\n", 37}, {"runpod:\n  diskGB: 0\n", 37}, {"runpod:\n  diskGB: -2\n", -2}, {"runpod:\n  diskGB: 4\n", 4}} {
			cfg := baseConfig()
			cfg.Runpod.DiskGB = 37
			var file fileConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Runpod.DiskGB != tc.want {
				t.Fatalf("disk file=%q got=%d want=%d", tc.yaml, cfg.Runpod.DiskGB, tc.want)
			}
		}
	}
}

func TestRunpodBindingEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"empty", "alias", "equal", "whitespace", "value", "API_KEY", "API_URL"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Runpod.APIKey = "inert-prior"
			cfg.Runpod.TemplateID = "prior-template"
			cfg.Runpod.User = "prior-user"
			cfg.Runpod.WorkRoot = "/prior/root"
			want := cfg.Runpod
			cfg.credentialProvenance.runpodAPIKey = credentialSourceFlag
			cfg.credentialProvenance.runpodAPIURL = credentialSourceFlag
			accepted := map[string]bool{}
			for _, f := range []struct {
				suffix, alias string
				v             *string
			}{{"API_KEY", "RUNPOD_API_KEY", &want.APIKey}, {"API_URL", "RUNPOD_API_URL", &want.APIURL}, {"CLOUD_TYPE", "RUNPOD_CLOUD_TYPE", &want.CloudType}, {"INSTANCE_ID", "RUNPOD_INSTANCE_ID", &want.InstanceID}, {"IMAGE", "RUNPOD_IMAGE", &want.Image}, {"TEMPLATE_ID", "RUNPOD_TEMPLATE_ID", &want.TemplateID}, {"USER", "", &want.User}, {"WORK_ROOT", "", &want.WorkRoot}} {
				raw, alias := *f.v, "alias-value"
				if mode == "empty" {
					raw = ""
					alias = ""
				}
				if mode == "alias" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				if (mode == "API_KEY" || mode == "API_URL") && mode != f.suffix {
					raw = ""
					alias = ""
				}
				t.Setenv("CRABBOX_RUNPOD_"+f.suffix, raw)
				if f.alias != "" {
					t.Setenv(f.alias, alias)
				}
				if raw != "" {
					*f.v = raw
					accepted[f.suffix] = true
				} else if f.alias != "" && alias != "" {
					*f.v = alias
					accepted[f.suffix] = true
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			key, url := credentialSourceFlag, credentialSourceFlag
			if accepted["API_KEY"] {
				key = credentialSourceEnvironment
			}
			if accepted["API_URL"] {
				url = credentialSourceEnvironment
			}
			if cfg.Runpod != want || cfg.credentialProvenance.runpodAPIKey != key || cfg.credentialProvenance.runpodAPIURL != url {
				t.Fatalf("env mode=%s got=%#v want=%#v", mode, cfg.Runpod, want)
			}
		})
	}
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 37}, {"invalid", 37}, {" 4 ", 37}, {"0", 0}, {"-2", -2}, {"4", 4}} {
		t.Run("disk-"+tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Runpod.DiskGB = 37
			t.Setenv("CRABBOX_RUNPOD_DISK_GB", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Runpod.DiskGB != tc.want {
				t.Fatalf("env disk=%d want=%d", cfg.Runpod.DiskGB, tc.want)
			}
		})
	}
}

func TestRunpodBindingCentralURLPhase(t *testing.T) {
	original := providerRegistry["aws"]
	t.Cleanup(func() { providerRegistry["aws"] = original })
	for _, visited := range []bool{false, true} {
		cfg := baseConfig()
		cfg.Provider = "aws"
		cfg.credentialProvenance.runpodAPIURL = credentialSourceTrustedFile
		seen := credentialSourceUnknown
		providerRegistry["aws"] = credentialFlagPhaseTestProvider{Provider: original, observe: func(cfg Config) { seen = cfg.credentialProvenance.runpodAPIURL }}
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		fs.String("runpod-url", "", "")
		if visited {
			if err := fs.Parse([]string{"--runpod-url="}); err != nil {
				t.Fatal(err)
			}
		}
		if err := applyProviderFlags(&cfg, fs, providerFlagValues{}); err != nil {
			t.Fatal(err)
		}
		want := credentialSourceTrustedFile
		if visited {
			want = credentialSourceFlag
		}
		if seen != credentialSourceTrustedFile || cfg.credentialProvenance.runpodAPIURL != want {
			t.Fatal("URL source did not stay in central post-success phase")
		}
	}
}

func TestVastBindingFileContract(t *testing.T) {
	defaults := VastConfig{APIURL: "https://console.vast.ai/api/v0", InstanceType: "ondemand", Image: "nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04", Runtype: "ssh_direct", DiskGB: 20, Order: "dlperf_per_dphtotal desc", User: "root", WorkRoot: "/work/crabbox", ReleaseAction: "destroy"}
	if got := baseConfig().Vast; got != defaults {
		t.Fatalf("defaults=%#v want=%#v", got, defaults)
	}
	if reflect.TypeOf(VastConfig{}).NumField() != 15 || reflect.TypeOf(fileVastConfig{}).NumField() != 14 {
		t.Fatal("field grants changed")
	}
	if _, ok := reflect.TypeOf(fileVastConfig{}).FieldByName("APIKey"); ok {
		t.Fatal("API key YAML source introduced")
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Vast.APIKey = "inert-prior"
			cfg.Vast.GPUName = "prior-gpu"
			cfg.Vast.TemplateID = "prior-template"
			want := cfg.Vast
			source := credentialSourceFlag
			cfg.credentialProvenance.vastAPIKey = source
			cfg.credentialProvenance.vastAPIURL = source
			fields := map[string]any{"apiKey": "ignored-inert"}
			accepted := mode == "equal" || mode == "whitespace" || mode == "value"
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiUrl", &want.APIURL}, {"instanceType", &want.InstanceType}, {"gpuName", &want.GPUName}, {"image", &want.Image}, {"templateId", &want.TemplateID}, {"runtype", &want.Runtype}, {"order", &want.Order}, {"user", &want.User}, {"workRoot", &want.WorkRoot}, {"releaseAction", &want.ReleaseAction}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
				}
				if mode == "value" && f.key == "instanceType" {
					raw = " On_Demand "
				}
				fields[f.key] = raw
				if accepted {
					*f.v = raw.(string)
				}
			}
			if accepted {
				source = credentialSourceForFile(trusted)
			}
			data, err := yaml.Marshal(map[string]any{"vast": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Vast != want || cfg.credentialProvenance.vastAPIURL != source || cfg.credentialProvenance.vastAPIKey != credentialSourceFlag || IsVastWorkRootExplicit(&cfg) != accepted || DeleteOnReleaseExplicit(cfg, "vast") != accepted {
				t.Fatalf("file mode=%s trusted=%t", mode, trusted)
			}
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, raw := range []string{"omitted", "null", "0", "-2", "4"} {
			cfg := baseConfig()
			cfg.Vast.GPUCount = 37
			cfg.Vast.DiskGB = 37
			cfg.Vast.MaxDphTotal = .75
			cfg.Vast.MinReliability = .75
			want := cfg.Vast
			body := "vast: {}\n"
			if raw != "omitted" {
				body = fmt.Sprintf("vast:\n  gpuCount: %s\n  diskGB: %s\n  maxDphTotal: %s\n  minReliability: %s\n", raw, raw, raw, raw)
			}
			if n, err := strconv.Atoi(raw); err == nil {
				if n != 0 {
					want.GPUCount = n
					want.DiskGB = n
				}
				want.MaxDphTotal = float64(n)
				want.MinReliability = float64(n)
			}
			var file fileConfig
			if err := yaml.Unmarshal([]byte(body), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Vast != want {
				t.Fatalf("numeric file=%q got=%#v want=%#v", raw, cfg.Vast, want)
			}
		}
	}
}

func TestVastBindingEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"empty", "alias", "equal", "whitespace", "value", "API_KEY", "API_URL", "WORK_ROOT", "RELEASE_ACTION"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Vast.APIKey = "inert-prior"
			cfg.Vast.GPUName = "prior-gpu"
			cfg.Vast.TemplateID = "prior-template"
			want := cfg.Vast
			cfg.credentialProvenance.vastAPIKey = credentialSourceFlag
			cfg.credentialProvenance.vastAPIURL = credentialSourceFlag
			accepted := map[string]bool{}
			for _, f := range []struct {
				suffix, alias string
				v             *string
			}{{"API_KEY", "VAST_API_KEY", &want.APIKey}, {"API_URL", "VAST_API_URL", &want.APIURL}, {"INSTANCE_TYPE", "", &want.InstanceType}, {"GPU_NAME", "", &want.GPUName}, {"IMAGE", "", &want.Image}, {"TEMPLATE_ID", "", &want.TemplateID}, {"RUNTYPE", "", &want.Runtype}, {"ORDER", "", &want.Order}, {"USER", "", &want.User}, {"WORK_ROOT", "", &want.WorkRoot}, {"RELEASE_ACTION", "", &want.ReleaseAction}} {
				raw, alias := *f.v, "alias-value"
				if mode == "empty" {
					raw = ""
					alias = ""
				}
				if mode == "alias" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "fixture-value"
					if f.suffix == "INSTANCE_TYPE" {
						raw = " On_Demand "
					}
				}
				if (mode == "API_KEY" || mode == "API_URL" || mode == "WORK_ROOT" || mode == "RELEASE_ACTION") && mode != f.suffix {
					raw = ""
					alias = ""
				}
				t.Setenv("CRABBOX_VAST_"+f.suffix, raw)
				if f.alias != "" {
					t.Setenv(f.alias, alias)
				}
				if raw != "" {
					*f.v = raw
					accepted[f.suffix] = true
				} else if f.alias != "" && alias != "" {
					*f.v = alias
					accepted[f.suffix] = true
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			key, url := credentialSourceFlag, credentialSourceFlag
			if accepted["API_KEY"] {
				key = credentialSourceEnvironment
			}
			if accepted["API_URL"] {
				url = credentialSourceEnvironment
			}
			if cfg.Vast != want || cfg.credentialProvenance.vastAPIKey != key || cfg.credentialProvenance.vastAPIURL != url || IsVastWorkRootExplicit(&cfg) != accepted["WORK_ROOT"] || DeleteOnReleaseExplicit(cfg, "vast") != accepted["RELEASE_ACTION"] {
				t.Fatalf("env mode=%s", mode)
			}
		})
	}
	for _, raw := range []string{"", "invalid", " 4 ", "0", "-2", "0.25"} {
		t.Run("numeric-"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Vast.GPUCount = 37
			cfg.Vast.DiskGB = 37
			cfg.Vast.MaxDphTotal = .75
			cfg.Vast.MinReliability = .75
			want := cfg.Vast
			for _, suffix := range []string{"GPU_COUNT", "DISK_GB", "MAX_DPH_TOTAL", "MIN_RELIABILITY"} {
				t.Setenv("CRABBOX_VAST_"+suffix, raw)
			}
			if n, err := strconv.Atoi(raw); err == nil {
				want.GPUCount = n
				want.DiskGB = n
			}
			if n, err := strconv.ParseFloat(raw, 64); err == nil {
				want.MaxDphTotal = n
				want.MinReliability = n
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Vast != want {
				t.Fatalf("env numeric=%q got=%#v want=%#v", raw, cfg.Vast, want)
			}
		})
	}
}

func TestVastBindingCoreDefaultsAndMarkers(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "vast"
	cfg.Vast = VastConfig{}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	want := VastConfig{APIURL: "https://console.vast.ai/api/v0", InstanceType: "ondemand", Image: "nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04", Runtype: "ssh_direct", DiskGB: 20, Order: "dlperf_per_dphtotal desc", User: "root", WorkRoot: "/work/crabbox", ReleaseAction: "destroy"}
	if cfg.Vast != want {
		t.Fatalf("core defaults=%#v", cfg.Vast)
	}
	for _, tc := range []struct {
		root                 string
		marked               bool
		effective, projected string
	}{{"/work/crabbox", false, "/generic/root", "/generic/root"}, {"/work/crabbox", true, "/work/crabbox", "/work/crabbox"}, {"", true, "/work/crabbox", "/work/crabbox"}, {"/provider/root", false, "/provider/root", "/generic/root"}, {"/provider/root", true, "/provider/root", "/provider/root"}} {
		cfg := baseConfig()
		cfg.Provider = "vast"
		cfg.WorkRoot = "/generic/root"
		MarkWorkRootExplicit(&cfg)
		cfg.SSHUser = "generic-user"
		MarkSSHUserExplicit(&cfg)
		cfg.Vast.User = "provider-user"
		cfg.Vast.WorkRoot = tc.root
		if tc.marked {
			MarkVastWorkRootExplicit(&cfg)
		}
		if got := EffectiveVastWorkRoot(cfg); got != tc.effective {
			t.Fatalf("effective root=%q want=%q", got, tc.effective)
		}
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Vast.WorkRoot != tc.projected || cfg.WorkRoot != tc.projected || cfg.SSHUser != "generic-user" {
			t.Fatal("core explicit projection changed")
		}
	}
}

func TestVastBindingCentralURLPhase(t *testing.T) {
	original := providerRegistry["aws"]
	t.Cleanup(func() { providerRegistry["aws"] = original })
	for _, visited := range []bool{false, true} {
		cfg := baseConfig()
		cfg.Provider = "aws"
		cfg.credentialProvenance.vastAPIURL = credentialSourceTrustedFile
		seen := credentialSourceUnknown
		providerRegistry["aws"] = credentialFlagPhaseTestProvider{Provider: original, observe: func(cfg Config) { seen = cfg.credentialProvenance.vastAPIURL }}
		fs := flag.NewFlagSet("contract", flag.ContinueOnError)
		fs.String("vast-api-url", "", "")
		if visited {
			if err := fs.Parse([]string{"--vast-api-url="}); err != nil {
				t.Fatal(err)
			}
		}
		if err := applyProviderFlags(&cfg, fs, providerFlagValues{}); err != nil {
			t.Fatal(err)
		}
		want := credentialSourceTrustedFile
		if visited {
			want = credentialSourceFlag
		}
		if seen != credentialSourceTrustedFile || cfg.credentialProvenance.vastAPIURL != want {
			t.Fatal("URL source moved out of central post-success phase")
		}
	}
}

func TestWandbBindingFileContract(t *testing.T) {
	if got := baseConfig().Wandb; got != (WandbConfig{}) {
		t.Fatalf("raw defaults=%#v", got)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "whitespace", "value"} {
			cfg := baseConfig()
			cfg.Wandb = WandbConfig{APIKey: "inert-prior", DefaultImage: "prior-image", MaxLifetimeSeconds: 37}
			want := cfg.Wandb
			fields := map[string]any{}
			for _, f := range []struct {
				key string
				v   *string
			}{{"apiKey", &want.APIKey}, {"defaultImage", &want.DefaultImage}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "whitespace" {
					raw = "  "
				}
				if mode == "value" {
					raw = "inert-value"
				}
				fields[f.key] = raw
				if mode == "equal" || mode == "whitespace" || mode == "value" {
					*f.v = raw.(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"wandb": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Wandb != want {
				t.Fatalf("file mode=%s trusted=%t got=%#v want=%#v", mode, trusted, cfg.Wandb, want)
			}
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			raw  string
			want int
		}{{"null", 37}, {"0", 37}, {"-2", 37}, {"45", 45}} {
			cfg := baseConfig()
			cfg.Wandb.MaxLifetimeSeconds = 37
			var file fileConfig
			if err := yaml.Unmarshal([]byte("wandb:\n  maxLifetimeSeconds: "+tc.raw+"\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.Wandb.MaxLifetimeSeconds != tc.want {
				t.Fatalf("file lifetime=%s got=%d want=%d", tc.raw, cfg.Wandb.MaxLifetimeSeconds, tc.want)
			}
		}
	}
}

func TestWandbBindingEnvironmentContract(t *testing.T) {
	for _, tc := range []struct{ name, key, primaryImage, aliasImage, wantKey, wantImage string }{
		{"empty", "", "", "", "inert-prior", "prior-image"},
		{"primary", "inert-primary", "primary-image", "alias-image", "inert-primary", "primary-image"},
		{"image-alias", "", "", "alias-image", "inert-prior", "alias-image"},
		{"whitespace", "  ", "  ", "alias-image", "  ", "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Wandb = WandbConfig{APIKey: "inert-prior", DefaultImage: "prior-image", MaxLifetimeSeconds: 37}
			t.Setenv("CRABBOX_WANDB_API_KEY", tc.key)
			t.Setenv("CRABBOX_WANDB_DEFAULT_IMAGE", tc.primaryImage)
			t.Setenv("WANDB_DEFAULT_IMAGE", tc.aliasImage)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Wandb != (WandbConfig{APIKey: tc.wantKey, DefaultImage: tc.wantImage, MaxLifetimeSeconds: 37}) {
				t.Fatalf("env strings=%#v", cfg.Wandb)
			}
		})
	}
	for _, tc := range []struct {
		name, primary, vendor         string
		missingPrimary, missingVendor bool
		want                          int
	}{
		{"missing-primary", "", "45", true, false, 45}, {"empty-primary", "", "45", false, false, 45}, {"malformed-primary", "invalid", "45", false, false, 45},
		{"overflow-primary", "999999999999999999999999999999999999", "45", false, false, 45}, {"padded-primary", " 46 ", "45", false, false, 45},
		{"zero-primary", "0", "45", false, false, 0}, {"negative-primary", "-2", "45", false, false, -2}, {"positive-primary", "46", "45", false, false, 46},
		{"both-invalid", "invalid", "invalid", false, false, 37}, {"bad-vendor", "", "invalid", false, false, 37}, {"padded-vendor", "", " 45 ", false, false, 37}, {"both-missing", "", "", true, true, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Wandb.MaxLifetimeSeconds = 37
			t.Setenv("CRABBOX_WANDB_MAX_LIFETIME_SECONDS", tc.primary)
			t.Setenv("WANDB_MAX_LIFETIME_SECONDS", tc.vendor)
			if tc.missingPrimary {
				if err := os.Unsetenv("CRABBOX_WANDB_MAX_LIFETIME_SECONDS"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missingVendor {
				if err := os.Unsetenv("WANDB_MAX_LIFETIME_SECONDS"); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Wandb.MaxLifetimeSeconds != tc.want {
				t.Fatalf("nested lifetime got=%d want=%d", cfg.Wandb.MaxLifetimeSeconds, tc.want)
			}
		})
	}
}

func TestScalewayBindingFileContract(t *testing.T) {
	defaults := ScalewayConfig{Region: "fr-par", Zone: "fr-par-1", Image: "ubuntu_noble", Type: "DEV1-S"}
	if got := baseConfig().Scaleway; !reflect.DeepEqual(got, defaults) {
		t.Fatalf("defaults=%#v", got)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "padded", "custom"} {
			cfg := baseConfig()
			cfg.Scaleway.ProjectID = "prior-project"
			cfg.Scaleway.OrganizationID = "prior-org"
			cfg.Scaleway.SecurityGroup = "prior-group"
			want := cfg.Scaleway
			fields := map[string]any{}
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, f := range []struct {
				key string
				v   *string
			}{{"region", &want.Region}, {"zone", &want.Zone}, {"image", &want.Image}, {"type", &want.Type}, {"projectId", &want.ProjectID}, {"organizationId", &want.OrganizationID}, {"securityGroup", &want.SecurityGroup}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "padded" {
					raw = "  " + *f.v + "  "
				}
				if mode == "custom" {
					raw = "fixture"
				}
				fields[f.key] = raw
				if accepted {
					*f.v = raw.(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"scaleway": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Scaleway, want) || ScalewayRegionWasExplicit(cfg) != accepted || ScalewayZoneWasExplicit(cfg) != accepted || ScalewayImageWasExplicit(cfg) != accepted || ScalewayTypeWasExplicit(cfg) != accepted {
				t.Fatalf("file mode=%s trusted=%t", mode, trusted)
			}
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			body     string
			accepted bool
			want     []string
		}{{"scaleway: {}\n", false, nil}, {"scaleway:\n  sshCIDRs: null\n", false, nil}, {"scaleway:\n  sshCIDRs: []\n", false, nil}, {"scaleway:\n  sshCIDRs: ['']\n", true, []string{""}}, {"scaleway:\n  sshCIDRs: [' 203.0.113.0/24 ', '', ' ', '2001:db8::/64', '203.0.113.0/24']\n", true, []string{" 203.0.113.0/24 ", "", " ", "2001:db8::/64", "203.0.113.0/24"}}} {
			for _, prior := range [][]string{nil, {}, {"prior"}} {
				cfg := baseConfig()
				cfg.Scaleway.SSHCIDRs = prior
				var file fileConfig
				if err := yaml.Unmarshal([]byte(tc.body), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				want := prior
				if tc.accepted {
					want = tc.want
				}
				if !reflect.DeepEqual(cfg.Scaleway.SSHCIDRs, want) {
					t.Fatalf("file list=%q got=%#v want=%#v", tc.body, cfg.Scaleway.SSHCIDRs, want)
				}
				if !tc.accepted {
					if len(prior) > 0 && &cfg.Scaleway.SSHCIDRs[0] != &prior[0] {
						t.Fatal("ignored list changed backing array")
					}
					continue
				}
				second := baseConfig()
				if err := applyFileConfigWithTrust(&second, file, trusted); err != nil {
					t.Fatal(err)
				}
				source := reflect.ValueOf(file.Scaleway).Elem().FieldByName("SSHCIDRs")
				if source.Kind() == reflect.Pointer {
					source = source.Elem()
				}
				raw := source.Interface().([]string)
				if &raw[0] != &cfg.Scaleway.SSHCIDRs[0] || &raw[0] != &second.Scaleway.SSHCIDRs[0] {
					t.Fatal("accepted file list no longer directly shared")
				}
				cfg.Scaleway.SSHCIDRs[0] = "198.51.100.0/24"
				if raw[0] != "198.51.100.0/24" || second.Scaleway.SSHCIDRs[0] != raw[0] {
					t.Fatal("shared element update lost")
				}
			}
		}
	}
}

func TestScalewayBindingEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "equal", "padded", "custom"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			want := cfg.Scaleway
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, f := range []struct {
				suffix string
				v      *string
			}{{"REGION", &want.Region}, {"ZONE", &want.Zone}, {"IMAGE", &want.Image}, {"TYPE", &want.Type}, {"PROJECT_ID", &want.ProjectID}, {"ORGANIZATION_ID", &want.OrganizationID}, {"SECURITY_GROUP", &want.SecurityGroup}} {
				raw := *f.v
				if mode == "missing" || mode == "empty" {
					raw = ""
				}
				if mode == "padded" {
					raw = "  " + *f.v + "  "
				}
				if mode == "custom" {
					raw = "fixture"
				}
				name := "CRABBOX_SCALEWAY_" + f.suffix
				t.Setenv(name, raw)
				if mode == "missing" {
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
				}
				if raw != "" {
					*f.v = raw
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Scaleway, want) || ScalewayRegionWasExplicit(cfg) != accepted || ScalewayZoneWasExplicit(cfg) != accepted || ScalewayImageWasExplicit(cfg) != accepted || ScalewayTypeWasExplicit(cfg) != accepted {
				t.Fatalf("env mode=%s", mode)
			}
		})
	}
	for _, tc := range []struct {
		name, raw string
		missing   bool
		want      []string
	}{{"missing", "", true, []string{"prior"}}, {"empty", "", false, []string{"prior"}}, {"blanks", " , \t , ", false, []string{}}, {"none", "none", false, []string{"none"}}, {"ordered", " 203.0.113.0/24, ,2001:db8::/64,203.0.113.0/24 ", false, []string{"203.0.113.0/24", "2001:db8::/64", "203.0.113.0/24"}}} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Scaleway.SSHCIDRs = []string{"prior"}
			t.Setenv("CRABBOX_SCALEWAY_SSH_CIDRS", tc.raw)
			if tc.missing {
				if err := os.Unsetenv("CRABBOX_SCALEWAY_SSH_CIDRS"); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Scaleway.SSHCIDRs, tc.want) {
				t.Fatalf("env list=%#v want=%#v", cfg.Scaleway.SSHCIDRs, tc.want)
			}
		})
	}
}

func TestScalewayBindingIndependentMarkers(t *testing.T) {
	for _, name := range []string{"region", "zone", "image", "type"} {
		for _, source := range []string{"user", "repo", "env"} {
			t.Run(name+"-"+source, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				if source == "env" {
					t.Setenv("CRABBOX_SCALEWAY_"+strings.ToUpper(name), "  ")
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					var file fileConfig
					if err := yaml.Unmarshal([]byte("scaleway:\n  "+name+": '  '\n"), &file); err != nil {
						t.Fatal(err)
					}
					if err := applyFileConfigWithTrust(&cfg, file, source == "user"); err != nil {
						t.Fatal(err)
					}
				}
				for field, got := range map[string]bool{"region": ScalewayRegionWasExplicit(cfg), "zone": ScalewayZoneWasExplicit(cfg), "image": ScalewayImageWasExplicit(cfg), "type": ScalewayTypeWasExplicit(cfg)} {
					if got != (field == name) {
						t.Fatalf("marker=%s got=%t", field, got)
					}
				}
			})
		}
	}
}

func TestScalewayBindingCoreDefaults(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "scaleway"
	cfg.Scaleway = ScalewayConfig{}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	want := ScalewayConfig{Region: "fr-par", Zone: "fr-par-1", Image: "ubuntu_noble", Type: "DEV1-S"}
	if !reflect.DeepEqual(cfg.Scaleway, want) || ScalewayRegionWasExplicit(cfg) || ScalewayZoneWasExplicit(cfg) || ScalewayImageWasExplicit(cfg) || ScalewayTypeWasExplicit(cfg) {
		t.Fatal("raw core defaults or markers changed")
	}
	cfg = baseConfig()
	cfg.Provider = "scaleway"
	cfg.Scaleway.Image = "prior-unmarked-image"
	cfg.OSImage = "ubuntu:24.04"
	cfg.osImageExplicit = true
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Scaleway.Image != "ubuntu_noble" {
		t.Fatal("fixed portable image mapping changed")
	}
}

func TestTencentBindingFileContract(t *testing.T) {
	if got := baseConfig().TencentCloud; !reflect.DeepEqual(got, TencentCloudConfig{}) {
		t.Fatalf("raw base=%#v", got)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "empty", "equal", "padded", "custom"} {
			cfg := baseConfig()
			cfg.TencentCloud = TencentCloudConfig{Region: "prior-region", Zone: "prior-zone", Image: "prior-image", Type: "prior-type", VPCID: "prior-vpc", SubnetID: "prior-subnet", SecurityGroupID: "prior-group", InternetChargeType: "prior-charge", APIEndpoint: "https://endpoint.example.test"}
			want := cfg.TencentCloud
			fields := map[string]any{}
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, f := range []struct {
				key string
				v   *string
			}{{"region", &want.Region}, {"zone", &want.Zone}, {"image", &want.Image}, {"type", &want.Type}, {"vpcId", &want.VPCID}, {"subnetId", &want.SubnetID}, {"securityGroupId", &want.SecurityGroupID}, {"internetChargeType", &want.InternetChargeType}, {"apiEndpoint", &want.APIEndpoint}} {
				if mode == "omitted" {
					continue
				}
				var raw any = *f.v
				if mode == "null" {
					raw = nil
				}
				if mode == "empty" {
					raw = ""
				}
				if mode == "padded" {
					raw = "  " + *f.v + "  "
				}
				if mode == "custom" {
					raw = "fixture"
					if f.key == "apiEndpoint" {
						raw = "https://custom.example.test"
					}
				}
				fields[f.key] = raw
				if accepted && (trusted || f.key != "apiEndpoint") {
					*f.v = raw.(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"tencentcloud": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.TencentCloud, want) || TencentCloudRegionWasExplicit(cfg) != accepted || TencentCloudZoneWasExplicit(cfg) != accepted || TencentCloudImageWasExplicit(cfg) != accepted || TencentCloudTypeWasExplicit(cfg) != accepted {
				t.Fatalf("file mode=%s trusted=%t got=%#v want=%#v", mode, trusted, cfg.TencentCloud, want)
			}
		}
	}
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			raw  string
			want int64
		}{{"null", 37}, {"0", 37}, {"-2", 37}, {"8589934592", 8589934592}, {"9223372036854775807", 9223372036854775807}} {
			cfg := baseConfig()
			cfg.TencentCloud.RootGB = 37
			cfg.TencentCloud.InternetMaxBandwidthOut = 37
			var file fileConfig
			if err := yaml.Unmarshal([]byte("tencentcloud:\n  rootGB: "+tc.raw+"\n  internetMaxBandwidthOut: "+tc.raw+"\n"), &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if cfg.TencentCloud.RootGB != tc.want || cfg.TencentCloud.InternetMaxBandwidthOut != tc.want {
				t.Fatalf("file int64=%s got=%d/%d", tc.raw, cfg.TencentCloud.RootGB, cfg.TencentCloud.InternetMaxBandwidthOut)
			}
		}
	}
	for _, body := range []string{"tencentcloud: {}\n", "tencentcloud:\n  sshCIDRs: null\n", "tencentcloud:\n  sshCIDRs: []\n"} {
		cfg := baseConfig()
		prior := []string{"prior"}
		cfg.TencentCloud.SSHCIDRs = prior
		var file fileConfig
		if err := yaml.Unmarshal([]byte(body), &file); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg.TencentCloud.SSHCIDRs, prior) || &cfg.TencentCloud.SSHCIDRs[0] != &prior[0] {
			t.Fatal("empty file list changed prior")
		}
	}
	for _, trusted := range []bool{false, true} {
		cfg := baseConfig()
		var file fileConfig
		if err := yaml.Unmarshal([]byte("tencentcloud:\n  sshCIDRs: [' 203.0.113.0/24 ', '', '203.0.113.0/24']\n"), &file); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg.TencentCloud.SSHCIDRs, []string{" 203.0.113.0/24 ", "", "203.0.113.0/24"}) {
			t.Fatal("file list normalized")
		}
		source := reflect.ValueOf(file.TencentCloud).Elem().FieldByName("SSHCIDRs")
		if source.Kind() == reflect.Pointer {
			source = source.Elem()
		}
		raw := source.Interface().([]string)
		cfg.TencentCloud.SSHCIDRs[0] = "198.51.100.0/24"
		if raw[0] != "198.51.100.0/24" {
			t.Fatal("file list no longer shares backing")
		}
	}
}

func TestTencentBindingEnvironmentContract(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "equal", "padded", "custom"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.TencentCloud = TencentCloudConfig{Region: "prior", Zone: "prior", Image: "prior", Type: "prior", VPCID: "prior", SubnetID: "prior", SecurityGroupID: "prior", InternetChargeType: "prior", APIEndpoint: "https://endpoint.example.test"}
			want := cfg.TencentCloud
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, f := range []struct {
				suffix string
				v      *string
			}{{"REGION", &want.Region}, {"ZONE", &want.Zone}, {"IMAGE", &want.Image}, {"TYPE", &want.Type}, {"VPC_ID", &want.VPCID}, {"SUBNET_ID", &want.SubnetID}, {"SECURITY_GROUP_ID", &want.SecurityGroupID}, {"INTERNET_CHARGE_TYPE", &want.InternetChargeType}, {"API_ENDPOINT", &want.APIEndpoint}} {
				raw := *f.v
				if mode == "missing" || mode == "empty" {
					raw = ""
				}
				if mode == "padded" {
					raw = "  " + *f.v + "  "
				}
				if mode == "custom" {
					raw = "fixture"
					if f.suffix == "API_ENDPOINT" {
						raw = "https://custom.example.test"
					}
				}
				name := "CRABBOX_TENCENTCLOUD_" + f.suffix
				t.Setenv(name, raw)
				if mode == "missing" {
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
				}
				if raw != "" {
					*f.v = raw
				}
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.TencentCloud, want) || TencentCloudRegionWasExplicit(cfg) != accepted || TencentCloudZoneWasExplicit(cfg) != accepted || TencentCloudImageWasExplicit(cfg) != accepted || TencentCloudTypeWasExplicit(cfg) != accepted {
				t.Fatalf("env mode=%s", mode)
			}
		})
	}
	for _, tc := range []struct {
		raw  string
		want int64
	}{{"", 37}, {"invalid", 37}, {" 50 ", 37}, {"9223372036854775808", 37}, {"0", 0}, {"-2", -2}, {"8589934592", 8589934592}, {"-9223372036854775808", -9223372036854775808}, {"9223372036854775807", 9223372036854775807}} {
		t.Run("int64-"+tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.TencentCloud.RootGB = 37
			cfg.TencentCloud.InternetMaxBandwidthOut = 37
			t.Setenv("CRABBOX_TENCENTCLOUD_ROOT_GB", tc.raw)
			t.Setenv("CRABBOX_TENCENTCLOUD_INTERNET_MAX_BANDWIDTH_OUT", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.TencentCloud.RootGB != tc.want || cfg.TencentCloud.InternetMaxBandwidthOut != tc.want {
				t.Fatalf("env int64=%q got=%d/%d", tc.raw, cfg.TencentCloud.RootGB, cfg.TencentCloud.InternetMaxBandwidthOut)
			}
		})
	}
	for _, tc := range []struct {
		raw  string
		want []string
	}{{"", []string{"prior"}}, {" , , ", []string{}}, {"none", []string{"none"}}, {" 203.0.113.0/24,,2001:db8::/64,203.0.113.0/24 ", []string{"203.0.113.0/24", "2001:db8::/64", "203.0.113.0/24"}}} {
		t.Run("list-"+tc.raw, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.TencentCloud.SSHCIDRs = []string{"prior"}
			t.Setenv("CRABBOX_TENCENTCLOUD_SSH_CIDRS", tc.raw)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.TencentCloud.SSHCIDRs, tc.want) {
				t.Fatalf("env list=%#v want=%#v", cfg.TencentCloud.SSHCIDRs, tc.want)
			}
		})
	}
}

func TestTencentBindingMarkersAndCoreDefaults(t *testing.T) {
	for _, name := range []string{"region", "zone", "image", "type"} {
		for _, source := range []string{"user", "repo", "env"} {
			t.Run(name+source, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				if source == "env" {
					t.Setenv("CRABBOX_TENCENTCLOUD_"+strings.ToUpper(name), "  ")
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					var file fileConfig
					if err := yaml.Unmarshal([]byte("tencentcloud:\n  "+name+": '  '\n"), &file); err != nil {
						t.Fatal(err)
					}
					if err := applyFileConfigWithTrust(&cfg, file, source == "user"); err != nil {
						t.Fatal(err)
					}
				}
				for field, got := range map[string]bool{"region": TencentCloudRegionWasExplicit(cfg), "zone": TencentCloudZoneWasExplicit(cfg), "image": TencentCloudImageWasExplicit(cfg), "type": TencentCloudTypeWasExplicit(cfg)} {
					if got != (field == name) {
						t.Fatalf("marker=%s got=%t", field, got)
					}
				}
			})
		}
	}
	for _, n := range []int64{0, -2, 8589934592} {
		cfg := baseConfig()
		cfg.Provider = "tencentcloud"
		cfg.TencentCloud.RootGB = n
		cfg.TencentCloud.InternetMaxBandwidthOut = n
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		root, bandwidth := n, n
		if n == 0 {
			root = 50
			bandwidth = 5
		}
		if cfg.TencentCloud.Region != "ap-shanghai" || cfg.TencentCloud.Zone != "ap-shanghai-2" || cfg.TencentCloud.Type != "SA5.MEDIUM2" || cfg.TencentCloud.RootGB != root || cfg.TencentCloud.InternetMaxBandwidthOut != bandwidth || cfg.TencentCloud.InternetChargeType != "TRAFFIC_POSTPAID_BY_HOUR" || cfg.TencentCloud.Image != "" || cfg.TencentCloud.APIEndpoint != "" {
			t.Fatalf("core runtime n=%d cfg=%#v", n, cfg.TencentCloud)
		}
	}
}

func TestDigitalOceanBindingSources(t *testing.T) {
	if got := baseConfig().DigitalOcean; !reflect.DeepEqual(got, DigitalOceanConfig{}) {
		t.Fatalf("raw=%#v", got)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"missing", "null", "empty", "equal", "padded", "custom"} {
			cfg := baseConfig()
			cfg.DigitalOcean = DigitalOceanConfig{Region: "prior", Image: "prior", VPCUUID: "prior"}
			want := cfg.DigitalOcean
			fields := map[string]any{}
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, key := range []string{"region", "image", "vpc"} {
				switch mode {
				case "null":
					fields[key] = nil
				case "empty":
					fields[key] = ""
				case "equal":
					fields[key] = "prior"
				case "padded":
					fields[key] = "  "
				case "custom":
					fields[key] = "fixture"
				}
			}
			if accepted {
				v := fields["image"].(string)
				want.Region = v
				want.Image = v
				want.VPCUUID = v
			}
			data, err := yaml.Marshal(map[string]any{"digitalocean": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.DigitalOcean, want) || cfg.digitalOceanImageExplicit != accepted {
				t.Fatalf("file trusted=%t mode=%s got=%#v marker=%t", trusted, mode, cfg.DigitalOcean, cfg.digitalOceanImageExplicit)
			}
		}
	}
	for _, raw := range []string{"", "prior", "  ", "fixture"} {
		t.Run("env-"+raw, func(t *testing.T) {
			for _, key := range []string{"REGION", "IMAGE", "VPC"} {
				t.Setenv("CRABBOX_DIGITALOCEAN_"+key, raw)
			}
			cfg := baseConfig()
			cfg.DigitalOcean = DigitalOceanConfig{Region: "prior", Image: "prior", VPCUUID: "prior"}
			want := cfg.DigitalOcean
			if raw != "" {
				want.Region = raw
				want.Image = raw
				want.VPCUUID = raw
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.DigitalOcean, want) || cfg.digitalOceanImageExplicit != (raw != "") {
				t.Fatalf("env raw=%q got=%#v marker=%t", raw, cfg.DigitalOcean, cfg.digitalOceanImageExplicit)
			}
		})
	}
}

func TestDigitalOceanBindingLists(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, raw := range []string{"{}", "{sshCIDRs: null}", "{sshCIDRs: []}", "{sshCIDRs: [' 192.0.2.0/24 ', '', '192.0.2.0/24']}"} {
			var file fileConfig
			if err := yaml.Unmarshal([]byte("digitalocean: "+raw), &file); err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			cfg.DigitalOcean.SSHCIDRs = []string{"prior"}
			prior := &cfg.DigitalOcean.SSHCIDRs[0]
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if len(cfg.DigitalOcean.SSHCIDRs) == 1 {
				if cfg.DigitalOcean.SSHCIDRs[0] != "prior" || &cfg.DigitalOcean.SSHCIDRs[0] != prior {
					t.Fatal("ignored list changed")
				}
				continue
			}
			want := []string{" 192.0.2.0/24 ", "", "192.0.2.0/24"}
			if !reflect.DeepEqual(cfg.DigitalOcean.SSHCIDRs, want) {
				t.Fatalf("file list=%#v", cfg.DigitalOcean.SSHCIDRs)
			}
			v := reflect.ValueOf(file.DigitalOcean).Elem().FieldByName("SSHCIDRs")
			if v.Kind() == reflect.Pointer {
				v = v.Elem()
			}
			if v.Pointer() != reflect.ValueOf(cfg.DigitalOcean.SSHCIDRs).Pointer() {
				t.Fatal("file list must share backing")
			}
		}
	}
	for _, tc := range []struct {
		raw  string
		want []string
	}{{"", nil}, {" ,  ,", []string{}}, {"none", []string{"none"}}, {" 192.0.2.0/24, ,198.51.100.0/24,192.0.2.0/24 ", []string{"192.0.2.0/24", "198.51.100.0/24", "192.0.2.0/24"}}} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("CRABBOX_DIGITALOCEAN_SSH_CIDRS", tc.raw)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.DigitalOcean.SSHCIDRs, tc.want) {
				t.Fatalf("list=%#v want=%#v", cfg.DigitalOcean.SSHCIDRs, tc.want)
			}
		})
	}
}

func TestDigitalOceanBindingCoreDefaults(t *testing.T) {
	for _, raw := range []string{"", "  ", "custom"} {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		cfg.DigitalOcean.Region = raw
		cfg.DigitalOcean.Image = raw
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		region, image := raw, raw
		if raw == "" {
			region = "nyc3"
			image = "ubuntu-24-04-x64"
		}
		if cfg.DigitalOcean.Region != region || cfg.DigitalOcean.Image != image {
			t.Fatalf("defaults=%#v", cfg.DigitalOcean)
		}
	}
	for _, tc := range []struct {
		os, image string
		explicit  bool
		want      string
	}{{"ubuntu:24.04", "", false, "ubuntu-24-04-x64"}, {"ubuntu:26.04", "", false, ""}, {"ubuntu:26.04", "custom", true, "custom"}} {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		cfg.OSImage = tc.os
		cfg.osImageExplicit = true
		cfg.DigitalOcean.Image = tc.image
		cfg.digitalOceanImageExplicit = tc.explicit
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != tc.want {
			t.Fatalf("OS=%s image=%q", tc.os, cfg.DigitalOcean.Image)
		}
	}
}

func TestVultrBindingSources(t *testing.T) {
	if got := baseConfig().Vultr; !reflect.DeepEqual(got, VultrConfig{}) {
		t.Fatalf("raw=%#v", got)
	}
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"missing", "null", "empty", "equal", "padded", "custom"} {
			cfg := baseConfig()
			cfg.Vultr = VultrConfig{Region: "prior", OS: "prior", Image: "prior", Snapshot: "prior", FirewallGroup: "prior", UserScheme: "prior"}
			want := cfg.Vultr
			fields := map[string]any{}
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, f := range []struct {
				key string
				v   *string
			}{{"region", &want.Region}, {"os", &want.OS}, {"image", &want.Image}, {"snapshot", &want.Snapshot}, {"firewallGroup", &want.FirewallGroup}, {"userScheme", &want.UserScheme}} {
				switch mode {
				case "null":
					fields[f.key] = nil
				case "empty":
					fields[f.key] = ""
				case "equal":
					fields[f.key] = "prior"
				case "padded":
					fields[f.key] = "  "
				case "custom":
					fields[f.key] = "fixture"
				}
				if accepted {
					*f.v = fields[f.key].(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"vultr": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Vultr, want) {
				t.Fatalf("file trusted=%t mode=%s got=%#v want=%#v", trusted, mode, cfg.Vultr, want)
			}
		}
	}
	for _, raw := range []string{"", "prior", "  ", "fixture"} {
		t.Run("env-"+raw, func(t *testing.T) {
			for _, key := range []string{"REGION", "OS", "IMAGE", "SNAPSHOT", "FIREWALL_GROUP", "USER_SCHEME"} {
				t.Setenv("CRABBOX_VULTR_"+key, raw)
			}
			cfg := baseConfig()
			cfg.Vultr = VultrConfig{Region: "prior", OS: "prior", Image: "prior", Snapshot: "prior", FirewallGroup: "prior", UserScheme: "prior"}
			v := raw
			if v == "" {
				v = "prior"
			}
			want := VultrConfig{Region: v, OS: v, Image: v, Snapshot: v, FirewallGroup: v, UserScheme: v}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Vultr, want) {
				t.Fatalf("env=%#v want=%#v", cfg.Vultr, want)
			}
		})
	}
}

func TestVultrBindingLists(t *testing.T) {
	for _, field := range []struct{ key, member, env string }{{"vpcIds", "VPCIDs", "CRABBOX_VULTR_VPC_IDS"}, {"sshCIDRs", "SSHCIDRs", "CRABBOX_VULTR_SSH_CIDRS"}} {
		for _, trusted := range []bool{false, true} {
			for _, mode := range []string{"missing", "null", "empty", "raw"} {
				fields := map[string]any{}
				switch mode {
				case "null":
					fields[field.key] = nil
				case "empty":
					fields[field.key] = []string{}
				case "raw":
					fields[field.key] = []string{" fixture ", "", "fixture", "fixture"}
				}
				data, err := yaml.Marshal(map[string]any{"vultr": fields})
				if err != nil {
					t.Fatal(err)
				}
				var file fileConfig
				if err := yaml.Unmarshal(data, &file); err != nil {
					t.Fatal(err)
				}
				cfg := baseConfig()
				dest := reflect.ValueOf(&cfg.Vultr).Elem().FieldByName(field.member)
				dest.Set(reflect.ValueOf([]string{"prior"}))
				prior := dest.Pointer()
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if mode != "raw" {
					if !reflect.DeepEqual(dest.Interface(), []string{"prior"}) || dest.Pointer() != prior {
						t.Fatal("ignored list changed")
					}
				} else {
					if !reflect.DeepEqual(dest.Interface(), []string{" fixture ", "", "fixture", "fixture"}) {
						t.Fatalf("raw list=%#v", dest.Interface())
					}
					v := reflect.ValueOf(file.Vultr).Elem().FieldByName(field.member)
					if v.Kind() == reflect.Pointer {
						v = v.Elem()
					}
					if dest.Pointer() != v.Pointer() {
						t.Fatal("list backing not shared")
					}
				}
			}
		}
		for _, tc := range []struct {
			raw  string
			want []string
		}{{"", nil}, {" , , ", []string{}}, {"none", []string{"none"}}, {" a, , b,a ", []string{"a", "b", "a"}}} {
			t.Run(field.key+tc.raw, func(t *testing.T) {
				t.Setenv(field.env, tc.raw)
				cfg := baseConfig()
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				got := reflect.ValueOf(cfg.Vultr).FieldByName(field.member).Interface()
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("env list=%#v want=%#v", got, tc.want)
				}
				if tc.raw == "" {
					reflect.ValueOf(&cfg.Vultr).Elem().FieldByName(field.member).Set(reflect.ValueOf([]string{"prior"}))
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(reflect.ValueOf(cfg.Vultr).FieldByName(field.member).Interface(), []string{"prior"}) {
						t.Fatal("empty env changed prior")
					}
				}
			})
		}
	}
}

func TestVultrBindingCoreDefaults(t *testing.T) {
	for _, raw := range []string{"", "  ", "custom"} {
		cfg := baseConfig()
		cfg.Provider = "vultr"
		cfg.Vultr.Region = raw
		cfg.Vultr.UserScheme = raw
		cfg.Location = "generic"
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		r, u := raw, raw
		if raw == "" {
			r = "ewr"
			u = "root"
		}
		if cfg.Vultr.Region != r || cfg.Vultr.UserScheme != u || cfg.Vultr.OS != "" || cfg.Vultr.Image != "" || cfg.Vultr.Snapshot != "" {
			t.Fatalf("defaults=%#v", cfg.Vultr)
		}
	}
}

func TestVultrRuntimeTransformCore(t *testing.T) {
	for _, region := range []string{"", "custom-region", "  "} {
		for _, scheme := range []string{"", "custom-scheme", "  ", "limited", "LIMITED", " limited "} {
			for _, lists := range []string{"nil", "empty", "shared"} {
				for _, explicit := range []bool{false, true} {
					cfg := baseConfig()
					cfg.Provider = "vultr"
					cfg.Location = "generic-region"
					cfg.Class = "standard"
					cfg.Vultr = VultrConfig{Region: region, UserScheme: scheme, OS: "raw-os", Image: "raw-image", Snapshot: "raw-snapshot", FirewallGroup: "raw-group"}
					switch lists {
					case "empty":
						cfg.Vultr.VPCIDs = []string{}
						cfg.Vultr.SSHCIDRs = []string{}
					case "shared":
						cfg.Vultr.VPCIDs = []string{"vpc-a", "vpc-a"}
						cfg.Vultr.SSHCIDRs = []string{" 192.0.2.0/24 ", ""}
					}
					before := cfg.Vultr
					want := before
					if region == "" {
						want.Region = "ewr"
					}
					if scheme == "" {
						want.UserScheme = "root"
					}
					user, port, root := "root", "22", "/work/crabbox"
					if explicit {
						user, port, root = "alice", "2200", "/srv/project"
						cfg.SSHUser = user
						cfg.SSHPort = port
						cfg.WorkRoot = root
						MarkSSHUserExplicit(&cfg)
						MarkSSHPortExplicit(&cfg)
						MarkWorkRootExplicit(&cfg)
					}
					if err := applyProviderConfigDefaults(&cfg); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(cfg.Vultr, want) {
						t.Fatalf("region=%q scheme=%q lists=%s got=%#v want=%#v", region, scheme, lists, cfg.Vultr, want)
					}
					if reflect.ValueOf(cfg.Vultr.VPCIDs).Pointer() != reflect.ValueOf(before.VPCIDs).Pointer() || reflect.ValueOf(cfg.Vultr.SSHCIDRs).Pointer() != reflect.ValueOf(before.SSHCIDRs).Pointer() {
						t.Fatal("core changed slice backing")
					}
					if cfg.SSHUser != user || cfg.SSHPort != port || cfg.WorkRoot != root || cfg.Class != "standard" || cfg.Location != "generic-region" || cfg.TargetOS != targetLinux || cfg.SSHFallbackPorts != nil {
						t.Fatalf("generic effects user=%q port=%q root=%q class=%q location=%q target=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, cfg.Class, cfg.Location, cfg.TargetOS)
					}
				}
			}
		}
	}
}

func TestVultrWithRuntimeDefaults(t *testing.T) {
	for _, tc := range []struct{ region, scheme, wantRegion, wantScheme string }{
		{"", "", "ewr", "root"},
		{"custom-region", "custom-scheme", "custom-region", "custom-scheme"},
		{"  ", " limited ", "  ", " limited "},
		{"", "custom-scheme", "ewr", "custom-scheme"},
		{"custom-region", "", "custom-region", "root"},
	} {
		for _, listState := range []string{"nil", "empty", "populated"} {
			input := VultrConfig{Region: tc.region, UserScheme: tc.scheme, OS: "raw-os", Image: "raw-image", Snapshot: "raw-snapshot", FirewallGroup: "raw-group"}
			switch listState {
			case "empty":
				input.VPCIDs = []string{}
				input.SSHCIDRs = []string{}
			case "populated":
				input.VPCIDs = []string{"vpc-a", "vpc-a"}
				input.SSHCIDRs = []string{" 192.0.2.0/24 ", ""}
			}
			// Independent slice copies retain nilness and expose mutations to input storage.
			original := input
			if input.VPCIDs != nil {
				original.VPCIDs = make([]string, len(input.VPCIDs))
				copy(original.VPCIDs, input.VPCIDs)
			}
			if input.SSHCIDRs != nil {
				original.SSHCIDRs = make([]string, len(input.SSHCIDRs))
				copy(original.SSHCIDRs, input.SSHCIDRs)
			}
			want := original
			want.Region = tc.wantRegion
			want.UserScheme = tc.wantScheme
			got := input.WithRuntimeDefaults()
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(input, original) {
				t.Fatalf("region=%q scheme=%q lists=%s got=%#v input=%#v want=%#v", tc.region, tc.scheme, listState, got, input, want)
			}
			if reflect.ValueOf(got.VPCIDs).Pointer() != reflect.ValueOf(input.VPCIDs).Pointer() || reflect.ValueOf(got.SSHCIDRs).Pointer() != reflect.ValueOf(input.SSHCIDRs).Pointer() {
				t.Fatal("result must share slice backing")
			}
			again := got.WithRuntimeDefaults()
			if !reflect.DeepEqual(again, want) || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(input, original) {
				t.Fatal("runtime defaults must be idempotent without mutating receiver or slices")
			}
			if reflect.ValueOf(again.VPCIDs).Pointer() != reflect.ValueOf(input.VPCIDs).Pointer() || reflect.ValueOf(again.SSHCIDRs).Pointer() != reflect.ValueOf(input.SSHCIDRs).Pointer() {
				t.Fatal("idempotent result must retain slice backing")
			}
		}
	}
}

func TestLinodeBindingSources(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, mode := range []string{"missing", "null", "empty", "equal", "padded", "custom"} {
			cfg := baseConfig()
			cfg.Linode = LinodeConfig{Region: "prior", Image: "prior", Type: "prior", FirewallID: "prior"}
			want := cfg.Linode
			fields := map[string]any{}
			accepted := mode == "equal" || mode == "padded" || mode == "custom"
			for _, f := range []struct {
				key string
				v   *string
			}{{"region", &want.Region}, {"image", &want.Image}, {"type", &want.Type}, {"firewall", &want.FirewallID}} {
				switch mode {
				case "null":
					fields[f.key] = nil
				case "empty":
					fields[f.key] = ""
				case "equal":
					fields[f.key] = "prior"
				case "padded":
					fields[f.key] = "  "
				case "custom":
					fields[f.key] = "fixture"
				}
				if accepted {
					*f.v = fields[f.key].(string)
				}
			}
			data, err := yaml.Marshal(map[string]any{"linode": fields})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Linode, want) || cfg.linodeImageExplicit != accepted || cfg.linodeTypeExplicit != accepted {
				t.Fatalf("file mode=%s trusted=%t cfg=%#v markers=%t/%t", mode, trusted, cfg.Linode, cfg.linodeImageExplicit, cfg.linodeTypeExplicit)
			}
		}
	}
	for _, raw := range []string{"", "prior", "  ", "fixture"} {
		t.Run("env-"+raw, func(t *testing.T) {
			for _, key := range []string{"REGION", "IMAGE", "TYPE", "FIREWALL"} {
				t.Setenv("CRABBOX_LINODE_"+key, raw)
			}
			cfg := baseConfig()
			cfg.Linode = LinodeConfig{Region: "prior", Image: "prior", Type: "prior", FirewallID: "prior"}
			v := raw
			if v == "" {
				v = "prior"
			}
			want := LinodeConfig{Region: v, Image: v, Type: v, FirewallID: v}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Linode, want) || cfg.linodeImageExplicit != (raw != "") || cfg.linodeTypeExplicit != (raw != "") {
				t.Fatalf("env cfg=%#v markers=%t/%t", cfg.Linode, cfg.linodeImageExplicit, cfg.linodeTypeExplicit)
			}
		})
	}
	for _, field := range []string{"image", "type"} {
		for _, source := range []string{"user", "repo", "env"} {
			t.Run(field+source, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Linode.Image = "same"
				cfg.Linode.Type = "same"
				if source == "env" {
					t.Setenv("CRABBOX_LINODE_"+strings.ToUpper(field), "same")
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					var file fileConfig
					if err := yaml.Unmarshal([]byte("linode: {"+field+": same}"), &file); err != nil {
						t.Fatal(err)
					}
					if err := applyFileConfigWithTrust(&cfg, file, source == "user"); err != nil {
						t.Fatal(err)
					}
				}
				if cfg.linodeImageExplicit != (field == "image") || cfg.linodeTypeExplicit != (field == "type") {
					t.Fatal("independent accepted markers changed")
				}
			})
		}
	}
}

func TestLinodeBindingLists(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, raw := range []string{"{}", "{sshCIDRs: null}", "{sshCIDRs: []}", "{sshCIDRs: [' 192.0.2.0/24 ', '', '192.0.2.0/24']}"} {
			var file fileConfig
			if err := yaml.Unmarshal([]byte("linode: "+raw), &file); err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			cfg.Linode.SSHCIDRs = []string{"prior"}
			prior := &cfg.Linode.SSHCIDRs[0]
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Linode.SSHCIDRs) == 1 {
				if cfg.Linode.SSHCIDRs[0] != "prior" || &cfg.Linode.SSHCIDRs[0] != prior {
					t.Fatal("ignored list changed")
				}
				continue
			}
			if !reflect.DeepEqual(cfg.Linode.SSHCIDRs, []string{" 192.0.2.0/24 ", "", "192.0.2.0/24"}) {
				t.Fatalf("list=%#v", cfg.Linode.SSHCIDRs)
			}
			v := reflect.ValueOf(file.Linode).Elem().FieldByName("SSHCIDRs")
			if v.Kind() == reflect.Pointer {
				v = v.Elem()
			}
			if v.Pointer() != reflect.ValueOf(cfg.Linode.SSHCIDRs).Pointer() {
				t.Fatal("file list backing not shared")
			}
		}
	}
	for _, tc := range []struct {
		raw  string
		want []string
	}{{"", nil}, {" , , ", []string{}}, {"none", []string{"none"}}, {" a, ,b,a ", []string{"a", "b", "a"}}} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("CRABBOX_LINODE_SSH_CIDRS", tc.raw)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Linode.SSHCIDRs, tc.want) {
				t.Fatalf("env list=%#v want=%#v", cfg.Linode.SSHCIDRs, tc.want)
			}
			if tc.raw == "" {
				cfg.Linode.SSHCIDRs = []string{"prior"}
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(cfg.Linode.SSHCIDRs, []string{"prior"}) {
					t.Fatal("empty env changed prior")
				}
			}
		})
	}
}

func TestLinodeBindingCoreDefaults(t *testing.T) {
	cfg := baseConfig()
	if cfg.OSImage != "ubuntu:26.04" || !reflect.DeepEqual(cfg.Linode, LinodeConfig{Region: "us-ord", Type: "g6-standard-1"}) || cfg.linodeImageExplicit || cfg.linodeTypeExplicit {
		t.Fatalf("raw base=%#v os=%q", cfg.Linode, cfg.OSImage)
	}
	for _, tc := range []struct {
		os, image               string
		explicit, providerImage bool
		want                    string
	}{{"ubuntu:26.04", "", false, false, "linode/ubuntu24.04"}, {"ubuntu:24.04", "", true, false, "linode/ubuntu24.04"}, {"ubuntu:26.04", "", true, false, ""}, {"ubuntu:26.04", "custom", true, true, "custom"}} {
		cfg := baseConfig()
		cfg.Provider = "linode"
		cfg.Linode.Region = ""
		cfg.Linode.Type = ""
		cfg.OSImage = tc.os
		cfg.osImageExplicit = tc.explicit
		cfg.Linode.Image = tc.image
		cfg.linodeImageExplicit = tc.providerImage
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Linode.Region != "us-ord" || cfg.Linode.Type != "g6-standard-1" || cfg.Linode.Image != tc.want {
			t.Fatalf("os=%q cfg=%#v", tc.os, cfg.Linode)
		}
	}
	for _, raw := range []string{"  ", "custom"} {
		cfg := baseConfig()
		cfg.Provider = "linode"
		cfg.Linode = LinodeConfig{Region: raw, Type: raw, Image: raw}
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Linode.Region != raw || cfg.Linode.Type != raw || cfg.Linode.Image != raw {
			t.Fatalf("raw defaults=%#v", cfg.Linode)
		}
	}
}

func TestLinodeTypedInitializer(t *testing.T) {
	for _, image := range []string{"", "linode/ubuntu24.04", " custom-image "} {
		got := initialLinodeConfig(image)
		want := LinodeConfig{Region: "us-ord", Image: image, Type: "g6-standard-1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("initialLinodeConfig(%q) = %#v, want %#v", image, got, want)
		}
	}
}

func TestLambdaBindingSources(t *testing.T) {
	for _, source := range []string{"user", "repo", "env"} {
		for _, raw := range []string{"", "same", "  ", "custom"} {
			t.Run(source+raw, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Lambda = LambdaConfig{Region: "same", Type: "same", Image: "same", ImageFamily: "same", FirewallRuleset: "same"}
				v := raw
				if v == "" {
					v = "same"
				}
				want := LambdaConfig{Region: v, Type: v, Image: v, ImageFamily: v, FirewallRuleset: v}
				if source == "env" {
					for _, key := range []string{"REGION", "TYPE", "IMAGE", "IMAGE_FAMILY", "FIREWALL_RULESET"} {
						t.Setenv("CRABBOX_LAMBDA_"+key, raw)
					}
					if raw != "" {
						want.Image = ""
					}
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					data, err := yaml.Marshal(map[string]any{"lambda": map[string]any{"region": raw, "type": raw, "image": raw, "imageFamily": raw, "firewallRuleset": raw}})
					if err != nil {
						t.Fatal(err)
					}
					var file fileConfig
					if err := yaml.Unmarshal(data, &file); err != nil {
						t.Fatal(err)
					}
					if err := applyFileConfigWithTrust(&cfg, file, source == "user"); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(cfg.Lambda, want) || cfg.lambdaTypeExplicit != (raw != "") || cfg.lambdaImageExplicit != (raw != "") || cfg.lambdaImageFamilyExplicit != (raw != "") {
					t.Fatalf("source=%s raw=%q got=%#v want=%#v", source, raw, cfg.Lambda, want)
				}
			})
		}
	}
	for _, raw := range []string{"{}", "null", "{region: null, type: null, image: null, imageFamily: null, firewallRuleset: null}"} {
		var file fileConfig
		if err := yaml.Unmarshal([]byte("lambda: "+raw), &file); err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		before := cfg.Lambda
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg.Lambda, before) || cfg.lambdaTypeExplicit || cfg.lambdaImageExplicit || cfg.lambdaImageFamilyExplicit {
			t.Fatal("empty/null input changed config")
		}
	}
}

func TestLambdaBindingPairs(t *testing.T) {
	for _, source := range []string{"user", "repo", "env"} {
		for _, tc := range []struct{ image, family, wantImage, wantFamily string }{{"new-image", "", "new-image", ""}, {"", "new-family", "old-image", "new-family"}, {"new-image", "new-family", "new-image", "new-family"}} {
			t.Run(source+tc.image+tc.family, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Lambda.Image = "old-image"
				cfg.Lambda.ImageFamily = "old-family"
				wi, wf := tc.wantImage, tc.wantFamily
				if source == "env" {
					t.Setenv("CRABBOX_LAMBDA_IMAGE", tc.image)
					t.Setenv("CRABBOX_LAMBDA_IMAGE_FAMILY", tc.family)
					if tc.family != "" {
						wi = ""
					}
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := applyFileConfigWithTrust(&cfg, fileConfig{Lambda: &fileLambdaConfig{Image: tc.image, ImageFamily: tc.family}}, source == "user"); err != nil {
						t.Fatal(err)
					}
				}
				if cfg.Lambda.Image != wi || cfg.Lambda.ImageFamily != wf || cfg.lambdaImageExplicit != (tc.image != "") || cfg.lambdaImageFamilyExplicit != (tc.family != "") || cfg.lambdaTypeExplicit {
					t.Fatalf("pair image=%q family=%q", cfg.Lambda.Image, cfg.Lambda.ImageFamily)
				}
			})
		}
	}
}

func TestLambdaBindingLists(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			file := fileConfig{Lambda: &fileLambdaConfig{SSHCIDRs: []string{" raw ", "", "raw"}, FilesystemNames: []string{" data ", "data"}, FilesystemMounts: []LambdaFilesystemMount{{Name: " data ", MountPath: " /mnt/data "}, {}}}}
			cfg := baseConfig()
			if empty {
				file.Lambda.SSHCIDRs = []string{}
				file.Lambda.FilesystemNames = []string{}
				file.Lambda.FilesystemMounts = []LambdaFilesystemMount{}
				cfg.Lambda.SSHCIDRs = []string{"prior"}
				cfg.Lambda.FilesystemNames = []string{"prior"}
				cfg.Lambda.FilesystemMounts = []LambdaFilesystemMount{{Name: "prior"}}
			}
			before := cfg.Lambda
			if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
				t.Fatal(err)
			}
			if empty {
				if !reflect.DeepEqual(cfg.Lambda, before) {
					t.Fatal("empty lists replaced prior")
				}
			} else {
				for _, name := range []string{"SSHCIDRs", "FilesystemNames", "FilesystemMounts"} {
					got := reflect.ValueOf(cfg.Lambda).FieldByName(name)
					want := reflect.ValueOf(file.Lambda).Elem().FieldByName(name)
					if !reflect.DeepEqual(got.Interface(), want.Interface()) || got.Pointer() != want.Pointer() {
						t.Fatalf("file list %s not raw/shared", name)
					}
				}
			}
		}
	}
	for _, raw := range []string{"", " , ", "none", " a:/mnt/a, b, a:/mnt/a "} {
		t.Run(raw, func(t *testing.T) {
			for _, key := range []string{"SSH_CIDRS", "FILESYSTEM_NAMES", "FILESYSTEM_MOUNTS"} {
				t.Setenv("CRABBOX_LAMBDA_"+key, raw)
			}
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			var strs []string
			var mounts []LambdaFilesystemMount
			switch raw {
			case " , ":
				strs = []string{}
				mounts = []LambdaFilesystemMount{}
			case "none":
				strs = []string{"none"}
				mounts = []LambdaFilesystemMount{{Name: "none"}}
			case " a:/mnt/a, b, a:/mnt/a ":
				strs = []string{"a:/mnt/a", "b", "a:/mnt/a"}
				mounts = []LambdaFilesystemMount{{Name: "a", MountPath: "/mnt/a"}, {Name: "b"}, {Name: "a", MountPath: "/mnt/a"}}
			}
			if !reflect.DeepEqual(cfg.Lambda.SSHCIDRs, strs) || !reflect.DeepEqual(cfg.Lambda.FilesystemNames, strs) || !reflect.DeepEqual(cfg.Lambda.FilesystemMounts, mounts) {
				t.Fatalf("env lists=%#v", cfg.Lambda)
			}
			if len(strs) > 0 {
				other := baseConfig()
				if err := applyEnv(&other); err != nil {
					t.Fatal(err)
				}
				cfg.Lambda.SSHCIDRs[0] = "changed"
				cfg.Lambda.FilesystemNames[0] = "changed"
				cfg.Lambda.FilesystemMounts[0].Name = "changed"
				if !reflect.DeepEqual(other.Lambda.SSHCIDRs, strs) || !reflect.DeepEqual(other.Lambda.FilesystemNames, strs) || !reflect.DeepEqual(other.Lambda.FilesystemMounts, mounts) {
					t.Fatal("env collections unexpectedly shared")
				}
			}
		})
	}
}

func TestLambdaBindingCoreDefaults(t *testing.T) {
	cfg := baseConfig()
	if !reflect.DeepEqual(cfg.Lambda, LambdaConfig{Region: "us-west-1", Type: "gpu_1x_a10", ImageFamily: "lambda-stack-24-04"}) || cfg.lambdaTypeExplicit || cfg.lambdaImageExplicit || cfg.lambdaImageFamilyExplicit {
		t.Fatalf("base=%#v", cfg.Lambda)
	}
	for _, tc := range []struct {
		os       string
		explicit bool
		family   string
	}{{"ubuntu:26.04", false, "lambda-stack-24-04"}, {"ubuntu:24.04", true, "lambda-stack-24-04"}, {"ubuntu:26.04", true, ""}} {
		cfg := baseConfig()
		cfg.Provider = "lambda"
		cfg.OSImage = tc.os
		cfg.osImageExplicit = tc.explicit
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Lambda.ImageFamily != tc.family || cfg.Lambda.Image != "" {
			t.Fatalf("OS=%s lambda=%#v", tc.os, cfg.Lambda)
		}
	}
	for _, raw := range []string{"", "  ", "custom", "us-west-1"} {
		for _, pair := range []struct{ image, family, wantFamily string }{{"", "", "lambda-stack-24-04"}, {"image", "", ""}, {"", "family", "family"}, {"image", "family", "family"}, {"  ", "  ", "  "}} {
			cfg := baseConfig()
			cfg.Provider = "lambda"
			cfg.Class = "standard"
			cfg.Lambda = LambdaConfig{Region: raw, Type: raw, Image: pair.image, ImageFamily: pair.family, FirewallRuleset: "rule", SSHCIDRs: []string{" raw ", ""}, FilesystemNames: []string{"data", "data"}, FilesystemMounts: []LambdaFilesystemMount{{Name: " data ", MountPath: " /mnt/data "}}}
			cidrs, names, mounts := cfg.Lambda.SSHCIDRs, cfg.Lambda.FilesystemNames, cfg.Lambda.FilesystemMounts
			r, typ := raw, raw
			if raw == "" {
				r = "us-west-1"
				typ = "gpu_1x_a10"
			}
			want := LambdaConfig{Region: r, Type: typ, Image: pair.image, ImageFamily: pair.wantFamily, FirewallRuleset: "rule", SSHCIDRs: []string{" raw ", ""}, FilesystemNames: []string{"data", "data"}, FilesystemMounts: []LambdaFilesystemMount{{Name: " data ", MountPath: " /mnt/data "}}}
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Lambda, want) || &cfg.Lambda.SSHCIDRs[0] != &cidrs[0] || &cfg.Lambda.FilesystemNames[0] != &names[0] || &cfg.Lambda.FilesystemMounts[0] != &mounts[0] {
				t.Fatalf("raw=%q pair=%#v cfg=%#v", raw, pair, cfg.Lambda)
			}
			if cfg.SSHUser != "ubuntu" || cfg.SSHPort != "22" || cfg.WorkRoot != "/work/crabbox" || cfg.Class != "standard" || cfg.TargetOS != targetLinux {
				t.Fatal("core generic effects changed")
			}
		}
	}
	for _, tc := range []struct {
		os                        string
		imageMarker, familyMarker bool
		want                      string
	}{{"ubuntu:24.04", false, false, "lambda-stack-24-04"}, {"ubuntu:26.04", false, false, ""}, {"ubuntu:26.04", true, false, "family"}, {"ubuntu:26.04", false, true, "family"}, {"ubuntu:24.04", true, true, "family"}} {
		cfg := baseConfig()
		cfg.Provider = "lambda"
		cfg.Lambda = LambdaConfig{Image: "image", ImageFamily: "family"}
		cfg.OSImage = tc.os
		cfg.osImageExplicit = true
		cfg.lambdaImageExplicit = tc.imageMarker
		cfg.lambdaImageFamilyExplicit = tc.familyMarker
		cfg.SSHUser = "alice"
		cfg.SSHPort = "2200"
		cfg.WorkRoot = "/srv/project"
		MarkSSHUserExplicit(&cfg)
		MarkSSHPortExplicit(&cfg)
		MarkWorkRootExplicit(&cfg)
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Lambda.Image != "image" || cfg.Lambda.ImageFamily != tc.want || cfg.Lambda.Region != "us-west-1" || cfg.Lambda.Type != "gpu_1x_a10" || cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WorkRoot != "/srv/project" || cfg.Lambda.SSHCIDRs != nil || cfg.Lambda.FilesystemNames != nil || cfg.Lambda.FilesystemMounts != nil {
			t.Fatalf("explicit OS case=%#v cfg=%#v", tc, cfg.Lambda)
		}
	}

}

func TestLambdaWithRuntimeDefaults(t *testing.T) {
	fixture := func() LambdaConfig {
		return LambdaConfig{FirewallRuleset: "rule", SSHCIDRs: []string{" raw ", ""}, FilesystemNames: []string{"data", "data"}, FilesystemMounts: []LambdaFilesystemMount{{Name: "data", MountPath: "/mnt/data"}}}
	}
	cfg, before, want := fixture(), fixture(), fixture()
	want.Region, want.Type, want.ImageFamily = "us-west-1", "gpu_1x_a10", "lambda-stack-24-04"
	got := cfg.WithRuntimeDefaults()
	if !reflect.DeepEqual(cfg, before) || !reflect.DeepEqual(got, want) {
		t.Fatalf("receiver=%#v result=%#v", cfg, got)
	}
	if &got.SSHCIDRs[0] != &cfg.SSHCIDRs[0] || &got.FilesystemNames[0] != &cfg.FilesystemNames[0] || &got.FilesystemMounts[0] != &cfg.FilesystemMounts[0] {
		t.Fatal("runtime defaults copied collection backing arrays")
	}
	if !reflect.DeepEqual(got.WithRuntimeDefaults(), got) {
		t.Fatal("runtime defaults are not idempotent")
	}
}

func TestLookupEnvFloatAcceptance(t *testing.T) {
	const name = "CRABBOX_TEST_FLOAT_ACCEPTANCE"
	const fallback = 17.25
	for _, tc := range []struct {
		name, raw        string
		absent, accepted bool
		want             float64
	}{
		{name: "absent", absent: true}, {name: "empty"}, {name: "space", raw: " "},
		{name: "padded", raw: " 2.5 "}, {name: "invalid", raw: "invalid"}, {name: "range", raw: "1e400"},
		{name: "zero", raw: "0", accepted: true}, {name: "negative-zero", raw: "-0", accepted: true, want: math.Copysign(0, -1)},
		{name: "negative", raw: "-2.5", accepted: true, want: -2.5}, {name: "same", raw: "17.25", accepted: true, want: fallback},
		{name: "hex", raw: "0x1.8p+1", accepted: true, want: 3}, {name: "nan", raw: "NaN", accepted: true, want: math.NaN()},
		{name: "positive-inf", raw: "+Inf", accepted: true, want: math.Inf(1)}, {name: "negative-inf", raw: "-Inf", accepted: true, want: math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(name, tc.raw)
			if tc.absent {
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			got, accepted := lookupEnvFloat(name)
			if accepted != tc.accepted {
				t.Fatalf("accepted=%t want=%t", accepted, tc.accepted)
			}
			if math.IsNaN(tc.want) {
				if !math.IsNaN(got) {
					t.Fatalf("value=%v want NaN", got)
				}
			} else if got != tc.want || math.Signbit(got) != math.Signbit(tc.want) {
				t.Fatalf("value=%v want=%v", got, tc.want)
			}
			wrapped := getenvFloat(name, fallback)
			want := tc.want
			if !tc.accepted {
				want = fallback
			}
			if math.IsNaN(want) {
				if !math.IsNaN(wrapped) {
					t.Fatalf("wrapper=%v want NaN", wrapped)
				}
			} else if wrapped != want || math.Signbit(wrapped) != math.Signbit(want) {
				t.Fatalf("wrapper=%v want=%v", wrapped, want)
			}
		})
	}
}

func TestGetenvNonNegativeIntAcceptance(t *testing.T) {
	const name = "CRABBOX_TEST_INT_ACCEPTANCE"
	const fallback = 37
	for _, tc := range []struct {
		name, raw        string
		absent, accepted bool
		want             int
		errorSuffix      string
	}{
		{name: "absent", absent: true, want: fallback}, {name: "empty", want: fallback},
		{name: "zero", raw: "0", accepted: true}, {name: "negative-zero", raw: "-0", accepted: true},
		{name: "same", raw: "37", accepted: true, want: fallback}, {name: "positive", raw: "+12", accepted: true, want: 12},
		{name: "negative", raw: "-2", errorSuffix: " must be non-negative"},
		{name: "space", raw: " ", errorSuffix: " must be an integer"}, {name: "padded", raw: " 12 ", errorSuffix: " must be an integer"},
		{name: "fractional", raw: "1.5", errorSuffix: " must be an integer"}, {name: "invalid", raw: "invalid", errorSuffix: " must be an integer"},
		{name: "range", raw: "999999999999999999999999999999", errorSuffix: " must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(name, tc.raw)
			if tc.absent {
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			got, accepted, err := getenvNonNegativeIntAccepted(name, fallback)
			wrapped, wrappedErr := getenvNonNegativeInt(name, fallback)
			if got != tc.want || wrapped != tc.want || accepted != tc.accepted {
				t.Fatalf("value=%d wrapper=%d accepted=%t want=%d/%t", got, wrapped, accepted, tc.want, tc.accepted)
			}
			for _, result := range []error{err, wrappedErr} {
				if tc.errorSuffix == "" {
					if result != nil {
						t.Fatal(result)
					}
				} else {
					var exitErr ExitError
					if result == nil || result.Error() != name+tc.errorSuffix || !AsExitError(result, &exitErr) || exitErr.Code != 2 {
						t.Fatalf("error=%v want exit2 %q", result, name+tc.errorSuffix)
					}
				}
			}
		})
	}
}

func TestConcreteConfigInputSourceAttribution(t *testing.T) {
	for _, tc := range []struct {
		root, key, env, raw string
		value               any
		owners              []configInputOwner
	}{
		{"appleContainer", "cliPath", "CRABBOX_APPLE_CONTAINER_CLI", "container", "container", []configInputOwner{"apple-container", "apple-machine"}},
		{"appleContainer", "image", "CRABBOX_APPLE_CONTAINER_IMAGE", "fixture", "fixture", []configInputOwner{"apple-container", "apple-machine"}},
		{"appleContainer", "user", "CRABBOX_APPLE_CONTAINER_USER", "crabbox", "crabbox", []configInputOwner{"apple-container", "apple-machine"}},
		{"appleContainer", "workRoot", "CRABBOX_APPLE_CONTAINER_WORK_ROOT", "/work/crabbox", "/work/crabbox", []configInputOwner{"apple-container", "apple-machine"}},
		{"appleContainer", "cpus", "CRABBOX_APPLE_CONTAINER_CPUS", "2", 2, []configInputOwner{"apple-container", "apple-machine"}},
		{"appleContainer", "memory", "CRABBOX_APPLE_CONTAINER_MEMORY", " ", " ", []configInputOwner{"apple-container", "apple-machine"}},
		{"appleContainer", "extraRunArgs", "CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS", "fixture", []string{"fixture"}, []configInputOwner{"apple-container", "apple-machine"}},
		{"appleVM", "helperPath", "CRABBOX_APPLE_VM_HELPER", "helper", "helper", []configInputOwner{"apple-vm"}},
		{"appleVM", "image", "CRABBOX_APPLE_VM_IMAGE", "fixture", "fixture", []configInputOwner{"apple-vm"}},
		{"appleVM", "imageSHA256", "CRABBOX_APPLE_VM_IMAGE_SHA256", "fixture", "fixture", []configInputOwner{"apple-vm"}},
		{"appleVM", "user", "CRABBOX_APPLE_VM_USER", "crabbox", "crabbox", []configInputOwner{"apple-vm"}},
		{"appleVM", "workRoot", "CRABBOX_APPLE_VM_WORK_ROOT", "/work/crabbox", "/work/crabbox", []configInputOwner{"apple-vm"}},
		{"appleVM", "cpus", "CRABBOX_APPLE_VM_CPUS", "0", 0, []configInputOwner{"apple-vm"}},
		{"appleVM", "memoryMiB", "CRABBOX_APPLE_VM_MEMORY", "-1", -1, []configInputOwner{"apple-vm"}},
		{"appleVM", "diskGiB", "CRABBOX_APPLE_VM_DISK", "30", 30, []configInputOwner{"apple-vm"}},
		{"lambda", "region", "CRABBOX_LAMBDA_REGION", "us-west-1", "us-west-1", []configInputOwner{"lambda"}},
		{"lambda", "type", "CRABBOX_LAMBDA_TYPE", "gpu_1x_a10", "gpu_1x_a10", []configInputOwner{"lambda"}},
		{"lambda", "image", "CRABBOX_LAMBDA_IMAGE", "fixture", "fixture", []configInputOwner{"lambda"}},
		{"lambda", "imageFamily", "CRABBOX_LAMBDA_IMAGE_FAMILY", "fixture", "fixture", []configInputOwner{"lambda"}},
		{"lambda", "firewallRuleset", "CRABBOX_LAMBDA_FIREWALL_RULESET", " ", " ", []configInputOwner{"lambda"}},
		{"lambda", "sshCIDRs", "CRABBOX_LAMBDA_SSH_CIDRS", " , ", []string{""}, []configInputOwner{"lambda"}},
		{"lambda", "filesystemNames", "CRABBOX_LAMBDA_FILESYSTEM_NAMES", " , ", []string{""}, []configInputOwner{"lambda"}},
		{"lambda", "filesystemMounts", "CRABBOX_LAMBDA_FILESYSTEM_MOUNTS", " , ", []LambdaFilesystemMount{{Name: "fixture", MountPath: "/mnt/fixture"}}, []configInputOwner{"lambda"}},
		{"localContainer", "runtime", "CRABBOX_LOCAL_CONTAINER_RUNTIME", "docker", "docker", []configInputOwner{"local-container"}},
		{"localContainer", "image", "CRABBOX_LOCAL_CONTAINER_IMAGE", "fixture", "fixture", []configInputOwner{"local-container"}},
		{"localContainer", "user", "CRABBOX_LOCAL_CONTAINER_USER", "crabbox", "crabbox", []configInputOwner{"local-container"}},
		{"localContainer", "workRoot", "CRABBOX_LOCAL_CONTAINER_WORK_ROOT", "/work/crabbox", "/work/crabbox", []configInputOwner{"local-container"}},
		{"localContainer", "cpus", "CRABBOX_LOCAL_CONTAINER_CPUS", "2", 2, []configInputOwner{"local-container"}},
		{"localContainer", "memory", "CRABBOX_LOCAL_CONTAINER_MEMORY", " ", " ", []configInputOwner{"local-container"}},
		{"localContainer", "network", "CRABBOX_LOCAL_CONTAINER_NETWORK", "bridge", "bridge", []configInputOwner{"local-container"}},
		{"localContainer", "dockerSocket", "CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET", "false", false, []configInputOwner{"local-container"}},
		{"localContainer", "noHostname", "CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME", "false", false, []configInputOwner{"local-container"}},
	} {
		for _, source := range []string{"user_config", "repo_config", "environment"} {
			t.Run(tc.root+"/"+tc.key+"/"+source, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				for repetition := 0; repetition < 2; repetition++ {
					// Reapply equal values with an empty ledger: equality is not acceptance.
					cfg.inputProvenance = nil
					if source == "environment" {
						t.Setenv(tc.env, tc.raw)
						if err := applyEnv(&cfg); err != nil {
							t.Fatal(err)
						}
					} else {
						data, err := yaml.Marshal(map[string]any{tc.root: map[string]any{tc.key: tc.value}})
						if err != nil {
							t.Fatal(err)
						}
						var file fileConfig
						if err := yaml.Unmarshal(data, &file); err != nil {
							t.Fatal(err)
						}
						inputSource := providerSelectionRepoConfig
						if source == "user_config" {
							inputSource = providerSelectionUserConfig
						}
						if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, source == "user_config", inputSource); err != nil {
							t.Fatal(err)
						}
					}
					if len(cfg.inputProvenance) != len(tc.owners) {
						t.Fatalf("ledger=%#v", cfg.inputProvenance)
					}
					for _, owner := range tc.owners {
						got := cfg.inputProvenance.summary(owner)
						if got.state != "present" || got.complete || got.effects != configInputValue || !reflect.DeepEqual(got.sources, []string{source}) {
							t.Fatalf("%s: %#v", owner, got)
						}
					}
				}
			})
		}
	}
}

func TestConcreteConfigInputIgnoredAndPartial(t *testing.T) {
	for _, tc := range []struct{ env, value string }{
		{"CRABBOX_APPLE_CONTAINER_CPUS", "not-an-int"},
		{"CRABBOX_LOCAL_CONTAINER_CPUS", "not-an-int"},
		{"CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME", "not-a-bool"},
		{"CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS", "  "},
	} {
		t.Run(tc.env, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			t.Setenv(tc.env, tc.value)
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.inputProvenance) != 0 {
				t.Fatalf("ignored input counted: %#v", cfg.inputProvenance)
			}
		})
	}
	for _, earlier := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial-%t", earlier), func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			t.Setenv("CRABBOX_APPLE_VM_CPUS", "not-an-int")
			if earlier {
				t.Setenv("CRABBOX_APPLE_VZ_USER", "crabbox")
			}
			if err := applyEnv(&cfg); err == nil || !strings.Contains(err.Error(), "CRABBOX_APPLE_VM_CPUS must be an integer") {
				t.Fatalf("error=%v", err)
			}
			got := cfg.inputProvenance.summary("apple-vm")
			if (got.state == "present") != earlier || got.complete {
				t.Fatalf("partial attribution=%#v", got)
			}
		})
	}
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("appleContainer: {cpus: 0, extraRunArgs: []}\nappleVM: {}\nlambda: {filesystemMounts: []}\nlocalContainer: {cpus: -1}\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
		t.Fatal(err)
	}
	if len(cfg.inputProvenance) != 0 {
		t.Fatalf("ignored file/defaults counted: %#v", cfg.inputProvenance)
	}
}

func TestConfigInputGenericFileGroups(t *testing.T) {
	for _, tc := range []struct {
		name, yaml    string
		accepted, bad bool
	}{
		{"unknown", "unknownFixture: true", false, false},
		{"selection-only", "provider: fixture", false, false},
		{"broker-selection-only", "broker: {provider: fixture}", false, false},
		{"empty-sections", "broker: {}\nssh: {}\nsync: {}\nrun: {}\nenv: {}\ncapacity: {}\nactions: {}\ntailscale: {}\nresults: {}\nshard: {}\ncache: {}", false, false},
		{"host-id", "hostId: fixture", true, false},
		{"coordinator", "coordinator: https://fixture.invalid", true, false},
		{"broker-mode", "broker: {mode: managed}", true, false},
		{"broker-bool", "broker: {autoWebVNC: false}", true, false},
		{"broker-access", "broker: {access: {clientId: fixture}}", true, false},
		{"ssh-port", "ssh: {port: '22'}", true, false},
		{"ssh-clear", "ssh: {fallbackPorts: []}", true, false},
		{"root", "workRoot: /work/fixture", true, false},
		{"lease-invalid", "ttl: invalid\nlease: {idleTimeout: 0s}", false, false},
		{"lease-valid", "lease: {ttl: 1s}", true, false},
		{"sync-blank-addition", "sync: {include: [' '], excludes: ['']}", false, false},
		{"sync-include", "sync: {include: [fixture], includes: [fixture]}", true, false},
		{"sync-exclude", "sync: {exclude: [fixture], excludes: [fixture]}", true, false},
		{"sync-false", "sync: {delete: false}", true, false},
		{"sync-zero-timeout", "sync: {timeout: 0s}", true, false},
		{"sync-negative-timeout", "sync: {timeout: '-1s'}", true, false},
		{"sync-invalid-timeout", "sync: {timeout: invalid}", false, false},
		{"sync-positive-size", "sync: {warnBytes: 1}", true, false},
		{"sync-zero-size", "sync: {warnBytes: 0}", false, false},
		{"preflight-clear", "run: {preflightTools: []}", true, false},
		{"allow-clear", "env: {allow: []}", true, false},
		{"capacity-replace-blank", "capacity: {regions: [' ']}", true, false},
		{"capacity-false", "capacity: {hints: false}", true, false},
		{"actions-replace-blank", "actions: {fields: [' ']}", true, false},
		{"actions-false", "actions: {ephemeral: false}", true, false},
		{"tailscale-false", "tailscale: {enabled: false}", true, false},
		{"tailscale-clear", "tailscale: {exitNode: ' '}", true, false},
		{"results-clear", "results: {junit: []}", true, false},
		{"results-false", "results: {auto: false}", true, false},
		{"shard-zero", "shard: {maxCount: 0}", true, false},
		{"cache-false", "cache: {pnpm: false}", true, false},
		{"cache-clear", "cache: {volumes: []}", true, false},
		{"cache-invalid", "cache: {volumes: [{key: fixture}]}", false, true},
		{"cache-partial", "cache: {pnpm: false, volumes: [{key: fixture}]}", true, true},
	} {
		for _, origin := range []providerSelectionSource{providerSelectionUserConfig, providerSelectionRepoConfig} {
			t.Run(tc.name+"/"+string(origin), func(t *testing.T) {
				var file fileConfig
				if err := yaml.Unmarshal([]byte(tc.yaml), &file); err != nil {
					t.Fatal(err)
				}
				cfg := baseConfig()
				for repeat := 0; repeat < 2; repeat++ {
					cfg.inputProvenance = nil
					err := applyFileConfigWithTrustAndProviderSource(&cfg, file, origin == providerSelectionUserConfig, origin)
					if (err != nil) != tc.bad {
						t.Fatalf("error=%v, want error %t", err, tc.bad)
					}
					got := cfg.inputProvenance.summary(configInputGeneric)
					if (got.effects&configInputValue != 0) != tc.accepted || got.complete {
						t.Fatalf("accepted=%t summary=%#v", tc.accepted, got)
					}
					if tc.accepted && !reflect.DeepEqual(got.sources, []string{string(origin)}) {
						t.Fatalf("sources=%v", got.sources)
					}
				}
			})
		}
	}
}

func TestConfigInputGenericNamedDefinitions(t *testing.T) {
	for _, group := range []string{"profiles", "presets", "proofTemplates", "jobs"} {
		for _, name := range []string{" ", "fixture"} {
			t.Run(group+"/"+name, func(t *testing.T) {
				data, err := yaml.Marshal(map[string]any{group: map[string]any{name: map[string]any{}}})
				if err != nil {
					t.Fatal(err)
				}
				var file fileConfig
				if err := yaml.Unmarshal(data, &file); err != nil {
					t.Fatal(err)
				}
				cfg := baseConfig()
				if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
					t.Fatal(err)
				}
				got := cfg.inputProvenance.summary(configInputGeneric)
				if (got.effects == configInputValue) != (name == "fixture") || got.complete {
					t.Fatalf("named definition: %#v", got)
				}
			})
		}
	}
	for _, raw := range []string{"0s", "-1s"} {
		t.Run("job-hydrate/"+raw, func(t *testing.T) {
			var file fileConfig
			if err := yaml.Unmarshal([]byte("jobs: {fixture: {provider: fixture-provider, hydrate: {waitTimeout: '"+raw+"'}}}"), &file); err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
				t.Fatal(err)
			}
			want, _ := time.ParseDuration(raw)
			if cfg.Jobs["fixture"].Hydrate.WaitTimeout != want || cfg.inputProvenance.summary(configInputGeneric).effects != configInputValue || len(cfg.inputProvenance) != 1 {
				t.Fatal("job duration policy or generic attribution changed")
			}
		})
	}
}

func TestConfigInputGenericEnvironmentGroups(t *testing.T) {
	for _, tc := range []struct {
		name, value   string
		accepted, bad bool
	}{
		{"CRABBOX_HOST_ID", "fixture", true, false},
		{"CRABBOX_COORDINATOR_MODE", "managed", true, false},
		{"CRABBOX_COORDINATOR_AUTO_WEBVNC", "false", true, false},
		{"CRABBOX_COORDINATOR_TOKEN_COMMAND", " ", false, false},
		{"CRABBOX_COORDINATOR_TOKEN_COMMAND", "[]", false, true},
		{"CRABBOX_COORDINATOR_TOKEN_COMMAND", "[\"fixture-tool\"]", true, false},
		{"CRABBOX_ACCESS_CLIENT_ID", "fixture", true, false},
		{"CRABBOX_SSH_PORT", "22", true, false},
		{"CRABBOX_SSH_FALLBACK_PORTS", "", true, false},
		{"CRABBOX_WORK_ROOT", "/work/fixture", true, false},
		{"CRABBOX_TTL", "invalid", false, false},
		{"CRABBOX_TTL", "1s", true, false},
		{"CRABBOX_CAPACITY_HINTS", "false", true, false},
		{"CRABBOX_CAPACITY_REGIONS", " , ", true, false},
		{"CRABBOX_ACTIONS_JOB", "fixture", true, false},
		{"CRABBOX_ACTIONS_EPHEMERAL", "false", true, false},
		{"CRABBOX_TAILSCALE", "false", true, false},
		{"CRABBOX_TAILSCALE_TAGS", " , ", true, false},
		{"CRABBOX_RESULTS_JUNIT", " , ", true, false},
		{"CRABBOX_RESULTS_AUTO", "invalid", false, false},
		{"CRABBOX_RESULTS_AUTO", "false", true, false},
		{"CRABBOX_CACHE_MAX_GB", "invalid", false, false},
		{"CRABBOX_CACHE_MAX_GB", "-1", true, false},
		{"CRABBOX_CACHE_VOLUMES", " , ", true, false},
		{"CRABBOX_SYNC_TIMEOUT", "0s", true, false},
		{"CRABBOX_SYNC_TIMEOUT", "-1s", true, false},
		{"CRABBOX_SYNC_TIMEOUT", "invalid", false, false},
		{"CRABBOX_SYNC_WARN_BYTES", "invalid", false, false},
		{"CRABBOX_SYNC_WARN_BYTES", "0", true, false},
		{"CRABBOX_SYNC_DELETE", "false", true, false},
		{"CRABBOX_ENV_ALLOW", " , ", true, false},
		{"CRABBOX_PREFLIGHT_TOOLS", " , ", true, false},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_SSH_FALLBACK_PORTS", "")
			if err := os.Unsetenv("CRABBOX_SSH_FALLBACK_PORTS"); err != nil {
				t.Fatal(err)
			}
			t.Setenv(tc.name, tc.value)
			cfg := baseConfig()
			for repeat := 0; repeat < 2; repeat++ {
				cfg.inputProvenance = nil
				err := applyEnv(&cfg)
				if (err != nil) != tc.bad {
					t.Fatalf("error=%v want error %t", err, tc.bad)
				}
				got := cfg.inputProvenance.summary(configInputGeneric)
				if (got.effects&configInputValue != 0) != tc.accepted || got.complete {
					t.Fatalf("accepted=%t summary=%#v", tc.accepted, got)
				}
			}
		})
	}
}

func TestConfigInputGenericAppendAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		extra, unique, ordered []string
		accepted               bool
	}{
		{"absent", nil, []string{"fixture"}, []string{"fixture"}, false},
		{"blank", []string{" ", ""}, []string{"fixture"}, []string{"fixture"}, false},
		{"equal", []string{"fixture"}, []string{"fixture"}, []string{"fixture", "fixture"}, true},
		{"ordered", []string{" second ", "second"}, []string{"fixture", "second"}, []string{"fixture", "second", "second"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unique, accepted := appendUniqueStringsAccepted([]string{"fixture"}, tc.extra...)
			if accepted != tc.accepted || !reflect.DeepEqual(unique, tc.unique) {
				t.Fatalf("unique=%v accepted=%t", unique, accepted)
			}
			ordered, accepted := appendOrderedStringsAccepted([]string{"fixture"}, tc.extra...)
			if accepted != tc.accepted || !reflect.DeepEqual(ordered, tc.ordered) {
				t.Fatalf("ordered=%v accepted=%t", ordered, accepted)
			}
		})
	}
	if values, accepted := appendUniqueStringsAccepted([]string{" padded "}); accepted || !reflect.DeepEqual(values, []string{"padded"}) {
		t.Fatal("normalizing inherited elements manufactured input")
	}
}

func TestConfigInputGenericFinalBoundaries(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("broker-origins/%t", trusted), func(t *testing.T) {
			cfg := baseConfig()
			file := fileConfig{Broker: &fileBrokerConfig{LoginRedirectOrigins: []string{" "}}}
			origin := providerSelectionRepoConfig
			if trusted {
				origin = providerSelectionUserConfig
			}
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, trusted, origin); err != nil {
				t.Fatal(err)
			}
			if got := cfg.inputProvenance.summary(configInputGeneric); (got.effects == configInputValue) != trusted {
				t.Fatalf("trusted=%t summary=%#v", trusted, got)
			}
		})
	}
	t.Run("dynamic-environment", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("CRABBOX_SSH_FALLBACK_PORTS", "")
		if err := os.Unsetenv("CRABBOX_SSH_FALLBACK_PORTS"); err != nil {
			t.Fatal(err)
		}
		const name = "CRABBOX_TEST_GENERIC_DYNAMIC_VALUE"
		for _, value := range []string{"", "fixture"} {
			t.Setenv(name, value)
			cfg := baseConfig()
			cfg.Tailscale.AuthKeyEnv = name
			cfg.Tailscale.AuthKey = "prior"
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.inputProvenance.summary(configInputGeneric); (got.effects == configInputValue) != (value != "") || cfg.Tailscale.AuthKey != value {
				t.Fatal("dynamic fallback and acceptance diverged")
			}
		}
	})
}

func TestConfigInputProvenanceGeneratedOwners(t *testing.T) {
	// One ordinary scalar binding per generated owner proves attribution, not
	// completeness of any provider's configuration or readiness.
	for _, tc := range []struct {
		owner, root, key, env, value string
		ignoreEmpty, repo            bool
	}{
		{"aws-lambda-microvm", "awsLambdaMicroVM", "workdir", "CRABBOX_AWS_LAMBDA_MICROVM_WORKDIR", "/workspace/crabbox", true, true},
		{"agent-sandbox", "agentSandbox", "workdir", "CRABBOX_AGENT_SANDBOX_WORKDIR", "/workspace/crabbox", true, false},
		{"anthropic-sandbox-runtime", "anthropicSandboxRuntime", "cliPath", "CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI", "srt", true, true},
		{"azure-dynamic-sessions", "azureDynamicSessions", "workdir", "CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR", "/workspace/crabbox", true, true},
		{"blaxel", "blaxel", "region", "CRABBOX_BLAXEL_REGION", "fixture-region", true, true},
		{"cloud-run-sandbox", "cloudRunSandbox", "workdir", "CRABBOX_CLOUD_RUN_SANDBOX_WORKDIR", "/tmp/crabbox", true, true},
		{"cloudflare", "cloudflare", "workdir", "CRABBOX_CLOUDFLARE_WORKDIR", "/workspace/crabbox", true, true},
		{"cloudflare-sandbox", "cloudflareSandbox", "workdir", "CRABBOX_CLOUDFLARE_SANDBOX_WORKDIR", "/workspace/crabbox", false, true},
		{"codesandbox", "codeSandbox", "workdir", "CRABBOX_CODESANDBOX_WORKDIR", "/project/workspace", false, true},
		{"coder", "coder", "cliPath", "CRABBOX_CODER_CLI", "coder", true, true},
		{"cua", "cua", "region", "CRABBOX_CUA_REGION", "fixture-region", false, true},
		{"digitalocean", "digitalocean", "region", "CRABBOX_DIGITALOCEAN_REGION", "fixture-region", true, true},
		{"e2b", "e2b", "workdir", "CRABBOX_E2B_WORKDIR", "crabbox", true, true},
		{"exe-dev", "exeDev", "image", "CRABBOX_EXE_DEV_IMAGE", "fixture-image", true, true},
		{"fastapi-cloud", "fastapiCloud", "appId", "CRABBOX_FASTAPI_CLOUD_APP_ID", "fixture-app", true, true},
		{"kubevirt", "kubevirt", "namespace", "CRABBOX_KUBEVIRT_NAMESPACE", "default", true, true},
		{"linode", "linode", "region", "CRABBOX_LINODE_REGION", "us-ord", true, true},
		{"lume", "lume", "cliPath", "CRABBOX_LUME_CLI", "lume", true, false},
		{"machine0", "machine0", "region", "CRABBOX_MACHINE0_REGION", "eu", true, true},
		{"modal", "modal", "workdir", "CRABBOX_MODAL_WORKDIR", "/workspace/crabbox", true, true},
		{"morph", "morph", "snapshot", "CRABBOX_MORPH_SNAPSHOT", "fixture-snapshot", true, true},
		{"multipass", "multipass", "cliPath", "CRABBOX_MULTIPASS_CLI", "multipass", true, true},
		{"namespace-devbox", "namespace", "image", "CRABBOX_NAMESPACE_IMAGE", "builtin:base", true, true},
		{"namespace-instance", "namespaceInstance", "region", "CRABBOX_NAMESPACE_INSTANCE_REGION", "fixture-region", true, false},
		{"ovh", "ovh", "region", "CRABBOX_OVH_REGION", "fixture-region", true, true},
		{"opencomputer", "openComputer", "workdir", "CRABBOX_OPENCOMPUTER_WORKDIR", "/workspace/crabbox", true, true},
		{"opensandbox", "openSandbox", "workdir", "CRABBOX_OPENSANDBOX_WORKDIR", "/workspace/crabbox", false, true},
		{"orgo", "orgo", "workspaceID", "CRABBOX_ORGO_WORKSPACE_ID", "fixture-workspace", true, true},
		{"railway", "railway", "projectId", "CRABBOX_RAILWAY_PROJECT_ID", "fixture-project", true, true},
		{"runpod", "runpod", "image", "CRABBOX_RUNPOD_IMAGE", "runpod/pytorch:2.8.0-py3.11-cuda12.8.1-cudnn-devel-ubuntu22.04", true, true},
		{"scaleway", "scaleway", "region", "CRABBOX_SCALEWAY_REGION", "fr-par", true, true},
		{"sealos-devbox", "sealosDevbox", "image", "CRABBOX_SEALOS_DEVBOX_IMAGE", "fixture-image", true, false},
		{"semaphore", "semaphore", "project", "CRABBOX_SEMAPHORE_PROJECT", "fixture-project", true, true},
		{"smolvm", "smolvm", "workdir", "CRABBOX_SMOLVM_WORKDIR", "/workspace", true, true},
		{"tencentcloud", "tencentcloud", "region", "CRABBOX_TENCENTCLOUD_REGION", "fixture-region", true, true},
		{"tensorlake", "tensorlake", "workdir", "CRABBOX_TENSORLAKE_WORKDIR", "/workspace/crabbox", true, true},
		{"upstash-box", "upstashBox", "workdir", "CRABBOX_UPSTASH_BOX_WORKDIR", "/workspace/home/crabbox", true, true},
		{"vast", "vast", "image", "CRABBOX_VAST_IMAGE", "nvidia/cuda:12.8.1-cudnn-devel-ubuntu22.04", true, true},
		{"vercel-sandbox", "vercelSandbox", "workdir", "CRABBOX_VERCEL_SANDBOX_WORKDIR", "/vercel/sandbox/crabbox", false, true},
		{"vultr", "vultr", "region", "CRABBOX_VULTR_REGION", "fixture-region", true, true},
		{"wandb", "wandb", "defaultImage", "CRABBOX_WANDB_DEFAULT_IMAGE", "fixture-image", true, true},
	} {
		for _, source := range []string{"user_config", "repo_config", "environment"} {
			for _, mode := range []string{"missing", "empty", "accepted"} {
				t.Run(tc.owner+"/"+source+"/"+mode, func(t *testing.T) {
					clearConfigEnv(t)
					cfg := baseConfig()
					value := tc.value
					if mode != "accepted" {
						value = ""
					}
					accepted := mode == "accepted"
					if source == "environment" {
						t.Setenv(tc.env, value)
						if mode == "missing" {
							if err := os.Unsetenv(tc.env); err != nil {
								t.Fatal(err)
							}
						}
						if err := applyEnv(&cfg); err != nil {
							t.Fatal(err)
						}
					} else {
						fields := map[string]any{}
						if mode != "missing" {
							fields[tc.key] = value
						}
						data, err := yaml.Marshal(map[string]any{tc.root: fields})
						if err != nil {
							t.Fatal(err)
						}
						var file fileConfig
						if err := yaml.Unmarshal(data, &file); err != nil {
							t.Fatal(err)
						}
						trusted, providerSource := source == "user_config", providerSelectionRepoConfig
						if trusted {
							providerSource = providerSelectionUserConfig
						}
						accepted = (mode == "accepted" || (mode == "empty" && !tc.ignoreEmpty)) && (trusted || tc.repo)
						if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, trusted, providerSource); err != nil {
							t.Fatal(err)
						}
					}
					got := cfg.inputProvenance.summary(configInputOwner(tc.owner))
					wantState := "unknown"
					wantSources := []string{}
					wantEffects := configInputEffect(0)
					wantOwners := 0
					if accepted {
						wantState = "present"
						wantSources = []string{source}
						wantEffects = configInputValue
						wantOwners = 1
					}
					if got.state != wantState || !reflect.DeepEqual(got.sources, wantSources) || got.effects != wantEffects || got.complete || len(cfg.inputProvenance) != wantOwners {
						t.Fatalf("summary=%#v ledger=%#v accepted=%t", got, cfg.inputProvenance, accepted)
					}
					if generic := cfg.inputProvenance.summary(configInputGeneric); generic.state != "unknown" || generic.complete {
						t.Fatalf("provider scalar attributed to generic: %#v", generic)
					}
				})
			}
		}
	}
}

func clearManualBatchAConfigEnv(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	// The common fixture sets empty strings; these actual presence-based inputs
	// need absence when testing a different field's acceptance.
	for _, name := range []string{"CRABBOX_BOXD_API_URL", "CRABBOX_BOXD_ORG", "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS", "CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "CRABBOX_DOCKER_SANDBOX_MCP", "CRABBOX_DOCKER_SANDBOX_KIT"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigInputProvenanceManualBatchA(t *testing.T) {
	for _, tc := range []struct {
		owner, root, key, env, value      string
		emptyFile, emptyEnv, repo, intent bool
	}{
		{"ascii-box", "asciiBox", "workdir", "CRABBOX_ASCII_BOX_WORKDIR", "/work/fixture", false, false, true, false},
		{"blacksmith-testbox", "blacksmith", "org", "CRABBOX_BLACKSMITH_ORG", "fixture", false, false, true, false},
		{"boxd", "boxd", "workRoot", "CRABBOX_BOXD_WORK_ROOT", "/work/fixture", false, false, true, true},
		{"cloudflare-dynamic-workers", "cloudflareDynamicWorkers", "compatibilityDate", "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_DATE", "2026-06-12", false, false, true, false},
		{"crownest", "crownest", "projectId", "CRABBOX_CROWNEST_PROJECT_ID", "fixture", true, false, true, false},
		{"cubesandbox", "cubeSandbox", "template", "CRABBOX_CUBESANDBOX_TEMPLATE", "fixture", false, false, true, false},
		{"daytona", "daytona", "snapshot", "CRABBOX_DAYTONA_SNAPSHOT", "fixture", false, false, true, false},
		{"docker-sandbox", "dockerSandbox", "template", "CRABBOX_DOCKER_SANDBOX_TEMPLATE", "fixture", true, false, true, false},
		{"external", "external", "workRoot", "CRABBOX_EXTERNAL_WORK_ROOT", "/work/fixture", false, false, true, false},
		{"firecracker", "firecracker", "user", "CRABBOX_FIRECRACKER_USER", "fixture", false, false, true, false},
	} {
		for _, source := range []string{"user_config", "repo_config", "environment"} {
			for _, mode := range []string{"missing", "empty", "equal"} {
				t.Run(tc.owner+"/"+source+"/"+mode, func(t *testing.T) {
					clearManualBatchAConfigEnv(t)
					cfg := baseConfig()
					value := ""
					if mode == "equal" {
						value = tc.value
					}
					accepted := mode == "equal"
					var apply func() error
					if source == "environment" {
						t.Setenv(tc.env, value)
						if mode == "missing" {
							if err := os.Unsetenv(tc.env); err != nil {
								t.Fatal(err)
							}
						}
						accepted = accepted || (mode == "empty" && tc.emptyEnv)
						apply = func() error { return applyEnv(&cfg) }
					} else {
						fields := map[string]any{}
						if mode != "missing" {
							fields[tc.key] = value
						}
						data, err := yaml.Marshal(map[string]any{tc.root: fields})
						if err != nil {
							t.Fatal(err)
						}
						var file fileConfig
						if err := yaml.Unmarshal(data, &file); err != nil {
							t.Fatal(err)
						}
						trusted, origin := source == "user_config", providerSelectionRepoConfig
						if trusted {
							origin = providerSelectionUserConfig
						}
						accepted = (accepted || (mode == "empty" && tc.emptyFile)) && (trusted || tc.repo)
						apply = func() error { return applyFileConfigWithTrustAndProviderSource(&cfg, file, trusted, origin) }
					}
					// Repeat against the same values with a fresh ledger: equality is still acceptance.
					for repeat := 0; repeat < 2; repeat++ {
						cfg.inputProvenance = nil
						if err := apply(); err != nil {
							t.Fatal(err)
						}
						got := cfg.inputProvenance.summary(configInputOwner(tc.owner))
						want, effects := []string{}, configInputEffect(0)
						if accepted {
							want = []string{source}
							effects = configInputValue
							if tc.intent {
								effects |= configInputIntent
							}
						}
						if !reflect.DeepEqual(got.sources, want) || got.effects != effects || got.complete {
							t.Fatalf("summary=%#v accepted=%t", got, accepted)
						}
					}
				})
			}
		}
	}
}

func TestConfigInputProvenanceManualBatchAParsers(t *testing.T) {
	for _, tc := range []struct{ owner, env, invalid, accepted string }{
		{"firecracker", "CRABBOX_FIRECRACKER_CPUS", "invalid", "0"},
		{"daytona", "CRABBOX_DAYTONA_SSH_ACCESS_MINUTES", "invalid", "0"},
		{"cloudflare-dynamic-workers", "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_CPU_MS", "invalid", "0"},
		{"blacksmith-testbox", "CRABBOX_BLACKSMITH_IDLE_TIMEOUT", "invalid", "1s"},
		{"firecracker", "CRABBOX_FIRECRACKER_LAUNCH_TIMEOUT", "invalid", "1s"},
	} {
		t.Run(tc.owner+"/"+tc.env, func(t *testing.T) {
			clearManualBatchAConfigEnv(t)
			for _, raw := range []string{tc.invalid, tc.accepted} {
				cfg := baseConfig()
				t.Setenv(tc.env, raw)
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				got := cfg.inputProvenance.summary(configInputOwner(tc.owner))
				if (got.effects&configInputValue != 0) != (raw == tc.accepted) || got.complete {
					t.Fatalf("raw %q: %#v", raw, got)
				}
			}
		})
	}
	for _, source := range []providerSelectionSource{providerSelectionUserConfig, providerSelectionRepoConfig} {
		t.Run(string(source)+"/partial", func(t *testing.T) {
			cfg := baseConfig()
			project, invalid := "fixture", -1
			file := fileConfig{Crownest: &fileCrownestConfig{ProjectID: &project, TimeoutSecs: &invalid}}
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, source == providerSelectionUserConfig, source); err == nil {
				t.Fatal("expected existing negative timeout error")
			}
			if got := cfg.inputProvenance.summary("crownest"); got.effects != configInputValue || got.complete {
				t.Fatalf("earlier accepted input lost: %#v", got)
			}
		})
	}
}

func TestConfigInputProvenanceManualBatchAEdges(t *testing.T) {
	for _, tc := range []struct{ name, owner, env, value string }{
		{"boxd-clear", "boxd", "CRABBOX_BOXD_API_URL", ""},
		{"docker-list-clear", "docker-sandbox", "CRABBOX_DOCKER_SANDBOX_MCP", ""},
		{"workers-list-clear", "cloudflare-dynamic-workers", "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_COMPATIBILITY_FLAGS", "none"},
		{"crownest-false", "crownest", "CRABBOX_CROWNEST_FORGET_MISSING", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearManualBatchAConfigEnv(t)
			t.Setenv(tc.env, tc.value)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.inputProvenance.summary(configInputOwner(tc.owner)); got.effects != configInputValue || !reflect.DeepEqual(got.sources, []string{"environment"}) || got.complete {
				t.Fatalf("accepted clear: %#v", got)
			}
		})
	}
	t.Run("repository-cap", func(t *testing.T) {
		for _, limit := range []int{-1, 0, 17} {
			cfg := baseConfig()
			cfg.CloudflareDynamicWorkers.repositoryCPUMsCap = 2
			file := fileConfig{CloudflareDynamicWorkers: &fileCloudflareDynamicWorkersConfig{CPUMs: limit}}
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, false, providerSelectionRepoConfig); err != nil {
				t.Fatal(err)
			}
			got := cfg.inputProvenance.summary("cloudflare-dynamic-workers")
			if (got.effects == configInputValue) != (limit > 0) || cfg.CloudflareDynamicWorkers.repositoryCPUMsCap != 2 {
				t.Fatalf("cap acceptance %d: %#v", limit, got)
			}
		}
	})
	t.Run("untrusted-path-ignored", func(t *testing.T) {
		cfg := baseConfig()
		file := fileConfig{Firecracker: &fileFirecrackerConfig{Binary: "fixture-binary"}}
		if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, false, providerSelectionRepoConfig); err != nil {
			t.Fatal(err)
		}
		if got := cfg.inputProvenance.summary("firecracker"); got.effects != 0 {
			t.Fatalf("ignored input recorded: %#v", got)
		}
	})
	t.Run("external-empty-capabilities-assignment", func(t *testing.T) {
		cfg := baseConfig()
		file := fileConfig{External: &fileExternalConfig{Capabilities: &ExternalCapabilitiesConfig{}}}
		if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
			t.Fatal(err)
		}
		if got := cfg.inputProvenance.summary("external"); got.effects != configInputValue || got.complete {
			t.Fatalf("accepted nested assignment: %#v", got)
		}
	})
}

func TestConfigInputProvenancePartialGeneratedError(t *testing.T) {
	for _, source := range []string{"user_config", "repo_config", "environment"} {
		t.Run(source, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			var err error
			if source == "environment" {
				t.Setenv("CRABBOX_CODESANDBOX_TEMPLATE_ID", "fixture-template")
				t.Setenv("CRABBOX_CODESANDBOX_HIBERNATION_TIMEOUT_SECS", "-1")
				err = applyEnv(&cfg)
			} else {
				var file fileConfig
				if decodeErr := yaml.Unmarshal([]byte("codeSandbox: {templateId: fixture-template, hibernationTimeoutSecs: -1}"), &file); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				trusted, ps := source == "user_config", providerSelectionRepoConfig
				if trusted {
					ps = providerSelectionUserConfig
				}
				err = applyFileConfigWithTrustAndProviderSource(&cfg, file, trusted, ps)
			}
			if err == nil || !strings.Contains(err.Error(), "must be non-negative") {
				t.Fatalf("ordinary parse error=%v", err)
			}
			got := cfg.inputProvenance.summary("codesandbox")
			if cfg.CodeSandbox.TemplateID != "fixture-template" || got.state != "present" || !reflect.DeepEqual(got.sources, []string{source}) || got.effects != configInputValue || got.complete || len(cfg.inputProvenance) != 1 {
				t.Fatalf("partial input lost: cfg=%#v summary=%#v", cfg.CodeSandbox, got)
			}
		})
	}
}

func TestManualBatchCFileInputLedger(t *testing.T) {
	for _, tc := range []struct {
		owner, root, key string
		value            any
	}{
		{"parallels", "parallels", "template", "fixture"},
		{"parallels", "parallels", "source", "fixture"},
		{"parallels", "parallels", "sourceId", "fixture"},
		{"parallels", "parallels", "sourceSnapshot", "fixture"},
		{"parallels", "parallels", "sourceSnapshotId", "fixture"},
		{"parallels", "parallels", "cloneMode", "fixture"},
		{"parallels", "parallels", "host", "fixture"},
		{"parallels", "parallels", "hostUser", "fixture"},
		{"parallels", "parallels", "hostKey", "fixture"},
		{"parallels", "parallels", "vmRoot", "fixture"},
		{"parallels", "parallels", "user", "fixture"},
		{"parallels", "parallels", "workRoot", "fixture"},
		{"parallels", "parallels", "startupTimeout", "1s"},
		{"parallels", "parallels", "templates", map[string]any{"fixture": map[string]any{"source": "fixture"}}},
		{"parallels", "parallels", "hosts", []any{map[string]any{"host": "fixture.example.test"}}},
		{"phala", "phala", "cli", "fixture"},
		{"phala", "phala", "instanceType", "fixture"},
		{"phala", "phala", "workRoot", "fixture"},
		{"phala", "phala", "nodeId", "fixture"},
		{"phala", "phala", "compose", "fixture"},
		{"phala", "phala", "attest", true},
		{"proxmox", "proxmox", "apiUrl", "https://fixture.example.test"},
		{"proxmox", "proxmox", "tokenId", "fixture"},
		{"proxmox", "proxmox", "tokenSecret", "fixture"},
		{"proxmox", "proxmox", "node", "fixture"},
		{"proxmox", "proxmox", "templateId", 1},
		{"proxmox", "proxmox", "storage", "fixture"},
		{"proxmox", "proxmox", "pool", "fixture"},
		{"proxmox", "proxmox", "bridge", "fixture"},
		{"proxmox", "proxmox", "user", "fixture"},
		{"proxmox", "proxmox", "workRoot", "fixture"},
		{"proxmox", "proxmox", "fullClone", true},
		{"proxmox", "proxmox", "insecureTLS", true},
		{"sprites", "sprites", "apiUrl", "https://fixture.example.test"},
		{"sprites", "sprites", "workRoot", "fixture"},
		{"ssh", "static", "id", "fixture"},
		{"ssh", "static", "name", "fixture"},
		{"ssh", "static", "host", "fixture"},
		{"ssh", "static", "user", "fixture"},
		{"ssh", "static", "port", "fixture"},
		{"ssh", "static", "workRoot", "fixture"},
		{"superserve", "superserve", "baseUrl", "https://fixture.example.test"},
		{"superserve", "superserve", "template", "fixture"},
		{"superserve", "superserve", "snapshot", "fixture"},
		{"superserve", "superserve", "workdir", "fixture"},
		{"superserve", "superserve", "timeoutSecs", 1},
		{"superserve", "superserve", "execTimeoutSecs", 1},
		{"superserve", "superserve", "networkAllowOut", []string{"fixture", "fixture"}},
		{"superserve", "superserve", "networkDenyOut", []string{"fixture", "fixture"}},
		{"superserve", "superserve", "forgetMissing", true},
		{"tenki", "tenki", "cliPath", "fixture"},
		{"tenki", "tenki", "endpoint", "https://fixture.example.test"},
		{"tenki", "tenki", "gateway", "fixture"},
		{"tenki", "tenki", "workspace", "fixture"},
		{"tenki", "tenki", "project", "fixture"},
		{"tenki", "tenki", "image", "fixture"},
		{"tenki", "tenki", "snapshot", "fixture"},
		{"tenki", "tenki", "workRoot", "fixture"},
		{"tenki", "tenki", "cpus", 1},
		{"tenki", "tenki", "memoryMB", 1},
		{"tenki", "tenki", "diskGB", 1},
		{"unikraft-cloud", "unikraftCloud", "apiKey", "fixture"},
		{"unikraft-cloud", "unikraftCloud", "apiUrl", "https://fixture.example.test"},
		{"unikraft-cloud", "unikraftCloud", "metro", "fixture"},
		{"unikraft-cloud", "unikraftCloud", "image", "fixture"},
		{"unikraft-cloud", "unikraftCloud", "memoryMB", 1},
		{"windows-sandbox", "windowsSandbox", "workdir", "fixture"},
		{"windows-sandbox", "windowsSandbox", "tempRoot", "fixture"},
		{"windows-sandbox", "windowsSandbox", "networking", "fixture"},
		{"windows-sandbox", "windowsSandbox", "vgpu", "fixture"},
		{"windows-sandbox", "windowsSandbox", "clipboard", "fixture"},
		{"windows-sandbox", "windowsSandbox", "protectedClient", "fixture"},
		{"windows-sandbox", "windowsSandbox", "audioInput", "fixture"},
		{"windows-sandbox", "windowsSandbox", "videoInput", "fixture"},
		{"windows-sandbox", "windowsSandbox", "printerRedirection", "fixture"},
		{"windows-sandbox", "windowsSandbox", "memoryMB", 1},
		{"xcp-ng", "xcpNg", "apiUrl", "https://fixture.example.test"},
		{"xcp-ng", "xcpNg", "username", "fixture"},
		{"xcp-ng", "xcpNg", "password", "fixture"},
		{"xcp-ng", "xcpNg", "template", "fixture"},
		{"xcp-ng", "xcpNg", "templateUuid", "fixture"},
		{"xcp-ng", "xcpNg", "sr", "fixture"},
		{"xcp-ng", "xcpNg", "srUuid", "fixture"},
		{"xcp-ng", "xcpNg", "network", "fixture"},
		{"xcp-ng", "xcpNg", "networkUuid", "fixture"},
		{"xcp-ng", "xcpNg", "host", "fixture"},
		{"xcp-ng", "xcpNg", "user", "fixture"},
		{"xcp-ng", "xcpNg", "workRoot", "fixture"},
		{"xcp-ng", "xcpNg", "insecureTLS", true},
	} {
		t.Run(tc.owner+"/"+tc.key, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			data, err := yaml.Marshal(map[string]any{tc.root: map[string]any{tc.key: tc.value}})
			if err != nil {
				t.Fatal(err)
			}
			var file fileConfig
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
				t.Fatalf("file overlay: %v", err)
			}
			// A repeated accepted layer still contributes when values already match.
			cfg.inputProvenance = nil
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
				t.Fatalf("repeat file overlay: %v", err)
			}
			got := cfg.inputProvenance.summary(configInputOwner(tc.owner))
			if got.state != "present" || !reflect.DeepEqual(got.sources, []string{"user_config"}) || got.effects != configInputValue || got.complete || len(cfg.inputProvenance) != 1 {
				t.Fatalf("owner=%s summary=%#v", tc.owner, got)
			}
		})
	}
}

func TestManualBatchCEnvInputLedger(t *testing.T) {
	for _, tc := range []struct{ owner, key, value string }{
		{"parallels", "CRABBOX_PARALLELS_CLONE_MODE", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_HOST", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_HOST_KEY", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_HOST_USER", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_SOURCE", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_SOURCE_ID", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_SOURCE_SNAPSHOT", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_SOURCE_SNAPSHOT_ID", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_STARTUP_TIMEOUT", "1s"},
		{"parallels", "CRABBOX_PARALLELS_TEMPLATE", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_USER", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_VM_ROOT", "fixture"},
		{"parallels", "CRABBOX_PARALLELS_WORK_ROOT", "fixture"},
		{"phala", "CRABBOX_PHALA_ATTEST", "true"},
		{"phala", "CRABBOX_PHALA_CLI", "fixture"},
		{"phala", "CRABBOX_PHALA_COMPOSE", "fixture"},
		{"phala", "CRABBOX_PHALA_INSTANCE_TYPE", "fixture"},
		{"phala", "CRABBOX_PHALA_NODE_ID", "fixture"},
		{"phala", "CRABBOX_PHALA_WORK_ROOT", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_API_URL", "https://fixture.example.test"},
		{"proxmox", "CRABBOX_PROXMOX_BRIDGE", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_FULL_CLONE", "true"},
		{"proxmox", "CRABBOX_PROXMOX_INSECURE_TLS", "true"},
		{"proxmox", "CRABBOX_PROXMOX_NODE", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_POOL", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_STORAGE", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_TEMPLATE_ID", "1"},
		{"proxmox", "CRABBOX_PROXMOX_TOKEN_ID", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_TOKEN_SECRET", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_USER", "fixture"},
		{"proxmox", "CRABBOX_PROXMOX_WORK_ROOT", "fixture"},
		{"sprites", "CRABBOX_SPRITES_API_URL", "https://fixture.example.test"},
		{"sprites", "CRABBOX_SPRITES_TOKEN", "fixture"},
		{"sprites", "CRABBOX_SPRITES_WORK_ROOT", "fixture"},
		{"ssh", "CRABBOX_STATIC_HOST", "fixture"},
		{"ssh", "CRABBOX_STATIC_ID", "fixture"},
		{"ssh", "CRABBOX_STATIC_NAME", "fixture"},
		{"ssh", "CRABBOX_STATIC_PORT", "fixture"},
		{"ssh", "CRABBOX_STATIC_USER", "fixture"},
		{"ssh", "CRABBOX_STATIC_WORK_ROOT", "fixture"},
		{"superserve", "CRABBOX_SUPERSERVE_BASE_URL", "https://fixture.example.test"},
		{"superserve", "CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS", "1"},
		{"superserve", "CRABBOX_SUPERSERVE_FORGET_MISSING", "true"},
		{"superserve", "CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT", "fixture"},
		{"superserve", "CRABBOX_SUPERSERVE_NETWORK_DENY_OUT", "fixture"},
		{"superserve", "CRABBOX_SUPERSERVE_SNAPSHOT", "fixture"},
		{"superserve", "CRABBOX_SUPERSERVE_TEMPLATE", "fixture"},
		{"superserve", "CRABBOX_SUPERSERVE_TIMEOUT_SECS", "1"},
		{"superserve", "CRABBOX_SUPERSERVE_WORKDIR", "fixture"},
		{"tenki", "CRABBOX_TENKI_CLI", "fixture"},
		{"tenki", "CRABBOX_TENKI_CPUS", "1"},
		{"tenki", "CRABBOX_TENKI_DISK_GB", "1"},
		{"tenki", "CRABBOX_TENKI_ENDPOINT", "https://fixture.example.test"},
		{"tenki", "CRABBOX_TENKI_GATEWAY", "fixture"},
		{"tenki", "CRABBOX_TENKI_IMAGE", "fixture"},
		{"tenki", "CRABBOX_TENKI_MEMORY_MB", "1"},
		{"tenki", "CRABBOX_TENKI_PROJECT", "fixture"},
		{"tenki", "CRABBOX_TENKI_SNAPSHOT", "fixture"},
		{"tenki", "CRABBOX_TENKI_WORKSPACE", "fixture"},
		{"tenki", "CRABBOX_TENKI_WORK_ROOT", "fixture"},
		{"unikraft-cloud", "CRABBOX_UNIKRAFT_CLOUD_API_KEY", "fixture"},
		{"unikraft-cloud", "CRABBOX_UNIKRAFT_CLOUD_API_URL", "https://fixture.example.test"},
		{"unikraft-cloud", "CRABBOX_UNIKRAFT_CLOUD_IMAGE", "fixture"},
		{"unikraft-cloud", "CRABBOX_UNIKRAFT_CLOUD_METRO", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_AUDIO_INPUT", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_CLIPBOARD", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_MEMORY_MB", "1"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_NETWORKING", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_PRINTER_REDIRECTION", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_PROTECTED_CLIENT", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_TEMP_ROOT", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_VGPU", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_VIDEO_INPUT", "fixture"},
		{"windows-sandbox", "CRABBOX_WINDOWS_SANDBOX_WORKDIR", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_API_URL", "https://fixture.example.test"},
		{"xcp-ng", "CRABBOX_XCP_NG_HOST", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_INSECURE_TLS", "true"},
		{"xcp-ng", "CRABBOX_XCP_NG_NETWORK", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_NETWORK_UUID", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_PASSWORD", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_SR", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_SR_UUID", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_TEMPLATE", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_TEMPLATE_UUID", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_USER", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_USERNAME", "fixture"},
		{"xcp-ng", "CRABBOX_XCP_NG_WORK_ROOT", "fixture"},
		{"sprites", "SETUP_SPRITE_TOKEN", "fixture"},
		{"sprites", "SPRITES_API_URL", "https://fixture.example.test"},
		{"sprites", "SPRITES_TOKEN", "fixture"},
		{"sprites", "SPRITE_TOKEN", "fixture"},
		{"superserve", "SUPERSERVE_BASE_URL", "https://fixture.example.test"},
		{"tenki", "TENKI_CLI", "fixture"},
		{"tenki", "TENKI_ENDPOINT", "https://fixture.example.test"},
		{"tenki", "TENKI_GATEWAY", "fixture"},
		{"unikraft-cloud", "UKC_API_KEY", "fixture"},
		{"unikraft-cloud", "UKC_METRO", "fixture"},
		{"unikraft-cloud", "UKC_TOKEN", "fixture"},
		{"unikraft-cloud", "UNIKRAFT_CLOUD_API_KEY", "fixture"},
		{"unikraft-cloud", "UNIKRAFT_CLOUD_API_URL", "https://fixture.example.test"},
		{"unikraft-cloud", "UNIKRAFT_CLOUD_IMAGE", "fixture"},
		{"unikraft-cloud", "UNIKRAFT_CLOUD_METRO", "fixture"},
	} {
		t.Run(tc.owner+"/"+tc.key, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.value)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatalf("env overlay: %v", err)
			}
			cfg.inputProvenance = nil
			if err := applyEnv(&cfg); err != nil {
				t.Fatalf("repeat env overlay: %v", err)
			}
			got := cfg.inputProvenance.summary(configInputOwner(tc.owner))
			if got.state != "present" || !reflect.DeepEqual(got.sources, []string{"environment"}) || got.effects != configInputValue || got.complete || len(cfg.inputProvenance) != 1 {
				t.Fatalf("owner=%s summary=%#v", tc.owner, got)
			}
		})
	}
}

func TestManualBatchCIgnoredAndPartialInputs(t *testing.T) {
	clearConfigEnv(t)
	for _, tc := range []struct{ owner, root string }{{"parallels", "parallels"}, {"phala", "phala"}, {"proxmox", "proxmox"}, {"sprites", "sprites"}, {"ssh", "static"}, {"superserve", "superserve"}, {"tenki", "tenki"}, {"unikraft-cloud", "unikraftCloud"}, {"windows-sandbox", "windowsSandbox"}, {"xcp-ng", "xcpNg"}} {
		var file fileConfig
		if err := yaml.Unmarshal([]byte(tc.root+": {}"), &file); err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, true, providerSelectionUserConfig); err != nil {
			t.Fatal(err)
		}
		if got := cfg.inputProvenance.summary(configInputOwner(tc.owner)); got.state != "unknown" || got.complete {
			t.Fatalf("empty owner=%s summary=%#v", tc.owner, got)
		}
	}
	for _, key := range []string{"CRABBOX_TENKI_CPUS", "CRABBOX_PROXMOX_TEMPLATE_ID", "CRABBOX_WINDOWS_SANDBOX_MEMORY_MB", "CRABBOX_PARALLELS_STARTUP_TIMEOUT"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "invalid")
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.inputProvenance) != 0 {
				t.Fatal("invalid tolerant scalar recorded")
			}
		})
	}
	t.Run("partial superserve", func(t *testing.T) {
		t.Setenv("CRABBOX_SUPERSERVE_TEMPLATE", "fixture")
		t.Setenv("CRABBOX_SUPERSERVE_TIMEOUT_SECS", "-1")
		cfg := baseConfig()
		if err := applyEnv(&cfg); err == nil {
			t.Fatal("missing ordinary numeric error")
		}
		got := cfg.inputProvenance.summary("superserve")
		if got.state != "present" || !reflect.DeepEqual(got.sources, []string{"environment"}) || got.complete {
			t.Fatalf("partial=%#v", got)
		}
	})
}

func TestManualBatchCRepoAndZeroInputs(t *testing.T) {
	for _, tc := range []struct {
		owner, root, key string
		value            any
	}{
		{"parallels", "parallels", "source", "fixture"}, {"phala", "phala", "instanceType", "fixture"}, {"proxmox", "proxmox", "fullClone", false}, {"sprites", "sprites", "workRoot", "fixture"}, {"ssh", "static", "name", "fixture"}, {"superserve", "superserve", "timeoutSecs", 0}, {"tenki", "tenki", "project", "fixture"}, {"unikraft-cloud", "unikraftCloud", "metro", "fixture"}, {"windows-sandbox", "windowsSandbox", "workdir", "fixture"}, {"xcp-ng", "xcpNg", "template", "fixture"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			clearConfigEnv(t)
			var file fileConfig
			data, err := yaml.Marshal(map[string]any{tc.root: map[string]any{tc.key: tc.value}})
			if err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, false, providerSelectionRepoConfig); err != nil {
				t.Fatal(err)
			}
			got := cfg.inputProvenance.summary(configInputOwner(tc.owner))
			if got.state != "present" || !reflect.DeepEqual(got.sources, []string{"repo_config"}) || got.effects != configInputValue || got.complete {
				t.Fatalf("repo summary=%#v", got)
			}
		})
	}
}
