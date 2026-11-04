package cli

import (
	"context"
	"encoding/base64"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"strings"
	"testing"
)

type guestBootstrapAPIFake struct {
	*awsGuestNetworkFake
	id, data, console, consoleID string
}

func (f *guestBootstrapAPIFake) DescribeInstanceAttribute(_ context.Context, in *ec2.DescribeInstanceAttributeInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceAttributeOutput, error) {
	return &ec2.DescribeInstanceAttributeOutput{InstanceId: aws.String(f.id), UserData: &types.AttributeValue{Value: aws.String(f.data)}}, nil
}
func (f *guestBootstrapAPIFake) GetConsoleOutput(_ context.Context, in *ec2.GetConsoleOutputInput, _ ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error) {
	return &ec2.GetConsoleOutputOutput{InstanceId: aws.String(f.consoleID), Output: aws.String(base64.StdEncoding.EncodeToString([]byte(f.console)))}, nil
}
func TestGuestBootstrapAuthoritativeObservation(t *testing.T) {
	for _, mutation := range []string{"valid", "userdata", "attribute-resource", "console-resource", "console-absent", "console-ambiguous", "tokens", "hop", "metadata-tags", "ipv6", "mode", "boundary-mode", "pin-invalid"} {
		t.Run(mutation, func(t *testing.T) {
			p := managedGuestTestPolicy()
			key := guestBootstrapTestPublicKey(t)
			data, err := GuestPublicKeyUserData(key+" SECRET_COMMENT_MUST_NOT_SURVIVE", "ubuntu", "/work/project")
			if err != nil {
				t.Fatal(err)
			}
			plain, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(plain), "SECRET_COMMENT") || strings.Contains(string(plain), "PRIVATE KEY") {
				t.Fatal("nonpublic material in public-only template")
			}
			if !strings.Contains(string(plain), key) || !strings.Contains(string(plain), "CRABBOX_GUEST_HOST_KEY_V1") {
				t.Fatal("missing public bootstrap material")
			}
			hash, err := guestBootstrapHash(data)
			if err != nil {
				t.Fatal(err)
			}
			id := "i-0123456789abcdef0"
			api := &guestBootstrapAPIFake{awsGuestNetworkFake: newAWSGuestNetworkFake(), id: id, data: data, consoleID: id, console: "CRABBOX_GUEST_HOST_KEY_V1 " + key}
			b := AWSGuestNetworkBoundary{BootstrapMode: p.BootstrapMode, BootstrapSHA256: hash}
			instance := types.Instance{InstanceId: aws.String(id), MetadataOptions: &types.InstanceMetadataOptionsResponse{HttpEndpoint: types.InstanceMetadataEndpointStateEnabled, HttpTokens: types.HttpTokensStateRequired, HttpPutResponseHopLimit: aws.Int32(1), InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled, HttpProtocolIpv6: types.InstanceMetadataProtocolStateDisabled}}
			switch mutation {
			case "userdata":
				api.data = base64.StdEncoding.EncodeToString([]byte("modified"))
			case "attribute-resource":
				api.id = "i-other"
			case "console-resource":
				api.consoleID = "i-other"
			case "console-absent":
				api.console = "not ready"
			case "console-ambiguous":
				api.console += "\nCRABBOX_GUEST_HOST_KEY_V1 " + guestBootstrapTestPublicKey(t)
			case "tokens":
				instance.MetadataOptions.HttpTokens = types.HttpTokensStateOptional
			case "hop":
				instance.MetadataOptions.HttpPutResponseHopLimit = aws.Int32(2)
			case "metadata-tags":
				instance.MetadataOptions.InstanceMetadataTags = types.InstanceMetadataTagsStateEnabled
			case "ipv6":
				instance.MetadataOptions.HttpProtocolIpv6 = types.InstanceMetadataProtocolStateEnabled
			case "mode":
				p.BootstrapMode = "unknown"
			case "boundary-mode":
				b.BootstrapMode = ""
			case "pin-invalid":
				instance.Tags = []types.Tag{{Key: aws.String("guest_network_ssh_host_key"), Value: aws.String("untrusted")}}
			}
			facts, err := observeAWSGuestBootstrap(context.Background(), api, b, p, instance)
			if mutation == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if facts["sshHostKey"] != key || facts["bootstrapSha256"] != hash || facts["imds"] != "roleless-v2" {
					t.Fatal("unbound bootstrap facts")
				}
			} else if err == nil {
				t.Fatal("accepted mismatched authoritative bootstrap facts")
			}
		})
	}
}
