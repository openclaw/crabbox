package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestPauseResumeCommandsRouteOrdinaryClaims(t *testing.T) {
	t.Run("claim provider without ambient config", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
		if err := ClaimLeaseForRepoProvider("isb_1335_pause", "Pause Claimed", "islo", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if err := (App{Stdout: io.Discard, Stderr: &stderr}).pause(context.Background(), []string{"pause-claimed"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr.String(), "paused id=pause-claimed") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("explicit provider wins", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
		if err := ClaimLeaseForRepoProvider("isb_1335_resume", "Resume Claimed", "e2b", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if err := (App{Stdout: io.Discard, Stderr: &stderr}).resume(context.Background(), []string{"--provider", "islo", "resume-claimed"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr.String(), "resumed id=resume-claimed") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
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

type testPausableSSHProvider struct{}

func (testPausableSSHProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "pausable-ssh-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeaturePauseResume},
		Coordinator: CoordinatorNever,
	}
}
func (testPausableSSHProvider) RegisterFlags(*flag.FlagSet, Config) any { return nil }
func (testPausableSSHProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p testPausableSSHProvider) Configure(Config, Runtime) (Backend, error) {
	return testPausableSSHBackend{testSSHBackend: testSSHBackend{spec: p.Spec()}}, nil
}

// testPausableSSHBackend pauses but has no Status method, like machine0 and
// hyperv: the --json receipt must come from the shared lease status path.
type testPausableSSHBackend struct{ testSSHBackend }

func (testPausableSSHBackend) Pause(context.Context, PauseRequest) error   { return nil }
func (testPausableSSHBackend) Resume(context.Context, ResumeRequest) error { return nil }
func (b testPausableSSHBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{LeaseID: req.ID, Server: Server{Provider: b.spec.Name, CloudID: "vm-1", Status: "paused", Labels: map[string]string{"lease": req.ID}}}, nil
}

func init() { RegisterProvider(testPausableSSHProvider{}) }

func TestPauseResumeJSONEmitsStatusReceiptWithoutProviderStatusMethod(t *testing.T) {
	clearConfigEnv(t)
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.pauseResume(context.Background(), []string{"--provider", "pausable-ssh-test", "--id", "cbx_json", "--json"}, action); err != nil {
				t.Fatalf("%s --json: %v", action, err)
			}
			var view StatusView
			if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
				t.Fatalf("receipt is not JSON: %v: %q", err, stdout.String())
			}
			if view.ID != "cbx_json" || view.Provider != "pausable-ssh-test" || view.State != "paused" {
				t.Fatalf("receipt=%+v want id/provider/state paused", view)
			}
		})
	}
}
