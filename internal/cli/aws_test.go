package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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

func TestAWSInstanceToServerPreservesHostID(t *testing.T) {
	server := awsInstanceToServer(types.Instance{
		InstanceId:   aws.String("i-1234567890abcdef0"),
		InstanceType: types.InstanceTypeMac2Metal,
		Placement:    &types.Placement{HostId: aws.String("h-000000000001")},
		State:        &types.InstanceState{Name: types.InstanceStateNameRunning},
	})

	if server.HostID != "h-000000000001" {
		t.Fatalf("HostID=%q, want h-000000000001", server.HostID)
	}
}

func TestRetryableAWSSnapshotDeleteError(t *testing.T) {
	for _, message := range []string{
		"InvalidSnapshot.InUse: snapshot is currently in use by ami-123",
		"RequestLimitExceeded: request rate exceeded",
		"ThrottlingException: slow down",
		"ServiceUnavailable: try again",
		"InternalError: internal failure",
		"http 500: server error",
		"Snapshot snap-123 is currently in use",
	} {
		if !isRetryableAWSSnapshotDeleteError(message) {
			t.Fatalf("message %q should be retryable", message)
		}
	}
	if isRetryableAWSSnapshotDeleteError("AuthFailure: not authorized") {
		t.Fatal("AuthFailure should not be retryable")
	}
}

func TestCreateImageCheckpointRecordsCallerAccount(t *testing.T) {
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch action := r.Form.Get("Action"); action {
		case "GetCallerIdentity":
			writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDAEXAMPLE</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case "CreateImage":
			sawCreate = true
			writeEC2XML(w, `<CreateImageResponse><imageId>ami-12345678</imageId></CreateImageResponse>`)
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := testAWSClient(server.URL)
	image, err := client.CreateImageCheckpoint(context.Background(), "i-1234567890abcdef0", "checkpoint", true)
	if err != nil {
		t.Fatal(err)
	}
	if !sawCreate {
		t.Fatal("CreateImage was not called")
	}
	if image.AccountID != "123456789012" {
		t.Fatalf("AccountID=%q, want caller account", image.AccountID)
	}
}

func TestValidateImageCheckpointSourceChecksAccountAndInstance(t *testing.T) {
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		action := r.Form.Get("Action")
		actions = append(actions, action)
		switch action {
		case "GetCallerIdentity":
			writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDAEXAMPLE</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case "DescribeInstances":
			writeEC2XML(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-1234567890abcdef0</instanceId><instanceType>t3.micro</instanceType><ipAddress>203.0.113.44</ipAddress><instanceState><name>running</name></instanceState></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		case "CreateImage":
			t.Fatal("CreateImage must not be part of source validation")
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	accountID, err := testAWSClient(server.URL).ValidateImageCheckpointSource(context.Background(), "i-1234567890abcdef0")
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "123456789012" {
		t.Fatalf("accountID=%q, want caller account", accountID)
	}
	if got := strings.Join(actions, ","); got != "GetCallerIdentity,DescribeInstances" {
		t.Fatalf("actions=%s, want GetCallerIdentity,DescribeInstances", got)
	}
}

func TestValidateImageCheckpointSourceRejectsMissingInstance(t *testing.T) {
	var sawDescribe bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch action := r.Form.Get("Action"); action {
		case "GetCallerIdentity":
			writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDAEXAMPLE</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case "DescribeInstances":
			sawDescribe = true
			writeEC2XML(w, `<DescribeInstancesResponse><reservationSet></reservationSet></DescribeInstancesResponse>`)
		case "CreateImage":
			t.Fatal("CreateImage must not run when the source instance is missing")
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	_, err := testAWSClient(server.URL).ValidateImageCheckpointSource(context.Background(), "i-missing")
	if err == nil || !strings.Contains(err.Error(), "aws instance not found") {
		t.Fatalf("err=%v, want missing instance validation error", err)
	}
	if !sawDescribe {
		t.Fatal("DescribeInstances was not called")
	}
}

func TestDeleteImageCheckpointRefusesNotFoundWithoutAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if action := r.Form.Get("Action"); action != "DescribeImages" {
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
			return
		}
		writeEC2Error(w, "InvalidAMIID.NotFound", "image not found", http.StatusBadRequest)
	}))
	defer server.Close()

	err := testAWSClient(server.URL).DeleteImageCheckpoint(context.Background(), "ami-12345678", nil, "")
	if err == nil || !strings.Contains(err.Error(), "checkpoint record has no accountId") {
		t.Fatalf("err=%v, want account guard error", err)
	}
}

func TestDeleteImageCheckpointRefusesAccountMismatchBeforeDescribe(t *testing.T) {
	var describeHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch action := r.Form.Get("Action"); action {
		case "GetCallerIdentity":
			writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>999999999999</Account><Arn>arn:aws:iam::999999999999:user/test</Arn><UserId>AIDAEXAMPLE</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case "DescribeImages":
			describeHits++
			writeEC2XML(w, `<DescribeImagesResponse><imagesSet></imagesSet></DescribeImagesResponse>`)
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	err := testAWSClient(server.URL).DeleteImageCheckpoint(context.Background(), "ami-12345678", nil, "123456789012")
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("err=%v, want account mismatch", err)
	}
	if describeHits != 0 {
		t.Fatalf("DescribeImages called %d time(s), want zero", describeHits)
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
	var imageQueries []string
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
			name := params.Get("Filter.2.Value.1")
			imageQueries = append(imageQueries, name+":"+architecture)
			imageID := "ami-arm64"
			if architecture == "x86_64_mac" {
				imageID = "ami-x86"
			} else if name == "amzn-ec2-macos-15.*-arm64" {
				imageID = "ami-m4"
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
	if !slices.Contains(runImages, "ami-m4") {
		t.Fatalf("run image sequence=%v, want M4 candidates to use macOS 15 AMI", runImages)
	}
	wantQueries := make([]string, 0, len(awsMacOSInstanceTypeCandidates()))
	for _, instanceType := range awsMacOSInstanceTypeCandidates() {
		name, architecture := awsMacOSAMIQueryForInstanceType(instanceType)
		wantQueries = append(wantQueries, name+":"+architecture)
	}
	if !stringSlicesEqual(imageQueries, wantQueries) {
		t.Fatalf("image queries=%v, want %v", imageQueries, wantQueries)
	}
	if len(runTypes) != len(awsMacOSInstanceTypeCandidates()) || runTypes[0] != "mac2.metal" || runTypes[len(runTypes)-1] != "mac1.metal" {
		t.Fatalf("run type sequence=%v, want macOS fallback list ending with mac1", runTypes)
	}
}

func TestAWSMacOSAMIQueryForInstanceType(t *testing.T) {
	tests := []struct {
		name             string
		instanceType     string
		wantName         string
		wantArchitecture string
	}{
		{
			name:             "m2",
			instanceType:     "mac2-m2pro.metal",
			wantName:         "amzn-ec2-macos-14.*-arm64",
			wantArchitecture: "arm64_mac",
		},
		{
			name:             "m3-ultra",
			instanceType:     "mac-m3ultra.metal",
			wantName:         "amzn-ec2-macos-15.*-arm64",
			wantArchitecture: "arm64_mac",
		},
		{
			name:             "m4",
			instanceType:     "mac-m4pro.metal",
			wantName:         "amzn-ec2-macos-15.*-arm64",
			wantArchitecture: "arm64_mac",
		},
		{
			name:             "x86",
			instanceType:     "mac1.metal",
			wantName:         "amzn-ec2-macos-14.*",
			wantArchitecture: "x86_64_mac",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotArchitecture := awsMacOSAMIQueryForInstanceType(tt.instanceType)
			if gotName != tt.wantName || gotArchitecture != tt.wantArchitecture {
				t.Fatalf("query=(%q,%q), want (%q,%q)", gotName, gotArchitecture, tt.wantName, tt.wantArchitecture)
			}
		})
	}
}

func testAWSClient(endpoint string) *AWSClient {
	cfg := aws.Config{
		Region:       "eu-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "secret", ""),
		BaseEndpoint: aws.String(endpoint),
	}
	return &AWSClient{
		ec2:    ec2.NewFromConfig(cfg),
		sts:    sts.NewFromConfig(cfg),
		region: "eu-west-1",
	}
}

func writeEC2XML(w http.ResponseWriter, body string) {
	w.Header().Set("content-type", "text/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + body))
}

func writeSTSXML(w http.ResponseWriter, body string) {
	w.Header().Set("content-type", "text/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + body))
}

func writeEC2Error(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("content-type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>` + code + `</Code><Message>` + message + `</Message></Error></Errors></Response>`))
}
