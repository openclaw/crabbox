package coder

import (
	"flag"
	"path"
	"strings"
)

type coderFlagValues struct {
	CLIPath              *string
	Template             *string
	Preset               *string
	WorkspacePrefix      *string
	WorkRoot             *string
	DeleteOnRelease      *bool
	Wait                 *string
	UseParameterDefaults *bool
	Parameters           *string
	RichParameterFile    *string
}

func RegisterCoderProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return coderFlagValues{
		CLIPath:              fs.String("coder-cli", defaults.Coder.CLIPath, "Coder CLI path"),
		Template:             fs.String("coder-template", defaults.Coder.Template, "Coder template for new workspaces"),
		Preset:               fs.String("coder-preset", defaults.Coder.Preset, "Coder template preset"),
		WorkspacePrefix:      fs.String("coder-workspace-prefix", defaults.Coder.WorkspacePrefix, "prefix for Crabbox-managed Coder workspace names"),
		WorkRoot:             fs.String("coder-work-root", defaults.Coder.WorkRoot, "Coder workspace Crabbox work root"),
		DeleteOnRelease:      fs.Bool("coder-delete-on-release", defaults.Coder.DeleteOnRelease, "delete Coder workspace on release instead of stopping it"),
		Wait:                 fs.String("coder-wait", defaults.Coder.Wait, "Coder SSH startup wait mode: yes, no, or auto"),
		UseParameterDefaults: fs.Bool("coder-use-parameter-defaults", defaults.Coder.UseParameterDefaults, "pass --use-parameter-defaults to coder create"),
		Parameters:           fs.String("coder-parameter", strings.Join(defaults.Coder.Parameters, ","), "comma-separated Coder parameter values name=value"),
		RichParameterFile:    fs.String("coder-rich-parameter-file", defaults.Coder.RichParameterFile, "Coder rich parameter file"),
	}
}

func ApplyCoderProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == coderProvider {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=coder; choose size through the Coder template or --coder-preset")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=coder; choose a Coder template with --coder-template")
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=coder supports target=linux only")
		}
	}
	v, ok := values.(coderFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "coder-cli") {
		cfg.Coder.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "coder-template") {
		cfg.Coder.Template = *v.Template
	}
	if flagWasSet(fs, "coder-preset") {
		cfg.Coder.Preset = *v.Preset
	}
	if flagWasSet(fs, "coder-workspace-prefix") {
		cfg.Coder.WorkspacePrefix = *v.WorkspacePrefix
	}
	if flagWasSet(fs, "coder-work-root") {
		cfg.Coder.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "coder-delete-on-release") {
		cfg.Coder.DeleteOnRelease = *v.DeleteOnRelease
	}
	if flagWasSet(fs, "coder-wait") {
		cfg.Coder.Wait = *v.Wait
	}
	if flagWasSet(fs, "coder-use-parameter-defaults") {
		cfg.Coder.UseParameterDefaults = *v.UseParameterDefaults
	}
	if flagWasSet(fs, "coder-parameter") {
		cfg.Coder.Parameters = splitCommaList(*v.Parameters)
	}
	if flagWasSet(fs, "coder-rich-parameter-file") {
		cfg.Coder.RichParameterFile = *v.RichParameterFile
	}
	if cfg.Provider == coderProvider {
		return validateCoderConfig(*cfg)
	}
	return nil
}

func validateCoderConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Coder.CLIPath) == "" {
		return exit(2, "coder.cliPath must not be empty")
	}
	if err := validateCoderWait(cfg.Coder.Wait); err != nil {
		return err
	}
	if _, err := cleanCoderWorkRoot(coderWorkRoot(cfg)); err != nil {
		return err
	}
	if _, err := cleanCoderWorkspacePrefix(cfg.Coder.WorkspacePrefix); err != nil {
		return err
	}
	for _, param := range cfg.Coder.Parameters {
		if strings.TrimSpace(param) == "" {
			return exit(2, "coder.parameters entries must not be empty")
		}
		if !strings.Contains(param, "=") {
			return exit(2, "coder parameter %q must use name=value", param)
		}
	}
	return nil
}

func validateCoderWait(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "yes", "no", "auto":
		return nil
	default:
		return exit(2, "coder.wait must be yes, no, or auto")
	}
}

func coderWorkRoot(cfg Config) string {
	if strings.TrimSpace(cfg.Coder.WorkRoot) != "" {
		return strings.TrimSpace(cfg.Coder.WorkRoot)
	}
	return "/home/coder/crabbox"
}

func cleanCoderWorkRoot(workRoot string) (string, error) {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return "", exit(2, "coder.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspaces":
		return "", exit(2, "coder.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func cleanCoderWorkspacePrefix(prefix string) (string, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		prefix = "crabbox-"
	}
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		return "", exit(2, "coder.workspacePrefix must include at least one letter or number")
	}
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return "", exit(2, "coder.workspacePrefix must contain only letters, numbers, and hyphens")
	}
	return prefix + "-", nil
}

func splitCommaList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
