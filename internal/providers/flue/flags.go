package flue

import (
	"flag"
	"path"
	"strings"
)

type flueFlagValues struct {
	CLIPath     *string
	Root        *string
	Workflow    *string
	Target      *string
	Config      *string
	EnvFile     *string
	Output      *string
	Workdir     *string
	TimeoutSecs *int
}

func RegisterFlueProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flueFlagValues{
		CLIPath:     fs.String("flue-cli", defaults.Flue.CLIPath, "Path to the flue CLI binary"),
		Root:        fs.String("flue-root", defaults.Flue.Root, "Flue project root directory"),
		Workflow:    fs.String("flue-workflow", defaults.Flue.Workflow, "Flue workflow name for Crabbox delegated runs"),
		Target:      fs.String("flue-target", defaults.Flue.Target, "Flue run target (v1 supports node only)"),
		Config:      fs.String("flue-config", defaults.Flue.Config, "Flue config file path"),
		EnvFile:     fs.String("flue-env", defaults.Flue.EnvFile, "Flue env file path; secrets stay in this file or Flue-managed env"),
		Output:      fs.String("flue-output", defaults.Flue.Output, "Flue output mode"),
		Workdir:     fs.String("flue-workdir", defaults.Flue.Workdir, "Absolute working directory inside the Flue sandbox"),
		TimeoutSecs: fs.Int("flue-timeout-secs", defaults.Flue.TimeoutSecs, "Flue delegated run timeout in seconds"),
	}
}

func ApplyFlueProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; configure the Flue workflow instead", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; configure the Flue workflow instead", providerName)
		}
	}
	v, ok := values.(flueFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "flue-cli") {
		cfg.Flue.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "flue-root") {
		cfg.Flue.Root = *v.Root
	}
	if flagWasSet(fs, "flue-workflow") {
		cfg.Flue.Workflow = *v.Workflow
	}
	if flagWasSet(fs, "flue-target") {
		cfg.Flue.Target = *v.Target
	}
	if flagWasSet(fs, "flue-config") {
		cfg.Flue.Config = *v.Config
	}
	if flagWasSet(fs, "flue-env") {
		cfg.Flue.EnvFile = *v.EnvFile
	}
	if flagWasSet(fs, "flue-output") {
		cfg.Flue.Output = *v.Output
	}
	if flagWasSet(fs, "flue-workdir") {
		cfg.Flue.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "flue-timeout-secs") {
		cfg.Flue.TimeoutSecs = *v.TimeoutSecs
	}
	return ValidateFlueDoctorConfig(*cfg)
}

func ValidateFlueConfig(cfg Config) error {
	if err := ValidateFlueDoctorConfig(cfg); err != nil {
		return err
	}
	return ValidateFlueRunTarget(cfg)
}

func ValidateFlueDoctorConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Flue.CLIPath) == "" {
		return exit(2, "flue cliPath must not be empty")
	}
	if strings.TrimSpace(cfg.Flue.Workflow) == "" {
		return exit(2, "flue workflow must not be empty")
	}
	if cfg.Flue.TimeoutSecs < 0 {
		return exit(2, "flue timeoutSecs must be non-negative")
	}
	if _, err := cleanWorkdir(cfg.Flue.Workdir); err != nil {
		return err
	}
	return nil
}

func ValidateFlueRunTarget(cfg Config) error {
	target := strings.ToLower(strings.TrimSpace(cfg.Flue.Target))
	if target == "" {
		target = defaultTarget
	}
	if target != defaultTarget {
		return exit(2, "provider=%s supports flue target=node only in v1; upload/HTTP staging is required before %q can be used", providerName, target)
	}
	return nil
}

func cleanWorkdir(value string) (string, error) {
	workdir := strings.TrimSpace(value)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	if !strings.HasPrefix(workdir, "/") {
		return "", exit(2, "flue workdir %q must be absolute", workdir)
	}
	clean := path.Clean(workdir)
	switch clean {
	case "/", "/tmp", "/home", "/workspace":
		return "", exit(2, "flue workdir %q is too broad; use a provider-owned subdirectory such as %s", clean, defaultWorkdir)
	}
	return clean, nil
}
