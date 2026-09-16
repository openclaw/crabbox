package scaleway

import (
	"regexp"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	tagCrabbox                = "crabbox"
	tagPrefix                 = "crabbox:"
	ownershipTagConflictLabel = "_scaleway_ownership_tag_conflict"
)

var tagSafeRe = regexp.MustCompile(`[^A-Za-z0-9_:\-]`)

// Share field definitions while retaining Scaleway's own decoding precedence.
var tagSchema = shared.LeaseTagSchema(append(shared.TailscaleTagFields(),
	shared.TagLabelField{Key: "recovery"},
	shared.TagLabelField{Key: "scaleway_project"},
	shared.TagLabelField{Key: "scaleway_organization"},
	shared.TagLabelField{Key: "scaleway_region"},
	shared.TagLabelField{Key: "scaleway_zone"},
	shared.TagLabelField{Key: "scaleway_ssh_key_id"},
	shared.TagLabelField{Key: "scaleway_ssh_key_name"},
)...)

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

func labelsFromTags(tags []string) map[string]string {
	labels := map[string]string{}
	ownershipConflicts := map[string]bool{}
	for _, tag := range tags {
		lowerTag := strings.ToLower(tag)
		switch {
		case lowerTag == tagCrabbox:
			labels["crabbox"] = "true"
			labels["created_by"] = "crabbox"
		case strings.HasPrefix(lowerTag, tagPrefix):
			parts := strings.SplitN(tag[len(tagPrefix):], ":", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.ToLower(parts[0])
			value := parts[1]
			if logical, ok := versionedExactTagValueKey(key); ok {
				labels[logical] = shared.DecodeExactTagValue(value)
				continue
			}
			switch key {
			case "provider", "lease", "slug", "target":
				if prior := labels[key]; prior != "" && prior != value {
					ownershipConflicts[key] = true
				}
				if key == "provider" || key == "target" {
					value = strings.ToLower(value)
				}
				labels[key] = value
			case "state":
				labels[key] = strings.ToLower(value)
			default:
				labels[key] = value
			}
		}
	}
	if len(ownershipConflicts) > 0 {
		keys := make([]string, 0, len(ownershipConflicts))
		for key := range ownershipConflicts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		labels[ownershipTagConflictLabel] = strings.Join(keys, ",")
	}
	return labels
}
