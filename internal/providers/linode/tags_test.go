package linode

import (
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLinodeLeaseTagsRoundTripOwnershipLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "g6-standard-1"
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.Tags = []string{"tag:crabbox", "tag:ci runner"}
	now := time.Unix(1700000000, 0).UTC()

	labels := labelsFromTags(leaseTags(cfg, "cbx_test", "my-app", "ready", true, now))
	for _, key := range []string{"crabbox", "created_by", "provider", "target", "lease", "slug", "state", "keep", "server_type", "provider_key"} {
		if labels[key] == "" {
			t.Fatalf("labels missing %s: %v", key, labels)
		}
	}
	if labels["provider"] != providerName || labels["target"] != core.TargetLinux || labels["lease"] != "cbx_test" || labels["slug"] != "my-app" || labels["state"] != "ready" || labels["keep"] != "true" {
		t.Fatalf("labels=%v", labels)
	}
	if labels["tailscale_tags"] != "tag:crabbox,tag:ci runner" {
		t.Fatalf("tailscale_tags=%q", labels["tailscale_tags"])
	}
	if err := validateLinodeLabels(labels); err != nil {
		t.Fatalf("validateLinodeLabels: %v", err)
	}
}

func TestLinodeOwnershipConflictFailsClosed(t *testing.T) {
	tags := []string{
		tagCrabbox,
		"crabbox:provider:linode",
		"crabbox:provider:digitalocean",
		"crabbox:target:linux",
		"crabbox:lease:cbx_test",
		"crabbox:slug:my-app",
	}
	labels := labelsFromTags(tags)
	if labels[ownershipTagConflictLabel] != "provider" {
		t.Fatalf("conflict label=%q labels=%v", labels[ownershipTagConflictLabel], labels)
	}
	if err := validateLinodeLabels(labels); err == nil || !strings.Contains(err.Error(), "non-Crabbox Linode") {
		t.Fatalf("validate err=%v", err)
	}
}

func TestLinodeOwnedPredicateRequiresCompleteTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	item := linodeInstance{ID: 1, Label: "crabbox-cbx_test-my-app", Tags: leaseTags(cfg, "cbx_test", "my-app", "ready", false, time.Unix(1, 0))}
	if !isOwnedLinode(item) {
		t.Fatalf("expected owned item")
	}
	item.Tags = []string{tagCrabbox, "crabbox:provider:linode", "crabbox:target:linux", "crabbox:lease:cbx_test"}
	if isOwnedLinode(item) {
		t.Fatalf("partial tags should not be owned")
	}
	item.Tags = []string{tagCrabbox, "crabbox:provider:aws", "crabbox:target:linux", "crabbox:lease:cbx_test", "crabbox:slug:my-app"}
	if isOwnedLinode(item) {
		t.Fatalf("foreign provider should not be owned")
	}
}

func TestLinodeStateAndExpirationPreferSaferNewestValues(t *testing.T) {
	labels := labelsFromTags([]string{
		tagCrabbox,
		"crabbox:provider:linode",
		"crabbox:target:linux",
		"crabbox:lease:cbx_test",
		"crabbox:slug:my-app",
		"crabbox:state:provisioning",
		"crabbox:state:ready",
		"crabbox:expires_at:100",
		"crabbox:expires_at:200",
	})
	if labels["state"] != "ready" || labels["expires_at"] != "200" {
		t.Fatalf("labels=%v", labels)
	}
}
