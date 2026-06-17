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
	tags := leaseTags(cfg, "cbx_123", "blue-box", "ready", true, time.Unix(1700000000, 0).UTC())
	for _, want := range []string{tagCrabbox, "crabbox:provider:scaleway", "crabbox:target:linux", "crabbox:lease:cbx_123", "crabbox:slug:blue-box", "crabbox:state:ready"} {
		if !containsTag(tags, want) {
			t.Fatalf("tags=%v missing %q", tags, want)
		}
	}
	labels := labelsFromTags(tags)
	if labels["provider"] != providerName || labels["lease"] != "cbx_123" || labels["slug"] != "blue-box" || labels["state"] != "ready" || labels["target"] != core.TargetLinux {
		t.Fatalf("labels=%v", labels)
	}
}

func TestExactTagsPreserveTailscaleValues(t *testing.T) {
	labels := map[string]string{
		"provider":           providerName,
		"lease":              "cbx_123",
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
