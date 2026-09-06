package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	claimRoutingUsableProvider     = "claim-routing-usable-test"
	claimRoutingConfiguredProvider = "claim-routing-configured-test"
	claimRoutingExplicitProvider   = "claim-routing-explicit-test"
	claimRoutingUnusableProvider   = "claim-routing-unusable-test"
)

func init() {
	RegisterProvider(claimRoutingCommandProvider{name: claimRoutingUsableProvider})
	RegisterProvider(claimRoutingCommandProvider{name: claimRoutingConfiguredProvider})
	RegisterProvider(claimRoutingCommandProvider{name: claimRoutingExplicitProvider})
	RegisterProvider(claimRoutingCommandProvider{name: claimRoutingUnusableProvider, configureErr: errors.New("configured provider credentials are unavailable")})
}

type claimRoutingCommandProvider struct {
	name         string
	configureErr error
}

func (p claimRoutingCommandProvider) Name() string      { return p.name }
func (p claimRoutingCommandProvider) Aliases() []string { return nil }
func (p claimRoutingCommandProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        p.name,
		Family:      "claim-routing-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Coordinator: CoordinatorNever,
	}
}
func (claimRoutingCommandProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (claimRoutingCommandProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p claimRoutingCommandProvider) Configure(Config, Runtime) (Backend, error) {
	if p.configureErr != nil {
		return nil, p.configureErr
	}
	return claimRoutingStatusBackend{spec: p.Spec()}, nil
}

type claimRoutingStatusBackend struct {
	spec ProviderSpec
}

func (b claimRoutingStatusBackend) Spec() ProviderSpec { return b.spec }
func (b claimRoutingStatusBackend) Warmup(context.Context, WarmupRequest) error {
	return nil
}
func (b claimRoutingStatusBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, nil
}
func (b claimRoutingStatusBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b claimRoutingStatusBackend) Status(_ context.Context, req StatusRequest) (StatusView, error) {
	return StatusView{
		ID:       req.ID,
		Provider: b.spec.Name,
		TargetOS: targetLinux,
		State:    "ready",
		Ready:    true,
	}, nil
}
func (b claimRoutingStatusBackend) Stop(context.Context, StopRequest) error {
	return errors.New("stop provider=" + b.spec.Name)
}
func (b claimRoutingStatusBackend) Acquire(_ context.Context, req AcquireRequest) (LeaseTarget, error) {
	return b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID})
}
func (b claimRoutingStatusBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	leaseID := firstNonBlank(req.ID, "cbx_claim_routing")
	return LeaseTarget{
		LeaseID: leaseID,
		Server: Server{
			Provider: b.spec.Name,
			CloudID:  leaseID,
			Status:   "ready",
			Labels:   map[string]string{"lease": leaseID, "provider": b.spec.Name, "state": "ready"},
		},
		SSH: SSHTarget{Host: "claim-routing.example.test", User: "runner", Port: "22", TargetOS: targetLinux},
	}, nil
}
func (claimRoutingStatusBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	return req.Lease.Server, nil
}
func (claimRoutingStatusBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}

func TestStatusAndInspectRouteImplicitIdentifiersThroughLocalClaims(t *testing.T) {
	commands := []struct {
		name string
		run  func(App, context.Context, []string) error
	}{
		{name: "status", run: func(app App, ctx context.Context, args []string) error { return app.status(ctx, args) }},
		{name: "inspect", run: func(app App, ctx context.Context, args []string) error { return app.inspect(ctx, args) }},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			t.Run("implicit exact lease id", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
				const leaseID = "cbx_1293aa000001"
				mustWriteClaimRoutingTestClaim(t, leaseID, "exact-lease", claimRoutingUsableProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000002", leaseID, claimRoutingExplicitProvider)

				output, err := runClaimRoutingCommand(command.run, []string{"--id", leaseID})
				if err != nil {
					t.Fatalf("%s exact id: %v", command.name, err)
				}
				assertClaimRoutingProvider(t, output, claimRoutingUsableProvider)
			})

			t.Run("missing canonical id does not select slug provider", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000002", "cbx-1293aa000001", claimRoutingUsableProvider)

				output, err := runClaimRoutingCommand(command.run, []string{"--id", "cbx_1293aa000001"})
				if err != nil {
					t.Fatalf("%s missing canonical id: %v", command.name, err)
				}
				assertClaimRoutingProvider(t, output, claimRoutingConfiguredProvider)
			})

			t.Run("implicit unambiguous slug", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000003", "Claimed Slug", claimRoutingUsableProvider)

				output, err := runClaimRoutingCommand(command.run, []string{"--id", "claimed-slug"})
				if err != nil {
					t.Fatalf("%s slug: %v", command.name, err)
				}
				assertClaimRoutingProvider(t, output, claimRoutingUsableProvider)
			})

			t.Run("explicit provider precedence", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000004", "shared-slug", claimRoutingUsableProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000005", "shared-slug", claimRoutingConfiguredProvider)

				output, err := runClaimRoutingCommand(command.run, []string{"--provider", claimRoutingExplicitProvider, "--id", "shared-slug"})
				if err != nil {
					t.Fatalf("%s explicit provider: %v", command.name, err)
				}
				assertClaimRoutingProvider(t, output, claimRoutingExplicitProvider)
			})

			t.Run("ambiguous slug", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000006", "ambiguous-slug", claimRoutingUsableProvider)
				mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000007", "ambiguous-slug", claimRoutingExplicitProvider)

				_, err := runClaimRoutingCommand(command.run, []string{"--id", "ambiguous-slug"})
				if err == nil || !strings.Contains(err.Error(), "lease identifier ambiguous-slug matches local claims from multiple providers; use a canonical lease id or pass --provider") {
					t.Fatalf("%s ambiguous slug error=%v", command.name, err)
				}
			})

			t.Run("missing claim configured fallback", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)

				output, err := runClaimRoutingCommand(command.run, []string{"--id", "missing-claim"})
				if err != nil {
					t.Fatalf("%s missing claim: %v", command.name, err)
				}
				assertClaimRoutingProvider(t, output, claimRoutingConfiguredProvider)
			})

			t.Run("claim bypasses unusable configured provider", func(t *testing.T) {
				setupClaimRoutingCommandTest(t, claimRoutingUnusableProvider)
				const leaseID = "cbx_1293aa000008"
				mustWriteClaimRoutingTestClaim(t, leaseID, "usable-claim", claimRoutingUsableProvider)

				output, err := runClaimRoutingCommand(command.run, []string{"--id", leaseID})
				if err != nil {
					t.Fatalf("%s claim-routed usable provider: %v", command.name, err)
				}
				assertClaimRoutingProvider(t, output, claimRoutingUsableProvider)

				if _, err := runClaimRoutingCommand(command.run, []string{"--provider", claimRoutingUnusableProvider, "--id", leaseID}); err == nil || !strings.Contains(err.Error(), "configured provider credentials are unavailable") {
					t.Fatalf("%s explicit unusable provider error=%v", command.name, err)
				}
			})
		})
	}
}

func TestStopRoutesImplicitIdentifiersThroughLocalClaims(t *testing.T) {
	for _, tt := range []struct {
		name       string
		leaseID    string
		slug       string
		identifier string
	}{
		{name: "exact lease id", leaseID: "cbx_1293aa000011", slug: "stop-exact", identifier: "cbx_1293aa000011"},
		{name: "unambiguous slug", leaseID: "cbx_1293aa000012", slug: "Stop Slug", identifier: "stop-slug"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupClaimRoutingCommandTest(t, claimRoutingUnusableProvider)
			mustWriteClaimRoutingTestClaim(t, tt.leaseID, tt.slug, claimRoutingUsableProvider)

			_, err := runClaimRoutingCommand(func(app App, ctx context.Context, args []string) error {
				return app.stop(ctx, args)
			}, []string{"--id", tt.identifier})
			if err == nil || !strings.Contains(err.Error(), "stop provider="+claimRoutingUsableProvider) {
				t.Fatalf("stop error=%v, want claim-routed provider", err)
			}
		})
	}
}

func TestLoadLeaseTargetConfigKeepsProviderResourceIdentifiersOnConfiguredProvider(t *testing.T) {
	setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
	mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000009", "native-vm-name", claimRoutingUsableProvider)
	defaults := defaultConfig()
	fs := newFlagSet("provider resource", io.Discard)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, defaults.Provider, targetFlags, networkFlags, leaseTargetConfigOptions{
		LeaseID:            "native-vm-name",
		ProviderResourceID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != claimRoutingConfiguredProvider {
		t.Fatalf("provider=%q want configured provider %q", cfg.Provider, claimRoutingConfiguredProvider)
	}
	if cfg.providerSelectionSource != defaults.providerSelectionSource {
		t.Fatalf("provider source=%q want %q", cfg.providerSelectionSource, defaults.providerSelectionSource)
	}
}

func TestLoadLeaseTargetConfigClaimRoutingSetsLeaseContextProvenance(t *testing.T) {
	setupClaimRoutingCommandTest(t, claimRoutingConfiguredProvider)
	mustWriteClaimRoutingTestClaim(t, "cbx_1293aa000010", "provenance-claim", claimRoutingUsableProvider)
	defaults := defaultConfig()
	fs := newFlagSet("claim provenance", io.Discard)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, defaults.Provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: "provenance-claim"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != claimRoutingUsableProvider {
		t.Fatalf("provider=%q want claim provider %q", cfg.Provider, claimRoutingUsableProvider)
	}
	if cfg.providerSelectionSource != providerSelectionLeaseContext {
		t.Fatalf("provider source=%q want %q", cfg.providerSelectionSource, providerSelectionLeaseContext)
	}
}

func setupClaimRoutingCommandTest(t *testing.T, configuredProvider string) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("CRABBOX_PROVIDER", "")
	configPath := filepath.Join(t.TempDir(), "crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("provider: "+configuredProvider+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
}

func mustWriteClaimRoutingTestClaim(t *testing.T, leaseID, slug, provider string) {
	t.Helper()
	if err := claimLeaseForRepoProvider(leaseID, slug, provider, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
}

func runClaimRoutingCommand(run func(App, context.Context, []string) error, args []string) (string, error) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err := run(app, context.Background(), args)
	return stdout.String(), err
}

func assertClaimRoutingProvider(t *testing.T, output, provider string) {
	t.Helper()
	if !strings.Contains(output, "provider="+provider) {
		t.Fatalf("output=%q, want provider=%s", output, provider)
	}
}
