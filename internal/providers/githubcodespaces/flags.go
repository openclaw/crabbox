package githubcodespaces

import (
	"flag"
	"path"
	"strings"
	"time"
)

type flagValues struct {
	Repo            *string
	Ref             *string
	Machine         *string
	Devcontainer    *string
	WorkingDir      *string
	Geo             *string
	IdleTimeout     *time.Duration
	RetentionPeriod *time.Duration
	DeleteOnRelease *bool
	GHPath          *string
	WorkRoot        *string
}

func RegisterGitHubCodespacesProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		Repo:            fs.String("github-codespaces-repo", defaults.GitHubCodespaces.Repo, "GitHub repository owner/name for Codespaces"),
		Ref:             fs.String("github-codespaces-ref", defaults.GitHubCodespaces.Ref, "Git ref for a new GitHub Codespace"),
		Machine:         fs.String("github-codespaces-machine", defaults.GitHubCodespaces.Machine, "GitHub Codespaces machine slug"),
		Devcontainer:    fs.String("github-codespaces-devcontainer-path", defaults.GitHubCodespaces.DevcontainerPath, "devcontainer path for a new GitHub Codespace"),
		WorkingDir:      fs.String("github-codespaces-working-directory", defaults.GitHubCodespaces.WorkingDirectory, "working directory inside the GitHub Codespace"),
		Geo:             fs.String("github-codespaces-geo", defaults.GitHubCodespaces.Geo, "GitHub Codespaces geographic location preference"),
		IdleTimeout:     fs.Duration("github-codespaces-idle-timeout", defaults.GitHubCodespaces.IdleTimeout, "GitHub Codespaces idle timeout"),
		RetentionPeriod: fs.Duration("github-codespaces-retention-period", defaults.GitHubCodespaces.RetentionPeriod, "GitHub Codespaces retention period"),
		DeleteOnRelease: fs.Bool("github-codespaces-delete-on-release", defaults.GitHubCodespaces.DeleteOnRelease, "delete claim-owned GitHub Codespaces on release"),
		GHPath:          fs.String("github-codespaces-gh-path", defaults.GitHubCodespaces.GHPath, "GitHub CLI executable path"),
		WorkRoot:        fs.String("github-codespaces-work-root", defaults.GitHubCodespaces.WorkRoot, "work root inside GitHub Codespaces"),
	}
}

func ApplyGitHubCodespacesProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isGitHubCodespacesProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=github-codespaces; use --type or --github-codespaces-machine for a Codespaces machine slug")
		}
		if cfg.TargetOS != "" && strings.ToLower(strings.TrimSpace(cfg.TargetOS)) != targetLinux {
			return exit(2, "provider=github-codespaces supports target=linux only")
		}
		if flagWasSet(fs, "type") && !flagWasSet(fs, "github-codespaces-machine") {
			if flag := fs.Lookup("type"); flag != nil {
				cfg.GitHubCodespaces.Machine = strings.TrimSpace(flag.Value.String())
			}
		}
	}
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "github-codespaces-repo") {
		cfg.GitHubCodespaces.Repo = *v.Repo
	}
	if flagWasSet(fs, "github-codespaces-ref") {
		cfg.GitHubCodespaces.Ref = *v.Ref
	}
	if flagWasSet(fs, "github-codespaces-machine") {
		cfg.GitHubCodespaces.Machine = *v.Machine
		cfg.ServerType = strings.TrimSpace(*v.Machine)
		cfg.ServerTypeExplicit = true
	}
	if flagWasSet(fs, "github-codespaces-devcontainer-path") {
		cfg.GitHubCodespaces.DevcontainerPath = *v.Devcontainer
	}
	if flagWasSet(fs, "github-codespaces-working-directory") {
		cfg.GitHubCodespaces.WorkingDirectory = *v.WorkingDir
	}
	if flagWasSet(fs, "github-codespaces-geo") {
		cfg.GitHubCodespaces.Geo = *v.Geo
	}
	if flagWasSet(fs, "github-codespaces-idle-timeout") {
		cfg.GitHubCodespaces.IdleTimeout = *v.IdleTimeout
	}
	if flagWasSet(fs, "github-codespaces-retention-period") {
		cfg.GitHubCodespaces.RetentionPeriod = *v.RetentionPeriod
		markRetentionPeriodExplicit(cfg)
	}
	if flagWasSet(fs, "github-codespaces-delete-on-release") {
		cfg.GitHubCodespaces.DeleteOnRelease = *v.DeleteOnRelease
		markDeleteOnReleaseExplicit(cfg)
	}
	if flagWasSet(fs, "github-codespaces-gh-path") {
		cfg.GitHubCodespaces.GHPath = *v.GHPath
	}
	if flagWasSet(fs, "github-codespaces-work-root") {
		cfg.GitHubCodespaces.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
		markWorkRootExplicit(cfg)
	}
	return ValidateGitHubCodespacesConfig(*cfg)
}

func ValidateGitHubCodespacesConfig(cfg Config) error {
	if isGitHubCodespacesProviderName(cfg.Provider) && strings.TrimSpace(cfg.TargetOS) != "" && strings.ToLower(strings.TrimSpace(cfg.TargetOS)) != targetLinux {
		return exit(2, "provider=github-codespaces supports target=linux only")
	}
	c := cfg.GitHubCodespaces
	if strings.TrimSpace(c.Repo) != "" && !validRepo(c.Repo) {
		return exit(2, "github-codespaces repo must be owner/name")
	}
	if c.IdleTimeout < 0 {
		return exit(2, "github-codespaces idle timeout must be non-negative")
	}
	if c.IdleTimeout > 0 && (c.IdleTimeout < 5*time.Minute || c.IdleTimeout > 4*time.Hour) {
		return exit(2, "github-codespaces idle timeout must be between 5m and 4h")
	}
	if c.RetentionPeriod < 0 {
		return exit(2, "github-codespaces retention period must be non-negative")
	}
	if c.RetentionPeriod > 30*24*time.Hour {
		return exit(2, "github-codespaces retention period must not exceed 30 days")
	}
	if err := validateGitHubCodespacesWorkRoot("work root", c.WorkRoot); err != nil {
		return err
	}
	if err := validateGitHubCodespacesWorkRoot("generic work root", cfg.WorkRoot); err != nil {
		return err
	}
	if strings.TrimSpace(c.GHPath) == "" {
		return exit(2, "github-codespaces gh path is required")
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
		return exit(2, "github-codespaces %s must be absolute", label)
	}
	if clean != trimmed {
		return exit(2, "github-codespaces %s must be a canonical path", label)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace", "/workspaces":
		return exit(2, "github-codespaces %s %q is too broad; choose a dedicated subdirectory", label, clean)
	}
	return nil
}

func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	return ok && validRepoOwner(owner) && validRepoName(name)
}

func isGitHubCodespacesProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "codespaces", "gh-codespaces":
		return true
	default:
		return false
	}
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
