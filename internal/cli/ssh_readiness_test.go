package cli

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSSHReadinessProbeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell exit-status fixture")
	}
	for _, test := range []struct {
		name       string
		code       int
		diagnostic string
		canceled   bool
		permanent  bool
	}{
		{name: "changed host key", code: 255, diagnostic: "private-key-data\nHost key verification failed.\r\n", permanent: true},
		{name: "authentication pending", code: 255, diagnostic: "Permission denied (publickey)."},
		{name: "connection pending", code: 255, diagnostic: "Connection refused"},
		{name: "toolchain pending", code: 1, diagnostic: "command not found"},
		{name: "nontransport failure", code: 1, diagnostic: "Host key verification failed."},
		{name: "success", diagnostic: "Host key verification failed."},
		{name: "canceled", code: 255, diagnostic: "Host key verification failed.", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			var cause error
			if test.code != 0 {
				cause = exec.Command("sh", "-c", "exit "+strconv.Itoa(test.code)).Run()
				if exitCode(cause) != test.code {
					t.Fatalf("fixture exit status: %v", cause)
				}
			}
			err := sshReadinessProbeError(ctx, cause, test.diagnostic)
			if errors.Is(err, errSSHHostKeyVerification) != test.permanent || !errors.Is(err, cause) {
				t.Fatalf("incorrect classification or lost cause: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-key-data") {
				t.Fatal("captured diagnostic leaked into error")
			}
			if !test.permanent && err != cause {
				t.Fatalf("ordinary probe result changed: %v", err)
			}
		})
	}
}
