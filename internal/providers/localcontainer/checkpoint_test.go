package localcontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLocalContainerCheckpointCLIConfigDependency(t *testing.T) {
	for _, tc := range []struct {
		name           string
		missingRuntime bool
		validConfig    bool
		wantError      string
	}{
		{name: "recorded_runtime"},
		{name: "missing_runtime_invalid_config", missingRuntime: true, wantError: "parse config"},
		{name: "missing_runtime_configured_fallback", missingRuntime: true, validConfig: true},
		{name: "changed_daemon", wantError: "Docker daemon changed"},
		{name: "changed_endpoint", wantError: "endpoint changed"},
		{name: "changed_image", wantError: "tag now points to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			t.Setenv("XDG_STATE_HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("DOCKER_CONTEXT", "ambient-invalid")
			t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")
			configPath := filepath.Join(root, "config.yaml")
			t.Setenv("CRABBOX_CONFIG", configPath)
			runtimePath, callsPath := filepath.Join(root, "docker"), filepath.Join(root, "calls")
			write := func(path, value string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), mode); err != nil {
					t.Fatal(err)
				}
			}
			config := "machine0: [\n"
			if tc.validConfig {
				config = fmt.Sprintf("provider: local-container\nlocalContainer:\n  runtime: %q\n", runtimePath)
			}
			write(configPath, config, 0o600)
			write(callsPath, "", 0o600)
			metadata := checkpointScopeMetadata(checkpointScope{Runtime: runtimePath, Context: "recorded", Config: filepath.Join(root, "docker-config"), Endpoint: "unix:///recorded.sock", DaemonID: "daemon-owned"})
			if tc.missingRuntime {
				delete(metadata, checkpointMetadataRuntime)
			}
			imageID := "sha256:" + strings.Repeat("a", 64)
			endpoint, daemon, digest := metadata[checkpointMetadataEndpoint], metadata[checkpointMetadataDaemonID], imageID
			switch tc.name {
			case "changed_daemon":
				daemon = "daemon-other"
			case "changed_endpoint":
				endpoint = "unix:///other.sock"
			case "changed_image":
				digest = "sha256:" + strings.Repeat("b", 64)
			}
			// The fixture rejects ambient Docker routing before any simulated operation.
			write(runtimePath, fmt.Sprintf(`#!/bin/sh
[ "$DOCKER_CONFIG" = %q ] && [ -z "$DOCKER_HOST" ] && [ -z "$DOCKER_CONTEXT" ] || exit 90
[ "$1 $2" = '--context recorded' ] || exit 91
shift 2
printf '%%s\n' "$1" >> %q
case "$*" in
  'context inspect recorded --format '*) printf '%%s\n' %q ;;
  'info --format {{.ID}}') printf '%%s\n' %q ;;
  'image inspect crabbox-checkpoint-fixture --format {{.Id}}') printf '%%s\n' %q ;;
  'rmi -f crabbox-checkpoint-fixture') exit 0 ;;
  *) exit 92 ;;
esac
`, metadata[checkpointMetadataConfig], callsPath, endpoint, daemon, digest), 0o700)
			metaPath := filepath.Join(root, "crabbox", "checkpoints", "chk_config", "checkpoint.json")
			if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(map[string]any{
				"id": "chk_config", "kind": core.CheckpointKindDockerCommit, "provider": providerName,
				"createdAt": time.Now().UTC().Format(time.RFC3339),
				"native":    map[string]any{"provider": providerName, "imageId": imageID, "name": "crabbox-checkpoint-fixture", "direct": true, "metadata": metadata},
			})
			if err != nil {
				t.Fatal(err)
			}
			write(metaPath, string(before), 0o600)
			var stdout bytes.Buffer
			app := core.App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.Run(t.Context(), []string{"checkpoint", "inspect", "chk_config", "--verify", "--json"}); err != nil {
				t.Fatal(err)
			}
			wantState := "available"
			if tc.wantError != "" {
				wantState = "unknown"
				if tc.name == "changed_image" {
					wantState = "conflict"
				}
			}
			if !strings.Contains(stdout.String(), `"providerState":"`+wantState+`"`) {
				t.Errorf("verify: %s, want %s", &stdout, wantState)
			}
			err = app.Run(t.Context(), []string{"checkpoint", "delete", "chk_config"})
			if tc.wantError == "" {
				if err != nil {
					t.Errorf("cleanup: %v", err)
				}
				if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
					t.Errorf("local record retained: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("cleanup: %v, want %q", err, tc.wantError)
				}
				after, readErr := os.ReadFile(metaPath)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Errorf("failed cleanup changed record: %v", readErr)
				}
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(calls), "rmi\n"); (tc.wantError == "" && got != 1) || (tc.wantError != "" && got != 0) {
				t.Errorf("unsafe or missing image deletion: %s", calls)
			}
			if tc.missingRuntime && !tc.validConfig && len(calls) != 0 {
				t.Errorf("runtime used despite invalid config: %s", calls)
			}
		})
	}
}

func TestNativeCheckpointWorkdirUsesResolvedLeaseRoot(t *testing.T) {
	cfg := core.Config{Provider: providerName, WorkRoot: "/stale"}
	got := (Provider{}).NativeCheckpointWorkdir(core.NativeCheckpointWorkdirRequest{
		Config:   cfg,
		Server:   core.Server{Labels: map[string]string{"work_root": "/resolved"}},
		LeaseID:  "cbx_123",
		RepoName: "my-app",
	})
	if got != "/resolved/cbx_123/my-app" {
		t.Fatalf("workdir=%q", got)
	}
}

func TestPodmanScopeCapturesConnectionAndRuntimeIdentity(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "podman")
	script := `#!/bin/sh
case "$*" in
  "system connection list --format json")
    printf '[{"Name":"team","URI":"%s","Default":true}]\n' "${PODMAN_TEST_URI:-ssh://runner@example.invalid/run/podman.sock}"
    ;;
  "--connection team info --format "*)
    printf '%s\n' 'host-a|/var/lib/containers|/run/containers|/run/podman.sock|true'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_CONNECTION", "team")
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "tcp://unrelated-docker.invalid:2376")
	cfg := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: runtimePath}}
	scope, err := podmanScopeForConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Runtime != runtimePath || scope.Context != "team" || scope.Endpoint != "ssh://runner@example.invalid/run/podman.sock" || !strings.HasPrefix(scope.DaemonID, "podman-") {
		t.Fatalf("scope=%#v", scope)
	}
	if err := validateCheckpointScope(context.Background(), scope); err != nil {
		t.Fatalf("validate matching scope: %v", err)
	}
	t.Setenv("PODMAN_TEST_URI", "ssh://runner@other.invalid/run/podman.sock")
	if err := validateCheckpointScope(context.Background(), scope); err == nil {
		t.Fatal("repointed Podman connection validated")
	}
}

func TestCheckpointImageNameNormalizesAndBoundsRepository(t *testing.T) {
	got := checkpointImageName("___", "sha256:ABCDEF0123456789")
	if got != "crabbox-checkpoint-checkpoint-abcdef012345" {
		t.Fatalf("punctuation-only name=%q", got)
	}
	got = checkpointImageName(strings.Repeat("A_", 300), "sha256:ABCDEF0123456789")
	if len(got) > 255 || got != strings.ToLower(got) {
		t.Fatalf("invalid repository name=%q length=%d", got, len(got))
	}
}

func TestCheckpointMountIntersectingWorkdir(t *testing.T) {
	mounts := []checkpointMount{{Destination: "/cache"}, {Destination: "/work/shared"}}
	for _, tc := range []struct {
		workdir string
		want    string
	}{
		{workdir: "/work/shared", want: "/work/shared"},
		{workdir: "/work/shared/repo", want: "/work/shared"},
		{workdir: "/work", want: "/work/shared"},
		{workdir: "/work/other", want: ""},
	} {
		if got := checkpointMountIntersectingWorkdir(mounts, tc.workdir); got != tc.want {
			t.Fatalf("workdir=%q got=%q want=%q", tc.workdir, got, tc.want)
		}
	}
}

func TestCheckpointRollbackContextOutlivesRequestCancellation(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	if requestCtx.Err() == nil {
		t.Fatal("request context was not canceled")
	}
	rollbackCtx, cancelRollback := checkpointRollbackContext()
	defer cancelRollback()
	if err := rollbackCtx.Err(); err != nil {
		t.Fatalf("rollback context inherited request cancellation: %v", err)
	}
}

func TestParseCheckpointImageIDIgnoresNonDigestOutput(t *testing.T) {
	got, err := parseCheckpointImageID("warning\nsha256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Fatalf("image id=%q", got)
	}
}

func TestCheckpointResetLeaseLabelsClearsOwnershipSelectors(t *testing.T) {
	change := checkpointResetLeaseLabelsChange()
	for _, key := range []string{"crabbox", "provider", "lease", "slug", "keep", "expires_at"} {
		if !strings.Contains(change, ` `+key+`=""`) {
			t.Fatalf("label reset missing %s", key)
		}
	}
}

func TestCheckpointBootableCommandDoesNotReferenceBootstrapMount(t *testing.T) {
	change := checkpointBootableCommandChange()
	if strings.Contains(change, "crabbox-bootstrap") || !strings.HasPrefix(change, "CMD ") {
		t.Fatalf("invalid checkpoint command change: %q", change)
	}
}

func TestNativeCheckpointCapabilityReturnsDockerCommit(t *testing.T) {
	cap, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
		Server: core.Server{CloudID: "abc123"},
	})
	if !ok {
		t.Fatal("expected capability to be supported")
	}
	if cap.Kind != core.CheckpointKindDockerCommit {
		t.Fatalf("Kind=%q, want %q", cap.Kind, core.CheckpointKindDockerCommit)
	}
	if !cap.Direct {
		t.Fatal("Direct=false, want true")
	}
}

func TestNativeCheckpointCapabilityRequiresCloudID(t *testing.T) {
	_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
		Server: core.Server{},
	})
	if ok {
		t.Fatal("expected capability to be unsupported without CloudID")
	}
}

func TestNativeCheckpointCapabilityRejectsExplicitStrategies(t *testing.T) {
	for _, strategy := range []string{"image", "ami", "disk-snapshot", "disk", "snapshot"} {
		t.Run(strategy, func(t *testing.T) {
			_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
				Server:           core.Server{CloudID: "abc123"},
				Strategy:         core.NormalizeCheckpointStrategy(strategy),
				StrategyExplicit: true,
			})
			if ok {
				t.Fatalf("expected capability to be unsupported with strategy=%s", strategy)
			}
		})
	}
}

func TestNativeCheckpointCapabilityAcceptsNormalizedDefaultStrategy(t *testing.T) {
	_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
		Server:   core.Server{CloudID: "abc123"},
		Strategy: core.CheckpointStrategyDiskSnapshot,
	})
	if !ok {
		t.Fatal("expected normalized default strategy to remain supported")
	}
}

func TestNativeCheckpointCapabilitySkipsDockerSocket(t *testing.T) {
	_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
		Server: core.Server{CloudID: "abc123"},
		Config: core.Config{LocalContainer: core.LocalContainerConfig{DockerSocket: true}},
	})
	if ok {
		t.Fatal("expected capability to be unsupported with docker-socket")
	}
}

func TestNativeCheckpointCapabilitySkipsDockerSocketLabel(t *testing.T) {
	_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
		Server: core.Server{CloudID: "abc123", Labels: map[string]string{"docker_socket": "1"}},
	})
	if ok {
		t.Fatal("expected capability to be unsupported when the lease label marks docker-socket mode")
	}
}

func TestNativeCheckpointCapabilityLeaseLabelOverridesDockerSocketConfig(t *testing.T) {
	_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
		Server: core.Server{
			CloudID: "abc123",
			Labels:  map[string]string{"docker_socket": "0"},
		},
		Config: core.Config{LocalContainer: core.LocalContainerConfig{DockerSocket: true}},
	})
	if !ok {
		t.Fatal("expected recorded docker_socket=0 to override current config")
	}
}

func TestNativeCheckpointCapabilityUsesResolvedLeaseRuntime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		label    string
		fallback string
		want     bool
	}{
		{name: "detected podman", label: "podman", fallback: "docker", want: false},
		{name: "detected docker", label: "/usr/local/bin/docker", fallback: "podman", want: true},
		{name: "configured nerdctl", fallback: "nerdctl", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := Provider{}.NativeCheckpointCapability(core.NativeCheckpointRequest{
				Server: core.Server{
					CloudID: "abc123",
					Labels:  map[string]string{"runtime": tc.label},
				},
				Config: core.Config{LocalContainer: core.LocalContainerConfig{Runtime: tc.fallback}},
			})
			if ok != tc.want {
				t.Fatalf("supported=%v, want %v", ok, tc.want)
			}
		})
	}
}

func TestSpecAdvertisesForkFeature(t *testing.T) {
	if !(Provider{}).Spec().Features.Has(core.FeatureFork) {
		t.Fatal("expected local-container to advertise checkpoint fork support")
	}
}

func TestCheckpointForkUsesTagForDisplay(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.LocalContainer.Image = "sha256:123"
	cfg.LocalContainer.CheckpointMetadata = map[string]string{
		checkpointMetadataForkName: "crabbox-checkpoint-demo-123",
	}
	applyDefaults(&cfg)
	if cfg.ServerType != "crabbox-checkpoint-demo-123" {
		t.Fatalf("server type=%q", cfg.ServerType)
	}
	if cfg.LocalContainer.Image != "sha256:123" {
		t.Fatalf("launch image=%q", cfg.LocalContainer.Image)
	}
}

func TestCheckpointScopeForServerUsesPersistedLabels(t *testing.T) {
	binDir := writeCheckpointScopeDocker(t, "unix:///tmp/docker.sock", "daemon-123")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONTEXT", "ambient-invalid")
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")
	labels := map[string]string{
		checkpointMetadataRuntime:  "docker",
		checkpointMetadataContext:  "remote-context",
		checkpointMetadataConfig:   "/tmp/docker-config",
		checkpointMetadataEndpoint: "unix:///tmp/docker.sock",
		checkpointMetadataDaemonID: "daemon-123",
	}
	scope, err := checkpointScopeForServer(context.Background(), core.Config{}, core.Server{Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if scope.Context != "remote-context" || scope.Config != "/tmp/docker-config" || scope.DaemonID != "daemon-123" {
		t.Fatalf("scope=%#v", scope)
	}
	labels[checkpointMetadataDaemonID] = "another-daemon"
	if _, err := checkpointScopeForServer(context.Background(), core.Config{}, core.Server{Labels: labels}); err == nil {
		t.Fatal("expected changed persisted daemon identity to fail")
	}
}

func TestCheckpointScopeForServerUsesExactClaimSnapshot(t *testing.T) {
	binDir := writeCheckpointScopeDocker(t, "unix:///claim/docker.sock", "daemon-123")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONTEXT", "ambient-invalid")
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")
	server := core.Server{Labels: map[string]string{
		checkpointMetadataRuntime:  "docker",
		checkpointMetadataContext:  "remote-context",
		checkpointMetadataDaemonID: "daemon-123",
	}}
	claimLabels := cloneLabels(server.Labels)
	claimLabels[checkpointMetadataConfig] = "/claim/docker-config"
	claimLabels[checkpointMetadataEndpoint] = "unix:///claim/docker.sock"
	core.SetServerLeaseClaimSnapshot(&server, core.LeaseClaim{Labels: claimLabels}, true)
	scope, err := checkpointScopeForServer(context.Background(), core.Config{}, server)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Context != "remote-context" || scope.Config != "/claim/docker-config" || scope.Endpoint != "unix:///claim/docker.sock" || scope.DaemonID != "daemon-123" {
		t.Fatalf("claim scope=%#v", scope)
	}
}

func TestCheckpointScopeForServerCompletesPrivateRoutingOmittedFromLabels(t *testing.T) {
	binDir := writeCheckpointScopeDocker(t, "unix:///tmp/docker.sock", "daemon-123")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONTEXT", "ambient-invalid")
	t.Setenv("DOCKER_HOST", "tcp://ambient.invalid:2376")
	labels := map[string]string{
		checkpointMetadataRuntime:  "docker",
		checkpointMetadataContext:  "remote-context",
		checkpointMetadataDaemonID: "daemon-123",
	}
	scope, err := checkpointScopeForServer(context.Background(), core.Config{}, core.Server{Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if scope.Context != "remote-context" || scope.Endpoint != "unix:///tmp/docker.sock" || scope.Config == "" || scope.DaemonID != "daemon-123" {
		t.Fatalf("completed scope=%#v", scope)
	}
}

func TestCheckpointMetadataForServerPreservesUserAndWorkRoot(t *testing.T) {
	metadata := checkpointMetadataForServer(
		checkpointScope{Runtime: "docker", DaemonID: "daemon-123"},
		core.Config{LocalContainer: core.LocalContainerConfig{User: "configured", WorkRoot: "/configured"}},
		core.Server{Labels: map[string]string{"ssh_user": "runner", "work_root": "/workspace"}},
	)
	if metadata[checkpointMetadataUser] != "runner" || metadata[checkpointMetadataWorkRoot] != "/workspace" {
		t.Fatalf("metadata=%#v", metadata)
	}
}

func writeCheckpointScopeDocker(t *testing.T, endpoint, daemonID string) string {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--context" ]; then
  shift 2
fi
case "$1" in
  context) printf '%%s\n' '%s' ;;
  info) printf '%%s\n' '%s' ;;
esac
`, endpoint, daemonID)
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func TestApplyNativeCheckpointForkConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.LocalContainer.DockerSocket = true
	metadata := map[string]string{
		checkpointMetadataRuntime:  "docker",
		checkpointMetadataContext:  "orbstack",
		checkpointMetadataConfig:   "/tmp/docker-config",
		checkpointMetadataEndpoint: "unix:///tmp/docker.sock",
		checkpointMetadataDaemonID: "daemon-123",
		checkpointMetadataUser:     "runner",
		checkpointMetadataWorkRoot: "/workspace",
	}
	err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind:     core.CheckpointKindDockerCommit,
			ImageID:  "sha256:123",
			Name:     "crabbox-checkpoint-demo-123",
			Metadata: metadata,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalContainer.Image != "sha256:123" {
		t.Fatalf("image=%q", cfg.LocalContainer.Image)
	}
	if cfg.LocalContainer.Runtime != "docker" {
		t.Fatalf("runtime=%q", cfg.LocalContainer.Runtime)
	}
	if cfg.LocalContainer.User != "runner" || cfg.SSHUser != "runner" {
		t.Fatalf("users local=%q ssh=%q", cfg.LocalContainer.User, cfg.SSHUser)
	}
	if cfg.LocalContainer.WorkRoot != "/workspace" || cfg.WorkRoot != "/workspace" {
		t.Fatalf("work roots local=%q generic=%q", cfg.LocalContainer.WorkRoot, cfg.WorkRoot)
	}
	if cfg.LocalContainer.DockerSocket {
		t.Fatal("fork must disable Docker socket passthrough")
	}
	if got := cfg.LocalContainer.CheckpointMetadata[checkpointMetadataForkID]; got != "sha256:123" {
		t.Fatalf("fork image id=%q", got)
	}
	if _, ok := metadata[checkpointMetadataForkID]; ok {
		t.Fatal("fork config mutated persisted checkpoint metadata")
	}
}

func TestApplyNativeCheckpointForkConfigRejectsInvalidRecord(t *testing.T) {
	for _, record := range []core.NativeCheckpointForkRecord{
		{Kind: "workspace-archive", ImageID: "sha256:123"},
		{Kind: core.CheckpointKindDockerCommit},
		{Kind: core.CheckpointKindDockerCommit, ImageID: "sha256:123"},
		{Kind: core.CheckpointKindDockerCommit, Name: "crabbox-checkpoint-demo-123"},
		{
			Kind:     core.CheckpointKindDockerCommit,
			ImageID:  "sha256:123",
			Name:     "crabbox-checkpoint-demo-123",
			Metadata: map[string]string{checkpointMetadataRuntime: "podman"},
		},
		{
			Kind:    core.CheckpointKindDockerCommit,
			ImageID: "sha256:123",
			Name:    "crabbox-checkpoint-demo-123",
			Metadata: map[string]string{
				checkpointMetadataRuntime: "docker",
			},
		},
	} {
		cfg := core.BaseConfig()
		if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &cfg, Record: record}); err == nil {
			t.Fatalf("expected record to fail: %#v", record)
		}
	}
}
