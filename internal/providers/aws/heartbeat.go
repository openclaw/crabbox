package aws

import (
	"context"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *awsLeaseBackend) AuthorizeStatusTouchClaim(ctx context.Context, lease LeaseTarget, claim core.LeaseClaim) error {
	client, err := newAWSClient(ctx, awsConfigForServer(b.Cfg, lease.Server))
	if err != nil {
		return err
	}
	return authorizeAWSTouch(ctx, client, lease, claim)
}

func authorizeAWSTouch(ctx context.Context, client awsClient, lease LeaseTarget, claim core.LeaseClaim) error {
	if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
		return err
	}
	if err := validateExactAWSClaim(lease.Server, lease.LeaseID, claim); err != nil {
		return err
	}
	accountID := strings.TrimSpace(claim.Labels["aws_account_id"])
	expectedScope := ""
	if claim.Provider == core.FixedAWSClaimProvider {
		expectedScope = "account:" + accountID
		intent := claim.FixedCreateIntent
		if intent == nil || intent.State != fixedAWSIntentAcquired || intent.Fingerprint == "" {
			return exit(4, "AWS lease %s has no acquired fixed intent; refusing touch", lease.LeaseID)
		}
		if err := validateFixedAWSServer(lease.Server, lease.LeaseID, claim.Slug, intent.Fingerprint, accountID); err != nil {
			return err
		}
	}
	if accountID == "" || claim.ProviderScope != expectedScope || lease.Server.Labels["aws_account_id"] != accountID || isAWSTerminalServer(lease.Server) {
		return exit(4, "AWS lease %s account, scope, or live instance state differs from its exact claim; refusing touch", lease.LeaseID)
	}
	matched, err := awsClaimMatchesCurrentAccount(ctx, client, claim)
	if err != nil {
		return err
	}
	if !matched {
		return exit(4, "AWS lease %s caller account differs from its exact claim; refusing touch", lease.LeaseID)
	}
	return nil
}

func (b *awsLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	expected, exists, set := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return Server{}, exit(4, "AWS lease %s has no exact claim snapshot; refusing touch", req.Lease.LeaseID)
	}
	if err := validateExactAWSClaim(req.Lease.Server, req.Lease.LeaseID, expected); err != nil {
		return Server{}, err
	}
	cfg := awsConfigForServer(b.Cfg, req.Lease.Server)
	if expected.IdleTimeoutSeconds > 0 {
		cfg.IdleTimeout = time.Duration(expected.IdleTimeoutSeconds) * time.Second
	}
	now := time.Now().UTC()
	// Fence the current claim across account/resource checks and the tag write;
	// a coordinator round trip must not let an old owner renew a replacement.
	updated, server, _, err := core.UpdateLeaseClaimTouchIfUnchangedAction(req.Lease.LeaseID, expected, now, req.IdleTimeoutOverride, func() (Server, SSHTarget, bool, error) {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return Server{}, SSHTarget{}, false, err
		}
		server, err := client.GetServer(ctx, expected.CloudID)
		if err != nil {
			return Server{}, SSHTarget{}, false, err
		}
		server = annotateAWSServerRegion(server, cfg.AWSRegion)
		lease := req.Lease
		lease.Server = server
		if err := authorizeAWSTouch(ctx, client, lease, expected); err != nil {
			return Server{}, SSHTarget{}, false, err
		}
		server.Labels = core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(server.Labels, cfg, req.State, now, req.IdleTimeoutOverride)
		if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
			return Server{}, SSHTarget{}, false, err
		}
		return server, req.Lease.SSH, true, nil
	})
	if err != nil {
		return Server{}, err
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}
