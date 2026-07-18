package githubcodespaces

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHConfigParsesProxyTarget(t *testing.T) {
	target, err := selectSSHTarget(Config{GitHubCodespaces: GitHubCodespacesConfig{WorkRoot: "/workspaces/my-app"}}, `Host sturdy-space
  User vscode
  IdentityFile "/tmp/codespaces/key"
  UserKnownHostsFile=/dev/null
  ProxyCommand gh codespace ssh -c sturdy-space --stdio
`, "sturdy-space")
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "vscode" || target.Host != "sturdy-space" || target.Port != "22" || target.Key != "/tmp/codespaces/key" {
		t.Fatalf("target=%#v", target)
	}
	if !target.SSHConfigProxy || target.ProxyCommand != "gh codespace ssh -c sturdy-space --stdio" || target.ChildEnv["GH_HOST"] != "github.com" {
		t.Fatalf("proxy target=%#v", target)
	}
	if target.KnownHostsFile != "/dev/null" || target.TargetOS != targetLinux || target.NetworkKind != networkPublic {
		t.Fatalf("target metadata=%#v", target)
	}
	for _, want := range []string{"git", "rsync", "tar", "test -d '/workspaces/my-app'"} {
		if !strings.Contains(target.ReadyCheck, want) {
			t.Fatalf("ready check %q missing %q", target.ReadyCheck, want)
		}
	}
}

func TestSSHConfigSelectsGeneratedGitHubCLIAliasByProxyCodespace(t *testing.T) {
	target, err := selectSSHTarget(Config{GitHubCodespaces: GitHubCodespacesConfig{GHPath: "/opt/github/bin/gh"}}, `Host cs.sturdy-space.main
  User vscode
  IdentityFile "/tmp/codespaces/key"
  UserKnownHostsFile /dev/null
  ProxyCommand gh codespace ssh -c sturdy-space --stdio
`, "sturdy-space")
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "cs.sturdy-space.main" || !target.SSHConfigProxy {
		t.Fatalf("target=%#v", target)
	}
	if target.ProxyCommand != "'/opt/github/bin/gh' codespace ssh -c sturdy-space --stdio" || target.ChildEnv["GH_HOST"] != "github.com" {
		t.Fatalf("proxy=%q", target.ProxyCommand)
	}
}

func TestSSHConfigProxyPinsConfiguredGitHubHost(t *testing.T) {
	target, err := selectSSHTarget(Config{GitHubCodespaces: GitHubCodespacesConfig{APIURL: "https://api.octocorp.ghe.com"}}, `Host sturdy-space
  User vscode
  IdentityFile /tmp/codespaces/key
  ProxyCommand gh codespace ssh -c sturdy-space --stdio
`, "sturdy-space")
	if err != nil {
		t.Fatal(err)
	}
	if target.ProxyCommand != "gh codespace ssh -c sturdy-space --stdio" || target.ChildEnv["GH_HOST"] != "octocorp.ghe.com" {
		t.Fatalf("target=%#v", target)
	}
}

func TestSSHConfigPreservesHashesInsideDirectiveValues(t *testing.T) {
	entries, err := parseSSHConfig(`# generated configuration
Host sturdy-space
  User vscode # generated user
  IdentityFile /tmp/alice#dev/codespaces.auto
  ProxyCommand /opt/github#dev/gh codespace ssh -c sturdy-space --stdio # generated proxy
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].User != "vscode" || entries[0].IdentityFile != "/tmp/alice#dev/codespaces.auto" || entries[0].ProxyCommand != "/opt/github#dev/gh codespace ssh -c sturdy-space --stdio" {
		t.Fatalf("entries=%#v", entries)
	}
}

func TestSSHConfigParsesDirectTarget(t *testing.T) {
	target, err := selectSSHTarget(Config{}, `Host sturdy-space
  HostName 127.0.0.1
  User vscode
  Port 2222
  IdentityFile "/tmp/codespaces/key"
`, "sturdy-space")
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "127.0.0.1" || target.Port != "2222" || target.SSHConfigProxy {
		t.Fatalf("target=%#v", target)
	}
}

func TestSSHConfigRejectsMissingFieldsAndAmbiguousAlias(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "missing user", data: `Host sturdy
  IdentityFile "/tmp/key"
  ProxyCommand gh codespace ssh -c sturdy --stdio
`, want: "missing User"},
		{name: "missing identity", data: `Host sturdy
  User vscode
  ProxyCommand gh codespace ssh -c sturdy --stdio
`, want: "missing IdentityFile"},
		{name: "missing route", data: `Host sturdy
  User vscode
  IdentityFile "/tmp/key"
`, want: "missing HostName or ProxyCommand"},
		{name: "missing alias", data: `Host other
  User vscode
  IdentityFile "/tmp/key"
  ProxyCommand gh codespace ssh -c other --stdio
`, want: "not found"},
		{name: "ambiguous", data: `Host sturdy
  User vscode
  IdentityFile "/tmp/key"
  ProxyCommand gh codespace ssh -c sturdy --stdio

Host sturdy
  User vscode
  IdentityFile "/tmp/key"
  ProxyCommand gh codespace ssh -c sturdy --stdio
`, want: "ambiguous"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := selectSSHTarget(Config{}, tt.data, "sturdy")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want %q", err, tt.want)
			}
		})
	}
}

func TestSSHConfigRejectsInvalidUsers(t *testing.T) {
	for _, user := range []string{"-oProxyCommand=sh", "alice@example.com", "alice bob", "alice\tbob"} {
		_, err := selectSSHTarget(Config{}, `Host sturdy
  User `+user+`
  IdentityFile "/tmp/key"
  ProxyCommand gh codespace ssh -c sturdy --stdio
`, "sturdy")
		if err == nil || !strings.Contains(err.Error(), "invalid User") {
			t.Fatalf("user=%q err=%v", user, err)
		}
	}
}

func TestStoreSSHConfigRejectsExistingSymlink(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "crabbox", "github-codespaces")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cbx_123456789abc.ssh_config")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := storeSSHConfig("cbx_123456789abc", "replacement\n"); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("err=%v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original\n" {
		t.Fatalf("target=%q err=%v", data, err)
	}
}

func TestStoreSSHConfigAtomicallyReplacesPrivateFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_123456789abd"
	path, err := storeSSHConfig(leaseID, "first\n")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := storeSSHConfig(leaseID, "second\n")
	if err != nil {
		t.Fatal(err)
	}
	if updated != path {
		t.Fatalf("updated path=%q want %q", updated, path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second\n" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if err := validatePrivateSSHConfigFile(path); err != nil {
		t.Fatal(err)
	}
}
