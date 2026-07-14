package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	awsUbuntuOwner           = "099720109477"
	awsSSHIngressDescription = "Crabbox SSH"
	awsSpotQuotaCode         = "L-34B43A08"
	awsOnDemandQuotaCode     = "L-1216C47A"
)

var awsSnapshotDeleteBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

type AWSClient struct {
	ec2           *ec2.Client
	serviceQuotas *servicequotas.Client
	sts           *sts.Client
	region        string
}

func newAWSClient(ctx context.Context, cfg Config) (*AWSClient, error) {
	if cfg.AWSRegion == "" {
		return nil, exit(3, "CRABBOX_AWS_REGION or AWS_REGION is required")
	}
	return newAWSClientForRegion(ctx, cfg, cfg.AWSRegion)
}

func newAWSClientForRegion(ctx context.Context, cfg Config, region string) (*AWSClient, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &AWSClient{
		ec2:           ec2.NewFromConfig(awsCfg),
		serviceQuotas: servicequotas.NewFromConfig(awsCfg),
		sts:           sts.NewFromConfig(awsCfg),
		region:        region,
	}, nil
}

func NewAWSClient(ctx context.Context, cfg Config) (*AWSClient, error) {
	return newAWSClient(ctx, cfg)
}

func (c *AWSClient) CapacityDoctorChecks(ctx context.Context, cfg Config) []DoctorCheck {
	if cfg.TargetOS == targetMacOS || c.serviceQuotas == nil {
		return nil
	}
	if cfg.Provider == "" {
		cfg.Provider = "aws"
	}
	if cfg.ServerType == "" {
		cfg.ServerType = serverTypeForConfig(cfg)
	}
	checks := make([]DoctorCheck, 0, 2)
	for _, market := range awsCapacityDoctorMarkets(cfg) {
		limit, known, err := c.appliedEC2ServiceQuota(ctx, awsQuotaCodeForMarket(market))
		checks = append(checks, awsCapacityDoctorCheckForQuota(cfg, market, limit, known, err))
	}
	return checks
}

func (c *AWSClient) appliedEC2ServiceQuota(ctx context.Context, quotaCode string) (float64, bool, error) {
	out, err := c.serviceQuotas.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
		ServiceCode: aws.String("ec2"),
		QuotaCode:   aws.String(quotaCode),
	})
	if err != nil {
		return 0, false, err
	}
	if out.Quota == nil || out.Quota.Value == nil {
		return 0, false, nil
	}
	return *out.Quota.Value, true, nil
}

func (c *AWSClient) SpotPlacementScores(ctx context.Context, cfg Config) ([]types.SpotPlacementScore, error) {
	regions := cfg.Capacity.Regions
	if len(regions) == 0 && cfg.AWSRegion != "" {
		regions = []string{cfg.AWSRegion}
	}
	if len(regions) == 0 {
		return nil, nil
	}
	candidates := awsInstanceTypeCandidatesForConfig(cfg)
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

type awsKeyPairBinding struct {
	ID      string
	Managed bool
	Created bool
}

func (c *AWSClient) ensureSSHKeyBinding(ctx context.Context, name, publicKey string) (awsKeyPairBinding, error) {
	out, err := c.ec2.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
		KeyNames:         []string{name},
		IncludePublicKey: aws.Bool(true),
	})
	if err == nil {
		if err := verifyAWSKeyPairMatches(name, publicKey, out.KeyPairs); err != nil {
			return awsKeyPairBinding{}, err
		}
		keyPairID, ownershipErr := validateAWSCleanupKeyPair(name, out.KeyPairs)
		if ownershipErr != nil {
			if IsAWSCleanupKeyOwnershipError(ownershipErr) {
				return awsKeyPairBinding{}, nil
			}
			return awsKeyPairBinding{}, ownershipErr
		}
		return awsKeyPairBinding{ID: keyPairID, Managed: true}, nil
	}
	if !strings.Contains(err.Error(), "InvalidKeyPair.NotFound") {
		return awsKeyPairBinding{}, err
	}
	created, err := c.ec2.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName:           aws.String(name),
		PublicKeyMaterial: []byte(publicKey),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeKeyPair,
				Tags:         awsTags(map[string]string{"crabbox": "true", "created_by": "crabbox"}),
			},
		},
	})
	if err != nil {
		return awsKeyPairBinding{}, err
	}
	keyPairID := strings.TrimSpace(aws.ToString(created.KeyPairId))
	if keyPairID == "" {
		return awsKeyPairBinding{}, exit(5, "aws imported key pair %q without an immutable key pair id", name)
	}
	return awsKeyPairBinding{ID: keyPairID, Managed: true, Created: true}, nil
}

func (c *AWSClient) EnsureSSHKey(ctx context.Context, name, publicKey string) error {
	_, err := c.ensureSSHKeyBinding(ctx, name, publicKey)
	return err
}

func verifyAWSKeyPairMatches(name, publicKey string, keyPairs []types.KeyPairInfo) error {
	if len(keyPairs) == 0 {
		return exit(2, "aws key pair %q exists but DescribeKeyPairs returned no key material", name)
	}
	existing := keyPairs[0]
	if sameOpenSSHPublicKey(aws.ToString(existing.PublicKey), publicKey) {
		return nil
	}
	fingerprints, err := awsImportedPublicKeyFingerprints(publicKey)
	if err != nil {
		return err
	}
	if awsKeyFingerprintMatches(aws.ToString(existing.KeyFingerprint), fingerprints) {
		return nil
	}
	if existing.PublicKey != nil && strings.TrimSpace(aws.ToString(existing.PublicKey)) != "" {
		return exit(2, "aws key pair %q already exists with different public key; delete it or configure a unique provider key", name)
	}
	if existing.KeyFingerprint == nil || strings.TrimSpace(aws.ToString(existing.KeyFingerprint)) == "" {
		return exit(2, "aws key pair %q already exists but Crabbox cannot verify its public key; delete it or configure a unique provider key", name)
	}
	return exit(2, "aws key pair %q already exists with fingerprint %s, expected %s; delete it or configure a unique provider key", name, aws.ToString(existing.KeyFingerprint), strings.Join(fingerprints, " or "))
}

func sameOpenSSHPublicKey(left, right string) bool {
	leftType, leftBlob, err := parseOpenSSHPublicKey(left)
	if err != nil {
		return false
	}
	rightType, rightBlob, err := parseOpenSSHPublicKey(right)
	if err != nil {
		return false
	}
	return leftType == rightType && bytes.Equal(leftBlob, rightBlob)
}

func awsImportedPublicKeyFingerprints(publicKey string) ([]string, error) {
	keyType, blob, err := parseOpenSSHPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	switch keyType {
	case "ssh-ed25519":
		sum := sha256.Sum256(blob)
		raw := base64.RawStdEncoding.EncodeToString(sum[:])
		return []string{"SHA256:" + raw, raw}, nil
	case "ssh-rsa":
		pub, err := parseOpenSSHRSAPublicKey(blob)
		if err != nil {
			return nil, err
		}
		blobSum := md5.Sum(blob)                           //nolint:gosec // EC2 reports MD5 fingerprints for imported RSA public keys.
		derSum := md5.Sum(x509.MarshalPKCS1PublicKey(pub)) //nolint:gosec // AWS examples also derive imported RSA fingerprints from PKCS#1 DER.
		return []string{colonHex(blobSum[:]), colonHex(derSum[:])}, nil
	default:
		return nil, exit(2, "unsupported AWS SSH public key type %q", keyType)
	}
}

func awsKeyFingerprintMatches(existing string, expected []string) bool {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return false
	}
	for _, want := range expected {
		if normalizeAWSKeyFingerprint(existing) == normalizeAWSKeyFingerprint(want) {
			return true
		}
	}
	return false
}

func normalizeAWSKeyFingerprint(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "md5:") {
		return strings.TrimPrefix(lower, "md5:")
	}
	if strings.HasPrefix(value, "SHA256:") {
		return value
	}
	return lower
}

func parseOpenSSHPublicKey(publicKey string) (string, []byte, error) {
	fields := strings.Fields(strings.TrimSpace(publicKey))
	if len(fields) < 2 {
		return "", nil, exit(2, "invalid SSH public key")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", nil, exit(2, "invalid SSH public key: %v", err)
	}
	innerType, rest, err := readSSHString(blob)
	if err != nil {
		return "", nil, exit(2, "invalid SSH public key: %v", err)
	}
	if innerType != fields[0] {
		return "", nil, exit(2, "invalid SSH public key: type %q does not match blob type %q", fields[0], innerType)
	}
	_ = rest
	return innerType, blob, nil
}

func parseOpenSSHRSAPublicKey(blob []byte) (*rsa.PublicKey, error) {
	keyType, rest, err := readSSHString(blob)
	if err != nil {
		return nil, exit(2, "invalid RSA public key: %v", err)
	}
	if keyType != "ssh-rsa" {
		return nil, exit(2, "invalid RSA public key: type %q", keyType)
	}
	eBytes, rest, err := readSSHBytes(rest)
	if err != nil {
		return nil, exit(2, "invalid RSA public key exponent: %v", err)
	}
	nBytes, rest, err := readSSHBytes(rest)
	if err != nil {
		return nil, exit(2, "invalid RSA public key modulus: %v", err)
	}
	if len(rest) != 0 {
		return nil, exit(2, "invalid RSA public key: trailing data")
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Sign() <= 0 || e.Int64() > int64(^uint(0)>>1) {
		return nil, exit(2, "invalid RSA public key exponent")
	}
	n := new(big.Int).SetBytes(nBytes)
	if n.Sign() <= 0 {
		return nil, exit(2, "invalid RSA public key modulus")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func readSSHString(data []byte) (string, []byte, error) {
	value, rest, err := readSSHBytes(data)
	if err != nil {
		return "", nil, err
	}
	return string(value), rest, nil
}

func readSSHBytes(data []byte) ([]byte, []byte, error) {
	if len(data) < 4 {
		return nil, nil, fmt.Errorf("short field")
	}
	n := int(binary.BigEndian.Uint32(data[:4]))
	if n < 0 || len(data[4:]) < n {
		return nil, nil, fmt.Errorf("short field")
	}
	return data[4 : 4+n], data[4+n:], nil
}

func colonHex(data []byte) string {
	encoded := hex.EncodeToString(data)
	parts := make([]string, 0, len(encoded)/2)
	for i := 0; i < len(encoded); i += 2 {
		parts = append(parts, encoded[i:i+2])
	}
	return strings.Join(parts, ":")
}

func (c *AWSClient) DeleteSSHKey(ctx context.Context, name string) error {
	_, err := c.ec2.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyName: aws.String(name)})
	if err != nil && strings.Contains(err.Error(), "InvalidKeyPair.NotFound") {
		return nil
	}
	return err
}

type awsCleanupKeyOwnershipError struct{ err error }

func (e *awsCleanupKeyOwnershipError) Error() string { return e.err.Error() }
func (e *awsCleanupKeyOwnershipError) Unwrap() error { return e.err }

// IsAWSCleanupKeyOwnershipError reports a cleanup-time key ownership mismatch
// that must skip the instance instead of deleting either resource.
func IsAWSCleanupKeyOwnershipError(err error) bool {
	var ownershipErr *awsCleanupKeyOwnershipError
	return errors.As(err, &ownershipErr)
}

func NewAWSCleanupKeyOwnershipError(message string) error {
	return &awsCleanupKeyOwnershipError{err: errors.New(message)}
}

func validateAWSCleanupKeyPair(name string, keyPairs []types.KeyPairInfo) (string, error) {
	if len(keyPairs) != 1 || strings.TrimSpace(aws.ToString(keyPairs[0].KeyName)) != strings.TrimSpace(name) {
		return "", &awsCleanupKeyOwnershipError{err: fmt.Errorf("AWS cleanup key %q did not resolve to one exact key pair", name)}
	}
	tags := make(map[string]string, len(keyPairs[0].Tags))
	for _, tag := range keyPairs[0].Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if tags["crabbox"] != "true" || tags["created_by"] != "crabbox" {
		return "", &awsCleanupKeyOwnershipError{err: fmt.Errorf("AWS cleanup key %q lacks canonical Crabbox ownership tags", name)}
	}
	keyPairID := strings.TrimSpace(aws.ToString(keyPairs[0].KeyPairId))
	if keyPairID == "" {
		return "", &awsCleanupKeyOwnershipError{err: fmt.Errorf("AWS cleanup key %q has no immutable key pair id", name)}
	}
	return keyPairID, nil
}

func (c *AWSClient) ResolveCleanupSSHKeyID(ctx context.Context, name string) (string, error) {
	out, err := c.ec2.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{KeyNames: []string{name}})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidKeyPair.NotFound") {
			return "", nil
		}
		return "", err
	}
	return validateAWSCleanupKeyPair(name, out.KeyPairs)
}

func (c *AWSClient) DeleteCleanupSSHKeyID(ctx context.Context, keyPairID string) error {
	if keyPairID == "" {
		return nil
	}
	_, err := c.ec2.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyPairId: aws.String(keyPairID)})
	if err != nil && strings.Contains(err.Error(), "InvalidKeyPair.NotFound") {
		return nil
	}
	return err
}

func (c *AWSClient) CreateServerWithFallback(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (Server, Config, error) {
	regions := awsRegionCandidates(cfg, c.region)
	if len(regions) > 1 {
		var errs []error
		for _, region := range regions {
			next := cfg
			next.AWSRegion = region
			client := c
			if region != c.region {
				var err error
				client, err = newAWSClientForRegion(ctx, next, region)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", region, err))
					continue
				}
			}
			if logf != nil && region != c.region {
				logf("fallback provisioning region=%s after capacity/quota rejection\n", region)
			}
			server, resolved, err := client.createServerWithFallbackInRegion(ctx, next, publicKey, leaseID, slug, keep, logf)
			if err == nil {
				return server, resolved, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", region, err))
			if !isRetryableAWSRegionProvisioningError(err) {
				return Server{}, resolved, joinErrors(errs)
			}
		}
		return Server{}, cfg, joinErrors(errs)
	}
	return c.createServerWithFallbackInRegion(ctx, cfg, publicKey, leaseID, slug, keep, logf)
}

func (c *AWSClient) createServerWithFallbackInRegion(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (result Server, resolved Config, resultErr error) {
	if cfg.ProviderKey == "" {
		cfg.ProviderKey = "crabbox-steipete"
	}
	keyBinding, err := c.ensureSSHKeyBinding(ctx, cfg.ProviderKey, publicKey)
	if err != nil {
		return Server{}, cfg, err
	}
	defer func() {
		if resultErr == nil || !keyBinding.Created {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := c.DeleteCleanupSSHKeyID(cleanupCtx, keyBinding.ID); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback AWS key pair %s: %w", keyBinding.ID, err))
		}
	}()
	bindCleanupKey := func(server Server) Server {
		if keyBinding.Managed {
			server.Labels["aws_key_pair_id"] = keyBinding.ID
		}
		return server
	}
	staticImageID := ""
	if cfg.TargetOS != targetMacOS || cfg.AWSAMI != "" {
		imageID, err := c.resolveAMI(ctx, cfg)
		if err != nil {
			return Server{}, cfg, err
		}
		staticImageID = imageID
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
		imageID, err := c.resolveLaunchAMI(ctx, next, staticImageID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s image: %w", instanceType, err))
			if !isRetryableAWSProvisioningError(err) {
				return Server{}, next, joinErrors(errs)
			}
			continue
		}
		server, err := c.createServer(ctx, next, publicKey, leaseID, slug, keep, imageID, securityGroupID, useSpot)
		if err == nil {
			return bindCleanupKey(server), next, nil
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
			imageID, err := c.resolveLaunchAMI(ctx, next, staticImageID)
			if err != nil {
				errs = append(errs, fmt.Errorf("on-demand %s image: %w", instanceType, err))
				if !isRetryableAWSProvisioningError(err) {
					return Server{}, next, joinErrors(errs)
				}
				continue
			}
			server, err := c.createServer(ctx, next, publicKey, leaseID, slug, keep, imageID, securityGroupID, false)
			if err == nil {
				return bindCleanupKey(server), next, nil
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

func (c *AWSClient) resolveLaunchAMI(ctx context.Context, cfg Config, staticImageID string) (string, error) {
	if staticImageID != "" {
		return staticImageID, nil
	}
	return c.resolveAMI(ctx, cfg)
}

func (c *AWSClient) createServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, imageID, securityGroupID string, spot bool) (Server, error) {
	_ = publicKey
	name := leaseProviderName(leaseID, slug)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = renderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := time.Now().UTC()
	labels := directLeaseLabels(cfg, leaseID, slug, "aws", mapMarket(spot), keep, now)
	labels["aws_region"] = cfg.AWSRegion
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
	applyAWSRunInstanceTargetOptions(input, cfg)
	if cfg.TargetOS == targetMacOS {
		hostID := cfg.HostID
		if hostID == "" {
			hostID = cfg.AWSMacHostID
		}
		input.Placement = &types.Placement{HostId: aws.String(hostID), Tenancy: types.TenancyHost}
	} else if cfg.AWSSubnetID == "" {
		if zone := awsAvailabilityZoneForRegion(cfg, cfg.AWSRegion); zone != "" {
			input.Placement = &types.Placement{AvailabilityZone: aws.String(zone)}
		}
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
		if err := sleepContext(ctx, 5*time.Second); err != nil {
			return Server{}, err
		}
	}
}

func (c *AWSClient) WaitForServerIP(ctx context.Context, id string) (Server, error) {
	return c.waitForServerIP(ctx, id)
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

func (c *AWSClient) CreateImageCheckpoint(ctx context.Context, instanceID, name string, noReboot bool) (CoordinatorImage, error) {
	accountID, err := c.CallerAccountID(ctx)
	if err != nil {
		return CoordinatorImage{}, err
	}
	tags := awsTagsWithName(map[string]string{
		"crabbox":           "true",
		"created_by":        "crabbox",
		"crabbox:source_id": instanceID,
	}, name)
	out, err := c.ec2.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId: aws.String(instanceID),
		Name:       aws.String(name),
		NoReboot:   aws.Bool(noReboot),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeImage, Tags: tags},
			{ResourceType: types.ResourceTypeSnapshot, Tags: tags},
		},
	})
	if err != nil {
		return CoordinatorImage{}, err
	}
	imageID := aws.ToString(out.ImageId)
	if imageID == "" {
		return CoordinatorImage{}, exit(5, "aws returned no image id")
	}
	return CoordinatorImage{
		ID:         imageID,
		Name:       name,
		State:      "pending",
		Provider:   "aws",
		Kind:       checkpointKindAWSAMI,
		Region:     c.region,
		ResourceID: imageID,
		AccountID:  accountID,
		Direct:     true,
	}, nil
}

func (c *AWSClient) ValidateImageCheckpointSource(ctx context.Context, instanceID string) (string, error) {
	accountID, err := c.CallerAccountID(ctx)
	if err != nil {
		return "", err
	}
	if _, err := c.GetServer(ctx, instanceID); err != nil {
		return "", err
	}
	return accountID, nil
}

func (c *AWSClient) GetImageCheckpoint(ctx context.Context, imageID string) (CoordinatorImage, error) {
	out, err := c.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{imageID},
	})
	if err != nil {
		return CoordinatorImage{}, err
	}
	if len(out.Images) == 0 {
		return CoordinatorImage{}, exit(4, "aws image not found: %s", imageID)
	}
	image := out.Images[0]
	return CoordinatorImage{
		ID:           aws.ToString(image.ImageId),
		Name:         aws.ToString(image.Name),
		State:        string(image.State),
		Provider:     "aws",
		Kind:         checkpointKindAWSAMI,
		Region:       c.region,
		ResourceID:   aws.ToString(image.ImageId),
		SnapshotIDs:  awsImageSnapshotIDs(image),
		Direct:       true,
		Architecture: string(image.Architecture),
	}, nil
}

func (c *AWSClient) DeleteImageCheckpoint(ctx context.Context, imageID string, fallbackSnapshotIDs []string, expectedAccountID string) error {
	if err := c.GuardAccount(ctx, expectedAccountID); err != nil {
		return err
	}
	out, err := c.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{imageID},
	})
	imageNotFound := err != nil && strings.Contains(err.Error(), "InvalidAMIID.NotFound")
	if err != nil {
		if !imageNotFound {
			return err
		}
		if expectedAccountID == "" {
			return exit(3, "cannot confirm direct AWS checkpoint delete for %s: image not found and checkpoint record has no accountId; switch to the original AWS account or use --local-only", imageID)
		}
	}
	snapshotIDs := append([]string(nil), fallbackSnapshotIDs...)
	if err == nil && len(out.Images) > 0 {
		snapshotIDs = append(snapshotIDs, awsImageSnapshotIDs(out.Images[0])...)
	}
	snapshotIDs = uniqueStrings(snapshotIDs)
	if _, err := c.ec2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(imageID)}); err != nil && !strings.Contains(err.Error(), "InvalidAMIID.NotFound") {
		return err
	}
	for _, snapshotID := range snapshotIDs {
		if err := c.deleteSnapshotWithRetry(ctx, snapshotID); err != nil {
			return err
		}
	}
	return nil
}

func (c *AWSClient) CallerAccountID(ctx context.Context) (string, error) {
	if c.sts == nil {
		return "", exit(3, "aws sts client is unavailable")
	}
	out, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	accountID := aws.ToString(out.Account)
	if accountID == "" {
		return "", exit(3, "aws returned no caller account id")
	}
	return accountID, nil
}

func (c *AWSClient) GuardAccount(ctx context.Context, expectedAccountID string) error {
	expectedAccountID = strings.TrimSpace(expectedAccountID)
	if expectedAccountID == "" {
		return nil
	}
	accountID, err := c.CallerAccountID(ctx)
	if err != nil {
		return err
	}
	if accountID != expectedAccountID {
		return exit(3, "direct AWS checkpoint account mismatch: current account %s does not match checkpoint account %s", accountID, expectedAccountID)
	}
	return nil
}

func (c *AWSClient) deleteSnapshotWithRetry(ctx context.Context, snapshotID string) error {
	var lastErr error
	for attempt := 0; attempt <= len(awsSnapshotDeleteBackoff); attempt++ {
		if _, err := c.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}); err != nil {
			message := err.Error()
			if strings.Contains(message, "InvalidSnapshot.NotFound") {
				return nil
			}
			if !isRetryableAWSSnapshotDeleteError(message) {
				return err
			}
			lastErr = err
		} else {
			return nil
		}
		if attempt >= len(awsSnapshotDeleteBackoff) {
			break
		}
		timer := time.NewTimer(awsSnapshotDeleteBackoff[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func awsImageSnapshotIDs(image types.Image) []string {
	var snapshotIDs []string
	for _, mapping := range image.BlockDeviceMappings {
		if mapping.Ebs == nil {
			continue
		}
		if snapshotID := aws.ToString(mapping.Ebs.SnapshotId); snapshotID != "" {
			snapshotIDs = append(snapshotIDs, snapshotID)
		}
	}
	return uniqueStrings(snapshotIDs)
}

func isRetryableAWSSnapshotDeleteError(message string) bool {
	return strings.Contains(message, "InvalidSnapshot.InUse") ||
		strings.Contains(message, "RequestLimitExceeded") ||
		strings.Contains(message, "Throttl") ||
		strings.Contains(message, "ServiceUnavailable") ||
		strings.Contains(message, "InternalError") ||
		strings.Contains(message, "http 5") ||
		strings.Contains(strings.ToLower(message), "currently in use")
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
	if cfg.TargetOS == targetWindows {
		return c.resolveLatestAmazonAMI(ctx, "Windows_Server-2022-English-Full-Base-*", "x86_64")
	}
	if cfg.TargetOS == targetMacOS {
		name, architecture := awsMacOSAMIQueryForInstanceType(cfg.ServerType)
		return c.resolveLatestAmazonAMI(ctx, name, architecture)
	}
	architecture := awsLinuxImageArchitecture(effectiveArchitectureForConfig(cfg))
	name, label, err := awsLinuxAMIQueryForOS(cfg.OSImage, effectiveArchitectureForConfig(cfg))
	if err != nil {
		return "", err
	}
	out, err := c.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{awsUbuntuOwner},
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
		return "", exit(3, "no %s %s AMI found in %s; set CRABBOX_AWS_AMI", label, architecture, cfg.AWSRegion)
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.ToString(out.Images[i].CreationDate) > aws.ToString(out.Images[j].CreationDate)
	})
	return aws.ToString(out.Images[0].ImageId), nil
}

func awsLinuxImageArchitecture(architecture string) string {
	if architecture == ArchitectureARM64 {
		return "arm64"
	}
	return "x86_64"
}

func awsMacOSAMIQueryForInstanceType(instanceType string) (string, string) {
	if strings.HasPrefix(instanceType, "mac1.") {
		return "amzn-ec2-macos-14.*", "x86_64_mac"
	}
	if strings.HasPrefix(instanceType, "mac-m") {
		return "amzn-ec2-macos-15.*-arm64", "arm64_mac"
	}
	return "amzn-ec2-macos-14.*-arm64", "arm64_mac"
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
	var groupID string
	var group *types.SecurityGroup
	if cfg.AWSSGID != "" {
		groupID = cfg.AWSSGID
		existing, err := c.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{groupID},
		})
		if err != nil {
			return "", err
		}
		if len(existing.SecurityGroups) > 0 {
			group = &existing.SecurityGroups[0]
		}
	} else {
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
		if len(existing.SecurityGroups) > 0 {
			group = &existing.SecurityGroups[0]
			groupID = aws.ToString(group.GroupId)
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
	}
	if groupID == "" {
		return "", exit(3, "aws security group id is empty")
	}
	ports := sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts)
	if group != nil {
		if err := c.pruneStaleSSHIngress(ctx, groupID, *group, ports, cfg.AWSSSHCIDRs); err != nil {
			return "", err
		}
	}
	for _, port := range ports {
		if err := c.allowTCP(ctx, groupID, port, cfg.AWSSSHCIDRs); err != nil && !strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
			return "", err
		}
	}
	return groupID, nil
}

func (c *AWSClient) pruneStaleSSHIngress(ctx context.Context, groupID string, group types.SecurityGroup, ports []string, cidrs []string) error {
	stalePermissions := staleAWSCrabboxSSHIngressPermissions(group, ports, cidrs)
	if len(stalePermissions) == 0 {
		return nil
	}
	_, err := c.ec2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId:       aws.String(groupID),
		IpPermissions: stalePermissions,
	})
	if err != nil && strings.Contains(err.Error(), "InvalidPermission.NotFound") {
		return nil
	}
	return err
}

func staleAWSCrabboxSSHIngressPermissions(group types.SecurityGroup, ports []string, cidrs []string) []types.IpPermission {
	desiredPorts := map[int32]struct{}{}
	for _, port := range ports {
		p, ok := parsePort32(port)
		if ok {
			desiredPorts[p] = struct{}{}
		}
	}
	if len(desiredPorts) == 0 {
		return nil
	}
	desiredCIDRs := awsSSHDesiredCIDRs(cidrs)
	var stale []types.IpPermission
	for _, permission := range group.IpPermissions {
		port, ok := exactTCPPermissionPort(permission)
		if !ok {
			continue
		}
		_, keepPort := desiredPorts[port]
		staleIPv4 := staleAWSIpRanges(permission.IpRanges, desiredCIDRs, keepPort)
		staleIPv6 := staleAWSIpv6Ranges(permission.Ipv6Ranges, desiredCIDRs, keepPort)
		if len(staleIPv4) == 0 && len(staleIPv6) == 0 {
			continue
		}
		stale = append(stale, types.IpPermission{
			FromPort:   permission.FromPort,
			IpProtocol: permission.IpProtocol,
			IpRanges:   staleIPv4,
			Ipv6Ranges: staleIPv6,
			ToPort:     permission.ToPort,
		})
	}
	return stale
}

func awsSSHDesiredCIDRs(cidrs []string) map[string]struct{} {
	desired := map[string]struct{}{}
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr != "" {
			desired[cidr] = struct{}{}
		}
	}
	if len(desired) == 0 {
		desired["0.0.0.0/0"] = struct{}{}
	}
	return desired
}

func exactTCPPermissionPort(permission types.IpPermission) (int32, bool) {
	if aws.ToString(permission.IpProtocol) != "tcp" || permission.FromPort == nil || permission.ToPort == nil || *permission.FromPort != *permission.ToPort {
		return 0, false
	}
	return *permission.FromPort, true
}

func staleAWSIpRanges(ranges []types.IpRange, desiredCIDRs map[string]struct{}, keepPort bool) []types.IpRange {
	stale := make([]types.IpRange, 0, len(ranges))
	for _, r := range ranges {
		if aws.ToString(r.Description) != awsSSHIngressDescription {
			continue
		}
		cidr := aws.ToString(r.CidrIp)
		_, keepCIDR := desiredCIDRs[cidr]
		if !keepPort || !keepCIDR {
			stale = append(stale, types.IpRange{CidrIp: r.CidrIp, Description: r.Description})
		}
	}
	return stale
}

func staleAWSIpv6Ranges(ranges []types.Ipv6Range, desiredCIDRs map[string]struct{}, keepPort bool) []types.Ipv6Range {
	stale := make([]types.Ipv6Range, 0, len(ranges))
	for _, r := range ranges {
		if aws.ToString(r.Description) != awsSSHIngressDescription {
			continue
		}
		cidr := aws.ToString(r.CidrIpv6)
		_, keepCIDR := desiredCIDRs[cidr]
		if !keepPort || !keepCIDR {
			stale = append(stale, types.Ipv6Range{CidrIpv6: r.CidrIpv6, Description: r.Description})
		}
	}
	return stale
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
			ranges = append(ranges, types.IpRange{CidrIp: aws.String(cidr), Description: aws.String(awsSSHIngressDescription)})
		}
	}
	if len(ranges) == 0 {
		ranges = append(ranges, types.IpRange{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String(awsSSHIngressDescription)})
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
		ProviderMetadata: map[string]any{
			"instanceProfileAttached": instance.IamInstanceProfile != nil,
		},
	}
	if instance.Placement != nil {
		server.HostID = aws.ToString(instance.Placement.HostId)
	}
	server.PublicNet.IPv4.IP = aws.ToString(instance.PublicIpAddress)
	server.ServerType.Name = string(instance.InstanceType)
	return server
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
		strings.Contains(s, "InvalidHostID.NotFound") ||
		strings.Contains(s, "no AWS AMI found") ||
		strings.Contains(s, "no available EC2 Mac Dedicated Host") ||
		strings.Contains(s, "Unsupported") ||
		strings.Contains(s, "InvalidParameterValue") ||
		(strings.Contains(s, "InvalidParameterCombination") &&
			(strings.Contains(s, "Free Tier") ||
				strings.Contains(s, "eligible") ||
				strings.Contains(s, "InstanceType") ||
				strings.Contains(s, "instance type")))
}

func isRetryableAWSRegionProvisioningError(err error) bool {
	s := err.Error()
	return isRetryableAWSProvisioningError(err) ||
		strings.Contains(s, "quota ") ||
		strings.Contains(s, "capacity")
}

func awsRegionCandidates(cfg Config, preferredRegion string) []string {
	return appendUniqueStrings([]string{preferredRegion, cfg.AWSRegion}, cfg.Capacity.Regions...)
}

func awsAvailabilityZoneForRegion(cfg Config, region string) string {
	for _, zone := range cfg.Capacity.AvailabilityZones {
		if strings.HasPrefix(zone, region) {
			return zone
		}
	}
	return ""
}

func awsLaunchCandidates(cfg Config) []string {
	if cfg.ServerTypeExplicit {
		return []string{cfg.ServerType}
	}
	if cfg.TargetOS == targetMacOS {
		return appendUniqueStrings([]string{cfg.ServerType}, awsInstanceTypeCandidatesForConfig(cfg)...)
	}
	fallback := "t3.small"
	if cfg.TargetOS == targetLinux && effectiveArchitectureForConfig(cfg) == ArchitectureARM64 {
		fallback = "t4g.small"
	}
	if cfg.TargetOS == targetWindows {
		fallback = "t3.large"
		if cfg.WindowsMode == windowsModeWSL2 {
			fallback = "m8i.large"
		}
	}
	return appendUniqueStrings([]string{cfg.ServerType}, append(awsInstanceTypeCandidatesForConfig(cfg), fallback)...)
}

func awsCapacityDoctorMarkets(cfg Config) []string {
	market := strings.TrimSpace(cfg.Capacity.Market)
	if market == "" {
		market = "spot"
	}
	markets := []string{market}
	if market == "spot" && strings.HasPrefix(cfg.Capacity.Fallback, "on-demand") {
		markets = append(markets, "on-demand")
	}
	return appendUniqueStrings(markets)
}

func awsQuotaCodeForMarket(market string) string {
	if market == "on-demand" {
		return awsOnDemandQuotaCode
	}
	return awsSpotQuotaCode
}

func awsCapacityDoctorCheckForQuota(cfg Config, market string, quotaValue float64, quotaKnown bool, quotaErr error) DoctorCheck {
	serverType := strings.TrimSpace(cfg.ServerType)
	if serverType == "" {
		serverType = serverTypeForConfig(cfg)
	}
	quotaCode := awsQuotaCodeForMarket(market)
	needed := awsInstanceTypeVCPUs(serverType)
	base := map[string]string{
		"provider":             "aws",
		"market":               market,
		"quota_code":           quotaCode,
		"default_class":        cfg.Class,
		"default_type":         serverType,
		"default_needed_vcpus": strconv.Itoa(needed),
	}
	if quotaErr != nil {
		base["hint"] = "allow_servicequotas_getservicequota"
		base["error"] = quotaErr.Error()
		return DoctorCheck{
			Status:  "skip",
			Check:   "capacity",
			Message: awsDoctorMessage("provider=aws capacity=unknown", base),
			Details: base,
		}
	}
	if !quotaKnown {
		base["hint"] = "servicequotas_unavailable"
		return DoctorCheck{
			Status:  "skip",
			Check:   "capacity",
			Message: awsDoctorMessage("provider=aws capacity=unknown", base),
			Details: base,
		}
	}
	limit := int(quotaValue)
	base["limit_vcpus"] = strconv.Itoa(limit)
	if needed == 0 {
		base["hint"] = "unknown_instance_vcpus"
		return DoctorCheck{
			Status:  "skip",
			Check:   "capacity",
			Message: awsDoctorMessage("provider=aws capacity=unknown", base),
			Details: base,
		}
	}
	if quotaValue < float64(needed) {
		recommendedClass, recommendedType := awsRecommendedClassForQuota(cfg, limit)
		if recommendedClass != "" {
			base["recommended_class"] = recommendedClass
			base["recommended_type"] = recommendedType
		}
		base["hint"] = "lower_class_or_request_quota"
		return DoctorCheck{
			Status:  "warning",
			Check:   "capacity",
			Message: awsDoctorMessage("provider=aws capacity=quota_pressure", base),
			Details: base,
		}
	}
	base["hint"] = "quota_satisfies_default_class"
	return DoctorCheck{
		Status:  "ok",
		Check:   "capacity",
		Message: awsDoctorMessage("provider=aws capacity=ready", base),
		Details: base,
	}
}

func awsDoctorMessage(prefix string, details map[string]string) string {
	keys := make([]string, 0, len(details))
	for key := range details {
		if key == "provider" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(prefix)
	for _, key := range keys {
		if details[key] == "" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(strings.ReplaceAll(details[key], " ", "_"))
	}
	return b.String()
}

func awsRecommendedClassForQuota(cfg Config, limitVCPUs int) (string, string) {
	if limitVCPUs <= 0 {
		return "", ""
	}
	architecture := effectiveArchitectureForConfig(cfg)
	classes := []string{"beast", "large", "fast", "standard"}
	for _, class := range classes {
		candidates := awsInstanceTypeCandidatesForTargetModeArchitectureClass(cfg.TargetOS, cfg.WindowsMode, architecture, class)
		if len(candidates) == 0 {
			continue
		}
		if awsInstanceTypeVCPUs(candidates[0]) <= limitVCPUs {
			return class, candidates[0]
		}
	}
	for _, serverType := range awsInstanceTypeCandidatesForTargetModeArchitectureClass(cfg.TargetOS, cfg.WindowsMode, architecture, "standard") {
		if awsInstanceTypeVCPUs(serverType) <= limitVCPUs {
			return "standard", serverType
		}
	}
	return "", ""
}

func awsInstanceTypeVCPUs(serverType string) int {
	_, size, ok := strings.Cut(strings.TrimSpace(serverType), ".")
	if !ok || size == "" {
		return 0
	}
	if strings.HasSuffix(size, "xlarge") {
		multiplier := strings.TrimSuffix(size, "xlarge")
		if multiplier == "" {
			return 4
		}
		parsed, err := strconv.Atoi(multiplier)
		if err != nil {
			return 0
		}
		return parsed * 4
	}
	switch size {
	case "nano", "micro", "small", "medium", "large":
		return 2
	default:
		return 0
	}
}

func awsInstanceTypeSupportsNestedVirtualization(instanceType string) bool {
	family, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(instanceType)), ".")
	switch family {
	case "c8i", "m8i", "m8i-flex", "r8i":
		return true
	default:
		return false
	}
}

func applyAWSRunInstanceTargetOptions(input *ec2.RunInstancesInput, cfg Config) {
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 {
		input.CpuOptions = &types.CpuOptionsRequest{
			NestedVirtualization: types.NestedVirtualizationSpecificationEnabled,
		}
	}
}

func parsePort32(port string) (int32, bool) {
	n, err := strconv.ParseInt(port, 10, 32)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return int32(n), true
}
