package cli

// ResolveInheritedWorkRoot preserves a raw nonempty provider root, otherwise
// inheriting a non-default generic root or using the caller's fallback.
func ResolveInheritedWorkRoot(providerRoot, genericRoot, fallback string) string {
	if providerRoot != "" {
		return providerRoot
	}
	if !isDefaultWorkRoot(genericRoot) {
		return genericRoot
	}
	return fallback
}
