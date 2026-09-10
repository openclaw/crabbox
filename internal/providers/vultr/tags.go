package vultr

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
	ownershipTagConflictLabel = "_vultr_ownership_tag_conflict"
)

var tagSafeRe = regexp.MustCompile(`[^A-Za-z0-9_:\-]`)

var tagSchema = shared.LeaseTagSchema(
	shared.TagLabelField{Key: "provider_key_id"},
	shared.TagLabelField{Key: "provider_key_owned", Lowercase: true},
)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	tags := []string{
		tagCrabbox,
		"crabbox:provider:" + providerName,
		"crabbox:target:" + core.TargetLinux,
	}
	for _, key := range tagSchema.Keys() {
		if value := labels[key]; value != "" {
			tags = append(tags, encodeTagKV(key, value))
		}
	}
	return normalizeTags(tags)
}

func tagsFromLabels(labels map[string]string) []string {
	tags := []string{
		tagCrabbox,
		"crabbox:provider:" + providerName,
		"crabbox:target:" + core.TargetLinux,
	}
	for _, key := range tagSchema.Keys() {
		if value := labels[key]; value != "" {
			tags = append(tags, encodeTagKV(key, value))
		}
	}
	return normalizeTags(tags)
}

func encodeTagKV(key, value string) string {
	return tagPrefix + sanitizeTagPart(key) + ":" + sanitizeTagPart(value)
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

func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func labelsFromTags(tags []string) map[string]string {
	reducer := tagSchema.Reducer()
	for _, tag := range tags {
		lowerTag := strings.ToLower(tag)
		switch {
		case lowerTag == tagCrabbox:
			reducer.MarkOwned()
		case strings.HasPrefix(lowerTag, tagPrefix):
			parts := strings.SplitN(tag[len(tagPrefix):], ":", 2)
			if len(parts) == 2 {
				reducer.Apply(strings.ToLower(parts[0]), parts[1])
			}
		}
	}
	return reducer.Finish(ownershipTagConflictLabel)
}

func isOwnedInstance(inst vultrInstance) bool {
	return validateInstanceLabels(labelsFromTags(inst.Tags)) == nil
}

func validateInstanceLabels(labels map[string]string) error {
	if labels == nil ||
		labels[ownershipTagConflictLabel] != "" ||
		labels["crabbox"] != "true" ||
		labels["created_by"] != "crabbox" ||
		labels["provider"] != providerName ||
		!core.IsCanonicalLeaseID(labels["lease"]) ||
		labels["slug"] == "" ||
		labels["target"] != core.TargetLinux {
		return core.Exit(2, "refusing to operate on non-Crabbox Vultr instance")
	}
	return nil
}
