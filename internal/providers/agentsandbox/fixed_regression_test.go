package agentsandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func fixedRegressionFixture(t *testing.T) (*backend, *fixedPositiveClient, core.LeaseClaim) {
	t.Helper()
	cfg := testAgentSandboxConfig(t)
	cfg.TTL = time.Hour
	const id = "cbx_174200000005"
	fake := &fixedPositiveClient{fakeKubernetesClient: readyFakeClient(cfg), t: t, leaseID: id}
	fake.objects[warmPoolResource+"/"+cfg.AgentSandbox.Namespace+"/"+cfg.AgentSandbox.WarmPool] = &kubernetesObject{Metadata: objectMeta{Name: cfg.AgentSandbox.WarmPool, UID: "pool-fixed-regression"}}
	b := testBackend(cfg, fake.fakeKubernetesClient, nil, nil)
	b.newClient = func(context.Context, core.Config, core.Runtime) (kubernetesClient, error) { return fake, nil }
	req := core.FixedWarmupRequest{
		WarmupRequest:    core.WarmupRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "fixed-regression", Keep: true},
		RequestedLeaseID: id,
		OnAcquired:       func(core.FixedAcquisitionReceipt) error { return nil },
	}
	if err := b.WarmupFixed(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	return b, fake, claim
}

func TestFixedMissingRunPreservesCustodyWithForgetMissing(t *testing.T) {
	for _, forget := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain", true: "forget"}[forget], func(t *testing.T) {
			b, fake, before := fixedRegressionFixture(t)
			b.cfg.AgentSandbox.ForgetMissing = forget
			delete(fake.objects, sandboxClaimResource+"/"+b.cfg.AgentSandbox.Namespace+"/"+claimNameFromLocalClaim(before))
			_, err := b.Run(t.Context(), core.RunRequest{ID: before.LeaseID, Repo: core.Repo{Root: before.RepoRoot}, NoSync: true, Command: []string{"not-run"}})
			if err == nil || !strings.Contains(err.Error(), "missing") {
				t.Fatalf("missing fixed claim must reject the command: %v", err)
			}
			after, err := core.ReadLeaseClaim(before.LeaseID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("missing fixed claim lost custody: %v", err)
			}
			if len(fake.execs) != 0 || fake.deletes != 0 || fake.creates != 1 {
				t.Fatal("missing fixed claim executed, deleted, or reallocated resources")
			}
		})
	}
}

func TestFixedStatusAndListRejectReplacementWorkload(t *testing.T) {
	for _, resource := range []string{"sandbox", "pod"} {
		t.Run(resource, func(t *testing.T) {
			b, fake, before := fixedRegressionFixture(t)
			if resource == "sandbox" {
				key := sandboxResource + "/" + b.cfg.AgentSandbox.Namespace + "/" + before.Labels[claimLabelSandboxName]
				fake.objects[key].Metadata.UID = "uid-replacement-sandbox"
			}
			for key, pods := range fake.pods {
				for i := range pods {
					if resource == "sandbox" {
						pods[i].OwnerReferences[0].UID = "uid-replacement-sandbox"
					} else {
						pods[i].UID = "uid-replacement-pod"
					}
				}
				fake.pods[key] = pods
			}
			for _, wait := range []bool{false, true} {
				view, err := b.Status(t.Context(), core.StatusRequest{ID: before.LeaseID, Wait: wait})
				if err == nil || !strings.Contains(err.Error(), "workload identity changed") || view.Ready {
					t.Fatalf("replacement workload published ready: view=%+v err=%v", view, err)
				}
			}
			if _, err := b.List(t.Context(), core.ListRequest{}); err == nil || !strings.Contains(err.Error(), "workload identity changed") {
				t.Fatalf("inventory accepted replacement workload: %v", err)
			}
			after, err := core.ReadLeaseClaim(before.LeaseID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("observation rebound fixed identity: %v", err)
			}
		})
	}
}

func TestFixedExpiredRunCleanupHonorsBudgetAndCaller(t *testing.T) {
	for _, mode := range []string{"slow completion", "canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b, fake, before := fixedRegressionFixture(t)
				b.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "deadline" {
					var deadlineCancel context.CancelFunc
					parent, deadlineCancel = context.WithTimeout(parent, 2*time.Second)
					defer deadlineCancel()
				}
				fake.afterForeground = func(context.Context) {
					fake.getStarted = make(chan struct{}, 1)
					fake.getRelease = make(chan struct{})
					if mode == "canceled" {
						cancel()
					} else if mode == "slow completion" {
						go func() {
							time.Sleep(20 * time.Second)
							close(fake.getRelease)
						}()
					}
				}
				started := time.Now()
				result, err := b.Run(parent, core.RunRequest{ID: before.LeaseID, Repo: core.Repo{Root: before.RepoRoot}, NoSync: true, Command: []string{"not-run"}})
				if mode == "slow completion" {
					<-fake.getRelease
				}
				if err == nil || !strings.Contains(err.Error(), "TTL expiry") || len(fake.execs) != 0 {
					t.Fatalf("expired run must reject execution: result=%+v err=%v", result, err)
				}
				after, readErr := core.ReadLeaseClaim(before.LeaseID)
				if readErr != nil || fake.foreground != 1 || after.CloudImmutableID != before.CloudImmutableID {
					t.Fatalf("cleanup lost fixed custody: %v", readErr)
				}
				if mode == "slow completion" {
					if after.FixedCreateIntent.State != "released" || result.Session == nil || result.Session.Kept {
						t.Fatalf("fixed cleanup stopped before foreground completion: result=%+v err=%v", result, err)
					}
					return
				}
				want := error(context.Canceled)
				if mode == "deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) || time.Since(started) > 2*time.Second || after.FixedCreateIntent.State == "released" || result.Session == nil || !result.Session.Kept {
					t.Fatalf("cleanup ignored caller or lost pending receipt: elapsed=%s result=%+v err=%v", time.Since(started), result, err)
				}
				fake.getStarted, fake.getRelease, fake.afterForeground = nil, nil, nil
				if err := b.Stop(t.Context(), core.StopRequest{ID: before.LeaseID}); err != nil {
					t.Fatalf("pending cleanup cannot resume: %v", err)
				}
			})
		})
	}
}

type poolReplacingClient struct {
	*fixedPositiveClient
	afterCreate func()
}

func (c *poolReplacingClient) Create(ctx context.Context, ref resourceRef, namespace string, obj *kubernetesObject) (*kubernetesObject, error) {
	live, err := c.fixedPositiveClient.Create(ctx, ref, namespace, obj)
	if c.afterCreate != nil {
		c.afterCreate()
	}
	return live, err
}

func TestFixedWarmupRejectsPoolReplacementDuringAcquisition(t *testing.T) {
	for _, phase := range []string{"create", "lost response", "readiness"} {
		t.Run(phase, func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			cfg.TTL = time.Hour
			const id = "cbx_174200000006"
			fake := &poolReplacingClient{fixedPositiveClient: &fixedPositiveClient{fakeKubernetesClient: readyFakeClient(cfg), t: t, leaseID: id}}
			poolKey := warmPoolResource + "/" + cfg.AgentSandbox.Namespace + "/" + cfg.AgentSandbox.WarmPool
			fake.objects[poolKey] = &kubernetesObject{Metadata: objectMeta{Name: cfg.AgentSandbox.WarmPool, UID: "original-pool-uid"}}
			replacePool := func() { fake.objects[poolKey].Metadata.UID = "replacement-pool-uid" }
			if phase != "readiness" {
				fake.afterCreate = replacePool
			}
			if phase == "lost response" {
				fake.createErrs = []error{errors.New("accepted create response lost")}
			}
			b := testBackend(cfg, fake.fakeKubernetesClient, nil, nil)
			b.newClient = func(context.Context, core.Config, core.Runtime) (kubernetesClient, error) { return fake, nil }
			acks := 0
			req := core.FixedWarmupRequest{
				WarmupRequest:    core.WarmupRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "fixed-pool-race", Keep: true},
				RequestedLeaseID: id,
				OnAcquired: func(core.FixedAcquisitionReceipt) error {
					acks++
					if phase == "readiness" {
						replacePool()
					}
					return nil
				},
			}
			if err := b.WarmupFixed(t.Context(), req); err == nil || !strings.Contains(err.Error(), "warm pool incarnation changed") {
				t.Fatalf("warmup accepted replacement pool: %v", err)
			}
			wantAcks := 0
			if phase == "readiness" {
				wantAcks = 1
			}
			claim, err := core.ReadLeaseClaim(id)
			if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" || claim.Labels[fixedPoolUIDLabel] != "original-pool-uid" || claim.Labels["fixed_pod_uid"] != "" || acks != wantAcks {
				t.Fatalf("pool race published acquisition or lost original intent: acks=%d err=%v", acks, err)
			}
			if err := b.WarmupFixed(t.Context(), req); err == nil || fake.creates != 1 || fake.deletes != 0 || acks != wantAcks {
				t.Fatalf("pool race retry replaced original attempt: %v", err)
			}
		})
	}
}
