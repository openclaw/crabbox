package nomad

import (
	"context"
	"errors"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeClient struct {
	selfCalls      int
	regionCalls    int
	namespaceCalls int
	regions        []string
	selfErr        error
	regionsErr     error
	namespaceErr   error
}

func (f *fakeClient) AgentSelf(context.Context) (*nomadapi.AgentSelf, error) {
	f.selfCalls++
	return &nomadapi.AgentSelf{}, f.selfErr
}

func (f *fakeClient) Regions(context.Context) ([]string, error) {
	f.regionCalls++
	return f.regions, f.regionsErr
}

func (f *fakeClient) NamespaceInfo(context.Context, string) (*nomadapi.Namespace, error) {
	f.namespaceCalls++
	return &nomadapi.Namespace{Name: "team-a"}, f.namespaceErr
}

func (f *fakeClient) RegisterJob(context.Context, *nomadapi.Job) (string, error) {
	return "", nil
}

func (f *fakeClient) JobInfo(context.Context, string) (*nomadapi.Job, error) {
	return nil, nil
}

func (f *fakeClient) JobAllocations(context.Context, string, bool) ([]*nomadapi.AllocationListStub, error) {
	return nil, nil
}

func (f *fakeClient) EvaluationInfo(context.Context, string) (*nomadapi.Evaluation, error) {
	return nil, nil
}

func (f *fakeClient) DeregisterJob(context.Context, string, bool) (string, error) {
	return "", nil
}

func (f *fakeClient) AllocationExec(context.Context, nomadExecRequest) (int, error) {
	return 0, nil
}

func TestDoctorReportsMissingConfigWithoutClientCall(t *testing.T) {
	t.Setenv("NOMAD_TOKEN", "secret-token")
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = ""
	called := false
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		clientFactory: func(Config, Runtime) (Client, error) {
			called = true
			return &fakeClient{}, nil
		},
	}
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("client factory called despite missing address")
	}
	if result.Status != "failed" || len(result.Checks) != 1 || result.Checks[0].Check != "config" || result.Checks[0].Details["class"] != "missing_address" {
		t.Fatalf("result=%#v", result)
	}
}

func TestDoctorReportsMissingTokenWithoutClientCall(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = "https://nomad.example.test:4646"
	cfg.Nomad.TokenEnv = "TEAM_A_NOMAD_TOKEN"
	called := false
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		clientFactory: func(Config, Runtime) (Client, error) {
			called = true
			return &fakeClient{}, nil
		},
	}
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("client factory called despite missing token")
	}
	if result.Status != "failed" || len(result.Checks) != 1 || result.Checks[0].Check != "auth" || result.Checks[0].Details["token_env"] != "TEAM_A_NOMAD_TOKEN" {
		t.Fatalf("result=%#v", result)
	}
}

func TestDoctorUsesOnlyReadOnlyReadinessCalls(t *testing.T) {
	t.Setenv("NOMAD_TOKEN", "secret-token")
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = "https://nomad.example.test:4646"
	cfg.Nomad.Region = "global"
	cfg.Nomad.Namespace = "team-a"
	fake := &fakeClient{regions: []string{"global", "edge"}}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		clientFactory: func(Config, Runtime) (Client, error) {
			return fake, nil
		},
	}
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("result=%#v", result)
	}
	if fake.selfCalls != 1 || fake.regionCalls != 1 || fake.namespaceCalls != 1 {
		t.Fatalf("calls self=%d regions=%d namespace=%d", fake.selfCalls, fake.regionCalls, fake.namespaceCalls)
	}
	for _, check := range result.Checks {
		if check.Details["mutation"] != "false" {
			t.Fatalf("mutating check: %#v", check)
		}
	}
}

func TestDoctorReportsRegionAndNamespaceFailures(t *testing.T) {
	t.Setenv("NOMAD_TOKEN", "secret-token")
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = "https://nomad.example.test:4646"
	cfg.Nomad.Region = "missing"
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		clientFactory: func(Config, Runtime) (Client, error) {
			return &fakeClient{regions: []string{"global"}}, nil
		},
	}
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.Checks[len(result.Checks)-1].Details["class"] != "missing_region" {
		t.Fatalf("region result=%#v", result)
	}

	cfg.Nomad.Region = ""
	cfg.Nomad.Namespace = "team-a"
	b.cfg = cfg
	b.clientFactory = func(Config, Runtime) (Client, error) {
		return &fakeClient{namespaceErr: errors.New("Unexpected response code: 403")}, nil
	}
	result, err = b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.Checks[len(result.Checks)-1].Check != "namespace" {
		t.Fatalf("namespace result=%#v", result)
	}
}
