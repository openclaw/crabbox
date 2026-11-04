package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ValidateAWSGuestNetworkConfig bounds this initial path to trusted prebuilt Linux
// images. No Crabbox user-data, IAM role, overlay credentials or shared caches
// are delivered before the provider's actual-resource inspection gate.
func ValidateAWSGuestNetworkConfig(cfg Config) error {
	if cfg.GuestNetworkPolicy == nil {
		if cfg.UntrustedWorkload {
			return errors.New("direct guest allocation requires operator policy custody")
		}
		return nil
	}
	if err := awsGuestBootstrapPolicy(*cfg.GuestNetworkPolicy); err != nil {
		return err
	}
	publicBootstrap := cfg.GuestNetworkPolicy.BootstrapMode == GuestBootstrapPublicKeyV1
	if publicBootstrap {
		osImage, err := normalizeOSImage(cfg.OSImage)
		if err != nil || (osImage != "ubuntu:22.04" && osImage != "ubuntu:24.04") || cfg.AWSAMI != "" || cfg.AWSSnapshot != "" {
			return errors.New("public-key guest bootstrap requires an ordinary supported Ubuntu image, not a snapshot/custom AMI")
		}
	}
	if _, err := GuestNetworkPolicyDigest(*cfg.GuestNetworkPolicy); err != nil {
		return err
	}
	if cfg.TargetOS != "linux" || (!publicBootstrap && strings.TrimSpace(cfg.AWSAMI) == "") || strings.TrimSpace(cfg.AWSSubnetID) == "" || cfg.AWSSGID != "" || cfg.AWSProfile != "" || cfg.Tailscale.Enabled || cfg.Network != NetworkPublic || len(cfg.ExposedPorts) != 0 {
		return errors.New("guest AWS networking requires a qualified Linux image, an explicit subnet, public SSH routing, no supplied security group, instance profile, overlay, or exposed ports")
	}
	if len(cfg.EnvAllow) != 0 || cfg.Cache.Pnpm || cfg.Cache.Npm || cfg.Cache.Docker || cfg.Cache.Git || len(cfg.Cache.Volumes) != 0 {
		return errors.New("guest AWS networking requires empty environment forwarding and disabled shared caches/volumes")
	}
	return nil
}

func applyAWSGuestNetworkLaunchOptions(input *ec2.RunInstancesInput, cfg Config) error {
	if cfg.GuestNetworkPolicy == nil {
		return nil
	}
	if err := ValidateAWSGuestNetworkConfig(cfg); err != nil {
		return err
	}
	if cfg.guestNetworkBoundary == nil || len(input.NetworkInterfaces) != 1 || len(input.NetworkInterfaces[0].Groups) != 1 || input.NetworkInterfaces[0].Groups[0] != cfg.guestNetworkBoundary.SecurityGroupID || aws.ToString(input.NetworkInterfaces[0].SubnetId) != cfg.guestNetworkBoundary.SubnetID {
		return errors.New("guest AWS launch has no enforced network boundary")
	}
	if cfg.GuestNetworkPolicy.BootstrapMode == GuestBootstrapPublicKeyV1 {
		hash, err := guestBootstrapHash(aws.ToString(input.UserData))
		if err != nil || hash != cfg.guestNetworkBoundary.BootstrapSHA256 {
			return errors.New("guest launch public-key bootstrap custody missing or mismatched")
		}
	} else {
		input.UserData = nil
	}
	input.IamInstanceProfile = nil
	input.MetadataOptions = &types.InstanceMetadataOptionsRequest{HttpEndpoint: types.InstanceMetadataEndpointStateDisabled, HttpTokens: types.HttpTokensStateRequired, InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled, HttpProtocolIpv6: types.InstanceMetadataProtocolStateDisabled}
	if cfg.GuestNetworkPolicy.BootstrapMode == GuestBootstrapPublicKeyV1 {
		input.MetadataOptions.HttpEndpoint = types.InstanceMetadataEndpointStateEnabled
		input.MetadataOptions.HttpPutResponseHopLimit = aws.Int32(1)
	}
	return nil
}

func awsGuestBoundaryLabels(boundary AWSGuestNetworkBoundary) map[string]string {
	labels := map[string]string{"guest_network_group": boundary.SecurityGroupID, "guest_network_subnet": boundary.SubnetID, "guest_network_vpc": boundary.VPCID, "guest_network_policy_sha256": boundary.PolicyDigest}
	if boundary.BootstrapMode != "" {
		labels["guest_network_bootstrap_mode"] = boundary.BootstrapMode
		labels["guest_network_bootstrap_sha256"] = boundary.BootstrapSHA256
	}
	return labels
}
func AWSGuestNetworkBoundaryFromServer(server Server) (AWSGuestNetworkBoundary, error) {
	b := AWSGuestNetworkBoundary{BootstrapMode: server.Labels["guest_network_bootstrap_mode"], BootstrapSHA256: server.Labels["guest_network_bootstrap_sha256"], LeaseID: server.Labels["lease"], SubnetID: server.Labels["guest_network_subnet"], VPCID: server.Labels["guest_network_vpc"], SecurityGroupID: server.Labels["guest_network_group"], PolicyDigest: server.Labels["guest_network_policy_sha256"]}
	if !IsCanonicalLeaseID(b.LeaseID) || !validAWSGuestID(b.SubnetID, "subnet") || !validAWSGuestID(b.VPCID, "vpc") || !validAWSGuestID(b.SecurityGroupID, "sg") || len(b.PolicyDigest) != 64 {
		return AWSGuestNetworkBoundary{}, errors.New("AWS guest boundary identity missing or invalid")
	}
	return b, nil
}

// InspectGuestNetworkServer reads the control plane on every acquisition/replay
// and access resolution; labels locate the boundary but never attest its policy.
func (c *AWSClient) InspectGuestNetworkServer(ctx context.Context, cfg Config, server Server, leaseID string) (*GuestNetworkReceipt, error) {
	if cfg.GuestNetworkPolicy == nil {
		return nil, errors.New("operator guest network policy missing")
	}
	if cfg.GuestNetworkPolicy.BootstrapMode == GuestBootstrapPublicKeyV1 && server.Labels["guest_network_ssh_host_key"] == "" {
		return nil, errors.New("guest provider transport pin not finalized")
	}
	if err := ValidateAWSGuestNetworkConfig(cfg); err != nil {
		return nil, err
	}
	boundary, err := AWSGuestNetworkBoundaryFromServer(server)
	if err != nil {
		return nil, err
	}
	if boundary.LeaseID != leaseID || boundary.SubnetID != cfg.AWSSubnetID {
		return nil, errors.New("AWS guest boundary lease/subnet mismatch")
	}
	receipt, err := c.InspectGuestNetwork(ctx, boundary, server.CloudID, *cfg.GuestNetworkPolicy)
	if err != nil {
		return nil, err
	}
	if err := ValidateGuestNetworkReceipt(*cfg.GuestNetworkPolicy, &receipt, leaseID, "aws", server.CloudID); err != nil {
		return nil, err
	}
	if cfg.GuestNetworkPolicy.BootstrapMode == GuestBootstrapPublicKeyV1 && receipt.Facts["sshHostKey"] != server.Labels["guest_network_ssh_host_key"] {
		return nil, errors.New("guest provider transport pin mismatch")
	}
	return &receipt, nil
}

// CleanupGuestNetworkForServer is called only by the owned instance release
// path. The provider must confirm instance termination before deleting the SG.
func (c *AWSClient) CleanupGuestNetworkForServer(ctx context.Context, server Server) error {
	boundary, err := AWSGuestNetworkBoundaryFromServer(server)
	if err != nil {
		return err
	}
	if err := ec2.NewInstanceTerminatedWaiter(c.ec2).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{server.CloudID}}, 2*time.Minute); err != nil {
		return fmt.Errorf("confirm guest instance termination before network cleanup: %w", err)
	}
	return c.CleanupGuestNetwork(ctx, boundary)
}
