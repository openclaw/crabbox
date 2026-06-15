package cli

import "os"

const (
	privateRunOutputDirMode  = 0o700
	privateRunOutputFileMode = 0o600
)

func createPrivateRunOutputDir(path string) error {
	return os.MkdirAll(path, privateRunOutputDirMode)
}
