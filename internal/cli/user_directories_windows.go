//go:build windows

package cli

import "os"

// userConfigDirectory resolves the per-user configuration root.
//
// XDG_CONFIG_HOME is a POSIX convention and is deliberately not consulted on
// Windows, where the platform root is %AppData%. Honoring XDG here would move
// lease and config state off the expected Windows location whenever the
// variable happens to be set in the environment.
func userConfigDirectory() (string, error) {
	return os.UserConfigDir()
}
