//go:build !windows

package cli

import (
	"os"
)

// userConfigDirectory resolves the per-user configuration root. It deliberately
// defers to the operating system's convention: Linux honors XDG_CONFIG_HOME, and
// macOS keeps ~/Library/Application Support so existing credentials and lease
// identities are not stranded by an unrelated XDG variable.
func userConfigDirectory() (string, error) {
	return os.UserConfigDir()
}
