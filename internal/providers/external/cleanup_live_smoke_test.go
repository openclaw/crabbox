//go:build smoke

package external

// Instrumented LIVE smoke for the external-provider Cleanup orphan sweep, opt-in
// behind the `smoke` build tag and CRABBOX_LIVE=1. Unlike the hermetic tests in
// cleanup_toctou_test.go (which return canned JSON from an in-process fake), this
// drives a REAL external helper subprocess that implements the versioned JSON
// protocol on stdin/stdout, plus a genuine routing file on disk, through the real
// Cleanup code path. It proves the availability fix against real behavior: a
// genuine missing-lease orphan claim is removed while the external routing a
// concurrent Acquire persisted before publishing its claim is RETAINED.
//
// Isolation: isolateCrabboxState redirects HOME/XDG_CONFIG_HOME/XDG_STATE_HOME to
// temp dirs, so the claim and the routing file never touch the developer's real
// config.
//
// Run: CRABBOX_LIVE=1 go test -tags smoke -run TestLiveExternal -v ./internal/providers/external/

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// liveExternalRunner execs the real configured external helper command, piping the
// protocol request in on stdin exactly as the production invoke path does.
type liveExternalRunner struct{}

func (liveExternalRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = append(os.Environ(), req.Env...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if req.Stdin != nil {
		cmd.Stdin = req.Stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if req.Stderr != nil {
		cmd.Stderr = io.MultiWriter(req.Stderr, &stderr)
	} else {
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	res := core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exit.ExitCode()
		return res, fmt.Errorf("%s: exit %d: %s", req.Name, exit.ExitCode(), stderr.String())
	}
	return res, err
}

func TestLiveExternalCleanupRetainsRoutingForConcurrentAcquire(t *testing.T) {
	if os.Getenv("CRABBOX_LIVE") != "1" {
		t.Skip("set CRABBOX_LIVE=1 to run the live external-provider cleanup smoke")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the external helper subprocess")
	}
	isolateCrabboxState(t)
	repoRoot := t.TempDir()

	// A REAL external helper subprocess implementing the JSON protocol: it reads the
	// request from stdin, logs the operation to stderr, and returns an empty lease
	// list (so the pre-existing claim is a genuine missing-lease orphan).
	helper := filepath.Join(t.TempDir(), "external-helper.sh")
	script := "#!/usr/bin/env bash\n" +
		"set -eu\n" +
		"req=\"$(cat)\"\n" +
		"op=\"$(printf '%s' \"$req\" | sed -n 's/.*\"operation\":\"\\([a-z]*\\)\".*/\\1/p')\"\n" +
		"printf 'external-helper: handled protocol operation=%s (returning 0 live leases)\\n' \"$op\" >&2\n" +
		"printf '%s' '{\"protocolVersion\":1,\"leases\":[]}'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	const (
		lease = "cbx_liveextrouting01"
		slug  = "liveext-routing"
	)
	cfg := testConfig()
	cfg.External.Command = helper
	cfg.External.Args = nil

	var out bytes.Buffer
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: liveExternalRunner{}, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatalf("configure external backend: %v", err)
	}
	b := backend.(*leaseBackend)
	scope := b.claimScope()
	if scope == "" {
		t.Fatalf("test setup: external scope resolved empty")
	}

	// A genuine orphan claim (its lease is absent from the helper's live "list"),
	// unchanged during Cleanup, so the guarded sweep legitimately removes the CLAIM.
	server := core.Server{
		CloudID: "provider/node-liveorphan", Provider: providerName, Name: "crabbox-liveorphan", Status: "idle",
		Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": lease, "slug": slug, "state": "idle"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		lease, slug, providerName, scope, "", repoRoot, 30*time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("register orphan claim: %v", err)
	}

	// A concurrent Acquire of the SAME lease persisted its routing (a real file on
	// disk) before publishing a replacement claim.
	routingPath, err := core.PersistExternalRouting(lease, cfg.External)
	if err != nil {
		t.Fatalf("persist routing: %v", err)
	}
	if _, err := os.Stat(routingPath); err != nil {
		t.Fatalf("routing file was not created: %v", err)
	}

	// Run the real Cleanup: it invokes the real helper subprocess for the protocol
	// "cleanup" and "list" operations.
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// The genuine orphan claim is removed...
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease, providerName); err != nil {
		t.Fatalf("ResolveLeaseClaimForProvider: %v", err)
	} else if ok {
		t.Fatalf("LIVE: expected the genuine orphan claim to be removed, but it survived\ncleanup output:\n%s", out.String())
	}
	// ...but the routing persisted by the concurrent Acquire MUST survive.
	if _, err := os.Stat(routingPath); err != nil {
		t.Fatalf("LIVE: cleanup deleted external routing persisted by a concurrent Acquire (availability regression): %v\ncleanup output:\n%s", err, out.String())
	}
	t.Logf("LIVE PROOF (real external helper subprocess + real routing file): genuine orphan claim removed, external routing RETAINED for the concurrent Acquire:\n%s", out.String())
}
