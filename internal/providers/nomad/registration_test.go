package nomad

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

type registrationHookClient struct {
	Client
	register   func(context.Context, *nomadapi.Job) (string, error)
	evaluation func(context.Context, string) (*nomadapi.Evaluation, error)
}

func (c registrationHookClient) RegisterJob(ctx context.Context, job *nomadapi.Job) (string, error) {
	return c.register(ctx, job)
}

func (c registrationHookClient) EvaluationInfo(ctx context.Context, id string) (*nomadapi.Evaluation, error) {
	if c.evaluation != nil {
		return c.evaluation(ctx, id)
	}
	return c.Client.EvaluationInfo(ctx, id)
}

func onlyRegistrationClaim(t *testing.T) LeaseClaim {
	t.Helper()
	claims, err := listNomadLeaseClaims()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%d err=%v, want one retained registration", len(claims), err)
	}
	return claims[0]
}

func TestNomadRegistrationUncertaintyRetainsRecovery(t *testing.T) {
	for _, primary := range []struct {
		name  string
		cause error
		code  int
	}{
		{"typed-four", exit(4, "registration response unavailable"), 4},
		{"typed-five", exit(5, "registration response unavailable"), 5},
		{"generic", errors.New("registration response unavailable"), 1},
		{"canceled", context.Canceled, 1},
		{"deadline", context.DeadlineExceeded, 1},
	} {
		for _, mode := range []string{"warmup", "run", "run-keep"} {
			t.Run(primary.name+"/"+mode, func(t *testing.T) {
				fake := newLifecycleFakeClient()
				b, _, stderr := testBackend(t, fake)
				cause := primary.cause
				calls := 0
				client := registrationHookClient{Client: fake, register: func(_ context.Context, job *nomadapi.Job) (string, error) {
					calls++
					claim := onlyRegistrationClaim(t)
					if state, err := registrationState(claim); err != nil || state != registrationSubmitting || claim.Labels[claimLabelJobID] != stringValue(job.ID) {
						t.Fatalf("registration was not durably submitting: state=%s err=%v", state, err)
					}
					if claim.Labels[claimLabelAllocationID] != "" {
						t.Fatal("pending registration fabricated an allocation")
					}
					return "", cause
				}}
				b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
				repo := Repo{Root: t.TempDir(), Name: "ordinary-repo"}
				var err error
				if mode == "warmup" {
					err = b.Warmup(context.Background(), WarmupRequest{Repo: repo, Keep: true})
				} else {
					var result RunResult
					result, err = b.Run(context.Background(), RunRequest{Repo: repo, Command: []string{"true"}, NoSync: true, Keep: mode == "run-keep", TimingJSON: true})
					claim := onlyRegistrationClaim(t)
					if result.Session == nil || !result.Session.Kept || result.Session.Reused || result.LeaseID != claim.LeaseID || result.Session.CleanupCommand == "" {
						t.Fatalf("missing recovery session: %#v", result.Session)
					}
					if !strings.Contains(stderr.String(), claim.LeaseID) {
						t.Fatal("timing/reporting omitted retained lease")
					}
				}
				claim := onlyRegistrationClaim(t)
				var displayed core.ExitError
				if !errors.Is(err, cause) || !core.AsExitError(err, &displayed) || displayed.Code != primary.code {
					t.Fatalf("primary error/code lost: code=%d want=%d: %v", displayed.Code, primary.code, err)
				}
				for _, value := range []string{claim.LeaseID, claim.Labels[claimLabelJobID], claim.ProviderScope, "crabbox stop --provider nomad"} {
					if !strings.Contains(displayed.Message, value) {
						t.Fatalf("CLI-selected error omitted %q: %s", value, displayed.Message)
					}
				}
				view, err := b.Status(context.Background(), StatusRequest{ID: claim.LeaseID})
				if err != nil || view.State != "registration-pending" || view.Ready {
					t.Fatalf("pending status=%#v err=%v", view, err)
				}
				for _, dryRun := range []bool{true, false} {
					if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: dryRun}); err == nil || !strings.Contains(err.Error(), "registration outcome is unknown; claim retained") {
						t.Fatalf("cleanup dryRun=%t erased uncertainty: %v", dryRun, err)
					}
				}
				if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err == nil {
					t.Fatal("pending absence treated as completed stop")
				}
				assertNomadClaimRetained(t, claim)
				if calls != 1 || len(fake.deregisters) != 0 || len(fake.execs) != 0 {
					t.Fatalf("unexpected retry/cleanup/exec: register=%d purge=%v exec=%v", calls, fake.deregisters, fake.execs)
				}
			})
		}
	}
}

func TestNomadAcceptedRegistrationHTTPDeadlineRollsBack(t *testing.T) {
	old := nomadControlRequestTimeout
	nomadControlRequestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { nomadControlRequestTimeout = old })
	t.Setenv("NOMAD_TOKEN", "")
	b, _, _ := testBackend(t, nil)
	var mu sync.Mutex
	var stored *nomadapi.Job
	var submittedClaim LeaseClaim
	var requests []string
	registrationCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut && r.URL.Path == "/v1/jobs" {
			claims, err := listNomadLeaseClaims()
			if err != nil || len(claims) != 1 {
				t.Errorf("registration reached HTTP without one durable claim: claims=%d err=%v", len(claims), err)
				http.Error(w, "missing registration claim", http.StatusInternalServerError)
				return
			}
			claim := claims[0]
			if state, err := registrationState(claim); err != nil || state != registrationSubmitting {
				t.Errorf("registration reached HTTP before submitting: state=%s err=%v", state, err)
			}
			var body struct{ Job *nomadapi.Job }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Job == nil {
				t.Errorf("decode submitted job: job=%v err=%v", body.Job, err)
				http.Error(w, "invalid job", http.StatusBadRequest)
				return
			}
			if stringValue(body.Job.ID) != claim.Labels[claimLabelJobID] {
				t.Errorf("submitted job does not match durable claim")
			}
			mu.Lock()
			stored, submittedClaim = body.Job, claim
			requests = append(requests, "PUT")
			mu.Unlock()
			// Accept the unchanged job, but withhold the rest of the JSON acknowledgement.
			_, _ = w.Write([]byte(`{"EvalID":"`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(registrationCanceled)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method)
		if submittedClaim.LeaseID == "" || r.URL.Path != "/v1/job/"+submittedClaim.Labels[claimLabelJobID] {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if stored == nil {
				http.NotFound(w, r)
				return
			}
			if err := json.NewEncoder(w).Encode(stored); err != nil {
				t.Errorf("write stored job: %v", err)
			}
		case http.MethodDelete:
			if stored == nil || r.URL.Query().Get("purge") != "true" {
				t.Errorf("cleanup did not purge the accepted job")
			}
			stored = nil
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request method: %s", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	b.cfg.Nomad.Address = server.URL
	b.clientFactory = newNomadClient
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.Warmup(ctx, WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "ordinary-repo"}, Keep: true})
	var displayed core.ExitError
	if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || !core.AsExitError(err, &displayed) || displayed.Code != 1 {
		t.Fatalf("initiating request deadline/code lost: caller=%v code=%d err=%v", ctx.Err(), displayed.Code, err)
	}
	select {
	case <-registrationCanceled:
	case <-ctx.Done():
		t.Fatal("registration HTTP request was not canceled")
	}
	mu.Lock()
	defer mu.Unlock()
	if stored != nil || !slices.Equal(requests, []string{"PUT", "GET", "DELETE", "GET"}) {
		t.Fatalf("accepted registration rollback sequence: stored=%v requests=%v", stored != nil, requests)
	}
	if !strings.Contains(displayed.Message, submittedClaim.LeaseID) || !strings.Contains(displayed.Message, submittedClaim.Labels[claimLabelJobID]) || !strings.Contains(displayed.Message, "rolled back") || strings.Contains(displayed.Message, "recover with") {
		t.Fatalf("rollback diagnostic lost identity or implied retention: %s", displayed.Message)
	}
	if claims, err := listNomadLeaseClaims(); err != nil || len(claims) != 0 {
		t.Fatalf("claim remains after confirmed HTTP rollback: claims=%d err=%v", len(claims), err)
	}
}

func TestNomadRegistrationAcknowledgedBeforeReadiness(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "temporary", true: "keep"}[keep], func(t *testing.T) {
			fake := newLifecycleFakeClient()
			b, _, _ := testBackend(t, fake)
			confirmed := false
			client := registrationHookClient{Client: fake, register: fake.RegisterJob, evaluation: func(ctx context.Context, id string) (*nomadapi.Evaluation, error) {
				if !strings.HasPrefix(id, "eval-deregister-") {
					claim := onlyRegistrationClaim(t)
					state, err := registrationState(claim)
					if err != nil || state != registrationConfirmed || claim.Labels[registrationEvalLabel] != id || claim.Labels[claimLabelAllocationID] != "" {
						t.Fatalf("acknowledgement not persisted before wait: state=%s err=%v", state, err)
					}
					confirmed = true
					return &nomadapi.Evaluation{ID: id, Status: nomadapi.EvalStatusFailed}, nil
				}
				return fake.EvaluationInfo(ctx, id)
			}}
			b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
			result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "repo"}, Command: []string{"true"}, NoSync: true, Keep: keep})
			if err == nil || !confirmed || len(fake.deregisters) != 1 || result.Session != nil {
				t.Fatalf("readiness rollback changed Keep contract: err=%v confirmed=%t purges=%v session=%#v", err, confirmed, fake.deregisters, result.Session)
			}
			var displayed core.ExitError
			if !core.AsExitError(err, &displayed) || !strings.Contains(displayed.Message, "rolled back") || !strings.Contains(displayed.Message, "lease=") || !strings.Contains(displayed.Message, "job=") || strings.Contains(displayed.Message, "recover with") {
				t.Fatalf("successful rollback lost identity or implied retention: %v", err)
			}
			claims, err := listNomadLeaseClaims()
			if err != nil || len(claims) != 0 {
				t.Fatalf("claim remains after rollback: %d %v", len(claims), err)
			}
		})
	}
}

func TestNomadPreparedRegistrationNeverSubmitsAfterRemoval(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	prepared, err := b.prepareRegistration(context.Background(), Repo{Root: t.TempDir()}, "prepared")
	if err != nil {
		t.Fatal(err)
	}
	lookup := cleanupHookClient{Client: fake, jobInfo: func(context.Context, string) (*nomadapi.Job, error) {
		t.Fatal("prepared cleanup must not query remote jobs")
		return nil, nil
	}}
	if _, err := b.removeOwnedJob(context.Background(), lookup, prepared.claim, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.submitRegistration(context.Background(), fake, prepared); err == nil || fake.registers != 0 {
		t.Fatalf("removed attempt submitted: err=%v calls=%d", err, fake.registers)
	}
	if current, err := readLeaseClaim(prepared.claim.LeaseID); err != nil || current.LeaseID != "" {
		t.Fatalf("removed claim recreated: %#v %v", current, err)
	}
}

func TestNomadRegistrationAdmissionFailureDoesNotDispatch(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, []byte("ordinary fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", notDirectory)
	_, _, recovery, err := b.createJob(context.Background(), fake, Repo{Root: t.TempDir()}, "no-dispatch")
	if err == nil || recovery != nil || fake.registers != 0 {
		t.Fatalf("admission failure dispatched: err=%v recovery=%#v calls=%d", err, recovery, fake.registers)
	}
}

func TestNomadObservedRegistrationErrorRollsBack(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	cause := exit(5, "registration response unavailable")
	client := registrationHookClient{Client: fake, register: func(ctx context.Context, job *nomadapi.Job) (string, error) {
		_, err := fake.RegisterJob(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		return "", cause
	}}
	_, claim, recovery, err := b.createJob(context.Background(), client, Repo{Root: t.TempDir()}, "observed")
	if !errors.Is(err, cause) || recovery != nil || claim.LeaseID != "" || fake.registers != 1 || len(fake.deregisters) != 1 {
		t.Fatalf("observed registration rollback: claim=%s recovery=%#v calls=%d purge=%v err=%v", claim.LeaseID, recovery, fake.registers, fake.deregisters, err)
	}
}

func TestNomadAcknowledgementRecordedAfterCallerCancel(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observedConfirmed := false
	reads := cleanupHookClient{Client: fake, jobInfo: func(ctx context.Context, id string) (*nomadapi.Job, error) {
		claim := onlyRegistrationClaim(t)
		if state, err := registrationState(claim); err != nil || state != registrationConfirmed {
			t.Fatalf("successful acknowledgement remained unknown: state=%s err=%v", state, err)
		}
		observedConfirmed = true
		return fake.JobInfo(ctx, id)
	}}
	client := registrationHookClient{Client: reads, register: func(ctx context.Context, job *nomadapi.Job) (string, error) {
		id, err := fake.RegisterJob(ctx, job)
		cancel()
		return id, err
	}}
	_, _, recovery, err := b.createJob(ctx, client, Repo{Root: t.TempDir()}, "canceled-after-ack")
	if !errors.Is(err, context.Canceled) || recovery != nil || !observedConfirmed || len(fake.deregisters) != 1 {
		t.Fatalf("ack/cancel recovery=%#v confirmed=%t purge=%v err=%v", recovery, observedConfirmed, fake.deregisters, err)
	}
}

func TestNomadRecoveryVerificationHonorsCancellation(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	prepared, err := b.prepareRegistration(context.Background(), Repo{Root: t.TempDir()}, "verify-canceled")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if recovery, err := registrationRecovery(ctx, prepared.claim); recovery != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification recovery=%#v err=%v", recovery, err)
	}
	assertNomadClaimRetained(t, prepared.claim)
}

func TestNomadRegistrationMetadataAndLegacyBoundary(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_a77777777777", "metadata", "crabbox-a77777777777", "alloc-a")
	job := fake.jobs[claim.Labels[claimLabelJobID]]
	job.Meta["custom"] = "original-value"
	job.Meta["empty"] = ""
	claim = markRegistrationClaim(t, claim, job, registrationSubmitting)
	if strings.Contains(claim.Labels[registrationMetaKeysLabel]+claim.Labels[registrationMetaHashLabel], "original-value") {
		t.Fatal("arbitrary metadata value persisted")
	}
	job.Meta["server-added"] = "allowed"
	delete(job.Meta, "empty")
	if err := validateRegistrationMetadata(claim, job); err != nil {
		t.Fatalf("additional metadata or missing expected-empty changed matching: %v", err)
	}
	job.Meta["custom"] = "changed"
	if err := validateRegistrationMetadata(claim, job); err == nil {
		t.Fatal("pending original metadata change accepted")
	}
	ready := claim
	ready.Labels = maps.Clone(claim.Labels)
	ready.Labels[registrationStateLabel] = registrationConfirmed
	ready.Labels[claimLabelAllocationID] = "alloc-a"
	if err := validateRemoteOwnership(b.cfg, ready, job); err != nil {
		t.Fatalf("normal ready ownership semantics strengthened: %v", err)
	}
	if err := validateRegistrationMetadata(ready, job); err != nil {
		t.Fatalf("ready claim retained setup-only full map guard: %v", err)
	}
	for _, labels := range []map[string]string{{registrationVersionLabel: "2"}, {registrationStateLabel: registrationSubmitting}, {registrationMetaHashLabel: "unknown"}} {
		if _, err := registrationState(LeaseClaim{Labels: labels}); err == nil {
			t.Fatal("unknown or incomplete registration schema accepted")
		}
	}
	if state, err := registrationState(LeaseClaim{}); err != nil || state != registrationConfirmed {
		t.Fatalf("legacy claim semantics changed: %s %v", state, err)
	}
	job.Meta["custom"] = "original-value"
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatalf("later matching observation did not reconcile: %v", err)
	}
	if len(fake.deregisters) != 1 {
		t.Fatalf("matching observed job purges=%v", fake.deregisters)
	}
	if _, err := fake.JobInfo(context.Background(), claim.Labels[claimLabelJobID]); !isNotFoundError(err) {
		t.Fatalf("confirmed removal not observed: %v, want HTTP %d", err, http.StatusNotFound)
	}
}
