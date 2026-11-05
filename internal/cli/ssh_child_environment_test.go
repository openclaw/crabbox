package cli

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSSHCommandsResetInheritedWindowsDescriptors(t *testing.T) {
	t.Setenv("OPENSSH_STDIO_MODE", "sock")
	t.Setenv(windowsOpenSSHInheritedFDState, "parent-descriptors")
	t.Setenv("CRABBOX_TEST_CHILD_DENIED", "private")
	target := SSHTarget{
		ChildEnvDenylist: []string{"CRABBOX_TEST_CHILD_DENIED"},
		ChildEnv:         map[string]string{"CRABBOX_TEST_CHILD_KEEP": "target", "OPENSSH_STDIO_MODE": "sock"},
	}
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "owned"}[owned], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var cmd *exec.Cmd
			if owned {
				cmd = newOwnedSSHTransportCommand(ctx, target, []string{"-V"}).cmd
			} else {
				cmd = sshCommandContext(ctx, target, "-V")
			}
			env := map[string]string{}
			for _, entry := range cmd.Environ() {
				name, value, _ := strings.Cut(entry, "=")
				env[strings.ToUpper(name)] = value
			}
			if _, ok := env["CRABBOX_TEST_CHILD_DENIED"]; ok {
				t.Fatal("target denylist lost")
			}
			if env["CRABBOX_TEST_CHILD_KEEP"] != "target" {
				t.Fatal("target override lost")
			}
			if runtime.GOOS != "windows" {
				if env["OPENSSH_STDIO_MODE"] != "sock" || env[strings.ToUpper(windowsOpenSSHInheritedFDState)] != "parent-descriptors" {
					t.Fatal("changed non-Windows child environment")
				}
				return
			}
			if env["OPENSSH_STDIO_MODE"] != "nonsock_sync" {
				t.Fatal("native SSH must use Go's synchronous handles")
			}
			if _, ok := env[strings.ToUpper(windowsOpenSSHInheritedFDState)]; ok {
				t.Fatal("native SSH inherited stale parent descriptors")
			}
			if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "OpenSSH") {
				t.Fatalf("native SSH startup: %v: %s", err, out)
			}
		})
	}
}
