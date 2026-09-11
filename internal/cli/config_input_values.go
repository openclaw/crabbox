package cli

import "strconv"

// These helpers record accepted input, not the provenance of a fallback or a
// later provider-specific normalization. They never retain the input value.
func configInputEnvString(cfg *Config, owner configInputOwner, fallback string, names ...string) string {
	if value, accepted := firstNonEmptyEnv(names...); accepted {
		recordConfigInput(cfg, owner, configInputEnvironment, true)
		return value
	}
	return fallback
}

func configInputEnvInteger(cfg *Config, owner configInputOwner, fallback int64, bits int, names ...string) int64 {
	accepted := false
	// Nested fallback getters read the last alias first. A valid primary value,
	// including zero, wins; malformed primary input keeps a valid alias.
	for i := len(names) - 1; i >= 0; i-- {
		if value, ok := lookupEnvInteger(names[i], bits); ok {
			fallback, accepted = value, true
		}
	}
	recordConfigInput(cfg, owner, configInputEnvironment, accepted)
	return fallback
}

func configInputEnvInt(cfg *Config, owner configInputOwner, fallback int, names ...string) int {
	return int(configInputEnvInteger(cfg, owner, int64(fallback), strconv.IntSize, names...))
}

func configInputEnvInt32(cfg *Config, owner configInputOwner, fallback int32, names ...string) int32 {
	return int32(configInputEnvInteger(cfg, owner, int64(fallback), 32, names...))
}

func recordConfigInputIntent(cfg *Config, owner configInputOwner, source configInputSource, applied bool) {
	if applied {
		if source == configInputFlag && cfg.synthesizedFlagInputs {
			return
		}
		cfg.inputProvenance = cfg.inputProvenance.withInput(owner, source, configInputIntent)
	}
}

// RecordProviderFlagIntents records explicitness effects that can apply without
// an accepted value. A raw flag visit alone does not establish such an effect.
func RecordProviderFlagIntents(cfg *Config, applied bool, providers ...string) {
	for _, provider := range providers {
		recordConfigInputIntent(cfg, configInputOwner(provider), configInputFlag, applied)
	}
}
