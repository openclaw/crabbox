package digitalocean

import (
	"regexp"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var digitalOceanTagNameRE = regexp.MustCompile(`^[A-Za-z0-9_:\-]+$`)

func TestLeaseTagsRoundTripOwnedLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.TTL = time.Hour
	cfg.IdleTimeout = 10 * time.Minute

	tags := leaseTags(cfg, "cbx_abcdef123456", "blue-box", "ready", true, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	labels := labelsFromTags(tags)
	if err := validateDropletLabels(labels); err != nil {
		t.Fatalf("validateDropletLabels() err = %v tags=%v labels=%v", err, tags, labels)
	}
	for key, want := range map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"provider":   providerName,
		"lease":      "cbx_abcdef123456",
		"slug":       "blue-box",
		"state":      "ready",
		"target":     core.TargetLinux,
		"keep":       "true",
	} {
		if got := labels[key]; got != want {
			t.Fatalf("labels[%q] = %q, want %q (all=%v)", key, got, want, labels)
		}
	}
}

func TestValidateDropletLabelsRejectsPartialOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{name: "nil", labels: nil},
		{name: "generic crabbox only", labels: map[string]string{"crabbox": "true"}},
		{name: "wrong provider", labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "hetzner", "lease": "cbx_1", "slug": "x", "target": core.TargetLinux}},
		{name: "missing lease", labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": providerName, "slug": "x", "target": core.TargetLinux}},
		{name: "wrong target", labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": providerName, "lease": "cbx_1", "slug": "x", "target": "windows"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateDropletLabels(tc.labels); err == nil {
				t.Fatalf("validateDropletLabels(%v) accepted partial ownership", tc.labels)
			}
		})
	}
}

func TestLabelsFromTagsIgnoresMalformedCrabboxLikeTags(t *testing.T) {
	labels := labelsFromTags([]string{
		"crabbox",
		"crabbox:provider:digitalocean",
		"crabbox:lease",
		"crabbox:slug",
		"crabbox:target:linux",
		"crabbox:state:ready",
	})
	if labels["lease"] != "" || labels["slug"] != "" {
		t.Fatalf("malformed tags decoded identity: %v", labels)
	}
	if err := validateDropletLabels(labels); err == nil {
		t.Fatalf("malformed tags unexpectedly validated: %v", labels)
	}
}

func TestLeaseTagsPreserveTailscaleMetadata(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.Hostname = "cbx-blue.example.com"
	cfg.Tailscale.Tags = []string{"tag:ci", "tag:crabbox"}
	cfg.Tailscale.ExitNode = "exit.example"
	cfg.Tailscale.ExitNodeAllowLANAccess = true

	tags := leaseTags(cfg, "cbx_abcdef123456", "blue", "ready", false, time.Now())
	for _, tag := range tags {
		if !digitalOceanTagNameRE.MatchString(tag) {
			t.Fatalf("invalid digitalocean tag %q in %v", tag, tags)
		}
	}

	labels := labelsFromTags(tags)
	for key, want := range map[string]string{
		"tailscale":                            "true",
		"tailscale_state":                      "requested",
		"tailscale_hostname":                   "cbx-blue-example-com",
		"tailscale_tags":                       "tag:ci-tag:crabbox",
		"tailscale_exit_node":                  "exit-example",
		"tailscale_exit_node_allow_lan_access": "true",
	} {
		if got := labels[key]; got != want {
			t.Fatalf("labels[%q]=%q want %q; all=%v", key, got, want, labels)
		}
	}
}

func TestLeaseTagsRoundTripCapabilityAndPondLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Profile = "daily"
	cfg.Desktop = true
	cfg.DesktopEnv = "gnome"
	cfg.Browser = true
	cfg.Code = true
	cfg.Pond = "ci pond"
	cfg.ExposedPorts = []string{"3000", "5173"}

	tags := leaseTags(cfg, "cbx_abcdef123456", "blue", "ready", true, time.Now())
	for _, tag := range tags {
		if !digitalOceanTagNameRE.MatchString(tag) {
			t.Fatalf("invalid digitalocean tag %q in %v", tag, tags)
		}
	}

	labels := labelsFromTags(tags)
	for key, want := range map[string]string{
		"profile":               "daily",
		"desktop":               "true",
		"desktop_env":           "gnome",
		"browser":               "true",
		"code":                  "true",
		"pond":                  "ci-pond",
		"crabbox_exposed_ports": "3000-5173",
	} {
		if got := labels[key]; got != want {
			t.Fatalf("labels[%q]=%q want %q; all=%v tags=%v", key, got, want, labels, tags)
		}
	}
}
