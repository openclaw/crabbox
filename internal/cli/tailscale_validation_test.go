package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestTailscaleCapabilityCatalogValidation(t *testing.T) {
	for _, provider := range registeredProviders() {
		t.Run(provider.Spec().Name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = provider.Spec().Name
			cfg.Tailscale.Enabled = true
			err := validateRequestedCapabilities(cfg)
			supported := ProviderFeaturesForConfig(provider, cfg).Has(FeatureTailscale)
			if !supported && (err == nil || !strings.Contains(err.Error(), "--tailscale is not supported")) {
				t.Fatalf("unsupported provider passed Tailscale validation: %v", err)
			}
			if supported && err != nil {
				t.Fatalf("capable provider rejected Tailscale: %v", err)
			}
		})
	}
}

func TestTailscaleExitNodeRequiresEnrollment(t *testing.T) {
	for _, source := range []string{"flags", "environment", "config"} {
		for _, lanOnly := range []bool{false, true} {
			t.Run(source+"/"+map[bool]string{false: "exit-node", true: "lan-only"}[lanOnly], func(t *testing.T) {
				clearConfigEnv(t)
				cfg := baseConfig()
				args := []string{}
				switch source {
				case "flags":
					if lanOnly {
						args = []string{"--tailscale-exit-node-allow-lan-access"}
					} else {
						args = []string{"--tailscale-exit-node=100.100.100.100"}
					}
				case "environment":
					if lanOnly {
						t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS", "1")
					} else {
						t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE", "100.100.100.100")
					}
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				case "config":
					cfg.Tailscale.ExitNodeAllowLANAccess = lanOnly
					if !lanOnly {
						cfg.Tailscale.ExitNode = "100.100.100.100"
					}
				}
				fs := newFlagSet("test", io.Discard)
				values := registerNetworkFlags(fs, cfg)
				if err := fs.Parse(args); err != nil {
					t.Fatal(err)
				}
				err := applyNetworkFlagOverrides(&cfg, fs, values)
				want := "requires tailscale.enabled"
				if lanOnly {
					want = "requires tailscale.exitNode"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error=%v, want %s", err, want)
				}
				// An explicit enable flag can resolve exit-node settings from lower-precedence sources.
				if !lanOnly {
					if err := fs.Parse([]string{"--tailscale"}); err != nil {
						t.Fatal(err)
					}
					if err := applyNetworkFlagOverrides(&cfg, fs, values); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestTailscaleRealTagGrammar(t *testing.T) {
	for _, tag := range []string{"tag:ci", "tag:CI-1", "tag:a-", "tag:" + strings.Repeat("a", 100)} {
		if !validTailscaleTag(tag) {
			t.Errorf("valid tag rejected: %q", tag)
		}
	}
	for _, tag := range []string{"", "tag:", "ci", "tag:1foo", "tag:-foo", "tag:foo_bar", "tag:bad!", "tag:é", "tag:a\n"} {
		cfg := baseConfig()
		cfg.Tailscale.Enabled = true
		cfg.Tailscale.Tags = []string{"tag:ci", tag}
		if err := validateNetworkConfig(cfg); err == nil {
			t.Errorf("invalid tag accepted: %q", tag)
		}
	}
}

func TestTailscaleDirectTagRoundTrip(t *testing.T) {
	for _, tags := range [][]string{{"tag:crabbox"}, {"tag:ci", "tag:staging"}, {"tag:ci", "tag:" + strings.Repeat("a", 100)}} {
		cfg := baseConfig()
		cfg.Tailscale.Enabled = true
		cfg.Tailscale.Tags = tags
		labels := DirectLeaseLabels(cfg, "cbx_abcdef123456", "blue", "gcp", "", false, time.Now())
		encoded, err := json.Marshal(Server{Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		var hetzner Server
		if err := json.Unmarshal(encoded, &hetzner); err != nil {
			t.Fatal(err)
		}
		aws := awsInstanceToServer(ec2types.Instance{Tags: awsTags(labels), State: &ec2types.InstanceState{}})
		azure := azureVMToServer(armcompute.VirtualMachine{Tags: azureLabelsToTags(labels)}, "", "")
		gcp := gcpInstanceToServer("us-central1-a", &computepb.Instance{Labels: gcpLabels(labels)})
		for _, stored := range []map[string]string{hetzner.Labels, aws.Labels, azure.Labels, gcp.Labels} {
			got := serverTailscaleMetadata(Server{Labels: stored})
			if !reflect.DeepEqual(got.Tags, tags) {
				t.Errorf("tags=%q, want %q (labels=%v)", got.Tags, tags, stored)
			}
		}
	}
}

func TestTailscaleExitNodeLoadedFlagPrecedence(t *testing.T) {
	for _, source := range []string{"environment", "file"} {
		t.Run(source, func(t *testing.T) {
			clearConfigEnv(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			config := "target: linux\nprovider: hetzner\n"
			if source == "file" {
				config += "tailscale:\n  exitNode: 100.100.100.100\n"
			} else {
				t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE", "100.100.100.100")
			}
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", path)
			for _, args := range [][]string{{"--tailscale"}, {"--tailscale-exit-node="}} {
				fs := newFlagSet("test", io.Discard)
				registerNetworkFlags(fs, baseConfig())
				if err := fs.Parse(args); err != nil {
					t.Fatal(err)
				}
				if _, err := loadConfigForFlags(fs); err != nil {
					t.Fatalf("%v failed to override %s: %v", args, source, err)
				}
			}
		})
	}
}

func TestTailscaleTagMetadataRejectsPartialAndLegacyCorruption(t *testing.T) {
	cfg := baseConfig()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.Tags = []string{"tag:ci", "tag:" + strings.Repeat("a", 100)}
	labels := DirectLeaseLabels(cfg, "cbx_abcdef123456", "blue", "gcp", "", false, time.Now())
	delete(labels, "tailscale_tags_1")
	for _, damaged := range []map[string]string{labels, {"tailscale_tags": "tag_ci_tag_staging"}} {
		meta := serverTailscaleMetadata(Server{Labels: damaged})
		if len(meta.Tags) != 0 || meta.Error == "" {
			t.Fatalf("damaged tags reported as valid: %+v", meta)
		}
	}
	meta := serverTailscaleMetadata(Server{Labels: map[string]string{"tailscale_tags": "tag:ci,tag:staging"}})
	if !reflect.DeepEqual(meta.Tags, []string{"tag:ci", "tag:staging"}) {
		t.Fatal(meta)
	}
}
