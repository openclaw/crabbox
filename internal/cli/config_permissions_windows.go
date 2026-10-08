//go:build windows

package cli

import "os"

func configFilePermissionProblemForInfo(path string, _ os.FileInfo) string {
	if err := verifySSHTransportPathPrivate(path, false); err != nil {
		return err.Error()
	}
	return ""
}
