//go:build windows

package cli

import "fmt"

func controllerHostSupported() error {
	return fmt.Errorf("adapter serve is not supported on Windows; run the adapter on Linux or macOS")
}
