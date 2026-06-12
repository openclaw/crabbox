package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestPauseResumeCommandsDispatchToPausableBackend(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_PROVIDER", "islo")

	var stderr bytes.Buffer
	app := App{Stdout: io.Discard, Stderr: &stderr}
	if err := app.pause(context.Background(), []string{"swift-crab"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "paused id=swift-crab") {
		t.Fatalf("pause stderr=%q", got)
	}

	stderr.Reset()
	if err := app.resume(context.Background(), []string{"--id", "isb_crabbox-repo-abcdef"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "resumed id=isb_crabbox-repo-abcdef") {
		t.Fatalf("resume stderr=%q", got)
	}
}

func TestPauseResumeCommandsRejectAmbiguousIdentifiers(t *testing.T) {
	clearConfigEnv(t)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			err := app.pauseResume(context.Background(), []string{"--provider", "islo", "--id", "one", "two"}, action)
			if err == nil || !strings.Contains(err.Error(), "usage: crabbox "+action+" --id") {
				t.Fatalf("%s err=%v", action, err)
			}
		})
	}
}

func TestPauseResumeCommandsRejectUnsupportedProvider(t *testing.T) {
	clearConfigEnv(t)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			err := app.pauseResume(context.Background(), []string{"--provider", "e2b", "box_123"}, action)
			if err == nil || !strings.Contains(err.Error(), "provider=e2b does not support "+action) {
				t.Fatalf("%s err=%v", action, err)
			}
		})
	}
}
