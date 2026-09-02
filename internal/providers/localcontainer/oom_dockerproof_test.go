//go:build dockerproof

package localcontainer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type oomProofRunner struct{}

func (oomProofRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = append(os.Environ(), req.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	return result, err
}

func TestDockerOOMKillEvidenceProof(t *testing.T) {
	runtimeName := strings.TrimSpace(os.Getenv("CRABBOX_LOCAL_CONTAINER_RUNTIME"))
	if runtimeName == "" {
		runtimeName = "docker"
	}
	if _, err := exec.LookPath(runtimeName); err != nil {
		t.Skipf("%s is not installed: %v", runtimeName, err)
	}
	if out, err := exec.Command(runtimeName, "info").CombinedOutput(); err != nil {
		t.Skipf("%s daemon is unavailable: %v: %s", runtimeName, err, out)
	}

	name := fmt.Sprintf("crabbox-oom-proof-%d", time.Now().UnixNano())
	image := "alpine:3"
	create := exec.Command(runtimeName, "run", "-d", "--name", name, "--memory", "64m", image, "sleep", "300")
	out, err := create.CombinedOutput()
	if err != nil {
		t.Skipf("cannot create bounded proof container: %v: %s", err, out)
	}
	containerID := name
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, runtimeName, "rm", "-f", containerID).Run()
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.Runtime = runtimeName
	core.MarkLocalContainerRuntimeExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: oomProofRunner{}, Stdout: os.Stdout, Stderr: os.Stderr}).(*backend)
	req := core.RunFailureEvidenceRequest{Lease: core.LeaseTarget{Server: core.Server{CloudID: containerID}}}

	ordinary, err := b.BeginRunFailureEvidence(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(runtimeName, "exec", containerID, "sh", "-c", "exit 7").Run(); err == nil {
		t.Fatal("ordinary failure control exited zero")
	}
	ordinaryEvidence, err := ordinary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ordinaryEvidence.ResourceExhaustion != "" {
		t.Fatalf("ordinary exit produced resource exhaustion: %#v", ordinaryEvidence)
	}

	oom, err := b.BeginRunFailureEvidence(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	oomCommand := "dd if=/dev/zero of=/dev/shm/crabbox-oom bs=1M count=256 2>/dev/null"
	if err := exec.Command(runtimeName, "exec", containerID, "sh", "-c", oomCommand).Run(); err == nil {
		t.Fatal("memory exhaustion command unexpectedly exited zero")
	}
	oomEvidence, err := oom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oomEvidence.ResourceExhaustion != core.ResourceExhaustionMemory {
		t.Fatalf("OOM evidence=%#v, want memory resource exhaustion", oomEvidence)
	}
}
