package localcontainer

import (
	"context"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type servedRecordingRunner struct{ args []string }

func (r *servedRecordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.args = append([]string(nil), req.Args...)
	return core.LocalCommandResult{Stdout: "container-id\n"}, nil
}
func TestServedCreationUsesSharedBootstrapWithoutHostMounts(t *testing.T) {
	runner := &servedRecordingRunner{}
	cfg := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "example:stable", CPUs: 3, Memory: "6g", Network: "host", DockerSocket: true, Volumes: []string{"/:/host"}}, TTL: time.Hour, IdleTimeout: time.Minute}
	r := NewServedRuntime(cfg, core.Runtime{Exec: runner})
	if r.b.cfg.LocalContainer.Network != "bridge" || r.b.cfg.LocalContainer.DockerSocket || len(r.b.cfg.LocalContainer.Volumes) != 0 {
		t.Fatal("served policy inherited unsafe local-container options")
	}
	_, _, err := r.b.createContainerWithOptions(context.Background(), r.b.cfg, "name", "cbx_123456abcdef", "", "ssh-ed25519 AAAA", "", false, &ServedContainerOptions{Deadline: 123, Labels: map[string]string{"provider": "serve", "crabbox.serve": "true", "crabbox.serve.machine": "machine"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.args, "\n")
	for _, expected := range []string{"CRABBOX_MANAGED_HOME=1", "--rm", "--cpu-shares\n2", "--cpus\n3", "--memory\n6g", "127.0.0.1::2222", "provider=serve", "crabbox.serve.machine=machine", bootstrapScript, "umask 022", "CRABBOX_SERVE_DEADLINE=123"} {
		if !strings.Contains(args, expected) {
			t.Errorf("missing %q", expected)
		}
	}
	for _, arg := range runner.args {
		if arg == "-v" || arg == "--privileged" || arg == "host" {
			t.Errorf("unsafe argument %q", arg)
		}
	}
}

func TestServedRuntimeRejectsForeignImageBeforeExecution(t *testing.T) {
	runner := &servedRecordingRunner{}
	r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "configured:stable"}}, core.Runtime{Exec: runner})
	r.preparedImageID = "sha256:prepared"
	_, err := r.Create(context.Background(), "cbx_123456abcdef", "ssh-ed25519 AAAA", 1, 1024, "foreign:latest", nil, ServedContainerOptions{})
	if err == nil || !strings.Contains(err.Error(), "serve_image_mismatch") {
		t.Fatal(err)
	}
	if len(runner.args) != 0 {
		t.Fatal("foreign image reached runtime", runner.args)
	}
}

// Served leases regenerate host keys per lease; ordinary local-container leases must keep the shared bootstrap unchanged.
func TestServedHostKeyPolicyStaysOutOfOrdinaryBootstrap(t *testing.T) {
	for _, served := range []string{"ssh-keygen -A", "rm -f /etc/ssh/ssh_host_*"} {
		if strings.Contains(bootstrapScript, served) {
			t.Fatalf("served host-key policy %q leaked into ordinary local-container bootstrap", served)
		}
	}
}
