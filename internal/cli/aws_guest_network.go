package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// AWSGuestNetworkAPI is the privileged EC2 control-plane seam. No observation in
// this contract is obtained from commands, files, or assertions inside a guest.
type AWSGuestNetworkAPI interface {
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	RevokeSecurityGroupEgress(context.Context, *ec2.RevokeSecurityGroupEgressInput, ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error)
	AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	AuthorizeSecurityGroupEgress(context.Context, *ec2.AuthorizeSecurityGroupEgressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// AWSGuestNetworkBoundary is an allocator-owned cleanup/launch handle, not a
// receipt or caller-provided proof. Prepare must finish before RunInstances.
// Launch must use this exact subnet and sole security group, no instance profile,
// and HttpEndpoint=disabled from the outset. Inspect must finish before any
// workload bootstrap, workspace transfer, hooks, or access is released.
type AWSGuestNetworkBoundary struct {
	CreateAttemptID        string `json:"createAttemptId,omitempty"`
	CreateRejectedNoEffect bool   `json:"createRejectedNoEffect,omitempty"`
	BootstrapMode          string `json:"bootstrapMode,omitempty"`
	BootstrapSHA256        string `json:"bootstrapSha256,omitempty"`
	Region                 string `json:"region,omitempty"`
	LeaseID                string `json:"leaseId"`
	SubnetID               string `json:"subnetId"`
	VPCID                  string `json:"vpcId"`
	SecurityGroupID        string `json:"securityGroupId"`
	SecurityGroupName      string `json:"securityGroupName"`
	PolicyDigest           string `json:"policyDigest"`
}

const AWSGuestNetworkPlatformServices = "aws-platform-services-v1"

const (
	awsGuestLeaseTag  = "crabbox-guest-lease"
	awsGuestPolicyTag = "crabbox-guest-policy"
	awsGuestOwnerTag  = "crabbox-guest-boundary"
)

var awsGuestResourceID = regexp.MustCompile(`^(sg|subnet|vpc|i|eni)-([0-9a-f]{8}|[0-9a-f]{17})$`)

func validAWSGuestID(id, kind string) bool {
	return strings.HasPrefix(id, kind+"-") && awsGuestResourceID.MatchString(id)
}

func (c *AWSClient) PrepareGuestNetwork(ctx context.Context, leaseID, subnetID string, policy GuestNetworkPolicy) (AWSGuestNetworkBoundary, error) {
	return PrepareAWSGuestNetwork(ctx, c.ec2, leaseID, subnetID, policy)
}

func (c *AWSClient) InspectGuestNetwork(ctx context.Context, boundary AWSGuestNetworkBoundary, resourceID string, policy GuestNetworkPolicy) (GuestNetworkReceipt, error) {
	return InspectAWSGuestNetwork(ctx, c.ec2, boundary, resourceID, policy)
}

func (c *AWSClient) CleanupGuestNetwork(ctx context.Context, boundary AWSGuestNetworkBoundary) error {
	return CleanupAWSGuestNetwork(ctx, c.ec2, boundary)
}

// PrepareAWSGuestNetwork creates a dedicated SG, removes its default unrestricted
// egress, installs only the operator's literal IPv4/TCP rules, and reads back the
// effective policy. It never changes an existing/shared SG. EC2 SGs explicitly do
// not filter AmazonProvidedDNS; this exception is included in every receipt and
// must not be represented as DNS isolation or a complete packet sandbox.
func PrepareAWSGuestNetwork(ctx context.Context, api AWSGuestNetworkAPI, leaseID, subnetID string, policy GuestNetworkPolicy) (AWSGuestNetworkBoundary, error) {
	return PrepareAWSGuestNetworkWithCustody(ctx, api, leaseID, subnetID, policy, nil)
}

func (c *AWSClient) PrepareGuestNetworkWithCustody(ctx context.Context, leaseID, subnetID string, policy GuestNetworkPolicy, persist func(AWSGuestNetworkBoundary) error) (AWSGuestNetworkBoundary, error) {
	wrapped := persist
	if persist != nil {
		wrapped = func(boundary AWSGuestNetworkBoundary) error { boundary.Region = c.region; return persist(boundary) }
	}
	boundary, err := PrepareAWSGuestNetworkWithCustody(ctx, c.ec2, leaseID, subnetID, policy, wrapped)
	boundary.Region = c.region
	return boundary, err
}

func PrepareAWSGuestNetworkWithCustody(ctx context.Context, api AWSGuestNetworkAPI, leaseID, subnetID string, policy GuestNetworkPolicy, persist func(AWSGuestNetworkBoundary) error) (boundary AWSGuestNetworkBoundary, retErr error) {
	digest, ingress, egress, err := awsGuestPolicy(policy)
	if err != nil {
		return boundary, err
	}
	if api == nil || leaseID == "" || len(leaseID) > 128 || strings.TrimSpace(leaseID) != leaseID || !validAWSGuestID(subnetID, "subnet") {
		return boundary, errors.New("AWS guest boundary requires an allocator lease and an actual subnet ID")
	}
	subnets, err := api.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err != nil {
		return boundary, fmt.Errorf("inspect AWS guest subnet: %w", err)
	}
	if subnets == nil || aws.ToString(subnets.NextToken) != "" || len(subnets.Subnets) != 1 || aws.ToString(subnets.Subnets[0].SubnetId) != subnetID || !validAWSGuestID(aws.ToString(subnets.Subnets[0].VpcId), "vpc") || subnets.Subnets[0].State != types.SubnetStateAvailable || aws.ToBool(subnets.Subnets[0].Ipv6Native) {
		return boundary, errors.New("AWS guest subnet observation is missing, mismatched, unavailable, or IPv6-only")
	}
	boundary = AWSGuestNetworkBoundary{CreateAttemptID: rand.Text(), LeaseID: leaseID, SubnetID: subnetID, VPCID: aws.ToString(subnets.Subnets[0].VpcId), PolicyDigest: digest}
	vpcs, err := api.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{boundary.VPCID}})
	if err != nil {
		return boundary, fmt.Errorf("inspect AWS guest VPC: %w", err)
	}
	if vpcs == nil || aws.ToString(vpcs.NextToken) != "" || len(vpcs.Vpcs) != 1 || aws.ToString(vpcs.Vpcs[0].VpcId) != boundary.VPCID || vpcs.Vpcs[0].State != types.VpcStateAvailable {
		return boundary, errors.New("AWS guest VPC observation is missing, mismatched, or unavailable")
	}
	boundary.SecurityGroupName = awsGuestGroupName(boundary)
	if persist != nil {
		if err := persist(boundary); err != nil {
			return boundary, fmt.Errorf("persist guest network cleanup custody before creation: %w", err)
		}
	}
	created, err := api.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		VpcId: aws.String(boundary.VPCID), GroupName: aws.String(boundary.SecurityGroupName),
		Description:       aws.String("Dedicated operator-controlled guest network boundary"),
		TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeSecurityGroup, Tags: awsGuestTags(boundary)}},
	}, func(options *ec2.Options) { options.Retryer = aws.NopRetryer{} })
	if err != nil && (created == nil || aws.ToString(created.GroupId) == "") && awsGuestCreateRejectedNoEffect(err) {
		boundary.CreateRejectedNoEffect = true
		if persist != nil {
			if saveErr := persist(boundary); saveErr != nil {
				return boundary, fmt.Errorf("persist definitive guest network creation rejection: %w", saveErr)
			}
		}
		return boundary, fmt.Errorf("create AWS guest boundary rejected without effect: %w", err)
	}
	if err != nil || created == nil || !validAWSGuestID(aws.ToString(created.GroupId), "sg") {
		createErr := errors.New("AWS guest boundary creation returned no valid resource ID")
		if err != nil {
			createErr = fmt.Errorf("create AWS guest boundary: %w", err)
		}
		// CreateSecurityGroup has no idempotency token. Preserve the deterministic
		// name on uncertain responses; never retry creation or adopt for launch.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		recovered, recoverErr := ResolveAWSGuestNetworkBoundary(cleanupCtx, api, boundary)
		if recoverErr != nil {
			return boundary, errors.Join(createErr, recoverErr)
		}
		var custodyErr error
		if persist != nil {
			custodyErr = persist(recovered)
		}
		return recovered, errors.Join(createErr, custodyErr, CleanupAWSGuestNetwork(cleanupCtx, api, recovered))
	}
	boundary.SecurityGroupID = aws.ToString(created.GroupId)
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			retErr = errors.Join(retErr, CleanupAWSGuestNetwork(cleanupCtx, api, boundary))
		}
	}()
	if persist != nil {
		if err := persist(boundary); err != nil {
			return boundary, fmt.Errorf("persist created guest network cleanup custody: %w", err)
		}
	}
	group, err := awsGuestGroup(ctx, api, boundary)
	if err != nil {
		return boundary, err
	}
	if len(group.IpPermissions) != 0 {
		return boundary, errors.New("new AWS guest boundary has unexpected ingress")
	}
	if len(group.IpPermissionsEgress) > 0 {
		if _, err := api.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String(boundary.SecurityGroupID), IpPermissions: group.IpPermissionsEgress}); err != nil {
			return boundary, fmt.Errorf("remove AWS guest default egress: %w", err)
		}
	}
	if len(ingress) > 0 {
		if _, err := api.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(boundary.SecurityGroupID), IpPermissions: ingress}); err != nil {
			return boundary, fmt.Errorf("install AWS guest ingress: %w", err)
		}
	}
	if len(egress) > 0 {
		if _, err := api.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String(boundary.SecurityGroupID), IpPermissions: egress}); err != nil {
			return boundary, fmt.Errorf("install AWS guest egress: %w", err)
		}
	}
	if err := awsGuestEffectivePolicy(ctx, api, boundary, ingress, egress); err != nil {
		return boundary, err
	}
	return boundary, nil
}

// InspectAWSGuestNetwork re-observes both the boundary and the actual EC2
// instance, not launch-request metadata or an SG/subnet assertion from a caller.
func InspectAWSGuestNetwork(ctx context.Context, api AWSGuestNetworkAPI, boundary AWSGuestNetworkBoundary, resourceID string, policy GuestNetworkPolicy) (GuestNetworkReceipt, error) {
	var receipt GuestNetworkReceipt
	digest, ingress, egress, err := awsGuestPolicy(policy)
	if err != nil {
		return receipt, err
	}
	if api == nil || digest != boundary.PolicyDigest || !validAWSGuestID(resourceID, "i") {
		return receipt, errors.New("AWS guest boundary policy or resource binding mismatch")
	}
	if err := awsGuestEffectivePolicy(ctx, api, boundary, ingress, egress); err != nil {
		return receipt, err
	}
	observed, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{resourceID}})
	if err != nil {
		return receipt, fmt.Errorf("inspect AWS guest instance: %w", err)
	}
	if observed == nil || observed.NextToken != nil || len(observed.Reservations) != 1 || len(observed.Reservations[0].Instances) != 1 {
		return receipt, errors.New("AWS guest instance observation is absent or ambiguous")
	}
	instance := observed.Reservations[0].Instances[0]
	if aws.ToString(instance.InstanceId) != resourceID || aws.ToString(instance.SubnetId) != boundary.SubnetID || aws.ToString(instance.VpcId) != boundary.VPCID || !awsGuestOnlyGroup(instance.SecurityGroups, boundary.SecurityGroupID) || len(instance.NetworkInterfaces) != 1 || !aws.ToBool(instance.SourceDestCheck) || !awsGuestInstanceLease(instance.Tags, boundary.LeaseID) {
		return receipt, errors.New("AWS guest instance network binding mismatch")
	}
	if instance.State == nil || (instance.State.Name != types.InstanceStateNamePending && instance.State.Name != types.InstanceStateNameRunning) || instance.IamInstanceProfile != nil || instance.MetadataOptions == nil || instance.MetadataOptions.State != types.InstanceMetadataOptionsStateApplied {
		return receipt, errors.New("AWS guest instance identity endpoint or lifecycle is not isolated")
	}
	bootstrapFacts, err := observeAWSGuestBootstrap(ctx, api, boundary, policy, instance)
	if err != nil {
		return receipt, err
	}
	nic := instance.NetworkInterfaces[0]
	if !validAWSGuestID(aws.ToString(nic.NetworkInterfaceId), "eni") || aws.ToString(nic.SubnetId) != boundary.SubnetID || aws.ToString(nic.VpcId) != boundary.VPCID || !awsGuestOnlyGroup(nic.Groups, boundary.SecurityGroupID) || len(nic.Ipv6Addresses) != 0 || !aws.ToBool(nic.SourceDestCheck) {
		return receipt, errors.New("AWS guest interface network binding mismatch")
	}
	// Re-read after instance inspection too: never publish a cached pre-launch SG
	// observation as the actual instance's current policy. Concurrent operator
	// changes remain outside this bounded receipt and require ongoing inspection.
	if err := awsGuestEffectivePolicy(ctx, api, boundary, ingress, egress); err != nil {
		return receipt, err
	}
	receipt = GuestNetworkReceipt{BootstrapMode: policy.BootstrapMode, Version: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, PolicyDigest: digest, LeaseID: boundary.LeaseID, Provider: "aws", ResourceID: resourceID, BoundaryID: boundary.SecurityGroupID, Facts: map[string]string{
		"subnetId": boundary.SubnetID, "vpcId": boundary.VPCID, "securityGroupId": boundary.SecurityGroupID, "networkInterfaceId": aws.ToString(nic.NetworkInterfaceId),
		"platformExceptionsPolicy": AWSGuestNetworkPlatformServices,
		"imds":                     "disabled", "instanceProfile": "none", "ipv6": "absent", "egress": "operator-public-ipv4-tcp-allowlist", "amazonProvidedDNS": "aws-resolver-exception", "awsPlatformExceptions": "dns,dhcp,time-sync,vpc-router,windows-license-activation,ecs-task-metadata",
	}}
	for key, value := range bootstrapFacts {
		receipt.Facts[key] = value
	}
	return receipt, nil
}

// CleanupAWSGuestNetwork deletes only a boundary with matching provider-observed
// ownership and no remaining ENIs. In particular it never deletes a shared SG or
// tears down networking under an attached (even stopped) instance.
func CleanupAWSGuestNetwork(ctx context.Context, api AWSGuestNetworkAPI, boundary AWSGuestNetworkBoundary) error {
	if boundary.CreateRejectedNoEffect {
		if !AWSGuestNetworkCreateRejectedNoEffect(boundary) {
			return errors.New("invalid definitive guest network rejection custody")
		}
		return nil
	}
	if api == nil {
		return errors.New("AWS guest cleanup requires a control-plane client")
	}
	if _, err := awsGuestGroup(ctx, api, boundary); err != nil {
		var apiErr smithy.APIError
		if validAWSGuestID(boundary.SecurityGroupID, "sg") && errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidGroup.NotFound" {
			return nil
		}
		return err
	}
	interfaces, err := api.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{Filters: []types.Filter{{Name: aws.String("group-id"), Values: []string{boundary.SecurityGroupID}}}})
	if err != nil {
		return fmt.Errorf("inspect AWS guest boundary attachments before cleanup: %w", err)
	}
	if interfaces == nil || aws.ToString(interfaces.NextToken) != "" || len(interfaces.NetworkInterfaces) != 0 {
		return errors.New("AWS guest boundary still has attachments or an incomplete attachment observation")
	}
	if _, err := api.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(boundary.SecurityGroupID)}); err != nil {
		return fmt.Errorf("delete AWS guest boundary: %w", err)
	}
	// Success is confirmed by a separate authoritative absence read.
	_, err = awsGuestGroup(ctx, api, boundary)
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidGroup.NotFound" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("confirm AWS guest boundary deletion: %w", err)
	}
	return errors.New("AWS guest boundary remains present after deletion")
}

func awsGuestTags(boundary AWSGuestNetworkBoundary) []types.Tag {
	return []types.Tag{{Key: aws.String(awsGuestOwnerTag), Value: aws.String("v1")}, {Key: aws.String(awsGuestLeaseTag), Value: aws.String(boundary.LeaseID)}, {Key: aws.String(awsGuestPolicyTag), Value: aws.String(boundary.PolicyDigest)}}
}

func awsGuestGroup(ctx context.Context, api AWSGuestNetworkAPI, boundary AWSGuestNetworkBoundary) (types.SecurityGroup, error) {
	var zero types.SecurityGroup
	if boundary.LeaseID == "" || len(boundary.PolicyDigest) != 64 || !validAWSGuestID(boundary.SecurityGroupID, "sg") || !validAWSGuestID(boundary.SubnetID, "subnet") || !validAWSGuestID(boundary.VPCID, "vpc") {
		return zero, errors.New("AWS guest boundary cleanup/inspection handle is incomplete")
	}
	response, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{boundary.SecurityGroupID}})
	if err != nil {
		return zero, fmt.Errorf("inspect AWS guest security group: %w", err)
	}
	if response == nil || aws.ToString(response.NextToken) != "" || len(response.SecurityGroups) != 1 {
		return zero, errors.New("AWS guest security group observation is absent or ambiguous")
	}
	group := response.SecurityGroups[0]
	if aws.ToString(group.GroupId) != boundary.SecurityGroupID || aws.ToString(group.VpcId) != boundary.VPCID || aws.ToString(group.GroupName) != awsGuestGroupName(boundary) {
		return zero, errors.New("AWS guest security group identity mismatch")
	}
	for _, expected := range awsGuestTags(boundary) {
		matched := 0
		for _, actual := range group.Tags {
			if aws.ToString(actual.Key) == aws.ToString(expected.Key) {
				if aws.ToString(actual.Value) != aws.ToString(expected.Value) {
					return zero, errors.New("AWS guest security group ownership mismatch")
				}
				matched++
			}
		}
		if matched != 1 {
			return zero, errors.New("AWS guest security group ownership is missing or ambiguous")
		}
	}
	return group, nil
}

func awsGuestEffectivePolicy(ctx context.Context, api AWSGuestNetworkAPI, boundary AWSGuestNetworkBoundary, ingress, egress []types.IpPermission) error {
	group, err := awsGuestGroup(ctx, api, boundary)
	if err != nil {
		return err
	}
	for _, pair := range [][2][]types.IpPermission{{group.IpPermissions, ingress}, {group.IpPermissionsEgress, egress}} {
		actual, err := awsGuestPermissionKeys(pair[0])
		if err != nil {
			return err
		}
		expected, err := awsGuestPermissionKeys(pair[1])
		if err != nil || !slices.Equal(actual, expected) {
			return errors.New("AWS guest effective network policy does not match operator policy")
		}
	}
	return nil
}

func awsGuestOnlyGroup(groups []types.GroupIdentifier, id string) bool {
	return len(groups) == 1 && aws.ToString(groups[0].GroupId) == id
}

func awsGuestInstanceLease(tags []types.Tag, leaseID string) bool {
	leaseMatches, managedMatches := 0, 0
	for _, tag := range tags {
		switch aws.ToString(tag.Key) {
		case "lease":
			if aws.ToString(tag.Value) != leaseID {
				return false
			}
			leaseMatches++
		case "crabbox":
			if aws.ToString(tag.Value) != "true" {
				return false
			}
			managedMatches++
		}
	}
	return leaseMatches == 1 && managedMatches == 1
}

func awsGuestPermissionKeys(permissions []types.IpPermission) ([]string, error) {
	var keys []string
	for _, permission := range permissions {
		if aws.ToString(permission.IpProtocol) != "tcp" || permission.FromPort == nil || permission.ToPort == nil || *permission.FromPort != *permission.ToPort || *permission.FromPort < 1 || *permission.FromPort > 65535 || len(permission.Ipv6Ranges) != 0 || len(permission.PrefixListIds) != 0 || len(permission.UserIdGroupPairs) != 0 || len(permission.IpRanges) == 0 {
			return nil, errors.New("AWS guest effective policy contains an unsupported or unrestricted rule")
		}
		for _, cidr := range permission.IpRanges {
			prefix, err := netip.ParsePrefix(aws.ToString(cidr.CidrIp))
			if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() {
				return nil, errors.New("AWS guest effective policy contains an invalid IPv4 range")
			}
			keys = append(keys, prefix.String()+":"+strconv.Itoa(int(*permission.FromPort)))
		}
	}
	slices.Sort(keys)
	return keys, nil
}

func awsGuestPolicy(policy GuestNetworkPolicy) (string, []types.IpPermission, []types.IpPermission, error) {
	if err := awsGuestBootstrapPolicy(policy); err != nil {
		return "", nil, nil, err
	}
	digest, err := GuestNetworkPolicyDigest(policy)
	if err != nil {
		return "", nil, nil, err
	}
	var ingress, egress []types.IpPermission
	permission := func(cidr string, port int) types.IpPermission {
		return types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(int32(port)), ToPort: aws.Int32(int32(port)), IpRanges: []types.IpRange{{CidrIp: aws.String(cidr)}}}
	}
	for _, source := range policy.SSHSources {
		prefix, err := netip.ParsePrefix(source)
		if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() == 0 {
			return "", nil, nil, errors.New("AWS guest SSH source must be a canonical non-wildcard IPv4 CIDR")
		}
		ingress = append(ingress, permission(source, 22))
	}
	for _, rule := range policy.Egress {
		prefix, err := netip.ParsePrefix(rule.CIDR)
		if err != nil || !awsGuestPublicIPv4(prefix) || len(rule.Ports) == 0 {
			return "", nil, nil, errors.New("AWS guest egress must use canonical public IPv4 CIDRs and explicit TCP ports")
		}
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return "", nil, nil, errors.New("AWS guest egress TCP port is out of range")
			}
			egress = append(egress, permission(prefix.String(), port))
		}
	}
	if len(ingress) > 50 || len(egress) > 50 {
		return "", nil, nil, errors.New("AWS guest policy exceeds the bounded security-group rule budget")
	}
	return digest, ingress, egress, nil
}

func awsGuestPublicIPv4(prefix netip.Prefix) bool {
	if !prefix.IsValid() || !prefix.Addr().Is4() || prefix != prefix.Masked() {
		return false
	}
	// Reject any overlap, not just the first address of a large allow range.
	for _, reserved := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"} {
		blocked := netip.MustParsePrefix(reserved)
		if prefix.Overlaps(blocked) {
			return false
		}
	}
	return true
}

// ResolveAWSGuestNetworkBoundary recovers an uncertain create's exact cleanup
// handle by deterministic name AND provider-observed ownership. An absent read
// is not settlement: retain this handle rather than submit another create.
func ResolveAWSGuestNetworkBoundary(ctx context.Context, api AWSGuestNetworkAPI, boundary AWSGuestNetworkBoundary) (AWSGuestNetworkBoundary, error) {
	if boundary.CreateRejectedNoEffect {
		if !AWSGuestNetworkCreateRejectedNoEffect(boundary) {
			return boundary, errors.New("invalid definitive guest network rejection custody")
		}
		return boundary, nil
	}
	if api == nil || boundary.LeaseID == "" || len(boundary.PolicyDigest) != 64 || !validAWSGuestID(boundary.SubnetID, "subnet") || !validAWSGuestID(boundary.VPCID, "vpc") || boundary.SecurityGroupName != awsGuestGroupName(boundary) {
		return boundary, errors.New("AWS guest boundary recovery handle is incomplete")
	}
	if boundary.SecurityGroupID != "" {
		_, err := awsGuestGroup(ctx, api, boundary)
		return boundary, err
	}
	observed, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []types.Filter{{Name: aws.String("group-name"), Values: []string{boundary.SecurityGroupName}}, {Name: aws.String("vpc-id"), Values: []string{boundary.VPCID}}}})
	if err != nil {
		return boundary, fmt.Errorf("recover AWS guest boundary: %w", err)
	}
	if observed == nil || aws.ToString(observed.NextToken) != "" || len(observed.SecurityGroups) != 1 || !validAWSGuestID(aws.ToString(observed.SecurityGroups[0].GroupId), "sg") {
		return boundary, errors.New("AWS guest boundary creation outcome is unresolved; retain the cleanup handle")
	}
	boundary.SecurityGroupID = aws.ToString(observed.SecurityGroups[0].GroupId)
	_, err = awsGuestGroup(ctx, api, boundary)
	return boundary, err
}

func (c *AWSClient) ResolveGuestNetworkBoundary(ctx context.Context, boundary AWSGuestNetworkBoundary) (AWSGuestNetworkBoundary, error) {
	return ResolveAWSGuestNetworkBoundary(ctx, c.ec2, boundary)
}

func awsGuestGroupName(boundary AWSGuestNetworkBoundary) string {
	nameHash := sha256.Sum256([]byte(boundary.LeaseID + "\x00" + boundary.SubnetID + "\x00" + boundary.PolicyDigest))
	return "crabbox-guest-" + hex.EncodeToString(nameHash[:12])
}

// Only an explicit provider API rejection, never an HTTP class or message match.
// CreateSecurityGroup is sent with SDK retries disabled: a later rejection cannot
// erase an earlier ambiguous SDK attempt.
func awsGuestCreateRejectedNoEffect(err error) bool {
	var operation *smithy.OperationError
	if errors.As(err, &operation) && operation.OperationName != "CreateSecurityGroup" {
		return false
	}
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) && response.HTTPStatusCode() != 403 {
		return false
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "UnauthorizedOperation" && apiErr.ErrorFault() != smithy.FaultServer
}
func AWSGuestNetworkCreateRejectedNoEffect(b AWSGuestNetworkBoundary) bool {
	return b.CreateRejectedNoEffect && b.CreateAttemptID != "" && b.SecurityGroupID == "" && b.LeaseID != "" && b.PolicyDigest != "" && b.SecurityGroupName != "" && validAWSGuestID(b.SubnetID, "subnet") && validAWSGuestID(b.VPCID, "vpc")
}
