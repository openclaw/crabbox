package aws

import (
	"bytes"
	"encoding/json"
	"errors"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The legacy JSON has no fixed intent or pre-submit audit. Run the actual
// command owner, provider release and subsequent exact-absence reconciliation.
func TestAWSOrdinaryAbandonSameCommandReplay(t *testing.T) {
	for _, mode := range []string{"removed claim", "legacy empty scope", "interrupted cleanup", "changed generation", "wrong account"} {
		t.Run(mode, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			repo := t.TempDir()
			t.Chdir(repo)
			for _, key := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
				t.Setenv(key, "")
			}
			config := filepath.Join(repo, "config.yaml")
			if err := os.WriteFile(config, []byte("provider: aws\ntarget: linux\naws:\n  region: us-west-2\n  sshTransport: ssm\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", config)
			t.Setenv("CRABBOX_AWS_REGION", "us-west-2")
			const leaseID = "cbx_abcdef123456"
			const checkpointID = "chk_ordinary_abandon"
			server := awsTestServer("i-0123456789abcdef0", leaseID, "ordinary", "us-west-2")
			server.Labels["aws_account_id"] = "123456789012"
			server.Labels["aws_ssh_transport"] = "ssm"
			server.Labels["provider_key"] = core.ProviderKeyForLease(leaseID)
			server.Labels["aws_key_pair_id"] = "key-id-for-" + server.Labels["provider_key"]
			cfg := core.Config{Provider: "aws", AWSRegion: "us-west-2", AWSSSHTransport: "ssm"}
			if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "ordinary", cfg, server, core.SSHTarget{}, repo, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			if mode != "legacy empty scope" {
				if err := core.WithDurableLeaseClaimLock(leaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
					c.ProviderScope = "account:123456789012"
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			capture := core.NativeCheckpointCapture{SourceDisposition: "retire", Phase: "submitting", SourceID: server.CloudID, SourceName: server.Name, SourceScope: claim.ProviderScope, SourceRevision: claim.Revision, SourceClaimedAt: claim.ClaimedAt}
			if err := core.WithDurableLeaseClaimLock(leaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
				c.CheckpointCapture = &core.CheckpointCaptureBinding{ID: checkpointID, Revision: claim.Revision}
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			record := map[string]any{"createdAt": time.Now().UTC().Format(time.RFC3339), "lastUsedAt": time.Now().UTC().Format(time.RFC3339), "id": checkpointID, "kind": "aws-ami", "provider": "aws", "leaseId": leaseID, "capture": capture, "repo": map[string]string{"root": repo}, "native": map[string]any{"provider": "aws", "kind": "aws-ami", "direct": true, "strategy": "image"}}
			path := filepath.Join(dirs.StateHome, "crabbox", "checkpoints", checkpointID, "checkpoint.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(record)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			evidence := filepath.Join(repo, "evidence.txt")
			if err := os.WriteFile(evidence, []byte("prepare native checkpoint source: fixture failure\n"), 0600); err != nil {
				t.Fatal(err)
			}
			fake := &fakeAWSClient{servers: []core.Server{server}}
			t.Cleanup(installFixedAWSTestClient(t, fake))
			args := []string{"checkpoint", "abandon", checkpointID, "--provider", "aws", "--id", leaseID, "--expected-provider-resource-id", server.CloudID, "--pre-submit-error-file", evidence, "--json"}
			run := func() error {
				var out bytes.Buffer
				return (core.App{Stdout: &out, Stderr: &out}).Run(t.Context(), args)
			}
			if mode == "interrupted cleanup" || mode == "changed generation" || mode == "wrong account" {
				fake.terminateSSMErr = errors.New("interrupted cleanup")
			}
			first := run()
			if (mode == "removed claim" || mode == "legacy empty scope") && first != nil {
				t.Fatal(first)
			}
			if mode != "removed claim" && mode != "legacy empty scope" && (first == nil || !strings.Contains(first.Error(), "interrupted cleanup")) {
				t.Fatal("expected interrupted cleanup")
			}
			fake.terminateSSMErr = nil
			fake.deletedInstances = nil
			fake.deletedKeys = nil
			fake.terminatedSSM = nil
			if mode == "changed generation" {
				if err := core.WithDurableLeaseClaimLock(leaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
					c.ClaimedAt = "changed"
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong account" {
				fake.accountID = "999999999999"
			}
			if mode == "removed claim" || mode == "legacy empty scope" {
				fake.servers = nil
			}
			err = run()
			if mode == "changed generation" || mode == "wrong account" {
				if err == nil {
					t.Fatal("accepted changed authority")
				}
				if len(fake.deletedInstances)+len(fake.deletedKeys)+len(fake.terminatedSSM) != 0 {
					t.Fatal("unauthorized destructive provider calls")
				}
				return
			}
			if err != nil {
				t.Fatalf("same-command replay: %v", err)
			}
			if mode == "interrupted cleanup" {
				fake.servers = nil
				if err := run(); err != nil {
					t.Fatal(err)
				}
			}
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Capture core.NativeCheckpointCapture `json:"capture"`
				Native  struct {
					ImageID  string            `json:"imageId"`
					Metadata map[string]string `json:"metadata"`
				} `json:"native"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Capture.Phase != "abandoned" || got.Native.ImageID != "" || !strings.Contains(got.Capture.Error, "unresolved") || got.Native.Metadata["crabbox_pre_submit_failure_sha256"] == "" {
				t.Fatalf("lost unresolved obligation: %s", data)
			}
			if err := run(); err != nil {
				t.Fatalf("terminal same-command replay: %v", err)
			}
		})
	}
}
