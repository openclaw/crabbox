package nvidiabrev

import "flag"

type nvidiaBrevFlagValues struct {
	CLI           *string
	Org           *string
	Type          *string
	GPUName       *string
	Provider      *string
	Mode          *string
	Launchable    *string
	StartupScript *string
	ReleaseAction *string
	Target        *string
	User          *string
	WorkRoot      *string
}

// RegisterNvidiaBrevProviderFlags exposes only non-secret Brev settings.
// Authentication is owned by the Brev CLI credential store, not Crabbox argv.
func RegisterNvidiaBrevProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return nvidiaBrevFlagValues{
		CLI:           fs.String("nvidia-brev-cli", defaults.NvidiaBrev.CLI, "NVIDIA Brev CLI path"),
		Org:           fs.String("nvidia-brev-org", defaults.NvidiaBrev.Org, "NVIDIA Brev organization selector"),
		Type:          fs.String("nvidia-brev-type", defaults.NvidiaBrev.Type, "NVIDIA Brev instance type selector"),
		GPUName:       fs.String("nvidia-brev-gpu-name", defaults.NvidiaBrev.GPUName, "NVIDIA Brev GPU name selector"),
		Provider:      fs.String("nvidia-brev-provider", defaults.NvidiaBrev.Provider, "NVIDIA Brev cloud provider selector"),
		Mode:          fs.String("nvidia-brev-mode", defaults.NvidiaBrev.Mode, "NVIDIA Brev mode: vm"),
		Launchable:    fs.String("nvidia-brev-launchable", defaults.NvidiaBrev.Launchable, "NVIDIA Brev launchable selector"),
		StartupScript: fs.String("nvidia-brev-startup-script", defaults.NvidiaBrev.StartupScript, "NVIDIA Brev startup script inline command or @file path"),
		ReleaseAction: fs.String("nvidia-brev-release-action", defaults.NvidiaBrev.ReleaseAction, "NVIDIA Brev release action: delete or stop"),
		Target:        fs.String("nvidia-brev-target", defaults.NvidiaBrev.Target, "NVIDIA Brev SSH target: container or host"),
		User:          fs.String("nvidia-brev-user", defaults.NvidiaBrev.User, "SSH user for NVIDIA Brev workspaces"),
		WorkRoot:      fs.String("nvidia-brev-work-root", defaults.NvidiaBrev.WorkRoot, "remote Crabbox work root on NVIDIA Brev workspaces"),
	}
}

func ApplyNvidiaBrevProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isNvidiaBrevProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --nvidia-brev-gpu-name", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --nvidia-brev-type", providerName)
		}
	}
	v, ok := values.(nvidiaBrevFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "nvidia-brev-cli") {
		cfg.NvidiaBrev.CLI = *v.CLI
	}
	if flagWasSet(fs, "nvidia-brev-org") {
		cfg.NvidiaBrev.Org = *v.Org
	}
	if flagWasSet(fs, "nvidia-brev-type") {
		cfg.NvidiaBrev.Type = *v.Type
	}
	if flagWasSet(fs, "nvidia-brev-gpu-name") {
		cfg.NvidiaBrev.GPUName = *v.GPUName
	}
	if flagWasSet(fs, "nvidia-brev-provider") {
		cfg.NvidiaBrev.Provider = *v.Provider
	}
	if flagWasSet(fs, "nvidia-brev-mode") {
		cfg.NvidiaBrev.Mode = *v.Mode
	}
	if flagWasSet(fs, "nvidia-brev-launchable") {
		cfg.NvidiaBrev.Launchable = *v.Launchable
	}
	if flagWasSet(fs, "nvidia-brev-startup-script") {
		cfg.NvidiaBrev.StartupScript = *v.StartupScript
	}
	if flagWasSet(fs, "nvidia-brev-release-action") {
		cfg.NvidiaBrev.ReleaseAction = *v.ReleaseAction
	}
	if flagWasSet(fs, "nvidia-brev-target") {
		cfg.NvidiaBrev.Target = *v.Target
	}
	if flagWasSet(fs, "nvidia-brev-user") {
		cfg.NvidiaBrev.User = *v.User
	}
	if flagWasSet(fs, "nvidia-brev-work-root") {
		cfg.NvidiaBrev.WorkRoot = *v.WorkRoot
	}
	if isNvidiaBrevProviderName(cfg.Provider) {
		applyNvidiaBrevDefaults(cfg)
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}
