package runcloud

import (
	"flag"
	"path"
	"strings"
)

type flagValues struct {
	CLIPath *string
	Image   *string
	Region  *string
	Workdir *string
}

func RegisterRunCloudProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		CLIPath: fs.String("run-cloud-cli", defaults.RunCloud.CLIPath, "Run Cloud CLI path"),
		Image:   fs.String("run-cloud-image", defaults.RunCloud.Image, "Run Cloud sandbox image"),
		Region:  fs.String("run-cloud-region", defaults.RunCloud.Region, "Run Cloud sandbox region"),
		Workdir: fs.String("run-cloud-workdir", defaults.RunCloud.Workdir, "absolute working directory inside the Run Cloud sandbox"),
	}
}

func ApplyRunCloudProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return exit(2, "provider=%s supports target=linux only", providerName)
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "run-cloud-cli") {
		cfg.RunCloud.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "run-cloud-image") {
		cfg.RunCloud.Image = *v.Image
	}
	if flagWasSet(fs, "run-cloud-region") {
		cfg.RunCloud.Region = *v.Region
	}
	if flagWasSet(fs, "run-cloud-workdir") {
		cfg.RunCloud.Workdir = *v.Workdir
	}
	if cfg.Provider == providerName {
		cleaned, err := cleanWorkdir(cfg.RunCloud.Workdir)
		if err != nil {
			return err
		}
		cfg.WorkRoot = cleaned
	}
	return nil
}

func cleanWorkdir(value string) (string, error) {
	cleaned := path.Clean(strings.TrimSpace(value))
	if cleaned == "." || !strings.HasPrefix(cleaned, "/") {
		return "", exit(2, "runCloud.workdir %q must resolve to an absolute path", value)
	}
	switch cleaned {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "runCloud.workdir %q is too broad; choose a dedicated subdirectory", cleaned)
	}
	return cleaned, nil
}
