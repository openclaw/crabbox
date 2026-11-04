package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const awsGuestCompanionKey = "aws-guest-network-v1"

type awsGuestCompanion struct {
	Version   int                          `json:"version"`
	Region    string                       `json:"region"`
	AccountID string                       `json:"accountId"`
	Boundary  core.AWSGuestNetworkBoundary `json:"boundary"`
}

func persistAWSGuestCompanion(tx *core.FixedTransaction, region, accountID string, boundary core.AWSGuestNetworkBoundary) error {
	if tx == nil || tx.Claim == nil || tx.Claim.FixedCreateIntent == nil || boundary.LeaseID != tx.Claim.LeaseID || boundary.SecurityGroupName == "" || boundary.PolicyDigest == "" {
		return errors.New("guest network cleanup custody is incomplete")
	}
	if boundary.Region != "" {
		region = boundary.Region
	}
	if region == "" || accountID == "" {
		return errors.New("guest network cleanup region/account missing")
	}
	if previous := tx.Claim.FixedCreateIntent.Companions[awsGuestCompanionKey]; previous != "" {
		var old awsGuestCompanion
		if err := json.Unmarshal([]byte(previous), &old); err != nil || old.Version != 1 || old.Region != region || old.AccountID != accountID || old.Boundary.LeaseID != boundary.LeaseID || old.Boundary.PolicyDigest != boundary.PolicyDigest || old.Boundary.CreateAttemptID != boundary.CreateAttemptID || old.Boundary.CreateRejectedNoEffect && !boundary.CreateRejectedNoEffect || old.Boundary.SubnetID != boundary.SubnetID || old.Boundary.VPCID != boundary.VPCID || old.Boundary.SecurityGroupName != boundary.SecurityGroupName || old.Boundary.SecurityGroupID != "" && old.Boundary.SecurityGroupID != boundary.SecurityGroupID {
			return errors.New("guest network custody cannot be replaced by a mismatched boundary")
		}
	}
	if boundary.CreateRejectedNoEffect && (!core.AWSGuestNetworkCreateRejectedNoEffect(boundary) || tx.Claim.FixedCreateIntent.Companions[awsGuestCompanionKey] == "") {
		return errors.New("definitive guest rejection lacks matching prior attempt custody")
	}
	data, err := json.Marshal(awsGuestCompanion{Version: 1, Region: region, AccountID: accountID, Boundary: boundary})
	if err != nil {
		return err
	}
	intent := tx.Claim.FixedCreateIntent
	intent.Companions = maps.Clone(intent.Companions)
	if intent.Companions == nil {
		intent.Companions = map[string]string{}
	}
	intent.Companions[awsGuestCompanionKey] = string(data)
	phase := "prepared"
	if intent.Journal != nil {
		phase = intent.Journal.Phase
	}
	return tx.Record(phase)
}

type awsGuestCompanionClient interface {
	ResolveGuestNetworkBoundary(context.Context, core.AWSGuestNetworkBoundary) (core.AWSGuestNetworkBoundary, error)
	CleanupGuestNetwork(context.Context, core.AWSGuestNetworkBoundary) error
}

func cleanupAWSFixedTerminal(claim core.LeaseClaim) error {
	if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Companions[awsGuestCompanionKey] != "" {
		var custody awsGuestCompanion
		if err := json.Unmarshal([]byte(claim.FixedCreateIntent.Companions[awsGuestCompanionKey]), &custody); err != nil || custody.Version != 1 || custody.Region == "" || custody.AccountID == "" || custody.Boundary.LeaseID != claim.LeaseID {
			return errors.New("guest network cleanup custody record invalid")
		}
		if custody.Boundary.CreateRejectedNoEffect {
			if !core.AWSGuestNetworkCreateRejectedNoEffect(custody.Boundary) {
				return errors.New("invalid definitive guest rejection custody")
			}
			return cleanupAWSLeaseSSH(claim.LeaseID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		client, err := newAWSClient(ctx, core.Config{Provider: "aws", AWSRegion: custody.Region})
		if err != nil {
			return err
		}
		account, err := client.CallerAccountID(ctx)
		if err != nil {
			return err
		}
		if account != custody.AccountID {
			return errors.New("guest network cleanup account mismatch")
		}
		capable, ok := client.(awsGuestCompanionClient)
		if !ok {
			return errors.New("AWS client cannot recover owned guest network cleanup")
		}
		boundary := custody.Boundary
		if boundary.SecurityGroupID == "" {
			boundary, err = capable.ResolveGuestNetworkBoundary(ctx, boundary)
			if err != nil {
				return fmt.Errorf("resolve uncertain guest network creation; retain custody: %w", err)
			}
		}
		if err := capable.CleanupGuestNetwork(ctx, boundary); err != nil {
			return fmt.Errorf("clean up owned guest network companion: %w", err)
		}
	}
	return cleanupAWSLeaseSSH(claim.LeaseID)
}

// Retire only the exact persisted no-effect attempt; never resolve an unknown
// creation by assuming absence, and never remove other provider guard references.
func retireAWSGuestRejectedCompanion(tx *core.FixedTransaction) (bool, error) {
	if tx == nil || tx.Claim == nil || tx.Claim.FixedCreateIntent == nil {
		return false, errors.New("missing guest companion transaction")
	}
	intent := tx.Claim.FixedCreateIntent
	raw := intent.Companions[awsGuestCompanionKey]
	if raw == "" {
		return false, nil
	}
	var custody awsGuestCompanion
	if json.Unmarshal([]byte(raw), &custody) != nil || custody.Version != 1 || custody.Region == "" || custody.AccountID == "" || custody.Boundary.LeaseID != tx.Claim.LeaseID {
		return false, errors.New("invalid guest companion custody")
	}
	if !custody.Boundary.CreateRejectedNoEffect {
		return false, nil
	}
	if !core.AWSGuestNetworkCreateRejectedNoEffect(custody.Boundary) {
		return false, errors.New("invalid definitive guest rejection custody")
	}
	previous := intent.Companions
	intent.Companions = maps.Clone(previous)
	delete(intent.Companions, awsGuestCompanionKey)
	phase := "prepared"
	if intent.Journal != nil {
		phase = intent.Journal.Phase
	}
	if err := tx.Record(phase); err != nil {
		intent.Companions = previous
		return false, err
	}
	return true, nil
}
