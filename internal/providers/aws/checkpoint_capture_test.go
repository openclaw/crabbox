package aws

import (
	"context"
	"errors"
	"github.com/openclaw/crabbox/internal/testutil"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCheckpointSourceAbsenceUsesExactAccountRegionAndID(t *testing.T) {
	for _, tc := range []struct {
		name, account, state string
		err                  error
		absent, failure      bool
	}{
		{name: "unlabelled live source", state: "stopped"},
		{name: "terminated source", state: "terminated", absent: true},
		{name: "exact missing", err: &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"}, absent: true},
		{name: "transport failure", err: errors.New("transport unavailable"), failure: true},
		{name: "wrong account", account: "999999999999", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAWSClient{accountID: tc.account, getErr: tc.err, get: map[string]core.Server{"i-fixture": {CloudID: "i-fixture", Status: tc.state}}}
			old := newAWSClient
			newAWSClient = func(_ context.Context, cfg core.Config) (awsClient, error) {
				if cfg.AWSRegion != "us-east-1" {
					t.Fatalf("wrong source region: %q", cfg.AWSRegion)
				}
				return fake, nil
			}
			t.Cleanup(func() { newAWSClient = old })
			b := &awsLeaseBackend{}
			absent, err := b.CheckpointSourceAbsent(context.Background(), core.CheckpointSourceRequest{AccountID: "123456789012", Capture: core.NativeCheckpointCapture{SourceID: "i-fixture"}, Resource: core.NativeCheckpointResourceRequest{Image: core.NativeCheckpointImage{Region: "us-east-1"}}})
			if absent != tc.absent || (err != nil) != tc.failure {
				t.Fatalf("absent=%v err=%v", absent, err)
			}
			if len(fake.deletedInstances) != 0 || len(fake.servers) != 0 {
				t.Fatal("absence used managed inventory or mutated source")
			}
			if tc.account == "" && (len(fake.getIDs) != 1 || fake.getIDs[0] != "i-fixture") {
				t.Fatalf("reads=%v", fake.getIDs)
			}
		})
	}
}

func TestCheckpointAccessRebindsAWSStoredKey(t *testing.T) {
	for _, transport := range []string{"direct", "ssm"} {
		t.Run(transport, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			const leaseID = "cbx_abcdef123456"
			cfg := core.Config{Provider: "aws", AWSRegion: "us-west-2", AWSSSHTransport: transport}
			key, _, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
			if err != nil {
				t.Fatal(err)
			}
			server := awsTestServer("i-0123456789abcdef0", leaseID, "capture", "us-west-2")
			server.Labels["aws_ssh_transport"] = transport
			fake := &fakeAWSClient{servers: []core.Server{server}}
			old := newAWSClient
			newAWSClient = func(context.Context, core.Config) (awsClient, error) { return fake, nil }
			t.Cleanup(func() { newAWSClient = old })
			b := NewAWSLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*awsLeaseBackend)
			lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true, CheckpointAccess: true, NoLocalStateMutations: true})
			if err != nil || lease.SSH.Key != key {
				t.Fatalf("key=%s want=%s err=%v", lease.SSH.Key, key, err)
			}
			if transport == "ssm" && !strings.Contains(lease.SSH.ProxyCommand, "AWS-StartSSHSession") {
				t.Fatalf("lost SSM preparation transport: %+v", lease.SSH)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
				t.Fatalf("resolution mutated local claim: exists=%v err=%v", exists, err)
			}
		})
	}
}
