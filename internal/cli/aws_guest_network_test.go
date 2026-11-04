package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"net/http"
)

const (
	guestTestSubnet   = "subnet-11111111"
	guestTestVPC      = "vpc-11111111"
	guestTestSG       = "sg-11111111"
	guestTestInstance = "i-11111111"
)

type awsGuestNetworkFake struct {
	createErr         error
	createMaxAttempts int
	group             *types.SecurityGroup
	subnet            types.Subnet
	vpc               types.Vpc
	createUncertain   bool
	instance          types.Instance
	attached          bool
	calls             []string
	failAt            string
	keepDeleted       bool
}

func newAWSGuestNetworkFake() *awsGuestNetworkFake {
	return &awsGuestNetworkFake{subnet: types.Subnet{SubnetId: aws.String(guestTestSubnet), VpcId: aws.String(guestTestVPC), State: types.SubnetStateAvailable}, vpc: types.Vpc{VpcId: aws.String(guestTestVPC), State: types.VpcStateAvailable}}
}

func awsGuestTestPolicy() GuestNetworkPolicy {
	return GuestNetworkPolicy{Version: 1, ID: "example-policy", Revision: "1", PlatformExceptions: []string{AWSGuestNetworkPlatformServices}, SSHSources: []string{"198.51.100.7/32"}, Egress: []GuestNetworkRule{{CIDR: "93.184.216.34/32", Ports: []int{443}}}}
}

func (f *awsGuestNetworkFake) call(name string) error {
	f.calls = append(f.calls, name)
	if f.failAt == name {
		return errors.New("synthetic control-plane failure: " + name)
	}
	return nil
}

func (f *awsGuestNetworkFake) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if err := f.call("describe-subnet"); err != nil {
		return nil, err
	}
	if len(in.SubnetIds) != 1 || in.SubnetIds[0] != aws.ToString(f.subnet.SubnetId) {
		return &ec2.DescribeSubnetsOutput{}, nil
	}
	return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{f.subnet}}, nil
}
func (f *awsGuestNetworkFake) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, options ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	if err := f.call("create"); err != nil {
		return nil, err
	}
	settings := ec2.Options{}
	for _, apply := range options {
		apply(&settings)
	}
	if settings.Retryer != nil {
		f.createMaxAttempts = settings.Retryer.MaxAttempts()
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.group = &types.SecurityGroup{GroupId: aws.String(guestTestSG), GroupName: in.GroupName, VpcId: in.VpcId, Tags: slices.Clone(in.TagSpecifications[0].Tags), IpPermissionsEgress: []types.IpPermission{{IpProtocol: aws.String("-1"), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}}}
	if f.createUncertain {
		return nil, errors.New("synthetic lost create response")
	}
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String(guestTestSG)}, nil
}
func (f *awsGuestNetworkFake) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if err := f.call("describe-group"); err != nil {
		return nil, err
	}
	if f.group == nil {
		return nil, &smithy.GenericAPIError{Code: "InvalidGroup.NotFound", Message: "absent"}
	}
	if len(in.GroupIds) == 0 && len(in.Filters) == 2 && slices.Equal(in.Filters[0].Values, []string{aws.ToString(f.group.GroupName)}) && slices.Equal(in.Filters[1].Values, []string{aws.ToString(f.group.VpcId)}) {
		return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []types.SecurityGroup{*f.group}}, nil
	}
	if len(in.GroupIds) != 1 || in.GroupIds[0] != aws.ToString(f.group.GroupId) {
		return &ec2.DescribeSecurityGroupsOutput{}, nil
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []types.SecurityGroup{*f.group}}, nil
}
func (f *awsGuestNetworkFake) RevokeSecurityGroupEgress(_ context.Context, in *ec2.RevokeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	if err := f.call("revoke-egress"); err != nil {
		return nil, err
	}
	if aws.ToString(in.GroupId) != guestTestSG || len(in.IpPermissions) == 0 {
		return nil, errors.New("bad revoke")
	}
	f.group.IpPermissionsEgress = nil
	return &ec2.RevokeSecurityGroupEgressOutput{}, nil
}
func (f *awsGuestNetworkFake) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	if err := f.call("authorize-ingress"); err != nil {
		return nil, err
	}
	f.group.IpPermissions = slices.Clone(in.IpPermissions)
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}
func (f *awsGuestNetworkFake) AuthorizeSecurityGroupEgress(_ context.Context, in *ec2.AuthorizeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	if err := f.call("authorize-egress"); err != nil {
		return nil, err
	}
	f.group.IpPermissionsEgress = slices.Clone(in.IpPermissions)
	return &ec2.AuthorizeSecurityGroupEgressOutput{}, nil
}
func (f *awsGuestNetworkFake) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if err := f.call("describe-instance"); err != nil {
		return nil, err
	}
	if len(in.InstanceIds) != 1 || in.InstanceIds[0] != aws.ToString(f.instance.InstanceId) {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{f.instance}}}}, nil
}
func (f *awsGuestNetworkFake) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	if err := f.call("describe-attachments"); err != nil {
		return nil, err
	}
	if len(in.Filters) != 1 || aws.ToString(in.Filters[0].Name) != "group-id" || !slices.Equal(in.Filters[0].Values, []string{guestTestSG}) {
		return nil, errors.New("bad attachment query")
	}
	if f.attached {
		return &ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []types.NetworkInterface{{NetworkInterfaceId: aws.String("eni-11111111")}}}, nil
	}
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}
func (f *awsGuestNetworkFake) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	if err := f.call("delete"); err != nil {
		return nil, err
	}
	if aws.ToString(in.GroupId) != guestTestSG {
		return nil, errors.New("foreign delete")
	}
	if !f.keepDeleted {
		f.group = nil
	}
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *awsGuestNetworkFake) setInstance(boundary AWSGuestNetworkBoundary) {
	f.instance = types.Instance{InstanceId: aws.String(guestTestInstance), SubnetId: aws.String(boundary.SubnetID), VpcId: aws.String(boundary.VPCID),
		State: &types.InstanceState{Name: types.InstanceStateNamePending}, SourceDestCheck: aws.Bool(true),
		SecurityGroups:    []types.GroupIdentifier{{GroupId: aws.String(boundary.SecurityGroupID)}},
		MetadataOptions:   &types.InstanceMetadataOptionsResponse{HttpEndpoint: types.InstanceMetadataEndpointStateDisabled, State: types.InstanceMetadataOptionsStateApplied},
		Tags:              []types.Tag{{Key: aws.String("lease"), Value: aws.String(boundary.LeaseID)}, {Key: aws.String("crabbox"), Value: aws.String("true")}},
		NetworkInterfaces: []types.InstanceNetworkInterface{{NetworkInterfaceId: aws.String("eni-11111111"), SubnetId: aws.String(boundary.SubnetID), VpcId: aws.String(boundary.VPCID), SourceDestCheck: aws.Bool(true), Groups: []types.GroupIdentifier{{GroupId: aws.String(boundary.SecurityGroupID)}}}},
	}
}

func preparedAWSGuestBoundary(t *testing.T) (*awsGuestNetworkFake, AWSGuestNetworkBoundary, GuestNetworkPolicy) {
	t.Helper()
	f := newAWSGuestNetworkFake()
	policy := awsGuestTestPolicy()
	boundary, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", guestTestSubnet, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.setInstance(boundary)
	return f, boundary, policy
}

func TestAWSGuestNetworkEnforceInspectAndOwnedCleanup(t *testing.T) {
	f, boundary, policy := preparedAWSGuestBoundary(t)
	if !slices.Equal(f.calls, []string{"describe-subnet", "describe-vpc", "create", "describe-group", "revoke-egress", "authorize-ingress", "authorize-egress", "describe-group"}) {
		t.Fatalf("unexpected pre-launch enforcement order: %v", f.calls)
	}
	receipt, err := InspectAWSGuestNetwork(context.Background(), f, boundary, guestTestInstance, policy)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Version != 1 || receipt.LeaseID != boundary.LeaseID || receipt.ResourceID != guestTestInstance || receipt.Provider != "aws" || receipt.PolicyDigest != boundary.PolicyDigest || receipt.PolicyID != policy.ID || receipt.PolicyRevision != policy.Revision || receipt.BoundaryID != guestTestSG {
		t.Fatalf("unbound receipt: %+v", receipt)
	}
	if receipt.Facts["amazonProvidedDNS"] != "aws-resolver-exception" || receipt.Facts["imds"] != "disabled" {
		t.Fatalf("missing effective policy exceptions: %v", receipt.Facts)
	}
	if err := CleanupAWSGuestNetwork(context.Background(), f, boundary); err != nil {
		t.Fatal(err)
	}
	if f.group != nil {
		t.Fatal("owned boundary was not deleted")
	}
	if err := CleanupAWSGuestNetwork(context.Background(), f, boundary); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
}

func TestAWSGuestNetworkRejectsUnobservedSubnet(t *testing.T) {
	for _, subnet := range []string{"subnet-review-trusted", "subnet-22222222"} {
		t.Run(subnet, func(t *testing.T) {
			f := newAWSGuestNetworkFake()
			if _, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", subnet, awsGuestTestPolicy()); err == nil {
				t.Fatal("caller-asserted subnet was accepted")
			}
			if slices.Contains(f.calls, "create") {
				t.Fatal("allocated before authoritative subnet observation")
			}
		})
	}
}

func TestAWSGuestNetworkPreparationFailureCleansOnlyOwnedGroup(t *testing.T) {
	for _, failure := range []string{"revoke-egress", "authorize-ingress", "authorize-egress"} {
		t.Run(failure, func(t *testing.T) {
			f := newAWSGuestNetworkFake()
			f.failAt = failure
			boundary, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", guestTestSubnet, awsGuestTestPolicy())
			if err == nil || boundary.SecurityGroupID != guestTestSG || f.group != nil || !slices.Contains(f.calls, "delete") {
				t.Fatalf("failure lost owned cleanup: boundary=%+v err=%v calls=%v", boundary, err, f.calls)
			}
		})
	}
}

func TestAWSGuestNetworkRejectsActualResourceCounterexamples(t *testing.T) {
	cases := map[string]func(*awsGuestNetworkFake){
		"absent instance": func(f *awsGuestNetworkFake) { f.instance.InstanceId = nil },
		"wrong lease":     func(f *awsGuestNetworkFake) { f.instance.Tags[0].Value = aws.String("other-lease") },
		"missing lease":   func(f *awsGuestNetworkFake) { f.instance.Tags = nil },
		"wrong subnet":    func(f *awsGuestNetworkFake) { f.instance.SubnetId = aws.String("subnet-22222222") },
		"wrong vpc":       func(f *awsGuestNetworkFake) { f.instance.VpcId = aws.String("vpc-22222222") },
		"extra group": func(f *awsGuestNetworkFake) {
			f.instance.SecurityGroups = append(f.instance.SecurityGroups, types.GroupIdentifier{GroupId: aws.String("sg-22222222")})
		},
		"extra interface": func(f *awsGuestNetworkFake) {
			f.instance.NetworkInterfaces = append(f.instance.NetworkInterfaces, f.instance.NetworkInterfaces[0])
		},
		"interface group": func(f *awsGuestNetworkFake) {
			f.instance.NetworkInterfaces[0].Groups[0].GroupId = aws.String("sg-22222222")
		},
		"ipv6": func(f *awsGuestNetworkFake) {
			f.instance.NetworkInterfaces[0].Ipv6Addresses = []types.InstanceIpv6Address{{Ipv6Address: aws.String("2001:db8::1")}}
		},
		"instance profile": func(f *awsGuestNetworkFake) {
			f.instance.IamInstanceProfile = &types.IamInstanceProfile{Arn: aws.String("arn:aws:iam::000000000000:instance-profile/example")}
		},
		"metadata enabled": func(f *awsGuestNetworkFake) {
			f.instance.MetadataOptions.HttpEndpoint = types.InstanceMetadataEndpointStateEnabled
		},
		"metadata pending": func(f *awsGuestNetworkFake) {
			f.instance.MetadataOptions.State = types.InstanceMetadataOptionsStatePending
		},
		"metadata absent":             func(f *awsGuestNetworkFake) { f.instance.MetadataOptions = nil },
		"source destination spoofing": func(f *awsGuestNetworkFake) { f.instance.SourceDestCheck = aws.Bool(false) },
		"terminated":                  func(f *awsGuestNetworkFake) { f.instance.State.Name = types.InstanceStateNameTerminated },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, boundary, policy := preparedAWSGuestBoundary(t)
			mutate(f)
			receipt, err := InspectAWSGuestNetwork(context.Background(), f, boundary, guestTestInstance, policy)
			if err == nil || receipt.Version != 0 {
				t.Fatalf("issued receipt for unsafe observation: %+v %v", receipt, err)
			}
		})
	}
}

func TestAWSGuestNetworkRejectsPolicyDriftAndReplay(t *testing.T) {
	cases := map[string]func(*awsGuestNetworkFake, *AWSGuestNetworkBoundary, *GuestNetworkPolicy){
		"policy revision": func(_ *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, p *GuestNetworkPolicy) { p.Revision = "2" },
		"other lease": func(_ *awsGuestNetworkFake, b *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			b.LeaseID = "other-lease"
		},
		"missing handle": func(_ *awsGuestNetworkFake, b *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			*b = AWSGuestNetworkBoundary{}
		},
		"unrestricted egress": func(f *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			f.group.IpPermissionsEgress = []types.IpPermission{{IpProtocol: aws.String("-1"), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}}
		},
		"ipv6 egress": func(f *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			f.group.IpPermissionsEgress[0].Ipv6Ranges = []types.Ipv6Range{{CidrIpv6: aws.String("::/0")}}
		},
		"prefix list": func(f *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			f.group.IpPermissionsEgress[0].PrefixListIds = []types.PrefixListId{{PrefixListId: aws.String("pl-11111111")}}
		},
		"peer group": func(f *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			f.group.IpPermissionsEgress[0].UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: aws.String("sg-22222222")}}
		},
		"missing egress": func(f *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			f.group.IpPermissionsEgress = nil
		},
		"extra ingress": func(f *awsGuestNetworkFake, _ *AWSGuestNetworkBoundary, _ *GuestNetworkPolicy) {
			f.group.IpPermissions = append(f.group.IpPermissions, types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(80), ToPort: aws.Int32(80), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, boundary, policy := preparedAWSGuestBoundary(t)
			mutate(f, &boundary, &policy)
			receipt, err := InspectAWSGuestNetwork(context.Background(), f, boundary, guestTestInstance, policy)
			if err == nil || receipt.Version != 0 {
				t.Fatalf("issued receipt for policy drift/replay: %+v %v", receipt, err)
			}
		})
	}
}

func TestAWSGuestNetworkCleanupRefusesForeignOrAttachedBoundary(t *testing.T) {
	for _, mode := range []string{"foreign", "attached", "delete failure", "still present"} {
		t.Run(mode, func(t *testing.T) {
			f, boundary, _ := preparedAWSGuestBoundary(t)
			switch mode {
			case "foreign":
				f.group.Tags[1].Value = aws.String("another-lease")
			case "attached":
				f.attached = true
			case "delete failure":
				f.failAt = "delete"
			case "still present":
				f.keepDeleted = true
			}
			f.calls = nil
			if err := CleanupAWSGuestNetwork(context.Background(), f, boundary); err == nil {
				t.Fatal("unsafe/incomplete cleanup was accepted")
			}
			if f.group == nil {
				t.Fatal("removed an unowned or attached group")
			}
			if (mode == "foreign" || mode == "attached") && slices.Contains(f.calls, "delete") {
				t.Fatal("attempted forbidden deletion")
			}
		})
	}
}

func TestAWSGuestNetworkPublicRangeValidation(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "10.0.0.0/8", "169.254.169.254/32", "100.100.100.200/32", "172.16.0.0/12", "192.168.0.0/16", "126.0.0.0/7", "2001:db8::/32", "93.184.216.34/24"} {
		t.Run(strings.ReplaceAll(cidr, "/", "_"), func(t *testing.T) {
			f := newAWSGuestNetworkFake()
			p := awsGuestTestPolicy()
			p.Egress[0].CIDR = cidr
			if _, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", guestTestSubnet, p); err == nil {
				t.Fatal("nonpublic/wildcard egress accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("invalid policy reached AWS: %v", f.calls)
			}
		})
	}
}

func (f *awsGuestNetworkFake) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if err := f.call("describe-vpc"); err != nil {
		return nil, err
	}
	if len(in.VpcIds) != 1 || in.VpcIds[0] != aws.ToString(f.vpc.VpcId) {
		return &ec2.DescribeVpcsOutput{}, nil
	}
	return &ec2.DescribeVpcsOutput{Vpcs: []types.Vpc{f.vpc}}, nil
}

func TestAWSGuestNetworkUncertainCreationRetainsAndSettlesOwnedHandle(t *testing.T) {
	t.Run("accepted response lost", func(t *testing.T) {
		f := newAWSGuestNetworkFake()
		f.createUncertain = true
		boundary, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", guestTestSubnet, awsGuestTestPolicy())
		if err == nil || boundary.SecurityGroupID != guestTestSG || boundary.SecurityGroupName == "" || f.group != nil || !slices.Contains(f.calls, "delete") {
			t.Fatalf("uncertain accepted create lost cleanup: %+v %v %v", boundary, err, f.calls)
		}
	})
	t.Run("not observed retains unresolved handle", func(t *testing.T) {
		f := newAWSGuestNetworkFake()
		f.failAt = "create"
		boundary, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", guestTestSubnet, awsGuestTestPolicy())
		if err == nil || boundary.SecurityGroupName == "" || boundary.SecurityGroupID != "" || slices.Contains(f.calls, "delete") {
			t.Fatalf("uncertain create was called settled: %+v %v %v", boundary, err, f.calls)
		}
	})
	t.Run("foreign recovery refused", func(t *testing.T) {
		f, boundary, _ := preparedAWSGuestBoundary(t)
		boundary.SecurityGroupID = ""
		f.group.Tags[1].Value = aws.String("another-lease")
		if _, err := ResolveAWSGuestNetworkBoundary(context.Background(), f, boundary); err == nil {
			t.Fatal("recovered foreign security group")
		}
		if slices.Contains(f.calls, "delete") {
			t.Fatal("deleted foreign security group")
		}
	})
}

func TestAWSGuestNetworkMissingVPCNeverAllocates(t *testing.T) {
	f := newAWSGuestNetworkFake()
	f.vpc.VpcId = nil
	if _, err := PrepareAWSGuestNetwork(context.Background(), f, "lease-example", guestTestSubnet, awsGuestTestPolicy()); err == nil {
		t.Fatal("accepted missing provider VPC")
	}
	if slices.Contains(f.calls, "create") {
		t.Fatal("allocated before VPC observation")
	}
}

func TestAWSGuestNetworkDefinitiveUnauthorizedNoEffect(t *testing.T) {
	api := newAWSGuestNetworkFake()
	api.createErr = &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "synthetic denial", Fault: smithy.FaultClient}
	var saved []AWSGuestNetworkBoundary
	boundary, err := PrepareAWSGuestNetworkWithCustody(t.Context(), api, "cbx_0123456789ab", guestTestSubnet, awsGuestTestPolicy(), func(b AWSGuestNetworkBoundary) error { saved = append(saved, b); return nil })
	if err == nil || !AWSGuestNetworkCreateRejectedNoEffect(boundary) || len(saved) != 2 || saved[0].CreateRejectedNoEffect || saved[0].CreateAttemptID != saved[1].CreateAttemptID || api.createMaxAttempts != 1 {
		t.Fatalf("definitive rejection custody/single attempt not established: saved=%d attempts=%d err=%v", len(saved), api.createMaxAttempts, err)
	}
	calls := len(api.calls)
	if _, err := ResolveAWSGuestNetworkBoundary(t.Context(), api, boundary); err != nil {
		t.Fatal(err)
	}
	if err := CleanupAWSGuestNetwork(t.Context(), api, boundary); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != calls {
		t.Fatal("no-effect retirement called provider cleanup")
	}
	api.createErr = nil
	next, err := PrepareAWSGuestNetworkWithCustody(t.Context(), api, "cbx_0123456789ab", guestTestSubnet, awsGuestTestPolicy(), func(AWSGuestNetworkBoundary) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if next.SecurityGroupID != guestTestSG || next.CreateAttemptID == boundary.CreateAttemptID {
		t.Fatal("repaired IAM did not permit a new owned attempt")
	}
}
func TestAWSGuestNetworkCreateUnknownErrorsDoNotRetire(t *testing.T) {
	for _, failure := range []error{errors.New("UnauthorizedOperation"), context.DeadlineExceeded, &smithy.GenericAPIError{Code: "AccessDenied", Message: "UnauthorizedOperation"}, &smithy.GenericAPIError{Code: "RequestLimitExceeded"}} {
		api := newAWSGuestNetworkFake()
		api.createErr = failure
		var persisted AWSGuestNetworkBoundary
		boundary, err := PrepareAWSGuestNetworkWithCustody(t.Context(), api, "cbx_0123456789ab", guestTestSubnet, awsGuestTestPolicy(), func(b AWSGuestNetworkBoundary) error { persisted = b; return nil })
		if err == nil || boundary.CreateRejectedNoEffect || persisted.CreateRejectedNoEffect || persisted.CreateAttemptID == "" {
			t.Fatal("unknown outcome retired as a definitive no-effect rejection")
		}
	}
}

func TestAWSGuestNetworkCreateDefinitiveSDKResponseClassification(t *testing.T) {
	unauthorized := &smithy.GenericAPIError{Code: "UnauthorizedOperation", Fault: smithy.FaultClient}
	wrap := func(status int, errorCode error) error {
		return &smithy.OperationError{ServiceID: "EC2", OperationName: "CreateSecurityGroup", Err: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}}, Err: errorCode}}
	}
	if !awsGuestCreateRejectedNoEffect(wrap(403, unauthorized)) {
		t.Fatal("structured EC2 UnauthorizedOperation not recognized")
	}
	for _, err := range []error{wrap(500, unauthorized), wrap(403, &smithy.GenericAPIError{Code: "AccessDenied"}), &smithy.OperationError{ServiceID: "EC2", OperationName: "DescribeInstances", Err: unauthorized}, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Fault: smithy.FaultServer}} {
		if awsGuestCreateRejectedNoEffect(err) {
			t.Fatal("non-definitive/wrong-operation response retired as no effect")
		}
	}
}
