package githubcodespaces

import (
	"flag"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerFamily || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%#v missing %s", spec.Features, feature)
		}
	}
}

func TestProviderAliases(t *testing.T) {
	got := strings.Join(Provider{}.Aliases(), ",")
	if got != "codespaces,gh-codespaces" {
		t.Fatalf("aliases=%q", got)
	}
}

func TestServerTypeForConfigUsesMachineOrExplicitType(t *testing.T) {
	provider := Provider{}
	if got := provider.ServerTypeForConfig(core.Config{GitHubCodespaces: core.GitHubCodespacesConfig{Machine: "standardLinux32gb"}}); got != "standardLinux32gb" {
		t.Fatalf("machine ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForConfig(core.Config{ServerType: "premiumLinux", ServerTypeExplicit: true, GitHubCodespaces: core.GitHubCodespacesConfig{Machine: "standardLinux32gb"}}); got != "premiumLinux" {
		t.Fatalf("explicit ServerTypeForConfig=%q", got)
	}
	if got := provider.ServerTypeForClass("beast"); got != defaultCodespaceMachine {
		t.Fatalf("ServerTypeForClass=%q", got)
	}
}

func TestApplyFlagsSetsCodespacesConfigAndRejectsClass(t *testing.T) {
	cfg := core.Config{Provider: providerName, TargetOS: core.TargetLinux}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterGitHubCodespacesProviderFlags(fs, core.Config{
		GitHubCodespaces: core.GitHubCodespacesConfig{
			GHPath:          "gh",
			Machine:         defaultCodespaceMachine,
			IdleTimeout:     30 * time.Minute,
			RetentionPeriod: 7 * 24 * time.Hour,
			WorkRoot:        defaultWorkRoot,
		},
	})
	fs.String("class", "", "")
	fs.String("type", "", "")
	args := []string{
		"--github-codespaces-repo", "example-org/my-app",
		"--github-codespaces-ref", "main",
		"--github-codespaces-machine", "standardLinux32gb",
		"--github-codespaces-devcontainer-path", ".devcontainer/devcontainer.json",
		"--github-codespaces-working-directory", "/workspaces/my-app",
		"--github-codespaces-geo", "UsWest",
		"--github-codespaces-idle-timeout", "45m",
		"--github-codespaces-retention-period", "48h",
		"--github-codespaces-delete-on-release",
		"--github-codespaces-gh-path", "/usr/local/bin/gh",
		"--github-codespaces-work-root", "/workspaces/my-app",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := ApplyGitHubCodespacesProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.Repo != "example-org/my-app" ||
		cfg.GitHubCodespaces.Ref != "main" ||
		cfg.GitHubCodespaces.Machine != "standardLinux32gb" ||
		cfg.GitHubCodespaces.DevcontainerPath != ".devcontainer/devcontainer.json" ||
		cfg.GitHubCodespaces.WorkingDirectory != "/workspaces/my-app" ||
		cfg.GitHubCodespaces.Geo != "UsWest" ||
		cfg.GitHubCodespaces.IdleTimeout != 45*time.Minute ||
		cfg.GitHubCodespaces.RetentionPeriod != 48*time.Hour ||
		!cfg.GitHubCodespaces.DeleteOnRelease ||
		cfg.GitHubCodespaces.GHPath != "/usr/local/bin/gh" ||
		cfg.GitHubCodespaces.WorkRoot != "/workspaces/my-app" {
		t.Fatalf("config=%#v", cfg.GitHubCodespaces)
	}
	if !core.DeleteOnReleaseExplicit(cfg, providerName) {
		t.Fatal("delete-on-release flag not marked explicit")
	}

	typeAliasDefaults := core.Config{
		GitHubCodespaces: core.GitHubCodespacesConfig{
			GHPath:          "gh",
			Machine:         defaultCodespaceMachine,
			IdleTimeout:     30 * time.Minute,
			RetentionPeriod: 7 * 24 * time.Hour,
			WorkRoot:        defaultWorkRoot,
		},
	}
	typeAlias := typeAliasDefaults
	typeAlias.Provider = providerName
	typeAlias.TargetOS = core.TargetLinux
	typeFS := flag.NewFlagSet("test", flag.ContinueOnError)
	typeValues := RegisterGitHubCodespacesProviderFlags(typeFS, typeAliasDefaults)
	typeFS.String("class", "", "")
	typeFS.String("type", "", "")
	if err := typeFS.Parse([]string{"--type", "premiumLinux"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyGitHubCodespacesProviderFlags(&typeAlias, typeFS, typeValues); err != nil {
		t.Fatal(err)
	}
	if typeAlias.GitHubCodespaces.Machine != "premiumLinux" {
		t.Fatalf("--type machine=%q", typeAlias.GitHubCodespaces.Machine)
	}

	reject := flag.NewFlagSet("test", flag.ContinueOnError)
	rejectValues := RegisterGitHubCodespacesProviderFlags(reject, core.Config{})
	reject.String("class", "", "")
	reject.String("type", "", "")
	if err := reject.Parse([]string{"--class", "beast"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyGitHubCodespacesProviderFlags(&cfg, reject, rejectValues); err == nil || !strings.Contains(err.Error(), "--class is not supported") {
		t.Fatalf("class err=%v", err)
	}
}

func TestNoTokenFlagRegistered(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterGitHubCodespacesProviderFlags(fs, core.Config{})
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(strings.ToLower(f.Name), "token") {
			t.Fatalf("token-bearing flag registered: %s", f.Name)
		}
	})
}

func TestValidateGitHubCodespacesConfig(t *testing.T) {
	valid := core.Config{
		Provider: providerName,
		TargetOS: core.TargetLinux,
		GitHubCodespaces: core.GitHubCodespacesConfig{
			GHPath:          "gh",
			Repo:            "example-org/my-app",
			IdleTimeout:     time.Minute,
			RetentionPeriod: time.Hour,
			WorkRoot:        "/workspaces/my-app",
		},
	}
	if err := ValidateGitHubCodespacesConfig(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		mut  func(*core.Config)
		want string
	}{
		{name: "non-linux", mut: func(cfg *core.Config) { cfg.TargetOS = core.TargetMacOS }, want: "target=linux only"},
		{name: "bad repo", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.Repo = "example-org" }, want: "owner/name"},
		{name: "negative idle", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.IdleTimeout = -time.Second }, want: "non-negative"},
		{name: "relative work root", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.WorkRoot = "workspace" }, want: "absolute"},
		{name: "missing gh", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.GHPath = "" }, want: "gh path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mut(&cfg)
			err := ValidateGitHubCodespacesConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want %q", err, tt.want)
			}
		})
	}
}
