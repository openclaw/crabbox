package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestProvisionServerCandidatesFailureBoundaries(t *testing.T) {
	rejection := errors.New("capacity rejected")
	admission := errors.New("admission rejected")
	for _, tc := range []struct {
		name        string
		prepareFail bool
		retry       bool
		succeed     bool
		wantEvents  []string
		wantConfig  string
		wantError   string
	}{
		{"success after fallback", false, true, true, []string{"prepare:first", "create:first", "retry", "log:second", "prepare:second", "create:second"}, "second", ""},
		{"exhaustion", false, true, false, []string{"prepare:first", "create:first", "retry", "log:second", "prepare:second", "create:second", "retry"}, "original", "first: capacity rejected; second: capacity rejected"},
		{"terminal create", false, false, false, []string{"prepare:first", "create:first", "retry"}, "first", "first: capacity rejected"},
		{"admission after fallback", true, true, false, []string{"prepare:first", "create:first", "retry", "log:second", "prepare:second"}, "second", "admission rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			candidates := []ProvisioningCandidate{
				{Config: Config{ServerType: "first"}, FailureLabel: "first"},
				{Config: Config{ServerType: "second"}, FailureLabel: "second", FallbackMessage: "log:second"},
			}
			server, cfg, err := ProvisionServerCandidates(context.Background(), Config{ServerType: "original"}, candidates, ServerProvisioner{
				Prepare: func(_ context.Context, cfg Config) error {
					events = append(events, "prepare:"+cfg.ServerType)
					if tc.prepareFail && cfg.ServerType == "second" {
						return admission
					}
					return nil
				},
				Create: func(_ context.Context, cfg Config) (Server, error) {
					events = append(events, "create:"+cfg.ServerType)
					if tc.succeed && cfg.ServerType == "second" {
						return Server{CloudID: "allocated"}, nil
					}
					return Server{CloudID: "partial-not-authority"}, rejection
				},
				CanRetry: func(err error) bool {
					if err != rejection {
						t.Fatalf("classifier received rewritten error: %v", err)
					}
					events = append(events, "retry")
					return tc.retry
				},
			}, func(format string, args ...any) { events = append(events, fmt.Sprintf(format, args...)) })
			if !reflect.DeepEqual(events, tc.wantEvents) || cfg.ServerType != tc.wantConfig {
				t.Fatalf("events=%v config=%s", events, cfg.ServerType)
			}
			if tc.wantError == "" {
				if err != nil || server.CloudID != "allocated" {
					t.Fatalf("server=%+v err=%v", server, err)
				}
			} else if err == nil || err.Error() != tc.wantError || server.CloudID != "" {
				t.Fatalf("server=%+v err=%v", server, err)
			}
			if tc.prepareFail && err != admission {
				t.Fatalf("admission error identity lost: %v", err)
			}
			if !tc.retry && !errors.Is(err, rejection) {
				t.Fatalf("single create error identity lost: %v", err)
			}
		})
	}
}

func TestProvisionServerCandidatesRequiresRetryPermission(t *testing.T) {
	calls := 0
	rejection := errors.New("unsettled allocation")
	_, cfg, err := ProvisionServerCandidates(context.Background(), Config{}, []ProvisioningCandidate{
		{Config: Config{ServerType: "first"}, FailureLabel: "first"},
		{Config: Config{ServerType: "second"}, FailureLabel: "second"},
	}, ServerProvisioner{Create: func(context.Context, Config) (Server, error) {
		calls++
		return Server{}, rejection
	}}, nil)
	if calls != 1 || cfg.ServerType != "first" || !errors.Is(err, rejection) {
		t.Fatalf("calls=%d cfg=%s err=%v", calls, cfg.ServerType, err)
	}
}

func TestCloudProvisioningPlansKeepExactTypesAndMarketOrder(t *testing.T) {
	for _, provider := range []string{"gcp", "azure"} {
		t.Run(provider, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = provider
			cfg.ServerType, cfg.ServerTypeExplicit = "exact-type", true
			cfg.Capacity.Market, cfg.Capacity.Fallback = "spot", "on-demand"
			cfg.GCPZone = "zone-a"
			cfg.Capacity.AvailabilityZones = []string{"zone-b", "zone-a"}
			plan := azureProvisioningPlan
			want := []string{"spot/exact-type", "on-demand/exact-type"}
			if provider == "gcp" {
				plan = gcpProvisioningPlan
				want = []string{"spot/zone-a/exact-type", "spot/zone-b/exact-type", "on-demand/zone-a/exact-type", "on-demand/zone-b/exact-type"}
			}
			attempts, err := plan(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for i, attempt := range attempts {
				got = append(got, attempt.Config.Capacity.Market+"/"+strings.TrimPrefix(attempt.FailureLabel, "on-demand "))
				if attempt.Config.ServerType != "exact-type" || (i == 0) != (attempt.FallbackMessage == "") {
					t.Fatalf("unexpected attempt %d: type=%s message=%s", i, attempt.Config.ServerType, attempt.FallbackMessage)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%v want=%v", got, want)
			}
			cfg.Capacity.Fallback = "none"
			attempts, err = plan(cfg)
			if err != nil || len(attempts) != len(want)/2 {
				t.Fatalf("disabled market fallback: attempts=%d err=%v", len(attempts), err)
			}
		})
	}
}
