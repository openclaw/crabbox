package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

// Resume the persisted checkpoint journal after the real AWS release owner has
// replaced its bound source claim with a scoped terminal receipt.
func TestCheckpointRetirementReplaysAWSRetainedReceipt(t *testing.T) {
	for _, tc := range []checkpointReceiptCase{
		{name: "exact released receipt"},
		{name: "changed intent", mutate: func(c *core.LeaseClaim) { c.FixedCreateIntent.Fingerprint = strings.Repeat("b", 64) }, wantError: "terminal claim changed"},
		{name: "changed generation", mutate: func(c *core.LeaseClaim) { c.ClaimedAt = "2026-01-01T00:00:00Z" }, wantError: "terminal claim changed"},
		{name: "changed resource", mutate: func(c *core.LeaseClaim) { c.CloudID = "i-replacement" }, wantError: "terminal resource identity changed"},
		{name: "changed scope", mutate: func(c *core.LeaseClaim) { c.ProviderScope = "account:999999999999" }, wantError: "terminal resource identity changed"},
		{name: "changed repository", mutate: func(c *core.LeaseClaim) { c.RepoRoot += "-other" }, wantError: "terminal resource identity changed"},
		{name: "invalid receipt", mutate: func(c *core.LeaseClaim) { delete(c.Labels, "aws_key_pair_id") }, wantError: "valid scoped terminal receipt"},
		{name: "wrong current account", account: "999999999999", wantError: "source account changed"},
		{name: "outside configured region", region: "us-west-2", wantError: "outside configured AWS regions"},
		{name: "unknown absence", getErr: errors.New("synthetic resource read failure"), wantError: "synthetic resource read failure"},
	} {
		t.Run(tc.name, func(t *testing.T) { runCheckpointRetirementReceipt(t, tc) })
	}
}

type checkpointReceiptCase struct {
	name, account, region, wantError string
	mutate                           func(*core.LeaseClaim)
	getErr                           error
}

func runCheckpointRetirementReceipt(t *testing.T, tc checkpointReceiptCase) {
	t.Helper()
	dirs := testutil.IsolateUserDirs(t)
	repo := t.TempDir()
	t.Chdir(repo)
	for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
		t.Setenv(name, "")
	}
	configPath := filepath.Join(repo, "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: aws\ntarget: linux\naws:\n  region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_AWS_REGION", "us-east-1")
	fake := &fakeAWSClient{}
	t.Cleanup(installFixedAWSTestClient(t, fake))
	cfg := fixedAWSTestConfig()
	cfg.AWSSSHCIDRs = []string{"127.0.0.1/32"}
	b := NewAWSLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
	lease, err := b.Acquire(t.Context(), AcquireRequest{Repo: core.Repo{Root: repo}, Keep: true, RequestedLeaseID: "cbx_abcdef123493", RequestedSlug: "retirement-receipt"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	const checkpointID = "chk_aws_retained_receipt"
	capture := core.NativeCheckpointCapture{
		SourceDisposition: "retire", Phase: "retiring", SourceID: source.CloudID, SourceName: lease.Server.Name,
		SourceScope: source.ProviderScope, SourceRevision: source.Revision, SourceClaimedAt: source.ClaimedAt,
		SourceIntent: source.FixedCreateIntent.Fingerprint,
	}
	if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		claim.CheckpointCapture = &core.CheckpointCaptureBinding{ID: checkpointID, Revision: source.Revision}
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	bound, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	record := map[string]any{
		"id": checkpointID, "kind": "aws-ami", "provider": "aws", "leaseId": lease.LeaseID,
		"createdAt": stamp, "lastUsedAt": stamp, "capture": capture, "repo": map[string]string{"root": repo},
		"native": map[string]any{"provider": "aws", "kind": "aws-ami", "imageId": "ami-captured", "region": "us-east-1", "accountId": "123456789012", "direct": true, "strategy": "image", "noReboot": true},
	}
	path := filepath.Join(dirs.StateHome, "crabbox", "checkpoints", checkpointID, "checkpoint.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, bound, true)
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease, CheckpointID: checkpointID}); err != nil {
		t.Fatal(err)
	}
	terminal, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil || terminal.CloudID != source.CloudID || terminal.FixedCreateIntent.State != "released" || terminal.CheckpointCapture != nil {
		t.Fatalf("release did not produce the scoped terminal receipt: %v", err)
	}
	claimPath := filepath.Join(dirs.StateHome, "crabbox", "claims", lease.LeaseID+".json")
	if tc.mutate != nil {
		tc.mutate(&terminal)
		data, err := json.Marshal(terminal)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(claimPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	// Reopen the backend with empty inventory, as after process interruption and
	// EC2's disappearance of terminated instances. No create/delete is allowed.
	reloaded := &fakeAWSClient{accountID: tc.account, getErr: tc.getErr}
	newAWSClient = func(context.Context, Config) (awsClient, error) { return reloaded, nil }
	if tc.region != "" {
		t.Setenv("CRABBOX_AWS_REGION", tc.region)
	}
	for replay := 0; replay < 2; replay++ {
		var output bytes.Buffer
		err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), []string{"checkpoint", "create", "--provider", "aws", "--id", lease.LeaseID, "--checkpoint-id", checkpointID, "--retire-source", "--mode", "native", "--wait=false", "--json"})
		if tc.wantError != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("replay error=%v, want %q", err, tc.wantError)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(data, after) {
				t.Fatal("refused replay changed the checkpoint journal")
			}
			continue
		}
		if err != nil {
			t.Fatalf("retirement replay %d rejected the released receipt: %v; output=%s", replay, err, output.String())
		}
		var got struct {
			Capture core.NativeCheckpointCapture `json:"capture"`
		}
		if err := json.Unmarshal(output.Bytes(), &got); err != nil || got.Capture.Phase != "retired" {
			t.Fatalf("retirement did not finish: %v %s", err, output.String())
		}
	}
	after, err := os.ReadFile(claimPath)
	if err != nil || !bytes.Equal(before, after) || reloaded.createCalls != 0 || len(reloaded.deletedInstances) != 0 || len(reloaded.deletedKeys) != 0 || len(fake.deletedInstances) != 1 || len(fake.deletedKeys) != 1 {
		t.Fatal("retirement replay changed the receipt or repeated provider effects")
	}
}
