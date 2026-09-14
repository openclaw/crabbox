//go:build !darwin && !linux

package cli

import "fmt"

func localCommandProcessGroup() int { return 0 }

func observeLocalCommandLeader(int, int) (bool, error) {
	return false, fmt.Errorf("observing an unreaped local command requires macOS or Linux")
}
