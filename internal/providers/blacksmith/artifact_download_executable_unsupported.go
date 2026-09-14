//go:build !darwin && !linux

package blacksmith

import (
	"errors"
	"os"
)

func openBlacksmithDownloadExecutable(string) (*os.File, error) {
	return nil, errors.New("native artifact download requires macOS or Linux")
}

func checkBlacksmithDownloadCapabilities(*os.File) error {
	return errors.New("native artifact download requires macOS or Linux")
}
