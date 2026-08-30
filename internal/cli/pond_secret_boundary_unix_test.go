//go:build darwin || linux

package cli

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPondSecretBoundaryForwards(t *testing.T) {
	for _, route := range []bool{false, true} {
		for _, ending := range []string{"cancel", "exit"} {
			name := ending + "/managed"
			if route {
				name = ending + "/config-route"
			}
			t.Run(name, func(t *testing.T) {
				f := newForwardBoundaryFixture(t, route, "ready")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				port, _ := strconv.Atoi(boundaryPort(t))
				second, _ := strconv.Atoi(boundaryPort(t))
				for second == port {
					second, _ = strconv.Atoi(boundaryPort(t))
				}
				members := []pondMember{{Name: "peer", Lease: "cbx_boundary", SSH: f.target}}
				summary := pondMeshSummary{Forwards: []pondMeshForward{
					{Peer: "peer", LeaseID: "cbx_boundary", LocalPort: port, RemotePort: 8080},
					{Peer: "peer", LeaseID: "cbx_boundary", LocalPort: second, RemotePort: 8081},
				}}
				result := make(chan error, 1)
				go func() {
					result <- runPondMeshForwards(ctx, pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard, Runner: pondMeshExecRunner{}}, members, summary)
				}()
				r := f.waitRecord(t)
				f.assertAliveBoundary(t, r)
				if len(r.Forwards) != 2 {
					t.Error("same-member forwards no longer share one SSH child")
				}
				if ending == "cancel" {
					cancel()
				} else {
					f.exit(t)
				}
				err := boundaryResult(t, result)
				if (ending == "cancel") != (err == nil) {
					t.Errorf("pond cancellation/natural-exit provenance changed: %v", err)
				}
				f.assertReapedBoundary(t, r)
			})
		}
	}
}

// Only discovery is synthetic. App.pondConnect uses its actual daemon runner,
// spawn, early-exit waiters, state publication, and disconnect implementation.
type forwardBoundaryProvider struct {
	testExternalProvider
	lease LeaseTarget
}

func (forwardBoundaryProvider) Name() string { return "forward-boundary" }
func (forwardBoundaryProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: "forward-boundary", Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{{OS: targetLinux}}, Features: FeatureSet{FeatureSSH}, Coordinator: CoordinatorNever}
}
func (forwardBoundaryProvider) ApplyFlags(*Config, *flag.FlagSet, any) error { return nil }
func (forwardBoundaryProvider) RegisterFlags(*flag.FlagSet, Config) any      { return nil }
func (p forwardBoundaryProvider) Configure(Config, Runtime) (Backend, error) {
	return forwardBoundaryBackend{SSHLeaseBackend: &pondMeshResolveRecordingBackend{}, lease: p.lease}, nil
}

type forwardBoundaryBackend struct {
	SSHLeaseBackend
	lease LeaseTarget
}

func (b forwardBoundaryBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.lease, nil
}
func (b forwardBoundaryBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return []LeaseView{b.lease.Server}, nil
}

func TestPondSecretBoundaryDaemon(t *testing.T) {
	for _, route := range []bool{false, true} {
		name := "managed"
		if route {
			name = "config-route"
		}
		t.Run(name, func(t *testing.T) {
			f := newForwardBoundaryFixture(t, route, "ready")
			lease := LeaseTarget{LeaseID: "cbx_boundary", SSH: f.target, Server: Server{
				Name: "peer", CloudID: "cbx_boundary", Provider: "forward-boundary",
				Labels: map[string]string{"pond": "boundary", "lease": "cbx_boundary", "slug": "peer", pondExposedPortsLabelKey: "8080-8081"},
			}}
			provider := forwardBoundaryProvider{lease: lease}
			if providerRegistry[provider.Name()] != nil {
				t.Fatal("fixture provider name already registered")
			}
			providerRegistry[provider.Name()] = provider
			t.Cleanup(func() { delete(providerRegistry, provider.Name()) })
			if err := claimLeaseForRepoProviderScopePond(lease.LeaseID, "peer", provider.Name(), "", "boundary", f.root, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := app.pondConnect(ctx, []string{"boundary", "--provider", provider.Name(), "--export"}); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatal("name-first --export did not detach before the startup deadline")
			}
			r := f.waitRecord(t)
			t.Cleanup(func() { _, _ = stopPondMeshDaemonState(f.home, "boundary") })
			cancel()
			if err := syscall.Kill(r.PID, 0); err != nil {
				t.Fatal("export daemon did not survive caller return/cancellation")
			}
			f.assertDetachedBoundary(t, r)
			if len(r.Forwards) != 2 {
				t.Error("export daemon did not group both member forwards")
			}
			state, err := os.ReadFile(filepath.Join(f.home, pondMeshHostsRoot, "boundary", pondMeshDaemonFileName))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(state), forwardBoundaryUser) {
				t.Error("pond daemon state persisted the synthetic credential in its command field")
			}
			stopped, err := stopPondMeshDaemonState(f.home, "boundary")
			if err != nil || stopped != 1 {
				t.Fatalf("disconnect must stop the matching daemon: stopped=%d err=%v", stopped, err)
			}
			f.assertReapedBoundary(t, r, true)
		})
	}
}
