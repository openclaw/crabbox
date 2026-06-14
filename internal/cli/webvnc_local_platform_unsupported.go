//go:build !darwin && !linux

package cli

import "fmt"

func localWebVNCSupported() bool { return false }

func localWebVNCListenerIdentity(string) (localWebVNCSourceIdentity, error) {
	return localWebVNCSourceIdentity{}, fmt.Errorf("local WebVNC listener identity is unsupported on this platform")
}
