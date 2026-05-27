package cli

import (
	"strings"
	"testing"
)

func TestParseFreshPRSpec(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		value  string
		want   string
	}{
		{"number scp remote", "git@github.com:example-org/my-app.git", "123", "example-org/my-app#123"},
		{"number ssh url remote", "ssh://git@github.com/example-org/my-app.git", "123", "example-org/my-app#123"},
		{"number credentialed https remote", "https://user:token@github.com/example-org/my-app.git", "123", "example-org/my-app#123"},
		{"owner repo", "", "example-org/my-app#66278", "example-org/my-app#66278"},
		{"github url", "", "https://github.com/example-org/my-app/pull/66278", "example-org/my-app#66278"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFreshPRSpec(tt.value, Repo{RemoteURL: tt.remote})
			if err != nil {
				t.Fatalf("parse %q: %v", tt.value, err)
			}
			if got.Slug() != tt.want {
				t.Fatalf("parse %q = %q, want %q", tt.value, got.Slug(), tt.want)
			}
		})
	}
}

func TestParseFreshPRSpecRejectsNonGitHubPRURL(t *testing.T) {
	_, err := parseFreshPRSpec("https://ghe.example.com/example-org/my-app/pull/66278", Repo{})
	if err == nil {
		t.Fatal("expected non-github PR URL to fail")
	}
	if !strings.Contains(err.Error(), "host must be github.com") {
		t.Fatalf("error=%v", err)
	}
}

func TestRemoteFreshPRCheckoutCommand(t *testing.T) {
	got := remoteFreshPRCheckoutCommand("/work/cbx/fresh-pr-example-org-my-app-66278", FreshPRSpec{
		Owner:  "example-org",
		Repo:   "my-app",
		Number: 66278,
	})
	for _, want := range []string{
		"git clone --quiet --filter=blob:none",
		"https://github.com/example-org/my-app.git",
		"git fetch --quiet origin",
		"pull/66278/head:crabbox-pr-66278",
		"git checkout --quiet",
		"crabbox-pr-66278",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("checkout command missing %q in %q", want, got)
		}
	}
}

func TestWindowsRemoteFreshPRCheckoutCommand(t *testing.T) {
	got := remoteFreshPRCheckoutCommandForTarget(`C:\crabbox\cbx\fresh-pr-example-org-my-app-66278`, FreshPRSpec{
		Owner:  "example-org",
		Repo:   "my-app",
		Number: 66278,
	}, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		"Remove-Item -LiteralPath $workdir -Recurse -Force",
		"git clone --quiet --filter=blob:none",
		"https://github.com/example-org/my-app.git",
		"git fetch --quiet origin",
		"pull/66278/head:crabbox-pr-66278",
		"git checkout --quiet",
		"crabbox-pr-66278",
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("checkout command missing %q in %q", want, decoded)
		}
	}
}

func TestWindowsRemoteApplyLocalPatchCommand(t *testing.T) {
	got := remoteApplyLocalPatchCommandForTarget(`C:\crabbox\cbx\fresh-pr-example-org-my-app-66278`, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\cbx\fresh-pr-example-org-my-app-66278'`,
		"git apply --whitespace=nowarn -",
		"git apply failed with exit $LASTEXITCODE",
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("apply-patch command missing %q in %q", want, decoded)
		}
	}
}
