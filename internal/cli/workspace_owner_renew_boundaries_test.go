package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceOwnerRenewalConfirmedDenialsAreNotRetried(t *testing.T) {
	for _, response := range []string{"MISMATCH", "EXPIRED", "AMBIGUOUS", "CHILD", "BUSY", "unrecognized"} {
		for _, failedTransport := range []bool{false, true} {
			name := response
			if failedTransport {
				name += "/transport-error"
			}
			t.Run(name, func(t *testing.T) {
				calls := 0
				owner := &workspaceOwner{
					ctx: t.Context(), stop: make(chan struct{}), confirmed: time.Now(), ttl: time.Minute,
					transport: workspaceOwnerTransportFunc(func(context.Context, workspaceOwnerRemoteRequest) (string, error) {
						calls++
						if failedTransport {
							return response, errors.New("synthetic transport failure")
						}
						return response, nil
					}),
				}
				if err := owner.renewWithinOwnership(time.Second); err == nil || calls != 1 {
					t.Fatalf("response=%q calls=%d err=%v; want terminal denial", response, calls, err)
				}
			})
		}
	}
}

func TestWorkspaceOwnerRenewalRefusesRetryOutsideConfirmedWindow(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no confirmation", true: "insufficient time"}[confirmed], func(t *testing.T) {
			calls := 0
			original := errors.New("synthetic transport failure")
			owner := &workspaceOwner{
				ctx: t.Context(), stop: make(chan struct{}), ttl: time.Minute,
				transport: workspaceOwnerTransportFunc(func(context.Context, workspaceOwnerRemoteRequest) (string, error) {
					calls++
					return "", original
				}),
			}
			if confirmed {
				owner.confirmed = time.Now().Add(-owner.ttl + workspaceOwnerRenewMargin + 100*time.Millisecond)
			}
			err := owner.renewWithinOwnership(time.Second)
			if !errors.Is(err, original) || calls != 1 {
				t.Fatalf("calls=%d err=%v; want original failure without retry", calls, err)
			}
			if confirmed && !strings.Contains(err.Error(), "no retry fits") {
				t.Fatalf("err=%v; want confirmed-window refusal", err)
			}
		})
	}
}
