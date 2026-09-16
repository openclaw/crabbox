package shared

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

// RejectExplicitMachineSizingFlags rejects visited class and type flags in that
// order. Provider selection and the call's validation phase remain with callers.
func RejectExplicitMachineSizingFlags(fs *flag.FlagSet, provider, classGuidance, typeGuidance string) error {
	if core.FlagWasSet(fs, "class") {
		suffix := ""
		if classGuidance != "" {
			suffix = "; " + classGuidance
		}
		return core.Exit(2, "--class is not supported for provider=%s%s", provider, suffix)
	}
	if core.FlagWasSet(fs, "type") {
		suffix := ""
		if typeGuidance != "" {
			suffix = "; " + typeGuidance
		}
		return core.Exit(2, "--type is not supported for provider=%s%s", provider, suffix)
	}
	return nil
}
