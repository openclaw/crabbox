package cli

import (
	"bytes"
	"context"
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Keep real deadline contracts concurrent without sharing their process-wide
// environment. The existing process owner reaps the child and its descendants.
func runParallelCLIContract(t *testing.T, childTimeout time.Duration) bool {
	t.Helper()
	const childEnvironment = "CRABBOX_TEST_PARALLEL_CONTRACT"
	if os.Getenv(childEnvironment) == t.Name() {
		return false
	}
	// Keep coverage counters and focused selection in the original test process,
	// without rewriting Go's slash-separated regular expressions for the child.
	if testing.CoverMode() != "" || testing.Short() ||
		strings.Contains(flag.Lookup("test.run").Value.String(), "/") ||
		flag.Lookup("test.failfast").Value.String() == "true" {
		return false
	}
	for _, name := range []string{"test.skip", "test.cpu", "test.cpuprofile", "test.memprofile", "test.blockprofile", "test.mutexprofile", "test.trace"} {
		if flag.Lookup(name).Value.String() != "" {
			return false
		}
	}
	t.Parallel()
	const cleanupHeadroom = 5 * time.Second
	var deadline time.Time
	if childTimeout > 0 {
		deadline = time.Now().Add(childTimeout + cleanupHeadroom)
	}
	if packageDeadline, ok := t.Deadline(); ok {
		if latest := packageDeadline.Add(-cleanupHeadroom); deadline.IsZero() || latest.Before(deadline) {
			deadline = latest
		}
	}
	ctx := t.Context()
	if !deadline.IsZero() {
		// The package alarm panics instead of canceling t.Context. Give the
		// process owner time to kill/reap before that alarm can end the parent.
		childTimeout = time.Until(deadline) - cleanupHeadroom
		if childTimeout <= 0 {
			t.Fatal("insufficient package deadline remaining to own an isolated contract")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	args := []string{"-test.run=^" + regexp.QuoteMeta(t.Name()) + "$", "-test.v", "-test.count=1"}
	if childTimeout > 0 {
		args = append(args, "-test.timeout="+childTimeout.String())
	}
	owner := pondMeshExecCommand(ctx, nil, os.Args[0], args...).(*pondMeshExecHandle)
	owner.cmd.Env = append(os.Environ(), childEnvironment+"="+t.Name())
	var output bytes.Buffer
	owner.cmd.Stdout, owner.cmd.Stderr = &output, &output
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err != nil {
		t.Fatalf("isolated contract subprocess: %v\n%s", err, output.String())
	}
	t.Log(output.String())
	return true
}
