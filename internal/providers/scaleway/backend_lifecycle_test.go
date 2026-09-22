package scaleway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	marketplace "github.com/scaleway/scaleway-sdk-go/api/marketplace/v2"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestWaitForPublicIPv4HonorsCanceledCaller(t *testing.T) {
	backend, client := newTestBackend(t)
	client.server = testServer("srv-1", "ready", nil, "203.0.113.10")
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("caller finished")
	cancel(cause)
	server, err := backend.waitForPublicIPv4(ctx, client, "srv-1")
	if server != nil || !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || client.getCalls != 0 {
		t.Fatalf("server=%v err=%v calls=%d; want caller cause without observation", server, err, client.getCalls)
	}
}

func TestWaitForPublicIPv4CancelsRealHTTPObservation(t *testing.T) {
	requestSeen := make(chan struct{}, 1)
	requestCanceled := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/servers/srv-1") || req.TLS == nil {
			t.Errorf("unexpected SDK request: method=%s path=%s TLS=%t", req.Method, req.URL.Path, req.TLS != nil)
		}
		select {
		case requestSeen <- struct{}{}:
		case <-releaseHandler:
			return
		}
		select {
		case <-req.Context().Done():
			select {
			case requestCanceled <- struct{}{}:
			default:
			}
		case <-releaseHandler:
		}
	}))
	defer server.Close()
	defer close(releaseHandler)
	httpClient := server.Client()
	defer httpClient.CloseIdleConnections()
	client := newTestScalewaySDKClient(t, server.URL, httpClient)
	guard, stopGuard := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopGuard()
	ctx, cancel := context.WithCancelCause(guard)
	defer cancel(nil)
	type result struct {
		server *instance.Server
		err    error
	}
	done := make(chan result, 1)
	go func() {
		got, err := (&Backend{}).waitForPublicIPv4(ctx, client, "srv-1")
		done <- result{got, err}
	}()
	select {
	case <-requestSeen:
	case got := <-done:
		t.Fatalf("SDK returned before the HTTPS observation reached the server: %v", got.err)
	case <-ctx.Done():
		t.Fatal("HTTPS observation did not reach the server")
	}
	started := time.Now()
	cause := core.Exit(7, "caller stopped Scaleway readiness")
	cancel(cause)
	select {
	case got := <-done:
		if got.server != nil || !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, cause) || core.ExitCodeForError(got.err, 1) != 7 || got.err.Error() != cause.Error() {
			t.Fatalf("server=%v err=%v, want caller cancellation", got.server, got.err)
		}
		classified := core.FinalizeRunResult(core.RunResult{}, got.err)
		want := core.FinalizeRunResult(core.RunResult{}, ctx.Err())
		if classified.Status != want.Status || classified.ErrorKind != want.ErrorKind {
			t.Fatalf("classification=%s/%s want=%s/%s", classified.Status, classified.ErrorKind, want.Status, want.ErrorKind)
		}
		t.Logf("real HTTPS SDK readiness: caller cause and cancellation retained, exit=7 diagnostic=%q classification=%s/%s", got.err.Error(), classified.Status, classified.ErrorKind)
	case <-time.After(5 * time.Second):
		t.Fatal("SDK observation did not return after cancellation")
	}
	select {
	case <-requestCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTPS server did not observe request cancellation")
	}
	t.Logf("real HTTPS SDK request reached server; caller cancellation returned and server observed cancellation in %s", time.Since(started))
}

func TestWaitForPublicIPv4SDKObservationBudget(t *testing.T) {
	readError := errors.New("observation failed")
	for _, name := range []string{"ready", "pending then ready", "owned timeout", "caller deadline", "client deadline", "read error", "read error after deadline"} {
		t.Run(name, func(t *testing.T) {
			clearScalewayEnv(t)
			t.Setenv("SCW_ACCESS_KEY", testScalewayAccessKey)
			t.Setenv("SCW_SECRET_KEY", testScalewaySecretKey)
			t.Setenv("SCW_DEFAULT_PROJECT_ID", testScalewayProjectID)
			t.Setenv("SCW_DEFAULT_ORGANIZATION_ID", testScalewayOrganizationID)
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				budget := 5 * time.Minute
				if name == "caller deadline" {
					var cancel context.CancelFunc
					budget = time.Minute
					ctx, cancel = context.WithTimeout(ctx, budget)
					defer cancel()
				}
				start := time.Now()
				calls := 0
				client, err := newClient(core.Config{}, core.Runtime{HTTP: &http.Client{Transport: scalewayRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					deadline, bounded := req.Context().Deadline()
					if !bounded || !deadline.Equal(start.Add(budget)) {
						t.Errorf("SDK observation deadline=%v bounded=%t; want %v", deadline, bounded, start.Add(budget))
						return nil, readError
					}
					switch name {
					case "owned timeout", "caller deadline", "read error after deadline":
						<-req.Context().Done()
						if name == "read error after deadline" {
							return nil, readError
						}
						return nil, req.Context().Err()
					case "client deadline":
						return nil, context.DeadlineExceeded
					case "read error":
						return nil, readError
					}
					body := `{"server":{"id":"srv-1","public_ip":{"address":"203.0.113.10"}}}`
					if name == "pending then ready" && calls == 1 {
						body = `{"server":null}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				server, err := (&Backend{}).waitForPublicIPv4(ctx, client, "srv-1")
				switch name {
				case "ready", "pending then ready":
					if err != nil || publicIPv4(server) != "203.0.113.10" {
						t.Fatalf("server=%v err=%v", server, err)
					}
				case "owned timeout":
					var exit core.ExitError
					if server != nil || !errors.Is(err, context.DeadlineExceeded) || !core.AsExitError(err, &exit) || exit.Code != 5 || exit.Message != "timed out waiting for Scaleway Instance public IPv4" {
						t.Fatalf("server=%v err=%v; want readiness timeout", server, err)
					}
					classified := core.FinalizeRunResult(core.RunResult{}, err)
					want := core.FinalizeRunResult(core.RunResult{}, context.DeadlineExceeded)
					if classified.Status != want.Status || classified.ErrorKind != want.ErrorKind {
						t.Fatalf("classification=%s/%s want=%s/%s", classified.Status, classified.ErrorKind, want.Status, want.ErrorKind)
					}
				case "caller deadline", "client deadline":
					if server != nil || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("server=%v err=%v; want original deadline", server, err)
					}
				default:
					if server != nil || !errors.Is(err, readError) {
						t.Fatalf("server=%v err=%v; want original observation error", server, err)
					}
				}
				wantCalls := 1
				if name == "pending then ready" {
					wantCalls = 2
					if elapsed := time.Since(start); elapsed != 3*time.Second {
						t.Fatalf("poll interval=%v, want 3s", elapsed)
					}
				}
				if calls != wantCalls {
					t.Fatalf("observations=%d, want %d", calls, wantCalls)
				}
			})
		})
	}
}

func TestWaitForPublicIPv4CompletedSDKResponseWinsCancellation(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			client := newTestScalewaySDKClient(t, "https://api.scaleway.com", &http.Client{Transport: scalewayRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				cancel(core.Exit(7, "caller stopped readiness"))
				status, body := http.StatusForbidden, `{"type":"permissions_denied","message":"fixture denied"}`
				if ready {
					status, body = http.StatusOK, `{"server":{"id":"srv-1","state":"stopped","public_ips":[{"address":"203.0.113.10"}]}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})})
			server, err := (&Backend{}).waitForPublicIPv4(ctx, client, "srv-1")
			if ready {
				if err != nil || publicIPv4(server) != "203.0.113.10" {
					t.Fatalf("server=%v err=%v; completed public IP must win without a running-state gate", server, err)
				}
			} else {
				var denied *scw.PermissionsDeniedError
				if server != nil || !errors.As(err, &denied) || errors.Is(err, context.Canceled) {
					t.Fatalf("server=%v err=%v; completed API response must win", server, err)
				}
			}
		})
	}
}

func TestScalewayAcquireListResolveTouchReleaseLifecycle(t *testing.T) {
	backend, fake := newTestBackend(t)
	var observed core.LeaseTarget
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "blue-box",
		OnAcquired: func(target core.LeaseTarget) error {
			observed = target
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID == "" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
	}
	if observed.Server.CloudID != lease.Server.CloudID || observed.LeaseID != lease.LeaseID || observed.SSH.Host != lease.SSH.Host {
		t.Fatalf("OnAcquired observed=%#v lease=%#v", observed, lease)
	}
	if len(fake.keys) != 1 {
		t.Fatalf("created keys=%#v", fake.keys)
	}
	if fake.lastCreate == nil || fake.lastCreate.DynamicIPRequired == nil || !*fake.lastCreate.DynamicIPRequired {
		t.Fatalf("create request did not request dynamic IP: %#v", fake.lastCreate)
	}
	if fake.lastCreate.Project == nil || *fake.lastCreate.Project != "project-1" || fake.lastCreate.Zone != scw.Zone("fr-par-1") {
		t.Fatalf("create project/zone=%#v", fake.lastCreate)
	}
	if fake.lastCreate.SecurityGroup == nil || *fake.lastCreate.SecurityGroup != "sg-1" {
		t.Fatalf("security group=%#v", fake.lastCreate.SecurityGroup)
	}
	if fake.lastListOptions < 2 {
		t.Fatal("inventory list did not request all pages")
	}
	if !strings.Contains(fake.userData, "ssh_authorized_keys") {
		t.Fatalf("cloud-init user data missing ssh keys: %s", fake.userData)
	}
	if !fake.poweredOn {
		t.Fatal("server was not powered on after cloud-init user data was set")
	}
	if got := labelsFromTags(fake.server.Tags); got["state"] != "ready" || got["scaleway_project"] != "project-1" || got["scaleway_ssh_key_id"] == "" {
		t.Fatalf("ready tags labels=%#v tags=%v", got, fake.server.Tags)
	}

	list, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].CloudID != fake.server.ID {
		t.Fatalf("list=%#v", list)
	}
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "blue-box"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.LeaseID != lease.LeaseID || resolved.Server.CloudID != fake.server.ID {
		t.Fatalf("resolved=%#v", resolved)
	}
	override := 4 * time.Hour
	touched, err := backend.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "running", IdleTimeout: override, IdleTimeoutOverride: &override})
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if touched.Labels["state"] != "running" {
		t.Fatalf("touch labels=%#v", touched.Labels)
	}
	if touched.Labels["idle_timeout_secs"] != "14400" {
		t.Fatalf("touch did not persist idle timeout override: %#v", touched.Labels)
	}
	claim, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || claim.IdleTimeoutSeconds != 14400 || claim.Labels["idle_timeout_secs"] != "14400" {
		t.Fatalf("explicit touch claim=%#v err=%v", claim, err)
	}
	touched, err = backend.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "ready", IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err = core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || claim.IdleTimeoutSeconds != 14400 || claim.Labels["idle_timeout_secs"] != "14400" || touched.Labels["idle_timeout_secs"] != "14400" || labelsFromTags(fake.server.Tags)["idle_timeout_secs"] != "14400" {
		t.Fatalf("ordinary touch changed idle: claim=%#v server=%#v err=%v", claim, touched, err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !fake.deletedServer || !fake.deletedKey {
		t.Fatalf("deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	if len(fake.volumes) != 0 {
		t.Fatalf("release retained %d allocation-created volumes after deleting the server and key", len(fake.volumes))
	}
	if !fake.poweredOff {
		t.Fatal("running server was not powered off before deletion")
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil || ok {
		t.Fatalf("claim after release ok=%t err=%v", ok, err)
	}
}

func TestScalewayResolveReadOnlyIgnoresStaleClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_121212121212"
	slug := "stale-read-only"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.server = testServer("srv-live", core.LeaseProviderName(leaseID, slug), tagsFromLabels(labels), "203.0.113.21")
	claimServer := core.Server{Provider: providerName, CloudID: "srv-stale", Name: fake.server.Name, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != fake.server.ID {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.CloudID != "srv-stale" {
		t.Fatalf("read-only resolve changed stale claim: claim=%#v exists=%v err=%v", claim, exists, err)
	}
}

func TestScalewayRootVolumeCleanupRetainsUserAttachedDisk(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-owner"})
	if err != nil {
		t.Fatal(err)
	}
	foreignID := "55555555-5555-5555-5555-555555555555"
	fake.server.Volumes["1"] = &instance.VolumeServer{ID: foreignID}
	fake.volumes[foreignID] = &instance.Volume{ID: foreignID, Project: fake.ProjectID(), Zone: scw.Zone(fake.Zone()), Server: &instance.ServerSummary{ID: fake.server.ID}}
	if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(fake.volumes) != 1 || fake.volumes[foreignID] == nil {
		t.Fatalf("foreign disk was deleted or allocation disk remains: %v", fake.volumes)
	}
}

func TestScalewayRootVolumeCleanupRetriesAfterServerGone(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-retry"})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("volume service unavailable")
	fake.deleteVolumeErr = failure
	err = backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
	if !errors.Is(err, failure) || !fake.deletedServer || fake.deletedKey {
		t.Fatalf("err=%v deletedServer=%t deletedKey=%t", err, fake.deletedServer, fake.deletedKey)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || claim.Labels[rootVolumeLabel] != lease.Server.Labels[rootVolumeLabel] {
		t.Fatalf("root identity not retained: exists=%t err=%v", exists, err)
	}
	fake.server = nil
	fake.getErr = &scw.ResourceNotFoundError{}
	fake.deleteVolumeErr = nil
	recovery, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: recovery}); err != nil {
		t.Fatal(err)
	}
	_, exists, err = core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || exists || len(fake.volumes) != 0 || !fake.deletedKey {
		t.Fatalf("cleanup incomplete: claim=%t volumes=%d key=%t err=%v", exists, len(fake.volumes), fake.deletedKey, err)
	}
}

func TestScalewayRootVolumeRollbackFailureVetoesFreshAllocation(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanupFails), func(t *testing.T) {
			backend, fake := newTestBackend(t)
			fake.servers = []*instance.Server{}
			primary := core.Exit(5, "timed out waiting for SSH during fixture bootstrap")
			cleanup := errors.New("fixture root-volume deletion unavailable")
			backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return primary }
			if cleanupFails {
				fake.deleteVolumeErr = cleanup
			}
			_, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-retry-veto"})
			wantCalls := core.AcquireAttempts(false)
			if cleanupFails {
				wantCalls = 1
			}
			if !errors.Is(err, primary) || core.ExitCodeForError(err, 1) != 5 || fake.createCalls != wantCalls || cleanupFails && (!errors.Is(err, cleanup) || fake.deletedKey || len(fake.volumes) != 1) {
				t.Fatalf("error=%v creates=%d want=%d keyDeleted=%t volumes=%d", err, fake.createCalls, wantCalls, fake.deletedKey, len(fake.volumes))
			}
		})
	}
}

func TestScalewayRootVolumeCleanupRefusesChangedNativeIdentity(t *testing.T) {
	for _, change := range []string{"attached elsewhere", "project", "zone", "id"} {
		t.Run(change, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-guard"})
			if err != nil {
				t.Fatal(err)
			}
			volume := fake.volumes[lease.Server.Labels[rootVolumeLabel]]
			switch change {
			case "attached elsewhere":
				volume.Server = &instance.ServerSummary{ID: "other-server"}
			case "project":
				volume.Project = "other-project"
			case "zone":
				volume.Zone = scw.Zone("nl-ams-1")
			case "id":
				volume.ID = "55555555-5555-5555-5555-555555555555"
			}
			err = backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if err == nil || fake.deletedServer || fake.deletedKey || len(fake.volumes) != 1 {
				t.Fatalf("err=%v serverDeleted=%t keyDeleted=%t volumes=%d", err, fake.deletedServer, fake.deletedKey, len(fake.volumes))
			}
		})
	}
}

func TestScalewayRootManifestCannotDisappearDuringMutation(t *testing.T) {
	for _, operation := range []string{"touch", "tailscale", "resolve", "status"} {
		t.Run(operation, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-metadata"})
			if err != nil {
				t.Fatal(err)
			}
			before, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			labels := labelsFromTags(fake.server.Tags)
			delete(labels, rootVolumeLabel)
			delete(labels, volumeContractLabel)
			fake.server.Tags = tagsFromLabels(labels)
			updates := fake.updateCalls
			switch operation {
			case "touch":
				_, err = backend.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "ready"})
			case "tailscale":
				_, err = backend.UpdateTailscaleMetadata(t.Context(), lease, core.TailscaleMetadata{})
			case "resolve":
				_, err = backend.Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: t.TempDir()}})
			case "status":
				if backend.StatusTouchClaimMatches(core.LeaseTarget{Server: backend.serverFromScaleway(fake.server)}, before) {
					t.Fatal("status accepted missing allocation identity")
				}
				err = errors.New("status rejected")
			}
			after, exists, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err == nil || readErr != nil || !exists || before.Revision != after.Revision || fake.updateCalls != updates {
				t.Fatalf("mutation escaped guard: err=%v readErr=%v exists=%t updates=%d/%d revision=%s/%s", err, readErr, exists, fake.updateCalls, updates, before.Revision, after.Revision)
			}
		})
	}
}

func TestScalewayRootManifestPublicationFailureCanOnlyBeReleased(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.updateErr = errors.New("tag publication failed")
	fake.deleteErr = errors.New("server deletion failed")
	_, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-pending"})
	if err == nil || fake.deletedKey {
		t.Fatalf("err=%v keyDeleted=%t", err, fake.deletedKey)
	}
	leaseID := labelsFromTags(fake.server.Tags)["lease"]
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.Labels[volumePendingLabel] != "true" || claim.Labels[rootVolumeLabel] == "" {
		t.Fatalf("pending manifest not retained: exists=%t err=%v", exists, err)
	}
	if _, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: leaseID}); err == nil {
		t.Fatal("pending manifest allowed reuse")
	}
	fake.updateErr, fake.deleteErr = nil, nil
	recovery, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: recovery}); err != nil {
		t.Fatal(err)
	}
	if len(fake.volumes) != 0 || !fake.deletedKey {
		t.Fatal("pending allocation cleanup incomplete")
	}
}

func TestScalewayIncompleteNewRootManifestCannotBecomeLegacy(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.omitRootVolume = true
	_, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-unknown"})
	if err == nil || fake.deletedServer || fake.deletedKey {
		t.Fatalf("incomplete allocation was destroyed: err=%v server=%t key=%t", err, fake.deletedServer, fake.deletedKey)
	}
	labels := labelsFromTags(fake.server.Tags)
	if labels[volumeContractLabel] != rootVolumeContract || labels[rootVolumeLabel] != "" {
		t.Fatalf("initial creation lost contract marker: %v", labels)
	}
	core.RemoveLeaseClaim(labels["lease"])
	_, err = backend.Resolve(t.Context(), core.ResolveRequest{ID: labels["lease"], Reclaim: true, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil {
		t.Fatal("claimless new allocation was reclaimed as legacy")
	}
}

func TestScalewayRootManifestMustPersistBeforeDestructiveRollback(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.afterCreate = func() {
		state, err := core.CrabboxStateDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(state, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "claims"), []byte("fixture blocks claim directory"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-journal"})
	if err == nil || !strings.Contains(err.Error(), "persist Scaleway") || fake.server == nil || fake.deletedServer || fake.deletedKey || fake.updateCalls != 0 {
		t.Fatalf("unsafe rollback after journal failure: err=%v server=%v deleted=%t keyDeleted=%t updates=%d", err, fake.server != nil, fake.deletedServer, fake.deletedKey, fake.updateCalls)
	}
}

func TestScalewayRootManifestPublicationIsDurableBeforeBootstrap(t *testing.T) {
	backend, fake := newTestBackend(t)
	called := false
	backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		called = true
		labels := labelsFromTags(fake.server.Tags)
		claim, exists, err := core.ReadLeaseClaimWithPresence(labels["lease"])
		if err != nil || !exists || claim.Labels[volumePendingLabel] != "" || claim.Labels[rootVolumeLabel] == "" || claim.Labels[rootVolumeLabel] != labels[rootVolumeLabel] {
			t.Fatalf("manifest not durable before bootstrap: exists=%t pending=%q id=%q err=%v", exists, claim.Labels[volumePendingLabel], claim.Labels[rootVolumeLabel], err)
		}
		return nil
	}
	lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-published"})
	if err != nil || !called {
		t.Fatalf("Acquire err=%v bootstrap=%t", err, called)
	}
	if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestScalewayRollbackCannotOverwriteChangedAllocationIdentity(t *testing.T) {
	for _, field := range []string{"lease", "slug", "provider", "target", "provider_key", "scaleway_project", "scaleway_zone", "scaleway_ssh_key_id", "scaleway_ssh_key_name", "scaleway_organization", "scaleway_region", "claim slug", "claim lease"} {
		t.Run(field, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			var path string
			var changed []byte
			backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
				leaseID := labelsFromTags(fake.server.Tags)["lease"]
				claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
				if err != nil || !exists {
					t.Fatalf("allocation claim unavailable: %v", err)
				}
				switch field {
				case "claim slug":
					claim.Slug = "changed-slug"
				case "claim lease":
					claim.LeaseID = "cbx_999999999999"
				default:
					claim.Labels[field] = "changed-identity"
				}
				changed, err = json.Marshal(claim)
				if err != nil {
					t.Fatal(err)
				}
				state, err := core.CrabboxStateDir()
				if err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(state, "claims", leaseID+".json")
				if err := os.WriteFile(path, changed, 0600); err != nil {
					t.Fatal(err)
				}
				return errors.New("fixture bootstrap failed")
			}
			_, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "volume-changed"})
			if err == nil || path == "" || fake.deletedServer || fake.deletedKey {
				t.Fatalf("changed allocation was destroyed: err=%v pathSet=%t server=%t key=%t", err, path != "", fake.deletedServer, fake.deletedKey)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != string(changed) {
				t.Fatalf("changed claim overwritten: readErr=%v", readErr)
			}
		})
	}
}

func TestScalewayAcquireCannotPublishAfterClaimReplacement(t *testing.T) {
	for _, phase := range []string{"bootstrap", "callback"} {
		for _, field := range []string{rootVolumeLabel, "concurrent-owner-note"} {
			t.Run(phase+"/"+field, func(t *testing.T) {
				backend, fake := newTestBackend(t)
				var claimPath string
				var changed []byte
				changeClaim := func() {
					leaseID := labelsFromTags(fake.server.Tags)["lease"]
					claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
					if err != nil || !exists {
						t.Fatalf("claim unavailable: %v", err)
					}
					labels := maps.Clone(claim.Labels)
					labels[field] = "55555555-5555-5555-5555-555555555555"
					if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
						t.Fatal(err)
					}
					state, err := core.CrabboxStateDir()
					if err != nil {
						t.Fatal(err)
					}
					claimPath = filepath.Join(state, "claims", leaseID+".json")
					changed, err = os.ReadFile(claimPath)
					if err != nil {
						t.Fatal(err)
					}
				}
				backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
					if phase == "bootstrap" {
						changeClaim()
					}
					return nil
				}
				callbacks := 0
				_, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "stale-publication", OnAcquired: func(core.LeaseTarget) error {
					callbacks++
					if phase == "callback" {
						changeClaim()
					}
					return nil
				}})
				wantCallbacks := 0
				if phase == "callback" {
					wantCallbacks = 1
				}
				if err == nil || claimPath == "" || fake.updateCalls != 1 || fake.deletedServer || fake.deletedKey || callbacks != wantCallbacks {
					t.Fatalf("stale acquisition acted: err=%v updates=%d serverDeleted=%t keyDeleted=%t callbacks=%d want=%d", err, fake.updateCalls, fake.deletedServer, fake.deletedKey, callbacks, wantCallbacks)
				}
				after, readErr := os.ReadFile(claimPath)
				if readErr != nil || string(after) != string(changed) {
					t.Fatalf("new owner's claim overwritten: %v", readErr)
				}
			})
		}
	}
}

func TestScalewayAcquireObserverCannotRewriteRootOwnership(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "observer-copy", OnAcquired: func(observed core.LeaseTarget) error {
		observed.Server.Labels[rootVolumeLabel] = "55555555-5555-5555-5555-555555555555"
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	rootID := fake.server.Volumes["0"].ID
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || lease.Server.Labels[rootVolumeLabel] != rootID || claim.Labels[rootVolumeLabel] != rootID || labelsFromTags(fake.server.Tags)[rootVolumeLabel] != rootID {
		t.Fatalf("observer rewrote ownership: exists=%t err=%v", exists, err)
	}
	if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestScalewayAcquireEmptyRepoRootStillFencesCompletion(t *testing.T) {
	for _, replaceClaim := range []bool{false, true} {
		t.Run(fmt.Sprint(replaceClaim), func(t *testing.T) {
			backend, fake := newTestBackend(t)
			backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
				if replaceClaim {
					id := labelsFromTags(fake.server.Tags)["lease"]
					claim, _, err := core.ReadLeaseClaimWithPresence(id)
					if err != nil {
						t.Fatal(err)
					}
					labels := maps.Clone(claim.Labels)
					labels["concurrent-owner-note"] = "changed"
					if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(id, claim, labels); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			lease, err := backend.Acquire(t.Context(), core.AcquireRequest{RequestedSlug: "empty-root"})
			if replaceClaim {
				if err == nil || fake.updateCalls != 1 || fake.deletedServer || fake.deletedKey {
					t.Fatalf("empty-root stale publication escaped guard: err=%v updates=%d", err, fake.updateCalls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			cwd, cwdErr := os.Getwd()
			if err != nil || cwdErr != nil || !exists || claim.RepoRoot != cwd || claim.Labels["state"] != "ready" || claim.Labels["recovery"] != "" || labelsFromTags(fake.server.Tags)["state"] != "ready" || fake.updateCalls != 2 {
				t.Fatalf("empty-root completion not published: exists=%t state=%q recovery=%q updates=%d err=%v cwdErr=%v", exists, claim.Labels["state"], claim.Labels["recovery"], fake.updateCalls, err, cwdErr)
			}
		})
	}
}

func TestScalewaySDKOwnershipGuardsBeforeHTTPMutations(t *testing.T) {
	const serverID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const rootID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const keyID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const otherID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const base = "/instance/v1/zones/fr-par-1"
	for _, scenario := range []string{"success", "bootstrap replacement", "callback replacement", "reassigned disk"} {
		t.Run(scenario, func(t *testing.T) {
			backend, _ := newTestBackend(t)
			var mu sync.Mutex
			var server *instance.Server
			var volume *instance.Volume
			var key *iam.SSHKey
			var requests []string
			https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.TLS == nil {
					t.Error("SDK request was not HTTPS")
				}
				reply := func(status int, value any) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					if value != nil {
						_ = json.NewEncoder(w).Encode(value)
					}
				}
				decode := func(value any) bool {
					if err := json.NewDecoder(r.Body).Decode(value); err != nil {
						t.Errorf("decode SDK request: %v", err)
						reply(http.StatusBadRequest, map[string]string{"type": "invalid_arguments"})
						return false
					}
					return true
				}
				if strings.HasPrefix(r.URL.Path, "/marketplace/v2/") {
					// The adapter's supported explicit image-ID fallback needs no catalogue fixture.
					reply(http.StatusNotFound, map[string]string{"type": "not_found"})
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "GET " + base + "/servers":
					items := []*instance.Server{}
					if server != nil {
						items = append(items, server)
					}
					reply(http.StatusOK, map[string]any{"servers": items})
				case "POST /iam/v1alpha1/ssh-keys":
					var req iam.CreateSSHKeyRequest
					if !decode(&req) {
						return
					}
					key = &iam.SSHKey{ID: keyID, Name: req.Name, PublicKey: req.PublicKey, ProjectID: req.ProjectID}
					reply(http.StatusCreated, key)
				case "POST " + base + "/servers":
					var req instance.CreateServerRequest
					if !decode(&req) {
						return
					}
					server = &instance.Server{ID: serverID, Name: req.Name, Tags: req.Tags, Project: testScalewayProjectID, Organization: testScalewayOrganizationID, Zone: scw.Zone("fr-par-1"), State: instance.ServerStateStopped, CommercialType: req.CommercialType,
						PublicIP: &instance.ServerIP{Address: net.ParseIP("203.0.113.10")}, Volumes: map[string]*instance.VolumeServer{"0": {ID: rootID, Zone: scw.Zone("fr-par-1")}}}
					volume = &instance.Volume{ID: rootID, Project: testScalewayProjectID, Zone: scw.Zone("fr-par-1"), Server: &instance.ServerSummary{ID: serverID}}
					reply(http.StatusCreated, map[string]any{"server": server})
				case "GET " + base + "/servers/" + serverID:
					if server == nil {
						reply(http.StatusNotFound, map[string]string{"type": "not_found"})
						return
					}
					reply(http.StatusOK, map[string]any{"server": server})
				case "PATCH " + base + "/servers/" + serverID:
					var req instance.UpdateServerRequest
					if !decode(&req) {
						return
					}
					if server == nil || req.Tags == nil {
						t.Error("unexpected SDK server update")
						reply(http.StatusBadRequest, nil)
						return
					}
					server.Tags = *req.Tags
					reply(http.StatusOK, map[string]any{"server": server})
				case "PATCH " + base + "/servers/" + serverID + "/user_data/cloud-init":
					_, _ = io.Copy(io.Discard, r.Body)
					reply(http.StatusNoContent, nil)
				case "POST " + base + "/servers/" + serverID + "/action":
					var req instance.ServerActionRequest
					if !decode(&req) {
						return
					}
					if server == nil {
						t.Error("action on absent fixture server")
						reply(http.StatusNotFound, nil)
						return
					}
					switch req.Action {
					case instance.ServerActionPoweron:
						server.State = instance.ServerStateRunning
					case instance.ServerActionPoweroff:
						server.State = instance.ServerStateStopped
					default:
						t.Errorf("unexpected SDK action %s", req.Action)
					}
					reply(http.StatusOK, map[string]any{})
				case "GET " + base + "/volumes/" + rootID:
					if volume == nil {
						reply(http.StatusNotFound, map[string]string{"type": "not_found"})
						return
					}
					reply(http.StatusOK, map[string]any{"volume": volume})
				case "DELETE " + base + "/servers/" + serverID:
					server = nil
					if volume != nil {
						volume.Server = nil
					}
					reply(http.StatusNoContent, nil)
				case "DELETE " + base + "/volumes/" + rootID:
					volume = nil
					reply(http.StatusNoContent, nil)
				case "DELETE /iam/v1alpha1/ssh-keys/" + keyID:
					key = nil
					reply(http.StatusNoContent, nil)
				default:
					t.Errorf("unexpected SDK request %s %s", r.Method, r.URL.Path)
					reply(http.StatusBadRequest, map[string]string{"type": "invalid_arguments"})
				}
			}))
			defer https.Close()
			httpClient := https.Client()
			defer httpClient.CloseIdleConnections()
			client := newTestScalewaySDKClient(t, https.URL, httpClient)
			backend.cfg.Scaleway.ProjectID = testScalewayProjectID
			backend.cfg.Scaleway.OrganizationID = testScalewayOrganizationID
			backend.cfg.Scaleway.Image = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
			backend.cfg.Scaleway.SecurityGroup = "ffffffff-ffff-ffff-ffff-ffffffffffff"
			backend.newClient = func(core.Config, core.Runtime) (Client, error) { return client, nil }
			checkpoint := -1
			var claimPath string
			var expectedClaim []byte
			replaceClaim := func() {
				mu.Lock()
				id := labelsFromTags(server.Tags)["lease"]
				checkpoint = len(requests)
				mu.Unlock()
				claim, exists, err := core.ReadLeaseClaimWithPresence(id)
				if err != nil || !exists {
					t.Fatalf("SDK allocation claim absent: %v", err)
				}
				labels := maps.Clone(claim.Labels)
				labels["concurrent-sdk-owner"] = "replacement"
				if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(id, claim, labels); err != nil {
					t.Fatal(err)
				}
				state, err := core.CrabboxStateDir()
				if err != nil {
					t.Fatal(err)
				}
				claimPath = filepath.Join(state, "claims", id+".json")
				expectedClaim, err = os.ReadFile(claimPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			backend.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
				if scenario == "bootstrap replacement" {
					replaceClaim()
				}
				return nil
			}
			lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "sdk-ownership", OnAcquired: func(core.LeaseTarget) error {
				if scenario == "callback replacement" {
					replaceClaim()
				}
				return nil
			}})
			if strings.HasSuffix(scenario, "replacement") {
				if err == nil || checkpoint < 0 {
					t.Fatalf("expected stale-claim refusal after SDK provisioning, got %v", err)
				}
				after, readErr := os.ReadFile(claimPath)
				if readErr != nil || string(after) != string(expectedClaim) {
					t.Fatalf("replacement claim changed: %v", readErr)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(requests) != checkpoint || server == nil || volume == nil || key == nil {
					t.Fatalf("stale SDK owner performed requests after replacement: %v", requests[checkpoint:])
				}
				t.Logf("actual Scaleway SDK HTTPS: %s; provisioningRequests=%d subsequentRequests=0 resourcesUntouched=true replacementClaimBytesPreserved=true", scenario, checkpoint)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			state, stateErr := core.CrabboxStateDir()
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			claimPath = filepath.Join(state, "claims", lease.LeaseID+".json")
			expectedClaim, err = os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			checkpoint = len(requests)
			if scenario == "reassigned disk" {
				volume.Server = &instance.ServerSummary{ID: otherID}
			}
			mu.Unlock()
			err = backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			mu.Lock()
			defer mu.Unlock()
			mutations := 0
			for _, request := range requests[checkpoint:] {
				if !strings.HasPrefix(request, "GET ") {
					mutations++
				}
			}
			if scenario == "reassigned disk" {
				if err == nil || core.ExitCodeForError(err, 1) != 4 || mutations != 0 || server == nil || key == nil || volume == nil || volume.Server.ID != otherID {
					t.Fatalf("reassigned disk was not protected: err=%v requests=%v", err, requests[checkpoint:])
				}
				after, readErr := os.ReadFile(claimPath)
				if readErr != nil || string(after) != string(expectedClaim) {
					t.Fatalf("refused release changed its recovery claim: %v", readErr)
				}
				t.Logf("actual Scaleway SDK HTTPS: reassigned disk; observationRequests=%d destructiveOrUpdateRequests=0 serverDiskKeyUntouched=true claimBytesPreserved=true exit=4", len(requests)-checkpoint)
			} else {
				if err != nil || server != nil || volume != nil || key != nil || mutations != 4 {
					t.Fatalf("SDK positive cleanup failed: err=%v requests=%v", err, requests[checkpoint:])
				}
				if _, statErr := os.Stat(claimPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("successful SDK cleanup retained claim: %v", statErr)
				}
				t.Logf("actual Scaleway SDK HTTPS: positive acquisition/publication/release; cleanupMutations=%d serverDiskKeyAbsent=true", mutations)
			}
		})
	}
}

func TestScalewayNoMutationResolveDoesNotBindAmbiguousRecovery(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_131313131313"
	slug := "no-mutation-recovery"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-no-mutation"
	claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	delete(liveLabels, "recovery")
	fake.server = testServer("srv-no-mutation", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.22")

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "srv-no-mutation" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !exists || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("no-mutation resolve changed claim: claim=%#v exists=%t err=%v", claim, exists, claimErr)
	}
}

func TestStatusTouchClaimRequiresMatchingProviderIdentity(t *testing.T) {
	backend := &Backend{}
	labels := map[string]string{
		"scaleway_project":      "project-a",
		"scaleway_organization": "organization-a",
		"scaleway_zone":         "fr-par-1",
	}
	claim := core.LeaseClaim{Labels: maps.Clone(labels)}
	lease := core.LeaseTarget{Server: core.Server{Labels: maps.Clone(labels)}}
	if !backend.StatusTouchClaimMatches(lease, claim) {
		t.Fatal("matching provider identity was rejected")
	}
	for _, key := range []string{"scaleway_project", "scaleway_zone"} {
		mismatched := lease
		mismatched.Server.Labels = maps.Clone(lease.Server.Labels)
		mismatched.Server.Labels[key] = "other"
		if backend.StatusTouchClaimMatches(mismatched, claim) {
			t.Fatalf("mismatched %s was accepted", key)
		}
		missing := claim
		missing.Labels = maps.Clone(claim.Labels)
		delete(missing.Labels, key)
		if backend.StatusTouchClaimMatches(lease, missing) {
			t.Fatalf("missing claim %s was accepted", key)
		}
	}
	withoutOrganization := claim
	withoutOrganization.Labels = maps.Clone(claim.Labels)
	delete(withoutOrganization.Labels, "scaleway_organization")
	if !backend.StatusTouchClaimMatches(lease, withoutOrganization) {
		t.Fatal("optional organization was required")
	}
	mismatchedOrganization := lease
	mismatchedOrganization.Server.Labels = maps.Clone(lease.Server.Labels)
	mismatchedOrganization.Server.Labels["scaleway_organization"] = "other"
	if backend.StatusTouchClaimMatches(mismatchedOrganization, claim) {
		t.Fatal("present organization mismatch was accepted")
	}
}

func TestScalewayAcquireOnAcquiredErrorRollsBack(t *testing.T) {
	backend, fake := newTestBackend(t)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "reject",
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected identity") {
		t.Fatalf("Acquire err=%v", err)
	}
	if !fake.deletedServer || !fake.deletedKey {
		t.Fatalf("rollback deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 0 {
		t.Fatalf("rollback should remove recovery claim after cleanup: %#v", claims)
	}
}

func TestScalewayAcquireOnAcquiredErrorRollsBackWithKeep(t *testing.T) {
	backend, fake := newTestBackend(t)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "reject-kept",
		Keep:          true,
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected kept identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected kept identity") {
		t.Fatalf("Acquire err=%v", err)
	}
	if !fake.deletedServer || !fake.deletedKey {
		t.Fatalf("rollback deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 0 {
		t.Fatalf("rollback should remove recovery claim after cleanup: %#v", claims)
	}
}

func TestScalewayAcquireRejectsUnsupportedSSHCIDRs(t *testing.T) {
	backend, fake := newTestBackend(t)
	backend.cfg.Scaleway.SSHCIDRs = []string{"203.0.113.0/24"}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "cidrs"})
	if err == nil || !strings.Contains(err.Error(), "does not yet manage security-group SSH CIDRs") {
		t.Fatalf("Acquire err=%v", err)
	}
	if fake.lastCreate != nil {
		t.Fatalf("server was created despite unsupported CIDRs: %#v", fake.lastCreate)
	}
}

func TestScalewayAcquireRejectsUnsupportedPortableOSBeforeDefaultingImage(t *testing.T) {
	backend, fake := newTestBackend(t)
	backend.cfg.OSImage = "ubuntu:26.04"
	backend.cfg.Scaleway.Image = ""
	core.SetOSImageExplicit(&backend.cfg)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "unsupported-os"})
	if err == nil || !strings.Contains(err.Error(), "does not support os") {
		t.Fatalf("Acquire err=%v", err)
	}
	if fake.lastCreate != nil {
		t.Fatalf("server was created despite unsupported OS: %#v", fake.lastCreate)
	}
}

func TestScalewayCleanupSkipsForeignAndDeletesExpiredOwned(t *testing.T) {
	backend, fake := newTestBackend(t)
	now := backend.clockNow()
	ownedLabels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_111111111111", "owned", providerName, "", false, now.Add(-3*time.Hour))
	ownedLabels["scaleway_project"] = "project-1"
	ownedLabels["scaleway_zone"] = "fr-par-1"
	ownedLabels["scaleway_ssh_key_id"] = "key-owned"
	claimlessLabels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_000000000000", "claimless", providerName, "", false, now.Add(-3*time.Hour))
	claimlessLabels["scaleway_project"] = "project-1"
	claimlessLabels["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{
		testServer("srv-claimless", "crabbox-cbx-claimless", tagsFromLabels(claimlessLabels), "203.0.113.10"),
		testServer("srv-owned", "crabbox-cbx-owned", tagsFromLabels(ownedLabels), "203.0.113.11"),
		testServer("srv-foreign", "foreign", []string{"crabbox", "crabbox:provider:other"}, "203.0.113.12"),
	}
	claimServer := backend.serverFromScaleway(fake.servers[1])
	if err := core.ClaimLeaseTargetForConfig(
		"cbx_111111111111",
		"owned",
		backend.cfgForRun(),
		claimServer,
		core.SSHTarget{},
		backend.cfgForRun().IdleTimeout,
	); err != nil {
		t.Fatal(err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if !fake.deletedServer {
		t.Fatal("owned expired server was not deleted")
	}
	if fake.deletedServerID != "srv-owned" {
		t.Fatalf("deleted server id=%q", fake.deletedServerID)
	}
}

func TestScalewayCleanupRefusesMismatchedProject(t *testing.T) {
	backend, fake := newTestBackend(t)
	labels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_abcdefabcdef", "bad", providerName, "", false, backend.clockNow().Add(-3*time.Hour))
	labels["scaleway_project"] = "other-project"
	labels["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{testServer("srv-bad", "crabbox-cbx-bad", tagsFromLabels(labels), "203.0.113.13")}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("mismatched project should be skipped as foreign ownership, got: %v", err)
	}
	if fake.deletedServer {
		t.Fatal("mismatched project server was deleted")
	}
}

func TestScalewayAmbiguousCreatePersistsRecoveryClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.createErr = context.DeadlineExceeded
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%#v", claims)
	}
	if claims[0].Provider != providerName || claims[0].Labels["recovery"] != "ambiguous-create" || claims[0].Labels["scaleway_ssh_key_name"] == "" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claim=%#v", claims[0])
	}
	if fake.deletedKey {
		t.Fatal("ambiguous create must retain the managed Scaleway SSH key for recovery")
	}
}

func TestScalewayAmbiguousSSHKeyCreateReconcilesCleanableKey(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.createKeyErr = context.DeadlineExceeded
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.acquireOnce(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ambiguous-key"})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "rollback-key-cleanup" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claims=%#v", claims)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: claims[0].LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("Resolve recovery claim: %v", err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("Release recovery claim: %v", err)
	}
	if !fake.deletedKey {
		t.Fatal("release did not delete reconciled SSH key")
	}
}

func TestScalewayReleaseOnlyPreservesAmbiguousSlugError(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	labelsA := core.DirectLeaseLabels(cfg, "cbx_222222222222", "duplicate", providerName, "", false, backend.clockNow())
	labelsA["scaleway_project"] = "project-1"
	labelsA["scaleway_zone"] = "fr-par-1"
	labelsB := core.DirectLeaseLabels(cfg, "cbx_333333333333", "duplicate", providerName, "", false, backend.clockNow())
	labelsB["scaleway_project"] = "project-1"
	labelsB["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{
		testServer("srv-a", "duplicate-a", tagsFromLabels(labelsA), "203.0.113.21"),
		testServer("srv-b", "duplicate-b", tagsFromLabels(labelsB), "203.0.113.22"),
	}
	claimServer := backend.serverFromScaleway(fake.servers[0])
	if err := core.ClaimLeaseTargetForConfig("cbx_222222222222", "duplicate", cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "duplicate", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "matches multiple active leases") {
		t.Fatalf("Resolve ambiguous slug err=%v", err)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "srv-a", ReleaseOnly: true})
	if err != nil || lease.Server.CloudID != "srv-a" {
		t.Fatalf("Resolve exact cloud id lease=%#v err=%v", lease, err)
	}
}

func TestScalewayReleaseOnlyDoesNotRetargetForeignExactClaimToSlug(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	const (
		requestedID = "cbx_222222222222"
		lookalikeID = "cbx_333333333333"
	)

	foreignCfg := cfg
	foreignCfg.Provider = "aws"
	foreignLabels := core.DirectLeaseLabels(foreignCfg, requestedID, "foreign", "aws", "", false, backend.clockNow())
	if err := core.ClaimLeaseTargetForConfig(requestedID, "foreign", foreignCfg, core.Server{Provider: "aws", CloudID: "i-foreign", Labels: foreignLabels}, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	labels := core.DirectLeaseLabels(cfg, lookalikeID, requestedID, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.server = testServer("srv-lookalike", "lookalike", tagsFromLabels(labels), "203.0.113.23")
	server := backend.serverFromScaleway(fake.server)
	if err := core.ClaimLeaseTargetForConfig(lookalikeID, requestedID, cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: requestedID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("retargeted cleanup deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayReleaseOnlyDoesNotFallBackToCanonicalClaimSlug(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	const (
		requestedID = "cbx_aaaaaaaaaaaa"
		lookalikeID = "cbx_bbbbbbbbbbbb"
	)
	labels := core.DirectLeaseLabels(cfg, lookalikeID, requestedID, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.server = testServer("srv-lookalike", "lookalike", tagsFromLabels(labels), "203.0.113.24")
	server := backend.serverFromScaleway(fake.server)
	if err := core.ClaimLeaseTargetForConfig(lookalikeID, requestedID, cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: requestedID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "exact lease identifier") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("retargeted cleanup deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayUpdateTailscaleMetadataPersistsTagsAndClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "tailnet"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	updated, err := backend.UpdateTailscaleMetadata(context.Background(), lease, core.TailscaleMetadata{
		Enabled:  true,
		Hostname: "tailnet-host",
		FQDN:     "tailnet-host.example.ts.net",
		IPv4:     "100.64.0.42",
		Tags:     []string{"tag:ci"},
		State:    "ready",
	})
	if err != nil {
		t.Fatalf("UpdateTailscaleMetadata: %v", err)
	}
	for key, want := range map[string]string{
		"tailscale":          "true",
		"tailscale_hostname": "tailnet-host",
		"tailscale_fqdn":     "tailnet-host.example.ts.net",
		"tailscale_ipv4":     "100.64.0.42",
		"tailscale_tags":     "tag:ci",
		"tailscale_state":    "ready",
	} {
		if got := updated.Labels[key]; got != want {
			t.Fatalf("updated label %s=%q want %q", key, got, want)
		}
		if got := labelsFromTags(fake.server.Tags)[key]; got != want {
			t.Fatalf("live tag %s=%q want %q", key, got, want)
		}
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels["tailscale_ipv4"] != "100.64.0.42" || claim.Labels["tailscale_fqdn"] != "tailnet-host.example.ts.net" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestScalewayFailedPreCreateKeyCleanupPersistsRecoveryClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	backend.cfg.Scaleway.Image = "missing"
	fake.deleteKeyErr = errors.New("iam unavailable")
	_, err := backend.acquireOnce(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "key-recovery"})
	if err == nil || !strings.Contains(err.Error(), "rollback SSH key cleanup failed") {
		t.Fatalf("Acquire err=%v", err)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "rollback-key-cleanup" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claims=%#v", claims)
	}
}

func TestScalewayMissingCreateIdentityPersistsRecoveryClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.createResponseWithoutServer = true
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "missing-id"})
	if err == nil || !strings.Contains(err.Error(), "omitted server id") {
		t.Fatalf("Acquire err=%v", err)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 || claims[0].Labels["recovery"] != "ambiguous-create-response" || claims[0].Labels["scaleway_ssh_key_id"] == "" {
		t.Fatalf("claims=%#v", claims)
	}
	if fake.deletedKey {
		t.Fatal("ambiguous create response deleted the access key")
	}
}

func TestScalewayReleaseRetainsIdentitylessRecoveryClaim(t *testing.T) {
	backend, _ := newTestBackend(t)
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_444444444444", "recover", providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	server := core.Server{Provider: providerName, Name: "recover", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_444444444444", "recover", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_444444444444", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "claim retained") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("cbx_444444444444", providerName); claimErr != nil || !ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
}

func TestScalewayAmbiguousCreateBindsUniqueServerBeforeCleanup(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_454545454545"
	slug := "recovered"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-recovered"
	claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	delete(liveLabels, "recovery")
	fake.server = testServer("srv-recovered", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.24")
	fake.deleteKeyErr = errors.New("key cleanup failed")

	err := backend.deleteServer(context.Background(), fake, backend.serverFromScaleway(fake.server))
	if err == nil || !strings.Contains(err.Error(), "key cleanup failed") {
		t.Fatalf("deleteServer err=%v", err)
	}
	if !fake.deletedServer {
		t.Fatal("recovered server was not deleted")
	}
	claim, ok, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "srv-recovered" || claim.Labels["scaleway_ssh_key_id"] != "key-recovered" {
		t.Fatalf("bound recovery claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestScalewayAmbiguousCreateRefusesDuplicateServers(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_464646464646"
	slug := "duplicates"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["recovery"] = "ambiguous-create"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-duplicates"
	claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	delete(liveLabels, "recovery")
	fake.servers = []*instance.Server{
		testServer("srv-first", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.25"),
		testServer("srv-second", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.26"),
	}

	err := backend.deleteServer(context.Background(), fake, backend.serverFromScaleway(fake.servers[0]))
	if err == nil || !strings.Contains(err.Error(), "found 2 matching servers") {
		t.Fatalf("deleteServer err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("ambiguous cleanup mutated resources: server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
	claim, ok, claimErr := core.ReadLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "" {
		t.Fatalf("ambiguous claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestScalewayCleanupRefusesChangedLiveSSHKeyIdentity(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	leaseID := "cbx_474747474747"
	slug := "changed-key"
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-claimed"
	claimServer := core.Server{Provider: providerName, CloudID: "srv-changed-key", Name: slug, Labels: labels}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	liveLabels := maps.Clone(labels)
	liveLabels["scaleway_ssh_key_id"] = "key-other"
	fake.server = testServer("srv-changed-key", core.LeaseProviderName(leaseID, slug), tagsFromLabels(liveLabels), "203.0.113.27")

	err := backend.deleteServer(context.Background(), fake, claimServer)
	if err == nil || !strings.Contains(err.Error(), "SSH key identity does not match") {
		t.Fatalf("deleteServer err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("changed key cleanup mutated resources: server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayReleaseCleansIdentitylessRollbackKeyClaim(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_555555555555", "key-recover", providerName, "", false, backend.clockNow())
	labels["recovery"] = "rollback-key-cleanup"
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-recover"
	server := core.Server{Provider: providerName, Name: "key-recover", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_555555555555", "key-recover", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_555555555555", Server: server}}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !fake.deletedKey {
		t.Fatal("release did not delete the identityless recovery key")
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("cbx_555555555555", providerName); claimErr != nil || ok {
		t.Fatalf("claim after recovery-key cleanup ok=%t err=%v", ok, claimErr)
	}
}

func TestScalewayRejectsKeyOnlyRecoveryClaimForLiveServer(t *testing.T) {
	for _, recovery := range []string{"ambiguous-key-create", "rollback-key-cleanup", "rollback-cleanup"} {
		t.Run(recovery, func(t *testing.T) {
			backend, fake := newTestBackend(t)
			cfg := backend.cfgForRun()
			leaseID := "cbx_565656565656"
			slug := "key-only-live"
			labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, backend.clockNow())
			labels["recovery"] = recovery
			labels["scaleway_project"] = "project-1"
			labels["scaleway_organization"] = "organization-1"
			labels["scaleway_zone"] = "fr-par-1"
			labels["scaleway_ssh_key_id"] = "key-recover"
			claimServer := core.Server{Provider: providerName, Name: slug, Labels: labels}
			if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
				t.Fatal(err)
			}
			fake.server = testServer("server-live", core.LeaseProviderName(leaseID, slug), tagsFromLabels(labels), "203.0.113.23")
			server := backend.serverFromScaleway(fake.server)
			err := backend.deleteServer(context.Background(), fake, server)
			if err == nil || !strings.Contains(err.Error(), "no server identity or valid recovery state") {
				t.Fatalf("deleteServer err=%v", err)
			}
			if fake.deletedServer || fake.deletedKey {
				t.Fatalf("key-only recovery mutated live resources: deletedServer=%t deletedKey=%t", fake.deletedServer, fake.deletedKey)
			}
		})
	}
}

func TestScalewayReleaseCleansClaimAndKeyWhenServerAlreadyDeleted(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.getErr = &scw.ResourceNotFoundError{}
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_666666666666", "gone", providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-gone"
	server := core.Server{Provider: providerName, CloudID: "srv-gone", Name: "gone", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_666666666666", "gone", cfg, server, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_666666666666", Server: server}}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if !fake.deletedKey {
		t.Fatal("release did not delete managed SSH key after server not found")
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("cbx_666666666666", providerName); claimErr != nil || ok {
		t.Fatalf("claim after stale release ok=%t err=%v", ok, claimErr)
	}
}

func TestScalewayReleaseOnlyRefusesLiveServerWithChangedTags(t *testing.T) {
	backend, fake := newTestBackend(t)
	cfg := backend.cfgForRun()
	labels := core.DirectLeaseLabels(cfg, "cbx_777777777777", "stale", providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	labels["scaleway_ssh_key_id"] = "key-stale"
	claimServer := core.Server{Provider: providerName, CloudID: "srv-stale", Name: "stale", Labels: labels}
	if err := core.ClaimLeaseTargetForConfig("cbx_777777777777", "stale", cfg, claimServer, core.SSHTarget{}, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
	fake.server = testServer("srv-stale", "changed", []string{"foreign"}, "203.0.113.20")
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_777777777777", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "non-Crabbox Scaleway") {
		t.Fatalf("Resolve release-only err=%v", err)
	}
}

func TestScalewayReleaseLeaseRefusesLiveServerWithChangedTags(t *testing.T) {
	backend, fake := newTestBackend(t)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "changed-before-release"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	fake.server.Tags = []string{"foreign"}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "non-Crabbox Scaleway") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if fake.deletedServer || fake.deletedKey {
		t.Fatalf("changed live server cleanup deleted server=%t key=%t", fake.deletedServer, fake.deletedKey)
	}
}

func TestScalewayDoctorReportsInventoryAndMissingAuth(t *testing.T) {
	backend, fake := newTestBackend(t)
	labels := core.DirectLeaseLabels(backend.cfgForRun(), "cbx_888888888888", "doc", providerName, "", false, backend.clockNow())
	labels["scaleway_project"] = "project-1"
	labels["scaleway_zone"] = "fr-par-1"
	fake.servers = []*instance.Server{testServer("srv-doc", "crabbox-cbx-doc", tagsFromLabels(labels), "203.0.113.14")}
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if result.Status != "ok" || !strings.Contains(result.Message, "leases=1") || strings.Contains(result.Message, "secret") {
		t.Fatalf("doctor=%#v", result)
	}
	if fake.lastConfig.Scaleway.Zone != "fr-par-1" {
		t.Fatalf("doctor did not use default-normalized config: %#v", fake.lastConfig.Scaleway)
	}

	backend.newClient = func(core.Config, core.Runtime) (Client, error) {
		return nil, core.Exit(3, "SCW_SECRET_KEY or Scaleway SDK secret_key is required")
	}
	result, err = backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor missing auth: %v", err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "SCW_SECRET_KEY") {
		t.Fatalf("missing auth doctor=%#v", result)
	}
}

func newTestBackend(t *testing.T) (*Backend, *fakeScalewayClient) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	fake := newFakeScalewayClient()
	cfg := core.Config{
		Provider: providerName,
		TargetOS: core.TargetLinux,
		SSHUser:  "root",
		SSHPort:  "22",
		WorkRoot: "/work/crabbox",
		Class:    "standard",
		Scaleway: core.ScalewayConfig{
			Region:        "fr-par",
			Zone:          "fr-par-1",
			Image:         "ubuntu_noble",
			Type:          "DEV1-S",
			ProjectID:     "project-1",
			SecurityGroup: "sg-1",
		},
	}
	backend := &Backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (Client, error) {
			return fake, nil
		},
		waitSSH: func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil },
		now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	backend.newClient = func(cfg core.Config, rt core.Runtime) (Client, error) {
		fake.lastConfig = cfg
		return fake, nil
	}
	return backend, fake
}

type fakeScalewayClient struct {
	instance *fakeInstanceAPI
	iam      *fakeIAMAPI
	market   *fakeMarketplaceAPI

	servers                     []*instance.Server
	server                      *instance.Server
	keys                        []*iam.SSHKey
	volumes                     map[string]*instance.Volume
	lastCreate                  *instance.CreateServerRequest
	lastListOptions             int
	lastConfig                  core.Config
	userData                    string
	deletedServer               bool
	deletedServerID             string
	deletedKey                  bool
	poweredOn                   bool
	poweredOff                  bool
	createErr                   error
	createCalls                 int
	createKeyErr                error
	getErr                      error
	getCalls                    int
	deleteErr                   error
	deleteKeyErr                error
	deleteVolumeErr             error
	updateErr                   error
	updateCalls                 int
	omitRootVolume              bool
	afterCreate                 func()
	createResponseWithoutServer bool
}

func newFakeScalewayClient() *fakeScalewayClient {
	f := &fakeScalewayClient{}
	f.instance = &fakeInstanceAPI{f: f}
	f.iam = &fakeIAMAPI{f: f}
	f.market = &fakeMarketplaceAPI{}
	return f
}

func (f *fakeScalewayClient) Instance() InstanceAPI       { return f.instance }
func (f *fakeScalewayClient) IAM() IAMAPI                 { return f.iam }
func (f *fakeScalewayClient) Marketplace() MarketplaceAPI { return f.market }
func (f *fakeScalewayClient) ProjectID() string           { return "project-1" }
func (f *fakeScalewayClient) OrganizationID() string      { return "org-1" }
func (f *fakeScalewayClient) Region() string              { return "fr-par" }
func (f *fakeScalewayClient) Zone() string                { return "fr-par-1" }

type fakeInstanceAPI struct{ f *fakeScalewayClient }

func (api *fakeInstanceAPI) ListServers(req *instance.ListServersRequest, opts ...scw.RequestOption) (*instance.ListServersResponse, error) {
	api.f.lastListOptions = len(opts)
	if api.f.servers != nil {
		return &instance.ListServersResponse{Servers: api.f.servers}, nil
	}
	if api.f.server == nil {
		return &instance.ListServersResponse{}, nil
	}
	return &instance.ListServersResponse{Servers: []*instance.Server{api.f.server}}, nil
}

func (api *fakeInstanceAPI) GetServer(req *instance.GetServerRequest, _ ...scw.RequestOption) (*instance.GetServerResponse, error) {
	api.f.getCalls++
	for _, server := range append(api.f.servers, api.f.server) {
		if server != nil && server.ID == req.ServerID {
			return &instance.GetServerResponse{Server: server}, nil
		}
	}
	if api.f.getErr != nil {
		return nil, api.f.getErr
	}
	return nil, errors.New("not found")
}

func (api *fakeInstanceAPI) CreateServer(req *instance.CreateServerRequest, _ ...scw.RequestOption) (*instance.CreateServerResponse, error) {
	api.f.createCalls++
	api.f.lastCreate = req
	if api.f.createErr != nil {
		return nil, api.f.createErr
	}
	api.f.server = testServer("srv-1", req.Name, req.Tags, "203.0.113.10")
	api.f.server.CommercialType = req.CommercialType
	rootID := "44444444-4444-4444-4444-444444444444"
	api.f.server.Volumes = map[string]*instance.VolumeServer{"0": {ID: rootID, Project: scw.StringPtr(api.f.ProjectID()), Zone: scw.Zone(api.f.Zone()), VolumeType: instance.VolumeServerVolumeTypeLSSD, Boot: true}}
	api.f.volumes = map[string]*instance.Volume{rootID: {ID: rootID, Project: api.f.ProjectID(), Zone: scw.Zone(api.f.Zone()), Server: &instance.ServerSummary{ID: api.f.server.ID}}}
	if api.f.omitRootVolume {
		api.f.server.Volumes = nil
	}
	if api.f.afterCreate != nil {
		api.f.afterCreate()
	}
	if api.f.createResponseWithoutServer {
		return &instance.CreateServerResponse{}, nil
	}
	return &instance.CreateServerResponse{Server: api.f.server}, nil
}

func (api *fakeInstanceAPI) UpdateServer(req *instance.UpdateServerRequest, _ ...scw.RequestOption) (*instance.UpdateServerResponse, error) {
	api.f.updateCalls++
	if api.f.updateErr != nil {
		return nil, api.f.updateErr
	}
	server := api.f.server
	if server == nil {
		for _, candidate := range api.f.servers {
			if candidate.ID == req.ServerID {
				server = candidate
				break
			}
		}
	}
	if server == nil {
		return nil, errors.New("not found")
	}
	if req.Tags != nil {
		server.Tags = *req.Tags
	}
	return &instance.UpdateServerResponse{Server: server}, nil
}

func (api *fakeInstanceAPI) DeleteServer(req *instance.DeleteServerRequest, _ ...scw.RequestOption) error {
	for _, server := range append(api.f.servers, api.f.server) {
		if server != nil && server.ID == req.ServerID && server.State != instance.ServerStateStopped && server.State != instance.ServerStateStoppedInPlace {
			return errors.New("precondition failed: resource is still in use, instance should be powered off")
		}
	}
	api.f.deletedServer = true
	api.f.deletedServerID = req.ServerID
	if api.f.deleteErr != nil {
		return api.f.deleteErr
	}
	for _, volume := range api.f.volumes {
		if volume.Server != nil && volume.Server.ID == req.ServerID {
			volume.Server = nil
		}
	}
	return nil
}

func (api *fakeInstanceAPI) GetVolume(req *instance.GetVolumeRequest, _ ...scw.RequestOption) (*instance.GetVolumeResponse, error) {
	volume := api.f.volumes[req.VolumeID]
	if volume == nil {
		return nil, &scw.ResourceNotFoundError{}
	}
	return &instance.GetVolumeResponse{Volume: volume}, nil
}

func (api *fakeInstanceAPI) DeleteVolume(req *instance.DeleteVolumeRequest, _ ...scw.RequestOption) error {
	if api.f.deleteVolumeErr != nil {
		return api.f.deleteVolumeErr
	}
	if volume := api.f.volumes[req.VolumeID]; volume != nil && volume.Server != nil {
		return errors.New("volume remains attached")
	}
	delete(api.f.volumes, req.VolumeID)
	return nil
}

func (api *fakeInstanceAPI) SetServerUserData(req *instance.SetServerUserDataRequest, _ ...scw.RequestOption) error {
	data, err := io.ReadAll(req.Content)
	if err != nil {
		return err
	}
	api.f.userData = string(data)
	return nil
}

func (api *fakeInstanceAPI) ServerAction(req *instance.ServerActionRequest, _ ...scw.RequestOption) (*instance.ServerActionResponse, error) {
	if req.Action == instance.ServerActionPoweron {
		api.f.poweredOn = true
	}
	if req.Action == instance.ServerActionPoweroff {
		api.f.poweredOff = true
		for _, server := range append(api.f.servers, api.f.server) {
			if server != nil && server.ID == req.ServerID {
				server.State = instance.ServerStateStopped
			}
		}
	}
	return &instance.ServerActionResponse{}, nil
}

type fakeIAMAPI struct{ f *fakeScalewayClient }

func (api *fakeIAMAPI) ListSSHKeys(req *iam.ListSSHKeysRequest, _ ...scw.RequestOption) (*iam.ListSSHKeysResponse, error) {
	return &iam.ListSSHKeysResponse{SSHKeys: api.f.keys}, nil
}
func (api *fakeIAMAPI) GetSSHKey(req *iam.GetSSHKeyRequest, _ ...scw.RequestOption) (*iam.SSHKey, error) {
	for _, key := range api.f.keys {
		if key.ID == req.SSHKeyID {
			return key, nil
		}
	}
	return nil, errors.New("not found")
}
func (api *fakeIAMAPI) CreateSSHKey(req *iam.CreateSSHKeyRequest, _ ...scw.RequestOption) (*iam.SSHKey, error) {
	key := &iam.SSHKey{ID: "key-1", Name: req.Name, PublicKey: req.PublicKey, ProjectID: req.ProjectID}
	api.f.keys = append(api.f.keys, key)
	if api.f.createKeyErr != nil {
		return nil, api.f.createKeyErr
	}
	return key, nil
}
func (api *fakeIAMAPI) DeleteSSHKey(req *iam.DeleteSSHKeyRequest, _ ...scw.RequestOption) error {
	api.f.deletedKey = true
	return api.f.deleteKeyErr
}

type fakeMarketplaceAPI struct{}

func (api *fakeMarketplaceAPI) GetLocalImageByLabel(req *marketplace.GetLocalImageByLabelRequest, _ ...scw.RequestOption) (*marketplace.LocalImage, error) {
	if req.ImageLabel == "ubuntu_noble" && req.Zone == scw.Zone("fr-par-1") && strings.EqualFold(req.CommercialType, "DEV1-S") {
		return &marketplace.LocalImage{ID: "image-1", Label: req.ImageLabel, Zone: req.Zone, CompatibleCommercialTypes: []string{"DEV1-S"}}, nil
	}
	return nil, errors.New("no image")
}

func testServer(id, name string, tags []string, publicIP string) *instance.Server {
	return &instance.Server{
		ID:             id,
		Name:           name,
		Project:        "project-1",
		Organization:   "org-1",
		Zone:           scw.Zone("fr-par-1"),
		State:          instance.ServerStateRunning,
		CommercialType: "DEV1-S",
		Tags:           append([]string(nil), tags...),
		PublicIP:       &instance.ServerIP{Address: net.ParseIP(publicIP), Dynamic: true},
	}
}
