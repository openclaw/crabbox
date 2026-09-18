package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_github_codespaces.go -output config_github_codespaces_generated.go -type GitHubCodespacesConfig -provider github-codespaces

// GitHubCodespacesConfig is intentionally token-free. Authentication comes
// from the GitHub CLI credential store or GitHub's standard environment
// variables at the point of use, never from Crabbox config or argv.
//
//configgen:flag-order Repo,Ref,Machine,DevcontainerPath,WorkingDirectory,Geo,IdleTimeout,RetentionPeriod,DeleteOnRelease,GHPath,WorkRoot
type GitHubCodespacesConfig struct {
	APIURL           string        `config:"apiUrl" env:"CRABBOX_GITHUB_CODESPACES_API_URL" sources:"user,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	GHPath           string        `config:"ghPath" env:"CRABBOX_GITHUB_CODESPACES_GH_PATH" flag:"github-codespaces-gh-path" sources:"user,env,flag" help:"GitHub CLI executable path" default:"gh" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Repo             string        `config:"repo" env:"CRABBOX_GITHUB_CODESPACES_REPO" flag:"github-codespaces-repo" sources:"user,env,flag" help:"GitHub repository owner/name for Codespaces" fileIgnoreEmpty:"true" fileStorage:"value"`
	Ref              string        `config:"ref" env:"CRABBOX_GITHUB_CODESPACES_REF" flag:"github-codespaces-ref" sources:"user,repo,env,flag" help:"Git ref for a new GitHub Codespace" fileIgnoreEmpty:"true" fileStorage:"value"`
	Machine          string        `config:"machine" env:"CRABBOX_GITHUB_CODESPACES_MACHINE" flag:"github-codespaces-machine" sources:"user,repo,env,flag" help:"GitHub Codespaces machine slug" default:"basicLinux32gb" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	DevcontainerPath string        `config:"devcontainerPath" env:"CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH" flag:"github-codespaces-devcontainer-path" sources:"user,repo,env,flag" help:"devcontainer path for a new GitHub Codespace" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkingDirectory string        `config:"workingDirectory" env:"CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY" flag:"github-codespaces-working-directory" sources:"user,repo,env,flag" help:"working directory inside the GitHub Codespace" fileIgnoreEmpty:"true" fileStorage:"value"`
	Geo              string        `config:"geo" env:"CRABBOX_GITHUB_CODESPACES_GEO" flag:"github-codespaces-geo" sources:"user,repo,env,flag" help:"GitHub Codespaces geographic location preference" fileIgnoreEmpty:"true" fileStorage:"value"`
	IdleTimeout      time.Duration `config:"idleTimeout" env:"CRABBOX_GITHUB_CODESPACES_IDLE_TIMEOUT" flag:"github-codespaces-idle-timeout" sources:"user,env,flag" help:"GitHub Codespaces idle timeout" default:"30m" duration:"positive-overlay" fileStorage:"value"`
	RetentionPeriod  time.Duration `config:"retentionPeriod" env:"CRABBOX_GITHUB_CODESPACES_RETENTION_PERIOD" flag:"github-codespaces-retention-period" sources:"user,env,flag" help:"GitHub Codespaces retention period" default:"168h" duration:"nonnegative-overlay" fileStorage:"value" reportApplied:"true"`
	DeleteOnRelease  bool          `config:"deleteOnRelease" env:"CRABBOX_GITHUB_CODESPACES_DELETE_ON_RELEASE" flag:"github-codespaces-delete-on-release" sources:"user,env,flag" help:"delete claim-owned GitHub Codespaces on release" default:"true" reportApplied:"true"`
	WorkRoot         string        `config:"workRoot" env:"CRABBOX_GITHUB_CODESPACES_WORK_ROOT" flag:"github-codespaces-work-root" sources:"user,repo,env,flag" help:"work root inside GitHub Codespaces" default:"/workspaces/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
}

func initialGitHubCodespacesConfig() GitHubCodespacesConfig {
	cfg := defaultGitHubCodespacesConfig()
	cfg.APIURL = "https://api.github.com"
	return cfg
}

func applyGitHubCodespacesFileConfig(cfg *Config, file *fileGitHubCodespacesConfig, trusted bool, source configInputSource) error {
	applied, err := cfg.GitHubCodespaces.applyFile(file, trusted)
	recordConfigInput(cfg, "github-codespaces", source, applied.InputAccepted)
	if applied.GHPath {
		cfg.GitHubCodespaces.GHPath = expandUserPath(cfg.GitHubCodespaces.GHPath)
	}
	if applied.RetentionPeriod {
		MarkGitHubCodespacesRetentionExplicit(cfg)
	}
	if applied.DeleteOnRelease {
		MarkDeleteOnReleaseExplicit(cfg, "github-codespaces")
	}
	return err
}

func applyGitHubCodespacesEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.GitHubCodespaces.applyEnv()
	recordConfigInput(cfg, "github-codespaces", configInputEnvironment, applied.InputAccepted)
	cfg.GitHubCodespaces.GHPath = expandUserPath(cfg.GitHubCodespaces.GHPath)
	if applied.RetentionPeriod {
		MarkGitHubCodespacesRetentionExplicit(cfg)
	}
	if applied.DeleteOnRelease {
		MarkDeleteOnReleaseExplicit(cfg, "github-codespaces")
	}
	return err
}
