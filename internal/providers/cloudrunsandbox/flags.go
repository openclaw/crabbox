package cloudrunsandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterCloudRunSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCloudRunSandboxConfigFlags(fs, defaults.CloudRunSandbox)
}

func ApplyCloudRunSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName, "gcrun-sandbox", "google-cloud-run-sandbox", "cloudrun-sandbox":
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=cloud-run-sandbox; sandboxes share Cloud Run service CPU/memory")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=cloud-run-sandbox; sandboxes share Cloud Run service CPU/memory")
		}
	}
	v, ok := values.(core.CloudRunSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.CloudRunSandbox, fs)
	return validateConfig(*cfg)
}
