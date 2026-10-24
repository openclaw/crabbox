//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const captureContainerID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
const captureContainerImage = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type captureContainerState struct {
	Container map[string]any
	Removed   bool
	Image     string
	Tag       string
	Commits   int
	Removes   int
	Calls     [][]string
	Failure   string
}

func TestLocalContainerCheckpointFailureCleanup(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	docker := buildCheckpointContainerRuntime(t, repo)
	for _, tc := range []struct {
		failure   string
		submitted bool
	}{
		{failure: "missing-workspace"},
		{failure: "mount-inspection"},
		{failure: "mounted-workspace"},
		{failure: "commit-error", submitted: true},
		{failure: "commit-unknown", submitted: true},
		{},
	} {
		t.Run(blank(tc.failure, "success"), func(t *testing.T) {
			f := newCheckpointContainerFixture(t, repo, binary, docker)
			statePath := filepath.Join(f.root, "docker.json")
			var state captureContainerState
			f.readJSON(statePath, &state)
			state.Failure = tc.failure
			f.writeJSON(statePath, state)
			result := f.run("checkpoint", "create", "--provider", "local-container", "--id", captureFixtureLease, "--mode", "native", "--json")
			if (result.err != nil) != (tc.failure != "") {
				t.Fatalf("capture: %v\n%s", result.err, result.stderr)
			}
			checkpoints := filepath.Join(f.root, "state", "crabbox", "checkpoints")
			records, err := filepath.Glob(filepath.Join(checkpoints, "*", checkpointMetaFile))
			if err != nil {
				t.Fatal(err)
			}
			preSubmissionFailure := tc.failure != "" && !tc.submitted
			if preSubmissionFailure {
				entries, err := os.ReadDir(checkpoints)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock") {
						t.Errorf("unsubmitted capture left orphaned state: %s", entry.Name())
					}
				}
				var outcome map[string]string
				if err := json.Unmarshal(result.stdout, &outcome); err != nil || outcome["outcome"] != "not_submitted" || outcome["localReservation"] != "removed" {
					t.Errorf("missing non-submission outcome: %s err=%v", result.stdout, err)
				}
			} else if len(records) != 1 {
				t.Fatalf("capture reservation lost: %v", records)
			}
			var recordBefore []byte
			if tc.submitted {
				recordBefore, err = os.ReadFile(records[0])
				if err != nil {
					t.Fatal(err)
				}
			}
			result = f.run("stop", "--provider", "local-container", captureFixtureLease)
			f.readJSON(statePath, &state)
			wantCommits := 1
			if preSubmissionFailure {
				wantCommits = 0
			}
			if state.Commits != wantCommits {
				t.Fatalf("commits=%d want=%d", state.Commits, wantCommits)
			}
			if tc.submitted {
				after, err := os.ReadFile(records[0])
				if result.err == nil || !bytes.Contains(result.stderr, []byte("unresolved checkpoint")) || state.Removed || state.Removes != 0 || err != nil || !bytes.Equal(recordBefore, after) {
					t.Fatalf("ambiguous commit lost cleanup hold: err=%v stderr=%s state=%+v journalErr=%v", result.err, result.stderr, state, err)
				}
				return
			}
			if result.err != nil || !state.Removed || state.Removes != 1 || f.claim().CheckpointCapture != nil {
				t.Fatalf("cleanup failed: err=%v stderr=%s state=%+v claim=%+v", result.err, result.stderr, state, f.claim())
			}
			if tc.failure == "" {
				var record checkpointRecord
				f.readJSON(records[0], &record)
				if !record.CaptureEstablished || record.Native.ImageID != captureContainerImage || record.Native.Name != state.Tag {
					t.Fatalf("successful capture changed: %+v", record)
				}
			}
		})
	}
}

func buildCheckpointContainerRuntime(t *testing.T, repo string) string {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	build := exec.Command("go", "build", "-trimpath", "-o", docker, "./internal/cli/testdata/checkpoint-container")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake Docker: %v\n%s", err, output)
	}
	return docker
}

// Use the real CLI and Docker command boundary: provider capability unit tests
// cannot catch core turning implicit Docker commit into an explicit VM strategy.
func runCheckpointContainerReviewContract(t *testing.T, repo, binary string) {
	t.Helper()
	docker := buildCheckpointContainerRuntime(t, repo)
	t.Parallel()
	for _, strategy := range []string{"", "auto", "image", "disk-snapshot"} {
		t.Run("local container retirement strategy "+blank(strategy, "default"), func(t *testing.T) {
			f := newCheckpointContainerFixture(t, repo, binary, docker)
			args := []string{"checkpoint", "create", "--provider", "local-container", "--id", captureFixtureLease, "--checkpoint-id", captureFixtureCheckpoint, "--retire-source", "--mode", "native", "--wait=false", "--json"}
			if strategy != "" {
				args = append(args, "--strategy", strategy)
			}
			claimPath := filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			result := f.run(args...)
			var state captureContainerState
			f.readJSON(filepath.Join(f.root, "docker.json"), &state)
			if strategy == "image" || strategy == "disk-snapshot" {
				after, readErr := os.ReadFile(claimPath)
				_, journalErr := os.Stat(filepath.Join(f.root, "state", "crabbox", "checkpoints", captureFixtureCheckpoint, checkpointMetaFile))
				if result.err == nil || readErr != nil || !bytes.Equal(before, after) || !os.IsNotExist(journalErr) || state.Commits != 0 || state.Removes != 0 {
					t.Fatalf("explicit VM strategy admitted or changed source: result=%v stderr=%s state=%+v journal=%v claim=%v", result.err, result.stderr, state, journalErr, readErr)
				}
				return
			}
			if result.err != nil {
				t.Fatalf("implicit Docker commit retirement rejected: %v\n%s\n%+v", result.err, result.stderr, state)
			}
			// Creation submits once and removes the exact source; a second bounded
			// invocation observes absence and finalizes, then replay stays terminal.
			for attempt := 0; attempt < 2; attempt++ {
				result = f.run(args...)
				if result.err != nil {
					t.Fatalf("same-operation replay failed: %v\n%s", result.err, result.stderr)
				}
			}
			var record checkpointRecord
			if err := json.Unmarshal(result.stdout, &record); err != nil {
				t.Fatalf("retirement JSON: %v\n%s", err, result.stdout)
			}
			f.readJSON(filepath.Join(f.root, "docker.json"), &state)
			if record.Kind != CheckpointKindDockerCommit || record.Capture == nil || record.Capture.Phase != "retired" || record.Capture.SourceID != captureContainerID || record.Native.ImageID != captureContainerImage || state.Commits != 1 || state.Removes != 1 || !state.Removed {
				t.Fatalf("implicit strategy did not commit once and prove retirement: record=%+v state=%+v", record, state)
			}
			claim := f.claim()
			if claim.CloudID != "" || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
				t.Fatalf("retirement did not finalize the fixed source claim: %+v", claim)
			}
		})
	}
}

func newCheckpointContainerFixture(t *testing.T, repo, binary, docker string) *checkpointCaptureFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &checkpointCaptureFixture{t: t, root: root, repo: repo, binary: binary}
	for _, dir := range []string{"home", "config", "cache", "tmp", "bin", "docker-config", "state/crabbox/claims"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	host := "unix://" + filepath.Join(root, "unused-docker.sock")
	scope := "runtime:" + docker + "/context:default/host:" + host
	config := "provider: local-container\nnetwork: public\nlocalContainer:\n  runtime: " + docker + "\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env = []string{
		"HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"TMPDIR=" + filepath.Join(root, "tmp"), "CRABBOX_CONFIG=" + filepath.Join(root, "config.yaml"),
		"PATH=" + filepath.Join(root, "bin") + ":/usr/bin:/bin", "CRABBOX_CONTAINER_FIXTURE=" + root,
		"DOCKER_HOST=" + host, "DOCKER_CONFIG=" + filepath.Join(root, "docker-config"),
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1",
	}
	labels := map[string]string{
		"crabbox": "true", "provider": "local-container", "lease": captureFixtureLease, "slug": "container-proof", "state": "ready",
		"runtime": docker, "docker_context": "default", "docker_host": host, "docker_endpoint": host, "docker_daemon_id": "fixture-daemon", "docker_config": filepath.Join(root, "docker-config"),
		"docker_socket": "false", "ssh_user": "runner", "work_root": "/workspace/crabbox",
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	claim := leaseClaim{
		LeaseID: captureFixtureLease, Revision: "container-generation-1", Slug: "container-proof", Provider: FixedLocalContainerClaimProvider,
		CloudID: captureContainerID, ProviderScope: scope, RepoRoot: repo, ClaimedAt: now, LastUsedAt: now, TargetOS: "linux", Labels: labels,
		FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: strings.Repeat("a", 64), ProviderScope: scope, Slug: "container-proof", CreatedAt: now, State: "acquired"},
	}
	f.writeJSON(filepath.Join(root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
	f.writeJSON(filepath.Join(root, "docker.json"), captureContainerState{Container: map[string]any{
		"Id": captureContainerID, "Name": "/container-proof", "Created": now,
		"Mounts":          []any{},
		"Config":          map[string]any{"Image": "ubuntu:24.04", "Labels": labels},
		"State":           map[string]any{"Status": "running", "Running": true},
		"NetworkSettings": map[string]any{"Ports": map[string]any{"2222/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49160"}}}},
	}})
	return f
}
