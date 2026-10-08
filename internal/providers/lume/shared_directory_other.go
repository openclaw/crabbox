//go:build !windows

package lume

import (
	"fmt"
	"strings"
)

func validateLumeSharedDirectoryPath(path string) error {
	if strings.Contains(path, ":") {
		return fmt.Errorf("cannot contain a colon")
	}
	return nil
}
