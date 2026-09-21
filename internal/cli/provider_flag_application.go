package cli

import (
	"flag"
	"reflect"
)

// ApplyProviderConfigFlags uses the generated Apply method (and applyConfigFlags)
// for adapters that consume only InputAccepted. The constraint binds the values,
// destination and generated report types without a second provider registry.
// A false result means values had the wrong type; callers must skip postprocessing.
func ApplyProviderConfigFlags[Values interface {
	Apply(*ProviderConfig, *flag.FlagSet) (Applied, error)
}, ProviderConfig, Applied any](cfg *Config, fs *flag.FlagSet, values any, dst *ProviderConfig, providers ...string) (bool, error) {
	parsed, ok := values.(Values)
	if !ok {
		return false, nil
	}
	applied, err := parsed.Apply(dst, fs)
	// Generated reports retain accepted assignments even when a later field fails.
	RecordProviderFlagInputs(cfg, reflect.ValueOf(applied).FieldByName("InputAccepted").Bool(), providers...)
	return true, err
}
