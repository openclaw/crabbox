//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVNCPasswordSSHProductionDeadline(t *testing.T) {
	if runParallelCLIContract(t, 50*time.Second) {
		return
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec sleep 120\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := SSHTarget{Host: "fixture.invalid", User: "tester", Port: "22", TargetOS: targetLinux, NoControlMaster: true}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	start := time.Now()
	out, err := runVNCPasswordSSH(ctx, target, remoteVNCCredentialReadCommand(target))
	if err == nil || out != "" || ctx.Err() != nil || time.Since(start) < 29*time.Second {
		t.Fatalf("read deadline lost: err=%v empty=%t caller=%v elapsed=%s", err, out == "", ctx.Err(), time.Since(start))
	}
	t.Logf("stalled SSH process reaped after %s with caller deadline still live", time.Since(start))
}
