package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

func (c *CoordinatorClient) prepareManagedGuestNetwork(ctx context.Context, cfg Config) error {
	if !guestNetworkRequested(cfg) {
		return nil
	}
	c.guestNetworkMu.Lock()
	defer c.guestNetworkMu.Unlock()
	if c.guestNetworkExpected != nil {
		if cfg.Provider != c.guestNetworkProvider {
			return errors.New("managed guest provider changed after policy handshake")
		}
		if cfg.GuestNetworkPolicy != nil {
			expected, err := guestNetworkPolicyIdentity(*cfg.GuestNetworkPolicy)
			if err != nil || expected != *c.guestNetworkExpected {
				return errors.New("managed guest expected policy changed after handshake")
			}
		}
		return nil
	}
	var response struct {
		Version         int                        `json:"version"`
		Provider        string                     `json:"provider"`
		AllocationRoute string                     `json:"allocationRoute"`
		State           string                     `json:"state"`
		Policy          GuestNetworkPolicyIdentity `json:"policy"`
	}
	if err := c.doRead(ctx, "/v1/guest-network/capability?provider="+url.QueryEscape(cfg.Provider), &response); err != nil {
		return fmt.Errorf("managed guest network capability required: %w", err)
	}
	if response.Version != GuestNetworkVersion || response.Provider != cfg.Provider || response.AllocationRoute != GuestNetworkRouteManagedCoordinator || response.State != "implemented" || response.Policy.Version != GuestNetworkVersion || response.Policy.PolicyID == "" || response.Policy.PolicyRevision == "" || len(response.Policy.PolicyDigest) != 64 || response.Policy.BootstrapMode != GuestBootstrapPublicKeyV1 {
		return errors.New("managed guest network capability/policy is unknown or unsupported")
	}
	if cfg.GuestNetworkPolicy != nil {
		expected, err := guestNetworkPolicyIdentity(*cfg.GuestNetworkPolicy)
		if err != nil {
			return err
		}
		if expected != response.Policy {
			return errors.New("local expected policy does not match independently configured server policy")
		}
	}
	expected := response.Policy
	c.guestNetworkExpected = &expected
	c.guestNetworkProvider = cfg.Provider
	return nil
}
func (c *CoordinatorClient) guestNetworkExpectation() *GuestNetworkPolicyIdentity {
	c.guestNetworkMu.RLock()
	defer c.guestNetworkMu.RUnlock()
	if c.guestNetworkExpected == nil {
		return nil
	}
	value := *c.guestNetworkExpected
	return &value
}
func (c *CoordinatorClient) validateManagedGuestLease(lease CoordinatorLease, requestedID string) error {
	expected := c.guestNetworkExpectation()
	if expected == nil {
		return errors.New("managed guest policy handshake missing")
	}
	if lease.ID != requestedID || lease.Provider != c.guestNetworkProvider || !lease.UntrustedWorkload {
		return errors.New("managed guest lease identity/mode mismatch")
	}
	if err := validateGuestNetworkIdentityReceipt(*expected, lease.GuestNetwork, requestedID, lease.Provider, lease.CloudID); err != nil {
		return err
	}
	if lease.SSHHostKey == "" || lease.GuestNetwork.Facts["sshHostKey"] != lease.SSHHostKey || lease.GuestNetwork.Facts["instanceProfile"] != "none" || lease.GuestNetwork.Facts["imds"] != "roleless-v2" || lease.GuestNetwork.Facts["bootstrapMode"] != GuestBootstrapPublicKeyV1 || len(lease.GuestNetwork.Facts["bootstrapSha256"]) != 64 {
		return errors.New("managed guest enforced bootstrap/transport facts missing or mismatched")
	}
	return nil
}
func (c *CoordinatorClient) getManagedGuestLease(ctx context.Context, id string) (CoordinatorLease, error) {
	var response struct {
		Lease  CoordinatorLease           `json:"lease"`
		Policy GuestNetworkPolicyIdentity `json:"policy"`
	}
	if err := c.doRead(ctx, "/v1/guest-network/leases/"+url.PathEscape(id), &response); err != nil {
		return CoordinatorLease{}, err
	}
	expected := c.guestNetworkExpectation()
	if expected == nil || *expected != response.Policy {
		return CoordinatorLease{}, errors.New("managed inspected policy precondition mismatch")
	}
	if err := c.validateManagedGuestLease(response.Lease, id); err != nil {
		return CoordinatorLease{}, err
	}
	return response.Lease, nil
}
func (c *CoordinatorClient) GetLeaseForRelease(ctx context.Context, id string) (CoordinatorLease, error) {
	return c.getLease(ctx, id, false)
}
func (c *CoordinatorClient) guestNetworkCreatePath(ctx context.Context, cfg Config, leaseID string) (string, *GuestNetworkPolicyIdentity, error) {
	if !guestNetworkRequested(cfg) {
		return "", nil, nil
	}
	if !IsCanonicalLeaseID(leaseID) {
		return "", nil, errors.New("managed guest create requires a fixed canonical lease ID")
	}
	if err := c.prepareManagedGuestNetwork(ctx, cfg); err != nil {
		return "", nil, err
	}
	return "/v1/guest-network/leases/" + url.PathEscape(leaseID), c.guestNetworkExpectation(), nil
}

// Keep the dedicated versioned route: an older server must return not-found,
// never silently ignore a guest flag on its ordinary create endpoint.
