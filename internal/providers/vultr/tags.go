package vultr

import (
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

var tagSchema = shared.LeaseTagSchema(
	shared.TagLabelField{Key: "provider_key_id"},
	shared.TagLabelField{Key: "provider_key_owned", Lowercase: true},
)

func leaseTags(cfg core.Config, leaseID, slug, state string, keep bool, now time.Time) []string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
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
	return tagPrefix + shared.SanitizeTagPart(key, 64) + ":" + shared.SanitizeTagPart(value, 64)
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
	if !shared.ValidLeaseTagLabels(labels, providerName, ownershipTagConflictLabel) {
		return core.Exit(2, "refusing to operate on non-Crabbox Vultr instance")
	}
	return nil
}
