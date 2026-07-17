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

	command := `gh codespace ssh -c sturdy --stdio --work-root "/workspaces/my app"`
	rewritten := rewriteProxyCommandGHPath(command, ghPath)
	if !strings.HasPrefix(rewritten, shellQuote(ghPath)+" ") || !strings.Contains(rewritten, `"/workspaces/my app"`) {
		t.Fatalf("proxy=%q", rewritten)
	}
	out, err := exec.Command("sh", "-c", rewritten).CombinedOutput()
	if err != nil {
		t.Fatalf("run proxy: %v: %s", err, out)
	}
	want := "codespace\nssh\n-c\nsturdy\n--stdio\n--work-root\n/workspaces/my app\n"
	if string(out) != want {
		t.Fatalf("args=%q want %q", out, want)
	}
}
