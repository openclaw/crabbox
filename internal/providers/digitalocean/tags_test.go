package digitalocean

import (
	"regexp"
	"strings"
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
		{name: "non-canonical lease", labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": providerName, "lease": "legacy-lease-1", "slug": "x", "target": core.TargetLinux}},
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
		"tailscale_hostname":                   "cbx-blue.example.com",
		"tailscale_tags":                       "tag:ci,tag:crabbox",
		"tailscale_exit_node":                  "exit.example",
		"tailscale_exit_node_allow_lan_access": "true",
	} {
		if got := labels[key]; got != want {
			t.Fatalf("labels[%q]=%q want %q; all=%v", key, got, want, labels)
		}
	}
}

func TestTailscaleEndpointTagsRoundTrip(t *testing.T) {
	labels := map[string]string{
		"crabbox":             "true",
		"created_by":          "crabbox",
		"provider":            providerName,
		"lease":               "cbx_abcdef123456",
		"slug":                "blue",
		"target":              core.TargetLinux,
		"tailscale_ipv4":      "100.64.1.2",
		"tailscale_fqdn":      "blue.example.ts.net",
		"tailscale_state":     "ready",
		"tailscale_error":     "Auth failed: retrying",
		"tailscale":           "true",
		"tailscale_hostname":  "blue",
		"tailscale_tags":      "tag:ci,tag:crabbox",
		"tailscale_exit_node": "exit.example.ts.net",
	}
	tags := tagsFromLabels(labels)
	for _, tag := range tags {
		if !digitalOceanTagNameRE.MatchString(tag) {
			t.Fatalf("invalid digitalocean tag %q in %v", tag, tags)
		}
	}
	decoded := labelsFromTags(tags)
	for _, key := range []string{"tailscale_ipv4", "tailscale_fqdn", "tailscale_state", "tailscale_error", "tailscale_tags", "tailscale_exit_node"} {
		if decoded[key] != labels[key] {
			t.Fatalf("decoded[%q]=%q want %q; tags=%v", key, decoded[key], labels[key], tags)
		}
	}
	if strings.EqualFold(
		encodeTagKV("tailscale_error", "Auth failed"),
		encodeTagKV("tailscale_error", "auth failed"),
	) {
		t.Fatal("exact tag encoding collapsed case-distinct values")
	}
}

func TestLabelsFromTagsAcceptsCanonicalCaseVariants(t *testing.T) {
	labels := labelsFromTags([]string{
		"Crabbox",
		"Crabbox:Provider:DigitalOcean",
		"Crabbox:Target:Linux",
		"Crabbox:Lease:cbx_abcdef123456",
		"Crabbox:Slug:blue",
	})
	if err := validateDropletLabels(labels); err != nil {
		t.Fatalf("validateDropletLabels err=%v labels=%v", err, labels)
	}
}

func TestLabelsFromTagsRejectsConflictingOwnershipValues(t *testing.T) {
	base := []string{
		"crabbox",
		"crabbox:provider:digitalocean",
		"crabbox:target:linux",
		"crabbox:lease:cbx_abcdef123456",
		"crabbox:slug:blue",
	}
	conflicts := map[string]string{
		"provider": "crabbox:provider:aws",
		"target":   "crabbox:target:windows",
		"lease":    "crabbox:lease:cbx_abcdef999999",
		"slug":     "crabbox:slug:red",
	}
	for key, conflict := range conflicts {
		t.Run(key, func(t *testing.T) {
			for _, tags := range [][]string{
				append(append([]string(nil), base...), conflict),
				append([]string{conflict}, base...),
			} {
				labels := labelsFromTags(tags)
				if labels[ownershipTagConflictLabel] != key {
					t.Fatalf("conflict=%q labels=%v tags=%v", labels[ownershipTagConflictLabel], labels, tags)
				}
				if err := validateDropletLabels(labels); err == nil {
					t.Fatalf("conflicting ownership tags validated: labels=%v tags=%v", labels, tags)
				}
			}
		})
	}

	labels := labelsFromTags(append(append([]string(nil), base...), "crabbox:lease:cbx_abcdef123456"))
	if labels[ownershipTagConflictLabel] != "" {
		t.Fatalf("equal ownership tags conflicted: %v", labels)
	}
	if err := validateDropletLabels(labels); err != nil {
		t.Fatalf("equal ownership tags rejected: %v labels=%v", err, labels)
	}
}

func TestLabelsFromTagsPreservesLegacyExactFieldUnderscores(t *testing.T) {
	legacy := "tag:ci_12"
	labels := labelsFromTags([]string{"crabbox:tailscale_tags:" + legacy})
	if labels["tailscale_tags"] != legacy {
		t.Fatalf("tailscale_tags=%q want legacy %q", labels["tailscale_tags"], legacy)
	}

	versioned := tagsFromLabels(map[string]string{"tailscale_tags": "tag:ci,tag:crabbox"})
	versioned = append(versioned, "crabbox:tailscale_tags:"+legacy)
	labels = labelsFromTags(versioned)
	if labels["tailscale_tags"] != "tag:ci,tag:crabbox" {
		t.Fatalf("versioned tailscale_tags=%q tags=%v", labels["tailscale_tags"], versioned)
	}

	labels = labelsFromTags([]string{"crabbox:tailscale_ipv4:100_2e64_2e1_2e2"})
	if labels["tailscale_ipv4"] != "100.64.1.2" {
		t.Fatalf("legacy tailscale_ipv4=%q", labels["tailscale_ipv4"])
	}
}

func TestLabelsFromTagsOmitsConflictingExactValues(t *testing.T) {
	base := []string{
		tagCrabbox,
		"crabbox:provider:digitalocean",
		"crabbox:target:linux",
		"crabbox:lease:cbx_abcdef123456",
		"crabbox:slug:blue",
		"crabbox:tailscale_fqdn:legacy.example.ts.net",
	}
	first := encodeTagKV("tailscale_fqdn", "first.example.ts.net")
	second := encodeTagKV("tailscale_fqdn", "second.example.ts.net")
	for _, tags := range [][]string{
		append(append([]string(nil), base...), first, second),
		append(append([]string(nil), base...), second, first),
	} {
		labels := labelsFromTags(tags)
		if _, ok := labels["tailscale_fqdn"]; ok {
			t.Fatalf("conflicting tailscale_fqdn decoded=%q tags=%v", labels["tailscale_fqdn"], tags)
		}
		if err := validateDropletLabels(labels); err != nil {
			t.Fatalf("ownership lost after mutable-tag conflict: %v labels=%v", err, labels)
		}
	}

	labels := labelsFromTags(append(append([]string(nil), base...), first, first))
	if labels["tailscale_fqdn"] != "first.example.ts.net" {
		t.Fatalf("duplicate equal tailscale_fqdn=%q", labels["tailscale_fqdn"])
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
