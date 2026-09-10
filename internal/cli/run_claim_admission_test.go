package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type runClaimAdmissionTestProvider struct{ runEnvProfileTestProvider }

func (runClaimAdmissionTestProvider) Name() string { return "run-claim-admission-test" }
func (p runClaimAdmissionTestProvider) Spec() ProviderSpec {
	spec := p.runEnvProfileTestProvider.Spec()
	spec.Name = p.Name()
	return spec
}
func (runClaimAdmissionTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runClaimAdmissionBackend, nil
}

var runClaimAdmissionBackend *runClaimAdmissionTestBackend

func init() { RegisterProvider(runClaimAdmissionTestProvider{}) }

type runClaimAdmissionTestBackend struct {
	runEnvProfileTestBackend
	lease   LeaseTarget
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *runClaimAdmissionTestBackend) Resolve(ctx context.Context, _ ResolveRequest) (LeaseTarget, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return b.lease, nil
	case <-ctx.Done():
		return LeaseTarget{}, ctx.Err()
	}
}

func (b *runClaimAdmissionTestBackend) ResolveRunLeaseUnderClaim(ctx context.Context, req ResolveRequest, original LeaseClaim) (LeaseTarget, error) {
	return b.Resolve(ctx, req)
}

func TestRunClaimAdmissionCoexistsWithHeartbeat(t *testing.T) {
	lease, _ := setupRunClaimSnapshotTest(t)
	removeLeaseClaim(lease.LeaseID)
	lease.LeaseID = "cbx_123456789abc"
	provider := runClaimAdmissionTestProvider{}
	lease.Server.Provider = provider.Name()
	lease.Server.Labels = cloneStringMap(lease.Server.Labels)
	lease.Server.Labels["provider"] = provider.Name()
	lease.Server.Labels["lease"] = lease.LeaseID
	lease.Server.Labels["tailscale_ipv4"] = "127.0.0.1"
	sshPath := filepath.Join(filepath.Dir(os.Getenv("CRABBOX_FAKE_SSH_LOG")), "ssh")
	ssh, err := os.ReadFile(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	probeEntered, probeRelease := sshPath+".entered", sshPath+".release"
	t.Setenv("CRABBOX_ADMISSION_PROBE_ENTERED", probeEntered)
	t.Setenv("CRABBOX_ADMISSION_PROBE_RELEASE", probeRelease)
	ssh = bytes.Replace(ssh, []byte("decoded=\"\""), []byte(`if [ "$cmd" = 'exit 0' ]; then
 : > "$CRABBOX_ADMISSION_PROBE_ENTERED"
 while [ ! -f "$CRABBOX_ADMISSION_PROBE_RELEASE" ]; do /bin/sleep 0.01; done
fi
decoded=""`), 1)
	if err := os.WriteFile(sshPath, ssh, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Provider = provider.Name()
	if err := claimLeaseTargetForRepoConfig(lease.LeaseID, "claim-snapshot", cfg, lease.Server, lease.SSH, repo.Root, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	original, err := readLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	SetServerLeaseClaimSnapshot(&lease.Server, original, true)
	b := &runClaimAdmissionTestBackend{runEnvProfileTestBackend: runEnvProfileTestBackend{spec: provider.Spec()},
		lease: lease, entered: make(chan struct{}), release: make(chan struct{})}
	runClaimAdmissionBackend = b
	t.Cleanup(func() { runClaimAdmissionBackend = nil; removeLeaseClaim(lease.LeaseID) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var release sync.Once
	defer release.Do(func() { close(b.release) })
	heartbeatGate := make(chan struct{})
	var releaseHeartbeat sync.Once
	defer releaseHeartbeat.Do(func() { close(heartbeatGate) })
	runDone, heartbeatDone := make(chan error, 1), make(chan error, 1)
	runJoined, heartbeatJoined, heartbeatStarted := false, false, false
	heartbeatEntered := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		release.Do(func() { close(b.release) })
		releaseHeartbeat.Do(func() { close(heartbeatGate) })
		_ = os.WriteFile(probeRelease, nil, 0o600)
		if !runJoined {
			<-runDone
		}
		if heartbeatStarted && !heartbeatJoined {
			<-heartbeatDone
		}
	})
	var stdout, stderr bytes.Buffer
	go func() {
		runDone <- (App{Stdout: &stdout, Stderr: &stderr}).runCommand(ctx, []string{
			"--provider", provider.Name(), "--id", lease.LeaseID, "--keep", "--no-sync", "--no-hydrate", "--", "claim-admission-sentinel",
		})
	}()
	select {
	case <-b.entered:
	case err := <-runDone:
		runJoined = true
		t.Fatalf("run stopped before resolution: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	heartbeatStarted = true
	go func() {
		_, _, _, err := UpdateLeaseClaimTouchIfUnchangedAction(ctx, lease.LeaseID, original, time.Now(), nil, func() (Server, SSHTarget, bool, error) {
			close(heartbeatEntered)
			select {
			case <-heartbeatGate:
				return lease.Server, lease.SSH, true, nil
			case <-ctx.Done():
				return Server{}, SSHTarget{}, false, ctx.Err()
			}
		})
		heartbeatDone <- err
	}()
	claimPath, err := leaseClaimPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	// The old path lets heartbeat acquire while provider resolution is pending.
	// The repaired admission owns this same mutex from the original claim read.
	waitClaimWriter(t, claimPath)
	release.Do(func() { close(b.release) })
	for {
		if _, err := os.Stat(probeEntered); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("network probe did not start")
		case <-time.After(time.Millisecond):
		}
	}
	// Network selection runs after the old resolver captured its claim. An
	// admitted heartbeat now publishes before the old registration CAS.
	releaseHeartbeat.Do(func() { close(heartbeatGate) })
	var heartbeatErr error
	select {
	case <-heartbeatEntered:
		heartbeatErr = <-heartbeatDone
		heartbeatJoined = true
	default: // The new run admission already owns the lock.
	}
	if err := os.WriteFile(probeRelease, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runErr := <-runDone
	runJoined = true
	if !heartbeatJoined {
		heartbeatErr = <-heartbeatDone
		heartbeatJoined = true
	}
	if runErr != nil {
		t.Fatalf("run lost admission to concurrent heartbeat: %v\n%s", runErr, stderr.String())
	}
	if heartbeatErr != nil && !strings.Contains(heartbeatErr.Error(), "claim changed") {
		t.Fatalf("unexpected heartbeat failure: %v", heartbeatErr)
	}
	current, err := readLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	// A heartbeat that waited behind admission may lose its old CAS; its next
	// ordinary scheduled call must succeed on the current exact snapshot.
	if _, _, _, err := UpdateLeaseClaimTouchIfUnchangedAction(ctx, lease.LeaseID, current, time.Now(), nil, func() (Server, SSHTarget, bool, error) {
		return lease.Server, lease.SSH, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(os.Getenv("CRABBOX_FAKE_SSH_LOG"))
	if err != nil || !strings.Contains(string(log), "claim-admission-sentinel") {
		t.Fatalf("command was not delivered: read=%v", err)
	}
}

func TestRunClaimAdmissionPublishesOnlyValidatedCurrentOwner(t *testing.T) {
	for _, test := range []struct {
		name        string
		alter       func(*LeaseTarget)
		changeClaim func(*leaseClaim)
		admit       func(context.CancelFunc, *LeaseTarget) error
		wantError   string
		legacy      bool
	}{
		{name: "prepared endpoint", admit: func(_ context.CancelFunc, lease *LeaseTarget) error { lease.SSH.Port = "2200"; return nil }},
		{name: "failed admission", admit: func(context.CancelFunc, *LeaseTarget) error { return errors.New("admission rejected") }, wantError: "admission rejected"},
		{name: "cancelled admission", admit: func(cancel context.CancelFunc, _ *LeaseTarget) error { cancel(); return nil }, wantError: "context canceled"},
		{name: "different resource", alter: func(lease *LeaseTarget) { lease.Server.CloudID = "replacement-resource" }, wantError: "original claim"},
		{name: "reassigned repository", changeClaim: func(claim *leaseClaim) { claim.RepoRoot += "-replacement" }, wantError: "claimed by"},
		{name: "providerless legacy binding", changeClaim: func(claim *leaseClaim) { claim.Provider = "" }, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease, _ := setupRunClaimSnapshotTest(t)
			lease.LeaseID = "cbx_123456789abc"
			provider := runClaimAdmissionTestProvider{}
			lease.Server.Provider = provider.Name()
			lease.Server.Labels["provider"] = provider.Name()
			lease.Server.Labels["lease"] = lease.LeaseID
			repo, err := findRepo()
			if err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			cfg.Provider = provider.Name()
			if err := claimLeaseTargetForRepoConfig(lease.LeaseID, "claim-snapshot", cfg, lease.Server, lease.SSH, repo.Root, cfg.IdleTimeout, false); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { removeLeaseClaim(lease.LeaseID) })
			if test.changeClaim != nil {
				if err := mutateLeaseClaim(lease.LeaseID, func(claim *leaseClaim) error { test.changeClaim(claim); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			before, err := readLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if test.alter != nil {
				test.alter(&lease)
			}
			b := &runClaimAdmissionTestBackend{runEnvProfileTestBackend: runEnvProfileTestBackend{spec: provider.Spec()}, lease: lease, entered: make(chan struct{}), release: make(chan struct{})}
			close(b.release)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, admitted, err := admitRunLeaseUnderClaim(ctx, b, ResolveRequest{ID: lease.LeaseID, Repo: repo, Prepare: true}, &cfg, func(target *LeaseTarget) error {
				if test.admit != nil {
					return test.admit(cancel, target)
				}
				return nil
			})
			if test.legacy {
				if admitted || err != nil {
					t.Fatalf("incomplete binding did not retain its existing resolver: admitted=%t err=%v", admitted, err)
				}
				after, readErr := readLeaseClaim(lease.LeaseID)
				if readErr != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("capability modified incomplete binding")
				}
				result, err = resolveSSHLeaseTarget(ctx, b, ResolveRequest{ID: lease.LeaseID, Repo: repo, Prepare: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := (App{}).claimRunLeaseTargetForRepoAndRegister(ctx, result.LeaseID, serverSlug(result.Server), cfg, &result.Server, result.SSH, repo.Root, false, true); err != nil {
					t.Fatal(err)
				}
				after, readErr = readLeaseClaim(lease.LeaseID)
				if readErr != nil || after.Provider != provider.Name() || after.CloudID != before.CloudID {
					t.Fatal("legacy resolution did not retain its resource and bind the provider")
				}
				return
			}
			if !admitted {
				t.Fatal("bound canonical claim skipped admission")
			}
			after, readErr := readLeaseClaim(lease.LeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.wantError != "" {
				if test.changeClaim != nil {
					select {
					case <-b.entered:
						t.Fatal("replacement reached provider")
					default:
					}
				}
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v, want %s", err, test.wantError)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed admission mutated the authoritative claim")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot, exists, set := ServerLeaseClaimSnapshot(result.Server)
			if !exists || !set || !reflect.DeepEqual(snapshot, after) || before.Revision == after.Revision || before.ClaimedAt != after.ClaimedAt {
				t.Fatal("admission did not carry its committed owner snapshot")
			}
			if result.SSH.Port != "2200" || after.SSHPort != 2200 {
				t.Fatalf("prepared endpoint not published: target=%q claim=%d", result.SSH.Port, after.SSHPort)
			}
		})
	}
}
