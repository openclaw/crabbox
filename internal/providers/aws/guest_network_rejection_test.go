package aws

import (
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
	"io"
	"strings"
	"testing"
)

type rejectingGuestNetworkAPI struct {
	core.AWSGuestNetworkAPI
	failure error
	creates int
}

func (a *rejectingGuestNetworkAPI) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{SubnetId: aws.String("subnet-11111111"), VpcId: aws.String("vpc-11111111"), State: types.SubnetStateAvailable}}}, nil
}
func (a *rejectingGuestNetworkAPI) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{VpcId: aws.String("vpc-11111111"), State: types.VpcStateAvailable}}}, nil
}
func (a *rejectingGuestNetworkAPI) CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	a.creates++
	return nil, a.failure
}
func (a *rejectingGuestNetworkAPI) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{}, nil
}
func TestGuestNetworkProviderDefinitiveRejectionRetry(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "structured unauthorized", false: "unknown timeout"}[known], func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			cfg := fixedAWSTestConfig()
			cfg.AWSAMI = "ami-11111111"
			cfg.AWSSubnetID = "subnet-11111111"
			cfg.Network = core.NetworkPublic
			cfg.Tailscale.Enabled = false
			cfg.ExposedPorts = nil
			cfg.EnvAllow = nil
			cfg.Cache.Pnpm = false
			cfg.Cache.Npm = false
			cfg.Cache.Docker = false
			cfg.Cache.Git = false
			cfg.Cache.Volumes = nil
			p := core.GuestNetworkPolicy{Version: 1, ID: "example-policy", Revision: "1", Egress: []core.GuestNetworkRule{{CIDR: "1.1.1.1/32", Ports: []int{443}}}, SSHSources: []string{"8.8.8.8/32"}, PlatformExceptions: []string{core.AWSGuestNetworkPlatformServices}}
			cfg.GuestNetworkPolicy = &p
			digest, err := core.GuestNetworkPolicyDigest(p)
			if err != nil {
				t.Fatal(err)
			}
			api := &rejectingGuestNetworkAPI{failure: context.DeadlineExceeded}
			if known {
				api.failure = &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "synthetic denial", Fault: smithy.FaultClient}
			}
			base := &fakeAWSClient{accountID: "000000000000"}
			client := &guestReceiptAWSClient{fakeAWSClient: base}
			calls := 0
			base.controlCreate = func(control *core.AWSFixedCreateControl, config core.Config, id, slug string) (core.Server, core.Config, error) {
				calls++
				if calls == 1 {
					_, err := core.PrepareAWSGuestNetworkWithCustody(t.Context(), api, id, "subnet-11111111", p, control.GuestNetworkCustody)
					return core.Server{}, config, err
				}
				attempt := fixedAWSTestAttempt(config)
				if err := control.BeforeAttempt(attempt); err != nil {
					return core.Server{}, config, err
				}
				server := fixedAWSTestServer(control, config, id, slug, attempt)
				base.created = server
				base.servers = []core.Server{server}
				client.receipt = &core.GuestNetworkReceipt{Version: 1, PolicyID: p.ID, PolicyRevision: p.Revision, PolicyDigest: digest, LeaseID: id, Provider: "aws", ResourceID: server.CloudID, BoundaryID: "sg-11111111"}
				return server, config, nil
			}
			restore := installFixedAWSTestClient(t, base)
			t.Cleanup(restore)
			newAWSClient = func(context.Context, core.Config) (awsClient, error) { return client, nil }
			backend := NewAWSLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			req := core.AcquireRequest{Repo: core.Repo{Root: dirs.Root}, Keep: true, RequestedLeaseID: "cbx_abcdef123480", RequestedSlug: "guest-rejection"}
			if _, err := backend.Acquire(t.Context(), req); err == nil {
				t.Fatal("expected first provider rejection")
			} else if calls != 1 {
				t.Fatalf("failed before provider mutation: %v", err)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			raw := claim.FixedCreateIntent.Companions[awsGuestCompanionKey]
			var observed awsGuestCompanion
			if raw == "" || json.Unmarshal([]byte(raw), &observed) != nil || observed.Boundary.CreateRejectedNoEffect != known {
				t.Fatalf("wrong durable creation outcome: known=%v", known)
			}
			_, err = backend.Acquire(t.Context(), req)
			if known {
				if err != nil {
					t.Fatal("IAM repair retry did not acquire:", err)
				}
				if calls != 2 {
					t.Fatal("repaired request never reached provider")
				}
			} else {
				if err == nil || calls != 1 {
					t.Fatal("prior unknown attempt was overwritten or retried")
				}
			}
			if api.creates != 1 {
				t.Fatal("single rejected request was retried by provider primitive")
			}
		})
	}
}
func TestGuestNetworkTerminalDefinitiveRejectionNeedsNoProviderCleanup(t *testing.T) {
	b := core.AWSGuestNetworkBoundary{CreateAttemptID: "attempt-1", CreateRejectedNoEffect: true, LeaseID: "cbx_0123456789ab", PolicyDigest: strings.Repeat("a", 64), SubnetID: "subnet-11111111", VPCID: "vpc-11111111", SecurityGroupName: "owned-name"}
	if !core.AWSGuestNetworkCreateRejectedNoEffect(b) {
		t.Fatal("fixture invalid")
	}
	testutil.IsolateUserDirs(t)
	old := newAWSClient
	newAWSClient = func(context.Context, core.Config) (awsClient, error) {
		t.Fatal("no-effect cleanup contacted provider")
		return nil, nil
	}
	t.Cleanup(func() { newAWSClient = old })
	raw, err := json.Marshal(awsGuestCompanion{Version: 1, Region: "us-east-1", AccountID: "000000000000", Boundary: b})
	if err != nil {
		t.Fatal(err)
	}
	claim := core.LeaseClaim{LeaseID: b.LeaseID, FixedCreateIntent: &core.FixedCreateIntent{Companions: map[string]string{awsGuestCompanionKey: string(raw), "other-guard": "retain"}}}
	if err := cleanupAWSFixedTerminal(claim); err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent.Companions["other-guard"] != "retain" {
		t.Fatal("unrelated guard reference changed")
	}
	b.CreateAttemptID = ""
	if core.AWSGuestNetworkCreateRejectedNoEffect(b) {
		t.Fatal("unfenced no-effect marker accepted")
	}
}
