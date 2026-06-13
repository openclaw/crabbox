//go:build !windows

package cli

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestControllerOwnedCapturedCommandKeepsOuterProcessGroup(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "1")
	result, err := (execCommandRunner{}).Run(context.Background(), LocalCommandRequest{
		Name:                   "sh",
		Args:                   []string{"-c", "ps -o pgid= -p $$; test -z \"$" + controllerProcessTreeOwnedEnv + "\""},
		Env:                    os.Environ(),
		MaxCapturedOutputBytes: 1024,
	})
	if err != nil {
		t.Fatalf("controller-owned provider command: %v stderr=%q", err, result.Stderr)
	}
	processGroup, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		t.Fatalf("provider process group %q: %v", result.Stdout, err)
	}
	if processGroup != syscall.Getpgrp() {
		t.Fatalf("provider process group=%d outer=%d", processGroup, syscall.Getpgrp())
	}
}
