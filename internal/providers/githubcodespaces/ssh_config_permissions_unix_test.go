//go:build !windows

package githubcodespaces

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePrivateSSHConfigFileRequires0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("Host sturdy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSSHConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSSHConfigFile(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("err=%v", err)
	}
}

func TestRewriteProxyCommandPreservesSpaceContainingArgument(t *testing.T) {
	binDir := filepath.Join(t.TempDir(), "GitHub CLI")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	identityFile := filepath.Join(t.TempDir(), "my codespace ssh key", "codespaces.auto")
	command := ghPath + ` cs ssh -c sturdy --stdio -- -i ` + identityFile
	rewritten := rewriteProxyCommandIdentityFile(
		rewriteProxyCommandGHPath(command, defaultGHPath),
		identityFile,
	)
	if !strings.HasPrefix(rewritten, shellQuote(ghPath)+" ") || !strings.HasSuffix(rewritten, shellQuote(identityFile)) {
		t.Fatalf("proxy=%q", rewritten)
	}
	out, err := exec.Command("sh", "-c", rewritten).CombinedOutput()
	if err != nil {
		t.Fatalf("run proxy: %v: %s", err, out)
	}
	want := "cs\nssh\n-c\nsturdy\n--stdio\n--\n-i\n" + identityFile + "\n"
	if string(out) != want {
		t.Fatalf("args=%q want %q", out, want)
	}
}
