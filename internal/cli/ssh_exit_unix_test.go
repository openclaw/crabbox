//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSSHSignaledExitCode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		command   string
		code, raw int
	}{{"kill -TERM $$", 143, -1}, {"kill -KILL $$", 137, -1}, {"exit 23", 23, 23}, {"exit 0", 0, 0}} {
		err := exec.Command("/bin/sh", "-c", tc.command).Run()
		if got := exitCode(err); got != tc.raw {
			t.Errorf("%s: local exit=%d, want %d", tc.command, got, tc.raw)
		}
		if tc.code != 0 && exitCode(errors.Join(err, context.Canceled)) != tc.raw {
			t.Errorf("%s: joined cancellation lost exit status", tc.command)
		}
		writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\n"+tc.command+"\n")
		target := SSHTarget{Host: "fixture.invalid", Port: "22", NoControlMaster: true, FallbackPorts: []string{}}
		if got, _ := runSSHStreamResult(t.Context(), target, "true", io.Discard, io.Discard); got != tc.code {
			t.Errorf("%s: SSH stream exit=%d, want %d", tc.command, got, tc.code)
		}
	}
}
