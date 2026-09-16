package unikraftcloud

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func indexUnikraftCloudInventory(instances []ukcInstance) (map[string]ukcInstance, error) {
	indexed := make(map[string]ukcInstance, len(instances))
	for _, instance := range instances {
		if !unikraftCloudUUIDPattern.MatchString(instance.UUID) {
			return nil, core.Exit(5, "%s inventory returned an invalid instance UUID", providerName)
		}
		key := strings.ToLower(instance.UUID)
		if _, exists := indexed[key]; exists {
			return nil, core.Exit(5, "%s inventory returned duplicate instance UUID %s", providerName, instance.UUID)
		}
		indexed[key] = instance
	}
	return indexed, nil
}
