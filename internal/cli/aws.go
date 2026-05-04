package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const awsUbuntuOwner = "099720109477"

type AWSClient struct {
	ec2    *ec2.Client
	region string
}

func newAWSClient(ctx context.Context, cfg Config) (*AWSClient, error) {
	if cfg.AWSRegion == "" {
		return nil, exit(3, "CRABBOX_AWS_REGION or AWS_REGION is required")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, err
	}
	return &AWSClient{ec2: ec2.NewFromConfig(awsCfg), region: cfg.AWSRegion}, nil
}

func (c *AWSClient) SpotPlacementScores(ctx context.Context, cfg Config) ([]types.SpotPlacementScore, error) {
	regions := cfg.Capacity.Regions
	if len(regions) == 0 && cfg.AWSRegion != "" {
		regions = []string{cfg.AWSRegion}
	}
	if len(regions) == 0 {
		return nil, nil
	}
	candidates := awsInstanceTypeCandidatesForClass(cfg.Class)
	if cfg.ServerType != "" {
		candidates = appendUniqueStrings([]string{cfg.ServerType}, candidates...)
	}
	target := int32(1)
	out, err := c.ec2.GetSpotPlacementScores(ctx, &ec2.GetSpotPlacementScoresInput{
		InstanceTypes:          candidates,
		RegionNames:            regions,
		TargetCapacity:         aws.Int32(target),
		TargetCapacityUnitType: types.TargetCapacityUnitTypeUnits,
	})
	if err != nil {
		return nil, err
	}
	scores := append([]types.SpotPlacementScore(nil), out.SpotPlacementScores...)
	sort.Slice(scores, func(i, j int) bool {
		left := int32(0)
		right := int32(0)
		if scores[i].Score != nil {
			left = *scores[i].Score
		}
		if scores[j].Score != nil {
			right = *scores[j].Score
		}
		if left == right {
			return aws.ToString(scores[i].Region) < aws.ToString(scores[j].Region)
		}
		return left > right
	})
	return scores, nil
}

func (c *AWSClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:crabbox"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0)
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			servers = append(servers, awsInstanceToServer(instance))
		}
	}
	return servers, nil
}

func (c *AWSClient) EnsureSSHKey(ctx context.Context, name, publicKey string) error {
	_, err := c.ec2.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
		KeyNames: []string{name},
	})
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "InvalidKeyPair.NotFound") {
		return err
	}
	_, err = c.ec2.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName:           aws.String(name),
		PublicKeyMaterial: []byte(publicKey),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeKeyPair,
				Tags:         awsTags(map[string]string{"crabbox": "true", "created_by": "crabbox"}),
			},
		},
	})
	return err
}

func (c *AWSClient) DeleteSSHKey(ctx context.Context, name string) error {
	_, err := c.ec2.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyName: aws.String(name)})
	if err != nil && strings.Contains(err.Error(), "InvalidKeyPair.NotFound") {
		return nil
	}
	return err
}

func (c *AWSClient) CreateServerWithFallback(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (Server, Config, error) {
	if cfg.ProviderKey == "" {
		cfg.ProviderKey = "crabbox-steipete"
	}
	if err := c.EnsureSSHKey(ctx, cfg.ProviderKey, publicKey); err != nil {
		return Server{}, cfg, err
	}
	imageID, err := c.resolveAMI(ctx, cfg)
	if err != nil {
		return Server{}, cfg, err
	}
	securityGroupID, err := c.ensureSecurityGroup(ctx, cfg)
	if err != nil {
		return Server{}, cfg, err
	}
	candidates := awsLaunchCandidates(cfg)
	useSpot := cfg.Capacity.Market != "on-demand"
	var errs []error
	for i, instanceType := range candidates {
		next := cfg
		next.ServerType = instanceType
		if i > 0 && logf != nil {
			logf("fallback provisioning type=%s after capacity/quota rejection\n", instanceType)
		}
		server, err := c.createServer(ctx, next, publicKey, leaseID, slug, keep, imageID, securityGroupID, useSpot)
		if err == nil {
			return server, next, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", instanceType, err))
		if !isRetryableAWSProvisioningError(err) {
			return Server{}, next, joinErrors(errs)
		}
	}
	if useSpot && strings.HasPrefix(cfg.Capacity.Fallback, "on-demand") {
		for _, instanceType := range candidates {
			next := cfg
			next.ServerType = instanceType
			if logf != nil {
				logf("fallback provisioning type=%s market=on-demand after spot rejection\n", instanceType)
			}
			server, err := c.createServer(ctx, next, publicKey, leaseID, slug, keep, imageID, securityGroupID, false)
			if err == nil {
				return server, next, nil
			}
			errs = append(errs, fmt.Errorf("on-demand %s: %w", instanceType, err))
			if !isRetryableAWSProvisioningError(err) {
				return Server{}, next, joinErrors(errs)
			}
		}
	}
	if cfg.ServerTypeExplicit {
		return Server{}, cfg, fmt.Errorf("requested exact AWS instance type %s failed; remove --type to allow class fallback: %w", cfg.ServerType, joinErrors(errs))
	}
	return Server{}, cfg, joinErrors(errs)
}

func (c *AWSClient) createServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, imageID, securityGroupID string, spot bool) (Server, error) {
	_ = publicKey
	name := leaseProviderName(leaseID, slug)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = renderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := time.Now().UTC()
	labels := directLeaseLabels(cfg, leaseID, slug, "aws", mapMarket(spot), keep, now)
	userData := base64.StdEncoding.EncodeToString([]byte(awsUserData(cfg, publicKey)))
	rootGB := cfg.AWSRootGB
	if rootGB <= 0 {
		rootGB = 400
	}
	one := int32(1)
	rootDevice := "/dev/sda1"
	tagSpecifications := []types.TagSpecification{
		{ResourceType: types.ResourceTypeInstance, Tags: awsTagsWithName(labels, name)},
		{ResourceType: types.ResourceTypeVolume, Tags: awsTagsWithName(labels, name)},
	}
	if spot {
		tagSpecifications = append(tagSpecifications, types.TagSpecification{ResourceType: types.ResourceTypeSpotInstancesRequest, Tags: awsTagsWithName(labels, name)})
	}
	input := &ec2.RunInstancesInput{
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{
				DeviceName: aws.String(rootDevice),
				Ebs: &types.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(true),
					Encrypted:           aws.Bool(true),
					VolumeSize:          aws.Int32(rootGB),
					VolumeType:          types.VolumeTypeGp3,
				},
			},
		},
		ClientToken:       aws.String(leaseID),
		ImageId:           aws.String(imageID),
		InstanceType:      types.InstanceType(cfg.ServerType),
		KeyName:           aws.String(cfg.ProviderKey),
		MaxCount:          aws.Int32(one),
		MinCount:          aws.Int32(one),
		SecurityGroupIds:  []string{securityGroupID},
		TagSpecifications: tagSpecifications,
		UserData:          aws.String(userData),
	}
	if spot {
		input.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
				SpotInstanceType:             types.SpotInstanceTypeOneTime,
			},
		}
	}
	if cfg.AWSProfile != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{Name: aws.String(cfg.AWSProfile)}
	}
	if cfg.AWSSubnetID != "" {
		input.SecurityGroupIds = nil
		input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{
			{
				AssociatePublicIpAddress: aws.Bool(true),
				DeleteOnTermination:      aws.Bool(true),
				DeviceIndex:              aws.Int32(0),
				Groups:                   []string{securityGroupID},
				SubnetId:                 aws.String(cfg.AWSSubnetID),
			},
		}
	}
	if cfg.TargetOS == targetMacOS {
		input.Placement = &types.Placement{HostId: aws.String(cfg.AWSMacHostID), Tenancy: types.TenancyHost}
	}
	out, err := c.ec2.RunInstances(ctx, input)
	if err != nil {
		return Server{}, err
	}
	if len(out.Instances) == 0 {
		return Server{}, exit(5, "aws returned no instances")
	}
	return awsInstanceToServer(out.Instances[0]), nil
}

func mapMarket(spot bool) string {
	if spot {
		return "spot"
	}
	return "on-demand"
}

func (c *AWSClient) waitForServerIP(ctx context.Context, id string) (Server, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		server, err := c.GetServer(ctx, id)
		if err != nil {
			return Server{}, err
		}
		if server.PublicNet.IPv4.IP != "" {
			return server, nil
		}
		if time.Now().After(deadline) {
			return Server{}, exit(5, "timed out waiting for AWS instance public IP")
		}
		time.Sleep(5 * time.Second)
	}
}

func (c *AWSClient) GetServer(ctx context.Context, id string) (Server, error) {
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		return Server{}, err
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			return awsInstanceToServer(instance), nil
		}
	}
	return Server{}, exit(4, "aws instance not found: %s", id)
}

func (c *AWSClient) DeleteServer(ctx context.Context, id string) error {
	_, err := c.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{id},
	})
	return err
}

func (c *AWSClient) SetTags(ctx context.Context, id string, labels map[string]string) error {
	_, err := c.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{id},
		Tags:      awsTags(labels),
	})
	return err
}

func (c *AWSClient) resolveAMI(ctx context.Context, cfg Config) (string, error) {
	if cfg.AWSAMI != "" {
		return cfg.AWSAMI, nil
	}
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
		return c.resolveLatestAmazonAMI(ctx, "Windows_Server-2022-English-Full-Base-*", "x86_64")
	}
	if cfg.TargetOS == targetMacOS {
		if strings.HasPrefix(cfg.ServerType, "mac1.") {
			return c.resolveLatestAmazonAMI(ctx, "amzn-ec2-macos-14.*", "x86_64_mac")
		}
		return c.resolveLatestAmazonAMI(ctx, "amzn-ec2-macos-14.*-arm64", "arm64_mac")
	}
	out, err := c.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{awsUbuntuOwner},
		Filters: []types.Filter{
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("name"), Values: []string{"ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"}},
			{Name: aws.String("root-device-type"), Values: []string{"ebs"}},
			{Name: aws.String("virtualization-type"), Values: []string{"hvm"}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.Images) == 0 {
		return "", exit(3, "no Ubuntu 24.04 x86_64 AMI found in %s; set CRABBOX_AWS_AMI", cfg.AWSRegion)
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.ToString(out.Images[i].CreationDate) > aws.ToString(out.Images[j].CreationDate)
	})
	return aws.ToString(out.Images[0].ImageId), nil
}

func (c *AWSClient) resolveLatestAmazonAMI(ctx context.Context, name, architecture string) (string, error) {
	out, err := c.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"amazon"},
		Filters: []types.Filter{
			{Name: aws.String("architecture"), Values: []string{architecture}},
			{Name: aws.String("name"), Values: []string{name}},
			{Name: aws.String("root-device-type"), Values: []string{"ebs"}},
			{Name: aws.String("virtualization-type"), Values: []string{"hvm"}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.Images) == 0 {
		return "", exit(3, "no AWS AMI found in %s for name=%s architecture=%s; set CRABBOX_AWS_AMI", c.region, name, architecture)
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.ToString(out.Images[i].CreationDate) > aws.ToString(out.Images[j].CreationDate)
	})
	return aws.ToString(out.Images[0].ImageId), nil
}

func (c *AWSClient) ensureSecurityGroup(ctx context.Context, cfg Config) (string, error) {
	if cfg.AWSSGID != "" {
		return cfg.AWSSGID, nil
	}
	vpcID, err := c.securityGroupVPC(ctx, cfg)
	if err != nil {
		return "", err
	}
	const name = "crabbox-runners"
	existing, err := c.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("group-name"), Values: []string{name}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", err
	}
	var groupID string
	if len(existing.SecurityGroups) > 0 {
		groupID = aws.ToString(existing.SecurityGroups[0].GroupId)
	} else {
		created, err := c.ec2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			Description: aws.String("Crabbox ephemeral test runners"),
			GroupName:   aws.String(name),
			TagSpecifications: []types.TagSpecification{
				{ResourceType: types.ResourceTypeSecurityGroup, Tags: awsTags(map[string]string{"Name": name, "crabbox": "true", "created_by": "crabbox"})},
			},
			VpcId: aws.String(vpcID),
		})
		if err != nil {
			return "", err
		}
		groupID = aws.ToString(created.GroupId)
	}
	for _, port := range sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts) {
		if err := c.allowTCP(ctx, groupID, port, cfg.AWSSSHCIDRs); err != nil && !strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
			return "", err
		}
	}
	return groupID, nil
}

func (c *AWSClient) defaultVPC(ctx context.Context) (string, error) {
	out, err := c.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{{Name: aws.String("is-default"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", err
	}
	if len(out.Vpcs) == 0 {
		return "", exit(3, "no default VPC found; set CRABBOX_AWS_SUBNET_ID and CRABBOX_AWS_SECURITY_GROUP_ID")
	}
	return aws.ToString(out.Vpcs[0].VpcId), nil
}

func (c *AWSClient) securityGroupVPC(ctx context.Context, cfg Config) (string, error) {
	if cfg.AWSSubnetID == "" {
		return c.defaultVPC(ctx)
	}
	out, err := c.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: []string{cfg.AWSSubnetID},
	})
	if err != nil {
		return "", err
	}
	if len(out.Subnets) == 0 {
		return "", exit(3, "AWS subnet not found: %s", cfg.AWSSubnetID)
	}
	return aws.ToString(out.Subnets[0].VpcId), nil
}

func (c *AWSClient) allowTCP(ctx context.Context, groupID, port string, cidrs []string) error {
	p, ok := parsePort32(port)
	if !ok {
		return exit(2, "invalid SSH port: %s", port)
	}
	ranges := make([]types.IpRange, 0, len(cidrs))
	for _, cidr := range cidrs {
		if strings.TrimSpace(cidr) != "" {
			ranges = append(ranges, types.IpRange{CidrIp: aws.String(cidr), Description: aws.String("Crabbox SSH")})
		}
	}
	if len(ranges) == 0 {
		ranges = append(ranges, types.IpRange{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("Crabbox SSH")})
	}
	_, err := c.ec2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(groupID),
		IpPermissions: []types.IpPermission{
			{
				FromPort:   aws.Int32(p),
				IpProtocol: aws.String("tcp"),
				IpRanges:   ranges,
				ToPort:     aws.Int32(p),
			},
		},
	})
	return err
}

func awsInstanceToServer(instance types.Instance) Server {
	labels := make(map[string]string)
	name := aws.ToString(instance.InstanceId)
	for _, tag := range instance.Tags {
		key := aws.ToString(tag.Key)
		value := aws.ToString(tag.Value)
		labels[key] = value
		if key == "Name" && value != "" {
			name = value
		}
	}
	server := Server{
		CloudID:  aws.ToString(instance.InstanceId),
		Provider: "aws",
		Name:     name,
		Status:   string(instance.State.Name),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = aws.ToString(instance.PublicIpAddress)
	server.ServerType.Name = string(instance.InstanceType)
	return server
}

func awsString(value *string) string {
	return aws.ToString(value)
}

func awsTags(labels map[string]string) []types.Tag {
	tags := make([]types.Tag, 0, len(labels))
	for key, value := range labels {
		tags = append(tags, types.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	sort.Slice(tags, func(i, j int) bool {
		return aws.ToString(tags[i].Key) < aws.ToString(tags[j].Key)
	})
	return tags
}

func awsTagsWithName(labels map[string]string, name string) []types.Tag {
	next := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		next[key] = value
	}
	next["Name"] = name
	return awsTags(next)
}

func isRetryableAWSProvisioningError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "InsufficientInstanceCapacity") ||
		strings.Contains(s, "MaxSpotInstanceCountExceeded") ||
		strings.Contains(s, "VcpuLimitExceeded") ||
		strings.Contains(s, "Unsupported") ||
		strings.Contains(s, "InvalidParameterValue") ||
		(strings.Contains(s, "InvalidParameterCombination") &&
			(strings.Contains(s, "Free Tier") ||
				strings.Contains(s, "eligible") ||
				strings.Contains(s, "InstanceType") ||
				strings.Contains(s, "instance type")))
}

func awsLaunchCandidates(cfg Config) []string {
	if cfg.ServerTypeExplicit {
		return []string{cfg.ServerType}
	}
	if cfg.TargetOS == targetMacOS {
		return appendUniqueStrings([]string{cfg.ServerType}, awsInstanceTypeCandidatesForTargetClass(cfg.TargetOS, cfg.Class)...)
	}
	fallback := "t3.small"
	if cfg.TargetOS == targetWindows {
		fallback = "t3.large"
	}
	return appendUniqueStrings([]string{cfg.ServerType}, append(awsInstanceTypeCandidatesForTargetClass(cfg.TargetOS, cfg.Class), fallback)...)
}

func parsePort32(port string) (int32, bool) {
	n, err := strconv.ParseInt(port, 10, 32)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return int32(n), true
}
