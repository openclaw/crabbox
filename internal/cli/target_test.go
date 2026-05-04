package cli

import (
	"strings"
	"testing"
)

func TestValidateProviderTargetRejectsBrokeredNonLinux(t *testing.T) {
	for _, target := range []string{targetMacOS, targetWindows} {
		cfg := baseConfig()
		cfg.Provider = "aws"
		cfg.TargetOS = target
		err := validateProviderTarget(cfg)
		if err == nil || !strings.Contains(err.Error(), "currently supports target=linux only") {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
}

func TestValidateProviderTargetAllowsStaticNonLinux(t *testing.T) {
	for _, target := range []string{targetMacOS, targetWindows} {
		cfg := baseConfig()
		cfg.Provider = staticProvider
		cfg.TargetOS = target
		if err := validateProviderTarget(cfg); err != nil {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
}
