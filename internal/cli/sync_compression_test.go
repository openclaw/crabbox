package cli

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSyncCompressionPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{{"", true}, {"always", true}, {"never", false}} {
		if got := syncCompressionEnabled(tc.mode); got != tc.want {
			t.Fatalf("mode=%q: compression=%t, want %t", tc.mode, got, tc.want)
		}
	}
}

func TestSyncCompressionConfiguration(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if effectiveSyncCompression(cfg) != "always" {
		t.Fatal("default is not always")
	}
	if err := applyFileConfig(&cfg, fileConfig{Sync: &fileSyncConfig{Compression: "never"}}); err != nil {
		t.Fatal(err)
	}
	if effectiveSyncCompression(cfg) != "never" {
		t.Fatal("file override was ignored")
	}
	t.Setenv("CRABBOX_SYNC_COMPRESSION", "always")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if effectiveSyncCompression(cfg) != "always" {
		t.Fatal("environment did not override file")
	}
	for _, mode := range []string{"", "always", "never", "invalid"} {
		cfg.Sync.Compression = mode
		if err := validateSyncCompression(cfg); (err != nil) != (mode == "invalid") {
			t.Fatalf("mode=%q: %v", mode, err)
		}
	}
}

func TestRsyncWorkspaceCompressionPreservesManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "arguments")
	input := filepath.Join(dir, "manifest")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(log) + "\ncat > " + shellQuote(input) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct{ mode, archive string }{{"", "-az"}, {"always", "-az"}, {"never", "-a"}} {
		t.Run(tc.mode, func(t *testing.T) {
			files := []byte("dir/space name\x00dir/newline\nname\x00")
			err := rsync(t.Context(), SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22"}, dir, "/work", []string{"dir"}, io.Discard, io.Discard,
				rsyncOptions{Compression: tc.mode, UseFilesFrom: true, FilesFrom: files, Checksum: true})
			if err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(args), tc.archive+"\n") || !strings.Contains(string(args), "--checksum\n") || !strings.Contains(string(args), "--from0\n") || strings.Contains(string(args), "--exclude\n") {
				t.Fatalf("changed transfer contract: %s", args)
			}
			got, err := os.ReadFile(input)
			if err != nil || string(got) != string(files) {
				t.Fatalf("manifest changed: %q, %v", got, err)
			}
		})
	}
}
