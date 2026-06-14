package lambda

import (
	"context"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeDoctorClient struct {
	regions     []Region
	types       []InstanceType
	images      []Image
	instances   []Instance
	filesystems []Filesystem
	firewalls   []FirewallRuleset
	err         error
	calls       []string
}

func (f *fakeDoctorClient) ListRegions(context.Context) ([]Region, error) {
	f.calls = append(f.calls, "regions")
	return f.regions, f.err
}
func (f *fakeDoctorClient) ListInstanceTypes(context.Context) ([]InstanceType, error) {
	f.calls = append(f.calls, "types")
	return f.types, f.err
}
func (f *fakeDoctorClient) ListImages(context.Context) ([]Image, error) {
	f.calls = append(f.calls, "images")
	return f.images, f.err
}
func (f *fakeDoctorClient) ListInstances(context.Context) ([]Instance, error) {
	f.calls = append(f.calls, "instances")
	return f.instances, f.err
}
func (f *fakeDoctorClient) ListFilesystems(context.Context) ([]Filesystem, error) {
	f.calls = append(f.calls, "filesystems")
	return f.filesystems, f.err
}
func (f *fakeDoctorClient) ListFirewallRulesets(context.Context) ([]FirewallRuleset, error) {
	f.calls = append(f.calls, "firewalls")
	return f.firewalls, f.err
}

func TestDoctorMissingTokenDoesNotCallHTTP(t *testing.T) {
	t.Setenv(tokenEnv, "")
	called := false
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  core.Config{Provider: providerName},
		clientFactory: func(core.Runtime) (lambdaAPI, error) {
			called = true
			return nil, errors.New("should not be called")
		},
	}
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("clientFactory called without token")
	}
	if len(result.Checks) != 1 || result.Checks[0].Details["class"] != "missing_token" {
		t.Fatalf("checks=%#v", result.Checks)
	}
}

func TestDoctorSuccessIsReadOnly(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lambda.Region = "us-west-1"
	cfg.Lambda.Type = "gpu_1x_a10"
	cfg.Lambda.ImageFamily = "lambda-stack-24-04"
	cfg.Lambda.FirewallRuleset = "crabbox"
	cfg.Lambda.FilesystemNames = []string{"cache"}
	client := &fakeDoctorClient{
		regions:     []Region{{Name: "us-west-1"}},
		types:       []InstanceType{{Name: "gpu_1x_a10", RegionsWithCapacityAvailable: []string{"us-west-1"}}},
		images:      []Image{{Family: "lambda-stack-24-04", Region: "us-west-1"}},
		filesystems: []Filesystem{{Name: "cache", Region: Region{Name: "us-west-1"}}},
		firewalls:   []FirewallRuleset{{Name: "crabbox", Region: Region{Name: "us-west-1"}}},
		instances:   []Instance{{ID: "i-1"}},
	}
	result, err := lambdaDoctor(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Checks) == 0 {
		t.Fatal("no checks")
	}
	if strings.Join(client.calls, ",") != "regions,types,images,filesystems,firewalls,instances" {
		t.Fatalf("calls=%v", client.calls)
	}
	for _, check := range result.Checks {
		if check.Details["mutation"] != "false" {
			t.Fatalf("mutating check: %#v", check)
		}
		if check.Status == "failed" {
			t.Fatalf("unexpected failed check: %#v", check)
		}
	}
}

func TestDoctorClassifiesAPIErrorCodes(t *testing.T) {
	tests := map[string]string{
		"global/invalid-api-key":                                 "invalid_auth",
		"global/account-inactive":                                "account_inactive",
		"global/invalid-address":                                 "invalid_billing",
		"global/quota-exceeded":                                  "quota",
		"instance-operations/launch/insufficient-capacity":       "capacity",
		"instance-operations/launch/file-system-in-wrong-region": "filesystem_region",
		"global/object-does-not-exist":                           "missing_resource",
		"global/invalid-parameters":                              "invalid_config",
		"provider/upstream":                                      "provider",
	}
	for code, want := range tests {
		err := &APIError{Code: code}
		if got := classifyError(err); got != want {
			t.Fatalf("classifyError(%q)=%q want %q", code, got, want)
		}
	}
}

func TestDoctorDetectsCapacityGap(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lambda.Region = "us-west-1"
	cfg.Lambda.Type = "gpu_1x_a10"
	cfg.Lambda.ImageFamily = ""
	client := &fakeDoctorClient{
		regions: []Region{{Name: "us-west-1"}},
		types:   []InstanceType{{Name: "gpu_1x_a10", RegionsWithCapacityAvailable: []string{"us-east-1"}}},
	}
	result, err := lambdaDoctor(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	last := result.Checks[len(result.Checks)-1]
	if last.Check != "capacity" || last.Details["class"] != "insufficient_capacity" {
		t.Fatalf("last=%#v", last)
	}
}
