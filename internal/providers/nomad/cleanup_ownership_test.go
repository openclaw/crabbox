package nomad

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

type cleanupHookClient struct {
	Client
	jobInfo func(context.Context, string) (*nomadapi.Job, error)
	purge   func(context.Context, string, bool) (string, error)
	exec    func(context.Context, nomadExecRequest) (int, error)
}

func (c cleanupHookClient) JobInfo(ctx context.Context, id string) (*nomadapi.Job, error) {
	if c.jobInfo != nil {
		return c.jobInfo(ctx, id)
	}
	return c.Client.JobInfo(ctx, id)
}

func (c cleanupHookClient) DeregisterJob(ctx context.Context, id string, purge bool) (string, error) {
	if c.purge != nil {
		return c.purge(ctx, id, purge)
	}
	return c.Client.DeregisterJob(ctx, id, purge)
}

func (c cleanupHookClient) AllocationExec(ctx context.Context, req nomadExecRequest) (int, error) {
	if c.exec != nil {
		return c.exec(ctx, req)
	}
	return c.Client.AllocationExec(ctx, req)
}

func assertNomadClaimRetained(t *testing.T, expected LeaseClaim) {
	t.Helper()
	got, err := readLeaseClaim(expected.LeaseID)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("claim changed: got=%#v want=%#v err=%v", got, expected, err)
	}
}

func TestNomadDestructionFencesValidationPurgeAndAbsence(t *testing.T) {
	for _, operation := range []string{"stop", "cleanup", "run", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			fake := newLifecycleFakeClient()
			b, _, _ := testBackend(t, fake)
			claim := createClaim(t, b, "cbx_a11111111111", "fence-crab", "crabbox-a11111111111", "alloc-a")
			if operation == "cleanup" {
				expireClaim(t, claim, b.now().Add(-time.Hour))
				claim, _ = readLeaseClaim(claim.LeaseID)
			}
			expectedJob := cloneJob(fake.jobs[claim.Labels[claimLabelJobID]])
			if operation == "rollback" {
				if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					t.Fatal(err)
				}
			}
			lockPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claim-locks", claim.LeaseID+".json.lock")
			if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
				t.Fatal(err)
			}
			assertFenced := func(phase string) {
				t.Helper()
				lock := flock.New(lockPath)
				acquired, err := lock.TryLock()
				if acquired {
					_ = lock.Unlock()
				}
				if err != nil || acquired {
					t.Errorf("%s performed without exclusive claim fence: acquired=%t err=%v", phase, acquired, err)
				}
			}
			reads, purges := 0, 0
			client := cleanupHookClient{Client: fake,
				jobInfo: func(ctx context.Context, id string) (*nomadapi.Job, error) {
					reads++
					if operation != "cleanup" || reads > 1 {
						assertFenced("remote validation/absence")
					}
					return fake.JobInfo(ctx, id)
				},
				purge: func(ctx context.Context, id string, purge bool) (string, error) {
					purges++
					assertFenced("purge")
					return fake.DeregisterJob(ctx, id, purge)
				},
			}
			b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
			var err error
			switch operation {
			case "stop":
				err = b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
			case "cleanup":
				err = b.Cleanup(context.Background(), CleanupRequest{})
			case "run":
				err = b.deleteOwnedRunJob(context.Background(), client, claim)
			case "rollback":
				cause := errors.New("setup failed")
				err = b.cleanupUnclaimedJob(context.Background(), client, expectedJob, cause)
				if err == cause {
					err = nil
				}
			}
			if err != nil || purges != 1 || reads < 2 {
				t.Fatalf("err=%v purges=%d reads=%d", err, purges, reads)
			}
			if got, err := readLeaseClaim(claim.LeaseID); err != nil || got.LeaseID != "" {
				t.Fatalf("claim remains after confirmed removal: %#v err=%v", got, err)
			}
		})
	}
}

func TestNomadCleanupRejectsClaimChangeAfterPreflight(t *testing.T) {
	for _, mutation := range []string{"replace", "remove"} {
		for _, dryRun := range []bool{false, true} {
			for _, missing := range []bool{false, true} {
				t.Run(mutation+"/dry="+strconv.FormatBool(dryRun)+"/missing="+strconv.FormatBool(missing), func(t *testing.T) {
					fake := newLifecycleFakeClient()
					b, stdout, _ := testBackend(t, fake)
					claim := createClaim(t, b, "cbx_a22222222222", "race-crab", "crabbox-a22222222222", "alloc-a")
					expireClaim(t, claim, b.now().Add(-time.Hour))
					claim, _ = readLeaseClaim(claim.LeaseID)
					if missing {
						delete(fake.jobs, claim.Labels[claimLabelJobID])
					}
					changed := false
					client := cleanupHookClient{Client: fake, jobInfo: func(ctx context.Context, id string) (*nomadapi.Job, error) {
						job, err := fake.JobInfo(ctx, id)
						if !changed {
							changed = true
							if mutation == "remove" {
								if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
									t.Fatal(err)
								}
							} else {
								replacement := claim
								replacement.RepoRoot = filepath.Join(t.TempDir(), "new-owner")
								if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
									t.Fatal(err)
								}
								claim, _ = readLeaseClaim(claim.LeaseID)
							}
						}
						return job, err
					}}
					b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
					err := b.Cleanup(context.Background(), CleanupRequest{DryRun: dryRun})
					if err == nil || len(fake.deregisters) != 0 || strings.Contains(stdout.String(), "would ") {
						t.Fatalf("err=%v purges=%v stdout=%q", err, fake.deregisters, stdout)
					}
					if mutation == "replace" {
						assertNomadClaimRetained(t, claim)
					}
				})
			}
		}
	}
}

func TestNomadRunCleanupDoesNotAdoptSuccessorClaim(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	original := createClaim(t, b, "cbx_a33333333333", "run-crab", "crabbox-a33333333333", "alloc-a")
	successor := original
	successor.Labels = maps.Clone(original.Labels)
	successor.Labels[claimLabelJobID] = "crabbox-successor"
	if err := core.ReplaceLeaseClaimIfUnchanged(original.LeaseID, original, successor); err != nil {
		t.Fatal(err)
	}
	successor, _ = readLeaseClaim(original.LeaseID)
	job := cloneJob(fake.jobs[original.Labels[claimLabelJobID]])
	job.ID = stringPtr("crabbox-successor")
	job.Meta[metadataJobID] = "crabbox-successor"
	fake.jobs["crabbox-successor"] = job
	if err := b.deleteOwnedRunJob(context.Background(), fake, original); err == nil || len(fake.deregisters) != 0 {
		t.Fatalf("err=%v purges=%v", err, fake.deregisters)
	}
	assertNomadClaimRetained(t, successor)
}

func TestNomadRetainedRefreshDoesNotAdoptChangedClaim(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(strconv.FormatBool(removed), func(t *testing.T) {
			fake := newLifecycleFakeClient()
			b, _, _ := testBackend(t, fake)
			original := createClaim(t, b, "cbx_a33333333334", "retained-crab", "crabbox-a33333333334", "alloc-a")
			var successor LeaseClaim
			if removed {
				if err := core.RemoveLeaseClaimIfUnchanged(original.LeaseID, original); err != nil {
					t.Fatal(err)
				}
			} else {
				replacement := original
				replacement.Labels = maps.Clone(original.Labels)
				replacement.Labels[claimLabelJobID] = "crabbox-successor"
				if err := core.ReplaceLeaseClaimIfUnchanged(original.LeaseID, original, replacement); err != nil {
					t.Fatal(err)
				}
				var err error
				successor, err = readLeaseClaim(original.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := refreshNomadLeaseActivity(b.cfg, original); err == nil {
				t.Error("retained refresh accepted a changed claim")
			}
			if removed {
				got, err := readLeaseClaim(original.LeaseID)
				if err != nil || got.LeaseID != "" {
					t.Fatalf("retired claim recreated: %#v err=%v", got, err)
				}
			} else {
				assertNomadClaimRetained(t, successor)
			}
		})
	}
}

func TestNomadReuseDoesNotAdoptClaimChangedAfterLookup(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(strconv.FormatBool(removed), func(t *testing.T) {
			fake := newLifecycleFakeClient()
			b, _, _ := testBackend(t, fake)
			original := createClaim(t, b, "cbx_a33333333335", "reuse-fence", "crabbox-a33333333335", "alloc-a")
			var successor LeaseClaim
			client := cleanupHookClient{Client: fake, jobInfo: func(ctx context.Context, id string) (*nomadapi.Job, error) {
				if removed {
					if err := core.RemoveLeaseClaimIfUnchanged(original.LeaseID, original); err != nil {
						t.Fatal(err)
					}
				} else {
					replacement := original
					replacement.Labels = maps.Clone(original.Labels)
					replacement.Labels[claimLabelJobID] = "crabbox-successor"
					var err error
					successor, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(original.LeaseID, original, replacement)
					if err != nil {
						t.Fatal(err)
					}
				}
				return fake.JobInfo(ctx, id)
			}}
			b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
			_, err := b.Run(context.Background(), RunRequest{ID: original.LeaseID, Repo: Repo{Root: original.RepoRoot}, Reclaim: true, NoSync: true, Command: []string{"true"}})
			if err == nil || len(fake.execs) != 0 || len(fake.deregisters) != 0 {
				t.Fatalf("err=%v executions=%d purges=%v", err, len(fake.execs), fake.deregisters)
			}
			if removed {
				got, err := readLeaseClaim(original.LeaseID)
				if err != nil || got.LeaseID != "" {
					t.Fatalf("retired claim recreated: %#v err=%v", got, err)
				}
			} else {
				assertNomadClaimRetained(t, successor)
			}
		})
	}
}

func TestNomadKeptRunRefreshFencesClaimChangedDuringExecution(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(strconv.FormatBool(removed), func(t *testing.T) {
			fake := newLifecycleFakeClient()
			b, _, _ := testBackend(t, fake)
			var original, successor LeaseClaim
			client := cleanupHookClient{Client: fake, exec: func(ctx context.Context, req nomadExecRequest) (int, error) {
				code, err := fake.AllocationExec(ctx, req)
				if len(fake.execs) != 2 {
					return code, err
				}
				original, err = readLeaseClaim(fake.jobs[req.JobID].Meta[metadataLeaseID])
				if err != nil || original.LeaseID == "" {
					t.Fatalf("original claim=%#v err=%v", original, err)
				}
				if removed {
					err = core.RemoveLeaseClaimIfUnchanged(original.LeaseID, original)
				} else {
					replacement := original
					replacement.Labels = maps.Clone(original.Labels)
					replacement.Labels[claimLabelJobID] = "crabbox-successor"
					successor, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(original.LeaseID, original, replacement)
				}
				if err != nil {
					t.Fatal(err)
				}
				return code, nil
			}}
			b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
			result, err := b.Run(context.Background(), RunRequest{Repo: newNomadRunRepo(t), Keep: true, NoSync: true, Command: []string{"true"}})
			if err == nil || result.ExitCode == 0 || len(fake.deregisters) != 0 {
				t.Fatalf("result=%#v err=%v purges=%v", result, err, fake.deregisters)
			}
			assertNomadRunSession(t, result, false, true)
			if removed {
				got, err := readLeaseClaim(original.LeaseID)
				if err != nil || got.LeaseID != "" {
					t.Fatalf("retired claim recreated: %#v err=%v", got, err)
				}
			} else {
				assertNomadClaimRetained(t, successor)
			}
		})
	}
}

func TestNomadSetupRollbackRetainsPublishedClaim(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			fake := newLifecycleFakeClient()
			b, _, _ := testBackend(t, fake)
			claim := createClaim(t, b, "cbx_a44444444444", "setup-crab", "crabbox-a44444444444", "alloc-a")
			expected := cloneJob(fake.jobs[claim.Labels[claimLabelJobID]])
			if partial {
				var err error
				claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			cause := errors.New("publication failed")
			err := b.cleanupUnclaimedJob(context.Background(), fake, expected, cause)
			if !errors.Is(err, cause) || err == cause || len(fake.deregisters) != 0 {
				t.Fatalf("err=%v purges=%v", err, fake.deregisters)
			}
			assertNomadClaimRetained(t, claim)
		})
	}
}

func TestNomadCleanupRetainsReappearedJob(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_a55555555555", "return-crab", "crabbox-a55555555555", "alloc-a")
	reads := 0
	client := cleanupHookClient{Client: fake, jobInfo: func(ctx context.Context, id string) (*nomadapi.Job, error) {
		reads++
		if reads == 1 {
			return nil, fakeHTTPStatusError(http.StatusNotFound)
		}
		return fake.JobInfo(ctx, id)
	}}
	b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err == nil || len(fake.deregisters) != 0 {
		t.Fatalf("err=%v purges=%v", err, fake.deregisters)
	}
	assertNomadClaimRetained(t, claim)
}

func TestNomadStopHonorsCancellationBeforePurge(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_a66666666666", "cancel-crab", "crabbox-a66666666666", "alloc-a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Stop(ctx, StopRequest{ID: claim.LeaseID}); !errors.Is(err, context.Canceled) || len(fake.deregisters) != 0 {
		t.Fatalf("err=%v purges=%v", err, fake.deregisters)
	}
	assertNomadClaimRetained(t, claim)
}

func TestNomadStopBoundsUnconfirmedAbsence(t *testing.T) {
	fake := newLifecycleFakeClient()
	b, _, _ := testBackend(t, fake)
	claim := createClaim(t, b, "cbx_a77777777777", "pending-crab", "crabbox-a77777777777", "alloc-a")
	b.cfg.Nomad.EvalTimeout = time.Second
	// No network setup: the fake continuously reports the original job after purge.
	client := cleanupHookClient{Client: fake, jobInfo: func(ctx context.Context, id string) (*nomadapi.Job, error) {
		return cloneJob(fake.jobs[id]), nil
	}}
	b.clientFactory = func(Config, Runtime) (Client, error) { return client, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.Stop(ctx, StopRequest{ID: claim.LeaseID})
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || len(fake.deregisters) != 1 {
		t.Fatalf("err=%v outer=%v purges=%v", err, ctx.Err(), fake.deregisters)
	}
	assertNomadClaimRetained(t, claim)
}
