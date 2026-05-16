package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestApplyAWSRunInstanceTargetOptionsEnablesNestedVirtualizationForWSL2(t *testing.T) {
	input := &ec2.RunInstancesInput{}
	applyAWSRunInstanceTargetOptions(input, Config{
		TargetOS:    targetWindows,
		WindowsMode: windowsModeWSL2,
	})
	if input.CpuOptions == nil {
		t.Fatal("CpuOptions=nil, want nested virtualization enabled")
	}
	if input.CpuOptions.NestedVirtualization != types.NestedVirtualizationSpecificationEnabled {
		t.Fatalf("NestedVirtualization=%q", input.CpuOptions.NestedVirtualization)
	}
}

func TestApplyAWSRunInstanceTargetOptionsLeavesNativeWindowsDefault(t *testing.T) {
	input := &ec2.RunInstancesInput{}
	applyAWSRunInstanceTargetOptions(input, Config{
		TargetOS:    targetWindows,
		WindowsMode: windowsModeNormal,
	})
	if input.CpuOptions != nil {
		t.Fatalf("CpuOptions=%#v, want nil", input.CpuOptions)
	}
}

func TestStaleAWSCrabboxSSHIngressPermissionsPrunesOnlyOwnedCIDRs(t *testing.T) {
	group := types.SecurityGroup{
		IpPermissions: []types.IpPermission{
			{
				FromPort:   aws.Int32(2222),
				ToPort:     aws.Int32(2222),
				IpProtocol: aws.String("tcp"),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("203.0.113.10/32"), Description: aws.String(awsSSHIngressDescription)},
					{CidrIp: aws.String("198.51.100.20/32"), Description: aws.String(awsSSHIngressDescription)},
					{CidrIp: aws.String("192.0.2.30/32"), Description: aws.String("operator access")},
				},
				Ipv6Ranges: []types.Ipv6Range{
					{CidrIpv6: aws.String("2001:db8::1/128"), Description: aws.String(awsSSHIngressDescription)},
				},
			},
			{
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpProtocol: aws.String("tcp"),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("198.51.100.20/32"), Description: aws.String(awsSSHIngressDescription)},
				},
			},
			{
				FromPort:   aws.Int32(443),
				ToPort:     aws.Int32(443),
				IpProtocol: aws.String("tcp"),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("198.51.100.20/32"), Description: aws.String("operator access")},
				},
			},
		},
	}

	stale := staleAWSCrabboxSSHIngressPermissions(group, []string{"2222"}, []string{"203.0.113.10/32"})
	if len(stale) != 2 {
		t.Fatalf("len(stale)=%d, want 2: %#v", len(stale), stale)
	}
	byPort := map[int32]types.IpPermission{}
	for _, permission := range stale {
		byPort[aws.ToInt32(permission.FromPort)] = permission
	}
	currentPort := byPort[2222]
	if len(currentPort.IpRanges) != 1 || aws.ToString(currentPort.IpRanges[0].CidrIp) != "198.51.100.20/32" {
		t.Fatalf("IpRanges=%#v, want only stale Crabbox IPv4 range on current port", currentPort.IpRanges)
	}
	if len(currentPort.Ipv6Ranges) != 1 || aws.ToString(currentPort.Ipv6Ranges[0].CidrIpv6) != "2001:db8::1/128" {
		t.Fatalf("Ipv6Ranges=%#v, want only stale Crabbox IPv6 range on current port", currentPort.Ipv6Ranges)
	}
	removedPort := byPort[22]
	if len(removedPort.IpRanges) != 1 || aws.ToString(removedPort.IpRanges[0].CidrIp) != "198.51.100.20/32" {
		t.Fatalf("removed port IpRanges=%#v, want Crabbox ranges pruned from removed port", removedPort.IpRanges)
	}
}

func TestStaleAWSCrabboxSSHIngressPermissionsKeepsDefaultCIDR(t *testing.T) {
	group := types.SecurityGroup{
		IpPermissions: []types.IpPermission{
			{
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpProtocol: aws.String("tcp"),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String(awsSSHIngressDescription)},
				},
			},
		},
	}

	stale := staleAWSCrabboxSSHIngressPermissions(group, []string{"22"}, nil)
	if len(stale) != 0 {
		t.Fatalf("stale=%#v, want none for default fallback CIDR", stale)
	}
}

func TestAWSMacOSFallbackResolvesAMIForEachInstanceType(t *testing.T) {
	var runImages []string
	var runTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		switch action := params.Get("Action"); action {
		case "DescribeKeyPairs":
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName></item></keySet></DescribeKeyPairsResponse>`)
		case "DescribeImages":
			architecture := params.Get("Filter.1.Value.1")
			imageID := "ami-arm64"
			if architecture == "x86_64_mac" {
				imageID = "ami-x86"
			}
			writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>`+imageID+`</imageId><name>macos</name><creationDate>2026-05-01T00:00:00Z</creationDate></item></imagesSet></DescribeImagesResponse>`)
		case "RunInstances":
			instanceType := params.Get("InstanceType")
			runTypes = append(runTypes, instanceType)
			runImages = append(runImages, params.Get("ImageId"))
			if instanceType != "mac1.metal" {
				writeEC2Error(w, "InvalidParameterValue", "host does not support instance type", http.StatusBadRequest)
				return
			}
			writeEC2XML(w, `<RunInstancesResponse><instancesSet><item><instanceId>i-mac1</instanceId><instanceType>mac1.metal</instanceType><ipAddress>203.0.113.44</ipAddress><instanceState><name>pending</name></instanceState><placement><hostId>h-mac1</hostId></placement></item></instancesSet></RunInstancesResponse>`)
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := &AWSClient{
		ec2: ec2.NewFromConfig(aws.Config{
			Region:       "eu-west-1",
			Credentials:  credentials.NewStaticCredentialsProvider("test", "secret", ""),
			BaseEndpoint: aws.String(server.URL),
		}),
		region: "eu-west-1",
	}
	cfg := Config{
		Provider:    "aws",
		TargetOS:    targetMacOS,
		ServerType:  "mac2.metal",
		HostID:      "h-mac1",
		ProviderKey: "crabbox-test",
		AWSSGID:     "sg-123",
		Capacity: CapacityConfig{
			Market: "on-demand",
		},
		SSHPort: "22",
	}

	serverRecord, resolved, err := client.createServerWithFallbackInRegion(context.Background(), cfg, "ssh-ed25519 test", "cbx_123", "mac-test", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if serverRecord.ServerType.Name != "mac1.metal" || resolved.ServerType != "mac1.metal" {
		t.Fatalf("server type=%q resolved=%q, want mac1.metal", serverRecord.ServerType.Name, resolved.ServerType)
	}
	if len(runImages) != len(awsMacOSInstanceTypeCandidates()) || runImages[0] != "ami-arm64" || runImages[len(runImages)-1] != "ami-x86" {
		t.Fatalf("run image sequence=%v, want arm64 candidates ending with x86 mac1 AMI", runImages)
	}
	if len(runTypes) != len(awsMacOSInstanceTypeCandidates()) || runTypes[0] != "mac2.metal" || runTypes[len(runTypes)-1] != "mac1.metal" {
		t.Fatalf("run type sequence=%v, want macOS fallback list ending with mac1", runTypes)
	}
}

func writeEC2XML(w http.ResponseWriter, body string) {
	w.Header().Set("content-type", "text/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + body))
}

func writeEC2Error(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("content-type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>` + code + `</Code><Message>` + message + `</Message></Error></Errors></Response>`))
}
