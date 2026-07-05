package cli

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
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

func TestValidateAWSCleanupKeyPair(t *testing.T) {
	name := "crabbox-cbx-123456abcdef"
	owned := types.KeyPairInfo{
		KeyName:   aws.String(name),
		KeyPairId: aws.String("key-0123456789abcdef0"),
		Tags: []types.Tag{
			{Key: aws.String("crabbox"), Value: aws.String("true")},
			{Key: aws.String("created_by"), Value: aws.String("crabbox")},
		},
	}
	if keyPairID, err := validateAWSCleanupKeyPair(name, []types.KeyPairInfo{owned}); err != nil || keyPairID != "key-0123456789abcdef0" {
		t.Fatalf("owned key id=%q err=%v", keyPairID, err)
	}
	unowned := owned
	unowned.Tags = []types.Tag{{Key: aws.String("crabbox"), Value: aws.String("true")}}
	_, err := validateAWSCleanupKeyPair(name, []types.KeyPairInfo{unowned})
	if err == nil || !IsAWSCleanupKeyOwnershipError(err) {
		t.Fatalf("unowned key error=%v", err)
	}
}

func TestAWSDeleteCleanupSSHKeyUsesValidatedImmutableID(t *testing.T) {
	const (
		name      = "crabbox-cbx-123456abcdef"
		keyPairID = "key-0123456789abcdef0"
	)
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, params.Get("Action"))
		switch params.Get("Action") {
		case "DescribeKeyPairs":
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>`+name+`</keyName><keyPairId>`+keyPairID+`</keyPairId><tagSet><item><key>crabbox</key><value>true</value></item><item><key>created_by</key><value>crabbox</value></item></tagSet></item></keySet></DescribeKeyPairsResponse>`)
		case "DeleteKeyPair":
			if got := params.Get("KeyPairId"); got != keyPairID {
				t.Fatalf("KeyPairId=%q, want %q", got, keyPairID)
			}
			if got := params.Get("KeyName"); got != "" {
				t.Fatalf("KeyName=%q, want empty immutable-id deletion", got)
			}
			writeEC2XML(w, `<DeleteKeyPairResponse><return>true</return></DeleteKeyPairResponse>`)
		default:
			writeEC2Error(w, "Unexpected", params.Get("Action"), http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := testAWSClient(server.URL)
	resolvedID, err := client.ResolveCleanupSSHKeyID(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteCleanupSSHKeyID(context.Background(), resolvedID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actions, []string{"DescribeKeyPairs", "DeleteKeyPair"}) {
		t.Fatalf("actions=%v", actions)
	}
}

func TestAWSEnsureSSHKeyAcceptsMatchingExistingFingerprint(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	fingerprints, err := awsImportedPublicKeyFingerprints(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, params.Get("Action"))
		switch params.Get("Action") {
		case "DescribeKeyPairs":
			if params.Get("IncludePublicKey") != "true" {
				t.Fatalf("IncludePublicKey=%q, want true", params.Get("IncludePublicKey"))
			}
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName><keyFingerprint>`+fingerprints[0]+`</keyFingerprint></item></keySet></DescribeKeyPairsResponse>`)
		case "ImportKeyPair":
			t.Fatal("ImportKeyPair should not run for matching existing key")
		default:
			writeEC2Error(w, "Unexpected", params.Get("Action"), http.StatusBadRequest)
		}
	}))
	defer server.Close()

	if err := testAWSClient(server.URL).EnsureSSHKey(context.Background(), "crabbox-test", publicKey); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actions, []string{"DescribeKeyPairs"}) {
		t.Fatalf("actions=%v, want DescribeKeyPairs only", actions)
	}
}

func TestAWSCreateRollbackDeletesOnlyNewImmutableKeyID(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	const keyPairID = "key-created-id"
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		action := params.Get("Action")
		actions = append(actions, action)
		switch action {
		case "DescribeKeyPairs":
			writeEC2Error(w, "InvalidKeyPair.NotFound", "missing", http.StatusBadRequest)
		case "ImportKeyPair":
			writeEC2XML(w, `<ImportKeyPairResponse><keyName>crabbox-test</keyName><keyPairId>`+keyPairID+`</keyPairId></ImportKeyPairResponse>`)
		case "DescribeSecurityGroups":
			writeEC2Error(w, "UnauthorizedOperation", "denied", http.StatusForbidden)
		case "DeleteKeyPair":
			if got := params.Get("KeyPairId"); got != keyPairID {
				t.Fatalf("KeyPairId=%q, want %q", got, keyPairID)
			}
			if got := params.Get("KeyName"); got != "" {
				t.Fatalf("KeyName=%q, want no name-based rollback", got)
			}
			writeEC2XML(w, `<DeleteKeyPairResponse><return>true</return></DeleteKeyPairResponse>`)
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cfg := Config{Provider: "aws", ProviderKey: "crabbox-test", AWSAMI: "ami-test", AWSSGID: "sg-test"}
	_, _, err := testAWSClient(server.URL).createServerWithFallbackInRegion(context.Background(), cfg, publicKey, "cbx_123", "rollback", false, nil)
	if err == nil {
		t.Fatal("expected create failure")
	}
	if !slices.Equal(actions, []string{"DescribeKeyPairs", "ImportKeyPair", "DescribeSecurityGroups", "DeleteKeyPair"}) {
		t.Fatalf("actions=%v", actions)
	}
}

func TestAWSCreateRollbackPreservesMatchingUnmanagedKey(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		action := params.Get("Action")
		actions = append(actions, action)
		switch action {
		case "DescribeKeyPairs":
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName><publicKey>`+publicKey+`</publicKey><keyPairId>key-unmanaged-id</keyPairId></item></keySet></DescribeKeyPairsResponse>`)
		case "DescribeSecurityGroups":
			writeEC2Error(w, "UnauthorizedOperation", "denied", http.StatusForbidden)
		case "DeleteKeyPair":
			t.Fatal("unmanaged key must not be deleted during rollback")
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cfg := Config{Provider: "aws", ProviderKey: "crabbox-test", AWSAMI: "ami-test", AWSSGID: "sg-test"}
	_, _, err := testAWSClient(server.URL).createServerWithFallbackInRegion(context.Background(), cfg, publicKey, "cbx_123", "rollback", false, nil)
	if err == nil {
		t.Fatal("expected create failure")
	}
	if !slices.Equal(actions, []string{"DescribeKeyPairs", "DescribeSecurityGroups"}) {
		t.Fatalf("actions=%v", actions)
	}
}

func TestAWSEnsureSSHKeyRejectsMismatchedExistingFingerprint(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, params.Get("Action"))
		switch params.Get("Action") {
		case "DescribeKeyPairs":
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName><keyFingerprint>SHA256:mismatched</keyFingerprint></item></keySet></DescribeKeyPairsResponse>`)
		case "ImportKeyPair":
			t.Fatal("ImportKeyPair should not run for mismatched existing key")
		default:
			writeEC2Error(w, "Unexpected", params.Get("Action"), http.StatusBadRequest)
		}
	}))
	defer server.Close()

	err := testAWSClient(server.URL).EnsureSSHKey(context.Background(), "crabbox-test", publicKey)
	if err == nil || !strings.Contains(err.Error(), "already exists with fingerprint") || !strings.Contains(err.Error(), "unique provider key") {
		t.Fatalf("err=%v, want key fingerprint mismatch", err)
	}
	if !slices.Equal(actions, []string{"DescribeKeyPairs"}) {
		t.Fatalf("actions=%v, want DescribeKeyPairs only", actions)
	}
}

func TestAWSEnsureSSHKeyRejectsMismatchedExistingPublicKey(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	otherPublicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 2))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		switch params.Get("Action") {
		case "DescribeKeyPairs":
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName><publicKey>`+otherPublicKey+`</publicKey></item></keySet></DescribeKeyPairsResponse>`)
		case "ImportKeyPair":
			t.Fatal("ImportKeyPair should not run for mismatched existing key")
		default:
			writeEC2Error(w, "Unexpected", params.Get("Action"), http.StatusBadRequest)
		}
	}))
	defer server.Close()

	err := testAWSClient(server.URL).EnsureSSHKey(context.Background(), "crabbox-test", publicKey)
	if err == nil || !strings.Contains(err.Error(), "already exists with different public key") {
		t.Fatalf("err=%v, want key material mismatch", err)
	}
}

func TestAWSEnsureSSHKeyFallsBackToFingerprintForAlternatePublicKeyEncoding(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 1))
	fingerprints, err := awsImportedPublicKeyFingerprints(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		switch params.Get("Action") {
		case "DescribeKeyPairs":
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName><publicKey>---- BEGIN SSH2 PUBLIC KEY ----</publicKey><keyFingerprint>`+fingerprints[0]+`</keyFingerprint></item></keySet></DescribeKeyPairsResponse>`)
		case "ImportKeyPair":
			t.Fatal("ImportKeyPair should not run for matching fingerprint")
		default:
			writeEC2Error(w, "Unexpected", params.Get("Action"), http.StatusBadRequest)
		}
	}))
	defer server.Close()

	if err := testAWSClient(server.URL).EnsureSSHKey(context.Background(), "crabbox-test", publicKey); err != nil {
		t.Fatal(err)
	}
}

func TestAWSImportedRSAPublicKeyFingerprintsIncludeRFC4716Blob(t *testing.T) {
	publicKey := testOpenSSHPublicKey("ssh-rsa", []byte{1, 0, 1}, testBytes(128, 7))
	_, blob, err := parseOpenSSHPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(blob) //nolint:gosec // This test pins EC2's imported RSA MD5 fingerprint contract.
	want := colonHex(sum[:])
	fingerprints, err := awsImportedPublicKeyFingerprints(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprints) == 0 || fingerprints[0] != want {
		t.Fatalf("fingerprints=%v, want first %q", fingerprints, want)
	}
	if !awsKeyFingerprintMatches("MD5:"+want, fingerprints) {
		t.Fatalf("fingerprints=%v should match MD5-prefixed value %q", fingerprints, want)
	}
}

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

func TestAWSCapacityDoctorCheckWarnsWhenQuotaBelowDefaultClass(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Class = "beast"
	cfg.ServerType = serverTypeForConfig(cfg)

	check := awsCapacityDoctorCheckForQuota(cfg, "spot", 32, true, nil)

	if check.Status != "warning" {
		t.Fatalf("status=%q, want warning", check.Status)
	}
	if check.Details["quota_code"] != awsSpotQuotaCode {
		t.Fatalf("quota_code=%q", check.Details["quota_code"])
	}
	if check.Details["default_needed_vcpus"] != "192" {
		t.Fatalf("default_needed_vcpus=%q", check.Details["default_needed_vcpus"])
	}
	if check.Details["recommended_class"] != "standard" || check.Details["recommended_type"] != "c7a.8xlarge" {
		t.Fatalf("recommendation=(%q,%q), want standard/c7a.8xlarge", check.Details["recommended_class"], check.Details["recommended_type"])
	}
	if !strings.Contains(check.Message, "capacity=quota_pressure") {
		t.Fatalf("message=%q, want quota pressure", check.Message)
	}
}

func TestAWSCapacityDoctorCheckRecommendsARM64Types(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Class = "beast"
	cfg.Architecture = ArchitectureARM64
	cfg.architectureExplicit = true
	cfg.ServerType = serverTypeForConfig(cfg)

	check := awsCapacityDoctorCheckForQuota(cfg, "spot", 32, true, nil)

	if check.Status != "warning" {
		t.Fatalf("status=%q, want warning", check.Status)
	}
	if check.Details["recommended_class"] != "standard" || check.Details["recommended_type"] != "c7g.8xlarge" {
		t.Fatalf("recommendation=(%q,%q), want standard/c7g.8xlarge", check.Details["recommended_class"], check.Details["recommended_type"])
	}
}

func TestAWSCapacityDoctorCheckPassesWhenQuotaCoversDefaultClass(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Class = "beast"
	cfg.ServerType = serverTypeForConfig(cfg)

	check := awsCapacityDoctorCheckForQuota(cfg, "spot", 256, true, nil)

	if check.Status != "ok" {
		t.Fatalf("status=%q, want ok", check.Status)
	}
	if check.Details["hint"] != "quota_satisfies_default_class" {
		t.Fatalf("hint=%q", check.Details["hint"])
	}
}

func TestAWSCapacityDoctorCheckSkipsWhenQuotaUnknown(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Class = "beast"
	cfg.ServerType = serverTypeForConfig(cfg)

	check := awsCapacityDoctorCheckForQuota(cfg, "spot", 0, false, nil)

	if check.Status != "skip" {
		t.Fatalf("status=%q, want skip", check.Status)
	}
	if check.Details["hint"] != "servicequotas_unavailable" {
		t.Fatalf("hint=%q", check.Details["hint"])
	}
	if !strings.Contains(check.Message, "capacity=unknown") {
		t.Fatalf("message=%q, want unknown capacity", check.Message)
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

func TestEnsureAWSSecurityGroupRefreshesConfiguredGroupIngress(t *testing.T) {
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		action := r.Form.Get("Action")
		actions = append(actions, strings.Join([]string{
			action,
			r.Form.Get("GroupId"),
			r.Form.Get("GroupId.1"),
			r.Form.Get("IpPermissions.1.FromPort"),
			r.Form.Get("IpPermissions.1.IpRanges.1.CidrIp"),
		}, ":"))
		switch action {
		case "DescribeSecurityGroups":
			writeEC2XML(w, `<DescribeSecurityGroupsResponse>
  <securityGroupInfo>
    <item>
      <groupId>sg-fixed</groupId>
      <ipPermissions>
        <item>
          <ipProtocol>tcp</ipProtocol>
          <fromPort>2222</fromPort>
          <toPort>2222</toPort>
          <ipRanges>
            <item>
              <cidrIp>203.0.113.10/32</cidrIp>
              <description>`+awsSSHIngressDescription+`</description>
            </item>
          </ipRanges>
        </item>
      </ipPermissions>
    </item>
  </securityGroupInfo>
</DescribeSecurityGroupsResponse>`)
		case "RevokeSecurityGroupIngress":
			writeEC2XML(w, `<RevokeSecurityGroupIngressResponse />`)
		case "AuthorizeSecurityGroupIngress":
			writeEC2XML(w, `<AuthorizeSecurityGroupIngressResponse />`)
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	groupID, err := testAWSClient(server.URL).ensureSecurityGroup(context.Background(), Config{
		Provider:         "aws",
		AWSSGID:          "sg-fixed",
		SSHPort:          "2222",
		SSHFallbackPorts: []string{"22"},
		AWSSSHCIDRs:      []string{"198.51.100.77/32"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if groupID != "sg-fixed" {
		t.Fatalf("groupID=%q, want sg-fixed", groupID)
	}
	if !slices.Contains(actions, "DescribeSecurityGroups::sg-fixed::") {
		t.Fatalf("actions=%v, want configured group describe", actions)
	}
	if !slices.Contains(actions, "AuthorizeSecurityGroupIngress:sg-fixed::2222:198.51.100.77/32") {
		t.Fatalf("actions=%v, want 2222 ingress authorization", actions)
	}
	if !slices.Contains(actions, "AuthorizeSecurityGroupIngress:sg-fixed::22:198.51.100.77/32") {
		t.Fatalf("actions=%v, want 22 ingress authorization", actions)
	}
}

func TestAWSMacOSFallbackResolvesAMIForEachInstanceType(t *testing.T) {
	var imageQueries []string
	var runImages []string
	var runTypes []string
	publicKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 3))
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
			writeEC2XML(w, `<DescribeKeyPairsResponse><keySet><item><keyName>crabbox-test</keyName><publicKey>`+publicKey+`</publicKey></item></keySet></DescribeKeyPairsResponse>`)
		case "DescribeSecurityGroups":
			writeEC2XML(w, `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-123</groupId></item></securityGroupInfo></DescribeSecurityGroupsResponse>`)
		case "RevokeSecurityGroupIngress":
			writeEC2XML(w, `<RevokeSecurityGroupIngressResponse />`)
		case "AuthorizeSecurityGroupIngress":
			writeEC2XML(w, `<AuthorizeSecurityGroupIngressResponse />`)
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

	serverRecord, resolved, err := client.createServerWithFallbackInRegion(context.Background(), cfg, publicKey, "cbx_123", "mac-test", false, nil)
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
		t.Fatalf("run image sequence=%v, want M-series mac-m candidates to use macOS 15 AMI", runImages)
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
			name:             "m1",
			instanceType:     "mac2.metal",
			wantName:         "amzn-ec2-macos-14.*-arm64",
			wantArchitecture: "arm64_mac",
		},
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

func testOpenSSHPublicKey(keyType string, parts ...[]byte) string {
	blob := appendSSHString(nil, keyType)
	for _, part := range parts {
		blob = appendSSHBytes(blob, part)
	}
	return keyType + " " + base64.StdEncoding.EncodeToString(blob) + " crabbox-test"
}

func appendSSHString(dst []byte, value string) []byte {
	return appendSSHBytes(dst, []byte(value))
}

func appendSSHBytes(dst, value []byte) []byte {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(value)))
	dst = append(dst, lenBuf[:]...)
	return append(dst, value...)
}

func testBytes(length int, start byte) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
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
