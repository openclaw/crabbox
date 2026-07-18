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

func TestProviderConfigDefaults(t *testing.T) {
	t.Run("provider defaults", func(t *testing.T) {
		cfg := core.Config{
			Provider:         providerName,
			SSHFallbackPorts: []string{"2222"},
		}
		if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.GitHubCodespaces.GHPath != defaultGHPath ||
			cfg.GitHubCodespaces.Machine != defaultCodespaceMachine ||
			cfg.GitHubCodespaces.IdleTimeout != 30*time.Minute ||
			cfg.GitHubCodespaces.RetentionPeriod != 7*24*time.Hour ||
			cfg.GitHubCodespaces.WorkRoot != defaultWorkRoot {
			t.Fatalf("provider defaults not applied: %#v", cfg.GitHubCodespaces)
		}
		if cfg.TargetOS != targetLinux || cfg.WorkRoot != defaultWorkRoot || cfg.SSHPort != defaultSSHPort || cfg.ServerType != defaultCodespaceMachine || cfg.SSHFallbackPorts != nil {
			t.Fatalf("generic defaults not applied: %#v", cfg)
		}
	})

	t.Run("explicit generic values", func(t *testing.T) {
		cfg := core.Config{
			Provider:           providerName,
			TargetOS:           targetLinux,
			WorkRoot:           "/workspaces/explicit",
			SSHPort:            "2222",
			ServerType:         "custom-machine",
			ServerTypeExplicit: true,
			GitHubCodespaces: core.GitHubCodespacesConfig{
				GHPath:          "/opt/gh",
				Machine:         "premiumLinux",
				IdleTimeout:     time.Hour,
				RetentionPeriod: 48 * time.Hour,
				WorkRoot:        "/workspaces/provider",
			},
		}
		core.MarkTargetExplicit(&cfg)
		core.MarkWorkRootExplicit(&cfg)
		core.MarkSSHPortExplicit(&cfg)
		if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.WorkRoot != "/workspaces/explicit" || cfg.SSHPort != "2222" || cfg.ServerType != "custom-machine" || cfg.GitHubCodespaces.Machine != "custom-machine" || cfg.SSHFallbackPorts != nil {
			t.Fatalf("explicit generic values not preserved: %#v", cfg)
		}
	})
}

func TestProviderClaimScope(t *testing.T) {
	for _, test := range []struct {
		name   string
		apiURL string
		want   string
	}{
		{name: "blank default endpoint", want: "repo:example-org/my-app"},
		{name: "explicit default endpoint", apiURL: " https://API.GITHUB.COM/ ", want: "repo:example-org/my-app"},
		{name: "enterprise endpoint", apiURL: " https://API.GITHUB.EXAMPLE/api/v3/ ", want: "endpoint:https://api.github.example/api/v3|repo:example-org/my-app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := core.Config{GitHubCodespaces: core.GitHubCodespacesConfig{
				APIURL: test.apiURL,
				Repo:   " Example-Org/My-App ",
			}}
			if got := (Provider{}).ClaimScope(cfg); got != test.want {
				t.Fatalf("ClaimScope=%q, want %q", got, test.want)
			}
		})
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
	if cfg.ServerType != "standardLinux32gb" || !cfg.ServerTypeExplicit {
		t.Fatalf("machine flag did not synchronize generic type: type=%q explicit=%t", cfg.ServerType, cfg.ServerTypeExplicit)
	}
	if !core.GitHubCodespacesRetentionExplicit(cfg) {
		t.Fatal("retention-period flag not marked explicit")
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
	for _, provider := range []string{providerName, "codespaces", "gh-codespaces"} {
		typeAlias := typeAliasDefaults
		typeAlias.Provider = provider
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
			t.Fatalf("provider=%s --type machine=%q", provider, typeAlias.GitHubCodespaces.Machine)
		}
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

func TestProviderSpecificMachineFlagOverridesExplicitGenericType(t *testing.T) {
	defaults := core.Config{GitHubCodespaces: core.GitHubCodespacesConfig{GHPath: defaultGHPath, Machine: defaultCodespaceMachine}}
	cfg := defaults
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	cfg.ServerType = "stale-explicit-type"
	cfg.ServerTypeExplicit = true
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterGitHubCodespacesProviderFlags(fs, defaults)
	fs.String("class", "", "")
	fs.String("type", "", "")
	if err := fs.Parse([]string{"--github-codespaces-machine", "premiumLinux"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyGitHubCodespacesProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerType != "premiumLinux" || cfg.GitHubCodespaces.Machine != "premiumLinux" || (Provider{}).ServerTypeForConfig(cfg) != "premiumLinux" {
		t.Fatalf("provider machine did not win: type=%q machine=%q effective=%q", cfg.ServerType, cfg.GitHubCodespaces.Machine, (Provider{}).ServerTypeForConfig(cfg))
	}
}

func TestExplicitZeroRetentionSurvivesProviderDefaults(t *testing.T) {
	defaults := core.Config{GitHubCodespaces: core.GitHubCodespacesConfig{GHPath: defaultGHPath, RetentionPeriod: 7 * 24 * time.Hour}}
	cfg := defaults
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterGitHubCodespacesProviderFlags(fs, defaults)
	fs.String("class", "", "")
	fs.String("type", "", "")
	if err := fs.Parse([]string{"--github-codespaces-retention-period", "0s"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyGitHubCodespacesProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCodespaces.RetentionPeriod != 0 || !core.GitHubCodespacesRetentionExplicit(cfg) {
		t.Fatalf("retention=%s explicit=%t", cfg.GitHubCodespaces.RetentionPeriod, core.GitHubCodespacesRetentionExplicit(cfg))
	}
}

func TestCodespacesWorkRootFlagControlsDefaultDerivation(t *testing.T) {
	defaults := core.Config{GitHubCodespaces: core.GitHubCodespacesConfig{
		GHPath:   defaultGHPath,
		WorkRoot: defaultWorkRoot,
	}}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "unset derives repository root", want: "/workspaces/my-app"},
		{name: "explicit default-looking value is honored", args: []string{"--github-codespaces-work-root", defaultWorkRoot}, want: defaultWorkRoot},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaults
			cfg.Provider = providerName
			cfg.TargetOS = targetLinux
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := RegisterGitHubCodespacesProviderFlags(fs, defaults)
			fs.String("class", "", "")
			fs.String("type", "", "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if err := ApplyGitHubCodespacesProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			backend := newBackend(Provider{}.Spec(), cfg, Runtime{})
			if got := backend.effectiveWorkRoot("example-org/my-app"); got != tt.want {
				t.Fatalf("work root=%q want %q", got, tt.want)
			}
			if len(tt.args) > 0 && !core.IsWorkRootExplicit(&cfg) {
				t.Fatal("work root flag was not marked explicit")
			}
		})
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
			IdleTimeout:     30 * time.Minute,
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
		{name: "short idle", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.IdleTimeout = 5*time.Minute - time.Second }, want: "between 5m and 4h"},
		{name: "long idle", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.IdleTimeout = 4*time.Hour + time.Second }, want: "between 5m and 4h"},
		{name: "negative retention", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.RetentionPeriod = -time.Second }, want: "non-negative"},
		{name: "long retention", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.RetentionPeriod = 30*24*time.Hour + time.Second }, want: "not exceed 30 days"},
		{name: "relative work root", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.WorkRoot = "workspace" }, want: "absolute"},
		{name: "noncanonical work root", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.WorkRoot = "/workspaces/my-app/../.." }, want: "canonical"},
		{name: "broad work root", mut: func(cfg *core.Config) { cfg.GitHubCodespaces.WorkRoot = "/workspaces" }, want: "too broad"},
		{name: "broad generic work root", mut: func(cfg *core.Config) { cfg.WorkRoot = "/" }, want: "too broad"},
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

func TestValidRepoAllowsDotGitHubRepository(t *testing.T) {
	if !validRepo("example-org/.github") {
		t.Fatal("expected .github repository to be valid")
	}
	for _, repo := range []string{".example/repo", "example./repo", "example-org/.", "example-org/.."} {
		if validRepo(repo) {
			t.Fatalf("repo=%q should be invalid", repo)
		}
	}
}
