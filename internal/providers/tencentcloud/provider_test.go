package tencentcloud

import (
	"encoding/base64"
	"flag"
	"net/http"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderFlagsApply(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	provider := Provider{}
	values := provider.RegisterFlags(fs, core.Config{})
	args := []string{
		"--tencentcloud-region", "ap-guangzhou",
		"--tencentcloud-zone", "ap-guangzhou-7",
		"--tencentcloud-image", "img-test",
		"--tencentcloud-type", "S5.SMALL2",
		"--tencentcloud-vpc-id", "vpc-test",
		"--tencentcloud-subnet-id", "subnet-test",
		"--tencentcloud-security-group-id", "sg-test",
		"--tencentcloud-root-gb", "80",
		"--tencentcloud-internet-charge-type", "BANDWIDTH_POSTPAID_BY_HOUR",
		"--tencentcloud-internet-max-bandwidth-out", "10",
		"--tencentcloud-api-endpoint", "cvm.intl.tencentcloudapi.com",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.TencentCloud.Region != "ap-guangzhou" || !core.TencentCloudRegionWasExplicit(cfg) {
		t.Fatalf("region=%q explicit=%v", cfg.TencentCloud.Region, core.TencentCloudRegionWasExplicit(cfg))
	}
	if cfg.TencentCloud.Zone != "ap-guangzhou-7" || !core.TencentCloudZoneWasExplicit(cfg) {
		t.Fatalf("zone=%q explicit=%v", cfg.TencentCloud.Zone, core.TencentCloudZoneWasExplicit(cfg))
	}
	if cfg.TencentCloud.Image != "img-test" || !core.TencentCloudImageWasExplicit(cfg) {
		t.Fatalf("image=%q explicit=%v", cfg.TencentCloud.Image, core.TencentCloudImageWasExplicit(cfg))
	}
	if cfg.TencentCloud.Type != "S5.SMALL2" || !core.TencentCloudTypeWasExplicit(cfg) {
		t.Fatalf("type=%q explicit=%v", cfg.TencentCloud.Type, core.TencentCloudTypeWasExplicit(cfg))
	}
	if cfg.TencentCloud.VPCID != "vpc-test" || cfg.TencentCloud.SubnetID != "subnet-test" || cfg.TencentCloud.SecurityGroupID != "sg-test" {
		t.Fatalf("network config=%+v", cfg.TencentCloud)
	}
	if cfg.TencentCloud.RootGB != 80 || cfg.TencentCloud.InternetChargeType != "BANDWIDTH_POSTPAID_BY_HOUR" || cfg.TencentCloud.InternetMaxBandwidthOut != 10 {
		t.Fatalf("capacity config=%+v", cfg.TencentCloud)
	}
	if cfg.TencentCloud.APIEndpoint != "cvm.intl.tencentcloudapi.com" {
		t.Fatalf("api endpoint=%q", cfg.TencentCloud.APIEndpoint)
	}
}

func TestBuildRunInstanceRequest(t *testing.T) {
	cfg := cfgForRun(core.Config{
		TargetOS: core.TargetLinux,
		Class:    "standard",
		TencentCloud: core.TencentCloudConfig{
			Region:                  "ap-shanghai",
			Zone:                    "ap-shanghai-2",
			Image:                   "img-test",
			Type:                    "S5.SMALL2",
			VPCID:                   "vpc-test",
			SubnetID:                "subnet-test",
			SecurityGroupID:         "sg-test",
			RootGB:                  80,
			InternetChargeType:      "TRAFFIC_POSTPAID_BY_HOUR",
			InternetMaxBandwidthOut: 10,
		},
	})
	cfg.ProviderKey = core.ProviderKeyForLease("cbx_abcdef123456")
	cfg.ServerType = serverTypeForConfig(cfg)
	tags := leaseTags(cfg, "cbx_abcdef123456", "my-app", "provisioning", false, time.Unix(1700000000, 0))
	req := buildRunInstanceRequest(cfg, "cbx_abcdef123456", "my-app", "ssh-ed25519 AAAATEST", tags)

	if req.Placement.Zone != "ap-shanghai-2" || req.ImageID != "img-test" || req.InstanceType != "S5.SMALL2" {
		t.Fatalf("basic request=%+v", req)
	}
	if req.InstanceName != core.LeaseProviderName("cbx_abcdef123456", "my-app") {
		t.Fatalf("instance name=%q", req.InstanceName)
	}
	if req.VirtualPrivateCloud == nil || req.VirtualPrivateCloud.VPCID != "vpc-test" || req.VirtualPrivateCloud.SubnetID != "subnet-test" {
		t.Fatalf("vpc=%+v", req.VirtualPrivateCloud)
	}
	if len(req.SecurityGroupIDs) != 1 || req.SecurityGroupIDs[0] != "sg-test" {
		t.Fatalf("security groups=%v", req.SecurityGroupIDs)
	}
	if req.SystemDisk == nil || req.SystemDisk.DiskSize != 80 {
		t.Fatalf("system disk=%+v", req.SystemDisk)
	}
	if req.InternetAccessible == nil || !req.InternetAccessible.PublicIPAssigned || req.InternetAccessible.InternetMaxBandwidthOut != 10 {
		t.Fatalf("internet=%+v", req.InternetAccessible)
	}
	userData, err := base64.StdEncoding.DecodeString(req.UserData)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(userData), "ssh-ed25519 AAAATEST") {
		t.Fatalf("user data does not contain public key")
	}
	if len(req.TagSpecification) != 1 || req.TagSpecification[0].ResourceType != "instance" {
		t.Fatalf("tag spec=%+v", req.TagSpecification)
	}
}

func TestSignTencentCloudRequest(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://cvm.tencentcloudapi.com", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	signTencentCloudRequest(req, signInput{
		SecretID:  "secret-id",
		SecretKey: "secret-key",
		Service:   cvmService,
		Action:    "DescribeInstances",
		Version:   cvmVersion,
		Region:    "ap-shanghai",
		Timestamp: 1700000000,
		Payload:   []byte("{}"),
	})
	if req.Header.Get("X-TC-Action") != "DescribeInstances" || req.Header.Get("X-TC-Version") != cvmVersion || req.Header.Get("X-TC-Region") != "ap-shanghai" {
		t.Fatalf("headers=%v", req.Header)
	}
	auth := req.Header.Get("Authorization")
	for _, want := range []string{"TC3-HMAC-SHA256", "Credential=secret-id/2023-11-14/cvm/tc3_request", "SignedHeaders=content-type;host;x-tc-action", "Signature="} {
		if !strings.Contains(auth, want) {
			t.Fatalf("authorization %q missing %q", auth, want)
		}
	}
}

func TestResourceName(t *testing.T) {
	got := resourceName("ap-shanghai", "100000000001", "ins-abc")
	want := "qcs::cvm:ap-shanghai:uin/100000000001:instance/ins-abc"
	if got != want {
		t.Fatalf("resourceName=%q, want %q", got, want)
	}
}
