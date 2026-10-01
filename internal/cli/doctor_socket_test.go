package cli

import (
	"errors"
	"testing"
)

func TestDoctorMissingUnixSocketIsNetworkFailure(t *testing.T) {
	for _, tt := range []struct {
		name    string
		message string
		class   string
		hint    string
	}{
		{
			name:    "missing daemon socket",
			message: "container list failed: exit status 1: failed to connect to the docker API at unix:///tmp/runtime/docker.sock: dial unix /tmp/runtime/docker.sock: connect: no such file or directory",
			class:   "network",
			hint:    "check_network_and_provider_endpoint",
		},
		{
			name:    "missing executable in PATH",
			message: `container list failed: exec: "docker": executable file not found in $PATH`,
			class:   "tool",
			hint:    "install_provider_cli",
		},
		{
			name:    "missing executable at explicit path",
			message: "container list failed: fork/exec /opt/runtime/docker: no such file or directory",
			class:   "tool",
			hint:    "install_provider_cli",
		},
		{
			name:    "socket permission denied",
			message: "dial unix /tmp/runtime/docker.sock: connect: permission denied",
			class:   "auth",
			hint:    "check_provider_auth_and_config",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			class := doctorErrorClass(errors.New(tt.message))
			if class != tt.class {
				t.Errorf("class = %q, want %q", class, tt.class)
			}
			if hint := doctorErrorHint("local-container", class); hint != tt.hint {
				t.Errorf("hint = %q, want %q", hint, tt.hint)
			}
		})
	}
}
