//go:build !linux && !darwin && !windows

package cli

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

func adapterConnectHostSupported() error {
	return fmt.Errorf("adapter connect is not supported on this platform; run the connector on Linux or macOS")
}

func normalizeAdapterUnixSocketPath(string) (string, error) {
	return "", adapterConnectHostSupported()
}

func listenAdapterUnixSocket(string) (net.Listener, func(), error) {
	return nil, nil, adapterConnectHostSupported()
}

func newAdapterLocalClient(string, time.Duration) (*http.Client, error) {
	return nil, adapterConnectHostSupported()
}
