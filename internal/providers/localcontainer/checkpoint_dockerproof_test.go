//go:build dockerproof

package localcontainer

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDockerCommitCheckpointLifecycleProof(t *testing.T) {
	const runtime = "docker"
	if _, err := exec.LookPath(runtime); err != nil {
		t.Skipf("%s not on PATH: %v", runtime, err)
	}

	bootstrapDir := t.TempDir()
	if err := os.WriteFile(bootstrapDir+"/bootstrap.sh", []byte("#!/bin/sh\nmkdir -p /work\nexec sleep 600\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(runtime, "run", "-d", "-v", bootstrapDir+":/tmp/crabbox-bootstrap:ro", "alpine:3", "/bin/sh", "/tmp/crabbox-bootstrap/bootstrap.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	containerID := lastCheckpointLine(string(out))
	t.Cleanup(func() { _ = exec.Command(runtime, "rm", "-f", containerID).Run() })

	req := core.NativeCheckpointCreateRequest{
		Server:  core.Server{CloudID: containerID, Labels: map[string]string{"runtime": runtime}},
		Name:    "proof",
		Workdir: "/work",
	}
	result, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	img := result.Image
	if img.State != "available" || img.Kind != core.CheckpointKindDockerCommit || !img.Direct {
		t.Fatalf("unexpected checkpoint image: %+v", img)
	}
	t.Cleanup(func() { _ = exec.Command(runtime, "rmi", "-f", img.ID).Run() })
	t.Logf("CREATE: image=%s id=%s", img.Name, shortCheckpointID(img.ID))

	labelsOut, err := exec.Command(runtime, "image", "inspect", img.Name, "--format", `{{index .Config.Labels "crabbox"}}|{{index .Config.Labels "provider"}}|{{index .Config.Labels "lease"}}`).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect checkpoint labels: %v: %s", err, labelsOut)
	}
	if got := strings.TrimSpace(string(labelsOut)); got != "||" {
		t.Fatalf("checkpoint retained lease labels: %q", got)
	}

	derivedOut, err := exec.Command(runtime, "run", "-d", img.Name).CombinedOutput()
	if err != nil {
		t.Fatalf("run derived container: %v: %s", err, derivedOut)
	}
	derivedID := lastCheckpointLine(string(derivedOut))
	t.Cleanup(func() { _ = exec.Command(runtime, "rm", "-f", derivedID).Run() })
	runningOut, err := exec.Command(runtime, "inspect", derivedID, "--format", "{{.State.Running}}").CombinedOutput()
	if err != nil || strings.TrimSpace(string(runningOut)) != "true" {
		t.Fatalf("checkpoint image did not start with its default command: %v: %s", err, runningOut)
	}
	inventoryOut, err := exec.Command(runtime, "ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}").CombinedOutput()
	if err != nil {
		t.Fatalf("inventory derived container: %v: %s", err, inventoryOut)
	}
	if strings.Contains(string(inventoryOut), shortCheckpointID(derivedID)) {
		t.Fatalf("derived container %s inherited Crabbox lease ownership", shortCheckpointID(derivedID))
	}

	result2, err := (Provider{}).CreateNativeCheckpoint(context.Background(), req)
	if err != nil {
		t.Fatalf("create #2: %v", err)
	}
	img2 := result2.Image
	t.Cleanup(func() { _ = exec.Command(runtime, "rmi", "-f", img2.Name).Run() })
	if img2.ID == img.ID || img2.Name == img.Name {
		t.Fatalf("duplicate name reused identity: #1=%s/%s #2=%s/%s", img.Name, shortCheckpointID(img.ID), img2.Name, shortCheckpointID(img2.ID))
	}

	resource := core.NativeCheckpointResourceRequest{Image: img, Metadata: result.Metadata}
	verify, err := (Provider{}).VerifyNativeCheckpoint(context.Background(), resource)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verify.ProviderState != "available" || verify.NextAction != "delete" {
		t.Fatalf("verify=%+v", verify)
	}
	if err := (Provider{}).DeleteNativeCheckpoint(context.Background(), resource); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := exec.Command(runtime, "image", "inspect", img.Name).Run(); err == nil {
		t.Fatalf("tag %s still present after delete", img.Name)
	}
	if err := exec.Command(runtime, "inspect", derivedID).Run(); err != nil {
		t.Fatalf("dependent container removed with checkpoint tag: %v", err)
	}

	mountedWorkdir := t.TempDir()
	if err := os.WriteFile(mountedWorkdir+"/proof.txt", []byte("mounted"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountOut, err := exec.Command(runtime, "run", "-d", "-v", mountedWorkdir+":/work/repo/node_modules", "alpine:3", "sleep", "600").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run mounted workdir: %v: %s", err, mountOut)
	}
	mountedContainerID := lastCheckpointLine(string(mountOut))
	t.Cleanup(func() { _ = exec.Command(runtime, "rm", "-f", mountedContainerID).Run() })
	_, err = (Provider{}).CreateNativeCheckpoint(context.Background(), core.NativeCheckpointCreateRequest{
		Server:  core.Server{CloudID: mountedContainerID, Labels: map[string]string{"runtime": runtime}},
		Name:    "mounted-proof",
		Workdir: "/work/repo",
	})
	if err == nil || !strings.Contains(err.Error(), "mounted volume /work/repo/node_modules") {
		t.Fatalf("mounted workdir error=%v", err)
	}
}

func TestCheckpointScopePinsAndValidatesContextProof(t *testing.T) {
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("requires Docker")
	}
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	scope, err := checkpointScopeForServer(context.Background(), core.Config{}, core.Server{
		Labels: map[string]string{"runtime": runtime},
	})
	if err != nil {
		t.Skipf("requires a running Docker daemon: %v", err)
	}
	if scope.Context == "" || scope.Config == "" || scope.Endpoint == "" || scope.DaemonID == "" {
		t.Fatalf("incomplete scope: %#v", scope)
	}
	if err := validateCheckpointScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "ambient-context-must-not-win")
	recordedScope, err := checkpointScopeForServer(context.Background(), core.Config{}, core.Server{
		Labels: map[string]string{
			"runtime":         runtime,
			"runtime_context": scope.Context,
		},
	})
	if err != nil {
		t.Fatalf("recorded lease context was not preferred: %v", err)
	}
	if recordedScope.Context != scope.Context || recordedScope.DaemonID != scope.DaemonID {
		t.Fatalf("recorded scope=%#v, want context=%q daemon=%q", recordedScope, scope.Context, scope.DaemonID)
	}
	scope.Endpoint += ".changed"
	if err := validateCheckpointScope(context.Background(), scope); err == nil {
		t.Fatal("expected changed endpoint to fail validation")
	}
	scope.Endpoint = strings.TrimSuffix(scope.Endpoint, ".changed")
	scope.DaemonID += "-changed"
	if err := validateCheckpointScope(context.Background(), scope); err == nil {
		t.Fatal("expected changed daemon identity to fail validation")
	}
}

func shortCheckpointID(value string) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func lastCheckpointLine(value string) string {
	parts := strings.Split(strings.TrimSpace(value), "\n")
	return strings.TrimSpace(parts[len(parts)-1])
}
