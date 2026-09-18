package githubcodespaces

import (
	"flag"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterGitHubCodespacesProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterGitHubCodespacesConfigFlags(fs, defaults.GitHubCodespaces)
}

func ApplyGitHubCodespacesProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if core.FlagWasSet(fs, "class") {
			return core.Exit(2, "--class is not supported for provider=github-codespaces; use --type or --github-codespaces-machine for a Codespaces machine slug")
		}
		if cfg.TargetOS != "" && strings.ToLower(strings.TrimSpace(cfg.TargetOS)) != targetLinux {
			return core.Exit(2, "provider=github-codespaces supports target=linux only")
		}
		if core.FlagWasSet(fs, "type") && !core.FlagWasSet(fs, "github-codespaces-machine") {
			if flag := fs.Lookup("type"); flag != nil {
				cfg.GitHubCodespaces.Machine = strings.TrimSpace(flag.Value.String())
				core.RecordProviderFlagInputs(cfg, true, providerName)
			}
		}
	}
	v, ok := values.(core.GitHubCodespacesConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.GitHubCodespaces, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.Machine {
		cfg.ServerType = strings.TrimSpace(cfg.GitHubCodespaces.Machine)
		cfg.ServerTypeExplicit = true
	}
	if applied.RetentionPeriod {
		core.MarkGitHubCodespacesRetentionExplicit(cfg)
	}
	if applied.DeleteOnRelease {
		markDeleteOnReleaseExplicit(cfg)
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.GitHubCodespaces.WorkRoot
		core.MarkWorkRootExplicit(cfg)
	}
	if err != nil {
		return err
	}
	return ValidateGitHubCodespacesConfig(*cfg)
}

func ValidateGitHubCodespacesConfig(cfg core.Config) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) && strings.TrimSpace(cfg.TargetOS) != "" && strings.ToLower(strings.TrimSpace(cfg.TargetOS)) != targetLinux {
		return core.Exit(2, "provider=github-codespaces supports target=linux only")
	}
	c := cfg.GitHubCodespaces
	if strings.TrimSpace(c.Repo) != "" && !validRepo(c.Repo) {
		return core.Exit(2, "github-codespaces repo must be owner/name")
	}
	if c.IdleTimeout < 0 {
		return core.Exit(2, "github-codespaces idle timeout must be non-negative")
	}
	if c.IdleTimeout > 0 && (c.IdleTimeout < 5*time.Minute || c.IdleTimeout > 4*time.Hour) {
		return core.Exit(2, "github-codespaces idle timeout must be between 5m and 4h")
	}
	if c.RetentionPeriod < 0 {
		return core.Exit(2, "github-codespaces retention period must be non-negative")
	}
	if c.RetentionPeriod > 30*24*time.Hour {
		return core.Exit(2, "github-codespaces retention period must not exceed 30 days")
	}
	if err := validateGitHubCodespacesWorkRoot("work root", c.WorkRoot); err != nil {
		return err
	}
	if err := validateGitHubCodespacesWorkRoot("generic work root", cfg.WorkRoot); err != nil {
		return err
	}
	if strings.TrimSpace(c.GHPath) == "" {
		return core.Exit(2, "github-codespaces gh path is required")
	}
	return nil
}

func validateGitHubCodespacesWorkRoot(label, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	clean := path.Clean(trimmed)
	if !path.IsAbs(clean) {
		return core.Exit(2, "github-codespaces %s must be absolute", label)
	}
	if clean != trimmed {
		return core.Exit(2, "github-codespaces %s must be a canonical path", label)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace", "/workspaces":
		return core.Exit(2, "github-codespaces %s %q is too broad; choose a dedicated subdirectory", label, clean)
	}
	return nil
}

func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	return ok && validRepoOwner(owner) && validRepoName(name)
}

func validRepoOwner(value string) bool {
	value = strings.TrimSpace(value)
	return validRepoPart(value) && !strings.HasPrefix(value, ".") && !strings.HasSuffix(value, ".")
}

func validRepoName(value string) bool {
	value = strings.TrimSpace(value)
	return validRepoPart(value) && value != "." && value != ".."
}

func validRepoPart(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "/") {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}
