package aws

import (
	"context"
	"errors"
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
			fake := &fakeAWSClient{accountID: tc.account, getErr: tc.err, get: map[string]Server{"i-fixture": {CloudID: "i-fixture", Status: tc.state}}}
			old := newAWSClient
			newAWSClient = func(_ context.Context, cfg Config) (awsClient, error) {
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
