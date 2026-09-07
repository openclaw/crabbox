package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type coordinatorCreateLeaseResult struct {
	lease CoordinatorLease
	err   error
}

// The transport runs inside synctest, so the real production budgets advance
// deterministically without network I/O or wall-clock waits.
type coordinatorAsyncFixture struct {
	t          *testing.T
	backend    *coordinatorLeaseBackend
	cfg        Config
	stderr     bytes.Buffer
	fixed      bool
	checkpoint bool
	started    time.Time
	deadline   time.Time
	requested  string
	canonical  string
	attempt    string
	createBody []byte
	creates    int
	gets       int
	cancels    int
	onCreate   func(*http.Request) (*http.Response, error)
	onGet      func(*http.Request) (*http.Response, error)
	onCancel   func(*http.Request) (*http.Response, error)
}

func newCoordinatorAsyncFixture(t *testing.T, fixed bool) *coordinatorAsyncFixture {
	t.Helper()
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	f := &coordinatorAsyncFixture{
		t: t, fixed: fixed, started: time.Now(),
		requested: "cbx_abcdef123456", canonical: "cbx_abcdef123457",
		cfg: Config{Provider: "azure", TargetOS: targetWindows, WindowsMode: windowsModeNormal},
	}
	if fixed {
		f.canonical = f.requested
	}
	f.backend = &coordinatorLeaseBackend{
		cfg: f.cfg, rt: Runtime{Stderr: &f.stderr},
		coord: &CoordinatorClient{BaseURL: "http://coordinator.test", Client: &http.Client{Transport: roundTripFunc(f.roundTrip)}},
	}
	return f
}

func coordinatorAsyncResponse(code int, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// Deliberately omit Preference-Applied: legacy brokers may ignore Prefer.
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (f *coordinatorAsyncFixture) lease(state string) CoordinatorLease {
	lease := CoordinatorLease{
		ID: f.canonical, Provider: f.cfg.Provider, TargetOS: f.cfg.TargetOS,
		WindowsMode: f.cfg.WindowsMode, State: state,
	}
	if state == "active" {
		lease.Host = "203.0.113.44"
	}
	return lease
}

func (f *coordinatorAsyncFixture) reply(lease CoordinatorLease) (*http.Response, error) {
	return coordinatorAsyncResponse(http.StatusAccepted, map[string]any{"lease": lease})
}

func (f *coordinatorAsyncFixture) roundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	method, path := http.MethodPost, "/v1/leases"
	if f.fixed {
		method, path = http.MethodPut, "/v1/leases/"+f.requested
	}
	if f.checkpoint {
		path += "/from-checkpoint"
	}
	switch {
	case r.Method == method && r.URL.Path == path:
		f.creates++
		if r.Header.Get("Prefer") != "respond-async" {
			f.t.Errorf("create preference=%q", r.Header.Get("Prefer"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Fatal(err)
		}
		if f.creates == 1 {
			f.createBody = body
			var request struct {
				LeaseID         string `json:"leaseID"`
				CreateAttemptID string `json:"createAttemptID"`
				Keep            bool   `json:"keep"`
				CheckpointID    string `json:"checkpointID"`
				CheckpointClaim string `json:"checkpointUseClaim"`
				AWSSnapshot     string `json:"awsSnapshot"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				f.t.Fatal(err)
			}
			if request.LeaseID != f.requested || !request.Keep || (request.CreateAttemptID == "") != f.fixed {
				f.t.Fatalf("unexpected create intent: %#v", request)
			}
			if f.checkpoint && (request.CheckpointID != "chk_async_fixed" || request.CheckpointClaim != "synthetic-checkpoint-claim" || request.AWSSnapshot != f.cfg.AWSSnapshot) {
				f.t.Fatal("fixed checkpoint create lost its source or use claim")
			}
			f.attempt = request.CreateAttemptID
			f.deadline, _ = r.Context().Deadline()
		} else if !bytes.Equal(body, f.createBody) {
			f.t.Fatal("replay changed the create intent")
		}
		return f.onCreate(r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+f.canonical:
		f.gets++
		if f.onGet == nil {
			f.t.Fatal("GET before canonical create confirmation")
		}
		return f.onGet(r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+f.requested+"/cancel-create":
		f.cancels++
		if f.fixed {
			f.t.Error("fixed operation was canceled")
		}
		if r.Header.Get("Prefer") != "" {
			f.t.Error("create preference leaked into cancellation")
		}
		var body struct {
			CreateAttemptID string `json:"createAttemptID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatal(err)
		}
		if body.CreateAttemptID != f.attempt || body.CreateAttemptID == "" {
			f.t.Fatal("cancellation did not bind the original attempt")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) != 10*time.Second || r.Context().Err() != nil {
			f.t.Fatalf("cancel context budget=%s err=%v", time.Until(deadline), r.Context().Err())
		}
		if f.onCancel != nil {
			return f.onCancel(r)
		}
		return f.cancelReply()
	default:
		f.t.Fatalf("unexpected request (possible adoption or unrelated cleanup): %s %s", r.Method, r.URL.Path)
		return nil, errors.New("unexpected request")
	}
}

func (f *coordinatorAsyncFixture) cancelReply() (*http.Response, error) {
	return coordinatorAsyncResponse(http.StatusOK, map[string]any{"canceledCreate": CoordinatorCanceledCreateAttestation{
		Version: 1, RequestedLeaseID: f.requested, CreateAttemptID: f.attempt, State: "canceled",
	}})
}

func (f *coordinatorAsyncFixture) acquire(ctx context.Context) (CoordinatorLease, error) {
	if f.checkpoint {
		f.cfg.Provider, f.cfg.TargetOS, f.cfg.WindowsMode = "aws", targetLinux, ""
		f.cfg.AWSSnapshot = "snap-0123456789abcdef0"
		f.backend.cfg = f.cfg
		ctx = withCheckpointLeaseClaim(ctx, "chk_async_fixed", "synthetic-checkpoint-claim")
	}
	return f.backend.createCoordinatorLeaseWithProgressMode(ctx, f.cfg, "synthetic-public-key", true, f.requested, "azure-resume", f.fixed)
}

func TestCoordinatorAsyncReplayReadinessOutlivesRebindBudget(t *testing.T) {
	for _, mode := range []string{"ordinary", "fixed", "fixed checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCoordinatorAsyncFixture(t, mode != "ordinary")
				f.checkpoint = mode == "fixed checkpoint"
				var confirmed time.Time
				f.onCreate = func(r *http.Request) (*http.Response, error) {
					if f.creates == 1 {
						time.Sleep(11 * time.Minute)
						return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
					}
					deadline, _ := r.Context().Deadline()
					if time.Until(deadline) != 90*time.Second {
						t.Fatalf("rebind budget=%s", time.Until(deadline))
					}
					confirmed = time.Now()
					return f.reply(f.lease("provisioning"))
				}
				f.onGet = func(r *http.Request) (*http.Response, error) {
					if deadline, _ := r.Context().Deadline(); deadline.After(f.deadline) {
						t.Fatal("activation reset the original deadline")
					}
					if time.Since(confirmed) < 2*time.Minute {
						return f.reply(f.lease("provisioning"))
					}
					return f.reply(f.lease("active"))
				}
				lease, err := f.acquire(context.Background())
				if err != nil || lease.ID != f.canonical || lease.State != "active" {
					t.Fatalf("lease=%#v err=%v", lease, err)
				}
				if f.creates != 2 || f.cancels != 0 || f.gets < 2 || time.Since(f.started) != 13*time.Minute || f.deadline.Sub(f.started) != 30*time.Minute {
					t.Fatalf("creates=%d cancels=%d gets=%d elapsed=%s budget=%s", f.creates, f.cancels, f.gets, time.Since(f.started), f.deadline.Sub(f.started))
				}
				if !strings.Contains(f.stderr.String(), "elapsed=12m0s") {
					t.Fatalf("no progress during recovered activation: %s", f.stderr.String())
				}
			})
		})
	}
}

func TestCoordinatorAsyncOriginalDeadlineExhaustion(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, phase := range []string{"initial", "rebind", "activation"} {
			t.Run(fmt.Sprintf("fixed=%t/%s", fixed, phase), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newCoordinatorAsyncFixture(t, fixed)
					f.cfg.TargetOS, f.backend.cfg.TargetOS = targetLinux, targetLinux
					f.onCreate = func(r *http.Request) (*http.Response, error) {
						if phase == "initial" || (phase == "rebind" && f.creates == 2) {
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						if f.creates == 1 {
							time.Sleep(9*time.Minute + 30*time.Second)
							return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
						}
						return f.reply(f.lease("provisioning"))
					}
					f.onGet = func(r *http.Request) (*http.Response, error) {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					lease, err := f.acquire(context.Background())
					wantCreates, wantCancels := 2, 1
					if phase == "initial" {
						wantCreates = 1
					}
					if fixed {
						wantCancels = 0
					}
					if !errors.Is(err, context.DeadlineExceeded) || lease.ID != "" || time.Since(f.started) != 10*time.Minute || f.creates != wantCreates || f.cancels != wantCancels {
						t.Fatalf("lease=%#v err=%v elapsed=%s creates=%d cancels=%d", lease, err, time.Since(f.started), f.creates, f.cancels)
					}
				})
			})
		}
	}
}

func TestCoordinatorAsyncCallerCancellationWhilePolling(t *testing.T) {
	for _, mode := range []string{"ordinary", "fixed", "fixed checkpoint"} {
		for _, replay := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replay=%t", mode, replay), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newCoordinatorAsyncFixture(t, mode != "ordinary")
					f.checkpoint = mode == "fixed checkpoint"
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					f.onCreate = func(*http.Request) (*http.Response, error) {
						if replay && f.creates == 1 {
							return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
						}
						return f.reply(f.lease("provisioning"))
					}
					f.onGet = func(*http.Request) (*http.Response, error) {
						cancel()
						// A concurrent active response cannot overrule caller cancellation.
						return f.reply(f.lease("active"))
					}
					lease, err := f.acquire(ctx)
					wantCancels := 1
					if f.fixed {
						wantCancels = 0
					}
					if !errors.Is(err, context.Canceled) || lease.ID != "" || f.gets != 1 || f.cancels != wantCancels {
						t.Fatalf("lease=%#v err=%v gets=%d cancels=%d", lease, err, f.gets, f.cancels)
					}
				})
			})
		}
	}
}

func TestCoordinatorAsyncCallerDeadlineAndFinalCancellationAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCoordinatorAsyncFixture(t, false)
		f.onCreate = func(*http.Request) (*http.Response, error) { return f.reply(f.lease("provisioning")) }
		f.onGet = func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		f.onCancel = func(r *http.Request) (*http.Response, error) {
			if f.cancels == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return f.cancelReply()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		lease, err := f.acquire(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || lease.ID != "" || f.creates != 1 || f.cancels != 2 || time.Since(f.started) != 30*time.Second || f.deadline.Sub(f.started) != 20*time.Second {
			t.Fatalf("lease=%#v err=%v creates=%d cancels=%d elapsed=%s budget=%s", lease, err, f.creates, f.cancels, time.Since(f.started), f.deadline.Sub(f.started))
		}
	})
}

func TestCoordinatorAsyncRejectsIdentityMismatchBeforeFurtherPolling(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, replay := range []bool{false, true} {
			for _, stage := range []string{"accepted", "poll"} {
				for _, field := range []string{"id", "empty id", "provider", "unknown provider", "target", "windows mode"} {
					if !fixed && stage == "accepted" && field == "id" {
						continue // Ordinary POST is authoritative for canonical remapping.
					}
					t.Run(fmt.Sprintf("fixed=%t/replay=%t/%s/%s", fixed, replay, stage, field), func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							f := newCoordinatorAsyncFixture(t, fixed)
							corrupt := func(lease CoordinatorLease) CoordinatorLease {
								switch field {
								case "id":
									lease.ID = "cbx_unrelated"
								case "empty id":
									lease.ID = ""
								case "provider":
									lease.Provider = "aws"
								case "unknown provider":
									lease.Provider = "unknown-provider"
								case "target":
									lease.TargetOS = targetLinux
								case "windows mode":
									lease.WindowsMode = windowsModeWSL2
								}
								return lease
							}
							f.onCreate = func(*http.Request) (*http.Response, error) {
								if replay && f.creates == 1 {
									return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
								}
								lease := f.lease("provisioning")
								if stage == "accepted" {
									lease = corrupt(lease)
								}
								return f.reply(lease)
							}
							f.onGet = func(*http.Request) (*http.Response, error) {
								// Reject even another provisioning reply, before a later GET
								// could hide the mismatch behind a valid active response.
								return f.reply(corrupt(f.lease("provisioning")))
							}
							lease, err := f.acquire(context.Background())
							wantGets, wantCancels, wantCreates := 0, 1, 1
							if stage == "poll" {
								wantGets = 1
							}
							if fixed {
								wantCancels = 0
							}
							if replay {
								wantCreates = 2
							}
							if err == nil || lease.ID != "" || f.gets != wantGets || f.cancels != wantCancels || f.creates != wantCreates {
								t.Fatalf("lease=%#v err=%v gets=%d cancels=%d creates=%d", lease, err, f.gets, f.cancels, f.creates)
							}
						})
					})
				}
			}
		}
	}
}

func TestCoordinatorAsyncDefinitiveRepliesStopRecovery(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, stage := range []string{"initial", "rebind", "poll"} {
			for _, outcome := range []string{"unauthorized", "forbidden", "conflict", "failed", "released", "expired", "invalid_state"} {
				t.Run(fmt.Sprintf("fixed=%t/%s/%s", fixed, stage, outcome), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						f := newCoordinatorAsyncFixture(t, fixed)
						definitive := func() (*http.Response, error) {
							switch outcome {
							case "unauthorized":
								return coordinatorAsyncResponse(http.StatusUnauthorized, outcome)
							case "forbidden":
								return coordinatorAsyncResponse(http.StatusForbidden, outcome)
							case "conflict":
								return coordinatorAsyncResponse(http.StatusConflict, "lease_id_conflict")
							default:
								return f.reply(f.lease(outcome))
							}
						}
						f.onCreate = func(*http.Request) (*http.Response, error) {
							if stage == "initial" || (stage == "rebind" && f.creates == 2) {
								return definitive()
							}
							if stage == "rebind" {
								return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
							}
							return f.reply(f.lease("provisioning"))
						}
						f.onGet = func(*http.Request) (*http.Response, error) { return definitive() }
						lease, err := f.acquire(context.Background())
						wantCreates, wantGets := 1, 0
						if stage == "rebind" {
							wantCreates = 2
						}
						if stage == "poll" {
							wantGets = 1
						}
						if err == nil || !strings.Contains(err.Error(), outcome) || lease.ID != "" || f.creates != wantCreates || f.gets != wantGets {
							t.Fatalf("lease=%#v err=%v creates=%d gets=%d", lease, err, f.creates, f.gets)
						}
					})
				})
			}
		}
	}
}

func TestCoordinatorAsyncRebindTimeoutDoesNotWaitForAcquisitionDeadline(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixed=%t", fixed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCoordinatorAsyncFixture(t, fixed)
				f.onCreate = func(r *http.Request) (*http.Response, error) {
					if f.creates > 1 {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
				}
				lease, err := f.acquire(context.Background())
				wantCancels := 1
				if fixed {
					wantCancels = 0
				}
				if !errors.Is(err, context.DeadlineExceeded) || lease.ID != "" || f.creates != 2 || f.gets != 0 || f.cancels != wantCancels || time.Since(f.started) != 90*time.Second {
					t.Fatalf("lease=%#v err=%v creates=%d gets=%d cancels=%d elapsed=%s", lease, err, f.creates, f.gets, f.cancels, time.Since(f.started))
				}
			})
		})
	}
}

func TestCoordinatorAsyncLegacyBrokerIgnoresPreference(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixed=%t", fixed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCoordinatorAsyncFixture(t, fixed)
				f.onCreate = func(*http.Request) (*http.Response, error) {
					lease := f.lease("active")
					lease.Provider, lease.TargetOS, lease.WindowsMode = "", "", ""
					return coordinatorAsyncResponse(http.StatusOK, map[string]any{"lease": lease})
				}
				lease, err := f.acquire(context.Background())
				if err != nil || lease.ID != f.canonical || f.creates != 1 || f.gets != 0 || f.cancels != 0 {
					t.Fatalf("lease=%#v err=%v creates=%d gets=%d cancels=%d", lease, err, f.creates, f.gets, f.cancels)
				}
			})
		})
	}
}

func TestCoordinatorAsyncProgressDuringStalledActivationRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCoordinatorAsyncFixture(t, false)
		f.onCreate = func(*http.Request) (*http.Response, error) { return f.reply(f.lease("provisioning")) }
		f.onGet = func(r *http.Request) (*http.Response, error) {
			if f.gets == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return f.reply(f.lease("active"))
		}
		lease, err := f.acquire(context.Background())
		if err != nil || lease.ID != f.canonical || f.creates != 1 || f.gets != 2 || f.cancels != 0 || !strings.Contains(f.stderr.String(), "elapsed=30s") {
			t.Fatalf("lease=%#v err=%v creates=%d gets=%d cancels=%d stderr=%s", lease, err, f.creates, f.gets, f.cancels, f.stderr.String())
		}
	})
}

func TestCoordinatorAsyncReplayedActiveLeaseRequiresEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCoordinatorAsyncFixture(t, false)
		f.onCreate = func(*http.Request) (*http.Response, error) {
			if f.creates == 1 {
				return coordinatorAsyncResponse(http.StatusInternalServerError, "error code: 1101")
			}
			lease := f.lease("active")
			lease.Host = ""
			return f.reply(lease)
		}
		lease, err := f.acquire(context.Background())
		if err == nil || !strings.Contains(err.Error(), "without an endpoint") || lease.ID != "" || f.creates != 2 || f.gets != 0 || f.cancels != 1 {
			t.Fatalf("lease=%#v err=%v creates=%d gets=%d cancels=%d", lease, err, f.creates, f.gets, f.cancels)
		}
	})
}

func TestCoordinatorAsyncTerminalDiagnosticDoesNotAuthorizeFreshAllocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCoordinatorAsyncFixture(t, false)
		f.onCreate = func(*http.Request) (*http.Response, error) {
			lease := f.lease("failed")
			lease.FailureError = "crabbox_aws_stale_instance_cleaned: InvalidInstanceID.NotFound"
			return f.reply(lease)
		}
		lease, err := f.acquire(context.Background())
		if err == nil || lease.ID != "" || f.creates != 1 || f.cancels != 1 || isCoordinatorStaleInstanceCleanedError(err) || isCoordinatorStaleInstanceCleanedSignal(err) {
			t.Fatalf("lease=%#v err=%v creates=%d cancels=%d", lease, err, f.creates, f.cancels)
		}
	})
}
