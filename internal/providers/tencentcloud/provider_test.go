package tencentcloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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

func TestTagUpdateSetUsesTagAPIFieldNames(t *testing.T) {
	got := tagUpdateSet([]tag{{Key: "state", Value: "ready"}, {Key: "", Value: "skip"}})
	if len(got) != 1 {
		t.Fatalf("tag updates=%+v", got)
	}
	if got[0].TagKey != "state" || got[0].TagValue != "ready" {
		t.Fatalf("tag update=%+v", got[0])
	}
}

func TestTagDeleteSetDeletesObsoleteManagedTagsOnly(t *testing.T) {
	got := tagDeleteSet(
		[]tag{
			{Key: "crabbox", Value: "true"},
			{Key: "provider", Value: providerName},
			{Key: "state", Value: "ready"},
			{Key: "tailscale_error", Value: "old failure"},
			{Key: "owner", Value: "external"},
		},
		[]tag{
			{Key: "crabbox", Value: "true"},
			{Key: "provider", Value: providerName},
			{Key: "state", Value: "ready"},
		},
	)
	if len(got) != 1 || got[0].TagKey != "tailscale_error" {
		t.Fatalf("tag deletes=%+v", got)
	}
}

func TestReplaceInstanceTagsSendsDeleteTagsForObsoleteManagedTags(t *testing.T) {
	var payload struct {
		Resource    string      `json:"Resource"`
		ReplaceTags []tagUpdate `json:"ReplaceTags"`
		DeleteTags  []tagDelete `json:"DeleteTags"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-TC-Action") != "ModifyResourceTags" {
			t.Fatalf("X-TC-Action=%q", r.Header.Get("X-TC-Action"))
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"Response":{"RequestId":"req-test"}}`))
	}))
	defer server.Close()

	client := &client{
		secretID:     "secret-id",
		secretKey:    "secret-key",
		httpClient:   server.Client(),
		region:       "ap-shanghai",
		tagEndpoint:  server.URL,
		accountID:    "100000000001",
		accountReady: true,
	}
	err := client.ReplaceInstanceTags(
		context.Background(),
		"ins-test",
		[]tag{
			{Key: "crabbox", Value: "true"},
			{Key: "provider", Value: providerName},
			{Key: "tailscale_error", Value: "old failure"},
			{Key: "owner", Value: "external"},
		},
		[]tag{
			{Key: "crabbox", Value: "true"},
			{Key: "provider", Value: providerName},
			{Key: "state", Value: "ready"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Resource != "qcs::cvm:ap-shanghai:uin/100000000001:instance/ins-test" {
		t.Fatalf("resource=%q", payload.Resource)
	}
	if len(payload.DeleteTags) != 1 || payload.DeleteTags[0].TagKey != "tailscale_error" {
		t.Fatalf("delete tags=%+v", payload.DeleteTags)
	}
	for _, item := range payload.DeleteTags {
		if item.TagKey == "owner" {
			t.Fatalf("external tag was deleted: %+v", payload.DeleteTags)
		}
	}
	if len(payload.ReplaceTags) == 0 || payload.ReplaceTags[0].TagKey == "" {
		t.Fatalf("replace tags=%+v", payload.ReplaceTags)
	}
}

func TestInstanceDecodesTencentCloudTags(t *testing.T) {
	var got instance
	if err := json.Unmarshal([]byte(`{"InstanceId":"ins-test","Tags":[{"Key":"crabbox","Value":"true"}]}`), &got); err != nil {
		t.Fatal(err)
	}
	labels := labelsFromTags(got.Tags)
	if labels["crabbox"] != "true" {
		t.Fatalf("labels=%v", labels)
	}
}

func TestUpdateTailscaleMetadataPassesCurrentAndDesiredTags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "SA5.MEDIUM2"
	cfg.ProviderKey = core.ProviderKeyForLease("cbx_abcdef123456")
	tags := leaseTags(cfg, "cbx_abcdef123456", "tailnet", "ready", false, time.Unix(1700000000, 0))
	tags = append(tags, tag{Key: "tailscale_error", Value: "old failure"})
	item := instance{
		InstanceID:    "ins-test",
		InstanceName:  core.LeaseProviderName("cbx_abcdef123456", "tailnet"),
		InstanceState: "RUNNING",
		Tags:          tags,
	}
	api := &fakeTencentCloudAPI{item: item}
	backend := &Backend{
		DirectSSHBackend: shared.DirectSSHBackend{Cfg: cfg},
		clientFactory: func(core.Config, core.Runtime) (tencentCloudAPI, error) {
			return api, nil
		},
	}
	server := serverFromInstance(item, cfg)
	updated, err := backend.UpdateTailscaleMetadata(context.Background(), core.LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456"}, core.TailscaleMetadata{
		Enabled: true,
		State:   "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updated.Labels["tailscale_error"]; ok {
		t.Fatalf("updated labels still include tailscale_error: %v", updated.Labels)
	}
	if !hasTag(api.replacedCurrent, "tailscale_error") {
		t.Fatalf("current tags did not include stale error: %+v", api.replacedCurrent)
	}
	if hasTag(api.replacedDesired, "tailscale_error") {
		t.Fatalf("desired tags still include stale error: %+v", api.replacedDesired)
	}
}

type fakeTencentCloudAPI struct {
	item            instance
	replacedCurrent []tag
	replacedDesired []tag
}

func (f *fakeTencentCloudAPI) AccountID(context.Context) (string, error) {
	return "100000000001", nil
}

func (f *fakeTencentCloudAPI) ListInstances(context.Context) ([]instance, error) {
	return []instance{f.item}, nil
}

func (f *fakeTencentCloudAPI) GetInstance(context.Context, string) (instance, error) {
	return f.item, nil
}

func (f *fakeTencentCloudAPI) RunInstance(context.Context, runInstanceRequest) (string, error) {
	return "ins-test", nil
}

func (f *fakeTencentCloudAPI) TerminateInstance(context.Context, string) error {
	return nil
}

func (f *fakeTencentCloudAPI) ReplaceInstanceTags(_ context.Context, _ string, current, desired []tag) error {
	f.replacedCurrent = append([]tag(nil), current...)
	f.replacedDesired = append([]tag(nil), desired...)
	f.item.Tags = append([]tag(nil), desired...)
	return nil
}

func hasTag(tags []tag, key string) bool {
	for _, item := range tags {
		if item.Key == key {
			return true
		}
	}
	return false
}
