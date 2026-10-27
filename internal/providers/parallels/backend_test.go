package parallels

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestNewBackendMacOSGuestUser(t *testing.T) {
	t.Setenv("USER", "host-user")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"omitted", nil, "host-user"},
		{"unrelated flag", []string{"--parallels-source", "source"}, "host-user"},
		{"explicit default", []string{"--parallels-user", "crabbox"}, "crabbox"},
		{"explicit nondefault", []string{"--parallels-user", "guest-user"}, "guest-user"},
		{"template default", []string{"--parallels-template", "mac"}, "crabbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = core.TargetMacOS
			cfg.Parallels.Templates = map[string]core.ParallelsTemplateConfig{"mac": {User: "crabbox"}}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			provider := Provider{}
			values := provider.RegisterFlags(fs, cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			backend := NewBackend(provider.Spec(), cfg, core.Runtime{}).(*leaseBackend)
			if got := backend.Cfg.SSHUser; got != tc.want {
				t.Fatalf("guest SSH user = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewBackendLoadedGuestUser(t *testing.T) {
	for _, tc := range []struct {
		name, userFile, repoFile, hiddenFile, envUser, sshUser string
		target                                                 string
		args                                                   []string
		want                                                   string
	}{
		{name: "omitted", want: "host-user"},
		{name: "unrelated flag", args: []string{"--parallels-source", "source"}, want: "host-user"},
		{name: "host user is not a guest selection", args: []string{"--parallels-host-user", "host-account"}, want: "host-user"},
		{name: "Linux default", target: core.TargetLinux, want: "crabbox"},
		{name: "Windows default", target: core.TargetWindows, want: "crabbox"},
		{name: "user file", userFile: "parallels: {user: crabbox}\n", want: "crabbox"},
		{name: "repository file", repoFile: "parallels: {user: crabbox}\n", want: "crabbox"},
		{name: "hidden repository file", hiddenFile: "parallels: {user: crabbox}\n", want: "crabbox"},
		{name: "environment", envUser: "crabbox", want: "crabbox"},
		{name: "empty environment preserves file", userFile: "parallels: {user: crabbox}\n", want: "crabbox"},
		{name: "repository overrides user file", userFile: "parallels: {user: earlier}\n", repoFile: "parallels: {user: crabbox}\n", want: "crabbox"},
		{name: "hidden file overrides repository", repoFile: "parallels: {user: earlier}\n", hiddenFile: "parallels: {user: crabbox}\n", want: "crabbox"},
		{name: "environment overrides files", userFile: "parallels: {user: earlier}\n", repoFile: "parallels: {user: file-user}\n", envUser: "crabbox", want: "crabbox"},
		{name: "selected template overrides environment", repoFile: "parallels:\n  template: mac\n  templates:\n    mac: {user: crabbox}\n", envUser: "env-user", want: "crabbox"},
		{name: "flag template overrides environment", repoFile: "parallels:\n  templates:\n    mac: {user: crabbox}\n", envUser: "env-user", args: []string{"--parallels-template", "mac"}, want: "crabbox"},
		{name: "unselected template", repoFile: "parallels:\n  templates:\n    mac: {user: crabbox}\n", want: "host-user"},
		{name: "template omits user", repoFile: "parallels:\n  template: mac\n  templates:\n    mac: {source: source}\n", want: "host-user"},
		{name: "flag overrides template and environment", repoFile: "parallels:\n  template: mac\n  templates:\n    mac: {user: template-user}\n", envUser: "env-user", args: []string{"--parallels-user", "crabbox"}, want: "crabbox"},
		{name: "flag overrides flag template", repoFile: "parallels:\n  templates:\n    mac: {user: template-user}\n", args: []string{"--parallels-user", "crabbox", "--parallels-template", "mac"}, want: "crabbox"},
		{name: "generic file default", repoFile: "ssh: {user: crabbox}\n", want: "crabbox"},
		{name: "generic file nondefault", repoFile: "ssh: {user: guest-user}\n", want: "guest-user"},
		{name: "generic environment default", sshUser: "crabbox", want: "crabbox"},
		{name: "generic environment nondefault", sshUser: "guest-user", want: "guest-user"},
		{name: "provider user overrides generic", envUser: "crabbox", sshUser: "generic-user", want: "crabbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Chdir(t.TempDir())
			for _, name := range []string{"CRABBOX_CONFIG", "CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_OS", "CRABBOX_TARGET", "CRABBOX_TARGET_OS", "CRABBOX_PARALLELS_TEMPLATE", "CRABBOX_WINDOWS_MODE"} {
				t.Setenv(name, "")
			}
			t.Setenv("USER", "host-user")
			t.Setenv("CRABBOX_PROVIDER", "parallels")
			t.Setenv("CRABBOX_PARALLELS_USER", tc.envUser)
			t.Setenv("CRABBOX_SSH_USER", tc.sshUser)
			configDir, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(configDir, "crabbox", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
				t.Fatal(err)
			}
			for path, content := range map[string]string{userPath: tc.userFile, "crabbox.yaml": tc.repoFile, ".crabbox.yaml": tc.hiddenFile} {
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := core.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.TargetOS = core.TargetMacOS
			if tc.target != "" {
				cfg.TargetOS = tc.target
			}
			provider := Provider{}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := provider.RegisterFlags(fs, cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			backend := NewBackend(provider.Spec(), cfg, core.Runtime{}).(*leaseBackend)
			if got := backend.Cfg.SSHUser; got != tc.want {
				t.Fatalf("guest SSH user = %q, want %q", got, tc.want)
			}
		})
	}
}
