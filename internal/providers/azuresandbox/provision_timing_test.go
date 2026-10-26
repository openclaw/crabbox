package azuresandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProvisionTimingUsesNonOverlappingClientDurations(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timing := &sandboxProvisionTiming{now: func() time.Time { return now }}
	ctx := context.WithValue(t.Context(), sandboxProvisionTimingKey{}, timing)
	for _, step := range []struct {
		name    string
		elapsed time.Duration
	}{{"credential", 3 * time.Millisecond}, {"create", 7 * time.Millisecond}, {"inspect", 2 * time.Millisecond}, {"poll_wait.creating", time.Second}, {"inspect", 4 * time.Millisecond}} {
		finish := measureSandboxProvisionStep(ctx, step.name)
		now = now.Add(step.elapsed)
		finish()
	}
	var sum int64
	for _, p := range timing.phases() {
		sum += p.Ms
	}
	if sum != 1016 {
		t.Fatalf("partition = %d", sum)
	}
	events := timing.events()
	if len(events) != 4 || events[2].Count != 2 || *events[2].DurationMs != 6 {
		t.Fatalf("events=%+v", events)
	}
	if !timing.phases()[1].Opaque || !timing.phases()[3].Opaque {
		t.Fatal("unobservable service phases presented as exact")
	}
}

func TestWarmupProvisionTimingSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "uncertain-create"}[fail], func(t *testing.T) {
			f := &fixture{loseCreateResponse: fail}
			b, req := fixtureBackend(t, f)
			var stderr bytes.Buffer
			b.rt.Stderr = &stderr
			req.TimingJSON = true
			err := b.WarmupFixed(t.Context(), req)
			if (err != nil) != fail {
				t.Fatalf("err=%v", err)
			}
			var report core.TimingReport
			if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &report); err != nil {
				t.Fatal(err)
			}
			if report.Provider != providerName || (!fail && report.LeaseID != req.RequestedLeaseID) || (fail && report.LeaseID != "") || (report.ExitCode != 0) != fail {
				t.Fatalf("outcome=%+v", report)
			}
			if fail && (report.RunStatus != core.RunStatusFailed || report.ErrorKind != core.RunErrorProvider) {
				t.Fatalf("provisioning failure misclassified: %+v", report)
			}
			counts := map[string]int64{}
			for _, e := range report.CreationEvents {
				counts[e.Step] = e.Count
			}
			if counts["provider.sandbox.credential"] == 0 || counts["provider.sandbox.create"] != 1 || counts["provider.sandbox.list"] == 0 {
				t.Fatalf("counts=%v", counts)
			}
			if strings.Contains(stderr.String(), "synthetic-token") || strings.Contains(stderr.String(), "unique-resource") {
				t.Fatal("private transport data leaked")
			}
			if f.creates != 1 {
				t.Fatalf("timing changed mutation count: %d", f.creates)
			}
		})
	}
}

func TestWarmupProvisionTimingInterruptedPoll(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "timed-out"}[deadline], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fixture{}
				b, req := fixtureBackend(t, f)
				var stdout, stderr bytes.Buffer
				b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
				req.TimingJSON = true
				var ctx context.Context
				var cancel context.CancelFunc
				if deadline {
					ctx, cancel = context.WithTimeout(t.Context(), 1500*time.Millisecond)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
					time.AfterFunc(1500*time.Millisecond, cancel)
				}
				defer cancel()
				b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if f.box != nil {
						f.box.State = "Creating"
					}
					return f.request(t, r)
				})
				err := b.WarmupFixed(ctx, req)
				wantErr := context.Canceled
				wantStatus, wantKind := core.RunStatusCanceled, core.RunErrorCanceled
				if deadline {
					wantErr = context.DeadlineExceeded
					wantStatus, wantKind = core.RunStatusTimedOut, core.RunErrorTimeout
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("err=%v, want %v", err, wantErr)
				}
				var report core.TimingReport
				if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &report); err != nil {
					t.Fatal(err)
				}
				if report.ExitCode != 1 || report.RunStatus != wantStatus || report.ErrorKind != wantKind {
					t.Fatalf("interrupted provisioning misclassified: %+v", report)
				}
				var poll *core.CreationEvent
				for i := range report.CreationEvents {
					if report.CreationEvents[i].Step == "provider.sandbox.poll_wait.creating" {
						poll = &report.CreationEvents[i]
					}
				}
				if poll == nil || poll.Count != 2 || poll.DurationMs == nil || *poll.DurationMs != 1500 {
					t.Fatalf("interrupted polling not retained: %+v", poll)
				}
				if stdout.Len() != 0 || report.LeaseID != "" || report.CreationRejected != nil || report.LeaseStopped != nil || f.creates != 1 || f.deletes != 0 {
					t.Fatalf("timing changed custody or claimed success: stdout=%s report=%+v creates=%d deletes=%d", stdout.String(), report, f.creates, f.deletes)
				}
			})
		})
	}
}

func TestWarmupProvisionTimingCredentialFailures(t *testing.T) {
	for _, outcome := range []string{"provider-error", "canceled", "timed-out"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fixture{}
				b, req := fixtureBackend(t, f)
				var stderr bytes.Buffer
				b.rt.Stderr = &stderr
				req.TimingJSON = true
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				b.api.credential = credentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
					switch outcome {
					case "canceled":
						cancel()
					case "timed-out":
						// Sleeping to the deadline can race its cancellation callback.
						<-ctx.Done()
					}
					return azcore.AccessToken{}, errors.New("private-credential-diagnostic")
				})
				err := b.WarmupFixed(ctx, req)
				if err == nil || err.Error() != "ACA Sandbox managed-identity authentication failed" {
					t.Fatalf("original safe credential error changed: %v", err)
				}
				var report core.TimingReport
				if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &report); err != nil {
					t.Fatal(err)
				}
				wantStatus, wantKind := core.RunStatusFailed, core.RunErrorProvider
				if outcome == "canceled" {
					wantStatus, wantKind = core.RunStatusCanceled, core.RunErrorCanceled
				} else if outcome == "timed-out" {
					wantStatus, wantKind = core.RunStatusTimedOut, core.RunErrorTimeout
				}
				if report.ExitCode != 1 || report.RunStatus != wantStatus || report.ErrorKind != wantKind {
					t.Fatalf("sanitized credential failure misclassified: %+v", report)
				}
				if len(report.CreationEvents) != 1 || report.CreationEvents[0].Step != "provider.sandbox.credential" || report.CreationEvents[0].Count != 1 || f.creates != 0 || f.deletes != 0 || strings.Contains(stderr.String(), "private-credential-diagnostic") {
					t.Fatalf("credential failure leaked private data or submitted work: %s", stderr.String())
				}
			})
		})
	}
}
