package aws

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type guestCustodyAWSClient struct {
	*fakeAWSClient
	resolved, cleaned int
	resolveErr        error
}

func (c *guestCustodyAWSClient) ResolveGuestNetworkBoundary(_ context.Context, b core.AWSGuestNetworkBoundary) (core.AWSGuestNetworkBoundary, error) {
	c.resolved++
	b.SecurityGroupID = "sg-11111111"
	return b, c.resolveErr
}
func (c *guestCustodyAWSClient) CleanupGuestNetwork(context.Context, core.AWSGuestNetworkBoundary) error {
	c.cleaned++
	return nil
}
func TestGuestNetworkTerminalCleanupUsesPersistedCustody(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"known group", "uncertain create", "unresolved create", "account mismatch"} {
		t.Run(name, func(t *testing.T) {
			client := &guestCustodyAWSClient{fakeAWSClient: &fakeAWSClient{accountID: "000000000000"}}
			boundary := core.AWSGuestNetworkBoundary{LeaseID: "cbx_0123456789ab", SecurityGroupID: "sg-11111111", SecurityGroupName: "owned-name"}
			if name == "uncertain create" || name == "unresolved create" {
				boundary.SecurityGroupID = ""
			}
			if name == "unresolved create" {
				client.resolveErr = errors.New("creation outcome unresolved")
			}
			if name == "account mismatch" {
				client.accountID = "111111111111"
			}
			custody := awsGuestCompanion{Version: 1, Region: "example-region-1", AccountID: "000000000000", Boundary: boundary}
			data, err := json.Marshal(custody)
			if err != nil {
				t.Fatal(err)
			}
			claim := core.LeaseClaim{LeaseID: boundary.LeaseID, FixedCreateIntent: &core.FixedCreateIntent{Companions: map[string]string{awsGuestCompanionKey: string(data)}}}
			old := newAWSClient
			newAWSClient = func(_ context.Context, cfg core.Config) (awsClient, error) {
				if cfg.AWSRegion != custody.Region {
					t.Fatal("cleanup lost region custody")
				}
				return client, nil
			}
			defer func() { newAWSClient = old }()
			err = cleanupAWSFixedTerminal(claim)
			rejected := name == "unresolved create" || name == "account mismatch"
			if rejected {
				if err == nil || client.cleaned != 0 {
					t.Fatalf("unproven cleanup accepted: %v cleaned=%d", err, client.cleaned)
				}
			} else if err != nil || client.cleaned != 1 {
				t.Fatalf("owned cleanup failed: %v cleaned=%d", err, client.cleaned)
			}
		})
	}
}
func TestGuestNetworkCustodyRejectsMismatchedOverwrite(t *testing.T) {
	old := awsGuestCompanion{Version: 1, Region: "example-region-1", AccountID: "000000000000", Boundary: core.AWSGuestNetworkBoundary{LeaseID: "cbx_0123456789ab", SubnetID: "subnet-11111111", VPCID: "vpc-11111111", SecurityGroupName: "owned-name", PolicyDigest: "original"}}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	tx := &core.FixedTransaction{Claim: &core.LeaseClaim{LeaseID: old.Boundary.LeaseID, FixedCreateIntent: &core.FixedCreateIntent{Companions: map[string]string{awsGuestCompanionKey: string(data)}}}}
	changed := old.Boundary
	changed.PolicyDigest = "replacement"
	if persistAWSGuestCompanion(tx, old.Region, old.AccountID, changed) == nil {
		t.Fatal("mismatched custody overwritten")
	}
	if tx.Claim.FixedCreateIntent.Companions[awsGuestCompanionKey] != string(data) {
		t.Fatal("failed overwrite mutated custody")
	}
}

func TestGuestNetworkCustodyCannotRetirePriorUncertainAttempt(t *testing.T) {
	old := awsGuestCompanion{Version: 1, Region: "us-east-1", AccountID: "000000000000", Boundary: core.AWSGuestNetworkBoundary{CreateAttemptID: "old-unknown", LeaseID: "cbx_0123456789ab", PolicyDigest: "digest", SubnetID: "subnet-11111111", VPCID: "vpc-11111111", SecurityGroupName: "owned-name"}}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	tx := &core.FixedTransaction{Claim: &core.LeaseClaim{LeaseID: old.Boundary.LeaseID, FixedCreateIntent: &core.FixedCreateIntent{Companions: map[string]string{awsGuestCompanionKey: string(data), "other-guard": "retain"}}}}
	replacement := old.Boundary
	replacement.CreateAttemptID = "new-rejected"
	replacement.CreateRejectedNoEffect = true
	if persistAWSGuestCompanion(tx, old.Region, old.AccountID, replacement) == nil {
		t.Fatal("new rejection overwrote old uncertain attempt")
	}
	if tx.Claim.FixedCreateIntent.Companions[awsGuestCompanionKey] != string(data) || tx.Claim.FixedCreateIntent.Companions["other-guard"] != "retain" {
		t.Fatal("failed attempt fence changed guards")
	}
}

func TestGuestNetworkRejectedRetirementRequiresFence(t *testing.T) {
	b := core.AWSGuestNetworkBoundary{CreateAttemptID: "rejected-attempt", CreateRejectedNoEffect: true, LeaseID: "cbx_0123456789ab", PolicyDigest: "digest", SubnetID: "subnet-11111111", VPCID: "vpc-11111111", SecurityGroupName: "owned-name"}
	raw, err := json.Marshal(awsGuestCompanion{Version: 1, Region: "us-east-1", AccountID: "000000000000", Boundary: b})
	if err != nil {
		t.Fatal(err)
	}
	tx := &core.FixedTransaction{Claim: &core.LeaseClaim{LeaseID: b.LeaseID, FixedCreateIntent: &core.FixedCreateIntent{Companions: map[string]string{awsGuestCompanionKey: string(raw), "other-guard": "retain"}}}}
	// This transaction has no matching engine fence. It must not retire custody.
	if retired, err := retireAWSGuestRejectedCompanion(tx); err == nil || retired {
		t.Fatal("retirement bypassed transaction fence")
	}
	if tx.Claim.FixedCreateIntent.Companions[awsGuestCompanionKey] != string(raw) || tx.Claim.FixedCreateIntent.Companions["other-guard"] != "retain" {
		t.Fatal("failed retirement changed persisted guard references")
	}
}
