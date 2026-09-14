package shared

import (
	"net/http"
	"time"
)

// ControlAndDataHTTPClients gives default control requests a whole-request
// timeout while data requests use their operation contexts. An injected client
// overrides both planes unchanged, including its timeout and redirect policy.
func ControlAndDataHTTPClients(injected *http.Client, controlTimeout time.Duration) (*http.Client, *http.Client) {
	if injected != nil {
		return injected, injected
	}
	return &http.Client{Timeout: controlTimeout}, &http.Client{}
}
