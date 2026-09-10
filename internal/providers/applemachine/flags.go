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
	if v.Apply(&cfg.AppleContainer, fs) {
		core.MarkAppleContainerImageExplicit(cfg)
	}
	return nil
}
