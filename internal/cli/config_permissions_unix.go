//go:build !windows

package cli

import (
	"fmt"
	"os"
)

func configFilePermissionProblemForInfo(_ string, info os.FileInfo) string {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("permissions %04o want 0600", info.Mode().Perm())
	}
	return ""
}
