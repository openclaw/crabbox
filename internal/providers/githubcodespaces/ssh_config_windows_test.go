//go:build windows

package githubcodespaces

import "testing"

func TestStoreSSHConfigUsesPrivateWindowsACL(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := storeSSHConfig("cbx_123456789abe", "Host codespace\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSSHConfigFile(path); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteProxyCommandQuotesWindowsExecutable(t *testing.T) {
	got := rewriteProxyCommandGHPath("gh codespace ssh -c sturdy --stdio", `C:\\Program Files\\GitHub CLI\\gh.exe`)
	want := `"C:\\Program Files\\GitHub CLI\\gh.exe" codespace ssh -c sturdy --stdio`
	if got != want {
		t.Fatalf("proxy=%q want %q", got, want)
	}
}

func TestRewriteProxyCommandQuotesWindowsMetacharacters(t *testing.T) {
	got := rewriteProxyCommandGHPath("gh codespace ssh -c sturdy --stdio", `C:\Tools\R&D\gh.exe`)
	want := `"C:\Tools\R&D\gh.exe" codespace ssh -c sturdy --stdio`
	if got != want {
		t.Fatalf("proxy=%q want %q", got, want)
	}
}
