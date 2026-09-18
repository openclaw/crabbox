package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UserDirs contains the test-owned directories installed by IsolateUserDirs.
type UserDirs struct {
	Root       string
	Home       string
	ConfigHome string
	StateHome  string
	AppData    string
}

// IsolateUserDirs redirects every user-directory input used by Crabbox and the
// Go standard library to a per-test temporary root.
func IsolateUserDirs(t *testing.T) UserDirs {
	t.Helper()
	dirs := userDirs(t.TempDir())
	if err := prepareUserDirs(dirs); err != nil {
		t.Fatalf("prepare isolated test user directories: %v", err)
	}
	for key, value := range dirs.environment() {
		t.Setenv(key, value)
	}
	assertUserConfigDir(t, dirs.Root)
	return dirs
}

// RunWithIsolatedUserDirs provides a package-wide safety net for tests that
// reach user state without installing per-test isolation first.
func RunWithIsolatedUserDirs(m *testing.M) int {
	if err := preserveGoCacheEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "preserve Go caches before isolating test user directories: %v\n", err)
		return 2
	}
	root, err := os.MkdirTemp("", "crabbox-test-user-dirs-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create isolated test user directories: %v\n", err)
		return 2
	}
	defer os.RemoveAll(root)

	dirs := userDirs(root)
	if err := prepareUserDirs(dirs); err != nil {
		fmt.Fprintf(os.Stderr, "prepare isolated test user directories: %v\n", err)
		return 2
	}
	for key, value := range dirs.environment() {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "set isolated test user directory %s: %v\n", key, err)
			return 2
		}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve isolated test config directory: %v\n", err)
		return 2
	}
	if err := requireWithinRoot(dirs.Root, configDir); err != nil {
		fmt.Fprintf(os.Stderr, "isolated test config directory guard: %v\n", err)
		return 2
	}
	return m.Run()
}

func preserveGoCacheEnv() error {
	if os.Getenv("GOCACHE") == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("resolve Go build cache parent: %w", err)
		}
		if err := os.Setenv("GOCACHE", filepath.Join(cacheDir, "go-build")); err != nil {
			return fmt.Errorf("set GOCACHE: %w", err)
		}
	}
	if os.Getenv("GOMODCACHE") == "" {
		goPath := os.Getenv("GOPATH")
		if goPath == "" {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve Go module cache parent: %w", err)
			}
			goPath = filepath.Join(homeDir, "go")
		}
		goPath = filepath.SplitList(goPath)[0]
		if err := os.Setenv("GOMODCACHE", filepath.Join(goPath, "pkg", "mod")); err != nil {
			return fmt.Errorf("set GOMODCACHE: %w", err)
		}
	}
	return nil
}

func userDirs(root string) UserDirs {
	return UserDirs{
		Root:       root,
		Home:       filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"),
		StateHome:  filepath.Join(root, "state"),
		AppData:    filepath.Join(root, "appdata"),
	}
}

func prepareUserDirs(dirs UserDirs) error {
	paths := []string{
		dirs.Root,
		dirs.Home,
		dirs.ConfigHome,
		dirs.StateHome,
		dirs.AppData,
		filepath.Join(dirs.Root, "local-appdata"),
	}
	for _, path := range paths {
		if path == dirs.StateHome {
			if err := createIsolatedStateHome(path); err != nil {
				return fmt.Errorf("create private state home %q: %w", path, err)
			}
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create %q: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure %q: %w", path, err)
		}
	}
	return nil
}

func (dirs UserDirs) environment() map[string]string {
	return map[string]string{
		"HOME":            dirs.Home,
		"home":            dirs.Home,
		"USERPROFILE":     dirs.Home,
		"XDG_CONFIG_HOME": dirs.ConfigHome,
		"XDG_STATE_HOME":  dirs.StateHome,
		"APPDATA":         dirs.AppData,
		"LOCALAPPDATA":    filepath.Join(dirs.Root, "local-appdata"),
	}
}

func assertUserConfigDir(t *testing.T, root string) {
	t.Helper()
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("resolve isolated test config directory: %v", err)
	}
	if err := requireWithinRoot(root, configDir); err != nil {
		t.Fatal(err)
	}
}

func requireWithinRoot(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("compare test root %q with resolved path %q: %w", root, path, err)
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved user directory %q escapes test root %q", path, root)
	}
	return nil
}
