package ovh

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeAPI struct {
	authCalls     int
	regionCalls   int
	flavorCalls   int
	imageCalls    int
	instanceCalls int
	mutatingCalls int
	regions       []Region
	flavors       []Flavor
	images        []Image
	instances     []Instance
}

func (f *fakeAPI) AuthTime(context.Context) (int64, error) {
	f.authCalls++
	return 1234567890, nil
}

func (f *fakeAPI) ListProjects(context.Context) ([]Project, error) {
	return []Project{{ID: "project-test"}}, nil
}

func (f *fakeAPI) ListRegions(context.Context, string) ([]Region, error) {
	f.regionCalls++
	return f.regions, nil
}

func (f *fakeAPI) ListFlavors(context.Context, string, string) ([]Flavor, error) {
	f.flavorCalls++
	return f.flavors, nil
}

func (f *fakeAPI) GetFlavor(context.Context, string, string) (Flavor, error) {
	f.mutatingCalls++
	return Flavor{}, nil
}

func (f *fakeAPI) ListImages(context.Context, string, string) ([]Image, error) {
	f.imageCalls++
	return f.images, nil
}

func (f *fakeAPI) GetImage(context.Context, string, string) (Image, error) {
	f.mutatingCalls++
	return Image{}, nil
}

func (f *fakeAPI) ListSSHKeys(context.Context, string) ([]SSHKey, error) {
	f.mutatingCalls++
	return nil, nil
}

func (f *fakeAPI) GetSSHKey(context.Context, string, string) (SSHKey, error) {
	f.mutatingCalls++
	return SSHKey{}, nil
}

func (f *fakeAPI) ListInstances(context.Context, string) ([]Instance, error) {
	f.instanceCalls++
	return f.instances, nil
}

func (f *fakeAPI) GetInstance(context.Context, string, string) (Instance, error) {
	f.mutatingCalls++
	return Instance{}, nil
}

func TestDoctorUsesReadOnlyDiscovery(t *testing.T) {
	fake := &fakeAPI{
		regions:   []Region{{Name: "GRA11"}},
		flavors:   []Flavor{{ID: "flavor-id", Name: "b3-8"}},
		images:    []Image{{ID: "image-id", Name: "Ubuntu 24.04"}},
		instances: []Instance{{ID: "one", Name: "crabbox-ready"}, {ID: "two", Name: "unrelated"}},
	}
	backend := NewBackend(Provider{}.Spec(), core.Config{OVH: core.OVHConfig{
		Endpoint:  "https://user:pass@api.us.ovhcloud.com/1.0",
		ProjectID: "project-test",
		Region:    "GRA11",
		Image:     "Ubuntu 24.04",
		Flavor:    "b3-8",
	}}, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		return fake, nil
	}

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "inventory=ready api=list mutation=false leases=1") {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Message, "user:pass") {
		t.Fatalf("doctor leaked endpoint userinfo: %s", result.Message)
	}
	if fake.authCalls != 1 || fake.regionCalls != 1 || fake.flavorCalls != 1 || fake.imageCalls != 1 || fake.instanceCalls != 1 {
		t.Fatalf("unexpected read call counts: %#v", fake)
	}
	if fake.mutatingCalls != 0 {
		t.Fatalf("doctor used non-discovery calls: %#v", fake)
	}
}

func TestDoctorReportsMissingProjectWithoutClient(t *testing.T) {
	backend := NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		t.Fatal("client should not be created when project ID is missing")
		return nil, nil
	}

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "mutation=false") || len(result.Checks) != 1 || result.Checks[0].Check != "configuration" {
		t.Fatalf("result=%#v", result)
	}
}

func TestDoctorReportsUnavailableFlavor(t *testing.T) {
	fake := &fakeAPI{
		regions: []Region{{Name: "GRA11"}},
		flavors: []Flavor{{ID: "other", Name: "b3-16"}},
	}
	backend := NewBackend(Provider{}.Spec(), core.Config{OVH: core.OVHConfig{
		ProjectID: "project-test",
		Region:    "GRA11",
		Flavor:    "b3-8",
	}}, core.Runtime{})
	backend.clientFactory = func(core.Config, core.Runtime) (API, error) {
		return fake, nil
	}

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || len(result.Checks) != 1 || result.Checks[0].Check != "flavor" || !strings.Contains(result.Checks[0].Message, "b3-8") {
		t.Fatalf("result=%#v", result)
	}
	if fake.imageCalls != 0 || fake.instanceCalls != 0 || fake.mutatingCalls != 0 {
		t.Fatalf("doctor continued after failed flavor check: %#v", fake)
	}
}

func TestBackendImplementsLeaseInterfacesWithNonMutatingStubs(t *testing.T) {
	var backend any = NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{})
	if _, ok := backend.(core.SSHLeaseBackend); !ok {
		t.Fatal("ovh backend should satisfy SSHLeaseBackend with explicit lifecycle stubs")
	}
	if _, ok := backend.(core.CleanupBackend); !ok {
		t.Fatal("ovh backend should satisfy CleanupBackend with explicit lifecycle stub")
	}
	err := backend.(*Backend).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{})
	if err == nil || !strings.Contains(err.Error(), "lifecycle is not implemented") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
}
