package cli

import (
	"encoding/base64"
	"testing"
)

func TestDirectProviderUserDataSizeBudgets(t *testing.T) {
	for _, tc := range []struct {
		provider string
		limit    int
		base64   bool
		desktop  bool
	}{
		// Azure's decoded customData limit is 64 KB; GCP metadata values are 256 KB.
		// https://learn.microsoft.com/en-us/azure/virtual-machines/custom-data
		// https://cloud.google.com/compute/docs/metadata/setting-custom-metadata
		{"azure", 64 * 1024, false, true},
		{"gcp", 256 * 1024, false, true},
		// DigitalOcean accepts 64 KiB of text; Linode caps the base64 field at 65535.
		// https://docs.digitalocean.com/products/droplets/how-to/provide-user-data/
		// https://techdocs.akamai.com/linode-api/reference/post-linode-instance
		{"digitalocean", 64 * 1024, false, false},
		{"linode", 65535, true, false},
		// Conservative regression budgets for the remaining shared bootstrap adapters.
		{"vultr", 64 * 1024, true, false},
		{"scaleway", 64 * 1024, false, false},
		{"ovh", 64 * 1024, false, false},
		{"nebius", 64 * 1024, false, false},
		{"lambda", 64 * 1024, false, false},
		{"incus", 64 * 1024, false, false},
		{"multipass", 64 * 1024, false, false},
		{"firecracker", 64 * 1024, false, false},
	} {
		desktops := []string{""}
		if tc.desktop {
			desktops = append(desktops, "xfce", "wayland", "gnome")
		}
		for _, desktop := range desktops {
			t.Run(tc.provider+"/"+blank(desktop, "headless"), func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = tc.provider
				cfg.Desktop, cfg.DesktopEnv = desktop != "", desktop
				cfg.Browser, cfg.Code = cfg.Desktop, cfg.Desktop
				cfg.Tailscale.Enabled, cfg.Tailscale.AuthKey = true, "synthetic-auth-key"
				cfg = WithGuestLeaseExpiry(cfg, map[string]string{"crabbox": "true", "state": "provisioning", "keep": "true", "expires_at": "1800000000"})
				data := cloudInit(cfg, "ssh-ed25519 fixture")
				if tc.provider == "azure" {
					data = azureLinuxCloudInit(cfg, "ssh-ed25519 fixture")
				}
				if tc.base64 {
					data = base64.StdEncoding.EncodeToString([]byte(data))
				}
				if len(data) > tc.limit {
					t.Fatalf("user-data is %d bytes; budget is %d", len(data), tc.limit)
				}
				t.Logf("user-data=%d budget=%d", len(data), tc.limit)
			})
		}
	}
}
