package cloudrunsandbox

import (
	"flag"
	"strings"
)

type cloudRunSandboxFlagValues struct {
	GatewayURL  *string
	CLIPath     *string
	Workdir     *string
	AllowEgress *bool
	Write       *bool
	Rootfs      *string
}

func RegisterCloudRunSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return cloudRunSandboxFlagValues{
		GatewayURL:  fs.String("cloud-run-sandbox-gateway-url", defaults.CloudRunSandbox.GatewayURL, "durable-routing Cloud Run sandbox gateway URL (HTTPS)"),
		CLIPath:     fs.String("cloud-run-sandbox-cli", defaults.CloudRunSandbox.CLIPath, "path to the Cloud Run sandbox CLI binary (direct mode)"),
		Workdir:     fs.String("cloud-run-sandbox-workdir", defaults.CloudRunSandbox.Workdir, "absolute working directory inside the sandbox (sync target)"),
		AllowEgress: fs.Bool("cloud-run-sandbox-allow-egress", defaults.CloudRunSandbox.AllowEgress, "allow outbound network access from the sandbox (default deny)"),
		Write:       fs.Bool("cloud-run-sandbox-write", defaults.CloudRunSandbox.Write, "allow writable mounted filesystems inside the sandbox"),
		Rootfs:      fs.String("cloud-run-sandbox-rootfs", defaults.CloudRunSandbox.Rootfs, "root filesystem exposed to the sandbox (default /)"),
	}
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
	v, ok := values.(cloudRunSandboxFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "cloud-run-sandbox-gateway-url") {
		cfg.CloudRunSandbox.GatewayURL = *v.GatewayURL
	}
	if flagWasSet(fs, "cloud-run-sandbox-cli") {
		cfg.CloudRunSandbox.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "cloud-run-sandbox-workdir") {
		cfg.CloudRunSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "cloud-run-sandbox-allow-egress") {
		cfg.CloudRunSandbox.AllowEgress = *v.AllowEgress
	}
	if flagWasSet(fs, "cloud-run-sandbox-write") {
		cfg.CloudRunSandbox.Write = *v.Write
	}
	if flagWasSet(fs, "cloud-run-sandbox-rootfs") {
		cfg.CloudRunSandbox.Rootfs = *v.Rootfs
	}
	return validateConfig(*cfg)
}
