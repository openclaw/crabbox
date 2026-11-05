//go:build !windows

package cli

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestSSHSignaledExitCode(t *testing.T) {
	for _, tc := range []struct {
		command string
		code    int
	}{{"kill -TERM $$", 143}, {"kill -KILL $$", 137}, {"exit 23", 23}, {"exit 0", 0}} {
		err := exec.Command("/bin/sh", "-c", tc.command).Run()
		if got := exitCode(err); got != tc.code {
			t.Errorf("%s: exit=%d, want %d", tc.command, got, tc.code)
		}
		if tc.code != 0 && exitCode(errors.Join(err, context.Canceled)) != tc.code {
			t.Errorf("%s: joined cancellation lost exit status", tc.command)
		}
	}
}
