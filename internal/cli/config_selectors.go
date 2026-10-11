package cli

import "flag"

// These selectors affect provider defaults and must be resolved before those
// defaults run. Only visited flags override inherited values.
func applyConfigSelectorFlags(cfg *Config, fs *flag.FlagSet) error {
	// The command records accepted flag inputs after identifying synthesized
	// job/adapter arguments. This early pass only supplies validation context.
	inputs := cfg.inputProvenance
	defer func() { cfg.inputProvenance = inputs }()
	if flagWasSet(fs, "provider") {
		setProviderSelection(cfg, fs.Lookup("provider").Value.String(), providerSelectionFlag)
		cfg.brokerProvider = ""
	}
	applyTargetSelectorFlags(cfg, fs, flagStringValue(fs, "target"), flagStringValue(fs, "windows-mode"))
	if flagWasSet(fs, "arch") {
		cfg.Architecture = fs.Lookup("arch").Value.String()
		cfg.architectureExplicit = true
	}
	if flagWasSet(fs, "os") {
		cfg.OSImage = fs.Lookup("os").Value.String()
		cfg.osImageExplicit = true
	}
	if flagWasSet(fs, "class") {
		cfg.Class = fs.Lookup("class").Value.String()
		cfg.classFlagExplicit = true
		MarkClassExplicit(cfg)
	}
	if flagWasSet(fs, "type") {
		cfg.ServerType = fs.Lookup("type").Value.String()
		cfg.ServerTypeExplicit = true
	}
	for _, name := range []string{"arch", "os", "class", "type"} {
		recordConfigInput(cfg, configInputGeneric, configInputFlag, flagWasSet(fs, name))
	}
	return nil
}

func flagStringValue(fs *flag.FlagSet, name string) string {
	if value := fs.Lookup(name); value != nil {
		return value.Value.String()
	}
	return ""
}

func applyTargetSelectorFlags(cfg *Config, fs *flag.FlagSet, target, windowsMode string) {
	if flagWasSet(fs, "target") {
		cfg.TargetOS = target
		cfg.targetExplicit = true
		cfg.targetFlagExplicit = true
		cfg.credentialProvenance.externalDesktopTarget = credentialSourceFlag
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
		if normalizeTargetOS(cfg.TargetOS) != targetWindows && !flagWasSet(fs, "windows-mode") {
			cfg.WindowsMode = windowsModeNormal
			cfg.explicitWindowsMode = ""
			cfg.credentialProvenance.externalDesktopMode = credentialSourceFlag
		}
	}
	if flagWasSet(fs, "windows-mode") {
		cfg.WindowsMode = windowsMode
		cfg.explicitWindowsMode = cfg.WindowsMode
		cfg.windowsModeFlagExplicit = true
		cfg.credentialProvenance.externalDesktopMode = credentialSourceFlag
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
}
