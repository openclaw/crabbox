package digitalocean

import (
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	providerName = "digitalocean"

	tagCrabbox                = "crabbox"
	tagPrefix                 = "crabbox:"
	ownershipTagConflictLabel = "_digitalocean_ownership_tag_conflict"
)

var tagSafeRe = regexp.MustCompile(`[^A-Za-z0-9_:\-]`)

var tagSchema = shared.LeaseTagSchema(shared.TailscaleTagFields()...)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	if cfg.Tailscale.Enabled && len(cfg.Tailscale.Tags) > 0 {
		labels["tailscale_tags"] = strings.Join(cfg.Tailscale.Tags, ",")
	}
	return tagsFromLabels(labels)
}

func tagsFromLabels(labels map[string]string) []string {
	return tagSchema.EncodeTags(labels, []string{
		tagCrabbox,
		"crabbox:provider:" + providerName,
		"crabbox:target:" + core.TargetLinux,
	}, encodeTagKV)
}

func encodeTagKV(key, value string) string {
	key = sanitizeTagPart(key)
	if tagSchema.Exact(key) {
		key += "_v1"
		return tagPrefix + key + ":" + shared.EncodeExactTagValue(value, 255-len(tagPrefix)-len(key)-1)
	}
	return tagPrefix + key + ":" + sanitizeTagPart(value)
}

func sanitizeTagPart(value string) string {
	value = strings.TrimSpace(value)
	value = tagSafeRe.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "unknown"
	}
	if len(value) > 64 {
		return value[:64]
	}
	return value
}

func versionedExactTagValueKey(key string) (string, bool) {
	logical := strings.TrimSuffix(key, "_v1")
	return logical, logical != key && tagSchema.Exact(logical)
}

func legacyEncodedExactTagValueKey(key string) bool {
	switch key {
	case "tailscale_ipv4", "tailscale_fqdn", "tailscale_error":
		return true
	default:
		return false
	}
}

func labelsFromTags(tags []string) map[string]string {
	reducer := tagSchema.Reducer()
	var versionedExact, legacyExact shared.TagValueSet
	for _, tag := range tags {
		lowerTag := strings.ToLower(tag)
		switch {
		case lowerTag == tagCrabbox:
			reducer.MarkOwned()
		case strings.HasPrefix(lowerTag, tagPrefix):
			parts := strings.SplitN(tag[len(tagPrefix):], ":", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.ToLower(parts[0])
			if logical, ok := versionedExactTagValueKey(key); ok {
				versionedExact.Record(logical, shared.DecodeExactTagValue(parts[1]))
				continue
			}
			if tagSchema.Exact(key) {
				value := parts[1]
				if legacyEncodedExactTagValueKey(key) {
					value = shared.DecodeExactTagValue(value)
				}
				legacyExact.Record(key, value)
				continue
			}
			reducer.Apply(key, parts[1])
		}
	}
	for _, key := range tagSchema.Keys() {
		if !tagSchema.Exact(key) {
			continue
		}
		if value, present, conflict := versionedExact.Get(key); present {
			if !conflict {
				reducer.Apply(key, value)
			}
			continue
		}
		if value, present, conflict := legacyExact.Get(key); present && !conflict {
			reducer.Apply(key, value)
		}
	}
	return reducer.Finish(ownershipTagConflictLabel)
}

func isOwnedDroplet(d droplet) bool {
	return validateDropletLabels(labelsFromTags(d.Tags)) == nil
}

func validateDropletLabels(labels map[string]string) error {
	if labels == nil ||
		labels[ownershipTagConflictLabel] != "" ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		!core.IsCanonicalLeaseID(labels["lease"]) ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox DigitalOcean Droplet")
	}
	return nil
}
