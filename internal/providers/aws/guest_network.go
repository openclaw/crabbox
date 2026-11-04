package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type awsGuestNetworkClient interface {
	InspectGuestNetworkServer(context.Context, core.Config, core.Server, string) (*core.GuestNetworkReceipt, error)
	CleanupGuestNetworkForServer(context.Context, core.Server) error
}

func (*awsLeaseBackend) GuestNetworkVersion() int { return core.GuestNetworkVersion }

func inspectAWSGuestBeforeAccess(ctx context.Context, client awsClient, cfg core.Config, server core.Server, leaseID string) (*core.GuestNetworkReceipt, error) {
	if cfg.GuestNetworkPolicy == nil {
		return nil, nil
	}
	capable, ok := client.(awsGuestNetworkClient)
	if !ok {
		return nil, errors.New("AWS client cannot inspect guest network enforcement")
	}
	receipt, err := capable.InspectGuestNetworkServer(ctx, cfg, server, leaseID)
	if err != nil {
		return nil, err
	}
	if err := core.ValidateGuestNetworkReceipt(*cfg.GuestNetworkPolicy, receipt, leaseID, "aws", server.CloudID); err != nil {
		return nil, err
	}
	return receipt, nil
}

// Only the fixed acquisition's already validated, owned resource reaches here.
func rejectAWSGuestBeforeAccess(client awsClient, server core.Server, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := client.DeleteServer(ctx, server.CloudID); err != nil {
		return errors.Join(cause, fmt.Errorf("terminate rejected guest allocation: %w", err))
	}
	if server.Labels["guest_network_group"] == "" {
		return errors.Join(cause, errors.New("rejected guest instance terminated; network cleanup identity missing, cleanup unconfirmed"))
	}
	if err := cleanupAWSGuestBoundary(ctx, client, server); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
func prepareAWSGuestAccess(ctx context.Context, client awsClient, cfg core.Config, server core.Server, leaseID string, prepare func() error) (*core.GuestNetworkReceipt, error) {
	if cfg.GuestNetworkPolicy != nil && cfg.GuestNetworkPolicy.BootstrapMode == core.GuestBootstrapPublicKeyV1 {
		finalizer, ok := client.(interface {
			FinalizeGuestNetworkBootstrap(context.Context, core.Config, *core.Server, string) error
		})
		if !ok {
			return nil, rejectAWSGuestBeforeAccess(client, server, errors.New("provider cannot finalize public-key guest bootstrap"))
		}
		if err := finalizer.FinalizeGuestNetworkBootstrap(ctx, cfg, &server, leaseID); err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			return nil, rejectAWSGuestBeforeAccess(client, server, err)
		}
	}
	receipt, err := inspectAWSGuestBeforeAccess(ctx, client, cfg, server, leaseID)
	if err != nil {
		return nil, rejectAWSGuestBeforeAccess(client, server, err)
	}
	if err := prepare(); err != nil {
		return nil, err
	}
	return receipt, nil
}

func cleanupAWSGuestBoundary(ctx context.Context, client awsClient, server core.Server) error {
	if server.Labels["guest_network_group"] == "" {
		return nil
	}
	capable, ok := client.(awsGuestNetworkClient)
	if !ok {
		return errors.New("AWS client cannot clean up owned guest network boundary")
	}
	return capable.CleanupGuestNetworkForServer(ctx, server)
}
func (b *awsLeaseBackend) resolvedAWSGuestTarget(ctx context.Context, cfg core.Config, server core.Server, target core.SSHTarget, leaseID string, releaseOnly bool) (core.LeaseTarget, error) {
	var receipt *core.GuestNetworkReceipt
	if cfg.GuestNetworkPolicy != nil && !releaseOnly {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		receipt, err = inspectAWSGuestBeforeAccess(ctx, client, cfg, server, leaseID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if receipt != nil && receipt.BootstrapMode == core.GuestBootstrapPublicKeyV1 {
		target.SSHHostKey = receipt.Facts["sshHostKey"]
	}
	lease, err := b.ResolvedLeaseTarget(server, target, leaseID, releaseOnly)
	if err != nil {
		return lease, err
	}
	lease.GuestNetwork = receipt
	return lease, nil
}
