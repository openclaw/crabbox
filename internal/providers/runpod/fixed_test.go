package runpod

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fixedPodAPI struct {
	account                    string
	pods                       []runpodPod
	input                      runpodDeployInput
	creates, deletes, port     int
	lostReply, hidden          bool
	getErr, listErr, deleteErr error
}

func (f *fixedPodAPI) Whoami(context.Context) (runpodMyself, error) {
	return runpodMyself{ID: f.account}, nil
}
func (f *fixedPodAPI) DeployPod(context.Context, runpodDeployInput) (runpodPod, error) {
	panic("fixed acquisition used ordinary deployment")
}
func (f *fixedPodAPI) DeployFixedPod(_ context.Context, input runpodDeployInput) (runpodPod, error) {
	f.creates++
	f.input = input
	pod := runpodPod{ID: "pod-fixed", Name: input.Name, Env: maps.Clone(input.Env), DesiredStatus: "RUNNING", PublicIP: "127.0.0.1", PortMappings: map[string]int{"22": f.port}}
	f.pods = append(f.pods, pod)
	if f.lostReply {
		return runpodPod{}, errors.New("create reply lost")
	}
	return pod, nil
}
func (f *fixedPodAPI) GetPod(_ context.Context, id string) (runpodPod, error) {
	if f.getErr != nil {
		return runpodPod{}, f.getErr
	}
	for _, pod := range f.pods {
		if pod.ID == id {
			return pod, nil
		}
	}
	return runpodPod{}, &runpodAPIError{StatusCode: http.StatusNotFound, Status: "404"}
}
func (f *fixedPodAPI) ListPods(context.Context) ([]runpodPod, error) {
	if f.hidden {
		return nil, f.listErr
	}
	return f.pods, f.listErr
}
func (f *fixedPodAPI) TerminatePod(_ context.Context, id string) error {
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.pods = nil
	return nil
}

func newFixedPodTest(t *testing.T) (*runpodLeaseBackend, *fixedPodAPI, core.AcquireRequest) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH stub")
	}
	testutil.IsolateUserDirs(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	cfg := core.BaseConfig()
	cfg.Provider, cfg.TargetOS = providerName, core.TargetLinux
	api := &fixedPodAPI{account: "account-one", port: port}
	backend := NewRunpodLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*runpodLeaseBackend)
	backend.client = api
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed-pod", Keep: true, Repo: core.Repo{Root: t.TempDir()}}
	return backend, api, req
}

func TestFixedPodAcquireRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lost, hidden bool
		wantError    bool
	}{
		{"fresh and replay", false, false, false},
		{"adopt lost response", true, false, false},
		{"invisible lost response never duplicates", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			if !b.SupportsRequestedLeaseID() {
				t.Fatal("fixed IDs not advertised")
			}
			api.lostReply = tc.lost
			first, err := b.Acquire(t.Context(), req)
			if (err != nil) != tc.lost {
				t.Fatalf("first acquire: %v", err)
			}
			api.hidden, api.lostReply = tc.hidden, false
			replay, err := b.Acquire(t.Context(), req)
			if (err != nil) != tc.wantError {
				t.Fatalf("replay: %v", err)
			}
			if api.creates != 1 || api.deletes != 0 {
				t.Fatalf("creates=%d deletes=%d", api.creates, api.deletes)
			}
			if tc.wantError {
				return
			}
			if replay.LeaseID != req.RequestedLeaseID || replay.Server.CloudID != "pod-fixed" || (!tc.lost && replay.Server.CloudID != first.Server.CloudID) {
				t.Fatalf("replay=%+v", replay)
			}
			key, err := core.TestboxKeyPath(req.RequestedLeaseID)
			if err != nil || replay.SSH.Key != key || api.input.PublicKey == "" {
				t.Fatalf("stored key not used: %v", err)
			}
		})
	}
}

func TestFixedPodConflicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *runpodLeaseBackend, *fixedPodAPI, *core.AcquireRequest)
	}{
		{"account", func(_ *testing.T, _ *runpodLeaseBackend, a *fixedPodAPI, _ *core.AcquireRequest) { a.account = "other" }},
		{"endpoint", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.APIURL = "https://other.example/v1"
		}},
		{"repository", func(t *testing.T, _ *runpodLeaseBackend, _ *fixedPodAPI, r *core.AcquireRequest) {
			r.Repo.Root = t.TempDir()
		}},
		{"keep", func(_ *testing.T, _ *runpodLeaseBackend, _ *fixedPodAPI, r *core.AcquireRequest) { r.Keep = false }},
		{"slug", func(_ *testing.T, _ *runpodLeaseBackend, _ *fixedPodAPI, r *core.AcquireRequest) {
			r.RequestedSlug = "different"
		}},
		{"image", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.Image = "other/image"
		}},
		{"instance", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.InstanceID = "other-gpu"
		}},
		{"cloud", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.CloudType = "COMMUNITY"
		}},
		{"template", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.TemplateID = "template-two"
		}},
		{"disk", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.DiskGB = 99
		}},
		{"user", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.User = "another"
		}},
		{"work root", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.Runpod.WorkRoot = "/different"
		}},
		{"ttl", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.TTL = 7 * time.Hour
		}},
		{"idle", func(_ *testing.T, b *runpodLeaseBackend, _ *fixedPodAPI, _ *core.AcquireRequest) {
			b.cfg.IdleTimeout = 9 * time.Minute
		}},
		{"marker", func(_ *testing.T, _ *runpodLeaseBackend, a *fixedPodAPI, _ *core.AcquireRequest) {
			a.pods[0].Env[fixedAttemptMarker] = "different"
		}},
		{"name", func(_ *testing.T, _ *runpodLeaseBackend, a *fixedPodAPI, _ *core.AcquireRequest) {
			a.pods[0].Name = "different"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			if _, err := b.Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			tc.change(t, b, api, &req)
			_, err := b.Acquire(t.Context(), req)
			if core.ExitCodeForError(err, 1) != 4 || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("conflict=%v", err)
			}
			if api.creates != 1 || api.deletes != 0 {
				t.Fatalf("unsafe mutation")
			}
		})
	}
}

func TestFixedPodStoredKeyPolicy(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated configuration", true: "missing stored key"}[missing], func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			first, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			b.cfg.SSHKey = "/missing/configured-key"
			b.cfg.Runpod.APIKey = "rotated-key"
			if missing {
				core.RemoveStoredTestboxKey(req.RequestedLeaseID)
			}
			replay, err := b.Acquire(t.Context(), req)
			if (err != nil) != missing {
				t.Fatalf("key replay=%v", err)
			}
			if !missing && replay.SSH.Key != first.SSH.Key {
				t.Fatal("key replaced")
			}
			if api.creates != 1 {
				t.Fatal("duplicate create")
			}
		})
	}
}

func TestFixedPodLifecycle(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "unbound lost response"}[lost], func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			api.lostReply = lost
			if _, err := b.Acquire(t.Context(), req); (err != nil) != lost {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			inspected, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true})
			if err != nil || inspected.Server.CloudID != "pod-fixed" {
				t.Fatalf("inspect=%+v %v", inspected, err)
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("inspection changed claim")
			}
			if !lost {
				touched, err := b.Touch(t.Context(), core.TouchRequest{Lease: inspected, State: "running"})
				if err != nil {
					t.Fatal(err)
				}
				if touched.Labels["state"] != "running" {
					t.Fatal("heartbeat not recorded")
				}
				if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: inspected}); err == nil {
					t.Fatal("stale release accepted")
				}
			}
			lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || terminal.FixedCreateIntent.State != "released" {
				t.Fatalf("terminal=%+v %v", terminal, err)
			}
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("terminal replay created resource")
			}
			lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			if api.creates != 1 || api.deletes != 1 {
				t.Fatalf("creates=%d deletes=%d", api.creates, api.deletes)
			}
		})
	}
}

func TestFixedPodAmbiguousInventory(t *testing.T) {
	for _, mode := range []string{"duplicate", "wrong marker", "list error"} {
		t.Run(mode, func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			api.lostReply = true
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected lost response")
			}
			switch mode {
			case "duplicate":
				p := api.pods[0]
				p.ID = "duplicate"
				api.pods = append(api.pods, p)
			case "wrong marker":
				api.pods[0].Env[fixedIntentMarker] = "wrong"
			case "list error":
				api.listErr = errors.New("inventory unavailable")
			}
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("ambiguous inventory adopted")
			}
			if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true}); err == nil {
				t.Fatal("ambiguous release resolved")
			}
			if api.creates != 1 || api.deletes != 0 {
				t.Fatal("unsafe mutation")
			}
		})
	}
}

func TestFixedPodLifecycleRejectsChangedIdentity(t *testing.T) {
	for _, change := range []string{"account", "name", "attempt", "missing"} {
		t.Run(change, func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "account":
				api.account = "other"
			case "name":
				api.pods[0].Name = "replacement"
			case "attempt":
				api.pods[0].Env[fixedAttemptMarker] = "other"
			case "missing":
				api.getErr = errors.New("detail unavailable")
			}
			if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true}); err == nil {
				t.Fatal("inspection accepted changed identity")
			}
			if _, err := b.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "running"}); err == nil {
				t.Fatal("heartbeat accepted changed identity")
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("release accepted changed identity")
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || !reflect.DeepEqual(before, after) || api.deletes != 0 {
				t.Fatal("claim or resource changed")
			}
		})
	}
}

func TestFixedPodInterruptedDelete(t *testing.T) {
	b, api, req := newFixedPodTest(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	api.deleteErr = errors.New("delete reply lost")
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("expected delete failure")
	}
	deleting, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Touch(t.Context(), core.TouchRequest{Lease: deleting, State: "running"}); err == nil {
		t.Fatal("heartbeat revived deleting claim")
	}
	api.pods = nil
	api.deleteErr = nil
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" || api.deletes != 1 {
		t.Fatalf("delete recovery=%+v %v", claim, err)
	}
}

func TestFixedPodReadinessFailureRetainsClaim(t *testing.T) {
	b, api, req := newFixedPodTest(t)
	req.Keep = false
	api.getErr = &runpodAPIError{StatusCode: 503, Status: "unavailable"}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected readiness failure")
	}
	if api.deletes != 0 {
		t.Fatal("fixed failure deleted pod")
	}
	api.getErr = nil
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if api.creates != 1 {
		t.Fatal("duplicate create")
	}
}

func TestFixedPodFreshInventoryConflict(t *testing.T) {
	for _, mode := range []string{"lease marker", "slug"} {
		t.Run(mode, func(t *testing.T) {
			b, api, req := newFixedPodTest(t)
			existing := runpodPod{ID: "existing", Name: core.LeaseProviderName("cbx_000000000000", req.RequestedSlug)}
			if mode == "lease marker" {
				existing.Name = "unrelated-name"
				existing.Env = map[string]string{fixedLeaseMarker: req.RequestedLeaseID}
			}
			api.pods = []runpodPod{existing}
			lease, err := b.Acquire(t.Context(), req)
			if mode == "lease marker" {
				if err == nil || api.creates != 0 {
					t.Fatal("existing lease identity accepted")
				}
			} else if err != nil || api.creates != 1 || lease.Server.Labels["slug"] == req.RequestedSlug || lease.Server.Name == existing.Name {
				t.Fatalf("slug collision was not allocated separately: %+v %v", lease, err)
			}
			if api.deletes != 0 {
				t.Fatal("inventory handling deleted pod")
			}
		})
	}
}
