package aws

import (
	core "github.com/openclaw/crabbox/internal/cli"
	"testing"
)

func TestAWSCheckpointAbandonScope(t *testing.T) {
	for _, tc := range []struct {
		name, account, region, priorAccount, priorRegion string
		missing, fail                                    bool
	}{
		{name: "legacy fixed scope", account: "123456789012", region: "us-east-1"},
		{name: "ordinary absent attested", priorAccount: "123456789012", priorRegion: "us-east-1", missing: true},
		{name: "ordinary absent unattested", missing: true, fail: true},
		{name: "wrong account", account: "999999999999", region: "us-east-1", fail: true},
		{name: "invalid account", account: "invalid-id12", region: "us-east-1", fail: true},
		{name: "missing region", account: "123456789012", fail: true},
		{name: "account conflict", account: "123456789012", region: "us-east-1", priorAccount: "999999999999", fail: true},
		{name: "region conflict", account: "123456789012", region: "us-east-1", priorRegion: "us-west-2", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := &core.LeaseClaim{Labels: map[string]string{"aws_account_id": tc.account, "aws_region": tc.region}}
			if tc.missing {
				claim = nil
			}
			got, err := (&awsLeaseBackend{}).NativeCheckpointAbandonScope(claim, core.NativeCheckpointCapture{SourceScope: "account:123456789012"}, core.NativeCheckpointImage{AccountID: tc.priorAccount, Region: tc.priorRegion})
			if (err != nil) != tc.fail {
				t.Fatalf("scope=%+v err=%v", got, err)
			}
			if !tc.fail && (got.AccountID != "123456789012" || got.Region != "us-east-1") {
				t.Fatalf("scope=%+v", got)
			}
		})
	}
}
