//go:build !darwin && !linux

package blacksmith

import "errors"

func publishBlacksmithArtifactFile(string, string) (bool, error) {
	return false, errors.New("native artifact publication requires macOS or Linux")
}
