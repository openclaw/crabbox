//go:build windows

package cli

func replaceClaimFile(tmpPath, path string) error {
	return replaceControllerFile(tmpPath, path)
}
