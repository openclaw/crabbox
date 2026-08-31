package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

const warmupFixedLeaseProviderName = "warmup-fixed-lease-test"

type warmupFixedLeaseProvider struct {
	backend Backend
}

func (warmupFixedLeaseProvider) Name() string      { return warmupFixedLeaseProviderName }
func (warmupFixedLeaseProvider) Aliases() []string { return nil }
func (warmupFixedLeaseProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: warmupFixedLeaseProviderName, Kind: ProviderKindDelegatedRun,
		Targets: []TargetSpec{{OS: targetLinux}}, Coordinator: CoordinatorNever,
	}
}
func (warmupFixedLeaseProvider) RegisterFlags(*flag.FlagSet, Config) any      { return nil }
func (warmupFixedLeaseProvider) ApplyFlags(*Config, *flag.FlagSet, any) error { return nil }
func (p warmupFixedLeaseProvider) Configure(Config, Runtime) (Backend, error) {
	return p.backend, nil
}

type warmupFixedLeaseBackend struct {
	requests []WarmupRequest
}

func (*warmupFixedLeaseBackend) Spec() ProviderSpec { return (warmupFixedLeaseProvider{}).Spec() }
func (b *warmupFixedLeaseBackend) Warmup(_ context.Context, req WarmupRequest) error {
	b.requests = append(b.requests, req)
	return nil
}
func (*warmupFixedLeaseBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, errors.New("unexpected run")
}
func (*warmupFixedLeaseBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, errors.New("unexpected list")
}
func (*warmupFixedLeaseBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, errors.New("unexpected status")
}
func (*warmupFixedLeaseBackend) Stop(context.Context, StopRequest) error {
	return errors.New("unexpected stop")
}

// The capability is advertised through an optional interface, so the capable and
// declining variants must be distinct types rather than one configurable method.
type warmupFixedLeaseCapableBackend struct{ *warmupFixedLeaseBackend }

func (warmupFixedLeaseCapableBackend) SupportsRequestedLeaseID() bool { return true }

type warmupFixedLeaseDecliningBackend struct{ *warmupFixedLeaseBackend }

func (warmupFixedLeaseDecliningBackend) SupportsRequestedLeaseID() bool { return false }

func setupWarmupFixedLeaseDelegated(t *testing.T, capability string) (App, *warmupFixedLeaseBackend) {
	t.Helper()
	clearConfigEnv(t)
	withTempClaims(t, nil)
	t.Chdir(t.TempDir())
	recorder := &warmupFixedLeaseBackend{}
	var backend Backend = recorder
	switch capability {
	case "capable":
		backend = warmupFixedLeaseCapableBackend{recorder}
	case "declining":
		backend = warmupFixedLeaseDecliningBackend{recorder}
	}
	if providerRegistry[warmupFixedLeaseProviderName] != nil {
		t.Fatal("test provider already registered")
	}
	RegisterProvider(warmupFixedLeaseProvider{backend: backend})
	t.Cleanup(func() { delete(providerRegistry, warmupFixedLeaseProviderName) })
	return App{Stdout: io.Discard, Stderr: io.Discard}, recorder
}

func TestWarmupPropagatesRequestedLeaseIDToDelegatedBackend(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	for _, tt := range []struct {
		name       string
		capability string
		args       []string
		wantErr    string
		wantID     string
		wantSlug   string
	}{
		{
			name: "capable receives the requested id verbatim", capability: "capable",
			args: []string{"--lease-id", leaseID}, wantID: leaseID,
		},
		{
			// The delegated WarmupRequest literal is keyed, so dropping a sibling
			// field still compiles. Pin one alongside the new field.
			name: "capable receives sibling warmup fields too", capability: "capable",
			args:     []string{"--lease-id", leaseID, "--slug", "sibling-fields"},
			wantID:   leaseID,
			wantSlug: "sibling-fields",
		},
		{
			// The SSH path trims before handing the ID to Acquire; delegated warmup
			// must agree so the same input names the same identity on both paths.
			name: "capable receives the trimmed requested id", capability: "capable",
			args: []string{"--lease-id", "  " + leaseID + "  "}, wantID: leaseID,
		},
		{
			name: "capable without a requested id", capability: "capable",
			args: nil, wantID: "",
		},
		{
			// warmup gates on a non-empty trimmed value, so an explicitly empty or
			// blank flag is indistinguishable from an absent one and a NON-fixed
			// lease is allocated. `checkpoint fork` instead gates on whether the flag
			// was set at all and rejects the same input. These two cases pin
			// warmup's existing behaviour so aligning the two gates later is a
			// deliberate change with a visible test diff, not an accident.
			name: "capable with an empty requested id", capability: "capable",
			args: []string{"--lease-id="}, wantID: "",
		},
		{
			name: "capable with a blank requested id", capability: "capable",
			args: []string{"--lease-id", "   "}, wantID: "",
		},
		{
			name: "capable rejects a non-canonical requested id", capability: "capable",
			args:    []string{"--lease-id", "cbx_NOT_CANONICAL"},
			wantErr: "--lease-id must match cbx_<12 lowercase hex characters>",
		},
		{
			name: "silent backend rejects a requested id", capability: "silent",
			args:    []string{"--lease-id", leaseID},
			wantErr: "provider=" + warmupFixedLeaseProviderName + " does not support fixed idempotent lease IDs",
		},
		{
			name: "declining backend rejects a requested id", capability: "declining",
			args:    []string{"--lease-id", leaseID},
			wantErr: "provider=" + warmupFixedLeaseProviderName + " does not support fixed idempotent lease IDs",
		},
		{
			name: "silent backend without a requested id", capability: "silent",
			args: nil, wantID: "",
		},
		{
			name: "declining backend with a blank requested id", capability: "declining",
			args: []string{"--lease-id", "   "}, wantID: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app, recorder := setupWarmupFixedLeaseDelegated(t, tt.capability)
			args := append([]string{"--provider", warmupFixedLeaseProviderName}, tt.args...)
			err := app.warmup(context.Background(), args)
			if tt.wantErr != "" {
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, tt.wantErr) {
					t.Fatalf("warmup error=%v, want exit 2 containing %q", err, tt.wantErr)
				}
				if len(recorder.requests) != 0 {
					t.Fatalf("rejected warmup still reached the backend: %#v", recorder.requests)
				}
				return
			}
			if err != nil {
				t.Fatalf("warmup error=%v", err)
			}
			if len(recorder.requests) != 1 {
				t.Fatalf("backend saw %d warmup requests, want 1", len(recorder.requests))
			}
			got := recorder.requests[0]
			if got.RequestedLeaseID != tt.wantID {
				t.Fatalf("RequestedLeaseID=%q, want %q", got.RequestedLeaseID, tt.wantID)
			}
			if got.RequestedSlug != tt.wantSlug {
				t.Fatalf("RequestedSlug=%q, want %q", got.RequestedSlug, tt.wantSlug)
			}
			if got.BeforeComplete == nil {
				t.Fatal("BeforeComplete was not carried onto the delegated warmup request")
			}
		})
	}
}
