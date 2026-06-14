//go:build !linux && !darwin && !windows

package cli

import "fmt"

func controllerHostSupported() error {
	return fmt.Errorf("adapter serve is not supported on this platform; run the adapter on Linux or macOS")
}
