//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHCommandContextUsesNativeWindowsOpenSSHEvenWhenPATHSelectsMSYS2(t *testing.T) {
	windowsDir := t.TempDir()
	nativeSSH := filepath.Join(windowsDir, "System32", "OpenSSH", "ssh.exe")
	if err := os.MkdirAll(filepath.Dir(nativeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeSSH, []byte("native fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	msysDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(msysDir, "ssh.exe"), []byte("MSYS2 fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINDIR", windowsDir)
	t.Setenv("PATH", msysDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := sshCommandContext(context.Background(), SSHTarget{}, "-V")
	if cmd.Path != nativeSSH {
		t.Fatalf("direct SSH executable=%q, want %q", cmd.Path, nativeSSH)
	}
}

func TestProxyJumpDirectCommandUsesNativeWindowsOpenSSH(t *testing.T) {
	windowsDir := t.TempDir()
	nativeSSH := filepath.Join(windowsDir, "System32", "OpenSSH", "ssh.exe")
	if err := os.MkdirAll(filepath.Dir(nativeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeSSH, []byte("native fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINDIR", windowsDir)

	command := proxyJumpDirectCommand(directSSHExecutable(), `C:\private\jump config`, "jump.example.test")
	wantPrefix := sshProxyCommandWords([]string{nativeSSH})[0] + " "
	if !strings.HasPrefix(command, wantPrefix) {
		t.Fatalf("command=%q, want native prefix %q", command, wantPrefix)
	}
}

func TestSSHProxyCommandWordsUseWindowsQuoting(t *testing.T) {
	got := sshProxyCommandWords([]string{"ssh", "-F", `C:\Users\O'Brien\ssh config`})
	want := []string{`"ssh"`, `"-F"`, `"C:\Users\O'Brien\ssh config"`}
	if len(got) != len(want) {
		t.Fatalf("words=%#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("word %d=%q, want %q", index, got[index], want[index])
		}
	}
}
