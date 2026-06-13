//go:build windows

package cli

import (
	"fmt"
	"os"
)

func openControllerTokenFile(string) (*os.File, error) {
	return nil, fmt.Errorf("controller token files are not supported on Windows")
}
