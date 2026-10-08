//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUserConfigDirectoryFollowsOperatingSystemConvention(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	got, err := userConfigDirectory()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("userConfigDirectory()=%q, want the OS convention %q", got, want)
	}
	if runtime.GOOS == "darwin" && got != filepath.Join(home, "Library", "Application Support") {
		t.Fatalf("an XDG variable must not move existing macOS config out of Library/Application Support: %q", got)
	}
}
