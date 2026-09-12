package cli

// ProviderNameMatches compares a name with provider metadata without consulting
// the registry or configuring a backend.
func ProviderNameMatches(name string, provider Provider) bool {
	name = normalizeProviderName(name)
	if name == "" {
		return false
	}
	if name == normalizeProviderName(provider.Name()) {
		return true
	}
	for _, alias := range provider.Aliases() {
		if name == normalizeProviderName(alias) {
			return true
		}
	}
	return false
}

// ProviderNameMatchesExact compares raw names with provider metadata without
// normalization, registry lookup, or backend configuration.
func ProviderNameMatchesExact(name string, provider Provider) bool {
	if name == provider.Name() {
		return true
	}
	for _, alias := range provider.Aliases() {
		if name == alias {
			return true
		}
	}
	return false
}
