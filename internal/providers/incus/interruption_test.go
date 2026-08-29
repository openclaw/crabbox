package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

// This daemon fixture survives the CLI process, like the remote Incus daemon.
type captureEvidence struct {
	Instances map[string]*api.Instance
	Snapshots map[string]*api.InstanceSnapshot
	Images    map[string]*api.Image
}
type captureCrashClient struct {
	*fakeClient
	root, phase string
}

func (c captureCrashClient) crash() {
	data, err := json.Marshal(captureEvidence{c.instances, c.snapshots, c.images})
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "daemon.json"), data, 0600); err != nil {
		panic(err)
	}
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		panic(err)
	}
	if err := process.Kill(); err != nil {
		panic(err)
	}
	select {}
}
func (c captureCrashClient) CreateSnapshot(ctx context.Context, name, snapshot string) error {
	if err := c.fakeClient.CreateSnapshot(ctx, name, snapshot); err != nil {
		return err
	}
	if c.phase == "snapshot" {
		c.crash()
	}
	return nil
}
func (c captureCrashClient) PublishSnapshot(ctx context.Context, name, snapshot string, props map[string]string) (string, error) {
	id, err := c.fakeClient.PublishSnapshot(ctx, name, snapshot, props)
	if err == nil {
		c.crash()
	}
	return id, err
}

func TestIncusCheckpointProcessDeathRetainsRecovery(t *testing.T) {
	if root := os.Getenv("CRABBOX_TEST_CAPTURE_CRASH"); root != "" {
		b, fake, req := lifecycleFixture(t)
		t.Setenv("HOME", root)
		t.Setenv("XDG_STATE_HOME", root)
		t.Setenv("CRABBOX_PROVIDER", "incus")
		t.Setenv("CRABBOX_CONFIG", filepath.Join(root, "missing.yaml"))
		req.Repo.Root = root
		t.Chdir(root)
		lease, err := b.Acquire(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		client := captureCrashClient{fake, root, os.Getenv("CRABBOX_TEST_CAPTURE_PHASE")}
		newClient = func(Config) (instanceClient, error) { return client, nil }
		err = (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), []string{"checkpoint", "create", "--provider", "incus", "--id", lease.LeaseID, "--mode", "native", "--json"})
		t.Fatalf("capture did not reach crash boundary: %v", err)
	}
	for _, phase := range []string{"snapshot", "publish"} {
		t.Run(phase, func(t *testing.T) {
			_, fake, _ := lifecycleFixture(t)
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIncusCheckpointProcessDeathRetainsRecovery$")
			cmd.Env = append(os.Environ(), "CRABBOX_TEST_CAPTURE_CRASH="+root, "CRABBOX_TEST_CAPTURE_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil {
				t.Fatalf("child not killed at mutation: %v %s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(root, "daemon.json"))
			if err != nil {
				t.Fatalf("child did not reach mutation: %v %s", err, output)
			}
			var evidence captureEvidence
			if err := json.Unmarshal(data, &evidence); err != nil {
				t.Fatal(err)
			}
			fake.instances, fake.snapshots, fake.images = evidence.Instances, evidence.Snapshots, evidence.Images
			t.Setenv("HOME", root)
			t.Setenv("XDG_STATE_HOME", root)
			var stdout bytes.Buffer
			app := core.App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.Run(context.Background(), []string{"checkpoint", "list", "--json"}); err != nil {
				t.Fatal(err)
			}
			var records []struct{ ID string }
			if err := json.Unmarshal(stdout.Bytes(), &records); err != nil || len(records) != 1 {
				t.Fatalf("missing reserved record: %s %v", stdout.String(), err)
			}
			id := records[0].ID
			stdout.Reset()
			if err := app.Run(context.Background(), []string{"checkpoint", "inspect", id, "--verify", "--json"}); err != nil {
				t.Fatal(err)
			}
			want := "available"
			if phase == "snapshot" {
				want = "pending"
			}
			var verified struct {
				ProviderState string `json:"providerState"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &verified); err != nil {
				t.Fatal(err)
			}
			if verified.ProviderState != want {
				t.Fatalf("unrecoverable %s capture after SIGKILL: %s", phase, stdout.String())
			}
			err = app.Run(context.Background(), []string{"checkpoint", "delete", id})
			if phase == "snapshot" {
				if err == nil || len(fake.snapshots) != 1 {
					t.Fatalf("uncertain publication identity erased: %v", err)
				}
			} else if err != nil || len(fake.images) != 0 {
				t.Fatalf("recovered image cleanup: %v", err)
			}
		})
	}
}

func TestIncusIncompleteAllocationLostDeleteReply(t *testing.T) {
	for _, scenario := range []string{"unresolved-create", "committed-create", "cleanup-replay"} {
		reachedServer := scenario != "unresolved-create"
		t.Run(scenario, func(t *testing.T) {
			b, fake, req := lifecycleFixture(t)
			sentinel := errors.New("lost response")
			if reachedServer {
				fake.lostCreateReply = sentinel
			} else {
				fake.createErr = sentinel
			}
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Fatal("expected incomplete create")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			lease := LeaseTarget{LeaseID: req.RequestedLeaseID, Server: core.Server{Provider: providerName, CloudID: claim.FixedCreateIntent.Attempt["name"], Labels: claim.Labels}}
			fake.lostDeleteReply = sentinel
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err == nil {
				t.Fatal("expected uncertain delete")
			}
			fake.lostDeleteReply = nil
			if scenario == "cleanup-replay" {
				err = b.Cleanup(context.Background(), CleanupRequest{})
			} else {
				resolved := lease
				if reachedServer {
					resolved, err = b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
					if err != nil {
						t.Fatalf("resolve deleted incomplete allocation: %v", err)
					}
				}
				err = b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: resolved, Force: true})
			}
			if !reachedServer {
				if err == nil {
					t.Fatal("unresolved create 404 was finalized")
				}
				return
			}
			if err != nil {
				t.Fatalf("committed incomplete allocation delete cannot reconcile: %v", err)
			}
			claim, err = core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State != "released" {
				t.Fatalf("missing terminal claim: %+v %v", claim, err)
			}
		})
	}
}

func TestIncusCLIPendingCaptureLostDeleteReply(t *testing.T) {
	b, fake, req := lifecycleFixture(t)
	t.Setenv("CRABBOX_PROVIDER", "incus")
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Chdir(req.Repo.Root)
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	fake.lostPublishReply = errors.New("publish committed reply lost")
	var stdout bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.Run(context.Background(), []string{"checkpoint", "create", "--provider", "incus", "--id", lease.LeaseID, "--mode", "native"}); err == nil {
		t.Fatal("expected publish error")
	}
	stdout.Reset()
	if err := app.Run(context.Background(), []string{"checkpoint", "list", "--json"}); err != nil {
		t.Fatal(err)
	}
	var records []struct{ ID string }
	if err := json.Unmarshal(stdout.Bytes(), &records); err != nil || len(records) != 1 {
		t.Fatalf("record missing: %s %v", stdout.String(), err)
	}
	id := records[0].ID
	fake.lostImageDeleteReply = errors.New("delete committed reply lost")
	if err := app.Run(context.Background(), []string{"checkpoint", "delete", id}); err == nil {
		t.Fatal("expected delete error")
	}
	if len(fake.images) != 0 {
		t.Fatal("delete did not commit")
	}
	fake.lostImageDeleteReply = nil
	if err := app.Run(context.Background(), []string{"checkpoint", "delete", id}); err != nil {
		t.Fatalf("retry wedged after image deletion: %v", err)
	}
}

type publishExpiryServer struct {
	incusclient.InstanceServer
	request api.ImagesPost
}

func (s *publishExpiryServer) CreateImage(req api.ImagesPost, _ *incusclient.ImageCreateArgs) (incusclient.Operation, error) {
	s.request = req
	return publishExpiryOperation{}, nil
}

type publishExpiryOperation struct{ incusclient.Operation }

func (publishExpiryOperation) WaitContext(context.Context) error { return nil }
func (publishExpiryOperation) Get() api.Operation {
	return api.Operation{StatusCode: api.Success, Metadata: map[string]any{"fingerprint": strings.Repeat("b", 64)}}
}
func TestSDKCheckpointPublicationClearsInheritedExpiry(t *testing.T) {
	server := &publishExpiryServer{}
	client := &sdkClient{server: server}
	if _, err := client.PublishSnapshot(context.Background(), "source", "snapshot", map[string]string{checkpointProperty: "chk_expiry"}); err != nil {
		t.Fatal(err)
	}
	// v7.1.0 Export preserves metadata.yaml expiry unless expiration.IsZero is false;
	// the server interprets exported Unix zero as no expiry.
	if server.request.ExpiresAt.IsZero() || server.request.ExpiresAt.Unix() != 0 {
		t.Fatalf("publication inherits base image expiry: %v", server.request.ExpiresAt)
	}
}

func TestIncusCheckpointPersistenceFailurePreventsMutation(t *testing.T) {
	for _, phase := range []string{"capture", "delete-recovered"} {
		t.Run(phase, func(t *testing.T) {
			b, fake, req := lifecycleFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("canonical store unavailable")
			failPersist := func(core.NativeCheckpointCreateResult) error { return sentinel }
			capture := core.NativeCheckpointCreateRequest{Config: b.cfg, Server: lease.Server, LeaseID: lease.LeaseID, CheckpointID: "chk_persistence", Persist: failPersist}
			p := Provider{}
			if phase == "capture" {
				_, err = p.CreateNativeCheckpoint(context.Background(), capture)
				if !errors.Is(err, sentinel) || len(fake.snapshots) != 0 || len(fake.images) != 0 {
					t.Fatalf("capture mutated without cleanup identity: %v", err)
				}
				return
			}
			capture.Persist = (&checkpointRecorder{}).persist
			fake.lostPublishReply = errors.New("lost publish response")
			checkpoint, err := p.CreateNativeCheckpoint(context.Background(), capture)
			if err == nil {
				t.Fatal("expected uncertain publication")
			}
			err = p.DeleteNativeCheckpoint(context.Background(), core.NativeCheckpointResourceRequest{Image: checkpoint.Image, Metadata: checkpoint.Metadata, Persist: failPersist})
			if !errors.Is(err, sentinel) || len(fake.snapshots) != 1 || len(fake.images) != 1 {
				t.Fatalf("deletion mutated before recovered identity persisted: %v", err)
			}
		})
	}
}
