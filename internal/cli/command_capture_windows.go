package cli

import (
	"errors"
	"os"
)

func lockCommandCaptureFile(_ *os.File, _ bool) (bool, error) {
	return false, errors.New("file output capture is unsupported on Windows")
}
