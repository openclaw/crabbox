package vultr

import (
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestVultrTagsRoundTripOwnership(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "vc2-1c-1gb"
	tags := leaseTags(cfg, "cbx_abcdef123456", "blue", "ready", false, time.Unix(1700000000, 0))
	labels := labelsFromTags(tags)
	if labels["crabbox"] != "true" || labels["provider"] != providerName || labels["lease"] != "cbx_abcdef123456" || labels["slug"] != "blue" || labels["target"] != core.TargetLinux {
		t.Fatalf("labels=%v tags=%v", labels, tags)
	}
	if err := validateInstanceLabels(labels); err != nil {
		t.Fatalf("validateInstanceLabels: %v", err)
	}
}

func TestVultrTagsRejectPartialForeignAndConflictingOwnership(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	for name, tags := range map[string][]string{
		"non-canonical lease": leaseTags(cfg, "legacy-lease-1", "blue", "ready", false, time.Unix(1, 0)),
		"partial": {
			tagCrabbox,
			"crabbox:provider:vultr",
		},
		"foreign": {
			tagCrabbox,
			"crabbox:provider:digitalocean",
			"crabbox:target:linux",
			"crabbox:lease:cbx_111111111111",
			"crabbox:slug:blue",
		},
		"conflict": {
			tagCrabbox,
			"crabbox:provider:vultr",
			"crabbox:provider:other",
			"crabbox:target:linux",
			"crabbox:lease:cbx_111111111111",
			"crabbox:slug:blue",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateInstanceLabels(labelsFromTags(tags))
			if err == nil || !strings.Contains(err.Error(), "non-Crabbox Vultr instance") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
