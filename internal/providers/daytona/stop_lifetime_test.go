package daytona

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

type observingDaytonaContextAPI struct {
	fixedDaytonaDeletionAPI
	beforeGet func(context.Context) error
}

func (a *observingDaytonaContextAPI) GetSandbox(ctx context.Context, id string) (*api.Sandbox, error) {
	if err := a.beforeGet(ctx); err != nil {
		return nil, err
	}
	return a.fixedDaytonaDeletionAPI.GetSandbox(ctx, id)
}

func observeDaytonaDeletionContext(t *testing.T, beforeGet func(context.Context) error) {
	t.Helper()
	original := newDaytonaClient
	newDaytonaClient = func(cfg core.Config, rt core.Runtime) (daytonaAPI, error) {
		client, err := original(cfg, rt)
		if err != nil {
			return nil, err
		}
		return &observingDaytonaContextAPI{fixedDaytonaDeletionAPI: client.(fixedDaytonaDeletionAPI), beforeGet: beforeGet}, nil
	}
	t.Cleanup(func() { newDaytonaClient = original })
}

func TestDaytonaStopAndCleanupContextOwnership(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, operation := range []string{"stop", "release", "background", "without-cancel"} {
			for _, deadline := range []bool{false, true} {
				if operation != "stop" && deadline {
					continue
				}
				t.Run(fmt.Sprintf("fixed=%t/%s/deadline=%t", fixed, operation, deadline), func(t *testing.T) {
					_, b, req := newFixedDaytonaFixture(t)
					if !fixed {
						req.RequestedLeaseID = ""
					}
					lease, err := b.Acquire(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					if deadline {
						cancel()
						ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
					}
					if operation == "background" {
						cancel()
						ctx = context.Background()
					} else if operation == "without-cancel" {
						ctx = context.WithoutCancel(ctx)
					}
					defer cancel()
					wantDeadline, wantBound := ctx.Deadline()
					reads := 0
					observeDaytonaDeletionContext(t, func(actual context.Context) error {
						reads++
						gotDeadline, gotBound := actual.Deadline()
						if operation == "stop" {
							if gotBound != wantBound || gotBound && !gotDeadline.Equal(wantDeadline) {
								t.Errorf("explicit stop replaced caller deadline: got=%v bounded=%t want=%v bounded=%t", gotDeadline, gotBound, wantDeadline, wantBound)
							}
						} else if !gotBound || time.Until(gotDeadline) > 30*time.Second {
							t.Errorf("non-cancelable stop or automatic release lost its bounded cleanup lifetime: deadline=%v bounded=%t", gotDeadline, gotBound)
						}
						return nil
					})
					if operation != "release" {
						err = b.Stop(ctx, core.StopRequest{ID: lease.LeaseID})
					} else {
						err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease})
					}
					if err != nil || reads == 0 {
						t.Fatalf("explicit %s did not finish confirmed cleanup: reads=%d err=%v", operation, reads, err)
					}
				})
			}
		}
	}
}

func TestDaytonaInterruptedExplicitStopRetainsAcknowledgment(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			f.deletionPending = true
			ctx, cancel := context.WithCancel(context.Background())
			wantErr := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
				wantErr = context.DeadlineExceeded
			}
			defer cancel()
			observeDaytonaDeletionContext(t, func(actual context.Context) error {
				f.mu.Lock()
				deletionSubmitted := f.deletes != 0
				f.mu.Unlock()
				if !deletionSubmitted {
					return nil
				}
				if !deadline {
					cancel()
				}
				<-actual.Done()
				return actual.Err()
			})
			err = b.Stop(ctx, core.StopRequest{ID: lease.LeaseID})
			if !errors.Is(err, wantErr) {
				t.Fatalf("stop did not honor caller termination: %v", err)
			}
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if readErr != nil || !exists || claim.FixedCreateIntent.State == "released" ||
				claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != lease.Server.CloudID || f.deletes != 1 {
				t.Fatalf("interrupted deletion lost acknowledgment or retired custody: claim=%+v err=%v", claim, readErr)
			}
		})
	}
}
