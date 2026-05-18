package daytona

import (
	"context"
	"strings"
	"testing"
	"time"

	apidaytona "github.com/daytonaio/daytona/libs/api-client-go"
)

type fakeDaytonaDoctorAPI struct {
	listCalls int
	mutated   bool
	sandboxes []apidaytona.Sandbox
}

func (a *fakeDaytonaDoctorAPI) CreateSandbox(context.Context, apidaytona.CreateSandbox) (*apidaytona.Sandbox, error) {
	a.mutated = true
	return nil, nil
}

func (a *fakeDaytonaDoctorAPI) GetSandbox(context.Context, string) (*apidaytona.Sandbox, error) {
	return nil, nil
}

func (a *fakeDaytonaDoctorAPI) ListCrabboxSandboxes(context.Context) ([]apidaytona.Sandbox, error) {
	a.listCalls++
	return a.sandboxes, nil
}

func (a *fakeDaytonaDoctorAPI) StartSandbox(context.Context, string) (*apidaytona.Sandbox, error) {
	a.mutated = true
	return nil, nil
}

func (a *fakeDaytonaDoctorAPI) DeleteSandbox(context.Context, string) error {
	a.mutated = true
	return nil
}

func (a *fakeDaytonaDoctorAPI) ReplaceLabels(context.Context, string, map[string]string) error {
	a.mutated = true
	return nil
}

func (a *fakeDaytonaDoctorAPI) UpdateLastActivity(context.Context, string) error {
	a.mutated = true
	return nil
}

func (a *fakeDaytonaDoctorAPI) CreateSSHAccess(context.Context, string, time.Duration) (daytonaSSHAccess, error) {
	a.mutated = true
	return daytonaSSHAccess{}, nil
}

func TestDaytonaDoctorListsInventoryOnly(t *testing.T) {
	sandbox := apidaytona.Sandbox{}
	sandbox.SetId("sandbox-one")
	fake := &fakeDaytonaDoctorAPI{sandboxes: []apidaytona.Sandbox{sandbox}}
	old := newDaytonaClient
	newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) {
		return fake, nil
	}
	t.Cleanup(func() { newDaytonaClient = old })

	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != daytonaProvider || !strings.Contains(result.Message, "inventory=ready api=list mutation=false leases=1 runtime=unchecked") {
		t.Fatalf("result=%#v", result)
	}
	if fake.listCalls != 1 {
		t.Fatalf("list calls=%d, want 1", fake.listCalls)
	}
	if fake.mutated {
		t.Fatal("doctor called a mutating Daytona method")
	}
}
