package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestOpenEditorZedTargetConstraints(t *testing.T) {
	zed := editorHandoffSpecs["zed"]
	tests := []struct {
		name    string
		cfg     Config
		target  SSHTarget
		wantErr string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}},
		{name: "config target", cfg: Config{TargetOS: targetLinux}},
		{name: "windows", target: SSHTarget{TargetOS: targetWindows}, wantErr: "Linux or macOS"},
		{name: "token username", target: SSHTarget{TargetOS: targetLinux, AuthSecret: true}, wantErr: "key-based SSH provider"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateEditorTarget("zed", zed, test.cfg, test.target)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v want exit 2 containing %q", err, test.wantErr)
			}
		})
	}
}

func TestOpenEditorZedSSHCommandLineKeepsExecutableUnquoted(t *testing.T) {
	got := editorSSHCommandLine(SSHTarget{
		User: "alice",
		Host: "203.0.113.10",
		Port: "2222",
		Key:  "/tmp/key with spaces",
	})
	if !strings.HasPrefix(got, "ssh ") {
		t.Fatalf("command=%q should start with an unquoted ssh executable", got)
	}
	for _, want := range []string{"'-i' '/tmp/key with spaces'", "'-p' '2222'", "'alice@203.0.113.10'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command=%q missing %q", got, want)
		}
	}
}

func TestOpenEditorZedHappyPathPrintsInstructions(t *testing.T) {
	fakeEditorSSH(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	out := &cancelBuffer{cancel: cancel}
	cfg := Config{WorkRoot: "/work", TargetOS: targetLinux}
	target := SSHTarget{User: "alice", Host: "example.com", Port: "22", TargetOS: targetLinux}
	resolved := resolvedSSHCommandTarget{Config: cfg, Lease: LeaseTarget{SSH: target}}

	err := (App{Stdout: out, Stderr: &bytes.Buffer{}}).runEditorHandoff(ctx, "zed", editorHandoffSpecs["zed"], resolved)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	folder := mappedRemoteCodeFolder(remoteJoin(cfg, repo.Name), repo)
	for _, want := range []string{
		"Zed Remote Projects",
		"Connect New Server",
		"Paste: ssh ",
		"'alice@example.com'",
		"Open: " + folder,
		"Keep this process running to maintain lease activity",
		"press Ctrl-C",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("instructions missing %q:\n%s", want, out.String())
		}
	}
}

func TestOpenEditorZedMissingSyncedFolder(t *testing.T) {
	fakeEditorSSH(t, 1)
	resolved := resolvedSSHCommandTarget{
		Config: Config{WorkRoot: "/work", TargetOS: targetLinux},
		Lease: LeaseTarget{
			LeaseID: "swift-crab",
			SSH:     SSHTarget{User: "alice", Host: "example.com", Port: "22", TargetOS: targetLinux},
		},
	}
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).runEditorHandoff(
		context.Background(), "zed", editorHandoffSpecs["zed"], resolved,
	)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("error=%v want exit 5", err)
	}
	if !strings.Contains(err.Error(), "crabbox run --id swift-crab --sync-only") {
		t.Fatalf("error=%v missing sync-only hint", err)
	}
}

func TestOpenEditorRejectsUnknownEditor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).open(context.Background(), []string{
		"--editor=vim",
		"--id", "swift-crab",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v want exit 2", err)
	}
	if !strings.Contains(err.Error(), `unknown editor "vim"`) || !strings.Contains(err.Error(), "available editors: zed") {
		t.Fatalf("error=%v missing editor choices", err)
	}
}

type cancelBuffer struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelBuffer) Write(p []byte) (int, error) {
	w.cancel()
	return w.Buffer.Write(p)
}

func fakeEditorSSH(t *testing.T, folderExit int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell script")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *"test -d "*) exit "$CRABBOX_FAKE_FOLDER_EXIT" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_FOLDER_EXIT", strconv.Itoa(folderExit))
}
