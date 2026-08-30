package tencentcloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestServerTypeForConfigHonorsClassAndTypeProvenance(t *testing.T) {
	provider := Provider{}
	defaults := core.BaseConfig()
	defaults.Provider = providerName
	defaults.TencentCloud.Type = ""
	if got := provider.ServerTypeForConfig(defaults); got != defaultType {
		t.Fatalf("inherited class server type=%q want provider default %q", got, defaultType)
	}

	providerDefault := defaults
	providerDefault.TencentCloud.Type = "S5.SMALL2"
	if got := provider.ServerTypeForConfig(providerDefault); got != "S5.SMALL2" {
		t.Fatalf("non-explicit provider default=%q", got)
	}

	explicitClass := defaults
	explicitClass.Class = "fast"
	core.MarkClassExplicit(&explicitClass)
	if got := provider.ServerTypeForConfig(explicitClass); got != "SA5.LARGE8" {
		t.Fatalf("explicit class server type=%q", got)
	}

	explicitProviderType := explicitClass
	explicitProviderType.TencentCloud.Type = "S5.SMALL2"
	core.SetTencentCloudTypeExplicit(&explicitProviderType)
	if got := provider.ServerTypeForConfig(explicitProviderType); got != "S5.SMALL2" {
		t.Fatalf("explicit provider type=%q", got)
	}

	explicitGenericType := explicitProviderType
	explicitGenericType.ServerType = "S6.MEDIUM4"
	explicitGenericType.ServerTypeExplicit = true
	if got := provider.ServerTypeForConfig(explicitGenericType); got != "S6.MEDIUM4" {
		t.Fatalf("explicit generic type=%q", got)
	}
}

func TestServerTypeForConfigUsesExplicitCLIClass(t *testing.T) {
	provider := Provider{}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TencentCloud.Type = ""
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	class := fs.String("class", cfg.Class, "machine class")
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--class", "fast"}); err != nil {
		t.Fatal(err)
	}
	cfg.Class = *class
	core.MarkClassExplicit(&cfg)
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if got := provider.ServerTypeForConfig(cfg); got != "SA5.LARGE8" {
		t.Fatalf("server type=%q want SA5.LARGE8", got)
	}
}

func TestCfgForRunPreservesLoadedMachineTypePrecedence(t *testing.T) {
	fastType, ok := providerClassType("fast")
	if !ok {
		t.Fatal("fast class profile is missing")
	}
	tests := []struct {
		name            string
		content         string
		classEnv        string
		wantClass       string
		wantType        string
		wantClassIntent bool
	}{
		{name: "inherited default", content: "provider: tencentcloud\n", wantClass: "beast", wantType: defaultType},
		{name: "YAML class", content: "provider: tencentcloud\nclass: fast\n", wantClass: "fast", wantType: fastType, wantClassIntent: true},
		{name: "environment class", content: "provider: tencentcloud\n", classEnv: "fast", wantClass: "fast", wantType: fastType, wantClassIntent: true},
		{name: "explicit Tencent type", content: "provider: tencentcloud\nclass: fast\ntencentcloud:\n  type: S5.SMALL2\n", wantClass: "fast", wantType: "S5.SMALL2", wantClassIntent: true},
		{name: "explicit generic type", content: "provider: tencentcloud\nclass: fast\nserverType: S6.MEDIUM4\n", wantClass: "fast", wantType: "S6.MEDIUM4", wantClassIntent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_DEFAULT_CLASS", test.classEnv)
			cfg, err := core.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Class != test.wantClass || core.ClassWasExplicit(cfg) != test.wantClassIntent {
				t.Fatalf("loaded class=%q explicit=%v", cfg.Class, core.ClassWasExplicit(cfg))
			}
			if cfg.TencentCloud.Type == "" {
				t.Fatal("LoadConfig did not populate TencentCloud.Type")
			}
			runtime := cfgForRun(cfg)
			if runtime.TencentCloud.Type != test.wantType || runtime.ServerType != test.wantType {
				t.Fatalf("runtime type=%q serverType=%q want %q", runtime.TencentCloud.Type, runtime.ServerType, test.wantType)
			}
		})
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

func TestTencentCloudReleaseRequiresExactClaimedInstance(t *testing.T) {
	for _, test := range []struct {
		name      string
		seedClaim bool
		staleID   bool
	}{
		{name: "missing claim"},
		{name: "stale instance claim", seedClaim: true, staleID: true},
		{name: "exact claim", seedClaim: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			leaseID := "cbx_abcdef123456"
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
			tags := leaseTags(cfg, leaseID, "owned", "ready", false, time.Now().UTC())
			tags = append(tags, tag{Key: accountLabel, Value: "100000000001"})
			item := instance{InstanceID: "ins-owned", InstanceName: core.LeaseProviderName(leaseID, "owned"), InstanceState: "RUNNING", Tags: tags}
			api := &fakeTencentCloudAPI{item: item}
			backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*Backend)
			backend.clientFactory = func(core.Config, core.Runtime) (tencentCloudAPI, error) { return api, nil }
			server := serverFromInstance(item, backend.Cfg)
			if test.seedClaim {
				claimed := server
				if test.staleID {
					claimed.CloudID = "ins-stale"
				}
				if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "owned", backend.Cfg, claimed, core.SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
					t.Fatal(err)
				}
			}
			err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
			if test.seedClaim && !test.staleID {
				if err != nil || strings.Join(api.terminated, ",") != item.InstanceID {
					t.Fatalf("exact release err=%v terminated=%v", err, api.terminated)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "exact local ownership claim") {
				t.Fatalf("unowned release err=%v", err)
			}
			if len(api.terminated) != 0 {
				t.Fatalf("unowned instance was terminated: %v", api.terminated)
			}
		})
	}
}

func TestTencentCloudCleanupSkipsNameMatchedUnclaimedInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	leaseID := "cbx_abcdef123457"
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	tags := leaseTags(cfg, leaseID, "unclaimed", "ready", false, time.Now().Add(-24*time.Hour))
	tags = append(tags, tag{Key: accountLabel, Value: "100000000001"})
	api := &fakeTencentCloudAPI{item: instance{InstanceID: "ins-unclaimed", InstanceName: core.LeaseProviderName(leaseID, "unclaimed"), InstanceState: "RUNNING", Tags: tags}}
	var stderr strings.Builder
	backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: &stderr}).(*Backend)
	backend.clientFactory = func(core.Config, core.Runtime) (tencentCloudAPI, error) { return api, nil }
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminated) != 0 || !strings.Contains(stderr.String(), "no-exact-local-claim") {
		t.Fatalf("terminated=%v stderr=%q", api.terminated, stderr.String())
	}
}

func TestTencentCloudTerminationFencesConcurrentClaimMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	leaseID := "cbx_abcdef123458"
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	tags := leaseTags(cfg, leaseID, "fenced", "ready", false, time.Now().UTC())
	tags = append(tags, tag{Key: accountLabel, Value: "100000000001"})
	item := instance{InstanceID: "ins-fenced", InstanceName: core.LeaseProviderName(leaseID, "fenced"), InstanceState: "RUNNING", Tags: tags}
	api := &fakeTencentCloudAPI{item: item}
	backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*Backend)
	backend.clientFactory = func(core.Config, core.Runtime) (tencentCloudAPI, error) { return api, nil }
	server := serverFromInstance(item, backend.Cfg)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "fenced", backend.Cfg, server, core.SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	claim, _, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	updated := make(chan error, 1)
	api.terminateFn = func() {
		go func() {
			close(started)
			labels := shared.CloneLabels(claim.Labels)
			labels["state"] = "renewed"
			_, updateErr := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels)
			updated <- updateErr
		}()
		<-started
		select {
		case err := <-updated:
			t.Errorf("claim mutation escaped termination fence: %v", err)
		default:
		}
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if err := <-updated; err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("claim mutation after termination err=%v", err)
	}
}

type fakeTencentCloudAPI struct {
	item            instance
	replacedCurrent []tag
	replacedDesired []tag
	terminated      []string
	terminateFn     func()
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

func (f *fakeTencentCloudAPI) TerminateInstance(_ context.Context, id string) error {
	f.terminated = append(f.terminated, id)
	if f.terminateFn != nil {
		f.terminateFn()
	}
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
