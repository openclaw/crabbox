package unikraftcloud

import "strings"

func indexUnikraftCloudInventory(instances []ukcInstance) (map[string]ukcInstance, error) {
	indexed := make(map[string]ukcInstance, len(instances))
	for _, instance := range instances {
		if !unikraftCloudUUIDPattern.MatchString(instance.UUID) {
			return nil, exit(5, "%s inventory returned an invalid instance UUID", providerName)
		}
		key := strings.ToLower(instance.UUID)
		if _, exists := indexed[key]; exists {
			return nil, exit(5, "%s inventory returned duplicate instance UUID %s", providerName, instance.UUID)
		}
		indexed[key] = instance
	}
	return indexed, nil
}
