package unikraftcloud

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func validateUnikraftCloudDeleteIdentity(instance ukcInstance, expectedUUID, expectedName string) error {
	if err := validateUnikraftCloudInstanceIdentity(instance, expectedUUID, ""); err != nil {
		return err
	}
	if name := strings.TrimSpace(instance.Name); name != "" && expectedName != "" && name != expectedName {
		return core.Exit(5, "%s delete response for instance %s changed name: got %q, want %q", providerName, instance.UUID, instance.Name, expectedName)
	}
	return nil
}
