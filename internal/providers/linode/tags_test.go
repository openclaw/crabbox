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

	labels := labelsFromTags(leaseTags(cfg, "cbx_abcdef123456", "my-app", "ready", true, now))
	for _, key := range []string{"crabbox", "created_by", "provider", "target", "lease", "slug", "state", "keep", "server_type", "provider_key"} {
		if labels[key] == "" {
			t.Fatalf("labels missing %s: %v", key, labels)
		}
	}
	if labels["provider"] != providerName || labels["target"] != core.TargetLinux || labels["lease"] != "cbx_abcdef123456" || labels["slug"] != "my-app" || labels["state"] != "ready" || labels["keep"] != "true" {
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
	item := linodeInstance{ID: 1, Label: "crabbox-cbx-abcdef123456-my-app", Tags: leaseTags(cfg, "cbx_abcdef123456", "my-app", "ready", false, time.Unix(1, 0))}
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

func TestLinodeOwnedPredicateRejectsNonCanonicalLease(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	item := linodeInstance{ID: 1, Label: "legacy", Tags: leaseTags(cfg, "legacy-lease-1", "legacy", "ready", false, time.Unix(1, 0))}
	if isOwnedLinode(item) {
		t.Fatal("non-canonical lease must not be treated as owned")
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

func TestLinodeTagsRespectLengthLimitAndRoundTripLongSlug(t *testing.T) {
	slug := strings.Repeat("long-slug-", 6)
	tags := tagsFromLabels(map[string]string{
		"lease": "cbx_test",
		"slug":  slug,
	})
	for _, tag := range tags {
		if len(tag) > maxLinodeTagLength {
			t.Fatalf("tag length=%d tag=%q", len(tag), tag)
		}
	}
	if got := labelsFromTags(tags)["slug"]; got != slug {
		t.Fatalf("slug=%q, want %q; tags=%v", got, slug, tags)
	}
}

func TestLinodeMissingOwnershipTagChunkFailsClosed(t *testing.T) {
	tags := tagsFromLabels(map[string]string{
		"lease": "cbx_test",
		"slug":  strings.Repeat("long-slug-", 6),
	})
	for i, tag := range tags {
		if strings.HasPrefix(tag, "crabbox:slug_v2:") {
			tags = append(tags[:i], tags[i+1:]...)
			break
		}
	}
	labels := labelsFromTags(tags)
	if labels[ownershipTagConflictLabel] != "slug" {
		t.Fatalf("labels=%v tags=%v", labels, tags)
	}
	if err := validateLinodeLabels(labels); err == nil {
		t.Fatal("missing ownership chunk should fail closed")
	}
}

func TestReplaceCrabboxTagsPreservesExternalTags(t *testing.T) {
	got := replaceCrabboxTags(
		[]string{"customer:production", "crabbox", "crabbox:state:ready"},
		[]string{"crabbox", "crabbox:state:running"},
	)
	if !containsString(got, "customer:production") || containsString(got, "crabbox:state:ready") || !containsString(got, "crabbox:state:running") {
		t.Fatalf("tags=%v", got)
	}
}
