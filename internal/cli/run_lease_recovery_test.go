package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type runRecoveryTestProvider struct{ runEnvProfileTestProvider }

func (runRecoveryTestProvider) Spec() ProviderSpec {
	spec := runEnvProfileTestProvider{}.Spec()
	spec.Name = "run-recovery-test"
	return spec
}

func (p runRecoveryTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runEnvProfileTestBackend{spec: p.Spec()}, nil
}

func (runRecoveryTestProvider) CommandRouting(Config, CommandRoutingRequest) CommandRouting {
	return CommandRouting{Args: []string{"--region", "test-region"}}
}

type runRecoveryOwnerTransport struct {
	failure string
}

func (runRecoveryOwnerTransport) CallBudget() time.Duration { return 20 * time.Millisecond }

func (r runRecoveryOwnerTransport) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	if req.Action == workspaceOwnerRelease {
		return "RELEASED", nil
	}
	switch r.failure {
	case "ambiguous":
		return "AMBIGUOUS", nil
	case "timeout":
		<-ctx.Done()
		return "", ctx.Err()
	default:
		return "OWNED", nil
	}
}

func TestRunLeaseRecoveryGuidance(t *testing.T) {
	RegisterProvider(runRecoveryTestProvider{})
	for _, tc := range []struct {
		name         string
		ownerFailure string
		acquireErr   error
		releaseErr   error
		flags        []string
		retained     bool
		wantReason   string
		wantReleases int
	}{
		{name: "ambiguous child", ownerFailure: "ambiguous", wantReason: "workspace owner", wantReleases: 0},
		{name: "child confirmation timeout", ownerFailure: "timeout", wantReason: "context deadline exceeded", wantReleases: 0},
		{name: "setup timeout with kept lease", acquireErr: context.DeadlineExceeded, flags: []string{"--keep"}, wantReason: "--keep", wantReleases: 0},
		{name: "interrupted release", releaseErr: context.Canceled, wantReason: "context canceled", wantReleases: 1},
		{name: "release timeout", releaseErr: context.DeadlineExceeded, wantReason: "context deadline exceeded", wantReleases: 1},
		{name: "provider retained lease", retained: true, wantReason: "provider did not confirm", wantReleases: 1},
		{name: "kept", flags: []string{"--keep"}, wantReason: "--keep", wantReleases: 0},
		{name: "reused", flags: []string{"--id", "cbx_env_profile_test"}, wantReason: "existing lease", wantReleases: 0},
		{name: "stop policy", flags: []string{"--stop-after", "never"}, wantReason: "--stop-after never", wantReleases: 0},
		{name: "successful one shot", wantReleases: 1},
	} {
		for _, timing := range []bool{false, true} {
			t.Run(tc.name+"/timing="+strconv.FormatBool(timing), func(t *testing.T) {
				dir := setupRunCleanupWorkspaceOwnerTest(t)
				// Exercise the same fresh-lease owner path as nonexclusive providers.
				runEnvProfileTestCloseBeforeRelease = true
				t.Cleanup(func() { runEnvProfileTestCloseBeforeRelease = false })
				runEnvProfileTestRetainsLease = tc.retained
				releases := 0
				runEnvProfileTestReleaseHook = func() error { releases++; return tc.releaseErr }
				var stdout, stderr bytes.Buffer
				app := App{Stdout: &stdout, Stderr: &stderr, workspaceOwnerAcquirer: func(ctx context.Context, target SSHTarget, leaseID string, _ io.Writer) (*workspaceOwner, error) {
					if tc.acquireErr != nil {
						return nil, tc.acquireErr
					}
					ownerCtx, cancel := context.WithCancel(ctx)
					done := make(chan struct{})
					close(done)
					return &workspaceOwner{
						target: target, transport: runRecoveryOwnerTransport{tc.ownerFailure},
						key: workspaceOwnerKey(leaseID), token: strings.Repeat("a", 64), ttl: time.Minute,
						ctx: ownerCtx, cancel: cancel, stop: make(chan struct{}), done: done,
					}, nil
				}}
				args := []string{"--provider", "run-recovery-test", "--target", "linux", "--no-sync", "--no-hydrate", "--timing-record", "off"}
				args = append(args, tc.flags...)
				recordPath := filepath.Join(dir, "timing.jsonl")
				if timing {
					args = append(args, "--timing-json", "--timing-record", recordPath)
				}
				args = append(args, "--", "renewal-cleanup-success")
				err := app.runCommand(t.Context(), args)
				out := stderr.String()
				// Timing mode reports provider release failures in leaseStopError.
				wantFailure := tc.ownerFailure != "" || tc.acquireErr != nil || (tc.releaseErr != nil && !timing)
				if (err != nil) != wantFailure {
					t.Fatalf("err=%v wantFailure=%t\n%s", err, wantFailure, out)
				}
				if tc.ownerFailure != "" {
					assertRunCleanupExitCode(t, err, 7, stdout.String(), out)
					if !strings.Contains(err.Error(), "refusing collection and cleanup") {
						t.Errorf("post-command ownership refusal missing: %v", err)
					}
				}
				if releases != tc.wantReleases {
					t.Errorf("release calls=%d want=%d", releases, tc.wantReleases)
				}
				const stop = "crabbox stop --provider run-recovery-test --target linux --region test-region --id cbx_env_profile_test"
				if tc.wantReason != "" {
					var recovery string
					for _, line := range strings.Split(out, "\n") {
						if strings.HasPrefix(line, "lease recovery ") {
							recovery = line
						}
					}
					if !strings.Contains(recovery, "stop_command="+strconv.Quote(stop)) || !strings.Contains(recovery, tc.wantReason) {
						t.Errorf("missing exact stop command and reason in recovery line %q\n%s", recovery, out)
					}
				} else if strings.Contains(out, "lease recovery ") || !strings.Contains(out, "run details ") {
					t.Errorf("successful cleanup reporting changed:\n%s", out)
				}
				if timing {
					lines := strings.Split(strings.TrimSpace(out), "\n")
					var report TimingReport
					if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
						t.Fatalf("final stderr line must be timing JSON: %v\n%s", err, out)
					}
					if report.StopCommand != stop || !strings.Contains(report.LeaseRetainedReason, tc.wantReason) || (tc.wantReason == "" && report.LeaseRetainedReason != "") {
						t.Errorf("timing recovery differs: %+v", report)
					}
					records, err := readBenchmarkTimingRecords(recordPath)
					if err != nil || len(records) != 1 {
						t.Fatalf("timing records=%d err=%v", len(records), err)
					}
					if recorded := records[0].Timing; recorded.StopCommand != stop || recorded.LeaseRetainedReason != report.LeaseRetainedReason {
						t.Errorf("stored timing recovery differs: %+v", recorded)
					}
				}
				if tc.acquireErr != nil && !errors.Is(err, tc.acquireErr) {
					t.Errorf("setup failure changed: %v", err)
				}
			})
		}
	}
}
