package localcontainer

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
