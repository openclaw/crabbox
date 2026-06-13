//go:build !darwin && !linux

package cli

import "fmt"

func stopControllerProcessGroup(processGroupID int) error {
	return fmt.Errorf("controller process-group recovery is unsupported for %d", processGroupID)
}

func controllerProcessGroupAlive(int) bool { return false }
