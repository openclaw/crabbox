package shared

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestD2LocalInstanceViewsUsesClaimIDForDisplayOnly(t *testing.T) {
	instances := []string{"foreign", "claimed", "crabbox-unclaimed", "label-only"}
	claims := map[string]core.LeaseClaim{
		"claimed":    {LeaseID: "cbx_d2", Labels: map[string]string{"lease": "different"}},
		"label-only": {Labels: map[string]string{"lease": "cbx_label_only"}},
	}
	var projected []string
	project := func(name string, claim core.LeaseClaim, cfg core.Config) core.Server {
		projected = append(projected, name)
		if cfg.Provider != "test" {
			t.Fatal("projection lost config")
		}
		return core.Server{Name: name, Labels: map[string]string{"lease": claim.LeaseID}}
	}
	views := LocalInstanceViews(instances, claims, core.Config{Provider: "test"}, func(name string) string { return name }, project)
	if !reflect.DeepEqual(projected, []string{"claimed", "crabbox-unclaimed"}) || len(views) != 2 || views[0].Labels["lease"] != "cbx_d2" || views[1].Labels["lease"] != "" {
		t.Fatalf("projected=%v views=%v", projected, views)
	}
	if empty := LocalInstanceViews(nil, claims, core.Config{}, func(name string) string { return name }, project); empty == nil || len(empty) != 0 {
		t.Fatalf("empty inventory must return an allocated empty slice: %#v", empty)
	}
}

func TestD2ClaimIdleGraceIsIndependentOfTTLAndParsingPolicy(t *testing.T) {
	claim := core.LeaseClaim{
		LastUsedAt: "2026-09-01T01:00:00+01:00", IdleTimeoutSeconds: 60,
		Labels: map[string]string{"expires_at": "1", "keep": "true"},
	}
	deadline := time.Date(2026, 9, 1, 2, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		at      time.Time
		expired bool
	}{{deadline, false}, {deadline.Add(time.Nanosecond), true}} {
		expired, reason := ClaimIdleExpiredAfterGrace(claim, tc.at, 2*time.Hour)
		wantReason := "claim active"
		if tc.expired {
			wantReason = "claim expired"
		}
		if expired != tc.expired || reason != wantReason {
			t.Fatalf("expiry=(%v, %q), want (%v, %q)", expired, reason, tc.expired, wantReason)
		}
	}
	claim.LastUsedAt = " " + claim.LastUsedAt
	if expired, _ := ClaimIdleExpiredAfterGrace(claim, deadline.Add(time.Hour), 0); expired {
		t.Fatal("helper must not trim a caller's timestamp")
	}
}

func TestD2ClaimIdleGraceRejectsUnrepresentableTimeouts(t *testing.T) {
	lastUsed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		seconds int64
	}{
		{"disabled", 0},
		{"negative", -1},
		{"negative wraps positive", -18446744073},
		{"above maximum duration", 9223372037},
		{"positive wraps positive", 18446744074},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seconds := int(tc.seconds)
			if int64(seconds) != tc.seconds {
				t.Skip("persisted timeout is not representable on this architecture")
			}
			claim := core.LeaseClaim{LastUsedAt: lastUsed.Format(time.RFC3339), IdleTimeoutSeconds: seconds}
			expired, reason := ClaimIdleExpiredAfterGrace(claim, lastUsed.AddDate(400, 0, 0), 12*time.Hour)
			if expired || reason != "claim active" {
				t.Fatalf("expiry=(%v, %q), want (false, claim active)", expired, reason)
			}
		})
	}
}

func TestD2ClaimIdleGraceMaximumDurationBoundary(t *testing.T) {
	maxSeconds := int64(math.MaxInt64) / int64(time.Second)
	seconds := int(maxSeconds)
	if int64(seconds) != maxSeconds {
		t.Skip("maximum duration seconds are not representable on this architecture")
	}
	lastUsed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	grace := 12 * time.Hour
	deadline := lastUsed.Add(time.Duration(maxSeconds) * time.Second).Add(grace)
	claim := core.LeaseClaim{LastUsedAt: lastUsed.Format(time.RFC3339), IdleTimeoutSeconds: seconds}
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			expired, reason := ClaimIdleExpiredAfterGrace(claim, deadline.Add(offset), grace)
			wantExpired := offset > 0
			wantReason := "claim active"
			if wantExpired {
				wantReason = "claim expired"
			}
			if expired != wantExpired || reason != wantReason {
				t.Fatalf("expiry=(%v, %q), want (%v, %q)", expired, reason, wantExpired, wantReason)
			}
		})
	}
}

func TestD2RetryLeaseLookupReturnsSuccessAfterTransientErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		calls := 0
		got, err := RetryLeaseLookup(ctx, core.Config{Provider: "test"}, "cbx_d2", time.Second, func(gotCtx context.Context, cfg core.Config, id string) (string, error) {
			calls++
			if gotCtx != ctx || cfg.Provider != "test" || id != "cbx_d2" {
				t.Fatal("lookup lost context or scope")
			}
			if calls < 3 {
				return "partial", errors.New("transient")
			}
			cancel()
			return "found", nil
		})
		if err != nil || got != "found" || calls != 3 || time.Since(start) != 500*time.Millisecond {
			t.Fatalf("got=%q err=%v calls=%d elapsed=%s", got, err, calls, time.Since(start))
		}
	})
}

func TestD2RetryLeaseLookupObservesBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := RetryLeaseLookup(ctx, core.Config{}, "", time.Hour, func(context.Context, core.Config, string) (int, error) {
		return 42, nil
	})
	if got != 42 || err != nil {
		t.Fatalf("got=%d err=%v", got, err)
	}
}
