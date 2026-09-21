//go:build !windows

package asciibox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestReleaseLeaseBoundsNativeChildAndRetainsClaim(t *testing.T) {
	b, _, claim, lease := ownedFixture(t)
	cliPath := filepath.Join(t.TempDir(), "box")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	b.rt.Stderr = &output
	withFakeAPI(t, &client{cliPath: cliPath, runner: core.RuntimeForProviderOperation(&output).Exec})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "phase=ownership-check") {
		t.Fatalf("lost cleanup deadline/phase: %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("native child outlived bounded cleanup")
	}
	if !strings.Contains(output.String(), "phase=ownership-check") || !strings.Contains(output.String(), "remaining=") {
		t.Fatalf("missing native progress: %s", output.String())
	}
	assertClaimRetained(t, claim)
}
