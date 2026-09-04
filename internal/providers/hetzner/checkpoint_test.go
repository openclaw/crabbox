package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestHetznerCheckpointCLIWithMalformedUnrelatedConfig(t *testing.T) {
	for _, operation := range []string{"delete", "prune", "image-delete"} {
		for _, outcome := range []string{"owned", "wrong-owner", "read-failure", "delete-failure"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				installHetznerClaimState(t)
				t.Chdir(t.TempDir())
				if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte("machine0: [\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				image := ownedCheckpointImage()
				client := &fakeHetznerSnapshotClient{created: image}
				wantError := ""
				switch outcome {
				case "wrong-owner":
					image.Labels["lease"] = "cbx_other123456"
					wantError = "mismatched lease label"
				case "read-failure":
					client.getImageErr = errors.New("fixture lookup uncertain")
					wantError = "fixture lookup uncertain"
				case "delete-failure":
					client.deleteErr = errors.New("fixture deletion uncertain")
					wantError = "fixture deletion uncertain"
				}
				installHetznerCheckpointHooks(t, client)
				metaPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints", "chk_123abc", "checkpoint.json")
				before, err := json.Marshal(map[string]any{
					"id": "chk_123abc", "kind": core.CheckpointKindHetzner, "provider": providerName,
					"createdAt": time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
					"native":    map[string]any{"provider": providerName, "imageId": "99", "region": "fsn1", "architecture": "x86", "direct": true, "metadata": checkpointMetadata()},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(metaPath, before, 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout bytes.Buffer
				app := core.App{Stdout: &stdout, Stderr: io.Discard}
				for _, args := range [][]string{{"checkpoint", "inspect", "chk_123abc", "--verify", "--json"}, {"checkpoint", "list", "--verify", "--json"}} {
					stdout.Reset()
					if err := app.Run(t.Context(), args); err != nil {
						t.Fatal(err)
					}
					if outcome == "wrong-owner" || outcome == "read-failure" {
						if !strings.Contains(stdout.String(), wantError) {
							t.Errorf("%v did not report %q: %s", args, wantError, &stdout)
						}
					} else if !strings.Contains(stdout.String(), `"providerState":"available"`) {
						t.Errorf("%v did not verify owned snapshot: %s", args, &stdout)
					}
				}
				args := []string{"checkpoint", "delete", "chk_123abc"}
				if operation == "prune" {
					args = []string{"checkpoint", "prune", "--older-than", "24h", "--kind", "native"}
				} else if operation == "image-delete" {
					args = []string{"image", "delete", "99", "--provider", providerName, "--region", "fsn1"}
				}
				err = app.Run(t.Context(), args)
				if wantError == "" {
					if err != nil {
						t.Errorf("owned cleanup failed: %v", err)
					}
					if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
						t.Errorf("metadata retained after successful cleanup: %v", err)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), wantError) {
						t.Errorf("cleanup err=%v, want %q", err, wantError)
					}
					after, readErr := os.ReadFile(metaPath)
					if readErr != nil || !bytes.Equal(before, after) {
						t.Errorf("uncertain cleanup changed local record: %v", readErr)
					}
				}
				wantEvents := []string{"get-image", "get-image", "get-image"}
				if outcome == "owned" || outcome == "delete-failure" {
					wantEvents = append(wantEvents, "delete-image")
					if !reflect.DeepEqual(client.deletedImages, []int64{99}) {
						t.Errorf("deleted IDs=%v", client.deletedImages)
					}
				}
				if !reflect.DeepEqual(client.events, wantEvents) {
					t.Errorf("provider events=%v, want %v", client.events, wantEvents)
				}
			})
		}
	}
}

type fakeHetznerSnapshotClient struct {
	server            Server
	serverErr         error
	created           core.HetznerImage
	createErr         error
	getImages         []core.HetznerImage
	getImageErr       error
	getImageFn        func(context.Context, int64) (core.HetznerImage, error)
	deleteErr         error
	createServerID    int64
	createDescription string
	createLabels      map[string]string
	getImageCalls     int
	deletedImages     []int64
	events            []string
}

func (f *fakeHetznerSnapshotClient) GetServer(context.Context, int64) (Server, error) {
	f.events = append(f.events, "get-server")
	return f.server, f.serverErr
}

func (f *fakeHetznerSnapshotClient) CreateServerSnapshot(_ context.Context, serverID int64, description string, labels map[string]string) (core.HetznerImage, error) {
	f.events = append(f.events, "create-snapshot")
	f.createServerID = serverID
	f.createDescription = description
	f.createLabels = cloneMetadata(labels)
	return f.created, f.createErr
}

func (f *fakeHetznerSnapshotClient) GetImage(ctx context.Context, imageID int64) (core.HetznerImage, error) {
	f.events = append(f.events, "get-image")
	f.getImageCalls++
	if f.getImageFn != nil {
		return f.getImageFn(ctx, imageID)
	}
	if f.getImageErr != nil {
		return core.HetznerImage{}, f.getImageErr
	}
	if len(f.getImages) == 0 {
		return f.created, nil
	}
	image := f.getImages[0]
	if len(f.getImages) > 1 {
		f.getImages = f.getImages[1:]
	}
	return image, nil
}

func (f *fakeHetznerSnapshotClient) DeleteImage(_ context.Context, imageID int64) error {
	f.events = append(f.events, "delete-image")
	f.deletedImages = append(f.deletedImages, imageID)
	return f.deleteErr
}

func installHetznerCheckpointHooks(t *testing.T, client *fakeHetznerSnapshotClient) *[]string {
	t.Helper()
	oldClient := newHetznerSnapshotClient
	oldPrepare := prepareHetznerCheckpointSource
	oldNow := checkpointNow
	oldSleep := checkpointSleep
	oldInterval := checkpointPollInterval
	events := &client.events
	newHetznerSnapshotClient = func() (hetznerSnapshotClient, error) { return client, nil }
	prepareHetznerCheckpointSource = func(context.Context, core.SSHTarget) error {
		*events = append(*events, "prepare-source")
		return nil
	}
	checkpointNow = time.Now
	checkpointPollInterval = time.Millisecond
	checkpointSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() {
		newHetznerSnapshotClient = oldClient
		prepareHetznerCheckpointSource = oldPrepare
		checkpointNow = oldNow
		checkpointSleep = oldSleep
		checkpointPollInterval = oldInterval
	})
	return events
}

func TestHetznerNativeCheckpointCapabilityMatrix(t *testing.T) {
	provider := Provider{}
	base := core.NativeCheckpointRequest{
		Config:   core.Config{TargetOS: core.TargetLinux},
		Server:   Server{CloudID: "42"},
		Target:   core.SSHTarget{TargetOS: core.TargetLinux},
		Strategy: core.CheckpointStrategyDiskSnapshot,
	}
	capability, ok := provider.NativeCheckpointCapability(base)
	if !ok || !capability.Direct || capability.Kind != core.CheckpointKindHetzner || capability.CreateUnsupported != "" {
		t.Fatalf("capability=%+v ok=%v", capability, ok)
	}
	image := base
	image.Strategy = core.CheckpointStrategyImage
	capability, ok = provider.NativeCheckpointCapability(image)
	if !ok || capability.CreateUnsupported == "" {
		t.Fatalf("image capability=%+v ok=%v", capability, ok)
	}
	for name, mutate := range map[string]func(*core.NativeCheckpointRequest){
		"brokered":             func(req *core.NativeCheckpointRequest) { req.Config.Coordinator = "https://coordinator.example" },
		"no cloud id":          func(req *core.NativeCheckpointRequest) { req.Server.CloudID = "" },
		"non-numeric cloud id": func(req *core.NativeCheckpointRequest) { req.Server.CloudID = "server-name" },
		"non-linux":            func(req *core.NativeCheckpointRequest) { req.Target.TargetOS = core.TargetMacOS },
	} {
		t.Run(name, func(t *testing.T) {
			req := base
			mutate(&req)
			if got, ok := provider.NativeCheckpointCapability(req); ok {
				t.Fatalf("capability=%+v, want unsupported", got)
			}
		})
	}
}

func TestCreateHetznerCheckpointWaitFalseRecordsBinding(t *testing.T) {
	installHetznerClaimState(t)
	source := checkpointHetznerServer()
	seedHetznerClaim(t, source)
	client := &fakeHetznerSnapshotClient{
		server:  source,
		created: core.HetznerImage{ID: 99, Type: "snapshot", Status: "creating", Description: "named", Architecture: "x86"},
	}
	events := installHetznerCheckpointHooks(t, client)
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), checkpointCreateRequest(false, 30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.ID != "99" || result.Image.State != "creating" || result.Image.Region != "fsn1" || result.Image.Architecture != "x86" || !result.Image.Direct {
		t.Fatalf("image=%+v", result.Image)
	}
	wantMetadata := checkpointMetadata()
	if !reflect.DeepEqual(result.Metadata, wantMetadata) {
		t.Fatalf("metadata=%v, want %v", result.Metadata, wantMetadata)
	}
	if client.createServerID != 42 || client.createDescription != "named" {
		t.Fatalf("create server=%d description=%q", client.createServerID, client.createDescription)
	}
	wantLabels := checkpointLabels(wantMetadata)
	if !reflect.DeepEqual(client.createLabels, wantLabels) {
		t.Fatalf("labels=%v, want %v", client.createLabels, wantLabels)
	}
	if client.getImageCalls != 0 {
		t.Fatalf("GetImage calls=%d, want 0", client.getImageCalls)
	}
	if want := []string{"get-server", "prepare-source", "create-snapshot"}; !reflect.DeepEqual(*events, want) {
		t.Fatalf("events=%v, want %v", *events, want)
	}
}

func TestCreateHetznerCheckpointRejectsOwnershipBeforeGuestReset(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Server)
		seed   bool
	}{
		"missing canonical label": {mutate: func(server *Server) { delete(server.Labels, "created_by") }, seed: true},
		"source lease mismatch":   {mutate: func(server *Server) { server.Labels["lease"] = "cbx_other123456" }, seed: true},
		"missing exact claim":     {mutate: func(*Server) {}, seed: false},
	} {
		t.Run(name, func(t *testing.T) {
			installHetznerClaimState(t)
			source := checkpointHetznerServer()
			if tc.seed {
				seedHetznerClaim(t, source)
			}
			tc.mutate(&source)
			client := &fakeHetznerSnapshotClient{server: source, created: core.HetznerImage{ID: 99}}
			events := installHetznerCheckpointHooks(t, client)
			_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), checkpointCreateRequest(false, 0))
			if err == nil {
				t.Fatal("expected ownership refusal")
			}
			if len(*events) != 1 || (*events)[0] != "get-server" {
				t.Fatalf("events=%v, guest reset or snapshot ran before ownership validation", *events)
			}
		})
	}
}

func TestCreateHetznerCheckpointWaitOutcomesRetainPartialResult(t *testing.T) {
	for name, tc := range map[string]struct {
		configure func(*fakeHetznerSnapshotClient)
		wantState string
		wantError string
	}{
		"success": {
			configure: func(client *fakeHetznerSnapshotClient) {
				client.getImages = []core.HetznerImage{{ID: 99, Type: "snapshot", Status: "available", Architecture: "x86"}}
			},
			wantState: "available",
		},
		"timeout": {
			configure: func(*fakeHetznerSnapshotClient) {},
			wantState: "creating",
			wantError: "timed out",
		},
		"failure": {
			configure: func(client *fakeHetznerSnapshotClient) { client.created.Status = "error" },
			wantState: "error",
			wantError: "failed",
		},
		"cancel": {
			configure: func(*fakeHetznerSnapshotClient) {},
			wantState: "creating",
			wantError: context.Canceled.Error(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			installHetznerClaimState(t)
			source := checkpointHetznerServer()
			seedHetznerClaim(t, source)
			client := &fakeHetznerSnapshotClient{server: source, created: core.HetznerImage{ID: 99, Type: "snapshot", Status: "creating", Architecture: "x86"}}
			tc.configure(client)
			installHetznerCheckpointHooks(t, client)
			request := checkpointCreateRequest(true, time.Minute)
			if name == "timeout" {
				request.WaitTimeout = 0
			}
			if name == "cancel" {
				checkpointSleep = func(context.Context, time.Duration) error { return context.Canceled }
			}
			result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), request)
			if result.Image.ID != "99" || result.Image.State != tc.wantState {
				t.Fatalf("image=%+v", result.Image)
			}
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("err=%v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestCreateHetznerCheckpointCapsFinalPollSleepAndSkipsExpiredGet(t *testing.T) {
	installHetznerClaimState(t)
	source := checkpointHetznerServer()
	seedHetznerClaim(t, source)
	client := &fakeHetznerSnapshotClient{
		server:  source,
		created: core.HetznerImage{ID: 99, Type: "snapshot", Status: "creating", Architecture: "x86"},
	}
	installHetznerCheckpointHooks(t, client)
	now := time.Unix(1_700_000_000, 0)
	checkpointNow = func() time.Time { return now }
	checkpointPollInterval = 15 * time.Second
	var sleeps []time.Duration
	checkpointSleep = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		now = now.Add(delay)
		return nil
	}

	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), checkpointCreateRequest(true, 2*time.Second))
	if err == nil || err.Error() != "timed out waiting for Hetzner snapshot 99; last state=creating" {
		t.Fatalf("err=%v", err)
	}
	if result.Image.ID != "99" || result.Image.State != "creating" {
		t.Fatalf("image=%+v", result.Image)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{2 * time.Second}) {
		t.Fatalf("sleeps=%v, want [2s]", sleeps)
	}
	if client.getImageCalls != 0 {
		t.Fatalf("GetImage calls=%d, want 0", client.getImageCalls)
	}
}

func TestCreateHetznerCheckpointWaitTimeoutCancelsInFlightGet(t *testing.T) {
	installHetznerClaimState(t)
	source := checkpointHetznerServer()
	seedHetznerClaim(t, source)
	client := &fakeHetznerSnapshotClient{
		server:  source,
		created: core.HetznerImage{ID: 99, Type: "snapshot", Status: "creating", Architecture: "x86"},
		getImageFn: func(ctx context.Context, _ int64) (core.HetznerImage, error) {
			<-ctx.Done()
			return core.HetznerImage{}, ctx.Err()
		},
	}
	installHetznerCheckpointHooks(t, client)
	parentCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := (Provider{}).CreateNativeCheckpoint(parentCtx, checkpointCreateRequest(true, 20*time.Millisecond))
	if err == nil || err.Error() != "timed out waiting for Hetzner snapshot 99; last state=creating" {
		t.Fatalf("err=%v", err)
	}
	if result.Image.ID != "99" || result.Image.State != "creating" {
		t.Fatalf("image=%+v", result.Image)
	}
	if client.getImageCalls != 1 {
		t.Fatalf("GetImage calls=%d, want 1", client.getImageCalls)
	}
}

func TestCreateHetznerCheckpointParentCancellationCancelsInFlightGet(t *testing.T) {
	installHetznerClaimState(t)
	source := checkpointHetznerServer()
	seedHetznerClaim(t, source)
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeHetznerSnapshotClient{
		server:  source,
		created: core.HetznerImage{ID: 99, Type: "snapshot", Status: "creating", Architecture: "x86"},
		getImageFn: func(ctx context.Context, _ int64) (core.HetznerImage, error) {
			cancel()
			<-ctx.Done()
			return core.HetznerImage{}, ctx.Err()
		},
	}
	installHetznerCheckpointHooks(t, client)

	result, err := (Provider{}).CreateNativeCheckpoint(parentCtx, checkpointCreateRequest(true, time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if result.Image.ID != "99" || result.Image.State != "creating" {
		t.Fatalf("image=%+v", result.Image)
	}
	if client.getImageCalls != 1 {
		t.Fatalf("GetImage calls=%d, want 1", client.getImageCalls)
	}
}

func TestCreateHetznerCheckpointPersistsBeforeReadiness(t *testing.T) {
	for _, failPersist := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistence failure=%t", failPersist), func(t *testing.T) {
			installHetznerClaimState(t)
			source := checkpointHetznerServer()
			seedHetznerClaim(t, source)
			var saved core.NativeCheckpointCreateResult
			client := &fakeHetznerSnapshotClient{
				server:  source,
				created: core.HetznerImage{ID: 99, Type: "snapshot", Status: "creating", Architecture: "x86"},
				getImageFn: func(context.Context, int64) (core.HetznerImage, error) {
					if saved.Image.ID != "99" || !reflect.DeepEqual(saved.Metadata, checkpointMetadata()) {
						t.Errorf("snapshot cleanup identity was not persisted before readiness: %+v", saved)
					}
					return core.HetznerImage{ID: 99, Type: "snapshot", Status: "available", Architecture: "x86"}, nil
				},
			}
			installHetznerCheckpointHooks(t, client)
			request := checkpointCreateRequest(true, time.Second)
			persistErr := errors.New("checkpoint store unavailable")
			request.Persist = func(result core.NativeCheckpointCreateResult) error {
				if failPersist {
					return persistErr
				}
				saved = result
				return nil
			}
			result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), request)
			if result.Image.ID != "99" {
				t.Fatalf("creation identity lost: %+v", result)
			}
			if failPersist {
				if !errors.Is(err, persistErr) || client.getImageCalls != 0 {
					t.Fatalf("persistence failure reached readiness: err=%v reads=%d", err, client.getImageCalls)
				}
			} else if err != nil || client.getImageCalls != 1 {
				t.Fatalf("creation failed: err=%v reads=%d", err, client.getImageCalls)
			}
		})
	}
}

func TestHetznerCheckpointVerifyAndDeleteRequireExactOwnedSnapshot(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate    func(*core.HetznerImage)
		wantError string
	}{
		"system image":          {mutate: func(image *core.HetznerImage) { image.Type = "system" }, wantError: "type=system"},
		"label mismatch":        {mutate: func(image *core.HetznerImage) { image.Labels["lease"] = "cbx_other123456" }, wantError: "mismatched lease label"},
		"architecture mismatch": {mutate: func(image *core.HetznerImage) { image.Architecture = "arm" }, wantError: "mismatched architecture"},
	} {
		t.Run(name, func(t *testing.T) {
			image := ownedCheckpointImage()
			tc.mutate(&image)
			client := &fakeHetznerSnapshotClient{getImages: []core.HetznerImage{image}}
			installHetznerCheckpointHooks(t, client)
			_, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), checkpointResourceRequest())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("verify err=%v, want %q", err, tc.wantError)
			}
			client.getImages = []core.HetznerImage{image}
			err = (Provider{}).DeleteNativeCheckpoint(context.Background(), checkpointResourceRequest())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("delete err=%v, want %q", err, tc.wantError)
			}
			if len(client.deletedImages) != 0 {
				t.Fatalf("deleted=%v, want no DELETE", client.deletedImages)
			}
		})
	}
}

func TestHetznerCheckpointOwnedDeletionAndNotFoundFailClosed(t *testing.T) {
	client := &fakeHetznerSnapshotClient{getImages: []core.HetznerImage{ownedCheckpointImage()}}
	installHetznerCheckpointHooks(t, client)
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), checkpointResourceRequest()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.deletedImages, []int64{99}) {
		t.Fatalf("deleted=%v", client.deletedImages)
	}

	client.getImageErr = core.HetznerHTTPError{Method: http.MethodGet, Path: "/images/99", StatusCode: http.StatusNotFound, Detail: "not found"}
	client.deletedImages = nil
	_, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), checkpointResourceRequest())
	if err == nil || !strings.Contains(err.Error(), "project identity cannot be proven") {
		t.Fatalf("verify err=%v", err)
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), checkpointResourceRequest()); err == nil || !strings.Contains(err.Error(), "project identity cannot be proven") {
		t.Fatalf("delete err=%v", err)
	}
	if len(client.deletedImages) != 0 {
		t.Fatalf("deleted=%v after 404", client.deletedImages)
	}
}

func TestHetznerCheckpointForkConfigPreservesImageLocationArchitectureAndOverrides(t *testing.T) {
	record := core.NativeCheckpointForkRecord{
		Kind:         core.CheckpointKindHetzner,
		ImageID:      "99",
		Region:       "fsn1",
		Direct:       true,
		Architecture: "x86",
		Metadata:     checkpointMetadata(),
	}
	cfg := core.Config{Coordinator: "https://coordinator.example", ServerType: "cpx31"}
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &cfg, Record: record}); err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "99" || cfg.Location != "fsn1" || cfg.Architecture != core.ArchitectureAMD64 || !core.IsArchitectureExplicit(cfg) || cfg.ServerType != "cpx31" {
		t.Fatalf("cfg=%+v", cfg)
	}

	armCfg := core.Config{Architecture: core.ArchitectureARM64}
	core.MarkArchitectureExplicit(&armCfg)
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &armCfg, Record: record}); err == nil || !strings.Contains(err.Error(), "cannot be forked") {
		t.Fatalf("err=%v, want architecture refusal", err)
	}
	badSource := record
	badSource.Metadata = cloneMetadata(record.Metadata)
	badSource.Metadata[checkpointMetadataSourceType] = "backup"
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &core.Config{}, Record: badSource}); err == nil {
		t.Fatal("expected source type refusal")
	}
}

func TestCreateHetznerCheckpointRejectsImageStrategyClearly(t *testing.T) {
	_, err := (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{Config: core.Config{TargetOS: core.TargetLinux}, Strategy: core.CheckpointStrategyImage})
	if err == nil || !strings.Contains(err.Error(), "--strategy image is unsupported") {
		t.Fatalf("err=%v", err)
	}
}

func checkpointHetznerServer() Server {
	server := crabboxHetznerServer(42, "cbx_abcdef123456")
	server.Location = &core.ServerLocationInfo{Name: "fsn1"}
	server.Image = &core.ServerImageInfo{Architecture: "x86"}
	return server
}

func checkpointCreateRequest(wait bool, timeout time.Duration) core.NativeCheckpointCreateRequest {
	return core.NativeCheckpointCreateRequest{
		Config:       core.Config{Provider: providerName, TargetOS: core.TargetLinux},
		Server:       checkpointHetznerServer(),
		Target:       core.SSHTarget{TargetOS: core.TargetLinux},
		CheckpointID: "chk_123abc",
		LeaseID:      "cbx_abcdef123456",
		Name:         "named",
		RepoName:     "repo",
		Strategy:     core.CheckpointStrategyDiskSnapshot,
		Wait:         wait,
		WaitTimeout:  timeout,
		Stderr:       io.Discard,
	}
}

func checkpointMetadata() map[string]string {
	return map[string]string{
		checkpointMetadataCheckpoint:         "chk_123abc",
		checkpointMetadataLease:              "cbx_abcdef123456",
		checkpointMetadataSourceServer:       "42",
		checkpointMetadataSourceLocation:     "fsn1",
		checkpointMetadataSourceArchitecture: "x86",
		checkpointMetadataSourceType:         "server",
	}
}

func ownedCheckpointImage() core.HetznerImage {
	image := core.HetznerImage{ID: 99, Type: "snapshot", Status: "available", Architecture: "x86", Labels: checkpointLabels(checkpointMetadata())}
	image.CreatedFrom = &struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}{ID: 42, Name: "crabbox-test"}
	return image
}

func checkpointResourceRequest() core.NativeCheckpointResourceRequest {
	return core.NativeCheckpointResourceRequest{
		Image: core.NativeCheckpointImage{
			ID:           "99",
			Provider:     providerName,
			Kind:         core.CheckpointKindHetzner,
			Region:       "fsn1",
			Architecture: "x86",
			Direct:       true,
		},
		Metadata: checkpointMetadata(),
	}
}
