package main

import (
	"slices"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
)

// These adapters bootstrap cloud and local VMs with the shared 2222/22 policy.
func TestCloudProvidersRetainSSHPortFallback(t *testing.T) {
	for _, name := range []string{"aws", "azure", "gcp", "hetzner", "ovh", "nebius", "xcp-ng", "incus", "proxmox"} {
		t.Run(name, func(t *testing.T) {
			cfg := cli.BaseConfig()
			cfg.Provider = name
			provider, err := cli.ProviderFor(name)
			if err != nil {
				t.Fatal(err)
			}
			if defaults, ok := provider.(cli.ProviderConfigDefaulter); ok {
				if err := defaults.ApplyConfigDefaults(&cfg); err != nil {
					t.Fatal(err)
				}
			}
			for _, host := range []string{"192.0.2.10", "127.0.0.1"} {
				target := cli.SSHTargetFromConfig(cfg, host)
				if target.Port != "2222" || !slices.Equal(target.FallbackPorts, []string{"22"}) {
					t.Fatalf("%s target port=%s fallbacks=%v", host, target.Port, target.FallbackPorts)
				}
			}
		})
	}
}
