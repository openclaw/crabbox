package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// SettleRejectedFixedCompanions is invoked under the durable fixed-claim fence.
// A terminal allocating-request rejection is not settled until original native
// companions are deleted and every lease resource slot is proved absent.
func (c *nativeAzureClient) SettleRejectedFixedCompanions(ctx context.Context, expected core.Server, binding core.AzureFixedCompanions) error {
	if err := c.validateRecoveryIdentity(expected, true); err != nil {
		return err
	}
	if expected.ImmutableID != "" || strings.TrimSpace(binding.NICGUID) == "" || strings.TrimSpace(binding.PublicIPGUID) == "" {
		return errors.New("Azure fixed rejection lacks unbound companion custody")
	}
	if err := c.verifyRejectedVMAndOtherResourcesAbsent(ctx, expected.CloudID); err != nil {
		return err
	}
	nic, _, err := c.inspectRejectedNetwork(ctx, expected, binding)
	if err != nil {
		return err
	}
	if nic {
		poller, err := c.Interfaces.BeginDelete(ctx, c.ResourceGroup, expected.CloudID+"-nic", nil)
		if err != nil && !azureResourceAbsent(err) {
			return err
		}
		if err == nil {
			if _, err := poller.PollUntilDone(ctx, nil); err != nil && !azureResourceAbsent(err) {
				return err
			}
		}
	}
	if err := c.verifyRejectedVMAndOtherResourcesAbsent(ctx, expected.CloudID); err != nil {
		return err
	}
	_, pip, err := c.inspectRejectedNetwork(ctx, expected, binding)
	if err != nil {
		return err
	}
	if pip {
		poller, err := c.PublicIPs.BeginDelete(ctx, c.ResourceGroup, expected.CloudID+"-pip", nil)
		if err != nil && !azureResourceAbsent(err) {
			return err
		}
		if err == nil {
			if _, err := poller.PollUntilDone(ctx, nil); err != nil && !azureResourceAbsent(err) {
				return err
			}
		}
	}
	_, err = c.VerifyFailedLeaseHoldAbsent(ctx, expected.Labels["lease"], expected.Labels["slug"])
	return err
}

func (c *nativeAzureClient) verifyRejectedVMAndOtherResourcesAbsent(ctx context.Context, name string) error {
	checks := []struct {
		kind string
		get  func() error
	}{
		{"VM", func() error { _, err := c.VirtualMachines.Get(ctx, c.ResourceGroup, name, nil); return err }},
		{"OS disk", func() error { _, err := c.Disks.Get(ctx, c.ResourceGroup, name+"-osdisk", nil); return err }},
		{"quarantine NSG", func() error {
			_, err := c.SecurityGroups.Get(ctx, c.ResourceGroup, name+azureSnapshotQuarantineNSGSuffix, nil)
			return err
		}},
	}
	for _, check := range checks {
		if err := check.get(); !azureResourceAbsent(err) {
			if err == nil {
				return fmt.Errorf("Azure fixed rejection cannot settle: %s still exists", check.kind)
			}
			return fmt.Errorf("verify Azure rejected %s absence: %w", check.kind, err)
		}
	}
	return nil
}

func fixedAttemptTagsMatch(tags map[string]*string, labels map[string]string) bool {
	for _, key := range []string{"fixed_attempt", "fixed_intent_sha256", "provider_key"} {
		if labels[key] == "" || stringValue(tags[key]) != labels[key] {
			return false
		}
	}
	return true
}

func (c *nativeAzureClient) inspectRejectedNetwork(ctx context.Context, expected core.Server, binding core.AzureFixedCompanions) (bool, bool, error) {
	name := expected.CloudID
	nicID := c.resourceID("Microsoft.Network", "networkInterfaces", name+"-nic")
	pipID := c.resourceID("Microsoft.Network", "publicIPAddresses", name+"-pip")
	nic, nicErr := c.Interfaces.Get(ctx, c.ResourceGroup, name+"-nic", nil)
	nicPresent := nicErr == nil
	if nicErr != nil && !azureResourceAbsent(nicErr) {
		return false, false, nicErr
	}
	if nicPresent {
		if err := validateAzureCleanupResourceTags("NIC", name+"-nic", nic.Tags, expected.Labels); err != nil {
			return false, false, err
		}
		if err := requireAzureResourceID("NIC", name+"-nic", stringValue(nic.ID), nicID); err != nil {
			return false, false, err
		}
		if !fixedAttemptTagsMatch(nic.Tags, expected.Labels) || nic.Properties == nil || nic.Properties.VirtualMachine != nil ||
			strings.TrimSpace(stringValue(nic.Properties.ResourceGUID)) != strings.TrimSpace(binding.NICGUID) {
			return false, false, errors.New("Azure rejected NIC changed identity, ownership, or attachment")
		}
		for _, config := range nic.Properties.IPConfigurations {
			if config != nil && config.Properties != nil && config.Properties.PublicIPAddress != nil &&
				!strings.EqualFold(strings.TrimSpace(stringValue(config.Properties.PublicIPAddress.ID)), pipID) {
				return false, false, errors.New("Azure rejected NIC references another public IP")
			}
		}
	}
	pip, pipErr := c.PublicIPs.Get(ctx, c.ResourceGroup, name+"-pip", nil)
	pipPresent := pipErr == nil
	if pipErr != nil && !azureResourceAbsent(pipErr) {
		return false, false, pipErr
	}
	if pipPresent {
		if err := validateAzureCleanupResourceTags("public IP", name+"-pip", pip.Tags, expected.Labels); err != nil {
			return false, false, err
		}
		if err := requireAzureResourceID("public IP", name+"-pip", stringValue(pip.ID), pipID); err != nil {
			return false, false, err
		}
		if !fixedAttemptTagsMatch(pip.Tags, expected.Labels) || pip.Properties == nil || pip.Properties.NatGateway != nil ||
			strings.TrimSpace(stringValue(pip.Properties.ResourceGUID)) != strings.TrimSpace(binding.PublicIPGUID) {
			return false, false, errors.New("Azure rejected public IP changed identity, ownership, or attachment")
		}
		if config := pip.Properties.IPConfiguration; config != nil {
			prefix := strings.ToLower(nicID) + "/ipconfigurations/"
			id := strings.ToLower(strings.TrimSpace(stringValue(config.ID)))
			if !nicPresent || !strings.HasPrefix(id, prefix) || strings.TrimPrefix(id, prefix) == "" || strings.Contains(strings.TrimPrefix(id, prefix), "/") {
				return false, false, errors.New("Azure rejected public IP has a foreign or unproven NIC association")
			}
		}
	}
	return nicPresent, pipPresent, nil
}
