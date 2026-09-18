//go:build !windows

package githubcodespaces

import (
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
)

func securePrivateSSHConfigFile(path string) error {
	return os.Chmod(path, defaultSSHConfigFileMode)
}

func validatePrivateSSHConfigPermissions(path string, info os.FileInfo) error {
	mode := info.Mode().Perm()
	if mode != defaultSSHConfigFileMode {
		return core.Exit(2, "github-codespaces SSH config path %q must have mode 0600, got %04o", path, mode)
	}
	return nil
}

func replaceSSHConfigFile(tmpPath, path string) error {
	return os.Rename(tmpPath, path)
}

func syncSSHConfigDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func quoteSSHProxyExecutable(path string) string {
	return core.ShellQuote(path)
}
