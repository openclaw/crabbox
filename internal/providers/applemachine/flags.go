package applemachine

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.AppleMachineConfigFlagValues)
	if !ok {
		return nil
	}
	applied := v.Apply(&cfg.AppleContainer, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "apple-container", "apple-machine")
	if applied.Image {
		core.MarkAppleContainerImageExplicit(cfg)
	}
	return nil
}
