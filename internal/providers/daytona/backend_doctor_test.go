package daytona

import (
	"context"
	"strings"
	"testing"
	"time"

	apidaytona "github.com/daytonaio/daytona/libs/api-client-go"
)

type fakeDaytonaDoctorAPI struct {
	listCalls    int
	mutated      bool
	sandboxes    []apidaytona.Sandbox
	getSandboxes map[string]*apidaytona.Sandbox
	deleted      []string
}

func (a *fakeDaytonaDoctorAPI) CreateSandbox(context.Context, apidaytona.CreateSandbox) (*apidaytona.Sandbox, error) {
	a.mutated = true
	return nil, nil
}

func (a *fakeDaytonaDoctorAPI) GetSandbox(_ context.Context, id string) (*apidaytona.Sandbox, error) {
	return a.getSandboxes[id], nil
}

func (a *fakeDaytonaDoctorAPI) ListCrabboxSandboxes(context.Context) ([]apidaytona.Sandbox, error) {
	a.listCalls++
	return a.sandboxes, nil
}

func (a *fakeDaytonaDoctorAPI) StartSandbox(context.Context, string) (*apidaytona.Sandbox, error) {
	a.mutated = true
	return nil, nil
}

func (a *fakeDaytonaDoctorAPI) DeleteSandbox(_ context.Context, id string) error {
	a.mutated = true
	a.deleted = append(a.deleted, id)
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
	owned := apidaytona.Sandbox{}
	owned.SetId("sandbox-one")
	owned.SetLabels(map[string]string{
		"crabbox":  "true",
		"provider": daytonaProvider,
		"lease":    "cbx_666666666666",
	})
	weaklyLabelled := apidaytona.Sandbox{}
	weaklyLabelled.SetId("sandbox-weak")
	weaklyLabelled.SetLabels(map[string]string{"crabbox": "true"})
	fake := &fakeDaytonaDoctorAPI{sandboxes: []apidaytona.Sandbox{owned, weaklyLabelled}}
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
