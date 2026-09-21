package superserve

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type d1ReadClient struct {
	superserveClient
	get func(context.Context, string) (superserveSandbox, error)
}

func (c d1ReadClient) GetSandbox(ctx context.Context, id string) (superserveSandbox, error) {
	return c.get(ctx, id)
}

func TestD1ReadCharacterization(t *testing.T) {
	for _, mode := range []string{"ready", "ready before cancellation", "terminal", "wait terminal", "raw error", "canceled fetch", "deadline fetch", "parent deadline", "missing labels", "foreign labels", "ownership before cancellation", "foreign scope", "unclaimed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := newFakeSuperserveClient()
			b := newSuperserveTestBackend(t, fake)
			scope, err := newSuperserveClaimScope(fake.baseURL)
			if err != nil {
				t.Fatal(err)
			}
			sb := superserveSandbox{ID: "d1-sandbox", Metadata: ownedMetadata(fake.baseURL, scope, leasePrefix+"d1-sandbox", "d1-status")}
			leaseID := leasePrefix + sb.ID
			sb.Status = " RUNNING "
			if strings.Contains(mode, "terminal") {
				sb.Status = "stopped"
			}
			if mode == "missing labels" || mode == "ownership before cancellation" {
				sb.Metadata = nil
			}
			if mode == "foreign labels" {
				for key := range sb.Metadata {
					sb.Metadata[key] = "foreign"
				}
			}
			if mode == "foreign scope" {
				scope = "foreign"
			}
			if mode != "unclaimed" {
				if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "d1-status", providerName, scope, "d1-pond", "/repo", time.Minute, false); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "parent deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			nativeErr := errors.New("D1 transport failure")
			calls := 0
			api := d1ReadClient{superserveClient: fake, get: func(ctx context.Context, id string) (superserveSandbox, error) {
				calls++
				if id != sb.ID {
					t.Fatalf("id = %q", id)
				}
				if mode == "canceled fetch" || mode == "ownership before cancellation" || mode == "ready before cancellation" {
					cancel()
				}
				if mode == "deadline fetch" || mode == "parent deadline" {
					<-ctx.Done()
				}
				if mode == "raw error" || strings.Contains(mode, "fetch") || mode == "parent deadline" {
					return superserveSandbox{}, nativeErr
				}
				return sb, nil
			}}
			b.newClient = func(core.Config, core.Runtime) (superserveClient, error) { return api, nil }
			wait := mode == "ready before cancellation" || mode == "wait terminal" || mode == "deadline fetch" || mode == "parent deadline"
			view, err := b.Status(ctx, core.StatusRequest{ID: leaseID, Wait: wait, WaitTimeout: time.Millisecond})
			switch mode {
			case "ready", "ready before cancellation", "terminal":
				state := strings.ToLower(strings.TrimSpace(sb.Status))
				want := core.StatusView{ID: leaseID, Slug: "d1-status", Provider: providerName, TargetOS: "linux", State: state, ServerID: sb.ID, Pond: "d1-pond", Network: "public", Ready: mode != "terminal", Labels: map[string]string{"provider": providerName, "lease": leaseID, "pond": "d1-pond", "state": state, "slug": "d1-status"}}
				if err != nil || !reflect.DeepEqual(view, want) {
					t.Fatalf("view=%#v err=%v want=%#v", view, err, want)
				}
			default:
				if err == nil || !reflect.DeepEqual(view, core.StatusView{}) {
					t.Fatalf("view=%#v err=%v", view, err)
				}
				switch mode {
				case "raw error":
					if !errors.Is(err, nativeErr) {
						t.Fatalf("err=%v", err)
					}
				case "canceled fetch":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("err=%v", err)
					}
				case "parent deadline":
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("err=%v", err)
					}
				case "deadline fetch":
					if err.Error() != "timed out waiting for "+providerName+" sandbox "+sb.ID+" to become ready" {
						t.Fatalf("err=%v", err)
					}
				case "wait terminal":
					if err.Error() != providerName+" sandbox "+sb.ID+" entered terminal state \"stopped\" before becoming ready" {
						t.Fatalf("err=%v", err)
					}
				default:
					if core.ExitCodeForError(err, 1) != 4 {
						t.Fatalf("err=%v", err)
					}
				}
			}
			wantCalls := 1
			if mode == "foreign scope" || mode == "unclaimed" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("fetches=%d want=%d", calls, wantCalls)
			}

			// The claim verifier must reject local scope before fetching, and remote ownership after fetching.
			if strings.Contains(mode, "fetch") || mode == "parent deadline" {
				return
			}
			calls = 0
			_, verifyErr := shared.VerifySandboxClaim(ctx, leaseID, sb.ID, func(claim core.LeaseClaim) error { return validateSuperserveClaimScope(claim, api.BaseURL()) }, api.GetSandbox, validateSuperserveSandboxOwnership)
			if mode == "ready" || mode == "ready before cancellation" || mode == "terminal" || mode == "wait terminal" {
				if verifyErr != nil {
					t.Fatal(verifyErr)
				}
			}
			if mode == "missing labels" || mode == "foreign labels" || mode == "foreign scope" || mode == "unclaimed" {
				if verifyErr == nil || calls != wantCalls {
					t.Fatalf("verify err=%v fetches=%d want=%d", verifyErr, calls, wantCalls)
				}
			}
		})
	}
}
