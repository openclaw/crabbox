//go:build !windows

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVNCPasswordSSHProductionDeadline(t *testing.T) {
	const childEnvironment = "CRABBOX_TEST_VNC_DEADLINE_CHILD"
	if os.Getenv(childEnvironment) != "1" {
		// Isolate PATH in a child so the real deadline overlaps other parallel
		// timing proofs without mutating their process-wide environment.
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 55*time.Second)
		defer cancel()
		owner := pondMeshExecCommand(ctx, nil, os.Args[0], "-test.run=^TestVNCPasswordSSHProductionDeadline$", "-test.v", "-test.timeout=50s").(*pondMeshExecHandle)
		owner.cmd.Env = append(os.Environ(), childEnvironment+"=1")
		var output bytes.Buffer
		owner.cmd.Stdout, owner.cmd.Stderr = &output, &output
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		if err := owner.Wait(); err != nil {
			t.Fatalf("deadline proof subprocess: %v\n%s", err, output.String())
		}
		t.Log(output.String())
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
