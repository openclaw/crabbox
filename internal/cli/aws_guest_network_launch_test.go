package cli

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func validAWSGuestConfig() Config {
	p := testGuestPolicy()
	p.PlatformExceptions = []string{AWSGuestNetworkPlatformServices}
	return Config{GuestNetworkPolicy: &p, TargetOS: "linux", AWSAMI: "ami-0123456789abcdef0", AWSSubnetID: "subnet-0123456789abcdef0", Network: NetworkPublic, guestNetworkBoundary: &AWSGuestNetworkBoundary{LeaseID: "cbx_0123456789ab", SubnetID: "subnet-0123456789abcdef0", VPCID: "vpc-0123456789abcdef0", SecurityGroupID: "sg-0123456789abcdef0"}}
}
func TestAWSGuestNetworkLaunchOmitsBootstrapAndCredentials(t *testing.T) {
	cfg := validAWSGuestConfig()
	input := &ec2.RunInstancesInput{UserData: aws.String("must not be delivered"), IamInstanceProfile: &types.IamInstanceProfileSpecification{Name: aws.String("must not be delivered")}, NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{SubnetId: aws.String(cfg.AWSSubnetID), Groups: []string{cfg.guestNetworkBoundary.SecurityGroupID}}}}
	if err := applyAWSGuestNetworkLaunchOptions(input, cfg); err != nil {
		t.Fatal(err)
	}
	if input.UserData != nil || input.IamInstanceProfile != nil || input.MetadataOptions == nil || input.MetadataOptions.HttpEndpoint != types.InstanceMetadataEndpointStateDisabled || input.MetadataOptions.HttpProtocolIpv6 != types.InstanceMetadataProtocolStateDisabled {
		t.Fatal("guest bootstrap/credential surface was not removed")
	}
}
func TestAWSGuestNetworkSyntheticConfigurationDoesNotAttestBoundary(t *testing.T) {
	cfg := validAWSGuestConfig()
	cfg.AWSSubnetID = "subnet-review-trusted"
	cfg.AWSSGID = "sg-review-trusted"
	if err := ValidateAWSGuestNetworkConfig(cfg); err == nil {
		t.Fatal("caller-selected security group accepted")
	}
	cfg = validAWSGuestConfig()
	cfg.guestNetworkBoundary = nil
	if err := applyAWSGuestNetworkLaunchOptions(&ec2.RunInstancesInput{}, cfg); err == nil {
		t.Fatal("public routing/config assertions accepted without enforced boundary")
	}
}
func TestAWSGuestNetworkLaunchRejectsUnsafeBootstrapModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"IAM", func(c *Config) { c.AWSProfile = "example-profile" }}, {"overlay", func(c *Config) { c.Tailscale.Enabled = true }}, {"environment", func(c *Config) { c.EnvAllow = []string{"EXAMPLE_TOKEN"} }}, {"shared cache", func(c *Config) { c.Cache.Pnpm = true }}, {"unbaked image", func(c *Config) { c.AWSAMI = "" }}, {"shared host", func(c *Config) { c.TargetOS = "macos" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validAWSGuestConfig()
			tc.change(&cfg)
			if ValidateAWSGuestNetworkConfig(cfg) == nil {
				t.Fatal("unsafe guest bootstrap accepted")
			}
		})
	}
}
func TestAWSGuestNetworkPolicyChangesFixedReplayIdentity(t *testing.T) {
	cfg := validAWSGuestConfig()
	req := FixedAWSCreateIntentRequest{AccountID: "000000000000", RequestedSlug: "example", SSHPublicKey: "ssh-ed25519 example"}
	// Match normal provider defaults for unrelated capacity fields.
	cfg.Capacity = baseConfig().Capacity
	a, err := FixedAWSCreateIntentFingerprint(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	p := *cfg.GuestNetworkPolicy
	p.Revision = "v2"
	cfg.GuestNetworkPolicy = &p
	b, err := FixedAWSCreateIntentFingerprint(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("changed policy reused fixed allocation identity")
	}
}

func TestAWSGuestNetworkPublicBootstrapLaunch(t *testing.T) {
	cfg := validAWSGuestConfig()
	policy := managedGuestTestPolicy()
	cfg.GuestNetworkPolicy = &policy
	cfg.AWSAMI = ""
	cfg.OSImage = "ubuntu:24.04"
	cfg.SSHPort = "22"
	data, err := GuestPublicKeyUserData(guestBootstrapTestPublicKey(t), "ubuntu", "/work/project")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := guestBootstrapHash(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg.guestNetworkBoundary.BootstrapMode = policy.BootstrapMode
	cfg.guestNetworkBoundary.BootstrapSHA256 = hash
	input := &ec2.RunInstancesInput{UserData: aws.String(data), IamInstanceProfile: &types.IamInstanceProfileSpecification{Name: aws.String("must-not-survive")}, NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{SubnetId: aws.String(cfg.AWSSubnetID), Groups: []string{cfg.guestNetworkBoundary.SecurityGroupID}}}}
	if err := applyAWSGuestNetworkLaunchOptions(input, cfg); err != nil {
		t.Fatal(err)
	}
	if input.IamInstanceProfile != nil || aws.ToString(input.UserData) != data || input.MetadataOptions.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || input.MetadataOptions.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(input.MetadataOptions.HttpPutResponseHopLimit) != 1 || input.MetadataOptions.InstanceMetadataTags != types.InstanceMetadataTagsStateDisabled {
		t.Fatal("roleless public-only launch contract not enforced")
	}
	input.UserData = aws.String("modified")
	if err := applyAWSGuestNetworkLaunchOptions(input, cfg); err == nil {
		t.Fatal("unbound userdata accepted")
	}
	for _, change := range []func(*Config){func(c *Config) { c.AWSAMI = "ami-custom" }, func(c *Config) { c.AWSProfile = "forbidden-role" }, func(c *Config) { c.OSImage = "debian:12" }, func(c *Config) { c.Tailscale.Enabled = true }} {
		bad := cfg
		change(&bad)
		if err := ValidateAWSGuestNetworkConfig(bad); err == nil {
			t.Fatal("unqualified image/secret transport accepted")
		}
	}
}
