package unikraftcloud

import "strings"

// preflightUnikraftCloudClaimOwnership rejects ambiguous or corrupted local
// ownership before any provider mutation or create-intent reconciliation.
func preflightUnikraftCloudClaimOwnership(claims []LeaseClaim, scope string) error {
	accountUUID := unikraftCloudScopeAccountUUID(scope)
	resourceOwners := make(map[string]string)
	instanceOwners := make(map[string]string)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if err := validateUnikraftCloudClaim(claim, scope); err != nil {
			return err
		}
		if claim.Labels[ukcLabelAccountUUID] != accountUUID {
			return exit(4, "%s lease %q account identity does not match its local claim scope", providerName, claim.LeaseID)
		}
		resourceName := strings.TrimSpace(claim.Labels[ukcLabelResourceName])
		if expected := leaseProviderName(claim.LeaseID, ""); resourceName != expected {
			return exit(4, "%s lease %q has an unexpected recovery resource name", providerName, claim.LeaseID)
		}
		if previous, exists := resourceOwners[resourceName]; exists {
			return exit(5, "%s resource name %q is claimed by both %s and %s", providerName, resourceName, previous, claim.LeaseID)
		}
		resourceOwners[resourceName] = claim.LeaseID

		instanceID := strings.ToLower(strings.TrimSpace(claim.CloudID))
		if instanceID == "" {
			continue
		}
		if previous, exists := instanceOwners[instanceID]; exists {
			return exit(5, "%s instance %s is claimed by both %s and %s", providerName, instanceID, previous, claim.LeaseID)
		}
		instanceOwners[instanceID] = claim.LeaseID
	}
	return nil
}

func unikraftCloudScopeAccountUUID(scope string) string {
	const marker = "|account:"
	index := strings.LastIndex(scope, marker)
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(scope[index+len(marker):])
}
