package aws

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestAWSStopGuestUsesStoredLeaseKey(t *testing.T) {
	testutil.IsolateUserDirs(t)
	dir := t.TempDir()
	t.Chdir(dir)
	for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_TOKEN", "CRABBOX_PROVIDER", "CRABBOX_SSH_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_NETWORK", "public")
	const id = "cbx_abcdef123456"
	cfg := core.BaseConfig()
	cfg.Provider = "aws"
	cfg.AWSRegion = "us-east-1"
	key, _, err := core.EnsureTestboxKeyForConfig(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	host, port, commands := testutil.RecordingSSHGuest(t, key+".pub")
	t.Setenv("CRABBOX_SSH_PORT", port)
	t.Setenv("CRABBOX_SSH_FALLBACK_PORTS", "none")
	t.Setenv("CRABBOX_SSH_USER", "guest")
	t.Setenv("CRABBOX_AWS_REGION", "us-east-1")
	cfg.SSHPort = port
	cfg.SSHUser = "guest"
	cfg.SSHFallbackPorts = nil
	server := awsTestServer("i-stop", id, "stop-guest", "us-east-1")
	server.PublicNet.IPv4.IP = host
	server.Labels["tailscale"] = "true"
	fake := &fakeAWSClient{servers: []core.Server{server}}
	old := newAWSClient
	newAWSClient = func(context.Context, core.Config) (awsClient, error) { return fake, nil }
	t.Cleanup(func() { newAWSClient = old })
	if err := core.ClaimLeaseTargetForRepoConfig(id, "stop-guest", cfg, server, core.SSHTarget{}, dir, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	// The configured identity is different, so authentication itself proves rebinding.
	other, _, err := core.EnsureTestboxKeyForConfig(cfg, "cbx_123456abcdef")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_SSH_KEY", other)
	var stderr bytes.Buffer
	err = (core.App{Stdout: io.Discard, Stderr: &stderr}).Run(t.Context(), []string{"stop", "--provider", "aws", "--id", id})
	if err != nil || len(fake.deletedInstances) != 1 {
		t.Fatalf("stop=%v deletes=%v stderr=%s", err, fake.deletedInstances, &stderr)
	}
	got := strings.Join(commands(), "\n")
	for _, want := range []string{".crabbox/actions/", "pkill -f", "tailscale logout"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing authenticated cleanup %q commands=%s stderr=%s", want, got, &stderr)
		}
	}
	if strings.Contains(stderr.String(), "Permission denied") {
		t.Fatalf("wrong SSH identity: %s", &stderr)
	}
}
