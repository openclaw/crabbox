package scaleway

import (
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLeaseTagsRoundTripOwnershipLabels(t *testing.T) {
	cfg := core.Config{
		Provider:   providerName,
		TargetOS:   core.TargetLinux,
		Class:      "standard",
		ServerType: "DEV1-S",
		Profile:    "daily",
	}
	tags := leaseTags(cfg, "cbx_999999999999", "blue-box", "ready", true, time.Unix(1700000000, 0).UTC())
	for _, want := range []string{tagCrabbox, "crabbox:provider:scaleway", "crabbox:target:linux", "crabbox:lease:cbx_999999999999", "crabbox:slug:blue-box", "crabbox:state:ready"} {
		if !containsTag(tags, want) {
			t.Fatalf("tags=%v missing %q", tags, want)
		}
	}
	labels := labelsFromTags(tags)
	if labels["provider"] != providerName || labels["lease"] != "cbx_999999999999" || labels["slug"] != "blue-box" || labels["state"] != "ready" || labels["target"] != core.TargetLinux {
		t.Fatalf("labels=%v", labels)
	}
}

func TestValidateScalewayLabelsRejectsNonCanonicalLease(t *testing.T) {
	labels := map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"provider":   providerName,
		"lease":      "legacy-lease-1",
		"slug":       "legacy",
		"target":     core.TargetLinux,
	}
	if err := validateScalewayLabels(labels); err == nil {
		t.Fatal("non-canonical lease must not be treated as owned")
	}
}

func TestExactTagsPreserveTailscaleValues(t *testing.T) {
	labels := map[string]string{
		"provider":           providerName,
		"lease":              "cbx_999999999999",
		"slug":               "blue-box",
		"target":             core.TargetLinux,
		"tailscale_hostname": "cbx-blue.example.ts.net",
		"tailscale_tags":     "tag:ci,tag:dev",
	}
	tags := tagsFromLabels(labels)
	joined := strings.Join(tags, " ")
	if strings.Contains(joined, "tag:ci,tag:dev") {
		t.Fatalf("tailscale tags were not encoded: %v", tags)
	}
	got := labelsFromTags(tags)
	if got["tailscale_hostname"] != "cbx-blue.example.ts.net" || got["tailscale_tags"] != "tag:ci,tag:dev" {
		t.Fatalf("decoded labels=%v tags=%v", got, tags)
	}
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func TestScalewayTagDecoderRetainsProviderPrecedence(t *testing.T) {
	labels := labelsFromTags([]string{
		"crabbox:state:running", "crabbox:state:provisioning",
		"crabbox:expires_at:20", "crabbox:expires_at:10",
		"crabbox:keep:TRUE", "crabbox:unknown_metadata:retained",
		"crabbox:provider:scaleway", "crabbox:provider:ScaleWay",
	})
	for key, want := range map[string]string{
		"state": "provisioning", "expires_at": "10", "keep": "TRUE",
		"unknown_metadata": "retained", ownershipTagConflictLabel: "provider",
	} {
		if labels[key] != want {
			t.Errorf("labels[%q]=%q, want %q", key, labels[key], want)
		}
	}
}

func TestScalewaySchemaPreservesExactAndRecoveryFields(t *testing.T) {
	labels := map[string]string{
		"tailscale_hostname": "box.example.ts.net", "tailscale_tags": "tag:ci,tag:dev",
		"tailscale_ipv4": "100.100.10.20", "tailscale_fqdn": "box.example.ts.net",
		"tailscale_error": "a diagnostic with spaces", "tailscale_exit_node": "100.100.10.21",
		"recovery": "pending", "scaleway_project": "project-1", "scaleway_organization": "org-1",
		"scaleway_region": "fr-par", "scaleway_zone": "fr-par-1",
		"scaleway_ssh_key_id": "key-1", "scaleway_ssh_key_name": "crabbox-key",
	}
	got := labelsFromTags(tagsFromLabels(labels))
	for key, want := range labels {
		if got[key] != want {
			t.Errorf("round-trip label[%q]=%q, want %q", key, got[key], want)
		}
	}
}
