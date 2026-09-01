package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/testutil"
)

const pondReleaseDelegatedProviderName = "pond-release-delegated-test"

type pondReleaseDelegatedProvider struct {
	backend *pondReleaseDelegatedBackend
}

func (pondReleaseDelegatedProvider) Name() string      { return pondReleaseDelegatedProviderName }
func (pondReleaseDelegatedProvider) Aliases() []string { return nil }
func (pondReleaseDelegatedProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: pondReleaseDelegatedProviderName, Kind: ProviderKindDelegatedRun,
		Targets: []TargetSpec{{OS: targetLinux}}, Coordinator: CoordinatorNever,
	}
}
func (pondReleaseDelegatedProvider) RegisterFlags(*flag.FlagSet, Config) any      { return nil }
func (pondReleaseDelegatedProvider) ApplyFlags(*Config, *flag.FlagSet, any) error { return nil }
func (p pondReleaseDelegatedProvider) Configure(Config, Runtime) (Backend, error) {
	return p.backend, nil
}

type pondReleaseDelegatedBackend struct {
	stop func(context.Context, StopRequest) error
}

func (*pondReleaseDelegatedBackend) Spec() ProviderSpec {
	return (pondReleaseDelegatedProvider{}).Spec()
}
func (*pondReleaseDelegatedBackend) Warmup(context.Context, WarmupRequest) error {
	return errors.New("unexpected warmup")
}
func (*pondReleaseDelegatedBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, errors.New("unexpected run")
}
func (*pondReleaseDelegatedBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, errors.New("unexpected list")
}
func (*pondReleaseDelegatedBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, errors.New("unexpected status")
}
func (b *pondReleaseDelegatedBackend) Stop(ctx context.Context, req StopRequest) error {
	return b.stop(ctx, req)
}

func setupPondReleaseDelegated(t *testing.T, stop func(context.Context, StopRequest) error) (App, *strings.Builder) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	withTempClaims(t, nil)
	t.Chdir(t.TempDir())
	if providerRegistry[pondReleaseDelegatedProviderName] != nil {
		t.Fatal("test provider already registered")
	}
	RegisterProvider(pondReleaseDelegatedProvider{backend: &pondReleaseDelegatedBackend{stop: stop}})
	t.Cleanup(func() { delete(providerRegistry, pondReleaseDelegatedProviderName) })
	stderr := &strings.Builder{}
	return App{Stdout: io.Discard, Stderr: stderr}, stderr
}

func publishPondReleaseDelegatedClaim(t *testing.T, claim leaseClaim) leaseClaim {
	t.Helper()
	// Publish through the production transaction owner, only into an absent slot.
	published, err := transactLeaseClaim(claim.LeaseID, leaseClaimTransaction{
		guard:     unchangedLeaseClaimGuard(claim.LeaseID, leaseClaim{}, false),
		directory: claimDirectoryCreate, revision: claimRevisionAfterMutation,
		mutate: func(current *leaseClaim) error {
			*current = cloneLeaseClaim(claim)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return published
}

func pondReleaseDelegatedClaim(id string) leaseClaim {
	return leaseClaim{
		LeaseID: id, Slug: id, Provider: pondReleaseDelegatedProviderName,
		ProviderScope: "synthetic-scope", CloudID: "synthetic-resource",
		Pond: "alpha", RepoRoot: "/synthetic/repo",
		ClaimedAt: "2026-01-02T03:04:05Z", LastUsedAt: "2026-01-02T03:04:05Z",
		SSHHost: "peer.example.invalid", SSHPort: 2222,
		Labels:       map[string]string{"state": "ready", "owner": "synthetic-owner"},
		CacheVolumes: []string{"synthetic-cache"},
	}
}

func assertPondReleaseDelegatedClaim(t *testing.T, id string, want leaseClaim, wantExists bool) {
	t.Helper()
	got, exists, err := readLeaseClaimWithPresence(id)
	if err != nil || exists != wantExists || !reflect.DeepEqual(got, want) {
		t.Fatalf("claim=%#v exists=%t err=%v; want %#v exists=%t", got, exists, err, want, wantExists)
	}
}

func TestPondReleaseDelegatedPreservesReplacement(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("synthetic stop failure after replacement")} {
		name := "success"
		if stopErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			var original, replacement leaseClaim
			calls, actions := 0, 0
			app, stderr := setupPondReleaseDelegated(t, func(_ context.Context, req StopRequest) error {
				calls++
				if req.ID != original.LeaseID {
					t.Fatalf("stopping %q, want %q", req.ID, original.LeaseID)
				}
				if err := removeLeaseClaimIfUnchangedAfter(req.ID, original, func() error {
					actions++
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				assertPondReleaseDelegatedClaim(t, req.ID, leaseClaim{}, false)
				// Deterministically model a writer after Stop's fence releases but
				// before pond resumes. Only the revision differs from the old claim.
				replacement = publishPondReleaseDelegatedClaim(t, original)
				withoutRevision := cloneLeaseClaim(replacement)
				withoutRevision.Revision = original.Revision
				if replacement.Revision == original.Revision || !reflect.DeepEqual(withoutRevision, original) {
					t.Fatal("replacement must differ only in revision")
				}
				return stopErr
			})
			original = publishPondReleaseDelegatedClaim(t, pondReleaseDelegatedClaim("cbx_replacement"))
			err := app.pondRelease(t.Context(), []string{"alpha"})
			if !errors.Is(err, stopErr) || calls != 1 || actions != 1 {
				t.Fatalf("err=%v calls=%d actions=%d", err, calls, actions)
			}
			assertPondReleaseDelegatedClaim(t, original.LeaseID, replacement, true)
			if strings.Contains(stderr.String(), "stop failed") != (stopErr != nil) {
				t.Fatalf("unexpected diagnostics: %s", stderr)
			}
		})
	}
}

func TestPondReleaseDelegatedOwnsClaimFinalization(t *testing.T) {
	for _, outcome := range []string{"removed", "retained", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			var original, want leaseClaim
			failure := errors.New("synthetic stop rejected")
			calls := 0
			app, stderr := setupPondReleaseDelegated(t, func(_ context.Context, req StopRequest) error {
				calls++
				switch outcome {
				case "retained":
					retained := cloneLeaseClaim(original)
					retained.Labels["state"] = "stopped"
					retained.SSHHost, retained.SSHPort = "", 0
					var err error
					want, err = replaceLeaseClaimIfUnchangedDurableAfter(req.ID, original, retained, func() error { return nil })
					return err
				default:
					return removeLeaseClaimIfUnchangedAfter(req.ID, original, func() error {
						if outcome == "failed" {
							return failure
						}
						return nil
					})
				}
			})
			original = publishPondReleaseDelegatedClaim(t, pondReleaseDelegatedClaim("cbx_owned"))
			if outcome == "failed" {
				want = original
			}
			err := app.pondRelease(t.Context(), []string{"alpha"})
			var wantErr error
			if outcome == "failed" {
				wantErr = failure
			}
			if !errors.Is(err, wantErr) || calls != 1 {
				t.Fatalf("err=%v want=%v calls=%d", err, wantErr, calls)
			}
			assertPondReleaseDelegatedClaim(t, original.LeaseID, want, outcome != "removed")
			if strings.Contains(stderr.String(), "released ") != (wantErr == nil) {
				t.Fatalf("unexpected diagnostics: %s", stderr)
			}
		})
	}
}

func TestPondReleaseDelegatedContinuesAfterFailure(t *testing.T) {
	firstErr, secondErr := errors.New("first stop failed"), errors.New("second stop failed")
	failures := map[string]error{"cbx_1": firstErr, "cbx_2": secondErr}
	claims := map[string]leaseClaim{}
	var calls []string
	app, stderr := setupPondReleaseDelegated(t, func(_ context.Context, req StopRequest) error {
		calls = append(calls, req.ID)
		return removeLeaseClaimIfUnchangedAfter(req.ID, claims[req.ID], func() error { return failures[req.ID] })
	})
	for _, id := range []string{"cbx_1", "cbx_2", "cbx_3", "cbx_other"} {
		claim := pondReleaseDelegatedClaim(id)
		if id == "cbx_other" {
			claim.Pond = "other"
		}
		claims[id] = publishPondReleaseDelegatedClaim(t, claim)
	}
	if err := app.pondRelease(t.Context(), []string{"alpha"}); !errors.Is(err, firstErr) {
		t.Fatalf("err=%v, want first failure", err)
	}
	if !reflect.DeepEqual(calls, []string{"cbx_1", "cbx_2", "cbx_3"}) {
		t.Fatalf("stops=%v", calls)
	}
	for id, claim := range claims {
		if id == "cbx_3" {
			assertPondReleaseDelegatedClaim(t, id, leaseClaim{}, false)
		} else {
			assertPondReleaseDelegatedClaim(t, id, claim, true)
		}
	}
	for _, diagnostic := range []string{
		fmt.Sprintf("warning: %s/cbx_1 stop failed: %v", pondReleaseDelegatedProviderName, firstErr),
		fmt.Sprintf("warning: %s/cbx_2 stop failed: %v", pondReleaseDelegatedProviderName, secondErr),
		"released " + pondReleaseDelegatedProviderName + "/cbx_3",
	} {
		if !strings.Contains(stderr.String(), diagnostic) {
			t.Errorf("missing %q in %q", diagnostic, stderr)
		}
	}
}
